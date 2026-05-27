// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scbuildstmt

import (
	"testing"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/stretchr/testify/require"
)

// TestCreateTableAlwaysRoutesToLegacy verifies that the routing infrastructure
// added in commit 1 always rejects CREATE TABLE, so every statement falls back
// to the legacy schema changer. Commit 2 will replace the "every CREATE TABLE
// is rejected" assertions with a narrower set that allows the trivial accepted
// surface.
func TestCreateTableAlwaysRoutesToLegacy(t *testing.T) {
	modes := []sessiondatapb.NewSchemaChangerMode{
		sessiondatapb.UseNewSchemaChangerOff,
		sessiondatapb.UseNewSchemaChangerOn,
		sessiondatapb.UseNewSchemaChangerUnsafe,
		sessiondatapb.UseNewSchemaChangerUnsafeAlways,
	}

	tests := []struct {
		name string
		stmt *tree.CreateTable
	}{
		{name: "empty CREATE TABLE", stmt: &tree.CreateTable{}},
		{name: "CREATE TABLE IF NOT EXISTS", stmt: &tree.CreateTable{IfNotExists: true}},
		{name: "CREATE TEMPORARY TABLE", stmt: &tree.CreateTable{Persistence: tree.PersistenceTemporary}},
		{name: "CREATE UNLOGGED TABLE", stmt: &tree.CreateTable{Persistence: tree.PersistenceUnlogged}},
		{name: "CREATE TABLE AS", stmt: &tree.CreateTable{AsSource: &tree.Select{}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range modes {
				require.False(t,
					IsFullySupportedWithFalsePositive(tc.stmt, clusterversion.ClusterVersion{}, mode),
					"mode=%v: CREATE TABLE must always route to legacy in commit 1", mode)
			}
		})
	}
}

// TestCreateTableBodyPanicsNotImplemented documents the belt-and-suspenders
// behavior: even if some future change accidentally lets a statement through
// the checks function, the builder body itself still raises a
// NotImplementedError so we fall back gracefully instead of emitting a
// half-built descriptor.
func TestCreateTableBodyPanicsNotImplemented(t *testing.T) {
	defer func() {
		r := recover()
		require.NotNil(t, r, "CreateTable body must panic in commit 1")
		err, ok := r.(error)
		require.True(t, ok, "panic value must be an error, got %T", r)
		require.True(t, scerrors.HasNotImplemented(err),
			"panic must be a NotImplementedError, got %+v", err)
	}()
	CreateTable(nil /* b */, &tree.CreateTable{})
}
