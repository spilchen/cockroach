// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/util/intsets"
	"github.com/cockroachdb/errors"
)

type optRLSConstraintBuilder struct {
	policies cat.Policies
	// lookupColumnOrdinal returns the table column ordinal of the ith column
	lookupColumnOrdinal func(colID descpb.ColumnID) (int, error)
}

var _ cat.CheckConstraintBuilder = &optRLSConstraintBuilder{}

// GetStaticConstraint is part of the cat.CheckConstraintBuilder interface.
func (r *optRLSConstraintBuilder) GetStaticConstraint() cat.CheckConstraint {
	// By definition, the synthetic check constraint for RLS isn't known until
	// runtime. So, we return nil when requesting the static one.
	return nil
}

// IsRLSConstraint is part of the cat.CheckConstraintBuilder interface.
func (r *optRLSConstraintBuilder) IsRLSConstraint() bool { return true }

// Build is part of the cat.CheckConstraintBuilder interface.
func (r *optRLSConstraintBuilder) Build(
	ctx context.Context, oc cat.Catalog, user username.SQLUsername, isUpdate bool,
) cat.CheckConstraint {
	expr, colIDs := r.genExpression(ctx, oc, user, isUpdate)
	if expr == "" {
		panic(fmt.Sprintf("must return some expression but empty string returned for user: %v", user))
	}

	return &optCheckConstraint{
		constraint:  expr,
		validated:   true,
		columnCount: len(colIDs),
		lookupColumnOrdinal: func(i int) (int, error) {
			return r.lookupColumnOrdinal(colIDs[i])
		},
	}
}

func (r *optRLSConstraintBuilder) genExpression(
	ctx context.Context, oc cat.Catalog, user username.SQLUsername, isUpdate bool,
) (string, descpb.ColumnIDs) {
	var sb strings.Builder

	// colIDs tracks the column IDs referenced in all the policy expressions
	// that are applied.
	var colIDs intsets.Fast

	// SPILLY - I don't like where we have this build function. Is it possible to
	// move the code into optbuilder/row_level_security.go? That is where the
	// mutation builder is.

	// Admin users are exempt from any RLS policies.
	isAdmin, err := oc.HasAdminRole(ctx)
	if err != nil {
		panic(err)
	}
	if isAdmin {
		// Return a constraint check that always passes.
		return "true", nil
	}

	for i := range r.policies.Permissive {
		p := &r.policies.Permissive[i]

		if !p.AppliesToRole(user) || !r.policyAppliesToCommand(p, isUpdate) {
			continue
		}
		var expr string
		// If the WITH CHECK expression is missing, we default to the USING
		// expression. If both are missing, then this policy doesn't apply and can
		// be skipped.
		if p.WithCheckExpr == "" {
			if p.UsingExpr == "" {
				continue
			}
			expr = p.UsingExpr
			colIDs = colIDs.Union(p.UsingColumnIDs)
		} else {
			expr = p.WithCheckExpr
			colIDs = colIDs.Union(p.WithCheckColumnIDs)
		}
		if sb.Len() != 0 {
			sb.WriteString(" OR ")
		}
		sb.WriteString("(")
		sb.WriteString(expr)
		sb.WriteString(")")
		// TODO(136742): Add support for multiple policies.
		break
	}

	// TODO(136742): Add support for restrictive policies.

	// If no policies apply, then we will add a false check as nothing is allowed
	// to be written.
	if sb.Len() == 0 {
		return "(false)", nil
	}

	orderedColIDs := make(descpb.ColumnIDs, 0, colIDs.Len())
	colIDs.ForEach(func(id int) { orderedColIDs = append(orderedColIDs, descpb.ColumnID(id)) })

	return sb.String(), orderedColIDs
}

// policyAppliesToCommand will return true iff the command set in the policy
// applies to the current mutation action.
func (r *optRLSConstraintBuilder) policyAppliesToCommand(policy *cat.Policy, isUpdate bool) bool {
	switch policy.Command {
	case catpb.PolicyCommand_ALL:
		return true
	case catpb.PolicyCommand_SELECT, catpb.PolicyCommand_DELETE:
		return false
	case catpb.PolicyCommand_INSERT:
		return !isUpdate
	case catpb.PolicyCommand_UPDATE:
		return isUpdate
	default:
		panic(errors.AssertionFailedf("unknown policy command %v", policy.Command))
	}
}
