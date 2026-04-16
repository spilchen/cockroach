// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package spanutils_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/crosscluster/replicationtestutils"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/spanutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestSpanToQueryBounds(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	testCases := []struct {
		desc string
		// tablePKValues are PK values initially inserted into the table.
		tablePKValues []string
		// startPKValue is the PK value used to create the span start key.
		startPKValue string
		// truncateStartPKValue removes end bytes from startPKValue to cause a
		// decoding error.
		truncateStartPKValue bool
		// endPKValue is the PK value used to create the span end key.
		endPKValue string
		// truncateEndPKValue removes end bytes from endPKValue to cause a
		// decoding error.
		truncateEndPKValue  bool
		expectedHasRows     bool
		expectedBoundsStart string
		expectedBoundsEnd   string
	}{
		{
			desc:            "empty table",
			tablePKValues:   []string{},
			expectedHasRows: false,
		},
		{
			desc:                "start key < table value",
			tablePKValues:       []string{"B"},
			startPKValue:        "A",
			expectedHasRows:     true,
			expectedBoundsStart: "B",
			expectedBoundsEnd:   "B",
		},
		{
			desc:                "start key = table value",
			tablePKValues:       []string{"A"},
			startPKValue:        "A",
			expectedHasRows:     true,
			expectedBoundsStart: "A",
			expectedBoundsEnd:   "A",
		},
		{
			desc:            "start key > table value",
			tablePKValues:   []string{"A"},
			startPKValue:    "B",
			expectedHasRows: false,
		},
		{
			desc:            "end key < table value",
			tablePKValues:   []string{"B"},
			endPKValue:      "A",
			expectedHasRows: false,
		},
		{
			desc:            "end key = table value",
			tablePKValues:   []string{"A"},
			endPKValue:      "A",
			expectedHasRows: false,
		},
		{
			desc:                "end key > table value",
			tablePKValues:       []string{"A"},
			endPKValue:          "B",
			expectedHasRows:     true,
			expectedBoundsStart: "A",
			expectedBoundsEnd:   "A",
		},
		{
			desc:                "start key between values",
			tablePKValues:       []string{"A", "B", "D", "E"},
			startPKValue:        "C",
			expectedHasRows:     true,
			expectedBoundsStart: "D",
			expectedBoundsEnd:   "E",
		},
		{
			desc:                "end key between values",
			tablePKValues:       []string{"A", "B", "D", "E"},
			endPKValue:          "C",
			expectedHasRows:     true,
			expectedBoundsStart: "A",
			expectedBoundsEnd:   "B",
		},
		{
			desc:                 "truncated start key",
			tablePKValues:        []string{"A", "B", "C"},
			startPKValue:         "B",
			truncateStartPKValue: true,
			expectedHasRows:      true,
			expectedBoundsStart:  "B",
			expectedBoundsEnd:    "C",
		},
		{
			desc:                "truncated end key",
			tablePKValues:       []string{"A", "B", "C"},
			endPKValue:          "B",
			truncateEndPKValue:  true,
			expectedHasRows:     true,
			expectedBoundsStart: "A",
			expectedBoundsEnd:   "A",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {

			const tableName = "tbl"
			ctx := context.Background()
			srv, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
			defer srv.Stopper().Stop(ctx)
			codec := srv.ApplicationLayer().Codec()

			sqlRunner := sqlutils.MakeSQLRunner(sqlDB)

			// Create table.
			sqlRunner.Exec(t, fmt.Sprintf("CREATE TABLE %s (id string PRIMARY KEY)", tableName))

			// Insert tablePKValues into table.
			if len(tc.tablePKValues) > 0 {
				insertValues := ""
				for i, val := range tc.tablePKValues {
					if i > 0 {
						insertValues += ", "
					}
					insertValues += "('" + val + "')"
				}
				sqlRunner.Exec(t, fmt.Sprintf("INSERT INTO %s VALUES %s", tableName, insertValues))
			}

			// Get table descriptor.
			tableDesc := desctestutils.TestingGetPublicTableDescriptor(
				kvDB,
				codec,
				"defaultdb", /* database */
				tableName,
			)
			primaryIndexDesc := tableDesc.GetPrimaryIndex().IndexDesc()
			pkColIDs := catalog.TableColMap{}
			for i, id := range primaryIndexDesc.KeyColumnIDs {
				pkColIDs.Set(id, i)
			}
			pkColTypes, err := spanutils.GetPKColumnTypes(tableDesc, primaryIndexDesc)
			require.NoError(t, err)
			pkColDirs := primaryIndexDesc.KeyColumnDirections

			var alloc tree.DatumAlloc
			primaryIndexSpan := tableDesc.PrimaryIndexSpan(codec)

			createKey := func(pkValue string, truncateKey bool, defaultKey roachpb.Key) roachpb.Key {
				if len(pkValue) == 0 {
					return defaultKey
				}
				keyValue := replicationtestutils.EncodeKV(t, codec, tableDesc, pkValue)
				key := keyValue.Key
				if truncateKey {
					key = key[:len(key)-3]
					kvKeyValues := []kv.KeyValue{{Key: key, Value: &keyValue.Value}}
					// Ensure truncated key cannot be decoded.
					_, err = rowenc.DecodeIndexKeyToDatums(codec, pkColIDs, pkColTypes, pkColDirs, kvKeyValues, &alloc)
					require.ErrorContainsf(t, err, "did not find terminator 0x0 in buffer", "pkValue=%s", pkValue)
				}
				return key
			}

			// Create keys for test.
			startKey := createKey(tc.startPKValue, tc.truncateStartPKValue, primaryIndexSpan.Key)
			endKey := createKey(tc.endPKValue, tc.truncateEndPKValue, primaryIndexSpan.EndKey)

			// Run test function.
			actualBounds, actualHasRows, err := spanutils.SpanToQueryBounds(ctx, kvDB, codec, pkColIDs, pkColTypes, pkColDirs, 1, roachpb.Span{
				Key:    startKey,
				EndKey: endKey,
			}, &alloc, hlc.Timestamp{})

			// Verify results.
			require.NoError(t, err)
			require.Equal(t, tc.expectedHasRows, actualHasRows)
			if actualHasRows {
				actualBoundsStart := string(*actualBounds.Start[0].(*tree.DString))
				require.Equalf(t, tc.expectedBoundsStart, actualBoundsStart, "start")
				actualBoundsEnd := string(*actualBounds.End[0].(*tree.DString))
				require.Equalf(t, tc.expectedBoundsEnd, actualBoundsEnd, "end")
			}
		})
	}
}

func TestSpanToQueryBoundsCompositeKeys(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderStress(t)
	skip.UnderRace(t)

	testCases := []struct {
		desc string
		// tablePKValues are PK values initially inserted into the table.
		tablePKValues [][]string
		// startPKValue is the PK value used to create the span start key.
		startPKValue []string
		// truncateStartPKValue removes end bytes from startPKValue to cause a
		// decoding error.
		truncateStartPKValue bool
		// endPKValue is the PK value used to create the span end key.
		endPKValue []string
		// truncateEndPKValue removes end bytes from endPKValue to cause a
		// decoding error.
		truncateEndPKValue  bool
		expectedHasRows     bool
		expectedBoundsStart []string
		expectedBoundsEnd   []string
	}{
		{
			desc:            "empty table",
			tablePKValues:   [][]string{},
			expectedHasRows: false,
		},
		{
			desc:                "start key < table value",
			tablePKValues:       [][]string{{"B", "2"}},
			startPKValue:        []string{"A", "1"},
			expectedHasRows:     true,
			expectedBoundsStart: []string{"B", "2"},
			expectedBoundsEnd:   []string{"B", "2"},
		},
		{
			desc:                "start key = table value",
			tablePKValues:       [][]string{{"A", "1"}},
			startPKValue:        []string{"A", "1"},
			expectedHasRows:     true,
			expectedBoundsStart: []string{"A", "1"},
			expectedBoundsEnd:   []string{"A", "1"},
		},
		{
			desc:            "start key > table value",
			tablePKValues:   [][]string{{"A", "1"}},
			startPKValue:    []string{"B", "2"},
			expectedHasRows: false,
		},
		{
			desc:            "end key < table value",
			tablePKValues:   [][]string{{"B", "2"}},
			endPKValue:      []string{"A", "1"},
			expectedHasRows: false,
		},
		{
			desc:            "end key = table value",
			tablePKValues:   [][]string{{"A", "1"}},
			endPKValue:      []string{"A", "1"},
			expectedHasRows: false,
		},
		{
			desc:                "end key > table value",
			tablePKValues:       [][]string{{"A", "1"}},
			endPKValue:          []string{"B", "2"},
			expectedHasRows:     true,
			expectedBoundsStart: []string{"A", "1"},
			expectedBoundsEnd:   []string{"A", "1"},
		},
		{
			desc:                "start key between values",
			tablePKValues:       [][]string{{"A", "1"}, {"B", "2"}, {"D", "4"}, {"E", "5"}},
			startPKValue:        []string{"C", "3"},
			expectedHasRows:     true,
			expectedBoundsStart: []string{"D", "4"},
			expectedBoundsEnd:   []string{"E", "5"},
		},
		{
			desc:                "end key between values",
			tablePKValues:       [][]string{{"A", "1"}, {"B", "2"}, {"D", "4"}, {"E", "5"}},
			endPKValue:          []string{"C", "3"},
			expectedHasRows:     true,
			expectedBoundsStart: []string{"A", "1"},
			expectedBoundsEnd:   []string{"B", "2"},
		},
		{
			desc:                 "truncated start key",
			tablePKValues:        [][]string{{"A", "1"}, {"B", "2"}, {"C", "3"}},
			startPKValue:         []string{"B", "2"},
			truncateStartPKValue: true,
			expectedHasRows:      true,
			expectedBoundsStart:  []string{"B", "2"},
			expectedBoundsEnd:    []string{"C", "3"},
		},
		{
			desc:                "truncated end key",
			tablePKValues:       [][]string{{"A", "1"}, {"B", "2"}, {"C", "3"}},
			endPKValue:          []string{"B", "2"},
			truncateEndPKValue:  true,
			expectedHasRows:     true,
			expectedBoundsStart: []string{"A", "1"},
			expectedBoundsEnd:   []string{"A", "1"},
		},
	}

	// Test with different column families, since this affects how the primary
	// key gets encoded.
	familyClauses := []string{
		"",
		"FAMILY (a, b), FAMILY (c),",
		"FAMILY (c), FAMILY (a, b),",
		"FAMILY (a), FAMILY (b), FAMILY (c),",
	}

	for _, tc := range testCases {
		for _, families := range familyClauses {
			t.Run(tc.desc, func(t *testing.T) {

				const tableName = "tbl"
				ctx := context.Background()
				srv, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
				defer srv.Stopper().Stop(ctx)
				codec := srv.ApplicationLayer().Codec()

				sqlRunner := sqlutils.MakeSQLRunner(sqlDB)

				// Create table.
				sqlRunner.Exec(t, fmt.Sprintf(`
				CREATE TABLE %s (
					a string,
					b string COLLATE en_US_u_ks_level2,
					c STRING,
					%s
					PRIMARY KEY(a,b)
				)`, tableName, families))

				// Insert tablePKValues into table.
				if len(tc.tablePKValues) > 0 {
					insertValues := ""
					for i, val := range tc.tablePKValues {
						if i > 0 {
							insertValues += ", "
						}
						insertValues += "('" + strings.Join(val, "','") + "')"
					}
					sqlRunner.Exec(t, fmt.Sprintf("INSERT INTO %s VALUES %s", tableName, insertValues))
				}

				// Get table descriptor.
				tableDesc := desctestutils.TestingGetPublicTableDescriptor(
					kvDB,
					codec,
					"defaultdb", /* database */
					tableName,
				)
				primaryIndexDesc := tableDesc.GetPrimaryIndex().IndexDesc()
				pkColIDs := catalog.TableColMap{}
				for i, id := range primaryIndexDesc.KeyColumnIDs {
					pkColIDs.Set(id, i)
				}
				pkColTypes, err := spanutils.GetPKColumnTypes(tableDesc, primaryIndexDesc)
				require.NoError(t, err)
				pkColDirs := primaryIndexDesc.KeyColumnDirections

				var alloc tree.DatumAlloc
				primaryIndexSpan := tableDesc.PrimaryIndexSpan(codec)

				createKey := func(pkValue []string, truncateKey bool, defaultKey roachpb.Key) roachpb.Key {
					if len(pkValue) == 0 {
						return defaultKey
					}
					require.Equal(t, 2, len(pkValue))
					dString := tree.NewDString(pkValue[0])
					dCollatedString, err := alloc.NewDCollatedString(pkValue[1], "en_US_u_ks_level2")
					require.NoError(t, err)

					keyValues := replicationtestutils.EncodeKVs(t, codec, tableDesc, dString, dCollatedString)
					key := keyValues[0].Key
					if truncateKey {
						key = key[:len(key)-3]
						kvKeyValues := make([]kv.KeyValue, len(keyValues))
						for i := range keyValues {
							kvKeyValues[i] = kv.KeyValue{Key: key, Value: &keyValues[i].Value}
						}
						// Ensure truncated key cannot be decoded.
						_, err = rowenc.DecodeIndexKeyToDatums(codec, pkColIDs, pkColTypes, pkColDirs, kvKeyValues, &alloc)
						require.ErrorContainsf(t, err, "did not find terminator 0x0 in buffer", "pkValue=%s", pkValue)
					}
					return key
				}

				// Create keys for test.
				startKey := createKey(tc.startPKValue, tc.truncateStartPKValue, primaryIndexSpan.Key)
				endKey := createKey(tc.endPKValue, tc.truncateEndPKValue, primaryIndexSpan.EndKey)

				// Run test function.
				actualBounds, actualHasRows, err := spanutils.SpanToQueryBounds(
					ctx, kvDB, codec, pkColIDs, pkColTypes, pkColDirs, tableDesc.NumFamilies(),
					roachpb.Span{
						Key:    startKey,
						EndKey: endKey,
					},
					&alloc,
					hlc.Timestamp{},
				)

				// Verify results.
				require.NoError(t, err)
				require.Equal(t, tc.expectedHasRows, actualHasRows)
				if actualHasRows {
					actualBoundsStart := []string{
						string(*actualBounds.Start[0].(*tree.DString)),
						actualBounds.Start[1].(*tree.DCollatedString).Contents,
					}
					require.Equalf(t, tc.expectedBoundsStart, actualBoundsStart, "start")
					actualBoundsEnd := []string{
						string(*actualBounds.End[0].(*tree.DString)),
						actualBounds.End[1].(*tree.DCollatedString).Contents,
					}
					require.Equalf(t, tc.expectedBoundsEnd, actualBoundsEnd, "end")
				}
			})
		}
	}
}

// TestQueryBoundsNoOverlapRandomized verifies that adjacent spans produced by
// range splits never generate overlapping SQL query bounds. It inserts random
// data, splits the table at random PK values, computes QueryBounds for each
// resulting span, and checks that:
//  1. No two adjacent spans' bounds overlap (end of span N >= start of span N+1).
//  2. The sum of per-span COUNT(*) queries equals the total row count.
func TestQueryBoundsNoOverlapRandomized(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderStress(t)
	skip.UnderRace(t)
	skip.UnderShort(t)

	ctx := context.Background()
	const numNodes = 3
	tcl := testcluster.StartTestCluster(t, numNodes, base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestControlsTenantsExplicitly,
		},
	})
	defer tcl.Stopper().Stop(ctx)

	srv := tcl.Server(0)
	sqlDB := sqlutils.MakeSQLRunner(srv.ApplicationLayer().SQLConn(t))
	kvDB := srv.ApplicationLayer().DB()
	codec := srv.ApplicationLayer().Codec()

	tests := []struct {
		name       string
		createStmt string
		numRows    int
		numSplits  int
		// insertRow generates an INSERT VALUES clause for the given pk.
		insertRow func(pk int) string
		// splitRow generates a SPLIT AT VALUES clause for the given pk.
		splitRow func(pk int) string
		// pkColNames for RenderQueryBounds.
		pkColNames []string
		pkDirs     []catenumpb.IndexColumn_Direction
	}{
		{
			name:       "single column int pk",
			createStmt: "CREATE TABLE test_single (k INT PRIMARY KEY, v STRING)",
			numRows:    10000,
			numSplits:  200,
			insertRow:  func(pk int) string { return fmt.Sprintf("(%d, 'val-%d')", pk, pk) },
			splitRow:   func(pk int) string { return fmt.Sprintf("(%d)", pk) },
			pkColNames: []string{"k"},
			pkDirs:     []catenumpb.IndexColumn_Direction{catenumpb.IndexColumn_ASC},
		},
		{
			name:       "composite pk asc/asc",
			createStmt: "CREATE TABLE test_composite (a INT, b INT, v STRING, PRIMARY KEY (a ASC, b ASC))",
			numRows:    10000,
			numSplits:  200,
			insertRow:  func(pk int) string { return fmt.Sprintf("(%d, %d, 'val')", pk/100, pk%100) },
			splitRow:   func(pk int) string { return fmt.Sprintf("(%d, %d)", pk/100, pk%100) },
			pkColNames: []string{"a", "b"},
			pkDirs: []catenumpb.IndexColumn_Direction{
				catenumpb.IndexColumn_ASC, catenumpb.IndexColumn_ASC,
			},
		},
		{
			name:       "composite pk asc/desc",
			createStmt: "CREATE TABLE test_mixed (a INT, b INT, v STRING, PRIMARY KEY (a ASC, b DESC))",
			numRows:    10000,
			numSplits:  200,
			insertRow:  func(pk int) string { return fmt.Sprintf("(%d, %d, 'val')", pk/100, pk%100) },
			splitRow:   func(pk int) string { return fmt.Sprintf("(%d, %d)", pk/100, pk%100) },
			pkColNames: []string{"a", "b"},
			pkDirs: []catenumpb.IndexColumn_Direction{
				catenumpb.IndexColumn_ASC, catenumpb.IndexColumn_DESC,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create table.
			sqlDB.Exec(t, tt.createStmt)

			// Extract table name from CREATE TABLE statement.
			tableName := strings.Fields(tt.createStmt)[2]

			// Insert rows with random gaps to create realistic key distribution.
			rng := rand.New(rand.NewSource(42))
			pks := make([]int, tt.numRows)
			pk := 0
			for i := 0; i < tt.numRows; i++ {
				pk += 1 + rng.Intn(5) // random gaps between keys
				pks[i] = pk
			}

			const batchSize = 100
			for i := 0; i < len(pks); i += batchSize {
				end := i + batchSize
				if end > len(pks) {
					end = len(pks)
				}
				var values []string
				for _, p := range pks[i:end] {
					values = append(values, tt.insertRow(p))
				}
				sqlDB.Exec(t, fmt.Sprintf("INSERT INTO %s VALUES %s", tableName, strings.Join(values, ",")))
			}

			// Split table at random PK values from the inserted data.
			splitIndices := rng.Perm(len(pks))[:tt.numSplits]
			for _, idx := range splitIndices {
				sqlDB.Exec(t, fmt.Sprintf(
					"ALTER TABLE %s SPLIT AT VALUES %s",
					tableName, tt.splitRow(pks[idx]),
				))
			}

			// Scatter ranges across nodes.
			sqlDB.Exec(t, fmt.Sprintf("ALTER TABLE %s SCATTER", tableName))

			// Get total row count.
			var totalRows int
			sqlDB.QueryRow(t, fmt.Sprintf("SELECT count(*) FROM %s", tableName)).Scan(&totalRows)
			require.Equal(t, tt.numRows, totalRows)

			// Get table descriptor.
			tableDesc := desctestutils.TestingGetPublicTableDescriptor(
				kvDB, codec, "defaultdb", tableName,
			)
			primaryIndex := tableDesc.GetPrimaryIndex()
			primaryIndexDesc := primaryIndex.IndexDesc()
			pkColIDs := catalog.TableColMap{}
			for i, id := range primaryIndexDesc.KeyColumnIDs {
				pkColIDs.Set(id, i)
			}
			pkColTypes, err := spanutils.GetPKColumnTypes(tableDesc, primaryIndexDesc)
			require.NoError(t, err)

			// Get all range spans for this table.
			primarySpan := tableDesc.PrimaryIndexSpan(codec)
			var rangeSpans []roachpb.Span
			rows := sqlDB.Query(t, fmt.Sprintf(
				"SELECT raw_start_key, raw_end_key FROM [SHOW RANGES FROM TABLE %s WITH KEYS]", tableName,
			))
			defer rows.Close()
			for rows.Next() {
				var startKey, endKey []byte
				require.NoError(t, rows.Scan(&startKey, &endKey))
				span := roachpb.Span{
					Key:    roachpb.Key(startKey),
					EndKey: roachpb.Key(endKey),
				}
				// Clamp to table bounds.
				if span.Key.Compare(primarySpan.Key) < 0 {
					span.Key = primarySpan.Key
				}
				if span.EndKey.Compare(primarySpan.EndKey) > 0 {
					span.EndKey = primarySpan.EndKey
				}
				if span.Key.Compare(span.EndKey) < 0 {
					rangeSpans = append(rangeSpans, span)
				}
			}
			require.NoError(t, rows.Err())
			require.NotEmpty(t, rangeSpans, "expected at least one range span")

			// Compute query bounds for each span and check for overlaps.
			type boundsResult struct {
				span   roachpb.Span
				bounds spanutils.QueryBounds
			}
			var allBounds []boundsResult
			var alloc tree.DatumAlloc

			for _, span := range rangeSpans {
				bounds, hasRows, err := spanutils.SpanToQueryBounds(
					ctx, kvDB, codec, pkColIDs, pkColTypes, tt.pkDirs,
					len(tableDesc.GetFamilies()), span, &alloc, hlc.Timestamp{},
				)
				require.NoError(t, err)
				if hasRows {
					allBounds = append(allBounds, boundsResult{span: span, bounds: bounds})
				}
			}
			require.NotEmpty(t, allBounds, "expected at least one span with rows")

			// Verify no overlap between adjacent bounds.
			for i := 1; i < len(allBounds); i++ {
				prev := allBounds[i-1]
				curr := allBounds[i]
				// Compare prev.End with curr.Start using datum comparison.
				// For no overlap, prev.End must be strictly less than curr.Start.
				require.Equal(t, len(prev.bounds.End), len(curr.bounds.Start),
					"bound lengths should match for spans %d and %d", i-1, i)
				cmp := compareDatums(t, prev.bounds.End, curr.bounds.Start, tt.pkDirs)
				require.Truef(t, cmp < 0,
					"overlap detected between span %d (end=%s) and span %d (start=%s)",
					i-1, prev.bounds.End, i, curr.bounds.Start,
				)
			}

			// Verify per-span counts sum to total using the actual rendered
			// predicates, matching how the inspect job counts rows.
			var countSum int
			for _, br := range allBounds {
				predicate, err := spanutils.RenderQueryBounds(
					tt.pkColNames, tt.pkDirs, pkColTypes,
					len(br.bounds.Start), len(br.bounds.End),
					true, /* startIncl */
					1,    /* endPlaceholderOffset */
				)
				require.NoError(t, err)

				// Build query args: end bounds first, then start bounds (matching
				// the convention in check_helpers.go).
				queryArgs := make([]interface{}, 0, len(br.bounds.End)+len(br.bounds.Start))
				for _, d := range br.bounds.End {
					queryArgs = append(queryArgs, d)
				}
				for _, d := range br.bounds.Start {
					queryArgs = append(queryArgs, d)
				}

				query := fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", tableName, predicate)
				var spanCount int
				sqlDB.QueryRow(t, query, queryArgs...).Scan(&spanCount)
				countSum += spanCount
			}
			require.Equalf(t, totalRows, countSum,
				"sum of per-span counts (%d) does not match total (%d)", countSum, totalRows)
		})
	}
}

// compareDatums compares two datum slices lexicographically, respecting column
// directions. Returns negative if a < b, 0 if equal, positive if a > b.
func compareDatums(t *testing.T, a, b tree.Datums, dirs []catenumpb.IndexColumn_Direction) int {
	t.Helper()
	evalCtx := eval.NewTestingEvalContext(cluster.MakeTestingClusterSettings())
	for i := range a {
		cmp, err := a[i].Compare(context.Background(), evalCtx, b[i])
		require.NoError(t, err)
		if dirs[i] == catenumpb.IndexColumn_DESC {
			cmp = -cmp
		}
		if cmp != 0 {
			return cmp
		}
	}
	return 0
}
