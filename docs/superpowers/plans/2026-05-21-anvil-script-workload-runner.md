# Anvil Script Workload Runner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a deterministic script-only VM workload runner so workload E2E can run uploaded `/workspace/workloads/*.sh` scripts without LLM/provider credentials.

**Architecture:** The guest `goose-agent` gets an authenticated `POST /workloads/run` endpoint that validates a relative `.sh` path under `/workspace/workloads/`, runs it with `bash` under a bounded timeout, captures stdout/stderr with size limits, and returns structured JSON. The daemon reuses the existing agent proxy pattern at `POST /vms/{vm_id}/workloads/run`, and the workload E2E harness switches from `/tasks` to the new endpoint. Docs are updated to separate deterministic workload execution from real LLM `/tasks` execution.

**Tech Stack:** Go 1.25, standard library `net/http`, `os/exec`, `context`, existing anvil daemon/agent proxy, Bash harness, existing Firecracker/KVM runtime.

---

## Scope Check

This plan implements one subsystem: script-only workload execution. It must not add a generic `/exec` endpoint, accept arbitrary shell command strings, add PTY/stdin streaming, or alter the real LLM `/tasks` route.

## File Structure

- Modify `cmd/goose-agent/main.go`: add workload request/response types, path/timeout validation helpers, truncating output capture, and authenticated `/workloads/run`.
- Modify `cmd/goose-agent/main_test.go`: add unit tests for validation, success, nonzero exit, timeout, and busy behavior.
- Modify `cmd/goose-daemon/api.go`: add `/vms/{vm_id}/workloads/run` route using existing `proxyAgentEndpoint`.
- Modify `cmd/goose-daemon/api_test.go`: add route/proxy tests for method handling and token injection.
- Modify `scripts/vm-workload-e2e.sh`: replace `/tasks` prompt with two deterministic workload runner calls.
- Modify docs:
  - `docs/architecture/runtime-architecture.md`
  - `docs/architecture/service-logic.md`
  - `docs/operations/runbook.md`
  - `docs/superpowers/specs/2026-05-20-anvil-vm-workload-e2e-design.md`

## Task 1: Guest Workload Validation Helpers

**Files:**
- Modify: `cmd/goose-agent/main.go`
- Modify: `cmd/goose-agent/main_test.go`
- Test: `cmd/goose-agent/main_test.go`

- [ ] **Step 1: Add failing validation tests**

Append these tests to `cmd/goose-agent/main_test.go` before `assertJSONError`:

```go
func TestWorkloadScriptPathAcceptsOnlyWorkloadShellScripts(t *testing.T) {
	root := t.TempDir()
	workloads := filepath.Join(root, "workloads")
	if err := os.MkdirAll(workloads, 0755); err != nil {
		t.Fatalf("mkdir workloads: %v", err)
	}
	script := filepath.Join(workloads, "nginx-smoke.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	fullPath, clean, status, err := workloadScriptPath(root, "workloads/nginx-smoke.sh")
	if err != nil {
		t.Fatalf("workloadScriptPath returned error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if clean != "workloads/nginx-smoke.sh" {
		t.Fatalf("clean path = %q, want workloads/nginx-smoke.sh", clean)
	}
	if fullPath != script {
		t.Fatalf("full path = %q, want %q", fullPath, script)
	}
}

func TestWorkloadScriptPathRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name   string
		script string
		status int
	}{
		{"empty", "", http.StatusBadRequest},
		{"absolute", "/workspace/workloads/run.sh", http.StatusBadRequest},
		{"traversal", "workloads/../secret.sh", http.StatusBadRequest},
		{"outside", "notes/run.sh", http.StatusBadRequest},
		{"not shell", "workloads/run.txt", http.StatusBadRequest},
		{"missing", "workloads/missing.sh", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, status, err := workloadScriptPath(root, tc.script)
			if err == nil {
				t.Fatalf("workloadScriptPath(%q) returned nil error", tc.script)
			}
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
		})
	}
}

func TestWorkloadTimeoutSeconds(t *testing.T) {
	cases := []struct {
		name    string
		seconds int
		want    time.Duration
		wantErr bool
	}{
		{"default", 0, defaultWorkloadTimeout, false},
		{"minimum", 1, time.Second, false},
		{"maximum", 1800, 1800 * time.Second, false},
		{"negative", -1, 0, true},
		{"too high", 1801, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := workloadTimeoutSeconds(tc.seconds)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("duration = %v, want %v", got, tc.want)
			}
		})
	}
}
```

Update the import block in `cmd/goose-agent/main_test.go` to include `time`:

```go
import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./cmd/goose-agent -run 'TestWorkload(ScriptPath|Timeout)' -count=1
```

Expected: FAIL with undefined identifiers such as `workloadScriptPath`, `workloadTimeoutSeconds`, and `defaultWorkloadTimeout`.

- [ ] **Step 3: Add workload request/response types and validation helpers**

In `cmd/goose-agent/main.go`, add these types near `TaskRequest` and `TaskResult`:

```go
type WorkloadRunRequest struct {
	Script         string `json:"script"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type WorkloadRunResult struct {
	Script          string `json:"script"`
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	DurationMs      int64  `json:"duration_ms"`
	TimedOut        bool   `json:"timed_out"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}
```

Add workload constants to the existing `const` block:

```go
	workloadDirName          = "workloads"
	workloadScriptSuffix     = ".sh"
	defaultWorkloadTimeout   = 600 * time.Second
	maxWorkloadTimeout       = 1800 * time.Second
	maxWorkloadOutputBytes   = 1 << 20
```

Add these helpers after `workspaceFilePath`:

```go
func workloadScriptPath(root, script string) (fullPath, clean string, status int, err error) {
	script = strings.TrimSpace(script)
	if script == "" || filepath.IsAbs(script) {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must be a non-empty relative path")
	}

	clean = filepath.Clean(script)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, string(os.PathSeparator)+".."+string(os.PathSeparator)) {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must stay within workspace workloads")
	}
	if !strings.HasPrefix(clean, workloadDirName+string(os.PathSeparator)) && !strings.HasPrefix(clean, workloadDirName+"/") {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must be under workloads/")
	}
	if !strings.HasSuffix(clean, workloadScriptSuffix) {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must end with .sh")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", http.StatusInternalServerError, fmt.Errorf("resolve workspace root: %w", err)
	}
	workloadRoot := filepath.Join(rootAbs, workloadDirName)
	fullPath, err = filepath.Abs(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", "", http.StatusInternalServerError, fmt.Errorf("resolve workload script: %w", err)
	}
	if fullPath != workloadRoot && !strings.HasPrefix(fullPath, workloadRoot+string(os.PathSeparator)) {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must stay within workloads/")
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", http.StatusNotFound, fmt.Errorf("workload script not found")
		}
		return "", "", http.StatusInternalServerError, fmt.Errorf("stat workload script: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", http.StatusBadRequest, fmt.Errorf("workload script must be a regular file")
	}
	return fullPath, filepath.ToSlash(clean), http.StatusOK, nil
}

func workloadTimeoutSeconds(seconds int) (time.Duration, error) {
	if seconds == 0 {
		return defaultWorkloadTimeout, nil
	}
	if seconds < 1 {
		return 0, fmt.Errorf("timeout_seconds must be >= 1")
	}
	timeout := time.Duration(seconds) * time.Second
	if timeout > maxWorkloadTimeout {
		return 0, fmt.Errorf("timeout_seconds must be <= %d", int(maxWorkloadTimeout/time.Second))
	}
	return timeout, nil
}
```

- [ ] **Step 4: Run validation tests**

Run:

```bash
go test ./cmd/goose-agent -run 'TestWorkload(ScriptPath|Timeout)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit validation helpers**

Run:

```bash
git add cmd/goose-agent/main.go cmd/goose-agent/main_test.go
git commit -m "feat(agent): validate workload scripts"
```

Expected: commit succeeds with only the two agent files.

## Task 2: Guest Workload Runner Endpoint

**Files:**
- Modify: `cmd/goose-agent/main.go`
- Modify: `cmd/goose-agent/main_test.go`
- Test: `cmd/goose-agent/main_test.go`

- [ ] **Step 1: Add failing endpoint tests**

Append these tests to `cmd/goose-agent/main_test.go` before `assertJSONError`:

```go
func TestHandleWorkloadRunSuccess(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/success.sh", "#!/usr/bin/env bash\necho stdout-ok\necho stderr-ok >&2\n")
	handler := workloadRunHandler(root)

	body := strings.NewReader(`{"script":"workloads/success.sh","timeout_seconds":5}`)
	req := httptest.NewRequest(http.MethodPost, "/workloads/run", body)
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	var result WorkloadRunResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "stdout-ok") {
		t.Fatalf("stdout = %q, want stdout-ok", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "stderr-ok") {
		t.Fatalf("stderr = %q, want stderr-ok", result.Stderr)
	}
	if result.Script != "workloads/success.sh" {
		t.Fatalf("script = %q, want workloads/success.sh", result.Script)
	}
}

func TestHandleWorkloadRunNonzeroExitReturnsResult(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/fail.sh", "#!/usr/bin/env bash\necho failed >&2\nexit 7\n")
	handler := workloadRunHandler(root)

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/fail.sh","timeout_seconds":5}`))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	var result WorkloadRunResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit_code = %d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "failed") {
		t.Fatalf("stderr = %q, want failed", result.Stderr)
	}
}

func TestHandleWorkloadRunTimeout(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/sleep.sh", "#!/usr/bin/env bash\nsleep 2\n")
	handler := workloadRunHandler(root)

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/sleep.sh","timeout_seconds":1}`))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	var result WorkloadRunResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("timed_out = false, want true")
	}
	if result.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", result.ExitCode)
	}
}

func TestHandleWorkloadRunRejectsInvalidPath(t *testing.T) {
	handler := workloadRunHandler(t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"../secret.sh"}`))
	rr := httptest.NewRecorder()

	handler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q; want 400", rr.Code, rr.Body.String())
	}
}

func TestHandleWorkloadRunBusy(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/success.sh", "#!/usr/bin/env bash\necho ok\n")
	handler := workloadRunHandler(root)

	mu.Lock()
	busy = true
	mu.Unlock()
	defer func() {
		mu.Lock()
		busy = false
		mu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/success.sh"}`))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func writeWorkloadScript(t *testing.T, root, relPath, content string) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir script parent: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./cmd/goose-agent -run 'TestHandleWorkloadRun' -count=1
```

Expected: FAIL with undefined `workloadRunHandler`.

- [ ] **Step 3: Add output limiter and runner implementation**

In `cmd/goose-agent/main.go`, add this type after `ErrorResponse`:

```go
type truncatingBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *truncatingBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = true
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *truncatingBuffer) String() string {
	return b.buf.String()
}
```

Add these functions after `handleTask`:

```go
func workloadRunHandler(root string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}

		var req WorkloadRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		fullPath, cleanScript, status, err := workloadScriptPath(root, req.Script)
		if err != nil {
			writeJSONError(w, status, err.Error())
			return
		}
		timeout, err := workloadTimeoutSeconds(req.TimeoutSeconds)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		mu.Lock()
		if busy {
			mu.Unlock()
			writeJSONError(w, http.StatusServiceUnavailable, "agent busy")
			return
		}
		busy = true
		mu.Unlock()
		defer func() {
			mu.Lock()
			busy = false
			mu.Unlock()
		}()

		result := runWorkloadScript(r.Context(), root, cleanScript, fullPath, timeout)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func runWorkloadScript(parent context.Context, root, cleanScript, fullPath string, timeout time.Duration) WorkloadRunResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	var stdout truncatingBuffer
	var stderr truncatingBuffer
	stdout.limit = maxWorkloadOutputBytes
	stderr.limit = maxWorkloadOutputBytes

	cmd := exec.CommandContext(ctx, "bash", fullPath)
	cmd.Dir = root
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()
	result := WorkloadRunResult{
		Script:          cleanScript,
		ExitCode:        0,
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		DurationMs:      durationMs,
		TimedOut:        false,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}

	if ctx.Err() == context.DeadlineExceeded {
		result.ExitCode = -1
		result.TimedOut = true
		if result.Stderr == "" {
			result.Stderr = fmt.Sprintf("workload timed out after %ds", int(timeout/time.Second))
		}
		return result
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result
		}
		result.ExitCode = -1
		if result.Stderr == "" {
			result.Stderr = err.Error()
		}
	}
	return result
}
```

Update the import block in `cmd/goose-agent/main.go` to add `errors`:

```go
	"errors"
```

- [ ] **Step 4: Register the endpoint**

In `cmd/goose-agent/main`, add this route next to `/workspace`:

```go
	mux.HandleFunc("/workloads/run", agentAuthMiddleware(token, workloadRunHandler(workspaceRoot)))
```

- [ ] **Step 5: Run endpoint tests**

Run:

```bash
go test ./cmd/goose-agent -run 'TestHandleWorkloadRun|TestWorkload' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run full agent tests**

Run:

```bash
go test ./cmd/goose-agent -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit guest endpoint**

Run:

```bash
git add cmd/goose-agent/main.go cmd/goose-agent/main_test.go
git commit -m "feat(agent): run workload scripts"
```

Expected: commit succeeds with only the two agent files.

## Task 3: Daemon Workload Proxy Route

**Files:**
- Modify: `cmd/goose-daemon/api.go`
- Modify: `cmd/goose-daemon/api_test.go`
- Test: `cmd/goose-daemon/api_test.go`

- [ ] **Step 1: Add failing daemon route tests**

Append these tests to `cmd/goose-daemon/api_test.go`:

```go
func TestHandleVMProxiesWorkloadRun(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workloads/run" {
			t.Fatalf("agent path = %q, want /workloads/run", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("Authorization = %q, want Bearer agent-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"script":"workloads/ok.sh","exit_code":0,"stdout":"ok","stderr":"","duration_ms":1,"timed_out":false}`))
	}))
	defer agent.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(agent.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	oldAgentPort := agentPort
	agentPort, err = strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	defer func() { agentPort = oldAgentPort }()

	cp := newTestCP(t)
	cp.agentHTTPClient = agent.Client()
	cp.vms["vm-1"] = &runningVM{
		VMInfo: VMInfo{
			VMID:    "vm-1",
			GuestIP: host,
		},
		agentToken: "agent-token",
	}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/workloads/run", strings.NewReader(`{"script":"workloads/ok.sh"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	cp.handleVM(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"exit_code":0`) {
		t.Fatalf("body = %q, want exit_code 0", rr.Body.String())
	}
}

func TestHandleVMWorkloadRunRequiresPost(t *testing.T) {
	cp := newTestCP(t)
	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/workloads/run", nil)
	rr := httptest.NewRecorder()

	cp.handleVM(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}
```

Update `cmd/goose-daemon/api_test.go` imports to include:

```go
	"net"
	"strconv"
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestHandleVM.*WorkloadRun' -count=1
```

Expected: FAIL because `/workloads/run` is not routed and GET does not return the expected route-specific error.

- [ ] **Step 3: Add daemon route**

In `cmd/goose-daemon/api.go`, add this block in `handleVM` after the `/tasks` block and before `/workspace`:

```go
	if strings.HasSuffix(path, "/workloads/run") {
		vmID := strings.TrimSuffix(path, "/workloads/run")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.proxyAgentEndpoint(w, r, vmID, "/workloads/run")
		return
	}
```

Add the endpoint to the startup log near `/tasks`:

```go
	log.Printf("  POST   /vms/{vm_id}/workloads/run        — proxy: run workload script on agent")
```

- [ ] **Step 4: Run daemon route tests**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestHandleVM.*WorkloadRun' -count=1
```

Expected: PASS.

- [ ] **Step 5: Run daemon package tests**

Run:

```bash
go test ./cmd/goose-daemon -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit daemon proxy route**

Run:

```bash
git add cmd/goose-daemon/api.go cmd/goose-daemon/api_test.go
git commit -m "feat(daemon): proxy workload runner"
```

Expected: commit succeeds with daemon API and test files.

## Task 4: Switch Workload Harness Off `/tasks`

**Files:**
- Modify: `scripts/vm-workload-e2e.sh`
- Test: `scripts/vm-workload-e2e.sh`

- [ ] **Step 1: Add workload runner helper functions**

In `scripts/vm-workload-e2e.sh`, add this function after `fetch_workspace_file`:

```bash
run_workload() {
  local label="$1"
  local script="$2"
  local marker="$3"
  local output="$ARTIFACT_DIR/${label}-run.json"
  local payload
  local code
  local exit_code
  local timed_out
  local stdout

  payload="$(jq -n --arg script "$script" --argjson timeout "$TASK_TIMEOUT" '{script: $script, timeout_seconds: $timeout}')"
  code="$(curl -sS "${CURL_AUTH_ARGS[@]}" --max-time "$((TASK_TIMEOUT + 30))" -o "$output" -w "%{http_code}" \
    -X POST "$API/vms/$VM_ID/workloads/run" \
    -H "Content-Type: application/json" \
    -d "$payload" || true)"
  if [ "$code" != "200" ]; then
    fail "task_failed: workload $script returned HTTP $code"
    return
  fi

  exit_code="$(jq -r '.exit_code // "missing"' "$output" 2>/dev/null || true)"
  timed_out="$(jq -r '.timed_out // false' "$output" 2>/dev/null || true)"
  stdout="$(jq -r '.stdout // ""' "$output" 2>/dev/null || true)"

  if [ "$timed_out" = "true" ]; then
    fail "task_failed: workload $script timed out"
    return
  fi
  if [ "$exit_code" != "0" ]; then
    fail "task_failed: workload $script exit_code=$exit_code"
    return
  fi
  if printf '%s' "$stdout" | grep -q "$marker"; then
    ok "Workload $script output contains $marker"
  else
    fail "task_failed: workload $script missing marker $marker"
  fi
}
```

- [ ] **Step 2: Replace `/tasks` section**

In `scripts/vm-workload-e2e.sh`, replace the entire block from:

```bash
step "Run VM workload task"
```

through the marker loop that reads `task-output.json` with:

```bash
step "Run VM workloads"
run_workload nginx workloads/nginx-smoke.sh ANVIL_WORKLOAD_NGINX_READY
run_workload go-http workloads/go-http-bench.sh ANVIL_WORKLOAD_GO_HTTP_READY
if jq -r '.stdout // ""' "$ARTIFACT_DIR/go-http-run.json" 2>/dev/null | grep -q ANVIL_WORKLOAD_BENCH_DONE; then
  ok "Workload workloads/go-http-bench.sh output contains ANVIL_WORKLOAD_BENCH_DONE"
else
  fail "task_failed: workload workloads/go-http-bench.sh missing marker ANVIL_WORKLOAD_BENCH_DONE"
fi
```

- [ ] **Step 3: Update artifact names in comments/help if present**

If `task-output.json` appears in `scripts/vm-workload-e2e.sh`, replace user-facing mentions with `nginx-run.json` and `go-http-run.json`. Do not change docs in this task.

Run:

```bash
rg -n "task-output|/tasks|workloads/run|nginx-run|go-http-run" scripts/vm-workload-e2e.sh
```

Expected: output contains `workloads/run`, `nginx-run`, and `go-http-run`; it does not contain `/tasks` or `task-output`.

- [ ] **Step 4: Run shell syntax and argument checks**

Run:

```bash
bash -n scripts/vm-workload-e2e.sh
bash scripts/vm-workload-e2e.sh --help
bash scripts/vm-workload-e2e.sh --help extra; test $? -eq 2
bash scripts/vm-workload-e2e.sh extra; test $? -eq 2
```

Expected: all commands pass.

- [ ] **Step 5: Commit harness update**

Run:

```bash
git add scripts/vm-workload-e2e.sh
git commit -m "test: run VM workloads without LLM"
```

Expected: commit succeeds with only the harness file.

## Task 5: Documentation Updates

**Files:**
- Modify: `docs/architecture/runtime-architecture.md`
- Modify: `docs/architecture/service-logic.md`
- Modify: `docs/operations/runbook.md`
- Modify: `docs/superpowers/specs/2026-05-20-anvil-vm-workload-e2e-design.md`

- [ ] **Step 1: Update runtime architecture**

In `docs/architecture/runtime-architecture.md`, update the Guest agent responsibility row to include `/workloads/run`, and add this guest disk/API note near the existing `/workspace` and `/tasks` descriptions:

```markdown
| `/workloads/run` | VM 내부 `/workspace/workloads/*.sh` script-only runner. LLM provider를 거치지 않고 deterministic workload smoke와 benchmark를 실행한다. |
```

- [ ] **Step 2: Update service logic**

In `docs/architecture/service-logic.md`, add the daemon endpoint row:

```markdown
| `/vms/{vm_id}/workloads/run` | `cmd/goose-daemon/api.go` | guest agent로 script-only workload 실행 proxy |
```

Add the guest endpoint row:

```markdown
| `POST /workloads/run` | VM별 Bearer token | `/workspace/workloads/*.sh` script를 timeout/output limit 안에서 실행 |
```

Add this flow near the current Task 실행 section:

```text
POST /workloads/run
  -> {"script":"workloads/name.sh","timeout_seconds":600} decode
  -> relative path, workloads/ prefix, .sh suffix, traversal reject
  -> busy이면 503 반환
  -> bash /workspace/workloads/name.sh 실행
  -> stdout/stderr/exit_code/duration_ms/timed_out 반환
```

- [ ] **Step 3: Update runbook**

In `docs/operations/runbook.md`, update the VM workload E2E section to say deterministic workload E2E does not require LLM provider credentials. Keep the existing secret guidance.

Add this sentence:

```markdown
이 workload 경로는 `/workloads/run`을 사용하므로 Gemini/Goose provider key 없이도 서비스 설치와 host-to-VM probe를 검증한다. real LLM 검증은 기존 `/tasks` 기반 suite에서 별도로 수행한다.
```

- [ ] **Step 4: Update previous workload E2E spec**

In `docs/superpowers/specs/2026-05-20-anvil-vm-workload-e2e-design.md`, add a short amendment section near `현재 제약` or `후속 확장`:

```markdown
## 후속 반영: Script Workload Runner

`2026-05-21-anvil-script-workload-runner-design.md`가 승인되면 deterministic workload E2E는 `/tasks` 대신 `/vms/{vm_id}/workloads/run`을 사용한다. 이 변경 후 nginx/Go HTTP workload 실행은 LLM provider credential에 의존하지 않는다. `/tasks`는 real LLM smoke 전용 경로로 유지한다.
```

- [ ] **Step 5: Verify docs**

Run:

```bash
rg -n "workloads/run|script-only|LLM provider|Gemini|task-output|nginx-run|go-http-run" docs/architecture/runtime-architecture.md docs/architecture/service-logic.md docs/operations/runbook.md docs/superpowers/specs/2026-05-20-anvil-vm-workload-e2e-design.md
git diff --check -- docs/architecture/runtime-architecture.md docs/architecture/service-logic.md docs/operations/runbook.md docs/superpowers/specs/2026-05-20-anvil-vm-workload-e2e-design.md
```

Expected: `rg` shows the new workload runner contract and deterministic/no-LLM wording; `git diff --check` exits `0`.

- [ ] **Step 6: Commit docs**

Run:

```bash
git add docs/architecture/runtime-architecture.md docs/architecture/service-logic.md docs/operations/runbook.md docs/superpowers/specs/2026-05-20-anvil-vm-workload-e2e-design.md
git commit -m "docs: document script workload runner"
```

Expected: commit succeeds with only docs.

## Task 6: Final Verification

**Files:**
- Verify all changed files.

- [ ] **Step 1: Run focused unit tests**

Run:

```bash
go test ./cmd/goose-agent -count=1
go test ./cmd/goose-daemon -run 'TestHandleVM.*WorkloadRun' -count=1
```

Expected: both commands pass.

- [ ] **Step 2: Run repository tests**

Run:

```bash
go test ./...
```

Expected: command exits `0`.

- [ ] **Step 3: Run shell checks**

Run:

```bash
bash -n scripts/workloads/nginx-smoke.sh
bash -n scripts/workloads/go-http-bench.sh
bash -n scripts/vm-workload-e2e.sh
```

Expected: all commands exit `0`.

- [ ] **Step 4: Build daemon**

Run:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
```

Expected: command exits `0`.

- [ ] **Step 5: Run full KVM workload E2E when root/KVM is available**

Run:

```bash
sudo -n bash scripts/vm-workload-e2e.sh
```

Expected: command exits `0`, writes `summary.json` with `"pass": true`, and no longer fails with Gemini/API key errors because the workload path does not call `/tasks`.

- [ ] **Step 6: Inspect artifacts without secrets**

Run:

```bash
artifact_dir="$(ls -td /tmp/anvil-workload-e2e-* | head -1)"
jq '{pass, failure_reasons, vm_id, guest_ip, nginx_http_status, go_http_status, host_benchmark_tool}' "$artifact_dir/summary.json"
rg -n "ANVIL_WORKLOAD_|tool=|requests=|ok=|duration_ms=" "$artifact_dir"
rg -n "GOOGLE_API_KEY|ANTHROPIC_API_KEY|OPENAI_API_KEY|Authorization: Bearer|agent_token" "$artifact_dir" && exit 1 || true
```

Expected: first `jq` shows `pass: true`; marker and benchmark lines appear; secret scan returns no matches.

## Spec Coverage Map

- No generic `/exec`: Tasks 1-3 add only `/workloads/run`; Task 4 removes `/tasks` from deterministic harness.
- Allowed path only: Task 1 `workloadScriptPath`.
- Timeout and output limits: Task 1 constants, Task 2 runner.
- Structured result: Task 2 `WorkloadRunResult`.
- Existing auth: Task 2 uses `agentAuthMiddleware`; Task 3 uses `proxyAgentEndpoint`.
- Harness no LLM dependency: Task 4.
- Docs: Task 5.
- Full verification: Task 6.
