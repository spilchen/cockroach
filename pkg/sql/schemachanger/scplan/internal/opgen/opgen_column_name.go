// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package opgen

import (
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
)

func init() {
	opRegistry.register(
		(*scpb.ColumnName)(nil),
		toPublic(
			scpb.Status_ABSENT,
			to(scpb.Status_PUBLIC,
				emit(func(this *scpb.ColumnName) *scop.SetColumnName {
					return &scop.SetColumnName{
						TableID:  this.TableID,
						ColumnID: this.ColumnID,
						Name:     this.Name,
					}
				}),
			),
		),
		toTransientAbsentLikePublic(),
		toAbsent(
			scpb.Status_PUBLIC,
			to(scpb.Status_ABSENT,
				emit(func(this *scpb.ColumnName) *scop.SetColumnName {
					op := &scop.SetColumnName{
						TableID:  this.TableID,
						ColumnID: this.ColumnID,
						Name:     tabledesc.ColumnNamePlaceholder(this.ColumnID),
					}
					// If a placeholder name was provided for the transition to absent, override it.
					// This ensures the column reverts back to its original name in case a column rename
					// needs to roll back, such as when altering a column's type that requires a backfill,
					// and an error occurs during the backfill process.
					if this.UndoName != "" {
						op.Name = this.UndoName
					}
					return op
				}),
			),
		),
	)
}
