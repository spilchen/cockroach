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
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
)

type options struct {
	db             isql.DB
	isInitialStart bool
	isSystemTenant bool
	testingKnobs   *TestingKnobs
}

type Option interface {
	apply(*options)
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) {
	f(o)
}

func WithDB(db isql.DB) Option {
	return optionFunc(func(o *options) {
		o.db = db
	})
}

func WithInitialStart(v bool) Option {
	return optionFunc(func(o *options) {
		o.isInitialStart = v
	})
}

func WithSystemTenant(v bool) Option {
	return optionFunc(func(o *options) {
		o.isSystemTenant = v
	})
}

func WithTestingKnobs(tk *TestingKnobs) Option {
	return optionFunc(func(o *options) {
		if tk != nil {
			o.testingKnobs = tk
		}
	})
}
