// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttlpartition

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/duration"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

// partitionWindow represents the time range for which partitions should exist.
type partitionWindow struct {
	// retentionStart is the earliest time for which data should be retained (now - retention).
	retentionStart time.Time
	// lookaheadEnd is the furthest time for which partitions should be created (now + lookahead).
	lookaheadEnd time.Time
}

// partitionSpec describes a partition to be created or dropped.
type partitionSpec struct {
	// name is the partition name (e.g., "p20250113").
	name string
	// lowerBound is the inclusive lower bound of the partition.
	lowerBound time.Time
	// upperBound is the exclusive upper bound of the partition.
	upperBound time.Time
}

// partitionTTLMaintenanceResumer implements the partition TTL maintenance job.
// This job is responsible for:
// 1. Computing the partition window [now - retention, now + lookahead]
// 2. Dropping expired partitions
// 3. Creating new partitions to maintain coverage
// 4. Triggering hybrid cleanup for dropped partitions
type partitionTTLMaintenanceResumer struct {
	job *jobs.Job
	st  *cluster.Settings
}

var _ jobs.Resumer = (*partitionTTLMaintenanceResumer)(nil)

// Resume implements the jobs.Resumer interface.
func (r *partitionTTLMaintenanceResumer) Resume(ctx context.Context, execCtx interface{}) error {
	jobExecCtx := execCtx.(sql.JobExecContext)
	execCfg := jobExecCtx.ExecCfg()
	db := execCfg.InternalDB

	details := r.job.Details().(jobspb.PartitionTTLMaintenanceDetails)

	log.Dev.Infof(ctx, "partition TTL maintenance job started for table %d", details.TableID)

	var tableDesc catalog.TableDescriptor
	var partitionTTL *catpb.PartitionTTLConfig

	// Step 1: Read table descriptor and partition TTL config.
	if err := db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		desc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, details.TableID)
		if err != nil {
			return err
		}

		if desc.GetPartitionTTL() == nil {
			return errors.Newf("table %d does not have partition TTL configured", details.TableID)
		}

		tableDesc = desc
		partitionTTL = desc.GetPartitionTTL()
		return nil
	}); err != nil {
		return err
	}

	// Step 2: Compute partition window [now - retention, now + lookahead].
	now := timeutil.Now()
	window, err := r.computePartitionWindow(partitionTTL, now)
	if err != nil {
		return err
	}

	log.Dev.Infof(ctx, "partition window: retention_start=%s, lookahead_end=%s",
		window.retentionStart.Format(time.RFC3339),
		window.lookaheadEnd.Format(time.RFC3339))

	// Step 3-4: Identify partitions to drop and create.
	toDrop, toCreate, err := r.computePartitionChanges(tableDesc, partitionTTL, window)
	if err != nil {
		return err
	}

	log.Dev.Infof(ctx, "partition changes: %d to drop, %d to create",
		len(toDrop), len(toCreate))

	// TODO: Steps 5-8 will be implemented in the next iteration:
	// 5. Execute partition drops
	// 6. Execute partition creates
	// 7. Trigger hybrid cleanup for dropped partitions
	// 8. Update job progress

	_ = toDrop
	_ = toCreate

	log.Dev.Infof(ctx, "partition TTL maintenance job completed for table %d", details.TableID)
	return nil
}

// OnFailOrCancel implements the jobs.Resumer interface.
func (r *partitionTTLMaintenanceResumer) OnFailOrCancel(
	ctx context.Context, execCtx interface{}, _ error,
) error {
	// TODO: Implement cleanup logic if needed.
	return nil
}

// CollectProfile implements the jobs.Resumer interface.
func (r *partitionTTLMaintenanceResumer) CollectProfile(_ context.Context, _ interface{}) error {
	return nil
}

// computePartitionWindow calculates the time range for which partitions should exist.
// Returns [now - retention, now + lookahead].
func (r *partitionTTLMaintenanceResumer) computePartitionWindow(
	config *catpb.PartitionTTLConfig, now time.Time,
) (partitionWindow, error) {
	// Parse retention duration.
	retentionInterval, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, config.Retention)
	if err != nil {
		return partitionWindow{}, errors.Wrapf(err, "failed to parse retention %q", config.Retention)
	}

	// Parse lookahead duration.
	lookaheadInterval, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, config.Lookahead)
	if err != nil {
		return partitionWindow{}, errors.Wrapf(err, "failed to parse lookahead %q", config.Lookahead)
	}

	return partitionWindow{
		retentionStart: duration.Add(now, retentionInterval.Duration.Mul(-1)),
		lookaheadEnd:   duration.Add(now, lookaheadInterval.Duration),
	}, nil
}

// computePartitionChanges determines which partitions need to be dropped and created.
// Partitions with upperBound < window.retentionStart should be dropped.
// New partitions should be created to maintain coverage until window.lookaheadEnd.
func (r *partitionTTLMaintenanceResumer) computePartitionChanges(
	tableDesc catalog.TableDescriptor, config *catpb.PartitionTTLConfig, window partitionWindow,
) (toDrop []partitionSpec, toCreate []partitionSpec, err error) {
	// Parse granularity to determine partition interval.
	granularityInterval, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, config.Granularity)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to parse granularity %q", config.Granularity)
	}

	// Get existing partitions from the primary index.
	primaryIndex := tableDesc.GetPrimaryIndex()
	partitioning := primaryIndex.GetPartitioning()

	// Track existing partition upper bounds to avoid creating duplicates.
	existingUpperBounds := make(map[time.Time]bool)

	// Iterate through existing RANGE partitions.
	err = partitioning.ForEachRange(func(name string, from, to []byte) error {
		// Parse the upper bound from the partition.
		// TODO: This needs to decode the partition bound. For now, we'll stub this out.
		// The actual implementation will need to decode the encoded bytes in 'to'.
		_ = name
		_ = from
		_ = to

		// For now, we'll skip the partition analysis since we need to properly decode
		// the partition bounds. This will be implemented in the next iteration.
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// TODO: Implement partition bound decoding and analysis.
	// This requires decoding the encoded bytes from PartitioningDescriptor.Range.ToExclusive
	// and comparing them against the partition window.

	_ = existingUpperBounds
	_ = granularityInterval

	return toDrop, toCreate, nil
}

func init() {
	jobs.RegisterConstructor(jobspb.TypePartitionTTLScheduler, func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
		return &partitionTTLMaintenanceResumer{
			job: job,
			st:  settings,
		}
	}, jobs.UsesTenantCostControl)
}
