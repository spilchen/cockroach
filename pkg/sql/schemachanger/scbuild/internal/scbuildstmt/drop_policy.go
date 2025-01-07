// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scbuildstmt

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

// DropPolicy implements DROP POLICY.
func DropPolicy(b BuildCtx, n *tree.DropPolicy) {
	// SPILLY - use var that Bergin added
	tableElems := b.ResolveTable(n.TableName, ResolveParams{
		IsExistenceOptional: n.IfExists,
		RequireOwnership:    true,
	})
	// SPILLY - don't use deprecated
	// SPILLY - add tests for If not exist when trigger doesn't exist and when table doesn't exist
	_, _, tbl := scpb.FindTable(tableElems)
	if tbl == nil {
		return
	}
	panicIfSchemaChangeIsDisallowed(tableElems, n)

	policyElems := b.ResolvePolicy(tbl.TableID, n.PolicyName, ResolveParams{
		IsExistenceOptional: n.IfExists,
	})
	// SPILLY - ensure that we drop the table that we drop the policy too
	policy := policyElems.FilterPolicy().MustGetZeroOrOneElement()
	if policy == nil {
		return // this can happen IF EXISTS was used with the drop
	}
	policyElems.ForEach(func(_ scpb.Status, _ scpb.TargetStatus, e scpb.Element) {
		switch e.(type) {
		case *scpb.Policy, *scpb.PolicyName:
			b.Drop(e)
		}
	})
	b.IncrementSchemaChangeDropCounter("policy")
}
