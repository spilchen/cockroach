// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package optbuilder

import (
	"fmt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
)

// addRowLevelSecurityFilter adds a filter based on the expressions of
// applicable RLS policies. If RLS is enabled but no policies are applicable,
// all rows will be filtered out.
func (b *Builder) addRowLevelSecurityFilter(
	tabMeta *opt.TableMeta, tableScope *scope, cmdScope cat.PolicyCommandScope,
) {
	if !tabMeta.Table.IsRowLevelSecurityEnabled() || cmdScope == cat.PolicyScopeExempt {
		return
	}

	// Admin users are exempt from any RLS filtering.
	isAdmin, err := b.catalog.HasAdminRole(b.ctx)
	if err != nil {
		panic(err)
	}
	if isAdmin {
		return
	}

	if tabMeta.Table.Name() == "debug" {
		fmt.Printf("SPILLY: debug here\n")
	}

	strExpr := b.buildRowLevelSecurityUsingExpression(tabMeta, cmdScope)
	if strExpr == "" {
		// If no permissive policies apply, filter out all rows by adding a "false" expression.
		tableScope.expr = b.factory.ConstructSelect(tableScope.expr,
			memo.FiltersExpr{b.factory.ConstructFiltersItem(memo.FalseSingleton)})
		return
	}

	parsedExpr, err := parser.ParseExpr(strExpr)
	if err != nil {
		panic(err)
	}
	typedExpr := tableScope.resolveType(parsedExpr, types.Any)
	scalar := b.buildScalar(typedExpr, tableScope, nil, nil, nil)
	tableScope.expr = b.factory.ConstructSelect(tableScope.expr,
		memo.FiltersExpr{b.factory.ConstructFiltersItem(scalar)})
}

// buildRowLevelSecurityUsingExpression generates an expression for read
// operations by combining all applicable RLS policies. If no policies apply, an
// empty string is returned.
func (b *Builder) buildRowLevelSecurityUsingExpression(
	tabMeta *opt.TableMeta, cmdScope cat.PolicyCommandScope,
) (expr string) {
	for i := 0; i < tabMeta.Table.PolicyCount(tree.PolicyTypePermissive); i++ {
		policy := tabMeta.Table.Policy(tree.PolicyTypePermissive, i)

		if !policy.AppliesTo(b.checkPrivilegeUser, cmdScope) {
			continue
		}
		strExpr := policy.GetUsingExpr()
		if strExpr == "" {
			continue
		}
		expr = strExpr
		// TODO(136742): Apply multiple RLS policies.
		return
	}

	// TODO(136742): Add support for restrictive policies.

	return
}
