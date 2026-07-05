package main

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"ephemera/internal/metrics"
)

// daemonMetrics groups every control-plane collector behind a single struct so
// the wiring stays in one place. ControlPlane carries a *daemonMetrics pointer.
//
// Gauges that derive their value from cp state are wired with NewGaugeFunc so
// they re-read the source on every /metrics scrape — keeps the collectors free
// of duplicate state and avoids stale values when callers add/remove resources.
type daemonMetrics struct {
	registry *metrics.Registry
	legacy   controlPlaneMetrics

	// Counters / counter vecs.
	vmSpawnTotal      *metrics.CounterVec // outcome=ok|fail
	vmDestroyTotal    *metrics.CounterVec // outcome=ok|fail
	snapshotCreate    *metrics.CounterVec // type=full|diff
	snapshotRestore   *metrics.CounterVec // outcome=ok|fail
	snapshotGC        *metrics.Counter
	autoSnapshot      *metrics.CounterVec // outcome=ok|fail (graceful-shutdown memory snapshot)
	autoRestore       *metrics.CounterVec // outcome=ok|fail (recovery warm-restore)
	flockSpawn        *metrics.Counter
	flockDestroy      *metrics.Counter
	watchdogDead      *metrics.Counter
	watchdogHeal      *metrics.Counter
	sighupReload      *metrics.Counter
	cleanupFailure    *metrics.Counter
	authFailure       *metrics.Counter
	cpTokenPropagated *metrics.CounterVec // outcome=ok|fail
	authTotal         *metrics.CounterVec // outcome=ok|denied|expired (v0.4.1)
	mcpToolCalls      *metrics.CounterVec // server, outcome=ok|fail|forbidden|rate_limited (v0.6.0)

	// Histograms (seconds).
	vmSpawnDuration         *metrics.Histogram
	snapshotRestoreDuration *metrics.Histogram
	watchdogProbeDuration   *metrics.Histogram

	// Legacy lifecycle gauge retained for existing Anvil scrapers.
	lifecycleQueueDepth *metrics.Gauge
}

// newDaemonMetrics registers every collector on a fresh Registry and returns
// the bundle. Gauges close over cp so they observe the live state at scrape time.
func newDaemonMetrics(cp *ControlPlane) *daemonMetrics {
	r := metrics.NewRegistry()
	m := &daemonMetrics{
		registry: r,

		vmSpawnTotal: r.NewCounterVec(
			"ephemera_vm_spawn_total",
			"Total VM spawn attempts by outcome.",
			"outcome",
		),
		vmDestroyTotal: r.NewCounterVec(
			"ephemera_vm_destroy_total",
			"Total VM destroy operations by outcome.",
			"outcome",
		),
		snapshotCreate: r.NewCounterVec(
			"ephemera_snapshot_create_total",
			"Total snapshot creations by type.",
			"type",
		),
		snapshotRestore: r.NewCounterVec(
			"ephemera_snapshot_restore_total",
			"Total snapshot restore attempts by outcome.",
			"outcome",
		),
		snapshotGC: r.NewCounter("ephemera_snapshot_gc_total", "Total snapshot GC applications."),
		autoSnapshot: r.NewCounterVec(
			"ephemera_auto_snapshot_total",
			"Total graceful-shutdown memory auto-snapshot attempts by outcome.",
			"outcome",
		),
		autoRestore: r.NewCounterVec(
			"ephemera_auto_restore_total",
			"Total recovery warm-restore attempts by outcome.",
			"outcome",
		),
		flockSpawn:   r.NewCounter("ephemera_flock_spawn_total", "Total flock create attempts."),
		flockDestroy: r.NewCounter("ephemera_flock_destroy_total", "Total flock destroy operations."),
		watchdogDead: r.NewCounter("ephemera_watchdog_dead_total", "Total agents marked dead by watchdog."),
		watchdogHeal: r.NewCounter("ephemera_watchdog_heal_total", "Total agents auto-healed by watchdog."),
		sighupReload: r.NewCounter("ephemera_sighup_reload_total", "Total SIGHUP-driven token reloads."),
		cleanupFailure: r.NewCounter(
			"ephemera_cleanup_failure_total",
			"Total cleanup failures while releasing VM resources.",
		),
		authFailure: r.NewCounter(
			"ephemera_auth_failure_total",
			"Total failed API Bearer-token authentication attempts.",
		),
		cpTokenPropagated: r.NewCounterVec(
			"ephemera_cp_token_propagated_total",
			"Total CP token propagation attempts to running VMs by outcome.",
			"outcome",
		),
		authTotal: r.NewCounterVec(
			"ephemera_auth_total",
			"Total API auth decisions by outcome.",
			"outcome",
		),
		mcpToolCalls: r.NewCounterVec(
			"ephemera_mcp_tool_calls_total",
			"Total MCP gateway tool calls by backend server and outcome.",
			"server", "outcome",
		),

		vmSpawnDuration: r.NewHistogram(
			"ephemera_vm_spawn_duration_seconds",
			"Wall-clock duration of VM spawn attempts in seconds.",
			nil,
		),
		snapshotRestoreDuration: r.NewHistogram(
			"ephemera_snapshot_restore_duration_seconds",
			"Wall-clock duration of snapshot restores in seconds.",
			nil,
		),
		watchdogProbeDuration: r.NewHistogram(
			"ephemera_watchdog_probe_duration_seconds",
			"Per-probe HTTP duration of watchdog /health checks in seconds.",
			[]float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		),
		lifecycleQueueDepth: r.NewGauge(
			"ephemera_lifecycle_queue_depth",
			"Current number of in-flight lifecycle operations.",
		),
	}

	r.NewGaugeFunc("ephemera_vm_count", "Currently registered VMs.", func() float64 {
		cp.mu.RLock()
		defer cp.mu.RUnlock()
		return float64(len(cp.vms))
	})
	r.NewGaugeFunc("ephemera_flock_count", "Currently registered flocks.", func() float64 {
		return float64(len(cp.flockMgr.List()))
	})
	r.NewGaugeFunc("ephemera_snapshot_count", "Currently catalogued snapshots.", func() float64 {
		cp.snapshotsMu.RLock()
		defer cp.snapshotsMu.RUnlock()
		return float64(len(cp.snapshots))
	})
	r.NewGaugeFunc("ephemera_api_clients_count", "Configured API clients.", func() float64 {
		return float64(len(cp.getClients()))
	})

	return m
}

func (m *daemonMetrics) IncVMCreate() {
	if m == nil {
		return
	}
	m.legacy.IncVMCreate()
}

func (m *daemonMetrics) IncVMRestore() {
	if m == nil {
		return
	}
	m.legacy.IncVMRestore()
}

func (m *daemonMetrics) IncVMDelete() {
	if m == nil {
		return
	}
	m.legacy.IncVMDelete()
}

func (m *daemonMetrics) IncSnapshotCreate() {
	if m == nil {
		return
	}
	m.legacy.IncSnapshotCreate()
}

func (m *daemonMetrics) IncSnapshotDelete() {
	if m == nil {
		return
	}
	m.legacy.IncSnapshotDelete()
}

func (m *daemonMetrics) IncSnapshotGC() {
	if m == nil {
		return
	}
	m.legacy.IncSnapshotGC()
	if m.snapshotGC != nil {
		m.snapshotGC.Inc()
	}
}

func (m *daemonMetrics) IncCleanupFailure() {
	if m == nil {
		return
	}
	m.legacy.IncCleanupFailure()
	if m.cleanupFailure != nil {
		m.cleanupFailure.Inc()
	}
}

func (m *daemonMetrics) IncAuthFailure() {
	if m == nil {
		return
	}
	m.legacy.IncAuthFailure()
	if m.authFailure != nil {
		m.authFailure.Inc()
	}
}

func (m *daemonMetrics) IncQueueDepth() {
	if m == nil {
		return
	}
	m.legacy.IncQueueDepth()
	if m.lifecycleQueueDepth != nil {
		m.lifecycleQueueDepth.Inc()
	}
}

func (m *daemonMetrics) DecQueueDepth() {
	if m == nil {
		return
	}
	m.legacy.DecQueueDepth()
	if m.lifecycleQueueDepth != nil {
		m.lifecycleQueueDepth.Dec()
	}
}

func (m *daemonMetrics) ObserveDuration(name string, duration time.Duration) {
	if m == nil {
		return
	}
	m.legacy.ObserveDuration(name, duration)
}

func (m *daemonMetrics) writeLegacy(w http.ResponseWriter) {
	if m == nil {
		return
	}
	s := m.legacy.snapshot()
	fmt.Fprintf(w, "anvil_vm_create_total %d\n", s.vmCreate)
	fmt.Fprintf(w, "anvil_vm_restore_total %d\n", s.vmRestore)
	fmt.Fprintf(w, "anvil_vm_delete_total %d\n", s.vmDelete)
	fmt.Fprintf(w, "anvil_snapshot_create_total %d\n", s.snapshotCreate)
	fmt.Fprintf(w, "anvil_snapshot_delete_total %d\n", s.snapshotDelete)
	fmt.Fprintf(w, "anvil_snapshot_gc_total %d\n", s.snapshotGC)
	fmt.Fprintf(w, "anvil_cleanup_failure_total %d\n", s.cleanupFailure)
	fmt.Fprintf(w, "anvil_auth_failure_total %d\n", s.authFailure)
	fmt.Fprintf(w, "anvil_lifecycle_queue_depth %d\n", s.queueDepth)

	durationNames := make([]string, 0, len(s.durations))
	for name := range s.durations {
		durationNames = append(durationNames, name)
	}
	sort.Strings(durationNames)
	for _, name := range durationNames {
		metric := s.durations[name]
		fmt.Fprintf(w, "anvil_%s_duration_seconds_count %d\n", name, metric.Count)
		fmt.Fprintf(w, "anvil_%s_duration_seconds_sum %.6f\n", name, metric.Sum)
	}
}

// handleMetrics renders the Prometheus exposition payload. The mux routes
// "/metrics" to this handler directly (auth optional via EPHEMERA_METRICS_REQUIRE_AUTH);
// see NewControlPlane for the mux split.
func (cp *ControlPlane) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"GET required"}`, http.StatusMethodNotAllowed)
		return
	}
	if cp.metrics == nil {
		http.Error(w, `{"error":"metrics not initialized"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := cp.metrics.registry.WriteTo(w); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"write metrics: %v"}`, err), http.StatusInternalServerError)
		return
	}
	cp.metrics.writeLegacy(w)
}
