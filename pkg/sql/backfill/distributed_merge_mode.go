// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package backfill

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/errors"
)

// DistributedMergeConsumer identifies which backfill pipeline is attempting to
// opt into the distributed merge infrastructure.
type DistributedMergeConsumer int

const (
	// DistributedMergeConsumerLegacy corresponds to the legacy schema changer.
	DistributedMergeConsumerLegacy DistributedMergeConsumer = iota
	// DistributedMergeConsumerDeclarative corresponds to the declarative schema
	// changer (new schema change).
	DistributedMergeConsumerDeclarative
)

type distributedMergeIndexBackfillMode int64

const (
	distributedMergeModeDisabled distributedMergeIndexBackfillMode = iota
	distributedMergeModeEnabled
	distributedMergeModeLegacy
	distributedMergeModeDeclarative
	// aliases for synonyms.
	distributedMergeModeAliasFalse
	distributedMergeModeAliasTrue
	distributedMergeModeAliasOff
	distributedMergeModeAliasOn
)

// DistributedMergeIndexBackfillMode exposes the cluster setting used to control
// when index backfills run through the distributed merge pipeline.
var DistributedMergeIndexBackfillMode = settings.RegisterEnumSetting(
	settings.ApplicationLevel,
	"bulkio.index_backfill.distributed_merge.mode",
	"controls when the distributed merge pipeline powers index backfills: disabled/off/false, legacy, declarative, or enabled/on/true",
	"disabled",
	map[distributedMergeIndexBackfillMode]string{
		distributedMergeModeDisabled:    "disabled",
		distributedMergeModeEnabled:     "enabled",
		distributedMergeModeLegacy:      "legacy",
		distributedMergeModeDeclarative: "declarative",
		distributedMergeModeAliasFalse:  "false",
		distributedMergeModeAliasTrue:   "true",
		distributedMergeModeAliasOff:    "off",
		distributedMergeModeAliasOn:     "on",
	},
	settings.WithRetiredName("bulkio.index_backfill.distributed_merge.enabled"),
)

// ShouldEnableDistributedMergeIndexBackfill determines whether the specified
// backfill consumer should opt into the distributed merge pipeline based on the
// current cluster setting and version state.
func ShouldEnableDistributedMergeIndexBackfill(
	ctx context.Context, st *cluster.Settings, consumer DistributedMergeConsumer,
) (bool, error) {
	mode := DistributedMergeIndexBackfillMode.Get(&st.SV)
	var enable bool
	switch mode {
	case distributedMergeModeDisabled, distributedMergeModeAliasFalse, distributedMergeModeAliasOff:
		return false, nil
	case distributedMergeModeLegacy:
		enable = consumer == DistributedMergeConsumerLegacy
	case distributedMergeModeDeclarative:
		enable = consumer == DistributedMergeConsumerDeclarative
	case distributedMergeModeEnabled, distributedMergeModeAliasTrue, distributedMergeModeAliasOn:
		enable = true
	default:
		return false, errors.AssertionFailedf("unrecognized distributed merge index backfill mode %d", mode)
	}
	if !enable {
		return false, nil
	}
	if !st.Version.IsActive(ctx, clusterversion.V26_1) {
		return false, pgerror.New(pgcode.FeatureNotSupported, "distributed merge requires cluster version 26.1")
	}
	return true, nil
}
