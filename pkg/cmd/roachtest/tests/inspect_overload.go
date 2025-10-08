// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tests

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachprod/grafana"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	roachtestgrafana "github.com/cockroachdb/cockroach/pkg/cmd/roachtest/grafana"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/option"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/roachtestutil"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/spec"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/roachprod/prometheus"
)

func registerInspectOverload(r registry.Registry) {
	// 4 nodes × 8 CPUs, 250M rows, ~3 hours
	r.Add(makeInspectOverloadTest(r, 4, 8, 25_000_000, 3*time.Hour))
}

// makeInspectOverloadTest creates a test that sets up a CRDB cluster, loads it
// up with bulkingest data, and sets up a foreground read-only workload. It then
// runs INSPECT twice: once with the default low QoS priority and once with
// normal priority, to measure the impact on foreground latency.
//
// The test ensures sufficient ranges are created so that INSPECT work is
// well-distributed across the CPUs.
func makeInspectOverloadTest(
	r registry.Registry, numCRDBNodes, numCPUs, numRows int, timeout time.Duration,
) registry.TestSpec {
	// totalNodes includes CRDB nodes + 1 workload node
	totalNodes := numCRDBNodes + 1

	return registry.TestSpec{
		Name:             fmt.Sprintf("inspect/overload/bulkingest/rows=%d", numRows),
		Timeout:          timeout,
		Owner:            registry.OwnerSQLFoundations,
		Benchmark:        true,
		CompatibleClouds: registry.AllExceptAWS,
		Suites:           registry.Suites(registry.Weekly),
		Cluster:          r.MakeClusterSpec(totalNodes, spec.CPU(numCPUs), spec.WorkloadNode()),
		Leases:           registry.MetamorphicLeases,
		Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
			// Adjust for local testing
			rowsToImport := numRows
			targetRanges := numCRDBNodes * numCPUs * 2
			if c.IsLocal() {
				rowsToImport = 100_000
				targetRanges = 4
			}

			// Calculate bulkingest parameters to achieve target row count
			// We'll use b × c = 1000, so a = numRows / 1000
			bNum := 1000
			cNum := 1000
			aNum := rowsToImport / (bNum * cNum)

			// For local, use simpler values
			if c.IsLocal() {
				aNum = 100000
				bNum = 1
				cNum = 1
			}

			payloadBytes := 40

			c.Start(
				ctx, t.L(), option.NewStartOpts(option.NoBackupSchedule),
				install.MakeClusterSettings(), c.CRDBNodes(),
			)

			{
				promCfg := &prometheus.Config{}
				promCfg.WithPrometheusNode(c.WorkloadNode().InstallNodes()[0])
				promCfg.WithNodeExporter(c.All().InstallNodes())
				promCfg.WithCluster(c.CRDBNodes().InstallNodes())
				promCfg.WithGrafanaDashboardJSON(roachtestgrafana.SnapshotAdmissionControlGrafanaJSON)
				promCfg.ScrapeConfigs = append(promCfg.ScrapeConfigs, prometheus.MakeWorkloadScrapeConfig("workload",
					"/", makeWorkloadScrapeNodes(c.WorkloadNode().InstallNodes()[0], []workloadInstance{
						{nodes: c.WorkloadNode()},
					})))
				_, cleanupFunc := setupPrometheusForRoachtest(ctx, t, c, promCfg, []workloadInstance{{nodes: c.WorkloadNode()}})
				defer cleanupFunc()
			}

			baselineDuration, err := time.ParseDuration(roachtestutil.IfLocal(c, "30s", "10m"))
			if err != nil {
				t.Fatal(err)
			}
			inspectDuration, err := time.ParseDuration(roachtestutil.IfLocal(c, "30s", "1h"))
			if err != nil {
				t.Fatal(err)
			}
			workloadDuration := baselineDuration + 2*inspectDuration + baselineDuration

			db := c.Conn(ctx, t.L(), 1)
			defer db.Close()

			if !t.SkipInit() {
				t.Status("importing bulkingest dataset")

				// Disable automatic row count validation during import
				disableRowCountValidation(t, db)

				// Import bulkingest data with the default secondary index
				cmdImport := fmt.Sprintf(
					"./cockroach workload fixtures import bulkingest {pgurl:1} --a %d --b %d --c %d --payload-bytes %d",
					aNum, bNum, cNum, payloadBytes,
				)
				c.Run(ctx, option.WithNodes(c.WorkloadNode()), cmdImport)

				// Split the table into the target number of ranges
				t.Status(fmt.Sprintf("splitting table into ~%d ranges", targetRanges))
				splitSQL := fmt.Sprintf("ALTER TABLE bulkingest.bulkingest SPLIT AT SELECT (i * %d) // %d FROM generate_series(1, %d-1) AS i",
					aNum, targetRanges, targetRanges)
				if _, err := db.Exec(splitSQL); err != nil {
					t.Fatal(err)
				}

				// Scatter the ranges to ensure even distribution before INSPECT.
				t.Status("scattering ranges across cluster")
				if _, err := db.Exec("ALTER TABLE bulkingest.bulkingest SCATTER"); err != nil {
					t.Fatal(err)
				}

				// Initialize kv workload for foreground traffic
				t.Status("initializing kv workload")
				splits := roachtestutil.IfLocal(c, " --splits=3", " --splits=100")
				c.Run(ctx, option.WithNodes(c.WorkloadNode()), "./cockroach workload init kv"+splits+" {pgurl:1}")

				// Enable scrub jobs for EXPERIMENTAL SCRUB to use job system.
				// TODO(148365): Remove this when INSPECT SQL is used.
				if _, err := db.Exec("SET enable_scrub_job = on"); err != nil {
					t.Fatal(err)
				}
			}

			// Start read-only foreground workload using kv (since bulkingest doesn't support reads).
			t.Status(fmt.Sprintf("starting read-only kv workload for %v", workloadDuration))
			m := c.NewDeprecatedMonitor(ctx, c.CRDBNodes())
			m.Go(func(ctx context.Context) error {
				concurrency := roachtestutil.IfLocal(c, "8", "256")
				c.Run(ctx, option.WithNodes(c.WorkloadNode()),
					fmt.Sprintf("./cockroach workload run kv --read-percent=100 --duration=%s --concurrency=%s {pgurl%s}",
						workloadDuration, concurrency, c.CRDBNodes()),
				)
				return nil
			})

			t.Status(fmt.Sprintf("waiting %v for workload to run before starting INSPECT", baselineDuration))
			time.Sleep(baselineDuration)

			// Run first INSPECT with default (low) QoS.
			t.Status("running first INSPECT with default low QoS")
			startTime := time.Now()
			// TODO(148365): Update to use INSPECT syntax when SQL is ready.
			// The default index created by bulkingest is on (b, c, a)
			inspectSQL := "EXPERIMENTAL SCRUB TABLE bulkingest.bulkingest WITH OPTIONS INDEX (bulkingest_b_c_a_idx)"
			if _, err := db.ExecContext(ctx, inspectSQL); err != nil {
				t.Fatal(err)
			}
			firstInspectDuration := time.Since(startTime)
			t.L().Printf("First INSPECT (low QoS) completed in %v\n", firstInspectDuration)
			if err := c.AddInternalGrafanaAnnotation(ctx, t.L(), grafana.AddAnnotationRequest{
				Text:      "INSPECT (low QoS)",
				StartTime: startTime.UnixMilli(),
				EndTime:   time.Now().UnixMilli(),
			}); err != nil {
				t.L().Printf("failed to add grafana annotation: %v", err)
			}

			t.Status(fmt.Sprintf("waiting %v between INSPECTs", baselineDuration))
			time.Sleep(baselineDuration)

			// Disable admission control for second INSPECT. This should have more of an
			// impact to the foreground workload.
			t.Status("disabling admission control for INSPECT")
			if _, err := db.ExecContext(ctx, "SET CLUSTER SETTING sql.inspect.admission_control_enabled = false"); err != nil {
				t.Fatal(err)
			}

			// Run second INSPECT with normal QoS
			t.Status("running second INSPECT with normal QoS")
			startTime = time.Now()
			if _, err := db.ExecContext(ctx, inspectSQL); err != nil {
				t.Fatal(err)
			}
			secondInspectDuration := time.Since(startTime)
			t.L().Printf("Second INSPECT (normal QoS) completed in %v\n", secondInspectDuration)
			if err := c.AddInternalGrafanaAnnotation(ctx, t.L(), grafana.AddAnnotationRequest{
				Text:      "INSPECT (norm QoS)",
				StartTime: startTime.UnixMilli(),
				EndTime:   time.Now().UnixMilli(),
			}); err != nil {
				t.L().Printf("failed to add grafana annotation: %v", err)
			}

			// Wait for both workload to complete (or fail)
			m.Wait()
		},
	}
}
