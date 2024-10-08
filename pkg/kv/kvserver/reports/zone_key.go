// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package reports

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/config"
)

// ZoneKey is the index of the first level in the constraint conformance report.
type ZoneKey struct {
	// ZoneID is the id of the zone this key is referencing.
	ZoneID config.ObjectID
	// SubzoneID identifies what subzone, if any, this key is referencing. The
	// zero value (also named NoSubzone) indicates that the key is referring to a
	// zone, not a subzone.
	SubzoneID base.SubzoneID
}

// NoSubzone is used inside a zoneKey to indicate that the key represents a
// zone, not a subzone.
const NoSubzone base.SubzoneID = 0

// MakeZoneKey creates a zoneKey.
//
// Use NoSubzone for subzoneID to indicate that the key references a zone, not a
// subzone.
func MakeZoneKey(zoneID config.ObjectID, subzoneID base.SubzoneID) ZoneKey {
	return ZoneKey{
		ZoneID:    zoneID,
		SubzoneID: subzoneID,
	}
}

func (k ZoneKey) String() string {
	return fmt.Sprintf("%d,%d", k.ZoneID, k.SubzoneID)
}

// Less compares two ZoneKeys.
func (k ZoneKey) Less(other ZoneKey) bool {
	if k.ZoneID < other.ZoneID {
		return true
	}
	if k.ZoneID > other.ZoneID {
		return false
	}
	return k.SubzoneID < other.SubzoneID
}
