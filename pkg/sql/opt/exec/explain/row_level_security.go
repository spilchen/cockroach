// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package explain

import (
	"context"
	"fmt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
	"strings"
)

// PlanPolicies keeps track of the policies that are enforced for each relation
// in the query.
type PlanPolicies struct {
	enforced []PoliciesEnforced
}

// OutputFields will output to the explain details about the row-level security
// policies enforced.
func (p *PlanPolicies) OutputFields(ob *OutputBuilder) {
	for _, tab := range p.enforced {
		// String together the names of the policies in the format that the
		// optimizer applies the policies.
		var sb strings.Builder
		for _, policyName := range tab.permissivePolicies {
			if sb.Len() == 0 {
				sb.WriteString("(")
			} else {
				sb.WriteString(" OR ")
			}
			sb.WriteString(policyName.Normalize())
		}
		if sb.Len() == 0 {
			sb.WriteString("none applied")
		}

		key := fmt.Sprintf("row-level security policies for %q", tab.name)
		ob.AddTopLevelField(key, sb.String())
	}
}

// PoliciesEnforced tracks the policies, both permissive and restrictive,
// that were applied to the query for a specific relation.
type PoliciesEnforced struct {
	name               tree.Name
	permissivePolicies []tree.Name
	// TODO(136742): Add a slice for restrictive policies when we support those.
}

// PlanPoliciesFactory will build a PlanPolicies
type PlanPoliciesFactory struct {
	mem     *memo.Memo
	catalog cat.Catalog
}

func (p *PlanPoliciesFactory) Init(
	mem *memo.Memo, catalog cat.Catalog,
) {
	// SPILLY - only store Metadata??
	p.mem = mem
	p.catalog = catalog
}

// Build will generate and return a PlanPolicies struct based on the rls
// policies used in the query.
func (p *PlanPoliciesFactory) Build(ctx context.Context) (PlanPolicies, error) {
	var planPolicies PlanPolicies
	if p.catalog == nil {
		return PlanPolicies{}, errors.AssertionFailedf("catalog is needed to format the policies enforced")
	}
	md := p.mem.Metadata()
	for _, tableMeta := range md.AllTables() {
		ids, rlsActive := md.GetPoliciesEnforced(tableMeta.MetaID) // SPILLY - should we consider have a big check that avoids this loop entirely?
		if !rlsActive {
			// Skip this table as row-level security is not active.
			continue
		}
		enforcement := PoliciesEnforced{
			name: tableMeta.Table.Name(),
		}
		// Get the name of each policy enforced for inclusion in the explain output.
		// TODO(136742): We only support permissive policies here. We need to
		// add support for restrictive policies.
		for i := 0; i < tableMeta.Table.PolicyCount(tree.PolicyTypePermissive); i++ {
			policy := tableMeta.Table.Policy(tree.PolicyTypePermissive, i)
			if ids.Contains(policy.ID) {
				enforcement.permissivePolicies = append(enforcement.permissivePolicies, policy.Name)
			}
		}
		planPolicies.enforced = append(planPolicies.enforced, enforcement)
	}
	return planPolicies, nil
}
