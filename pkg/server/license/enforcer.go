// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package license

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/utilccl/licenseccl"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

// Enforcer is responsible for enforcing license policies.
type Enforcer struct {
	// DB is used to access into the KV. This is set via InitDB. It must have
	// access to the system tenant since it read/writes KV keys in the system
	// keyspace.
	db descs.DB

	// TestingKnobs are used to control the behavior of the enforcer for testing.
	TestingKnobs *TestingKnobs

	// diagnosticReader is an interface for getting the timestamp of the last
	// successful ping to the telemetry server. For some licenses, sending
	// telemetry data is required to avoid throttling.
	diagnosticsReader DiagnosticsReader

	// gracePeriodInitTS is the timestamp of when the cluster first ran on a
	// version that requires a license. It is stored as the number of seconds
	// since the unix epoch. This is read/written to the KV.
	gracePeriodInitTS atomic.Int64

	// startTime is the time when the enforcer was created. This is used to seed
	// the grace period init timestamp if it's not set in the KV.
	startTime time.Time

	// licenseRequiresTelemetry will be true if the license requires that we send
	// periodic telemetry data.
	licenseRequiresTelemetry atomic.Bool

	// gracePeriodEndTS tracks when the grace period ends and throttling begins.
	// For licenses without throttling, this value will be 0.
	gracePeriodEndTS atomic.Int64

	// hasLicense is true if any license is installed.
	hasLicense atomic.Bool

	// lastLicenseThrottlingLogTime keeps track of the last time we logged a
	// message because we had to throttle due to a license issue.
	lastLicenseThrottlingLogTime atomic.Int64

	// lastTelemetryThrottlingLogTime keeps track of the last time we logged a
	// message because we had to throttle due to a telemetry issue.
	lastTelemetryThrottlingLogTime atomic.Int64
}

type TestingKnobs struct {
	// OverrideStartTime if set, overrides the time that's used to seed the
	// grace period init timestamp.
	OverrideStartTime *time.Time
}

// DiagnosticsReader is the interface to diagnostics reporter.
type DiagnosticsReader interface {
	// GetLastSuccessfulTelemetryPing returns the time of the last time the
	// diagnostics reporter got back an acknowledgement.
	GetLastSuccessfulTelemetryPing() time.Time
}

var instance *Enforcer
var once sync.Once

// GetEnforcerInstance returns the singleton instance of the Enforcer. The
// Enforcer is responsible for license enforcement policies.
func GetEnforcerInstance() *Enforcer {
	if instance == nil {
		once.Do(
			func() {
				instance = newEnforcer()
			})
	}
	return instance
}

// newEnforcer creates a new Enforcer object.
func newEnforcer() *Enforcer {
	return &Enforcer{
		startTime: timeutil.Now(),
	}
}

// Start will load the necessary metadata for the enforcer. It reads from the
// KV license metadata and will populate any missing data as needed. The DB
// passed in must have access to the system tenant.
func (e *Enforcer) Start(
	ctx context.Context, db descs.DB, diagnosticsReader DiagnosticsReader,
) error {
	e.db = db
	e.diagnosticsReader = diagnosticsReader

	return e.db.Txn(ctx, func(ctx context.Context, txn isql.Txn) error {
		// We could use a conditional put for this logic. However, we want to read
		// and cache the value, and the common case is that the value will be read.
		// Only during the initialization of the first node in the cluster will we
		// need to write a new timestamp. So, we optimize for the case where the
		// timestamp already exists.
		val, err := txn.KV().Get(ctx, keys.GracePeriodInitTimestamp)
		if err != nil {
			return err
		}
		if val.Value == nil {
			log.Infof(ctx, "generated new grace period init time: %s", e.startTime.UTC().String())
			e.gracePeriodInitTS.Store(e.getStartTime().Unix())
			return txn.KV().Put(ctx, keys.GracePeriodInitTimestamp, e.gracePeriodInitTS.Load())
		}
		e.gracePeriodInitTS.Store(val.ValueInt())
		log.Infof(ctx, "fetched existing grace period init time: %s", e.GetGracePeriodInitTS().String())
		return nil
	})
}

// GetGracePeriodInitTS will return the timestamp of when the cluster first ran
// on a version that requires a license.
func (e *Enforcer) GetGracePeriodInitTS() time.Time {
	// In the rare case that the grace period init timestamp has not been cached yet,
	// we will return an approximate value, the start time of the server. This
	// should only happen if we are in the process of caching the grace period init
	// timestamp, or we failed to cache it. This is preferable to returning an
	// error or a zero value.
	if e.gracePeriodInitTS.Load() == 0 {
		return e.getStartTime()
	}
	return timeutil.Unix(e.gracePeriodInitTS.Load(), 0)
}

// MaybeFailIfThrottled checks the current state and returns an error if throttling is active.
func (e *Enforcer) MaybeFailIfThrottled(ctx context.Context, txnsOpened int64) (err error) {
	// Early out if the number of transactions is below the max allowed.
	if txnsOpened < e.getMaxOpenTransactions() {
		return
	}

	// SPILLY - add testing knob to control this timestamp. This can be used in unit tests.
	now := timeutil.Now()
	gracePeriodEnd := e.calculateGracePeriodEnd()
	if gracePeriodEnd != nil && now.After(*gracePeriodEnd) {
		if e.hasLicense.Load() {
			err = errors.WithHintf(pgerror.Newf(pgcode.CCLValidLicenseRequired,
				"License expired. The maximum number of open transactions has been reached."),
				"Obtain and install a new license to continue.")
		} else {
			err = errors.WithHintf(pgerror.Newf(pgcode.CCLValidLicenseRequired,
				"No license installed. The maximum number of open transactions has been reached."),
				"Obtain and install a valid license to continue.")
		}
		e.maybeLogError(ctx, err, &e.lastLicenseThrottlingLogTime)
		return
	}

	if ts := e.getTimestampWhenTelemetryDataIsNeeded(); ts != nil && now.After(*ts) {
		err = errors.WithHintf(pgerror.Newf(pgcode.CCLValidLicenseRequired,
			"The maximum number of open transactions has been reached because the license requires "+
				"diagnostic reporting, but none has been received by Cockroach Labs."),
			"Ensure diagnostic reporting is enabled and verify that nothing is blocking network access to the "+
				"Cockroach Labs reporting server. You can also consider changing your license to one that doesn't "+
				"require diagnostic reporting to be emitted.")
		e.maybeLogError(ctx, err, &e.lastTelemetryThrottlingLogTime)
		return
	}
	return
}

// getStartTime returns the time when the enforcer was created. This accounts
// for testing knobs that may override the time.
func (e *Enforcer) getStartTime() time.Time {
	if e.TestingKnobs != nil && e.TestingKnobs.OverrideStartTime != nil {
		return *e.TestingKnobs.OverrideStartTime
	}
	return e.startTime
}

// refreshForLicense resets the state when the license changes. We cache certain
// information to optimize enforcement. Instead of reading the license from the
// settings, unmarshaling it, and checking its type and expiry each time,
// caching the information improves efficiency since licenses change infrequently.
func (e *Enforcer) refreshForLicense(license *licenseccl.License) {
	e.hasLicense.Store(license != nil)

	// SPILLY - call this function and add a callback for when the license changes
	// No license is used.
	if license == nil {
		e.storeNewGracePeriodEndDate(e.GetGracePeriodInitTS(), 7*24*time.Hour)
		e.licenseRequiresTelemetry.Store(false)
		return
	}

	switch license.Type {
	case licenseccl.License_Trial:
		e.storeNewGracePeriodEndDate(timeutil.Unix(license.ValidUntilUnixSec, 0), 7*24*time.Hour)
		e.licenseRequiresTelemetry.Store(true)
	case licenseccl.License_Free:
		e.storeNewGracePeriodEndDate(timeutil.Unix(license.ValidUntilUnixSec, 0), 30*24*time.Hour)
		e.licenseRequiresTelemetry.Store(true)
	default:
		e.storeNewGracePeriodEndDate(timeutil.UnixEpoch, 0)
		e.licenseRequiresTelemetry.Store(false)
	}
}

func (e *Enforcer) storeNewGracePeriodEndDate(start time.Time, duration time.Duration) {
	ts := start.Add(duration)
	e.gracePeriodEndTS.Store(ts.UnixMicro()) // SPILLY - is correct?
}

// getMaxOpenTransactions returns the number of open transactions allowed before
// throttling takes affect.
func (e *Enforcer) getMaxOpenTransactions() int64 {
	// SPILLY - can add check for env var.
	return 5
}

// calculateGracePeriodEnd returns the timestamp marking the end of the grace period.
// Some licenses allow for a grace period after the license expires or if it doesn't exist.
// If nil is returned, it means there is no grace period for the license type.
func (e *Enforcer) calculateGracePeriodEnd() *time.Time {
	if e.gracePeriodEndTS.Load() == 0 {
		return nil
	}
	// SPILLY - it probably should be unix
	ts := timeutil.FromUnixMicros(e.gracePeriodEndTS.Load())
	return &ts
}

// getTimestampWhenTelemetryDataIsNeeded returns a timestamp of when telemetry
// data needs to be received before we start to throttle. If a nil pointer is
// returned, then it means the license does not require telemetry data.
func (e *Enforcer) getTimestampWhenTelemetryDataIsNeeded() *time.Time {
	if !e.licenseRequiresTelemetry.Load() {
		return nil
	}

	lastTelemetryDataReceived := e.diagnosticsReader.GetLastSuccessfulTelemetryPing()
	throttleTS := lastTelemetryDataReceived.Add(7 * 24 * time.Hour)
	return &throttleTS
}

// maybeLogError logs an error message about throttling if one hasn’t been
// logged recently. The purpose is to alert the user about throttling without
// overwhelming the cockroach log.
func (e *Enforcer) maybeLogError(ctx context.Context, err error, lastLogTimestamp *atomic.Int64) {
	nextLogMessage := timeutil.Unix(lastLogTimestamp.Load(), 0)
	nextLogMessage.Add(5 * time.Minute)

	now := timeutil.Now()
	if now.After(nextLogMessage) {
		lastLogTimestamp.Store(now.Unix())
		log.Infof(ctx, "throttling for license enforcement is active: %s", err.Error())
	}
}
