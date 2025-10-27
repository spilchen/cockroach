#!/usr/bin/env bash
# Orchestrates a short INSPECT demo using the MovR dataset:
#   - Shows that INSPECT requires enable_inspect_command.
#   - Corrupts an entry in movr.vehicles' secondary index via cockroach debug send-kv-batch.
#   - Runs INSPECT and SHOW INSPECT ERRORS to surface the issue.
#
# Prerequisites:
#   * MovR dataset is loaded (e.g. via `cockroach demo movr` or RESTORE)
#   * cockroach binary in PATH (override with $COCKROACH_BIN if needed)
#   * $INSPECT_DEMO_URL set to a postgres connection URL with admin privileges
#   * Cluster version supports INSPECT + SHOW INSPECT ERRORS
set -euo pipefail

COCKROACH_BIN=${COCKROACH_BIN:-./cockroach}
INSPECT_DEMO_URL=${INSPECT_DEMO_URL:-postgresql://root@127.0.0.1:26257/movr?sslmode=disable}
INSPECT_KV_URL=${INSPECT_KV_URL:-}
INSPECT_KV_HOST=${INSPECT_KV_HOST:-}
INSPECT_KV_PORT=${INSPECT_KV_PORT:-}
STORE_DIR=${STORE_DIR:-/tmp/cockroach-inspect-test}
LISTEN_ADDR=${LISTEN_ADDR:-127.0.0.1:26257}
HTTP_ADDR=${HTTP_ADDR:-127.0.0.1:8090}

DATABASE_NAME=${DATABASE_NAME:-movr}
TABLE_NAME=${TABLE_NAME:-vehicles}
INDEX_NAME=${INDEX_NAME:-vehicles_auto_index_fk_city_ref_users}
INDEX_KEY_COLUMNS=${INDEX_KEY_COLUMNS:-}

thick_sep='============================================================'
cyan=$(printf '\033[36m')
green=$(printf '\033[32m')
reset=$(printf '\033[0m')

color_enabled=true
if [[ -t 1 ]]; then
  :
else
  color_enabled=false
fi

colorize() {
  local code="$1"
  shift
  if [ "$color_enabled" = true ]; then
    printf "${code}%s\033[0m" "$*"
  else
    printf "%s" "$*"
  fi
}

header() {
  if [ "$color_enabled" = true ]; then
    printf '\n'
    colorize '\033[36m' "$thick_sep"
    printf '\n'
    colorize '\033[32m' "✅  $1"
    printf '\n'
  else
    printf '\n=== %s ===\n' "$1"
  fi
  if [[ "${INSPECT_DEMO_PAUSE_HEADERS:-false}" == 'true' ]]; then
    read -r -p 'Press Enter to continue...'
  fi
}

header_quick() {
  if [ "$color_enabled" = true ]; then
    printf '\n'
    colorize '\033[32m' "---- $1 ----"
    printf '\n'
  else
    printf '\n=== %s ===\n' "$1"
  fi
}


if [[ "${INSPECT_DEMO_SKIP_SETUP:-false}" != "true" ]]; then
  header_quick "Stopping any existing cluster"
  pkill -f "cockroach start-single-node" 2>/dev/null || true
  sleep 2

  header_quick "Cleaning up old data directory"
  rm -rf "$STORE_DIR"

  header_quick "Starting single-node cluster"
  "$COCKROACH_BIN" start-single-node \
    --insecure \
    --store="$STORE_DIR" \
    --listen-addr="$LISTEN_ADDR" \
    --http-addr="$HTTP_ADDR" \
    --background

  header_quick "Waiting for cluster to be ready"
  sleep 3

  header_quick "Loading MovR dataset"
  "$COCKROACH_BIN" workload init movr "postgresql://root@${LISTEN_ADDR}?sslmode=disable"

  header_quick "Verifying dataset load"
  "$COCKROACH_BIN" sql --insecure --execute="
SELECT 
  'vehicles' AS table_name, 
  count(*) AS row_count 
FROM movr.vehicles
UNION ALL
SELECT 
  'users' AS table_name, 
  count(*) AS row_count 
FROM movr.users
UNION ALL
SELECT 
  'rides' AS table_name, 
  count(*) AS row_count 
FROM movr.rides;
" --format=table

  header_quick "Cluster connection information"
  printf 'SQL: postgresql://root@%s/movr?sslmode=disable\n' "$LISTEN_ADDR"
  printf 'HTTP: http://%s\n' "$HTTP_ADDR"
  printf 'To stop the cluster: pkill -f "cockroach start-single-node"\n'

  INSPECT_DEMO_URL="postgresql://root@${LISTEN_ADDR}/movr?sslmode=disable"
fi


header_quick "Verifying MovR dataset availability"
INFO_LINE=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv <<SQL | sed -n '2p'
SELECT t.table_id, i.index_id, i.index_name
FROM crdb_internal.tables AS t
JOIN crdb_internal.table_indexes AS i
  ON i.descriptor_id = t.table_id
WHERE t.database_name = '${DATABASE_NAME}'
  AND t.name = '${TABLE_NAME}'
  AND i.index_name = '${INDEX_NAME}';
SQL
)
IFS=$'\t' read -r TABLE_ID INDEX_ID INDEX_NAME_FOUND <<< "$INFO_LINE"
if [[ -z "${TABLE_ID:-}" || -z "${INDEX_ID:-}" ]]; then
  echo "Unable to locate ${DATABASE_NAME}.${TABLE_NAME} index ${INDEX_NAME}. Aborting." >&2
  exit 1
fi
INDEX_NAME=${INDEX_NAME_FOUND}
echo "Table id: ${TABLE_ID}, index id: ${INDEX_ID} (${INDEX_NAME})"

header_quick "Derive key columns for ${INDEX_NAME}"
COL_SELECT_LIST=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv <<SQL | sed -n '2p'
SELECT string_agg(format('%I', col_name), ', ' ORDER BY seq) AS cols
FROM (
  SELECT
    COALESCE(ic.column_name, tc.column_name) AS col_name,
    row_number() OVER (ORDER BY ic.column_id) AS seq
  FROM crdb_internal.index_columns AS ic
  LEFT JOIN crdb_internal.table_columns AS tc
    ON tc.descriptor_id = ic.descriptor_id
   AND tc.column_id = ic.column_id
  WHERE ic.descriptor_id = ${TABLE_ID}
    AND ic.index_id = ${INDEX_ID}
    AND ic.column_type = 'key'
) sub;
SQL
)
COL_SELECT_LIST=${COL_SELECT_LIST//$'\r'/}
if [[ -z "${COL_SELECT_LIST// }" ]]; then
  echo "Unable to determine key columns for index ${INDEX_NAME}. Aborting." >&2
  exit 1
fi
if [[ -n "${INDEX_KEY_COLUMNS// }" ]]; then
  echo "Using key column override from INDEX_KEY_COLUMNS: ${INDEX_KEY_COLUMNS}"
  COL_SELECT_LIST=${INDEX_KEY_COLUMNS}
else
  echo "Derived key column order: ${COL_SELECT_LIST//\"/}"
  if [[ "${TABLE_NAME}" == "vehicles" && "${INDEX_NAME}" == "vehicles_auto_index_fk_city_ref_users" ]]; then
    MOVR_EXPECTED_ORDER='city, owner_id, id'
    if [[ "${COL_SELECT_LIST//\"/}" != "${MOVR_EXPECTED_ORDER}" ]]; then
      echo "Adjusting key column order to ${MOVR_EXPECTED_ORDER} for MovR vehicles index."
      COL_SELECT_LIST=${MOVR_EXPECTED_ORDER}
    fi
  fi
fi

header "Run INSPECT" 
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table --echo-sql <<SQL
SET enable_inspect_command = true;
INSPECT TABLE ${DATABASE_NAME}.${TABLE_NAME} WITH OPTIONS INDEX (${INDEX_NAME});
SQL

header_quick "Latest INSPECT job status"
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table <<SQL
SELECT job_id, status, description, fraction_completed
FROM [SHOW JOBS]
WHERE job_type = 'INSPECT'
ORDER BY created DESC
LIMIT 1;
SQL

header "Select a vehicle"
ROW_LINE=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv --echo-sql <<SQL | sed -n '2p'
SELECT id, city, owner_id
FROM ${DATABASE_NAME}.${TABLE_NAME}
WHERE owner_id IS NOT NULL
ORDER BY city, id
LIMIT 1;
SQL
)
IFS=$'\t' read -r VEHICLE_ID VEHICLE_CITY OWNER_ID <<< "$ROW_LINE"
if [[ -z "${VEHICLE_ID:-}" || -z "${OWNER_ID:-}" ]]; then
  echo "Could not find a vehicle with a non-null owner. Aborting." >&2
  exit 1
fi
ESCAPED_CITY=${VEHICLE_CITY//\'/\'\'}

echo "Vehicle id: $VEHICLE_ID"
echo "City: $VEHICLE_CITY"
echo "Owner id: $OWNER_ID"

if [[ -n "${INSPECT_KV_HOST}" || -n "${INSPECT_KV_PORT}" ]]; then
  KV_URL=""
  KV_HOST=${INSPECT_KV_HOST:-127.0.0.1}
  KV_PORT=${INSPECT_KV_PORT:-26257}
  echo "Using KV host/port override: ${KV_HOST}:${KV_PORT}"
elif [[ -n "${INSPECT_KV_URL}" ]]; then
  KV_URL=${INSPECT_KV_URL}
  echo "Using INSPECT_KV_URL override."
elif command -v python3 >/dev/null 2>&1; then
  KV_URL=$(INSPECT_DEMO_URL="$INSPECT_DEMO_URL" python3 - <<'PY'
import os
from urllib.parse import urlparse, urlunparse, parse_qsl, urlencode

url = os.environ["INSPECT_DEMO_URL"]
parts = urlparse(url)
sanitized_q = []
for k, v in parse_qsl(parts.query, keep_blank_values=True):
    if k.lower() == "options":
        continue
    sanitized_q.append((k, v))
parts = parts._replace(path='', params='', query=urlencode(sanitized_q), fragment='')
sanitized = urlunparse(parts)
if not sanitized:
    sanitized = url
print(sanitized)
PY
)
else
  KV_URL=${INSPECT_DEMO_URL}
  if [[ "${KV_URL}" == *"/${DATABASE_NAME}" ]]; then
    KV_URL=${KV_URL/\/${DATABASE_NAME}/}
  fi
  KV_URL=${KV_URL%%\?options=*}
fi
echo "KV API connection (database stripped if present): ${KV_URL}"

KV_EXTRA_FLAGS=${INSPECT_KV_FLAGS:-}
if [[ -n "${KV_EXTRA_FLAGS// }" ]]; then
  echo "Additional KV debug flags: ${KV_EXTRA_FLAGS}"
  # shellcheck disable=SC2206
  KV_FLAGS_ARR=(${KV_EXTRA_FLAGS})
else
  KV_FLAGS_ARR=()
fi

header "Baseline query results for vehicle ${VEHICLE_ID}"
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table --echo-sql <<SQL
SELECT id, city, owner_id
FROM ${DATABASE_NAME}.${TABLE_NAME}@{FORCE_INDEX=primary}
WHERE id = '${VEHICLE_ID}';
SQL
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table --echo-sql <<SQL
SELECT id, city, owner_id
FROM ${DATABASE_NAME}.${TABLE_NAME}@{FORCE_INDEX=${INDEX_NAME}}
WHERE ROW(${COL_SELECT_LIST})
    = (SELECT ROW(${COL_SELECT_LIST})
       FROM ${DATABASE_NAME}.${TABLE_NAME}
       WHERE id = '${VEHICLE_ID}');
SQL

header "Baseline raw index scan entry"
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table --echo-sql <<SQL
SELECT city, owner_id, id
FROM ${DATABASE_NAME}.${TABLE_NAME}@${INDEX_NAME}
WHERE city = '${ESCAPED_CITY}'
  AND owner_id = '${OWNER_ID}'::UUID;
SQL

INDEX_COUNT_BEFORE=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv <<SQL | awk 'NR==2 {print $1}'
SELECT count(*)
FROM ${DATABASE_NAME}.${TABLE_NAME}@${INDEX_NAME}
WHERE city = '${ESCAPED_CITY}'
  AND owner_id = '${OWNER_ID}'::UUID
  AND id = '${VEHICLE_ID}'::UUID;
SQL
)
echo "Baseline index entry count: ${INDEX_COUNT_BEFORE}"

header_quick "Compute the secondary index key to delete"
INDEX_KEY=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv <<SQL | awk 'NR==2 {print $1}'
WITH key_vals AS (
  SELECT ROW(${COL_SELECT_LIST}) AS key_tuple
  FROM ${DATABASE_NAME}.${TABLE_NAME}
  WHERE id = '${VEHICLE_ID}'
  LIMIT 1
)
SELECT encode(
  crdb_internal.encode_key(
    t.table_id,
    i.index_id,
    (SELECT key_tuple FROM key_vals)
  ),
  'base64'
) AS idx_key
FROM crdb_internal.tables AS t
JOIN crdb_internal.table_indexes AS i
  ON i.descriptor_id = t.table_id
WHERE t.database_name = '${DATABASE_NAME}'
  AND t.name = '${TABLE_NAME}'
  AND i.index_id = ${INDEX_ID}
LIMIT 1;
SQL
)

if [[ -z "$INDEX_KEY" ]]; then
  echo "Failed to derive the index key to delete. Aborting." >&2
  exit 1
fi
echo "Encoded key (base64): $INDEX_KEY"

HEX_KEY=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv <<SQL | awk 'NR==2 {print $1}'
SELECT encode(decode('${INDEX_KEY}', 'base64'), 'hex');
SQL
)
PRETTY_KEY=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv <<SQL | awk 'NR==2 {print $1}'
SELECT crdb_internal.pretty_key(decode('${INDEX_KEY}', 'base64'), 0);
SQL
)
echo "Encoded key (hex): $HEX_KEY"
INDEX_KEY_B64="$INDEX_KEY"
echo "Pretty key: $PRETTY_KEY"
echo "Index key length (base64 chars): ${#INDEX_KEY_B64}"

# Show current KV entries via a KV scan to aid debugging.
START_KEY_BASE64="$INDEX_KEY_B64"
SCAN_RANGE_OUTPUT=$(python3 -c '
import base64, sys
if len(sys.argv) < 2:
    raise SystemExit("no key argument passed")
prefix_b64 = sys.argv[1].strip()
if not prefix_b64:
    raise SystemExit("encoded key is empty")
prefix = base64.b64decode(prefix_b64)
end = prefix + b"\xff"
print(prefix_b64, base64.b64encode(end).decode())
' "$INDEX_KEY_B64" 2>&1)
if [[ $? -ne 0 ]]; then
  echo "Failed to compute scan range." >&2
  echo "$SCAN_RANGE_OUTPUT" >&2
  exit 1
fi
read SCAN_START_B64 SCAN_END_B64 <<<"$SCAN_RANGE_OUTPUT"

header_quick "Current KV entries for this index key (via debug scan)"
SCAN_FILE=$(mktemp inspect-demo-kvscan-XXXXXX.json)
cat > "$SCAN_FILE" <<EOF
{"requests":[{"scan":{"header":{"key":"$SCAN_START_B64","endKey":"$SCAN_END_B64"}}}]}
EOF
echo "$COCKROACH_BIN debug send-kv-batch --url ${KV_URL} ${KV_FLAGS_ARR[*]} $SCAN_FILE"
"$COCKROACH_BIN" debug send-kv-batch --url "$KV_URL" "${KV_FLAGS_ARR[@]}" "$SCAN_FILE"

# Generate the exact KV keys for column families 0 and 1 by emulating MakeFamilyKey.
if ! DELETE_KEYS=$(python3 -c '
import base64, sys
INT_ZERO = 0x88
def make_family_key(prefix: bytes, fam_id: int) -> bytes:
    if fam_id == 0:
        return prefix + bytes([INT_ZERO])
    # fam_id encode (assumes fam_id <= intSmall).
    encoded_fam = bytes([INT_ZERO + fam_id])
    tmp = prefix + encoded_fam
    length = len(tmp) - len(prefix)
    encoded_len = bytes([INT_ZERO + length])
    return tmp + encoded_len

if len(sys.argv) < 2:
    raise SystemExit("no key argument passed")
prefix_b64 = sys.argv[1].strip()
if not prefix_b64:
    raise SystemExit("encoded key is empty")
try:
    prefix = base64.b64decode(prefix_b64)
except Exception as exc:
    raise SystemExit(f"base64 decode error: {exc}") from exc
fam0 = base64.b64encode(make_family_key(prefix, 0)).decode()
fam1 = base64.b64encode(make_family_key(prefix, 1)).decode()
print(fam0, fam1)
' "$INDEX_KEY_B64" 2>&1); then
  echo "Failed to compute KV keys for deletion." >&2
  echo "Input key (base64): $INDEX_KEY_B64" >&2
  echo "Python output:" >&2
  echo "$DELETE_KEYS" >&2
  exit 1
fi
read KV_KEY_FAM0 KV_KEY_FAM1 <<<"$DELETE_KEYS"

echo "KV key family0 (base64): $KV_KEY_FAM0"
echo "KV key family1 (base64): $KV_KEY_FAM1"

BATCH_FILE=$(mktemp inspect-demo-kv-XXXXXX.json)
cat > "$BATCH_FILE" <<EOF
{"requests":[
  {"delete":{"header":{"key":"$KV_KEY_FAM0"}}},
  {"delete":{"header":{"key":"$KV_KEY_FAM1"}}}
]}
EOF
echo "Wrote delete batch to $BATCH_FILE"

header "Delete index entry via cockroach debug send-kv-batch"
if [[ -n "${KV_HOST:-}" ]]; then
  echo "$COCKROACH_BIN debug send-kv-batch --host ${KV_HOST} --port ${KV_PORT} ${KV_FLAGS_ARR[*]} $BATCH_FILE"
  "$COCKROACH_BIN" debug send-kv-batch --host "$KV_HOST" --port "$KV_PORT" "${KV_FLAGS_ARR[@]}" "$BATCH_FILE"
else
  echo "$COCKROACH_BIN debug send-kv-batch --url ${KV_URL} ${KV_FLAGS_ARR[*]} $BATCH_FILE"
  "$COCKROACH_BIN" debug send-kv-batch --url "$KV_URL" "${KV_FLAGS_ARR[@]}" "$BATCH_FILE"
fi

header "After corruption: primary key scan still finds the vehicle (forced primary index)"
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table --echo-sql <<SQL
SELECT id, city, owner_id
FROM ${DATABASE_NAME}.${TABLE_NAME}@{FORCE_INDEX=primary}
WHERE id = '${VEHICLE_ID}';
SQL

header "After corruption: forced secondary index scan misses the vehicle"
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table --echo-sql <<SQL
SELECT id, city, owner_id
FROM ${DATABASE_NAME}.${TABLE_NAME}@{FORCE_INDEX=${INDEX_NAME}}
WHERE ROW(${COL_SELECT_LIST})
    = (SELECT ROW(${COL_SELECT_LIST})
       FROM ${DATABASE_NAME}.${TABLE_NAME}
       WHERE id = '${VEHICLE_ID}');
SQL

header_quick "Post-corruption raw index scan entry"
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table --echo-sql <<SQL
SELECT city, owner_id, id
FROM ${DATABASE_NAME}.${TABLE_NAME}@${INDEX_NAME}
WHERE city = '${ESCAPED_CITY}'
  AND owner_id = '${OWNER_ID}'::UUID;
SQL

INDEX_COUNT_AFTER=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv <<SQL | awk 'NR==2 {print $1}'
SELECT count(*)
FROM ${DATABASE_NAME}.${TABLE_NAME}@${INDEX_NAME}
WHERE city = '${ESCAPED_CITY}'
  AND owner_id = '${OWNER_ID}'::UUID
  AND id = '${VEHICLE_ID}'::UUID;
SQL
)

if [[ "${INDEX_COUNT_AFTER}" == "0" ]]; then
  echo "Index entry removed from ${TABLE_NAME}@${INDEX_NAME} (corruption confirmed)."
else
  echo "WARNING: Index entry still present in ${TABLE_NAME}@${INDEX_NAME} (count=${INDEX_COUNT_AFTER})."
fi

header "Run INSPECT to surface the inconsistency"
set +e
INSPECT_OUTPUT=$(
  "$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=table --echo-sql 2>&1 <<SQL
SET enable_inspect_command = true;
INSPECT TABLE ${DATABASE_NAME}.${TABLE_NAME} WITH OPTIONS INDEX (${INDEX_NAME});
SQL
)
set -e
echo "$INSPECT_OUTPUT" | awk '
/^ERROR: INSPECT found inconsistencies$/ {if (++seen_err > 1) next}
/^SQLSTATE: 22000$/ {if (++seen_state > 1) next}
/^HINT: Run '\''SHOW INSPECT ERRORS/ {if (++seen_hint > 1) next}
{print}'

JOB_ID=$("$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=tsv <<SQL | awk 'NR==2 {print $1}'
SELECT job_id
FROM [SHOW JOBS]
WHERE job_type = 'INSPECT'
ORDER BY created DESC
LIMIT 1;
SQL
)

if [[ -z "$JOB_ID" ]]; then
  echo "Failed to capture INSPECT job ID. Aborting." >&2
  exit 1
fi
echo "Latest INSPECT job id: $JOB_ID"

header "Wait for INSPECT job $JOB_ID to finish"
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=records --echo-sql --execute "SHOW JOB WHEN COMPLETE $JOB_ID;"

header "SHOW INSPECT ERRORS for job $JOB_ID (filtered to ${DATABASE_NAME}.${TABLE_NAME})"
"$COCKROACH_BIN" sql --url "$INSPECT_DEMO_URL" --format=records --echo-sql <<SQL
SET enable_inspect_command = true;
SHOW INSPECT ERRORS FOR TABLE ${DATABASE_NAME}.${TABLE_NAME} FOR JOB $JOB_ID WITH DETAILS;
SQL

echo
echo "Done. The kv-batch payload remains at: $BATCH_FILE"
