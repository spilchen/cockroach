// Copyright 2026 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scbuildstmt

import (
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scerrors"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
)

// createTableChecks is the cheap prefilter that decides whether CREATE TABLE
// should be handled by the declarative schema changer. It mirrors the role of
// alterTableChecks: the dispatcher in process.go calls it before resolving any
// descriptors, and a return of false routes the statement back to the legacy
// schema changer.
//
// Commit 1 always returns false. Commit 2 starts whitelisting a trivial subset
// (basic columns and an optional single-column inline PRIMARY KEY). Subsequent
// PRs will widen the surface one feature at a time.
func createTableChecks(
	n *tree.CreateTable,
	mode sessiondatapb.NewSchemaChangerMode,
	activeVersion clusterversion.ClusterVersion,
) bool {
	return false
}

// CreateTable implements CREATE TABLE. Commit 1 only wires the dispatch entry;
// the body itself unconditionally falls back to the legacy schema changer by
// panicking with a NotImplementedError. The panic is recovered upstream in
// schema_change_plan_node.go, which then routes the statement to the legacy
// planner.
//
// The unconditional panic here is defense-in-depth: createTableChecks is the
// real gate, but if a future change ever lets a statement through without
// landing real builder logic, we still fall back gracefully rather than emit a
// half-built descriptor.
func CreateTable(b BuildCtx, n *tree.CreateTable) {
	panic(scerrors.NotImplementedErrorf(n,
		"create table is not yet supported in the declarative schema changer"))
}
