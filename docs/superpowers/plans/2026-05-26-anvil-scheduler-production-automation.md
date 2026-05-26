# Anvil Scheduler Production Automation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** ephemera `v0.3.6` baseline 위에서 anvil scheduler를 설치, 기동, 검증하는 production automation을 추가한다.

**Architecture:** 새 `scripts/anvil-scheduler-smoke.sh`가 실행 중인 scheduler HTTP API의 최소 운영 계약을 검증한다. `scripts/install-anvil-scheduler-systemd.sh`는 `--verify` option으로 smoke harness를 호출하고, state/quota path와 systemd hardening 경계를 더 명확히 점검한다. README와 operations 문서는 같은 명령 흐름을 기준으로 갱신한다.

**Tech Stack:** Bash, curl, optional jq, Go `testing`/`httptest`/`os/exec`, existing `cmd/anvil-scheduler` and `internal/anvilmcp` scheduler API.

---

## Scope Check

이 계획은 scheduler 운영 자동화 하나만 다룬다. scheduler algorithm, quota data model,
multi-node HA, cross-host VM migration, upstream `main`의 `v0.4.0 PR-A` storage/recovery
변경은 포함하지 않는다. feature branch는 `sync/ephemera-v0.3.6` merge commit 위에
유지한다.

## File Structure

- Create `scripts/anvil_scheduler_smoke_test.go`: fake scheduler를 띄우고 smoke/installer script를 실행하는 Go tests.
- Create `scripts/anvil-scheduler-smoke.sh`: scheduler HTTP smoke harness.
- Modify `scripts/install-anvil-scheduler-systemd.sh`: `--verify`, quota dir 생성, `/var/lib/anvil` 밖 state path 경고, smoke command 실행.
- Modify `README.md`: scheduler 운영 검증 entry point 추가.
- Modify `docs/operations/runbook.md`: install/start/verify/failure triage 절차 갱신.
- Modify `docs/operations/release-checklist.md`: scheduler smoke release gate 추가.
- Modify `docs/operations/2026-05-26-anvil-v0.3.0-release-candidate-handoff.md`: scheduler automation excluded scope를 구현 상태로 갱신.

## Task 1: Scheduler Smoke Harness

**Files:**
- Create: `scripts/anvil_scheduler_smoke_test.go`
- Create: `scripts/anvil-scheduler-smoke.sh`
- Test: `scripts/anvil_scheduler_smoke_test.go`

- [ ] **Step 1: Write failing smoke harness tests**

Create `scripts/anvil_scheduler_smoke_test.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAnvilSchedulerSmokePassesAgainstFakeScheduler(t *testing.T) {
	var mu sync.Mutex
	var host map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			writeSmokeTestJSON(t, w, map[string]string{"status": "ok"})
		case "/hosts":
			switch r.Method {
			case http.MethodPut:
				var next map[string]any
				if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
					http.Error(w, "invalid host body", http.StatusBadRequest)
					return
				}
				if next["name"] != "smoke-test-host" {
					http.Error(w, "unexpected host name", http.StatusBadRequest)
					return
				}
				mu.Lock()
				host = next
				mu.Unlock()
				writeSmokeTestJSON(t, w, next)
			case http.MethodGet:
				mu.Lock()
				current := host
				mu.Unlock()
				if current == nil {
					writeSmokeTestJSON(t, w, []map[string]any{})
					return
				}
				writeSmokeTestJSON(t, w, []map[string]any{current})
			default:
				http.Error(w, "GET or PUT required", http.StatusMethodNotAllowed)
			}
		case "/schedule/spawn":
			if r.Method != http.MethodPost {
				http.Error(w, "POST required", http.StatusMethodNotAllowed)
				return
			}
			mu.Lock()
			current := host
			mu.Unlock()
			if current == nil {
				http.Error(w, "host missing", http.StatusBadRequest)
				return
			}
			writeSmokeTestJSON(t, w, map[string]any{
				"allowed":       true,
				"reason":        "scheduled",
				"tenant_id":     "smoke-tenant",
				"host":          current,
				"egress_policy": "profile",
				"requested":     map[string]any{"active_vms": 1},
			})
		case "/placements":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			mu.Lock()
			current := host
			mu.Unlock()
			hosts := map[string]any{}
			if current != nil {
				hosts["smoke-test-host"] = current
			}
			writeSmokeTestJSON(t, w, map[string]any{
				"hosts":              hosts,
				"vm_placements":      map[string]string{},
				"snapshot_locations": map[string][]string{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "summary.json")
	cmd := exec.Command("bash", "anvil-scheduler-smoke.sh", "--base-url", server.URL, "--host-id", "smoke-test-host", "--json-out", outPath)
	cmd.Dir = scriptsDir(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke script failed: %v\n%s", err, output)
	}

	summary := readSmokeSummary(t, outPath)
	if !summary.OK {
		t.Fatalf("summary ok = false, output=%s summary=%+v", output, summary)
	}
	if summary.HostID != "smoke-test-host" {
		t.Fatalf("host_id = %q, want smoke-test-host", summary.HostID)
	}
	if summary.SelectedHostID != "smoke-test-host" {
		t.Fatalf("selected_host_id = %q, want smoke-test-host", summary.SelectedHostID)
	}
}

func TestAnvilSchedulerSmokeFailsHealthWithSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "summary.json")
	cmd := exec.Command("bash", "anvil-scheduler-smoke.sh", "--base-url", server.URL, "--json-out", outPath)
	cmd.Dir = scriptsDir(t)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("smoke script unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "health_failed") {
		t.Fatalf("output = %s, want health_failed", output)
	}

	summary := readSmokeSummary(t, outPath)
	if summary.OK {
		t.Fatalf("summary ok = true, want false")
	}
	if summary.FailedStep != "health_failed" {
		t.Fatalf("failed_step = %q, want health_failed", summary.FailedStep)
	}
}

type smokeSummary struct {
	OK             bool   `json:"ok"`
	BaseURL        string `json:"base_url"`
	HostID         string `json:"host_id"`
	SelectedHostID string `json:"selected_host_id"`
	FailedStep     string `json:"failed_step"`
}

func scriptsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

func writeSmokeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func readSmokeSummary(t *testing.T, path string) smokeSummary {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var summary smokeSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("decode summary %s: %v", data, err)
	}
	if summary.BaseURL == "" {
		t.Fatalf("base_url is empty in summary %+v", summary)
	}
	if summary.HostID == "" {
		t.Fatalf("host_id is empty in summary %+v", summary)
	}
	return summary
}

func requireOutputContains(t *testing.T, output []byte, want string) {
	t.Helper()
	if !strings.Contains(string(output), want) {
		t.Fatalf("output missing %q:\n%s", want, output)
	}
}

func commandOutput(t *testing.T, name string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_USER=anvil-smoke-user",
		"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%w", err)
	}
	return output, nil
}
```

- [ ] **Step 2: Run tests to verify RED**

Run:

```bash
go test ./scripts -run 'TestAnvilSchedulerSmoke' -count=1
```

Expected: FAIL because `anvil-scheduler-smoke.sh` does not exist.

- [ ] **Step 3: Add scheduler smoke harness**

Create `scripts/anvil-scheduler-smoke.sh`:

```bash
#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: bash scripts/anvil-scheduler-smoke.sh [--base-url URL] [--host-id ID] [--json-out PATH]

Verifies a running anvil scheduler HTTP service.

Environment:
  ANVIL_SCHEDULER_BASE_URL=http://127.0.0.1:3010
USAGE
}

base_url="${ANVIL_SCHEDULER_BASE_URL:-http://127.0.0.1:3010}"
host_id="smoke-host-1"
json_out=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-url)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      base_url="$2"
      shift
      ;;
    --host-id)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      host_id="$2"
      shift
      ;;
    --json-out)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      json_out="$2"
      shift
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
  shift
done

base_url="${base_url%/}"
failed_step=""
selected_host_id=""
HTTP_STATUS=""
HTTP_BODY=""

if ! command -v curl >/dev/null 2>&1; then
  printf 'error: curl is required for scheduler smoke\n' >&2
  exit 1
fi

jq_bin=""
if command -v jq >/dev/null 2>&1; then
  jq_bin="$(command -v jq)"
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

json_escape() {
  local value="${1:-}"
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  value=${value//$'\r'/}
  value=${value//$'\t'/\\t}
  printf '%s' "$value"
}

write_summary() {
  local ok="$1"
  [[ -n "$json_out" ]] || return 0
  local dir tmp
  dir="$(dirname "$json_out")"
  mkdir -p "$dir"
  tmp="${json_out}.tmp"
  {
    printf '{\n'
    printf '  "ok": %s,\n' "$ok"
    printf '  "base_url": "%s",\n' "$(json_escape "$base_url")"
    printf '  "host_id": "%s",\n' "$(json_escape "$host_id")"
    printf '  "selected_host_id": "%s",\n' "$(json_escape "$selected_host_id")"
    printf '  "failed_step": "%s"\n' "$(json_escape "$failed_step")"
    printf '}\n'
  } >"$tmp"
  mv "$tmp" "$json_out"
}

fail_step() {
  failed_step="$1"
  shift
  printf 'error: %s: %s\n' "$failed_step" "$*" >&2
  if ! write_summary false; then
    printf 'error: json_write_failed: could not write %s\n' "$json_out" >&2
  fi
  exit 1
}

request_json() {
  local method="$1"
  local path="$2"
  local body="${3:-}"
  local response_file="$tmp_dir/response.json"
  local url="${base_url}${path}"
  local args=(-sS -o "$response_file" -w '%{http_code}' -X "$method")
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' --data "$body")
  fi
  if ! HTTP_STATUS="$(curl "${args[@]}" "$url")"; then
    return 1
  fi
  HTTP_BODY="$(cat "$response_file")"
  return 0
}

json_matches() {
  local body="$1"
  local jq_expr="$2"
  local grep_expr="$3"
  if [[ -n "$jq_bin" ]]; then
    printf '%s' "$body" | "$jq_bin" -e "$jq_expr" >/dev/null
    return
  fi
  printf '%s' "$body" | grep -Eq "$grep_expr"
}

if ! request_json GET /health; then
  fail_step health_failed "request failed"
fi
if [[ "$HTTP_STATUS" != "200" ]]; then
  fail_step health_failed "status=$HTTP_STATUS body=$HTTP_BODY"
fi
if ! json_matches "$HTTP_BODY" '.status == "ok"' '"status"[[:space:]]*:[[:space:]]*"ok"'; then
  fail_step health_failed "unexpected body=$HTTP_BODY"
fi

escaped_host_id="$(json_escape "$host_id")"
host_payload="$(printf '{"name":"%s","endpoint":"http://127.0.0.1:3000","healthy":true,"available_vms":1,"available_snapshot_bytes":1048576,"egress_policies":["profile"]}' "$escaped_host_id")"
if ! request_json PUT /hosts "$host_payload"; then
  fail_step host_put_failed "request failed"
fi
if [[ "$HTTP_STATUS" != "200" ]]; then
  fail_step host_put_failed "status=$HTTP_STATUS body=$HTTP_BODY"
fi

if ! request_json GET /hosts; then
  fail_step host_list_failed "request failed"
fi
if [[ "$HTTP_STATUS" != "200" ]]; then
  fail_step host_list_failed "status=$HTTP_STATUS body=$HTTP_BODY"
fi
if [[ -n "$jq_bin" ]]; then
  if ! printf '%s' "$HTTP_BODY" | "$jq_bin" -e --arg host_id "$host_id" '.[] | select(.name == $host_id)' >/dev/null; then
    fail_step host_list_failed "host_id=$host_id not found body=$HTTP_BODY"
  fi
else
  if ! printf '%s' "$HTTP_BODY" | grep -Fq "\"$host_id\""; then
    fail_step host_list_failed "host_id=$host_id not found body=$HTTP_BODY"
  fi
fi

schedule_payload='{"tenant_id":"smoke-tenant","egress_policy":"profile","requested":{"active_vms":1}}'
if ! request_json POST /schedule/spawn "$schedule_payload"; then
  fail_step schedule_spawn_failed "request failed"
fi
if [[ "$HTTP_STATUS" != "200" ]]; then
  fail_step schedule_spawn_failed "status=$HTTP_STATUS body=$HTTP_BODY"
fi
if ! json_matches "$HTTP_BODY" '.allowed == true' '"allowed"[[:space:]]*:[[:space:]]*true'; then
  fail_step schedule_spawn_failed "decision not allowed body=$HTTP_BODY"
fi
if [[ -n "$jq_bin" ]]; then
  selected_host_id="$(printf '%s' "$HTTP_BODY" | "$jq_bin" -r '.host.name // empty')"
else
  if printf '%s' "$HTTP_BODY" | grep -Fq "\"$host_id\""; then
    selected_host_id="$host_id"
  fi
fi
if [[ "$selected_host_id" != "$host_id" ]]; then
  fail_step schedule_spawn_failed "selected_host_id=$selected_host_id want=$host_id body=$HTTP_BODY"
fi

if ! request_json GET /placements; then
  fail_step placements_failed "request failed"
fi
if [[ "$HTTP_STATUS" != "200" ]]; then
  fail_step placements_failed "status=$HTTP_STATUS body=$HTTP_BODY"
fi
if [[ -n "$jq_bin" ]]; then
  if ! printf '%s' "$HTTP_BODY" | "$jq_bin" -e 'type == "object"' >/dev/null; then
    fail_step placements_failed "body is not JSON object: $HTTP_BODY"
  fi
else
  if ! printf '%s' "$HTTP_BODY" | grep -Eq '^[[:space:]]*\{'; then
    fail_step placements_failed "body is not JSON object: $HTTP_BODY"
  fi
fi

if ! write_summary true; then
  failed_step="json_write_failed"
  printf 'error: json_write_failed: could not write %s\n' "$json_out" >&2
  exit 1
fi

printf 'anvil scheduler smoke ok: base_url=%s host_id=%s selected_host_id=%s\n' "$base_url" "$host_id" "$selected_host_id"
```

- [ ] **Step 4: Make smoke script executable**

Run:

```bash
chmod +x scripts/anvil-scheduler-smoke.sh
```

- [ ] **Step 5: Run smoke harness tests to verify GREEN**

Run:

```bash
go test ./scripts -run 'TestAnvilSchedulerSmoke' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run shell syntax check**

Run:

```bash
bash -n scripts/anvil-scheduler-smoke.sh
```

Expected: no output and exit code 0.

- [ ] **Step 7: Commit smoke harness**

Run:

```bash
git add scripts/anvil_scheduler_smoke_test.go scripts/anvil-scheduler-smoke.sh
git commit -m "test: add scheduler smoke harness"
```

## Task 2: Installer Verify Mode

**Files:**
- Modify: `scripts/anvil_scheduler_smoke_test.go`
- Modify: `scripts/install-anvil-scheduler-systemd.sh`
- Test: `scripts/anvil_scheduler_smoke_test.go`

- [ ] **Step 1: Add failing installer tests**

Append these tests to `scripts/anvil_scheduler_smoke_test.go`:

```go
func TestInstallAnvilSchedulerDryRunVerifyPrintsSmokeCommand(t *testing.T) {
	output, err := commandOutput(t, "bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable", "--verify")
	if err != nil {
		t.Fatalf("installer dry-run failed: %v\n%s", err, output)
	}
	requireOutputContains(t, output, "scripts/anvil-scheduler-smoke.sh")
	requireOutputContains(t, output, "--base-url http://127.0.0.1:3010")
}

func TestInstallAnvilSchedulerDryRunCreatesQuotaDirectory(t *testing.T) {
	cmd := exec.Command("bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable")
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_USER=anvil-smoke-user",
		"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
		"ANVIL_SCHEDULER_QUOTA_STORE=/var/lib/anvil/quotas/tenants.json",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer dry-run failed: %v\n%s", err, output)
	}
	requireOutputContains(t, output, "/var/lib/anvil/quotas")
}

func TestInstallAnvilSchedulerWarnsForStateOutsideVarLib(t *testing.T) {
	cmd := exec.Command("bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable")
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_USER=anvil-smoke-user",
		"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
		"ANVIL_SCHEDULER_STATE=/tmp/anvil-scheduler/state.json",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer dry-run failed: %v\n%s", err, output)
	}
	requireOutputContains(t, output, "warning: ANVIL_SCHEDULER_STATE is outside /var/lib/anvil")
}
```

- [ ] **Step 2: Run installer tests to verify RED**

Run:

```bash
go test ./scripts -run 'TestInstallAnvilScheduler' -count=1
```

Expected: FAIL. The current installer rejects `--verify`, does not create a distinct quota
directory, and does not warn for state paths outside `/var/lib/anvil`.

- [ ] **Step 3: Add installer options and helper functions**

Modify `scripts/install-anvil-scheduler-systemd.sh`.

Change the usage line:

```bash
Usage: sudo bash scripts/install-anvil-scheduler-systemd.sh [--dry-run] [--no-build] [--no-enable] [--start] [--verify]
```

Add the new flag variable near the current flag defaults:

```bash
VERIFY=0
```

Add the parser branch:

```bash
    --verify)
      VERIFY=1
      ;;
```

Add `QUOTA_DIR` after `STATE_DIR`:

```bash
QUOTA_DIR="$(dirname "$QUOTA_PATH")"
```

Add these helper functions after `run()`:

```bash
scheduler_base_url() {
  case "$SVC_ADDR" in
    http://*|https://*)
      printf '%s\n' "$SVC_ADDR"
      ;;
    *)
      printf 'http://%s\n' "$SVC_ADDR"
      ;;
  esac
}

warn_if_outside_var_lib() {
  local name="$1"
  local path="$2"
  case "$path" in
    /var/lib/anvil|/var/lib/anvil/*)
      ;;
    *)
      printf 'warning: %s is outside /var/lib/anvil; deploy/systemd/anvil-scheduler.service has ReadWritePaths=/var/lib/anvil\n' "$name" >&2
      ;;
  esac
}

require_verify_tools() {
  if [[ "$VERIFY" != "1" || "$DRY_RUN" == "1" ]]; then
    return
  fi
  if ! command -v curl >/dev/null 2>&1; then
    printf 'error: --verify requires curl\n' >&2
    exit 1
  fi
}

run_scheduler_verify() {
  if [[ "$VERIFY" != "1" ]]; then
    return
  fi
  local base_url
  base_url="$(scheduler_base_url)"
  run bash scripts/anvil-scheduler-smoke.sh --base-url "$base_url"
}
```

- [ ] **Step 4: Wire installer validation and quota directory creation**

After variable initialization, before `require_root`, add:

```bash
warn_if_outside_var_lib ANVIL_SCHEDULER_STATE "$STATE_PATH"
warn_if_outside_var_lib ANVIL_SCHEDULER_QUOTA_STORE "$QUOTA_PATH"
require_verify_tools
```

Replace the single state directory creation:

```bash
run install -d -m 0750 -o "$SVC_USER" -g "$SVC_GROUP" "$STATE_DIR"
```

with:

```bash
run install -d -m 0750 -o "$SVC_USER" -g "$SVC_GROUP" "$STATE_DIR"
if [[ "$QUOTA_DIR" != "$STATE_DIR" ]]; then
  run install -d -m 0750 -o "$SVC_USER" -g "$SVC_GROUP" "$QUOTA_DIR"
fi
```

After the existing optional `--start` block, add:

```bash
run_scheduler_verify
```

- [ ] **Step 5: Run installer tests to verify GREEN**

Run:

```bash
go test ./scripts -run 'TestInstallAnvilScheduler|TestAnvilSchedulerSmoke' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run shell syntax checks**

Run:

```bash
bash -n scripts/install-anvil-scheduler-systemd.sh
bash -n scripts/anvil-scheduler-smoke.sh
```

Expected: no output and exit code 0.

- [ ] **Step 7: Commit installer verify mode**

Run:

```bash
git add scripts/anvil_scheduler_smoke_test.go scripts/install-anvil-scheduler-systemd.sh
git commit -m "feat: verify scheduler systemd installs"
```

## Task 3: Operations Documentation

**Files:**
- Modify: `README.md`
- Modify: `docs/operations/runbook.md`
- Modify: `docs/operations/release-checklist.md`
- Modify: `docs/operations/2026-05-26-anvil-v0.3.0-release-candidate-handoff.md`

- [ ] **Step 1: Update README scheduler operations section**

In `README.md`, in the scheduler operations area near the `anvil-scheduler` build/start
commands, add this command block:

```markdown
systemd 설치 host에서는 dry-run, start, smoke verify를 같은 installer로 수행한다.

```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --verify
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

이미 실행 중인 scheduler만 확인할 때는 standalone smoke harness를 사용한다.

```bash
bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010
```
```

- [ ] **Step 2: Update runbook install and triage commands**

In `docs/operations/runbook.md`, replace the current systemd scheduler install block:

```markdown
```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run
sudo bash scripts/install-anvil-scheduler-systemd.sh
sudo systemctl start anvil-scheduler.service
curl http://127.0.0.1:3010/health
```
```

with:

```markdown
```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --verify
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

이미 실행 중인 service만 재검증할 때는 다음 명령을 사용한다.

```bash
bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010
```
```

Add this failure triage paragraph immediately after the scheduler service exposure warning:

```markdown
`--verify`가 실패하면 먼저 `sudo systemctl status anvil-scheduler.service`와
`journalctl -u anvil-scheduler.service -n 100 --no-pager`를 확인한다. `health_failed`는
service bind/start 문제, `host_put_failed`는 state path 권한 또는 JSON body 처리 문제,
`schedule_spawn_failed`는 host inventory나 quota/usage 입력 문제를 우선 의심한다.
```

- [ ] **Step 3: Update release checklist gates**

In `docs/operations/release-checklist.md`, add scheduler smoke to the static command block:

```bash
bash -n scripts/anvil-scheduler-smoke.sh
```

After the workload runner release candidate paragraph, add:

```markdown
scheduler production automation을 포함하는 release candidate에서는 systemd host에서
다음 검증을 수행한다.

```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --verify
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

`--verify`는 `GET /health`, `PUT/GET /hosts`, `POST /schedule/spawn`,
`GET /placements`를 확인한다. scheduler는 기본적으로 `127.0.0.1:3010`에 bind하며,
외부 노출은 private network 또는 TLS 종료 reverse proxy policy 뒤에서만 수행한다.
```

- [ ] **Step 4: Update v0.3.0 handoff**

In `docs/operations/2026-05-26-anvil-v0.3.0-release-candidate-handoff.md`:

Add this included scope bullet under `anvil deploy/operations hardening`:

```markdown
  - scheduler smoke harness와 systemd installer `--verify`
```

이미 구현된 scheduler 운영 검증 자동화를 제외 범위로 보이게 하던 stale bullet을 제거한다.

Replace the stale release remaining-work bullet:

```markdown
- sync branch merge commit 작성
```

with:

```markdown
- feature branch의 scheduler production automation merge 정책 결정
```

- [ ] **Step 5: Review docs for stale scheduler automation wording**

Run:

```bash
rg -n "install-anvil-scheduler|anvil-scheduler-smoke|--verify" README.md docs/operations
```

Expected: no remaining wording says scheduler production automation is excluded; install/verify
commands point to `scripts/anvil-scheduler-smoke.sh` or installer `--verify`.

- [ ] **Step 6: Commit docs**

Run:

```bash
git add README.md docs/operations/runbook.md docs/operations/release-checklist.md docs/operations/2026-05-26-anvil-v0.3.0-release-candidate-handoff.md
git commit -m "docs: document scheduler production verification"
```

## Task 4: End-to-End Verification

**Files:**
- Verify only unless a previous task exposes a real failure.

- [ ] **Step 1: Run focused scheduler automation tests**

Run:

```bash
go test ./scripts -count=1
```

Expected: PASS.

- [ ] **Step 2: Run full Go test suite**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Run build gates**

Run:

```bash
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

Expected: all commands exit 0.

- [ ] **Step 4: Run shell syntax gates**

Run:

```bash
bash -n scripts/anvil-scheduler-smoke.sh
bash -n scripts/install-anvil-scheduler-systemd.sh
bash -n scripts/vm-workload-e2e.sh
```

Expected: no output and exit code 0.

- [ ] **Step 5: Run real scheduler smoke against local binary**

Start the scheduler in a temporary state directory:

```bash
tmpdir="$(mktemp -d)"
ANVIL_SCHEDULER_ADDR=127.0.0.1:3010 \
ANVIL_SCHEDULER_STATE="$tmpdir/scheduler.json" \
ANVIL_SCHEDULER_QUOTA_STORE="$tmpdir/tenants.json" \
go run ./cmd/anvil-scheduler &
pid="$!"
sleep 1
bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010 --json-out "$tmpdir/summary.json"
kill "$pid"
wait "$pid" 2>/dev/null || true
cat "$tmpdir/summary.json"
rm -rf "$tmpdir"
```

Expected: smoke exits 0 and summary JSON has `"ok": true`.

- [ ] **Step 6: Run dry-run installer verification**

Run:

```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --no-build --no-enable --verify
```

Expected: output includes the `scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010`
command and does not modify system files.

- [ ] **Step 7: Check formatting and tracked changes**

Run:

```bash
git diff --check
git status --short
```

Expected: `git diff --check` exits 0. `git status --short` shows only the intended branch
changes if Task 4 fixes were needed; after commits it should be clean.

## Self-Review

- Spec coverage: smoke harness, installer `--verify`, state/quota path handling, docs, release
  checklist, and v0.3.6 baseline all have tasks.
- Placeholder scan: every task names exact files, commands, expected failures, and expected passes.
- Type consistency: scheduler request payloads use existing `RuntimeHost`, `ScheduleRequest`, and
  `TenantUsage` JSON fields: `name`, `endpoint`, `healthy`, `available_vms`,
  `egress_policies`, `tenant_id`, `egress_policy`, and `requested.active_vms`.
