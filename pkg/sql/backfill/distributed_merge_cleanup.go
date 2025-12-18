// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package backfill

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/bulkutil"
	"github.com/cockroachdb/errors"
)

// DistributedMergeCleaner deletes distributed-merge artifacts (map outputs,
// merge outputs, and their parent directories). Cleanup operations are
// best-effort; callers should log returned errors rather than failing the
// job.
type DistributedMergeCleaner struct {
	mux *bulkutil.ExternalStorageMux
}

// NewDistributedMergeCleaner builds a cleaner that reuses external storage
// handles across delete/list operations.
func NewDistributedMergeCleaner(
	factory cloud.ExternalStorageFromURIFactory, user username.SQLUsername,
) *DistributedMergeCleaner {
	return &DistributedMergeCleaner{
		mux: bulkutil.NewExternalStorageMux(factory, user),
	}
}

// Close releases any cached external storage handles.
func (c *DistributedMergeCleaner) Close() error {
	if c == nil {
		return nil
	}
	return c.mux.Close()
}

// CleanupURIs deletes the provided URIs. The operation is best-effort; errors
// are aggregated and returned.
func (c *DistributedMergeCleaner) CleanupURIs(ctx context.Context, uris []string) error {
	if c == nil {
		return nil
	}
	var errOut error
	for _, uri := range uris {
		if uri == "" {
			continue
		}
		if err := c.mux.DeleteFile(ctx, uri); err != nil {
			errOut = errors.CombineErrors(errOut, err)
		}
	}
	return errOut
}

// CleanupManifests deletes the SSTs referenced by the provided manifests.
func (c *DistributedMergeCleaner) CleanupManifests(
	ctx context.Context, manifests []jobspb.IndexBackfillSSTManifest,
) error {
	return c.CleanupURIs(ctx, manifestURIs(manifests))
}

// CleanupJobDirectories enumerates all files under the job-scoped prefixes
// inferred from the provided URIs and removes them. This is intended as a
// catch-all sweep after targeted cleanup has already run.
func (c *DistributedMergeCleaner) CleanupJobDirectories(
	ctx context.Context, jobID jobspb.JobID, uriSources []string,
) error {
	if c == nil {
		return nil
	}
	prefixes := jobPrefixes(jobID, uriSources)
	var errOut error
	for _, prefix := range prefixes {
		listErr := c.mux.ListFiles(ctx, prefix, func(name string) error {
			trimmed := strings.TrimPrefix(name, "/")
			target := prefix
			if !strings.HasSuffix(target, "/") {
				target += "/"
			}
			target += trimmed
			if err := c.mux.DeleteFile(ctx, target); err != nil {
				errOut = errors.CombineErrors(errOut, err)
			}
			return nil
		})
		if errors.Is(listErr, cloud.ErrListingUnsupported) {
			continue
		}
		if listErr != nil {
			errOut = errors.CombineErrors(errOut, listErr)
		}
	}
	return errOut
}

// CleanupDistributedMergeJob sweeps distributed merge artifacts recorded in the
// job details. It is intended to be invoked when a job finishes or is
// canceled.
func CleanupDistributedMergeJob(
	ctx context.Context,
	factory cloud.ExternalStorageFromURIFactory,
	user username.SQLUsername,
	job *jobs.Job,
) error {
	details := job.Payload().GetNewSchemaChange()
	if details == nil || details.DistributedMergeMode != jobspb.IndexBackfillDistributedMergeMode_Enabled {
		return nil
	}

	cleaner := NewDistributedMergeCleaner(factory, user)
	defer cleaner.Close()

	uris := distributedMergeURIs(details)
	if len(uris) == 0 {
		return nil
	}

	var errOut error
	if err := cleaner.CleanupURIs(ctx, uris); err != nil {
		errOut = errors.CombineErrors(errOut, err)
	}
	if err := cleaner.CleanupJobDirectories(ctx, job.ID(), uris); err != nil {
		errOut = errors.CombineErrors(errOut, err)
	}
	return errOut
}

func distributedMergeURIs(details *jobspb.NewSchemaChangeDetails) []string {
	var uris []string
	for _, progress := range details.BackfillProgress {
		for _, m := range progress.SSTManifests {
			if m.URI == "" {
				continue
			}
			uris = append(uris, m.URI)
		}
	}
	return uris
}

func manifestURIs(manifests []jobspb.IndexBackfillSSTManifest) []string {
	out := make([]string, 0, len(manifests))
	for _, m := range manifests {
		if m.URI != "" {
			out = append(out, m.URI)
		}
	}
	return out
}

func jobPrefixes(jobID jobspb.JobID, uris []string) []string {
	jobToken := fmt.Sprintf("/job/%d/", jobID)
	seen := make(map[string]struct{})
	for _, uri := range uris {
		u, err := url.Parse(uri)
		if err != nil {
			continue
		}
		idx := strings.Index(u.Path, jobToken)
		if idx == -1 {
			continue
		}
		u.Path = u.Path[:idx+len(jobToken)]
		u.RawQuery = ""
		u.RawFragment = ""
		prefix := u.String()
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		seen[prefix] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for prefix := range seen {
		out = append(out, prefix)
	}
	return out
}
