package anvilmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSchedulerControlLoopPollTransitionsHostStatus(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/health" {
			t.Fatalf("path = %s, want /health", r.URL.Path)
		}
		if calls == 1 || calls == 2 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","available_vms":4,"available_snapshot_bytes":8192,"egress_policies":["profile"]}`))
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost: %v", err)
	}
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("first PollOnce: %v", err)
	}
	state := store.State()
	if state.HostObservations["host-a"].Status != HostStatusDegraded {
		t.Fatalf("first status = %+v, want degraded", state.HostObservations["host-a"])
	}
	if state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = true after degraded poll")
	}

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	state = store.State()
	if state.HostObservations["host-a"].Status != HostStatusUnhealthy {
		t.Fatalf("second status = %+v, want unhealthy", state.HostObservations["host-a"])
	}

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("third PollOnce: %v", err)
	}
	state = store.State()
	if state.HostObservations["host-a"].Status != HostStatusHealthy || state.Hosts["host-a"].AvailableVMs != 4 {
		t.Fatalf("third status/host = %+v host=%+v, want healthy capacity 4", state.HostObservations["host-a"], state.Hosts["host-a"])
	}
}

func TestSchedulerControlLoopPollSkipsMutationWhenParentContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("path = %s, want /health", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","available_vms":4,"available_snapshot_bytes":8192}`))
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost: %v", err)
	}
	initialObs := HostObservation{Status: HostStatusHealthy, AvailableVMs: 1}
	if err := store.SetHostObservation("host-a", initialObs); err != nil {
		t.Fatalf("SetHostObservation: %v", err)
	}
	if err := store.SetVMPlacement("vm-existing", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := loop.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	state := store.State()
	if !state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = false after canceled poll")
	}
	obs := state.HostObservations["host-a"]
	if obs != initialObs {
		t.Fatalf("observation = %+v, want unchanged %+v", obs, initialObs)
	}
	if _, ok := state.SuspectVMPlacements["vm-existing"]; ok {
		t.Fatalf("suspect placement created after canceled poll: %+v", state.SuspectVMPlacements["vm-existing"])
	}
}

func TestSchedulerControlLoopPollDoesNotResurrectHostDeletedDuringHealthRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("path = %s, want /health", r.URL.Path)
		}
		close(requestStarted)
		<-releaseResponse
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","available_vms":4,"available_snapshot_bytes":8192,"egress_policies":["profile"]}`))
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyDenyAll}}); err != nil {
		t.Fatalf("SetHost: %v", err)
	}
	if err := store.SetVMPlacement("vm-existing", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	done := make(chan error, 1)
	go func() {
		done <- loop.PollOnce(context.Background())
	}()
	<-requestStarted
	store.RemoveHost("host-a")
	close(releaseResponse)
	if err := <-done; err != nil {
		t.Fatalf("PollOnce: %v", err)
	}

	state := store.State()
	if _, ok := state.Hosts["host-a"]; ok {
		t.Fatalf("deleted host was resurrected: %+v", state.Hosts["host-a"])
	}
	if _, ok := state.HostObservations["host-a"]; ok {
		t.Fatalf("observation was created for deleted host: %+v", state.HostObservations["host-a"])
	}
	if _, ok := state.ControlLoopStatus.Hosts["host-a"]; ok {
		t.Fatalf("control loop host status was created for deleted host: %+v", state.ControlLoopStatus.Hosts["host-a"])
	}
	if _, ok := state.SuspectVMPlacements["vm-existing"]; ok {
		t.Fatalf("suspect placement created for deleted host: %+v", state.SuspectVMPlacements["vm-existing"])
	}
}

func TestSchedulerControlLoopPollDoesNotClearInventoryFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("path = %s, want /health", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","available_vms":4,"available_snapshot_bytes":8192}`))
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: false})
	_ = store.SetHostObservation("host-a", HostObservation{
		Status:       HostStatusDegraded,
		FailureCount: 1,
		LastError:    "list vms: list vms returned 503",
	})
	_ = store.SetVMPlacement("vm-existing", "host-a")
	_ = store.MarkHostPlacementsSuspect("host-a", "host_degraded")
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	state := store.State()
	if state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = true, want false until inventory reconcile succeeds")
	}
	obs := state.HostObservations["host-a"]
	if obs.Status != HostStatusDegraded || !strings.HasPrefix(obs.LastError, "list vms: ") {
		t.Fatalf("observation = %+v, want degraded inventory failure preserved", obs)
	}
	suspect := state.SuspectVMPlacements["vm-existing"]
	if suspect.Host != "host-a" || suspect.Reason != "host_degraded" {
		t.Fatalf("suspect placement = %+v, want host-a host_degraded", suspect)
	}
}

func TestSchedulerControlLoopPollPreservesInventoryFailureAcrossHealthFailureAndSuccess(t *testing.T) {
	healthCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vms":
			http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
		case "/health":
			healthCalls++
			if healthCalls == 1 {
				http.Error(w, "health unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","available_vms":4,"available_snapshot_bytes":8192}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusHealthy})
	_ = store.SetVMPlacement("vm-existing", "host-a")
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	assertInventoryFailurePreserved(t, store, HostStatusDegraded, "host_degraded")

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("health failure PollOnce: %v", err)
	}
	assertInventoryFailurePreserved(t, store, HostStatusDegraded, "host_degraded")

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("health success PollOnce: %v", err)
	}
	assertInventoryFailurePreserved(t, store, HostStatusDegraded, "host_degraded")
}

func assertInventoryFailurePreserved(t *testing.T, store *PlacementStore, wantStatus HostStatus, wantReason string) {
	t.Helper()
	state := store.State()
	obs := state.HostObservations["host-a"]
	if obs.Status != wantStatus || !strings.HasPrefix(obs.LastError, "list vms: ") {
		t.Fatalf("observation = %+v, want %s inventory failure preserved", obs, wantStatus)
	}
	if state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = true, want false until inventory reconcile succeeds")
	}
	suspect := state.SuspectVMPlacements["vm-existing"]
	if suspect.Host != "host-a" || suspect.Reason != wantReason {
		t.Fatalf("suspect placement = %+v, want host-a %s", suspect, wantReason)
	}
}

func TestSchedulerControlLoopReconcileSkipsMutationWhenParentContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms" {
			t.Fatalf("path = %s, want /vms", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]VMInfo{{VMID: "vm-live"}})
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	initialHost := RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true, AvailableVMs: 1}
	if err := store.SetHost(initialHost); err != nil {
		t.Fatalf("SetHost: %v", err)
	}
	initialObs := HostObservation{Status: HostStatusHealthy, AvailableVMs: 1}
	if err := store.SetHostObservation("host-a", initialObs); err != nil {
		t.Fatalf("SetHostObservation: %v", err)
	}
	if err := store.SetVMPlacement("vm-existing", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := loop.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	state := store.State()
	host := state.Hosts["host-a"]
	if host.Name != initialHost.Name || host.Endpoint != initialHost.Endpoint || host.Healthy != initialHost.Healthy || host.AvailableVMs != initialHost.AvailableVMs {
		t.Fatalf("host = %+v, want unchanged %+v", state.Hosts["host-a"], initialHost)
	}
	if state.HostObservations["host-a"] != initialObs {
		t.Fatalf("observation = %+v, want unchanged %+v", state.HostObservations["host-a"], initialObs)
	}
	if state.VMPlacements["vm-existing"] != "host-a" || len(state.VMPlacements) != 1 {
		t.Fatalf("placements = %+v, want only vm-existing on host-a", state.VMPlacements)
	}
	if _, ok := state.SuspectVMPlacements["vm-existing"]; ok {
		t.Fatalf("suspect placement created after canceled reconcile: %+v", state.SuspectVMPlacements["vm-existing"])
	}
}

func TestSchedulerControlLoopReconcilePreservesSuspectPlacementsForUnhealthyHost(t *testing.T) {
	healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vms":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]VMInfo{{VMID: "vm-live"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer healthyServer.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: healthyServer.URL, Healthy: true})
	_ = store.SetHost(RuntimeHost{Name: "host-b", Endpoint: "http://127.0.0.1:1", Healthy: false})
	_ = store.SetHostObservation("host-b", HostObservation{Status: HostStatusUnhealthy, FailureCount: 3})
	_ = store.SetVMPlacement("vm-stale", "host-b")
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        healthyServer.Client(),
		HostTimeout:       50 * time.Millisecond,
		FailureThreshold:  3,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	state := store.State()
	if state.VMPlacements["vm-live"] != "host-a" {
		t.Fatalf("vm-live placement = %q, want host-a", state.VMPlacements["vm-live"])
	}
	if state.VMPlacements["vm-stale"] != "host-b" {
		t.Fatalf("vm-stale placement removed, state=%+v", state.VMPlacements)
	}
	suspect := state.SuspectVMPlacements["vm-stale"]
	if suspect.Host != "host-b" || suspect.Reason != "host_unhealthy" {
		t.Fatalf("vm-stale suspect = %+v, want host-b host_unhealthy", suspect)
	}
}

func TestSchedulerControlLoopReconcilePromotesRepeatedInventoryFailures(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/vms" {
			t.Fatalf("path = %s, want /vms", r.URL.Path)
		}
		http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusHealthy})
	_ = store.SetVMPlacement("vm-existing", "host-a")
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("first ReconcileOnce: %v", err)
	}
	state := store.State()
	obs := state.HostObservations["host-a"]
	if obs.Status != HostStatusDegraded || obs.FailureCount != 1 {
		t.Fatalf("first observation = %+v, want degraded failure_count=1", obs)
	}
	if obs.LastError != "list vms: list vms returned 503" {
		t.Fatalf("first last error = %q, want inventory error marker", obs.LastError)
	}
	if state.VMPlacements["vm-existing"] != "host-a" {
		t.Fatalf("first placement = %q, want host-a", state.VMPlacements["vm-existing"])
	}
	if state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = true after first inventory failure")
	}
	suspect := state.SuspectVMPlacements["vm-existing"]
	if suspect.Host != "host-a" || suspect.Reason != "host_degraded" {
		t.Fatalf("first suspect = %+v, want host-a host_degraded", suspect)
	}

	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second ReconcileOnce: %v", err)
	}
	state = store.State()
	obs = state.HostObservations["host-a"]
	if obs.Status != HostStatusUnhealthy || obs.FailureCount != 2 {
		t.Fatalf("second observation = %+v, want unhealthy failure_count=2", obs)
	}
	if state.VMPlacements["vm-existing"] != "host-a" {
		t.Fatalf("second placement = %q, want host-a", state.VMPlacements["vm-existing"])
	}
	suspect = state.SuspectVMPlacements["vm-existing"]
	if suspect.Host != "host-a" || suspect.Reason != "host_unhealthy" {
		t.Fatalf("second suspect = %+v, want host-a host_unhealthy", suspect)
	}
	if calls != 2 {
		t.Fatalf("/vms calls = %d, want 2", calls)
	}
}

func TestSchedulerControlLoopReconcileDoesNotProbePollHealthFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/vms" {
			t.Fatalf("path = %s, want /vms", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]VMInfo{{VMID: "vm-live"}})
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true})
	_ = store.SetHostObservation("host-a", HostObservation{
		Status:       HostStatusDegraded,
		FailureCount: 1,
		LastError:    "health returned 503",
	})
	_ = store.SetVMPlacement("vm-existing", "host-a")
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	state := store.State()
	if calls != 0 {
		t.Fatalf("/vms calls = %d, want 0", calls)
	}
	if state.VMPlacements["vm-existing"] != "host-a" {
		t.Fatalf("placement = %q, want host-a", state.VMPlacements["vm-existing"])
	}
	suspect := state.SuspectVMPlacements["vm-existing"]
	if suspect.Host != "host-a" || suspect.Reason != "host_degraded" {
		t.Fatalf("suspect = %+v, want host-a host_degraded", suspect)
	}
	if state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = true, want false")
	}
}

func TestSchedulerControlLoopReconcileMarksHostUnhealthyOnSingleInventoryFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms" {
			t.Fatalf("path = %s, want /vms", r.URL.Path)
		}
		http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusHealthy})
	_ = store.SetVMPlacement("vm-existing", "host-a")
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	state := store.State()
	obs := state.HostObservations["host-a"]
	if obs.Status != HostStatusDegraded || obs.FailureCount != 1 {
		t.Fatalf("observation = %+v, want degraded failure_count=1", obs)
	}
	if state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = true after degraded reconcile inventory failure")
	}
	if state.VMPlacements["vm-existing"] != "host-a" {
		t.Fatalf("placement = %q, want host-a", state.VMPlacements["vm-existing"])
	}
	suspect := state.SuspectVMPlacements["vm-existing"]
	if suspect.Host != "host-a" || suspect.Reason != "host_degraded" {
		t.Fatalf("suspect = %+v, want host-a host_degraded", suspect)
	}
}

func TestSchedulerControlLoopReconcileClearsDegradedObservationOnInventorySuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms" {
			t.Fatalf("path = %s, want /vms", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]VMInfo{{VMID: "vm-live"}})
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: false})
	_ = store.SetHostObservation("host-a", HostObservation{
		Status:        HostStatusDegraded,
		FailureCount:  1,
		LastFailureAt: time.Now().UTC().Add(-time.Minute),
		LastError:     "list vms: list vms returned 503",
	})
	_ = store.SetVMPlacement("vm-stale", "host-a")
	_ = store.MarkHostPlacementsSuspect("host-a", "host_degraded")
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	state := store.State()
	obs := state.HostObservations["host-a"]
	if obs.Status != HostStatusHealthy || obs.FailureCount != 0 {
		t.Fatalf("observation = %+v, want healthy failure_count=0", obs)
	}
	if obs.LastError != "" {
		t.Fatalf("last error = %q, want cleared", obs.LastError)
	}
	if obs.LastSuccessAt.IsZero() {
		t.Fatalf("last success at = zero, want set")
	}
	if !state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = false after successful reconcile")
	}
	if _, ok := state.SuspectVMPlacements["vm-stale"]; ok {
		t.Fatalf("vm-stale suspect still present: %+v", state.SuspectVMPlacements["vm-stale"])
	}
	if state.VMPlacements["vm-live"] != "host-a" {
		t.Fatalf("vm-live placement = %q, want host-a", state.VMPlacements["vm-live"])
	}
	if _, ok := state.VMPlacements["vm-stale"]; ok {
		t.Fatalf("vm-stale placement still present: %+v", state.VMPlacements)
	}
}
