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
			FlockPlacementPhaseDaemonCreate:  120 * time.Millisecond,
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
