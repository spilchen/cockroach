#!/usr/bin/env bash
#
# run-ttl-demo.sh - Orchestrate TTL workflow through roachprod
#
# Usage:
#   ./scripts/run-ttl-demo.sh [row|partition] [cluster-name] [node]
#
# Examples:
#   ./scripts/run-ttl-demo.sh row local 1
#   ./scripts/run-ttl-demo.sh partition local 1
#   ./scripts/run-ttl-demo.sh row my-cluster 1
#

set -euo pipefail

# Parse arguments
TTL_TYPE="${1:-row}"
CLUSTER="${2:-local}"
NODE="${3:-1}"
WAIT_FOR_HYBRID="${4:-false}"

# Validate TTL type
if [[ "$TTL_TYPE" != "row" && "$TTL_TYPE" != "partition" ]]; then
  echo "Error: TTL type must be 'row' or 'partition'"
  echo "Usage: $0 [row|partition] [cluster-name] [node] [wait-for-hybrid]"
  echo ""
  echo "Arguments:"
  echo "  ttl-type:         'row' or 'partition' (default: row)"
  echo "  cluster-name:     roachprod cluster name (default: local)"
  echo "  node:             node number (default: 1)"
  echo "  wait-for-hybrid:  'true' to wait for hybrid cleanup job (default: false)"
  exit 1
fi

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Helper functions
log_info() {
  echo -e "${BLUE}[INFO]${NC} $*" >&2
}

log_success() {
  echo -e "${GREEN}[SUCCESS]${NC} $*" >&2
}

log_warning() {
  echo -e "${YELLOW}[WARNING]${NC} $*" >&2
}

log_error() {
  echo -e "${RED}[ERROR]${NC} $*" >&2
}

# Run SQL file
run_sql_file() {
  local file="$1"
  local description="$2"

  if [[ ! -f "$file" ]]; then
    log_error "SQL file not found: $file"
    exit 1
  fi

  log_info "$description"
  log_info "Running: roachprod sql ${CLUSTER}:${NODE} -- -f $file"
  roachprod sql "${CLUSTER}:${NODE}" -- -f "$file"
}

# Run SQL command
run_sql_cmd() {
  local cmd="$1"
  local description="$2"

  log_info "$description"
  log_info "Running: roachprod sql ${CLUSTER}:${NODE} -- -e '$cmd'"
  roachprod sql "${CLUSTER}:${NODE}" -- -e "$cmd"
}

# Get row count from table
get_row_count() {
  local table_name="$1"
  local description="$2"

  log_info "$description"
  local count
  count=$(roachprod sql "${CLUSTER}:${NODE}" -- -e "SELECT COUNT(*) FROM $table_name" --format=tsv 2>/dev/null | grep -v '^count$' | head -n 1)
  echo -e "  ${GREEN}Row count: $count${NC}" >&2
  echo "$count"
}

# Extract schedule ID from output
get_schedule_id() {
  local schedule_file="$1"

  log_info "Extracting schedule ID from $schedule_file"
  local schedule_id

  # Try TSV format first
  schedule_id=$(roachprod sql "${CLUSTER}:${NODE}" -- -f "$schedule_file" --format=tsv 2>/dev/null | grep -v '^schedule_id' | grep -E '^[0-9]+' | head -n 1 | awk '{print $1}')

  # If TSV didn't work, try parsing record format
  if [[ -z "$schedule_id" ]]; then
    schedule_id=$(roachprod sql "${CLUSTER}:${NODE}" -- -f "$schedule_file" 2>/dev/null | grep -E '^id[[:space:]]*\|' | awk -F '|' '{print $2}' | tr -d ' ')
  fi

  if [[ -z "$schedule_id" || ! "$schedule_id" =~ ^[0-9]+$ ]]; then
    log_error "Failed to extract schedule ID"
    log_error "Please check that $schedule_file returns a schedule with an 'id' field"
    exit 1
  fi

  echo "$schedule_id"
}

# Poll job status
poll_job_status() {
  local job_file="$1"
  local table_created_at="$2"
  local max_attempts="${3:-60}"
  local sleep_interval="${4:-5}"

  log_info "Polling job status (max ${max_attempts} attempts, ${sleep_interval}s interval)"
  log_info "Looking for jobs created after: $table_created_at"

  local attempt=0
  while [[ $attempt -lt $max_attempts ]]; do
    attempt=$((attempt + 1))

    log_info "Attempt $attempt/$max_attempts: Checking job status..."

    # Create a temporary SQL file that wraps the SELECT query with timestamp filter
    # Extract USE statements and \x commands separately
    local temp_query_file="/tmp/ttl_job_query_$$.sql"
    {
      # Extract USE statements and \x commands (preserve them)
      grep -E '^(use|USE|\\x)' "$job_file" || true

      # Build the CTE with the SELECT query (exclude USE and \x lines)
      echo "WITH base_query AS ("
      grep -vE '^(use|USE|\\x)' "$job_file" | sed 's/;$//'
      echo ")"
      echo "SELECT * FROM base_query"
      echo "WHERE created >= '$table_created_at'::TIMESTAMPTZ"
      echo "ORDER BY created DESC"
      echo "LIMIT 1;"
    } > "$temp_query_file"

    # Run the job status query with timestamp filter
    local output
    output=$(roachprod sql "${CLUSTER}:${NODE}" -- -f "$temp_query_file" --format=table 2>&1 || true)
    rm -f "$temp_query_file"

    # Check for SQL errors
    if echo "$output" | grep -qi "ERROR:"; then
      log_error "SQL error occurred:"
      echo "$output" | grep -A5 "ERROR:" >&2
      log_warning "Retrying..."
      if [[ $attempt -lt $max_attempts ]]; then
        sleep "$sleep_interval"
      fi
      continue
    fi

    # Check if we got results (look for actual data, not just SET)
    if echo "$output" | grep -q "0 rows"; then
      log_warning "No job found yet, waiting..."
    elif echo "$output" | grep -qE "job_id|status"; then
      # We have actual job data, display it
      echo "$output" >&2

      # Extract the status field value (look for status column in record or table format)
      local status=""
      # Try to extract from "status | value" format
      status=$(echo "$output" | grep -E '^status[[:space:]]*\|' | awk -F '|' '{print $2}' | tr -d ' ' | tr '[:upper:]' '[:lower:]')

      # If not found, try table format
      if [[ -z "$status" ]]; then
        status=$(echo "$output" | awk '/status/ {for(i=1;i<=NF;i++) if($i=="status") col=i} NR>2 && col {print $col; exit}' | tr '[:upper:]' '[:lower:]')
      fi

      # Check the actual status value
      if [[ "$status" == "succeeded" ]]; then
        log_success "Job completed successfully!"
        return 0
      elif [[ "$status" == "failed" ]]; then
        log_error "Job failed!"
        return 1
      elif [[ "$status" == "running" ]]; then
        log_info "Job is running..."
      else
        log_info "Job status: $status"
      fi
    else
      log_warning "Waiting for job to be created..."
    fi

    if [[ $attempt -lt $max_attempts ]]; then
      sleep "$sleep_interval"
    fi
  done

  log_warning "Polling timed out after ${max_attempts} attempts"
  return 1
}

# Main workflow
main() {
  log_info "Starting TTL Demo Workflow"
  log_info "TTL Type: $TTL_TYPE"
  log_info "Cluster: $CLUSTER"
  log_info "Node: $NODE"
  echo ""

  # Set file names based on TTL type
  if [[ "$TTL_TYPE" == "row" ]]; then
    CREATE_FILE="row-ttl-1-week-static.sql"
    SCHEDULE_FILE="row-ttl-schedule.sql"
    JOB_STATUS_FILE="last-row-level-ttl-job.sql"
    TTL_DESC="Row-Level TTL"
  else
    CREATE_FILE="partition-ttl-1-week-static.sql"
    SCHEDULE_FILE="partition-ttl-schedule.sql"
    JOB_STATUS_FILE="last-ttl-partition-maintenance-job.sql"
    TTL_DESC="Partition-Level TTL"
  fi

  # Step 1: Create the TTL table
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_info "Step 1: Creating ${TTL_DESC} Table"
  log_info "═══════════════════════════════════════════════════════════"

  # Capture the timestamp before creating the table
  TABLE_CREATED_AT=$(roachprod sql "${CLUSTER}:${NODE}" -- -e "SELECT now()" --format=tsv 2>/dev/null | grep -v '^now$' | head -n 1)
  log_info "Table creation timestamp: $TABLE_CREATED_AT"

  run_sql_file "$CREATE_FILE" "Creating table with ${TTL_DESC}"
  log_success "Table created"

  # Step 2: Get the schedule
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_info "Step 2: Finding Scheduled Job"
  log_info "═══════════════════════════════════════════════════════════"
  run_sql_file "$SCHEDULE_FILE" "Querying for schedule"

  # Step 3: Extract schedule ID and run it
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_info "Step 3: Running Scheduled Job"
  log_info "═══════════════════════════════════════════════════════════"

  SCHEDULE_ID=$(get_schedule_id "$SCHEDULE_FILE")
  log_success "Found schedule ID: $SCHEDULE_ID"

  # Get row count before running schedule
  echo ""
  INITIAL_COUNT=$(get_row_count "movr.rides" "Checking row count before TTL execution")
  echo ""

  run_sql_cmd "RUN SCHEDULE $SCHEDULE_ID" "Triggering schedule execution"
  log_success "Schedule triggered"

  # Step 4: Poll job status
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_info "Step 4: Monitoring Job Execution"
  log_info "═══════════════════════════════════════════════════════════"

  poll_job_status "$JOB_STATUS_FILE" "$TABLE_CREATED_AT" 60 5

  # Step 5: For partition TTL, optionally monitor hybrid cleanup job
  if [[ "$TTL_TYPE" == "partition" ]] && [[ "$WAIT_FOR_HYBRID" == "true" ]]; then
    echo ""
    log_info "═══════════════════════════════════════════════════════════"
    log_info "Step 5: Monitoring Hybrid Cleanup Job"
    log_info "═══════════════════════════════════════════════════════════"

    poll_job_status "last-hybrid-ttl-job.sql" "$TABLE_CREATED_AT" 60 5
  elif [[ "$TTL_TYPE" == "partition" ]]; then
    log_info "Skipping hybrid cleanup job monitoring (use 'true' as 4th argument to enable)"
  fi

  # Get row count after all jobs complete
  echo ""
  FINAL_COUNT=$(get_row_count "movr.rides" "Checking row count after TTL execution")
  echo ""

  # Final summary
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_success "TTL Demo Workflow Complete!"
  log_info "═══════════════════════════════════════════════════════════"
  echo ""
  log_info "Row count summary:"
  echo -e "  Before TTL: ${GREEN}$INITIAL_COUNT${NC}" >&2
  echo -e "  After TTL:  ${GREEN}$FINAL_COUNT${NC}" >&2
  if [[ -n "$INITIAL_COUNT" ]] && [[ -n "$FINAL_COUNT" ]]; then
    DELETED=$((INITIAL_COUNT - FINAL_COUNT))
    echo -e "  Deleted:    ${GREEN}$DELETED${NC} rows" >&2
  fi
  echo ""
  log_info "To clean up, you can drop the table:"
  if [[ "$TTL_TYPE" == "row" ]]; then
    log_info "  roachprod sql ${CLUSTER}:${NODE} -- -e 'DROP TABLE movr.rides'"
  else
    log_info "  roachprod sql ${CLUSTER}:${NODE} -- -e 'DROP TABLE movr.rides'"
  fi
  echo ""
}

# Run main workflow
main
