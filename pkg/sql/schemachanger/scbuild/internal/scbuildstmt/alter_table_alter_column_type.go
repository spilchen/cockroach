// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package scbuildstmt

import (
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachange"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/cast"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
)

func alterTableAlterColumnType(
	b BuildCtx, tn *tree.TableName, tbl *scpb.Table, t *tree.AlterTableAlterColumnType,
) {
	alterColumnPreChecks(b, tn, tbl, t.Column)
	colID := getColumnIDFromColumnName(b, tbl.TableID, t.Column, true /* required */)
	col := mustRetrieveColumnElem(b, tbl.TableID, colID)
	panicIfSystemColumn(col, t.Column.String())

	// Setup for the new type ahead of any checking. As we need its resolved type
	// for the checks.
	oldColType := retrieveColumnTypeElem(b, tbl.TableID, colID)
	newColType := *oldColType
	newColType.TypeT = b.ResolveTypeRef(t.ToType)
	// Increment a counter to indicate to DSC that we are changing an existing
	// column.
	newColType.SeqNum++

	// Check for dependent columns.
	//depCols := retrieveDependentColumnElem(b, tbl.TableID, colID)
	// SPILLY - need to go through View_Reference

	err := schemachange.ValidateAlterColumnTypeChecks(
		b, t, b.ClusterSettings().Version, newColType.Type,
		col.GeneratedAsIdentityType != catpb.GeneratedAsIdentityType_NOT_IDENTITY_COLUMN)
	if err != nil {
		panic(err)
	}

	validateAutomaticCastForNewType(b, tbl.TableID, colID, t.Column.String(),
		oldColType.Type, newColType.Type)
	// SPILLY - do we need to possible decrement seqNum for an undo or transition to delete?

	// We currently only support trivial conversions. Fallback to legacy schema
	// changer if not trivial.
	kind, err := schemachange.ClassifyConversionFromTree(b, t, oldColType.Type, newColType.Type)
	if err != nil {
		panic(err)
	}
	if kind != schemachange.ColumnConversionTrivial {
		panic(scerrors.NotImplementedErrorf(t,
			"alter type conversion not supported in the declarative schema changer"))
	}

	// Okay to proceed with DSC. Queue up the change.
	b.Add(&newColType)
}

// ValidateColExprForNewType will ensure that the existing expressions for
// DEFAULT and ON UPDATE will work for the new data type.
func validateAutomaticCastForNewType(
	b BuildCtx, tableID catid.DescID, colID catid.ColumnID, colName string, fromType, toType *types.T,
) {
	columnElements(b, tableID, colID).ForEach(func(
		_ scpb.Status, _ scpb.TargetStatus, e scpb.Element,
	) {
		var exprType string
		switch e.(type) {
		case *scpb.ColumnDefaultExpression:
			exprType = "default"
		case *scpb.ColumnOnUpdateExpression:
			exprType = "on update"
		}
		if exprType != "" {
			if validCast := cast.ValidCast(fromType, toType, cast.ContextAssignment); !validCast {
				panic(pgerror.Newf(
					pgcode.DatatypeMismatch,
					"%s for column %q cannot be cast automatically to type %s",
					exprType,
					colName,
					toType.SQLString(),
				))
			}
		}
	})
}
