// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"context"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/cloud/nodelocal"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlinstance"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

const (
	// instanceUnavailabilityInfoKey is the job_info key used to persist
	// instance unavailability tracking state across job restarts.
	instanceUnavailabilityInfoKey = "bulkmerge/instance_unavailability"
)

// InstanceAvailabilityContext provides the dependencies needed to check
// instance availability and track unavailability state.
type InstanceAvailabilityContext struct {
	JobID      jobspb.JobID
	InternalDB isql.DB
	Settings   *cluster.Settings
}

// ValidateRequiredInstancesAvailableWithRetryTracking checks that all SQL
// instances referenced in SST URIs are available. This is called before
// starting a distributed merge and uses SQL liveness plus a time+retry-limit
// heuristic to decide whether to keep retrying or fail permanently.
//
// Returns:
//   - nil if all required instances are available
//   - jobs.MarkAsRetryJobError if any required instance is unavailable
//   - jobs.MarkAsPermanentJobError only when the retry threshold is exceeded
//     (time/max retries). We do not rely on system-tenant-only signals here.
func ValidateRequiredInstancesAvailableWithRetryTracking(
	ctx context.Context,
	availCtx InstanceAvailabilityContext,
	ssts []execinfrapb.BulkMergeSpec_SST,
	availableInstances []sqlinstance.InstanceInfo,
) error {
	if len(ssts) == 0 {
		return nil
	}

	// Extract required instance IDs from SST URIs.
	requiredInstances, err := extractRequiredInstances(ssts)
	if err != nil {
		// Error parsing URIs is a transient issue; retry.
		return jobs.MarkAsRetryJobError(
			errors.Wrap(err, "parsing SST URIs for instance availability check"))
	}

	if len(requiredInstances) == 0 {
		// No nodelocal URIs found; nothing to check.
		return nil
	}

	// Build a set of available instance IDs.
	availableSet := make(map[base.SQLInstanceID]struct{}, len(availableInstances))
	for _, inst := range availableInstances {
		availableSet[inst.InstanceID] = struct{}{}
	}

	// Find unavailable instances.
	var unavailable []int32
	for instanceID := range requiredInstances {
		if _, ok := availableSet[instanceID]; !ok {
			unavailable = append(unavailable, int32(instanceID))
		}
	}

	if len(unavailable) == 0 {
		// All instances available; clear any existing tracking state.
		// Failure to clear state is not fatal; we just proceed anyway since
		// the job can continue. The state will be cleared on a future successful
		// attempt or ignored if no unavailability occurs.
		_ = clearUnavailabilityState(ctx, availCtx)
		return nil
	}

	// Some instances are unavailable. Track the failure and decide whether
	// to retry or fail permanently.
	return handleUnavailableInstances(ctx, availCtx, unavailable)
}

// extractRequiredInstances parses SST URIs and returns a set of unique
// SQLInstanceIDs that own nodelocal storage. Non-nodelocal URIs are ignored.
func extractRequiredInstances(
	ssts []execinfrapb.BulkMergeSpec_SST,
) (map[base.SQLInstanceID]struct{}, error) {
	required := make(map[base.SQLInstanceID]struct{})
	for _, sst := range ssts {
		if sst.URI == "" {
			continue
		}
		// Only check nodelocal URIs; skip cloud storage URIs.
		if !strings.HasPrefix(sst.URI, "nodelocal://") {
			continue
		}
		instanceID, err := nodelocal.ParseInstanceID(sst.URI)
		if err != nil {
			return nil, errors.Wrapf(err, "parsing instance ID from URI %q", sst.URI)
		}
		// Instance ID 0 means "self" (local instance), which is always available.
		if instanceID != 0 {
			required[instanceID] = struct{}{}
		}
	}
	return required, nil
}

// handleUnavailableInstances loads existing tracking state, updates it,
// and decides whether to return a retry or permanent error.
func handleUnavailableInstances(
	ctx context.Context, availCtx InstanceAvailabilityContext, unavailable []int32,
) error {
	now := timeutil.Now()

	// Load existing state or create new.
	state, err := loadUnavailabilityState(ctx, availCtx)
	if err != nil {
		// Error loading state is transient; retry.
		return jobs.MarkAsRetryJobError(
			errors.Wrap(err, "loading instance unavailability state"))
	}

	// Initialize state on first failure.
	if state.FirstFailureNanos == 0 {
		state.FirstFailureNanos = now.UnixNano()
	}

	// Update state.
	state.RetryCount++
	state.UnavailableInstances = unavailable
	state.LastCheckNanos = now.UnixNano()

	// Check thresholds.
	timeout := InstanceUnavailabilityTimeout.Get(&availCtx.Settings.SV)
	maxRetries := InstanceUnavailabilityMaxRetries.Get(&availCtx.Settings.SV)

	elapsed := now.Sub(time.Unix(0, state.FirstFailureNanos))
	timeoutExceeded := elapsed > timeout
	retriesExceeded := maxRetries > 0 && int64(state.RetryCount) > maxRetries

	// Permanent failure only if both thresholds are exceeded (or only timeout
	// if max_retries is 0, meaning unlimited retries based on retry count).
	if timeoutExceeded && (maxRetries == 0 || retriesExceeded) {
		// Don't save state on permanent failure; job will be cleaned up.
		return jobs.MarkAsPermanentJobError(errors.Newf(
			"distributed merge has been waiting for unavailable instances %v for %s "+
				"(threshold: %s, retries: %d); the temporary SST files may be permanently "+
				"lost and the job cannot complete",
			unavailable, elapsed.Round(time.Second), timeout, state.RetryCount))
	}

	// Save updated state.
	if err := saveUnavailabilityState(ctx, availCtx, state); err != nil {
		// Error saving state is transient; retry anyway.
		return jobs.MarkAsRetryJobError(
			errors.Wrap(err, "saving instance unavailability state"))
	}

	return jobs.MarkAsRetryJobError(errors.Newf(
		"distributed merge requires SST files from instances %v which are temporarily "+
			"unavailable (failing for %s, retry %d); the job will retry",
		unavailable, elapsed.Round(time.Second), state.RetryCount))
}

// loadUnavailabilityState loads the tracking state from job_info.
// Returns an empty state if none exists.
func loadUnavailabilityState(
	ctx context.Context, availCtx InstanceAvailabilityContext,
) (*InstanceUnavailabilityState, error) {
	var state InstanceUnavailabilityState

	if err := availCtx.InternalDB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		infoStorage := jobs.InfoStorageForJob(txn, availCtx.JobID)
		found, err := infoStorage.GetProto(ctx, instanceUnavailabilityInfoKey, &state)
		if err != nil {
			return err
		}
		if !found {
			state = InstanceUnavailabilityState{}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &state, nil
}

// saveUnavailabilityState persists the tracking state to job_info.
func saveUnavailabilityState(
	ctx context.Context, availCtx InstanceAvailabilityContext, state *InstanceUnavailabilityState,
) error {
	return availCtx.InternalDB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		infoStorage := jobs.InfoStorageForJob(txn, availCtx.JobID)
		return infoStorage.WriteProto(ctx, instanceUnavailabilityInfoKey, state)
	})
}

// clearUnavailabilityState removes the tracking state from job_info.
// Called when all instances become available to reset the failure window.
func clearUnavailabilityState(ctx context.Context, availCtx InstanceAvailabilityContext) error {
	return availCtx.InternalDB.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		infoStorage := jobs.InfoStorageForJob(txn, availCtx.JobID)
		return infoStorage.Delete(ctx, instanceUnavailabilityInfoKey)
	})
}
