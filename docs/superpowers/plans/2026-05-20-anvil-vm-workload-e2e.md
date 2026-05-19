# Anvil VM Workload E2E Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a host-only KVM workload E2E that creates a VM, uploads workload assets, starts nginx and a Go HTTP server inside the VM, probes both services from the host, records benchmark artifacts, and cleans up the VM.

**Architecture:** Keep the existing `e2e_test.sh` lifecycle suite untouched and add a separate `scripts/vm-workload-e2e.sh` harness. The harness uses the existing daemon API surface: `POST /vms`, `PUT /vms/{vm_id}/workspace`, `POST /vms/{vm_id}/tasks`, `GET /vms/{vm_id}/workspace`, and `DELETE /vms/{vm_id}`. VM-internal scripts write marker lines and logs under `/workspace/workloads/results`, while the host harness collects those files and adds host-to-VM probes and benchmark output.

**Tech Stack:** Bash, Go standard library, `curl`, `jq`, existing anvil daemon HTTP API, Firecracker/KVM runtime, Debian package manager inside the guest.

---

## Scope Check

The approved spec covers one subsystem: VM workload E2E. The implementation should not add a guest `/exec` endpoint, change daemon API contracts, or alter existing full KVM lifecycle behavior.

## File Structure

- Create `scripts/workloads/nginx-smoke.sh`: VM-internal nginx install/start/probe script. Owns nginx package install, index page marker, local curl validation, and `nginx.log`.
- Create `scripts/workloads/go-http-server.go`: VM-internal HTTP test server. Owns `/health` and `/` responses on port `18080`.
- Create `scripts/workloads/go-http-bench.sh`: VM-internal Go toolchain check/install, Go server build/start, loopback benchmark, and `go-http.log`/`bench.txt`.
- Create `scripts/vm-workload-e2e.sh`: host-side orchestrator. Owns preflight, daemon lifecycle, VM lifecycle, workspace upload/download, task prompt, host probes, host benchmark, summary artifact, and cleanup.
- Modify `docs/operations/runbook.md`: add the new workload E2E command and artifact expectations.

## Task 1: Add VM-Internal Workload Assets

**Files:**
- Create: `scripts/workloads/nginx-smoke.sh`
- Create: `scripts/workloads/go-http-server.go`
- Create: `scripts/workloads/go-http-bench.sh`
- Test: `scripts/workloads/nginx-smoke.sh`
- Test: `scripts/workloads/go-http-server.go`
- Test: `scripts/workloads/go-http-bench.sh`

- [ ] **Step 1: Verify workload files are absent before creation**

Run:

```bash
test ! -e scripts/workloads/nginx-smoke.sh
test ! -e scripts/workloads/go-http-server.go
test ! -e scripts/workloads/go-http-bench.sh
```

Expected: all commands exit `0`.

- [ ] **Step 2: Create `scripts/workloads/nginx-smoke.sh`**

Create `scripts/workloads/nginx-smoke.sh` with this content:

```bash
#!/usr/bin/env bash
set -Eeuo pipefail

result_dir="/workspace/workloads/results"
log_file="$result_dir/nginx.log"
mkdir -p "$result_dir"
exec > >(tee "$log_file") 2>&1

export DEBIAN_FRONTEND=noninteractive

echo "[nginx] starting nginx workload smoke"
echo "[nginx] kernel: $(uname -a)"

if ! command -v apt-get >/dev/null 2>&1; then
  echo "[nginx] apt-get is required inside the VM" >&2
  exit 20
fi

apt-get update
apt-get install -y --no-install-recommends nginx curl ca-certificates

mkdir -p /var/www/html
printf '%s\n' 'anvil-nginx-ok' >/var/www/html/index.html

if command -v service >/dev/null 2>&1; then
  service nginx restart || true
fi

if ! pgrep -x nginx >/dev/null 2>&1; then
  nginx
fi

for attempt in $(seq 1 30); do
  if curl -fsS http://127.0.0.1/ | grep -q 'anvil-nginx-ok'; then
    echo "ANVIL_WORKLOAD_NGINX_READY"
    exit 0
  fi
  echo "[nginx] waiting for localhost response ($attempt/30)"
  sleep 1
done

echo "[nginx] failed to serve expected marker on localhost" >&2
if [ -f /var/log/nginx/error.log ]; then
  tail -50 /var/log/nginx/error.log >&2 || true
fi
exit 21
```

- [ ] **Step 3: Create `scripts/workloads/go-http-server.go`**

Create `scripts/workloads/go-http-server.go` with this content:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"
)

func main() {
	addr := ":18080"
	if v := os.Getenv("ANVIL_GO_HTTP_ADDR"); v != "" {
		addr = v
	}

	started := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"service":    "anvil-go-http",
			"status":     "ok",
			"go_version": runtime.Version(),
			"started_at": started.Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "anvil-go-http-ok")
	})

	log.Printf("anvil-go-http listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
```

- [ ] **Step 4: Create `scripts/workloads/go-http-bench.sh`**

Create `scripts/workloads/go-http-bench.sh` with this content:

```bash
#!/usr/bin/env bash
set -Eeuo pipefail

result_dir="/workspace/workloads/results"
log_file="$result_dir/go-http.log"
bench_file="$result_dir/bench.txt"
server_out="$result_dir/go-http-server.out"
pid_file="$result_dir/go-http-server.pid"
server_bin="/workspace/workloads/go-http-server"
server_src="/workspace/workloads/go-http-server.go"
requests="${ANVIL_WORKLOAD_REQUESTS:-50}"

mkdir -p "$result_dir"
exec > >(tee "$log_file") 2>&1

export DEBIAN_FRONTEND=noninteractive

echo "[go-http] starting Go HTTP workload"
echo "[go-http] kernel: $(uname -a)"

if ! command -v apt-get >/dev/null 2>&1; then
  echo "[go-http] apt-get is required inside the VM" >&2
  exit 30
fi

if ! command -v curl >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends curl ca-certificates
fi

if ! command -v go >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends golang-go
fi

go version
go build -o "$server_bin" "$server_src"

if pgrep -f "$server_bin" >/dev/null 2>&1; then
  pkill -f "$server_bin" || true
  sleep 1
fi

nohup "$server_bin" >"$server_out" 2>&1 &
server_pid=$!
printf '%s\n' "$server_pid" >"$pid_file"
echo "[go-http] started server pid=$server_pid"

for attempt in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:18080/health | grep -q '"status":"ok"'; then
    echo "ANVIL_WORKLOAD_GO_HTTP_READY"
    break
  fi
  echo "[go-http] waiting for localhost response ($attempt/30)"
  sleep 1
done

if ! curl -fsS http://127.0.0.1:18080/health | grep -q '"status":"ok"'; then
  echo "[go-http] server did not become ready" >&2
  tail -50 "$server_out" >&2 || true
  exit 31
fi

ok_count=0
fail_count=0
start_ns="$(date +%s%N)"
for _ in $(seq 1 "$requests"); do
  if curl -fsS http://127.0.0.1:18080/ >/dev/null; then
    ok_count=$((ok_count + 1))
  else
    fail_count=$((fail_count + 1))
  fi
done
end_ns="$(date +%s%N)"
duration_ms=$(((end_ns - start_ns) / 1000000))
if [ "$duration_ms" -le 0 ]; then
  duration_ms=1
fi

{
  printf 'tool=vm-curl-loop\n'
  printf 'requests=%s\n' "$requests"
  printf 'ok=%s\n' "$ok_count"
  printf 'failed=%s\n' "$fail_count"
  printf 'duration_ms=%s\n' "$duration_ms"
} | tee "$bench_file"

if [ "$fail_count" -ne 0 ]; then
  echo "[go-http] loopback benchmark had failed requests" >&2
  exit 32
fi

echo "ANVIL_WORKLOAD_BENCH_DONE"
```

- [ ] **Step 5: Make shell workload files executable**

Run:

```bash
chmod +x scripts/workloads/nginx-smoke.sh scripts/workloads/go-http-bench.sh
```

Expected: command exits `0`.

- [ ] **Step 6: Verify workload syntax and Go compilation**

Run:

```bash
bash -n scripts/workloads/nginx-smoke.sh
bash -n scripts/workloads/go-http-bench.sh
go test ./scripts/workloads
```

Expected: all commands exit `0`. The `go test` command prints a no-test-files style success for the workload package.

- [ ] **Step 7: Commit workload assets**

Run:

```bash
git add scripts/workloads/nginx-smoke.sh scripts/workloads/go-http-server.go scripts/workloads/go-http-bench.sh
git commit -m "test: add VM workload assets"
```

Expected: commit succeeds and includes only the three workload asset files.

## Task 2: Add Host-Side Workload E2E Harness

**Files:**
- Create: `scripts/vm-workload-e2e.sh`
- Test: `scripts/vm-workload-e2e.sh`

- [ ] **Step 1: Verify the host harness file is absent before creation**

Run:

```bash
test ! -e scripts/vm-workload-e2e.sh
```

Expected: command exits `0`.

- [ ] **Step 2: Create `scripts/vm-workload-e2e.sh`**

Create `scripts/vm-workload-e2e.sh` with this content:

```bash
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
USAGE
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
if [[ $# -gt 0 ]]; then
  usage
  exit 2
fi

API="${ANVIL_WORKLOAD_API:-http://127.0.0.1:3000}"
REUSE_DAEMON="${ANVIL_WORKLOAD_REUSE_DAEMON:-0}"
TASK_TIMEOUT="${ANVIL_WORKLOAD_TASK_TIMEOUT:-900}"
ARTIFACT_DIR="${ANVIL_WORKLOAD_ARTIFACT_DIR:-/tmp/anvil-workload-e2e-$(date +%Y%m%d-%H%M%S)}"
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
  if ! command -v jq >/dev/null 2>&1; then
    printf '{"pass":false,"failure_reasons":["preflight_failed: missing jq"],"artifact_dir":"%s","api":"%s","vm_id":"%s","guest_ip":"%s"}\n' \
      "$ARTIFACT_DIR" "$API" "$VM_ID" "$VM_IP" >"$ARTIFACT_DIR/summary.json"
    return
  fi

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
  step "Cleanup"
  if [ -n "$VM_ID" ]; then
    local delete_code
    delete_code="$(curl -sS -o "$ARTIFACT_DIR/delete-vm.json" -w "%{http_code}" -X DELETE "$API/vms/$VM_ID" || true)"
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
}
trap cleanup EXIT

upload_workspace_file() {
  local src="$1"
  local dst="$2"
  local code
  code="$(curl -sS -o "$ARTIFACT_DIR/upload-$(basename "$dst").json" -w "%{http_code}" \
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
  code="$(curl -sS -o "$ARTIFACT_DIR/$dst" -w "%{http_code}" \
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
    for _ in $(seq 1 50); do
      if curl -fsS "$url" >/dev/null; then
        ok_count=$((ok_count + 1))
      fi
    done
    end_ns="$(date +%s%N)"
    duration_ms=$(((end_ns - start_ns) / 1000000))
    printf 'requests=50\n'
    printf 'ok=%s\n' "$ok_count"
    printf 'duration_ms=%s\n' "$duration_ms"
  } >"$out"
  ok "Host benchmark completed with curl-loop"
}

step "Preflight"
require_cmd curl || exit 1
require_cmd jq || exit 1

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
  if curl -sS -o /dev/null "$API/vms" 2>/dev/null; then
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
vm_resp="$(curl -sS -w "\n%{http_code}" -X POST "$API/vms" -H "Content-Type: application/json" || true)"
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
task_code="$(curl -sS --max-time "$TASK_TIMEOUT" -o "$ARTIFACT_DIR/task-output.json" -w "%{http_code}" \
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
```

- [ ] **Step 3: Make the host harness executable**

Run:

```bash
chmod +x scripts/vm-workload-e2e.sh
```

Expected: command exits `0`.

- [ ] **Step 4: Verify host harness syntax and help output**

Run:

```bash
bash -n scripts/vm-workload-e2e.sh
bash scripts/vm-workload-e2e.sh --help
```

Expected: syntax check exits `0`, and the help command prints usage without requiring root.

- [ ] **Step 5: Commit host harness**

Run:

```bash
git add scripts/vm-workload-e2e.sh
git commit -m "test: add VM workload E2E harness"
```

Expected: commit succeeds and includes only `scripts/vm-workload-e2e.sh`.

## Task 3: Document Workload E2E Operation

**Files:**
- Modify: `docs/operations/runbook.md`
- Test: `docs/operations/runbook.md`

- [ ] **Step 1: Confirm runbook has the general verification section**

Run:

```bash
rg -n "## 일반 검증|sudo bash e2e_test.sh" docs/operations/runbook.md
```

Expected: output includes the existing general verification heading and full KVM E2E command.

- [ ] **Step 2: Add workload E2E runbook section**

Insert this section in `docs/operations/runbook.md` immediately after the existing full KVM E2E command block that ends with `sudo bash e2e_test.sh`:

````markdown
### VM workload E2E

VM 내부 서비스 설치, 기동, host-to-VM 접근, 기초 성능 artifact를 확인할 때는
workload E2E를 실행한다. 이 검증은 root/KVM/network 조건이 필요하며, VM 내부에서
`apt-get`을 사용하므로 outbound와 DNS 경로도 함께 검증한다.

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo -n bash scripts/vm-workload-e2e.sh
```

이미 daemon을 직접 띄운 상태에서 재사용하려면 다음처럼 실행한다.

```bash
ANVIL_WORKLOAD_REUSE_DAEMON=1 \
ANVIL_WORKLOAD_API=http://127.0.0.1:3000 \
bash scripts/vm-workload-e2e.sh
```

결과 artifact는 기본적으로 `/tmp/anvil-workload-e2e-<timestamp>/` 아래에 남는다.
핵심 파일은 `summary.json`, `task-output.json`, `nginx.log`, `go-http.log`,
`bench.txt`, `host-bench.txt`이다. 이 파일에는 provider token, API key,
control-plane token, agent token을 남기지 않는다.
````

- [ ] **Step 3: Verify runbook mentions the new command and artifacts**

Run:

```bash
rg -n "VM workload E2E|scripts/vm-workload-e2e.sh|summary.json|host-bench.txt" docs/operations/runbook.md
```

Expected: output includes all four search terms.

- [ ] **Step 4: Commit runbook update**

Run:

```bash
git add docs/operations/runbook.md
git commit -m "docs: document VM workload E2E"
```

Expected: commit succeeds and includes only the runbook update.

## Task 4: Verify Locally

**Files:**
- Verify: `scripts/workloads/nginx-smoke.sh`
- Verify: `scripts/workloads/go-http-server.go`
- Verify: `scripts/workloads/go-http-bench.sh`
- Verify: `scripts/vm-workload-e2e.sh`
- Verify: `docs/operations/runbook.md`

- [ ] **Step 1: Run static checks**

Run:

```bash
bash -n scripts/workloads/nginx-smoke.sh
bash -n scripts/workloads/go-http-bench.sh
bash -n scripts/vm-workload-e2e.sh
go test ./scripts/workloads
```

Expected: all commands exit `0`.

- [ ] **Step 2: Run repository tests**

Run:

```bash
go test ./...
```

Expected: command exits `0`.

- [ ] **Step 3: Build daemon**

Run:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
```

Expected: command exits `0` and refreshes `./anvil-daemon`.

- [ ] **Step 4: Run workload E2E on a KVM-capable host**

Run:

```bash
sudo -n bash scripts/vm-workload-e2e.sh
```

Expected: command exits `0`, prints `VM workload E2E passed`, and writes `/tmp/anvil-workload-e2e-<timestamp>/summary.json` with `"pass": true`.

- [ ] **Step 5: Inspect workload artifacts without printing secrets**

Run:

```bash
artifact_dir="$(ls -td /tmp/anvil-workload-e2e-* | head -1)"
jq '{pass, failure_reasons, vm_id, guest_ip, nginx_http_status, go_http_status, host_benchmark_tool}' "$artifact_dir/summary.json"
rg -n "ANVIL_WORKLOAD_|tool=|requests=|ok=|duration_ms=" "$artifact_dir"
```

Expected: summary shows `pass: true`, both HTTP statuses are `200`, and marker/benchmark lines appear in artifact files.

- [ ] **Step 6: Commit any verification-only doc corrections**

If verification reveals only runbook wording corrections, commit those corrections:

```bash
git add docs/operations/runbook.md
git commit -m "docs: clarify VM workload E2E"
```

Expected: commit succeeds only when the runbook changed. If no file changed, skip this step.

## Spec Coverage Map

- VM creation: Task 2 `POST /vms`.
- Workload upload: Task 2 workspace upload helper.
- nginx install/start/local probe: Task 1 `nginx-smoke.sh`.
- Go HTTP server build/start/local probe: Task 1 `go-http-server.go` and `go-http-bench.sh`.
- Host-to-VM probes: Task 2 `host_probe`.
- Benchmark artifact: Task 1 `bench.txt`, Task 2 `host-bench.txt`.
- Summary artifact: Task 2 `summary.json`.
- Secret redaction: Task 2 avoids writing VM create response and Task 3 documents artifact boundaries.
- Cleanup: Task 2 trap deletes the VM and stops a daemon it started.
- Runbook: Task 3.
