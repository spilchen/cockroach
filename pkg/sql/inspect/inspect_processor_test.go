// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package inspect

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

// testInspectLogger is a test implementation of inspectLogger that collects issues in memory.
type testInspectLogger struct {
	mu     sync.Mutex
	issues []*inspectIssue
}

// logIssue implements the inspectLogger interface.
func (l *testInspectLogger) logIssue(_ context.Context, issue *inspectIssue) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.issues = append(l.issues, issue)
	return nil
}

// getIssues returns the issues that have been emitted to the logger.
func (l *testInspectLogger) getIssues() []*inspectIssue {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*inspectIssue(nil), l.issues...)
}

// runProcessorAndWait executes the given inspectProcessor and waits for it to complete.
// It asserts that the processor finishes within a fixed timeout, and that the result
// matches the expected error outcome.
func runProcessorAndWait(t *testing.T, proc *inspectProcessor, expectErr bool) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	processorResultCh := make(chan error, 1)
	go func() {
		processorResultCh <- proc.runInspect(ctx, nil)
	}()

	select {
	case err := <-processorResultCh:
		if expectErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		return
	case <-time.After(10 * time.Second):
		t.Fatal("neither span source nor processor finished in time")
	}
}

// makeProcessor will create an inspect processor for test.
func makeProcessor(
	t *testing.T, checkFactory inspectCheckFactory, src spanSource, concurrency int,
) (*inspectProcessor, *testInspectLogger) {
	t.Helper()
	logger := &testInspectLogger{}
	proc := &inspectProcessor{
		spec:           execinfrapb.InspectSpec{},
		checkFactories: []inspectCheckFactory{checkFactory},
		cfg: &execinfra.ServerConfig{
			Settings: cluster.MakeTestingClusterSettings(),
		},
		spanSrc:     src,
		logger:      logger,
		concurrency: concurrency,
	}
	return proc, logger
}

func TestInspectProcessor_ControlFlow(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	tests := []struct {
		desc      string
		configs   []testingCheckConfig
		spanSrc   *testingSpanSource
		expectErr bool
	}{
		{
			desc: "all goroutines exit cleanly",
			configs: []testingCheckConfig{
				{mode: checkModeNone, stopAfter: 1},
				{mode: checkModeNone, stopAfter: 1},
			},
			spanSrc: &testingSpanSource{
				mode:     spanModeNormal,
				maxSpans: 30,
			},
		},
		{
			desc: "one worker fails",
			configs: []testingCheckConfig{
				{mode: checkModeFailsAfterN, failAfter: 2},
			},
			spanSrc: &testingSpanSource{
				mode:     spanModeNormal,
				maxSpans: 100,
			},
			expectErr: true,
		},
		{
			desc: "one worker fails, one worker hangs",
			configs: []testingCheckConfig{
				{mode: checkModeFailsAfterN, failAfter: 2},
				{mode: checkModeBlocksUntilCancel},
			},
			spanSrc: &testingSpanSource{
				mode:     spanModeNormal,
				maxSpans: 10,
			},
			expectErr: true,
		},
		{
			desc: "producer fails",
			configs: []testingCheckConfig{
				{mode: checkModeNone, stopAfter: 2},
				{mode: checkModeNone, stopAfter: 2},
				{mode: checkModeNone, stopAfter: 2},
			},
			spanSrc: &testingSpanSource{
				mode:      spanModeFailsAfterN,
				failAfter: 8,
			},
			expectErr: true,
		},
		{
			desc: "producer hangs after emitting spans, worker triggers cancel",
			configs: []testingCheckConfig{
				{mode: checkModeFailsAfterN, failAfter: 1},
				{mode: checkModeNone, stopAfter: 5},
			},
			spanSrc: &testingSpanSource{
				mode:      spanModeHangsAfterN,
				hangAfter: 5, // emit 5 spans, then block
			},
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			factory := func() inspectCheck {
				return &testingInspectCheck{
					configs: tc.configs,
				}
			}
			proc, _ := makeProcessor(t, factory, tc.spanSrc, len(tc.configs))
			runProcessorAndWait(t, proc, tc.expectErr)
		})
	}
}

func TestInspectProcessor_EmitIssues(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	spanSrc := &testingSpanSource{
		mode:     spanModeNormal,
		maxSpans: 1,
	}
	factory := func() inspectCheck {
		return &testingInspectCheck{
			configs: []testingCheckConfig{
				{
					mode: checkModeNone,
					issues: []*inspectIssue{
						{ErrorType: "test_error", PrimaryKey: "pk1"},
						{ErrorType: "test_error", PrimaryKey: "pk2"},
					},
				},
			},
		}
	}
	proc, logger := makeProcessor(t, factory, spanSrc, 1)

	runProcessorAndWait(t, proc, false)

	require.Len(t, logger.getIssues(), 2)
}
