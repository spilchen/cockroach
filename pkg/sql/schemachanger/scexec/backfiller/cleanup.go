// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package backfiller

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/bulkutil"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// phaseTransition represents a detected phase change in the distributed merge
// pipeline after checkpoint persistence.
type phaseTransition struct {
	TableID  descpb.ID
	OldPhase int32
	NewPhase int32
}

// phaseTransitionCleaner orchestrates SST cleanup for phase transitions.
type phaseTransitionCleaner struct {
	jobID                  jobspb.JobID
	externalStorageFactory cloud.ExternalStorageFromURIFactory
	getStoragePrefixes     func(descpb.ID) []string
}

// cleanupTransition performs SST cleanup for a single phase transition.
func (c *phaseTransitionCleaner) cleanupTransition(
	ctx context.Context, transition phaseTransition,
) error {
	log.Dev.Infof(ctx, "triggering SST cleanup for job %d, table %d after phase %d→%d transition",
		c.jobID, transition.TableID, transition.OldPhase, transition.NewPhase)

	// Determine which subdirectory to clean up based on completed phase:
	// - newPhase=1: cleanup map/ (map phase output, input to iteration 1)
	// - newPhase=2: cleanup merge/iter-1/ (iteration 1 output, input to iteration 2)
	subdirectory := bulkutil.NewDistMergePaths(c.jobID).InputSubdir(int(transition.NewPhase))

	// Get storage prefixes from closure (accesses job payload).
	storagePrefixes := c.getStoragePrefixes(transition.TableID)
	if len(storagePrefixes) == 0 {
		log.Dev.Infof(ctx, "no storage prefixes found, skipping cleanup")
		return nil
	}

	// Create cleaner and perform cleanup.
	cleaner := bulkutil.NewBulkJobCleaner(
		c.externalStorageFactory,
		username.NodeUserName(),
	)
	defer func() {
		if err := cleaner.Close(); err != nil {
			log.Ops.Warningf(ctx, "error closing bulk job cleaner: %v", err)
		}
	}()

	if err := cleaner.CleanupJobSubdirectory(ctx, c.jobID, storagePrefixes, subdirectory); err != nil {
		return errors.Wrapf(err, "cleaning up subdirectory %s", subdirectory)
	}

	log.Dev.Infof(ctx, "successfully cleaned up SSTs in %s", subdirectory)
	return nil
}
