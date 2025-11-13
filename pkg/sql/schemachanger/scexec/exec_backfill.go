// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scexec

import (
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// TODO(ajwerner): Consider separating out the dependencies for the
// backfill from that of mutations and validation. The fact that there
// is a transaction hanging around in the dependencies is more likely to
// be confusing than valuable. Not much is being done transactionally.

func executeBackfillOps(ctx context.Context, deps Dependencies, execute []scop.Op) (err error) {
	backfillsToExecute, mergesToExecute, partitionDeletions := extractBackfillsAndMergesFromOps(execute)

	// Execute partition deletions first since they're non-revertible
	if len(partitionDeletions) > 0 {
		if err := executePartitionDeletions(ctx, deps, partitionDeletions); err != nil {
			return err
		}
	}

	tables, err := getTableDescriptorsForBackfillsAndMerges(ctx, deps.Catalog(), backfillsToExecute, mergesToExecute)
	if err != nil {
		return err
	}
	tracker := deps.BackfillProgressTracker()
	backfillProgresses, mergeProgresses, err := loadProgressesAndMaybePerformInitialScan(
		ctx, deps, backfillsToExecute, mergesToExecute, tracker, tables,
	)
	if err != nil {
		return err
	}
	if err := runBackfiller(ctx, deps, tracker, backfillProgresses, mergeProgresses, deps.TransactionalJobRegistry().CurrentJob(), tables); err != nil {
		if errors.HasType(err, (*kvpb.InsufficientSpaceError)(nil)) {
			return jobs.MarkPauseRequestError(errors.UnwrapAll(err))
		}
		return err
	}
	return nil
}

func getTableDescriptorsForBackfillsAndMerges(
	ctx context.Context, cat Catalog, backfills []Backfill, merges []Merge,
) (_ map[descpb.ID]catalog.TableDescriptor, err error) {
	var descIDs catalog.DescriptorIDSet
	for _, bf := range backfills {
		descIDs.Add(bf.TableID)
	}
	for _, m := range merges {
		descIDs.Add(m.TableID)
	}
	tables := make(map[descpb.ID]catalog.TableDescriptor, descIDs.Len())
	descs, err := cat.MustReadImmutableDescriptors(ctx, descIDs.Ordered()...)
	if err != nil {
		return nil, err
	}
	for _, desc := range descs {
		tbl, ok := desc.(catalog.TableDescriptor)
		if !ok {
			return nil, errors.AssertionFailedf("descriptor %d is not a table", desc.GetID())
		}
		tables[tbl.GetID()] = tbl
	}
	return tables, nil
}

func extractBackfillsAndMergesFromOps(execute []scop.Op) ([]Backfill, []Merge, []*scop.DeletePartitionData) {
	var bfs []Backfill
	var ms []Merge
	var pds []*scop.DeletePartitionData
	for _, op := range execute {
		switch op := op.(type) {
		case *scop.BackfillIndex:
			bfs = append(bfs, Backfill{
				TableID:       op.TableID,
				SourceIndexID: op.SourceIndexID,
				DestIndexIDs:  []descpb.IndexID{op.IndexID},
			})
		case *scop.MergeIndex:
			ms = append(ms, Merge{
				TableID:        op.TableID,
				SourceIndexIDs: []descpb.IndexID{op.TemporaryIndexID},
				DestIndexIDs:   []descpb.IndexID{op.BackfilledIndexID},
			})
		case *scop.DeletePartitionData:
			// DeletePartitionData is handled separately in executePartitionDeletions.
			// It doesn't use the standard backfill/merge machinery.
			pds = append(pds, op)
		default:
			panic("unimplemented")
		}
	}
	return mergeBackfillsFromSameSource(bfs), mergeMergesFromSameTable(ms), pds
}

// mergeBackfillsFromSameSource will take a slice of backfills which
// potentially have more than one source and will compact them into
// exactly one backfill for each (TableID, SourceIndexID) tuple. The
// DestIndexIDs will be sorted.
func mergeBackfillsFromSameSource(toExecute []Backfill) []Backfill {
	sort.Slice(toExecute, func(i, j int) bool {
		if toExecute[i].TableID == toExecute[j].TableID {
			return toExecute[i].SourceIndexID < toExecute[j].SourceIndexID
		}
		return toExecute[i].TableID < toExecute[j].TableID
	})
	truncated := toExecute[:0]
	sameSource := func(a, b Backfill) bool {
		return a.TableID == b.TableID && a.SourceIndexID == b.SourceIndexID
	}
	for _, bf := range toExecute {
		if len(truncated) == 0 || !sameSource(truncated[len(truncated)-1], bf) {
			truncated = append(truncated, bf)
		} else {
			ord := len(truncated) - 1
			curIDs := truncated[ord].DestIndexIDs
			curIDs = curIDs[:len(curIDs):len(curIDs)]
			truncated[ord].DestIndexIDs = append(curIDs, bf.DestIndexIDs...)
		}
	}
	toExecute = truncated

	// Make sure all the DestIndexIDs are sorted.
	for i := range toExecute {
		dstIDs := toExecute[i].DestIndexIDs
		sort.Slice(dstIDs, func(i, j int) bool { return dstIDs[i] < dstIDs[j] })
	}
	return toExecute
}

// mergeMergesFromSameTable is like mergeBackfillsFromSameSource but for merges.
func mergeMergesFromSameTable(toExecute []Merge) []Merge {
	sort.Slice(toExecute, func(i, j int) bool {
		return toExecute[i].TableID < toExecute[j].TableID
	})
	truncated := toExecute[:0]
	for _, m := range toExecute {
		if len(truncated) == 0 || truncated[len(truncated)-1].TableID != m.TableID {
			truncated = append(truncated, m)
		} else {
			ord := len(truncated) - 1
			srcIDs := truncated[ord].SourceIndexIDs
			srcIDs = srcIDs[:len(srcIDs):len(srcIDs)]
			truncated[ord].SourceIndexIDs = append(srcIDs, m.SourceIndexIDs...)
			destIDs := truncated[ord].DestIndexIDs
			destIDs = destIDs[:len(destIDs):len(destIDs)]
			truncated[ord].DestIndexIDs = append(destIDs, m.DestIndexIDs...)
		}
	}
	toExecute = truncated

	// Make sure all the SourceIndexIDs are sorted.
	for i := range toExecute {
		m := &toExecute[i]
		srcToDest := make(map[descpb.IndexID]descpb.IndexID, len(m.SourceIndexIDs))
		for j, sourceID := range m.SourceIndexIDs {
			srcToDest[sourceID] = m.DestIndexIDs[j]
		}
		sort.Slice(m.SourceIndexIDs, func(i, j int) bool { return m.SourceIndexIDs[i] < m.SourceIndexIDs[j] })
		for j, sourceID := range m.SourceIndexIDs {
			m.DestIndexIDs[j] = srcToDest[sourceID]
		}
	}
	return toExecute
}

func loadProgressesAndMaybePerformInitialScan(
	ctx context.Context,
	deps Dependencies,
	backfillsToExecute []Backfill,
	mergesToExecute []Merge,
	tracker BackfillerTracker,
	tables map[descpb.ID]catalog.TableDescriptor,
) ([]BackfillProgress, []MergeProgress, error) {
	backfillProgresses, mergeProgresses, err := loadProgresses(ctx, backfillsToExecute, mergesToExecute, tracker)
	if err != nil {
		return nil, nil, err
	}
	{
		didScan, err := maybeScanDestinationIndexes(ctx, deps, backfillProgresses, tables, tracker)
		if err != nil {
			return nil, nil, err
		}
		if didScan {
			if err := tracker.FlushCheckpoint(ctx); err != nil {
				return nil, nil, err
			}
		}
	}
	return backfillProgresses, mergeProgresses, nil
}

// maybeScanDestinationIndexes runs a scan on any backfills in progresses
// which do not have their MinimumWriteTimestamp set. If a scan occurs for
// a backfill, the corresponding entry in progresses will be populated with
// the scan timestamp and the progress will be reported to the tracker.
// If any index was scanned successfully, didScan will be true.
func maybeScanDestinationIndexes(
	ctx context.Context,
	deps Dependencies,
	bs []BackfillProgress,
	tables map[descpb.ID]catalog.TableDescriptor,
	tracker BackfillerProgressWriter,
) (didScan bool, _ error) {
	for i := range bs {
		if didScan = didScan || bs[i].MinimumWriteTimestamp.IsEmpty(); didScan {
			break
		}
	}
	if !didScan {
		return false, nil
	}
	const op = "scan destination indexes"
	fn := func(ctx context.Context, p *BackfillProgress) error {
		if !p.MinimumWriteTimestamp.IsEmpty() {
			return nil
		}
		updated, err := deps.IndexBackfiller().MaybePrepareDestIndexesForBackfill(
			ctx, *p, tables[p.TableID],
		)
		if err != nil {
			return err
		}
		*p = updated
		return tracker.SetBackfillProgress(ctx, updated)
	}
	if err := forEachProgressConcurrently(ctx, op, bs, nil /* ms */, fn, nil /* mf */); err != nil {
		return false, err
	}
	return true, nil
}

func forEachProgressConcurrently(
	ctx context.Context,
	op redact.SafeString,
	bs []BackfillProgress,
	ms []MergeProgress,
	bf func(context.Context, *BackfillProgress) error,
	mf func(context.Context, *MergeProgress) error,
) error {
	g := ctxgroup.WithContext(ctx)
	br := func(i int) {
		g.GoCtx(func(ctx context.Context) (err error) {
			defer func() {
				switch r := recover().(type) {
				case nil:
					return
				case error:
					err = errors.Wrapf(r, "failed to %s", op)
				default:
					err = errors.AssertionFailedf("failed to %s: %v", op, r)
				}
			}()
			return bf(ctx, &bs[i])
		})
	}
	mr := func(j int) {
		g.GoCtx(func(ctx context.Context) (err error) {
			defer func() {
				switch r := recover().(type) {
				case nil:
					return
				case error:
					err = errors.Wrapf(r, "failed to %s", op)
				default:
					err = errors.AssertionFailedf("failed to %s: %v", op, r)
				}
			}()
			return mf(ctx, &ms[j])
		})
	}
	for i := range bs {
		br(i)
	}
	for j := range ms {
		mr(j)
	}
	return g.Wait()
}

func loadProgresses(
	ctx context.Context,
	backfillsToExecute []Backfill,
	mergesToExecute []Merge,
	tracker BackfillerProgressReader,
) (bs []BackfillProgress, ms []MergeProgress, _ error) {
	for _, b := range backfillsToExecute {
		progress, err := tracker.GetBackfillProgress(ctx, b)
		if err != nil {
			return nil, nil, err
		}
		bs = append(bs, progress)
	}
	for _, m := range mergesToExecute {
		progress, err := tracker.GetMergeProgress(ctx, m)
		if err != nil {
			return nil, nil, err
		}
		ms = append(ms, progress)
	}
	return bs, ms, nil
}

func runBackfiller(
	ctx context.Context,
	deps Dependencies,
	tracker BackfillerTracker,
	backfillProgresses []BackfillProgress,
	mergeProgresses []MergeProgress,
	job *jobs.Job,
	tables map[descpb.ID]catalog.TableDescriptor,
) error {
	if deps.GetTestingKnobs() != nil &&
		deps.GetTestingKnobs().RunBeforeBackfill != nil {
		err := deps.GetTestingKnobs().RunBeforeBackfill(backfillProgresses)
		if err != nil {
			return err
		}
	}
	stop := deps.PeriodicProgressFlusher().StartPeriodicUpdates(ctx, tracker)
	defer stop()
	ib := deps.IndexBackfiller()
	im := deps.IndexMerger()
	const op = "run backfills and merges"
	bf := func(ctx context.Context, p *BackfillProgress) error {
		return runBackfill(ctx, deps.IndexSpanSplitter(), ib, *p, tracker, job, tables[p.TableID])
	}
	mf := func(ctx context.Context, p *MergeProgress) error {
		return im.MergeIndexes(ctx, job, *p, tracker, tables[p.TableID])
	}
	if err := forEachProgressConcurrently(ctx, op, backfillProgresses, mergeProgresses, bf, mf); err != nil {
		pgCode := pgerror.GetPGCode(err)
		// Determine the type of error we encountered.
		if pgCode == pgcode.CheckViolation ||
			pgCode == pgcode.UniqueViolation ||
			pgCode == pgcode.ForeignKeyViolation ||
			pgCode == pgcode.NotNullViolation ||
			pgCode == pgcode.IntegrityConstraintViolation {
			deps.Telemetry().IncrementSchemaChangeErrorType("constraint_violation")
		} else {
			// We ran into an  uncategorized schema change error.
			deps.Telemetry().IncrementSchemaChangeErrorType("uncategorized")
		}
		return scerrors.SchemaChangerUserError(err)
	}
	if err := tracker.FlushFractionCompleted(ctx); err != nil {
		return err
	}
	return tracker.FlushCheckpoint(ctx)
}

func runBackfill(
	ctx context.Context,
	splitter IndexSpanSplitter,
	backfiller Backfiller,
	progress BackfillProgress,
	tracker BackfillerProgressWriter,
	job *jobs.Job,
	table catalog.TableDescriptor,
) error {
	return backfiller.BackfillIndexes(ctx, progress, tracker, job, table)
}

// executePartitionDeletions deletes the data for all partitions in the provided
// list of DeletePartitionData operations. This uses MVCC delete range to
// efficiently delete all data in each partition's key range.
func executePartitionDeletions(
	ctx context.Context, deps Dependencies, partitionDeletions []*scop.DeletePartitionData,
) error {
	if len(partitionDeletions) == 0 {
		return nil
	}

	codec := deps.Codec()
	txn := deps.Txn()

	// In test mode, Txn() returns nil. Skip actual deletion but the operation
	// generation will still be verified by tests.
	if txn == nil {
		return nil
	}

	for _, op := range partitionDeletions {
		// Get the table descriptor to access the index and partition information.
		descs, err := deps.Catalog().MustReadImmutableDescriptors(ctx, op.TableID)
		if err != nil {
			return err
		}
		if len(descs) != 1 {
			return errors.AssertionFailedf("expected exactly one descriptor, got %d", len(descs))
		}
		tableDesc, ok := descs[0].(catalog.TableDescriptor)
		if !ok {
			return errors.AssertionFailedf("descriptor %d is not a table", op.TableID)
		}

		// Get the index to compute partition spans.
		idx, err := catalog.MustFindIndexByID(tableDesc, op.IndexID)
		if err != nil {
			return err
		}

		// Compute the key spans for this partition.
		spans, err := computePartitionSpans(codec, tableDesc, idx, op)
		if err != nil {
			return err
		}

		// Delete the data in each span using MVCC delete range.
		for _, span := range spans {
			if _, err := txn.KV().DelRange(ctx, span.Key, span.EndKey, false /* returnKeys */); err != nil {
				return errors.Wrapf(err, "deleting partition %s data", op.PartitionName)
			}
		}
	}

	return nil
}

// minimalPartitioning implements catalog.Partitioning with just the essential
// methods needed for DecodePartitionTuple.
type minimalPartitioning struct {
	numColumns         uint32
	numImplicitColumns uint32
}

func (p minimalPartitioning) NumColumns() int {
	return int(p.numColumns)
}

func (p minimalPartitioning) NumImplicitColumns() int {
	// NumImplicitColumns must be <= NumColumns, otherwise validation fails.
	// Return 0 to avoid validation errors in other parts of the system.
	return 0
}

func (p minimalPartitioning) PartitioningDesc() *catpb.PartitioningDescriptor {
	return &catpb.PartitioningDescriptor{}
}

func (p minimalPartitioning) DeepCopy() catalog.Partitioning {
	return p
}

func (p minimalPartitioning) FindPartitionByName(name string) catalog.Partitioning {
	return nil
}

func (p minimalPartitioning) ForEachPartitionName(fn func(name string) error) error {
	return nil
}

func (p minimalPartitioning) ForEachList(fn func(name string, values [][]byte, subPartitioning catalog.Partitioning) error) error {
	return nil
}

func (p minimalPartitioning) ForEachRange(fn func(name string, from, to []byte) error) error {
	return nil
}

func (p minimalPartitioning) NumLists() int {
	return 0
}

func (p minimalPartitioning) NumRanges() int {
	return 0
}

func (p minimalPartitioning) NumPartitions() int {
	return 0
}

// computePartitionSpans computes the key spans for a partition based on its
// type (LIST or RANGE) and partition values.
func computePartitionSpans(
	codec keys.SQLCodec,
	tableDesc catalog.TableDescriptor,
	idx catalog.Index,
	op *scop.DeletePartitionData,
) ([]roachpb.Span, error) {
	var spans []roachpb.Span
	a := &tree.DatumAlloc{}

	// Create a minimal partitioning context using the stored metadata.
	// This is necessary because when the last partition is dropped, the index's
	// partitioning descriptor will be empty (NumColumns=0), which causes
	// DecodePartitionTuple to fail. We use the metadata that was stored when
	// the DeletePartitionData operation was created.
	part := minimalPartitioning{
		numColumns:         op.NumColumns,
		numImplicitColumns: op.NumImplicitColumns,
	}
	var prefixDatums tree.Datums

	// Navigate through the partition path to build the prefix datums.
	// For nested partitions, we need to decode ancestor partition values
	// to build up the prefix datums that will be used when decoding the
	// target partition.
	// Note: We can't use idx.GetPartitioning().FindPartitionByName() here because
	// the partitioning may have been removed from the index if this is the last partition.
	for i := 0; i < len(op.PartitionPath)-1; i++ {
		// For now, we don't support dropping nested partitions with the last partition.
		// This would require storing more metadata about ancestor partitions in the operation.
		return nil, errors.AssertionFailedf(
			"dropping nested partitions not yet supported when all partitions are removed")
	}

	if op.ListPartition != nil {
		// For LIST partitions, compute a span for each value in the list.
		for _, valueEncBuf := range op.ListPartition.Values {
			_, keyPrefix, err := rowenc.DecodePartitionTuple(
				a, codec, tableDesc, idx, part, valueEncBuf, prefixDatums)
			if err != nil {
				return nil, errors.Wrapf(err, "decoding partition tuple for %s", op.PartitionName)
			}
			// For list partitions, the span is from keyPrefix to keyPrefix.PrefixEnd().
			spans = append(spans, roachpb.Span{
				Key:    keyPrefix,
				EndKey: roachpb.Key(keyPrefix).PrefixEnd(),
			})
		}
	} else if op.RangePartition != nil {
		// For RANGE partitions, compute a single span from FromInclusive to ToExclusive.
		_, fromKey, err := rowenc.DecodePartitionTuple(
			a, codec, tableDesc, idx, part, op.RangePartition.FromInclusive, prefixDatums)
		if err != nil {
			return nil, errors.Wrapf(err, "decoding FROM partition tuple for %s", op.PartitionName)
		}

		var toKey []byte
		if len(op.RangePartition.ToExclusive) > 0 {
			_, toKey, err = rowenc.DecodePartitionTuple(
				a, codec, tableDesc, idx, part, op.RangePartition.ToExclusive, prefixDatums)
			if err != nil {
				return nil, errors.Wrapf(err, "decoding TO partition tuple for %s", op.PartitionName)
			}
		} else {
			// If ToExclusive is empty, the range extends to the end of the index.
			indexSpan := tableDesc.IndexSpan(codec, op.IndexID)
			toKey = indexSpan.EndKey
		}

		spans = append(spans, roachpb.Span{
			Key:    fromKey,
			EndKey: toKey,
		})
	} else {
		return nil, errors.AssertionFailedf("partition %s has neither LIST nor RANGE partition", op.PartitionName)
	}

	return spans, nil
}
