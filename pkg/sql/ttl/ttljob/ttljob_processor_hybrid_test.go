// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttljob

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

// mockHybridDeleteBuilder is a DeleteBuilder for hybrid cleaner mode that
// deletes from secondary indexes based on partition time bounds rather than
// selected primary keys.
type mockHybridDeleteBuilder struct {
	// indexID is the secondary index being cleaned
	indexID descpb.IndexID
	// partitionStart is the lower bound (inclusive) of the partition
	partitionStart time.Time
	// partitionEnd is the upper bound (exclusive) of the partition
	partitionEnd time.Time
	// deletedRows tracks how many rows were deleted per call
	deletedRows []int64
	// callCount tracks the number of times Run was called
	callCount int
	// errorOnCall if non-nil, return this error on the specified call index
	errorOnCall map[int]error
}

var _ DeleteQueryBuilder = (*mockHybridDeleteBuilder)(nil)

// Run deletes rows from the secondary index within partition bounds.
// Unlike normal TTL deletion, it doesn't require selected PK rows.
func (m *mockHybridDeleteBuilder) Run(
	_ context.Context, _ isql.Txn, rows []tree.Datums,
) (int64, error) {
	callIdx := m.callCount
	m.callCount++

	// Check if we should error on this call
	if err, ok := m.errorOnCall[callIdx]; ok && err != nil {
		return 0, err
	}

	// In hybrid mode, we ignore the 'rows' parameter since we're not
	// deleting based on selected PKs. Instead, we delete based on time bounds.
	// For testing, we'll simulate some deleted rows.
	var deleted int64
	if callIdx < len(m.deletedRows) {
		deleted = m.deletedRows[callIdx]
	}

	return deleted, nil
}

// BuildQuery generates a DELETE query for hybrid cleaner mode.
// The query targets a secondary index and filters by partition time bounds.
func (m *mockHybridDeleteBuilder) BuildQuery(numRows int) string {
	// For hybrid mode, we don't use numRows. The query format would be:
	// DELETE FROM table@secondary_idx WHERE ttl_col >= $start AND ttl_col < $end LIMIT $batch
	return "DELETE FROM test_table@idx WHERE ts >= $1 AND ts < $2 LIMIT 100"
}

// GetBatchSize returns the batch size for deletions.
func (m *mockHybridDeleteBuilder) GetBatchSize() int {
	return 100
}

// TestHybridCleanerBasicDeletion tests that hybrid cleaner mode can delete
// from secondary indexes without a SELECT phase.
func TestHybridCleanerBasicDeletion(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	srv, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()
	execCfg := s.ExecutorConfig().(sql.ExecutorConfig)

	// Create a test table with range partitioning and partition TTL.
	// Now that partitioning is available in the base binary, we can use real
	// range partitioned tables for testing.
	// Note: Using INT for partitioning (instead of TIMESTAMPTZ) since partition
	// boundary values must be constants without context-dependent operations.
	runner := sqlutils.MakeSQLRunner(sqlDB)
	runner.Exec(t, "CREATE DATABASE testdb")
	runner.Exec(t, "CREATE SCHEMA testdb.testschema")
	runner.Exec(t, `CREATE TABLE testdb.testschema.test_table (
		ts TIMESTAMPTZ NOT NULL,
		user_id INT NOT NULL,
		data STRING,
		PRIMARY KEY (ts, user_id)
	) PARTITION BY RANGE (ts) (
		PARTITION p1 VALUES FROM (MINVALUE) TO (MAXVALUE)
	) WITH (
		ttl_mode = 'partition',
		ttl_column = 'ts',
		ttl_retention = '30d',
		ttl_granularity = '1d',
		ttl_lookahead = '2d'
	)`)
	runner.Exec(t, "CREATE INDEX idx_data ON testdb.testschema.test_table(data)")

	var tableDesc catalog.TableDescriptor
	require.NoError(t, sql.DescsTxn(ctx, &execCfg, func(
		ctx context.Context, txn isql.Txn, descriptors *descs.Collection,
	) error {
		db, err := descriptors.ByName(txn.KV()).Get().Database(ctx, "testdb")
		if err != nil {
			return err
		}
		schema, err := descriptors.ByName(txn.KV()).Get().Schema(ctx, db, "testschema")
		if err != nil {
			return err
		}
		tableDesc, err = descriptors.ByName(txn.KV()).Get().Table(ctx, db, schema, "test_table")
		return err
	}))

	// Setup hybrid cleaner job details
	partitionStart := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	partitionEnd := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	// Get real primary key spans from the table (now that we have real partitions)
	primaryIndexSpan := tableDesc.PrimaryIndexSpan(s.Codec())

	hybridDetails := &jobspb.HybridCleanerDetails{
		PartitionStart: partitionStart,
		PartitionEnd:   partitionEnd,
		TargetIndexIDs: []descpb.IndexID{2}, // Secondary index ID
		PartitionSpans: []roachpb.Span{primaryIndexSpan},
	}

	var nodeID base.NodeIDContainer
	nodeID.Set(ctx, roachpb.NodeID(1))

	flowCtx := execinfra.FlowCtx{
		Cfg: &execinfra.ServerConfig{
			DB:          s.InternalDB().(descs.DB),
			Settings:    s.ClusterSettings(),
			Codec:       s.Codec(),
			JobRegistry: s.JobRegistry().(*jobs.Registry),
		},
		EvalCtx: &eval.Context{
			Codec:    s.Codec(),
			Settings: s.ClusterSettings(),
		},
		NodeID: base.NewSQLIDContainerForNode(&nodeID),
	}

	// Create a mock processor
	mockTTLProc := ttlProcessor{
		ttlSpec: execinfrapb.TTLSpec{
			RowLevelTTLDetails: jobspb.RowLevelTTLDetails{
				TableID:              tableDesc.GetID(),
				TableVersion:         tableDesc.GetVersion(),
				HybridCleanerDetails: hybridDetails,
			},
			DeleteBatchSize: 100,
			DeleteRateLimit: 1000000, // High limit to avoid rate limiting in tests
			Spans:           hybridDetails.PartitionSpans,
		},
		ProcessorBase: execinfra.ProcessorBase{
			ProcessorBaseNoHelper: execinfra.ProcessorBaseNoHelper{
				FlowCtx: &flowCtx,
			},
		},
	}

	// Setup progress updater
	mockRowReceiver := &metadataCache{}
	mockTTLProc.progressUpdater = &coordinatorStreamUpdater{proc: &mockTTLProc}

	// Run the hybrid cleaner
	err := mockTTLProc.work(ctx, mockRowReceiver)

	// Verify hybrid cleaner mode was invoked (no errors expected)
	require.NoError(t, err)

	// Verify that:
	// 1. The processor completed without errors
	// 2. Progress was tracked (at least one progress update)
	// Note: Since we haven't implemented actual deletion logic yet,
	// this test verifies the framework runs without panicking.
	require.NotNil(t, hybridDetails)
}

// TestHybridCleanerMultipleIndexes tests cleanup across multiple non-aligned
// secondary indexes.
func TestHybridCleanerMultipleIndexes(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// TODO(SPILLY): Test that:
	// 1. Multiple secondary indexes are processed sequentially
	// 2. Each index gets its own delete operations
	// 3. Progress is tracked per index
	// 4. Failure in one index doesn't affect others
}

// TestHybridCleanerPartitionBoundFiltering tests that deletions are correctly
// filtered by partition time bounds.
func TestHybridCleanerPartitionBoundFiltering(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// TODO(SPILLY): Test that:
	// 1. Only rows within [PartitionStart, PartitionEnd) are deleted
	// 2. Rows outside the bounds are not affected
	// 3. Boundary conditions are handled correctly (inclusive start, exclusive end)
}

// TestHybridCleanerErrorHandling tests error handling in hybrid cleaner mode.
func TestHybridCleanerErrorHandling(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// TODO(SPILLY): Test that:
	// 1. Retryable errors are retried appropriately
	// 2. Non-retryable errors cause job failure
	// 3. Partial progress is tracked even on failure
	// 4. Progress updates are sent correctly
}

// TestHybridCleanerNoSelectPhase tests that the SELECT phase is completely
// skipped in hybrid cleaner mode.
func TestHybridCleanerNoSelectPhase(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// TODO(SPILLY): Test that:
	// 1. SelectQueryBuilder is never created in hybrid mode
	// 2. No SELECT queries are executed
	// 3. DELETE operations proceed directly without SELECT results
	// 4. Performance is improved compared to row-level TTL
}

// TestHybridCleanerSpanDistribution tests that PK spans are correctly
// distributed across nodes for parallelism.
func TestHybridCleanerSpanDistribution(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// TODO(SPILLY): Test that:
	// 1. Multiple partition spans are processed in parallel
	// 2. Each span is assigned to appropriate nodes
	// 3. Progress is tracked per span
	// 4. All spans complete successfully
}
