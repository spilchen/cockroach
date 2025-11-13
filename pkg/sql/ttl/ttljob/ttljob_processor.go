// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttljob

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/execversion"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/lexbase"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/spanutils"
	"github.com/cockroachdb/cockroach/pkg/sql/ttl/ttlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/admission/admissionpb"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	pbtypes "github.com/gogo/protobuf/types"
)

// ttlMaxKVAutoRetry is the maximum number of times a TTL operation will
// automatically retry in the KV layer before reducing the batch size to handle
// contention.
var ttlMaxKVAutoRetry = settings.RegisterIntSetting(
	settings.ApplicationLevel,
	"sql.ttl.max_kv_auto_retries",
	"the number of times a TTL operation will automatically retry in the KV layer before reducing the batch size",
	10,
	settings.PositiveInt,
)

// ttlProcessor manages the work managed by a single node for a job run by
// rowLevelTTLResumer. SpanToQueryBounds converts a DistSQL span into
// QueryBounds. The QueryBounds are passed to SelectQueryBuilder and
// DeleteQueryBuilder which manage the state for the SELECT/DELETE loop
// that is run by runTTLOnQueryBounds.
type ttlProcessor struct {
	execinfra.ProcessorBase
	ttlSpec              execinfrapb.TTLSpec
	processorConcurrency int64
	progressUpdater      ttlProgressUpdater
}

var _ execinfra.RowSource = (*ttlProcessor)(nil)
var _ execinfra.Processor = (*ttlProcessor)(nil)

// ttlProgressUpdater abstracts how a TTL processor reports its progress.
// Implementations can either write directly to the job table (legacy) or stream
// metadata back to the coordinator (preferred).
type ttlProgressUpdater interface {
	// InitProgress is called once at the beginning of the TTL processor.
	InitProgress(totalSpanCount int64)
	// UpdateProgress is called to refresh the TTL processor progress.
	UpdateProgress(ctx context.Context, output execinfra.RowReceiver) error
	// OnSpanProcessed is called each time a span has been processed (even partially).
	OnSpanProcessed(span roachpb.Span, deletedRowCount int64)
	// FinalizeProgress is the final call to update the progress once all spans have been processed.
	FinalizeProgress(ctx context.Context, output execinfra.RowReceiver) error
}

// directJobProgressUpdater handles TTL progress updates by writing directly to
// the jobs table from this processor. This is the legacy model and exists to
// support mixed-version scenarios.
//
// This can be removed once version 25.4 is the minimum supported version.
type directJobProgressUpdater struct {
	// proc references the running TTL processor.
	proc *ttlProcessor

	// updateEvery is the number of spans that must be processed before triggering a progress update.
	updateEvery int64

	// updateEveryDuration is the minimum amount of time that must pass between progress updates.
	updateEveryDuration time.Duration

	// lastUpdated records the time of the last progress update.
	lastUpdated time.Time

	// totalSpanCount is the total number of spans assigned to this processor.
	totalSpanCount int64

	// rowsProcessed is the cumulative number of rows deleted.
	rowsProcessed atomic.Int64

	// rowsProcessedSinceLastUpdate is the number of rows deleted since the last progress update.
	rowsProcessedSinceLastUpdate atomic.Int64

	// spansProcessed is the cumulative number of spans processed.
	spansProcessed atomic.Int64

	// spansProcessedSinceLastUpdate is the number of spans processed since the last progress update.
	spansProcessedSinceLastUpdate atomic.Int64
}

// coordinatorStreamUpdater handles TTL progress updates by flowing the
// information back to the coordinator. The coordinator is then responsible for
// writing that back to the jobs table.
type coordinatorStreamUpdater struct {
	// proc references the running TTL processor.
	proc *ttlProcessor

	// totalSpanCount is the total number of spans assigned to this processor.
	totalSpanCount int64

	// deletedRowCount tracks the cumulative number of rows deleted by this processor.
	deletedRowCount atomic.Int64

	// progressLogger is used to control how often we log progress updates that
	// are sent back to the coordinator.
	progressLogger log.EveryN

	mu struct {
		syncutil.Mutex
		// completedSpans tracks all spans that have been processed by this
		// invocation of the processor.
		completedSpans []roachpb.Span
		// completedSpansSinceLastUpdate tracks all spans that have been processed since
		// the last progress message was sent back to the coordinator.
		completedSpansSinceLastUpdate []roachpb.Span
	}
}

// Start implements the execinfra.RowSource interface.
func (t *ttlProcessor) Start(context.Context) {}

// Run implements the execinfra.Processor interface.
func (t *ttlProcessor) Run(ctx context.Context, output execinfra.RowReceiver) {
	ctx = t.StartInternal(ctx, "ttl")
	v := execversion.FromContext(ctx)
	// TTL processors support two progress update models. The legacy model (used in V25_2)
	// has each processor write progress directly to the job table. The newer model flows
	// progress metadata back to the coordinator, which handles the job table updates centrally.
	// The selected behavior is gated on the active cluster version.
	// TODO(spilchen): remove directJobProgerssUpdater once 25.4 is the minimum supported version.
	if v == execversion.V25_2 {
		t.progressUpdater = &directJobProgressUpdater{proc: t}
	} else {
		t.progressUpdater = &coordinatorStreamUpdater{proc: t, progressLogger: log.Every(1 * time.Minute)}
	}
	err := t.work(ctx, output)
	if err != nil {
		output.Push(nil, &execinfrapb.ProducerMetadata{Err: err})
	}
	execinfra.SendTraceData(ctx, t.FlowCtx, output)
	output.ProducerDone()
}

func getTableInfo(
	ctx context.Context, db descs.DB, tableID descpb.ID,
) (
	relationName string,
	pkColIDs catalog.TableColMap,
	pkColNames []string,
	pkColTypes []*types.T,
	pkColDirs []catenumpb.IndexColumn_Direction,
	numFamilies int,
	labelMetrics bool,
	err error,
) {
	err = db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		desc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, tableID)
		if err != nil {
			return err
		}

		numFamilies = desc.NumFamilies()
		var buf bytes.Buffer
		primaryIndexDesc := desc.GetPrimaryIndex().IndexDesc()
		pkColNames = make([]string, 0, len(primaryIndexDesc.KeyColumnNames))
		for _, name := range primaryIndexDesc.KeyColumnNames {
			lexbase.EncodeRestrictedSQLIdent(&buf, name, lexbase.EncNoFlags)
			pkColNames = append(pkColNames, buf.String())
			buf.Reset()
		}
		pkColTypes, err = spanutils.GetPKColumnTypes(desc, primaryIndexDesc)
		if err != nil {
			return err
		}
		pkColDirs = primaryIndexDesc.KeyColumnDirections
		pkColIDs = catalog.TableColMap{}
		for i, id := range primaryIndexDesc.KeyColumnIDs {
			pkColIDs.Set(id, i)
		}

		// Check for either row-level TTL or partition TTL.
		hasRowLevelTTL := desc.HasRowLevelTTL()
		hasPartitionTTL := desc.GetPartitionTTL() != nil

		if !hasRowLevelTTL && !hasPartitionTTL {
			return errors.Newf("unable to find TTL on table %s", desc.GetName())
		}

		// labelMetrics is only available in row-level TTL config.
		// For partition TTL, we don't track per-row metrics since deletions
		// happen at the partition level.
		if hasRowLevelTTL {
			rowLevelTTL := desc.GetRowLevelTTL()
			labelMetrics = rowLevelTTL.LabelMetrics
		}

		tn, err := descs.GetObjectName(ctx, txn.KV(), txn.Descriptors(), desc)
		if err != nil {
			return errors.Wrapf(err, "error fetching table relation name for TTL")
		}

		relationName = tn.FQString() + "@" + lexbase.EscapeSQLIdent(primaryIndexDesc.Name)
		return nil
	})
	return relationName, pkColIDs, pkColNames, pkColTypes, pkColDirs, numFamilies, labelMetrics, err
}

func (t *ttlProcessor) work(ctx context.Context, output execinfra.RowReceiver) error {
	ttlSpec := t.ttlSpec
	flowCtx := t.FlowCtx
	serverCfg := flowCtx.Cfg
	db := serverCfg.DB
	codec := serverCfg.Codec
	details := ttlSpec.RowLevelTTLDetails
	tableID := details.TableID
	cutoff := details.Cutoff
	ttlExpr := ttlSpec.TTLExpr

	// Hybrid cleaner mode: skip standard TTL processing and clean secondary indexes.
	if details.HybridCleanerDetails != nil {
		log.Dev.Infof(ctx, "TTL processor started in hybrid cleaner mode processorID=%d tableID=%d", t.ProcessorID, tableID)
		return t.workHybridCleaner(ctx, output)
	}

	// Note: the ttl-restart test depends on this message to know what nodes are
	// involved in a TTL job.
	log.Dev.Infof(ctx, "TTL processor started processorID=%d tableID=%d", t.ProcessorID, tableID)

	// Each node sets up two rate limiters (one for SELECT, one for DELETE) per
	// table. The limiters apply to all ranges assigned to this processor, whether
	// or not the node is the leaseholder for those ranges.

	selectRateLimit := ttlSpec.SelectRateLimit
	// Default 0 value to "unlimited" in case job started on node <= v23.2.
	// todo(sql-foundations): Remove this in 25.1 for consistency with
	//  deleteRateLimit.
	if selectRateLimit == 0 {
		selectRateLimit = math.MaxInt64
	}
	selectRateLimiter := quotapool.NewRateLimiter(
		"ttl-select",
		quotapool.Limit(selectRateLimit),
		selectRateLimit,
	)

	deleteRateLimit := ttlSpec.DeleteRateLimit
	deleteRateLimiter := quotapool.NewRateLimiter(
		"ttl-delete",
		quotapool.Limit(deleteRateLimit),
		deleteRateLimit,
	)

	relationName, pkColIDs, pkColNames, pkColTypes, pkColDirs, numFamilies, labelMetrics, err := getTableInfo(
		ctx, db, tableID,
	)
	if err != nil {
		return err
	}

	jobRegistry := serverCfg.JobRegistry
	metrics := jobRegistry.MetricsStruct().RowLevelTTL.(*RowLevelTTLAggMetrics).loadMetrics(
		labelMetrics,
		relationName,
	)

	group := ctxgroup.WithContext(ctx)
	totalSpanCount := int64(len(ttlSpec.Spans))
	t.progressUpdater.InitProgress(totalSpanCount)
	t.processorConcurrency = ttlbase.GetProcessorConcurrency(&flowCtx.Cfg.Settings.SV, int64(runtime.GOMAXPROCS(0)))
	if totalSpanCount < t.processorConcurrency {
		t.processorConcurrency = totalSpanCount
	}

	err = func() error {
		boundsChan := make(chan spanutils.QueryBounds, t.processorConcurrency)
		defer close(boundsChan)
		for i := int64(0); i < t.processorConcurrency; i++ {
			group.GoCtx(func(ctx context.Context) error {
				for bounds := range boundsChan {
					start := timeutil.Now()
					selectBuilder := MakeSelectQueryBuilder(
						SelectQueryParams{
							RelationName:      relationName,
							PKColNames:        pkColNames,
							PKColDirs:         pkColDirs,
							PKColTypes:        pkColTypes,
							Bounds:            bounds,
							AOSTDuration:      ttlSpec.AOSTDuration,
							SelectBatchSize:   ttlSpec.SelectBatchSize,
							TTLExpr:           ttlExpr,
							SelectDuration:    metrics.SelectDuration,
							SelectRateLimiter: selectRateLimiter,
						},
						cutoff,
					)
					deleteBuilder := MakeDeleteQueryBuilder(
						DeleteQueryParams{
							RelationName:      relationName,
							PKColNames:        pkColNames,
							DeleteBatchSize:   ttlSpec.DeleteBatchSize,
							TTLExpr:           ttlExpr,
							DeleteDuration:    metrics.DeleteDuration,
							DeleteRateLimiter: deleteRateLimiter,
						},
						cutoff,
					)
					spanDeletedRowCount, err := t.runTTLOnQueryBounds(
						ctx,
						metrics,
						selectBuilder,
						deleteBuilder,
					)
					// Add to totals even on partial success.
					t.progressUpdater.OnSpanProcessed(bounds.Span, spanDeletedRowCount)
					if err != nil {
						// Continue until channel is fully read.
						// Otherwise, the keys input will be blocked.
						for bounds = range boundsChan {
						}
						return err
					}
					metrics.SpanTotalDuration.RecordValue(int64(timeutil.Since(start)))
				}
				return nil
			})
		}

		// Iterate over every span to feed work for the goroutine processors.
		kvDB := db.KV()
		var alloc tree.DatumAlloc
		for i, span := range ttlSpec.Spans {
			if bounds, hasRows, err := spanutils.SpanToQueryBounds(
				ctx,
				kvDB,
				codec,
				pkColIDs,
				pkColTypes,
				pkColDirs,
				numFamilies,
				span,
				&alloc,
				hlc.Timestamp{}, // Use current time bounds; TTL only deletes live rows.
			); err != nil {
				return errors.Wrapf(err, "SpanToQueryBounds error index=%d span=%s", i, span)
			} else if hasRows {
				// Only process bounds from spans with rows inside them.
				boundsChan <- bounds
			} else {
				// If the span has no rows, we still need to increment the processed
				// count.
				t.progressUpdater.OnSpanProcessed(span, 0 /* deletedRowCount */)
			}

			if err := t.progressUpdater.UpdateProgress(ctx, output); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		return err
	}

	if err := group.Wait(); err != nil {
		return err
	}
	return t.progressUpdater.FinalizeProgress(ctx, output)
}

// hybridCleanerWorkItem represents a unit of work for the hybrid cleaner:
// a (secondary index, PK span) pair to process.
type hybridCleanerWorkItem struct {
	indexID descpb.IndexID
	span    roachpb.Span
}

// workHybridCleaner implements the hybrid cleaner mode for partition TTL.
// This mode skips the SELECT phase and deletes directly from secondary indexes
// based on partition time bounds.
func (t *ttlProcessor) workHybridCleaner(ctx context.Context, output execinfra.RowReceiver) error {
	ttlSpec := t.ttlSpec
	flowCtx := t.FlowCtx
	serverCfg := flowCtx.Cfg
	db := serverCfg.DB
	details := ttlSpec.RowLevelTTLDetails
	tableID := details.TableID
	hybridDetails := details.HybridCleanerDetails

	// Get table info for metrics and relation name.
	relationName, _, _, _, _, _, labelMetrics, err := getTableInfo(ctx, db, tableID)
	if err != nil {
		return err
	}

	jobRegistry := serverCfg.JobRegistry
	metrics := jobRegistry.MetricsStruct().RowLevelTTL.(*RowLevelTTLAggMetrics).loadMetrics(
		labelMetrics,
		relationName,
	)

	// Setup delete rate limiter.
	deleteRateLimit := ttlSpec.DeleteRateLimit
	deleteRateLimiter := quotapool.NewRateLimiter(
		"ttl-delete",
		quotapool.Limit(deleteRateLimit),
		deleteRateLimit,
	)

	// Calculate total work: number of (index, span) pairs.
	// Progress tracking is per span, just like row-level TTL.
	totalSpanCount := int64(len(ttlSpec.Spans) * len(hybridDetails.TargetIndexIDs))
	t.progressUpdater.InitProgress(totalSpanCount)
	t.processorConcurrency = ttlbase.GetProcessorConcurrency(&flowCtx.Cfg.Settings.SV, int64(runtime.GOMAXPROCS(0)))
	if totalSpanCount < t.processorConcurrency {
		t.processorConcurrency = totalSpanCount
	}

	group := ctxgroup.WithContext(ctx)
	workChan := make(chan hybridCleanerWorkItem, t.processorConcurrency)

	// Launch worker pool with fixed concurrency.
	// Workers process (index, span) pairs from the work channel.
	for i := int64(0); i < t.processorConcurrency; i++ {
		group.GoCtx(func(ctx context.Context) error {
			for work := range workChan {
				start := timeutil.Now()
				deletedRowCount, err := t.runHybridCleanerOnSpan(
					ctx,
					metrics,
					deleteRateLimiter,
					work.indexID,
					work.span,
				)
				// Add to totals even on partial success.
				// Progress is tracked per span, matching row-level TTL behavior.
				t.progressUpdater.OnSpanProcessed(work.span, deletedRowCount)
				if err != nil {
					// Continue until channel is fully read.
					for range workChan {
					}
					return err
				}
				metrics.SpanTotalDuration.RecordValue(int64(timeutil.Since(start)))
			}
			return nil
		})
	}

	// Feed all (index, span) combinations to the worker pool.
	// This distributes work evenly across all indexes and spans.
	err = func() error {
		defer close(workChan)
		for _, indexID := range hybridDetails.TargetIndexIDs {
			for _, span := range ttlSpec.Spans {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case workChan <- hybridCleanerWorkItem{indexID: indexID, span: span}:
					// Progress updates are sent periodically.
					if err := t.progressUpdater.UpdateProgress(ctx, output); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}()
	if err != nil {
		return err
	}

	if err := group.Wait(); err != nil {
		return err
	}
	return t.progressUpdater.FinalizeProgress(ctx, output)
}

// runHybridCleanerOnSpan deletes entries from a secondary index within a PK span
// that fall within the partition time bounds.
func (t *ttlProcessor) runHybridCleanerOnSpan(
	ctx context.Context,
	metrics rowLevelTTLMetrics,
	deleteRateLimiter *quotapool.RateLimiter,
	indexID descpb.IndexID,
	span roachpb.Span,
) (deletedRowCount int64, err error) {
	metrics.NumActiveSpans.Inc(1)
	defer metrics.NumActiveSpans.Dec(1)

	ttlSpec := t.ttlSpec
	details := ttlSpec.RowLevelTTLDetails
	hybridDetails := details.HybridCleanerDetails
	flowCtx := t.FlowCtx
	serverCfg := flowCtx.Cfg
	ie := serverCfg.DB.Executor()
	settingsValues := &serverCfg.Settings.SV

	// Get table metadata and convert span to query bounds.
	relationName, pkColIDs, pkColNames, pkColTypes, pkColDirs, numFamilies, _, err := getTableInfo(
		ctx, serverCfg.DB, details.TableID,
	)
	if err != nil {
		return deletedRowCount, err
	}

	// Strip the primary index hint from relationName since we'll add our own for the secondary index.
	// getTableInfo returns "schema.table@primaryIndex", but we need "schema.table" only.
	if atPos := bytes.IndexByte([]byte(relationName), '@'); atPos != -1 {
		relationName = relationName[:atPos]
	}

	// Get the TTL column name and index name.
	var ttlColName string
	var indexName string
	if err := serverCfg.DB.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		desc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, details.TableID)
		if err != nil {
			return err
		}
		// Get the TTL column from partition TTL config.
		partitionTTL := desc.GetPartitionTTL()
		if partitionTTL == nil || partitionTTL.ColumnName == "" {
			return errors.Newf("partition TTL config not found or missing column name on table %s", desc.GetName())
		}
		ttlColName = partitionTTL.ColumnName

		// Get the index name.
		idx, err := catalog.MustFindIndexByID(desc, indexID)
		if err != nil {
			return errors.Wrapf(err, "error finding index %d", indexID)
		}
		indexName = idx.GetName()
		return nil
	}); err != nil {
		return deletedRowCount, err
	}

	// Convert the PK span to query bounds.
	// For testing with placeholder spans, this may fail, so we handle errors gracefully.
	kvDB := serverCfg.DB.KV()
	codec := serverCfg.Codec
	var alloc tree.DatumAlloc
	var bounds spanutils.QueryBounds
	var usePKBounds bool

	queryBounds, hasRows, err := spanutils.SpanToQueryBounds(
		ctx,
		kvDB,
		codec,
		pkColIDs,
		pkColTypes,
		pkColDirs,
		numFamilies,
		span,
		&alloc,
		hlc.Timestamp{}, // Use current time bounds.
	)
	if err != nil {
		return deletedRowCount, errors.Wrapf(err, "SpanToQueryBounds error for span=%s", span)
	}
	if !hasRows {
		// Span is empty, nothing to delete.
		return 0, nil
	}
	bounds = queryBounds
	usePKBounds = true

	// Build the DELETE query for the secondary index.
	// The query targets the secondary index and filters by:
	// 1. Partition time bounds: ttl_col >= $partitionStart AND ttl_col < $partitionEnd
	// 2. PK bounds: pk_col1 >= $start1 AND pk_col1 <= $end1 AND ...
	// The LIMIT ensures we process in batches.
	deleteBatchSize := ttlSpec.DeleteBatchSize

	// Build the WHERE clause with both time and PK bounds.
	var whereClause bytes.Buffer
	var queryArgs []interface{}

	// Add partition time bounds.
	whereClause.WriteString(lexbase.EscapeSQLIdent(ttlColName))
	fmt.Fprintf(&whereClause, " >= $%d", len(queryArgs)+1)
	queryArgs = append(queryArgs, hybridDetails.PartitionStart)

	whereClause.WriteString(" AND ")
	whereClause.WriteString(lexbase.EscapeSQLIdent(ttlColName))
	fmt.Fprintf(&whereClause, " < $%d", len(queryArgs)+1)
	queryArgs = append(queryArgs, hybridDetails.PartitionEnd)

	// Add PK bounds from the span (if available).
	if usePKBounds {
		for i, colName := range pkColNames {
			if bounds.Start != nil && i < len(bounds.Start) && bounds.Start[i] != tree.DNull {
				whereClause.WriteString(" AND ")
				whereClause.WriteString(colName)
				fmt.Fprintf(&whereClause, " >= $%d", len(queryArgs)+1)
				queryArgs = append(queryArgs, bounds.Start[i])
			}
			if bounds.End != nil && i < len(bounds.End) && bounds.End[i] != tree.DNull {
				whereClause.WriteString(" AND ")
				whereClause.WriteString(colName)
				fmt.Fprintf(&whereClause, " <= $%d", len(queryArgs)+1)
				queryArgs = append(queryArgs, bounds.End[i])
			}
		}
	}

	var buf bytes.Buffer
	buf.WriteString("DELETE FROM ")
	buf.WriteString(relationName)
	buf.WriteString("@")
	lexbase.EncodeRestrictedSQLIdent(&buf, indexName, lexbase.EncNoFlags)
	buf.WriteString(" WHERE ")
	buf.WriteString(whereClause.String())
	buf.WriteString(" LIMIT ")
	buf.WriteString(fmt.Sprintf("%d", deleteBatchSize))
	deleteQuery := buf.String()

	for {
		// Check if we should stop.
		select {
		case <-ctx.Done():
			return deletedRowCount, ctx.Err()
		default:
		}

		// Execute the DELETE query.
		rowsDeleted, err := ie.ExecEx(
			ctx,
			"hybrid-cleaner-delete",
			nil, /* txn */
			sessiondata.NodeUserSessionDataOverride,
			deleteQuery,
			queryArgs...,
		)
		if err != nil {
			return deletedRowCount, errors.Wrapf(err, "error deleting from secondary index")
		}

		deleted := int64(rowsDeleted)
		if deleted == 0 {
			// No more rows to delete.
			break
		}

		deletedRowCount += deleted
		metrics.RowDeletions.Inc(deleted)

		// Apply rate limiting.
		if err := deleteRateLimiter.WaitN(ctx, deleted); err != nil {
			return deletedRowCount, err
		}

		// Respect the delete rate limit setting changes.
		var rowLevelTTL *catpb.RowLevelTTL
		if err := serverCfg.DB.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
			desc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, details.TableID)
			if err != nil {
				return err
			}
			rowLevelTTL = desc.GetRowLevelTTL()
			return nil
		}); err != nil {
			return deletedRowCount, err
		}
		deleteRateLimit := ttlbase.GetDeleteRateLimit(settingsValues, rowLevelTTL)
		deleteRateLimiter.UpdateLimit(quotapool.Limit(deleteRateLimit), deleteRateLimit)
	}

	return deletedRowCount, nil
}

// runTTLOnQueryBounds runs the SELECT/DELETE loop for a single DistSQL span.
// spanRowCount should be checked even if the function returns an error
// because it may have partially succeeded.
func (t *ttlProcessor) runTTLOnQueryBounds(
	ctx context.Context,
	metrics rowLevelTTLMetrics,
	selectBuilder SelectQueryBuilder,
	deleteBuilder DeleteQueryBuilder,
) (spanRowCount int64, err error) {
	metrics.NumActiveSpans.Inc(1)
	defer metrics.NumActiveSpans.Dec(1)

	// TODO(#82140): investigate improving row deletion performance with secondary indexes

	ttlSpec := t.ttlSpec
	details := ttlSpec.RowLevelTTLDetails

	// TODO(SPILLY): Hybrid cleaner mode deletion
	// if details.HybridCleanerMode {
	//   - Skip SELECT phase entirely (no expiration check needed)
	//   - Directly delete all rows in the secondary index span within partition bounds
	//   - Use deleteBuilder.Run() with all keys in the span, not selected PKs
	//   - Return early after deletion completes
	// }

	flowCtx := t.FlowCtx
	serverCfg := flowCtx.Cfg
	ie := serverCfg.DB.Executor()

	preSelectStatement := ttlSpec.PreSelectStatement
	if preSelectStatement != "" {
		if _, err := ie.ExecEx(
			ctx,
			"pre-select-delete-statement",
			nil, /* txn */
			// This is a test-only knob, so we're ok not specifying custom
			// InternalExecutorOverride.
			sessiondata.NodeUserSessionDataOverride,
			preSelectStatement,
		); err != nil {
			return spanRowCount, err
		}
	}

	settingsValues := &serverCfg.Settings.SV
	for {
		// Check the job is enabled on every iteration.
		if err := ttlbase.CheckJobEnabled(settingsValues); err != nil {
			return spanRowCount, err
		}

		// Step 1. Fetch some rows we want to delete using a historical
		// SELECT query.
		expiredRowsPKs, hasNext, err := selectBuilder.Run(ctx, ie)
		if err != nil {
			return spanRowCount, errors.Wrapf(err, "error selecting rows to delete")
		}

		numExpiredRows := len(expiredRowsPKs)
		metrics.RowSelections.Inc(int64(numExpiredRows))

		// Step 2. Delete the rows which have expired.
		deleteBatchSize := deleteBuilder.GetBatchSize()
		for startRowIdx := 0; startRowIdx < numExpiredRows; startRowIdx += deleteBatchSize {
			// We are going to attempt a delete of size deleteBatchSize. But we use
			// retry.Batch to allow retrying with a smaller batch size in case of
			// an error.
			rb := retry.Batch{
				Do: func(ctx context.Context, processed, batchSize int) error {
					until := startRowIdx + processed + batchSize
					if until > numExpiredRows {
						until = numExpiredRows
					}
					deleteBatch := expiredRowsPKs[startRowIdx+processed : until]
					var batchRowCount int64
					do := func(ctx context.Context, txn descs.Txn) error {
						txn.KV().SetDebugName("ttljob-delete-batch")
						// We explicitly specify a low retry limit because this operation is
						// wrapped with its own retry function that will also take care of
						// adjusting the batch size on each retry.
						maxAutoRetries := ttlMaxKVAutoRetry.Get(&flowCtx.Cfg.Settings.SV)
						txn.KV().SetMaxAutoRetries(int(maxAutoRetries))
						if ttlSpec.DisableChangefeedReplication {
							txn.KV().SetOmitInRangefeeds()
						}
						// If we detected a schema change here, the DELETE will not succeed
						// (the SELECT still will because of the AOST). Early exit here.
						desc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, details.TableID)
						if err != nil {
							return err
						}
						if ttlSpec.PreDeleteChangeTableVersion || desc.GetVersion() != details.TableVersion {
							return errors.Newf(
								"table has had a schema change since the job has started at %s, job will run at the next scheduled time",
								desc.GetModificationTime().GoTime().Format(time.RFC3339),
							)
						}
						batchRowCount, err = deleteBuilder.Run(ctx, txn, deleteBatch)
						if err != nil {
							return err
						}
						return nil
					}
					if err := serverCfg.DB.DescsTxn(
						ctx, do, isql.SteppingEnabled(), isql.WithPriority(admissionpb.BulkLowPri),
					); err != nil {
						return errors.Wrapf(err, "error during row deletion")
					}
					metrics.RowDeletions.Inc(batchRowCount)
					spanRowCount += batchRowCount
					return nil
				},
				IsRetryableError: kv.IsAutoRetryLimitExhaustedError,
				OnRetry: func(err error, nextBatchSize int) error {
					metrics.NumDeleteBatchRetries.Inc(1)
					log.Dev.Infof(ctx,
						"row-level TTL reached the auto-retry limit, reducing batch size to %d rows. Error: %v",
						nextBatchSize, err)
					return nil
				},
			}
			// Adjust the batch size if we are on the final batch.
			deleteBatchSize = min(deleteBatchSize, numExpiredRows-startRowIdx)
			if err := rb.Execute(ctx, deleteBatchSize); err != nil {
				return spanRowCount, err
			}
		}

		// Step 3. Early exit if necessary.

		// If we selected less than the select batch size, we have selected every
		// row and so we end it here.
		if !hasNext {
			break
		}
	}

	return spanRowCount, nil
}

func (t *ttlProcessor) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	return nil, t.DrainHelper()
}

func newTTLProcessor(
	ctx context.Context, flowCtx *execinfra.FlowCtx, processorID int32, spec execinfrapb.TTLSpec,
) (execinfra.Processor, error) {
	ttlProcessor := &ttlProcessor{
		ttlSpec: spec,
	}
	if err := ttlProcessor.Init(
		ctx,
		ttlProcessor,
		&execinfrapb.PostProcessSpec{},
		[]*types.T{},
		flowCtx,
		processorID,
		nil, /* memMonitor */
		execinfra.ProcStateOpts{},
	); err != nil {
		return nil, err
	}
	return ttlProcessor, nil
}

// InitProgress implements the ttlProgressUpdater interface.
func (c *coordinatorStreamUpdater) InitProgress(totalSpanCount int64) {
	c.totalSpanCount = totalSpanCount
}

// OnSpanProcessed implements the ttlProgressUpdater interface.
func (c *coordinatorStreamUpdater) OnSpanProcessed(span roachpb.Span, deletedRowCount int64) {
	c.deletedRowCount.Add(deletedRowCount)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mu.completedSpansSinceLastUpdate = append(c.mu.completedSpansSinceLastUpdate, span)
	c.mu.completedSpans = append(c.mu.completedSpans, span)
}

// UpdateProgress implements the ttlProgressUpdater interface.
func (c *coordinatorStreamUpdater) UpdateProgress(
	ctx context.Context, output execinfra.RowReceiver,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	nodeID := c.proc.FlowCtx.NodeID.SQLInstanceID()
	progressMsg := &jobspb.RowLevelTTLProcessorProgress{
		ProcessorID:          c.proc.ProcessorID,
		SQLInstanceID:        nodeID,
		ProcessorConcurrency: c.proc.processorConcurrency,
		DeletedRowCount:      c.deletedRowCount.Load(),
		ProcessedSpanCount:   int64(len(c.mu.completedSpans)),
		TotalSpanCount:       c.totalSpanCount,
	}
	progressAny, err := pbtypes.MarshalAny(progressMsg)
	if err != nil {
		return errors.Wrap(err, "unable to marshal TTL processor progress")
	}
	meta := &execinfrapb.ProducerMetadata{
		BulkProcessorProgress: &execinfrapb.RemoteProducerMetadata_BulkProcessorProgress{
			// The checkpointProgressTracker will treat CompletedSpans in this message
			// to be the number of spans completed since the last message.
			CompletedSpans:  c.mu.completedSpansSinceLastUpdate,
			ProgressDetails: *progressAny,
			NodeID:          nodeID,
			FlowID:          c.proc.FlowCtx.ID,
			ProcessorID:     c.proc.ProcessorID,
			Drained:         int64(len(c.mu.completedSpans)) == c.totalSpanCount,
		},
	}
	// Push progress after each span. Throttling is now handled by the coordinator.
	status := output.Push(nil, meta)
	if status != execinfra.NeedMoreRows {
		return errors.Errorf("output receiver rejected progress metadata: %v", status)
	}
	if c.progressLogger.ShouldLog() {
		log.Dev.Infof(ctx, "TTL processor progress: %v", progressMsg)
	}
	c.mu.completedSpansSinceLastUpdate = c.mu.completedSpansSinceLastUpdate[:0]
	return nil
}

// FinalizeProgress implements the ttlProgressUpdater interface.
func (c *coordinatorStreamUpdater) FinalizeProgress(
	ctx context.Context, output execinfra.RowReceiver,
) error {
	return c.UpdateProgress(ctx, output)
}

// InitProgress implements the ttlProgressUpdater interface.
func (d *directJobProgressUpdater) InitProgress(totalSpanCount int64) {
	// Update progress for approximately every 1% of spans processed, at least
	// 60 seconds apart with jitter.
	d.totalSpanCount = totalSpanCount
	d.updateEvery = max(1, totalSpanCount/100)
	d.updateEveryDuration = 60*time.Second + time.Duration(rand.Int63n(10*1000))*time.Millisecond
	d.lastUpdated = timeutil.Now()
}

// OnSpanProcessed implements the ttlProgressUpdater interface.
func (d *directJobProgressUpdater) OnSpanProcessed(span roachpb.Span, deletedRowCount int64) {
	d.rowsProcessed.Add(deletedRowCount)
	d.rowsProcessedSinceLastUpdate.Add(deletedRowCount)
	d.spansProcessed.Add(1)
	d.spansProcessedSinceLastUpdate.Add(1)
}

func (d *directJobProgressUpdater) updateFractionCompleted(ctx context.Context) error {
	jobID := d.proc.ttlSpec.JobID
	d.lastUpdated = timeutil.Now()
	spansToAdd := d.spansProcessedSinceLastUpdate.Swap(0)
	rowsToAdd := d.rowsProcessedSinceLastUpdate.Swap(0)

	var deletedRowCount, processedSpanCount, totalSpanCount int64
	var fractionCompleted float32

	err := d.proc.FlowCtx.Cfg.JobRegistry.UpdateJobWithTxn(
		ctx,
		jobID,
		nil, /* txn */
		func(_ isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
			progress := md.Progress
			rowLevelTTL := progress.Details.(*jobspb.Progress_RowLevelTTL).RowLevelTTL
			rowLevelTTL.JobProcessedSpanCount += spansToAdd
			rowLevelTTL.JobDeletedRowCount += rowsToAdd
			deletedRowCount = rowLevelTTL.JobDeletedRowCount
			processedSpanCount = rowLevelTTL.JobProcessedSpanCount
			totalSpanCount = rowLevelTTL.JobTotalSpanCount

			fractionCompleted = float32(rowLevelTTL.JobProcessedSpanCount) / float32(rowLevelTTL.JobTotalSpanCount)
			progress.Progress = &jobspb.Progress_FractionCompleted{
				FractionCompleted: fractionCompleted,
			}

			ju.UpdateProgress(progress)
			return nil
		},
	)
	if err != nil {
		return err
	}
	log.Dev.Infof(
		ctx,
		"TTL fractionCompleted updated processorID=%d tableID=%d deletedRowCount=%d processedSpanCount=%d totalSpanCount=%d fractionCompleted=%.3f",
		d.proc.ProcessorID, d.proc.ttlSpec.RowLevelTTLDetails.TableID, deletedRowCount, processedSpanCount, totalSpanCount, fractionCompleted,
	)
	return nil
}

// UpdateProgress implements the ttlProgressUpdater interface.
func (d *directJobProgressUpdater) UpdateProgress(
	ctx context.Context, _ execinfra.RowReceiver,
) error {
	if d.spansProcessedSinceLastUpdate.Load() >= d.updateEvery &&
		timeutil.Since(d.lastUpdated) >= d.updateEveryDuration {
		if err := d.updateFractionCompleted(ctx); err != nil {
			return err
		}
	}
	return nil
}

// FinalizeProgress implements the ttlProgressUpdater interface.
func (d *directJobProgressUpdater) FinalizeProgress(
	ctx context.Context, _ execinfra.RowReceiver,
) error {
	if err := d.updateFractionCompleted(ctx); err != nil {
		return err
	}

	sqlInstanceID := d.proc.FlowCtx.NodeID.SQLInstanceID()
	jobID := d.proc.ttlSpec.JobID
	return d.proc.FlowCtx.Cfg.JobRegistry.UpdateJobWithTxn(
		ctx,
		jobID,
		nil, /* txn */
		func(_ isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
			progress := md.Progress
			rowLevelTTL := progress.Details.(*jobspb.Progress_RowLevelTTL).RowLevelTTL
			processorID := d.proc.ProcessorID
			rowLevelTTL.ProcessorProgresses = append(rowLevelTTL.ProcessorProgresses, jobspb.RowLevelTTLProcessorProgress{
				ProcessorID:          processorID,
				SQLInstanceID:        sqlInstanceID,
				DeletedRowCount:      d.rowsProcessed.Load(),
				ProcessedSpanCount:   d.spansProcessed.Load(),
				ProcessorConcurrency: d.proc.processorConcurrency,
			})
			var fractionCompleted float32
			if f, ok := progress.Progress.(*jobspb.Progress_FractionCompleted); ok {
				fractionCompleted = f.FractionCompleted
			}
			ju.UpdateProgress(progress)
			log.Dev.VInfof(
				ctx,
				2, /* level */
				"TTL processorRowCount updated processorID=%d sqlInstanceID=%d tableID=%d jobRowCount=%d processorRowCount=%d fractionCompleted=%.3f",
				processorID, sqlInstanceID, d.proc.ttlSpec.RowLevelTTLDetails.TableID, rowLevelTTL.JobDeletedRowCount,
				d.rowsProcessed.Load(), fractionCompleted,
			)
			return nil
		},
	)
}

func init() {
	rowexec.NewTTLProcessor = newTTLProcessor
}
