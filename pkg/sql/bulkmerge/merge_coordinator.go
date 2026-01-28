// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/taskset"
	"github.com/cockroachdb/errors"
)

var (
	_ execinfra.Processor = &mergeCoordinator{}
	_ execinfra.RowSource = &mergeCoordinator{}
)

// Emits a single row on completion which is a protobuf containing the details
// of the merged SSTs. The protobuf is BulkMergeSpec_Output, which contains the
// list of output SSTs with their URIs and key ranges.
var mergeCoordinatorOutputTypes = []*types.T{
	types.Bytes,
}

type mergeCoordinator struct {
	execinfra.ProcessorBase

	input execinfra.RowSource
	spec  execinfrapb.MergeCoordinatorSpec
	tasks taskset.TaskSet

	loopback chan rowenc.EncDatumRow
	cleanup  func()

	done    bool
	results execinfrapb.BulkMergeSpec_Output

	// partitionBounds contains the key span boundaries for each worker partition.
	// Each worker is assigned a contiguous range of task spans, and partitionBounds[i]
	// represents the min/max key range covering all spans assigned to worker i.
	partitionBounds []roachpb.Span

	// taskToPartition maps each task ID to its partition index.
	// This is needed for work-stealing scenarios where a worker processes tasks
	// from different partitions.
	taskToPartition []int
}

type mergeCoordinatorInput struct {
	sqlInstanceID string
	taskID        taskset.TaskID
	outputSSTs    []execinfrapb.BulkMergeSpec_SST
}

// parseCoordinatorInput ensures each column has the correct type and unmarshals
// the output SSTs.
func parseCoordinatorInput(row rowenc.EncDatumRow) (mergeCoordinatorInput, error) {
	if len(row) != 3 {
		return mergeCoordinatorInput{}, errors.Newf("expected 3 columns, got %d", len(row))
	}
	if err := row[0].EnsureDecoded(types.Bytes, nil); err != nil {
		return mergeCoordinatorInput{}, err
	}
	sqlInstanceID, ok := row[0].Datum.(*tree.DBytes)
	if !ok {
		return mergeCoordinatorInput{},
			errors.Newf("expected bytes column for sqlInstanceID, got %s", row[0].Datum.String())
	}
	if err := row[1].EnsureDecoded(types.Int4, nil); err != nil {
		return mergeCoordinatorInput{}, err
	}
	taskID, ok := row[1].Datum.(*tree.DInt)
	if !ok {
		return mergeCoordinatorInput{},
			errors.Newf("expected int4 column for taskID, got %s", row[1].Datum.String())
	}
	if err := row[2].EnsureDecoded(types.Bytes, nil); err != nil {
		return mergeCoordinatorInput{}, err
	}
	outputBytes, ok := row[2].Datum.(*tree.DBytes)
	if !ok {
		return mergeCoordinatorInput{},
			errors.Newf("expected bytes column for outputSSTs, got %s", row[2].Datum.String())
	}
	results := execinfrapb.BulkMergeSpec_Output{}
	if err := protoutil.Unmarshal([]byte(*outputBytes), &results); err != nil {
		return mergeCoordinatorInput{}, err
	}
	return mergeCoordinatorInput{
		sqlInstanceID: string(*sqlInstanceID),
		taskID:        taskset.TaskID(*taskID),
		outputSSTs:    results.SSTs,
	}, nil
}

// Next implements execinfra.RowSource.
func (m *mergeCoordinator) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	for m.State == execinfra.StateRunning {
		row, meta := m.input.Next()
		switch {
		case row != nil:
			err := m.handleRow(row)
			if err != nil {
				m.MoveToDraining(err)
			}
		case meta == nil:
			if m.done {
				m.MoveToDraining(nil /* err */)
				break
			}
			m.done = true
			return m.emitResults()
		case meta.Err != nil:
			m.closeLoopback()
			m.MoveToDraining(meta.Err)
		default:
			// If there is non-nil meta, we pass it up the processor chain. It might
			// be something like a trace.
			return nil, meta
		}
	}
	return nil, m.DrainHelper()
}

func (m *mergeCoordinator) emitResults() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	marshaled, err := protoutil.Marshal(&m.results)
	if err != nil {
		m.MoveToDraining(errors.Wrap(err, "failed to marshal results"))
		return nil, m.DrainHelper()
	}
	return rowenc.EncDatumRow{
		rowenc.EncDatum{Datum: tree.NewDBytes(tree.DBytes(marshaled))},
	}, nil
}

func (m *mergeCoordinator) publishInitialTasks() {
	for workerIdx, sqlInstanceID := range m.spec.WorkerSqlInstanceIds {
		taskID := m.tasks.ClaimFirst()
		if taskID.IsDone() {
			m.closeLoopback()
			return
		}

		// Get partition bounds for this TASK (not this worker).
		// This supports work-stealing where a worker may process tasks from different partitions.
		partitionIdx := m.taskToPartition[taskID]
		partitionBoundsBytes, err := encodePartitionBounds(m.partitionBounds[partitionIdx])
		if err != nil {
			// If we can't encode partition bounds, we have a serious problem.
			// Close the loopback and the workers will get an error.
			m.closeLoopback()
			return
		}

		log.Dev.Infof(m.Ctx(), "assigning task %d (partition %d) to worker %d (SQL instance %s)",
			taskID, partitionIdx, workerIdx, sqlInstanceID)

		m.loopback <- rowenc.EncDatumRow{
			rowenc.EncDatum{Datum: tree.NewDBytes(tree.DBytes(sqlInstanceID))},
			rowenc.EncDatum{Datum: tree.NewDInt(tree.DInt(taskID))},
			rowenc.EncDatum{Datum: tree.NewDBytes(tree.DBytes(partitionBoundsBytes))},
		}
	}
}

func (m *mergeCoordinator) closeLoopback() {
	if m.cleanup != nil {
		m.cleanup()
		m.cleanup = nil
	}
}

// computePartitionBounds divides the task spans across workers and computes
// the min/max key range for each worker's partition. Returns partition bounds
// and a mapping from task ID to partition index.
//
// Partitioning is done by balancing SST counts across workers to ensure fair
// work distribution. Each worker is assigned a contiguous range of spans such
// that the total number of SSTs overlapping those spans is roughly equal.
func computePartitionBounds(
	spans []roachpb.Span, ssts []execinfrapb.BulkMergeSpec_SST, numWorkers int,
) (partitionBounds []roachpb.Span, taskToPartition []int) {
	if numWorkers == 0 || len(spans) == 0 {
		return nil, nil
	}

	bounds := make([]roachpb.Span, numWorkers)
	taskToPartition = make([]int, len(spans))

	// Count SSTs overlapping each span.
	sstCountPerSpan := make([]int, len(spans))
	for spanIdx, span := range spans {
		for _, sst := range ssts {
			if sstOverlapsSpan(sst, span) {
				sstCountPerSpan[spanIdx]++
			}
		}
	}

	// Calculate total SST count and target SSTs per worker.
	totalSSTs := 0
	for _, count := range sstCountPerSpan {
		totalSSTs += count
	}
	sstsPerWorker := totalSSTs / numWorkers
	if sstsPerWorker == 0 {
		sstsPerWorker = 1 // Avoid division by zero in edge cases.
	}

	// Distribute spans to workers to balance SST counts.
	currentWorker := 0
	currentWorkerSSTs := 0
	partitionStart := 0

	for spanIdx, sstCount := range sstCountPerSpan {
		// If adding this span would exceed the target and we're not on the last worker,
		// finalize the current partition and move to the next worker.
		if currentWorkerSSTs+sstCount > sstsPerWorker && currentWorker < numWorkers-1 && currentWorkerSSTs > 0 {
			// End current partition.
			bounds[currentWorker] = roachpb.Span{
				Key:    spans[partitionStart].Key,
				EndKey: spans[spanIdx-1].EndKey,
			}
			currentWorker++
			partitionStart = spanIdx
			currentWorkerSSTs = 0
		}
		taskToPartition[spanIdx] = currentWorker
		currentWorkerSSTs += sstCount
	}

	// Finalize the last partition.
	if partitionStart < len(spans) {
		bounds[currentWorker] = roachpb.Span{
			Key:    spans[partitionStart].Key,
			EndKey: spans[len(spans)-1].EndKey,
		}
	}

	// Handle case where we have more workers than partitions created.
	// Remaining workers get empty spans.
	for i := currentWorker + 1; i < numWorkers; i++ {
		bounds[i] = roachpb.Span{}
	}

	return bounds, taskToPartition
}

// encodePartitionBounds marshals a Span into bytes for transmission via loopback.
func encodePartitionBounds(span roachpb.Span) ([]byte, error) {
	return protoutil.Marshal(&span)
}

// getWorkerIndex returns the worker index for a given SQL instance ID.
// Returns -1 if not found.
func (m *mergeCoordinator) getWorkerIndex(sqlInstanceID string) int {
	for i, id := range m.spec.WorkerSqlInstanceIds {
		if string(id) == sqlInstanceID {
			return i
		}
	}
	return -1
}

// handleRow accepts a row output by the merge processor, marks its task as
// complete
func (m *mergeCoordinator) handleRow(row rowenc.EncDatumRow) error {
	input, err := parseCoordinatorInput(row)
	if err != nil {
		return err
	}

	completedPartitionIdx := m.taskToPartition[input.taskID]
	m.results.SSTs = append(m.results.SSTs, input.outputSSTs...)

	next := m.tasks.ClaimNext(input.taskID)
	if next.IsDone() {
		m.closeLoopback()
		return nil
	}

	// Get partition bounds for the NEXT TASK (not this worker).
	// This supports work-stealing where a worker may process tasks from different partitions.
	nextPartitionIdx := m.taskToPartition[next]
	partitionBoundsBytes, err := encodePartitionBounds(m.partitionBounds[nextPartitionIdx])
	if err != nil {
		return errors.Wrap(err, "failed to encode partition bounds")
	}

	// Log work-stealing detection.
	if completedPartitionIdx != nextPartitionIdx {
		log.Dev.Infof(m.Ctx(), "work-stealing: worker %s completed task %d (partition %d), "+
			"now assigned task %d (partition %d)",
			input.sqlInstanceID, input.taskID, completedPartitionIdx,
			next, nextPartitionIdx)
	}

	m.loopback <- rowenc.EncDatumRow{
		rowenc.EncDatum{Datum: tree.NewDBytes(tree.DBytes(input.sqlInstanceID))},
		rowenc.EncDatum{Datum: tree.NewDInt(tree.DInt(next))},
		rowenc.EncDatum{Datum: tree.NewDBytes(tree.DBytes(partitionBoundsBytes))},
	}

	return nil
}

// Start implements execinfra.RowSource.
func (m *mergeCoordinator) Start(ctx context.Context) {
	m.StartInternal(ctx, "mergeCoordinator")
	m.input.Start(ctx)

	// Compute partition bounds for each worker based on the task spans and SSTs.
	// Partitioning is done by balancing SST counts across workers to ensure fair
	// work distribution. Also build a mapping from task ID to partition index for
	// work-stealing scenarios.
	m.partitionBounds, m.taskToPartition = computePartitionBounds(
		m.spec.Spans, m.spec.SSTs, len(m.spec.WorkerSqlInstanceIds))

	m.publishInitialTasks()
}

func init() {
	rowexec.NewMergeCoordinatorProcessor = func(
		ctx context.Context,
		flow *execinfra.FlowCtx,
		flowID int32,
		spec execinfrapb.MergeCoordinatorSpec,
		postSpec *execinfrapb.PostProcessSpec,
		input execinfra.RowSource,
	) (execinfra.Processor, error) {
		channel, cleanup := loopback.create(flow)
		mc := &mergeCoordinator{
			input:    input,
			tasks:    taskset.MakeTaskSet(spec.TaskCount, int64(len(spec.WorkerSqlInstanceIds))),
			loopback: channel,
			cleanup:  cleanup,
			spec:     spec,
		}
		err := mc.Init(
			ctx, mc, postSpec, mergeCoordinatorOutputTypes, flow, flowID, nil,
			execinfra.ProcStateOpts{
				InputsToDrain: []execinfra.RowSource{input},
			},
		)
		if err != nil {
			return nil, err
		}
		return mc, nil
	}
}
