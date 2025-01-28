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
			sb.WriteString(policyName)
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
	// SPILLY - strings really??
	name               string
	permissivePolicies []string
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

func (p *PlanPoliciesFactory) Build(ctx context.Context) (PlanPolicies, error) {
	if p.catalog == nil {
		return PlanPolicies{}, errors.AssertionFailedf("catalog is needed to format the policies enforced")
	}
	md := p.mem.Metadata()
	for _, tableMeta := range md.AllTables() {
		enforcement := PoliciesEnforced{
			name: tableMeta.Table.Name().Normalize(),
		}
		ids, rlsActive := md.GetPoliciesEnforced(tableMeta.MetaID)
		if !rlsActive {
			// Skip this table as row-level security is not active.
			continue
		}
		// For each policy ID, we need to find its name as that's what we report on
		// and figure out if it's permissive or restrictive, as the output groups
		// them by type.
		// SPILLY - first cut we will leave out restrictive policies
		// SPILLY - figure out mess of MetaID vs StableID.
		for _, id := range ids.Ordered() {
			// SPILLY - we may be able to skip ResolvePolicy and just get the Policy
			// information from the tableMeta.Table. It would require a new way to
			// potentially store the policy information for efficient lookup (yi.e
			// map)
			policy, err := p.catalog.ResolvePolicy(ctx, StableID.(table.MetaID), id)
			if err != nil {
				return PlanPolicies{}, err
			}
			// TODO(136742): Group them by type when we support restrictive policies.

		}

	}
	// SPILLY - guts
	return PlanPolicies{}, nil
}

// SPILLY - add function to add table info
//func (p *PlanPoliciesFactory) AddTable()
