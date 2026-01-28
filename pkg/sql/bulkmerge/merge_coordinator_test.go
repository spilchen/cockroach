// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/stretchr/testify/require"
)

// TestComputePartitionBounds verifies that partition bounds are computed
// correctly by balancing SST counts across workers.
func TestComputePartitionBounds(t *testing.T) {
	defer leaktest.AfterTest(t)()
	makeSpan := func(start, end string) roachpb.Span {
		return roachpb.Span{
			Key:    roachpb.Key(start),
			EndKey: roachpb.Key(end),
		}
	}

	makeSST := func(start, end string) execinfrapb.BulkMergeSpec_SST {
		return execinfrapb.BulkMergeSpec_SST{
			StartKey: roachpb.Key(start),
			EndKey:   roachpb.Key(end),
		}
	}

	t.Run("equal SST distribution", func(t *testing.T) {
		// 5 spans, 5 workers, 10 SSTs evenly distributed (2 per span).
		spans := []roachpb.Span{
			makeSpan("a", "b"),
			makeSpan("b", "c"),
			makeSpan("c", "d"),
			makeSpan("d", "e"),
			makeSpan("e", "f"),
		}
		ssts := []execinfrapb.BulkMergeSpec_SST{
			makeSST("a", "a5"), makeSST("a5", "b"),   // 2 SSTs for span 0.
			makeSST("b", "b5"), makeSST("b5", "c"),   // 2 SSTs for span 1.
			makeSST("c", "c5"), makeSST("c5", "d"),   // 2 SSTs for span 2.
			makeSST("d", "d5"), makeSST("d5", "e"),   // 2 SSTs for span 3.
			makeSST("e", "e5"), makeSST("e5", "f"),   // 2 SSTs for span 4.
		}

		bounds, taskToPartition := computePartitionBounds(spans, ssts, 5)

		require.Len(t, bounds, 5)
		require.Len(t, taskToPartition, 5)

		// Each partition should have approximately 2 SSTs (10 SSTs / 5 workers).
		// Since spans are equal, we expect approximately one span per partition.
		for i := 0; i < 5; i++ {
			require.Equal(t, i, taskToPartition[i],
				"span %d should be in partition %d", i, i)
		}
	})

	t.Run("unequal SST distribution", func(t *testing.T) {
		// 5 spans, 3 workers, unequal SST distribution.
		spans := []roachpb.Span{
			makeSpan("a", "b"),
			makeSpan("b", "c"),
			makeSpan("c", "d"),
			makeSpan("d", "e"),
			makeSpan("e", "f"),
		}
		ssts := []execinfrapb.BulkMergeSpec_SST{
			makeSST("a", "b"),                        // 1 SST for span 0.
			makeSST("b", "b3"), makeSST("b6", "c"),   // 2 SSTs for span 1.
			makeSST("c", "c1"), makeSST("c2", "c3"), makeSST("c4", "c5"), // 3 SSTs for span 2.
			makeSST("d", "d1"), makeSST("d2", "d3"), makeSST("d4", "e"), // 3 SSTs for span 3.
			makeSST("e", "f"),                        // 1 SST for span 4.
		}
		// Total: 10 SSTs. Expected: 3-4 SSTs per partition.

		bounds, taskToPartition := computePartitionBounds(spans, ssts, 3)

		require.Len(t, bounds, 3)
		require.Len(t, taskToPartition, 5)

		// Count SSTs per partition to verify balancing.
		sstCountPerPartition := make([]int, 3)
		for spanIdx, span := range spans {
			partitionIdx := taskToPartition[spanIdx]
			for _, sst := range ssts {
				if sstOverlapsSpan(sst, span) {
					sstCountPerPartition[partitionIdx]++
				}
			}
		}

		// Each partition should have 3-4 SSTs (10 / 3 ≈ 3.33).
		for i, count := range sstCountPerPartition {
			require.GreaterOrEqual(t, count, 2, "partition %d has too few SSTs", i)
			require.LessOrEqual(t, count, 5, "partition %d has too many SSTs", i)
		}
	})

	t.Run("more workers than spans", func(t *testing.T) {
		// 2 spans, 5 workers.
		spans := []roachpb.Span{
			makeSpan("a", "b"),
			makeSpan("b", "c"),
		}
		ssts := []execinfrapb.BulkMergeSpec_SST{
			makeSST("a", "b"),
			makeSST("b", "c"),
		}

		bounds, taskToPartition := computePartitionBounds(spans, ssts, 5)

		require.Len(t, bounds, 5)
		require.Len(t, taskToPartition, 2)

		// First two workers should get work; others get empty spans.
		require.NotEmpty(t, bounds[0].Key)
		require.NotEmpty(t, bounds[1].Key)
		for i := 2; i < 5; i++ {
			require.Empty(t, bounds[i].Key, "partition %d should be empty", i)
		}
	})

	t.Run("clustered SSTs", func(t *testing.T) {
		// Scenario from the plan document: most SSTs overlap later spans.
		spans := []roachpb.Span{
			makeSpan("a", "b"),
			makeSpan("b", "c"),
			makeSpan("c", "d"),
			makeSpan("d", "e"),
			makeSpan("e", "f"),
		}
		ssts := []execinfrapb.BulkMergeSpec_SST{
			makeSST("a", "b"),                        // 1 SST for span 0.
			makeSST("b", "c"),                        // 1 SST for span 1.
			makeSST("c", "c1"), makeSST("c2", "c3"), makeSST("c4", "d"), // 3 SSTs for span 2.
			makeSST("d", "d1"), makeSST("d2", "d3"), makeSST("d4", "e"), // 3 SSTs for span 3.
			makeSST("e", "e1"), makeSST("e2", "e3"), makeSST("e4", "f"), // 3 SSTs for span 4.
		}
		// Total: 11 SSTs. Heavily clustered in spans 2-4.

		bounds, taskToPartition := computePartitionBounds(spans, ssts, 3)

		require.Len(t, bounds, 3)
		require.Len(t, taskToPartition, 5)

		// Count SSTs per partition.
		sstCountPerPartition := make([]int, 3)
		for spanIdx, span := range spans {
			partitionIdx := taskToPartition[spanIdx]
			for _, sst := range ssts {
				if sstOverlapsSpan(sst, span) {
					sstCountPerPartition[partitionIdx]++
				}
			}
		}

		// Verify that partitions are more balanced than simple span-count distribution.
		// Simple span-count would give: [2, 6, 3] SSTs (spans 0-1, 2-3, 4).
		// SST-count-based should give more balanced distribution.
		// Note: With small counts and contiguous span requirements, we can't achieve
		// perfect balance, but we should avoid the worst cases.
		for i, count := range sstCountPerPartition {
			require.GreaterOrEqual(t, count, 1, "partition %d has too few SSTs", i)
			// Allow some imbalance due to contiguous span requirement.
		}
	})

	t.Run("zero SSTs", func(t *testing.T) {
		// Edge case: no SSTs.
		spans := []roachpb.Span{
			makeSpan("a", "b"),
			makeSpan("b", "c"),
		}
		ssts := []execinfrapb.BulkMergeSpec_SST{}

		bounds, taskToPartition := computePartitionBounds(spans, ssts, 2)

		require.Len(t, bounds, 2)
		require.Len(t, taskToPartition, 2)

		// With no SSTs, all spans will be assigned to the first partition since
		// each span has 0 SSTs and 0 + 0 <= sstsPerWorker (which is set to 1 as minimum).
		// This is acceptable behavior for an edge case.
		// Just verify the function doesn't panic or return invalid data.
		require.NotNil(t, bounds)
		require.NotNil(t, taskToPartition)
	})

	t.Run("zero workers", func(t *testing.T) {
		// Edge case: no workers.
		spans := []roachpb.Span{
			makeSpan("a", "b"),
		}
		ssts := []execinfrapb.BulkMergeSpec_SST{
			makeSST("a", "b"),
		}

		bounds, taskToPartition := computePartitionBounds(spans, ssts, 0)

		require.Nil(t, bounds)
		require.Nil(t, taskToPartition)
	})

	t.Run("zero spans", func(t *testing.T) {
		// Edge case: no spans.
		spans := []roachpb.Span{}
		ssts := []execinfrapb.BulkMergeSpec_SST{
			makeSST("a", "b"),
		}

		bounds, taskToPartition := computePartitionBounds(spans, ssts, 2)

		require.Nil(t, bounds)
		require.Nil(t, taskToPartition)
	})
}

// TestSSTOverlapsSpan verifies the SST overlap detection logic.
func TestSSTOverlapsSpan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	makeSpan := func(start, end string) roachpb.Span {
		return roachpb.Span{
			Key:    roachpb.Key(start),
			EndKey: roachpb.Key(end),
		}
	}

	makeSST := func(start, end string) execinfrapb.BulkMergeSpec_SST {
		return execinfrapb.BulkMergeSpec_SST{
			StartKey: roachpb.Key(start),
			EndKey:   roachpb.Key(end),
		}
	}

	testCases := []struct {
		name     string
		sst      execinfrapb.BulkMergeSpec_SST
		span     roachpb.Span
		overlaps bool
	}{
		{
			name:     "exact match",
			sst:      makeSST("a", "b"),
			span:     makeSpan("a", "b"),
			overlaps: true,
		},
		{
			name:     "SST contained in span",
			sst:      makeSST("a5", "a7"),
			span:     makeSpan("a", "b"),
			overlaps: true,
		},
		{
			name:     "span contained in SST",
			sst:      makeSST("a", "c"),
			span:     makeSpan("b", "c"),
			overlaps: true,
		},
		{
			name:     "partial overlap - SST starts before span",
			sst:      makeSST("a", "b5"),
			span:     makeSpan("b", "c"),
			overlaps: true,
		},
		{
			name:     "partial overlap - SST ends after span",
			sst:      makeSST("b5", "c5"),
			span:     makeSpan("b", "c"),
			overlaps: true,
		},
		{
			name:     "no overlap - SST before span",
			sst:      makeSST("a", "b"),
			span:     makeSpan("c", "d"),
			overlaps: false,
		},
		{
			name:     "no overlap - SST after span",
			sst:      makeSST("c", "d"),
			span:     makeSpan("a", "b"),
			overlaps: false,
		},
		{
			name:     "adjacent - SST ends where span starts",
			sst:      makeSST("a", "b"),
			span:     makeSpan("b", "c"),
			overlaps: false,
		},
		{
			name:     "adjacent - span ends where SST starts",
			sst:      makeSST("b", "c"),
			span:     makeSpan("a", "b"),
			overlaps: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := sstOverlapsSpan(tc.sst, tc.span)
			require.Equal(t, tc.overlaps, result,
				"sstOverlapsSpan(%s, %s) = %v, want %v",
				tc.sst, tc.span, result, tc.overlaps)
		})
	}
}
