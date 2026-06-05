package anvilmcp

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	FlockPlacementOutcomeSuccess                = "success"
	FlockPlacementOutcomeDenied                 = "denied"
	FlockPlacementOutcomeSchedulerError         = "scheduler_error"
	FlockPlacementOutcomeDaemonError            = "daemon_error"
	FlockPlacementOutcomeDaemonNilResponse      = "daemon_nil_response"
	FlockPlacementOutcomePlacementSaveError     = "placement_save_error"
	FlockPlacementOutcomeCrossHostSuccess       = "cross_host_success"
	FlockPlacementOutcomeCrossHostDenied        = "cross_host_denied"
	FlockPlacementOutcomeCrossHostSpawnError    = "cross_host_spawn_error"
	FlockPlacementOutcomeCrossHostRollbackError = "cross_host_rollback_error"
	FlockPlacementOutcomeCrossHostRegistryError = "cross_host_registry_error"

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
	FlockPlacementPhasePlan          = "plan"
	FlockPlacementPhaseAgentSpawn    = "agent_spawn"
	FlockPlacementPhaseRegistrySave  = "registry_save"
	FlockPlacementPhaseRollback      = "rollback"
)

var flockPlacementLatencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

type FlockPlacementMetricObservation struct {
	Outcome   string
	Reason    string
	Latencies map[string]time.Duration
	At        time.Time
}

type FlockPlacementMetricsState struct {
	AttemptsByOutcomeReason map[string]int64                 `json:"attempts_by_outcome_reason,omitempty"`
	LatencyByPhase          map[string]LatencyHistogramState `json:"latency_by_phase,omitempty"`
	LastSuccessAt           time.Time                        `json:"last_success_at,omitempty"`
	LastFailureAt           time.Time                        `json:"last_failure_at,omitempty"`
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

	attempts := make(map[string]int64, len(state.AttemptsByOutcomeReason))
	for key, count := range state.AttemptsByOutcomeReason {
		outcome, reason := splitFlockPlacementAttemptKey(key)
		attempts[flockPlacementAttemptKey(outcome, reason)] += count
	}
	state.AttemptsByOutcomeReason = attempts

	latencies := make(map[string]LatencyHistogramState, len(state.LatencyByPhase))
	for phase, hist := range state.LatencyByPhase {
		normalizedPhase := normalizeFlockPlacementPhase(phase)
		if normalizedPhase == "" {
			continue
		}
		if hist.Buckets == nil {
			hist.Buckets = make(map[string]int64)
		}
		existing := latencies[normalizedPhase]
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
		latencies[normalizedPhase] = existing
	}
	state.LatencyByPhase = latencies
}

func cloneFlockPlacementMetricsState(state FlockPlacementMetricsState) FlockPlacementMetricsState {
	out := newFlockPlacementMetricsState()
	for key, count := range state.AttemptsByOutcomeReason {
		outcome, reason := splitFlockPlacementAttemptKey(key)
		out.AttemptsByOutcomeReason[flockPlacementAttemptKey(outcome, reason)] += count
	}
	for phase, hist := range state.LatencyByPhase {
		phase = normalizeFlockPlacementPhase(phase)
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
		out.LatencyByPhase[phase] = LatencyHistogramState{
			Buckets:    existing.Buckets,
			SumSeconds: existing.SumSeconds,
			Count:      existing.Count,
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
	defer s.mu.Unlock()
	s.ensureMaps()
	if strings.TrimSpace(s.path) != "" {
		persisted, exists, err := readPlacementStoreState(s.path)
		if err != nil {
			return err
		}
		if exists {
			s.state.FlockPlacementMetrics = cloneFlockPlacementMetricsState(persisted.FlockPlacementMetrics)
		}
	}
	previous := clonePlacementStoreState(s.state)
	metrics := &s.state.FlockPlacementMetrics
	normalizeFlockPlacementMetricsState(metrics)
	metrics.AttemptsByOutcomeReason[flockPlacementAttemptKey(outcome, reason)]++
	for phase, duration := range obs.Latencies {
		phase = normalizeFlockPlacementPhase(phase)
		if phase == "" || duration < 0 {
			continue
		}
		hist := metrics.LatencyByPhase[phase]
		recordLatencyHistogramObservation(&hist, duration)
		metrics.LatencyByPhase[phase] = hist
	}
	if outcome == FlockPlacementOutcomeSuccess {
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
	case FlockPlacementOutcomeSchedulerError:
		return FlockPlacementOutcomeSchedulerError
	case FlockPlacementOutcomeDaemonError:
		return FlockPlacementOutcomeDaemonError
	case FlockPlacementOutcomeDaemonNilResponse:
		return FlockPlacementOutcomeDaemonNilResponse
	case FlockPlacementOutcomePlacementSaveError:
		return FlockPlacementOutcomePlacementSaveError
	case FlockPlacementOutcomeCrossHostSuccess:
		return FlockPlacementOutcomeCrossHostSuccess
	case FlockPlacementOutcomeCrossHostDenied:
		return FlockPlacementOutcomeCrossHostDenied
	case FlockPlacementOutcomeCrossHostSpawnError:
		return FlockPlacementOutcomeCrossHostSpawnError
	case FlockPlacementOutcomeCrossHostRollbackError:
		return FlockPlacementOutcomeCrossHostRollbackError
	case FlockPlacementOutcomeCrossHostRegistryError:
		return FlockPlacementOutcomeCrossHostRegistryError
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
	case FlockPlacementReasonUnknown:
		return FlockPlacementReasonUnknown
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
	case FlockPlacementPhaseSchedule,
		FlockPlacementPhaseDaemonCreate,
		FlockPlacementPhasePlacementSave,
		FlockPlacementPhaseTotal,
		FlockPlacementPhasePlan,
		FlockPlacementPhaseAgentSpawn,
		FlockPlacementPhaseRegistrySave,
		FlockPlacementPhaseRollback:
		return true
	default:
		return false
	}
}

func isAllowedFlockPlacementBucket(value string) bool {
	if value == "+Inf" {
		return true
	}
	for _, upper := range flockPlacementLatencyBuckets {
		if value == formatFlockPlacementBucket(upper) {
			return true
		}
	}
	return false
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
