// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package inspect

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

func indexEntryForDatums(
	row []tree.Datum, tableDesc catalog.TableDescriptor, index catalog.Index,
) (rowenc.IndexEntry, error) {
	var colIDtoRowIndex catalog.TableColMap
	for i, c := range tableDesc.PublicColumns() {
		colIDtoRowIndex.Set(c.GetID(), i)
	}
	indexEntries, err := rowenc.EncodeSecondaryIndex(
		context.Background(), keys.SystemSQLCodec, tableDesc, index,
		colIDtoRowIndex, row, rowenc.EmptyVectorIndexEncodingHelper, true, /* includeEmpty */
	)
	if err != nil {
		return rowenc.IndexEntry{}, err
	}

	if len(indexEntries) != 1 {
		return rowenc.IndexEntry{}, errors.Newf("expected 1 index entry, got %d. got %#v", len(indexEntries), indexEntries)
	}
	return indexEntries[0], nil
}

// removeIndexEntryForDatums removes the index entries for the row
// that represents the given datums. It assumes the datums are in the
// order of the public columns of the table. It further assumes that
// the row only produces a single index entry.
func removeIndexEntryForDatums(
	ctx context.Context,
	row []tree.Datum,
	kvDB *kv.DB,
	tableDesc catalog.TableDescriptor,
	index catalog.Index,
) error {
	entry, err := indexEntryForDatums(row, tableDesc, index)
	if err != nil {
		return err
	}
	keys, err := kvDB.Del(ctx, entry.Key)
	if err != nil {
		return err
	}
	for i := range keys {
		fmt.Printf("SPILLY: removing index entry: %+v\n", keys[i])
	}
	return err
}

// addIndexEntryForDatums adds an index entry for the given datums. It assumes the datums are in the
// order of the public columns of the table. It further assumes that the row only produces a single
// index entry.
func addIndexEntryForDatums(
	ctx context.Context,
	row []tree.Datum,
	kvDB *kv.DB,
	tableDesc catalog.TableDescriptor,
	index catalog.Index,
) error {
	entry, err := indexEntryForDatums(row, tableDesc, index)
	if err != nil {
		return err
	}
	fetchKV, err := kvDB.Get(ctx, entry.Key)
	if err != nil {
		return err
	}
	fmt.Printf("SPILLY: fetched existing entry: %+v\n", fetchKV)
	fmt.Printf("SPILLY: adding index entry: %+v\n", entry)
	err = kvDB.Put(ctx, entry.Key, &entry.Value)
	if err != nil {
		return err
	}
	fetchKV, err = kvDB.Get(ctx, entry.Key)
	if err != nil {
		return err
	}
	fmt.Printf("SPILLY: fetched existing entry: %+v\n", fetchKV)
	return nil
}

func TestDetectIndexConsistencyErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	issueLogger := &testIssueCollector{}
	ctx := context.Background()
	const numNodes = 3
	cl := serverutils.StartCluster(t, numNodes, base.TestClusterArgs{
		ReplicationMode: base.ReplicationManual,
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
			Knobs: base.TestingKnobs{
				Inspect: &sql.InspectTestingKnobs{
					InspectIssueLogger: issueLogger,
				},
			},
		},
	})
	defer cl.Stopper().Stop(ctx)

	db := cl.ServerConn(0)
	kvDB := cl.Server(0).DB()
	codec := cl.Server(0).Codec()
	ie := cl.Server(0).InternalExecutor().(*sql.InternalExecutor)
	r := sqlutils.MakeSQLRunner(db)

	for _, tc := range []struct {
		// desc is a description of the test case.
		desc string
		// splitRangeDDL is the DDL to split the table into multiple ranges. The
		// table will be populated via generate_series using values up to 1000.
		splitRangeDDL string
		// indexDDL is the DDL to create the indexes on the table.
		indexDDL []string
		// missingIndexEntrySelector defines a SQL predicate that selects rows
		// whose secondary index entries will be manually deleted to simulate
		// missing index entries (i.e., present in the primary index but not in the
		// secondary).
		missingIndexEntrySelector string
		// danglingIndexEntryInsertQuery is a full SQL SELECT expression that generates
		// rows to be inserted directly into the secondary index without corresponding
		// primary index entries, simulating dangling entries.
		danglingIndexEntryInsertQuery string
		// SPILLY temp debug code
		secFetcher string
		// expectedIssues is the list of expected issues that should be found.
		expectedIssues []inspectIssue
	}{
		{
			desc: "happy path sanity",
			indexDDL: []string{
				"CREATE INDEX idx_t_a ON test.t (b)",
			},
			missingIndexEntrySelector: "", /* nothing corrupted */
		},
		{
			desc:          "3 ranges, secondary index on a, 1 missing entry",
			splitRangeDDL: "ALTER TABLE test.t SPLIT AT VALUES (333),(666)",
			indexDDL: []string{
				"CREATE INDEX idx_t_a ON test.t (a)",
			},
			missingIndexEntrySelector: "a = 4",
			expectedIssues: []inspectIssue{
				{ErrorType: "missing_secondary_index_entry", PrimaryKey: "e'(4, \\'d_4\\')'"},
			},
		},
		{
			desc:          "2 ranges, secondary index on 'b' with storing 'e', 1 missing entry",
			splitRangeDDL: "ALTER TABLE test.t SPLIT AT VALUES (500)",
			indexDDL: []string{
				"CREATE INDEX idx_t_a ON test.t (b) STORING (e)",
			},
			missingIndexEntrySelector: "a = 800",
			expectedIssues: []inspectIssue{
				{ErrorType: "missing_secondary_index_entry", PrimaryKey: "e'(800, \\'d_800\\')'"},
			},
		},
		{
			desc:          "10 ranges, secondary index on c with storing 'f', many missing entry",
			splitRangeDDL: "ALTER TABLE test.t SPLIT AT VALUES (100),(200),(300),(400),(500),(600),(700),(800),(900)",
			indexDDL: []string{
				"CREATE INDEX idx_t_a ON test.t (c) STORING (f)",
			},
			missingIndexEntrySelector: "A BETWEEN 598 AND 601",
			expectedIssues: []inspectIssue{
				{ErrorType: "missing_secondary_index_entry", PrimaryKey: "e'(598, \\'d_598\\')'"},
				{ErrorType: "missing_secondary_index_entry", PrimaryKey: "e'(599, \\'d_599\\')'"},
				{ErrorType: "missing_secondary_index_entry", PrimaryKey: "e'(600, \\'d_600\\')'"},
				{ErrorType: "missing_secondary_index_entry", PrimaryKey: "e'(601, \\'d_601\\')'"},
			},
		},
		{
			desc:          "2 ranges, secondary index on 'a', 1 dangling entry",
			splitRangeDDL: "ALTER TABLE test.t SPLIT AT VALUES (500)",
			indexDDL: []string{
				"CREATE INDEX idx_t_a ON test.t (a) STORING (f)",
			},
			// SPILLY - this ended up being a good test case because it causes the INSPECT query to fail with an internal error
			danglingIndexEntryInsertQuery: "SELECT 3, 30, 300, 'd_3', 'e_3', -56.712",
			secFetcher:                    "SELECT 3, 30, 300, 'd_3', 'e_3', 4.5 UNION ALL SELECT 3, 30, 300, 'd_3', 'e_3', -56.712",
			expectedIssues: []inspectIssue{
				{ErrorType: "dangling_secondary_index_entry"},
			},
		},
		{
			desc:          "2 ranges, secondary index on 'b' storing 'f', 1 dangling entry",
			splitRangeDDL: "ALTER TABLE test.t SPLIT AT VALUES (500)",
			indexDDL: []string{
				"CREATE INDEX idx_t_a ON test.t (b) STORING (c)",
			},
			danglingIndexEntryInsertQuery: "SELECT 15, 30, 300, 'corrupt', 'e_3', 300.5",
			expectedIssues: []inspectIssue{
				{ErrorType: "dangling_secondary_index_entry", PrimaryKey: "e'(15, \\'corrupt\\')'"},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			issueLogger.Reset()
			r.ExecMultiple(t,
				`DROP DATABASE IF EXISTS test`,
				`CREATE DATABASE test`,
				`CREATE TABLE test.t (
					a INT,
					b INT,
					c INT NOT NULL,
				  d TEXT,
				  e TEXT NOT NULL,
				  f FLOAT,
				  PRIMARY KEY (a, d),
					FAMILY fam0 (a, b, c, d, e, f)
				)`,
				`INSERT INTO test.t (a, b, c, d, e, f)
				SELECT
					gs1 AS a,
					gs1 * 10 AS b,
					gs1 * 100 AS c,
					'd_' || gs1::STRING AS d,
					'e_' || gs1::STRING AS e,
					gs1 * 1.5 AS f
				FROM generate_series(1, 10) AS gs1;`,
			)

			// Split the values and relocate leases so that the INSPECT job ends up
			// running in parallel across multiple nodes.
			r.ExecMultiple(t, tc.splitRangeDDL)
			ranges, err := db.Query(`
				WITH r AS (SHOW RANGES FROM TABLE test.t WITH DETAILS)
        SELECT range_id FROM r ORDER BY 1`)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, ranges.Close())
			})
			for i := 0; ranges.Next(); i++ {
				var rangeID int
				err = ranges.Scan(&rangeID)
				require.NoError(t, err)
				relocate := fmt.Sprintf("ALTER RANGE %d RELOCATE LEASE TO %d", rangeID, (i%numNodes)+1)
				r.Exec(t, relocate)
			}

			r.ExecMultiple(t, tc.indexDDL...)

			tableDesc := desctestutils.TestingGetPublicTableDescriptor(kvDB, codec, "test", "t")
			secIndex := tableDesc.PublicNonPrimaryIndexes()[0]

			// Apply test-specific corruption based on configured selectors:
			// - If missingIndexEntrySelector is set, we delete the secondary index entries
			//   for all rows matching the predicate. This simulates missing index entries
			//   (i.e., primary rows with no corresponding secondary index key).
			// - If danglingIndexEntryInsertQuery is set, we evaluate the query to produce
			//   synthetic rows and manually insert their secondary index entries without
			//   inserting matching primary rows. This simulates dangling entries
			//   (i.e., secondary index keys pointing to non-existent primary keys).
			if tc.missingIndexEntrySelector != "" {
				rows, err := ie.QueryBufferedEx(ctx, "missing-index-entry-query-filter", nil /* no txn */, sessiondata.NodeUserSessionDataOverride,
					`SELECT * FROM test.t WHERE `+tc.missingIndexEntrySelector)
				require.NoError(t, err)
				require.Greater(t, len(rows), 0)
				t.Logf("Corrupting %d rows that match filter by deleting secondary index keys: %s\n", len(rows), tc.missingIndexEntrySelector)
				for _, row := range rows {
					err = removeIndexEntryForDatums(ctx, row, kvDB, tableDesc, secIndex)
					require.NoError(t, err)
				}
			}
			if tc.danglingIndexEntryInsertQuery != "" {
				rows, err := ie.QueryBufferedEx(ctx, "dangling-index-insert-query", nil, /* no txn */
					sessiondata.NodeUserSessionDataOverride, tc.danglingIndexEntryInsertQuery)
				require.NoError(t, err)
				require.Greater(t, len(rows), 0)
				t.Logf("Corrupting %d rows by inserting keys into secondary index returned by this query: %s",
					len(rows), tc.danglingIndexEntryInsertQuery)
				for _, row := range rows {
					err = addIndexEntryForDatums(ctx, row, kvDB, tableDesc, secIndex)
					require.NoError(t, err)
				}
			}
			if tc.secFetcher != "" {
				rows, err := ie.QueryBufferedEx(ctx, "dangling-index-insert-query", nil, /* no txn */
					sessiondata.NodeUserSessionDataOverride, tc.secFetcher)
				require.NoError(t, err)
				require.Greater(t, len(rows), 0)
				for _, row := range rows {
					entry, err := indexEntryForDatums(row, tableDesc, secIndex)
					require.NoError(t, err)
					fetchKV, err := kvDB.Get(ctx, entry.Key)
					require.NoError(t, err)
					fmt.Printf("SPILLY: fetched existing entry: %+v\n", fetchKV)
				}
			}
			r.Exec(t,
				`INSERT INTO test.t (a, b, c, d, e, f)
				SELECT
					gs1 AS a,
					gs1 * 10 AS b,
					gs1 * 100 AS c,
					'd_' || gs1::STRING AS d,
					'e_' || gs1::STRING AS e,
					gs1 * 1.5 AS f
				FROM generate_series(50, 60) AS gs1;`,
			)

			//selRows, err := db.Query(`SELECT * FROM test.t@{FORCE_INDEX=[1]}`)
			//require.NoError(t, err)
			//defer selRows.Close()
			//for selRows.Next() {
			//	var a, b, c int
			//	var d, e string
			//	var f float64
			//	err = selRows.Scan(&a, &b, &c, &d, &e, &f)
			//	require.NoError(t, err)
			//	fmt.Printf("SPILLY: row: pk: a=%d b=%d c=%d d=%s e=%s f=%f\n", a, b, c, d, e, f)
			//}
			//selRows, err = db.Query(`SELECT * FROM test.t@{FORCE_INDEX=[2]}`)
			//require.NoError(t, err)
			//defer selRows.Close()
			//for selRows.Next() {
			//	var a, b, c int
			//	var d, e string
			//	var f float64
			//	err = selRows.Scan(&a, &b, &c, &d, &e, &f)
			//	require.NoError(t, err)
			//	fmt.Printf("SPILLY: row: sec: a=%d b=%d c=%d d=%s e=%s f=%f\n", a, b, c, d, e, f)
			//}
			//selRows, err = db.Query(`SELECT * FROM test.t@idx_t_a`)
			//require.NoError(t, err)
			//defer selRows.Close()
			//for selRows.Next() {
			//	var a, b, c int
			//	var d, e string
			//	var f float64
			//	err = selRows.Scan(&a, &b, &c, &d, &e, &f)
			//	require.NoError(t, err)
			//	fmt.Printf("SPILLY: row: sec: a=%d b=%d c=%d d=%s e=%s f=%f\n", a, b, c, d, e, f)
			//}

			//	selRows, err = db.Query(`
			//SELECT pri.a, pri.d, sec.a, sec.d
			//FROM
			//  (SELECT a, d FROM [106 AS table_pri]@{FORCE_INDEX=[1]}) AS pri
			//FULL OUTER JOIN
			//  (SELECT a, d FROM [106 AS table_sec]@{FORCE_INDEX=[2]}) AS sec
			//ON pri.a = sec.a AND pri.d = sec.d
			//WHERE pri.a IS NULL OR sec.a IS NULL`)
			//	require.NoError(t, err)
			//	defer selRows.Close()
			//	for selRows.Next() {
			//		var pri_a, sec_a int
			//		var pri_d, sec_d string
			//		err = selRows.Scan(&pri_a, &pri_d, &sec_a, &sec_d)
			//		require.NoError(t, err)
			//		fmt.Printf("SPILLY: row: scrub: pri_a=%d pri_d=%s sec_a=%d sec_d=%s\n", pri_a, pri_d, sec_a, sec_d)
			//	}

			// SPILLY - the explain output shows that it's a no-op. There is a rule that eliminates this.
			selRows, err := db.Query(`
	 EXPLAIN (plan,verbose) SELECT pri.a, pri.d, sec.a, sec.d
	 FROM
	   (SELECT a, d FROM test.t@t_pkey) AS pri
	 FULL OUTER JOIN
	   (SELECT a, d FROM test.t@idx_t_a) AS sec
	 ON pri.a = sec.a AND pri.d = sec.d
	 WHERE pri.a IS NULL OR sec.a IS NULL`)
			require.NoError(t, err)
			defer selRows.Close()
			fmt.Printf("SPILLY: EXPLAIN output:\n")
			for selRows.Next() {
				var plan string
				err = selRows.Scan(&plan)
				require.NoError(t, err)
				fmt.Printf("%s\n", plan)
			}

			// TODO(148365): Run INSPECT instead of SCRUB.
			_, err = db.Exec(`SET enable_scrub_job=true`)
			require.NoError(t, err)
			_, err = db.Query(`EXPERIMENTAL SCRUB TABLE test.t WITH OPTIONS INDEX ALL`)
			if len(tc.expectedIssues) == 0 {
				require.NoError(t, err)
				require.Equal(t, 0, issueLogger.NumIssuesFound())
				return
			}

			require.Error(t, err)
			require.Regexp(t, "INSPECT found inconsistencies", err.Error())

			numExpected := len(tc.expectedIssues)
			numFound := issueLogger.NumIssuesFound()

			dumpAllFoundIssues := func() {
				t.Log("Dumping all found issues:")
				for i := 0; i < numFound; i++ {
					t.Logf("  [%d] %s", i, issueLogger.Issue(i))
				}
			}

			// The number of expected and actual issues must match exactly.
			// If they don't, dump all found issues for debugging and fail.
			if numExpected != numFound {
				t.Logf("Mismatch in number of issues: expected %d, found %d", numExpected, numFound)
				dumpAllFoundIssues()
				t.Fatalf("expected %d issues, but found %d", numExpected, numFound)
			}

			// For each expected issue, ensure it was found.
			for i, expectedIssue := range tc.expectedIssues {
				foundIssue := issueLogger.FindIssue(expectedIssue.ErrorType, expectedIssue.PrimaryKey)
				if foundIssue == nil {
					t.Logf("Expected issue not found: %q", expectedIssue)
					dumpAllFoundIssues()
					t.Fatalf("expected issue %d (%q) not found", i, expectedIssue)
				}
				require.NotEqual(t, 0, foundIssue.DatabaseID, "expected issue to have a database ID: %s", expectedIssue)
				require.NotEqual(t, 0, foundIssue.SchemaID, "expected issue to have a schema ID: %s", expectedIssue)
				require.NotEqual(t, 0, foundIssue.ObjectID, "expected issue to have an object ID: %s", expectedIssue)
				require.NotEqual(t, time.Time{}, foundIssue.AOST, "expected issue to have an AOST time: %s", expectedIssue)
			}

			// SPILLY - validate the job status
		})
	}
}
