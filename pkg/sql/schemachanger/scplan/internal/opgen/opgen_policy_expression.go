// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package opgen

import (
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
)

// SPILLY - split this file into 3

// SPILLY - do we need an opgen rule  to ensure ordering of policydeps with the
// expressions? At the least, for drop we want to call
// UpdateTableBackReferenceInTypes after we have added a new PolicyDeps (if
// replacing) or the policy has been completely removed.

func init() {
	opRegistry.register((*scpb.PolicyDeps)(nil),
		toPublic(
			scpb.Status_ABSENT,
			to(scpb.Status_PUBLIC,
				emit(func(this *scpb.PolicyDeps) *scop.SetPolicyForwardReferences {
					// Note: Even if the dependencies are empty, we still set the policy's
					// forward references. This ensures that any old forward dependencies
					// are properly cleared.
					return &scop.SetPolicyForwardReferences{Deps: *protoutil.Clone(this).(*scpb.PolicyDeps)}
				}),
				emit(func(this *scpb.PolicyDeps) *scop.UpdateTableBackReferencesInTypes {
					if len(this.UsesTypeIDs) == 0 {
						return nil
					}
					return &scop.UpdateTableBackReferencesInTypes{
						TypeIDs:               this.UsesTypeIDs,
						BackReferencedTableID: this.TableID,
					}
				}),
				emit(func(this *scpb.PolicyDeps) *scop.UpdateTableBackReferencesInSequences {
					if len(this.UsesSequenceIDs) == 0 {
						return nil
					}
					return &scop.UpdateTableBackReferencesInSequences{
						SequenceIDs:           this.UsesSequenceIDs,
						BackReferencedTableID: this.TableID,
					}
				}),
			),
		),
		toAbsent(
			scpb.Status_PUBLIC,
			to(scpb.Status_ABSENT,
				emit(func(this *scpb.PolicyDeps) *scop.UpdateTableBackReferencesInTypes {
					if len(this.UsesTypeIDs) == 0 {
						return nil
					}
					return &scop.UpdateTableBackReferencesInTypes{
						TypeIDs:               this.UsesTypeIDs,
						BackReferencedTableID: this.TableID,
					}
				}),
				emit(func(this *scpb.PolicyDeps) *scop.UpdateTableBackReferencesInSequences {
					if len(this.UsesSequenceIDs) == 0 {
						return nil
					}
					return &scop.UpdateTableBackReferencesInSequences{
						SequenceIDs:           this.UsesSequenceIDs,
						BackReferencedTableID: this.TableID,
					}
				}),
			),
		),
	)

	opRegistry.register((*scpb.PolicyWithCheckExpr)(nil),
		toPublic(
			scpb.Status_ABSENT,
			to(scpb.Status_PUBLIC,
				emit(func(this *scpb.PolicyWithCheckExpr) *scop.AddPolicyExpression {
					return emitAddPolicyExpression(this.TableID, this.PolicyID, this.Expr, true)
				}),
				// SPILLY - emit backreferences for functions
			),
		),
		toAbsent(
			scpb.Status_PUBLIC,
			to(scpb.Status_ABSENT,
				emit(func(this *scpb.PolicyWithCheckExpr) *scop.RemovePolicyExpression {
					return emitRemovePolicyExpression(this.TableID, this.PolicyID, this.Expr, true)
				}),
				// SPILLY - emit backreferences for functions
			),
		),
	)

	opRegistry.register((*scpb.PolicyUsingExpr)(nil),
		toPublic(
			scpb.Status_ABSENT,
			to(scpb.Status_PUBLIC,
				emit(func(this *scpb.PolicyUsingExpr) *scop.AddPolicyExpression {
					return emitAddPolicyExpression(this.TableID, this.PolicyID, this.Expr, false)
				}),
				// SPILLY - emit backreferences for functions
			),
		),
		toAbsent(
			scpb.Status_PUBLIC,
			to(scpb.Status_ABSENT,
				emit(func(this *scpb.PolicyUsingExpr) *scop.RemovePolicyExpression {
					return emitRemovePolicyExpression(this.TableID, this.PolicyID, this.Expr, false)
				}),
				// SPILLY - emit backreferences for functions
			),
		),
	)
}

func emitAddPolicyExpression(
	tableID descpb.ID, policyID descpb.PolicyID, expr catpb.Expression, isWithCheckExpr bool,
) *scop.AddPolicyExpression {
	return &scop.AddPolicyExpression{
		TableID:               tableID,
		PolicyID:              policyID,
		Expr:                  expr,
		IsWithCheckExpression: isWithCheckExpr,
	}
}

func emitRemovePolicyExpression(
	tableID descpb.ID, policyID descpb.PolicyID, expr catpb.Expression, isWithCheckExpr bool,
) *scop.RemovePolicyExpression {
	return &scop.RemovePolicyExpression{
		TableID:               tableID,
		PolicyID:              policyID,
		Expr:                  expr,
		IsWithCheckExpression: isWithCheckExpr,
	}
}
