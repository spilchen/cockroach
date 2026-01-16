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
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/errors"
)

// Merge creates and waits on a DistSQL flow that merges the provided SSTs into
// the ranges defined by the input splits.
//
// expectedKeyCount, if non-zero, is used to validate that the total number of keys
// in the output SSTs matches the expected count. If there is a mismatch, an
// error is returned with detailed logging.
func Merge(
	ctx context.Context,
	execCtx sql.JobExecContext,
	ssts []execinfrapb.BulkMergeSpec_SST,
	spans []roachpb.Span,
	genOutputURIAndRecordPrefix func(sqlInstance base.SQLInstanceID) (string, error),
	iteration int,
	maxIterations int,
	writeTS *hlc.Timestamp,
	expectedKeyCount uint64,
) ([]execinfrapb.BulkMergeSpec_SST, error) {
	logMergeInputs(ctx, ssts, iteration, maxIterations, expectedKeyCount)

	execCfg := execCtx.ExecCfg()

	plan, planCtx, err := newBulkMergePlan(ctx, execCtx, ssts, spans, genOutputURIAndRecordPrefix, iteration, maxIterations, writeTS)
	if err != nil {
		return nil, err
	}

	var result execinfrapb.BulkMergeSpec_Output
	rowWriter := sql.NewCallbackResultWriter(func(ctx context.Context, row tree.Datums) error {
		return protoutil.Unmarshal([]byte(*row[0].(*tree.DBytes)), &result)
	})

	sqlReciever := sql.MakeDistSQLReceiver(
		ctx,
		rowWriter,
		tree.Rows,
		execCfg.RangeDescriptorCache,
		nil,
		nil,
		execCtx.ExtendedEvalContext().Tracing)
	defer sqlReciever.Release()

	execCtx.DistSQLPlanner().Run(
		ctx,
		planCtx,
		nil,
		plan,
		sqlReciever,
		&execCtx.ExtendedEvalContext().Context,
		nil,
	)

	if err := rowWriter.Err(); err != nil {
		return nil, err
	}

	if iteration == maxIterations {
		// Final iteration writes directly to KV; no SST outputs expected.
		return nil, nil
	}

	// Sort the SSTs by their range start key. Ingest requires that SSTs are
	// sorted an non-overlapping. The output of merge is not sorted because SSTs
	// are emitted as their task is completed.
	slices.SortFunc(result.SSTs, func(i, j execinfrapb.BulkMergeSpec_SST) int {
		return bytes.Compare(i.StartKey, j.StartKey)
	})

	if err := validateKeyCount(ctx, result.SSTs, expectedKeyCount, iteration, maxIterations); err != nil {
		return nil, err
	}

	return result.SSTs, nil
}

// validateKeyCount validates that the total number of keys in the output SSTs
// matches the expected count. If expectedKeyCount is 0, validation is skipped.
// Returns an error if validation fails, with detailed logging of the mismatch.
func validateKeyCount(
	ctx context.Context,
	ssts []execinfrapb.BulkMergeSpec_SST,
	expectedKeyCount uint64,
	iteration int,
	maxIterations int,
) error {
	// Skip validation if no expected count was provided.
	if expectedKeyCount == 0 {
		return nil
	}

	// Compute total key count by summing individual SST counts.
	var totalKeyCount uint64
	for _, sst := range ssts {
		totalKeyCount += sst.KeyCount
	}

	// If counts match, log success and return.
	if totalKeyCount == expectedKeyCount {
		log.Dev.Infof(ctx, "Distributed merge iteration %d/%d completed: processed %d keys across %d SSTs",
			iteration, maxIterations, totalKeyCount, len(ssts))
		return nil
	}

	// Validation failed - log detailed breakdown and return error.
	var diff uint64
	var diffType string
	if totalKeyCount < expectedKeyCount {
		diff = expectedKeyCount - totalKeyCount
		diffType = "missing"
	} else {
		diff = totalKeyCount - expectedKeyCount
		diffType = "extra"
	}

	log.Dev.Errorf(ctx, "Distributed merge key count mismatch:")
	log.Dev.Errorf(ctx, "  Expected: %d keys", expectedKeyCount)
	log.Dev.Errorf(ctx, "  Got: %d keys (%d %s)", totalKeyCount, diff, diffType)
	log.Dev.Errorf(ctx, "  Iteration: %d/%d", iteration, maxIterations)
	log.Dev.Errorf(ctx, "  Output SSTs breakdown:")
	for i, sst := range ssts {
		log.Dev.Errorf(ctx, "    SST[%d]: %d keys, span=[%s, %s), uri=%s",
			i, sst.KeyCount, sst.StartKey, sst.EndKey, sst.URI)
	}

	return errors.AssertionFailedf(
		"distributed merge validation failed at iteration %d/%d: expected %d keys but got %d (%d %s). "+
			"This indicates data loss during the merge. See logs for SST breakdown.",
		iteration, maxIterations, expectedKeyCount, totalKeyCount, diff, diffType,
	)
}

// logMergeInputs logs the input SSTs for the current merge iteration.
// All logging is opt-in via log.V(2) for detailed iteration tracking.
func logMergeInputs(
	ctx context.Context,
	ssts []execinfrapb.BulkMergeSpec_SST,
	iteration int,
	maxIterations int,
	expectedKeyCount uint64,
) {
	// All iteration logging is verbose (opt-in).
	if !log.V(2) {
		return
	}

	log.Dev.Infof(ctx, "Distributed merge iteration %d/%d starting with %d input SSTs (expecting %d keys)",
		iteration, maxIterations, len(ssts), expectedKeyCount)

	var totalInputKeys uint64
	for i, sst := range ssts {
		totalInputKeys += sst.KeyCount
		log.Dev.Infof(ctx, "  input SST[%d]: %d keys, span=[%s, %s), uri=%s",
			i, sst.KeyCount, sst.StartKey, sst.EndKey, sst.URI)
	}
	log.Dev.Infof(ctx, "  Total input keys: %d", totalInputKeys)
}

func init() {
	sql.RegisterBulkMerge(Merge)
}
