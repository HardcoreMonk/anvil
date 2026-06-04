# Scheduler Flock Placement Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add persisted scheduler flock placement counters and latency histograms to existing scheduler `/metrics`.

**Architecture:** `RuntimeRouter.CreateFlock` records bounded aggregate observations into `PlacementStoreState`. `RenderSchedulerMetrics` renders the aggregate as `anvil_scheduler_flock_placement_*` Prometheus text without high-cardinality or secret-bearing labels. Metrics recording is best-effort and must not change flock lifecycle behavior.

**Tech Stack:** Go standard library, existing `internal/anvilmcp` placement store and scheduler metrics renderer, existing Go tests.

---

## File Structure

- Create `internal/anvilmcp/flock_placement_metrics.go`: bounded outcome/reason/phase constants, histogram bucket helpers, `PlacementStore.RecordFlockPlacementMetrics`, and sanitization helpers.
- Create `internal/anvilmcp/flock_placement_metrics_test.go`: focused tests for state aggregation, histogram bucket counts, bounded labels, and state cloning.
- Modify `internal/anvilmcp/placement_store.go`: add `FlockPlacementMetricsState` to `PlacementStoreState`, normalize/clone it, and keep zero-state compatibility.
- Modify `internal/anvilmcp/scheduler_metrics.go`: render `anvil_scheduler_flock_placement_*` counters, histograms, and timestamp gauges.
- Modify `internal/anvilmcp/scheduler_metrics_test.go`: verify renderer output and no token/endpoint/high-cardinality leakage.
- Modify `internal/anvilmcp/runtime_router.go`: time scheduler, daemon create, placement save, total phases and record best-effort observations on each outcome.
- Modify `internal/anvilmcp/runtime_router_test.go`: verify success, denial, daemon error, and nil response metrics.
- Modify `RELEASE_NOTES.md`: add a new top-level `Unreleased` section for flock placement metrics.
- Modify `docs/operations/observability.md`: document new metrics and label security boundary.
- Create `docs/operations/2026-06-04-flock-placement-metrics-handoff.md`: record verification and residual risk.

---

### Task 1: Placement Store Metrics State

**Files:**
- Create: `internal/anvilmcp/flock_placement_metrics.go`
- Create: `internal/anvilmcp/flock_placement_metrics_test.go`
- Modify: `internal/anvilmcp/placement_store.go`

- [ ] **Step 1: Write failing placement metrics state tests**

Create `internal/anvilmcp/flock_placement_metrics_test.go`:

```go
package anvilmcp

import (
	"testing"
	"time"
)

func TestPlacementStoreRecordsFlockPlacementMetrics(t *testing.T) {
	store := NewPlacementStore("")
	at := time.Date(2026, 6, 4, 1, 2, 3, 0, time.UTC)

	if err := store.RecordFlockPlacementMetrics(FlockPlacementMetricObservation{
		Outcome: FlockPlacementOutcomeSuccess,
		Reason:  FlockPlacementReasonScheduled,
		At:      at,
		Latencies: map[string]time.Duration{
			FlockPlacementPhaseSchedule:      7 * time.Millisecond,
			FlockPlacementPhaseDaemonCreate: 120 * time.Millisecond,
			FlockPlacementPhasePlacementSave: 3 * time.Millisecond,
			FlockPlacementPhaseTotal:         140 * time.Millisecond,
		},
	}); err != nil {
		t.Fatalf("RecordFlockPlacementMetrics() error = %v", err)
	}

	state := store.State().FlockPlacementMetrics
	key := flockPlacementAttemptKey(FlockPlacementOutcomeSuccess, FlockPlacementReasonScheduled)
	if got := state.AttemptsByOutcomeReason[key]; got != 1 {
		t.Fatalf("attempt count = %d, want 1", got)
	}
	if !state.LastSuccessAt.Equal(at) {
		t.Fatalf("LastSuccessAt = %v, want %v", state.LastSuccessAt, at)
	}
	if !state.LastFailureAt.IsZero() {
		t.Fatalf("LastFailureAt = %v, want zero", state.LastFailureAt)
	}

	schedule := state.LatencyByPhase[FlockPlacementPhaseSchedule]
	if schedule.Count != 1 || schedule.SumSeconds <= 0 {
		t.Fatalf("schedule histogram = %+v, want one positive observation", schedule)
	}
	if schedule.Buckets["0.01"] != 1 || schedule.Buckets["+Inf"] != 1 {
		t.Fatalf("schedule buckets = %+v, want observation in 0.01 and +Inf", schedule.Buckets)
	}
}

func TestPlacementStoreSanitizesFlockPlacementMetricLabels(t *testing.T) {
	store := NewPlacementStore("")

	if err := store.RecordFlockPlacementMetrics(FlockPlacementMetricObservation{
		Outcome: "tenant-1",
		Reason:  "http://host-a:3000/agent_token",
		At:      time.Date(2026, 6, 4, 1, 2, 3, 0, time.UTC),
		Latencies: map[string]time.Duration{
			"vm-1": 15 * time.Millisecond,
		},
	}); err != nil {
		t.Fatalf("RecordFlockPlacementMetrics() error = %v", err)
	}

	state := store.State().FlockPlacementMetrics
	key := flockPlacementAttemptKey(FlockPlacementOutcomeSchedulerError, FlockPlacementReasonUnknown)
	if got := state.AttemptsByOutcomeReason[key]; got != 1 {
		t.Fatalf("sanitized attempt count = %d, want 1", got)
	}
	if len(state.LatencyByPhase) != 0 {
		t.Fatalf("LatencyByPhase = %+v, want invalid phase ignored", state.LatencyByPhase)
	}
}

func TestPlacementStoreClonesFlockPlacementMetrics(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.RecordFlockPlacementMetrics(FlockPlacementMetricObservation{
		Outcome: FlockPlacementOutcomeDenied,
		Reason:  FlockPlacementReasonNoEligibleHost,
		At:      time.Date(2026, 6, 4, 1, 2, 3, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordFlockPlacementMetrics() error = %v", err)
	}

	state := store.State()
	key := flockPlacementAttemptKey(FlockPlacementOutcomeDenied, FlockPlacementReasonNoEligibleHost)
	state.FlockPlacementMetrics.AttemptsByOutcomeReason[key] = 99
	state.FlockPlacementMetrics.LastFailureAt = time.Time{}

	reloaded := store.State().FlockPlacementMetrics
	if got := reloaded.AttemptsByOutcomeReason[key]; got != 1 {
		t.Fatalf("stored attempt count changed through clone = %d, want 1", got)
	}
	if reloaded.LastFailureAt.IsZero() {
		t.Fatalf("stored LastFailureAt was mutated through clone")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestPlacementStore.*FlockPlacementMetrics' -count=1
```

Expected: FAIL with undefined identifiers such as `FlockPlacementMetricObservation` and missing `FlockPlacementMetrics` field.

- [ ] **Step 3: Add metrics state types and placement store normalization**

In `internal/anvilmcp/placement_store.go`, add the new field to `PlacementStoreState`:

```go
type PlacementStoreState struct {
	Hosts                 map[string]RuntimeHost        `json:"hosts"`
	VMPlacements          map[string]string             `json:"vm_placements"`
	SnapshotLocations     map[string][]string           `json:"snapshot_locations"`
	ConfigManagedHosts    map[string]bool               `json:"config_managed_hosts,omitempty"`
	HostObservations      map[string]HostObservation    `json:"host_observations,omitempty"`
	SuspectVMPlacements   map[string]SuspectVMPlacement `json:"suspect_vm_placements,omitempty"`
	ControlLoopStatus     ControlLoopStatus             `json:"control_loop_status,omitempty"`
	FlockPlacementMetrics FlockPlacementMetricsState    `json:"flock_placement_metrics,omitempty"`
}
```

In `NewPlacementStore`, initialize the field:

```go
FlockPlacementMetrics: newFlockPlacementMetricsState(),
```

In `normalizePlacementStoreState`, add:

```go
normalizeFlockPlacementMetricsState(&state.FlockPlacementMetrics)
```

In `clonePlacementStoreState`, add:

```go
out.FlockPlacementMetrics = cloneFlockPlacementMetricsState(state.FlockPlacementMetrics)
```

- [ ] **Step 4: Add metrics helper implementation**

Create `internal/anvilmcp/flock_placement_metrics.go`:

```go
package anvilmcp

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	FlockPlacementOutcomeSuccess            = "success"
	FlockPlacementOutcomeDenied             = "denied"
	FlockPlacementOutcomeSchedulerError     = "scheduler_error"
	FlockPlacementOutcomeDaemonError        = "daemon_error"
	FlockPlacementOutcomeDaemonNilResponse  = "daemon_nil_response"
	FlockPlacementOutcomePlacementSaveError = "placement_save_error"

	FlockPlacementReasonScheduled           = "scheduled"
	FlockPlacementReasonQuotaExceeded       = "quota_exceeded"
	FlockPlacementReasonNoEligibleHost      = "no_eligible_host"
	FlockPlacementReasonInvalidRequest      = "invalid_request"
	FlockPlacementReasonDaemonCreateFailed  = "daemon_create_failed"
	FlockPlacementReasonDaemonNilResponse   = "daemon_nil_response"
	FlockPlacementReasonPlacementSaveFailed = "placement_save_failed"
	FlockPlacementReasonUnknown             = "unknown"

	FlockPlacementPhaseSchedule      = "schedule"
	FlockPlacementPhaseDaemonCreate  = "daemon_create"
	FlockPlacementPhasePlacementSave = "placement_save"
	FlockPlacementPhaseTotal         = "total"
)

var flockPlacementLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type FlockPlacementMetricObservation struct {
	Outcome   string
	Reason    string
	Latencies map[string]time.Duration
	At        time.Time
}

type FlockPlacementMetricsState struct {
	AttemptsByOutcomeReason map[string]int64                   `json:"attempts_by_outcome_reason,omitempty"`
	LatencyByPhase          map[string]LatencyHistogramState   `json:"latency_by_phase,omitempty"`
	LastSuccessAt            time.Time                          `json:"last_success_at,omitempty"`
	LastFailureAt            time.Time                          `json:"last_failure_at,omitempty"`
}

type LatencyHistogramState struct {
	Buckets    map[string]int64 `json:"buckets,omitempty"`
	SumSeconds float64          `json:"sum_seconds,omitempty"`
	Count      int64            `json:"count,omitempty"`
}

func newFlockPlacementMetricsState() FlockPlacementMetricsState {
	return FlockPlacementMetricsState{
		AttemptsByOutcomeReason: make(map[string]int64),
		LatencyByPhase:          make(map[string]LatencyHistogramState),
	}
}

func normalizeFlockPlacementMetricsState(state *FlockPlacementMetricsState) {
	if state.AttemptsByOutcomeReason == nil {
		state.AttemptsByOutcomeReason = make(map[string]int64)
	}
	if state.LatencyByPhase == nil {
		state.LatencyByPhase = make(map[string]LatencyHistogramState)
	}
	for phase, hist := range state.LatencyByPhase {
		if !isAllowedFlockPlacementPhase(phase) {
			delete(state.LatencyByPhase, phase)
			continue
		}
		if hist.Buckets == nil {
			hist.Buckets = make(map[string]int64)
		}
		state.LatencyByPhase[phase] = hist
	}
}

func cloneFlockPlacementMetricsState(state FlockPlacementMetricsState) FlockPlacementMetricsState {
	normalizeFlockPlacementMetricsState(&state)
	out := newFlockPlacementMetricsState()
	for key, count := range state.AttemptsByOutcomeReason {
		out.AttemptsByOutcomeReason[key] = count
	}
	for phase, hist := range state.LatencyByPhase {
		buckets := make(map[string]int64, len(hist.Buckets))
		for bucket, count := range hist.Buckets {
			buckets[bucket] = count
		}
		out.LatencyByPhase[phase] = LatencyHistogramState{
			Buckets:    buckets,
			SumSeconds: hist.SumSeconds,
			Count:      hist.Count,
		}
	}
	out.LastSuccessAt = state.LastSuccessAt
	out.LastFailureAt = state.LastFailureAt
	return out
}

func (s *PlacementStore) RecordFlockPlacementMetrics(obs FlockPlacementMetricObservation) error {
	if obs.At.IsZero() {
		obs.At = time.Now().UTC()
	}
	outcome := normalizeFlockPlacementOutcome(obs.Outcome)
	reason := normalizeFlockPlacementReason(obs.Reason)

	s.mu.Lock()
	s.ensureMaps()
	normalizeFlockPlacementMetricsState(&s.state.FlockPlacementMetrics)
	s.state.FlockPlacementMetrics.AttemptsByOutcomeReason[flockPlacementAttemptKey(outcome, reason)]++
	for phase, duration := range obs.Latencies {
		phase = normalizeFlockPlacementPhase(phase)
		if phase == "" || duration < 0 {
			continue
		}
		hist := s.state.FlockPlacementMetrics.LatencyByPhase[phase]
		recordLatencyHistogramObservation(&hist, duration)
		s.state.FlockPlacementMetrics.LatencyByPhase[phase] = hist
	}
	if outcome == FlockPlacementOutcomeSuccess {
		s.state.FlockPlacementMetrics.LastSuccessAt = obs.At
	} else {
		s.state.FlockPlacementMetrics.LastFailureAt = obs.At
	}
	s.mu.Unlock()
	return s.Save()
}

func recordLatencyHistogramObservation(hist *LatencyHistogramState, duration time.Duration) {
	if hist.Buckets == nil {
		hist.Buckets = make(map[string]int64)
	}
	seconds := duration.Seconds()
	hist.SumSeconds += seconds
	hist.Count++
	for _, upper := range flockPlacementLatencyBuckets {
		if seconds <= upper {
			hist.Buckets[formatFlockPlacementBucket(upper)]++
		}
	}
	hist.Buckets["+Inf"]++
}

func flockPlacementAttemptKey(outcome, reason string) string {
	return normalizeFlockPlacementOutcome(outcome) + "|" + normalizeFlockPlacementReason(reason)
}

func splitFlockPlacementAttemptKey(key string) (string, string) {
	parts := strings.SplitN(key, "|", 2)
	if len(parts) != 2 {
		return FlockPlacementOutcomeSchedulerError, FlockPlacementReasonUnknown
	}
	return normalizeFlockPlacementOutcome(parts[0]), normalizeFlockPlacementReason(parts[1])
}

func normalizeFlockPlacementOutcome(value string) string {
	switch strings.TrimSpace(value) {
	case FlockPlacementOutcomeSuccess:
		return FlockPlacementOutcomeSuccess
	case FlockPlacementOutcomeDenied:
		return FlockPlacementOutcomeDenied
	case FlockPlacementOutcomeDaemonError:
		return FlockPlacementOutcomeDaemonError
	case FlockPlacementOutcomeDaemonNilResponse:
		return FlockPlacementOutcomeDaemonNilResponse
	case FlockPlacementOutcomePlacementSaveError:
		return FlockPlacementOutcomePlacementSaveError
	default:
		return FlockPlacementOutcomeSchedulerError
	}
}

func normalizeFlockPlacementReason(value string) string {
	switch strings.TrimSpace(value) {
	case FlockPlacementReasonScheduled:
		return FlockPlacementReasonScheduled
	case FlockPlacementReasonQuotaExceeded:
		return FlockPlacementReasonQuotaExceeded
	case FlockPlacementReasonNoEligibleHost:
		return FlockPlacementReasonNoEligibleHost
	case FlockPlacementReasonInvalidRequest:
		return FlockPlacementReasonInvalidRequest
	case FlockPlacementReasonDaemonCreateFailed:
		return FlockPlacementReasonDaemonCreateFailed
	case FlockPlacementReasonDaemonNilResponse:
		return FlockPlacementReasonDaemonNilResponse
	case FlockPlacementReasonPlacementSaveFailed:
		return FlockPlacementReasonPlacementSaveFailed
	default:
		return FlockPlacementReasonUnknown
	}
}

func normalizeFlockPlacementPhase(value string) string {
	value = strings.TrimSpace(value)
	if isAllowedFlockPlacementPhase(value) {
		return value
	}
	return ""
}

func isAllowedFlockPlacementPhase(value string) bool {
	switch value {
	case FlockPlacementPhaseSchedule, FlockPlacementPhaseDaemonCreate, FlockPlacementPhasePlacementSave, FlockPlacementPhaseTotal:
		return true
	default:
		return false
	}
}

func formatFlockPlacementBucket(value float64) string {
	return fmt.Sprintf("%g", value)
}

func sortedFlockPlacementAttemptKeys(attempts map[string]int64) []string {
	keys := make([]string, 0, len(attempts))
	for key := range attempts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
```

- [ ] **Step 5: Run placement metrics state tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestPlacementStore.*FlockPlacementMetrics' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit state helpers**

```bash
git add internal/anvilmcp/placement_store.go internal/anvilmcp/flock_placement_metrics.go internal/anvilmcp/flock_placement_metrics_test.go
git commit -m "feat: persist flock placement metrics state"
```

---

### Task 2: Scheduler Metrics Renderer

**Files:**
- Modify: `internal/anvilmcp/scheduler_metrics.go`
- Modify: `internal/anvilmcp/scheduler_metrics_test.go`

- [ ] **Step 1: Write failing renderer test**

Append this test to `internal/anvilmcp/scheduler_metrics_test.go`:

```go
func TestRenderSchedulerMetricsIncludesFlockPlacementMetrics(t *testing.T) {
	successAt := time.Date(2026, 6, 4, 1, 2, 3, 0, time.UTC)
	failureAt := time.Date(2026, 6, 4, 1, 3, 4, 0, time.UTC)
	state := PlacementStoreState{
		Hosts: map[string]RuntimeHost{
			"host-a": {Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2},
		},
		FlockPlacementMetrics: FlockPlacementMetricsState{
			AttemptsByOutcomeReason: map[string]int64{
				flockPlacementAttemptKey(FlockPlacementOutcomeSuccess, FlockPlacementReasonScheduled):      2,
				flockPlacementAttemptKey(FlockPlacementOutcomeDenied, FlockPlacementReasonQuotaExceeded): 1,
			},
			LatencyByPhase: map[string]LatencyHistogramState{
				FlockPlacementPhaseSchedule: {
					Buckets: map[string]int64{"0.005": 1, "0.01": 2, "+Inf": 2},
					SumSeconds: 0.012,
					Count:      2,
				},
			},
			LastSuccessAt: successAt,
			LastFailureAt: failureAt,
		},
	}

	output := RenderSchedulerMetrics(state)
	requireMetricLine(t, output, "anvil_scheduler_flock_placement_attempts_total{outcome=\"denied\",reason=\"quota_exceeded\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_flock_placement_attempts_total{outcome=\"success\",reason=\"scheduled\"} 2")
	requireMetricLine(t, output, "anvil_scheduler_flock_placement_latency_seconds_bucket{phase=\"schedule\",le=\"0.005\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_flock_placement_latency_seconds_bucket{phase=\"schedule\",le=\"0.01\"} 2")
	requireMetricLine(t, output, "anvil_scheduler_flock_placement_latency_seconds_bucket{phase=\"schedule\",le=\"+Inf\"} 2")
	requireMetricLine(t, output, "anvil_scheduler_flock_placement_latency_seconds_sum{phase=\"schedule\"} 0.012")
	requireMetricLine(t, output, "anvil_scheduler_flock_placement_latency_seconds_count{phase=\"schedule\"} 2")
	requireMetricLine(t, output, fmt.Sprintf("anvil_scheduler_flock_placement_last_success_timestamp_seconds %d", successAt.Unix()))
	requireMetricLine(t, output, fmt.Sprintf("anvil_scheduler_flock_placement_last_failure_timestamp_seconds %d", failureAt.Unix()))
	if strings.Contains(output, "http://host-a") || strings.Contains(output, "tenant-1") ||
		strings.Contains(output, "flock-1") || strings.Contains(output, "vm-1") ||
		strings.Contains(output, "agent_token") {
		t.Fatalf("metrics output leaked high-cardinality or token-like data:\n%s", output)
	}
}
```

- [ ] **Step 2: Run renderer test to verify it fails**

Run:

```bash
go test ./internal/anvilmcp -run TestRenderSchedulerMetricsIncludesFlockPlacementMetrics -count=1
```

Expected: FAIL because the new metric lines are not rendered.

- [ ] **Step 3: Implement metrics rendering**

In `internal/anvilmcp/scheduler_metrics.go`, add `renderFlockPlacementMetrics` and call it before `return out.String()`:

```go
	renderFlockPlacementMetrics(&out, state.FlockPlacementMetrics)
	return out.String()
```

Add these helpers to the same file:

```go
func renderFlockPlacementMetrics(out *strings.Builder, state FlockPlacementMetricsState) {
	normalizeFlockPlacementMetricsState(&state)

	out.WriteString("# HELP anvil_scheduler_flock_placement_attempts_total Scheduler flock placement attempts by bounded outcome and reason.\n")
	out.WriteString("# TYPE anvil_scheduler_flock_placement_attempts_total counter\n")
	for _, key := range sortedFlockPlacementAttemptKeys(state.AttemptsByOutcomeReason) {
		outcome, reason := splitFlockPlacementAttemptKey(key)
		fmt.Fprintf(out, "anvil_scheduler_flock_placement_attempts_total{outcome=%q,reason=%q} %d\n", outcome, reason, state.AttemptsByOutcomeReason[key])
	}

	out.WriteString("# HELP anvil_scheduler_flock_placement_latency_seconds Scheduler flock placement latency by bounded phase.\n")
	out.WriteString("# TYPE anvil_scheduler_flock_placement_latency_seconds histogram\n")
	for _, phase := range []string{FlockPlacementPhaseSchedule, FlockPlacementPhaseDaemonCreate, FlockPlacementPhasePlacementSave, FlockPlacementPhaseTotal} {
		hist := state.LatencyByPhase[phase]
		for _, upper := range flockPlacementLatencyBuckets {
			bucket := formatFlockPlacementBucket(upper)
			fmt.Fprintf(out, "anvil_scheduler_flock_placement_latency_seconds_bucket{phase=%q,le=%q} %d\n", phase, bucket, hist.Buckets[bucket])
		}
		fmt.Fprintf(out, "anvil_scheduler_flock_placement_latency_seconds_bucket{phase=%q,le=\"+Inf\"} %d\n", phase, hist.Buckets["+Inf"])
		fmt.Fprintf(out, "anvil_scheduler_flock_placement_latency_seconds_sum{phase=%q} %s\n", phase, strconv.FormatFloat(hist.SumSeconds, 'f', -1, 64))
		fmt.Fprintf(out, "anvil_scheduler_flock_placement_latency_seconds_count{phase=%q} %d\n", phase, hist.Count)
	}

	writeSchedulerGauge(out, "anvil_scheduler_flock_placement_last_success_timestamp_seconds", "Unix timestamp of the last successful scheduler flock placement.", timestampMetric(state.LastSuccessAt))
	writeSchedulerGauge(out, "anvil_scheduler_flock_placement_last_failure_timestamp_seconds", "Unix timestamp of the last failed scheduler flock placement.", timestampMetric(state.LastFailureAt))
}
```

- [ ] **Step 4: Run renderer tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRenderSchedulerMetrics|TestSchedulerServiceMetrics' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit renderer**

```bash
git add internal/anvilmcp/scheduler_metrics.go internal/anvilmcp/scheduler_metrics_test.go
git commit -m "feat: expose flock placement scheduler metrics"
```

---

### Task 3: Runtime Router Instrumentation

**Files:**
- Modify: `internal/anvilmcp/runtime_router.go`
- Modify: `internal/anvilmcp/runtime_router_test.go`

- [ ] **Step 1: Write failing router metrics tests**

Append these tests to `internal/anvilmcp/runtime_router_test.go`:

```go
func TestRuntimeRouterCreateFlockRecordsSuccessMetrics(t *testing.T) {
	store := NewPlacementStore("")
	host := RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 3, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}
	daemon := &routerFlockFakeDaemon{flockResp: &FlockCreateResponse{
		FlockID: "flock-1",
		Agents: []FlockAgentInfo{
			{AgentID: "agent-1", VMID: "vm-1"},
			{AgentID: "agent-2", VMID: "vm-2"},
		},
	}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler([]RuntimeHost{host}, nil, nil),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	if _, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Task:         "ship",
		Roles:        []string{"orchestrator", "worker"},
	}); err != nil {
		t.Fatalf("CreateFlock() error = %v", err)
	}

	metrics := store.State().FlockPlacementMetrics
	requireFlockPlacementAttempt(t, metrics, FlockPlacementOutcomeSuccess, FlockPlacementReasonScheduled, 1)
	requireFlockPlacementHistogramCount(t, metrics, FlockPlacementPhaseSchedule, 1)
	requireFlockPlacementHistogramCount(t, metrics, FlockPlacementPhaseDaemonCreate, 1)
	requireFlockPlacementHistogramCount(t, metrics, FlockPlacementPhasePlacementSave, 1)
	requireFlockPlacementHistogramCount(t, metrics, FlockPlacementPhaseTotal, 1)
	if metrics.LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt is zero")
	}
}

func TestRuntimeRouterCreateFlockRecordsDeniedMetrics(t *testing.T) {
	store := NewPlacementStore("")
	host := RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 3, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}
	daemon := &routerFlockFakeDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler([]RuntimeHost{host}, map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}}, map[string]TenantUsage{"tenant-1": {ActiveVMs: 1}}),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Task:         "ship",
		Roles:        []string{"orchestrator", "worker"},
	})
	if err == nil {
		t.Fatal("CreateFlock() error = nil, want quota denial")
	}

	metrics := store.State().FlockPlacementMetrics
	requireFlockPlacementAttempt(t, metrics, FlockPlacementOutcomeDenied, FlockPlacementReasonQuotaExceeded, 1)
	requireFlockPlacementHistogramCount(t, metrics, FlockPlacementPhaseSchedule, 1)
	requireFlockPlacementHistogramCount(t, metrics, FlockPlacementPhaseTotal, 1)
	if daemon.createFlockCalls != 0 {
		t.Fatalf("daemon CreateFlock calls = %d, want 0", daemon.createFlockCalls)
	}
}

func TestRuntimeRouterCreateFlockRecordsDaemonErrorMetrics(t *testing.T) {
	store := NewPlacementStore("")
	host := RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 3, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}
	daemon := &routerFlockFakeDaemon{createFlockErr: errors.New("agent_token raw body should not become a label")}
	router := NewRuntimeRouterWithOptions(
		NewScheduler([]RuntimeHost{host}, nil, nil),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Task:         "ship",
		Roles:        []string{"orchestrator"},
	})
	if err == nil {
		t.Fatal("CreateFlock() error = nil, want daemon error")
	}

	metrics := store.State().FlockPlacementMetrics
	requireFlockPlacementAttempt(t, metrics, FlockPlacementOutcomeDaemonError, FlockPlacementReasonDaemonCreateFailed, 1)
	requireFlockPlacementHistogramCount(t, metrics, FlockPlacementPhaseDaemonCreate, 1)
	if strings.Contains(RenderSchedulerMetrics(store.State()), "agent_token") {
		t.Fatalf("daemon error leaked into metrics:\n%s", RenderSchedulerMetrics(store.State()))
	}
}

func TestRuntimeRouterCreateFlockRecordsNilResponseMetrics(t *testing.T) {
	store := NewPlacementStore("")
	host := RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 3, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}
	daemon := &routerFlockFakeDaemon{flockResp: nil}
	router := NewRuntimeRouterWithOptions(
		NewScheduler([]RuntimeHost{host}, nil, nil),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Task:         "ship",
		Roles:        []string{"orchestrator"},
	})
	if err == nil {
		t.Fatal("CreateFlock() error = nil, want nil response error")
	}

	requireFlockPlacementAttempt(t, store.State().FlockPlacementMetrics, FlockPlacementOutcomeDaemonNilResponse, FlockPlacementReasonDaemonNilResponse, 1)
}

func requireFlockPlacementAttempt(t *testing.T, metrics FlockPlacementMetricsState, outcome, reason string, want int64) {
	t.Helper()
	key := flockPlacementAttemptKey(outcome, reason)
	if got := metrics.AttemptsByOutcomeReason[key]; got != want {
		t.Fatalf("attempt[%s] = %d, want %d", key, got, want)
	}
}

func requireFlockPlacementHistogramCount(t *testing.T, metrics FlockPlacementMetricsState, phase string, want int64) {
	t.Helper()
	hist := metrics.LatencyByPhase[phase]
	if hist.Count != want {
		t.Fatalf("histogram[%s].Count = %d, want %d; hist=%+v", phase, hist.Count, want, hist)
	}
}
```

If `runtime_router_test.go` does not already import `errors` and `strings`, add them to its import block.

- [ ] **Step 2: Run router tests to verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterCreateFlockRecords.*Metrics' -count=1
```

Expected: FAIL because `RuntimeRouter.CreateFlock` does not record metrics.

- [ ] **Step 3: Instrument `RuntimeRouter.CreateFlock`**

In `internal/anvilmcp/runtime_router.go`, replace `CreateFlock` with this implementation:

```go
func (r *RuntimeRouter) CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	startedAt := time.Now()
	latencies := make(map[string]time.Duration)
	requestedActiveVMs := int64(len(req.Roles))
	if requestedActiveVMs == 0 {
		requestedActiveVMs = 1
	}
	scheduleReq := ScheduleRequest{
		TenantID:           req.TenantID,
		RequestedActiveVMs: requestedActiveVMs,
		EgressPolicy:       EgressPolicy(req.EgressPolicy),
	}

	scheduleStarted := time.Now()
	decision, daemon, err := r.scheduleDaemon(scheduleReq, TenantUsage{ActiveVMs: requestedActiveVMs})
	latencies[FlockPlacementPhaseSchedule] = time.Since(scheduleStarted)
	if err != nil {
		latencies[FlockPlacementPhaseTotal] = time.Since(startedAt)
		r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
			Outcome:   flockPlacementOutcomeForScheduleError(err),
			Reason:    flockPlacementReasonForScheduleError(err),
			At:        time.Now().UTC(),
			Latencies: latencies,
		})
		return nil, err
	}

	req.TenantID = decision.TenantID
	req.EgressPolicy = string(decision.EgressPolicy)
	daemonStarted := time.Now()
	resp, err := daemon.CreateFlock(ctx, req)
	latencies[FlockPlacementPhaseDaemonCreate] = time.Since(daemonStarted)
	if err != nil {
		latencies[FlockPlacementPhaseTotal] = time.Since(startedAt)
		r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
			Outcome:   FlockPlacementOutcomeDaemonError,
			Reason:    FlockPlacementReasonDaemonCreateFailed,
			At:        time.Now().UTC(),
			Latencies: latencies,
		})
		return nil, err
	}
	if resp == nil {
		latencies[FlockPlacementPhaseTotal] = time.Since(startedAt)
		r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
			Outcome:   FlockPlacementOutcomeDaemonNilResponse,
			Reason:    FlockPlacementReasonDaemonNilResponse,
			At:        time.Now().UTC(),
			Latencies: latencies,
		})
		return nil, fmt.Errorf("runtime daemon CreateFlock returned nil response")
	}

	placementStarted := time.Now()
	err = r.recordFlockPlacements(resp, decision.Host.Name)
	latencies[FlockPlacementPhasePlacementSave] = time.Since(placementStarted)
	latencies[FlockPlacementPhaseTotal] = time.Since(startedAt)
	if err != nil {
		r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
			Outcome:   FlockPlacementOutcomePlacementSaveError,
			Reason:    FlockPlacementReasonPlacementSaveFailed,
			At:        time.Now().UTC(),
			Latencies: latencies,
		})
		flockID := strings.TrimSpace(resp.FlockID)
		return nil, fmt.Errorf("flock created but placement save failed: flock_id=%q: %w", flockID, err)
	}

	r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
		Outcome:   FlockPlacementOutcomeSuccess,
		Reason:    FlockPlacementReasonScheduled,
		At:        time.Now().UTC(),
		Latencies: latencies,
	})
	return resp, nil
}
```

Add `time` to the import block and add these helpers to `runtime_router.go`:

```go
func (r *RuntimeRouter) recordFlockPlacementMetric(obs FlockPlacementMetricObservation) {
	if r == nil || r.placementStore == nil {
		return
	}
	_ = r.placementStore.RecordFlockPlacementMetrics(obs)
}

func flockPlacementOutcomeForScheduleError(err error) string {
	var denied *ScheduleDeniedError
	if errors.As(err, &denied) {
		return FlockPlacementOutcomeDenied
	}
	return FlockPlacementOutcomeSchedulerError
}

func flockPlacementReasonForScheduleError(err error) string {
	var denied *ScheduleDeniedError
	if errors.As(err, &denied) {
		return normalizeScheduleDecisionReason(denied.Decision.Reason)
	}
	return FlockPlacementReasonInvalidRequest
}

func normalizeScheduleDecisionReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case FlockPlacementReasonQuotaExceeded:
		return FlockPlacementReasonQuotaExceeded
	case FlockPlacementReasonNoEligibleHost:
		return FlockPlacementReasonNoEligibleHost
	default:
		return FlockPlacementReasonUnknown
	}
}
```

Add `errors` to `runtime_router.go` imports if it is not already present.

- [ ] **Step 4: Run router metrics tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterCreateFlockRecords.*Metrics|TestRuntimeRouterCreateFlockSchedulesByRoleCountAndRecordsPlacements|TestRuntimeRouterCreateFlockRejectsQuotaBeforeDaemonCall|TestRuntimeRouterCreateFlockReportsPlacementSaveFailureWithoutSecrets' -count=1
```

Expected: PASS.

- [ ] **Step 5: Run full anvilmcp package tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit router instrumentation**

```bash
git add internal/anvilmcp/runtime_router.go internal/anvilmcp/runtime_router_test.go
git commit -m "feat: record flock placement metrics"
```

---

### Task 4: Documentation and Verification

**Files:**
- Modify: `RELEASE_NOTES.md`
- Modify: `docs/operations/observability.md`
- Create: `docs/operations/2026-06-04-flock-placement-metrics-handoff.md`

- [ ] **Step 1: Update release notes**

At the top of `RELEASE_NOTES.md`, add:

```markdown
# Unreleased — Scheduler flock placement metrics

## 추가됨

- scheduler `/metrics`에 `anvil_scheduler_flock_placement_*` aggregate metrics를
  추가한다.
  - `anvil_scheduler_flock_placement_attempts_total`
  - `anvil_scheduler_flock_placement_latency_seconds`
  - `anvil_scheduler_flock_placement_last_success_timestamp_seconds`
  - `anvil_scheduler_flock_placement_last_failure_timestamp_seconds`
- `RuntimeRouter.CreateFlock` scheduler-aware path는 schedule, daemon create,
  placement save, total latency를 bounded phase histogram으로 기록한다.

## 보안/운영 강화

- flock placement metrics label은 `outcome`, `reason`, `phase` bounded enum만 사용한다.
- `tenant_id`, `flock_id`, `vm_id`, host endpoint, authorization header, `agent_token`,
  daemon raw body는 metrics state와 exposition에 저장하지 않는다.

## 검증 예정

- `go test ./internal/anvilmcp -count=1`
- `go test ./cmd/anvil-scheduler -count=1`
- `go test ./cmd/anvil-mcp -count=1`
- `go test ./... -count=1`
- `go build ./cmd/anvil-scheduler`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/goose-daemon`

```

- [ ] **Step 2: Update observability docs**

In `docs/operations/observability.md`, add a section:

```markdown
## Scheduler flock placement metrics

Scheduler `/metrics` also exposes aggregate metrics for the MCP router's
scheduler-aware `anvil_spawn_flock` path.

- `anvil_scheduler_flock_placement_attempts_total{outcome,reason}` counts bounded
  placement outcomes.
- `anvil_scheduler_flock_placement_latency_seconds{phase}` records schedule,
  daemon create, placement save, and total latency as persisted histogram aggregates.
- `anvil_scheduler_flock_placement_last_success_timestamp_seconds` records the last
  fully successful scheduler-aware flock placement.
- `anvil_scheduler_flock_placement_last_failure_timestamp_seconds` records the last
  denied or failed scheduler-aware flock placement.

The labels are intentionally bounded. Do not add `tenant_id`, `flock_id`, `vm_id`,
host endpoint, daemon raw error text, or token-bearing values as labels.
```

- [ ] **Step 3: Add handoff document**

Create `docs/operations/2026-06-04-flock-placement-metrics-handoff.md`:

```markdown
# Scheduler flock placement metrics 운영 인계

작성일: 2026-06-04

## 릴리즈 범위

- scheduler-aware `RuntimeRouter.CreateFlock` path에 flock placement aggregate metrics를
  추가했다.
- scheduler `/metrics`에 `anvil_scheduler_flock_placement_*` metrics를 노출한다.
- latency histogram은 `schedule`, `daemon_create`, `placement_save`, `total` phase를
  기록한다.

## 보안 경계

- metrics labels는 bounded enum인 `outcome`, `reason`, `phase`만 사용한다.
- `tenant_id`, `flock_id`, `vm_id`, host endpoint, daemon raw body, authorization
  header, `agent_token`은 metrics state와 output에 저장하지 않는다.

## 검증

- `go test ./internal/anvilmcp -count=1`
- `go test ./cmd/anvil-scheduler -count=1`
- `go test ./cmd/anvil-mcp -count=1`
- `go test ./... -count=1`
- `go build ./cmd/anvil-scheduler`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/goose-daemon`
- `git diff --check`

## 잔여 위험

- metrics aggregate는 `PlacementStoreState` JSON file에 저장된다. 기존 placement store의
  multi-process write 모델은 이번 작업에서 바꾸지 않았다.
- host별 placement 편향은 아직 직접 노출하지 않는다.
- true cross-host flock placement는 별도 설계 범위다.
```

- [ ] **Step 4: Run documentation diff check**

Run:

```bash
git diff --check
```

Expected: PASS.

- [ ] **Step 5: Run focused tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-scheduler -count=1
go test ./cmd/anvil-mcp -count=1
```

Expected: PASS.

- [ ] **Step 6: Run broad verification**

Run:

```bash
go test ./... -count=1
go build ./cmd/anvil-scheduler
go build ./cmd/anvil-mcp
go build ./cmd/goose-daemon
git diff --check
```

Expected: PASS.

- [ ] **Step 7: Commit docs and verification record**

```bash
git add RELEASE_NOTES.md docs/operations/observability.md docs/operations/2026-06-04-flock-placement-metrics-handoff.md
git commit -m "docs: document flock placement metrics"
```

---

## Self-Review

- Spec coverage: Tasks cover persisted aggregate state, renderer output, router instrumentation, bounded labels, histogram buckets, timestamp gauges, tests, release notes, observability docs, and handoff.
- Scope check: The plan does not add host labels, tenant/flock/VM labels, a scheduler service event API, daemon direct `POST /flocks` instrumentation, Prometheus dependency, or cross-host flock placement.
- Type consistency: `FlockPlacementMetricsState`, `LatencyHistogramState`, `FlockPlacementMetricObservation`, constants, and helper names are introduced in Task 1 before being referenced by Tasks 2 and 3.
- Verification: Focused package tests run before broad `go test ./...` and builds.
