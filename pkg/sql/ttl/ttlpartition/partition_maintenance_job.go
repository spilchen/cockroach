// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttlpartition

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/lexbase"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
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
	codec := execCfg.Codec
	toDrop, toCreate, err := r.computePartitionChanges(codec, tableDesc, partitionTTL, window)
	if err != nil {
		return err
	}

	log.Dev.Infof(ctx, "partition changes: %d to drop, %d to create",
		len(toDrop), len(toCreate))

	// Step 5: Execute partition drops.
	if err := r.executePartitionDrops(ctx, db, details.TableID, toDrop); err != nil {
		return errors.Wrap(err, "failed to drop expired partitions")
	}

	// Step 6: Execute partition creates.
	if err := r.executePartitionCreates(ctx, db, details.TableID, toCreate); err != nil {
		return errors.Wrap(err, "failed to create new partitions")
	}

	// TODO: Steps 7-8 will be implemented in the next iteration:
	// 7. Trigger hybrid cleanup for dropped partitions
	// 8. Update job progress

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
	codec keys.SQLCodec,
	tableDesc catalog.TableDescriptor,
	config *catpb.PartitionTTLConfig,
	window partitionWindow,
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

	// Datum allocator for decoding partition bounds.
	a := &tree.DatumAlloc{}

	// Empty prefix datums (no parent partitioning for partition TTL).
	var fakePrefixDatums tree.Datums

	// Iterate through existing RANGE partitions and decode their bounds.
	err = partitioning.ForEachRange(func(name string, from, to []byte) error {
		// Decode the FromInclusive bound.
		fromTuple, _, err := rowenc.DecodePartitionTuple(
			a, codec, tableDesc, primaryIndex, partitioning, from, fakePrefixDatums)
		if err != nil {
			return errors.Wrapf(err, "failed to decode FROM bound for partition %q", name)
		}

		// Decode the ToExclusive bound.
		toTuple, _, err := rowenc.DecodePartitionTuple(
			a, codec, tableDesc, primaryIndex, partitioning, to, fakePrefixDatums)
		if err != nil {
			return errors.Wrapf(err, "failed to decode TO bound for partition %q", name)
		}

		// Extract timestamps from the decoded bounds.
		if len(fromTuple.Datums) == 0 || len(toTuple.Datums) == 0 {
			return errors.Newf("partition %q has invalid bounds", name)
		}

		// Get the lower and upper bound timestamps.
		var lowerBound, upperBound time.Time
		lowerBound, err = extractTimestamp(fromTuple.Datums[0])
		if err != nil {
			return errors.Wrapf(err, "failed to extract lower bound timestamp for partition %q", name)
		}

		upperBound, err = extractTimestamp(toTuple.Datums[0])
		if err != nil {
			return errors.Wrapf(err, "failed to extract upper bound timestamp for partition %q", name)
		}

		// Track existing upper bounds.
		existingUpperBounds[upperBound] = true

		// Check if this partition should be dropped (upper bound is before retention start).
		if upperBound.Before(window.retentionStart) {
			toDrop = append(toDrop, partitionSpec{
				name:       name,
				lowerBound: lowerBound,
				upperBound: upperBound,
			})
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	// Generate partitions to create for coverage until lookaheadEnd.
	// Start from the retention start and create partitions at granularity intervals.
	current := truncateToGranularity(window.retentionStart, granularityInterval.Duration)
	for current.Before(window.lookaheadEnd) {
		next := duration.Add(current, granularityInterval.Duration)

		// Only create if this upper bound doesn't already exist.
		if !existingUpperBounds[next] {
			partName := formatPartitionName(current)
			toCreate = append(toCreate, partitionSpec{
				name:       partName,
				lowerBound: current,
				upperBound: next,
			})
		}

		current = next
	}

	return toDrop, toCreate, nil
}

// extractTimestamp extracts a time.Time value from a datum.
func extractTimestamp(d tree.Datum) (time.Time, error) {
	switch t := d.(type) {
	case *tree.DTimestamp:
		return t.Time, nil
	case *tree.DTimestampTZ:
		return t.Time, nil
	default:
		return time.Time{}, errors.Newf("expected TIMESTAMP or TIMESTAMPTZ, got %T", d)
	}
}

// truncateToGranularity truncates a timestamp to the start of its granularity period.
// For example, with 1-day granularity, it truncates to midnight UTC.
func truncateToGranularity(t time.Time, granularity duration.Duration) time.Time {
	// For now, we'll implement this for day-based granularities.
	// TODO: Support hour/week/month granularities if needed.
	if granularity.Days > 0 && granularity.Months == 0 {
		// Truncate to the start of the day.
		return t.Truncate(24 * time.Hour)
	}
	// For other granularities, just return the original time.
	// This will need refinement based on actual use cases.
	return t
}

// formatPartitionName generates a unique partition name from a timestamp.
// Uses Unix nanoseconds to guarantee uniqueness for any granularity.
// The partition name is monotonically increasing and collision-free.
// Examples:
//   - p1736870400000000000 (2025-01-15 00:00:00 UTC)
//   - p1736956800000000000 (2025-01-16 00:00:00 UTC)
//   - p1736870400500000000 (2025-01-15 00:00:00.5 UTC for sub-second granularity)
func formatPartitionName(t time.Time) string {
	return fmt.Sprintf("p%d", t.UnixNano())
}

// buildDropPartitionStatement constructs an ALTER TABLE DROP PARTITION statement.
// Returns the SQL statement as a string.
func buildDropPartitionStatement(tableName *tree.TableName, partitionName string) string {
	escapedPartitionName := lexbase.EscapeSQLIdent(partitionName)
	return fmt.Sprintf(
		"ALTER TABLE %s DROP PARTITION IF EXISTS %s WITH DATA",
		tableName.FQString(),
		escapedPartitionName,
	)
}

// buildAddPartitionStatement constructs an ALTER TABLE ADD PARTITION statement for a RANGE partition.
// Returns the SQL statement as a string.
func buildAddPartitionStatement(
	tableName *tree.TableName, partitionName string, fromBound, toBound time.Time,
) string {
	escapedPartitionName := lexbase.EscapeSQLIdent(partitionName)
	// Format timestamps as SQL string literals in RFC3339 format.
	// The database will parse these according to the column type.
	fromStr := fromBound.Format(time.RFC3339)
	toStr := toBound.Format(time.RFC3339)
	return fmt.Sprintf(
		"ALTER TABLE %s ADD PARTITION %s VALUES FROM ('%s') TO ('%s')",
		tableName.FQString(),
		escapedPartitionName,
		fromStr,
		toStr,
	)
}

// executePartitionDrops executes ALTER TABLE DROP PARTITION statements for the specified partitions.
func (r *partitionTTLMaintenanceResumer) executePartitionDrops(
	ctx context.Context, db *sql.InternalDB, tableID catid.DescID, toDrop []partitionSpec,
) error {
	if len(toDrop) == 0 {
		return nil
	}

	return db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		// Get the table descriptor to construct the fully qualified table name.
		tableDesc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, tableID)
		if err != nil {
			return err
		}

		// Get database descriptor.
		dbDesc, err := txn.Descriptors().ByIDWithoutLeased(txn.KV()).Get().Database(ctx, tableDesc.GetParentID())
		if err != nil {
			return err
		}

		// Get schema descriptor.
		schemaDesc, err := txn.Descriptors().ByIDWithoutLeased(txn.KV()).Get().Schema(ctx, tableDesc.GetParentSchemaID())
		if err != nil {
			return err
		}

		// Construct fully qualified table name.
		tableName := tree.NewTableNameWithSchema(
			tree.Name(dbDesc.GetName()),
			tree.Name(schemaDesc.GetName()),
			tree.Name(tableDesc.GetName()))

		// Drop each partition.
		for _, partition := range toDrop {
			alterStmt := buildDropPartitionStatement(tableName, partition.name)

			log.Dev.Infof(ctx, "dropping partition %s on table %s (upper bound: %s)",
				partition.name, tableName.FQString(), partition.upperBound.Format(time.RFC3339))

			// Execute the DROP PARTITION statement.
			if _, err := txn.Exec(ctx, "drop-partition-ttl", txn.KV(), alterStmt); err != nil {
				return errors.Wrapf(err, "failed to drop partition %q", partition.name)
			}

			log.Dev.Infof(ctx, "successfully dropped partition %s", partition.name)
		}

		return nil
	})
}

// executePartitionCreates executes ALTER TABLE ADD PARTITION statements for the specified partitions.
func (r *partitionTTLMaintenanceResumer) executePartitionCreates(
	ctx context.Context, db *sql.InternalDB, tableID catid.DescID, toCreate []partitionSpec,
) error {
	if len(toCreate) == 0 {
		return nil
	}

	return db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		// Get the table descriptor to construct the fully qualified table name.
		tableDesc, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, tableID)
		if err != nil {
			return err
		}

		// Get database descriptor.
		dbDesc, err := txn.Descriptors().ByIDWithoutLeased(txn.KV()).Get().Database(ctx, tableDesc.GetParentID())
		if err != nil {
			return err
		}

		// Get schema descriptor.
		schemaDesc, err := txn.Descriptors().ByIDWithoutLeased(txn.KV()).Get().Schema(ctx, tableDesc.GetParentSchemaID())
		if err != nil {
			return err
		}

		// Construct fully qualified table name.
		tableName := tree.NewTableNameWithSchema(
			tree.Name(dbDesc.GetName()),
			tree.Name(schemaDesc.GetName()),
			tree.Name(tableDesc.GetName()))

		// Create each partition.
		for _, partition := range toCreate {
			alterStmt := buildAddPartitionStatement(
				tableName, partition.name, partition.lowerBound, partition.upperBound)

			log.Dev.Infof(ctx, "creating partition %s on table %s (from: %s, to: %s)",
				partition.name, tableName.FQString(),
				partition.lowerBound.Format(time.RFC3339),
				partition.upperBound.Format(time.RFC3339))

			// Execute the ADD PARTITION statement.
			if _, err := txn.Exec(ctx, "add-partition-ttl", txn.KV(), alterStmt); err != nil {
				return errors.Wrapf(err, "failed to create partition %q", partition.name)
			}

			log.Dev.Infof(ctx, "successfully created partition %s", partition.name)
		}

		return nil
	})
}

func init() {
	jobs.RegisterConstructor(jobspb.TypePartitionTTLScheduler, func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
		return &partitionTTLMaintenanceResumer{
			job: job,
			st:  settings,
		}
	}, jobs.UsesTenantCostControl)
}
