// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scbuildstmt

import (
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

// CreatePolicy implements CREATE POLICY.
func CreatePolicy(b BuildCtx, n *tree.CreatePolicy) {
	// SPILLY - use var that Bergin added
	b.IncrementSchemaChangeCreateCounter("policy")

	tableElts := b.ResolveTable(n.TableName, ResolveParams{
		RequiredPrivilege: privilege.CREATE,
	})
	panicIfSchemaChangeIsDisallowed(tableElts, n)
	// SPILLY - don't use deprecated
	_, _, tbl := scpb.FindTable(tableElts)
	policyID := b.NextTablePolicyID(tbl.TableID)

	// SPILLY - when do I check if the policy name is already used?

	b.Add(&scpb.Policy{
		TableID:  tbl.TableID,
		PolicyID: policyID,
	})
	b.Add(&scpb.PolicyName{
		TableID:  tbl.TableID,
		PolicyID: policyID,
		Name:     string(n.PolicyName),
	})
}
