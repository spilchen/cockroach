// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlinstance"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlliveness"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// mockLivenessReader implements sqlliveness.Reader for testing.
type mockLivenessReader struct {
	aliveSessions map[sqlliveness.SessionID]bool
	errorOnCheck  map[sqlliveness.SessionID]error
}

func newMockLivenessReader() *mockLivenessReader {
	return &mockLivenessReader{
		aliveSessions: make(map[sqlliveness.SessionID]bool),
		errorOnCheck:  make(map[sqlliveness.SessionID]error),
	}
}

func (m *mockLivenessReader) setAlive(sessionID sqlliveness.SessionID, alive bool) {
	m.aliveSessions[sessionID] = alive
}

func (m *mockLivenessReader) setError(sessionID sqlliveness.SessionID, err error) {
	m.errorOnCheck[sessionID] = err
}

func (m *mockLivenessReader) IsAlive(
	_ context.Context, sessionID sqlliveness.SessionID,
) (bool, error) {
	if err, ok := m.errorOnCheck[sessionID]; ok {
		return false, err
	}
	alive, ok := m.aliveSessions[sessionID]
	if !ok {
		// Default to alive if not explicitly set.
		return true, nil
	}
	return alive, nil
}

// makeSSTs creates test SST specs with the given URIs.
func makeSSTs(uris ...string) []execinfrapb.BulkMergeSpec_SST {
	ssts := make([]execinfrapb.BulkMergeSpec_SST, len(uris))
	for i, uri := range uris {
		ssts[i] = execinfrapb.BulkMergeSpec_SST{URI: uri}
	}
	return ssts
}

// makeInstances creates test InstanceInfo structs.
func makeInstances(ids ...base.SQLInstanceID) []sqlinstance.InstanceInfo {
	instances := make([]sqlinstance.InstanceInfo, len(ids))
	for i, id := range ids {
		instances[i] = sqlinstance.InstanceInfo{
			InstanceID: id,
			SessionID:  sqlliveness.SessionID(string(rune('A' + i))),
		}
	}
	return instances
}

func TestValidateRequiredInstancesAvailable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	t.Run("empty SST list returns nil", func(t *testing.T) {
		reader := newMockLivenessReader()
		err := ValidateRequiredInstancesAvailable(ctx, nil, nil, reader)
		require.NoError(t, err)
	})

	t.Run("all instances alive returns nil", func(t *testing.T) {
		ssts := makeSSTs(
			"nodelocal://1/job/123/file1.sst",
			"nodelocal://2/job/123/file2.sst",
		)
		instances := makeInstances(1, 2, 3)
		reader := newMockLivenessReader()
		for _, inst := range instances {
			reader.setAlive(inst.SessionID, true)
		}

		err := ValidateRequiredInstancesAvailable(ctx, ssts, instances, reader)
		require.NoError(t, err)
	})

	t.Run("instance not in registry returns permanent error", func(t *testing.T) {
		ssts := makeSSTs(
			"nodelocal://1/job/123/file1.sst",
			"nodelocal://5/job/123/file2.sst", // Instance 5 not in registry.
		)
		instances := makeInstances(1, 2, 3)
		reader := newMockLivenessReader()
		for _, inst := range instances {
			reader.setAlive(inst.SessionID, true)
		}

		err := ValidateRequiredInstancesAvailable(ctx, ssts, instances, reader)
		require.Error(t, err)
		require.True(t, jobs.IsPermanentJobError(err), "expected permanent error, got: %v", err)
		require.Contains(t, err.Error(), "instance 5 which no longer exists")
	})

	t.Run("session expired returns permanent error", func(t *testing.T) {
		ssts := makeSSTs(
			"nodelocal://1/job/123/file1.sst",
			"nodelocal://2/job/123/file2.sst",
		)
		instances := makeInstances(1, 2)
		reader := newMockLivenessReader()
		reader.setAlive(instances[0].SessionID, true)
		reader.setAlive(instances[1].SessionID, false) // Session expired.

		err := ValidateRequiredInstancesAvailable(ctx, ssts, instances, reader)
		require.Error(t, err)
		require.True(t, jobs.IsPermanentJobError(err), "expected permanent error, got: %v", err)
		require.Contains(t, err.Error(), "session has expired")
	})

	t.Run("liveness check error returns retry error", func(t *testing.T) {
		ssts := makeSSTs("nodelocal://1/job/123/file1.sst")
		instances := makeInstances(1)
		reader := newMockLivenessReader()
		reader.setError(instances[0].SessionID, errors.New("transient liveness error"))

		err := ValidateRequiredInstancesAvailable(ctx, ssts, instances, reader)
		require.Error(t, err)
		require.True(t, jobs.IsRetryJobError(err), "expected retry error, got: %v", err)
		require.Contains(t, err.Error(), "error checking liveness")
	})

	t.Run("non-nodelocal URIs are ignored", func(t *testing.T) {
		ssts := makeSSTs(
			"s3://bucket/file1.sst",
			"gs://bucket/file2.sst",
			"azure://container/file3.sst",
		)
		// No instances available, but should still succeed since all URIs are
		// non-nodelocal.
		reader := newMockLivenessReader()

		err := ValidateRequiredInstancesAvailable(ctx, ssts, nil, reader)
		require.NoError(t, err)
	})

	t.Run("mixed URIs only check nodelocal", func(t *testing.T) {
		ssts := makeSSTs(
			"s3://bucket/file1.sst",
			"nodelocal://1/job/123/file2.sst",
			"gs://bucket/file3.sst",
		)
		instances := makeInstances(1)
		reader := newMockLivenessReader()
		reader.setAlive(instances[0].SessionID, true)

		err := ValidateRequiredInstancesAvailable(ctx, ssts, instances, reader)
		require.NoError(t, err)
	})

	t.Run("duplicate instance IDs in SSTs checked once", func(t *testing.T) {
		ssts := makeSSTs(
			"nodelocal://1/job/123/file1.sst",
			"nodelocal://1/job/123/file2.sst",
			"nodelocal://1/job/123/file3.sst",
		)
		instances := makeInstances(1)
		reader := newMockLivenessReader()
		reader.setAlive(instances[0].SessionID, true)

		err := ValidateRequiredInstancesAvailable(ctx, ssts, instances, reader)
		require.NoError(t, err)
	})
}
