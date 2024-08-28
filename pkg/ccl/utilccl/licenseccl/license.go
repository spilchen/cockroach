// Copyright 2017 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package licenseccl

import (
	"encoding/base64"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
)

// EnterpriseLicense is the setting that stores the license.
//
// TODO(spilchen): move this setting to pkg/server and don't export it like we
// used to do before. This has to live here until we do cleanup and/or possible
// removal of the ccl/utilccl package.
var EnterpriseLicense = settings.RegisterStringSetting(
	settings.SystemVisible,
	"enterprise.license",
	"the encoded cluster license",
	"",
	settings.WithValidateString(
		func(sv *settings.Values, s string) error {
			_, err := Decode(s)
			if err != nil {
				return pgerror.WithCandidateCode(err, pgcode.Syntax)
			}
			return nil
		},
	),
	// Even though string settings are non-reportable by default, we
	// still mark them explicitly in case a future code change flips the
	// default.
	settings.WithReportable(false),
	settings.WithPublic,
)

// LicensePrefix is a prefix on license strings to make them easily recognized.
const LicensePrefix = "crl-0-"

// Encode serializes the License as a base64 string.
func (l *License) Encode() (string, error) {
	bytes, err := protoutil.Marshal(l)
	if err != nil {
		return "", err
	}
	return LicensePrefix + base64.RawStdEncoding.EncodeToString(bytes), nil
}

// Decode attempts to read a base64 encoded License.
func Decode(s string) (*License, error) {
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, LicensePrefix) {
		return nil, errors.New("invalid license string")
	}
	s = strings.TrimPrefix(s, LicensePrefix)
	data, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid license string")
	}
	var lic License
	if err := protoutil.Unmarshal(data, &lic); err != nil {
		return nil, errors.Wrap(err, "invalid license string")
	}
	return &lic, nil
}

func (u License_Usage) String() string {
	switch u {
	case Unspecified:
		return ""
	case Production:
		return "production"
	case PreProduction:
		return "pre-production"
	case Development:
		return "development"
	default:
		return "other"
	}
}
