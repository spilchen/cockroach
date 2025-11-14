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
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
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

// PartitionWindow represents the time range for which partitions should exist.
type PartitionWindow struct {
	// retentionStart is the earliest time for which data should be retained (now - retention).
	RetentionStart time.Time
	// lookaheadEnd is the furthest time for which partitions should be created (now + lookahead).
	LookaheadEnd time.Time
}

// PartitionSpec describes a partition to be created or dropped.
type PartitionSpec struct {
	// name is the partition name (e.g., "p20250113").
	name string
	// lowerBound is the inclusive lower bound of the partition.
	lowerBound time.Time
	// upperBound is the exclusive upper bound of the partition.
	upperBound time.Time
}

// PartitionTTLMaintenanceResumer implements the partition TTL maintenance job.
// This job is responsible for:
// 1. Computing the partition window [now - retention, now + lookahead]
// 2. Dropping expired partitions
// 3. Creating new partitions to maintain coverage
// 4. Triggering hybrid cleanup for dropped partitions
type PartitionTTLMaintenanceResumer struct {
	Job *jobs.Job
	St  *cluster.Settings
}

var _ jobs.Resumer = (*PartitionTTLMaintenanceResumer)(nil)

// Resume implements the jobs.Resumer interface.
func (r *PartitionTTLMaintenanceResumer) Resume(ctx context.Context, execCtx interface{}) error {
	jobExecCtx := execCtx.(sql.JobExecContext)
	execCfg := jobExecCtx.ExecCfg()
	db := execCfg.InternalDB

	details := r.Job.Details().(jobspb.PartitionTTLMaintenanceDetails)

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
	window, err := r.ComputePartitionWindow(partitionTTL, now)
	if err != nil {
		return err
	}

	log.Dev.Infof(ctx, "partition window: retention_start=%s, lookahead_end=%s",
		window.RetentionStart.Format(time.RFC3339),
		window.LookaheadEnd.Format(time.RFC3339))

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

	// Step 7: Trigger hybrid cleanup for dropped partitions.
	if err := r.triggerHybridCleanup(ctx, execCfg, tableDesc, partitionTTL, toDrop); err != nil {
		return errors.Wrap(err, "failed to trigger hybrid cleanup")
	}

	// Step 8: Update job progress.
	if err := r.Job.NoTxn().FractionProgressed(ctx, jobs.FractionUpdater(1.0)); err != nil {
		return errors.Wrap(err, "failed to update job progress")
	}

	log.Dev.Infof(ctx, "partition TTL maintenance job completed for table %d", details.TableID)
	return nil
}

// OnFailOrCancel implements the jobs.Resumer interface.
func (r *PartitionTTLMaintenanceResumer) OnFailOrCancel(
	ctx context.Context, execCtx interface{}, _ error,
) error {
	// TODO: Implement cleanup logic if needed.
	return nil
}

// CollectProfile implements the jobs.Resumer interface.
func (r *PartitionTTLMaintenanceResumer) CollectProfile(_ context.Context, _ interface{}) error {
	return nil
}

// computePartitionWindow calculates the time range for which partitions should exist.
// Returns [now - retention, now + lookahead].
func (r *PartitionTTLMaintenanceResumer) ComputePartitionWindow(
	config *catpb.PartitionTTLConfig, now time.Time,
) (PartitionWindow, error) {
	// Parse retention duration.
	retentionInterval, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, config.Retention)
	if err != nil {
		return PartitionWindow{}, errors.Wrapf(err, "failed to parse retention %q", config.Retention)
	}

	// Parse lookahead duration.
	lookaheadInterval, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, config.Lookahead)
	if err != nil {
		return PartitionWindow{}, errors.Wrapf(err, "failed to parse lookahead %q", config.Lookahead)
	}

	return PartitionWindow{
		RetentionStart: duration.Add(now, retentionInterval.Duration.Mul(-1)),
		LookaheadEnd:   duration.Add(now, lookaheadInterval.Duration),
	}, nil
}

// computePartitionChanges determines which partitions need to be dropped and created.
// Partitions with upperBound < window.RetentionStart should be dropped.
// New partitions should be created to maintain coverage until window.LookaheadEnd.
func (r *PartitionTTLMaintenanceResumer) computePartitionChanges(
	codec keys.SQLCodec,
	tableDesc catalog.TableDescriptor,
	config *catpb.PartitionTTLConfig,
	window PartitionWindow,
) (toDrop []PartitionSpec, toCreate []PartitionSpec, err error) {
	// Parse granularity to determine partition interval.
	granularityInterval, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, config.Granularity)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to parse granularity %q", config.Granularity)
	}

	// Validate granularity meets minimum requirements.
	if err := ValidateGranularity(granularityInterval.Duration, config.Granularity); err != nil {
		return nil, nil, err
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
		lowerBound, err = ExtractTimestamp(fromTuple.Datums[0])
		if err != nil {
			return errors.Wrapf(err, "failed to extract lower bound timestamp for partition %q", name)
		}

		upperBound, err = ExtractTimestamp(toTuple.Datums[0])
		if err != nil {
			return errors.Wrapf(err, "failed to extract upper bound timestamp for partition %q", name)
		}

		// Track existing upper bounds.
		existingUpperBounds[upperBound] = true

		// Check if this partition should be dropped (upper bound is before retention start).
		if upperBound.Before(window.RetentionStart) {
			toDrop = append(toDrop, PartitionSpec{
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
	current := TruncateToGranularity(window.RetentionStart, granularityInterval.Duration)
	for current.Before(window.LookaheadEnd) {
		next := duration.Add(current, granularityInterval.Duration)

		// Only create if this upper bound doesn't already exist.
		if !existingUpperBounds[next] {
			partName := FormatPartitionName(current)
			toCreate = append(toCreate, PartitionSpec{
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
func ExtractTimestamp(d tree.Datum) (time.Time, error) {
	switch t := d.(type) {
	case *tree.DTimestamp:
		return t.Time, nil
	case *tree.DTimestampTZ:
		return t.Time, nil
	default:
		return time.Time{}, errors.Newf("expected TIMESTAMP or TIMESTAMPTZ, got %T", d)
	}
}

// validateGranularity validates that the granularity meets minimum requirements.
// The minimum granularity is 10 seconds to prevent creating too many partitions.
func ValidateGranularity(granularity duration.Duration, granularityStr string) error {
	const minGranularityNanos = int64(10 * time.Second)

	// Month-based granularities are always valid (months are > 10 seconds).
	if granularity.Months > 0 {
		return nil
	}

	// Day-based granularities are always valid (days are > 10 seconds).
	if granularity.Days > 0 {
		return nil
	}

	// For sub-day granularities, check total duration in nanoseconds.
	totalNanos := granularity.Nanos()
	if totalNanos < minGranularityNanos {
		return errors.Newf(
			"partition granularity %q is too small; minimum granularity is 10 seconds",
			granularityStr,
		)
	}

	return nil
}

// truncateToGranularity truncates a timestamp to the start of its granularity period.
// Supports month, day (including week), hour, minute, and second granularities.
// Examples:
//   - 1 month: truncates to start of month
//   - 7 days: truncates to start of week (aligned to Unix epoch)
//   - 1 day: truncates to midnight UTC
//   - 6 hours: truncates to 6-hour period (00:00, 06:00, 12:00, 18:00)
//   - 10 seconds: truncates to 10-second period (useful for testing)
func TruncateToGranularity(t time.Time, granularity duration.Duration) time.Time {
	// Handle month-based granularities.
	if granularity.Months > 0 {
		// Truncate to the start of the month.
		year, month, _ := t.Date()

		// Calculate which month period we're in.
		// Convert to months since year 0.
		monthsSinceEpoch := year*12 + int(month) - 1
		periodStartMonth := (monthsSinceEpoch / int(granularity.Months)) * int(granularity.Months)

		periodYear := periodStartMonth / 12
		periodMonth := time.Month((periodStartMonth % 12) + 1)

		return time.Date(periodYear, periodMonth, 1, 0, 0, 0, 0, time.UTC)
	}

	// Handle day-based granularities (including weeks).
	if granularity.Days > 0 {
		// Truncate to midnight UTC first.
		dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)

		// For multi-day granularities (e.g., 7 days for weeks), align to Unix epoch.
		if granularity.Days > 1 {
			// Calculate days since Unix epoch (1970-01-01).
			epochStart := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
			daysSinceEpoch := int(dayStart.Sub(epochStart).Hours() / 24)

			// Find the start of the current period.
			periodStartDays := (daysSinceEpoch / int(granularity.Days)) * int(granularity.Days)
			return epochStart.AddDate(0, 0, periodStartDays)
		}

		return dayStart
	}

	// Handle sub-day granularities (hours, minutes, seconds).
	// Convert duration.Duration to time.Duration using Nanos().
	nanos := granularity.Nanos()
	if nanos > 0 {
		timeDuration := time.Duration(nanos)

		// Calculate time since Unix epoch.
		epochStart := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
		timeSinceEpoch := t.Sub(epochStart)

		// Find the start of the current period.
		periodStart := (timeSinceEpoch / timeDuration) * timeDuration
		return epochStart.Add(periodStart)
	}

	// Fallback: return original time.
	return t
}

// formatPartitionName generates a unique partition name from a timestamp.
// Uses Unix nanoseconds to guarantee uniqueness for any granularity.
// The partition name is monotonically increasing and collision-free.
// Examples:
//   - p1736870400000000000 (2025-01-15 00:00:00 UTC)
//   - p1736956800000000000 (2025-01-16 00:00:00 UTC)
//   - p1736870400500000000 (2025-01-15 00:00:00.5 UTC for sub-second granularity)
func FormatPartitionName(t time.Time) string {
	return fmt.Sprintf("p%d", t.UnixNano())
}

// buildDropPartitionStatement constructs an ALTER TABLE DROP PARTITION statement.
// Returns the SQL statement as a string.
func BuildDropPartitionStatement(tableName *tree.TableName, partitionName string) string {
	escapedPartitionName := lexbase.EscapeSQLIdent(partitionName)
	return fmt.Sprintf(
		"ALTER TABLE %s DROP PARTITION IF EXISTS %s WITH DATA",
		tableName.FQString(),
		escapedPartitionName,
	)
}

// buildAddPartitionStatement constructs an ALTER TABLE ADD PARTITION statement for a RANGE partition.
// Returns the SQL statement as a string.
func BuildAddPartitionStatement(
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
func (r *PartitionTTLMaintenanceResumer) executePartitionDrops(
	ctx context.Context, db *sql.InternalDB, tableID catid.DescID, toDrop []PartitionSpec,
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
			alterStmt := BuildDropPartitionStatement(tableName, partition.name)

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
func (r *PartitionTTLMaintenanceResumer) executePartitionCreates(
	ctx context.Context, db *sql.InternalDB, tableID catid.DescID, toCreate []PartitionSpec,
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
			alterStmt := BuildAddPartitionStatement(
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

// triggerHybridCleanup creates hybrid cleanup jobs for dropped partitions.
// These jobs clean up orphaned secondary index entries that remain after partition drops.
func (r *PartitionTTLMaintenanceResumer) triggerHybridCleanup(
	ctx context.Context,
	execCfg *sql.ExecutorConfig,
	tableDesc catalog.TableDescriptor,
	config *catpb.PartitionTTLConfig,
	droppedPartitions []PartitionSpec,
) error {
	// If no partitions were dropped, nothing to clean up.
	if len(droppedPartitions) == 0 {
		return nil
	}

	// Determine which secondary indexes need cleanup.
	// Non-aligned indexes (where the partition column is not the leftmost key) need hybrid cleanup.
	targetIndexIDs, err := DetermineNonAlignedIndexes(tableDesc, config.ColumnName)
	if err != nil {
		return err
	}

	// If there are no non-aligned indexes, no hybrid cleanup is needed.
	if len(targetIndexIDs) == 0 {
		log.Dev.Infof(ctx, "no non-aligned secondary indexes found, skipping hybrid cleanup")
		return nil
	}

	log.Dev.Infof(ctx, "triggering hybrid cleanup for %d dropped partitions targeting %d non-aligned indexes",
		len(droppedPartitions), len(targetIndexIDs))

	// Create a hybrid cleanup job for each dropped partition.
	for _, partition := range droppedPartitions {
		if err := r.createHybridCleanupJob(ctx, execCfg, tableDesc, partition, targetIndexIDs); err != nil {
			return errors.Wrapf(err, "failed to create hybrid cleanup job for partition %s", partition.name)
		}
	}

	return nil
}

// DetermineNonAlignedIndexes returns the list of secondary index IDs that are not aligned
// with the partition column. An aligned index has the partition column as its leftmost key.
// This function is exported for testing purposes.
func DetermineNonAlignedIndexes(
	tableDesc catalog.TableDescriptor, partitionColumnName string,
) ([]descpb.IndexID, error) {
	var nonAlignedIndexes []descpb.IndexID

	// Iterate through all secondary indexes.
	for _, idx := range tableDesc.NonPrimaryIndexes() {
		// Check if the partition column is the leftmost key in this index.
		if idx.NumKeyColumns() == 0 {
			continue
		}

		firstColID := idx.GetKeyColumnID(0)
		firstCol, err := catalog.MustFindColumnByID(tableDesc, firstColID)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to find column %d in index %d", firstColID, idx.GetID())
		}

		// If the leftmost column is NOT the partition column, this index needs cleanup.
		if firstCol.GetName() != partitionColumnName {
			nonAlignedIndexes = append(nonAlignedIndexes, idx.GetID())
		}
	}

	return nonAlignedIndexes, nil
}

// createHybridCleanupJob creates a single hybrid cleanup job for a dropped partition.
func (r *PartitionTTLMaintenanceResumer) createHybridCleanupJob(
	ctx context.Context,
	execCfg *sql.ExecutorConfig,
	tableDesc catalog.TableDescriptor,
	partition PartitionSpec,
	targetIndexIDs []descpb.IndexID,
) error {
	// Compute the PK spans for this partition.
	codec := execCfg.Codec
	pkSpans, err := r.computePartitionPKSpans(codec, tableDesc, partition)
	if err != nil {
		return errors.Wrapf(err, "failed to compute PK spans for partition %s", partition.name)
	}

	// Submit the job within a transaction.
	db := execCfg.InternalDB
	jobRegistry := execCfg.JobRegistry
	jobID := jobRegistry.MakeJobID()

	return db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		// Get database and schema descriptors to construct the fully qualified table name.
		dbDesc, err := txn.Descriptors().ByIDWithoutLeased(txn.KV()).Get().Database(ctx, tableDesc.GetParentID())
		if err != nil {
			return err
		}

		schemaDesc, err := txn.Descriptors().ByIDWithoutLeased(txn.KV()).Get().Schema(ctx, tableDesc.GetParentSchemaID())
		if err != nil {
			return err
		}

		// Construct fully qualified table name.
		tableName := tree.NewTableNameWithSchema(
			tree.Name(dbDesc.GetName()),
			tree.Name(schemaDesc.GetName()),
			tree.Name(tableDesc.GetName()))

		description := fmt.Sprintf("hybrid cleanup for partition %s on table %s", partition.name, tableName.FQString())

		// Create the hybrid cleaner details.
		hybridCleanerDetails := &jobspb.HybridCleanerDetails{
			PartitionStart: partition.lowerBound,
			PartitionEnd:   partition.upperBound,
			TargetIndexIDs: targetIndexIDs,
			PartitionSpans: pkSpans,
		}

		// Create the job record.
		record := jobs.Record{
			Description: description,
			Username:    username.NodeUserName(),
			Details: jobspb.RowLevelTTLDetails{
				TableID:              tableDesc.GetID(),
				Cutoff:               partition.upperBound, // Use partition end as cutoff
				TableVersion:         tableDesc.GetVersion(),
				HybridCleanerDetails: hybridCleanerDetails,
			},
			Progress: jobspb.RowLevelTTLProgress{},
		}

		// Submit the job using the transaction.
		if _, err := jobRegistry.CreateAdoptableJobWithTxn(ctx, record, jobID, txn); err != nil {
			return errors.Wrap(err, "failed to create hybrid cleanup job")
		}

		log.Dev.Infof(ctx, "created hybrid cleanup job %d for partition %s", jobID, partition.name)
		return nil
	})
}

// computePartitionPKSpans computes the primary key spans for a given partition.
// Returns a span that covers exactly the partition's time range in the primary index.
func (r *PartitionTTLMaintenanceResumer) computePartitionPKSpans(
	codec keys.SQLCodec, tableDesc catalog.TableDescriptor, partition PartitionSpec,
) ([]roachpb.Span, error) {
	primaryIndex := tableDesc.GetPrimaryIndex()

	// Create the index key prefix for the primary index.
	keyPrefix := rowenc.MakeIndexKeyPrefix(codec, tableDesc.GetID(), primaryIndex.GetID())

	// The partition column must be the first column in the primary index for partition TTL.
	// We need to encode the partition bounds (timestamps) into PK keys.
	if primaryIndex.NumKeyColumns() == 0 {
		return nil, errors.New("primary index has no key columns")
	}

	// Get the first key column (partition column).
	partitionColID := primaryIndex.GetKeyColumnID(0)

	// Create datums for the lower and upper bounds.
	lowerDatum := &tree.DTimestampTZ{Time: partition.lowerBound}
	upperDatum := &tree.DTimestampTZ{Time: partition.upperBound}

	// Build a column map for encoding.
	var colMap catalog.TableColMap
	colMap.Set(partitionColID, 0)

	// Encode the lower bound key.
	lowerKey, err := rowenc.EncodeColumns(
		[]descpb.ColumnID{partitionColID},
		primaryIndex.IndexDesc().KeyColumnDirections[:1],
		colMap,
		[]tree.Datum{lowerDatum},
		keyPrefix,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode partition lower bound")
	}

	// Encode the upper bound key.
	upperKey, err := rowenc.EncodeColumns(
		[]descpb.ColumnID{partitionColID},
		primaryIndex.IndexDesc().KeyColumnDirections[:1],
		colMap,
		[]tree.Datum{upperDatum},
		keyPrefix,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to encode partition upper bound")
	}

	// Create the span from lower (inclusive) to upper (exclusive).
	span := roachpb.Span{
		Key:    lowerKey,
		EndKey: upperKey,
	}

	return []roachpb.Span{span}, nil
}

func init() {
	jobs.RegisterConstructor(jobspb.TypePartitionTTLScheduler, func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
		return &PartitionTTLMaintenanceResumer{
			Job: job,
			St:  settings,
		}
	}, jobs.UsesTenantCostControl)
}
