// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package opt

import (
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
)

// RowLevelSecurityMeta contains metadata pertaining to row-level security
// policies that were enforced when building the query plan.
type RowLevelSecurityMeta struct {
	// IsInitialized indicates that the struct has been initialized. This gets
	// lazily initialized, only when the query plan building comes across a table
	// that is enabled for row-level security.
	IsInitialized bool

	// User is the user that constructed the metadata. This is important since
	// RLS policies differ based on the role of the user executing the query.
	User username.SQLUsername

	// HasAdminRole is true if the current user was part of the admin role when
	// creating the query plan. This is only set if at least one table in the
	// query had RLS enabled.
	HasAdminRole bool

	// PoliciesEnforced is the set of policies that were applied for each relation
	// in the query.
	PoliciesEnforced map[TableID]*catalog.PolicyIDSet
}

func (r *RowLevelSecurityMeta) MaybeInit(evalCtx *eval.Context, hasAdminRole bool) {
	if r.IsInitialized {
		return
	}
	r.User = evalCtx.SessionData().User()
	r.HasAdminRole = hasAdminRole
	r.PoliciesEnforced = make(map[TableID]*catalog.PolicyIDSet)
	r.IsInitialized = true
}

// AddPolicyUse is used to indicate the given policyID of a table was applied in
// the query.
func (r *RowLevelSecurityMeta) AddPolicyUse(tableID TableID, policyID descpb.PolicyID) {
	if set, found := r.PoliciesEnforced[tableID]; !found {
		newSet := catalog.MakePolicyIDSet(policyID)
		r.PoliciesEnforced[tableID] = &newSet
	} else {
		set.Add(policyID)
	}
}
