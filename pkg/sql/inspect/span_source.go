// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package inspect

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/errors"
)

// spanSource defines a source of spans for the INSPECT processor to consume.
// It allows testing and production implementations to supply spans dynamically.
type spanSource interface {
	// NextSpan returns the next span to process.
	// ok is false when there are no more spans.
	// error is returned on fatal errors.
	NextSpan(ctx context.Context) (roachpb.Span, bool, error)
}

// testingSpanSourceMode defines behavior for test-only span sources.
// It is used to simulate producer-side edge cases.
type testingSpanSourceMode int

const (
	// Producer emits spans normally and stops.
	spanModeNormal testingSpanSourceMode = iota

	// Producer fails after N spans.
	spanModeFailsAfterN

	// Producer hangs after N spans. It hangs indefinitely unless context is
	// canceled.
	spanModeHangsAfterN
)

type testingSpanSource struct {
	mode      testingSpanSourceMode
	count     int
	maxSpans  int
	failAfter int
	hangAfter int
}

var _ spanSource = (*testingSpanSource)(nil)

// NextSpan implements the spanSource interface.
func (s *testingSpanSource) NextSpan(ctx context.Context) (roachpb.Span, bool, error) {
	switch s.mode {
	case spanModeFailsAfterN:
		if s.failAfter == 0 || s.count >= s.failAfter {
			return roachpb.Span{}, false, errors.New("simulated producer failure")
		}
	case spanModeHangsAfterN:
		if s.hangAfter == 0 || s.count >= s.hangAfter {
			for {
				select {
				case <-ctx.Done():
					return roachpb.Span{}, false, ctx.Err()
				default:
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	case spanModeNormal:
		// No special error or hang handling.
	}

	if s.maxSpans > 0 && s.count >= s.maxSpans {
		return roachpb.Span{}, false, nil
	}
	s.count++
	return roachpb.Span{Key: roachpb.Key(fmt.Sprintf("test-span-%d", s.count))}, true, nil
}

// sliceSpanSource is a spanSource implementation backed by a fixed slice of spans.
// It emits spans sequentially in the order they appear in the slice.
type sliceSpanSource struct {
	spans []roachpb.Span
	index int
}

var _ spanSource = (*sliceSpanSource)(nil)

func newSliceSpanSource(spans []roachpb.Span) *sliceSpanSource {
	return &sliceSpanSource{spans: spans}
}

// NextSpan implements the spanSource interface.
func (s *sliceSpanSource) NextSpan(ctx context.Context) (roachpb.Span, bool, error) {
	if s.index >= len(s.spans) {
		return roachpb.Span{}, false, nil
	}
	span := s.spans[s.index]
	s.index++
	return span, true, nil
}
