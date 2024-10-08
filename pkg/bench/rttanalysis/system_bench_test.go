// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package rttanalysis

import "testing"

func BenchmarkSystemDatabaseQueries(b *testing.B) { reg.Run(b) }
func init() {
	reg.Register("SystemDatabaseQueries", []RoundTripBenchTestCase{
		// This query performs 1-2 lookups: getting the descriptor ID by Name, then
		// fetching the system table descriptor. The descriptor is then cached.
		{
			Name: "select system.users with schema Name",
			Stmt: `SELECT username, "hashedPassword" FROM system.public.users WHERE username = 'root'`,
		},
		// This query performs 1 extra lookup since the executor first tries to
		// lookup the Name `current_db.system.users`.
		{
			Name: "select system.users without schema Name",
			Stmt: `SELECT username, "hashedPassword" FROM system.users WHERE username = 'root'`,
		},
		// This query performs 0 extra lookups since the Name resolution logic does
		// not try to resolve `"".system.users` and instead resolves
		//`system.public.users` right away.
		{
			Name:  "select system.users with empty database Name",
			Setup: `SET sql_safe_updates = false; USE "";`,
			Stmt:  `SELECT username, "hashedPassword"  FROM system.users WHERE username = 'root'`,
		},
	})
}
