#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: bash scripts/anvil-scheduler-smoke.sh [--base-url URL] [--host-id ID] [--json-out PATH]

Validates a running anvil scheduler HTTP API.

Required commands:
  curl
  python3

Environment:
  ANVIL_SCHEDULER_BASE_URL=http://127.0.0.1:3010
  ANVIL_SCHEDULER_CONNECT_TIMEOUT=2
  ANVIL_SCHEDULER_REQUEST_TIMEOUT=10
USAGE
}

BASE_URL="${ANVIL_SCHEDULER_BASE_URL:-http://127.0.0.1:3010}"
CONNECT_TIMEOUT="${ANVIL_SCHEDULER_CONNECT_TIMEOUT:-2}"
REQUEST_TIMEOUT="${ANVIL_SCHEDULER_REQUEST_TIMEOUT:-10}"
HOST_ID="smoke-host-1"
JSON_OUT=""
SELECTED_HOST_ID=""
TMP_DIR=""
SMOKE_HOST_REGISTERED=0
SMOKE_HOST_PREEXISTING=0
ORIGINAL_HOST_BODY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-url)
      if [[ $# -lt 2 ]]; then
        usage
        exit 2
      fi
      BASE_URL="$2"
      shift 2
      ;;
    --host-id)
      if [[ $# -lt 2 ]]; then
        usage
        exit 2
      fi
      HOST_ID="$2"
      shift 2
      ;;
    --json-out)
      if [[ $# -lt 2 ]]; then
        usage
        exit 2
      fi
      JSON_OUT="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

BASE_URL="${BASE_URL%/}"

cleanup() {
  local original_status=$?
  local cleanup_failed=0

  if [[ "$SMOKE_HOST_REGISTERED" == "1" ]]; then
    if ! cleanup_registered_host; then
      cleanup_failed=1
    fi
  fi

  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi

  if [[ "$original_status" == "0" && "$cleanup_failed" == "1" ]]; then
    if ! write_summary "false" "host_cleanup_failed"; then
      printf 'json_write_failed: could not write summary to %s\n' "$JSON_OUT" >&2
    fi
    exit 1
  fi

  exit "$original_status"
}
trap cleanup EXIT

require_command() {
  local name="$1"
  if ! command -v "$name" >/dev/null 2>&1; then
    printf 'missing required command: %s (required commands: curl, python3)\n' "$name" >&2
    exit 2
  fi
}

write_summary() {
  local ok="$1"
  local failed_step="${2:-}"

  if [[ -z "$JSON_OUT" ]]; then
    return 0
  fi

  python3 - "$JSON_OUT" "$ok" "$BASE_URL" "$HOST_ID" "$SELECTED_HOST_ID" "$failed_step" <<'PY'
import json
import os
import sys

path, ok, base_url, host_id, selected_host_id, failed_step = sys.argv[1:7]
summary = {
    "ok": ok == "true",
    "base_url": base_url,
    "host_id": host_id,
    "selected_host_id": selected_host_id,
}
if failed_step:
    summary["failed_step"] = failed_step

directory = os.path.dirname(path)
if directory:
    os.makedirs(directory, exist_ok=True)
with open(path, "w", encoding="utf-8") as handle:
    json.dump(summary, handle, sort_keys=True)
    handle.write("\n")
PY
}

fail_step() {
  local step="$1"
  local message="$2"

  printf '%s: %s\n' "$step" "$message" >&2
  if ! write_summary "false" "$step"; then
    printf 'json_write_failed: could not write summary to %s\n' "$JSON_OUT" >&2
  fi
  exit 1
}

request_json() {
  local method="$1"
  local path="$2"
  local body_file="$3"
  local output_file="$4"
  local error_file="$5"
  local url="${BASE_URL}${path}"
  local status

  if [[ -n "$body_file" ]]; then
    status="$(curl -sS -o "$output_file" -w '%{http_code}' \
      --connect-timeout "$CONNECT_TIMEOUT" \
      --max-time "$REQUEST_TIMEOUT" \
      -X "$method" \
      -H 'Content-Type: application/json' \
      --data-binary "@${body_file}" \
      "$url" 2>"$error_file")" || return 1
  else
    status="$(curl -sS -o "$output_file" -w '%{http_code}' \
      --connect-timeout "$CONNECT_TIMEOUT" \
      --max-time "$REQUEST_TIMEOUT" \
      -X "$method" \
      "$url" 2>"$error_file")" || return 1
  fi

  printf '%s' "$status"
}

response_body_snippet() {
  local path="$1"

  python3 - "$path" <<'PY'
import sys

path = sys.argv[1]
try:
    with open(path, "rb") as handle:
        data = handle.read(500)
except OSError:
    data = b""
text = data.decode("utf-8", errors="replace").replace("\n", "\\n")
print(text)
PY
}

urlencode_path_segment() {
  local value="$1"

  python3 - "$value" <<'PY'
import sys
from urllib.parse import quote

print(quote(sys.argv[1], safe=""))
PY
}

cleanup_registered_host() {
  local encoded_host_id
  local cleanup_body
  local cleanup_err
  local cleanup_status

  if [[ "$SMOKE_HOST_PREEXISTING" == "1" ]]; then
    cleanup_body="$TMP_DIR/host_restore.json"
    cleanup_err="$TMP_DIR/host_restore.err"
    if ! cleanup_status="$(request_json PUT /hosts "$ORIGINAL_HOST_BODY" "$cleanup_body" "$cleanup_err")"; then
      printf 'host_cleanup_failed: PUT /hosts restore request failed: %s\n' "$(<"$cleanup_err")" >&2
      return 1
    fi
    if [[ "$cleanup_status" != "200" ]]; then
      printf 'host_cleanup_failed: PUT /hosts restore returned HTTP %s body=%s\n' "$cleanup_status" "$(response_body_snippet "$cleanup_body")" >&2
      return 1
    fi
  else
    encoded_host_id="$(urlencode_path_segment "$HOST_ID")"
    cleanup_body="$TMP_DIR/host_delete.json"
    cleanup_err="$TMP_DIR/host_delete.err"
    if ! cleanup_status="$(request_json DELETE "/hosts/${encoded_host_id}" "" "$cleanup_body" "$cleanup_err")"; then
      printf 'host_cleanup_failed: DELETE /hosts/%s request failed: %s\n' "$encoded_host_id" "$(<"$cleanup_err")" >&2
      return 1
    fi
    if [[ "$cleanup_status" != "200" ]]; then
      printf 'host_cleanup_failed: DELETE /hosts/%s returned HTTP %s body=%s\n' "$encoded_host_id" "$cleanup_status" "$(response_body_snippet "$cleanup_body")" >&2
      return 1
    fi
  fi

  SMOKE_HOST_REGISTERED=0
  return 0
}

require_json_status_ok() {
  local path="$1"

  python3 - "$path" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
if not isinstance(value, dict) or value.get("status") != "ok":
    raise SystemExit(1)
PY
}

require_host_in_list() {
  local path="$1"
  local host_id="$2"

  python3 - "$path" "$host_id" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
host_id = sys.argv[2]
if not isinstance(value, list):
    raise SystemExit(1)
for host in value:
    if isinstance(host, dict) and host.get("name") == host_id:
        raise SystemExit(0)
raise SystemExit(1)
PY
}

existing_host_from_list() {
  local path="$1"
  local host_id="$2"
  local output_json="$3"

  python3 - "$path" "$host_id" "$output_json" <<'PY'
import json
import sys

path, host_id, output_json = sys.argv[1:4]
try:
    with open(path, encoding="utf-8") as handle:
        value = json.load(handle)
except (OSError, json.JSONDecodeError):
    raise SystemExit(2)
if not isinstance(value, list):
    raise SystemExit(2)
for host in value:
    if isinstance(host, dict) and host.get("name") == host_id:
        try:
            with open(output_json, "w", encoding="utf-8") as output:
                json.dump(host, output)
                output.write("\n")
        except OSError:
            raise SystemExit(2)
        raise SystemExit(0)
raise SystemExit(1)
PY
}

selected_host_from_decision() {
  local path="$1"

  python3 - "$path" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
if not isinstance(value, dict) or value.get("allowed") is not True:
    raise SystemExit(1)
host = value.get("host")
if not isinstance(host, dict):
    raise SystemExit(1)
name = str(host.get("name") or "").strip()
if not name:
    raise SystemExit(1)
print(name)
PY
}

require_json_object() {
  local path="$1"

  python3 - "$path" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)
if not isinstance(value, dict):
    raise SystemExit(1)
PY
}

write_request_bodies() {
  local host_body="$1"
  local schedule_body="$2"

  python3 - "$host_body" "$schedule_body" "$HOST_ID" <<'PY'
import json
import sys

host_body, schedule_body, host_id = sys.argv[1:4]
host = {
    "name": host_id,
    "endpoint": "http://127.0.0.1:0",
    "healthy": True,
    "available_vms": 1,
    "available_snapshot_bytes": 1073741824,
    "egress_policies": ["profile", "deny_all", "allow_all"],
}
schedule = {
    "tenant_id": "smoke-tenant",
    "egress_policy": "profile",
    "preferred_hosts": [host_id],
    "requested": {"active_vms": 1},
}
with open(host_body, "w", encoding="utf-8") as handle:
    json.dump(host, handle)
with open(schedule_body, "w", encoding="utf-8") as handle:
    json.dump(schedule, handle)
PY
}

require_command curl
require_command python3

TMP_DIR="$(mktemp -d)"
HOST_BODY="$TMP_DIR/host.json"
SCHEDULE_BODY="$TMP_DIR/schedule.json"
ORIGINAL_HOST_BODY="$TMP_DIR/original_host.json"
write_request_bodies "$HOST_BODY" "$SCHEDULE_BODY"

HEALTH_BODY="$TMP_DIR/health.json"
HEALTH_ERR="$TMP_DIR/health.err"
if ! HEALTH_STATUS="$(request_json GET /health "" "$HEALTH_BODY" "$HEALTH_ERR")"; then
  fail_step health_failed "GET /health request failed: $(<"$HEALTH_ERR")"
fi
if [[ "$HEALTH_STATUS" != "200" ]]; then
  fail_step health_failed "GET /health returned HTTP $HEALTH_STATUS body=$(response_body_snippet "$HEALTH_BODY")"
fi
if ! require_json_status_ok "$HEALTH_BODY" 2>/dev/null; then
  fail_step health_failed 'GET /health response did not include status ok'
fi

HOST_PREFLIGHT_BODY="$TMP_DIR/host_list_preflight.json"
HOST_PREFLIGHT_ERR="$TMP_DIR/host_list_preflight.err"
if ! HOST_PREFLIGHT_STATUS="$(request_json GET /hosts "" "$HOST_PREFLIGHT_BODY" "$HOST_PREFLIGHT_ERR")"; then
  fail_step host_list_failed "GET /hosts request failed: $(<"$HOST_PREFLIGHT_ERR")"
fi
if [[ "$HOST_PREFLIGHT_STATUS" != "200" ]]; then
  fail_step host_list_failed "GET /hosts returned HTTP $HOST_PREFLIGHT_STATUS body=$(response_body_snippet "$HOST_PREFLIGHT_BODY")"
fi
if existing_host_from_list "$HOST_PREFLIGHT_BODY" "$HOST_ID" "$ORIGINAL_HOST_BODY"; then
  SMOKE_HOST_PREEXISTING=1
else
  EXISTING_HOST_STATUS=$?
  if [[ "$EXISTING_HOST_STATUS" != "1" ]]; then
    fail_step host_list_failed 'GET /hosts response did not include a JSON host list'
  fi
fi

HOST_PUT_BODY="$TMP_DIR/host_put.json"
HOST_PUT_ERR="$TMP_DIR/host_put.err"
if ! HOST_PUT_STATUS="$(request_json PUT /hosts "$HOST_BODY" "$HOST_PUT_BODY" "$HOST_PUT_ERR")"; then
  fail_step host_put_failed "PUT /hosts request failed: $(<"$HOST_PUT_ERR")"
fi
if [[ "$HOST_PUT_STATUS" != "200" ]]; then
  fail_step host_put_failed "PUT /hosts returned HTTP $HOST_PUT_STATUS body=$(response_body_snippet "$HOST_PUT_BODY")"
fi
SMOKE_HOST_REGISTERED=1

HOST_LIST_BODY="$TMP_DIR/host_list.json"
HOST_LIST_ERR="$TMP_DIR/host_list.err"
if ! HOST_LIST_STATUS="$(request_json GET /hosts "" "$HOST_LIST_BODY" "$HOST_LIST_ERR")"; then
  fail_step host_list_failed "GET /hosts request failed: $(<"$HOST_LIST_ERR")"
fi
if [[ "$HOST_LIST_STATUS" != "200" ]]; then
  fail_step host_list_failed "GET /hosts returned HTTP $HOST_LIST_STATUS body=$(response_body_snippet "$HOST_LIST_BODY")"
fi
if ! require_host_in_list "$HOST_LIST_BODY" "$HOST_ID" 2>/dev/null; then
  fail_step host_list_failed "GET /hosts did not include host $HOST_ID"
fi

SCHEDULE_BODY_OUT="$TMP_DIR/schedule_spawn.json"
SCHEDULE_ERR="$TMP_DIR/schedule_spawn.err"
if ! SCHEDULE_STATUS="$(request_json POST /schedule/spawn "$SCHEDULE_BODY" "$SCHEDULE_BODY_OUT" "$SCHEDULE_ERR")"; then
  fail_step schedule_spawn_failed "POST /schedule/spawn request failed: $(<"$SCHEDULE_ERR")"
fi
if [[ "$SCHEDULE_STATUS" != "200" ]]; then
  fail_step schedule_spawn_failed "POST /schedule/spawn returned HTTP $SCHEDULE_STATUS body=$(response_body_snippet "$SCHEDULE_BODY_OUT")"
fi
if ! SELECTED_HOST_ID="$(selected_host_from_decision "$SCHEDULE_BODY_OUT" 2>/dev/null)"; then
  fail_step schedule_spawn_failed 'POST /schedule/spawn did not return an allowed host decision'
fi
if [[ "$SELECTED_HOST_ID" != "$HOST_ID" ]]; then
  fail_step schedule_spawn_failed "POST /schedule/spawn selected host $SELECTED_HOST_ID, want $HOST_ID"
fi

PLACEMENTS_BODY="$TMP_DIR/placements.json"
PLACEMENTS_ERR="$TMP_DIR/placements.err"
if ! PLACEMENTS_STATUS="$(request_json GET /placements "" "$PLACEMENTS_BODY" "$PLACEMENTS_ERR")"; then
  fail_step placements_failed "GET /placements request failed: $(<"$PLACEMENTS_ERR")"
fi
if [[ "$PLACEMENTS_STATUS" != "200" ]]; then
  fail_step placements_failed "GET /placements returned HTTP $PLACEMENTS_STATUS body=$(response_body_snippet "$PLACEMENTS_BODY")"
fi
if ! require_json_object "$PLACEMENTS_BODY" 2>/dev/null; then
  fail_step placements_failed 'GET /placements did not return a JSON object'
fi

if [[ "$SMOKE_HOST_REGISTERED" == "1" ]]; then
  if ! cleanup_registered_host; then
    SMOKE_HOST_REGISTERED=0
    if ! write_summary "false" "host_cleanup_failed"; then
      printf 'json_write_failed: could not write summary to %s\n' "$JSON_OUT" >&2
    fi
    exit 1
  fi
fi

if ! write_summary "true" ""; then
  printf 'json_write_failed: could not write summary to %s\n' "$JSON_OUT" >&2
  exit 1
fi

printf 'anvil scheduler smoke passed: base_url=%s host_id=%s selected_host_id=%s\n' "$BASE_URL" "$HOST_ID" "$SELECTED_HOST_ID"
