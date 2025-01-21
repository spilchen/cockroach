// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package testcat

import (
	"context"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
)

// CreatePolicy handles the CREATE POLICY statement.
func (tc *Catalog) CreatePolicy(n *tree.CreatePolicy) {
	ctx := context.Background()
	tableName := n.TableName.ToTableName()
	ds, _, err := tc.ResolveDataSource(ctx, cat.Flags{}, &tableName)
	if err != nil {
		panic(err)
	}
	ts, isTable := ds.(*Table)
	if !isTable {
		panic(errors.New("policies can only be added to a table"))
	}

	foundPolicy, _, _ := ts.findPolicyByName(n.PolicyName)
	if foundPolicy != nil {
		panic(errors.Newf(`policy %q already exists on table %q`, n.PolicyName, ts.Name()))
	}

	policy := &Policy{
		name:    n.PolicyName,
		roles:   make(map[string]struct{}),
		command: n.Cmd,
	}
	if n.Cmd == tree.PolicyCommandDefault {
		policy.command = tree.PolicyCommandAll
	}
	if n.Exprs.Using != nil {
		policy.usingExpr = n.Exprs.Using.String()
	}
	if n.Exprs.WithCheck != nil {
		policy.withCheckExpr = n.Exprs.WithCheck.String()
	}
	// If all roles are omitted, we default to adding the PUBLIC role so that it
	// applies to everyone.
	if len(n.Roles) == 0 {
		policy.roles = nil
	}
	for _, r := range n.Roles {
		if r.Name == username.PublicRole {
			// Clear the roles to indicate the policy applies to all users (public role).
			policy.roles = nil
			break
		}
		policy.roles[r.Name] = struct{}{}
	}

	// Determine the policy type. Default to permissive if no type is specified.
	policyType := n.Type
	if policyType == tree.PolicyTypeDefault {
		policyType = tree.PolicyTypePermissive
	}
	// Add the policy to the appropriate map based on its type.
	ts.policies[policyType] = append(ts.policies[policyType], policy)
}
