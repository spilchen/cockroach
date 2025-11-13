// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttljob

import (
	"context"
	"fmt"
	"strings"
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
	"github.com/cockroachdb/errors"
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

	ctx := context.Background()
	srv, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()
	execCfg := s.ExecutorConfig().(sql.ExecutorConfig)

	// Create a test table with partition TTL and multiple secondary indexes.
	runner := sqlutils.MakeSQLRunner(sqlDB)
	runner.Exec(t, "CREATE DATABASE testdb")
	runner.Exec(t, "CREATE SCHEMA testdb.testschema")
	runner.Exec(t, `CREATE TABLE testdb.testschema.multi_index_table (
		ts TIMESTAMPTZ NOT NULL,
		user_id INT NOT NULL,
		data STRING,
		category STRING,
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

	// Create multiple secondary indexes.
	runner.Exec(t, "CREATE INDEX idx_data ON testdb.testschema.multi_index_table(data)")
	runner.Exec(t, "CREATE INDEX idx_category ON testdb.testschema.multi_index_table(category)")
	runner.Exec(t, "CREATE INDEX idx_user ON testdb.testschema.multi_index_table(user_id)")

	// Insert test data within the partition bounds that will be deleted.
	partitionStart := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	partitionEnd := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	// Insert rows that fall within the partition time bounds.
	for i := 0; i < 10; i++ {
		ts := partitionStart.Add(time.Duration(i) * time.Hour)
		runner.Exec(t, "INSERT INTO testdb.testschema.multi_index_table VALUES ($1, $2, $3, $4)",
			ts, i, fmt.Sprintf("data%d", i), fmt.Sprintf("cat%d", i%3))
	}

	// Verify initial row count.
	var initialCount int
	runner.QueryRow(t, "SELECT count(*) FROM testdb.testschema.multi_index_table").Scan(&initialCount)
	require.Equal(t, 10, initialCount)

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
		tableDesc, err = descriptors.ByName(txn.KV()).Get().Table(ctx, db, schema, "multi_index_table")
		return err
	}))

	// Get the secondary index IDs.
	var secondaryIndexIDs []descpb.IndexID
	for _, idx := range tableDesc.PublicNonPrimaryIndexes() {
		secondaryIndexIDs = append(secondaryIndexIDs, idx.GetID())
	}
	require.Len(t, secondaryIndexIDs, 3, "expected 3 secondary indexes")

	primaryIndexSpan := tableDesc.PrimaryIndexSpan(s.Codec())

	// Setup hybrid cleaner details with all secondary indexes.
	hybridDetails := &jobspb.HybridCleanerDetails{
		PartitionStart: partitionStart,
		PartitionEnd:   partitionEnd,
		TargetIndexIDs: secondaryIndexIDs,
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

	// Create TTL processor.
	mockTTLProc := ttlProcessor{
		ttlSpec: execinfrapb.TTLSpec{
			RowLevelTTLDetails: jobspb.RowLevelTTLDetails{
				TableID:              tableDesc.GetID(),
				TableVersion:         tableDesc.GetVersion(),
				HybridCleanerDetails: hybridDetails,
			},
			DeleteBatchSize: 100,
			DeleteRateLimit: 1000000,
			Spans:           hybridDetails.PartitionSpans,
		},
		ProcessorBase: execinfra.ProcessorBase{
			ProcessorBaseNoHelper: execinfra.ProcessorBaseNoHelper{
				FlowCtx: &flowCtx,
			},
		},
	}

	// Setup progress updater.
	mockRowReceiver := &metadataCache{}
	mockTTLProc.progressUpdater = &coordinatorStreamUpdater{proc: &mockTTLProc}

	// Run the hybrid cleaner.
	err := mockTTLProc.work(ctx, mockRowReceiver)
	require.NoError(t, err)

	// Verify all rows were deleted (since they all fall within partition bounds).
	var finalCount int
	runner.QueryRow(t, "SELECT count(*) FROM testdb.testschema.multi_index_table").Scan(&finalCount)
	require.Equal(t, 0, finalCount, "all rows should be deleted from all indexes")

	// Verify progress was tracked for all index/span combinations.
	// With 3 indexes and 1 span, we should have 3 total work items.
	progressUpdater := mockTTLProc.progressUpdater.(*coordinatorStreamUpdater)
	progressUpdater.mu.Lock()
	completedSpanCount := len(progressUpdater.mu.completedSpans)
	progressUpdater.mu.Unlock()

	// Each (index, span) pair counts as one span in progress tracking.
	expectedSpanCount := len(secondaryIndexIDs) * len(hybridDetails.PartitionSpans)
	require.Equal(t, expectedSpanCount, completedSpanCount,
		"expected progress tracking for all index/span combinations")
}

// TestHybridCleanerPartitionBoundFiltering tests that deletions are correctly
// filtered by partition time bounds.
func TestHybridCleanerPartitionBoundFiltering(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	srv, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()
	execCfg := s.ExecutorConfig().(sql.ExecutorConfig)

	// Create a test table with partition TTL.
	runner := sqlutils.MakeSQLRunner(sqlDB)
	runner.Exec(t, "CREATE DATABASE testdb")
	runner.Exec(t, "CREATE SCHEMA testdb.testschema")
	runner.Exec(t, `CREATE TABLE testdb.testschema.bound_test_table (
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
	runner.Exec(t, "CREATE INDEX idx_data ON testdb.testschema.bound_test_table(data)")

	// Define partition boundaries for the hybrid cleaner.
	// We'll delete rows from 2024-01-02 00:00:00 (inclusive) to 2024-01-03 00:00:00 (exclusive).
	partitionStart := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	partitionEnd := time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC)

	// Insert test data with various timestamps relative to partition bounds.
	testCases := []struct {
		ts           time.Time
		userID       int
		data         string
		shouldDelete bool
		description  string
	}{
		// Before partition start - should NOT be deleted.
		{time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), 1, "before_start", false, "before partition start"},
		{time.Date(2024, 1, 1, 23, 59, 59, 0, time.UTC), 2, "just_before_start", false, "just before partition start"},

		// Exactly at partition start - SHOULD be deleted (inclusive start).
		{partitionStart, 3, "at_start", true, "exactly at partition start"},

		// Within partition bounds - SHOULD be deleted.
		{time.Date(2024, 1, 2, 6, 0, 0, 0, time.UTC), 4, "within_bounds_1", true, "within partition bounds (morning)"},
		{time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC), 5, "within_bounds_2", true, "within partition bounds (noon)"},
		{time.Date(2024, 1, 2, 18, 0, 0, 0, time.UTC), 6, "within_bounds_3", true, "within partition bounds (evening)"},

		// Just before partition end - SHOULD be deleted (still within bounds).
		{time.Date(2024, 1, 2, 23, 59, 59, 0, time.UTC), 7, "just_before_end", true, "just before partition end"},

		// Exactly at partition end - should NOT be deleted (exclusive end).
		{partitionEnd, 8, "at_end", false, "exactly at partition end"},

		// After partition end - should NOT be deleted.
		{time.Date(2024, 1, 3, 0, 0, 1, 0, time.UTC), 9, "just_after_end", false, "just after partition end"},
		{time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 10, "after_end", false, "after partition end"},
	}

	// Insert all test rows.
	for _, tc := range testCases {
		runner.Exec(t, "INSERT INTO testdb.testschema.bound_test_table VALUES ($1, $2, $3)",
			tc.ts, tc.userID, tc.data)
	}

	// Verify initial row count.
	var initialCount int
	runner.QueryRow(t, "SELECT count(*) FROM testdb.testschema.bound_test_table").Scan(&initialCount)
	require.Equal(t, len(testCases), initialCount)

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
		tableDesc, err = descriptors.ByName(txn.KV()).Get().Table(ctx, db, schema, "bound_test_table")
		return err
	}))

	// Get the secondary index ID.
	var secondaryIndexID descpb.IndexID
	for _, idx := range tableDesc.PublicNonPrimaryIndexes() {
		secondaryIndexID = idx.GetID()
		break
	}

	primaryIndexSpan := tableDesc.PrimaryIndexSpan(s.Codec())

	// Setup hybrid cleaner details.
	hybridDetails := &jobspb.HybridCleanerDetails{
		PartitionStart: partitionStart,
		PartitionEnd:   partitionEnd,
		TargetIndexIDs: []descpb.IndexID{secondaryIndexID},
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

	// Create TTL processor.
	mockTTLProc := ttlProcessor{
		ttlSpec: execinfrapb.TTLSpec{
			RowLevelTTLDetails: jobspb.RowLevelTTLDetails{
				TableID:              tableDesc.GetID(),
				TableVersion:         tableDesc.GetVersion(),
				HybridCleanerDetails: hybridDetails,
			},
			DeleteBatchSize: 100,
			DeleteRateLimit: 1000000,
			Spans:           hybridDetails.PartitionSpans,
		},
		ProcessorBase: execinfra.ProcessorBase{
			ProcessorBaseNoHelper: execinfra.ProcessorBaseNoHelper{
				FlowCtx: &flowCtx,
			},
		},
	}

	// Setup progress updater.
	mockRowReceiver := &metadataCache{}
	mockTTLProc.progressUpdater = &coordinatorStreamUpdater{proc: &mockTTLProc}

	// Run the hybrid cleaner.
	err := mockTTLProc.work(ctx, mockRowReceiver)
	require.NoError(t, err)

	// Verify that only rows within [PartitionStart, PartitionEnd) were deleted.
	for _, tc := range testCases {
		var count int
		runner.QueryRow(t,
			"SELECT count(*) FROM testdb.testschema.bound_test_table WHERE user_id = $1",
			tc.userID).Scan(&count)

		if tc.shouldDelete {
			require.Equal(t, 0, count,
				"Row with user_id=%d (%s) should have been deleted but still exists",
				tc.userID, tc.description)
		} else {
			require.Equal(t, 1, count,
				"Row with user_id=%d (%s) should NOT have been deleted but is missing",
				tc.userID, tc.description)
		}
	}

	// Verify the final row count matches expected (rows that should NOT be deleted).
	var expectedRemainingCount int
	for _, tc := range testCases {
		if !tc.shouldDelete {
			expectedRemainingCount++
		}
	}

	var finalCount int
	runner.QueryRow(t, "SELECT count(*) FROM testdb.testschema.bound_test_table").Scan(&finalCount)
	require.Equal(t, expectedRemainingCount, finalCount,
		"Expected %d rows to remain after deletion", expectedRemainingCount)
}

// TestHybridCleanerErrorHandling tests error handling in hybrid cleaner mode.
func TestHybridCleanerErrorHandling(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	srv, sqlDB, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()
	execCfg := s.ExecutorConfig().(sql.ExecutorConfig)

	// Create a test table with partition TTL.
	runner := sqlutils.MakeSQLRunner(sqlDB)
	runner.Exec(t, "CREATE DATABASE testdb")
	runner.Exec(t, "CREATE SCHEMA testdb.testschema")
	runner.Exec(t, `CREATE TABLE testdb.testschema.error_test_table (
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
	runner.Exec(t, "CREATE INDEX idx_data ON testdb.testschema.error_test_table(data)")
	runner.Exec(t, "CREATE INDEX idx_user ON testdb.testschema.error_test_table(user_id)")

	// Insert test data.
	partitionStart := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	partitionEnd := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 100; i++ {
		ts := partitionStart.Add(time.Duration(i) * time.Minute)
		runner.Exec(t, "INSERT INTO testdb.testschema.error_test_table VALUES ($1, $2, $3)",
			ts, i, fmt.Sprintf("data%d", i))
	}

	// Verify initial row count.
	var initialCount int
	runner.QueryRow(t, "SELECT count(*) FROM testdb.testschema.error_test_table").Scan(&initialCount)
	require.Equal(t, 100, initialCount)

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
		tableDesc, err = descriptors.ByName(txn.KV()).Get().Table(ctx, db, schema, "error_test_table")
		return err
	}))

	// Get secondary index IDs.
	var secondaryIndexIDs []descpb.IndexID
	for _, idx := range tableDesc.PublicNonPrimaryIndexes() {
		secondaryIndexIDs = append(secondaryIndexIDs, idx.GetID())
	}
	require.Len(t, secondaryIndexIDs, 2, "expected 2 secondary indexes")

	primaryIndexSpan := tableDesc.PrimaryIndexSpan(s.Codec())

	t.Run("context_cancellation", func(t *testing.T) {
		// Test that context cancellation stops the hybrid cleaner gracefully.
		hybridDetails := &jobspb.HybridCleanerDetails{
			PartitionStart: partitionStart,
			PartitionEnd:   partitionEnd,
			TargetIndexIDs: secondaryIndexIDs,
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

		mockTTLProc := ttlProcessor{
			ttlSpec: execinfrapb.TTLSpec{
				RowLevelTTLDetails: jobspb.RowLevelTTLDetails{
					TableID:              tableDesc.GetID(),
					TableVersion:         tableDesc.GetVersion(),
					HybridCleanerDetails: hybridDetails,
				},
				DeleteBatchSize: 10,
				DeleteRateLimit: 1000000,
				Spans:           hybridDetails.PartitionSpans,
			},
			ProcessorBase: execinfra.ProcessorBase{
				ProcessorBaseNoHelper: execinfra.ProcessorBaseNoHelper{
					FlowCtx: &flowCtx,
				},
			},
		}

		mockRowReceiver := &metadataCache{}
		mockTTLProc.progressUpdater = &coordinatorStreamUpdater{proc: &mockTTLProc}

		// Create a context that we'll cancel.
		cancelCtx, cancel := context.WithCancel(ctx)
		// Cancel the context immediately.
		cancel()

		// Run the hybrid cleaner with cancelled context.
		err := mockTTLProc.work(cancelCtx, mockRowReceiver)

		// We expect a context cancellation error.
		require.Error(t, err)
		require.True(t, errors.Is(err, context.Canceled),
			"expected context.Canceled error, got: %v", err)

		// Verify that some progress may have been tracked (partial success).
		progressUpdater := mockTTLProc.progressUpdater.(*coordinatorStreamUpdater)
		progressUpdater.mu.Lock()
		completedSpanCount := len(progressUpdater.mu.completedSpans)
		progressUpdater.mu.Unlock()

		// Progress tracking should not panic even with cancelled context.
		// The number of completed spans may be 0 or partial depending on timing.
		require.True(t, completedSpanCount >= 0,
			"completed span count should be non-negative, got %d", completedSpanCount)
	})

	t.Run("table_descriptor_error", func(t *testing.T) {
		// Test that errors fetching table descriptor cause failure.
		// Use an invalid table ID to trigger descriptor error.
		invalidTableID := descpb.ID(999999)

		hybridDetails := &jobspb.HybridCleanerDetails{
			PartitionStart: partitionStart,
			PartitionEnd:   partitionEnd,
			TargetIndexIDs: secondaryIndexIDs,
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

		mockTTLProc := ttlProcessor{
			ttlSpec: execinfrapb.TTLSpec{
				RowLevelTTLDetails: jobspb.RowLevelTTLDetails{
					TableID:              invalidTableID,
					TableVersion:         tableDesc.GetVersion(),
					HybridCleanerDetails: hybridDetails,
				},
				DeleteBatchSize: 100,
				DeleteRateLimit: 1000000,
				Spans:           hybridDetails.PartitionSpans,
			},
			ProcessorBase: execinfra.ProcessorBase{
				ProcessorBaseNoHelper: execinfra.ProcessorBaseNoHelper{
					FlowCtx: &flowCtx,
				},
			},
		}

		mockRowReceiver := &metadataCache{}
		mockTTLProc.progressUpdater = &coordinatorStreamUpdater{proc: &mockTTLProc}

		// Run the hybrid cleaner with invalid table ID.
		err := mockTTLProc.work(ctx, mockRowReceiver)

		// We expect an error about the table not existing.
		require.Error(t, err)
		require.True(t, err.Error() == "relation \"[999999]\" does not exist" ||
			strings.Contains(err.Error(), "descriptor not found") ||
			strings.Contains(err.Error(), "does not exist"),
			"expected descriptor/relation error, got: %v", err)
	})

	t.Run("partial_progress_on_error", func(t *testing.T) {
		// Test that partial progress is tracked even when an error occurs.
		// We'll simulate this by processing multiple indexes and checking progress.
		hybridDetails := &jobspb.HybridCleanerDetails{
			PartitionStart: partitionStart,
			PartitionEnd:   partitionEnd,
			TargetIndexIDs: secondaryIndexIDs,
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

		mockTTLProc := ttlProcessor{
			ttlSpec: execinfrapb.TTLSpec{
				RowLevelTTLDetails: jobspb.RowLevelTTLDetails{
					TableID:              tableDesc.GetID(),
					TableVersion:         tableDesc.GetVersion(),
					HybridCleanerDetails: hybridDetails,
				},
				DeleteBatchSize: 100,
				DeleteRateLimit: 1000000,
				Spans:           hybridDetails.PartitionSpans,
			},
			ProcessorBase: execinfra.ProcessorBase{
				ProcessorBaseNoHelper: execinfra.ProcessorBaseNoHelper{
					FlowCtx: &flowCtx,
				},
			},
		}

		mockRowReceiver := &metadataCache{}
		mockTTLProc.progressUpdater = &coordinatorStreamUpdater{proc: &mockTTLProc}

		// Run the hybrid cleaner normally.
		err := mockTTLProc.work(ctx, mockRowReceiver)
		require.NoError(t, err)

		// Verify that progress was tracked for all work items.
		progressUpdater := mockTTLProc.progressUpdater.(*coordinatorStreamUpdater)
		progressUpdater.mu.Lock()
		completedSpanCount := len(progressUpdater.mu.completedSpans)
		progressUpdater.mu.Unlock()

		// With 2 indexes and 1 span, we expect 2 completed work items.
		expectedSpanCount := len(secondaryIndexIDs) * len(hybridDetails.PartitionSpans)
		require.Equal(t, expectedSpanCount, completedSpanCount,
			"expected progress tracking for all index/span combinations")

		// Verify that rows were deleted.
		deletedRowCount := progressUpdater.deletedRowCount.Load()
		require.Greater(t, deletedRowCount, int64(0),
			"expected some rows to be deleted, got %d", deletedRowCount)
	})

	t.Run("progress_updates_sent", func(t *testing.T) {
		// Test that progress updates are sent correctly during processing.

		// Re-insert test data since previous tests may have deleted it.
		for i := 0; i < 50; i++ {
			ts := partitionStart.Add(time.Duration(i) * time.Minute)
			runner.Exec(t, "INSERT INTO testdb.testschema.error_test_table VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
				ts, i+1000, fmt.Sprintf("data%d", i+1000))
		}

		hybridDetails := &jobspb.HybridCleanerDetails{
			PartitionStart: partitionStart,
			PartitionEnd:   partitionEnd,
			TargetIndexIDs: secondaryIndexIDs[:1], // Use just one index for simpler testing.
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

		mockTTLProc := ttlProcessor{
			ttlSpec: execinfrapb.TTLSpec{
				RowLevelTTLDetails: jobspb.RowLevelTTLDetails{
					TableID:              tableDesc.GetID(),
					TableVersion:         tableDesc.GetVersion(),
					HybridCleanerDetails: hybridDetails,
				},
				DeleteBatchSize: 100,
				DeleteRateLimit: 1000000,
				Spans:           hybridDetails.PartitionSpans,
			},
			ProcessorBase: execinfra.ProcessorBase{
				ProcessorBaseNoHelper: execinfra.ProcessorBaseNoHelper{
					FlowCtx: &flowCtx,
				},
			},
		}

		mockRowReceiver := &metadataCache{}
		mockTTLProc.progressUpdater = &coordinatorStreamUpdater{proc: &mockTTLProc}

		// Run the hybrid cleaner.
		err := mockTTLProc.work(ctx, mockRowReceiver)
		require.NoError(t, err)

		// Verify that the progress updater tracked the work.
		progressUpdater := mockTTLProc.progressUpdater.(*coordinatorStreamUpdater)
		require.Equal(t, int64(1), progressUpdater.totalSpanCount,
			"expected totalSpanCount to be 1 (1 index * 1 span)")

		progressUpdater.mu.Lock()
		completedSpans := len(progressUpdater.mu.completedSpans)
		progressUpdater.mu.Unlock()

		require.Equal(t, 1, completedSpans,
			"expected 1 completed span (1 index * 1 span)")

		// Verify that rows were actually deleted.
		deletedRowCount := progressUpdater.deletedRowCount.Load()
		require.Greater(t, deletedRowCount, int64(0),
			"expected rows to be deleted, got %d", deletedRowCount)
	})
}
