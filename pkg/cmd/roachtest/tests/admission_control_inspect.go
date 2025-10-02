// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tests

import (
	"context"
	"database/sql"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
)

// admissionControlInspectOperation performs concurrent EXPERIMENTAL SCRUB operations
// on multiple TPC-E tables to test admission control during table inspection.
func admissionControlInspectOperation(
	ctx context.Context, t test.Test, c cluster.Cluster, db *sql.DB,
) error {
	// Enable the scrub job to allow EXPERIMENTAL SCRUB to run asynchronously.
	if _, err := db.ExecContext(ctx,
		"SET CLUSTER SETTING sql.scrub.enable_scrub_job = true;",
	); err != nil {
		return err
	}

	// TODO(148365): Replace EXPERIMENTAL SCRUB with INSPECT once available.
	//
	// Run EXPERIMENTAL SCRUB on multiple large TPC-E tables concurrently.
	// These tables are chosen because they contain significant data volumes
	// and will generate substantial admission control load.
	m := c.NewDeprecatedMonitor(ctx, c.CRDBNodes())

	m.Go(func(ctx context.Context) error {
		t.Status("starting scrub on trade table")
		_, err := db.ExecContext(ctx,
			"EXPERIMENTAL SCRUB TABLE tpce.trade WITH OPTIONS INDEX ALL;",
		)
		t.Status("finished scrub on trade table")
		return err
	})

	m.Go(func(ctx context.Context) error {
		time.Sleep(5 * time.Minute)
		t.Status("starting scrub on trade_history table")
		_, err := db.ExecContext(ctx,
			"EXPERIMENTAL SCRUB TABLE tpce.trade_history WITH OPTIONS INDEX ALL;",
		)
		t.Status("finished scrub on trade_history table")
		return err
	})

	m.Go(func(ctx context.Context) error {
		time.Sleep(10 * time.Minute)
		t.Status("starting scrub on cash_transaction table")
		_, err := db.ExecContext(ctx,
			"EXPERIMENTAL SCRUB TABLE tpce.cash_transaction WITH OPTIONS INDEX ALL;",
		)
		t.Status("finished scrub on cash_transaction table")
		return err
	})

	m.Go(func(ctx context.Context) error {
		time.Sleep(15 * time.Minute)
		t.Status("starting scrub on holding_history table")
		_, err := db.ExecContext(ctx,
			"EXPERIMENTAL SCRUB TABLE tpce.holding_history WITH OPTIONS INDEX ALL;",
		)
		t.Status("finished scrub on holding_history table")
		return err
	})

	m.Wait()
	return nil
}

// registerInspectOverload registers the INSPECT admission control test.
func registerInspectOverload(r registry.Registry) {
	registerAdmissionControlIndexTest(
		r,
		"admission-control/inspect",
		registry.OwnerSQLFoundations,
		6*time.Hour,
		admissionControlInspectOperation,
	)
}
