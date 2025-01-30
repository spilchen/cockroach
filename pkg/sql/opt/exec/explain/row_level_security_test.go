// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package explain_test

import (
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/sql/opt/exec/explain"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/testutils/opttester"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/testutils/testcat"
	"github.com/cockroachdb/cockroach/pkg/testutils/datapathutils"
	"github.com/cockroachdb/datadriven"
	"github.com/stretchr/testify/require"
)

func makePlanPolicies(ot *opttester.OptTester, t *testing.T) *explain.PlanPolicies {
	expr, err := ot.Optimize()
	if err != nil {
		t.Error(err)
	}
	rel, ok := expr.(memo.RelExpr)
	if !ok {
		t.Fatalf("%v could not be coerced into a memo.RelExpr", rel)
	}
	md := rel.Memo().Metadata()
	f := explain.NewPlanPoliciesFactory(md)
	planPolicies, err := f.Build()
	require.NoError(t, err)
	return planPolicies
}

func TestPlanRowLevelSecurityBuilder(t *testing.T) {
	catalog := testcat.New()
	testRLS := func(t *testing.T, d *datadriven.TestData) string {
		ot := opttester.New(catalog, d.Input)

		for _, a := range d.CmdArgs {
			if err := ot.Flags.Set(a); err != nil {
				d.Fatalf(t, "%+v", err)
			}
		}
		switch d.Cmd {
		case "explain-plan-rls":
			pp := makePlanPolicies(ot, t)
			if pp == nil {
				return ""
			}
			return strings.Join(pp.BuildStringRows(), "\n")
		default:
			return ot.RunCommand(t, d)
		}
	}
	datadriven.RunTest(t, datapathutils.TestDataPath(t, "row_level_security"), testRLS)
}
