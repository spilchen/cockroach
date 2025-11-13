// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttlpartition

import (
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/duration"
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

// TestExtractTimestamp tests the extractTimestamp helper function.
func TestExtractTimestamp(t *testing.T) {
	testCases := []struct {
		name          string
		datum         tree.Datum
		expected      time.Time
		expectError   bool
		errorContains string
	}{
		{
			name:        "DTimestamp",
			datum:       &tree.DTimestamp{Time: time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)},
			expected:    time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC),
			expectError: false,
		},
		{
			name:        "DTimestampTZ",
			datum:       &tree.DTimestampTZ{Time: time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)},
			expected:    time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC),
			expectError: false,
		},
		{
			name:          "Invalid type - DString",
			datum:         tree.NewDString("not a timestamp"),
			expectError:   true,
			errorContains: "expected TIMESTAMP or TIMESTAMPTZ",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := extractTimestamp(tc.datum)

			if tc.expectError {
				require.Error(t, err)
				if tc.errorContains != "" {
					require.Contains(t, err.Error(), tc.errorContains)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expected, result)
			}
		})
	}
}

// TestTruncateToGranularity tests the truncateToGranularity helper function.
func TestTruncateToGranularity(t *testing.T) {
	testCases := []struct {
		name        string
		input       time.Time
		granularity string
		expected    time.Time
	}{
		{
			name:        "1 day granularity",
			input:       time.Date(2025, 1, 15, 14, 30, 45, 0, time.UTC),
			granularity: "1 day",
			expected:    time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name:        "7 day granularity",
			input:       time.Date(2025, 1, 15, 14, 30, 45, 0, time.UTC),
			granularity: "7 days",
			expected:    time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			granularityInterval, err := tree.ParseDInterval(duration.IntervalStyle_POSTGRES, tc.granularity)
			require.NoError(t, err)

			result := truncateToGranularity(tc.input, granularityInterval.Duration)
			require.Equal(t, tc.expected, result)
		})
	}
}

// TestFormatPartitionName tests the formatPartitionName helper function.
func TestFormatPartitionName(t *testing.T) {
	testCases := []struct {
		name     string
		input    time.Time
		expected string
	}{
		{
			name:     "2025-01-15",
			input:    time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
			expected: "p20250115",
		},
		{
			name:     "2024-12-31",
			input:    time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC),
			expected: "p20241231",
		},
		{
			name:     "2025-02-01",
			input:    time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
			expected: "p20250201",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := formatPartitionName(tc.input)
			require.Equal(t, tc.expected, result)
		})
	}
}
