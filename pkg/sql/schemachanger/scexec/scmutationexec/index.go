// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scmutationexec

import (
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/errors"
)

func (i *immediateVisitor) MakeAbsentIndexBackfilling(
	ctx context.Context, op scop.MakeAbsentIndexBackfilling,
) error {
	return addNewIndexMutation(
		ctx, i, op.Index, op.IsSecondaryIndex, op.IsDeletePreserving,
		descpb.DescriptorMutation_BACKFILLING,
	)
}

func (i *immediateVisitor) MakeAbsentTempIndexDeleteOnly(
	ctx context.Context, op scop.MakeAbsentTempIndexDeleteOnly,
) error {
	const isDeletePreserving = true // temp indexes are always delete preserving
	return addNewIndexMutation(
		ctx, i, op.Index, op.IsSecondaryIndex, isDeletePreserving,
		descpb.DescriptorMutation_DELETE_ONLY,
	)
}

func addNewIndexMutation(
	ctx context.Context,
	i *immediateVisitor,
	opIndex scpb.Index,
	isSecondary bool,
	isDeletePreserving bool,
	state descpb.DescriptorMutation_State,
) error {
	tbl, err := i.checkOutTable(ctx, opIndex.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	// TODO(ajwerner): deal with ordering the indexes or sanity checking this
	// or what-not.
	if opIndex.IndexID >= tbl.NextIndexID {
		tbl.NextIndexID = opIndex.IndexID + 1
	}
	if opIndex.ConstraintID >= tbl.NextConstraintID {
		tbl.NextConstraintID = opIndex.ConstraintID + 1
	}

	// Set up the index descriptor type.
	indexType := descpb.IndexDescriptor_FORWARD
	if opIndex.IsInverted {
		indexType = descpb.IndexDescriptor_INVERTED
	}
	// Set up the encoding type.
	encodingType := catenumpb.PrimaryIndexEncoding
	indexVersion := descpb.LatestIndexDescriptorVersion
	if isSecondary {
		encodingType = catenumpb.SecondaryIndexEncoding
	}
	// Create an index descriptor from the operation.
	idx := &descpb.IndexDescriptor{
		ID:                          opIndex.IndexID,
		Name:                        tabledesc.IndexNamePlaceholder(opIndex.IndexID),
		Unique:                      opIndex.IsUnique,
		NotVisible:                  opIndex.IsNotVisible,
		Invisibility:                opIndex.Invisibility,
		Version:                     indexVersion,
		Type:                        indexType,
		CreatedExplicitly:           true,
		EncodingType:                encodingType,
		ConstraintID:                opIndex.ConstraintID,
		UseDeletePreservingEncoding: isDeletePreserving,
		StoreColumnNames:            []string{},
	}
	if isSecondary && !isDeletePreserving {
		idx.CreatedAtNanos = i.clock.ApproximateTime().UnixNano()
	}
	if opIndex.Sharding != nil {
		idx.Sharded = *opIndex.Sharding
	}
	if opIndex.GeoConfig != nil {
		idx.GeoConfig = *opIndex.GeoConfig
	}
	return enqueueIndexMutation(tbl, idx, state, descpb.DescriptorMutation_ADD)
}

func (i *immediateVisitor) SetAddedIndexPartialPredicate(
	ctx context.Context, op scop.SetAddedIndexPartialPredicate,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	mut, err := FindMutation(tbl, MakeIndexIDMutationSelector(op.IndexID))
	if err != nil {
		return err
	}
	idx := mut.AsIndex().IndexDesc()
	idx.Predicate = string(op.Expr)
	return nil
}

func (i *immediateVisitor) MakeBackfillingIndexDeleteOnly(
	ctx context.Context, op scop.MakeBackfillingIndexDeleteOnly,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	return mutationStateChange(
		tbl,
		MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_BACKFILLING,
		descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_ADD,
	)
}

func (i *immediateVisitor) MakeDeleteOnlyIndexWriteOnly(
	ctx context.Context, op scop.MakeDeleteOnlyIndexWriteOnly,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	return mutationStateChange(
		tbl,
		MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_WRITE_ONLY,
		descpb.DescriptorMutation_ADD,
	)
}

func (i *immediateVisitor) MakeBackfilledIndexMerging(
	ctx context.Context, op scop.MakeBackfilledIndexMerging,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	return mutationStateChange(
		tbl,
		MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_MERGING,
		descpb.DescriptorMutation_ADD,
	)
}

func (i *immediateVisitor) MakeMergedIndexWriteOnly(
	ctx context.Context, op scop.MakeMergedIndexWriteOnly,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	return mutationStateChange(
		tbl,
		MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_MERGING,
		descpb.DescriptorMutation_WRITE_ONLY,
		descpb.DescriptorMutation_ADD,
	)
}

func (i *immediateVisitor) MakeValidatedPrimaryIndexPublic(
	ctx context.Context, op scop.MakeValidatedPrimaryIndexPublic,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	index, err := catalog.MustFindIndexByID(tbl, op.IndexID)
	if err != nil {
		return err
	}
	tbl.PrimaryIndex = index.IndexDescDeepCopy()
	_, err = RemoveMutation(
		tbl,
		MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_WRITE_ONLY,
	)
	return err
}

func (i *immediateVisitor) MakeValidatedSecondaryIndexPublic(
	ctx context.Context, op scop.MakeValidatedSecondaryIndexPublic,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}

	for idx, idxMutation := range tbl.GetMutations() {
		if idxMutation.GetIndex() != nil &&
			idxMutation.GetIndex().ID == op.IndexID {
			// If this is a rollback of a drop, we are trying to add the index back,
			// so swap the direction before making it complete.
			idxMutation.Direction = descpb.DescriptorMutation_ADD
			err := tbl.MakeMutationComplete(idxMutation)
			if err != nil {
				return err
			}
			tbl.Mutations = append(tbl.Mutations[:idx], tbl.Mutations[idx+1:]...)
			break
		}
	}
	if len(tbl.Mutations) == 0 {
		tbl.Mutations = nil
	}
	return nil
}

func (i *immediateVisitor) MakePublicPrimaryIndexWriteOnly(
	ctx context.Context, op scop.MakePublicPrimaryIndexWriteOnly,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	if tbl.GetPrimaryIndexID() != op.IndexID {
		return errors.AssertionFailedf("index being dropped (%d) does not match existing primary index (%d).", op.IndexID, tbl.PrimaryIndex.ID)
	}
	desc := tbl.GetPrimaryIndex().IndexDescDeepCopy()
	tbl.TableDesc().PrimaryIndex = descpb.IndexDescriptor{} // zero-out the current primary index
	return enqueueIndexMutation(tbl, &desc, descpb.DescriptorMutation_WRITE_ONLY, descpb.DescriptorMutation_DROP)
}

func (i *immediateVisitor) MakePublicSecondaryIndexWriteOnly(
	ctx context.Context, op scop.MakePublicSecondaryIndexWriteOnly,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil {
		return err
	}
	for i, idx := range tbl.PublicNonPrimaryIndexes() {
		if idx.GetID() == op.IndexID {
			desc := idx.IndexDescDeepCopy()
			tbl.Indexes = append(tbl.Indexes[:i], tbl.Indexes[i+1:]...)
			return enqueueIndexMutation(tbl, &desc, descpb.DescriptorMutation_WRITE_ONLY, descpb.DescriptorMutation_DROP)
		}
	}
	return errors.AssertionFailedf("failed to find secondary index %d in descriptor %v", op.IndexID, tbl)
}

func (i *immediateVisitor) MakeWriteOnlyIndexDeleteOnly(
	ctx context.Context, op scop.MakeWriteOnlyIndexDeleteOnly,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	idx, err := catalog.MustFindIndexByID(tbl, op.IndexID)
	if err != nil {
		return err
	}
	// It's okay if the index is in MERGING.
	exp := descpb.DescriptorMutation_WRITE_ONLY
	if idx.Merging() {
		exp = descpb.DescriptorMutation_MERGING
	}
	return mutationStateChange(
		tbl,
		MakeIndexIDMutationSelector(op.IndexID),
		exp, descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_DROP,
	)
}

func (i *immediateVisitor) RemoveDroppedIndexPartialPredicate(
	ctx context.Context, op scop.RemoveDroppedIndexPartialPredicate,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	mut, err := FindMutation(tbl, MakeIndexIDMutationSelector(op.IndexID))
	if err != nil {
		return err
	}
	idx := mut.AsIndex().IndexDesc()
	idx.Predicate = ""
	return nil
}

func (i *immediateVisitor) MakeIndexAbsent(ctx context.Context, op scop.MakeIndexAbsent) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	_, err = RemoveMutation(
		tbl,
		MakeIndexIDMutationSelector(op.IndexID),
		descpb.DescriptorMutation_DELETE_ONLY,
		descpb.DescriptorMutation_BACKFILLING,
	)
	return err
}

func (i *immediateVisitor) AddIndexPartitionInfo(
	ctx context.Context, op scop.AddIndexPartitionInfo,
) error {
	tbl, err := i.checkOutTable(ctx, op.Partitioning.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	index, err := catalog.MustFindIndexByID(tbl, op.Partitioning.IndexID)
	if err != nil {
		return err
	}
	index.IndexDesc().Partitioning = op.Partitioning.PartitioningDescriptor
	return nil
}

func (i *immediateVisitor) SetIndexName(ctx context.Context, op scop.SetIndexName) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	index, err := catalog.MustFindIndexByID(tbl, op.IndexID)
	if err != nil {
		return err
	}
	index.IndexDesc().Name = op.Name
	return nil
}

func (i *immediateVisitor) AddColumnToIndex(ctx context.Context, op scop.AddColumnToIndex) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	index, err := catalog.MustFindIndexByID(tbl, op.IndexID)
	if err != nil {
		return err
	}
	column, err := catalog.MustFindColumnByID(tbl, op.ColumnID)
	if err != nil {
		return err
	}
	indexDesc := index.IndexDesc()

	// We have a dependency rule that ensurse columns are added to the index in
	// ascending ordinal order. Each helper function validates this constraint.
	insertIntoNames := func(s *[]string) error {
		if len(*s) != int(op.Ordinal) {
			return errors.AssertionFailedf(
				"Expected to add index columns in order: column %s is being added at position %d but array %T has %v",
				column.GetName(), op.Ordinal, *s, *s)
		}
		*s = append(*s, column.GetName())
		return nil
	}
	insertIntoDirections := func(s *[]catenumpb.IndexColumn_Direction) error {
		if len(*s) != int(op.Ordinal) {
			return errors.AssertionFailedf(
				"Expected to add index columns in order: column %s is being added at position %d but array %T has %v",
				column.GetName(), op.Ordinal, *s, *s)
		}
		*s = append(*s, op.Direction)
		return nil
	}
	insertIntoIDs := func(s *[]descpb.ColumnID) error {
		if len(*s) != int(op.Ordinal) {
			return errors.AssertionFailedf(
				"Expected to add index columns in order: column %s is being added at position %d but array %T has %v",
				column.GetName(), op.Ordinal, *s, *s)
		}
		*s = append(*s, column.GetID())
		return nil
	}
	switch op.Kind {
	case scpb.IndexColumn_KEY:
		if err := insertIntoIDs(&indexDesc.KeyColumnIDs); err != nil {
			return err
		}
		if err := insertIntoNames(&indexDesc.KeyColumnNames); err != nil {
			return err
		}
		if err := insertIntoDirections(&indexDesc.KeyColumnDirections); err != nil {
			return err
		}
	case scpb.IndexColumn_KEY_SUFFIX:
		if err := insertIntoIDs(&indexDesc.KeySuffixColumnIDs); err != nil {
			return err
		}
	case scpb.IndexColumn_STORED:
		if err := insertIntoIDs(&indexDesc.StoreColumnIDs); err != nil {
			return err
		}
		if err := insertIntoNames(&indexDesc.StoreColumnNames); err != nil {
			return err
		}
	}
	// If this is a composite column, note that.
	if colinfo.CanHaveCompositeKeyEncoding(column.GetType()) &&
		// We don't need to track the composite column IDs for stored columns.
		op.Kind != scpb.IndexColumn_STORED {

		var colOrdMap catalog.TableColMap
		for i := 0; i < index.NumKeyColumns(); i++ {
			colOrdMap.Set(index.GetKeyColumnID(i), i)
		}
		for i := 0; i < index.NumKeySuffixColumns(); i++ {
			colOrdMap.Set(index.GetKeySuffixColumnID(i), i+index.NumKeyColumns())
		}
		indexDesc.CompositeColumnIDs = append(indexDesc.CompositeColumnIDs, column.GetID())
		cids := indexDesc.CompositeColumnIDs
		sort.Slice(cids, func(i, j int) bool {
			return colOrdMap.GetDefault(cids[i]) < colOrdMap.GetDefault(cids[j])
		})
	}
	// If this is an inverted column, note that.
	if indexDesc.Type == descpb.IndexDescriptor_INVERTED && op.ColumnID == indexDesc.InvertedColumnID() {
		indexDesc.InvertedColumnKinds = append(indexDesc.InvertedColumnKinds, op.InvertedKind)
	}
	return nil
}

func (i *immediateVisitor) RemoveColumnFromIndex(
	ctx context.Context, op scop.RemoveColumnFromIndex,
) error {
	tbl, err := i.checkOutTable(ctx, op.TableID)
	if err != nil || tbl.Dropped() {
		return err
	}
	index, err := catalog.MustFindIndexByID(tbl, op.IndexID)
	if err != nil || index.Dropped() {
		return err
	}
	column, err := catalog.MustFindColumnByID(tbl, op.ColumnID)
	if err != nil {
		return err
	}

	// We have a dependency rule when removing index columns, that we always
	// remove the column with the highest ordinal first. This approach ensures
	// that the slices are reduced in size incrementally without invalidating the
	// ordinal of subsequent elements.
	idx := index.IndexDesc()
	switch op.Kind {
	case scpb.IndexColumn_KEY:
		if int(op.Ordinal) >= len(idx.KeyColumnNames) {
			return errors.AssertionFailedf("invalid ordinal %d for key columns %v",
				op.Ordinal, idx.KeyColumnNames)
		}
		idx.KeyColumnIDs = append(idx.KeyColumnIDs[:op.Ordinal], idx.KeyColumnIDs[op.Ordinal+1:]...)
		idx.KeyColumnNames = append(idx.KeyColumnNames[:op.Ordinal], idx.KeyColumnNames[op.Ordinal+1:]...)
		idx.KeyColumnDirections = append(idx.KeyColumnDirections[:op.Ordinal], idx.KeyColumnDirections[op.Ordinal+1:]...)

		// SPILLY - need to maintain the InvertedColumnKinds
		// SPILLY - maybe we can just recompute it from scratch whenever removeing a key from an inverted index
		// SPILLY - the InvertedColumnKinds is always going to be 1 or 0 in length
		for i := len(idx.KeyColumnIDs) - 1; i >= 0 && idx.KeyColumnIDs[i] == 0; i-- {
			idx.KeyColumnNames = idx.KeyColumnNames[:i]
			idx.KeyColumnIDs = idx.KeyColumnIDs[:i]
			idx.KeyColumnDirections = idx.KeyColumnDirections[:i]
			if idx.Type == descpb.IndexDescriptor_INVERTED && i == len(idx.KeyColumnIDs)-1 {
				idx.InvertedColumnKinds = nil
			}
		}
	case scpb.IndexColumn_KEY_SUFFIX:
		if int(op.Ordinal) >= len(idx.KeySuffixColumnIDs) {
			return errors.AssertionFailedf("invalid ordinal %d for key suffix columns %v",
				op.Ordinal, idx.KeySuffixColumnIDs)
		}
		idx.KeySuffixColumnIDs = append(idx.KeySuffixColumnIDs[:op.Ordinal], idx.KeySuffixColumnIDs[op.Ordinal+1:]...)
	case scpb.IndexColumn_STORED:
		if int(op.Ordinal) >= len(idx.StoreColumnNames) {
			return errors.AssertionFailedf("invalid ordinal %d for stored columns %v",
				op.Ordinal, idx.StoreColumnNames)
		}
		// Remove the entry at the specified ordinal.
		idx.StoreColumnIDs = append(idx.StoreColumnIDs[:op.Ordinal], idx.StoreColumnIDs[op.Ordinal+1:]...)
		idx.StoreColumnNames = append(idx.StoreColumnNames[:op.Ordinal], idx.StoreColumnNames[op.Ordinal+1:]...)
	}
	// If this is a composite column, remove it from the list.
	if colinfo.CanHaveCompositeKeyEncoding(column.GetType()) &&
		// We don't need to track the composite column IDs for stored columns.
		op.Kind != scpb.IndexColumn_STORED {
		for i, colID := range idx.CompositeColumnIDs {
			if colID == column.GetID() {
				idx.CompositeColumnIDs = append(
					idx.CompositeColumnIDs[:i], idx.CompositeColumnIDs[i+1:]...,
				)
			}
		}
	}
	return nil
}

func (m *deferredVisitor) MaybeAddSplitForIndex(
	_ context.Context, op scop.MaybeAddSplitForIndex,
) error {
	m.AddIndexForMaybeSplitAndScatter(op.TableID, op.IndexID)
	return nil
}

func (i *immediateVisitor) AddIndexZoneConfig(_ context.Context, op scop.AddIndexZoneConfig) error {
	i.ImmediateMutationStateUpdater.UpdateSubzoneConfig(
		op.TableID, op.Subzone, op.SubzoneSpans, op.SubzoneIndexToDelete)
	return nil
}

func (i *immediateVisitor) AddPartitionZoneConfig(
	_ context.Context, op scop.AddPartitionZoneConfig,
) error {
	i.ImmediateMutationStateUpdater.UpdateSubzoneConfig(
		op.TableID, op.Subzone, op.SubzoneSpans, op.SubzoneIndexToDelete)
	return nil
}
