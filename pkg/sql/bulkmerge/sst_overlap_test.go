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
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestSSTOverlapsSpan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testCases := []struct {
		name     string
		sst      execinfrapb.BulkMergeSpec_SST
		span     roachpb.Span
		expected bool
	}{
		{
			name: "SST fully within span",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("b"),
				EndKey:   []byte("c"),
			},
			span:     roachpb.Span{Key: []byte("a"), EndKey: []byte("d")},
			expected: true,
		},
		{
			name: "span fully within SST",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("a"),
				EndKey:   []byte("d"),
			},
			span:     roachpb.Span{Key: []byte("b"), EndKey: []byte("c")},
			expected: true,
		},
		{
			name: "SST overlaps span start",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("a"),
				EndKey:   []byte("c"),
			},
			span:     roachpb.Span{Key: []byte("b"), EndKey: []byte("d")},
			expected: true,
		},
		{
			name: "SST overlaps span end",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("c"),
				EndKey:   []byte("e"),
			},
			span:     roachpb.Span{Key: []byte("b"), EndKey: []byte("d")},
			expected: true,
		},
		{
			name: "SST before span - no overlap",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("a"),
				EndKey:   []byte("b"),
			},
			span:     roachpb.Span{Key: []byte("c"), EndKey: []byte("d")},
			expected: false,
		},
		{
			name: "SST after span - no overlap",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("e"),
				EndKey:   []byte("f"),
			},
			span:     roachpb.Span{Key: []byte("c"), EndKey: []byte("d")},
			expected: false,
		},
		{
			name: "SST end equals span start - overlap (SST EndKey is inclusive)",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("a"),
				EndKey:   []byte("c"),
			},
			span:     roachpb.Span{Key: []byte("c"), EndKey: []byte("d")},
			expected: true,
		},
		{
			name: "SST start equals span end - no overlap (span EndKey is exclusive)",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("d"),
				EndKey:   []byte("e"),
			},
			span:     roachpb.Span{Key: []byte("c"), EndKey: []byte("d")},
			expected: false,
		},
		{
			name: "SST with no key metadata - assume overlap",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: nil,
				EndKey:   nil,
			},
			span:     roachpb.Span{Key: []byte("c"), EndKey: []byte("d")},
			expected: true,
		},
		{
			name: "SST with only StartKey",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("c"),
				EndKey:   nil,
			},
			span:     roachpb.Span{Key: []byte("a"), EndKey: []byte("d")},
			expected: true,
		},
		{
			name: "SST with only StartKey - after span",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: []byte("e"),
				EndKey:   nil,
			},
			span:     roachpb.Span{Key: []byte("a"), EndKey: []byte("d")},
			expected: false,
		},
		{
			name: "SST with only EndKey",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: nil,
				EndKey:   []byte("c"),
			},
			span:     roachpb.Span{Key: []byte("b"), EndKey: []byte("d")},
			expected: true,
		},
		{
			name: "SST with only EndKey - before span",
			sst: execinfrapb.BulkMergeSpec_SST{
				StartKey: nil,
				EndKey:   []byte("a"),
			},
			span:     roachpb.Span{Key: []byte("b"), EndKey: []byte("d")},
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := sstOverlapsSpan(tc.sst, tc.span)
			require.Equal(t, tc.expected, result)
		})
	}
}

func TestBuildSSTOverlapIndex(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testCases := []struct {
		name     string
		ssts     []execinfrapb.BulkMergeSpec_SST
		spans    []roachpb.Span
		expected sstOverlapIndex
	}{
		{
			name:     "empty SSTs",
			ssts:     nil,
			spans:    []roachpb.Span{{Key: []byte("a"), EndKey: []byte("b")}},
			expected: nil,
		},
		{
			name: "empty spans",
			ssts: []execinfrapb.BulkMergeSpec_SST{
				{StartKey: []byte("a"), EndKey: []byte("b")},
			},
			spans:    nil,
			expected: nil,
		},
		{
			name: "single SST overlaps single span",
			ssts: []execinfrapb.BulkMergeSpec_SST{
				{StartKey: []byte("a"), EndKey: []byte("c")},
			},
			spans: []roachpb.Span{
				{Key: []byte("b"), EndKey: []byte("d")},
			},
			expected: sstOverlapIndex{
				0: {0},
			},
		},
		{
			name: "single SST overlaps multiple spans",
			ssts: []execinfrapb.BulkMergeSpec_SST{
				{StartKey: []byte("a"), EndKey: []byte("z")},
			},
			spans: []roachpb.Span{
				{Key: []byte("b"), EndKey: []byte("c")},
				{Key: []byte("d"), EndKey: []byte("e")},
				{Key: []byte("f"), EndKey: []byte("g")},
			},
			expected: sstOverlapIndex{
				0: {0},
				1: {0},
				2: {0},
			},
		},
		{
			name: "multiple SSTs each overlap different spans",
			ssts: []execinfrapb.BulkMergeSpec_SST{
				{StartKey: []byte("a"), EndKey: []byte("c")},
				{StartKey: []byte("d"), EndKey: []byte("f")},
				{StartKey: []byte("g"), EndKey: []byte("i")},
			},
			spans: []roachpb.Span{
				{Key: []byte("a"), EndKey: []byte("b")},
				{Key: []byte("e"), EndKey: []byte("f")},
				{Key: []byte("h"), EndKey: []byte("i")},
			},
			expected: sstOverlapIndex{
				0: {0},
				1: {1},
				2: {2},
			},
		},
		{
			name: "multiple SSTs overlap single span",
			ssts: []execinfrapb.BulkMergeSpec_SST{
				{StartKey: []byte("a"), EndKey: []byte("c")},
				{StartKey: []byte("b"), EndKey: []byte("d")},
				{StartKey: []byte("c"), EndKey: []byte("e")},
			},
			spans: []roachpb.Span{
				{Key: []byte("b"), EndKey: []byte("d")},
			},
			expected: sstOverlapIndex{
				0: {0, 1, 2},
			},
		},
		{
			name: "some spans have no overlapping SSTs",
			ssts: []execinfrapb.BulkMergeSpec_SST{
				{StartKey: []byte("a"), EndKey: []byte("b")},
				{StartKey: []byte("e"), EndKey: []byte("f")},
			},
			spans: []roachpb.Span{
				{Key: []byte("a"), EndKey: []byte("b")},
				{Key: []byte("c"), EndKey: []byte("d")}, // no overlap
				{Key: []byte("e"), EndKey: []byte("f")},
			},
			expected: sstOverlapIndex{
				0: {0},
				2: {1},
			},
		},
		{
			name: "complex overlap pattern",
			ssts: []execinfrapb.BulkMergeSpec_SST{
				{URI: "sst0", StartKey: []byte("a"), EndKey: []byte("d")},
				{URI: "sst1", StartKey: []byte("c"), EndKey: []byte("f")},
				{URI: "sst2", StartKey: []byte("e"), EndKey: []byte("h")},
			},
			spans: []roachpb.Span{
				{Key: []byte("a"), EndKey: []byte("c")}, // overlaps sst0
				{Key: []byte("c"), EndKey: []byte("e")}, // overlaps sst0, sst1
				{Key: []byte("e"), EndKey: []byte("g")}, // overlaps sst1, sst2
				{Key: []byte("g"), EndKey: []byte("i")}, // overlaps sst2
			},
			expected: sstOverlapIndex{
				0: {0},
				1: {0, 1},
				2: {1, 2},
				3: {2},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := buildSSTOverlapIndex(tc.ssts, tc.spans)
			require.Equal(t, tc.expected, result)
		})
	}
}
