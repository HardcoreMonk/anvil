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
	// SnapshotReplicationReasonExportFailed/ImportFailed (Follow-Up 0,
	// transfer-failure classification refinement) split the retryable
	// "error" outcome by which side of a non-"replicated" ReplicateSnapshot
	// response failed — see classifySnapshotReplication. Only an explicit
	// target refusal uses SnapshotReplicationReasonRejected (terminal); a
	// source-side, ambiguous target-side, or internal bookkeeping failure
	// uses one of these two (or, for the internal case, the pre-existing
	// SnapshotReplicationReasonTransferError).
	SnapshotReplicationReasonExportFailed = "export_failed"
	SnapshotReplicationReasonImportFailed = "import_failed"

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
		SnapshotReplicationReasonNoEligibleHost,
		SnapshotReplicationReasonExportFailed,
		SnapshotReplicationReasonImportFailed:
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
