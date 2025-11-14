# Partition TTL – Prototype Design

## 1. Goal

Partition-based TTL provides a fast, predictable way to enforce data retention using **time partitions** instead of per-row deletes.

Users declare which column defines expiration, how long data is retained, and the partition granularity.  
The system automatically creates new partitions ahead of time, drops expired ones, and runs a **hybrid TTL cleaner** for secondary indexes that aren’t aligned with the TTL column.

Unlike traditional row-level TTL, this model:
- Avoids large scans over live data.
- Removes expired data via metadata operations (`DROP PARTITION`).
- Relies entirely on CockroachDB’s **admission control** for pacing — no governor needed.

---

## 2. SQL Interface

### Enabling Partition TTL on an Existing Table

```sql
ALTER TABLE t
SET (
  ttl_mode        = 'partition',
  ttl_column      = 'ts',
  ttl_retention   = '30d',
  ttl_granularity = '1d',
  ttl_lookahead   = '2d'
);
```

### Enabling Partition TTL at Create Time

```sql
CREATE TABLE events (
  ts TIMESTAMPTZ NOT NULL,
  user_id UUID NOT NULL,
  payload STRING,
  PRIMARY KEY (ts, user_id)
) WITH (
  ttl_mode        = 'partition',
  ttl_column      = 'ts',
  ttl_retention   = '30d',
  ttl_granularity = '1d',
  ttl_lookahead   = '2d'
);
```

#### Create-Time Behavior
- Validation identical to `ALTER TABLE … SET (…)`.
- Descriptor initialized with `PartitionTTLConfig { column, retention, granularity, lookahead }` and `ttl_mode='partition'`.
- Table descriptors may start with an **empty partitioning clause**; the maintenance job is responsible for seeding and maintaining all partitions once TTL is enabled.
- **Bootstrap partitions:**
  - Create partitions covering `[now, now + ttl_lookahead]` for ingestion safety.
  - Older partitions up to `now - ttl_retention` are created on the scheduler's first run.
- **Scheduling:**
  - The `partition_ttl_maintenance` job maintains partitions covering `[now - retention, now + lookahead]`.

#### Later DDL Interactions
- Adding inbound FKs is blocked.
- Adding secondary indexes later is supported; hybrid cleanup detects them dynamically.
- Switching TTL modes requires disabling the current one first.

### Validation Rules (common to ALTER and CREATE)
- `ttl_column` must exist and be of timestamp type.
- `ttl_granularity`, `ttl_lookahead` > 0; default lookahead = granularity.
- Granularity must be at least **10 seconds** (sub-day) or any whole-day/month interval to avoid creating an excessive number of partitions.
- Table cannot have inbound foreign keys.
- All secondary indexes are handled dynamically by the hybrid cleaner.

### Introspection
- `SHOW CREATE TABLE` includes TTL settings.
- `SHOW PARTITIONS FROM TABLE t` lists partitions.
- `crdb_internal.partition_ttl_status` exposes scheduler and cleanup info.

---

## 3. Lifecycle and Behavior

### Dynamic Partition Maintenance
Automatically maintain partitions covering:
```
[now - ttl_retention, now + ttl_lookahead]
```
At each run:
1. Compute window (`start`, `end`).
2. Drop partitions with `upper_bound < start`.
3. Create new partitions every `ttl_granularity` until coverage ≥ `end`.

If ingestion ever hits “key outside partition range,” a future improvement may trigger the scheduler automatically.

#### Manual Partition Mode
- Cluster setting `sql.ttl.partition.auto_add_partitions.enabled` (default `true`) controls whether the maintenance job creates forward partitions.
- When disabled, the job **only drops expired partitions** and still kicks off hybrid cleanup, letting operators manage future partitions manually.
- The expected window size is enforced by trimming the oldest partitions when more exist than the retention+lookahead window permits.

### Lookahead and Ingestion Safety
- Lookahead ensures partitions exist for slightly future timestamps.  
- Default lookahead = granularity.  
- Scheduler cadence ≤ granularity/2.

### Space Reclamation
`DROP PARTITION` is metadata-only. Physical KV data is freed by the range GC queue after `gc.ttlseconds`.

**Future Improvement:** automatically shorten `gc.ttlseconds` for dropped partitions.

### Admission Control
All partition TTL work runs as **elastic** (low-priority) jobs under admission control:
- Partition drops = metadata DDL.
- Hybrid cleanup = small batches of deletes on cold key ranges.

---

## 4. Components

### 4.1 Descriptor and Validation

Extend `TableDescriptor`:

```go
message PartitionTTLConfig {
  string  ColumnName   = 1;
  string  Retention    = 2;
  string  Granularity  = 3;
  string  Lookahead    = 4;
}
```

#### DDL Processing (Expanded)
1. **Column and Type Validation**
   - Verify `ttl_column` exists and is `TIMESTAMP/TIMESTAMPTZ NOT NULL`.  
   - If not part of PK, record that for planner decisions.
2. **Duration and Parameter Validation**
   - Parse and normalize durations for retention, granularity, and lookahead.
   - Enforce relationships (`granularity ≤ retention`, `lookahead ≥ granularity / 2`).
3. **Foreign-Key Safety Check**
   - Search for inbound FKs and reject if any found.
4. **Descriptor Update**
   - Attach `PartitionTTLConfig`, mark table as `ttl_mode='partition'`, persist via schema changer.
5. **No Stored Index Classification**
   - The hybrid cleaner determines affected indexes dynamically at runtime.

#### Future Work for DDL Layer
- **Cascade-Aware Partition Drops**: allow automatic dropping of aligned child partitions.
- **Schema-Change Compatibility**: revalidate TTL config after PK or column changes.

### 4.2 Partition Planner and Scheduler

`PlanPartitions(desc, now) -> {toCreate[], toDrop[]}`

1. Compute `start = now - retention`, `end = now + lookahead`.
2. Drop expired partitions, add new ones.
3. Scheduler job (`partition_ttl_maintenance`) runs hourly or per granularity/2.
4. For each dropped partition, trigger hybrid cleanup.

### 4.3 Hybrid TTL Cleaner

#### Purpose
Remove orphaned secondary index entries after partition drops.

#### Runtime Detection of Affected Indexes
At runtime, when a partition is dropped:
1. Read the latest table descriptor.  
2. For each secondary index:
   - Check if the TTL column is the **leftmost key** and partitioning matches the table.
   - If aligned → skip.  
   - Otherwise → schedule a cleanup job for that index.

#### Job Execution
The hybrid cleaner builds directly on the existing **row-level TTL job processor**, adding a new lightweight mode flag that skips the normal expiration filtering and select phase.

When invoked, the existing TTL job infrastructure:
- Receives explicit partition spans and target index IDs from the partition scheduler.
- Switches to **delete-only** mode (no historical read or expiration evaluation).
- Uses the same batching, progress tracking, retries, and admission-control machinery already implemented in the row-level TTL processor.

This approach reuses mature logic for distribution, monitoring, and resumability while avoiding a second implementation path. Hooks in the TTL job’s planner and executor determine whether to run in standard or hybrid-cleaner mode.

##### SQL equivalent
```sql
DELETE FROM t@idx WHERE ts >= $start AND ts < $end;
```
The implementation uses the row-level TTL processor in delete-only mode, scanning index spans for `[partition_start, partition_end)`, issuing batched deletes, running under elastic admission control, and tracking progress for resumption. Although this may devolve into a full index scan, the indexes are typically much smaller than the base table and still cheaper than scanning live data.

#### Metrics
Reuse existing TTL metrics (`jobs.row_level_ttl.*`).

#### Multi-Index Cleanup
The scheduler launches cleanup jobs for all non-aligned indexes, sequentially or concurrently.

---

## 5. Migration from Row-Level TTL

Migration is **out of scope** for prototype but documented for future work.

### Direct Enablement
If PK starts with TTL column:
```sql
ALTER TABLE events
SET (ttl_mode='partition', ttl_column='ts', ttl_retention='30d', ttl_granularity='1d');
```
No data rewrite required.

### PK Rebuild or Copy-Based Migration
If TTL column is not PK prefix, use `ALTER PRIMARY KEY` or create a new table and copy data.

### Migration Helper (Future)
`CALL crdb_internal.convert_ttl_to_partition_retention('table_name');`

---

## 6. Foreign Keys

Partition TTL **blocks inbound FKs**. Outward FKs are fine. Hybrid cleaner unaffected.

| FK Type | Behavior |
|----------|-----------|
| Inbound (parent) | ❌ Block partition TTL |
| Outbound (child) | ✅ Allowed |
| Hybrid cleaner | ✅ Safe |

**Future:** cascade-aware partition drops for aligned FKs.

---

## 7. Observability and Demo

### Virtual Table

```sql
crdb_internal.partition_ttl_status (
  table_name STRING,
  retention INTERVAL,
  granularity INTERVAL,
  lookahead INTERVAL,
  last_run TIMESTAMPTZ,
  next_run TIMESTAMPTZ,
  partitions_kept INT,
  partitions_dropped INT,
  last_dropped_partition STRING,
  hybrid_cleanup JSONB
);
```

### Metrics
Reuse existing TTL metrics for both TTL modes.

### Logs / Events
- Partition creation/drop.
- Hybrid cleanup per index with row counts and duration.
- Job start/stop messages for both TTL modes.

### On-Demand TTL Schedule Execution (for Row-Level TTL)

To support demos and quick experiments, the system adds a way to nudge the **row-level TTL** scheduler to run “now” without waiting for the next cron tick.

#### SQL usage
```sql
-- Trigger an individual schedule.
RUN SCHEDULE <schedule_id>;

-- Trigger a set of schedules produced by a query.
RUN SCHEDULES <select_query_returning_ids>;
```
This mirrors the `PAUSE/RESUME/DROP SCHEDULE[S]` verbs and reuses the same infrastructure.

#### Behavior
- The statement bumps each targeted schedule’s `next_run` timestamp to the current time; the job scheduler daemon will pick it up on its next loop.
- If a schedule is paused the statement raises an error (the user must `RESUME SCHEDULE` first).
- Because execution still relies on the background scheduler, jobs are not guaranteed to start immediately—there can be a short delay or a missed run if the daemon is busy or the job is already in progress.

#### Implementation Notes
- New `RUN SCHEDULE` / `RUN SCHEDULES` grammar lowers to `ControlSchedules{Command: RunSchedule}`.
- No `ALTER TABLE ... EXECUTE TTL IMMEDIATELY` wrapper; the explicit control statement is the supported path.

#### Demo use
```sql
RUN SCHEDULES
SELECT id FROM [SHOW SCHEDULES] WHERE schedule_name LIKE '%row-level%';
```
This keeps the SQL the same across row TTL and partition TTL demos, while acknowledging the start time remains asynchronous.

#### Observability
- Schedule rows briefly show `next_run` equal to “now”; once the job fires the usual state transitions apply.
- No dedicated event log entry was added; `SHOW SCHEDULES` remains the primary surface.

#### Deliverable Summary
| Task | Prototype? | Notes |
|------|-------------|-------|
| Support RUN SCHEDULE for TTL jobs | ✅ (asynchronous trigger) | Sets `next_run` to `now`; relies on job daemon |
| Add TTL event log entry | ❌ | Future follow-up |
| Implement ALTER TABLE wrapper | ❌ | Deferred |

---

## 8. Prototype Scope vs Future Work

| Feature | Prototype | Future |
|----------|------------|--------|
| Partition scheduling/drop | ✅ |  |
| Lookahead partitions | ✅ |  |
| Hybrid cleanup (reuse TTL processor) | ✅ |  |
| Immediate TTL execution | ✅ (best-effort) | `RUN SCHEDULE[S]` bumps `next_run`, still asynchronous |
| Migration helper | ❌ | Planned |
| Shorter GC TTL | ❌ | Planned |
| Cascade-aware drops | ❌ | Planned |
| Co-partitioned index fast drop | ❌ | Planned |
| Auto-trigger scheduler on ingest error | ❌ | Planned |

---

## 9. Test Plan

- Validation: bad column, FK blocked.
- Partition lifecycle: create/drop, lookahead coverage.
- Hybrid cleaner: multi-index, resumable spans.
- Performance: measure vs row TTL.
- GC behavior: reclaim after GC TTL.
- Observability: status + metrics.
- Failure recovery: job resume correctness.
- Immediate schedule execution: verify TTL starts on command.

---

## 10. Rationale Summary

| Problem | Design Choice | Why |
|----------|---------------|-----|
| Heavy TTL scans | Partition drops | O(1) logical delete |
| Secondary index bloat | Hybrid cleaner | Minimal cold-range deletes |
| Ingest gaps | Lookahead | Always future coverage |
| Governor complexity | Admission control only | Simpler tuning |
| FK correctness | Block inbound FKs | Safe default |
| Migration complexity | Omitted | Scope control |
| Index drift | Runtime detection | Always current |
| Demo trigger | EXECUTE SCHEDULE | Sync start for demos |

---

## 11. Rollout Plan

**Phase 1 (Prototype)**  
- DDL + validation  
- Scheduler + hybrid cleanup  
- Observability and demo tooling

**Phase 2 (Post-prototype)**  
- GC TTL tuning  
- Cascade-aware drops  
- Scheduler auto-trigger  
- Migration helper  
- Co-partitioned secondary fast-drop optimization

---

## 12. Learnings from Prototyping and Next Steps for Productization

### Key Learnings

- **Delete batch size limits.** The SQL placeholder ceiling (65,536) effectively caps delete batches in the row-level TTL processor. With `n` primary-key columns, the practical batch limit is `floor(65536 / n)` rows (e.g., ~32 K rows for a two-column PK). Production code should either cap batch sizes dynamically or split deletes automatically to stay within the limit.
- **Bootstrap sequencing.** Newly created tables start without partitions. The prototype immediately ran the partition TTL maintenance job right after marking the descriptor partitioned so the job could synthesize initial ranges. Productization needs a deterministic bootstrap sequence baked into the core workflow.
- **Schedule lifecycle.** Enabling partition TTL automatically creates a `partition_ttl_maintenance` schedule; dropping the table tears it down. This implicit lifecycle was convenient but needs to be formally documented and tested.
- **Multi-region hazards.** Dropping a partition in multi-region configurations currently deletes data outright. Without guardrails, this can violate locality guarantees; future work must introduce protections or explicit confirmations.
- **Out-of-range writes.** CockroachDB still accepts inserts outside partition bounds. True range partitioning would reject these, but today’s behavior (especially with multi-region tables) is more permissive. We need to evaluate whether and how to enforce bounds without surprising users.
- **Hybrid cleaner validation.** The cleanup path relies on the row-level TTL processor in delete-only mode, but it lacks exhaustive correctness and performance coverage. More testing is required before release quality.

### Unfinished Business / Productization Items

- Define a robust bootstrap procedure that initializes partition metadata and triggers the first maintenance run automatically.
- Add multi-region guardrails (or confirmations) when dropping partitions to avoid accidental cross-region data loss.
- Decide on stricter handling of out-of-range inserts, balancing correctness with existing multi-region semantics.
- Expand hybrid cleaner testing: targeted logic tests, end-to-end workloads, and performance benchmarks.
- Document and automate the schedule lifecycle (creation and cleanup) for partition TTL tables.
- Design and implement an `ALTER TABLE ... SET (...)` (or equivalent) workflow so existing tables can migrate into partition TTL without manual descriptor edits.
- Provide a guided migration path for users moving from row-level TTL to partition-based TTL, including tooling and documentation.
