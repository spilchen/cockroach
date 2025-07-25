// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package ttljob

import (
	"context"
	"math/rand"
	"time"

	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	pbtypes "github.com/gogo/protobuf/types"
	"golang.org/x/sync/errgroup"
)

var checkpointInterval = settings.RegisterDurationSetting(
	settings.ApplicationLevel,
	"sql.ttl.checkpoint_interval",
	"the amount of time between row-level TTL checkpoint updates",
	30*time.Second,
	settings.DurationWithMinimum(1*time.Millisecond),
)

var fractionUpdateInterval = settings.RegisterDurationSetting(
	settings.ApplicationLevel,
	"sql.ttl.fraction_update_interval",
	"the amount of time between row-level TTL % complete progress updates",
	10*time.Second,
	settings.DurationWithMinimum(1*time.Millisecond),
)

type progressUpdater interface {
	// SPILLY - comments
	initProgress(jobSpanCount int64) (*jobspb.Progress, error)
	refreshProgress(
		ctx context.Context, md *jobs.JobMetadata, meta *execinfrapb.ProducerMetadata,
	) (*jobspb.Progress, error)
	cleanupProgress()
}

type legacyProgressUpdater struct {
	job *jobs.Job

	mu struct {
		syncutil.Mutex
		// lastUpdateTime is the wall time of the last job progress update.
		// Used to gate how often we persist job progress in refreshProgress.
		lastUpdateTime time.Time
		// lastSpanCount is the number of spans processed as of the last persisted update.
		lastSpanCount int64
		// updateEvery determines how many spans must be processed before we persist a new update.
		updateEvery int64
		// updateEveryDuration is the minimum time that must pass before allowing another progress update.
		updateEveryDuration time.Duration
	}
}

var _ progressUpdater = (*legacyProgressUpdater)(nil)

// initProgress initializes the RowLevelTTL job progress metadata, including
// total span count and per-processor progress entries, based on the physical plan.
// This should be called before the job starts execution.
func (t *legacyProgressUpdater) initProgress(jobSpanCount int64) (*jobspb.Progress, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// To avoid too many progress updates, especially if a lot of the spans don't
	// have expired rows, we will gate the updates to approximately every 1% of
	// spans processed, and at least 60 seconds apart with jitter. This gating is
	// done in refreshProgress.
	t.mu.updateEvery = max(1, jobSpanCount/100)
	t.mu.updateEveryDuration = 60*time.Second + time.Duration(rand.Int63n(10*1000))*time.Millisecond
	t.mu.lastUpdateTime = timeutil.Now()
	t.mu.lastSpanCount = 0

	rowLevelTTL := &jobspb.RowLevelTTLProgress{
		JobTotalSpanCount:     jobSpanCount,
		JobProcessedSpanCount: 0,
	}

	progress := &jobspb.Progress{
		Details: &jobspb.Progress_RowLevelTTL{RowLevelTTL: rowLevelTTL},
		Progress: &jobspb.Progress_FractionCompleted{
			FractionCompleted: 0,
		},
	}
	return progress, nil
}

// refreshProgress ingests per-processor metadata pushed from TTL processors
// and updates the job level progress. It recomputes total spans processed
// and rows deleted, and sets the job level fraction completed.
func (t *legacyProgressUpdater) refreshProgress(
	ctx context.Context, md *jobs.JobMetadata, meta *execinfrapb.ProducerMetadata,
) (*jobspb.Progress, error) {
	if meta.BulkProcessorProgress == nil {
		return nil, nil
	}
	var incomingProcProgress jobspb.RowLevelTTLProcessorProgress
	if err := pbtypes.UnmarshalAny(&meta.BulkProcessorProgress.ProgressDetails, &incomingProcProgress); err != nil {
		return nil, errors.Wrapf(err, "unable to unmarshal ttl progress details")
	}

	orig := md.Progress.GetRowLevelTTL()
	if orig == nil {
		return nil, errors.New("job progress does not contain RowLevelTTL details")
	}
	rowLevelTTL := protoutil.Clone(orig).(*jobspb.RowLevelTTLProgress)

	// Update or insert the incoming processor progress.
	foundMatchingProcessor := false
	for i := range rowLevelTTL.ProcessorProgresses {
		if rowLevelTTL.ProcessorProgresses[i].ProcessorID == incomingProcProgress.ProcessorID {
			rowLevelTTL.ProcessorProgresses[i] = incomingProcProgress
			foundMatchingProcessor = true
			break
		}
	}
	if !foundMatchingProcessor {
		rowLevelTTL.ProcessorProgresses = append(rowLevelTTL.ProcessorProgresses, incomingProcProgress)
	}

	// Recompute job level counters from scratch.
	rowLevelTTL.JobDeletedRowCount = 0
	rowLevelTTL.JobProcessedSpanCount = 0
	totalSpanCount := int64(0)
	for i := range rowLevelTTL.ProcessorProgresses {
		pp := &rowLevelTTL.ProcessorProgresses[i]
		rowLevelTTL.JobDeletedRowCount += pp.DeletedRowCount
		rowLevelTTL.JobProcessedSpanCount += pp.ProcessedSpanCount
		totalSpanCount += pp.TotalSpanCount
	}

	if totalSpanCount > rowLevelTTL.JobTotalSpanCount {
		return nil, errors.Errorf(
			"computed span total cannot exceed job total: computed=%d jobRecorded=%d",
			totalSpanCount, rowLevelTTL.JobTotalSpanCount)
	}

	// Avoid the update if doing this too frequently.
	t.mu.Lock()
	defer t.mu.Unlock()
	processedDelta := rowLevelTTL.JobProcessedSpanCount - t.mu.lastSpanCount
	processorComplete := incomingProcProgress.ProcessedSpanCount == incomingProcProgress.TotalSpanCount
	firstProgressForProcessor := !foundMatchingProcessor

	if !(processedDelta >= t.mu.updateEvery ||
		timeutil.Since(t.mu.lastUpdateTime) >= t.mu.updateEveryDuration ||
		processorComplete ||
		firstProgressForProcessor) {
		// SPILLY - we can skip the update because the processor will send us a
		// complete update. Not just what was seen last time.
		return nil, nil // Skip the update
	}
	t.mu.lastSpanCount = rowLevelTTL.JobProcessedSpanCount
	t.mu.lastUpdateTime = timeutil.Now()

	newProgress := &jobspb.Progress{
		Details: &jobspb.Progress_RowLevelTTL{
			RowLevelTTL: rowLevelTTL,
		},
		Progress: &jobspb.Progress_FractionCompleted{
			FractionCompleted: float32(rowLevelTTL.JobProcessedSpanCount) /
				float32(rowLevelTTL.JobTotalSpanCount),
		},
	}
	return newProgress, nil
}

func (t *legacyProgressUpdater) cleanupProgress() {
}

type checkpointProgressUpdater struct {
	job                *jobs.Job
	settings           *cluster.Settings
	clock              timeutil.TimeSource
	checkpointInterval func() time.Duration
	fractionInterval   func() time.Duration

	mu struct {
		syncutil.Mutex
		// cachedProgress holds the latest progress update from processors
		cachedProgress *jobspb.Progress
		// lastFractionUpdate tracks when we last sent a fraction-only update
		lastFractionUpdate time.Time
		// lastCheckpointUpdate tracks when we last sent a full checkpoint update
		lastCheckpointUpdate time.Time
	}

	// Goroutine management
	stopFunc func()
}

var _ progressUpdater = (*legacyProgressUpdater)(nil)

func newLegacyProgressUpdater(job *jobs.Job) *legacyProgressUpdater {
	return &legacyProgressUpdater{
		job: job,
	}
}

var _ progressUpdater = (*checkpointProgressUpdater)(nil)

func newCheckpointProgressUpdater(job *jobs.Job, sv *settings.Values) *checkpointProgressUpdater {
	return &checkpointProgressUpdater{
		job:                job,
		clock:              timeutil.DefaultTimeSource{},
		fractionInterval:   func() time.Duration { return fractionUpdateInterval.Get(sv) },
		checkpointInterval: func() time.Duration { return checkpointInterval.Get(sv) },
	}
}

func (t *checkpointProgressUpdater) initProgress(jobSpanCount int64) (*jobspb.Progress, error) {
	// SPILLY - Do we need to fetch the old job to see if we are doing a resume?
	t.mu.Lock()
	defer t.mu.Unlock()

	// Initialize progress similar to legacy updater
	rowLevelTTL := &jobspb.RowLevelTTLProgress{
		JobTotalSpanCount: jobSpanCount,
	}

	progress := &jobspb.Progress{
		Details: &jobspb.Progress_RowLevelTTL{RowLevelTTL: rowLevelTTL},
		Progress: &jobspb.Progress_FractionCompleted{
			FractionCompleted: 0,
		},
	}

	t.mu.cachedProgress = progress
	now := timeutil.Now()
	t.mu.lastFractionUpdate = now
	t.mu.lastCheckpointUpdate = now

	// Start the progress update goroutines
	t.stopFunc = t.startPeriodicUpdates(context.Background())

	return progress, nil
}

func (t *checkpointProgressUpdater) refreshProgress(
	ctx context.Context, md *jobs.JobMetadata, meta *execinfrapb.ProducerMetadata,
) (*jobspb.Progress, error) {
	if meta.BulkProcessorProgress == nil {
		return nil, nil
	}

	var incomingProcProgress jobspb.RowLevelTTLProcessorProgress
	if err := pbtypes.UnmarshalAny(&meta.BulkProcessorProgress.ProgressDetails, &incomingProcProgress); err != nil {
		return nil, errors.Wrapf(err, "unable to unmarshal ttl progress details")
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.mu.cachedProgress == nil {
		return nil, errors.AssertionFailedf("progress not initialized")
	}

	orig := t.mu.cachedProgress.GetRowLevelTTL()
	if orig == nil {
		return nil, errors.AssertionFailedf("cached progress does not contain RowLevelTTL details")
	}
	rowLevelTTL := protoutil.Clone(orig).(*jobspb.RowLevelTTLProgress)

	// Update or insert the incoming processor progress
	foundMatchingProcessor := false
	for i := range rowLevelTTL.ProcessorProgresses {
		if rowLevelTTL.ProcessorProgresses[i].ProcessorID == incomingProcProgress.ProcessorID {
			rowLevelTTL.ProcessorProgresses[i] = incomingProcProgress
			foundMatchingProcessor = true
			break
		}
	}
	if !foundMatchingProcessor {
		rowLevelTTL.ProcessorProgresses = append(rowLevelTTL.ProcessorProgresses, incomingProcProgress)
	}

	// The CompletedSpans in the progress message is the delta of number of spans
	// completed since the last progress update.
	rowLevelTTL.CompletedSpans = append(rowLevelTTL.CompletedSpans, meta.BulkProcessorProgress.CompletedSpans...)

	// Recompute job level counters from scratch
	rowLevelTTL.JobDeletedRowCount = 0
	// SPILLY - do we need to update JobProcessedSpanCount? We can refer to the completed spans I think
	rowLevelTTL.JobProcessedSpanCount = 0
	totalSpanCount := int64(0)
	for i := range rowLevelTTL.ProcessorProgresses {
		pp := &rowLevelTTL.ProcessorProgresses[i]
		rowLevelTTL.JobDeletedRowCount += pp.DeletedRowCount
		rowLevelTTL.JobProcessedSpanCount += pp.ProcessedSpanCount
		totalSpanCount += pp.TotalSpanCount
	}

	// SPILLY - maybe don't deprecate JobProcessedSpanCount since its used as a sanity check
	if rowLevelTTL.JobProcessedSpanCount != int64(len(rowLevelTTL.CompletedSpans)) {
		return nil, errors.AssertionFailedf(
			"completed spans mismatch: %d vs %d: %v",
			rowLevelTTL.JobProcessedSpanCount, len(rowLevelTTL.CompletedSpans), rowLevelTTL.CompletedSpans)
	}

	if totalSpanCount > rowLevelTTL.JobTotalSpanCount {
		return nil, errors.AssertionFailedf(
			"computed span total cannot exceed job total: computed=%d jobRecorded=%d",
			totalSpanCount, rowLevelTTL.JobTotalSpanCount)
	}

	// Update cached progress - the goroutine will handle actual persistence
	t.mu.cachedProgress = &jobspb.Progress{
		Details: &jobspb.Progress_RowLevelTTL{
			RowLevelTTL: rowLevelTTL,
		},
		Progress: &jobspb.Progress_FractionCompleted{
			FractionCompleted: float32(rowLevelTTL.JobProcessedSpanCount) /
				float32(rowLevelTTL.JobTotalSpanCount),
		},
	}

	// We denote the final progress from a producer using the Drained field. If
	// that's the set, then return the progress to have the caller update the job.
	// SPILLY- improve this comment
	if meta.BulkProcessorProgress.Drained {
		return t.mu.cachedProgress, nil
	}

	// Return nil to indicate we're not immediately persisting
	return nil, nil
}

func (t *checkpointProgressUpdater) cleanupProgress() {
	if t.stopFunc != nil {
		t.stopFunc()
	}
}

func (t *checkpointProgressUpdater) startPeriodicUpdates(ctx context.Context) (stop func()) {
	stopCh := make(chan struct{})
	runPeriodicWrite := func(
		ctx context.Context,
		write func(context.Context) error,
		interval func() time.Duration,
	) error {
		timer := t.clock.NewTimer()
		defer timer.Stop()
		for {
			timer.Reset(interval())
			select {
			case <-stopCh:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.Ch():
				if err := write(ctx); err != nil {
					log.Warningf(ctx, "could not flush progress: %v", err)
				}
			}
		}
	}

	var g errgroup.Group
	g.Go(func() error {
		return runPeriodicWrite(
			ctx, t.flushFractionUpdate, t.fractionInterval)
	})
	g.Go(func() error {
		return runPeriodicWrite(
			ctx, t.flushCheckpointUpdate, t.checkpointInterval)
	})
	
	toClose := stopCh // make the returned function idempotent
	return func() {
		if toClose != nil {
			close(toClose)
			toClose = nil
		}
		if err := g.Wait(); err != nil {
			log.Warningf(ctx, "waiting for progress flushing goroutines: %v", err)
		}
	}
}

func (t *checkpointProgressUpdater) flushFractionUpdate(ctx context.Context) error {
	t.mu.Lock()
	cachedProgress := t.mu.cachedProgress
	t.mu.Unlock()

	if cachedProgress == nil {
		return nil
	}

	return t.job.NoTxn().Update(ctx, func(_ isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
		if md.Progress != nil {
			newProgress := protoutil.Clone(md.Progress).(*jobspb.Progress)
			newProgress.Progress = cachedProgress.Progress
			ju.UpdateProgress(newProgress)
		}
		return nil
	})
}

func (t *checkpointProgressUpdater) flushCheckpointUpdate(ctx context.Context) error {
	t.mu.Lock()
	cachedProgress := t.mu.cachedProgress
	t.mu.Unlock()

	if cachedProgress == nil {
		return nil
	}

	return t.job.NoTxn().Update(ctx, func(_ isql.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
		// Copy our cached progress details into the latest job metadata
		newProgress := protoutil.Clone(md.Progress).(*jobspb.Progress)
		if cachedTTL := cachedProgress.GetRowLevelTTL(); cachedTTL != nil {
			newProgress.Details = &jobspb.Progress_RowLevelTTL{
				RowLevelTTL: protoutil.Clone(cachedTTL).(*jobspb.RowLevelTTLProgress),
			}
			newProgress.Progress = cachedProgress.Progress
		}
		ju.UpdateProgress(newProgress)
		return nil
	})
}
