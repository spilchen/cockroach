// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/physicalplan"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/errors"
	"github.com/gogo/protobuf/proto"
)

func NewBulkMergePlan(
	ctx context.Context,
	execCtx sql.JobExecContext,
	ssts []execinfrapb.BulkMergeSpec_SST,
	spans []roachpb.Span,
	outputURI func(sqlInstance base.SQLInstanceID) string,
) (*sql.PhysicalPlan, *sql.PlanningCtx, error) {
	planCtx, sqlInstanceIDs, err := execCtx.DistSQLPlanner().SetupAllNodesPlanning(
		ctx, execCtx.ExtendedEvalContext(), execCtx.ExecCfg(),
	)
	if err != nil {
		return nil, nil, err
	}

	plan := planCtx.NewPhysicalPlan()
	coordinator := sqlInstanceIDs[:1]

	routingKeys := make([][]byte, len(sqlInstanceIDs))
	for i, sqlInstanceID := range sqlInstanceIDs {
		routingKeys[i] = routingKeyForSQLInstance(sqlInstanceID)
	}

	router, err := makeKeyRouter(routingKeys)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to build merge router")
	}

	loopbackID := plan.AddProcessor(physicalplan.Processor{
		SQLInstanceID: coordinator[0],
		Spec: execinfrapb.ProcessorSpec{
			Core: execinfrapb.ProcessorCoreUnion{MergeLoopback: &execinfrapb.MergeLoopbackSpec{}},
			Post: execinfrapb.PostProcessSpec{},
			Output: []execinfrapb.OutputRouterSpec{{
				Type:            execinfrapb.OutputRouterSpec_BY_RANGE,
				RangeRouterSpec: router,
			}},
			StageID:     plan.NewStageOnNodes(coordinator),
			ResultTypes: mergeLoopbackOutputTypes,
		},
	})

	mergeStage := plan.NewStageOnNodes(sqlInstanceIDs)
	for streamID, sqlInstanceID := range sqlInstanceIDs {
		outputPath := outputURI(sqlInstanceID)
		storeConf, err := cloud.ExternalStorageConfFromURI(outputPath, execCtx.User())
		if err != nil {
			return nil, nil, err
		}
		storeConfCopy := storeConf
		pIdx := plan.AddProcessor(physicalplan.Processor{
			SQLInstanceID: sqlInstanceID,
			Spec: execinfrapb.ProcessorSpec{
				Input: []execinfrapb.InputSyncSpec{{
					ColumnTypes: mergeLoopbackOutputTypes,
				}},
				Core: execinfrapb.ProcessorCoreUnion{
					BulkMerge: &execinfrapb.BulkMergeSpec{
						Ssts:        ssts,
						Spans:       spans,
						OutputUri:   outputPath,
						OutputStore: &storeConfCopy,
					},
				},
				Post: execinfrapb.PostProcessSpec{},
				Output: []execinfrapb.OutputRouterSpec{{
					Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
				}},
				StageID:     mergeStage,
				ResultTypes: bulkMergeProcessorOutputTypes,
			},
		})
		plan.Streams = append(plan.Streams, physicalplan.Stream{
			SourceProcessor:  loopbackID,
			SourceRouterSlot: streamID,
			DestProcessor:    pIdx,
			DestInput:        0,
		})
		plan.ResultRouters = append(plan.ResultRouters, pIdx)
	}

	plan.AddSingleGroupStage(ctx, coordinator[0], execinfrapb.ProcessorCoreUnion{
		MergeCoordinator: &execinfrapb.MergeCoordinatorSpec{
			TaskCount:            proto.Int64(int64(len(spans))),
			WorkerSqlInstanceIds: routingKeys,
		},
	}, execinfrapb.PostProcessSpec{}, MergeCoordinatorOutputTypes, nil)

	plan.PlanToStreamColMap = []int{0}
	sql.FinalizePlan(ctx, planCtx, plan)

	return plan, planCtx, nil
}

func routingKeyForSQLInstance(sqlInstanceID base.SQLInstanceID) roachpb.Key {
	return roachpb.Key(fmt.Sprintf("node%d", sqlInstanceID))
}

func routingSpanForSQLInstance(key []byte) ([]byte, []byte, error) {
	var alloc tree.DatumAlloc
	startDatum, err := rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(key)))
	if err != nil {
		return nil, nil, err
	}
	endDatum, err := rowenc.DatumToEncDatum(types.Bytes, tree.NewDBytes(tree.DBytes(roachpb.Key(key).Next())))
	if err != nil {
		return nil, nil, err
	}

	startBytes, endBytes := make([]byte, 0), make([]byte, 0)
	startBytes, err = startDatum.Encode(types.Bytes, &alloc, catenumpb.DatumEncoding_ASCENDING_KEY, startBytes)
	if err != nil {
		return nil, nil, err
	}
	endBytes, err = endDatum.Encode(types.Bytes, &alloc, catenumpb.DatumEncoding_ASCENDING_KEY, endBytes)
	if err != nil {
		return nil, nil, err
	}
	return startBytes, endBytes, nil
}

func makeKeyRouter(keys [][]byte) (execinfrapb.OutputRouterSpec_RangeRouterSpec, error) {
	var zero execinfrapb.OutputRouterSpec_RangeRouterSpec
	defaultStream := int32(0)
	rangeRouterSpec := execinfrapb.OutputRouterSpec_RangeRouterSpec{
		DefaultDest: &defaultStream,
		Encodings: []execinfrapb.OutputRouterSpec_RangeRouterSpec_ColumnEncoding{{
			Column:   0,
			Encoding: catenumpb.DatumEncoding_ASCENDING_KEY,
		}},
	}
	for stream, key := range keys {
		startBytes, endBytes, err := routingSpanForSQLInstance(key)
		if err != nil {
			return zero, err
		}
		rangeRouterSpec.Spans = append(rangeRouterSpec.Spans, execinfrapb.OutputRouterSpec_RangeRouterSpec_Span{
			Start:  startBytes,
			End:    endBytes,
			Stream: int32(stream),
		})
	}
	slices.SortFunc(rangeRouterSpec.Spans, func(a, b execinfrapb.OutputRouterSpec_RangeRouterSpec_Span) int {
		return bytes.Compare(a.Start, b.Start)
	})
	return rangeRouterSpec, nil
}
