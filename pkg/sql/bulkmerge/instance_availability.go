// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"context"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/cloud/nodelocal"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlinstance"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlliveness"
	"github.com/cockroachdb/errors"
)

// ValidateRequiredInstancesAvailable checks that all SQL instances referenced
// in SST URIs are available. This is called before starting a distributed merge
// to detect permanently unavailable instances early.
//
// Returns:
//   - nil if all required instances are available
//   - jobs.MarkAsRetryJobError for transient errors (e.g., liveness check fails)
//   - jobs.MarkAsPermanentJobError if any instance is permanently gone
func ValidateRequiredInstancesAvailable(
	ctx context.Context,
	ssts []execinfrapb.BulkMergeSpec_SST,
	availableInstances []sqlinstance.InstanceInfo,
	livenessReader sqlliveness.Reader,
) error {
	if len(ssts) == 0 {
		return nil
	}

	// Extract unique SQLInstanceIDs from nodelocal URIs.
	requiredInstances := make(map[base.SQLInstanceID]struct{})
	for _, sst := range ssts {
		// Only check nodelocal URIs. Other storage schemes (S3, GCS, etc.) are
		// not tied to specific SQL instances.
		if !strings.HasPrefix(sst.URI, "nodelocal://") {
			continue
		}
		instanceID, err := nodelocal.ParseInstanceID(sst.URI)
		if err != nil {
			// If we can't parse the URI, it's likely a malformed nodelocal URI.
			// Treat this as a permanent error since we can't verify instance
			// availability.
			return jobs.MarkAsPermanentJobError(
				errors.Wrapf(err, "invalid nodelocal URI in SST manifest"))
		}
		requiredInstances[instanceID] = struct{}{}
	}

	if len(requiredInstances) == 0 {
		// No nodelocal URIs to check.
		return nil
	}

	// Build a map from available instances: instanceID -> SessionID.
	instanceToSession := make(map[base.SQLInstanceID]sqlliveness.SessionID, len(availableInstances))
	for _, inst := range availableInstances {
		instanceToSession[inst.InstanceID] = inst.SessionID
	}

	// Check each required instance.
	for instanceID := range requiredInstances {
		sessionID, found := instanceToSession[instanceID]
		if !found {
			// Instance is not in the registry - it's permanently gone.
			return jobs.MarkAsPermanentJobError(errors.Newf(
				"distributed merge requires SST files from SQL instance %d which no longer exists; "+
					"the temporary SST files are permanently lost and the job cannot complete",
				instanceID))
		}

		// Check if the session is still alive.
		isAlive, err := livenessReader.IsAlive(ctx, sessionID)
		if err != nil {
			// Transient error checking liveness - retry the job.
			return jobs.MarkAsRetryJobError(errors.Wrapf(err,
				"error checking liveness of SQL instance %d; retrying", instanceID))
		}

		if !isAlive {
			// Session has expired - instance is permanently gone.
			return jobs.MarkAsPermanentJobError(errors.Newf(
				"distributed merge requires SST files from SQL instance %d whose session has expired; "+
					"the temporary SST files are permanently lost and the job cannot complete",
				instanceID))
		}
	}

	return nil
}
