// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

// TestKeySuffixRoundTrip verifies that keys can be suffixed and then have the
// suffix removed to recover the original key.
func TestKeySuffixRoundTrip(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testCases := []struct {
		name    string
		key     roachpb.Key
		sstPath string
	}{
		{
			name:    "simple key",
			key:     roachpb.Key("key-1"),
			sstPath: "nodelocal://1/merge/sst-001.sst",
		},
		{
			name:    "empty key",
			key:     roachpb.Key{},
			sstPath: "nodelocal://1/merge/sst-002.sst",
		},
		{
			name:    "key with special bytes",
			key:     roachpb.Key{0x00, 0xFF, 0x08, 0x00, 0xFF},
			sstPath: "nodelocal://2/merge/sst-003.sst",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a mock iterator with the key.
			mockIter := &mockSimpleMVCCIterator{
				key: storage.MVCCKey{
					Key:       tc.key,
					Timestamp: hlc.Timestamp{WallTime: 1},
				},
				valid: true,
			}

			// Wrap with suffixing iterator.
			suffixingIter := newSuffixingIterator(mockIter, tc.sstPath)
			defer suffixingIter.Close()

			// Get the suffixed key.
			suffixedKey := suffixingIter.UnsafeKey()

			// Verify the suffixed key is longer than the original.
			require.Greater(t, len(suffixedKey.Key), len(tc.key),
				"suffixed key should be longer than original")
			require.Equal(t, len(tc.key)+keySuffixLen, len(suffixedKey.Key),
				"suffix should add exactly %d bytes", keySuffixLen)

			// Verify timestamp is preserved.
			require.Equal(t, mockIter.key.Timestamp, suffixedKey.Timestamp,
				"timestamp should be preserved")

			// Remove suffix and verify we get the original key back.
			baseKey, err := removeKeySuffix(suffixedKey.Key)
			require.NoError(t, err)
			require.Equal(t, tc.key, baseKey, "base key should match original")
		})
	}
}

// TestDifferentSSTPaths verifies that different SST paths produce different
// suffixes, making keys distinguishable.
func TestDifferentSSTPaths(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	key := roachpb.Key("key-1")
	sstPaths := []string{
		"nodelocal://1/merge/sst-001.sst",
		"nodelocal://1/merge/sst-002.sst",
		"nodelocal://2/merge/sst-001.sst",
		"nodelocal://1/node-1/job-123/sst-001.sst",
		"nodelocal://1/node-2/job-123/sst-001.sst",
	}

	suffixedKeys := make([]roachpb.Key, len(sstPaths))
	for i, path := range sstPaths {
		mockIter := &mockSimpleMVCCIterator{
			key: storage.MVCCKey{
				Key:       key,
				Timestamp: hlc.Timestamp{WallTime: 1},
			},
			valid: true,
		}
		suffixingIter := newSuffixingIterator(mockIter, path)
		suffixedKeys[i] = suffixingIter.UnsafeKey().Key.Clone()
		suffixingIter.Close()
	}

	// Verify all suffixed keys are different.
	for i := 0; i < len(suffixedKeys); i++ {
		for j := i + 1; j < len(suffixedKeys); j++ {
			require.NotEqual(t, suffixedKeys[i], suffixedKeys[j],
				"keys from different SST paths should have different suffixes")
		}
	}

	// Verify all base keys are the same.
	for i, suffixedKey := range suffixedKeys {
		baseKey, err := removeKeySuffix(suffixedKey)
		require.NoError(t, err)
		require.Equal(t, key, baseKey, "base key should be the same for path %d", i)
	}
}

// TestRemoveKeySuffixErrors verifies that removeKeySuffix returns errors for
// invalid keys.
func TestRemoveKeySuffixErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testCases := []struct {
		name string
		key  roachpb.Key
	}{
		{
			name: "key too short",
			key:  roachpb.Key{0x01, 0x02, 0x03},
		},
		{
			name: "key just under suffix length",
			key:  roachpb.Key{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		},
		{
			name: "invalid suffix length marker",
			// 9 bytes, but the length marker (second-to-last byte) is not 8.
			key: roachpb.Key{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := removeKeySuffix(tc.key)
			require.Error(t, err, "should return error for invalid key")
		})
	}
}

// TestMergingIteratorSurfacesAllKeys verifies that the merging iterator
// surfaces all keys from all input iterators, including duplicates.
func TestMergingIteratorSurfacesAllKeys(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ts := hlc.Timestamp{WallTime: 1}

	// Create two iterators with overlapping keys.
	// Iterator 1: keys a, b, c.
	// Iterator 2: keys b, c, d.
	iter1 := newMockListIterator([]storage.MVCCKey{
		{Key: roachpb.Key("a"), Timestamp: ts},
		{Key: roachpb.Key("b"), Timestamp: ts},
		{Key: roachpb.Key("c"), Timestamp: ts},
	})
	iter2 := newMockListIterator([]storage.MVCCKey{
		{Key: roachpb.Key("b"), Timestamp: ts},
		{Key: roachpb.Key("c"), Timestamp: ts},
		{Key: roachpb.Key("d"), Timestamp: ts},
	})

	// Wrap with suffixing iterators.
	suffixedIter1 := newSuffixingIterator(iter1, "sst-1")
	suffixedIter2 := newSuffixingIterator(iter2, "sst-2")

	// Merge them.
	mergingIter := newMergingIterator(
		[]storage.SimpleMVCCIterator{suffixedIter1, suffixedIter2},
		storage.IterOptions{},
	)
	defer mergingIter.Close()

	// Collect all keys.
	var keys []roachpb.Key
	mergingIter.SeekGE(storage.MVCCKey{})
	for {
		ok, err := mergingIter.Valid()
		require.NoError(t, err)
		if !ok {
			break
		}
		key := mergingIter.UnsafeKey()
		baseKey, err := removeKeySuffix(key.Key)
		require.NoError(t, err)
		keys = append(keys, baseKey)
		mergingIter.NextKey()
	}

	// Expect all 6 keys: a, b(1), b(2), c(1), c(2), d.
	require.Len(t, keys, 6, "should see all keys including duplicates")

	// Verify order: a, b, b, c, c, d.
	expectedKeys := []string{"a", "b", "b", "c", "c", "d"}
	for i, expected := range expectedKeys {
		require.Equal(t, roachpb.Key(expected), keys[i],
			"key %d should be %s", i, expected)
	}
}

// TestMergingIteratorDuplicateDetection verifies that consecutive base keys
// can be used to detect duplicates.
func TestMergingIteratorDuplicateDetection(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ts := hlc.Timestamp{WallTime: 1}

	// Create two iterators with a duplicate key "b".
	iter1 := newMockListIterator([]storage.MVCCKey{
		{Key: roachpb.Key("a"), Timestamp: ts},
		{Key: roachpb.Key("b"), Timestamp: ts},
	})
	iter2 := newMockListIterator([]storage.MVCCKey{
		{Key: roachpb.Key("b"), Timestamp: ts},
		{Key: roachpb.Key("c"), Timestamp: ts},
	})

	// Wrap with suffixing iterators.
	suffixedIter1 := newSuffixingIterator(iter1, "sst-1")
	suffixedIter2 := newSuffixingIterator(iter2, "sst-2")

	// Merge them.
	mergingIter := newMergingIterator(
		[]storage.SimpleMVCCIterator{suffixedIter1, suffixedIter2},
		storage.IterOptions{},
	)
	defer mergingIter.Close()

	// Simulate duplicate detection logic like in processMergedData.
	var prevBaseKey roachpb.Key
	duplicateFound := false
	var duplicateKey roachpb.Key

	mergingIter.SeekGE(storage.MVCCKey{})
	for {
		ok, err := mergingIter.Valid()
		require.NoError(t, err)
		if !ok {
			break
		}

		key := mergingIter.UnsafeKey()
		baseKey, err := removeKeySuffix(key.Key)
		require.NoError(t, err)

		if prevBaseKey != nil && baseKey.Equal(prevBaseKey) {
			duplicateFound = true
			duplicateKey = baseKey.Clone()
			break
		}
		prevBaseKey = baseKey.Clone()
		mergingIter.NextKey()
	}

	require.True(t, duplicateFound, "should detect duplicate key")
	require.Equal(t, roachpb.Key("b"), duplicateKey, "duplicate key should be 'b'")
}

// mockSimpleMVCCIterator is a simple mock for SimpleMVCCIterator for testing.
type mockSimpleMVCCIterator struct {
	key   storage.MVCCKey
	val   []byte
	valid bool
}

func (m *mockSimpleMVCCIterator) Close()                       {}
func (m *mockSimpleMVCCIterator) SeekGE(key storage.MVCCKey)   {}
func (m *mockSimpleMVCCIterator) Valid() (bool, error)         { return m.valid, nil }
func (m *mockSimpleMVCCIterator) Next()                        { m.valid = false }
func (m *mockSimpleMVCCIterator) NextKey()                     { m.valid = false }
func (m *mockSimpleMVCCIterator) UnsafeKey() storage.MVCCKey   { return m.key }
func (m *mockSimpleMVCCIterator) UnsafeValue() ([]byte, error) { return m.val, nil }

func (m *mockSimpleMVCCIterator) MVCCValueLenAndIsTombstone() (int, bool, error) {
	return len(m.val), false, nil
}

func (m *mockSimpleMVCCIterator) ValueLen() int                  { return len(m.val) }
func (m *mockSimpleMVCCIterator) HasPointAndRange() (bool, bool) { return true, false }
func (m *mockSimpleMVCCIterator) RangeBounds() roachpb.Span      { return roachpb.Span{} }
func (m *mockSimpleMVCCIterator) RangeKeys() storage.MVCCRangeKeyStack {
	return storage.MVCCRangeKeyStack{}
}
func (m *mockSimpleMVCCIterator) RangeKeyChanged() bool { return false }

var _ storage.SimpleMVCCIterator = (*mockSimpleMVCCIterator)(nil)

// mockListIterator is a mock iterator that iterates over a list of keys.
type mockListIterator struct {
	keys  []storage.MVCCKey
	index int
}

func newMockListIterator(keys []storage.MVCCKey) *mockListIterator {
	return &mockListIterator{
		keys:  keys,
		index: -1, // not positioned
	}
}

func (m *mockListIterator) Close() {}

func (m *mockListIterator) SeekGE(target storage.MVCCKey) {
	for i, k := range m.keys {
		if target.Key.Compare(k.Key) <= 0 {
			m.index = i
			return
		}
	}
	m.index = len(m.keys) // exhausted
}

func (m *mockListIterator) Valid() (bool, error) {
	return m.index >= 0 && m.index < len(m.keys), nil
}

func (m *mockListIterator) Next() {
	if m.index >= 0 {
		m.index++
	}
}

func (m *mockListIterator) NextKey() { m.Next() }

func (m *mockListIterator) UnsafeKey() storage.MVCCKey {
	if m.index >= 0 && m.index < len(m.keys) {
		return m.keys[m.index]
	}
	return storage.MVCCKey{}
}

func (m *mockListIterator) UnsafeValue() ([]byte, error) {
	return []byte(fmt.Sprintf("value-%d", m.index)), nil
}

func (m *mockListIterator) MVCCValueLenAndIsTombstone() (int, bool, error) {
	return 8, false, nil
}

func (m *mockListIterator) ValueLen() int                  { return 8 }
func (m *mockListIterator) HasPointAndRange() (bool, bool) { return true, false }
func (m *mockListIterator) RangeBounds() roachpb.Span      { return roachpb.Span{} }
func (m *mockListIterator) RangeKeys() storage.MVCCRangeKeyStack {
	return storage.MVCCRangeKeyStack{}
}
func (m *mockListIterator) RangeKeyChanged() bool { return false }

var _ storage.SimpleMVCCIterator = (*mockListIterator)(nil)
