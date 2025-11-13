// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package opgen

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
)

func init() {
	// IndexPartitionEntry represents individual partitions that can be added
	// or dropped independently. Each partition in the hierarchy is represented
	// by its own element.
	opRegistry.register((*scpb.IndexPartitionEntry)(nil),
		toPublic(
			scpb.Status_ABSENT,
			to(scpb.Status_DROPPED,
				emit(func(this *scpb.IndexPartitionEntry) *scop.AddIndexPartitionEntry {
					return &scop.AddIndexPartitionEntry{
						PartitionEntry: *protoutil.Clone(this).(*scpb.IndexPartitionEntry),
					}
				}),
			),
			to(scpb.Status_PUBLIC),
		),
		toAbsent(
			scpb.Status_PUBLIC,
			to(scpb.Status_DROPPED,
				emit(func(this *scpb.IndexPartitionEntry) *scop.RemoveIndexPartitionEntry {
					return &scop.RemoveIndexPartitionEntry{
						PartitionEntry: *protoutil.Clone(this).(*scpb.IndexPartitionEntry),
					}
				}),
			),
			to(scpb.Status_ABSENT,
				revertible(false),
				// Emit DeletePartitionData to delete the partition's data in the non-revertible phase.
				// This operation uses MVCC delete range to efficiently delete all data.
				// The partition metadata is passed to the operation, and the actual key spans
				// will be computed during execution when we have access to the table descriptor
				// and codec.
				emit(func(this *scpb.IndexPartitionEntry) *scop.DeletePartitionData {
					partitionName := ""
					if lp := this.GetListPartition(); lp != nil {
						partitionName = lp.Name
					} else if rp := this.GetRangePartition(); rp != nil {
						partitionName = rp.Name
					}
					return &scop.DeletePartitionData{
						TableID:            this.TableID,
						IndexID:            this.IndexID,
						PartitionName:      partitionName,
						NumColumns:         this.NumColumns,
						NumImplicitColumns: this.NumImplicitColumns,
						PartitionPath:      this.PartitionPath,
						ListPartition:      this.GetListPartition(),
						RangePartition:     this.GetRangePartition(),
					}
				}),
			),
		),
	)
}
