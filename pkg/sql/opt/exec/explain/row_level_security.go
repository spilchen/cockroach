// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package explain

import (
	"fmt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/treeprinter"
	"strings"
)

// PlanPolicies keeps track of the policies that are enforced for each relation
// in the query.
type PlanPolicies struct {
	enforced []PoliciesEnforced
}

// BuildStringRows generates output of []string of RLS information to include in
// explain output.
func (p *PlanPolicies) BuildStringRows() []string {
	if len(p.enforced) == 0 {
		return nil
	}

	tp := treeprinter.NewWithStyle(treeprinter.BulletStyle)
	root := tp.Child("row-level security policies")
	for i := range p.enforced {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("table: %s\n", p.enforced[i].name))
		sb.WriteString(fmt.Sprintf("permissive: [%s]", flattenNames(p.enforced[i].permissivePolicies)))
		// TODO(136742): Print out restrictive policies when we support those.
		root.Child(sb.String())
	}

	fmtRows := tp.FormattedRows()
	rows := make([]string, 1, 1+len(fmtRows)) // prefix with 1 empty row
	rows = append(rows, fmtRows...)
	return rows
}

// flattenNames is a helper that takes a slice of Names and returns them as a
// single string, concatenated and separated by commas.
func flattenNames(names []tree.Name) string {
	var sb strings.Builder
	for _, name := range names {
		if sb.Len() > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(name.Normalize())
	}
	return sb.String()
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
	md *opt.Metadata
}

func (p *PlanPoliciesFactory) Init(
	md *opt.Metadata,
) {
	p.md = md
}

// Build will generate and return a PlanPolicies struct based on the rls
// policies used in the query.
func (p *PlanPoliciesFactory) Build() (*PlanPolicies, error) {
	// Early out if the query didn't see any tables with row-level security enabled.
	if !p.md.IsRLSEnabled() {
		return nil, nil
	}
	var planPolicies PlanPolicies
	for _, tableMeta := range p.md.AllTables() {
		ids, rlsActive := p.md.GetPoliciesEnforced(tableMeta.MetaID)
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
	return &planPolicies, nil
}
