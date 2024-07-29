// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package backfill

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

func TestReduceBatchSizeWhenAutoRetryLimitExhausted(t *testing.T) {
	var initialBatchSize = batchLimit{nextRows: 1000, minRows: 1, nextBytes: 1000000, minBytes: 1024}
	for _, tc := range []struct {
		name             string
		kvErrs           []error
		expectedFinalErr error
		initialBatchSize batchLimit
		finalBatchSize   batchLimit
	}{
		{
			name:             "no errors",
			kvErrs:           []error{nil},
			expectedFinalErr: nil,
			initialBatchSize: initialBatchSize,
			finalBatchSize:   initialBatchSize,
		},
		{
			name:             "retry once",
			kvErrs:           []error{errors.Mark(errors.New("retry"), kv.ErrAutoRetryLimitExhausted), nil},
			expectedFinalErr: nil,
			initialBatchSize: initialBatchSize,
			finalBatchSize:   *(initialBatchSize.copy().reduceForRetry()),
		},
		{
			name:             "retry max times",
			kvErrs:           []error{errors.Mark(errors.New("retry"), kv.ErrAutoRetryLimitExhausted)},
			expectedFinalErr: errors.Mark(errors.New("retry"), kv.ErrAutoRetryLimitExhausted),
			initialBatchSize: initialBatchSize,
			finalBatchSize:   batchLimit{nextRows: initialBatchSize.minRows, nextBytes: initialBatchSize.minBytes},
		},
		{
			name:             "don't retry if not a auto retry limit exhausted error",
			kvErrs:           []error{errors.Mark(errors.New("retry"), kv.ErrAutoRetryLimitExhausted), errors.New("not a retry")},
			expectedFinalErr: errors.New("not a retry"),
			initialBatchSize: initialBatchSize,
			finalBatchSize:   *(initialBatchSize.copy().reduceForRetry()), // We do one retry before bailing out.
		},
	} {
		ctx := context.Background()
		t.Run(tc.name, func(t *testing.T) {
			i := 0
			limit := tc.initialBatchSize
			err := retryWithReducedBatchWhenAutoRetryLimitExceeded(ctx, &limit, func(ctx context.Context, limit *batchLimit) error {
				// Return the next error in the list, or the last one if we run out.
				if i >= len(tc.kvErrs) {
					i = len(tc.kvErrs) - 1
				}
				err := tc.kvErrs[i]
				i++
				return err
			})
			if tc.expectedFinalErr == nil {
				require.NoError(t, err)
			} else {
				require.True(t, kv.IsAutoRetryLimitExhaustedError(tc.expectedFinalErr) == kv.IsAutoRetryLimitExhaustedError(err))
			}
			require.Equal(t, limit.nextBytes, tc.finalBatchSize.nextBytes,
				"bytes %d != %d", limit.nextBytes, tc.finalBatchSize.nextBytes)
			require.Equal(t, limit.nextRows, tc.finalBatchSize.nextRows,
				"rows %d != %d", limit.nextRows, tc.finalBatchSize.nextRows)
		})
	}
}
