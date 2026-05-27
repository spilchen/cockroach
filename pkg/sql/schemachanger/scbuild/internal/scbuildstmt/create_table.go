// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scbuildstmt

import (
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scdecomp"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// createTableChecks is the cheap prefilter that decides whether CREATE TABLE
// should be handled by the declarative schema changer. It runs before any
// descriptor resolution; a return of false causes the dispatcher in process.go
// to panic NotImplementedError, which is recovered upstream and routes the
// statement back to the legacy schema changer.
//
// The accepted surface for the initial implementation is intentionally
// minimal: plain columns with NULL/NOT NULL and at most one column flagged
// inline PRIMARY KEY. Everything else falls back. Future PRs will widen the
// surface one feature at a time.
//
// The DSC path is also gated on the `unsafe_always` schema-changer mode so
// that the default `on` setting continues to use the legacy path for now.
// This avoids broad churn in existing end-to-end test fixtures that depend on
// the exact descriptor shape the legacy CREATE TABLE produces (mutation IDs,
// default-column IDs, primary-index naming, schema_locked). A follow-up PR
// will reconcile those differences and flip the gate.
func createTableChecks(
	n *tree.CreateTable,
	mode sessiondatapb.NewSchemaChangerMode,
	activeVersion clusterversion.ClusterVersion,
) bool {
	if mode != sessiondatapb.UseNewSchemaChangerUnsafeAlways {
		return false
	}
	if n.Persistence != tree.PersistencePermanent {
		return false
	}
	if n.AsSource != nil {
		return false
	}
	if n.PartitionByTable != nil {
		return false
	}
	if n.Locality != nil {
		return false
	}
	if len(n.StorageParams) > 0 {
		return false
	}
	if n.OnCommit != tree.CreateTableOnCommitUnset {
		return false
	}

	primaryKeyCount := 0
	for _, def := range n.Defs {
		col, ok := def.(*tree.ColumnTableDef)
		if !ok {
			// Non-*tree.ColumnTableDef defs all fall back for now.
			return false
		}
		if !isSupportedColumnTableDef(col) {
			return false
		}
		if col.PrimaryKey.IsPrimaryKey {
			primaryKeyCount++
		}
	}
	if primaryKeyCount > 1 {
		// Composite inline primary keys fall back for now.
		return false
	}
	return true
}

// isSupportedColumnTableDef returns true iff the column definition uses only
// the features supported by the initial DSC implementation: a name, a type,
// nullability, and optionally an inline non-sharded single-column PRIMARY KEY.
func isSupportedColumnTableDef(col *tree.ColumnTableDef) bool {
	if col.IsSerial {
		return false
	}
	if col.GeneratedIdentity.IsGeneratedAsIdentity {
		return false
	}
	if col.Hidden {
		return false
	}
	if col.Unique.IsUnique {
		return false
	}
	if col.DefaultExpr.Expr != nil {
		return false
	}
	if col.OnUpdateExpr.Expr != nil {
		return false
	}
	if len(col.CheckExprs) > 0 {
		return false
	}
	if col.References.Table != nil {
		return false
	}
	if col.Computed.Computed {
		return false
	}
	if col.Family.Name != "" || col.Family.Create {
		return false
	}
	if col.PrimaryKey.Sharded {
		return false
	}
	return true
}

// CreateTable implements CREATE TABLE for the declarative schema changer. It
// is only reached when createTableChecks has accepted the statement, so the
// body can assume:
//
//   - Persistence is permanent (no TEMPORARY, no UNLOGGED).
//   - There is no PARTITION BY, LOCALITY, STORAGE PARAMS, ON COMMIT, or AS
//     source.
//   - Every entry in n.Defs is a *tree.ColumnTableDef using only the
//     supported column features (see isSupportedColumnTableDef).
//   - At most one column has PrimaryKey.IsPrimaryKey set.
//
// Anything else either falls back via createTableChecks or, for issues that
// can only be detected after descriptor resolution (multi-region database,
// non-scalar column type), via NotImplementedErrorf below.
func CreateTable(b BuildCtx, n *tree.CreateTable) {
	dbElts, scElts := b.ResolveTargetObject(n.Table.ToUnresolvedObjectName(), privilege.CREATE)
	dbElement := dbElts.FilterDatabase().MustGetOneElement()
	schemaElement := scElts.FilterSchema().MustGetOneElement()
	dbNamespace := dbElts.FilterNamespace().MustGetOneElement()
	scNamespace := scElts.FilterNamespace().MustGetOneElement()

	// Multi-region databases are not yet supported: locality elements need to
	// be emitted, and we don't model that in this initial implementation.
	if regionConfig := dbElts.FilterDatabaseRegionConfig().MustGetZeroOrOneElement(); regionConfig != nil {
		panic(scerrors.NotImplementedErrorf(n,
			"create table is not yet supported on multi-region clusters"))
	}

	// Normalize the table name now that we know the database and schema.
	n.Table.SchemaName = tree.Name(scNamespace.Name)
	n.Table.CatalogName = tree.Name(dbNamespace.Name)
	n.Table.ExplicitCatalog = true
	n.Table.ExplicitSchema = true

	// Existence check. ResolveTypes catches collisions with type names too.
	existing := b.ResolveRelation(n.Table.ToUnresolvedObjectName(), ResolveParams{
		IsExistenceOptional: true,
		RequiredPrivilege:   privilege.USAGE,
		WithOffline:         true,
		ResolveTypes:        true,
	})
	if existing != nil && !existing.IsEmpty() {
		if n.IfNotExists {
			return
		}
		panic(sqlerrors.NewRelationAlreadyExistsError(n.Table.FQString()))
	}

	// Resolve column types up front so we can bail out via
	// NotImplementedErrorf (routing to legacy) BEFORE we allocate a
	// descriptor ID or emit any elements. Otherwise a UDT or array column
	// would burn a descriptor ID even though the statement ends up falling
	// back to legacy.
	resolvedTypes := make([]*types.T, len(n.Defs))
	for i, def := range n.Defs {
		col := def.(*tree.ColumnTableDef)
		typ, err := tree.ResolveType(b, col.Type, b.SemaCtx().TypeResolver)
		if err != nil {
			panic(err)
		}
		if !isSupportedScalarType(typ) {
			panic(scerrors.NotImplementedErrorf(n,
				redact.Sprintf("create table column type %s is not yet supported in the declarative schema changer",
					redact.SafeString(typ.SQLString()))))
		}
		resolvedTypes[i] = typ
	}

	tableID := b.GenerateUniqueDescID()
	b.IncrementSchemaChangeCreateCounter("table")

	// Table-level elements.
	tableElem := &scpb.Table{TableID: tableID}
	b.Add(tableElem)
	b.Add(&scpb.Namespace{
		DatabaseID:   dbElement.DatabaseID,
		SchemaID:     schemaElement.SchemaID,
		DescriptorID: tableID,
		Name:         string(n.Table.ObjectName),
	})
	b.Add(&scpb.SchemaChild{
		ChildObjectID: tableID,
		SchemaID:      schemaElement.SchemaID,
	})
	b.Add(&scpb.TableData{
		TableID:    tableID,
		DatabaseID: dbElement.DatabaseID,
	})

	// Walk the user-defined columns. Column IDs start at 1.
	var primaryKeyColumnID catid.ColumnID
	var storedColumnIDs []catid.ColumnID
	nextColumnID := catid.ColumnID(1)
	for i, def := range n.Defs {
		col := def.(*tree.ColumnTableDef)
		columnID := nextColumnID
		nextColumnID++
		addUserColumn(b, tableID, columnID, col, resolvedTypes[i])
		if col.PrimaryKey.IsPrimaryKey {
			primaryKeyColumnID = columnID
		} else {
			storedColumnIDs = append(storedColumnIDs, columnID)
		}
	}

	// Primary index. Either a user-supplied single-column PK (with all other
	// columns stored) or the implicit rowid path (with all user-defined
	// columns stored). The primary-key index name matches the legacy
	// convention so test fixtures and downstream tooling are unaffected by
	// the planner choice.
	pkIndexName := tabledesc.PrimaryKeyIndexName(string(n.Table.ObjectName))
	if primaryKeyColumnID != 0 {
		addUserPrimaryKey(b, tableID, pkIndexName, primaryKeyColumnID, storedColumnIDs)
	} else {
		addImplicitRowIDPrimaryKey(b, tableID, pkIndexName, nextColumnID, storedColumnIDs)
	}

	// Ownership and per-user privileges.
	ownerElem, userPrivElems := b.BuildUserPrivilegesFromDefaultPrivileges(
		dbElement, schemaElement, tableID, privilege.Tables, b.CurrentUser())
	b.Add(ownerElem)
	for _, p := range userPrivElems {
		b.Add(p)
	}

	// Mirror the legacy default: external SQL sessions create tables as
	// schema_locked unless the user opted out. The session-data check here is
	// the same one the legacy CREATE TABLE path uses (see create_table.go).
	if !b.SessionData().Internal && b.SessionData().CreateTableWithSchemaLocked {
		b.Add(&scpb.TableSchemaLocked{TableID: tableID})
	}

	b.LogEventForExistingTarget(tableElem)
}

// addUserColumn emits the elements for a single user-defined column. The
// type must already be resolved and supported (see isSupportedScalarType);
// the caller validates this up front so that we don't emit any elements for
// a statement that will eventually fall back to legacy.
func addUserColumn(
	b BuildCtx, tableID catid.DescID, columnID catid.ColumnID, def *tree.ColumnTableDef, typ *types.T,
) {
	b.Add(&scpb.Column{TableID: tableID, ColumnID: columnID})
	b.Add(&scpb.ColumnType{
		TableID:                 tableID,
		ColumnID:                columnID,
		TypeT:                   newTypeT(typ),
		ElementCreationMetadata: scdecomp.NewElementCreationMetadata(b.EvalCtx().Settings.Version.ActiveVersion(b)),
	})
	b.Add(&scpb.ColumnName{
		TableID:  tableID,
		ColumnID: columnID,
		Name:     string(def.Name),
	})
	// NOT NULL is emitted when the user asked for it explicitly OR when this
	// is the inline primary-key column (PK columns must be NOT NULL).
	if def.Nullable.Nullability == tree.NotNull || def.PrimaryKey.IsPrimaryKey {
		b.Add(&scpb.ColumnNotNull{TableID: tableID, ColumnID: columnID})
	}
}

// isSupportedScalarType returns true iff typ is a basic scalar the initial
// DSC implementation can emit. Arrays, UDTs/enums, collated strings, tuples,
// and pseudo-types fall back to legacy.
func isSupportedScalarType(typ *types.T) bool {
	switch typ.Family() {
	case types.ArrayFamily,
		types.TupleFamily,
		types.EnumFamily,
		types.CollatedStringFamily,
		types.AnyFamily,
		types.VoidFamily,
		types.UnknownFamily:
		return false
	}
	if typ.UserDefined() {
		return false
	}
	return true
}

// addUserPrimaryKey emits the primary-index elements for a CREATE TABLE that
// has an explicit single-column inline PRIMARY KEY clause. storedColumnIDs
// lists the non-key user columns to add to the primary index's STORE list.
func addUserPrimaryKey(
	b BuildCtx,
	tableID catid.DescID,
	indexName string,
	keyColumnID catid.ColumnID,
	storedColumnIDs []catid.ColumnID,
) {
	addPrimaryIndexElements(b, tableID, indexName, []catid.ColumnID{keyColumnID}, storedColumnIDs)
}

// addImplicitRowIDPrimaryKey emits the hidden rowid column and the primary
// index over it for a CREATE TABLE that did not specify a primary key. The
// column ID for the new rowid column is the next available column ID, and
// storedColumnIDs lists the user columns that go into the primary index's
// STORE list.
func addImplicitRowIDPrimaryKey(
	b BuildCtx,
	tableID catid.DescID,
	indexName string,
	rowIDColumnID catid.ColumnID,
	storedColumnIDs []catid.ColumnID,
) {
	b.Add(&scpb.Column{
		TableID:  tableID,
		ColumnID: rowIDColumnID,
	})
	b.Add(&scpb.ColumnHidden{
		TableID:  tableID,
		ColumnID: rowIDColumnID,
	})
	b.Add(&scpb.ColumnType{
		TableID:                 tableID,
		ColumnID:                rowIDColumnID,
		TypeT:                   newTypeT(types.Int),
		ElementCreationMetadata: scdecomp.NewElementCreationMetadata(b.EvalCtx().Settings.Version.ActiveVersion(b)),
	})
	b.Add(&scpb.ColumnNotNull{TableID: tableID, ColumnID: rowIDColumnID})
	b.Add(&scpb.ColumnName{
		TableID:  tableID,
		ColumnID: rowIDColumnID,
		Name:     "rowid",
	})
	// unique_rowid() default: parse and wrap the expression. See
	// alter_table_alter_column_set_default.go for the canonical idiom.
	rowIDExpr, err := parser.ParseExpr("unique_rowid()")
	if err != nil {
		panic(errors.NewAssertionErrorWithWrappedErrf(err, "parsing unique_rowid()"))
	}
	b.Add(&scpb.ColumnDefaultExpression{
		TableID:    tableID,
		ColumnID:   rowIDColumnID,
		Expression: *b.WrapExpression(tableID, rowIDExpr),
	})
	addPrimaryIndexElements(b, tableID, indexName, []catid.ColumnID{rowIDColumnID}, storedColumnIDs)
}

// addPrimaryIndexElements emits the PrimaryIndex, IndexName, and IndexColumn
// elements for the freshly-created table's primary index. The index always
// has ID=1; keyColumnIDs are added as KEY columns in the order given, and
// storedColumnIDs as STORED columns.
func addPrimaryIndexElements(
	b BuildCtx,
	tableID catid.DescID,
	indexName string,
	keyColumnIDs []catid.ColumnID,
	storedColumnIDs []catid.ColumnID,
) {
	const primaryIndexID = catid.IndexID(1)
	b.Add(&scpb.PrimaryIndex{
		Index: scpb.Index{
			TableID:  tableID,
			IndexID:  primaryIndexID,
			IsUnique: true,
		},
	})
	b.Add(&scpb.IndexName{
		TableID: tableID,
		IndexID: primaryIndexID,
		Name:    indexName,
	})
	for ord, colID := range keyColumnIDs {
		b.Add(&scpb.IndexColumn{
			TableID:       tableID,
			IndexID:       primaryIndexID,
			ColumnID:      colID,
			OrdinalInKind: uint32(ord),
			Kind:          scpb.IndexColumn_KEY,
			Direction:     catenumpb.IndexColumn_ASC,
		})
	}
	for ord, colID := range storedColumnIDs {
		b.Add(&scpb.IndexColumn{
			TableID:       tableID,
			IndexID:       primaryIndexID,
			ColumnID:      colID,
			OrdinalInKind: uint32(ord),
			Kind:          scpb.IndexColumn_STORED,
		})
	}
}
