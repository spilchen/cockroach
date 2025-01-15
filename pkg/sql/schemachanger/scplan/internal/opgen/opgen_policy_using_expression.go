// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package opgen

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
)

// SPILLY - do we need an opgen rule  to ensure ordering of policydeps with the
// expressions? At the least, for drop we want to call
// UpdateTableBackReferenceInTypes after we have added a new PolicyDeps (if
// replacing) or the policy has been completely removed.

func init() {
	opRegistry.register((*scpb.PolicyUsingExpr)(nil),
		toPublic(
			scpb.Status_ABSENT,
			to(scpb.Status_PUBLIC,
				emit(func(this *scpb.PolicyUsingExpr) *scop.AddPolicyUsingExpression {
					return &scop.AddPolicyUsingExpression{
						El: *protoutil.Clone(this).(*scpb.PolicyUsingExpr),
					}
				}),
			),
		),
		toAbsent(
			scpb.Status_PUBLIC,
			to(scpb.Status_ABSENT,
				emit(func(this *scpb.PolicyUsingExpr) *scop.RemovePolicyUsingExpression {
					return &scop.RemovePolicyUsingExpression{
						TableID:  this.TableID,
						PolicyID: this.PolicyID,
					}
				}),
			),
		),
	)
}
