// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tests

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/option"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/roachprod/config"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
)

var hibernateReleaseTagRegex = regexp.MustCompile(`^(?P<major>\d+)\.(?P<minor>\d+)\.(?P<point>\d+)$`)

// WARNING: DO NOT MODIFY the name of the below constant/variable without approval from the docs team.
// This is used by docs automation to produce a list of supported versions for ORM's.
var supportedHibernateTag = "6.6.20"

type hibernateOptions struct {
	testName string
	testDir  string
	buildCmd,
	testCmd string
	listWithName listWithName
	dbSetupFunc  func(ctx context.Context, t test.Test, c cluster.Cluster)
}

var (
	hibernateOpts = hibernateOptions{
		testName: "hibernate",
		testDir:  "hibernate-core",
		buildCmd: `cd /mnt/data1/hibernate/hibernate-core/ && ./../gradlew test -Pdb=cockroachdb ` +
			`--tests org.hibernate.orm.test.jdbc.util.BasicFormatterTest.*`,
		testCmd: `cd /mnt/data1/hibernate/hibernate-core/ && ./../gradlew test -Pdb=cockroachdb` +
			` --tests org.hibernate.orm.test.sql.SQLTest.test_sql_hibernate_dto_query_example`,
		listWithName: listWithName{
			blocklistName:  "hibernateBlockList",
			blocklist:      hibernateBlockList,
			ignorelistName: "hibernateIgnoreList",
			ignorelist:     hibernateIgnoreList,
		},
		dbSetupFunc: nil,
	}
	hibernateSpatialOpts = hibernateOptions{
		testName: "hibernate-spatial",
		testDir:  "hibernate-spatial",
		buildCmd: `cd /mnt/data1/hibernate/hibernate-spatial/ && ./../gradlew test -Pdb=cockroachdb ` +
			`--tests org.hibernate.spatial.dialect.postgis.*`,
		testCmd: `cd /mnt/data1/hibernate/hibernate-spatial && ` +
			`HIBERNATE_CONNECTION_LEAK_DETECTION=true ./../gradlew test -Pdb=cockroachdb`,
		listWithName: listWithName{
			blocklistName:  "hibernateSpatialBlockList",
			blocklist:      hibernateSpatialBlockList,
			ignorelistName: "hibernateSpatialIgnoreList",
			ignorelist:     hibernateSpatialIgnoreList,
		},
		dbSetupFunc: func(ctx context.Context, t test.Test, c cluster.Cluster) {
			db := c.Conn(ctx, t.L(), 1)
			defer db.Close()
			if _, err := db.ExecContext(
				ctx,
				"SET CLUSTER SETTING sql.spatial.experimental_box2d_comparison_operators.enabled = on",
			); err != nil {
				t.Fatal(err)
			}
		},
	}
)

// This test runs one of hibernate's test suite against a single cockroach
// node.

func registerHibernate(r registry.Registry, opt hibernateOptions) {
	runHibernate := func(
		ctx context.Context,
		t test.Test,
		c cluster.Cluster,
	) {
		if c.IsLocal() {
			t.Fatal("cannot be run in local mode")
		}
		node := c.Node(1)
		t.Status("setting up cockroach")
		startOpts := option.NewStartOpts(sqlClientsInMemoryDB)
		startOpts.RoachprodOpts.SQLPort = config.DefaultSQLPort
		// Hibernate uses a hardcoded connection string with ssl disabled.
		c.Start(ctx, t.L(), startOpts, install.MakeClusterSettings(install.SimpleSecureOption(false)), c.All())

		if opt.dbSetupFunc != nil {
			opt.dbSetupFunc(ctx, t, c)
		}

		version, err := fetchCockroachVersion(ctx, t.L(), c, node[0])
		if err != nil {
			t.Fatal(err)
		}

		if err := alterZoneConfigAndClusterSettings(ctx, t, version, c, node[0]); err != nil {
			t.Fatal(err)
		}

		t.Status("cloning hibernate and installing prerequisites")
		latestTag, err := repeatGetLatestTag(
			ctx, t, "hibernate", "hibernate-orm", hibernateReleaseTagRegex,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.L().Printf("Latest Hibernate release is %s.", latestTag)
		t.L().Printf("Supported Hibernate release is %s.", supportedHibernateTag)

		if err := repeatRunE(
			ctx, t, c, node, "update apt-get", `sudo apt-get -qq update`,
		); err != nil {
			t.Fatal(err)
		}

		if err := repeatRunE(
			ctx,
			t,
			c,
			node,
			"install dependencies",
			`sudo apt-get -qq install openjdk-11-jre-headless openjdk-11-jdk-headless`,
		); err != nil {
			t.Fatal(err)
		}

		if err := repeatRunE(
			ctx, t, c, node, "remove old Hibernate", `rm -rf /mnt/data1/hibernate`,
		); err != nil {
			t.Fatal(err)
		}

		if err := repeatGitCloneE(
			ctx,
			t,
			c,
			"https://github.com/hibernate/hibernate-orm.git",
			"/mnt/data1/hibernate",
			supportedHibernateTag,
			node,
		); err != nil {
			t.Fatal(err)
		}

		t.Status("building hibernate (without tests)")
		// Build hibernate and run a single test, this step involves some
		// downloading, so it needs a retry loop as well. Just building was not
		// enough as the test libraries are not downloaded unless at least a
		// single test is invoked.
		if err := repeatRunE(
			ctx,
			t,
			c,
			node,
			"building hibernate (without tests)",
			opt.buildCmd,
		); err != nil {
			t.Fatal(err)
		}

		// Delete the test result; the test will be executed again later.
		if err := repeatRunE(
			ctx,
			t,
			c,
			node,
			"delete test result from build output",
			fmt.Sprintf(`rm -rf /mnt/data1/hibernate/%s/target/test-results/test`, opt.testDir),
		); err != nil {
			t.Fatal(err)
		}

		t.L().Printf("Running cockroach version %s", version)

		t.Status("running hibernate test (filtered to single test)")
		// TODO(SPILLY): Running only the failing test for faster reproduction.
		// Skip result-comparison logic since we're not running the full suite.
		if err := c.RunE(ctx, option.WithNodes(node), opt.testCmd); err != nil {
			t.L().Printf("test command failed (expected): %v", err)
		} else {
			t.L().Printf("test command succeeded")
		}
	}

	r.Add(registry.TestSpec{
		Name:             opt.testName,
		Owner:            registry.OwnerSQLFoundations,
		Cluster:          r.MakeClusterSpec(1),
		Leases:           registry.MetamorphicLeases,
		NativeLibs:       registry.LibGEOS,
		CompatibleClouds: registry.AllExceptAWS,
		Suites:           registry.Suites(registry.Nightly, registry.ORM),
		Timeout:          6 * time.Hour,
		Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
			runHibernate(ctx, t, c)
		},
	})
}
