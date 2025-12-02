// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/bulkutil"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/taskset"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
)

var (
	_ execinfra.Processor = &bulkMergeProcessor{}
	_ execinfra.RowSource = &bulkMergeProcessor{}
)

var (
	targetFileSize = settings.RegisterByteSizeSetting(
		settings.ApplicationLevel,
		"bulkio.merge.file_size",
		"target size for individual data files produced during merge phase",
		60<<20,
		settings.WithPublic,
	)
)

var bulkMergeProcessorOutputTypes = []*types.T{
	types.Bytes, // Encoded routing key (SQL instance ID)
	types.Int4,  // Task ID
	types.Bytes, // Encoded list of output SSTs
}

// bulkMergeProcessor merges a subset of the input SSTs into new SSTs that
// perfectly cover the span assigned to the processor's current task. Each
// processor consumes rows produced by the merge loopback processor. Every row
// identifies the SQL instance's routing key and a task ID to process.
type bulkMergeProcessor struct {
	execinfra.ProcessorBase

	spec       execinfrapb.BulkMergeSpec
	input      execinfra.RowSource
	flowCtx    *execinfra.FlowCtx
	storageMux *bulkutil.ExternalStorageMux
}

type mergeProcessorInput struct {
	sqlInstanceID string
	taskID        taskset.TaskID
}

func parseMergeProcessorInput(
	row rowenc.EncDatumRow, typs []*types.T,
) (mergeProcessorInput, error) {
	if len(row) != 2 {
		return mergeProcessorInput{}, errors.Newf("expected 2 columns, got %d", len(row))
	}
	if err := row[0].EnsureDecoded(typs[0], nil); err != nil {
		return mergeProcessorInput{}, err
	}
	if err := row[1].EnsureDecoded(typs[1], nil); err != nil {
		return mergeProcessorInput{}, err
	}

	sqlInstanceID, ok := row[0].Datum.(*tree.DBytes)
	if !ok {
		return mergeProcessorInput{}, errors.Newf("expected bytes column for sqlInstanceID, got %s", row[0].Datum)
	}
	taskID, ok := row[1].Datum.(*tree.DInt)
	if !ok {
		return mergeProcessorInput{}, errors.Newf("expected int4 column for taskID, got %s", row[1].Datum)
	}
	return mergeProcessorInput{
		sqlInstanceID: string(*sqlInstanceID),
		taskID:        taskset.TaskID(*taskID),
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
		spec:       spec,
		input:      input,
		flowCtx:    flowCtx,
		storageMux: bulkutil.NewExternalStorageMux(flowCtx.Cfg.ExternalStorageFromURI, username.RootUserName()),
	}
	err := mp.Init(
		ctx, mp, post, bulkMergeProcessorOutputTypes, flowCtx, processorID, nil,
		execinfra.ProcStateOpts{InputsToDrain: []execinfra.RowSource{input}},
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
			m.MoveToDraining(nil)
			return nil, m.DrainHelper()
		case meta != nil:
			if meta.Err != nil {
				m.MoveToDraining(meta.Err)
				return nil, m.DrainHelper()
			}
			return nil, meta
		default:
			output, err := m.handleRow(row)
			if err != nil {
				log.Ops.Errorf(m.Ctx(), "merge processor error: %+v", err)
				m.MoveToDraining(err)
				return nil, m.DrainHelper()
			}
			return output, nil
		}
	}
	return nil, m.DrainHelper()
}

func (m *bulkMergeProcessor) handleRow(row rowenc.EncDatumRow) (rowenc.EncDatumRow, error) {
	input, err := parseMergeProcessorInput(row, m.input.OutputTypes())
	if err != nil {
		return nil, err
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
}

func (m *bulkMergeProcessor) Close(ctx context.Context) {
	if err := m.storageMux.Close(); err != nil {
		log.Ops.Errorf(ctx, "failed to close external storage mux: %v", err)
	}
	m.ProcessorBase.Close(ctx)
}

// isOverlapping returns true if the SST overlaps the given merge span. Merge
// spans are [start, end) while SST metadata is [start, end].
func isOverlapping(mergeSpan roachpb.Span, sst execinfrapb.BulkMergeSpec_SST) bool {
	if bytes.Compare(sst.EndKey, mergeSpan.Key) < 0 {
		return false
	}
	if len(mergeSpan.EndKey) > 0 && bytes.Compare(mergeSpan.EndKey, sst.StartKey) <= 0 {
		return false
	}
	return true
}

func (m *bulkMergeProcessor) getOutputStorage(ctx context.Context) (cloud.ExternalStorage, error) {
	if m.spec.OutputStore != nil {
		store, err := m.flowCtx.Cfg.ExternalStorage(ctx, *m.spec.OutputStore)
		if err != nil {
			return nil, err
		}
		return store, nil
	}
	if m.spec.OutputUri != "" {
		return m.flowCtx.Cfg.ExternalStorageFromURI(ctx, m.spec.OutputUri, username.RootUserName())
	}
	return nil, errors.New("bulk merge spec missing output storage definition")
}

func (m *bulkMergeProcessor) mergeSSTs(
	ctx context.Context, taskID taskset.TaskID,
) (execinfrapb.BulkMergeSpec_Output, error) {
	idx := int(taskID)
	if idx < 0 || idx >= len(m.spec.Spans) {
		return execinfrapb.BulkMergeSpec_Output{}, errors.Newf("invalid task %d", taskID)
	}
	mergeSpan := m.spec.Spans[idx]

	if m.spec.OutputUri == "" {
		return execinfrapb.BulkMergeSpec_Output{}, errors.New("bulk merge spec missing output URI")
	}

	destStore, err := m.getOutputStorage(ctx)
	if err != nil {
		return execinfrapb.BulkMergeSpec_Output{}, err
	}
	defer destStore.Close()

	var storeFiles []storageccl.StoreFile
	for _, sst := range m.spec.Ssts {
		if !isOverlapping(mergeSpan, sst) {
			continue
		}
		file, err := m.storageMux.StoreFile(ctx, sst.Uri)
		if err != nil {
			return execinfrapb.BulkMergeSpec_Output{}, err
		}
		storeFiles = append(storeFiles, file)
	}
	if len(storeFiles) == 0 {
		return execinfrapb.BulkMergeSpec_Output{}, nil
	}

	iterOpts := storage.IterOptions{
		KeyTypes:   storage.IterKeyTypePointsAndRanges,
		LowerBound: mergeSpan.Key,
		UpperBound: mergeSpan.EndKey,
	}
	iter, err := storageccl.ExternalSSTReader(ctx, storeFiles, nil, iterOpts)
	if err != nil {
		return execinfrapb.BulkMergeSpec_Output{}, err
	}
	defer iter.Close()

	baseOutputURI := strings.TrimRight(m.spec.OutputUri, "/")
	if baseOutputURI == "" {
		baseOutputURI = m.spec.OutputUri
	}

	var (
		writerOpen   bool
		writer       storage.SSTWriter
		writerCloser io.Closer
		currentURI   string
		fileIndex    int
	)
	closeWriter := func(finish bool) error {
		if !writerOpen {
			return nil
		}
		var closeErr error
		if finish {
			if err := writer.Finish(); err != nil {
				closeErr = err
			}
		}
		writer.Close()
		if writerCloser != nil {
			if err := writerCloser.Close(); closeErr == nil {
				closeErr = err
			}
			writerCloser = nil
		}
		writerOpen = false
		currentURI = ""
		return closeErr
	}
	defer func() { _ = closeWriter(false /* finish */) }()

	openWriter := func() error {
		if writerOpen {
			return errors.AssertionFailedf("writer already open")
		}
		fileName := fmt.Sprintf("%d-%d.sst", taskID, fileIndex)
		fileIndex++
		sink, err := destStore.Writer(ctx, fileName)
		if err != nil {
			return err
		}
		writer = storage.MakeIngestionSSTWriter(ctx, m.flowCtx.EvalCtx.Settings, objstorageprovider.NewRemoteWritable(sink))
		writerCloser = sink
		currentURI = fmt.Sprintf("%s/%s", baseOutputURI, fileName)
		writerOpen = true
		return nil
	}

	var (
		firstKey roachpb.Key
		lastKey  roachpb.Key
		sstSize  int64
		outputs  execinfrapb.BulkMergeSpec_Output
	)

	finalizeCurrentFile := func() error {
		if !writerOpen || sstSize == 0 {
			return nil
		}
		uri := currentURI
		if err := closeWriter(true /* finish */); err != nil {
			return err
		}
		outputs.Ssts = append(outputs.Ssts, execinfrapb.BulkMergeSpec_SST{
			StartKey: firstKey,
			EndKey:   lastKey,
			Uri:      uri,
		})
		firstKey = nil
		lastKey = nil
		sstSize = 0
		return nil
	}

	targetSize := targetFileSize.Get(&m.flowCtx.EvalCtx.Settings.SV)

	for iter.SeekGE(storage.MVCCKey{Key: mergeSpan.Key}); ; iter.NextKey() {
		ok, err := iter.Valid()
		if err != nil {
			return execinfrapb.BulkMergeSpec_Output{}, err
		}
		if !ok {
			break
		}

		if writerOpen && sstSize >= targetSize {
			if err := finalizeCurrentFile(); err != nil {
				return execinfrapb.BulkMergeSpec_Output{}, err
			}
		}

		hasPoint, hasRange := iter.HasPointAndRange()
		if hasRange {
			return execinfrapb.BulkMergeSpec_Output{}, errors.AssertionFailedf("bulk merge processor encountered unsupported MVCC range key")
		}
		if !hasPoint {
			return execinfrapb.BulkMergeSpec_Output{}, errors.AssertionFailedf("bulk merge processor landed on non-point, non-range position")
		}

		if !writerOpen {
			if err := openWriter(); err != nil {
				return execinfrapb.BulkMergeSpec_Output{}, err
			}
		}

		key := iter.UnsafeKey()
		val, err := iter.UnsafeValue()
		if err != nil {
			return execinfrapb.BulkMergeSpec_Output{}, err
		}

		if firstKey == nil {
			firstKey = key.Key.Clone()
		}
		if err := writer.PutRawMVCC(key, val); err != nil {
			return execinfrapb.BulkMergeSpec_Output{}, err
		}
		lastKey = key.Key.Clone()
		sstSize += int64(len(key.Key) + len(val))
	}

	if err := finalizeCurrentFile(); err != nil {
		return execinfrapb.BulkMergeSpec_Output{}, err
	}

	return outputs, nil
}

func init() {
	rowexec.NewBulkMergeProcessor = newBulkMergeProcessor
}
