// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"net/url"
	"strconv"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/errors"
)

// ExtractSourceInstanceID parses a nodelocal URI and returns the source
// SQL instance ID. Returns an error if the URI cannot be parsed or is not
// a nodelocal URI with a numeric host.
func ExtractSourceInstanceID(uri string) (base.SQLInstanceID, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return 0, errors.Wrapf(err, "failed to parse SST URI %q", uri)
	}
	if u.Scheme != "nodelocal" {
		return 0, errors.Newf("expected nodelocal URI, got scheme %q in %q", u.Scheme, uri)
	}
	if u.Host == "" || u.Host == "self" {
		return 0, errors.Newf("nodelocal URI %q has no explicit instance ID", uri)
	}
	id, err := strconv.Atoi(u.Host)
	if err != nil {
		return 0, errors.Wrapf(err, "failed to parse instance ID from URI %q", uri)
	}
	return base.SQLInstanceID(id), nil
}
