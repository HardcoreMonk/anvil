#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  cat >&2 <<'USAGE'
Usage: sudo -n bash scripts/vm-workload-e2e.sh

Environment:
  ANVIL_WORKLOAD_API=http://127.0.0.1:3000
  ANVIL_WORKLOAD_REUSE_DAEMON=1
  ANVIL_WORKLOAD_ARTIFACT_DIR=/tmp/anvil-workload-e2e-custom
  ANVIL_WORKLOAD_TASK_TIMEOUT=900
  ANVIL_WORKLOAD_API_TOKEN=optional bearer token for existing daemon auth
USAGE
}

if [[ $# -eq 1 && ( "${1:-}" == "-h" || "${1:-}" == "--help" ) ]]; then
  usage
  exit 0
fi
if [[ $# -gt 0 ]]; then
  usage
  exit 2
fi

API="${ANVIL_WORKLOAD_API:-http://127.0.0.1:3000}"
API_TOKEN="${ANVIL_WORKLOAD_API_TOKEN:-}"
REUSE_DAEMON="${ANVIL_WORKLOAD_REUSE_DAEMON:-0}"
TASK_TIMEOUT="${ANVIL_WORKLOAD_TASK_TIMEOUT:-900}"
ARTIFACT_DIR="${ANVIL_WORKLOAD_ARTIFACT_DIR:-/tmp/anvil-workload-e2e-$(date +%Y%m%d-%H%M%S)}"
CURL_AUTH_ARGS=()
if [ -n "$API_TOKEN" ]; then
  CURL_AUTH_ARGS=(-H "Authorization: Bearer $API_TOKEN")
fi
PASS=true
FAIL_REASONS=()
VM_ID=""
VM_IP=""
DAEMON_PID=""
STARTED_DAEMON=0
NGINX_STATUS=""
GO_STATUS=""
HOST_BENCH_TOOL="skipped"

mkdir -p "$ARTIFACT_DIR"

step() { printf '\n━━━ %s ━━━\n' "$*"; }
ok() { printf '  ✓ %s\n' "$*"; }
fail() {
  printf '  ✗ %s\n' "$*"
  PASS=false
  FAIL_REASONS+=("$1")
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    fail "preflight_failed: missing $cmd"
    return 1
  fi
}

write_summary() {
  local reasons_json
  if [ "${#FAIL_REASONS[@]}" -eq 0 ]; then
    reasons_json='[]'
  else
    reasons_json="$(printf '%s\n' "${FAIL_REASONS[@]}" | jq -R . | jq -s .)"
  fi

  jq -n \
    --argjson pass "$PASS" \
    --argjson reasons "$reasons_json" \
    --arg artifact_dir "$ARTIFACT_DIR" \
    --arg api "$API" \
    --arg vm_id "$VM_ID" \
    --arg guest_ip "$VM_IP" \
    --arg nginx_status "$NGINX_STATUS" \
    --arg go_status "$GO_STATUS" \
    --arg host_benchmark_tool "$HOST_BENCH_TOOL" \
    --arg finished_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{
      pass: $pass,
      failure_reasons: $reasons,
      artifact_dir: $artifact_dir,
      api: $api,
      vm_id: $vm_id,
      guest_ip: $guest_ip,
      nginx_http_status: $nginx_status,
      go_http_status: $go_status,
      host_benchmark_tool: $host_benchmark_tool,
      finished_at: $finished_at
    }' >"$ARTIFACT_DIR/summary.json"
}

cleanup() {
  local status=$?
  trap - EXIT

  step "Cleanup"
  if [ -n "$VM_ID" ]; then
    local delete_code
    delete_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o "$ARTIFACT_DIR/delete-vm.json" -w "%{http_code}" -X DELETE "$API/vms/$VM_ID" || true)"
    if [ "$delete_code" = "200" ]; then
      ok "Deleted VM $VM_ID"
    else
      fail "cleanup_failed: DELETE /vms/$VM_ID returned $delete_code"
    fi
  fi

  if [ "$STARTED_DAEMON" = "1" ] && [ -n "$DAEMON_PID" ]; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    ok "Stopped daemon PID $DAEMON_PID"
  fi

  write_summary
  printf '  Artifact directory: %s\n' "$ARTIFACT_DIR"
  if [ "$PASS" != "true" ] || [ "$status" -ne 0 ]; then
    exit 1
  fi
  exit 0
}

upload_workspace_file() {
  local src="$1"
  local dst="$2"
  local code
  code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o "$ARTIFACT_DIR/upload-$(basename "$dst").json" -w "%{http_code}" \
    -X PUT "$API/vms/$VM_ID/workspace?path=$dst&overwrite=true" \
    --data-binary @"$src" || true)"
  if [ "$code" = "200" ]; then
    ok "Uploaded $dst"
  else
    fail "workspace_upload_failed: $dst returned HTTP $code"
  fi
}

fetch_workspace_file() {
  local src="$1"
  local dst="$2"
  local code
  code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o "$ARTIFACT_DIR/$dst" -w "%{http_code}" \
    "$API/vms/$VM_ID/workspace?path=$src" || true)"
  if [ "$code" = "200" ]; then
    ok "Fetched $src"
  else
    fail "task_failed: could not fetch $src, HTTP $code"
  fi
}

host_probe() {
  local label="$1"
  local url="$2"
  local marker="$3"
  local output="$ARTIFACT_DIR/${label}.body"
  local reason="nginx_host_probe_failed"
  local code
  code="$(curl -sS --connect-timeout 5 --max-time 10 -o "$output" -w "%{http_code}" "$url" || true)"
  if [ "$label" = "nginx" ]; then
    NGINX_STATUS="$code"
  else
    GO_STATUS="$code"
    reason="go_host_probe_failed"
  fi

  if [ "$code" = "200" ] && grep -q "$marker" "$output"; then
    ok "Host probe $label returned HTTP 200 with expected marker"
  else
    fail "$reason: HTTP $code from $url"
  fi
}

host_benchmark() {
  local url="http://$VM_IP:18080/"
  local out="$ARTIFACT_DIR/host-bench.txt"

  if command -v hey >/dev/null 2>&1; then
    HOST_BENCH_TOOL="hey"
    hey -z 5s -c 10 "$url" >"$out" 2>&1 || fail "benchmark_failed: hey"
    return
  fi
  if command -v wrk >/dev/null 2>&1; then
    HOST_BENCH_TOOL="wrk"
    wrk -t2 -c10 -d5s "$url" >"$out" 2>&1 || fail "benchmark_failed: wrk"
    return
  fi
  if command -v ab >/dev/null 2>&1; then
    HOST_BENCH_TOOL="ab"
    ab -n 100 -c 10 "$url" >"$out" 2>&1 || fail "benchmark_failed: ab"
    return
  fi

  HOST_BENCH_TOOL="curl-loop"
  {
    printf 'tool=curl-loop\n'
    start_ns="$(date +%s%N)"
    ok_count=0
    failed_count=0
    for _ in $(seq 1 50); do
      if curl -fsS --connect-timeout 5 --max-time 10 "$url" >/dev/null; then
        ok_count=$((ok_count + 1))
      else
        failed_count=$((failed_count + 1))
      fi
    done
    end_ns="$(date +%s%N)"
    duration_ms=$(((end_ns - start_ns) / 1000000))
    printf 'requests=50\n'
    printf 'ok=%s\n' "$ok_count"
    printf 'failed=%s\n' "$failed_count"
    printf 'duration_ms=%s\n' "$duration_ms"
  } >"$out"
  if [ "$ok_count" = "50" ] && [ "$failed_count" = "0" ]; then
    ok "Host benchmark completed with curl-loop"
  else
    fail "benchmark_failed: curl-loop ok=$ok_count failed=$failed_count"
  fi
}

step "Preflight"
require_cmd curl || exit 1
require_cmd jq || exit 1
trap cleanup EXIT

if [ "$REUSE_DAEMON" != "1" ]; then
  if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    fail "preflight_failed: root required when starting daemon"
    exit 1
  fi
  if [ ! -e /dev/kvm ]; then
    fail "preflight_failed: /dev/kvm missing"
    exit 1
  fi
  if [ ! -x ./anvil-daemon ]; then
    require_cmd go || exit 1
    go build -o anvil-daemon ./cmd/goose-daemon
  fi
fi
ok "Preflight complete"

step "Start daemon"
if [ "$REUSE_DAEMON" = "1" ]; then
  ok "Using existing daemon at $API"
else
  EPHEMERA_API_ADDR=0.0.0.0:3000 ./anvil-daemon >"$ARTIFACT_DIR/daemon.log" 2>&1 &
  DAEMON_PID=$!
  STARTED_DAEMON=1
  ok "Daemon PID $DAEMON_PID"
fi

for attempt in $(seq 1 120); do
  if curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null "$API/vms" 2>/dev/null; then
    ok "Control plane API ready"
    break
  fi
  if [ "$STARTED_DAEMON" = "1" ] && ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    fail "daemon_unavailable: daemon exited early"
    exit 1
  fi
  if [ "$attempt" = "120" ]; then
    fail "daemon_unavailable: API not ready after 120s"
    exit 1
  fi
  sleep 1
done

step "Create workload VM"
vm_resp="$(curl -sS "${CURL_AUTH_ARGS[@]}" -w "\n%{http_code}" -X POST "$API/vms" -H "Content-Type: application/json" || true)"
vm_code="$(printf '%s\n' "$vm_resp" | tail -1)"
vm_body="$(printf '%s\n' "$vm_resp" | sed '$d')"
if [ "$vm_code" != "201" ]; then
  fail "vm_create_failed: POST /vms returned HTTP $vm_code"
  exit 1
fi
VM_ID="$(printf '%s' "$vm_body" | jq -r '.vm_id')"
VM_IP="$(printf '%s' "$vm_body" | jq -r '.guest_ip')"
if [ -z "$VM_ID" ] || [ "$VM_ID" = "null" ] || [ -z "$VM_IP" ] || [ "$VM_IP" = "null" ]; then
  fail "vm_create_failed: response missing vm_id or guest_ip"
  exit 1
fi
ok "Created VM $VM_ID at $VM_IP"

step "Upload workload files"
upload_workspace_file scripts/workloads/nginx-smoke.sh workloads/nginx-smoke.sh
upload_workspace_file scripts/workloads/go-http-server.go workloads/go-http-server.go
upload_workspace_file scripts/workloads/go-http-bench.sh workloads/go-http-bench.sh

step "Run VM workload task"
task_prompt="$(cat <<'PROMPT'
Run the uploaded workload scripts exactly as shell commands inside this VM.

Commands:
chmod +x /workspace/workloads/nginx-smoke.sh /workspace/workloads/go-http-bench.sh
bash /workspace/workloads/nginx-smoke.sh
bash /workspace/workloads/go-http-bench.sh

After the commands complete, print the marker lines from the scripts and the final 20 lines of each workload log. Do not print any secret, token, or config file content.
PROMPT
)"
task_payload="$(jq -n --arg prompt "$task_prompt" '{prompt: $prompt}')"
task_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" --max-time "$TASK_TIMEOUT" -o "$ARTIFACT_DIR/task-output.json" -w "%{http_code}" \
  -X POST "$API/vms/$VM_ID/tasks" \
  -H "Content-Type: application/json" \
  -d "$task_payload" || true)"
if [ "$task_code" != "200" ]; then
  fail "task_failed: workload task returned HTTP $task_code"
fi

task_output="$(jq -r '.output // ""' "$ARTIFACT_DIR/task-output.json" 2>/dev/null || true)"
for marker in ANVIL_WORKLOAD_NGINX_READY ANVIL_WORKLOAD_GO_HTTP_READY ANVIL_WORKLOAD_BENCH_DONE; do
  if printf '%s' "$task_output" | grep -q "$marker"; then
    ok "Task output contains $marker"
  else
    fail "task_failed: missing marker $marker"
  fi
done

step "Fetch VM workload logs"
fetch_workspace_file workloads/results/nginx.log nginx.log
fetch_workspace_file workloads/results/go-http.log go-http.log
fetch_workspace_file workloads/results/bench.txt bench.txt

step "Host probes"
host_probe nginx "http://$VM_IP/" "anvil-nginx-ok"
host_probe go-http "http://$VM_IP:18080/health" '"status":"ok"'

step "Host benchmark"
host_benchmark

if [ "$PASS" = "true" ]; then
  step "Result"
  ok "VM workload E2E passed"
  exit 0
fi

step "Result"
printf '  Failure reasons:\n'
printf '  - %s\n' "${FAIL_REASONS[@]}"
exit 1
