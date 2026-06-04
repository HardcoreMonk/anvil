package anvilmcp

import (
	"fmt"
	"strconv"
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
	renderFlockPlacementMetrics(&out, state.FlockPlacementMetrics)
	return out.String()
}

func renderFlockPlacementMetrics(out *strings.Builder, state FlockPlacementMetricsState) {
	normalizeFlockPlacementMetricsState(&state)

	out.WriteString("# HELP anvil_scheduler_flock_placement_attempts_total Flock placement attempts by outcome and reason.\n")
	out.WriteString("# TYPE anvil_scheduler_flock_placement_attempts_total counter\n")
	for _, key := range sortedFlockPlacementAttemptKeys(state.AttemptsByOutcomeReason) {
		outcome, reason := splitFlockPlacementAttemptKey(key)
		fmt.Fprintf(out, "anvil_scheduler_flock_placement_attempts_total{outcome=\"%s\",reason=\"%s\"} %d\n", outcome, reason, state.AttemptsByOutcomeReason[key])
	}

	out.WriteString("# HELP anvil_scheduler_flock_placement_latency_seconds Flock placement latency by phase.\n")
	out.WriteString("# TYPE anvil_scheduler_flock_placement_latency_seconds histogram\n")
	for _, phase := range []string{
		FlockPlacementPhaseSchedule,
		FlockPlacementPhaseDaemonCreate,
		FlockPlacementPhasePlacementSave,
		FlockPlacementPhaseTotal,
	} {
		hist := state.LatencyByPhase[phase]
		for _, upper := range flockPlacementLatencyBuckets {
			bucket := formatFlockPlacementBucket(upper)
			fmt.Fprintf(out, "anvil_scheduler_flock_placement_latency_seconds_bucket{phase=\"%s\",le=\"%s\"} %d\n", phase, bucket, hist.Buckets[bucket])
		}
		fmt.Fprintf(out, "anvil_scheduler_flock_placement_latency_seconds_bucket{phase=\"%s\",le=\"+Inf\"} %d\n", phase, hist.Buckets["+Inf"])
		fmt.Fprintf(out, "anvil_scheduler_flock_placement_latency_seconds_sum{phase=\"%s\"} %s\n", phase, strconv.FormatFloat(hist.SumSeconds, 'f', -1, 64))
		fmt.Fprintf(out, "anvil_scheduler_flock_placement_latency_seconds_count{phase=\"%s\"} %d\n", phase, hist.Count)
	}

	writeSchedulerGauge(out, "anvil_scheduler_flock_placement_last_success_timestamp_seconds", "Unix timestamp of the last successful flock placement.", timestampMetric(state.LastSuccessAt))
	writeSchedulerGauge(out, "anvil_scheduler_flock_placement_last_failure_timestamp_seconds", "Unix timestamp of the last failed flock placement.", timestampMetric(state.LastFailureAt))
}

func writeSchedulerGauge(out *strings.Builder, name string, help string, value float64) {
	fmt.Fprintf(out, "# HELP %s %s\n", name, help)
	fmt.Fprintf(out, "# TYPE %s gauge\n", name)
	fmt.Fprintf(out, "%s %s\n", name, strconv.FormatFloat(value, 'f', -1, 64))
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
