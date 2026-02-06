// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"time"

	"github.com/cockroachdb/cockroach/pkg/settings"
)

// InstanceUnavailabilityTimeout is the duration after which the job will be
// marked as permanently failed if required SQL instances remain unavailable.
// The timeout alone is not sufficient; the retry count must also exceed
// InstanceUnavailabilityMaxRetries (if set > 0).
var InstanceUnavailabilityTimeout = settings.RegisterDurationSetting(
	settings.ApplicationLevel,
	"bulkmerge.instance_unavailability_timeout",
	"duration to wait for unavailable SQL instances before permanently failing the job; "+
		"both this timeout and max_retries must be exceeded for permanent failure",
	30*time.Minute,
	settings.WithPublic,
)

// InstanceUnavailabilityMaxRetries is the maximum number of retries allowed
// when SQL instances are unavailable. If set to 0 (default), there is no
// retry limit and only the timeout is used. For permanent failure, both
// the timeout and this retry limit (if > 0) must be exceeded.
var InstanceUnavailabilityMaxRetries = settings.RegisterIntSetting(
	settings.ApplicationLevel,
	"bulkmerge.instance_unavailability_max_retries",
	"maximum number of retries when SQL instances are unavailable; "+
		"0 means unlimited retries (only timeout applies); "+
		"both timeout and max_retries must be exceeded for permanent failure",
	0,
	settings.WithPublic,
)
