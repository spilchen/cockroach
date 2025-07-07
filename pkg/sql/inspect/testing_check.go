// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package inspect

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// testingCheckMode defines behavior for test-only inspect checks.
// It is used to simulate worker-side edge cases.
type testingCheckMode int

const (
	checkModeNone testingCheckMode = iota

	// Worker fails after N iterations.
	checkModeFailsAfterN

	// Worker hangs until its context is cancelled.
	checkModeBlocksUntilCancel
)

type testingCheckConfig struct {
	mode      testingCheckMode
	stopAfter int
	failAfter int
	issues    []*inspectIssue
}

type testingInspectCheck struct {
	started     bool
	configs     []testingCheckConfig
	index       int
	iteration   int
	issueCursor int
}

var _ inspectCheck = (*testingInspectCheck)(nil)

// Started implements the inspectCheck interface.
func (t *testingInspectCheck) Started() bool {
	return t.started
}

// Start implements the inspectCheck interface.
func (t *testingInspectCheck) Start(
	ctx context.Context, _ *execinfra.ServerConfig, _ roachpb.Span, workerIndex int,
) error {
	log.Infof(ctx, "Worker index %d given span", workerIndex)
	t.started = true
	t.index = workerIndex
	t.iteration = 0
	t.issueCursor = 0
	return nil
}

// Next implements the inspectCheck interface.
func (t *testingInspectCheck) Next(
	ctx context.Context, _ *execinfra.ServerConfig,
) (*inspectIssue, error) {
	cfg := t.configs[t.index]
	t.iteration++

	switch cfg.mode {
	case checkModeFailsAfterN:
		if cfg.failAfter == 0 || t.iteration >= cfg.failAfter {
			log.Infof(ctx, "Worker %d failing via test check", t.index)
			return nil, errors.New("worker failure triggered by test check")
		}
		time.Sleep(10 * time.Millisecond)

	case checkModeBlocksUntilCancel:
		log.Infof(ctx, "Worker %d blocking until cancelled", t.index)
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				time.Sleep(100 * time.Millisecond)
			}
		}

	case checkModeNone:
		time.Sleep(5 * time.Millisecond)
	}

	if t.issueCursor < len(cfg.issues) {
		issue := cfg.issues[t.issueCursor]
		log.Infof(ctx, "Worker %d emitting issue: %+v", t.index, issue)
		t.issueCursor++
		return issue, nil
	}

	return nil, nil
}

// Done implements the inspectCheck interface.
func (t *testingInspectCheck) Done(context.Context) bool {
	cfg := t.configs[t.index]
	switch cfg.mode {
	case checkModeFailsAfterN, checkModeBlocksUntilCancel:
		// Never returns done. These test modes runs forever and depend on errors,
		// context cancellation to stop.
		return false
	default:
		if cfg.stopAfter > 0 {
			return t.iteration >= cfg.stopAfter
		}
		// Fall back: done when all issues are emitted
		return t.issueCursor >= len(cfg.issues)
	}
}

// Close implements the inspectCheck interface.
func (t *testingInspectCheck) Close(context.Context) error {
	return nil
}
