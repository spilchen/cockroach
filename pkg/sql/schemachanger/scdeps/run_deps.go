// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package scdeps

import (
	"context"
	"fmt"
	"math"

	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/bulkutil"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descs"
	"github.com/cockroachdb/cockroach/pkg/sql/isql"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scexec"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scexec/backfiller"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scrun"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/stats"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

type ExternalStorageFactory = cloud.ExternalStorageFromURIFactory

// NewJobRunDependencies returns an scrun.JobRunDependencies implementation built from the
// given arguments.
func NewJobRunDependencies(
	collectionFactory *descs.CollectionFactory,
	db descs.DB,
	backfiller scexec.Backfiller,
	spanSplitter scexec.IndexSpanSplitter,
	merger scexec.Merger,
	rangeCounter backfiller.RangeCounter,
	eventLoggerFactory func(isql.Txn) scrun.EventLogger,
	jobRegistry *jobs.Registry,
	job *jobs.Job,
	codec keys.SQLCodec,
	settings *cluster.Settings,
	indexValidator scexec.Validator,
	metadataUpdaterFactory MetadataUpdaterFactory,
	statsRefresher scexec.StatsRefresher,
	tableStatsCache *stats.TableStatisticsCache,
	testingKnobs *scexec.TestingKnobs,
	statements []string,
	sessionData *sessiondata.SessionData,
	kvTrace bool,
	externalStorageFactory ExternalStorageFactory,
) scrun.JobRunDependencies {
	return &jobExecutionDeps{
		collectionFactory:       collectionFactory,
		db:                      db,
		backfiller:              backfiller,
		spanSplitter:            spanSplitter,
		merger:                  merger,
		rangeCounter:            rangeCounter,
		eventLoggerFactory:      eventLoggerFactory,
		jobRegistry:             jobRegistry,
		job:                     job,
		codec:                   codec,
		settings:                settings,
		testingKnobs:            testingKnobs,
		statements:              statements,
		indexValidator:          indexValidator,
		commentUpdaterFactory:   metadataUpdaterFactory,
		sessionData:             sessionData,
		kvTrace:                 kvTrace,
		statsRefresher:          statsRefresher,
		tableStatsCache:         tableStatsCache,
		externalStorageFactory:  externalStorageFactory,
	}
}

type jobExecutionDeps struct {
	collectionFactory       *descs.CollectionFactory
	db                      descs.DB
	statsRefresher          scexec.StatsRefresher
	tableStatsCache         *stats.TableStatisticsCache
	backfiller              scexec.Backfiller
	spanSplitter            scexec.IndexSpanSplitter
	merger                  scexec.Merger
	commentUpdaterFactory   MetadataUpdaterFactory
	rangeCounter            backfiller.RangeCounter
	eventLoggerFactory      func(isql.Txn) scrun.EventLogger
	jobRegistry             *jobs.Registry
	job                     *jobs.Job
	kvTrace                 bool
	externalStorageFactory  ExternalStorageFactory

	indexValidator scexec.Validator

	codec        keys.SQLCodec
	settings     *cluster.Settings
	testingKnobs *scexec.TestingKnobs
	statements   []string
	sessionData  *sessiondata.SessionData

	mu struct {
		syncutil.Mutex
		explainOutput string
	}
}

var _ scrun.JobRunDependencies = (*jobExecutionDeps)(nil)

// ClusterSettings implements the scrun.JobRunDependencies interface.
func (d *jobExecutionDeps) ClusterSettings() *cluster.Settings {
	return d.settings
}

// WithTxnInJob implements the scrun.JobRunDependencies interface.
func (d *jobExecutionDeps) WithTxnInJob(ctx context.Context, fn scrun.JobTxnFunc) error {
	var createdJobs []jobspb.JobID
	var tableStatsToRefresh []descpb.ID
	err := d.db.DescsTxn(ctx, func(
		ctx context.Context, txn descs.Txn,
	) error {
		pl := d.job.Payload()
		// Create cleanup callback for phase transitions.
		onPersistedPhaseChange := func(
			ctx context.Context, jobID jobspb.JobID, tableID descpb.ID, oldPhase, newPhase int32,
		) error {
			log.Dev.Infof(ctx, "triggering SST cleanup for job %d, table %d after phase %d→%d transition",
				jobID, tableID, oldPhase, newPhase)

			// Determine which subdirectory to clean up.
			// newPhase is the iteration that just completed:
			// - If newPhase=1, cleanup map/ (map phase was input to iteration 1)
			// - If newPhase=2, cleanup merge/iter-1/ (iteration 1 was input to iteration 2)
			subdirectory := bulkutil.NewDistMergePaths(jobID).InputSubdir(int(newPhase))

			// Get storage prefixes from current backfill progress.
			var storagePrefixes []string
			for _, bp := range pl.GetNewSchemaChange().BackfillProgress {
				if bp.TableID == tableID {
					storagePrefixes = bp.SSTStoragePrefixes
					break
				}
			}

			if len(storagePrefixes) == 0 {
				log.Dev.Infof(ctx, "no storage prefixes found, skipping cleanup")
				return nil
			}

			// Create cleaner and perform cleanup.
			cleaner := bulkutil.NewBulkJobCleaner(
				d.externalStorageFactory,
				username.NodeUserName(),
			)
			defer func() {
				if err := cleaner.Close(); err != nil {
					log.Ops.Warningf(ctx, "error closing bulk job cleaner: %v", err)
				}
			}()

			if err := cleaner.CleanupJobSubdirectory(ctx, jobID, storagePrefixes, subdirectory); err != nil {
				return errors.Wrapf(err, "cleaning up subdirectory %s", subdirectory)
			}

			log.Dev.Infof(ctx, "successfully cleaned up SSTs in %s", subdirectory)
			return nil
		}

		ed := &execDeps{
			txnDeps: txnDeps{
				txn:                txn,
				codec:              d.codec,
				descsCollection:    txn.Descriptors(),
				jobRegistry:        d.jobRegistry,
				validator:          d.indexValidator,
				statsRefresher:     d.statsRefresher,
				tableStatsCache:    d.tableStatsCache,
				schemaChangerJobID: d.job.ID(),
				schemaChangerJob:   d.job,
				kvTrace:            d.kvTrace,
				settings:           d.settings,
			},
			backfiller:   d.backfiller,
			merger:       d.merger,
			spanSplitter: d.spanSplitter,
			backfillerTracker: backfiller.NewTrackerWithCleanup(
				d.codec,
				d.rangeCounter,
				d.job,
				d.db,
				pl.GetNewSchemaChange().BackfillProgress,
				pl.GetNewSchemaChange().MergeProgress,
				onPersistedPhaseChange,
			),
			periodicProgressFlusher: backfiller.NewPeriodicProgressFlusherForIndexBackfill(d.settings),
			statements:              d.statements,
			user:                    pl.UsernameProto.Decode(),
			clock:                   NewConstantClock(timeutil.FromUnixMicros(pl.StartedMicros)),
			metadataUpdater:         d.commentUpdaterFactory(ctx, txn.Descriptors(), txn),
			sessionData:             d.sessionData,
			testingKnobs:            d.testingKnobs,
		}
		if err := fn(ctx, ed, d.eventLoggerFactory(txn)); err != nil {
			return err
		}
		createdJobs = ed.CreatedJobs()
		tableStatsToRefresh = ed.tableStatsToRefresh
		return nil
	})
	if err != nil {
		return err
	}
	if len(createdJobs) > 0 {
		d.jobRegistry.NotifyToResume(ctx, createdJobs...)
	}
	if len(tableStatsToRefresh) > 0 {
		err := d.db.DescsTxn(ctx, func(
			ctx context.Context, txn descs.Txn,
		) error {
			for _, id := range tableStatsToRefresh {
				tbl, err := txn.Descriptors().ByIDWithLeased(txn.KV()).WithoutNonPublic().Get().Table(ctx, id)
				if err != nil {
					return err
				}
				d.statsRefresher.NotifyMutation(ctx, tbl, math.MaxInt32)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// GetExplain returns the previously saved explain output.
func (d *jobExecutionDeps) GetExplain() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mu.explainOutput
}

// SetExplain is a setter to store explain output for later retrieval.
//
// The parameters are meant to be the return variables from Plan.ExplainCompact().
// We do error checking of ExplainCompact() here as we don't want that to impact
// the schema change. If there is an error, we will record the error in the
// explain output and then ignore it.
//
// We could have called ExplainCompact() right in the getter and not even store
// the explain output. But that would require saving the Plan somewhere. It
// seemed easier to just save the explain output.
func (d *jobExecutionDeps) SetExplain(op string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		// The explain output is just for debugging and shouldn't impact the actual
		// schema change. We will log the error message, then ignore the error.
		d.mu.explainOutput = fmt.Sprintf("failed to get explain output: %s", err)
		return
	}
	// The explain output is taken We opted store the explain output here, rather than
	d.mu.explainOutput = op
}
