// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"container/heap"
	"hash/fnv"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/errors"
)

// keySuffixLen is the total suffix size: 1 byte for length + 8 bytes for hash.
const keySuffixLen = 9

// suffixingIterator wraps a SimpleMVCCIterator and adds a suffix to each key
// to prevent Pebble from shadowing duplicates across SSTs. The suffix is a
// 64-bit FNV hash of the SST file path, which is globally unique.
type suffixingIterator struct {
	iter   storage.SimpleMVCCIterator
	suffix []byte // pre-computed suffix (1 length byte + 8 hash bytes)

	// Cached current key with suffix applied.
	currentKey     storage.MVCCKey
	currentKeySafe bool
}

// newSuffixingIterator creates an iterator that appends a suffix derived from
// the SST file path to all keys. This makes otherwise-identical keys from
// different SSTs distinguishable, preventing Pebble's merging iterator from
// shadowing duplicates.
func newSuffixingIterator(iter storage.SimpleMVCCIterator, sstFilePath string) *suffixingIterator {
	// Pre-compute suffix: 1 length byte + 8 hash bytes.
	h := fnv.New64a()
	h.Write([]byte(sstFilePath))
	hashBytes := h.Sum(nil)

	suffix := make([]byte, keySuffixLen)
	suffix[0] = 8 // length of hash
	copy(suffix[1:], hashBytes)

	return &suffixingIterator{
		iter:   iter,
		suffix: suffix,
	}
}

// Close implements SimpleMVCCIterator.
func (s *suffixingIterator) Close() {
	s.iter.Close()
}

// SeekGE implements SimpleMVCCIterator. Note that we seek using the suffixed
// key to match the stored representation.
func (s *suffixingIterator) SeekGE(key storage.MVCCKey) {
	s.currentKeySafe = false
	s.iter.SeekGE(key)
}

// Valid implements SimpleMVCCIterator.
func (s *suffixingIterator) Valid() (bool, error) {
	return s.iter.Valid()
}

// Next implements SimpleMVCCIterator.
func (s *suffixingIterator) Next() {
	s.currentKeySafe = false
	s.iter.Next()
}

// NextKey implements SimpleMVCCIterator.
func (s *suffixingIterator) NextKey() {
	s.currentKeySafe = false
	s.iter.NextKey()
}

// UnsafeKey implements SimpleMVCCIterator. Returns the key with suffix appended.
func (s *suffixingIterator) UnsafeKey() storage.MVCCKey {
	if !s.currentKeySafe {
		baseKey := s.iter.UnsafeKey()
		// Add suffix to Key portion only (preserve Timestamp).
		s.currentKey.Key = make([]byte, 0, len(baseKey.Key)+len(s.suffix))
		s.currentKey.Key = append(s.currentKey.Key, baseKey.Key...)
		s.currentKey.Key = append(s.currentKey.Key, s.suffix...)
		s.currentKey.Timestamp = baseKey.Timestamp
		s.currentKeySafe = true
	}
	return s.currentKey
}

// UnsafeValue implements SimpleMVCCIterator.
func (s *suffixingIterator) UnsafeValue() ([]byte, error) {
	return s.iter.UnsafeValue()
}

// MVCCValueLenAndIsTombstone implements SimpleMVCCIterator.
func (s *suffixingIterator) MVCCValueLenAndIsTombstone() (int, bool, error) {
	return s.iter.MVCCValueLenAndIsTombstone()
}

// ValueLen implements SimpleMVCCIterator.
func (s *suffixingIterator) ValueLen() int {
	return s.iter.ValueLen()
}

// HasPointAndRange implements SimpleMVCCIterator.
func (s *suffixingIterator) HasPointAndRange() (bool, bool) {
	return s.iter.HasPointAndRange()
}

// RangeBounds implements SimpleMVCCIterator.
func (s *suffixingIterator) RangeBounds() roachpb.Span {
	return s.iter.RangeBounds()
}

// RangeKeys implements SimpleMVCCIterator.
func (s *suffixingIterator) RangeKeys() storage.MVCCRangeKeyStack {
	return s.iter.RangeKeys()
}

// RangeKeyChanged implements SimpleMVCCIterator.
func (s *suffixingIterator) RangeKeyChanged() bool {
	return s.iter.RangeKeyChanged()
}

var _ storage.SimpleMVCCIterator = (*suffixingIterator)(nil)

// removeKeySuffix removes the suffix added by suffixingIterator.
// Returns an error if the key doesn't have a valid suffix.
func removeKeySuffix(key roachpb.Key) (roachpb.Key, error) {
	if len(key) < keySuffixLen {
		return nil, errors.AssertionFailedf(
			"key too short to have suffix: len=%d, need=%d", len(key), keySuffixLen)
	}

	// Verify length marker in suffix.
	if key[len(key)-keySuffixLen] != 8 {
		return nil, errors.AssertionFailedf(
			"invalid suffix length: %d (expected 8)", key[len(key)-keySuffixLen])
	}

	return key[:len(key)-keySuffixLen], nil
}

// iterHeapItem represents an iterator position in the merging heap.
type iterHeapItem struct {
	iter  storage.SimpleMVCCIterator
	key   storage.MVCCKey
	index int // original iterator index for stable ordering
}

// iterHeap implements heap.Interface for merging multiple iterators.
type iterHeap []*iterHeapItem

func (h iterHeap) Len() int { return len(h) }

func (h iterHeap) Less(i, j int) bool {
	cmp := h[i].key.Key.Compare(h[j].key.Key)
	if cmp != 0 {
		return cmp < 0
	}
	// Same key: compare by timestamp (newer first).
	if !h[i].key.Timestamp.Equal(h[j].key.Timestamp) {
		return h[j].key.Timestamp.Less(h[i].key.Timestamp)
	}
	// Same key and timestamp: use iterator index for stable ordering.
	return h[i].index < h[j].index
}

func (h iterHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *iterHeap) Push(x interface{}) {
	*h = append(*h, x.(*iterHeapItem))
}

func (h *iterHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[0 : n-1]
	return item
}

// mergingIterator merges multiple SimpleMVCCIterators using a min-heap.
// Unlike Pebble's internal merging, this iterator surfaces ALL keys from all
// iterators, including duplicates.
type mergingIterator struct {
	iters []storage.SimpleMVCCIterator
	heap  iterHeap
	opts  storage.IterOptions

	// Current position state.
	currentItem *iterHeapItem
	err         error
}

// newMergingIterator creates an iterator that merges multiple iterators.
// All keys from all iterators are surfaced in sorted order.
func newMergingIterator(
	iters []storage.SimpleMVCCIterator, opts storage.IterOptions,
) *mergingIterator {
	return &mergingIterator{
		iters: iters,
		heap:  make(iterHeap, 0, len(iters)),
		opts:  opts,
	}
}

// Close implements SimpleMVCCIterator.
func (m *mergingIterator) Close() {
	for _, iter := range m.iters {
		iter.Close()
	}
	m.iters = nil
	m.heap = nil
}

// SeekGE implements SimpleMVCCIterator.
func (m *mergingIterator) SeekGE(key storage.MVCCKey) {
	m.err = nil
	m.currentItem = nil
	m.heap = m.heap[:0]

	// Position all iterators and add valid ones to heap.
	for i, iter := range m.iters {
		iter.SeekGE(key)
		if ok, err := iter.Valid(); err != nil {
			m.err = err
			return
		} else if ok {
			m.heap = append(m.heap, &iterHeapItem{
				iter:  iter,
				key:   iter.UnsafeKey().Clone(),
				index: i,
			})
		}
	}

	heap.Init(&m.heap)
	m.advance()
}

// Valid implements SimpleMVCCIterator.
func (m *mergingIterator) Valid() (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	return m.currentItem != nil, nil
}

// Next implements SimpleMVCCIterator.
func (m *mergingIterator) Next() {
	m.advanceCurrentIterator(false /* nextKey */)
}

// NextKey implements SimpleMVCCIterator.
func (m *mergingIterator) NextKey() {
	m.advanceCurrentIterator(true /* nextKey */)
}

// advanceCurrentIterator advances the current iterator and updates the heap.
func (m *mergingIterator) advanceCurrentIterator(nextKey bool) {
	if m.currentItem == nil {
		return
	}

	// Advance the iterator that was just consumed.
	iter := m.currentItem.iter
	if nextKey {
		iter.NextKey()
	} else {
		iter.Next()
	}

	if ok, err := iter.Valid(); err != nil {
		m.err = err
		return
	} else if ok {
		m.currentItem.key = iter.UnsafeKey().Clone()
		heap.Fix(&m.heap, 0)
	} else {
		heap.Pop(&m.heap)
	}
	m.advance()
}

// advance sets currentItem to the heap's minimum, if any.
func (m *mergingIterator) advance() {
	if len(m.heap) == 0 {
		m.currentItem = nil
		return
	}
	m.currentItem = m.heap[0]
}

// UnsafeKey implements SimpleMVCCIterator.
func (m *mergingIterator) UnsafeKey() storage.MVCCKey {
	if m.currentItem == nil {
		return storage.MVCCKey{}
	}
	return m.currentItem.iter.UnsafeKey()
}

// UnsafeValue implements SimpleMVCCIterator.
func (m *mergingIterator) UnsafeValue() ([]byte, error) {
	if m.currentItem == nil {
		return nil, nil
	}
	return m.currentItem.iter.UnsafeValue()
}

// MVCCValueLenAndIsTombstone implements SimpleMVCCIterator.
func (m *mergingIterator) MVCCValueLenAndIsTombstone() (int, bool, error) {
	if m.currentItem == nil {
		return 0, false, nil
	}
	return m.currentItem.iter.MVCCValueLenAndIsTombstone()
}

// ValueLen implements SimpleMVCCIterator.
func (m *mergingIterator) ValueLen() int {
	if m.currentItem == nil {
		return 0
	}
	return m.currentItem.iter.ValueLen()
}

// HasPointAndRange implements SimpleMVCCIterator.
func (m *mergingIterator) HasPointAndRange() (bool, bool) {
	if m.currentItem == nil {
		return false, false
	}
	return m.currentItem.iter.HasPointAndRange()
}

// RangeBounds implements SimpleMVCCIterator.
func (m *mergingIterator) RangeBounds() roachpb.Span {
	if m.currentItem == nil {
		return roachpb.Span{}
	}
	return m.currentItem.iter.RangeBounds()
}

// RangeKeys implements SimpleMVCCIterator.
func (m *mergingIterator) RangeKeys() storage.MVCCRangeKeyStack {
	if m.currentItem == nil {
		return storage.MVCCRangeKeyStack{}
	}
	return m.currentItem.iter.RangeKeys()
}

// RangeKeyChanged implements SimpleMVCCIterator.
func (m *mergingIterator) RangeKeyChanged() bool {
	if m.currentItem == nil {
		return false
	}
	return m.currentItem.iter.RangeKeyChanged()
}

var _ storage.SimpleMVCCIterator = (*mergingIterator)(nil)
