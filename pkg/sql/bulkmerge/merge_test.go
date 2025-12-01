// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package bulkmerge

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/cloud/nodelocal"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/bulksst"
	"github.com/cockroachdb/cockroach/pkg/sql/bulkutil"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/stretchr/testify/require"
)

func TestDistributedMergeOneNode(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	dir, cleanup := testutils.TempDir(t)
	defer cleanup()

	cleanupProvider := nodelocal.ReplaceNodeLocalForTesting(dir)
	t.Cleanup(cleanupProvider)

	srv := serverutils.StartServerOnly(t, base.TestServerArgs{
		ExternalIODir:     dir,
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
	})
	defer srv.Stopper().Stop(ctx)

	testMergeProcessors(t, srv.ApplicationLayer())
}

func TestDistributedMergeThreeNodes(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	dir, cleanup := testutils.TempDir(t)
	defer cleanup()

	cleanupProvider := nodelocal.ReplaceNodeLocalForTesting(dir)
	t.Cleanup(cleanupProvider)

	args := base.TestClusterArgs{ServerArgsPerNode: map[int]base.TestServerArgs{}}
	args.ServerArgs.DefaultTestTenant = base.TestIsSpecificToStorageLayerAndNeedsASystemTenant
	for i := 0; i < 3; i++ {
		args.ServerArgsPerNode[i] = base.TestServerArgs{
			ExternalIODir:     fmt.Sprintf("%s/node-%d", dir, i),
			DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
		}
	}

	tc := serverutils.StartCluster(t, 3, args)
	defer tc.Stopper().Stop(ctx)

	nodeIdx := rand.Intn(3)
	testMergeProcessors(t, tc.Server(nodeIdx))
}

func TestBulkMergeSpecCarriesOutputConfig(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	dir, cleanup := testutils.TempDir(t)
	defer cleanup()

	srv := serverutils.StartServerOnly(t, base.TestServerArgs{
		ExternalIODir:     dir,
		DefaultTestTenant: base.TestIsSpecificToStorageLayerAndNeedsASystemTenant,
	})
	defer srv.Stopper().Stop(ctx)

	execCfg := srv.ApplicationLayer().ExecutorConfig().(sql.ExecutorConfig)
	tsa := newTestServerAllocator(t, ctx, execCfg)

	jobExecCtx, close := sql.MakeJobExecContext(ctx, "test", username.RootUserName(), &sql.MemoryMetrics{}, &execCfg)
	defer close()

	bulksst.BatchSize.Override(ctx, &srv.ClusterSettings().SV, 1)
	fileAllocator := bulksst.NewExternalFileAllocator(tsa.es, tsa.mapPrefix, execCfg.Clock)
	writer := bulksst.NewUnsortedSSTBatcher(srv.ClusterSettings(), fileAllocator)
	_ = writeSSTs(t, ctx, writer, 5)
	ssts := importToMerge(fileAllocator.GetFileList())

	plan, _, err := newBulkMergePlan(ctx, jobExecCtx, ssts, []roachpb.Span{{Key: nil, EndKey: roachpb.KeyMax}}, func(instanceID base.SQLInstanceID) string {
		return fmt.Sprintf("userfile://defaultdb.public.userfile_table/%s/spec-out-%d", tsa.baseSubdir, instanceID)
	})
	require.NoError(t, err)
	defer plan.Release()

	found := 0
	for _, proc := range plan.Processors {
		if spec := proc.Spec.Core.BulkMerge; spec != nil {
			require.NotNil(t, spec.OutputStore)
			found++
		}
	}
	require.NotZero(t, found)

}

func randIntSlice(n int) []int {
	ls := make([]int, n)
	for i := range ls {
		ls[i] = i
	}
	for i := range ls {
		r := rand.Intn(n)
		ls[i], ls[r] = ls[r], ls[i]
	}
	return ls
}

func encodeKey(strKey string) storage.MVCCKey {
	key := storage.MVCCKey{Key: []byte(strKey)}
	return storage.MVCCKey{
		Timestamp: hlc.Timestamp{WallTime: 1},
		Key:       storage.EncodeMVCCKeyToBuf(nil, key),
	}
}

func writeSSTs(t *testing.T, ctx context.Context, w *bulksst.Writer, n int) []int {
	t.Helper()
	ls := randIntSlice(n)
	for idx, i := range ls {
		k := encodeKey(fmt.Sprintf("key-%d", idx))
		v := []byte(fmt.Sprintf("value-%d", i))
		require.NoError(t, w.AddMVCCKey(ctx, k, v))
	}
	require.NoError(t, w.Flush(ctx))
	return ls
}

type testServerAllocator struct {
	es         cloud.ExternalStorage
	mapPrefix  string
	baseSubdir string
}

func newTestServerAllocator(
	t *testing.T, ctx context.Context, execCfg sql.ExecutorConfig,
) *testServerAllocator {
	baseSubdir := fmt.Sprintf("merge/%d", rand.Int())
	mapPrefix := fmt.Sprintf("userfile://defaultdb.public.userfile_table/%s/input", baseSubdir)
	store, err := execCfg.DistSQLSrv.ExternalStorageFromURI(ctx, mapPrefix, username.RootUserName())
	if err != nil {
		if strings.Contains(err.Error(), "unsupported storage scheme") {
			skip.IgnoreLintf(t, "external storage unavailable in this environment: %v", err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &testServerAllocator{es: store, mapPrefix: mapPrefix, baseSubdir: baseSubdir}
}

func importToMerge(mapFiles *bulksst.SSTFiles) []execinfrapb.BulkMergeSpec_SST {
	ssts := make([]execinfrapb.BulkMergeSpec_SST, 0, len(mapFiles.SST))
	for _, file := range mapFiles.SST {
		ssts = append(ssts, execinfrapb.BulkMergeSpec_SST{
			Uri:      file.URI,
			StartKey: []byte(file.StartKey),
			EndKey:   []byte(file.EndKey),
		})
	}
	return ssts
}

func testMergeProcessors(t *testing.T, s serverutils.ApplicationLayerInterface) {
	ctx := context.Background()
	execCfg := s.ExecutorConfig().(sql.ExecutorConfig)
	tsa := newTestServerAllocator(t, ctx, execCfg)

	jobExecCtx, cleanup := sql.MakeJobExecContext(ctx, "test", username.RootUserName(), &sql.MemoryMetrics{}, &execCfg)
	defer cleanup()

	bulksst.BatchSize.Override(ctx, &s.ClusterSettings().SV, 1)
	targetFileSize.Override(ctx, &s.ClusterSettings().SV, 30)

	fileAllocator := bulksst.NewExternalFileAllocator(tsa.es, tsa.mapPrefix, execCfg.Clock)
	writer := bulksst.NewUnsortedSSTBatcher(s.ClusterSettings(), fileAllocator)
	ls := writeSSTs(t, ctx, writer, 13)
	ssts := importToMerge(fileAllocator.GetFileList())

	plan, planCtx, err := newBulkMergePlan(ctx, jobExecCtx, ssts, []roachpb.Span{{Key: nil, EndKey: roachpb.KeyMax}}, func(instanceID base.SQLInstanceID) string {
		return fmt.Sprintf("userfile://defaultdb.public.userfile_table/%s/out-%d", tsa.baseSubdir, instanceID)
	})
	require.NoError(t, err)
	defer plan.Release()

	require.Equal(t, mergeCoordinatorOutputTypes, plan.GetResultTypes())

	var result execinfrapb.BulkMergeSpec_Output
	rowWriter := sql.NewCallbackResultWriter(func(ctx context.Context, row tree.Datums) error {
		return protoutil.Unmarshal([]byte(*row[0].(*tree.DBytes)), &result)
	})

	receiver := sql.MakeDistSQLReceiver(
		ctx,
		rowWriter,
		tree.Rows,
		execCfg.RangeDescriptorCache,
		nil,
		nil,
		jobExecCtx.ExtendedEvalContext().Tracing,
	)
	defer receiver.Release()

	evalCtxCopy := jobExecCtx.ExtendedEvalContext().Copy()
	jobExecCtx.DistSQLPlanner().Run(
		ctx,
		planCtx,
		nil,
		plan,
		receiver,
		evalCtxCopy,
		nil,
	)

	require.NoError(t, rowWriter.Err())
	verifySSTs(t, ctx, execCfg, result.Ssts, ls, 30)
}

// verifySSTs validates the merged SST metadata and contents.
func verifySSTs(
	t *testing.T,
	ctx context.Context,
	execCfg sql.ExecutorConfig,
	ssts []execinfrapb.BulkMergeSpec_SST,
	shuffleOrder []int,
	maxSize int64,
) {
	t.Helper()
	require.True(t, len(shuffleOrder) > len(ssts), "expected merged SSTs to cover multiple input files")

	mux := bulkutil.NewExternalStorageMux(execCfg.DistSQLSrv.ExternalStorageFromURI, username.RootUserName())
	defer func() { _ = mux.Close() }()

	var prevEnd roachpb.Key
	for i, sst := range ssts {
		require.Regexp(t, "userfile://.*/merge/.*/out-[0-9]+/[0-9]+-[0-9]+.sst", sst.Uri)
		file, err := mux.StoreFile(ctx, sst.Uri)
		require.NoError(t, err)

		iter, err := storageccl.ExternalSSTReader(ctx, []storageccl.StoreFile{file}, nil, storage.IterOptions{
			LowerBound: sst.StartKey,
			UpperBound: sst.EndKey,
		})
		require.NoError(t, err)

		if i > 0 {
			require.True(t, bytes.Compare(prevEnd, sst.StartKey) <= 0,
				"SST %d start key %q overlaps previous end %q", i, sst.StartKey, prevEnd)
		}
		prevEnd = sst.EndKey

		var sstSize int64
		for iter.SeekGE(storage.MVCCKey{Key: sst.StartKey}); ; iter.NextKey() {
			ok, err := iter.Valid()
			require.NoError(t, err)
			if !ok {
				break
			}
			key := iter.UnsafeKey()
			val, err := iter.UnsafeValue()
			require.NoError(t, err)

			require.True(t, bytes.Compare(key.Key, sst.StartKey) >= 0)
			require.True(t, bytes.Compare(key.Key, sst.EndKey) <= 0)

			decoded, err := storage.DecodeMVCCKey(key.Key)
			require.NoError(t, err)
			idx, err := strconv.Atoi(string(bytes.TrimPrefix(decoded.Key, []byte("key-"))))
			require.NoError(t, err)
			expected := []byte(fmt.Sprintf("value-%d", shuffleOrder[idx]))
			require.Equal(t, expected, val)

			sstSize += int64(len(key.Key) + len(val))
		}
		iter.Close()
		require.True(t, sstSize <= maxSize, "SST %d too large: %d", i, sstSize)
	}
}
