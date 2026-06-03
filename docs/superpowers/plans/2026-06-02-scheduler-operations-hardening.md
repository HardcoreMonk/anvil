# Scheduler Operations Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add scheduler process integration coverage, scheduler Prometheus metrics, smoke verification, and systemd verification documentation for follow-up items 1-3.

**Architecture:** Keep scheduler scheduling semantics unchanged. Add a read-only `GET /metrics` endpoint to `SchedulerService`, extend the smoke harness to verify it, and add a `cmd/anvil-scheduler` process-level integration test using a fake daemon, temp hosts file, and temp placement state.

**Tech Stack:** Go 1.25, Go standard library HTTP tests, existing `internal/anvilmcp` scheduler types, Bash smoke harness, systemd installer script.

---

## Scope Check

This plan implements one subsystem: scheduler operations hardening. It does not sync upstream ephemera, change snapshot storage, implement snapshot replication, change flock placement, add scheduler auth, or alter the scheduling algorithm.

## File Structure

- Create `internal/anvilmcp/scheduler_metrics_test.go`: tests for Prometheus rendering and `/metrics` route behavior.
- Create `internal/anvilmcp/scheduler_metrics.go`: read-only scheduler metrics renderer.
- Modify `internal/anvilmcp/scheduler_service.go`: register `GET /metrics`.
- Modify `scripts/anvil_scheduler_smoke_test.go`: fake scheduler metrics endpoint and smoke failure coverage.
- Modify `scripts/anvil-scheduler-smoke.sh`: call `GET /metrics` and fail with `metrics_failed`.
- Modify `cmd/anvil-scheduler/main_test.go`: process-level scheduler integration test.
- Modify `README.md`: document scheduler `/metrics` as scheduler-specific observability.
- Modify `docs/operations/runbook.md`: add scheduler metrics check and `metrics_failed` triage.
- Modify `docs/operations/observability.md`: add scheduler metrics surface.
- Modify `docs/operations/release-checklist.md`: include scheduler process and metrics verification.
- Modify `docs/operations/2026-05-29-anvil-follow-up-development.md`: mark 1-3 as implemented or verified when validation is complete.
- Modify `RELEASE_NOTES.md`: add an unreleased scheduler operations hardening note.
- Create `docs/operations/2026-06-02-scheduler-operations-hardening-handoff.md`: release-to-operate handoff after verification.

## Task 1: Scheduler Metrics Endpoint

**Files:**
- Create: `internal/anvilmcp/scheduler_metrics_test.go`
- Create: `internal/anvilmcp/scheduler_metrics.go`
- Modify: `internal/anvilmcp/scheduler_service.go`

- [ ] **Step 1: Write failing metrics renderer and endpoint tests**

Create `internal/anvilmcp/scheduler_metrics_test.go`:

```go
package anvilmcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRenderSchedulerMetricsSummarizesControlLoopState(t *testing.T) {
	pollCompleted := time.Date(2026, 6, 2, 1, 2, 3, 0, time.UTC)
	reconcileCompleted := time.Date(2026, 6, 2, 1, 3, 4, 0, time.UTC)
	state := PlacementStoreState{
		Hosts: map[string]RuntimeHost{
			"host-a": {Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1},
			"host-b": {Name: "host-b", Endpoint: "http://host-b", Healthy: false, AvailableVMs: 1},
			"host-c": {Name: "host-c", Endpoint: "http://host-c", Healthy: false, AvailableVMs: 1},
			"host-d": {Name: "host-d", Endpoint: "http://host-d", Healthy: false, AvailableVMs: 1},
		},
		HostObservations: map[string]HostObservation{
			"host-a": {Status: HostStatusHealthy},
			"host-b": {Status: HostStatusDegraded, FailureCount: 1},
			"host-c": {Status: HostStatusUnhealthy, FailureCount: 3},
		},
		SuspectVMPlacements: map[string]SuspectVMPlacement{
			"vm-1": {Host: "host-b", Reason: "host_degraded"},
			"vm-2": {Host: "host-c", Reason: "host_unhealthy"},
		},
		ControlLoopStatus: ControlLoopStatus{
			Running:                  true,
			PersistenceDegraded:      true,
			PollIntervalSeconds:      10,
			ReconcileIntervalSeconds: 30,
			LastPollCompletedAt:      pollCompleted,
			LastReconcileCompletedAt: reconcileCompleted,
		},
	}

	output := RenderSchedulerMetrics(state)
	requireMetricLine(t, output, "anvil_scheduler_control_loop_running 1")
	requireMetricLine(t, output, "anvil_scheduler_persistence_degraded 1")
	requireMetricLine(t, output, "anvil_scheduler_host_status_count{status=\"healthy\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_host_status_count{status=\"degraded\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_host_status_count{status=\"unhealthy\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_host_status_count{status=\"unknown\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_suspect_vm_placements 2")
	requireMetricLine(t, output, fmt.Sprintf("anvil_scheduler_last_poll_completed_timestamp_seconds %d", pollCompleted.Unix()))
	requireMetricLine(t, output, fmt.Sprintf("anvil_scheduler_last_reconcile_completed_timestamp_seconds %d", reconcileCompleted.Unix()))
	requireMetricLine(t, output, "anvil_scheduler_poll_interval_seconds 10")
	requireMetricLine(t, output, "anvil_scheduler_reconcile_interval_seconds 30")
	if strings.Contains(output, "http://host-a") || strings.Contains(output, "agent_token") {
		t.Fatalf("metrics output leaked endpoint or token-like data:\n%s", output)
	}
}

func TestRenderSchedulerMetricsHandlesZeroState(t *testing.T) {
	output := RenderSchedulerMetrics(PlacementStoreState{})
	requireMetricLine(t, output, "anvil_scheduler_control_loop_running 0")
	requireMetricLine(t, output, "anvil_scheduler_persistence_degraded 0")
	requireMetricLine(t, output, "anvil_scheduler_host_status_count{status=\"healthy\"} 0")
	requireMetricLine(t, output, "anvil_scheduler_host_status_count{status=\"unknown\"} 0")
	requireMetricLine(t, output, "anvil_scheduler_suspect_vm_placements 0")
	requireMetricLine(t, output, "anvil_scheduler_last_poll_completed_timestamp_seconds 0")
	requireMetricLine(t, output, "anvil_scheduler_last_reconcile_completed_timestamp_seconds 0")
}

func TestSchedulerServiceMetricsEndpoint(t *testing.T) {
	store := NewPlacementStore("")
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusHealthy})
	_ = store.SetControlLoopStatus(ControlLoopStatus{Running: true, PollIntervalSeconds: 10, ReconcileIntervalSeconds: 30})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if contentType := rr.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", contentType)
	}
	requireMetricLine(t, rr.Body.String(), "anvil_scheduler_control_loop_running 1")
	requireMetricLine(t, rr.Body.String(), "anvil_scheduler_host_status_count{status=\"healthy\"} 1")
}

func TestSchedulerServiceMetricsRejectsNonGET(t *testing.T) {
	service := NewSchedulerService(SchedulerServiceOptions{})
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics status = %d body=%s, want 405", rr.Code, rr.Body.String())
	}
}

func requireMetricLine(t *testing.T, output string, want string) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("metrics output missing line %q:\n%s", want, output)
}
```

- [ ] **Step 2: Run focused tests to verify RED**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRenderSchedulerMetrics|TestSchedulerServiceMetrics' -count=1
```

Expected: FAIL because `RenderSchedulerMetrics` is undefined and `/metrics` is not registered.

- [ ] **Step 3: Add metrics renderer**

Create `internal/anvilmcp/scheduler_metrics.go`:

```go
package anvilmcp

import (
	"fmt"
	"strings"
	"time"
)

const schedulerMetricsContentType = "text/plain; version=0.0.4"

func RenderSchedulerMetrics(state PlacementStoreState) string {
	status := state.ControlLoopStatus
	hosts := runtimeHostsFromPlacementState(state)
	summary := SummarizeHostStatuses(hosts, state.HostObservations)

	var out strings.Builder
	writeSchedulerGauge(&out, "anvil_scheduler_control_loop_running", "Scheduler control loop running flag.", boolMetric(status.Running))
	writeSchedulerGauge(&out, "anvil_scheduler_persistence_degraded", "Scheduler persistence degraded flag.", boolMetric(status.PersistenceDegraded))

	out.WriteString("# HELP anvil_scheduler_host_status_count Scheduler host count by status.\n")
	out.WriteString("# TYPE anvil_scheduler_host_status_count gauge\n")
	fmt.Fprintf(&out, "anvil_scheduler_host_status_count{status=\"healthy\"} %d\n", summary.Healthy)
	fmt.Fprintf(&out, "anvil_scheduler_host_status_count{status=\"degraded\"} %d\n", summary.Degraded)
	fmt.Fprintf(&out, "anvil_scheduler_host_status_count{status=\"unhealthy\"} %d\n", summary.Unhealthy)
	fmt.Fprintf(&out, "anvil_scheduler_host_status_count{status=\"unknown\"} %d\n", summary.Unknown)

	writeSchedulerGauge(&out, "anvil_scheduler_suspect_vm_placements", "Scheduler suspect VM placement count.", float64(len(state.SuspectVMPlacements)))
	writeSchedulerGauge(&out, "anvil_scheduler_last_poll_completed_timestamp_seconds", "Unix timestamp of the last completed scheduler poll.", timestampMetric(status.LastPollCompletedAt))
	writeSchedulerGauge(&out, "anvil_scheduler_last_reconcile_completed_timestamp_seconds", "Unix timestamp of the last completed scheduler reconciliation.", timestampMetric(status.LastReconcileCompletedAt))
	writeSchedulerGauge(&out, "anvil_scheduler_poll_interval_seconds", "Configured scheduler poll interval in seconds.", float64(status.PollIntervalSeconds))
	writeSchedulerGauge(&out, "anvil_scheduler_reconcile_interval_seconds", "Configured scheduler reconcile interval in seconds.", float64(status.ReconcileIntervalSeconds))
	return out.String()
}

func writeSchedulerGauge(out *strings.Builder, name string, help string, value float64) {
	fmt.Fprintf(out, "# HELP %s %s\n", name, help)
	fmt.Fprintf(out, "# TYPE %s gauge\n", name)
	fmt.Fprintf(out, "%s %g\n", name, value)
}

func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func timestampMetric(value time.Time) float64 {
	if value.IsZero() {
		return 0
	}
	return float64(value.Unix())
}
```

- [ ] **Step 4: Register `/metrics` in scheduler service**

Modify `internal/anvilmcp/scheduler_service.go`.

Add the route in `Handler()` after `/health`:

```go
	mux.HandleFunc("/metrics", s.handleMetrics)
```

Add this method after `handleHealth`:

```go
func (s *SchedulerService) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", schedulerMetricsContentType)
	_, _ = w.Write([]byte(RenderSchedulerMetrics(s.placements.State())))
}
```

- [ ] **Step 5: Run focused tests to verify GREEN**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRenderSchedulerMetrics|TestSchedulerServiceMetrics' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit scheduler metrics endpoint**

Run:

```bash
git add internal/anvilmcp/scheduler_metrics.go internal/anvilmcp/scheduler_metrics_test.go internal/anvilmcp/scheduler_service.go
git commit -m "feat: expose scheduler metrics"
```

## Task 2: Smoke Harness Metrics Verification

**Files:**
- Modify: `scripts/anvil_scheduler_smoke_test.go`
- Modify: `scripts/anvil-scheduler-smoke.sh`

- [ ] **Step 1: Add failing smoke harness metrics tests**

Modify `scripts/anvil_scheduler_smoke_test.go`.

In `TestAnvilSchedulerSmokePassesAgainstFakeScheduler`, add this assertion after the existing `controlLoopStatusChecked` assertion:

```go
	if !server.metricsChecked() {
		t.Fatalf("smoke script did not call GET /metrics")
	}
```

Add `metricsCalls` and `metricsFailure` fields to `anvilSchedulerSmokeFakeServer`:

```go
	metricsCalls           int
	metricsFailure         string
```

Add these methods near `controlLoopStatusChecked()`:

```go
func (s *anvilSchedulerSmokeFakeServer) metricsChecked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metricsCalls > 0
}

func (s *anvilSchedulerSmokeFakeServer) failMetrics(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metricsFailure = message
}
```

Add this test near the other failure tests:

```go
func TestAnvilSchedulerSmokeFailsMetricsWithSummary(t *testing.T) {
	server := newAnvilSchedulerSmokeFakeServer(t, 0)
	server.failMetrics("metrics unavailable")
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "summary.json")
	cmd := exec.Command("bash", "anvil-scheduler-smoke.sh", "--base-url", server.URL, "--host-id", "smoke-test-host", "--json-out", outPath)
	cmd.Dir = scriptsDir(t)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("smoke script unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "metrics_failed") {
		t.Fatalf("output = %s, want metrics_failed", output)
	}
	if !strings.Contains(string(output), "metrics unavailable") {
		t.Fatalf("output = %s, want metrics unavailable", output)
	}

	summary := readSmokeSummary(t, outPath)
	if summary.OK {
		t.Fatalf("summary ok = true, want false")
	}
	if summary.FailedStep != "metrics_failed" {
		t.Fatalf("failed_step = %q, want metrics_failed", summary.FailedStep)
	}
}
```

Add a `/metrics` case in `newAnvilSchedulerSmokeFakeServerWithHost` before the `default` case:

```go
		case "/metrics":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			fake.mu.Lock()
			fake.metricsCalls++
			metricsFailure := fake.metricsFailure
			fake.mu.Unlock()
			if metricsFailure != "" {
				http.Error(w, metricsFailure, http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = fmt.Fprint(w, "# HELP anvil_scheduler_control_loop_running Scheduler control loop running flag.\n# TYPE anvil_scheduler_control_loop_running gauge\nanvil_scheduler_control_loop_running 1\n")
```

- [ ] **Step 2: Run smoke tests to verify RED**

Run:

```bash
go test ./scripts -run 'TestAnvilSchedulerSmoke' -count=1
```

Expected: FAIL because the smoke script does not call `/metrics`; `TestAnvilSchedulerSmokePassesAgainstFakeScheduler` reports the missing metrics call.

- [ ] **Step 3: Add text check helper to smoke script**

Modify `scripts/anvil-scheduler-smoke.sh`. Add this function after `require_json_key`:

```bash
require_text_contains() {
  local path="$1"
  local needle="$2"

  python3 - "$path" "$needle" <<'PY'
import sys

path, needle = sys.argv[1:3]
with open(path, encoding="utf-8", errors="replace") as handle:
    data = handle.read()
if needle not in data:
    raise SystemExit(1)
PY
}
```

- [ ] **Step 4: Call `GET /metrics` in smoke script**

Modify `scripts/anvil-scheduler-smoke.sh`. Add this block after the `/control-loop/status` validation and before cleanup:

```bash
METRICS_BODY="$TMP_DIR/metrics.txt"
METRICS_ERR="$TMP_DIR/metrics.err"
if ! METRICS_STATUS="$(request_json GET /metrics "" "$METRICS_BODY" "$METRICS_ERR")"; then
  fail_step metrics_failed "GET /metrics request failed: $(<"$METRICS_ERR")"
fi
if [[ "$METRICS_STATUS" != "200" ]]; then
  fail_step metrics_failed "GET /metrics returned HTTP $METRICS_STATUS body=$(response_body_snippet "$METRICS_BODY")"
fi
if ! require_text_contains "$METRICS_BODY" "anvil_scheduler_control_loop_running" 2>/dev/null; then
  fail_step metrics_failed "GET /metrics response missing anvil_scheduler_control_loop_running"
fi
```

- [ ] **Step 5: Run smoke tests to verify GREEN**

Run:

```bash
go test ./scripts -run 'TestAnvilSchedulerSmoke' -count=1
bash -n scripts/anvil-scheduler-smoke.sh
```

Expected: PASS for Go tests and no output from `bash -n`.

- [ ] **Step 6: Commit smoke metrics verification**

Run:

```bash
git add scripts/anvil_scheduler_smoke_test.go scripts/anvil-scheduler-smoke.sh
git commit -m "test: verify scheduler metrics in smoke harness"
```

## Task 3: Scheduler Full-Process Integration Test

**Files:**
- Modify: `cmd/anvil-scheduler/main_test.go`

- [ ] **Step 1: Write failing process integration test**

Modify `cmd/anvil-scheduler/main_test.go`.

Replace the import block with:

```go
import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ephemera/internal/anvilmcp"
)
```

Append these tests and helpers to the file:

```go
func TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules(t *testing.T) {
	if os.Getenv("ANVIL_SCHEDULER_TEST_CHILD") == "1" {
		return
	}

	fakeDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","vm_count":0}`))
		case "/vms":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeDaemon.Close()

	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "scheduler-state.json")
	quotaPath := filepath.Join(tempDir, "scheduler-quotas.json")
	hostsPath := filepath.Join(tempDir, "hosts.json")
	stale := anvilmcp.NewPlacementStore(statePath)
	if err := stale.SetHost(anvilmcp.RuntimeHost{Name: "host-a", Endpoint: "http://stale-host", Healthy: false, AvailableVMs: 0, EgressPolicies: []anvilmcp.EgressPolicy{anvilmcp.EgressPolicyProfile}}); err != nil {
		t.Fatalf("SetHost stale host: %v", err)
	}
	if err := stale.Save(); err != nil {
		t.Fatalf("Save stale scheduler state: %v", err)
	}
	hostsJSON := fmt.Sprintf(`{"hosts":[{"name":"host-a","endpoint":%q,"available_vms":2,"available_snapshot_bytes":4096,"egress_policies":["profile"]}]}`, fakeDaemon.URL)
	if err := os.WriteFile(hostsPath, []byte(hostsJSON), 0600); err != nil {
		t.Fatalf("write hosts file: %v", err)
	}

	addr := reserveLoopbackAddr(t)
	baseURL := "http://" + addr
	cmd := exec.Command(os.Args[0], "-test.run=TestSchedulerProcessChildMain", "-test.v")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_TEST_CHILD=1",
		"ANVIL_SCHEDULER_ADDR="+addr,
		"ANVIL_SCHEDULER_STATE="+statePath,
		"ANVIL_SCHEDULER_QUOTA_STORE="+quotaPath,
		"ANVIL_SCHEDULER_HOSTS_FILE="+hostsPath,
		"ANVIL_SCHEDULER_POLL_INTERVAL=50ms",
		"ANVIL_SCHEDULER_RECONCILE_INTERVAL=100ms",
		"ANVIL_SCHEDULER_HOST_TIMEOUT=500ms",
		"ANVIL_SCHEDULER_FAILURE_THRESHOLD=2",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start scheduler child: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	})

	waitForSchedulerBody(t, baseURL+"/health", func(body string) bool {
		return strings.Contains(body, `"status":"ok"`)
	}, &output)
	waitForSchedulerBody(t, baseURL+"/control-loop/status", func(body string) bool {
		return strings.Contains(body, `"running":true`)
	}, &output)
	waitForSchedulerBody(t, baseURL+"/metrics", func(body string) bool {
		return strings.Contains(body, "anvil_scheduler_control_loop_running 1") &&
			strings.Contains(body, "anvil_scheduler_host_status_count{status=\"healthy\"} 1")
	}, &output)

	resp, err := http.Post(baseURL+"/schedule/spawn", "application/json", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
	if err != nil {
		t.Fatalf("POST /schedule/spawn: %v\nscheduler output:\n%s", err, output.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /schedule/spawn status = %d, want 200\nscheduler output:\n%s", resp.StatusCode, output.String())
	}
	var decision anvilmcp.ScheduleDecision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		t.Fatalf("decode schedule decision: %v", err)
	}
	if !decision.Allowed || decision.Host.Name != "host-a" || decision.Host.AvailableVMs != 2 {
		t.Fatalf("decision = %+v, want allowed host-a with configured capacity", decision)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal scheduler child: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("scheduler child exited with error: %v\noutput:\n%s", err, output.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("scheduler child did not exit after SIGTERM\noutput:\n%s", output.String())
	}
}

func TestSchedulerProcessChildMain(t *testing.T) {
	if os.Getenv("ANVIL_SCHEDULER_TEST_CHILD") != "1" {
		return
	}
	main()
}

func reserveLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback addr: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}
	return addr
}

func waitForSchedulerBody(t *testing.T, url string, accepts func(string) bool, output *bytes.Buffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastBody string
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		bodyBytes := new(bytes.Buffer)
		_, _ = bodyBytes.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		lastBody = bodyBytes.String()
		if resp.StatusCode == http.StatusOK && accepts(lastBody) {
			return lastBody
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("scheduler endpoint %s did not become ready; lastErr=%v lastBody=%q\nscheduler output:\n%s", url, lastErr, lastBody, output.String())
	return ""
}
```

- [ ] **Step 2: Run process test to verify RED**

Run:

```bash
go test ./cmd/anvil-scheduler -run TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules -count=1
```

Expected: FAIL before Task 1 because `/metrics` is missing. If Task 1 is already complete, this test should pass after the process starts.

- [ ] **Step 3: Run process test to verify GREEN after metrics implementation**

Run:

```bash
go test ./cmd/anvil-scheduler -run TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules -count=1
```

Expected: PASS.

- [ ] **Step 4: Commit process integration test**

Run:

```bash
git add cmd/anvil-scheduler/main_test.go
git commit -m "test: cover scheduler process startup"
```

## Task 4: Documentation and Release Notes

**Files:**
- Modify: `README.md`
- Modify: `docs/operations/runbook.md`
- Modify: `docs/operations/observability.md`
- Modify: `docs/operations/release-checklist.md`
- Modify: `docs/operations/2026-05-29-anvil-follow-up-development.md`
- Modify: `RELEASE_NOTES.md`

- [ ] **Step 1: Update README scheduler operations section**

In `README.md`, update the scheduler service operations text near the existing scheduler endpoint list to include:

```markdown
Scheduler service는 operator JSON endpoint와 별도로 scheduler 전용 Prometheus text
`GET /metrics`를 제공한다. 이 endpoint는 daemon `/metrics`와 다른 surface이며
`anvil_scheduler_*` namespace로 control loop running flag, persistence degraded
flag, host status count, suspect placement count, last poll/reconcile timestamp를
반환한다. scheduler에는 자체 인증 계층이 없으므로 기존 scheduler 운영 경계처럼
loopback/private network 또는 reverse proxy policy 뒤에서만 노출한다.
```

- [ ] **Step 2: Update runbook with metrics checks**

In `docs/operations/runbook.md`, add this command block after the scheduler control loop status check:

````markdown
runtime scheduler metrics:

```bash
curl http://127.0.0.1:3010/metrics
```

`anvil_scheduler_persistence_degraded 1`이면 state file 저장 경로 권한과 disk 상태를
먼저 확인한다. `anvil_scheduler_host_status_count{status="unhealthy"}`가 0보다 크면
`/control-loop/status`의 host observation과 daemon host `/health`를 함께 확인한다.
```
````

Also add `metrics_failed` to the installer verify triage sentence:

```markdown
`metrics_failed`는 scheduler service가 `/metrics`를 제공하지 않거나 smoke가
`anvil_scheduler_control_loop_running` line을 찾지 못한 상태다.
```

- [ ] **Step 3: Update observability docs**

In `docs/operations/observability.md`, add this section after the runtime scheduler service health commands:

````markdown
## Scheduler metrics endpoint

runtime scheduler service는 scheduler control-plane 상태를 Prometheus text 형식으로
노출한다.

```bash
curl http://127.0.0.1:3010/metrics
```

현재 scheduler metric family:

- `anvil_scheduler_control_loop_running`
- `anvil_scheduler_persistence_degraded`
- `anvil_scheduler_host_status_count{status="healthy|degraded|unhealthy|unknown"}`
- `anvil_scheduler_suspect_vm_placements`
- `anvil_scheduler_last_poll_completed_timestamp_seconds`
- `anvil_scheduler_last_reconcile_completed_timestamp_seconds`
- `anvil_scheduler_poll_interval_seconds`
- `anvil_scheduler_reconcile_interval_seconds`

metric label에는 host name, endpoint, raw daemon response, authorization header,
`agent_token`을 넣지 않는다. scheduler service에는 자체 인증 계층이 없으므로
loopback/private network 또는 reverse proxy policy 뒤에서만 scrape한다.
```
````

- [ ] **Step 4: Update release checklist**

In `docs/operations/release-checklist.md`, add these verification commands to the scheduler production automation gate:

```markdown
go test ./cmd/anvil-scheduler -run TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules -count=1
curl http://127.0.0.1:3010/metrics
```

Add this release gate note:

```markdown
Scheduler release candidate는 `/control-loop/status`와 `/metrics`를 모두 smoke로
검증해야 한다. `/metrics`에는 `agent_token`, daemon raw body, host endpoint가 나오면
안 된다.
```

- [ ] **Step 5: Update follow-up development document**

In `docs/operations/2026-05-29-anvil-follow-up-development.md`, add this status note under "현재 기준":

```markdown
- 2026-06-02 scheduler operations hardening 범위에서 1-3번은 구현 검증 대상으로
  승격됐다. full-process integration test, scheduler `/metrics`, smoke metrics check,
  actual systemd `--start --verify` 결과는
  `docs/operations/2026-06-02-scheduler-operations-hardening-handoff.md`에 기록한다.
```

- [ ] **Step 6: Update release notes**

At the top of `RELEASE_NOTES.md`, add:

```markdown
# Unreleased — Scheduler operations hardening

## 추가됨

- scheduler service `GET /metrics` endpoint:
  - `anvil_scheduler_control_loop_running`
  - `anvil_scheduler_persistence_degraded`
  - `anvil_scheduler_host_status_count`
  - `anvil_scheduler_suspect_vm_placements`
  - last poll/reconcile timestamp gauges
- `cmd/anvil-scheduler` full-process integration test coverage for hosts file bootstrap,
  stale state override, fake daemon `/health`, `/control-loop/status`, `/schedule/spawn`,
  and `/metrics`.
- scheduler smoke harness `/metrics` verification.

## 보안/운영 hardening

- scheduler metrics do not include `agent_token`, host endpoint, daemon raw response,
  authorization header, or snapshot metadata.
- actual systemd verification remains an explicit operator action:
  `sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify`.

## 검증 예정

- `go test ./... -count=1`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `bash -n scripts/anvil-scheduler-smoke.sh`
- `bash -n scripts/install-anvil-scheduler-systemd.sh`
- approval-gated systemd `--start --verify`
```

- [ ] **Step 7: Run docs checks**

Run:

```bash
git diff --check
```

Expected: PASS.

- [ ] **Step 8: Commit docs**

Run:

```bash
git add README.md docs/operations/runbook.md docs/operations/observability.md docs/operations/release-checklist.md docs/operations/2026-05-29-anvil-follow-up-development.md RELEASE_NOTES.md
git commit -m "docs: document scheduler operations hardening"
```

## Task 5: Verification and Systemd Gate

**Files:**
- Create: `docs/operations/2026-06-02-scheduler-operations-hardening-handoff.md`

- [ ] **Step 1: Run focused verification**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRenderSchedulerMetrics|TestSchedulerServiceMetrics' -count=1
go test ./scripts -run 'TestAnvilSchedulerSmoke' -count=1
go test ./cmd/anvil-scheduler -run TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules -count=1
```

Expected: all PASS.

- [ ] **Step 2: Run full local verification**

Run:

```bash
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
bash -n scripts/anvil-scheduler-smoke.sh
bash -n scripts/install-anvil-scheduler-systemd.sh
git diff --check
```

Expected: all PASS and no whitespace errors.

- [ ] **Step 3: Run approval-gated actual systemd verification**

Request escalation for:

```bash
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

Expected: installer completes, `anvil-scheduler.service` is active, and smoke harness passes including `/metrics`.

- [ ] **Step 4: Create operation handoff**

Create `docs/operations/2026-06-02-scheduler-operations-hardening-handoff.md` with the actual verification outcomes observed in Steps 1-3:

```markdown
# Scheduler Operations Hardening Handoff

작성일: 2026-06-02

## Release Scope

- scheduler `GET /metrics` endpoint added.
- scheduler smoke harness verifies `/metrics`.
- `cmd/anvil-scheduler` full-process integration test added for hosts file bootstrap,
  stale state override, fake daemon health, control loop status, scheduling, and metrics.
- scheduler operations docs and release notes updated.

## Verification

- `go test ./internal/anvilmcp -run 'TestRenderSchedulerMetrics|TestSchedulerServiceMetrics' -count=1`: PASS
- `go test ./scripts -run 'TestAnvilSchedulerSmoke' -count=1`: PASS
- `go test ./cmd/anvil-scheduler -run TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules -count=1`: PASS
- `go test ./... -count=1`: PASS
- `go build ./cmd/goose-daemon`: PASS
- `go build ./cmd/anvil-mcp`: PASS
- `go build ./cmd/anvil-scheduler`: PASS
- `bash -n scripts/anvil-scheduler-smoke.sh`: PASS
- `bash -n scripts/install-anvil-scheduler-systemd.sh`: PASS
- `git diff --check`: PASS
- `sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify`: PASS or recorded failure with command output summary

## Audit

- New scheduler metrics use `anvil_scheduler_*` namespace.
- Metrics do not expose `agent_token`, daemon raw response, authorization header, host endpoint, or snapshot metadata.
- Scheduler service remains loopback/private-network oriented and does not add an auth layer in this change.

## Blockers

- None if all verification commands pass.

## Warnings

- Actual systemd verification changes `/etc/anvil`, `/var/lib/anvil`, `/usr/local/bin/anvil-scheduler`, and `anvil-scheduler.service`.

## Residual Risk

- Scheduler metrics are unauthenticated because scheduler service itself is unauthenticated. External exposure still requires loopback/private network or reverse proxy policy.
- Process integration test does not exercise KVM, Firecracker, or real daemon VM lifecycle.

## Current Lifecycle Stage

Operate has been entered for scheduler operations hardening after verification and review.

## Next Action

- Continue with follow-up item 4, cross-host snapshot replication, only after a separate design spec.

## Follow-Up Tasks

- Add scheduler poll/reconcile failure counters in a later release if alerting needs error-rate metrics.
- Keep upstream ephemera `v0.4.0`-`v0.5.0` adoption review separate from this scheduler hardening work.
```

- [ ] **Step 5: Commit handoff**

Run:

```bash
git add docs/operations/2026-06-02-scheduler-operations-hardening-handoff.md
git commit -m "docs: record scheduler operations handoff"
```

## Plan Self-Review

- Spec coverage: covers full-process integration test, systemd verification path, scheduler metrics, docs, and handoff.
- Red-flag scan: no incomplete sections or vague implementation steps are used.
- Type consistency: metrics use existing `PlacementStoreState`, `ControlLoopStatus`, `RuntimeHost`, `HostObservation`, `HostStatusSummary`, and `ScheduleDecision` types.
- Execution environment constraints: local tests require Go 1.25 and loopback HTTP. Actual systemd verification requires root, systemd, write access to `/etc/anvil`, `/var/lib/anvil`, `/usr/local/bin`, and the ability to restart `anvil-scheduler.service`.
