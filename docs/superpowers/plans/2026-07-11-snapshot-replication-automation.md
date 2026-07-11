# Cross-host Snapshot Replication Automation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** the adapter reconcile loop keeps every observed snapshot at a fixed replica factor **N=2** declaratively — each pass it discovers snapshots on reachable hosts, computes the desired↔actual replica drift, selects an eligible target (reusing `SelectRuntimeHost`), fires `ReplicateSnapshot` once, and heals under-replicated snapshots on later passes. Dial-class target failures are counted with a bounded in-memory counter (cap 3, giving-up, reset on target revival); reachable-but-refused transfers (D3 coarse-fs / tenant / validation) are terminal (no dial count, target excluded). State (attempts / latency / queue_depth / giving_up / last_success/failure) is persisted in `PlacementStore` and exposed on scheduler `/metrics`. anvil's alerting boundary is metric exposure + runbook alert expressions — no in-adapter alerting.

**Architecture:** design 확정본 [`docs/superpowers/specs/2026-07-11-snapshot-replication-automation-design.md`](../specs/2026-07-11-snapshot-replication-automation-design.md) (the 결정 기록 절 is the final contract). Everything happens inside the existing adapter (`internal/anvilmcp`) reconcile loop — no new process, no async job queue (async relay buffer is a rejected precedent, do not re-litigate). This slice mirrors the home-failover slice that already shipped on this branch: `ReconcilePlacements` → `probes map[string]hostProbe` (already present), a new `reconcileSnapshotReplication(ctx, probes)` sibling to `reconcileRoutedFlockWalls`, an in-memory per-`(snapshot,target)` failure counter guarded by `reconcileMu` (exactly like `homeFailures`), `isDialError`/probe reachability reuse for dial gating, and a `PlacementStore`-persisted metric family mirroring `flock_placement_metrics.go`.

**Tech Stack:** Go (existing deps only, no new dependency), bash KVM e2e.

## Global Constraints

- Design contract 원문: `docs/superpowers/specs/2026-07-11-snapshot-replication-automation-design.md`. Do not violate the 비목표: no lossless/synchronous-ack durability, no cross-region, no replica GC / topology rebalance, no unbounded async job queue, no policy configuration (replica factor / retry cap stay constants), no in-adapter alerting, no cross-tenant replication.
- **`snapshotReplicaFactor = 2`** (원본 + 복제 1) and **`snapshotReplicationFailureCap = 3`** — **constants, never configuration** (YAGNI, the same stance as `homeFailureThreshold`, `ADR_INDEX.md:48`). Do not add env vars or config fields.
- **Dial-only counting + revival reset:** only a target host observed **unreachable by the reconcile probe** (`hostProbe.reachable == false`, dial-class) advances the bounded `(snapshot,target)` counter; the counter (and giving-up mark) resets the moment the reconcile probe observes that target reachable again (mirror of failover's saturated-counter-fires-on-revival). HTTP responses / reset / EOF / ctx-cancel never count toward the dial cap. Reuse `isDialError` (`home_failover.go:17`) for any raw-error dial checks.
- **Terminal classification (D3 coarse / tenant / validation):** a target that answered the probe but returned a non-success transfer status is **terminal** — recorded with a terminal reason metric, excluded from re-selection in-memory (reset only on restart), and **never** counted toward the dial cap or retried against the same target. The adapter has no means to observe remote fs granularity (spec decision #5), so D3 coarse-fs rejection is accepted as a terminal-fail subsumed under the `terminal_rejected` reason (no per-target pre-exclusion).
- **Token / address redaction, low-cardinality metric labels:** metric labels are fixed low-cardinality identifiers only (`outcome`, `reason`, `phase`) — never a host address, token, snapshot secret, or snapshot id. Reconcile logs/errors carry snapshot id + host **name** only (name is a low-card identifier; the daemon **Endpoint** address and any token are forbidden in every log/error/serialization surface). Reuse `safeReplicationError`/`sanitizeSnapshotReplicationError` for any error strings surfaced through the tool path.
- **Tenant carry:** replication preserves the snapshot's owning tenant/egress. The sweep reads the snapshot's `SnapshotInfo.TenantID`/`EgressPolicy` and carries them into `SelectRuntimeHost` (tenant/egress-blind selection is forbidden). No replication to a different tenant.
- **`reconcileMu` discipline:** the sweep runs inside `ReconcilePlacements` (already `reconcileMu`-serialized end-to-end). All in-memory replication counters/maps are touched **only** inside the sweep, so they need no extra lock — annotate the fields "guarded by reconcileMu", exactly like `homeFailures`. `ReplicateSnapshot` itself takes only `PlacementStore` locks (never `reconcileMu`/`r.mu`), so calling it from the sweep is deadlock-free and stays safe against the concurrent manual `anvil_replicate_snapshot` path.
- **Commits: git trailer 금지** (anvil branch convention — no `Co-Authored-By`). Commit in small units.
- **Verification: `go test -race` in every task** (this slice touches the reconcile path that `-race`-verified the failover/hub-token work).
- **main 직접 push 금지** — branch `feature/snapshot-replication-automation` (already the worktree branch) + PR path. **자체 머지 금지** (merge only on user approval). Every worker Bash starts `cd /data/projects/claude-zone/anvil-wt-repl && …`; confirm `git branch --show-current` before committing (if `main` → BLOCKED).

## Baseline facts confirmed by code inspection (reviewer note)

- `RuntimeRouter.CreateSnapshot` (`runtime_router.go:251-265`) already seeds `SnapshotLocations` with the source host on create, and `ReplicateSnapshot` (`snapshot_replication.go:165`) records the target on success. So `SnapshotLocations` already tracks router-managed snapshots; the sweep additionally **discovers** actual locations from reachable daemons' `ListSnapshots` each pass (add-only union — replica GC is a non-goal) to (a) confirm a reachable source exists and (b) read the snapshot's tenant/egress for target selection.
- `ReplicateSnapshot` erases the transport error class (list failures return `errors.New(safeReplicationError(...))`, a plain string; export/import failures return `(resp, nil)` with `resp.Status="failed"/"partial"`). Therefore the **probe** (`ReconcilePlacements`'s `ListVMs` reachability, already collected into `probes`) is the only clean dial signal — the sweep gates dial-counting on `probes[target].reachable`, exactly as failover gates on `probes[homeHost].dialFailed`. `resp.Status != "replicated"` on a reachable target ⇒ terminal.
- The scheduler `/metrics` endpoint already renders the **full** `PlacementStore.State()` via `RenderSchedulerMetrics(s.placements.State())` (`scheduler_service.go:76`), and `State()` includes every cloned field except the two redacted token maps. So exposing the new metric family needs only a new render helper wired into `RenderSchedulerMetrics` — **no change to `scheduler_service.go`**.
- Generic `Save()` preserves on-disk metric families (`mergePersistedPlacementStoreFields(..., preserveMetrics=true)`); only `Record*` methods write in-memory metrics via `saveLockedRaw` (`preserveMetrics=false`). Adding a second metric family therefore requires (a) preserving it under `preserveMetrics`, and (b) refreshing BOTH families from disk inside each `Record*` method before `saveLockedRaw`, or a `Record` of one family would clobber a concurrent process's counters of the other (a regression of the current single-family guarantee). This plan factors that refresh into one helper used by both families.

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/anvilmcp/snapshot_replication_metrics.go` (신규) | metric state type + observation + outcome/reason/phase vocab + normalize/clone/key helpers (mirror `flock_placement_metrics.go`) |
| `internal/anvilmcp/snapshot_replication_metrics_test.go` (신규) | record/persist/gauge/redaction unit tests (mirror `flock_placement_metrics_test.go`) |
| `internal/anvilmcp/placement_store.go` (수정) | `PlacementStoreState.SnapshotReplicationMetrics` field + New/normalize/clone/merge; `refreshPersistedMetricsLocked` helper; `RecordFlockPlacementMetrics` uses it; new `RecordSnapshotReplicationMetrics` + `RecordSnapshotReplicationGauges` |
| `internal/anvilmcp/snapshot_replication.go` (수정) | `snapshotReplicaFactor`/`snapshotReplicationFailureCap` consts; `reconcileSnapshotReplication` sweep + `snapshotTargetKey` helpers + `classifySnapshotReplication` + `recordSnapshotReplicationMetric` |
| `internal/anvilmcp/runtime_router.go` (수정) | RuntimeRouter fields (`replicationDialFailures`, `replicationGivingUp`, `replicationTerminal`) + constructor init; wire `reconcileSnapshotReplication` into `ReconcilePlacements` |
| `internal/anvilmcp/snapshot_replication_sweep_test.go` (신규) | sweep unit tests (fake daemon): drift heal, dial cap/giving-up, revival reset, no-candidate, restart reset, eligibility, terminal, reconcile wiring + redaction |
| `internal/anvilmcp/scheduler_metrics.go` (수정) | `renderSnapshotReplicationMetrics` + call from `RenderSchedulerMetrics` |
| `internal/anvilmcp/scheduler_metrics_test.go` (수정) | render + `/metrics` endpoint tests for the new family |
| `scripts/anvil-snapshot-replication-e2e.sh` (신규) | KVM e2e (real source daemon + recorder stub targets + adapter members_only sweep) |
| `CONTEXT.md`, `docs/operations/runbook.md`, `docs/ADR_INDEX.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`, `docs/operations/2026-06-02-cross-host-snapshot-replication-handoff.md`, `docs/operations/2026-07-11-snapshot-replication-automation-handoff.md` (수정/신규) | backlog→done, runbook alert expressions + sweep behavior, ADR row, boundary review, prior handoff closure, new slice handoff |

**Interfaces 계약 전체 요약** (태스크 간 공유 — 각 태스크의 Interfaces 블록이 원문):

```go
// internal/anvilmcp/snapshot_replication_metrics.go (Task 1)
const (
	SnapshotReplicationOutcomeReplicated       = "replicated"
	SnapshotReplicationOutcomeAlreadyPresent   = "already_present"
	SnapshotReplicationOutcomeDialFailed       = "dial_failed"
	SnapshotReplicationOutcomeTerminalRejected = "terminal_rejected"
	SnapshotReplicationOutcomeError            = "error"
	SnapshotReplicationOutcomeNoCandidate      = "no_candidate"

	SnapshotReplicationReasonScheduled         = "scheduled"
	SnapshotReplicationReasonIdempotent        = "idempotent"
	SnapshotReplicationReasonTargetUnreachable = "target_unreachable"
	SnapshotReplicationReasonRejected          = "rejected"
	SnapshotReplicationReasonTransferError     = "transfer_error"
	SnapshotReplicationReasonNoEligibleHost    = "no_eligible_host"

	SnapshotReplicationPhaseTotal = "total"
)
type SnapshotReplicationMetricObservation struct{ Outcome, Reason string; Latencies map[string]time.Duration; At time.Time }
type SnapshotReplicationMetricsState struct {
	AttemptsByOutcomeReason map[string]int64
	LatencyByPhase          map[string]LatencyHistogramState // shared type from flock_placement_metrics.go
	QueueDepth, GivingUp    int64
	LastSuccessAt, LastFailureAt time.Time
}
func snapshotReplicationAttemptKey(outcome, reason string) string

// internal/anvilmcp/placement_store.go (Task 1)
func (s *PlacementStore) RecordSnapshotReplicationMetrics(obs SnapshotReplicationMetricObservation) error
func (s *PlacementStore) RecordSnapshotReplicationGauges(queueDepth, givingUp int64) error

// internal/anvilmcp/snapshot_replication.go (Task 2, refined Task 3)
const snapshotReplicaFactor = 2
const snapshotReplicationFailureCap = 3
func (r *RuntimeRouter) reconcileSnapshotReplication(ctx context.Context, probes map[string]hostProbe) error
func snapshotTargetKey(snapshotID, target string) string
func splitSnapshotTargetKey(key string) (string, string)

// internal/anvilmcp/runtime_router.go (Task 2/3): RuntimeRouter new fields (all reconcileMu-guarded)
//   replicationDialFailures map[string]int
//   replicationGivingUp     map[string]bool
//   replicationTerminal     map[string]bool   // added Task 3
```

---

### Task 1: PlacementStore snapshot-replication metric family (record + persist)

**Files:**
- Create: `internal/anvilmcp/snapshot_replication_metrics.go`
- Modify: `internal/anvilmcp/placement_store.go` (state field + New/normalize/clone/merge; `refreshPersistedMetricsLocked`; `RecordFlockPlacementMetrics`; new record/gauge methods)
- Test: `internal/anvilmcp/snapshot_replication_metrics_test.go`

**Interfaces:**
- Consumes: shared `LatencyHistogramState`, `recordLatencyHistogramObservation`, `flockPlacementLatencyBuckets`, `formatFlockPlacementBucket`, `isAllowedFlockPlacementBucket` (`flock_placement_metrics.go`); `saveLockedRaw`, `readPlacementStoreState`, `clonePlacementStoreState` (`placement_store.go`).
- Produces (Task 2/4 consume): `SnapshotReplicationMetricsState`, `SnapshotReplicationMetricObservation`, the outcome/reason/phase constants, `snapshotReplicationAttemptKey`/`splitSnapshotReplicationAttemptKey`/`sortedSnapshotReplicationAttemptKeys`, `newSnapshotReplicationMetricsState`/`normalizeSnapshotReplicationMetricsState`/`cloneSnapshotReplicationMetricsState`, `(*PlacementStore).RecordSnapshotReplicationMetrics`, `(*PlacementStore).RecordSnapshotReplicationGauges`.

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/anvilmcp/snapshot_replication_metrics_test.go` 신규:

```go
package anvilmcp

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPlacementStoreRecordsSnapshotReplicationMetrics(t *testing.T) {
	store := NewPlacementStore("")
	at := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)

	if err := store.RecordSnapshotReplicationMetrics(SnapshotReplicationMetricObservation{
		Outcome: SnapshotReplicationOutcomeReplicated,
		Reason:  SnapshotReplicationReasonScheduled,
		At:      at,
		Latencies: map[string]time.Duration{
			SnapshotReplicationPhaseTotal: 140 * time.Millisecond,
		},
	}); err != nil {
		t.Fatalf("RecordSnapshotReplicationMetrics() error = %v", err)
	}

	state := store.State().SnapshotReplicationMetrics
	key := snapshotReplicationAttemptKey(SnapshotReplicationOutcomeReplicated, SnapshotReplicationReasonScheduled)
	if got := state.AttemptsByOutcomeReason[key]; got != 1 {
		t.Fatalf("attempt count = %d, want 1", got)
	}
	if !state.LastSuccessAt.Equal(at) {
		t.Fatalf("LastSuccessAt = %v, want %v", state.LastSuccessAt, at)
	}
	if !state.LastFailureAt.IsZero() {
		t.Fatalf("LastFailureAt = %v, want zero", state.LastFailureAt)
	}
	total := state.LatencyByPhase[SnapshotReplicationPhaseTotal]
	if total.Count != 1 || total.SumSeconds <= 0 || total.Buckets["+Inf"] != 1 {
		t.Fatalf("total histogram = %+v, want one positive observation", total)
	}
}

func TestPlacementStoreRecordsSnapshotReplicationDialFailureAsFailureTimestamp(t *testing.T) {
	store := NewPlacementStore("")
	at := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
	if err := store.RecordSnapshotReplicationMetrics(SnapshotReplicationMetricObservation{
		Outcome: SnapshotReplicationOutcomeDialFailed,
		Reason:  SnapshotReplicationReasonTargetUnreachable,
		At:      at,
	}); err != nil {
		t.Fatalf("RecordSnapshotReplicationMetrics() error = %v", err)
	}
	state := store.State().SnapshotReplicationMetrics
	if got := state.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeDialFailed, SnapshotReplicationReasonTargetUnreachable)]; got != 1 {
		t.Fatalf("dial_failed attempt = %d, want 1", got)
	}
	if !state.LastFailureAt.Equal(at) {
		t.Fatalf("LastFailureAt = %v, want %v", state.LastFailureAt, at)
	}
	if !state.LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt = %v, want zero", state.LastSuccessAt)
	}
}

func TestPlacementStoreRecordsSnapshotReplicationGauges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placements.json")
	store := NewPlacementStore(path)
	if err := store.RecordSnapshotReplicationGauges(4, 1); err != nil {
		t.Fatalf("RecordSnapshotReplicationGauges() error = %v", err)
	}
	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	state := reloaded.State().SnapshotReplicationMetrics
	if state.QueueDepth != 4 || state.GivingUp != 1 {
		t.Fatalf("gauges = queue_depth %d giving_up %d, want 4/1", state.QueueDepth, state.GivingUp)
	}
}

func TestPlacementStoreSanitizesSnapshotReplicationMetricLabels(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.RecordSnapshotReplicationMetrics(SnapshotReplicationMetricObservation{
		Outcome: "tenant-1",
		Reason:  "http://host-a:3000/agent_token",
		At:      time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC),
		Latencies: map[string]time.Duration{
			"snap-1": 15 * time.Millisecond, // invalid phase → dropped
		},
	}); err != nil {
		t.Fatalf("RecordSnapshotReplicationMetrics() error = %v", err)
	}
	state := store.State().SnapshotReplicationMetrics
	key := snapshotReplicationAttemptKey(SnapshotReplicationOutcomeError, SnapshotReplicationReasonTransferError)
	if got := state.AttemptsByOutcomeReason[key]; got != 1 {
		t.Fatalf("sanitized attempt count = %d, want 1 (unknown outcome/reason bucketed to error/transfer_error)", got)
	}
	if len(state.LatencyByPhase) != 0 {
		t.Fatalf("LatencyByPhase = %+v, want invalid phase ignored", state.LatencyByPhase)
	}
}

func TestPlacementStoreSavePreservesSnapshotReplicationMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler", "placements.json")
	schedulerStore := NewPlacementStore(path)
	if err := schedulerStore.SetHostAndSave(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHostAndSave initial host: %v", err)
	}
	mcpStore := NewPlacementStore(path)
	if err := mcpStore.Load(); err != nil {
		t.Fatalf("mcp Load: %v", err)
	}
	if err := mcpStore.RecordSnapshotReplicationMetrics(SnapshotReplicationMetricObservation{
		Outcome: SnapshotReplicationOutcomeReplicated,
		Reason:  SnapshotReplicationReasonScheduled,
		At:      time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordSnapshotReplicationMetrics: %v", err)
	}
	// A stale generic save from the scheduler instance must not wipe the metric.
	if err := schedulerStore.SetHostAndSave(RuntimeHost{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 2}); err != nil {
		t.Fatalf("stale SetHostAndSave: %v", err)
	}
	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	key := snapshotReplicationAttemptKey(SnapshotReplicationOutcomeReplicated, SnapshotReplicationReasonScheduled)
	if got := reloaded.State().SnapshotReplicationMetrics.AttemptsByOutcomeReason[key]; got != 1 {
		t.Fatalf("attempt count after stale generic save = %d, want 1 (preserved)", got)
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/anvilmcp/ -run TestPlacementStore.*SnapshotReplication -v
```
Expected: FAIL — compile errors (`SnapshotReplicationMetricsState`, `RecordSnapshotReplicationMetrics`, constants undefined).

- [ ] **Step 3: 구현**

(a) `internal/anvilmcp/snapshot_replication_metrics.go` 신규:

```go
package anvilmcp

import (
	"sort"
	"strings"
	"time"
)

const (
	SnapshotReplicationOutcomeReplicated       = "replicated"
	SnapshotReplicationOutcomeAlreadyPresent   = "already_present"
	SnapshotReplicationOutcomeDialFailed       = "dial_failed"
	SnapshotReplicationOutcomeTerminalRejected = "terminal_rejected"
	SnapshotReplicationOutcomeError            = "error"
	SnapshotReplicationOutcomeNoCandidate      = "no_candidate"

	SnapshotReplicationReasonScheduled         = "scheduled"
	SnapshotReplicationReasonIdempotent        = "idempotent"
	SnapshotReplicationReasonTargetUnreachable = "target_unreachable"
	SnapshotReplicationReasonRejected          = "rejected"
	SnapshotReplicationReasonTransferError     = "transfer_error"
	SnapshotReplicationReasonNoEligibleHost    = "no_eligible_host"

	// The adapter reuses ReplicateSnapshot as one atomic transfer, so only the
	// end-to-end "total" latency is observable here (export/import sub-timings
	// live inside the daemon stream and are not surfaced by ReplicateSnapshot).
	SnapshotReplicationPhaseTotal = "total"
)

type SnapshotReplicationMetricObservation struct {
	Outcome   string
	Reason    string
	Latencies map[string]time.Duration
	At        time.Time
}

// SnapshotReplicationMetricsState mirrors FlockPlacementMetricsState (same
// counter/histogram/timestamp shape) plus two point-in-time gauges the reconcile
// sweep republishes every pass: QueueDepth (snapshots below the replica factor)
// and GivingUp (snapshots with a dial-saturated target). It reuses the shared
// LatencyHistogramState from flock_placement_metrics.go.
type SnapshotReplicationMetricsState struct {
	AttemptsByOutcomeReason map[string]int64                 `json:"attempts_by_outcome_reason,omitempty"`
	LatencyByPhase          map[string]LatencyHistogramState `json:"latency_by_phase,omitempty"`
	QueueDepth              int64                            `json:"queue_depth,omitempty"`
	GivingUp                int64                            `json:"giving_up,omitempty"`
	LastSuccessAt           time.Time                        `json:"last_success_at,omitempty"`
	LastFailureAt           time.Time                        `json:"last_failure_at,omitempty"`
}

func newSnapshotReplicationMetricsState() SnapshotReplicationMetricsState {
	return SnapshotReplicationMetricsState{
		AttemptsByOutcomeReason: make(map[string]int64),
		LatencyByPhase:          make(map[string]LatencyHistogramState),
	}
}

func normalizeSnapshotReplicationMetricsState(state *SnapshotReplicationMetricsState) {
	if state.AttemptsByOutcomeReason == nil {
		state.AttemptsByOutcomeReason = make(map[string]int64)
	}
	if state.LatencyByPhase == nil {
		state.LatencyByPhase = make(map[string]LatencyHistogramState)
	}
	attempts := make(map[string]int64, len(state.AttemptsByOutcomeReason))
	for key, count := range state.AttemptsByOutcomeReason {
		outcome, reason := splitSnapshotReplicationAttemptKey(key)
		attempts[snapshotReplicationAttemptKey(outcome, reason)] += count
	}
	state.AttemptsByOutcomeReason = attempts

	latencies := make(map[string]LatencyHistogramState, len(state.LatencyByPhase))
	for phase, hist := range state.LatencyByPhase {
		phase = normalizeSnapshotReplicationPhase(phase)
		if phase == "" {
			continue
		}
		existing := latencies[phase]
		if existing.Buckets == nil {
			existing.Buckets = make(map[string]int64)
		}
		for bucket, count := range hist.Buckets {
			if !isAllowedFlockPlacementBucket(bucket) {
				continue
			}
			existing.Buckets[bucket] += count
		}
		existing.SumSeconds += hist.SumSeconds
		existing.Count += hist.Count
		latencies[phase] = existing
	}
	state.LatencyByPhase = latencies
}

func cloneSnapshotReplicationMetricsState(state SnapshotReplicationMetricsState) SnapshotReplicationMetricsState {
	out := newSnapshotReplicationMetricsState()
	for key, count := range state.AttemptsByOutcomeReason {
		outcome, reason := splitSnapshotReplicationAttemptKey(key)
		out.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(outcome, reason)] += count
	}
	for phase, hist := range state.LatencyByPhase {
		phase = normalizeSnapshotReplicationPhase(phase)
		if phase == "" {
			continue
		}
		existing := out.LatencyByPhase[phase]
		if existing.Buckets == nil {
			existing.Buckets = make(map[string]int64)
		}
		for bucket, count := range hist.Buckets {
			if !isAllowedFlockPlacementBucket(bucket) {
				continue
			}
			existing.Buckets[bucket] += count
		}
		existing.SumSeconds += hist.SumSeconds
		existing.Count += hist.Count
		out.LatencyByPhase[phase] = existing
	}
	out.QueueDepth = state.QueueDepth
	out.GivingUp = state.GivingUp
	out.LastSuccessAt = state.LastSuccessAt
	out.LastFailureAt = state.LastFailureAt
	return out
}

func snapshotReplicationAttemptKey(outcome, reason string) string {
	return normalizeSnapshotReplicationOutcome(outcome) + "|" + normalizeSnapshotReplicationReason(reason)
}

func splitSnapshotReplicationAttemptKey(key string) (string, string) {
	parts := strings.SplitN(key, "|", 2)
	if len(parts) != 2 {
		return SnapshotReplicationOutcomeError, SnapshotReplicationReasonTransferError
	}
	return normalizeSnapshotReplicationOutcome(parts[0]), normalizeSnapshotReplicationReason(parts[1])
}

func normalizeSnapshotReplicationOutcome(value string) string {
	switch strings.TrimSpace(value) {
	case SnapshotReplicationOutcomeReplicated,
		SnapshotReplicationOutcomeAlreadyPresent,
		SnapshotReplicationOutcomeDialFailed,
		SnapshotReplicationOutcomeTerminalRejected,
		SnapshotReplicationOutcomeError,
		SnapshotReplicationOutcomeNoCandidate:
		return strings.TrimSpace(value)
	default:
		return SnapshotReplicationOutcomeError
	}
}

func normalizeSnapshotReplicationReason(value string) string {
	switch strings.TrimSpace(value) {
	case SnapshotReplicationReasonScheduled,
		SnapshotReplicationReasonIdempotent,
		SnapshotReplicationReasonTargetUnreachable,
		SnapshotReplicationReasonRejected,
		SnapshotReplicationReasonTransferError,
		SnapshotReplicationReasonNoEligibleHost:
		return strings.TrimSpace(value)
	default:
		return SnapshotReplicationReasonTransferError
	}
}

func isSnapshotReplicationSuccessOutcome(outcome string) bool {
	switch normalizeSnapshotReplicationOutcome(outcome) {
	case SnapshotReplicationOutcomeReplicated, SnapshotReplicationOutcomeAlreadyPresent:
		return true
	default:
		return false
	}
}

func normalizeSnapshotReplicationPhase(value string) string {
	if strings.TrimSpace(value) == SnapshotReplicationPhaseTotal {
		return SnapshotReplicationPhaseTotal
	}
	return ""
}

func sortedSnapshotReplicationAttemptKeys(attempts map[string]int64) []string {
	keys := make([]string, 0, len(attempts))
	for key := range attempts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
```

(b) `placement_store.go` — state field. `PlacementStoreState` (현재 `FlockPlacementMetrics` 필드 바로 아래):

```go
	FlockPlacementMetrics     FlockPlacementMetricsState     `json:"flock_placement_metrics,omitempty"`
	SnapshotReplicationMetrics SnapshotReplicationMetricsState `json:"snapshot_replication_metrics,omitempty"`
```

`NewPlacementStore` state literal에 `FlockPlacementMetrics: newFlockPlacementMetricsState(),` 아래:

```go
			SnapshotReplicationMetrics: newSnapshotReplicationMetricsState(),
```

`normalizePlacementStoreState` — `normalizeFlockPlacementMetricsState(&state.FlockPlacementMetrics)` 아래:

```go
	normalizeSnapshotReplicationMetricsState(&state.SnapshotReplicationMetrics)
```

`clonePlacementStoreState` — `out.FlockPlacementMetrics = cloneFlockPlacementMetricsState(state.FlockPlacementMetrics)` 아래:

```go
	out.SnapshotReplicationMetrics = cloneSnapshotReplicationMetricsState(state.SnapshotReplicationMetrics)
```

`mergePersistedPlacementStoreFields` — `if preserveMetrics {` 블록 안, `state.FlockPlacementMetrics = ...` 아래:

```go
		state.SnapshotReplicationMetrics = cloneSnapshotReplicationMetricsState(persisted.SnapshotReplicationMetrics)
```

(c) `placement_store.go` — refresh helper + reuse it in `RecordFlockPlacementMetrics`. 현재 `RecordFlockPlacementMetrics` 본문의 disk-refresh 블록:

```go
	if strings.TrimSpace(s.path) != "" {
		persisted, exists, err := readPlacementStoreState(s.path)
		if err != nil {
			return err
		}
		if exists {
			s.state.FlockPlacementMetrics = cloneFlockPlacementMetricsState(persisted.FlockPlacementMetrics)
		}
	}
```

를 아래로 교체하고, 새 helper를 추가한다:

```go
	if err := s.refreshPersistedMetricsLocked(); err != nil {
		return err
	}
```

```go
// refreshPersistedMetricsLocked reloads BOTH persisted metric families
// (flock-placement and snapshot-replication) from disk into s.state before a
// Record* method's saveLockedRaw, which writes both families from memory. Without
// this, recording one family would clobber a concurrent process's counters of the
// other. Caller must hold s.mu.
func (s *PlacementStore) refreshPersistedMetricsLocked() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	persisted, exists, err := readPlacementStoreState(s.path)
	if err != nil {
		return err
	}
	if exists {
		s.state.FlockPlacementMetrics = cloneFlockPlacementMetricsState(persisted.FlockPlacementMetrics)
		s.state.SnapshotReplicationMetrics = cloneSnapshotReplicationMetricsState(persisted.SnapshotReplicationMetrics)
	}
	return nil
}
```

(d) `placement_store.go` — new record + gauge methods (mirror `RecordFlockPlacementMetrics`):

```go
func (s *PlacementStore) RecordSnapshotReplicationMetrics(obs SnapshotReplicationMetricObservation) error {
	if obs.At.IsZero() {
		obs.At = time.Now().UTC()
	}
	outcome := normalizeSnapshotReplicationOutcome(obs.Outcome)
	reason := normalizeSnapshotReplicationReason(obs.Reason)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	if err := s.refreshPersistedMetricsLocked(); err != nil {
		return err
	}
	previous := clonePlacementStoreState(s.state)
	metrics := &s.state.SnapshotReplicationMetrics
	normalizeSnapshotReplicationMetricsState(metrics)
	metrics.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(outcome, reason)]++
	for phase, duration := range obs.Latencies {
		phase = normalizeSnapshotReplicationPhase(phase)
		if phase == "" || duration < 0 {
			continue
		}
		hist := metrics.LatencyByPhase[phase]
		recordLatencyHistogramObservation(&hist, duration)
		metrics.LatencyByPhase[phase] = hist
	}
	if isSnapshotReplicationSuccessOutcome(outcome) {
		metrics.LastSuccessAt = obs.At
	} else {
		metrics.LastFailureAt = obs.At
	}
	if err := s.saveLockedRaw(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

// RecordSnapshotReplicationGauges republishes the two point-in-time gauges the
// reconcile sweep computes each pass. queueDepth = snapshots below the replica
// factor; givingUp = snapshots with a dial-saturated target. Counters/timestamps
// are preserved (refreshed from disk first).
func (s *PlacementStore) RecordSnapshotReplicationGauges(queueDepth, givingUp int64) error {
	if queueDepth < 0 {
		queueDepth = 0
	}
	if givingUp < 0 {
		givingUp = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	if err := s.refreshPersistedMetricsLocked(); err != nil {
		return err
	}
	previous := clonePlacementStoreState(s.state)
	s.state.SnapshotReplicationMetrics.QueueDepth = queueDepth
	s.state.SnapshotReplicationMetrics.GivingUp = givingUp
	if err := s.saveLockedRaw(); err != nil {
		s.state = previous
		return err
	}
	return nil
}
```

- [ ] **Step 4: 통과 확인**

```bash
go test ./internal/anvilmcp/ -run 'TestPlacementStore.*SnapshotReplication' -v
go test -race ./internal/anvilmcp/ -run 'FlockPlacementMetrics|SnapshotReplication'
```
Expected: PASS — new tests + existing flock-placement metric tests (the `RecordFlockPlacementMetrics` refactor must not regress `TestPlacementStoreRecordsFlockPlacementMetrics`, `...StaleFlockPlacementMetricsRecorderPreservesPersistedMetricsBeforeIncrement`, `...SavePreservesExternallyPersistedFlockPlacementMetrics`).

- [ ] **Step 5: 패키지 빌드 + race**

```bash
go build ./... && go test -race ./internal/anvilmcp/
```
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/anvilmcp/snapshot_replication_metrics.go internal/anvilmcp/snapshot_replication_metrics_test.go internal/anvilmcp/placement_store.go
git commit -m "feat(anvilmcp): persist snapshot replication metric family in placement store"
```

---

### Task 2: reconcile sweep core — discover / drift / select / replicate / bounded dial counter

**Files:**
- Modify: `internal/anvilmcp/snapshot_replication.go` (consts + sweep + helpers + `classifySnapshotReplication` happy/dial paths + `recordSnapshotReplicationMetric`)
- Modify: `internal/anvilmcp/runtime_router.go` (RuntimeRouter fields `replicationDialFailures`/`replicationGivingUp` + constructor init)
- Test: `internal/anvilmcp/snapshot_replication_sweep_test.go`

**Interfaces:**
- Consumes: `hostProbe` (`runtime_router.go:284`), `isDialError` (`home_failover.go:17`, unused directly here — probe is the dial signal), `snapshotTransferDaemon` (`snapshot_replication.go:28`, has `ListSnapshots`/`ExportSnapshot`/`ImportSnapshot`), `SelectRuntimeHost` (`tenant_policy.go:155`), `r.scheduler.hosts`, `(*RuntimeRouter).ReplicateSnapshot`, `PlacementStore.SetSnapshotLocation`/`Save`/`SnapshotHosts`/`State`/`RecordSnapshotReplicationMetrics`/`RecordSnapshotReplicationGauges`, `(*RuntimeRouter).logf`.
- Produces (Task 3 wires + refines): `snapshotReplicaFactor`, `snapshotReplicationFailureCap`, `reconcileSnapshotReplication(ctx, probes) error`, `snapshotTargetKey`/`splitSnapshotTargetKey`, `classifySnapshotReplication`, `recordSnapshotReplicationMetric`, RuntimeRouter fields. NOTE: the terminal branch + `replicationTerminal` field + `ReconcilePlacements` wiring are Task 3 (this task ships the happy + dial paths; a reachable non-success transfer is collected as a generic `error` outcome and retried next pass). Tests call `reconcileSnapshotReplication` directly.

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/anvilmcp/snapshot_replication_sweep_test.go` 신규 (fake 필드는 실물 `routerFakeDaemon` 대조 완료: `snapshotList`, `listSnapshotErr`, `importCalls`, `importErrForBody`, `exportCalls`, `listVMResp`, `listVMErr`):

```go
package anvilmcp

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func replicationHosts(names ...string) []RuntimeHost {
	hosts := make([]RuntimeHost, 0, len(names))
	for _, n := range names {
		hosts = append(hosts, RuntimeHost{
			Name: n, Endpoint: "http://" + n + ".internal:8080",
			Healthy: true, AvailableVMs: 1,
			EgressPolicies: []EgressPolicy{EgressPolicyProfile},
		})
	}
	return hosts
}

func newReplicationRouter(t *testing.T, hosts []RuntimeHost, daemons map[string]*routerFakeDaemon) (*RuntimeRouter, *PlacementStore) {
	t.Helper()
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	dm := make(map[string]Daemon, len(daemons))
	for name, d := range daemons {
		dm[name] = d
	}
	router := NewRuntimeRouterWithOptions(NewScheduler(hosts, nil, nil), dm, RuntimeRouterOptions{PlacementStore: store})
	return router, store
}

func allReachable(names ...string) map[string]hostProbe {
	probes := make(map[string]hostProbe, len(names))
	for _, n := range names {
		probes[n] = hostProbe{reachable: true}
	}
	return probes
}

func snapInfo(id string) SnapshotInfo {
	return SnapshotInfo{SnapshotID: id, TenantID: "tenant-1", EgressPolicy: "profile", SnapshotType: "full"}
}

// TestSnapshotReplication_ReplicatesUnderReplicatedSnapshot is the core contract:
// a snapshot present on only one reachable host is replicated to one eligible
// target, its location is recorded, queue_depth reflects the pre-heal drift, and
// a replicated attempt is counted.
func TestSnapshotReplication_ReplicatesUnderReplicatedSnapshot(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})

	if err := router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB")); err != nil {
		t.Fatalf("sweep error: %v", err)
	}
	if len(hostB.importCalls) != 1 {
		t.Fatalf("target imports = %d, want 1", len(hostB.importCalls))
	}
	if hosts := store.SnapshotHosts("snap-1"); len(hosts) != 2 || !slices.Contains(hosts, "hostB") {
		t.Fatalf("SnapshotHosts(snap-1) = %v, want [hostA hostB]", hosts)
	}
	m := store.State().SnapshotReplicationMetrics
	if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeReplicated, SnapshotReplicationReasonScheduled)] != 1 {
		t.Fatalf("replicated attempt not recorded: %+v", m.AttemptsByOutcomeReason)
	}
	if m.QueueDepth != 1 {
		t.Fatalf("queue_depth gauge = %d, want 1", m.QueueDepth)
	}
}

// TestSnapshotReplication_GivesUpAfterDialFailureCap: a target that is Healthy in
// inventory but unreachable at probe time advances the dial counter each pass and
// hits giving-up exactly at the cap; K-1 passes never give up and never attempt an
// import toward the down target (probe short-circuit).
func TestSnapshotReplication_GivesUpAfterDialFailureCap(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	down := map[string]hostProbe{"hostA": {reachable: true}, "hostB": {dialFailed: true}}

	for i := 0; i < snapshotReplicationFailureCap-1; i++ {
		_ = router.reconcileSnapshotReplication(context.Background(), down)
	}
	if store.State().SnapshotReplicationMetrics.GivingUp != 0 {
		t.Fatalf("gave up before cap: giving_up=%d", store.State().SnapshotReplicationMetrics.GivingUp)
	}
	if len(hostB.importCalls) != 0 {
		t.Fatalf("import attempted toward an unreachable target: %d", len(hostB.importCalls))
	}
	_ = router.reconcileSnapshotReplication(context.Background(), down) // cap-th failure
	m := store.State().SnapshotReplicationMetrics
	if m.GivingUp != 1 {
		t.Fatalf("giving_up gauge = %d, want 1 after %d dial failures", m.GivingUp, snapshotReplicationFailureCap)
	}
	if got := m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeDialFailed, SnapshotReplicationReasonTargetUnreachable)]; got != int64(snapshotReplicationFailureCap) {
		t.Fatalf("dial_failed attempts = %d, want %d", got, snapshotReplicationFailureCap)
	}
}

// TestSnapshotReplication_DialCounterResetsWhenTargetRevives: once the target is
// observed reachable again the saturated counter resets and the snapshot is
// replicated (giving-up is not permanent — spec D-2 보강).
func TestSnapshotReplication_DialCounterResetsWhenTargetRevives(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	down := map[string]hostProbe{"hostA": {reachable: true}, "hostB": {dialFailed: true}}
	for i := 0; i < snapshotReplicationFailureCap; i++ {
		_ = router.reconcileSnapshotReplication(context.Background(), down)
	}
	if store.State().SnapshotReplicationMetrics.GivingUp != 1 {
		t.Fatal("precondition: sweep did not give up")
	}

	_ = router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB"))
	if len(hostB.importCalls) != 1 {
		t.Fatalf("revived target did not receive the retried replication: imports=%d", len(hostB.importCalls))
	}
	if store.State().SnapshotReplicationMetrics.GivingUp != 0 {
		t.Fatalf("giving_up gauge did not reset after revival: %d", store.State().SnapshotReplicationMetrics.GivingUp)
	}
}

// TestSnapshotReplication_NoCandidateSurfacesQueueDepth: a single-host cluster
// (no eligible target) is a no-op — surfaced via queue_depth + a no_candidate
// attempt, never an export.
func TestSnapshotReplication_NoCandidateSurfacesQueueDepth(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	router, store := newReplicationRouter(t, replicationHosts("hostA"),
		map[string]*routerFakeDaemon{"hostA": hostA})
	if err := router.reconcileSnapshotReplication(context.Background(), allReachable("hostA")); err != nil {
		t.Fatalf("no-candidate sweep should be a no-op, got error: %v", err)
	}
	m := store.State().SnapshotReplicationMetrics
	if m.QueueDepth != 1 {
		t.Fatalf("queue_depth = %d, want 1", m.QueueDepth)
	}
	if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeNoCandidate, SnapshotReplicationReasonNoEligibleHost)] != 1 {
		t.Fatalf("no_candidate not recorded: %+v", m.AttemptsByOutcomeReason)
	}
	if len(hostA.exportCalls) != 0 {
		t.Fatalf("export attempted with no candidate: %d", len(hostA.exportCalls))
	}
}

// TestSnapshotReplication_RestartResetsInMemoryCounters: a fresh router over the
// same store starts with empty in-memory counters, so a previously given-up
// target is retried once reachable (spec 경계: 재시작 후 수렴, in-memory 카운터만
// 리셋).
func TestSnapshotReplication_RestartResetsInMemoryCounters(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	hosts := replicationHosts("hostA", "hostB")
	router, store := newReplicationRouter(t, hosts,
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	down := map[string]hostProbe{"hostA": {reachable: true}, "hostB": {dialFailed: true}}
	for i := 0; i < snapshotReplicationFailureCap; i++ {
		_ = router.reconcileSnapshotReplication(context.Background(), down)
	}

	restarted := NewRuntimeRouterWithOptions(NewScheduler(hosts, nil, nil),
		map[string]Daemon{"hostA": hostA, "hostB": hostB},
		RuntimeRouterOptions{PlacementStore: store})
	_ = restarted.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB"))
	if len(hostB.importCalls) != 1 {
		t.Fatalf("restart did not re-attempt a previously given-up target: imports=%d", len(hostB.importCalls))
	}
}

// TestSnapshotReplication_RespectsHostEligibility: target selection reuses
// SelectRuntimeHost eligibility — a SmokeOnly host is skipped (not preferred), the
// eligible host receives the replica. Proves tenant/egress-aware selection.
func TestSnapshotReplication_RespectsHostEligibility(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{}
	hostC := &routerFakeDaemon{}
	hosts := replicationHosts("hostA", "hostB", "hostC")
	hosts[1].SmokeOnly = true // hostB ineligible unless preferred
	router, _ := newReplicationRouter(t, hosts,
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB, "hostC": hostC})
	_ = router.reconcileSnapshotReplication(context.Background(), allReachable("hostA", "hostB", "hostC"))
	if len(hostB.importCalls) != 0 {
		t.Fatalf("replicated to an ineligible SmokeOnly host: %d", len(hostB.importCalls))
	}
	if len(hostC.importCalls) != 1 {
		t.Fatalf("did not replicate to the eligible host: hostC imports=%d", len(hostC.importCalls))
	}
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/anvilmcp/ -run TestSnapshotReplication_ -v
```
Expected: FAIL — compile errors (`reconcileSnapshotReplication`, `snapshotReplicaFactor`, `snapshotReplicationFailureCap`, router fields undefined).

- [ ] **Step 3: 구현**

(a) `runtime_router.go` — RuntimeRouter 필드 (`reconcileLogf` 필드 아래):

```go
	// replicationDialFailures counts CONSECUTIVE dial-class replication failures
	// per "<snapshot>\x1f<target>" for the bounded giving-up cap. Guarded by
	// reconcileMu: only ever touched inside the reconcile sweep, which reconcileMu
	// serializes end-to-end.
	replicationDialFailures map[string]int
	// replicationGivingUp marks "<snapshot>\x1f<target>" pairs that hit the cap;
	// excluded from selection until the target host is observed reachable again.
	// reconcileMu.
	replicationGivingUp map[string]bool
```

`NewRuntimeRouterWithOptions` 리터럴 (`homeFailures: make(map[string]int),` 아래):

```go
		replicationDialFailures: make(map[string]int),
		replicationGivingUp:     make(map[string]bool),
```

(b) `snapshot_replication.go` — consts (파일 상단, import 아래):

```go
// snapshotReplicaFactor is the desired number of replicas per snapshot (원본 +
// 복제 1). Constant, not configuration (YAGNI — same stance as homeFailureThreshold).
const snapshotReplicaFactor = 2

// snapshotReplicationFailureCap is the number of CONSECUTIVE dial-class target
// failures before the sweep gives up on that (snapshot,target) pair. Constant.
const snapshotReplicationFailureCap = 3

func snapshotTargetKey(snapshotID, target string) string {
	return strings.TrimSpace(snapshotID) + "\x1f" + strings.TrimSpace(target)
}

func splitSnapshotTargetKey(key string) (string, string) {
	parts := strings.SplitN(key, "\x1f", 2)
	if len(parts) != 2 {
		return key, ""
	}
	return parts[0], parts[1]
}
```

`snapshot_replication.go` import에 `"sort"` 및 `"time"` 추가.

(c) `snapshot_replication.go` — the sweep + classify + record wrapper:

```go
// reconcileSnapshotReplication converges every observed snapshot toward
// snapshotReplicaFactor replicas. It is a sibling of reconcileRoutedFlockWalls and
// runs inside ReconcilePlacements (reconcileMu-serialized), so its in-memory
// counters need no extra lock. Each pass:
//  1. Discovers actual replica locations + snapshot tenant/egress from every
//     probe-reachable daemon's ListSnapshots (add-only union into SnapshotLocations
//     — replica GC is a non-goal).
//  2. Resets the dial counter + giving-up mark for any target observed reachable
//     again (spec D-2 보강: giving-up is not permanent).
//  3. For each snapshot below the replica factor with a reachable source: selects
//     an eligible target (SelectRuntimeHost, tenant/egress carried), and either
//     counts a dial failure (target unreachable at probe — short-circuit, no doomed
//     transfer) or fires one ReplicateSnapshot and classifies the outcome.
//  4. Republishes the queue_depth / giving_up gauges.
//  5. Sweeps in-memory counters for snapshots no longer observed (deleted).
// All log/error text carries snapshot id + host NAME only (never a daemon address
// or token).
func (r *RuntimeRouter) reconcileSnapshotReplication(ctx context.Context, probes map[string]hostProbe) error {
	if r == nil || r.placementStore == nil || r.scheduler == nil {
		return nil
	}
	var errs []error

	// 1. Discover.
	liveSnapshots := make(map[string]bool)
	observedHosts := make(map[string]map[string]bool)
	infoByID := make(map[string]SnapshotInfo)
	discovered := false
	for hostName, daemon := range r.daemons {
		if daemon == nil || !probes[hostName].reachable {
			continue
		}
		lister, ok := daemon.(snapshotTransferDaemon)
		if !ok {
			continue
		}
		snaps, err := lister.ListSnapshots(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("reconcile snapshot replication: list snapshots on runtime host %q failed", hostName))
			continue
		}
		for _, s := range snaps {
			id := strings.TrimSpace(s.SnapshotID)
			if id == "" {
				continue
			}
			liveSnapshots[id] = true
			if observedHosts[id] == nil {
				observedHosts[id] = make(map[string]bool)
			}
			observedHosts[id][hostName] = true
			if _, seen := infoByID[id]; !seen {
				infoByID[id] = s
			}
			if err := r.placementStore.SetSnapshotLocation(id, hostName); err == nil {
				discovered = true
			}
		}
	}
	if discovered {
		if err := r.placementStore.Save(); err != nil {
			errs = append(errs, fmt.Errorf("reconcile snapshot replication: persisting discovered locations failed"))
		}
	}

	// 2. Revival reset.
	for key := range r.replicationDialFailures {
		if _, target := splitSnapshotTargetKey(key); probes[target].reachable {
			delete(r.replicationDialFailures, key)
			delete(r.replicationGivingUp, key)
		}
	}

	// 3. Heal drift.
	locations := r.placementStore.State().SnapshotLocations
	ids := make([]string, 0, len(locations))
	for id := range locations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := time.Now().UTC()
	var queueDepth int64
	for _, id := range ids {
		hosts := locations[id] // sorted by SetSnapshotLocation
		if len(hosts) >= snapshotReplicaFactor {
			continue
		}
		queueDepth++
		source := ""
		for _, h := range hosts {
			if probes[h].reachable && observedHosts[id][h] {
				source = h
				break
			}
		}
		if source == "" {
			continue // source unreachable/absent this pass; still counted in queue_depth
		}
		info := infoByID[id]
		excluded := append([]string(nil), hosts...)
		excluded = append(excluded, r.excludedReplicationTargets(id)...)
		target, err := SelectRuntimeHost(r.scheduler.hosts, ScheduleRequest{
			TenantID:      info.TenantID,
			EgressPolicy:  EgressPolicy(info.EgressPolicy),
			ExcludedHosts: excluded,
		})
		if err != nil {
			r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
				Outcome: SnapshotReplicationOutcomeNoCandidate,
				Reason:  SnapshotReplicationReasonNoEligibleHost,
				At:      now,
			})
			continue
		}
		tgtName := strings.TrimSpace(target.Name)
		key := snapshotTargetKey(id, tgtName)
		if !probes[tgtName].reachable {
			r.replicationDialFailures[key]++
			r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
				Outcome: SnapshotReplicationOutcomeDialFailed,
				Reason:  SnapshotReplicationReasonTargetUnreachable,
				At:      now,
			})
			if r.replicationDialFailures[key] >= snapshotReplicationFailureCap {
				r.replicationGivingUp[key] = true
				r.logf("anvil-mcp: snapshot replication for %q giving up on target host %q after %d dial failures", id, tgtName, r.replicationDialFailures[key])
			}
			errs = append(errs, fmt.Errorf("reconcile snapshot replication for %q: target host %q unreachable", id, tgtName))
			continue
		}
		start := time.Now()
		resp, repErr := r.ReplicateSnapshot(ctx, SnapshotReplicationRequest{
			SnapshotID:          id,
			SourceHost:          source,
			TargetHost:          tgtName,
			IncludeDependencies: true,
		})
		errs = r.classifySnapshotReplication(id, tgtName, key, resp, repErr, time.Since(start), now, errs)
	}

	// 4. Gauges.
	givingUpSnaps := make(map[string]bool)
	for key := range r.replicationGivingUp {
		if s, _ := splitSnapshotTargetKey(key); s != "" {
			givingUpSnaps[s] = true
		}
	}
	if err := r.placementStore.RecordSnapshotReplicationGauges(queueDepth, int64(len(givingUpSnaps))); err != nil {
		errs = append(errs, fmt.Errorf("reconcile snapshot replication: recording gauges failed"))
	}

	// 5. Sweep counters for snapshots no longer observed (deleted/removed).
	for key := range r.replicationDialFailures {
		if s, _ := splitSnapshotTargetKey(key); !liveSnapshots[s] {
			delete(r.replicationDialFailures, key)
		}
	}
	for key := range r.replicationGivingUp {
		if s, _ := splitSnapshotTargetKey(key); !liveSnapshots[s] {
			delete(r.replicationGivingUp, key)
		}
	}
	return errors.Join(errs...)
}

// excludedReplicationTargets lists the in-memory targets to keep out of selection
// for a snapshot. Task 2: giving-up targets only. (Task 3 also adds terminal
// targets.)
func (r *RuntimeRouter) excludedReplicationTargets(snapshotID string) []string {
	var out []string
	for key := range r.replicationGivingUp {
		if s, tgt := splitSnapshotTargetKey(key); s == snapshotID {
			out = append(out, tgt)
		}
	}
	return out
}

// classifySnapshotReplication records the metric outcome for one replication
// attempt and updates the dial counter. Task 2: success (reset) vs error
// (collected, retried next pass). Task 3 splits terminal out of error.
func (r *RuntimeRouter) classifySnapshotReplication(id, target, key string, resp *SnapshotReplicationResponse, repErr error, total time.Duration, at time.Time, errs []error) []error {
	if resp != nil && repErr == nil && resp.Status == "replicated" {
		delete(r.replicationDialFailures, key)
		delete(r.replicationGivingUp, key)
		outcome, reason := SnapshotReplicationOutcomeReplicated, SnapshotReplicationReasonScheduled
		if len(resp.Replicated) == 0 { // every item was already present on the target
			outcome, reason = SnapshotReplicationOutcomeAlreadyPresent, SnapshotReplicationReasonIdempotent
		}
		r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
			Outcome:   outcome,
			Reason:    reason,
			Latencies: map[string]time.Duration{SnapshotReplicationPhaseTotal: total},
			At:        at,
		})
		r.logf("anvil-mcp: snapshot %q replicated to host %q (%d desired replicas)", id, target, snapshotReplicaFactor)
		return errs
	}
	// Reachable target but the transfer did not succeed. Task 2 collects it as a
	// generic error and retries next pass (Task 3 refines the non-error case into a
	// terminal exclusion).
	r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
		Outcome:   SnapshotReplicationOutcomeError,
		Reason:    SnapshotReplicationReasonTransferError,
		Latencies: map[string]time.Duration{SnapshotReplicationPhaseTotal: total},
		At:        at,
	})
	return append(errs, fmt.Errorf("reconcile snapshot replication for %q on target host %q failed", id, target))
}

func (r *RuntimeRouter) recordSnapshotReplicationMetric(obs SnapshotReplicationMetricObservation) {
	if r == nil || r.placementStore == nil {
		return
	}
	_ = r.placementStore.RecordSnapshotReplicationMetrics(obs)
}
```

- [ ] **Step 4: 통과 확인 + race**

```bash
go test ./internal/anvilmcp/ -run TestSnapshotReplication_ -v
go test -race ./internal/anvilmcp/ -run 'TestSnapshotReplication_|TestReconcile'
```
Expected: PASS (6 sweep tests + existing reconcile tests unaffected — the sweep is not yet wired into `ReconcilePlacements`).

- [ ] **Step 5: Commit**

```bash
git add internal/anvilmcp/snapshot_replication.go internal/anvilmcp/runtime_router.go internal/anvilmcp/snapshot_replication_sweep_test.go
git commit -m "feat(anvilmcp): reconcile snapshot replication sweep with bounded dial retry"
```

---

### Task 3: wire sweep into reconcile + terminal classification + redaction

**Files:**
- Modify: `internal/anvilmcp/runtime_router.go` (`ReconcilePlacements` calls the sweep; `replicationTerminal` field + init)
- Modify: `internal/anvilmcp/snapshot_replication.go` (`classifySnapshotReplication` terminal branch; `excludedReplicationTargets` adds terminal targets; terminal cleanup in the sweep's step 5)
- Test: `internal/anvilmcp/snapshot_replication_sweep_test.go` (terminal + reconcile-wiring/redaction tests)

**Interfaces:**
- Consumes: Task 2's sweep + all its helpers; `ReconcilePlacements` `probes` (already built from `ListVMs`).
- Produces: `ReconcilePlacements` now runs the sweep each pass (its error joined into the pass's `errors.Join`); a reachable non-success transfer is `terminal_rejected` (target excluded in-memory, never retried, never counted toward the dial cap); `replicationTerminal map[string]bool` (reconcileMu-guarded).

- [ ] **Step 1: 실패하는 테스트 작성**

`snapshot_replication_sweep_test.go`에 추가 (import에 `"errors"`, `"strings"` 추가):

```go
// TestSnapshotReplication_TerminalRejectionIsNotRetried: a reachable target that
// refuses the import (D3 coarse-fs / tenant / validation surface as a failed
// transfer status) is terminal — recorded once, excluded from re-selection, never
// retried against the same target, and never counted toward the dial cap.
func TestSnapshotReplication_TerminalRejectionIsNotRetried(t *testing.T) {
	hostA := &routerFakeDaemon{snapshotList: []SnapshotInfo{snapInfo("snap-1")}}
	hostB := &routerFakeDaemon{importErrForBody: map[string]error{
		"bundle:snap-1": errors.New("refusing overlay to avoid guest memory corruption (see D3)"),
	}}
	router, store := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	probes := allReachable("hostA", "hostB")

	_ = router.reconcileSnapshotReplication(context.Background(), probes) // first attempt → terminal
	if len(hostB.importCalls) != 1 {
		t.Fatalf("first pass import calls = %d, want 1", len(hostB.importCalls))
	}
	m := store.State().SnapshotReplicationMetrics
	if m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeTerminalRejected, SnapshotReplicationReasonRejected)] != 1 {
		t.Fatalf("terminal_rejected not recorded: %+v", m.AttemptsByOutcomeReason)
	}
	if got := m.AttemptsByOutcomeReason[snapshotReplicationAttemptKey(SnapshotReplicationOutcomeDialFailed, SnapshotReplicationReasonTargetUnreachable)]; got != 0 {
		t.Fatalf("terminal failure wrongly counted as dial: %d", got)
	}

	_ = router.reconcileSnapshotReplication(context.Background(), probes) // second pass → excluded, not retried
	if len(hostB.importCalls) != 1 {
		t.Fatalf("terminal target retried: import calls = %d, want still 1", len(hostB.importCalls))
	}
}

// TestReconcilePlacements_RunsSnapshotReplicationSweep proves the sweep is wired
// into ReconcilePlacements (probes built from ListVMs) and that its logs leak no
// daemon address.
func TestReconcilePlacements_RunsSnapshotReplicationSweep(t *testing.T) {
	hostA := &routerFakeDaemon{
		snapshotList: []SnapshotInfo{snapInfo("snap-1")},
		listVMResp:   []VMInfo{{VMID: "vm-a"}},
	}
	hostB := &routerFakeDaemon{listVMResp: []VMInfo{}}
	router, _ := newReplicationRouter(t, replicationHosts("hostA", "hostB"),
		map[string]*routerFakeDaemon{"hostA": hostA, "hostB": hostB})
	var logs []string
	router.reconcileLogf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	if err := router.ReconcilePlacements(context.Background()); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if len(hostB.importCalls) != 1 {
		t.Fatal("snapshot replication sweep did not run within ReconcilePlacements")
	}
	joined := strings.Join(logs, "\n")
	for _, forbidden := range []string{"hostA.internal:8080", "hostB.internal:8080"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sweep log leaked a daemon address: %s", joined)
		}
	}
}
```

`snapshot_replication_sweep_test.go` import 블록에 `"fmt"` 도 추가 (위 테스트가 사용).

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/anvilmcp/ -run 'TestSnapshotReplication_TerminalRejectionIsNotRetried|TestReconcilePlacements_RunsSnapshotReplicationSweep' -v
```
Expected: FAIL — terminal test: `terminal_rejected not recorded` (Task 2 records it as `error`); wiring test: `sweep did not run within ReconcilePlacements`.

- [ ] **Step 3: 구현**

(a) `runtime_router.go` — field (`replicationGivingUp` 아래):

```go
	// replicationTerminal marks "<snapshot>\x1f<target>" pairs a reachable target
	// refused (D3 coarse-fs / tenant / validation). Excluded from re-selection for
	// this process lifetime; reset only on restart (in-memory, like the dial
	// counter). reconcileMu.
	replicationTerminal map[string]bool
```

`NewRuntimeRouterWithOptions` 리터럴 (`replicationGivingUp: ...` 아래):

```go
		replicationTerminal:     make(map[string]bool),
```

(b) `runtime_router.go` — wire into `ReconcilePlacements`. 현재:

```go
	errs = append(errs, r.reconcileRoutedFlockWalls(ctx, probes))
	return errors.Join(errs...)
```

변경 후:

```go
	errs = append(errs, r.reconcileRoutedFlockWalls(ctx, probes))
	errs = append(errs, r.reconcileSnapshotReplication(ctx, probes))
	return errors.Join(errs...)
```

(c) `snapshot_replication.go` — `excludedReplicationTargets`에 terminal 추가:

```go
func (r *RuntimeRouter) excludedReplicationTargets(snapshotID string) []string {
	var out []string
	for key := range r.replicationGivingUp {
		if s, tgt := splitSnapshotTargetKey(key); s == snapshotID {
			out = append(out, tgt)
		}
	}
	for key := range r.replicationTerminal {
		if s, tgt := splitSnapshotTargetKey(key); s == snapshotID {
			out = append(out, tgt)
		}
	}
	return out
}
```

(d) `snapshot_replication.go` — `classifySnapshotReplication`의 non-success 처리 교체. 현재의 마지막 블록(“Reachable target but … generic error …”)을 아래로 교체:

```go
	// Reachable target, transfer did not succeed. Two cases:
	//  - repErr != nil: ReplicateSnapshot failed BEFORE the transfer completed
	//    (source/target ListSnapshots, transient). The target answered the probe,
	//    so this is not a dial failure; surface it and retry next pass. Do NOT
	//    exclude the target — it may well succeed next time.
	//  - (resp, nil) with a non-"replicated" status: the reachable target refused
	//    the content — D3 coarse-fs overlay refusal, tenant/validation mismatch, or
	//    a missing diff base. TERMINAL (spec D-6): retrying the same target is
	//    futile, so exclude it in-memory (reset only on restart), record a terminal
	//    reason, and never count it toward the dial cap.
	if repErr != nil {
		r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
			Outcome:   SnapshotReplicationOutcomeError,
			Reason:    SnapshotReplicationReasonTransferError,
			Latencies: map[string]time.Duration{SnapshotReplicationPhaseTotal: total},
			At:        at,
		})
		return append(errs, fmt.Errorf("reconcile snapshot replication for %q on target host %q failed", id, target))
	}
	r.replicationTerminal[key] = true
	r.recordSnapshotReplicationMetric(SnapshotReplicationMetricObservation{
		Outcome:   SnapshotReplicationOutcomeTerminalRejected,
		Reason:    SnapshotReplicationReasonRejected,
		Latencies: map[string]time.Duration{SnapshotReplicationPhaseTotal: total},
		At:        at,
	})
	return append(errs, fmt.Errorf("reconcile snapshot replication for %q on target host %q rejected (terminal)", id, target))
```

(e) `snapshot_replication.go` — sweep step 5, terminal counter cleanup 추가 (dial/givingUp cleanup 아래):

```go
	for key := range r.replicationTerminal {
		if s, _ := splitSnapshotTargetKey(key); !liveSnapshots[s] {
			delete(r.replicationTerminal, key)
		}
	}
```

- [ ] **Step 4: 통과 확인 + 회귀 + race**

```bash
go test ./internal/anvilmcp/ -run 'TestSnapshotReplication_|TestReconcile' -v
go build ./... && go test -race ./internal/anvilmcp/
```
Expected: PASS. Existing reconcile tests (`TestReconcile_ReregistersSharedTownWall`, `TestReconcilePlacements_IsolatesUnreachableHost`, `TestFailover_*`) unaffected — the sweep runs after wall reconcile and its error joins the pass; those fakes list no snapshots so the sweep is a clean no-op there. If any existing test asserts an exact `ReconcilePlacements` error and the sweep adds a benign gauge write, confirm it stays nil (no snapshots ⇒ no sweep error).

- [ ] **Step 5: Commit**

```bash
git add internal/anvilmcp/runtime_router.go internal/anvilmcp/snapshot_replication.go internal/anvilmcp/snapshot_replication_sweep_test.go
git commit -m "feat(anvilmcp): run replication sweep in reconcile and classify terminal rejections"
```

---

### Task 4: expose the metric family on scheduler `/metrics`

**Files:**
- Modify: `internal/anvilmcp/scheduler_metrics.go` (`renderSnapshotReplicationMetrics` + call from `RenderSchedulerMetrics`)
- Test: `internal/anvilmcp/scheduler_metrics_test.go`

**Interfaces:**
- Consumes: `SnapshotReplicationMetricsState` + helpers (Task 1); `writeSchedulerGauge`, `timestampMetric`, `flockPlacementLatencyBuckets`, `formatFlockPlacementBucket` (`scheduler_metrics.go`). `PlacementStore.State()` already carries `SnapshotReplicationMetrics` (cloned, not redacted), and `scheduler_service.go:76` already renders full state — no endpoint change.
- Produces: Prometheus 0.0.4 lines `anvil_scheduler_snapshot_replication_attempts_total{outcome,reason}`, `..._latency_seconds{phase="total"}` histogram, `..._queue_depth`, `..._giving_up`, `..._last_success/last_failure_timestamp_seconds`.

- [ ] **Step 1: 실패하는 테스트 작성**

`scheduler_metrics_test.go`에 추가:

```go
func TestRenderSchedulerMetricsIncludesSnapshotReplicationMetrics(t *testing.T) {
	lastSuccess := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
	lastFailure := time.Date(2026, 7, 11, 2, 3, 4, 0, time.UTC)
	state := PlacementStoreState{
		SnapshotReplicationMetrics: SnapshotReplicationMetricsState{
			AttemptsByOutcomeReason: map[string]int64{
				snapshotReplicationAttemptKey(SnapshotReplicationOutcomeReplicated, SnapshotReplicationReasonScheduled):        3,
				snapshotReplicationAttemptKey(SnapshotReplicationOutcomeDialFailed, SnapshotReplicationReasonTargetUnreachable): 2,
				"tenant-1|http://host-a:3000": 5, // junk key must be normalized, not leaked
			},
			LatencyByPhase: map[string]LatencyHistogramState{
				SnapshotReplicationPhaseTotal: {
					Buckets:    map[string]int64{"0.5": 1, "+Inf": 1, "agent_token": 9},
					SumSeconds: 0.4,
					Count:      1,
				},
			},
			QueueDepth:    4,
			GivingUp:      1,
			LastSuccessAt: lastSuccess,
			LastFailureAt: lastFailure,
		},
	}

	output := RenderSchedulerMetrics(state)
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_attempts_total{outcome=\"replicated\",reason=\"scheduled\"} 3")
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_attempts_total{outcome=\"dial_failed\",reason=\"target_unreachable\"} 2")
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_latency_seconds_bucket{phase=\"total\",le=\"0.5\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_latency_seconds_bucket{phase=\"total\",le=\"+Inf\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_latency_seconds_count{phase=\"total\"} 1")
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_queue_depth 4")
	requireMetricLine(t, output, "anvil_scheduler_snapshot_replication_giving_up 1")
	requireMetricLine(t, output, fmt.Sprintf("anvil_scheduler_snapshot_replication_last_success_timestamp_seconds %d", lastSuccess.Unix()))
	requireMetricLine(t, output, fmt.Sprintf("anvil_scheduler_snapshot_replication_last_failure_timestamp_seconds %d", lastFailure.Unix()))
	for _, leaked := range []string{"tenant-1", "http://host-a", "agent_token"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("snapshot replication metrics leaked %q:\n%s", leaked, output)
		}
	}
}

func TestSchedulerServiceMetricsEndpointExposesSnapshotReplication(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.RecordSnapshotReplicationGauges(2, 0); err != nil {
		t.Fatalf("RecordSnapshotReplicationGauges: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	requireMetricLine(t, rr.Body.String(), "anvil_scheduler_snapshot_replication_queue_depth 2")
}
```

- [ ] **Step 2: 실패 확인**

```bash
go test ./internal/anvilmcp/ -run 'SnapshotReplication.*Metrics|MetricsEndpointExposesSnapshotReplication' -v
```
Expected: FAIL — the metric lines are absent from the rendered output.

- [ ] **Step 3: 구현**

`scheduler_metrics.go` — `RenderSchedulerMetrics`의 `renderFlockPlacementMetrics(&out, state.FlockPlacementMetrics)` 아래:

```go
	renderSnapshotReplicationMetrics(&out, state.SnapshotReplicationMetrics)
```

새 함수 (`renderFlockPlacementMetrics` 아래):

```go
func renderSnapshotReplicationMetrics(out *strings.Builder, state SnapshotReplicationMetricsState) {
	normalizeSnapshotReplicationMetricsState(&state)

	out.WriteString("# HELP anvil_scheduler_snapshot_replication_attempts_total Snapshot replication attempts by outcome and reason.\n")
	out.WriteString("# TYPE anvil_scheduler_snapshot_replication_attempts_total counter\n")
	for _, key := range sortedSnapshotReplicationAttemptKeys(state.AttemptsByOutcomeReason) {
		outcome, reason := splitSnapshotReplicationAttemptKey(key)
		fmt.Fprintf(out, "anvil_scheduler_snapshot_replication_attempts_total{outcome=\"%s\",reason=\"%s\"} %d\n", outcome, reason, state.AttemptsByOutcomeReason[key])
	}

	out.WriteString("# HELP anvil_scheduler_snapshot_replication_latency_seconds Snapshot replication latency by phase.\n")
	out.WriteString("# TYPE anvil_scheduler_snapshot_replication_latency_seconds histogram\n")
	hist := state.LatencyByPhase[SnapshotReplicationPhaseTotal]
	for _, upper := range flockPlacementLatencyBuckets {
		bucket := formatFlockPlacementBucket(upper)
		fmt.Fprintf(out, "anvil_scheduler_snapshot_replication_latency_seconds_bucket{phase=\"%s\",le=\"%s\"} %d\n", SnapshotReplicationPhaseTotal, bucket, hist.Buckets[bucket])
	}
	fmt.Fprintf(out, "anvil_scheduler_snapshot_replication_latency_seconds_bucket{phase=\"%s\",le=\"+Inf\"} %d\n", SnapshotReplicationPhaseTotal, hist.Buckets["+Inf"])
	fmt.Fprintf(out, "anvil_scheduler_snapshot_replication_latency_seconds_sum{phase=\"%s\"} %s\n", SnapshotReplicationPhaseTotal, strconv.FormatFloat(hist.SumSeconds, 'f', -1, 64))
	fmt.Fprintf(out, "anvil_scheduler_snapshot_replication_latency_seconds_count{phase=\"%s\"} %d\n", SnapshotReplicationPhaseTotal, hist.Count)

	writeSchedulerGauge(out, "anvil_scheduler_snapshot_replication_queue_depth", "Snapshots below the desired replica factor.", float64(state.QueueDepth))
	writeSchedulerGauge(out, "anvil_scheduler_snapshot_replication_giving_up", "Snapshots with a dial-saturated replication target.", float64(state.GivingUp))
	writeSchedulerGauge(out, "anvil_scheduler_snapshot_replication_last_success_timestamp_seconds", "Unix timestamp of the last successful snapshot replication.", timestampMetric(state.LastSuccessAt))
	writeSchedulerGauge(out, "anvil_scheduler_snapshot_replication_last_failure_timestamp_seconds", "Unix timestamp of the last failed snapshot replication.", timestampMetric(state.LastFailureAt))
}
```

- [ ] **Step 4: 통과 확인 + race**

```bash
go test ./internal/anvilmcp/ -run 'RenderSchedulerMetrics|MetricsEndpoint' -v
go test -race ./internal/anvilmcp/
```
Expected: PASS (new tests + `TestRenderSchedulerMetricsHandlesZeroState` still green — zero state renders queue_depth/giving_up 0 and no attempt lines).

- [ ] **Step 5: Commit**

```bash
git add internal/anvilmcp/scheduler_metrics.go internal/anvilmcp/scheduler_metrics_test.go
git commit -m "feat(anvilmcp): render snapshot replication metrics on scheduler /metrics"
```

---

### Task 5: KVM e2e — `scripts/anvil-snapshot-replication-e2e.sh`

**Files:**
- Create: `scripts/anvil-snapshot-replication-e2e.sh`

**Interfaces:**
- Consumes: Tasks 1-4 (adapter members_only sweep + persisted metrics + `/metrics`). Patterns: `scripts/anvil-cross-host-failover-e2e.sh` (real daemon + python recorder stubs + adapter over MCP stdio with `cross_host_flock_create_mode=members_only`, short `reconcile_interval`), `scripts/anvil-cross-host-wall-e2e.sh` (stub recorder + real VM + cleanup trap). The sweep only runs when `CrossHostFlockCreateMode == "members_only"` && `ReconcileIntervalParsed > 0` (`cmd/anvil-mcp/main.go:258`).
- Produces: a standalone KVM e2e gate (root + KVM required, like the other cross-host e2es — separate from `e2e_test.sh`).

**Topology** (single physical host — one real daemon, same bridge-collision reason as the wall/failover e2es):
- real anvil-daemon (auth-on, source of truth) — hosts one real KVM VM, creates a real snapshot on it (source replica).
- stub B (python3 recorder, `127.0.0.1:3101`) — the replication target. Extends the failover/wall recorder with the snapshot transfer surface: `GET /vms` → `[]` (adapter reachability probe), `GET /snapshots` → `[]` initially then the imported id after an import, `POST /snapshots/import` → record body + `{"snapshot_id":...,"status":"imported"}` 200 (or `{"status":"already_present"}` when it already holds it), and — since the real daemon is the source — the stub only needs the **import** side. Loopback-bound.
- adapter: `cmd/anvil-mcp` over MCP stdio, `cross_host_flock_create_mode=members_only`, `scheduler_state_path=<tmp>/placements.json`, `reconcile_interval=2s`. Copy gitignored `configs/goose.yaml`/`goose-secrets.yaml` from the main checkout so the real VM/snapshot path does not 500 silently.
- Host inventory: real-host (AvailableVMs≥1, egress profile) + stub-b (AvailableVMs≥1, egress profile), so `SelectRuntimeHost` picks stub-b as the only eligible target (real host is the source, excluded).

- [ ] **Step 1: 스크립트 골격** — failover e2e의 헤더 주석 스타일(무엇을 증명/무엇을 단일-host 범위에서 제외), `set -Eeuo pipefail`, `step()/ok()/fail()` 헬퍼, artifact 디렉토리, cleanup trap(stub kill + snapshot/VM teardown + adapter 종료 + root 소유 잔재 정리). 판정은 **exit code + 마지막 "passed ✓" 라인만** (tail cleanup 출력은 실패 시에도 동일).

- [ ] **Step 2: Phase 0 — 셋업.** adapter로 real VM spawn → real 스냅샷 생성(`anvil_create_snapshot`) → `placements.json`의 `snapshot_locations[<id>]`에 real-host 존재 확인, `/metrics`(scheduler service 또는 adapter가 노출하는 경로)에서 `anvil_scheduler_snapshot_replication_queue_depth`가 관측되는지 확인 (초기 1 예상 — 복제본 1/2).

- [ ] **Step 3: Phase 1 — 자동 복제.** stub B 살아있는 상태로 `reconcile_interval 2s`를 최소 1주기 대기(poll ≤ 30s): stub B capture에 `POST /snapshots/import`(body = export bundle) 도달 확인 → `snapshot_locations[<id>]`가 `[real-host, stub-b]`(len 2)로 수렴 → `/metrics`에서 `anvil_scheduler_snapshot_replication_attempts_total{outcome="replicated",reason="scheduled"}` ≥ 1, `queue_depth` 0. redaction spot check: `/metrics`와 adapter stderr에 stub 주소(`127.0.0.1`)·토큰 문자열 부재.

- [ ] **Step 4: Phase 2 — 대상 down → giving-up.** stub B kill → 새 스냅샷 하나 더 생성(다시 under-replicated) → poll로 `/metrics`의 `anvil_scheduler_snapshot_replication_attempts_total{outcome="dial_failed",...}` 증가 및 `anvil_scheduler_snapshot_replication_giving_up` ≥ 1 확인 (`reconcile_interval 2s × cap 3` ≈ 6-10s). stub B로 import 도달 없음(kill 상태).

- [ ] **Step 5: Phase 3 — 복귀 리셋.** stub B 재기동 → poll로 `giving_up` 0 복귀 및 두 번째 스냅샷의 `snapshot_locations` len 2 수렴, stub B capture에 새 import 도달 확인. adapter stderr에 "giving up"/"replicated" 로그가 snapshot id + host name만 담고 stub 주소·토큰 부재.

- [ ] **Step 6: 실행 검증**

```bash
sudo bash scripts/anvil-snapshot-replication-e2e.sh; echo "exit=$?"
```
Expected: `exit=0` + 마지막 라인 `All snapshot replication e2e steps passed ✓`. 실행 워크트리에 gitignored `configs/goose.yaml`/`goose-secrets.yaml`을 메인 checkout에서 복사. sudo 실행 후 root 소유 잔재(vms/, artifacts/)는 sudo rm으로 정리.

- [ ] **Step 7: Commit**

```bash
git add scripts/anvil-snapshot-replication-e2e.sh
git commit -m "test(e2e): cross-host snapshot replication automation KVM e2e"
```

---

### Task 6: docs — backlog→done, runbook alert expressions, ADR row, boundary, handoff

**Files:**
- Modify: `CONTEXT.md`, `docs/operations/runbook.md`, `docs/ADR_INDEX.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`, `docs/operations/2026-06-02-cross-host-snapshot-replication-handoff.md`
- Create: `docs/operations/2026-07-11-snapshot-replication-automation-handoff.md`

**Interfaces:**
- Consumes: Tasks 1-5 완료 상태 (구현 사실 기술).
- Produces: release 단계 zone 인벤토리 동기화 입력 (handoff의 zone 연동 절: `docs/FOLLOWUP.md`, `ops/units.yaml`/`ops/projects.yaml` 필요 시).

- [ ] **Step 1: CONTEXT.md** — backlog `:433-434`("cross-host snapshot replication 자동화(background retry queue·metrics·alert …)")를 구현 완료로 이동/치환: 자동 sweep(N=2 상수·bounded dial cap 3·terminal 분류·복귀 리셋), metric(`anvil_scheduler_snapshot_replication_*`), alert 경계(metric+runbook) 요약. 완료 상태 목록(`:209-228`) 및 baseline 서술 갱신. 남은 후속 후보에서 이 항목 제거.

- [ ] **Step 2: runbook.md** — 자동 복제 sweep 운영 절 추가: (a) 동작(reconcile 매 주기 discover→drift(N=2)→select(`SelectRuntimeHost`)→ReplicateSnapshot 1회, dial 실패 bounded cap 3 후 giving-up, 대상 복귀 시 재시도, terminal(D3/tenant/validation)=비재시도 exclude), (b) **권장 alert 식**(D-5): `anvil_scheduler_snapshot_replication_queue_depth > 0` 지속, `anvil_scheduler_snapshot_replication_giving_up > 0`, `time() - anvil_scheduler_snapshot_replication_last_success_timestamp_seconds` staleness, (c) D3 상호작용 교차참조(`runbook.md:435-471`): `terminal_rejected`가 coarse-fs cross-fs 복제 거부를 포함하며 재시도하지 않음, (d) redaction 계약(metric label = outcome/reason/phase만).

- [ ] **Step 3: ADR_INDEX.md** — §3 표에 row 추가(기존 cross-host 결정 row 관례: design spec을 결정 원문으로, 별도 `docs/adr/*.md` 없음). 포함: 재선언적 복제 자동화(reconcile sweep), replica factor N=2 상수·retry cap 3 상수·dial-only+revival reset·terminal 분류 = 전부 YAGNI 상수(`ADR_INDEX.md:48` 방침), metric PlacementStore 영속, alert 경계 = metric+runbook, 비목표(GC/rebalance/async queue/cross-tenant), 상태 링크(`superpowers/specs/2026-07-11-snapshot-replication-automation-design.md`, handoff).

- [ ] **Step 4: PUBLIC_RELEASE_BOUNDARY.md** — snapshot/replication 관련 row에 새 metric 표면(`anvil_scheduler_snapshot_replication_*`)이 공개 경계에 미치는 영향 검토: 저-cardinality label(outcome/reason/phase)만, host 주소·토큰·snapshot id 부재 — 공개 안전. "수동 동기 replication"의 SPOF 서술을 "자동 N=2 수렴(best-effort eventual, 무손실 보장 아님)"으로 갱신.

- [ ] **Step 5: 기존 handoff 종결** — `2026-06-02-cross-host-snapshot-replication-handoff.md`의 "후속 후보"(`:63,:77`: background retry queue·metrics·alert)를 이 slice로 해소 표시 + 신규 handoff 링크.

- [ ] **Step 6: 신규 handoff** — `docs/operations/2026-07-11-snapshot-replication-automation-handoff.md`: 무엇이 main에 있나(sweep/metric/e2e) / 검증 증거(unit -race + KVM e2e exit 0) / Follow-Up(실 multi-host 수동 검증, zone `docs/FOLLOWUP.md` 갱신, release 인벤토리 동기화). `Next Action`/`Follow-Up Tasks` 정렬.

- [ ] **Step 7: 링크 검증 + Commit**

```bash
grep -rn "snapshot-replication-automation\|snapshot_replication" docs/ CONTEXT.md | grep -v Binary
git add docs/ CONTEXT.md
git commit -m "docs: snapshot replication automation — backlog closure, runbook alerts, ADR row, boundary, handoff"
```

---

## 최종 검증 (전체 슬라이스)

- [ ] `go build ./... && go vet ./... && gofmt -l . | grep -v '^web/' ; go test -race ./internal/... ./cmd/...` — 전부 clean/PASS
- [ ] `sudo bash scripts/anvil-snapshot-replication-e2e.sh` — exit 0 + "passed ✓"
- [ ] 기존 cross-host e2e 회귀 (reconcile 경로를 만졌으므로 필수): `sudo bash scripts/anvil-cross-host-failover-e2e.sh`, `sudo bash scripts/anvil-cross-host-wall-e2e.sh` — exit 0
- [ ] 전체 KVM 게이트 `sudo bash e2e_test.sh` — exit code 판정
- [ ] secret-scan: `bash scripts/secret-scan.sh` — 신규 코드/로그/metric에 토큰·host 주소 유출 없음
- [ ] PR 생성 (`feature/snapshot-replication-automation` → main). **자체 머지 금지** — 머지는 사용자 승인으로만.

## Self-Review 기록 (플랜 작성 시점)

**Spec 경계 사례 → Task/테스트 매핑:**
- 대상 후보 0 (모든 타 host down / 단일-host) → Task 2 `TestSnapshotReplication_NoCandidateSurfacesQueueDepth` (no-op + queue_depth + no_candidate).
- 재시작 중 부분 복제 → Task 2 `TestSnapshotReplication_RestartResetsInMemoryCounters` (영속 desired/actual 비교로 재개, in-memory 카운터만 리셋).
- 동시 수동 + 자동 → discovery의 add-only `SetSnapshotLocation` + `ReplicateSnapshot` import idempotency (`already_present`) → Task 2 success 분기 `already_present` outcome (resp.Replicated 비었을 때). e2e Phase 1이 wire 확인.
- D3 coarse 대상 → Task 3 `TestSnapshotReplication_TerminalRejectionIsNotRetried` (reachable 대상 import 거부 → terminal_rejected + exclude + 재시도 0).
- 스냅샷 삭제 후 → sweep 대상 제외 + in-memory 카운터 청소(step 5, `liveSnapshots` 게이트 — `homeFailures` 청소 패턴 미러). (전용 유닛 테스트 없음 — `liveSnapshots` 미관측 시 카운터 delete 로 커버, 필요 시 워커가 추가.)
- diff 스냅샷 → 기존 `ReplicateSnapshot`의 `IncludeDependencies` 재사용(sweep가 `IncludeDependencies: true` 전달) — base full 선복제 로직 그대로.
- 다중 adapter 인스턴스 → `PlacementStore` last-writer-wins + `refreshPersistedMetricsLocked`가 metric 카운터 cross-family clobber 방지 → Task 1 `TestPlacementStoreSavePreservesSnapshotReplicationMetrics`.
- dial 실패 K회 giving-up + 복귀 리셋 → Task 2 `TestSnapshotReplication_GivesUpAfterDialFailureCap` + `...DialCounterResetsWhenTargetRevives`.
- tenant/egress 부적격 대상 제외 → Task 2 `TestSnapshotReplication_RespectsHostEligibility` (`SelectRuntimeHost` SmokeOnly 제외).
- metrics 유닛(Prometheus 0.0.4 + label cardinality) → Task 4 `TestRenderSchedulerMetricsIncludesSnapshotReplicationMetrics`(junk label 미유출) + 엔드포인트 노출.
- redaction 유닛(host 주소·토큰 부재) → Task 1 `TestPlacementStoreSanitizesSnapshotReplicationMetricLabels`, Task 3 `TestReconcilePlacements_RunsSnapshotReplicationSweep`(로그 주소 부재), Task 4 render redaction.
- KVM e2e(생성→대상 down→복귀 자동 복제 + /metrics attempts/last_success) → Task 5 Phase 0-3.
- 문서 반영(CONTEXT/runbook alert/handoff/ADR/PUBLIC_RELEASE_BOUNDARY) → Task 6.

**Global Constraints 매핑:** N=2/cap 3 상수(Task 2 consts, 설정화 없음) · dial-only+복귀 리셋(probe 게이트 + step 2 reset) · terminal 분류(Task 3) · redaction/저-cardinality label(Task 1/3/4) · tenant carry(sweep가 `SnapshotInfo` tenant/egress를 `SelectRuntimeHost`에 전달) · reconcileMu 규율(필드 주석 + 락 없음) · git trailer 금지/`-race`/main push 금지(각 Task + 최종 검증).

**Type consistency:** fake 필드명 실물 대조 완료 — `snapshotList`/`listSnapshotErr`/`importCalls`/`importErrForBody`/`exportCalls`/`listVMResp`/`listVMErr` (`runtime_router_test.go:19-72`), `SnapshotInfo{SnapshotID,TenantID,EgressPolicy,SnapshotType}` (`daemon_client.go:93`), `ReplicateSnapshot` success = `resp.Status=="replicated"` + `resp.Replicated`/`resp.Skipped` (`snapshot_replication.go:160-172`), `SelectRuntimeHost(hosts, ScheduleRequest{TenantID,EgressPolicy,ExcludedHosts})` (`tenant_policy.go:155`), `RuntimeHost.SmokeOnly`/`EgressPolicyProfile` (`tenant_policy.go:46,57`), metric 영속 이중 경로(`Save`=preserveMetrics true / `saveLockedRaw`=false, `refreshPersistedMetricsLocked` cross-family) (`placement_store.go:201-215,337-342`).

**알려진 deviation / 리스크 (리뷰어 주의):**
1. **Latency phase = `total`만.** 스펙 §D-4는 export/import/total 세 phase를 열거하나, 스펙 D-3의 `ReplicateSnapshot` 재사용 지시상 adapter는 export/import sub-timing을 관측할 수단이 없다(스트림 내부). `total`만 기록/렌더하고 코드 주석 + 이 절에 명시. export/import를 별도 계측하려면 sweep가 `ReplicateSnapshot`을 우회해 export/import를 직접 호출해야 하는데, 이는 diff-deps/idempotency/location-record 로직 중복이라 D-3(재사용)에 위배. → 컨트롤러 판단 필요.
2. **터미널 오분류 창.** probe reachable 직후 대상이 죽어 `ReplicateSnapshot`이 `(nil,error)`(list dial)로 실패하면 `error`(비-exclude, 재시도) 분기로 가고, `(resp,nil)` non-success면 `terminal`(exclude)로 간다. 순수 일시적 import 실패가 `terminal`로 영구(프로세스 수명) 제외될 수 있으나 재시작이 in-memory 제외를 리셋(스펙 "무한 재시도 금지" 준수, 복제 GC 비목표와 정합). 스펙 결정 #5(coarse-fs terminal-fail 수용)와 정합하나, 이 이분법이 과한지 컨트롤러 확인 권장.
3. **uniform N=2 discovery.** discovery가 모든 reachable host의 모든 스냅샷을 `SnapshotLocations`에 add-only 수집 → base/throwaway 스냅샷도 N=2로 수렴(스펙 "모든 스냅샷 균일 N"과 정합, 비목표에 클래스 예외 없음). 첫 sweep 트래픽이 클 수 있으나 bounded/idempotent 수렴. 클래스 예외가 필요하면 별도 결정 필요.
4. **Task 3의 terminal 분류를 Task 2에서 error로 선-구현 → Task 3에서 분리** (브리프의 "terminal 분류=Task 3"을 TDD 진행으로 반영). 브리프의 "자연스럽게 조정 가능" 범위 내 재배치이며 최종 배선/재분류는 Task 3에 온전히 존재.
