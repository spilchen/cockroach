// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttlpartition

import (
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/stretchr/testify/require"
)

// TestComputePartitionWindow tests the computePartitionWindow function.
func TestComputePartitionWindow(t *testing.T) {
	r := &partitionTTLMaintenanceResumer{}

	testCases := []struct {
		name           string
		config         *catpb.PartitionTTLConfig
		now            time.Time
		expectedWindow partitionWindow
		expectError    bool
		errorContains  string
	}{
		{
			name: "30 day retention, 2 day lookahead",
			config: &catpb.PartitionTTLConfig{
				Retention: "30 days",
				Lookahead: "2 days",
			},
			now: time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC),
			expectedWindow: partitionWindow{
				retentionStart: time.Date(2024, 12, 16, 12, 0, 0, 0, time.UTC), // 30 days ago
				lookaheadEnd:   time.Date(2025, 1, 17, 12, 0, 0, 0, time.UTC),  // 2 days ahead
			},
			expectError: false,
		},
		{
			name: "7 day retention, 1 day lookahead",
			config: &catpb.PartitionTTLConfig{
				Retention: "7 days",
				Lookahead: "1 day",
			},
			now: time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
			expectedWindow: partitionWindow{
				retentionStart: time.Date(2025, 1, 8, 0, 0, 0, 0, time.UTC),  // 7 days ago
				lookaheadEnd:   time.Date(2025, 1, 16, 0, 0, 0, 0, time.UTC), // 1 day ahead
			},
			expectError: false,
		},
		{
			name: "invalid retention format",
			config: &catpb.PartitionTTLConfig{
				Retention: "invalid",
				Lookahead: "2 days",
			},
			now:           time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
			expectError:   true,
			errorContains: "failed to parse retention",
		},
		{
			name: "invalid lookahead format",
			config: &catpb.PartitionTTLConfig{
				Retention: "30 days",
				Lookahead: "invalid",
			},
			now:           time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
			expectError:   true,
			errorContains: "failed to parse lookahead",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			window, err := r.computePartitionWindow(tc.config, tc.now)

			if tc.expectError {
				require.Error(t, err)
				if tc.errorContains != "" {
					require.Contains(t, err.Error(), tc.errorContains)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expectedWindow.retentionStart, window.retentionStart)
				require.Equal(t, tc.expectedWindow.lookaheadEnd, window.lookaheadEnd)
			}
		})
	}
}

// TestComputePartitionChanges tests the computePartitionChanges function.
// Note: This is currently a stub test since the partition bound decoding
// is not yet implemented.
func TestComputePartitionChanges(t *testing.T) {
	t.Skip("Partition bound decoding not yet implemented")

	// TODO: Add test cases once partition bound decoding is implemented.
	// Test cases should cover:
	// - Dropping expired partitions
	// - Creating new partitions for coverage
	// - Handling edge cases (no partitions, all partitions expired, etc.)
}
