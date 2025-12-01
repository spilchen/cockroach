// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"bytes"
	"context"
	"slices"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
)

// Merge builds and executes a DistSQL plan that merges the provided SST
// descriptors according to the supplied spans.
func Merge(
	ctx context.Context,
	execCtx sql.JobExecContext,
	ssts []execinfrapb.BulkMergeSpec_SST,
	spans []roachpb.Span,
	outputURI func(sqlInstance base.SQLInstanceID) string,
) ([]execinfrapb.BulkMergeSpec_SST, error) {
	execCfg := execCtx.ExecCfg()

	plan, planCtx, err := newBulkMergePlan(ctx, execCtx, ssts, spans, outputURI)
	if err != nil {
		return nil, err
	}
	defer plan.Release()

	var result execinfrapb.BulkMergeSpec_Output
	rowWriter := sql.NewCallbackResultWriter(func(ctx context.Context, row tree.Datums) error {
		return protoutil.Unmarshal([]byte(*row[0].(*tree.DBytes)), &result)
	})

	evalCtx := execCtx.ExtendedEvalContext()
	receiver := sql.MakeDistSQLReceiver(
		ctx,
		rowWriter,
		tree.Rows,
		execCfg.RangeDescriptorCache,
		nil,
		nil,
		evalCtx.Tracing,
	)
	defer receiver.Release()
	evalCtxCopy := evalCtx.Copy()

	execCtx.DistSQLPlanner().Run(
		ctx,
		planCtx,
		nil,
		plan,
		receiver,
		evalCtxCopy,
		nil,
	)

	if err := rowWriter.Err(); err != nil {
		return nil, err
	}

	slices.SortFunc(result.Ssts, func(i, j execinfrapb.BulkMergeSpec_SST) int {
		return bytes.Compare(i.StartKey, j.StartKey)
	})

	return result.Ssts, nil
}
