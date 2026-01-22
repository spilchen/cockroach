// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"bytes"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
)

// sstOverlapIndex maps task IDs to the indices of SSTs that overlap with that
// task's span. This enables lazy SST loading where only the SSTs needed for a
// particular task are opened, reducing memory usage from gRPC streaming buffers.
type sstOverlapIndex map[int][]int

// buildSSTOverlapIndex precomputes which SSTs overlap with which spans. This is
// a cheap operation since it only involves key comparisons, not actual SST
// file access.
//
// The function returns a map where:
//   - Key: taskID (index into spans array)
//   - Value: slice of SST indices that overlap with that task's span
func buildSSTOverlapIndex(
	ssts []execinfrapb.BulkMergeSpec_SST, spans []roachpb.Span,
) sstOverlapIndex {
	if len(ssts) == 0 || len(spans) == 0 {
		return nil
	}

	index := make(sstOverlapIndex, len(spans))
	for taskID, span := range spans {
		var overlapping []int
		for sstIdx, sst := range ssts {
			if sstOverlapsSpan(sst, span) {
				overlapping = append(overlapping, sstIdx)
			}
		}
		if len(overlapping) > 0 {
			index[taskID] = overlapping
		}
	}
	return index
}

// sstOverlapsSpan returns true if the SST's key range overlaps with the given
// span. Two ranges overlap if one starts before the other ends.
//
// SST range: [sst.StartKey, sst.EndKey]  (inclusive on both ends)
// Span:      [span.Key, span.EndKey)     (inclusive start, exclusive end)
//
// Overlap occurs when:
//   - SST starts before span ends AND
//   - SST ends at or after span starts
func sstOverlapsSpan(sst execinfrapb.BulkMergeSpec_SST, span roachpb.Span) bool {
	// If SST has no key range metadata, assume it could contain any key.
	if len(sst.StartKey) == 0 && len(sst.EndKey) == 0 {
		return true
	}

	// SST starts at or after span ends -> no overlap.
	if len(sst.StartKey) > 0 && bytes.Compare(sst.StartKey, span.EndKey) >= 0 {
		return false
	}

	// SST ends before span starts -> no overlap.
	// Note: SST.EndKey is inclusive, so we use < not <=.
	if len(sst.EndKey) > 0 && bytes.Compare(sst.EndKey, span.Key) < 0 {
		return false
	}

	return true
}
