// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttljob

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/joberror"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/physicalplan"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqltelemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/ttl/ttlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	pbtypes "github.com/gogo/protobuf/types"
)

var replanThreshold = settings.RegisterFloatSetting(
	settings.ApplicationLevel,
	"sql.ttl.replan_flow_threshold",
	"the fraction of flow instances that must change (added or updated) before the TTL job is replanned; set to 0 to disable",
	0.1,
	settings.FloatInRange(0, 1),
)

var replanFrequency = settings.RegisterDurationSetting(
	settings.ApplicationLevel,
	"sql.ttl.replan_flow_frequency",
	"how frequently the TTL job checks whether to replan its physical execution flow",
	time.Minute*2,
	settings.PositiveDuration,
)

// rowLevelTTLResumer implements the TTL job. The job can run on any node, but
// the job node distributes SELECT/DELETE work via DistSQL to ttlProcessor
// nodes. DistSQL divides work into spans that each ttlProcessor scans in a
// SELECT/DELETE loop.
type rowLevelTTLResumer struct {
	job          *jobs.Job
	st           *cluster.Settings
	physicalPlan *sql.PhysicalPlan
	planCtx      *sql.PlanningCtx
}

var _ jobs.Resumer = (*rowLevelTTLResumer)(nil)

// Resume implements the jobs.Resumer interface.
func (t rowLevelTTLResumer) Resume(ctx context.Context, execCtx interface{}) (retErr error) {
	defer func() {
		if retErr == nil {
			return
		} else if joberror.IsPermanentBulkJobError(retErr) && !errors.Is(retErr, sql.ErrPlanChanged) {
			retErr = jobs.MarkAsPermanentJobError(retErr)
		} else {
			retErr = jobs.MarkAsRetryJobError(retErr)
		}
	}()

	jobExecCtx := execCtx.(sql.JobExecContext)
	execCfg := jobExecCtx.ExecCfg()
	db := execCfg.InternalDB

	settingsValues := execCfg.SV()
	if err := ttlbase.CheckJobEnabled(settingsValues); err != nil {
		return err
	}

	telemetry.Inc(sqltelemetry.RowLevelTTLExecuted)

	var knobs sql.TTLTestingKnobs
	if ttlKnobs := execCfg.TTLTestingKnobs; ttlKnobs != nil {
		knobs = *ttlKnobs
	}

	details := t.job.Details().(jobspb.RowLevelTTLDetails)

	aostDuration := ttlbase.DefaultAOSTDuration
	if knobs.AOSTDuration != nil {
		aostDuration = *knobs.AOSTDuration
	}

	var rowLevelTTL *catpb.RowLevelTTL
	var relationName string
	var entirePKSpan roachpb.Span
	if err := db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		desc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, details.TableID)
		if err != nil {
			return err
		}
		// If the AOST timestamp is before the latest descriptor timestamp, exit
		// early as the delete will not work.
		modificationTime := desc.GetModificationTime().GoTime()
		aost := details.Cutoff.Add(aostDuration)
		if modificationTime.After(aost) {
			return pgerror.Newf(
				pgcode.ObjectNotInPrerequisiteState,
				"found a recent schema change on the table at %s, job will run at the next scheduled time",
				modificationTime.Format(time.RFC3339),
			)
		}

		if !desc.HasRowLevelTTL() {
			return errors.Newf("unable to find TTL on table %s", desc.GetName())
		}

		rowLevelTTL = desc.GetRowLevelTTL()

		if rowLevelTTL.Pause {
			return pgerror.Newf(pgcode.OperatorIntervention, "ttl jobs on table %s are currently paused", tree.Name(desc.GetName()))
		}

		tn, err := descs.GetObjectName(ctx, txn.KV(), txn.Descriptors(), desc)
		if err != nil {
			return errors.Wrapf(err, "error fetching table relation name for TTL")
		}
		relationName = tn.FQString()

		entirePKSpan = desc.PrimaryIndexSpan(execCfg.Codec)
		return nil
	}); err != nil {
		return err
	}

	ttlExpr := rowLevelTTL.GetTTLExpr()

	labelMetrics := rowLevelTTL.LabelMetrics
	statsCtx, statsCancel := context.WithCancelCause(ctx)
	defer statsCancel(nil)
	statsGroup := ctxgroup.WithContext(statsCtx)
	if rowLevelTTL.RowStatsPollInterval != 0 {
		metrics := execCfg.JobRegistry.MetricsStruct().RowLevelTTL.(*RowLevelTTLAggMetrics).loadMetrics(
			labelMetrics,
			relationName,
		)

		statsGroup.GoCtx(func(ctx context.Context) error {
			// Do once initially to ensure we have some base statistics.
			if err := metrics.fetchStatistics(ctx, execCfg, relationName, details, aostDuration, ttlExpr); err != nil {
				return err
			}
			// Wait until poll interval is reached, or early exit when we are done
			// with the TTL job.
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(rowLevelTTL.RowStatsPollInterval):
					if err := metrics.fetchStatistics(ctx, execCfg, relationName, details, aostDuration, ttlExpr); err != nil {
						return err
					}
				}
			}
		})
	}

	distSQLPlanner := jobExecCtx.DistSQLPlanner()

	jobSpanCount := 0
	makePlan := func(ctx context.Context, distSQLPlanner *sql.DistSQLPlanner) (*sql.PhysicalPlan, *sql.PlanningCtx, error) {
		// We don't return the compatible nodes here since PartitionSpans will
		// filter out incompatible nodes.
		planCtx, _, err := distSQLPlanner.SetupAllNodesPlanning(ctx, jobExecCtx.ExtendedEvalContext(), execCfg)
		if err != nil {
			return nil, nil, err
		}
		spanPartitions, err := distSQLPlanner.PartitionSpans(ctx, planCtx, []roachpb.Span{entirePKSpan}, sql.PartitionSpansBoundDefault)
		if err != nil {
			return nil, nil, err
		}
		expectedNumSpanPartitions := knobs.ExpectedNumSpanPartitions
		if expectedNumSpanPartitions != 0 {
			actualNumSpanPartitions := len(spanPartitions)
			if expectedNumSpanPartitions != actualNumSpanPartitions {
				return nil, nil, errors.AssertionFailedf(
					"incorrect number of span partitions expected=%d actual=%d",
					expectedNumSpanPartitions, actualNumSpanPartitions,
				)
			}
		}

		jobID := t.job.ID()
		selectBatchSize := ttlbase.GetSelectBatchSize(settingsValues, rowLevelTTL)
		deleteBatchSize := ttlbase.GetDeleteBatchSize(settingsValues, rowLevelTTL)
		selectRateLimit := ttlbase.GetSelectRateLimit(settingsValues, rowLevelTTL)
		deleteRateLimit := ttlbase.GetDeleteRateLimit(settingsValues, rowLevelTTL)
		disableChangefeedReplication := ttlbase.GetChangefeedReplicationDisabled(settingsValues, rowLevelTTL)
		newTTLSpec := func(spans []roachpb.Span) *execinfrapb.TTLSpec {
			return &execinfrapb.TTLSpec{
				JobID:                        jobID,
				RowLevelTTLDetails:           details,
				TTLExpr:                      ttlExpr,
				Spans:                        spans,
				SelectBatchSize:              selectBatchSize,
				DeleteBatchSize:              deleteBatchSize,
				SelectRateLimit:              selectRateLimit,
				DeleteRateLimit:              deleteRateLimit,
				LabelMetrics:                 rowLevelTTL.LabelMetrics,
				PreDeleteChangeTableVersion:  knobs.PreDeleteChangeTableVersion,
				PreSelectStatement:           knobs.PreSelectStatement,
				AOSTDuration:                 aostDuration,
				DisableChangefeedReplication: disableChangefeedReplication,
			}
		}

		jobSpanCount = 0
		for _, spanPartition := range spanPartitions {
			jobSpanCount += len(spanPartition.Spans)
		}

		sqlInstanceIDToTTLSpec := make(map[base.SQLInstanceID]*execinfrapb.TTLSpec, len(spanPartitions))
		for _, spanPartition := range spanPartitions {
			sqlInstanceIDToTTLSpec[spanPartition.SQLInstanceID] = newTTLSpec(spanPartition.Spans)
		}

		// Setup a one-stage plan with one proc per input spec.
		processorCorePlacements := make([]physicalplan.ProcessorCorePlacement, len(sqlInstanceIDToTTLSpec))
		i := 0
		for sqlInstanceID, ttlSpec := range sqlInstanceIDToTTLSpec {
			processorCorePlacements[i].SQLInstanceID = sqlInstanceID
			processorCorePlacements[i].Core.Ttl = ttlSpec
			i++
		}

		physicalPlan := planCtx.NewPhysicalPlan()
		// Job progress is updated inside ttlProcessor, so we
		// have an empty result stream.
		physicalPlan.AddNoInputStage(
			processorCorePlacements,
			execinfrapb.PostProcessSpec{},
			[]*types.T{},
			execinfrapb.Ordering{},
			nil, /* finalizeLastStageCb */
		)
		physicalPlan.PlanToStreamColMap = []int{}

		sql.FinalizePlan(ctx, planCtx, physicalPlan)
		return physicalPlan, planCtx, nil
	}

	var err error
	t.physicalPlan, t.planCtx, err = makePlan(ctx, distSQLPlanner)
	if err != nil {
		return err
	}

	updater := ttlJobProgressUpdater{job: t.job}
	if err := t.initProgress(ctx, updater, int64(jobSpanCount)); err != nil {
		return err
	}

	metadataCallbackWriter := sql.NewMetadataOnlyMetadataCallbackWriter(
		func(ctx context.Context, meta *execinfrapb.ProducerMetadata) error {
			return t.refreshProgress(ctx, updater, meta)
		},
	)

	// Get a function to be used in a goroutine to monitor whether a replan is
	// needed due to changes in node membership. This is important because if
	// there are idle nodes that become available, it's more efficient to restart
	// the TTL job to utilize those nodes for parallel work.
	replanChecker, cancelReplanner := sql.PhysicalPlanChangeChecker(
		ctx, t.physicalPlan, makePlan, jobExecCtx,
		sql.ReplanOnChangedFraction(func() float64 { return replanThreshold.Get(&execCfg.Settings.SV) }),
		func() time.Duration { return replanFrequency.Get(&execCfg.Settings.SV) },
	)

	// Create a separate context group to run the replanner and the TTL distSQL driver.
	// Note: this is distinct from the stats collection group, as errors from stats
	// collection are non-fatal and treated as warnings.
	g := ctxgroup.WithContext(ctx)
	g.GoCtx(func(ctx context.Context) error {
		defer cancelReplanner()
		if rowLevelTTL.RowStatsPollInterval != 0 {
			defer statsCancel(errors.New("cancelling TTL stats query because TTL job completed"))
		}
		distSQLReceiver := sql.MakeDistSQLReceiver(
			ctx,
			metadataCallbackWriter,
			tree.Rows,
			execCfg.RangeDescriptorCache,
			nil, /* txn */
			nil, /* clockUpdater */
			jobExecCtx.ExtendedEvalContext().Tracing,
		)
		defer distSQLReceiver.Release()

		// Copy the eval.Context, as dsp.Run() might change it.
		evalCtxCopy := jobExecCtx.ExtendedEvalContext().Context.Copy()
		distSQLPlanner.Run(
			ctx,
			t.planCtx,
			nil, /* txn */
			t.physicalPlan,
			distSQLReceiver,
			evalCtxCopy,
			nil, /* finishedSetupFn */
		)

		return metadataCallbackWriter.Err()
	})

	g.GoCtx(replanChecker)

	if err := g.Wait(); err != nil {
		return err
	}

	if err := statsGroup.Wait(); err != nil {
		// If the stats group was cancelled, use that error instead.
		err = errors.CombineErrors(context.Cause(statsCtx), err)
		if knobs.ReturnStatsError {
			return err
		}
		log.Warningf(ctx, "failed to get statistics for table id %d: %v", details.TableID, err)
	}
	return nil
}

// OnFailOrCancel implements the jobs.Resumer interface.
func (t rowLevelTTLResumer) OnFailOrCancel(
	ctx context.Context, execCtx interface{}, _ error,
) error {
	return nil
}

// CollectProfile implements the jobs.Resumer interface.
func (t rowLevelTTLResumer) CollectProfile(_ context.Context, _ interface{}) error {
	return nil
}

type jobProgressUpdater interface {
	Update(ctx context.Context, updateFn func(md jobs.JobMetadata, ju *jobs.JobUpdater) error) error
}

type ttlJobProgressUpdater struct {
	job *jobs.Job
}

func (u ttlJobProgressUpdater) Update(
	ctx context.Context, fn func(md jobs.JobMetadata, ju *jobs.JobUpdater) error,
) error {
	return u.job.NoTxn().Update(ctx, func(_ isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
		return fn(md, ju)
	})
}

// initProgress initializes the RowLevelTTL job progress metadata, including
// total span count and per-processor progress entries, based on the physical plan.
// This should be called before the job starts execution.
func (t rowLevelTTLResumer) initProgress(
	ctx context.Context, updater jobProgressUpdater, jobSpanCount int64,
) error {
	return updater.Update(ctx, func(md jobs.JobMetadata, ju *jobs.JobUpdater) error {
		rowLevelTTL := &jobspb.RowLevelTTLProgress{
			JobTotalSpanCount:     jobSpanCount,
			JobProcessedSpanCount: 0,
		}

		for _, proc := range t.physicalPlan.Processors {
			prog := jobspb.RowLevelTTLProcessorProgress{
				ProcessorID:   proc.Spec.ProcessorID,
				SQLInstanceID: proc.SQLInstanceID,
			}
			fmt.Printf("SPILLY: adding processor: processorID=%d, SQLInstanceID=%d\n", prog.ProcessorID, proc.SQLInstanceID)
			if proc.Spec.Core.Ttl != nil {
				prog.TotalSpanCount = int64(len(proc.Spec.Core.Ttl.Spans))
			}
			rowLevelTTL.ProcessorProgresses = append(rowLevelTTL.ProcessorProgresses, prog)
		}
		fmt.Printf("SPILLY: init setup %d processors\n", len(rowLevelTTL.ProcessorProgresses))

		progress := md.Progress
		progress.Details = &jobspb.Progress_RowLevelTTL{RowLevelTTL: rowLevelTTL}
		progress.Progress = &jobspb.Progress_FractionCompleted{
			FractionCompleted: 0,
		}
		ju.UpdateProgress(progress)
		return nil
	})
}

// SPILLY - use ttlprocessor mock to generate progress data
// SPILLY - inputs:
//  - init: bool
//  -
//
// SPILLY - add an interface for Update
// SPILLY - another way is to have the above function just take in the contents of the update function

// SPILLY - is there a way to mock the progress update? Just create your own JobUpdater

// refreshProgress ingests per-processor metadata pushed from TTL processors
// and updates the job level progress. It recomputes total spans processed
// and rows deleted, and sets the job level fraction completed.
func (t rowLevelTTLResumer) refreshProgress(
	ctx context.Context, updater jobProgressUpdater, meta *execinfrapb.ProducerMetadata,
) error {
	if meta.BulkProcessorProgress == nil {
		return nil
	}
	return updater.Update(ctx, func(md jobs.JobMetadata, ju *jobs.JobUpdater) error {
		var incomingProcProgress jobspb.RowLevelTTLProcessorProgress
		if err := pbtypes.UnmarshalAny(&meta.BulkProcessorProgress.ProgressDetails, &incomingProcProgress); err != nil {
			return errors.Wrapf(err, "unable to unmarshal ttl progress details")
		}

		rowLevelTTL := md.Progress.Details.(*jobspb.Progress_RowLevelTTL).RowLevelTTL

		// Reset job level counters before recomputing them from processor-level data.
		rowLevelTTL.JobDeletedRowCount = 0
		rowLevelTTL.JobProcessedSpanCount = 0
		totalSpanCount := int64(0)

		foundMatchingProcessor := false
		for i := range rowLevelTTL.ProcessorProgresses {
			existingProcProgress := &rowLevelTTL.ProcessorProgresses[i]
			if existingProcProgress.ProcessorID == incomingProcProgress.ProcessorID {
				*existingProcProgress = incomingProcProgress
				foundMatchingProcessor = true
			}
			rowLevelTTL.JobDeletedRowCount += existingProcProgress.DeletedRowCount
			rowLevelTTL.JobProcessedSpanCount += existingProcProgress.ProcessedSpanCount
			totalSpanCount += existingProcProgress.TotalSpanCount
		}
		if !foundMatchingProcessor {
			return errors.Errorf(
				"received progress for unknown processor: %v; known processors: %v",
				incomingProcProgress, rowLevelTTL)
		}
		if totalSpanCount != rowLevelTTL.JobTotalSpanCount {
			return errors.Errorf(
				"mismatch in span totals: computed=%d jobRecorded=%d",
				totalSpanCount, rowLevelTTL.JobTotalSpanCount)
		}

		md.Progress.Progress = &jobspb.Progress_FractionCompleted{
			FractionCompleted: float32(rowLevelTTL.JobProcessedSpanCount) / float32(rowLevelTTL.JobTotalSpanCount),
		}

		ju.UpdateProgress(md.Progress)
		return nil
	})
}

func init() {
	jobs.RegisterConstructor(jobspb.TypeRowLevelTTL, func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
		return &rowLevelTTLResumer{
			job: job,
			st:  settings,
		}
	}, jobs.UsesTenantCostControl)
}
