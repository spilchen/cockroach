#!/usr/bin/env bash
#
# run-ttl-demo-interactive.sh - Interactive TTL workflow orchestration
#
# Usage:
#   ./scripts/run-ttl-demo-interactive.sh
#

set -euo pipefail

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
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

print_header() {
  echo ""
  echo -e "${CYAN}╔════════════════════════════════════════════════════════════╗${NC}"
  echo -e "${CYAN}║${NC}  ${BOLD}CockroachDB TTL Demo Workflow${NC}                        ${CYAN}║${NC}"
  echo -e "${CYAN}╚════════════════════════════════════════════════════════════╝${NC}"
  echo ""
}

# Prompt for input with default
prompt_with_default() {
  local prompt="$1"
  local default="$2"
  local result

  read -p "$(echo -e ${YELLOW}$prompt${NC} [default: ${GREEN}$default${NC}]: )" result
  echo "${result:-$default}"
}

# Prompt for choice
prompt_choice() {
  local prompt="$1"
  shift
  local options=("$@")

  echo ""
  echo -e "${YELLOW}$prompt${NC}"
  for i in "${!options[@]}"; do
    echo -e "  ${CYAN}$((i + 1))${NC}) ${options[$i]}"
  done
  echo ""

  local choice
  while true; do
    read -p "$(echo -e ${YELLOW}Enter choice [1-${#options[@]}]${NC}: )" choice
    if [[ "$choice" =~ ^[0-9]+$ ]] && [[ "$choice" -ge 1 ]] && [[ "$choice" -le "${#options[@]}" ]]; then
      echo "${options[$((choice - 1))]}"
      return
    else
      log_error "Invalid choice. Please enter a number between 1 and ${#options[@]}"
    fi
  done
}

# Run SQL file
run_sql_file() {
  local cluster="$1"
  local node="$2"
  local file="$3"
  local description="$4"

  if [[ ! -f "$file" ]]; then
    log_error "SQL file not found: $file"
    exit 1
  fi

  log_info "$description"
  log_info "Running: roachprod sql ${cluster}:${node} -- -f $file"
  roachprod sql "${cluster}:${node}" -- -f "$file"
}

# Run SQL command
run_sql_cmd() {
  local cluster="$1"
  local node="$2"
  local cmd="$3"
  local description="$4"

  log_info "$description"
  log_info "Running: roachprod sql ${cluster}:${node} -- -e '$cmd'"
  roachprod sql "${cluster}:${node}" -- -e "$cmd"
}

# Get row count from table
get_row_count() {
  local cluster="$1"
  local node="$2"
  local table_name="$3"
  local description="$4"

  log_info "$description"
  local count
  count=$(roachprod sql "${cluster}:${node}" -- -e "SELECT COUNT(*) FROM $table_name" --format=tsv 2>/dev/null | grep -v '^count$' | head -n 1)
  echo -e "  ${GREEN}Row count: $count${NC}" >&2
  echo "$count"
}

# Extract schedule ID from output
get_schedule_id() {
  local cluster="$1"
  local node="$2"
  local schedule_file="$3"

  log_info "Extracting schedule ID from $schedule_file"
  local schedule_id

  # Try TSV format first
  schedule_id=$(roachprod sql "${cluster}:${node}" -- -f "$schedule_file" --format=tsv 2>/dev/null | grep -v '^schedule_id' | grep -E '^[0-9]+' | head -n 1 | awk '{print $1}')

  # If TSV didn't work, try parsing record format
  if [[ -z "$schedule_id" ]]; then
    schedule_id=$(roachprod sql "${cluster}:${node}" -- -f "$schedule_file" 2>/dev/null | grep -E '^id[[:space:]]*\|' | awk -F '|' '{print $2}' | tr -d ' ')
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
  local cluster="$1"
  local node="$2"
  local job_file="$3"
  local table_created_at="$4"
  local max_attempts="${5:-60}"
  local sleep_interval="${6:-5}"

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
    output=$(roachprod sql "${cluster}:${node}" -- -f "$temp_query_file" --format=table 2>&1 || true)
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

# Confirm action
confirm_action() {
  local message="$1"
  local response

  read -p "$(echo -e ${YELLOW}$message${NC} [y/N]: )" response
  case "$response" in
    [yY][eE][sS]|[yY])
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

# Main workflow
main() {
  print_header

  # Prompt for configuration
  local ttl_type
  ttl_type=$(prompt_choice "Select TTL type:" "Row-Level TTL" "Partition-Level TTL")

  local cluster
  cluster=$(prompt_with_default "Enter roachprod cluster name" "local")

  local node
  node=$(prompt_with_default "Enter node number" "1")

  # Map choice to type
  if [[ "$ttl_type" == "Row-Level TTL" ]]; then
    TTL_TYPE="row"
    CREATE_FILE="row-ttl-1-week-static.sql"
    SCHEDULE_FILE="row-ttl-schedule.sql"
    JOB_STATUS_FILE="last-row-level-ttl-job.sql"
    TTL_DESC="Row-Level TTL"
  else
    TTL_TYPE="partition"
    CREATE_FILE="partition-ttl-1-week-static.sql"
    SCHEDULE_FILE="partition-ttl-schedule.sql"
    JOB_STATUS_FILE="last-ttl-partition-maintenance-job.sql"
    TTL_DESC="Partition-Level TTL"
  fi

  # Display configuration
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_info "Configuration Summary"
  log_info "═══════════════════════════════════════════════════════════"
  echo -e "  TTL Type:        ${GREEN}$TTL_DESC${NC}"
  echo -e "  Cluster:         ${GREEN}$cluster${NC}"
  echo -e "  Node:            ${GREEN}$node${NC}"
  echo -e "  Create File:     ${GREEN}$CREATE_FILE${NC}"
  echo -e "  Schedule File:   ${GREEN}$SCHEDULE_FILE${NC}"
  echo -e "  Job Status File: ${GREEN}$JOB_STATUS_FILE${NC}"
  log_info "═══════════════════════════════════════════════════════════"
  echo ""

  if ! confirm_action "Proceed with workflow?"; then
    log_warning "Workflow cancelled by user"
    exit 0
  fi

  # Step 1: Create the TTL table
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_info "Step 1: Creating ${TTL_DESC} Table"
  log_info "═══════════════════════════════════════════════════════════"

  # Capture the timestamp before creating the table
  TABLE_CREATED_AT=$(roachprod sql "${cluster}:${node}" -- -e "SELECT now()" --format=tsv 2>/dev/null | grep -v '^now$' | head -n 1)
  log_info "Table creation timestamp: $TABLE_CREATED_AT"

  run_sql_file "$cluster" "$node" "$CREATE_FILE" "Creating table with ${TTL_DESC}"
  log_success "Table created"

  # Step 2: Get the schedule
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_info "Step 2: Finding Scheduled Job"
  log_info "═══════════════════════════════════════════════════════════"
  run_sql_file "$cluster" "$node" "$SCHEDULE_FILE" "Querying for schedule"

  # Step 3: Extract schedule ID and run it
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_info "Step 3: Running Scheduled Job"
  log_info "═══════════════════════════════════════════════════════════"

  SCHEDULE_ID=$(get_schedule_id "$cluster" "$node" "$SCHEDULE_FILE")
  log_success "Found schedule ID: $SCHEDULE_ID"

  if confirm_action "Run schedule $SCHEDULE_ID?"; then
    # Get row count before running schedule
    echo ""
    INITIAL_COUNT=$(get_row_count "$cluster" "$node" "movr.rides" "Checking row count before TTL execution")
    echo ""

    run_sql_cmd "$cluster" "$node" "RUN SCHEDULE $SCHEDULE_ID" "Triggering schedule execution"
    log_success "Schedule triggered"

    # Step 4: Poll job status
    echo ""
    log_info "═══════════════════════════════════════════════════════════"
    log_info "Step 4: Monitoring Job Execution"
    log_info "═══════════════════════════════════════════════════════════"

    poll_job_status "$cluster" "$node" "$JOB_STATUS_FILE" "$TABLE_CREATED_AT" 60 5

    # Step 5: For partition TTL, optionally monitor hybrid cleanup job
    if [[ "$TTL_TYPE" == "partition" ]]; then
      echo ""
      if confirm_action "Wait for hybrid cleanup job? (only needed if table has non-aligned secondary indexes)"; then
        log_info "═══════════════════════════════════════════════════════════"
        log_info "Step 5: Monitoring Hybrid Cleanup Job"
        log_info "═══════════════════════════════════════════════════════════"

        poll_job_status "$cluster" "$node" "last-hybrid-ttl-job.sql" "$TABLE_CREATED_AT" 60 5
      else
        log_info "Skipping hybrid cleanup job monitoring"
      fi
    fi

    # Get row count after all jobs complete
    echo ""
    FINAL_COUNT=$(get_row_count "$cluster" "$node" "movr.rides" "Checking row count after TTL execution")
    echo ""
  else
    log_info "Schedule execution skipped"
    log_info "You can manually run it with:"
    log_info "  roachprod sql ${cluster}:${node} -- -e 'RUN SCHEDULE $SCHEDULE_ID'"
  fi

  # Final summary
  echo ""
  log_info "═══════════════════════════════════════════════════════════"
  log_success "TTL Demo Workflow Complete!"
  log_info "═══════════════════════════════════════════════════════════"
  echo ""
  if [[ -n "$INITIAL_COUNT" ]] && [[ -n "$FINAL_COUNT" ]]; then
    log_info "Row count summary:"
    echo -e "  Before TTL: ${GREEN}$INITIAL_COUNT${NC}" >&2
    echo -e "  After TTL:  ${GREEN}$FINAL_COUNT${NC}" >&2
    DELETED=$((INITIAL_COUNT - FINAL_COUNT))
    echo -e "  Deleted:    ${GREEN}$DELETED${NC} rows" >&2
    echo ""
  fi
  echo ""
  log_info "Useful commands:"
  log_info "  View schedule:   roachprod sql ${cluster}:${node} -- -f $SCHEDULE_FILE"
  log_info "  Check job:       roachprod sql ${cluster}:${node} -- -f $JOB_STATUS_FILE"
  if [[ "$TTL_TYPE" == "partition" ]]; then
    log_info "  Hybrid cleanup:  roachprod sql ${cluster}:${node} -- -f last-hybrid-ttl-job.sql"
  fi
  log_info "  Drop table:      roachprod sql ${cluster}:${node} -- -e 'DROP TABLE movr.rides'"
  echo ""
}

# Run main workflow
main
