// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
)

// loopbackMap is a temporary coordination mechanism that lets the merge
// coordinator enqueue tasks directly into the merge loopback processor running
// on the same node.
type loopbackMap struct {
	syncutil.Mutex
	loopback map[execinfrapb.FlowID]chan rowenc.EncDatumRow
}

var loopback = &loopbackMap{loopback: make(map[execinfrapb.FlowID]chan rowenc.EncDatumRow)}

func (l *loopbackMap) get(flowCtx *execinfra.FlowCtx) (chan rowenc.EncDatumRow, bool) {
	l.Lock()
	defer l.Unlock()
	ch, ok := l.loopback[flowCtx.ID]
	return ch, ok
}

func (l *loopbackMap) create(flowCtx *execinfra.FlowCtx) (chan rowenc.EncDatumRow, func()) {
	l.Lock()
	defer l.Unlock()
	ch := make(chan rowenc.EncDatumRow)
	l.loopback[flowCtx.ID] = ch
	return ch, func() {
		l.Lock()
		defer l.Unlock()
		if existing, ok := l.loopback[flowCtx.ID]; ok && existing == ch {
			delete(l.loopback, flowCtx.ID)
		}
		close(ch)
	}
}

var (
	_ execinfra.Processor = &mergeLoopback{}
	_ execinfra.RowSource = &mergeLoopback{}
)

var mergeLoopbackOutputTypes = []*types.T{
	types.Bytes, // routing key (encoded SQL instance ID)
	types.Int4,  // task ID
}

type mergeLoopback struct {
	execinfra.ProcessorBase
	loopback chan rowenc.EncDatumRow
}

// Next implements execinfra.RowSource.
func (m *mergeLoopback) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	if m.State != execinfra.StateRunning {
		return nil, m.DrainHelper()
	}
	row, ok := <-m.loopback
	if !ok {
		m.MoveToDraining(nil)
		return nil, m.DrainHelper()
	}
	return row, nil
}

// Start implements execinfra.RowSource.
func (m *mergeLoopback) Start(ctx context.Context) {
	m.StartInternal(ctx, "mergeLoopback")
	ch, ok := loopback.get(m.FlowCtx)
	if !ok {
		m.MoveToDraining(errors.New("loopback channel not found"))
		return
	}
	m.loopback = ch
}

func init() {
	rowexec.NewMergeLoopbackProcessor = func(
		ctx context.Context,
		flow *execinfra.FlowCtx,
		processorID int32,
		spec execinfrapb.MergeLoopbackSpec,
		postSpec *execinfrapb.PostProcessSpec,
	) (execinfra.Processor, error) {
		ml := &mergeLoopback{}
		if err := ml.Init(
			ctx, ml, postSpec, mergeLoopbackOutputTypes, flow, processorID, nil,
			execinfra.ProcStateOpts{},
		); err != nil {
			return nil, err
		}
		return ml, nil
	}
}
