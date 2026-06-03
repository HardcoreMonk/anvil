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
	if contentType := rr.Header().Get("Content-Type"); contentType != schedulerMetricsContentType {
		t.Fatalf("Content-Type = %q, want %q", contentType, schedulerMetricsContentType)
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
