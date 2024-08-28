// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package license_test

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/license"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/stretchr/testify/require"
)

type mockDiagnosticsReporter struct {
	lastPingTime time.Time
}

func (m mockDiagnosticsReporter) GetLastSuccessfulTelemetryPing() time.Time { return m.lastPingTime }

func TestGracePeriodInitTSCache(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	// This is the timestamp that we'll override the grace period init timestamp with.
	// This will be set when bringing up the server.
	ts1 := timeutil.Unix(1724329716, 0)

	ctx := context.Background()
	srv := serverutils.StartServerOnly(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Server: &server.TestingKnobs{
				LicenseTestingKnobs: license.TestingKnobs{
					OverrideStartTime: &ts1,
				},
			},
		},
	})
	defer srv.Stopper().Stop(ctx)

	// Create a new enforcer, to test that it won't overwrite the grace period init
	// timestamp that was already setup.
	enforcer := &license.Enforcer{}
	ts2 := ts1.Add(1)
	enforcer.TestingKnobs = &license.TestingKnobs{
		OverrideStartTime: &ts2,
	}
	// Ensure request for the grace period init ts1 before start just returns the start
	// time used when the enforcer was created.
	require.Equal(t, ts2, enforcer.GetGracePeriodInitTS())
	// Start the enforcer to read the timestamp from the KV.
	enforcer.SetDiagnosticsReader(&mockDiagnosticsReporter{lastPingTime: ts1})
	err := enforcer.Start(ctx, srv.ClusterSettings(), srv.SystemLayer().InternalDB().(descs.DB))
	require.NoError(t, err)
	require.Equal(t, ts1, enforcer.GetGracePeriodInitTS())

	// Access the enforcer that is cached in the executor config to make sure they
	// work for the system tenant and secondary tenant.
	require.Equal(t, ts1, srv.SystemLayer().ExecutorConfig().(sql.ExecutorConfig).LicenseEnforcer.GetGracePeriodInitTS())
	require.Equal(t, ts1, srv.ApplicationLayer().ExecutorConfig().(sql.ExecutorConfig).LicenseEnforcer.GetGracePeriodInitTS())
}

func TestThrottle(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	const UnderTxnThreshold = 3
	const OverTxnThreshold = 7

	// SPILLY - come up with shorter variable names
	t0 := time.Unix(1724884362, 0)
	t1d := t0.Add(24 * time.Hour)
	t10d := t0.Add(10 * 24 * time.Hour)
	t15d := t0.Add(15 * 24 * time.Hour)
	t30d := t0.Add(30 * 24 * time.Hour)
	t45d := t0.Add(40 * 24 * time.Hour)

	// SPILLY - do we need to override the grace period duration? We have full
	// control over the check time. Maybe remove that if not needed. Any other
	// overrides that we don't need? Max transactions.

	// SPILLY - finish with these unit tests

	for i, tc := range []struct {
		openTxnsCount         int64
		licType               license.LicType
		gracePeriodInit       time.Time
		lastTelemetryPingTime time.Time
		licExpiry             time.Time
		checkTs               time.Time
		expectedErrRegex      string
	}{
		// Expired free license but under the transaction threshold
		{UnderTxnThreshold, license.LicTypeFree, t0, t1d, t30d, t45d, ""},
		// Expired trial license but under the transaction threshold
		{UnderTxnThreshold, license.LicTypeTrial, t0, t30d, t30d, t45d, ""},
		// Over the transaction threshold but not expired
		{OverTxnThreshold, license.LicTypeFree, t0, t10d, t45d, t10d, ""},
		// Expired free license, past the grace period
		{OverTxnThreshold, license.LicTypeFree, t0, t30d, t10d, t30d, "License expired"},
		// Expired free license, but not past the grace period
		{OverTxnThreshold, license.LicTypeFree, t0, t30d, t10d, t15d, ""},
		// Valid free license, but telemetry ping hasn't been received in 5 days.
		{OverTxnThreshold, license.LicTypeFree, t0, t10d, t45d, t15d, ""},
		// Valid free license, but telemetry ping hasn't been received in 20 days.
		{OverTxnThreshold, license.LicTypeFree, t0, t10d, t45d, t30d, "diagnostic reporting"},
	} {
		t.Run(fmt.Sprintf("test %d", i), func(t *testing.T) {
			e := license.Enforcer{
				TestingKnobs: &license.TestingKnobs{
					OverrideStartTime:         &tc.gracePeriodInit,
					OverrideThrottleCheckTime: &tc.checkTs,
				},
			}
			e.SetDiagnosticsReader(&mockDiagnosticsReporter{
				lastPingTime: tc.lastTelemetryPingTime,
			})
			e.RefreshForLicenseChange(tc.licType, tc.licExpiry)
			err := e.MaybeFailIfThrottled(ctx, tc.openTxnsCount)
			if tc.expectedErrRegex == "" {
				require.NoError(t, err)
			} else {
				re := regexp.MustCompile(tc.expectedErrRegex)
				match := re.MatchString(err.Error())
				require.NotNil(t, match, "Error text %q doesn't match the expected regexp of %q",
					err.Error(), tc.expectedErrRegex)
			}
		})
	}
}
