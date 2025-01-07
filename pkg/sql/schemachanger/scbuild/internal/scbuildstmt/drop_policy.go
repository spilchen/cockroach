// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scbuildstmt

import (
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgnotice"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

// DropPolicy implements DROP POLICY.
func DropPolicy(b BuildCtx, n *tree.DropPolicy) {
	noticeSender := b.EvalCtx().ClientNoticeSender
	failIfRLSIsNotEnabled(b)
	tableElems := b.ResolveTable(n.TableName, ResolveParams{
		IsExistenceOptional: n.IfExists,
		RequireOwnership:    true,
	})
	// SPILLY - add tests to create a policy with an existing name
	// SPILLY - add tests for If not exist when trigger doesn't exist and when table doesn't exist. Confirm notice.
	tbl := tableElems.FilterTable().MustGetZeroOrOneElement()
	if tbl == nil {
		noticeSender.BufferClientNotice(b,
			pgnotice.Newf("relation %q does not exist, skipping", n.TableName.String()))
		return
	}
	panicIfSchemaChangeIsDisallowed(tableElems, n)

	policyElems := b.ResolvePolicy(tbl.TableID, n.PolicyName, ResolveParams{
		IsExistenceOptional: n.IfExists,
	})
	policy := policyElems.FilterPolicy().MustGetZeroOrOneElement()
	if policy == nil {
		noticeSender.BufferClientNotice(b,
			pgnotice.Newf("policy %q for relation %q does not exist, skipping",
				n.PolicyName, n.TableName.String()))
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
