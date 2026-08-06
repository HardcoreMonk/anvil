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
	if err := store.RecordSnapshotReplicationGauges(4, 1, 0); err != nil {
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
