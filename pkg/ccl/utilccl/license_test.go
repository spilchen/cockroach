// Copyright 2017 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package utilccl

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl/licenseccl"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/server/license"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/stretchr/testify/require"
)

func TestLicense(t *testing.T) {
	t0 := timeutil.Unix(0, 0)
	ts := t0.AddDate(40, 0, 0)
	after := ts.Add(time.Hour * 24)
	before := ts.Add(time.Hour * -24)
	wayAfter := ts.Add(time.Hour * 24 * 365 * 200)

	// Generate random, yet deterministic, values for the two byte fields.
	// The first byte of each will be incremented after each test to ensure variation.
	orgID := []byte{0}
	licenseID := []byte{0}

	for i, tc := range []struct {
		licType    licenseccl.License_Type
		expiration time.Time
		checkOrg   string
		checkTime  time.Time
		err        string
	}{
		{licType: -1, err: "requires an enterprise license"},
		{licenseccl.License_Evaluation, ts, "tc-1", ts, ""},
		{licenseccl.License_Enterprise, ts, "tc-2", ts, ""},
		{licenseccl.License_NonCommercial, ts, "tc-3", ts, ""},
		{licenseccl.License_Evaluation, after, "tc-4", ts, ""},
		{licenseccl.License_Evaluation, ts, "tc-5", before, ""},
		{licenseccl.License_Evaluation, wayAfter, "tc-6", ts, ""},

		// expirations.
		{licenseccl.License_Evaluation, ts, "tc-7", after, "expired"},
		{licenseccl.License_Evaluation, after, "tc-8", wayAfter, "expired"},
		{licenseccl.License_NonCommercial, after, "tc-9", wayAfter, "expired"},
		{licenseccl.License_NonCommercial, t0, "tc-10", wayAfter, ""},
		{licenseccl.License_Evaluation, t0, "tc-11", wayAfter, ""},

		// grace period.
		{licenseccl.License_Enterprise, after, "tc-12", wayAfter, ""},

		// mismatch.
		{licenseccl.License_Enterprise, ts, "tc-13", ts, ""},
		{licenseccl.License_Enterprise, ts, "", ts, "license valid only for"},
		{licenseccl.License_Enterprise, ts, "tc-15", ts, ""},

		// free license.
		{licenseccl.License_Free, ts, "tc-16", ts, ""},
		{licenseccl.License_Free, after, "tc-17", wayAfter, "expired"},

		// trial license.
		{licenseccl.License_Trial, wayAfter, "tc-18", before, ""},
		{licenseccl.License_Trial, ts, "unmatched org", ts, "license valid only for"},
	} {
		t.Run("", func(t *testing.T) {
			var lic *licenseccl.License
			if tc.licType != -1 {
				s, err := (&licenseccl.License{
					ValidUntilUnixSec: tc.expiration.Unix(),
					Type:              tc.licType,
					OrganizationName:  fmt.Sprintf("tc-%d", i),
					OrganizationId:    orgID,
					LicenseId:         licenseID,
				}).Encode()
				if err != nil {
					t.Fatal(err)
				}

				lic, err = decode(s)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := check(
				lic, tc.checkTime, tc.checkOrg, "", true,
			); !testutils.IsError(err, tc.err) {
				t.Fatalf("%d: lic to %s, checked at %s.\n got %q", i,
					tc.expiration, tc.checkTime, err)
			}
			orgID[0]++
			licenseID[0]++
		})
	}
}

func TestBadLicenseStrings(t *testing.T) {
	for _, tc := range []struct{ lic, err string }{
		{"blah", "invalid license string"},
		{"crl-0-&&&&&", "invalid license string"},
		{"crl-0-blah", "invalid license string"},
	} {
		if _, err := decode(tc.lic); !testutils.IsError(err, tc.err) {
			t.Fatalf("%q: expected err %q, got %v", tc.lic, tc.err, err)
		}
	}
}

func TestExpiredLicenseLanguage(t *testing.T) {
	lic := &licenseccl.License{
		Type:              licenseccl.License_Evaluation,
		ValidUntilUnixSec: 1,
	}
	err := check(lic, timeutil.Now(), "", "RESTORE", true)
	expected := "Use of RESTORE requires an enterprise license. Your evaluation license expired on " +
		"January 1, 1970. If you're interested in getting a new license, please contact " +
		"subscriptions@cockroachlabs.com and we can help you out."
	if err == nil || err.Error() != expected {
		t.Fatalf("expected err:\n%s\ngot:\n%v", expected, err)
	}
}

func TestRefreshLicenseEnforcerOnLicenseChange(t *testing.T) {
	ts1 := timeutil.Unix(1724329716, 0)

	ctx := context.Background()
	srv := serverutils.StartServerOnly(t, base.TestServerArgs{
		// We are changing a cluster setting that can only be done at the system tenant.
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
		Knobs: base.TestingKnobs{
			Server: &server.TestingKnobs{
				LicenseTestingKnobs: license.TestingKnobs{
					OverrideStartTime: &ts1,
				},
			},
		},
	})
	defer srv.Stopper().Stop(ctx)

	// All of the licenses that we install later depend on this org name.
	_, err := srv.SystemLayer().SQLConn(t, serverutils.User(username.RootUser)).Exec(
		"SET CLUSTER SETTING cluster.organization = 'CRDB Unit Test'",
	)
	require.NoError(t, err)

	// Test to ensure that the state is correctly registered on startup before
	// changing the license.
	enforcer := license.GetEnforcerInstance()
	require.Equal(t, false, enforcer.GetHasLicense())
	gracePeriodTS, hasGracePeriod := enforcer.GetGracePeriodEnd()
	require.True(t, hasGracePeriod)
	require.Equal(t, ts1.Add(7*24*time.Hour), gracePeriodTS)

	jan1st2000 := timeutil.Unix(946728000, 0)

	for i, tc := range []struct {
		license                string
		expectedGracePeriodEnd time.Time
	}{
		// Note: all licenses below expire on Jan 1st 2000
		//
		// Free license - 30 days grace period
		{"crl-0-EMDYt8MDGAMiDkNSREIgVW5pdCBUZXN0", jan1st2000.Add(30 * 24 * time.Hour)},
		// Trial license - 7 days grace period
		{"crl-0-EMDYt8MDGAQiDkNSREIgVW5pdCBUZXN0", jan1st2000.Add(7 * 24 * time.Hour)},
		// Enterprise - no grace period
		{"crl-0-EMDYt8MDGAEiDkNSREIgVW5pdCBUZXN0KAM", timeutil.UnixEpoch},
		// No license - 7 days grace period
		{"", ts1.Add(7 * 24 * time.Hour)},
	} {
		t.Run(fmt.Sprintf("test %d", i), func(t *testing.T) {
			_, err := srv.SQLConn(t).Exec(
				fmt.Sprintf("SET CLUSTER SETTING enterprise.license = '%s'", tc.license),
			)
			require.NoError(t, err)
			// The SQL can return back before the callback has finished. So, we wait a
			// bit to see if the desired state is reached.
			var hasLicense bool
			require.Eventually(t, func() bool {
				hasLicense = enforcer.GetHasLicense()
				return (tc.license != "") == hasLicense
			}, 20*time.Second, time.Millisecond,
				"GetHasLicense() last returned %t", hasLicense)
			var ts time.Time
			var hasGracePeriod bool
			require.Eventually(t, func() bool {
				ts, hasGracePeriod = enforcer.GetGracePeriodEnd()
				if tc.expectedGracePeriodEnd.Equal(timeutil.UnixEpoch) {
					return !hasGracePeriod
				}
				return ts.Equal(tc.expectedGracePeriodEnd)
			}, 20*time.Second, time.Millisecond,
				"GetGracePeriodEnd() last returned %v (%t)", ts, hasGracePeriod)
		})
	}
}
