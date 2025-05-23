// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sctestbackupccl

import (
	"os"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/ccl"
	"github.com/cockroachdb/cockroach/pkg/ccl/schemachangerccl"
	"github.com/cockroachdb/cockroach/pkg/security/securityassets"
	"github.com/cockroachdb/cockroach/pkg/security/securitytest"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/sctest"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
)

func TestMain(m *testing.M) {
	defer ccl.TestingEnableEnterprise()()
	securityassets.SetLoader(securitytest.EmbeddedAssets)
	randutil.SeedForTests()
	serverutils.InitTestClusterFactory(testcluster.TestClusterFactory)
	serverutils.InitTestServerFactory(server.TestServerFactory)
	os.Exit(m.Run())
}

// MultiRegionTestClusterFactory is an alias for the type in the
// schemachangerccl package, exposed here so that tests in this package can
// easily use it.
type MultiRegionTestClusterFactory = schemachangerccl.MultiRegionTestClusterFactory

var _ sctest.TestServerFactory = MultiRegionTestClusterFactory{}

//go:generate ../../../util/leaktest/add-leaktest.sh *_test.go
