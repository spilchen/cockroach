// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge_test

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/sql/bulkmerge"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlinstance"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlliveness"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

// TestValidateRequiredInstancesAvailableWithRetryTracking tests the instance
// availability validation logic used before starting a distributed merge.
func TestValidateRequiredInstancesAvailableWithRetryTracking(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	srv := serverutils.StartServerOnly(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)

	s := srv.ApplicationLayer()
	internalDB := s.InternalDB().(isql.DB)

	t.Run("empty sst list returns nil", func(t *testing.T) {
		availCtx := bulkmerge.InstanceAvailabilityContext{
			JobID:      jobspb.JobID(1),
			InternalDB: internalDB,
			Settings:   s.ClusterSettings(),
		}

		err := bulkmerge.ValidateRequiredInstancesAvailableWithRetryTracking(
			ctx, availCtx, nil, nil,
		)
		require.NoError(t, err)
	})

	t.Run("all instances available returns nil", func(t *testing.T) {
		// Create SSTs with URIs for instance 1.
		ssts := []execinfrapb.BulkMergeSpec_SST{
			{URI: "nodelocal://1/job/123/map/file1.sst"},
			{URI: "nodelocal://1/job/123/map/file2.sst"},
		}

		// Create available instances list including instance 1.
		availableInstances := []sqlinstance.InstanceInfo{
			{InstanceID: base.SQLInstanceID(1), SessionID: sqlliveness.SessionID("session1")},
			{InstanceID: base.SQLInstanceID(2), SessionID: sqlliveness.SessionID("session2")},
		}

		availCtx := bulkmerge.InstanceAvailabilityContext{
			JobID:      jobspb.JobID(1),
			InternalDB: internalDB,
			Settings:   s.ClusterSettings(),
		}

		err := bulkmerge.ValidateRequiredInstancesAvailableWithRetryTracking(
			ctx, availCtx, ssts, availableInstances,
		)
		require.NoError(t, err)
	})

	t.Run("missing instance returns retry error", func(t *testing.T) {
		// Create SSTs with URIs for instance 2 (which is not available).
		ssts := []execinfrapb.BulkMergeSpec_SST{
			{URI: "nodelocal://2/job/123/map/file1.sst"},
		}

		// Create available instances list NOT including instance 2.
		availableInstances := []sqlinstance.InstanceInfo{
			{InstanceID: base.SQLInstanceID(1), SessionID: sqlliveness.SessionID("session1")},
		}

		availCtx := bulkmerge.InstanceAvailabilityContext{
			JobID:      jobspb.JobID(1),
			InternalDB: internalDB,
			Settings:   s.ClusterSettings(),
		}

		err := bulkmerge.ValidateRequiredInstancesAvailableWithRetryTracking(
			ctx, availCtx, ssts, availableInstances,
		)
		require.Error(t, err)
		require.True(t, jobs.IsRetryJobError(err), "expected retry error, got: %v", err)
		require.Contains(t, err.Error(), "temporarily unavailable")
	})

	t.Run("non-nodelocal URIs are ignored", func(t *testing.T) {
		// Create SSTs with non-nodelocal URIs.
		ssts := []execinfrapb.BulkMergeSpec_SST{
			{URI: "s3://bucket/job/123/map/file1.sst"},
			{URI: "gs://bucket/job/123/map/file2.sst"},
			{URI: ""}, // empty URI
		}

		// No instances available, but it should not matter.
		availableInstances := []sqlinstance.InstanceInfo{}

		availCtx := bulkmerge.InstanceAvailabilityContext{
			JobID:      jobspb.JobID(1),
			InternalDB: internalDB,
			Settings:   s.ClusterSettings(),
		}

		err := bulkmerge.ValidateRequiredInstancesAvailableWithRetryTracking(
			ctx, availCtx, ssts, availableInstances,
		)
		require.NoError(t, err)
	})

	t.Run("instance 0 (self) is ignored", func(t *testing.T) {
		// Create SSTs with instance 0 (self) URI.
		ssts := []execinfrapb.BulkMergeSpec_SST{
			{URI: "nodelocal://0/job/123/map/file1.sst"},
		}

		// No instances in list.
		availableInstances := []sqlinstance.InstanceInfo{}

		availCtx := bulkmerge.InstanceAvailabilityContext{
			JobID:      jobspb.JobID(1),
			InternalDB: internalDB,
			Settings:   s.ClusterSettings(),
		}

		err := bulkmerge.ValidateRequiredInstancesAvailableWithRetryTracking(
			ctx, availCtx, ssts, availableInstances,
		)
		require.NoError(t, err)
	})
}
