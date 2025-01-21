// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cat

import (
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
)

// PolicyCommandScope defines the scope of commands to which a policy is applied.
// It specifies whether a policy applies to specific SQL operations or if an operation
// is exempt from row-level security policies.
type PolicyCommandScope int

const (
	// PolicyScopeSelect indicates that the policy applies to SELECT operations.
	PolicyScopeSelect PolicyCommandScope = iota
	// PolicyScopeInsert indicates that the policy applies to INSERT operations.
	PolicyScopeInsert
	// PolicyScopeUpdate indicates that the policy applies to UPDATE operations.
	PolicyScopeUpdate
	// PolicyScopeDelete indicates that the policy applies to DELETE operations.
	PolicyScopeDelete
	// PolicyScopeExempt indicates that the operation is exempt from row-level security policies.
	PolicyScopeExempt
)

// Policy defines an interface for a row-level security (RLS) policy on a table.
// Policies use expressions to filter rows during read operations and/or restrict
// new rows during write operations.
type Policy interface {
	// Name returns the name of the policy. The name is unique within a table
	// and cannot be qualified.
	Name() tree.Name

	// GetUsingExpr returns the optional filter expression evaluated on rows during
	// read operations. If the policy does not define a USING expression, this returns
	// an empty string.
	GetUsingExpr() string

	// GetWithCheckExpr returns the optional validation expression applied to new rows
	// during write operations. If the policy does not define a WITH CHECK expression,
	// this returns an empty string.
	GetWithCheckExpr() string

	// AppliesTo checks whether the policy applies to the given usage.
	AppliesTo(user username.SQLUsername, commandScope PolicyCommandScope) bool
}
