package main

import (
	"net/http"

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

	// Counters / counter vecs.
	vmSpawnTotal      *metrics.CounterVec // outcome=ok|fail
	vmDestroyTotal    *metrics.CounterVec // outcome=ok|fail
	snapshotCreate    *metrics.CounterVec // type=full|diff
	snapshotRestore   *metrics.CounterVec // outcome=ok|fail
	autoSnapshot      *metrics.CounterVec // outcome=ok|fail (graceful-shutdown memory snapshot)
	autoRestore       *metrics.CounterVec // outcome=ok|fail (recovery warm-restore)
	flockSpawn        *metrics.Counter
	flockDestroy      *metrics.Counter
	watchdogDead      *metrics.Counter
	watchdogHeal      *metrics.Counter
	sighupReload      *metrics.Counter
	cpTokenPropagated *metrics.CounterVec // outcome=ok|fail
	authTotal         *metrics.CounterVec // outcome=ok|denied|expired (v0.4.1)

	// Histograms (seconds).
	vmSpawnDuration         *metrics.Histogram
	snapshotRestoreDuration *metrics.Histogram
	watchdogProbeDuration   *metrics.Histogram
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
	cp.metrics.registry.WriteTo(w)
}
