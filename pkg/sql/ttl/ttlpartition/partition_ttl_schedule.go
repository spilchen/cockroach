// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttlpartition

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/scheduledjobs"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
	pbtypes "github.com/gogo/protobuf/types"
)

type partitionTTLExecutor struct {
	metrics partitionTTLMetrics
}

var _ jobs.ScheduledJobController = (*partitionTTLExecutor)(nil)

type partitionTTLMetrics struct {
	*jobs.ExecutorMetrics
}

var _ metric.Struct = &partitionTTLMetrics{}

// MetricStruct implements metric.Struct interface.
func (m *partitionTTLMetrics) MetricStruct() {}

// OnDrop implements the jobs.ScheduledJobController interface.
func (s partitionTTLExecutor) OnDrop(
	ctx context.Context,
	scheduleControllerEnv scheduledjobs.ScheduleControllerEnv,
	env scheduledjobs.JobSchedulerEnv,
	schedule *jobs.ScheduledJob,
	txn isql.Txn,
	descsCol *descs.Collection,
) (int, error) {

	var args catpb.ScheduledPartitionTTLArgs
	if err := pbtypes.UnmarshalAny(schedule.ExecutionArgs().Args, &args); err != nil {
		return 0, err
	}

	canDrop, err := canDropPartitionTTLSchedule(ctx, txn.KV(), descsCol, schedule, args)
	if err != nil {
		return 0, err
	}

	if !canDrop {
		tbl, err := descsCol.ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, args.TableID)
		if err != nil {
			return 0, err
		}
		tn, err := descs.GetObjectName(ctx, txn.KV(), descsCol, tbl)
		if err != nil {
			return 0, err
		}
		return 0, errors.WithHintf(
			pgerror.Newf(
				pgcode.InvalidTableDefinition,
				"cannot drop a partition TTL schedule",
			),
			`use ALTER TABLE %s RESET (ttl_mode) instead`,
			tn.FQString(),
		)
	}
	return 0, nil
}

// canDropPartitionTTLSchedule determines whether we can drop a given partition TTL
// schedule. This is intended to only be permitted for schedules which are not
// valid.
func canDropPartitionTTLSchedule(
	ctx context.Context,
	txn *kv.Txn,
	descsCol *descs.Collection,
	schedule *jobs.ScheduledJob,
	args catpb.ScheduledPartitionTTLArgs,
) (bool, error) {
	desc, err := descsCol.ByIDWithLeased(txn).WithoutNonPublic().Get().Table(ctx, args.TableID)
	if err != nil {
		// If the descriptor does not exist we can drop this schedule.
		if sqlerrors.IsUndefinedRelationError(err) {
			return true, nil
		}
		return false, err
	}
	if desc == nil {
		return true, nil
	}
	// If there is no partition TTL on the table we can drop this schedule.
	if desc.GetPartitionTTL() == nil {
		return true, nil
	}
	// If there is a schedule id mismatch we can drop this schedule.
	if desc.GetPartitionTTL().ScheduleID != schedule.ScheduleID() {
		return true, nil
	}
	return false, nil
}

// ExecuteJob implements the jobs.ScheduledJobController interface.
func (s partitionTTLExecutor) ExecuteJob(
	ctx context.Context,
	txn isql.Txn,
	cfg *scheduledjobs.JobExecutionConfig,
	env scheduledjobs.JobSchedulerEnv,
	sj *jobs.ScheduledJob,
) error {
	args := &catpb.ScheduledPartitionTTLArgs{}
	if err := pbtypes.UnmarshalAny(sj.ExecutionArgs().Args, args); err != nil {
		return err
	}

	p, cleanup := cfg.PlanHookMaker(
		ctx,
		// TableID is not sensitive.
		redact.SafeString(fmt.Sprintf("invoke-partition-ttl-%d", args.TableID)),
		txn.KV(),
		username.NodeUserName(),
	)
	defer cleanup()

	execCfg := p.(sql.PlanHookState).ExecCfg()
	if err := createPartitionTTLMaintenanceJob(
		ctx,
		&jobs.CreatedByInfo{
			ID:   int64(sj.ScheduleID()),
			Name: jobs.CreatedByScheduledJobs,
		},
		txn,
		execCfg.JobRegistry,
		execCfg.InternalDB,
		*args,
	); err != nil {
		s.metrics.NumFailed.Inc(1)
		return err
	}
	s.metrics.NumStarted.Inc(1)
	return nil
}

// NotifyJobTermination implements the jobs.ScheduledJobController interface.
func (s partitionTTLExecutor) NotifyJobTermination(
	ctx context.Context,
	txn isql.Txn,
	jobID jobspb.JobID,
	jobStatus jobs.State,
	details jobspb.Details,
	env scheduledjobs.JobSchedulerEnv,
	sj *jobs.ScheduledJob,
) error {
	if jobStatus == jobs.StateFailed {
		jobs.DefaultHandleFailedRun(
			sj,
			"partition ttl maintenance for table [%d] job failed",
			details.(jobspb.PartitionTTLMaintenanceDetails).TableID,
		)
		s.metrics.NumFailed.Inc(1)
		return nil
	}

	if jobStatus == jobs.StateSucceeded {
		s.metrics.NumSucceeded.Inc(1)
	}

	sj.SetScheduleStatus(string(jobStatus))
	return nil
}

// Metrics implements the jobs.ScheduledJobController interface.
func (s partitionTTLExecutor) Metrics() metric.Struct {
	return &s.metrics
}

// GetCreateScheduleStatement implements the jobs.ScheduledJobController interface.
func (s partitionTTLExecutor) GetCreateScheduleStatement(
	ctx context.Context, txn isql.Txn, env scheduledjobs.JobSchedulerEnv, sj *jobs.ScheduledJob,
) (string, error) {
	descsCol := descs.FromTxn(txn)
	args := &catpb.ScheduledPartitionTTLArgs{}
	if err := pbtypes.UnmarshalAny(sj.ExecutionArgs().Args, args); err != nil {
		return "", err
	}
	tbl, err := descsCol.ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, args.TableID)
	if err != nil {
		return "", err
	}
	tn, err := descs.GetObjectName(ctx, txn.KV(), descsCol, tbl)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`ALTER TABLE %s WITH (ttl_mode = 'partition', ...)`, tn.FQString()), nil
}

// createPartitionTTLMaintenanceJob creates a partition TTL maintenance job.
// This is called by the scheduled job executor to spawn maintenance jobs.
func createPartitionTTLMaintenanceJob(
	ctx context.Context,
	createdByInfo *jobs.CreatedByInfo,
	txn isql.Txn,
	jobRegistry *jobs.Registry,
	db *sql.InternalDB,
	ttlArgs catpb.ScheduledPartitionTTLArgs,
) error {
	descsCol := descs.FromTxn(txn)
	tableID := ttlArgs.TableID
	tableDesc, err := descsCol.ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, tableID)
	if err != nil {
		return err
	}
	tn, err := descs.GetObjectName(ctx, txn.KV(), descsCol, tableDesc)
	if err != nil {
		return err
	}

	description := fmt.Sprintf(
		"partition maintenance for table %s (%d)",
		tn.FQString(),
		tableID,
	)

	details := jobspb.PartitionTTLMaintenanceDetails{
		TableID: tableID,
	}

	progress := jobspb.PartitionTTLMaintenanceProgress{
		PartitionsCreated: 0,
		PartitionsDropped: 0,
	}

	record := jobs.Record{
		Description: description,
		Username:    username.NodeUserName(),
		Details:     details,
		Progress:    progress,
		CreatedBy:   createdByInfo,
	}

	jobID := jobRegistry.MakeJobID()
	if _, err := jobRegistry.CreateAdoptableJobWithTxn(ctx, record, jobID, txn); err != nil {
		return err
	}

	return nil
}

func init() {
	jobs.RegisterScheduledJobExecutorFactory(
		tree.ScheduledPartitionTTLExecutor.InternalName(),
		func() (jobs.ScheduledJobExecutor, error) {
			m := jobs.MakeExecutorMetrics(tree.ScheduledPartitionTTLExecutor.InternalName())
			return &partitionTTLExecutor{
				metrics: partitionTTLMetrics{
					ExecutorMetrics: &m,
				},
			}, nil
		},
	)
}
