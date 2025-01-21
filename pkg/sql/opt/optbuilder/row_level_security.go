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

// addRowLevelSecurityPredicates will add predicates for any expressions of
// applicable RLS policies.
func (b *Builder) addRowLevelSecurityPredicates(
	tabMeta *opt.TableMeta, tableScope *scope, cmdScope cat.PolicyCommandScope,
) {
	if !tabMeta.Table.IsRowLevelSecurityEnabled() || cmdScope == cat.PolicyScopeExempt {
		return
	}
	fmt.Printf("SPILLY: table has RLS enabled. %d policies defined\n", tabMeta.Table.PolicyCount(tree.PolicyTypePermissive))

	for i := 0; i < tabMeta.Table.PolicyCount(tree.PolicyTypePermissive); i++ {
		policy := tabMeta.Table.Policy(tree.PolicyTypePermissive, i)

		if !policy.AppliesTo(b.checkPrivilegeUser, cmdScope) {
			continue
		}
		strExpr := policy.GetUsingExpr()
		if strExpr == "" {
			continue
		}

		parsedExpr, err := parser.ParseExpr(strExpr)
		if err != nil {
			panic(err)
		}
		typedExpr := tableScope.resolveType(parsedExpr, types.Any)
		scalar := b.buildScalar(typedExpr, tableScope, nil, nil, nil)
		tableScope.expr = b.factory.ConstructSelect(tableScope.expr,
			memo.FiltersExpr{b.factory.ConstructFiltersItem(scalar)})
		fmt.Printf("added filter expr: %s\n", strExpr)
		// TODO(136742): Apply multiple RLS policies.
		return
	}

	// SPILLY - leave a todo for restrictive policies

	// SPILLY - create the tableScope.expr once. Can we just return a string expression and parse that?

	// If no permissive policies apply, filter out all rows by adding a "false" expression.
	tableScope.expr = b.factory.ConstructSelect(tableScope.expr,
		memo.FiltersExpr{b.factory.ConstructFiltersItem(memo.FalseSingleton)})
}
