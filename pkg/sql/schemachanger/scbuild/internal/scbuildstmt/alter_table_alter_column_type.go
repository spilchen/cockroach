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
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
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

	// Check for elements depending on the column we are altering.
	walkColumnDependencies(b, col, "alter", "column type", func(e scpb.Element) {
		switch e := e.(type) {
		case *scpb.Column:
			if e.TableID == col.TableID && e.ColumnID == col.ColumnID {
				return
			}
			elts := b.QueryByID(e.TableID).Filter(hasColumnIDAttrFilter(e.ColumnID))
			_, _, computedColName := scpb.FindColumnName(elts.Filter(publicTargetFilter))
			panic(sqlerrors.NewColumnReferencedByComputedColumnError(t.Column.String(), computedColName.Name))
		case *scpb.View:
			_, _, ns := scpb.FindNamespace(b.QueryByID(col.TableID))
			_, _, nsDep := scpb.FindNamespace(b.QueryByID(e.ViewID))
			if nsDep.DatabaseID != ns.DatabaseID || nsDep.SchemaID != ns.SchemaID {
				panic(sqlerrors.NewDependentBlocksOpError("alter", "column type", t.Column.String(), "view", qualifiedName(b, e.ViewID)))
			}
			panic(sqlerrors.NewDependentBlocksOpError("alter", "column type", t.Column.String(), "view", nsDep.Name))
		case *scpb.FunctionBody:
			_, _, fnName := scpb.FindFunctionName(b.QueryByID(e.FunctionID))
			panic(sqlerrors.NewDependentObjectErrorf(
				"cannot alter data type of column %q because function %q depends on it",
				t.Column.String(), fnName.Name),
			)
		}
	})

	err := schemachange.ValidateAlterColumnTypeChecks(
		b, t, b.ClusterSettings().Version, newColType.Type,
		col.GeneratedAsIdentityType != catpb.GeneratedAsIdentityType_NOT_IDENTITY_COLUMN)
	if err != nil {
		panic(err)
	}

	validateAutomaticCastForNewType(b, tbl.TableID, colID, t.Column.String(),
		oldColType.Type, newColType.Type)

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
