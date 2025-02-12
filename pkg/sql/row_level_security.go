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
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/util/intsets"
)

type optRLSConstraintBuilder struct {
	policies cat.Policies
	// lookupColumnOrdinal returns the table column ordinal of the ith column in
	// this constraint.
	lookupColumnOrdinal func(colID descpb.ColumnID) (int, error)
}

var _ cat.CheckConstraintBuilder = &optRLSConstraintBuilder{}

// GetStaticConstraint is part of the cat.CheckConstraintBuilder interface.
func (r *optRLSConstraintBuilder) GetStaticConstraint() cat.CheckConstraint {
	// By definition, the synthetic check constraint for RLS isn't known until
	// runtime. So, we return nil when requesting the static one.
	return nil
}

// Build is part of the cat.CheckConstraintBuilder interface.
func (r *optRLSConstraintBuilder) Build(
	ctx context.Context, oc cat.Catalog, user username.SQLUsername,
) cat.CheckConstraint {
	expr, colIDs := r.genExpression(ctx, oc, user)
	if expr == "" {
		panic(fmt.Sprintf("must return some expression but empty string returned for user: %v", user))
	}
	fmt.Printf("SPILLY: building RLS constraint for user %s: %s\n", user, expr)

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
	ctx context.Context, oc cat.Catalog, user username.SQLUsername,
) (string, descpb.ColumnIDs) {
	var sb strings.Builder

	// colIDs tracks the column IDs referenced in all the policy expressions
	// that are applied.
	var colIDs intsets.Fast

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

		if !p.AppliesToRole(user) {
			continue
		}
		// SPILLY - how to handle missing with check expression. This isn't correct.
		if p.WithCheckExpr == "" {
			continue
		}
		if sb.Len() != 0 {
			sb.WriteString(" OR ")
		}
		sb.WriteString("(")
		sb.WriteString(p.WithCheckExpr)
		sb.WriteString(")")

		colIDs = colIDs.Union(p.WithCheckColumnIDs)
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
