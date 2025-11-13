// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scop

import (
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/redact"
)

//go:generate go run ./generate_visitor.go scop Backfill backfill.go backfill_visitor_generated.go

// Make sure baseOp is used for linter.
type backfillOp struct{ baseOp }

// Type implements the Op interface.
func (backfillOp) Type() Type { return BackfillType }

// BackfillIndex specifies an index backfill operation.
type BackfillIndex struct {
	backfillOp
	TableID       descpb.ID
	SourceIndexID descpb.IndexID
	IndexID       descpb.IndexID
}

func (BackfillIndex) Description() redact.RedactableString {
	return "Backfilling index"
}

// MergeIndex specifies an index merge operation.
type MergeIndex struct {
	backfillOp
	TableID           descpb.ID
	TemporaryIndexID  descpb.IndexID
	BackfilledIndexID descpb.IndexID
}

func (MergeIndex) Description() redact.RedactableString {
	return "Merging index"
}

// DeletePartitionData specifies a partition data deletion operation.
// This operation deletes all data in a partition's key range using MVCC delete range.
// This is a non-revertible operation that runs in the PostCommitNonRevertiblePhase.
type DeletePartitionData struct {
	backfillOp
	TableID           descpb.ID
	IndexID           descpb.IndexID
	PartitionName     string
	NumColumns        uint32 // Number of columns used in partitioning at this level
	NumImplicitColumns uint32 // Number of columns that implicitly prefix the index
	// PartitionPath represents the hierarchical path to this partition.
	PartitionPath []string
	// Exactly one of ListPartition or RangePartition must be set.
	ListPartition  *catpb.PartitioningDescriptor_List
	RangePartition *catpb.PartitioningDescriptor_Range
}

func (DeletePartitionData) Description() redact.RedactableString {
	return "Deleting partition data"
}

// Make sure baseOp is used for linter.
var _ = backfillOp{baseOp: baseOp{}}
