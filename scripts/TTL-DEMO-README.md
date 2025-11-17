# TTL Demo Scripts

Scripts to orchestrate Row-Level TTL and Partition-Level TTL workflows through roachprod.

## Prerequisites

1. A running CockroachDB cluster accessible via `roachprod`
2. The required SQL files in the working directory:
   - `row-ttl-1-week-static.sql` - Creates row-level TTL table
   - `row-ttl-schedule.sql` - Queries row-level TTL schedule
   - `last-row-level-ttl-job.sql` - Shows latest row-level TTL job status
   - `partition-ttl-1-week-static.sql` - Creates partition-level TTL table
   - `partition-ttl-schedule.sql` - Queries partition-level TTL schedule
   - `last-ttl-partition-maintenance-job.sql` - Shows latest partition maintenance job status
   - `last-hybrid-ttl-job.sql` - Shows latest hybrid cleanup job status (partition TTL only)

## Scripts

### 1. `run-ttl-demo.sh` - Non-Interactive Mode

Runs the complete TTL workflow with command-line arguments.

**Usage:**
```bash
./scripts/run-ttl-demo.sh [row|partition] [cluster-name] [node] [wait-for-hybrid]
```

**Arguments:**
- `ttl-type`: Type of TTL to demo (`row` or `partition`). Default: `row`
- `cluster-name`: Roachprod cluster name. Default: `local`
- `node`: Node number to connect to. Default: `1`
- `wait-for-hybrid`: Whether to wait for hybrid cleanup job (`true` or `false`). Default: `false`
  - Only applies to partition-level TTL
  - Set to `true` if table has non-aligned secondary indexes

**Examples:**
```bash
# Run row-level TTL demo on local cluster
./scripts/run-ttl-demo.sh row local 1

# Run partition-level TTL demo on local cluster (skip hybrid cleanup)
./scripts/run-ttl-demo.sh partition local 1

# Run partition-level TTL demo and wait for hybrid cleanup job
./scripts/run-ttl-demo.sh partition local 1 true

# Run on a different cluster
./scripts/run-ttl-demo.sh row my-cluster 1

# Use defaults (row-level TTL on local:1)
./scripts/run-ttl-demo.sh
```

### 2. `run-ttl-demo-interactive.sh` - Interactive Mode

Runs the TTL workflow with interactive prompts for configuration.

**Usage:**
```bash
./scripts/run-ttl-demo-interactive.sh
```

The script will prompt you for:
1. TTL type (Row-Level or Partition-Level)
2. Cluster name (default: `local`)
3. Node number (default: `1`)
4. Confirmation before running the schedule
5. Confirmation to wait for hybrid cleanup job (partition TTL only, if needed)

**Example Session:**
```
╔════════════════════════════════════════════════════════════╗
║  CockroachDB TTL Demo Workflow                             ║
╚════════════════════════════════════════════════════════════╝

Select TTL type:
  1) Row-Level TTL
  2) Partition-Level TTL

Enter choice [1-2]: 2

Enter roachprod cluster name [default: local]:
Enter node number [default: 1]:

[INFO] ═══════════════════════════════════════════════════════════
[INFO] Configuration Summary
[INFO] ═══════════════════════════════════════════════════════════
  TTL Type:        Partition-Level TTL
  Cluster:         local
  Node:            1
  ...

[INFO] ═══════════════════════════════════════════════════════════
[SUCCESS] TTL Demo Workflow Complete!
[INFO] ═══════════════════════════════════════════════════════════

[INFO] Row count summary:
  Before TTL: 1000
  After TTL:  850
  Deleted:    150 rows
```

## Workflow Steps

Both scripts perform the following steps (steps may vary based on TTL type and user choices):

### 1. Create TTL Table
Creates a table with either row-level or partition-level TTL configuration.

### 2. Find Scheduled Job
Queries the database to find the scheduled job created for TTL maintenance.

### 3. Run Scheduled Job
Extracts the schedule ID and queries the initial row count, then manually triggers the schedule to run immediately.

The script displays the row count before TTL execution to establish a baseline for comparison.

### 4. Monitor Job Execution
Polls the job status every 5 seconds (up to 60 attempts / 5 minutes) until the job completes.

The script filters jobs to only consider those created after the table creation timestamp, ensuring it doesn't accidentally track old jobs from previous table incarnations.

The script displays:
- Job status (running, succeeded, failed)
- Job progress information
- Final outcome

### 5. Monitor Hybrid Cleanup Job (Partition TTL Only - Optional)
For partition-level TTL, the script can optionally monitor the hybrid cleanup job that removes orphaned secondary index entries from dropped partitions.

**When to enable:**
- Only needed if the table has **non-aligned secondary indexes** (where the partition column is not the leftmost key)
- If all secondary indexes are aligned or if there are no secondary indexes, hybrid cleanup jobs are not created and this step can be skipped

**How to enable:**
- Non-interactive script: Pass `true` as the 4th argument (default: `false`)
- Interactive script: Answer `y` when prompted (default: `n`)

This step is only relevant for partition-level TTL workflows, as row-level TTL does not create hybrid cleanup jobs.

### 6. Final Row Count and Summary
After all jobs complete, the script queries the final row count and displays a summary showing:
- Row count before TTL execution
- Row count after TTL execution
- Number of rows deleted

This demonstrates the effectiveness of the TTL job in removing expired data.

## Output

Both scripts provide color-coded output:
- **Blue [INFO]**: Informational messages
- **Green [SUCCESS]**: Successful operations
- **Yellow [WARNING]**: Warnings or timeouts
- **Red [ERROR]**: Errors

## Cleanup

After running the demo, you can clean up the created table:

```bash
# Drop the table (this also removes the schedule)
roachprod sql local:1 -- -e 'DROP TABLE movr.rides'

# Verify schedule is removed
roachprod sql local:1 -- -e 'SHOW SCHEDULES'
```

## Manual Commands

If you prefer to run commands manually:

### Row-Level TTL
```bash
# Create table
roachprod sql local:1 -- -f row-ttl-1-week-static.sql

# Find schedule
roachprod sql local:1 -- -f row-ttl-schedule.sql

# Run schedule (replace SCHEDULE_ID with actual ID from previous query)
roachprod sql local:1 -- -e 'RUN SCHEDULE <SCHEDULE_ID>'

# Monitor job
roachprod sql local:1 -- -f last-row-level-ttl-job.sql
```

### Partition-Level TTL
```bash
# Create table
roachprod sql local:1 -- -f partition-ttl-1-week-static.sql

# Find schedule
roachprod sql local:1 -- -f partition-ttl-schedule.sql

# Run schedule (replace SCHEDULE_ID with actual ID from previous query)
roachprod sql local:1 -- -e 'RUN SCHEDULE <SCHEDULE_ID>'

# Monitor partition maintenance job
roachprod sql local:1 -- -f last-ttl-partition-maintenance-job.sql

# Monitor hybrid cleanup job
roachprod sql local:1 -- -f last-hybrid-ttl-job.sql
```

## Troubleshooting

### Script fails to find SQL files
- Ensure you're running the script from the directory containing the SQL files
- Check that all required SQL files exist

### Cannot connect to cluster
- Verify the cluster is running: `roachprod status <cluster-name>`
- Check cluster name and node number are correct
- Ensure roachprod is configured properly

### Schedule ID extraction fails
- The schedule file must return at least one row with a `schedule_id` column
- Check the SQL file returns the expected format

### Job polling times out
- Increase polling attempts by modifying `poll_job_status` call in the script
- Check if the job is running: `SELECT * FROM [SHOW JOBS]`
- Look for errors in the job details

### Permission denied
- Make scripts executable: `chmod +x scripts/*.sh`

## Advanced Usage

### Custom Polling Parameters

Edit the script to change polling behavior:
```bash
# In the script, find:
poll_job_status "$JOB_STATUS_FILE" 60 5

# Change to:
# poll_job_status <file> <max-attempts> <sleep-interval>
poll_job_status "$JOB_STATUS_FILE" 120 10  # 20 minutes, 10s intervals
```

### Running Multiple Demos

To compare row-level vs partition-level TTL:
```bash
# Terminal 1: Row-level TTL
./scripts/run-ttl-demo.sh row local 1

# Terminal 2: Partition-level TTL
./scripts/run-ttl-demo.sh partition local 2
```

## See Also

- [Row-Level TTL Documentation](https://www.cockroachlabs.com/docs/stable/row-level-ttl.html)
- [Partition-Level TTL Design](../docs/RFCS/...)
- [Scheduled Jobs](https://www.cockroachlabs.com/docs/stable/manage-schedules.html)
