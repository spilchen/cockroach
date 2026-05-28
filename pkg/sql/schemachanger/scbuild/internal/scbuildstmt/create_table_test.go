// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scbuildstmt

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/stretchr/testify/require"
)

// TestCreateTableChecksAcceptsTrivialSurface asserts that createTableChecks
// returns true for the small subset of CREATE TABLE that the declarative
// schema changer can handle. Each accepted shape uses only the supported
// features: plain columns, NULL/NOT NULL, IF NOT EXISTS, and at most one
// inline single-column PRIMARY KEY.
func TestCreateTableChecksAcceptsTrivialSurface(t *testing.T) {
	col := func(name string, opts ...func(*tree.ColumnTableDef)) *tree.ColumnTableDef {
		c := &tree.ColumnTableDef{Name: tree.Name(name), Type: types.Int}
		for _, opt := range opts {
			opt(c)
		}
		return c
	}
	notNull := func(c *tree.ColumnTableDef) {
		c.Nullable.Nullability = tree.NotNull
	}
	primaryKey := func(c *tree.ColumnTableDef) {
		c.PrimaryKey.IsPrimaryKey = true
	}

	tests := []struct {
		name string
		stmt *tree.CreateTable
	}{
		{
			name: "single int column",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a")}},
		},
		{
			name: "multiple columns with NOT NULL",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", notNull), col("b")}},
		},
		{
			name: "single-column inline primary key",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", primaryKey), col("b")}},
		},
		{
			name: "IF NOT EXISTS",
			stmt: &tree.CreateTable{IfNotExists: true, Defs: tree.TableDefs{col("a")}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t,
				createTableChecks(tc.stmt, sessiondatapb.UseNewSchemaChangerOn, clusterversion.ClusterVersion{}),
				"expected the trivial accepted surface to pass createTableChecks")
		})
	}
}

// TestCreateTableChecksAcceptsUnderAllDSCModes locks in the gate flip: the
// trivial accepted surface passes createTableChecks under every DSC mode the
// dispatcher delegates to (UseNewSchemaChangerOff is short-circuited
// upstream, so it is not exercised here).
func TestCreateTableChecksAcceptsUnderAllDSCModes(t *testing.T) {
	stmt := &tree.CreateTable{
		Defs: tree.TableDefs{&tree.ColumnTableDef{Name: tree.Name("a"), Type: types.Int}},
	}
	for _, mode := range []sessiondatapb.NewSchemaChangerMode{
		sessiondatapb.UseNewSchemaChangerOn,
		sessiondatapb.UseNewSchemaChangerUnsafe,
		sessiondatapb.UseNewSchemaChangerUnsafeAlways,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			require.True(t,
				createTableChecks(stmt, mode, clusterversion.ClusterVersion{}),
				"createTableChecks should accept the trivial surface under mode=%v", mode)
		})
	}
}

// TestCreateTableChecksRejectsUnsupported covers each individual reject
// branch in createTableChecks. Each case sets exactly one unsupported feature
// on top of an otherwise-accepted statement.
func TestCreateTableChecksRejectsUnsupported(t *testing.T) {
	col := func(name string, opts ...func(*tree.ColumnTableDef)) *tree.ColumnTableDef {
		c := &tree.ColumnTableDef{Name: tree.Name(name), Type: types.Int}
		for _, opt := range opts {
			opt(c)
		}
		return c
	}
	// Per-feature option functions, named after the feature each disables.
	serial := func(c *tree.ColumnTableDef) { c.IsSerial = true }
	generatedIdentity := func(c *tree.ColumnTableDef) { c.GeneratedIdentity.IsGeneratedAsIdentity = true }
	hidden := func(c *tree.ColumnTableDef) { c.Hidden = true }
	inlineUnique := func(c *tree.ColumnTableDef) { c.Unique.IsUnique = true }
	defaultExpr := func(c *tree.ColumnTableDef) { c.DefaultExpr.Expr = tree.DNull }
	onUpdate := func(c *tree.ColumnTableDef) { c.OnUpdateExpr.Expr = tree.DNull }
	check := func(c *tree.ColumnTableDef) {
		c.CheckExprs = []tree.ColumnTableDefCheckExpr{{Expr: tree.DNull}}
	}
	references := func(c *tree.ColumnTableDef) { c.References.Table = &tree.TableName{} }
	computed := func(c *tree.ColumnTableDef) { c.Computed.Computed = true }
	familyName := func(c *tree.ColumnTableDef) { c.Family.Name = "f1" }
	familyCreate := func(c *tree.ColumnTableDef) { c.Family.Create = true }
	primaryKey := func(c *tree.ColumnTableDef) { c.PrimaryKey.IsPrimaryKey = true }
	shardedPrimaryKey := func(c *tree.ColumnTableDef) {
		c.PrimaryKey.IsPrimaryKey = true
		c.PrimaryKey.Sharded = true
	}
	primaryKeyWithStorageParams := func(c *tree.ColumnTableDef) {
		c.PrimaryKey.IsPrimaryKey = true
		c.PrimaryKey.StorageParams = tree.StorageParams{{Key: "s2_max_level"}}
	}

	baseDefs := func() tree.TableDefs {
		return tree.TableDefs{col("a")}
	}

	tests := []struct {
		name string
		stmt *tree.CreateTable
	}{
		{
			name: "TEMPORARY persistence",
			stmt: &tree.CreateTable{Persistence: tree.PersistenceTemporary, Defs: baseDefs()},
		},
		{
			name: "UNLOGGED persistence",
			stmt: &tree.CreateTable{Persistence: tree.PersistenceUnlogged, Defs: baseDefs()},
		},
		{
			name: "CREATE TABLE AS",
			stmt: &tree.CreateTable{AsSource: &tree.Select{}, Defs: baseDefs()},
		},
		{
			name: "PARTITION BY",
			stmt: &tree.CreateTable{
				PartitionByTable: &tree.PartitionByTable{},
				Defs:             baseDefs(),
			},
		},
		{
			name: "LOCALITY",
			stmt: &tree.CreateTable{Locality: &tree.Locality{}, Defs: baseDefs()},
		},
		{
			name: "STORAGE PARAMS",
			stmt: &tree.CreateTable{
				StorageParams: tree.StorageParams{{Key: "fillfactor"}},
				Defs:          baseDefs(),
			},
		},
		{
			name: "ON COMMIT clause",
			stmt: &tree.CreateTable{
				OnCommit: tree.CreateTableOnCommitPreserveRows,
				Defs:     baseDefs(),
			},
		},
		{
			name: "non-column table def (FAMILY)",
			stmt: &tree.CreateTable{
				Defs: tree.TableDefs{col("a"), &tree.FamilyTableDef{}},
			},
		},
		{
			name: "SERIAL column",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", serial)}},
		},
		{
			name: "generated identity column",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", generatedIdentity)}},
		},
		{
			name: "hidden column",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", hidden)}},
		},
		{
			name: "column with DEFAULT",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", defaultExpr)}},
		},
		{
			name: "column with ON UPDATE",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", onUpdate)}},
		},
		{
			name: "column with CHECK",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", check)}},
		},
		{
			name: "column with FAMILY name",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", familyName)}},
		},
		{
			name: "column with FAMILY create",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", familyCreate)}},
		},
		{
			name: "column with inline UNIQUE",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", inlineUnique)}},
		},
		{
			name: "column with REFERENCES",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", references)}},
		},
		{
			name: "computed column",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", computed)}},
		},
		{
			name: "sharded primary key",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", shardedPrimaryKey)}},
		},
		{
			name: "primary key with inline storage params",
			stmt: &tree.CreateTable{Defs: tree.TableDefs{col("a", primaryKeyWithStorageParams)}},
		},
		{
			name: "multiple inline primary keys",
			stmt: &tree.CreateTable{
				Defs: tree.TableDefs{col("a", primaryKey), col("b", primaryKey)},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t,
				createTableChecks(tc.stmt, sessiondatapb.UseNewSchemaChangerOn, clusterversion.ClusterVersion{}),
				"expected createTableChecks to reject %s", tc.name)
		})
	}
}
