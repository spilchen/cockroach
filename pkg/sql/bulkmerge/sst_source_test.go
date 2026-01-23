// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestExtractSourceInstanceID(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testCases := []struct {
		name        string
		uri         string
		expected    base.SQLInstanceID
		expectError bool
	}{
		{
			name:        "valid nodelocal URI with instance 1",
			uri:         "nodelocal://1/path/to/file.sst",
			expected:    1,
			expectError: false,
		},
		{
			name:        "valid nodelocal URI with instance 42",
			uri:         "nodelocal://42/job/123/merge/file.sst",
			expected:    42,
			expectError: false,
		},
		{
			name:        "valid nodelocal URI with high instance ID",
			uri:         "nodelocal://999/some/path.sst",
			expected:    999,
			expectError: false,
		},
		{
			name:        "nodelocal with self host - error",
			uri:         "nodelocal://self/path",
			expected:    0,
			expectError: true,
		},
		{
			name:        "nodelocal with empty host - error",
			uri:         "nodelocal:///path",
			expected:    0,
			expectError: true,
		},
		{
			name:        "s3 URI - error (not nodelocal)",
			uri:         "s3://bucket/path",
			expected:    0,
			expectError: true,
		},
		{
			name:        "gs URI - error (not nodelocal)",
			uri:         "gs://bucket/path",
			expected:    0,
			expectError: true,
		},
		{
			name:        "invalid URI - error",
			uri:         "://invalid",
			expected:    0,
			expectError: true,
		},
		{
			name:        "nodelocal with non-numeric host - error",
			uri:         "nodelocal://abc/path",
			expected:    0,
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractSourceInstanceID(tc.uri)
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expected, got)
			}
		})
	}
}
