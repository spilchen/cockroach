// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"bytes"
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/bulk"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/bulksst"
	"github.com/cockroachdb/cockroach/pkg/sql/bulkutil"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/taskset"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

var (
	_ execinfra.Processor = &bulkMergeProcessor{}
	_ execinfra.RowSource = &bulkMergeProcessor{}
)

// targetFileSize controls the target SST size for non-final merge iterations
// (local merges). Larger files reduce the number of SSTs that the final
// iteration must process, improving efficiency.
var targetFileSize = settings.RegisterByteSizeSetting(
	settings.ApplicationLevel,
	"bulkio.merge.file_size",
	"target size for individual data files produced during local only merge phases",
	1<<30, // 1GB
	settings.WithPublic)

// Output row format for the bulk merge processor. The third column contains
// a marshaled BulkMergeSpec_Output protobuf with the list of output SSTs.
var bulkMergeProcessorOutputTypes = []*types.T{
	types.Bytes, // The encoded SQL Instance ID used for routing
	types.Int4,  // Task ID
	types.Bytes, // Encoded list of output SSTs (BulkMergeSpec_Output protobuf)
}

// bulkMergeProcessor accepts rows that include an assigned task id and emits
// rows that are (taskID, []output_sst) where output_sst is the name of SSTs
// that were produced by the merged output.
//
// The task ids are used to pick output [start, end) ranges to merge from the
// spec.spans.
//
// Task n is to process the input range from [spans[n].Key, spans[n].EndKey).
type bulkMergeProcessor struct {
	execinfra.ProcessorBase
	spec       execinfrapb.BulkMergeSpec
	input      execinfra.RowSource
	flowCtx    *execinfra.FlowCtx
	storageMux *bulkutil.ExternalStorageMux
	iter       storage.SimpleMVCCIterator

	// currentPartitionBounds tracks the partition bounds for which the iterator
	// was created. Used in final iteration to filter SSTs based on partition bounds.
	currentPartitionBounds *roachpb.Span

	// Statistics for tracking iterator lifecycle and work distribution.
	iteratorCreations   int                 // Number of times iterator was created from scratch.
	iteratorReuses      int                 // Number of times existing iterator was reused.
	iteratorRecreations int                 // Number of times iterator was recreated for new partition.
	tasksProcessed      int                 // Total number of tasks processed by this processor.
	partitionsVisited   map[string]struct{} // Set of partition bounds visited (for tracking work-stealing).
}

type mergeProcessorInput struct {
	sqlInstanceID   string
	taskID          taskset.TaskID
	partitionBounds roachpb.Span
}

func parseMergeProcessorInput(
	row rowenc.EncDatumRow, typs []*types.T,
) (mergeProcessorInput, error) {
	if len(row) != 3 {
		return mergeProcessorInput{}, errors.Newf("expected 3 columns, got %d", len(row))
	}
	if err := row[0].EnsureDecoded(typs[0], nil); err != nil {
		return mergeProcessorInput{}, err
	}
	if err := row[1].EnsureDecoded(typs[1], nil); err != nil {
		return mergeProcessorInput{}, err
	}
	if err := row[2].EnsureDecoded(typs[2], nil); err != nil {
		return mergeProcessorInput{}, err
	}
	sqlInstanceID, ok := row[0].Datum.(*tree.DBytes)
	if !ok {
		return mergeProcessorInput{}, errors.Newf("expected bytes column for sqlInstanceID, got %s", row[0].Datum.String())
	}
	taskID, ok := row[1].Datum.(*tree.DInt)
	if !ok {
		return mergeProcessorInput{}, errors.Newf("expected int4 column for taskID, got %s", row[1].Datum.String())
	}
	partitionBoundsBytes, ok := row[2].Datum.(*tree.DBytes)
	if !ok {
		return mergeProcessorInput{}, errors.Newf("expected bytes column for partitionBounds, got %s", row[2].Datum.String())
	}

	var partitionBounds roachpb.Span
	if err := protoutil.Unmarshal([]byte(*partitionBoundsBytes), &partitionBounds); err != nil {
		return mergeProcessorInput{}, errors.Wrap(err, "failed to unmarshal partition bounds")
	}

	return mergeProcessorInput{
		sqlInstanceID:   string(*sqlInstanceID),
		taskID:          taskset.TaskID(*taskID),
		partitionBounds: partitionBounds,
	}, nil
}

func newBulkMergeProcessor(
	ctx context.Context,
	flowCtx *execinfra.FlowCtx,
	processorID int32,
	spec execinfrapb.BulkMergeSpec,
	post *execinfrapb.PostProcessSpec,
	input execinfra.RowSource,
) (execinfra.Processor, error) {
	mp := &bulkMergeProcessor{
		input:             input,
		spec:              spec,
		flowCtx:           flowCtx,
		storageMux:        bulkutil.NewExternalStorageMux(flowCtx.Cfg.ExternalStorageFromURI, flowCtx.EvalCtx.SessionData().User()),
		partitionsVisited: make(map[string]struct{}),
	}
	err := mp.Init(
		ctx, mp, post, bulkMergeProcessorOutputTypes, flowCtx, processorID, nil,
		execinfra.ProcStateOpts{
			InputsToDrain: []execinfra.RowSource{input},
		},
	)
	if err != nil {
		return nil, err
	}
	return mp, nil
}

// Next implements execinfra.RowSource.
func (m *bulkMergeProcessor) Next() (rowenc.EncDatumRow, *execinfrapb.ProducerMetadata) {
	for m.State == execinfra.StateRunning {
		row, meta := m.input.Next()
		switch {
		case row == nil && meta == nil:
			m.MoveToDraining(nil /* err */)
		case meta != nil && meta.Err != nil:
			m.MoveToDraining(meta.Err)
		case meta != nil:
			// If there is non-nil meta, we pass it up the processor chain. It might
			// be something like a trace.
			return nil, meta
		case row != nil:
			output, err := m.handleRow(row)
			if err != nil {
				log.Dev.Errorf(m.Ctx(), "merge processor error: %+v", err)
				m.MoveToDraining(err)
			} else {
				return output, nil
			}
		}
	}
	return nil, m.DrainHelper()
}

func (m *bulkMergeProcessor) handleRow(row rowenc.EncDatumRow) (rowenc.EncDatumRow, error) {
	input, err := parseMergeProcessorInput(row, m.input.OutputTypes())
	if err != nil {
		return nil, err
	}

	// Lazy iterator creation for final iteration.
	// Non-final iterations create the iterator in Start().
	isFinal := m.spec.Iteration == m.spec.MaxIterations
	if isFinal && m.iter == nil {
		// First task for this processor - create iterator based on partition bounds.
		partitionKey := input.partitionBounds.String()
		m.partitionsVisited[partitionKey] = struct{}{}

		log.Dev.Infof(m.Ctx(), "task %d: creating iterator for partition [%s, %s)",
			input.taskID, input.partitionBounds.Key, input.partitionBounds.EndKey)

		iter, err := m.createIterForPartition(m.Ctx(), input.partitionBounds)
		if err != nil {
			return nil, err
		}
		m.iter = iter
		m.currentPartitionBounds = &input.partitionBounds
		m.iteratorCreations++
	} else if isFinal && m.currentPartitionBounds != nil {
		// Check if partition bounds changed (work-stealing to different partition).
		if !input.partitionBounds.Equal(*m.currentPartitionBounds) {
			partitionKey := input.partitionBounds.String()
			m.partitionsVisited[partitionKey] = struct{}{}

			log.Dev.Infof(m.Ctx(), "task %d: partition changed [%s, %s) → [%s, %s), recreating iterator",
				input.taskID,
				m.currentPartitionBounds.Key, m.currentPartitionBounds.EndKey,
				input.partitionBounds.Key, input.partitionBounds.EndKey)

			// Close old iterator and create new one for the new partition.
			m.iter.Close()

			iter, err := m.createIterForPartition(m.Ctx(), input.partitionBounds)
			if err != nil {
				return nil, err
			}
			m.iter = iter
			m.currentPartitionBounds = &input.partitionBounds
			m.iteratorRecreations++
		} else {
			// Same partition - reusing existing iterator.
			m.iteratorReuses++
		}
	}

	m.tasksProcessed++

	if knobs, ok := m.flowCtx.Cfg.TestingKnobs.BulkMergeTestingKnobs.(*TestingKnobs); ok {
		if knobs.RunBeforeMergeTask != nil {
			if err := knobs.RunBeforeMergeTask(m.Ctx(), m.flowCtx.ID, input.taskID); err != nil {
				return nil, err
			}
		}
	}

	results, err := m.mergeSSTs(m.Ctx(), input.taskID)
	if err != nil {
		return nil, err
	}

	marshaled, err := protoutil.Marshal(&results)
	if err != nil {
		return nil, err
	}

	return rowenc.EncDatumRow{
		rowenc.EncDatum{Datum: tree.NewDBytes(tree.DBytes(input.sqlInstanceID))},
		rowenc.EncDatum{Datum: tree.NewDInt(tree.DInt(input.taskID))},
		rowenc.EncDatum{Datum: tree.NewDBytes(tree.DBytes(marshaled))},
	}, nil
}

// Start implements execinfra.RowSource.
func (m *bulkMergeProcessor) Start(ctx context.Context) {
	ctx = m.StartInternal(ctx, "bulkMergeProcessor")
	m.input.Start(ctx)

	// Infer whether this is the final iteration from the spec fields.
	// Non-final iterations only merge local SSTs to reduce cross-node traffic.
	// This creates larger merged files locally before the final cross-node merge.
	isFinal := m.spec.Iteration == m.spec.MaxIterations
	localOnly := !isFinal

	// For non-final (local-only) iterations, create the iterator at startup.
	// For final iterations, defer iterator creation until the first task arrives,
	// allowing us to filter SSTs based on partition bounds.
	if localOnly {
		localInstanceID := m.flowCtx.NodeID.SQLInstanceID()
		log.Dev.Infof(ctx, "local iteration %d: filtering to local SSTs from instance %d",
			m.spec.Iteration, localInstanceID)
		iter, err := m.createIterLocalOnly(ctx, localInstanceID)
		if err != nil {
			m.MoveToDraining(err)
			return
		}
		m.iter = iter
	} else {
		log.Dev.Infof(ctx, "final iteration %d: deferring iterator creation until first task",
			m.spec.Iteration)
		// Iterator will be created lazily in handleRow() based on partition bounds.
	}
}

func (m *bulkMergeProcessor) Close(ctx context.Context) {
	if m.iter != nil {
		m.iter.Close()
	}

	// Log summary statistics for final iteration.
	isFinal := m.spec.Iteration == m.spec.MaxIterations
	if isFinal && m.tasksProcessed > 0 {
		log.Dev.Infof(ctx, "processor stats: %d tasks processed, %d partitions visited, "+
			"iterator: %d creations, %d reuses, %d recreations",
			m.tasksProcessed, len(m.partitionsVisited),
			m.iteratorCreations, m.iteratorReuses, m.iteratorRecreations)
	}

	err := m.storageMux.Close()
	if err != nil {
		log.Dev.Errorf(ctx, "failed to close external storage mux: %v", err)
	}
	m.ProcessorBase.Close(ctx)
}

func (m *bulkMergeProcessor) mergeSSTs(
	ctx context.Context, taskID taskset.TaskID,
) (execinfrapb.BulkMergeSpec_Output, error) {
	// If there's no iterator (no SSTs to merge), return an empty output.
	if m.iter == nil {
		return execinfrapb.BulkMergeSpec_Output{}, nil
	}

	mergeSpan := m.spec.Spans[taskID]
	log.Dev.Infof(ctx, "merge processor starting task %d with span %s", taskID, mergeSpan)

	// Seek the iterator if it's not positioned within the current task's span.
	// The spans are disjoint, so the only way the iterator would be contained
	// within the span is if the previous task's span preceded it.
	if ok, _ := m.iter.Valid(); !(ok && containsKey(mergeSpan, m.iter.UnsafeKey().Key)) {
		m.iter.SeekGE(storage.MVCCKey{Key: mergeSpan.Key})
	}

	// Infer whether this is the final iteration from the spec fields.
	if m.spec.Iteration == m.spec.MaxIterations {
		return m.ingestFinalIteration(ctx, m.iter, mergeSpan)
	}

	sstTargetSize := targetFileSize.Get(&m.flowCtx.EvalCtx.Settings.SV)
	destStore, err := m.flowCtx.Cfg.ExternalStorage(ctx, m.spec.OutputStorage)
	if err != nil {
		return execinfrapb.BulkMergeSpec_Output{}, err
	}
	defer destStore.Close()
	destFileAllocator := bulksst.NewExternalFileAllocator(destStore, m.spec.OutputStorage.URI,
		m.flowCtx.Cfg.DB.KV().Clock())

	writer, err := newExternalStorageWriter(
		ctx,
		m.flowCtx.EvalCtx.Settings,
		destFileAllocator,
		sstTargetSize,
	)
	if err != nil {
		return execinfrapb.BulkMergeSpec_Output{}, err
	}
	defer writer.Close(ctx)

	return processMergedData(ctx, m.iter, mergeSpan, writer)
}

// processMergedData is a unified function for iterating over merged data and
// writing it using a mergeWriter implementation. The iterator must already be
// positioned at or before the start of mergeSpan.
func processMergedData(
	ctx context.Context, iter storage.SimpleMVCCIterator, mergeSpan roachpb.Span, writer mergeWriter,
) (execinfrapb.BulkMergeSpec_Output, error) {
	var endKey roachpb.Key
	for {
		ok, err := iter.Valid()
		if err != nil {
			return execinfrapb.BulkMergeSpec_Output{}, err
		}
		if !ok {
			break
		}

		key := iter.UnsafeKey()
		if mergeSpan.EndKey.Compare(key.Key) <= 0 {
			// We've reached the end of the span.
			break
		}

		val, err := iter.UnsafeValue()
		if err != nil {
			return execinfrapb.BulkMergeSpec_Output{}, err
		}

		// If we've selected an endKey and this key is at or beyond that point,
		// complete the current output unit before adding this key.
		if endKey != nil && key.Key.Compare(endKey) >= 0 {
			if _, err := writer.Complete(ctx, endKey); err != nil {
				return execinfrapb.BulkMergeSpec_Output{}, err
			}
			endKey = nil
		}

		shouldSplit, err := writer.Add(ctx, key, val)
		if err != nil {
			return execinfrapb.BulkMergeSpec_Output{}, err
		}

		// If the writer wants to split and we haven't selected an endKey yet,
		// pick a safe split point after the current key.
		if shouldSplit && endKey == nil {
			safeKey, err := keys.EnsureSafeSplitKey(key.Key)
			if err != nil {
				return execinfrapb.BulkMergeSpec_Output{}, err
			}
			endKey = safeKey.PrefixEnd()
		}

		iter.NextKey()
	}

	return writer.Finish(ctx, mergeSpan.EndKey)
}

func (m *bulkMergeProcessor) ingestFinalIteration(
	ctx context.Context, iter storage.SimpleMVCCIterator, mergeSpan roachpb.Span,
) (execinfrapb.BulkMergeSpec_Output, error) {
	writeTS := m.spec.WriteTimestamp
	if writeTS.IsEmpty() {
		writeTS = m.flowCtx.Cfg.DB.KV().Clock().Now()
	}

	// Use SSTBatcher directly instead of BufferingAdder since the data is
	// already sorted from the merge iterator. This avoids the unnecessary
	// sorting overhead in BufferingAdder.
	batcher, err := bulk.MakeSSTBatcher(
		ctx,
		"bulk-merge-final",
		m.flowCtx.Cfg.DB.KV(),
		m.flowCtx.EvalCtx.Settings,
		hlc.Timestamp{}, // disallowShadowingBelow
		false,           // writeAtBatchTs
		false,           // scatterSplitRanges
		m.flowCtx.Cfg.BackupMonitor.MakeConcurrentBoundAccount(),
		m.flowCtx.Cfg.BulkSenderLimiter,
		nil, // range cache
	)
	if err != nil {
		return execinfrapb.BulkMergeSpec_Output{}, err
	}

	writer := newKVStorageWriter(batcher, writeTS)
	defer writer.Close(ctx)
	return processMergedData(ctx, iter, mergeSpan, writer)
}

// sstOverlapsSpan checks if an SST's key range overlaps with a given span.
// Uses half-open interval semantics where both ranges are [start, end).
func sstOverlapsSpan(sst execinfrapb.BulkMergeSpec_SST, span roachpb.Span) bool {
	return sst.StartKey.Compare(span.EndKey) < 0 && span.Key.Compare(sst.EndKey) < 0
}

// overlaps checks if two SST key ranges overlap using half-open interval semantics.
// Ranges [a.StartKey, a.EndKey) and [b.StartKey, b.EndKey) overlap if:
//
//	a.StartKey < b.EndKey AND b.StartKey < a.EndKey
//
// Examples:
//
//	[A,M) and [M,Z) → false (adjacent, not overlapping)
//	[A,M) and [K,Z) → true  (overlapping)
//	[A,Z) and [D,P) → true  (second contained in first)
func overlaps(a, b execinfrapb.BulkMergeSpec_SST) bool {
	return a.StartKey.Compare(b.EndKey) < 0 && b.StartKey.Compare(a.EndKey) < 0
}

// canAddToLevel checks if an SST can be added to a level without creating overlaps.
// Returns true only if the SST's key range doesn't overlap with any existing SST
// in the level.
func canAddToLevel(level []execinfrapb.BulkMergeSpec_SST, sst execinfrapb.BulkMergeSpec_SST) bool {
	for _, existing := range level {
		if overlaps(existing, sst) {
			return false
		}
	}
	return true
}

// validateNoOverlapsWithinLevels checks that no SSTs within the same level
// overlap. This is only called in test builds as a safety check.
func validateNoOverlapsWithinLevels(sstsByLevel [][]execinfrapb.BulkMergeSpec_SST) error {
	for levelIdx, level := range sstsByLevel {
		for i := 0; i < len(level)-1; i++ {
			for j := i + 1; j < len(level); j++ {
				if overlaps(level[i], level[j]) {
					return errors.AssertionFailedf("BUG: level %d contains overlapping SSTs:\n"+
						"  SST[%d]: [%s, %s) URI=%s\n"+
						"  SST[%d]: [%s, %s) URI=%s",
						levelIdx,
						i, level[i].StartKey, level[i].EndKey, level[i].URI,
						j, level[j].StartKey, level[j].EndKey, level[j].URI)
				}
			}
		}
	}
	return nil
}

// createIterForPartition builds an iterator over SSTs that overlap with the
// given partition bounds. This filters out SSTs that don't overlap with the
// partition, reducing memory and I/O. Uses the simpler ExternalSSTReader
// (without level grouping) since SST filtering provides the main optimization.
func (m *bulkMergeProcessor) createIterForPartition(
	ctx context.Context, partitionBounds roachpb.Span,
) (storage.SimpleMVCCIterator, error) {
	startTime := timeutil.Now()

	// If there are no SSTs, there's nothing to merge.
	if len(m.spec.SSTs) == 0 {
		return nil, nil
	}
	if len(m.spec.Spans) == 0 {
		return nil, errors.AssertionFailedf("no spans specified for merge processor")
	}

	// Filter SSTs to only those overlapping the partition bounds.
	var partitionSSTs []execinfrapb.BulkMergeSpec_SST
	for _, sst := range m.spec.SSTs {
		if sstOverlapsSpan(sst, partitionBounds) {
			partitionSSTs = append(partitionSSTs, sst)
		}
	}

	log.Dev.Infof(ctx, "partition [%s, %s): filtered to %d SSTs (from %d total) - %.1f%% reduction",
		partitionBounds.Key, partitionBounds.EndKey,
		len(partitionSSTs), len(m.spec.SSTs),
		100.0*(1.0-float64(len(partitionSSTs))/float64(len(m.spec.SSTs))))

	if len(partitionSSTs) == 0 {
		return nil, nil
	}

	// Convert to StoreFiles.
	storeFiles := make([]storageccl.StoreFile, 0, len(partitionSSTs))
	for _, sst := range partitionSSTs {
		file, err := m.storageMux.StoreFile(ctx, sst.URI)
		if err != nil {
			return nil, err
		}
		storeFiles = append(storeFiles, file)
	}

	// Use partition bounds as iterator bounds.
	iterOpts := storage.IterOptions{
		KeyTypes:   storage.IterKeyTypePointsAndRanges,
		LowerBound: partitionBounds.Key,
		UpperBound: partitionBounds.EndKey,
	}

	// Use the simpler ExternalSSTReader (no level grouping).
	// SST filtering provides the main optimization.
	iter, err := storageccl.ExternalSSTReader(ctx, storeFiles, nil, iterOpts)
	if err != nil {
		return nil, err
	}

	elapsed := timeutil.Since(startTime)
	log.Dev.Infof(ctx, "iterator created in %s (%d SSTs opened)",
		elapsed, len(partitionSSTs))

	return iter, nil
}

// createIter builds an iterator over all input SSTs. Actual access to the SSTs
// is deferred until the iterator seeks to one based on the merge span used for
// a given task ID.
//
// For the final iteration, SSTs are grouped into disjoint levels using a greedy
// algorithm to ensure non-overlapping SSTs within each level, satisfying Pebble's
// invariants. This typically achieves O(nodes) levels when nodes produce disjoint
// ranges.
//
// NOTE: This function is currently unused in the final iteration. Instead,
// createIterForPartition() is used to filter SSTs based on partition bounds.
func (m *bulkMergeProcessor) createIter(ctx context.Context) (storage.SimpleMVCCIterator, error) {
	// If there are no SSTs, there's nothing to merge.
	if len(m.spec.SSTs) == 0 {
		return nil, nil
	}
	if len(m.spec.Spans) == 0 {
		return nil, errors.AssertionFailedf("no spans specified for merge processor")
	}

	// Sort all SSTs by start key for deterministic level assignment.
	allSSTs := make([]execinfrapb.BulkMergeSpec_SST, len(m.spec.SSTs))
	copy(allSSTs, m.spec.SSTs)
	sort.Slice(allSSTs, func(i, j int) bool {
		return allSSTs[i].StartKey.Compare(allSSTs[j].StartKey) < 0
	})

	// Build disjoint levels using greedy algorithm.
	// Each level contains only non-overlapping SSTs, satisfying Pebble's requirement.
	sstsByLevel := make([][]execinfrapb.BulkMergeSpec_SST, 0)
	for _, sst := range allSSTs {
		placed := false

		// Try to place SST in an existing level (greedy: use first available).
		for levelIdx := range sstsByLevel {
			if canAddToLevel(sstsByLevel[levelIdx], sst) {
				sstsByLevel[levelIdx] = append(sstsByLevel[levelIdx], sst)
				placed = true
				break
			}
		}

		// If SST overlaps with all existing levels, create a new level.
		if !placed {
			sstsByLevel = append(sstsByLevel, []execinfrapb.BulkMergeSpec_SST{sst})
		}
	}

	// Convert to StoreFiles, preserving level structure.
	storeFilesByLevel := make([][]storageccl.StoreFile, 0, len(sstsByLevel))
	for _, level := range sstsByLevel {
		levelFiles := make([]storageccl.StoreFile, 0, len(level))
		for _, sst := range level {
			file, err := m.storageMux.StoreFile(ctx, sst.URI)
			if err != nil {
				return nil, err
			}
			levelFiles = append(levelFiles, file)
		}
		storeFilesByLevel = append(storeFilesByLevel, levelFiles)
	}

	log.Dev.Infof(ctx, "final iteration: created %d disjoint levels from %d total SSTs (avg %.1f SSTs/level)",
		len(storeFilesByLevel), len(m.spec.SSTs),
		float64(len(m.spec.SSTs))/float64(len(storeFilesByLevel)))

	// In development builds, validate non-overlapping within levels.
	if buildutil.CrdbTestBuild {
		if err := validateNoOverlapsWithinLevels(sstsByLevel); err != nil {
			return nil, err
		}
	}

	iterOpts := storage.IterOptions{
		KeyTypes: storage.IterKeyTypePointsAndRanges,
		// We don't really need bounds here because the merge iterator covers the
		// entire input data set, but the iterator has validation ensuring they
		// are set. These bounds work because the spans are non-overlapping and
		// sorted.
		LowerBound: m.spec.Spans[0].Key,
		UpperBound: m.spec.Spans[len(m.spec.Spans)-1].EndKey,
	}
	iter, err := storageccl.ExternalSSTReaderByLevel(ctx, storeFilesByLevel, nil, iterOpts)
	if err != nil {
		return nil, err
	}
	return iter, nil
}

// createIterLocalOnly builds an iterator over only the SSTs from the specified
// local instance. This is used for non-final iterations.
func (m *bulkMergeProcessor) createIterLocalOnly(
	ctx context.Context, localInstanceID base.SQLInstanceID,
) (storage.SimpleMVCCIterator, error) {
	if len(m.spec.SSTs) == 0 {
		return nil, nil
	}
	if len(m.spec.Spans) == 0 {
		return nil, errors.AssertionFailedf("no spans specified for merge processor")
	}

	var storeFiles []storageccl.StoreFile
	for _, sst := range m.spec.SSTs {
		sourceID, err := ExtractSourceInstanceID(sst.URI)
		if err != nil {
			return nil, err
		}
		if sourceID != localInstanceID {
			continue
		}
		file, err := m.storageMux.StoreFile(ctx, sst.URI)
		if err != nil {
			return nil, err
		}
		storeFiles = append(storeFiles, file)
	}

	log.Dev.Infof(ctx, "local-only iterator: selected %d/%d SSTs from instance %d",
		len(storeFiles), len(m.spec.SSTs), localInstanceID)

	if len(storeFiles) == 0 {
		return nil, nil
	}

	iterOpts := storage.IterOptions{
		KeyTypes:   storage.IterKeyTypePointsAndRanges,
		LowerBound: m.spec.Spans[0].Key,
		UpperBound: m.spec.Spans[len(m.spec.Spans)-1].EndKey,
	}
	return storageccl.ExternalSSTReader(ctx, storeFiles, nil, iterOpts)
}

// containsKey returns true if the given key is within the mergeSpan.
func containsKey(mergeSpan roachpb.Span, key roachpb.Key) bool {
	// key is to left
	if bytes.Compare(key, mergeSpan.Key) < 0 {
		return false
	}

	// key is to right
	if bytes.Compare(mergeSpan.EndKey, key) <= 0 {
		return false
	}

	return true
}

func init() {
	rowexec.NewBulkMergeProcessor = newBulkMergeProcessor
}
