// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Code generated by generate-logictest, DO NOT EDIT.

package testmultiregion_9node_3region_3azs_vec_off

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/build/bazel"
	"github.com/cockroachdb/cockroach/pkg/ccl"
	"github.com/cockroachdb/cockroach/pkg/security/securityassets"
	"github.com/cockroachdb/cockroach/pkg/security/securitytest"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/sql/logictest"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
)

const configIdx = 16

var cclLogicTestDir string

func init() {
	if bazel.BuiltWithBazel() {
		var err error
		cclLogicTestDir, err = bazel.Runfile("pkg/ccl/logictestccl/testdata/logic_test")
		if err != nil {
			panic(err)
		}
	} else {
		cclLogicTestDir = "../../../../ccl/logictestccl/testdata/logic_test"
	}
}

func TestMain(m *testing.M) {
	defer ccl.TestingEnableEnterprise()()
	securityassets.SetLoader(securitytest.EmbeddedAssets)
	randutil.SeedForTests()
	serverutils.InitTestServerFactory(server.TestServerFactory)
	serverutils.InitTestClusterFactory(testcluster.TestClusterFactory)

	defer serverutils.TestingSetDefaultTenantSelectionOverride(
		base.TestIsForStuffThatShouldWorkWithSecondaryTenantsButDoesntYet(76378),
	)()

	os.Exit(m.Run())
}

func runCCLLogicTest(t *testing.T, file string) {
	skip.UnderRace(t, "times out and/or hangs")
	skip.UnderDeadlock(t, "times out and/or hangs")
	logictest.RunLogicTest(t, logictest.TestServerArgs{}, configIdx, filepath.Join(cclLogicTestDir, file))
}

// TestLogic_tmp runs any tests that are prefixed with "_", in which a dedicated
// test is not generated for. This allows developers to create and run temporary
// test files that are not checked into the repository, without repeatedly
// regenerating and reverting changes to this file, generated_test.go.
//
// TODO(mgartner): Add file filtering so that individual files can be run,
// instead of all files with the "_" prefix.
func TestLogic_tmp(t *testing.T) {
	defer leaktest.AfterTest(t)()
	var glob string
	glob = filepath.Join(cclLogicTestDir, "_*")
	logictest.RunLogicTests(t, logictest.TestServerArgs{}, configIdx, glob)
}

func TestCCLLogic_global_placement_restricted(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "global_placement_restricted")
}

func TestCCLLogic_multi_region_import_export(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "multi_region_import_export")
}

func TestCCLLogic_multi_region_remote_access_error(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "multi_region_remote_access_error")
}

func TestCCLLogic_placement(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "placement")
}

func TestCCLLogic_regional_by_row(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "regional_by_row")
}

func TestCCLLogic_regional_by_row_foreign_key(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "regional_by_row_foreign_key")
}

func TestCCLLogic_regional_by_row_placement_restricted(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "regional_by_row_placement_restricted")
}

func TestCCLLogic_regional_by_row_read_committed(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "regional_by_row_read_committed")
}

func TestCCLLogic_regional_by_row_rename_column(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "regional_by_row_rename_column")
}

func TestCCLLogic_regional_by_table_placement_restricted(
	t *testing.T,
) {
	defer leaktest.AfterTest(t)()
	runCCLLogicTest(t, "regional_by_table_placement_restricted")
}
