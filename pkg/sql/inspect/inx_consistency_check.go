// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package inspect

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/idxtype"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondatapb"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
	"github.com/cockroachdb/errors"
)

type indexConsistencyCheck struct {
	db      descs.DB
	tableID descpb.ID
	indexID descpb.IndexID

	tableDesc catalog.TableDescriptor
	secIndex  catalog.Index
	priIndex  catalog.Index
	rowsIt    isql.Rows
}

var _ inspectCheck = (*indexConsistencyCheck)(nil)

// Started implements the inspectCheck interface.
func (c *indexConsistencyCheck) Started() bool {
	return false
}

// Start implements the inspectCheck interface.
func (c *indexConsistencyCheck) Start(
	ctx context.Context, cfg *execinfra.ServerConfig, span roachpb.Span, workerIndex int,
) error {
	// SPILLY - check if the span is valid for this table

	// Load up the index and table descriptors.
	if err := c.loadCatalogInfo(ctx); err != nil {
		return err
	}

	var colToIdx catalog.TableColMap
	for _, col := range c.tableDesc.PublicColumns() {
		colToIdx.Set(col.GetID(), col.Ordinal())
	}

	var pkColumns, otherColumns []catalog.Column

	for i := 0; i < c.tableDesc.GetPrimaryIndex().NumKeyColumns(); i++ {
		colID := c.tableDesc.GetPrimaryIndex().GetKeyColumnID(i)
		col := c.tableDesc.PublicColumns()[colToIdx.GetDefault(colID)]
		pkColumns = append(pkColumns, col)
		colToIdx.Set(colID, -1)
	}

	// Collect all of the columns we are fetching from the index. This
	// includes the columns involved in the index: columns, extra columns,
	// and store columns.
	colIDs := catalog.TableColSet{}
	colIDs.UnionWith(c.secIndex.CollectKeyColumnIDs())
	colIDs.UnionWith(c.secIndex.CollectSecondaryStoredColumnIDs())
	colIDs.UnionWith(c.secIndex.CollectKeySuffixColumnIDs())
	colIDs.ForEach(func(colID descpb.ColumnID) {
		pos := colToIdx.GetDefault(colID)
		if pos == -1 {
			return
		}
		col := c.tableDesc.PublicColumns()[pos]
		otherColumns = append(otherColumns, col)
	})

	colNames := func(cols []catalog.Column) []string {
		res := make([]string, len(cols))
		for i := range cols {
			res[i] = cols[i].GetName()
		}
		return res
	}

	checkQuery := c.createIndexCheckQuery(
		colNames(pkColumns), colNames(otherColumns), o.tableDesc.GetID(), o.index, o.tableDesc.GetPrimaryIndexID(),
	)

	it, err := c.db.Executor().QueryIteratorEx(
		ctx, "inspect-inx-consistency-check", nil, /* txn */
		sessiondata.InternalExecutorOverride{
			User:             username.NodeUserName(),
			QualityOfService: &sessiondatapb.BulkLowQoS,
		},
		checkQuery,
	)
	if err != nil {
		return err
	}

	c.rowsIt = it
	// SPILLY - why do we care about this?
	//i.primaryColIdxs = make([]int, len(pkColumns))
	//for i := range o.primaryColIdxs {
	//	o.primaryColIdxs[i] = i
	//}
	//o.columns = append(pkColumns, otherColumns...)
	return nil
}

// Next implements the inspectCheck interface.
func (c *indexConsistencyCheck) Next(
	ctx context.Context, cfg *execinfra.ServerConfig,
) (*inspectIssue, error) {
	// SPILLY - guts
	return nil, nil
}

// Done implements the inspectCheck interface.
func (c *indexConsistencyCheck) Done(ctx context.Context) bool {
	// SPILLY - guts
	return true
}

// Close implements the inspectCheck interface.
func (c *indexConsistencyCheck) Close(ctx context.Context) error {
	if c.rowsIt != nil {
		if err := c.rowsIt.Close(); err != nil {
			return errors.Wrap(err, "closing index consistency check iterator")
		}
		c.rowsIt = nil
	}
	// SPILLY - guts
	return nil
}

func (c *indexConsistencyCheck) loadCatalogInfo(ctx context.Context) error {
	return c.db.DescsTxn(ctx, func(ctx context.Context, txn descs.Txn) error {
		var err error
		c.tableDesc, err = txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, c.tableID)
		if err != nil {
			return err
		}

		c.priIndex = c.tableDesc.GetPrimaryIndex()

		for _, idx := range c.tableDesc.PublicNonPrimaryIndexes() {
			if idx.GetID() != c.indexID {
				continue
			}

			// We can only check a secondary index that has a 1-to-1 mapping between
			// keys in the primary index.
			if idx.IsPartial() {
				return unimplemented.New(
					"INSPECT",
					"unsupported index type for consistency check: partial index",
				)
			}
			switch idx.GetType() {
			case idxtype.INVERTED, idxtype.VECTOR:
				return unimplemented.Newf(
					"INSPECT",
					"unsupported index type for consistency check: %s", idx.GetType(),
				)
			}

			// We found the index and it is valid for checking.
			c.secIndex = idx
			return nil
		}
		return errors.AssertionFailedf("no index with ID %d found in table %d", c.indexID, c.tableID)
	})
}

// createIndexCheckQuery will make the index check query for a given
// table and secondary index.
//
// The primary column names and the rest of the index
// columnsIt will also take into account an AS OF
// SYSTEM TIME clause.
//
// For example, given the following table schema:
//
//	CREATE TABLE table (
//	  k INT, l INT, a INT, b INT, c INT,
//	  PRIMARY KEY (k, l),
//	  INDEX idx (a,b),
//	)
//
// The generated query to check the `v_idx` will be:
//
//	SELECT pri.k  pri.l, pri.a, pri.b,
//	       sec.k, sec.l, sec.a, sec.b
//	FROM
//	  (SELECT k, l, a, b FROM [tbl_id AS table_pri]@{FORCE_INDEX=[pri_idx_id]}) AS pri
//	FULL OUTER JOIN
//	  (SELECT k, l, a, b FROM [tbl_id AS table_sec]@{FORCE_INDEX=[idx_id]} AS sec
//	ON
//	  pri.k = sec.k AND
//	  pri.l = sec.l AND
//	  pri.a IS NOT DISTINCT FROM sec.a AND
//	  pri.b IS NOT DISTINCT FROM sec.b
//	WHERE
//	  pri.k IS NULL OR sec.k IS NULL
//
// Explanation:
//
//  1. We scan both the primary index and the secondary index.
//
//  2. We join them on equality on the PK columns and "IS NOT DISTINCT FROM" on
//     the other index columns. "IS NOT DISTINCT FROM" is like equality except
//     that NULL equals NULL; it is not needed for the PK columns because those
//     can't be NULL.
//
//     Note: currently, only the PK columns will be used as join equality
//     columns, but that is sufficient.
//
//  3. We select the "outer" rows (those that had no match), effectively
//     achieving a "double" anti-join. We use the PK columns which cannot be
//     NULL except on these rows.
//
//  4. The results are as follows:
//     - if a PK column on the left is NULL, that means that the right-hand
//     side row from the secondary index had no match in the primary index.
//     - if a PK column on the right is NULL, that means that the left-hand
//     side row from the primary key had no match in the secondary index.
func (c *indexConsistencyCheck) createIndexCheckQuery(
	pkColumns, otherColumns []string,
	tableID descpb.ID,
	index catalog.Index,
	primaryIndexID descpb.IndexID,
) string {
	allColumns := append(pkColumns, otherColumns...)
	predicate := ""
	// We need to make sure we can handle the non-public column `rowid`
	// that is created for implicit primary keys. In order to do so, the
	// rendered columns need to explicit in the inner selects.
	const checkIndexQuery = `
	 SELECT %[1]s, %[2]s
	 FROM
	   (SELECT %[8]s FROM [%[3]d AS table_pri]@{FORCE_INDEX=[%[9]d]}%[10]s) AS pri
	 FULL OUTER JOIN
	   (SELECT %[8]s FROM [%[3]d AS table_sec]@{FORCE_INDEX=[%[4]d]}%[10]s) AS sec
	 ON %[5]s
	 WHERE %[6]s IS NULL OR %[7]s IS NULL`
	return fmt.Sprintf(
		checkIndexQuery,

		// 1: pri.k, pri.l, pri.a, pri.b
		strings.Join(colRefs("pri", allColumns), ", "),

		// 2: sec.k, sec.l, sec.a, sec.b
		strings.Join(colRefs("sec", allColumns), ", "),

		// 3
		tableID,

		// 4
		index.GetID(),

		// 5: pri.k = sec.k AND pri.l = sec.l AND
		//    pri.a IS NOT DISTINCT FROM sec.a AND pri.b IS NOT DISTINCT FROM sec.b
		// Note: otherColumns can be empty.
		strings.Join(
			append(
				pairwiseOp(colRefs("pri", pkColumns), colRefs("sec", pkColumns), "="),
				pairwiseOp(colRefs("pri", otherColumns), colRefs("sec", otherColumns), "IS NOT DISTINCT FROM")...,
			),
			" AND ",
		),

		// 6: pri.k
		colRef("pri", pkColumns[0]),

		// 7: sec.k
		colRef("sec", pkColumns[0]),

		// 8: k, l, a, b
		strings.Join(colRefs("", append(pkColumns, otherColumns...)), ", "),

		// 9
		primaryIndexID,

		// 10: WHERE <some predicate>
		// Can be empty string for non-partial indexes
		predicate,
	)
}

// SPILLY - move to a helpers file?

// col returns the string for referencing a column, with a specific alias,
// e.g. "table.col".
func colRef(tableAlias string, columnName string) string {
	u := tree.UnrestrictedName(columnName)
	if tableAlias == "" {
		return u.String()
	}
	return fmt.Sprintf("%s.%s", tableAlias, &u)
}

// colRefs returns the strings for referencing a list of columns (as a list).
func colRefs(tableAlias string, columnNames []string) []string {
	res := make([]string, len(columnNames))
	for i := range res {
		res[i] = colRef(tableAlias, columnNames[i])
	}
	return res
}

// pairwiseOp joins each string on the left with the string on the right, with a
// given operator in-between. For example
//
//	pairwiseOp([]string{"a","b"}, []string{"x", "y"}, "=")
//
// returns
//
//	[]string{"a = x", "b = y"}.
func pairwiseOp(left []string, right []string, op string) []string {
	if len(left) != len(right) {
		panic(errors.AssertionFailedf("slice length mismatch (%d vs %d)", len(left), len(right)))
	}
	res := make([]string, len(left))
	for i := range res {
		res[i] = fmt.Sprintf("%s %s %s", left[i], op, right[i])
	}
	return res
}
