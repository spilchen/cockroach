// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scexec

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/backfill"
)

// DetermineDistributedMergeMode returns the per-job distributed merge mode for
// declarative schema change backfills based on the cluster settings.
func DetermineDistributedMergeMode(
	ctx context.Context, st *cluster.Settings,
) (jobspb.IndexBackfillDistributedMergeMode, error) {
	if st == nil {
		return jobspb.IndexBackfillDistributedMergeMode_INDEX_BACKFILL_DISTRIBUTED_MERGE_MODE_DISABLED, nil
	}
	useDistributedMerge, err := backfill.ShouldEnableDistributedMergeIndexBackfill(
		ctx, st, backfill.DistributedMergeConsumerDeclarative,
	)
	if err != nil {
		return jobspb.IndexBackfillDistributedMergeMode_INDEX_BACKFILL_DISTRIBUTED_MERGE_MODE_DISABLED, err
	}
	if useDistributedMerge {
		return jobspb.IndexBackfillDistributedMergeMode_INDEX_BACKFILL_DISTRIBUTED_MERGE_MODE_ENABLED, nil
	}
	return jobspb.IndexBackfillDistributedMergeMode_INDEX_BACKFILL_DISTRIBUTED_MERGE_MODE_DISABLED, nil
}
