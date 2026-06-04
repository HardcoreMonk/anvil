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

func TestPlacementStoreRollsBackFlockPlacementMetricsOnSaveFailure(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	at := time.Date(2026, 6, 4, 1, 2, 3, 0, time.UTC)

	err := store.RecordFlockPlacementMetrics(FlockPlacementMetricObservation{
		Outcome: FlockPlacementOutcomeSuccess,
		Reason:  FlockPlacementReasonScheduled,
		At:      at,
		Latencies: map[string]time.Duration{
			FlockPlacementPhaseSchedule: 7 * time.Millisecond,
		},
	})
	if err == nil {
		t.Fatal("RecordFlockPlacementMetrics unexpectedly succeeded with directory path")
	}

	state := store.State().FlockPlacementMetrics
	key := flockPlacementAttemptKey(FlockPlacementOutcomeSuccess, FlockPlacementReasonScheduled)
	if got := state.AttemptsByOutcomeReason[key]; got != 0 {
		t.Fatalf("attempt count after failed save = %d, want rollback to 0", got)
	}
	if !state.LastSuccessAt.IsZero() {
		t.Fatalf("LastSuccessAt after failed save = %v, want zero", state.LastSuccessAt)
	}
	if len(state.LatencyByPhase) != 0 {
		t.Fatalf("LatencyByPhase after failed save = %+v, want rollback to empty", state.LatencyByPhase)
	}
}

func TestPlacementStoreFiltersFlockPlacementMetricsHistogramBuckets(t *testing.T) {
	state := FlockPlacementMetricsState{
		LatencyByPhase: map[string]LatencyHistogramState{
			FlockPlacementPhaseSchedule: {
				Buckets: map[string]int64{
					"0.005":         1,
					"0.01":          2,
					"+Inf":          2,
					"agent_token":   99,
					"http://host-a": 42,
				},
				SumSeconds: 0.012,
				Count:      2,
			},
		},
	}

	normalizeFlockPlacementMetricsState(&state)
	assertFlockPlacementHistogramBucketsFiltered(t, state.LatencyByPhase[FlockPlacementPhaseSchedule])

	cloned := cloneFlockPlacementMetricsState(FlockPlacementMetricsState{
		LatencyByPhase: map[string]LatencyHistogramState{
			FlockPlacementPhaseSchedule: {
				Buckets: map[string]int64{
					"0.005":         1,
					"0.01":          2,
					"+Inf":          2,
					"agent_token":   99,
					"http://host-a": 42,
				},
				SumSeconds: 0.012,
				Count:      2,
			},
		},
	})
	assertFlockPlacementHistogramBucketsFiltered(t, cloned.LatencyByPhase[FlockPlacementPhaseSchedule])
}

func assertFlockPlacementHistogramBucketsFiltered(t *testing.T, hist LatencyHistogramState) {
	t.Helper()
	if hist.Buckets["0.005"] != 1 || hist.Buckets["0.01"] != 2 || hist.Buckets["+Inf"] != 2 {
		t.Fatalf("allowed buckets = %+v, want 0.005=1 0.01=2 +Inf=2", hist.Buckets)
	}
	if _, ok := hist.Buckets["agent_token"]; ok {
		t.Fatalf("histogram buckets retained token-like key: %+v", hist.Buckets)
	}
	if _, ok := hist.Buckets["http://host-a"]; ok {
		t.Fatalf("histogram buckets retained endpoint-like key: %+v", hist.Buckets)
	}
}
