package anvilmcp

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPlacementStorePersistsHostsVMsAndSnapshotLocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "placements.json")
	store := NewPlacementStore(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load missing store: %v", err)
	}
	if err := store.SetHost(RuntimeHost{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}); err != nil {
		t.Fatalf("SetHost host-b: %v", err)
	}
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyDenyAll}}); err != nil {
		t.Fatalf("SetHost host-a: %v", err)
	}
	if err := store.SetVMPlacement("vm-1", "host-b"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	if err := store.SetSnapshotLocation("snap-1", "host-b"); err != nil {
		t.Fatalf("SetSnapshotLocation: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	hosts := reloaded.ListHosts()
	if len(hosts) != 2 || hosts[0].Name != "host-a" || hosts[1].Name != "host-b" {
		t.Fatalf("hosts = %+v, want deterministic host-a,host-b", hosts)
	}
	if host, ok := reloaded.VMHost("vm-1"); !ok || host != "host-b" {
		t.Fatalf("VMHost = %q,%v want host-b,true", host, ok)
	}
	locations := reloaded.SnapshotHosts("snap-1")
	if len(locations) != 1 || locations[0] != "host-b" {
		t.Fatalf("SnapshotHosts = %+v, want [host-b]", locations)
	}
}

func TestPlacementStoreRemoveHostPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "placements.json")
	store := NewPlacementStore(path)
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-a: %v", err)
	}
	if err := store.SetHost(RuntimeHost{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-b: %v", err)
	}

	deleted := store.RemoveHost("host-a")
	if !deleted {
		t.Fatal("RemoveHost(host-a) = false, want true")
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	hosts := reloaded.ListHosts()
	if len(hosts) != 1 || hosts[0].Name != "host-b" {
		t.Fatalf("hosts after remove/reload = %+v, want only host-b", hosts)
	}
}

func TestPlacementStoreReplacesVMPlacementsDuringReconciliation(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	if err := store.SetVMPlacement("stale-vm", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement stale-vm: %v", err)
	}
	if err := store.ReplaceVMPlacements(map[string]string{"live-vm": "host-b"}); err != nil {
		t.Fatalf("ReplaceVMPlacements: %v", err)
	}
	if _, ok := store.VMHost("stale-vm"); ok {
		t.Fatal("stale-vm placement still exists after reconciliation")
	}
	if host, ok := store.VMHost("live-vm"); !ok || host != "host-b" {
		t.Fatalf("live-vm placement = %q,%v want host-b,true", host, ok)
	}
}

func TestPlacementStoreReconcileVMPlacementsPreservesConcurrentChanges(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	base := map[string]string{
		"vm-keep":         "host-a",
		"vm-suspect-keep": "host-a",
		"vm-remove":       "host-a",
		"vm-move":         "host-a",
	}
	if err := store.ReplaceVMPlacements(base); err != nil {
		t.Fatalf("ReplaceVMPlacements: %v", err)
	}
	if err := store.MarkHostPlacementsSuspect("host-a", "host_degraded"); err != nil {
		t.Fatalf("MarkHostPlacementsSuspect: %v", err)
	}
	if err := store.SetVMPlacement("vm-added", "host-c"); err != nil {
		t.Fatalf("SetVMPlacement vm-added: %v", err)
	}
	store.RemoveVMPlacement("vm-remove")
	if err := store.SetVMPlacement("vm-move", "host-b"); err != nil {
		t.Fatalf("SetVMPlacement vm-move: %v", err)
	}

	err := store.ReconcileVMPlacements(base, map[string]string{
		"vm-keep":         "host-reconciled",
		"vm-suspect-keep": "host-a",
		"vm-remove":       "host-reconciled",
		"vm-move":         "host-reconciled",
		"vm-new":          "host-d",
	})
	if err != nil {
		t.Fatalf("ReconcileVMPlacements: %v", err)
	}

	state := store.State()
	if state.VMPlacements["vm-keep"] != "host-reconciled" {
		t.Fatalf("vm-keep = %q, want host-reconciled", state.VMPlacements["vm-keep"])
	}
	if state.VMPlacements["vm-suspect-keep"] != "host-a" {
		t.Fatalf("vm-suspect-keep = %q, want host-a", state.VMPlacements["vm-suspect-keep"])
	}
	if _, ok := state.VMPlacements["vm-remove"]; ok {
		t.Fatalf("vm-remove was re-added after concurrent removal: %+v", state.VMPlacements)
	}
	if state.VMPlacements["vm-move"] != "host-b" {
		t.Fatalf("vm-move = %q, want concurrent host-b", state.VMPlacements["vm-move"])
	}
	if state.VMPlacements["vm-added"] != "host-c" {
		t.Fatalf("vm-added = %q, want concurrent host-c", state.VMPlacements["vm-added"])
	}
	if state.VMPlacements["vm-new"] != "host-d" {
		t.Fatalf("vm-new = %q, want host-d", state.VMPlacements["vm-new"])
	}
	if suspect, ok := state.SuspectVMPlacements["vm-suspect-keep"]; !ok || suspect.Host != "host-a" || suspect.Reason != "host_degraded" {
		t.Fatalf("vm-suspect-keep suspect = %+v,%v want host-a host_degraded,true", suspect, ok)
	}
	if _, ok := state.SuspectVMPlacements["vm-remove"]; ok {
		t.Fatalf("vm-remove suspect still exists after concurrent removal: %+v", state.SuspectVMPlacements["vm-remove"])
	}
	if _, ok := state.SuspectVMPlacements["vm-move"]; ok {
		t.Fatalf("vm-move suspect still exists after concurrent move: %+v", state.SuspectVMPlacements["vm-move"])
	}
}

func TestPlacementStoreSetHostAndSaveRollsBackOnFailure(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://old-host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost old host-a: %v", err)
	}
	if err := store.SetHostAndSave(RuntimeHost{Name: "host-a", Endpoint: "http://new-host-a", Healthy: true, AvailableVMs: 9}); err == nil {
		t.Fatal("SetHostAndSave unexpectedly succeeded with directory path")
	}
	host, ok := store.Host("host-a")
	if !ok {
		t.Fatal("host-a missing after failed SetHostAndSave")
	}
	if host.Endpoint != "http://old-host-a" || host.AvailableVMs != 1 {
		t.Fatalf("host-a after failed SetHostAndSave = %+v, want old host", host)
	}

	if err := store.SetHostAndSave(RuntimeHost{Name: "host-new", Endpoint: "http://host-new", Healthy: true, AvailableVMs: 1}); err == nil {
		t.Fatal("SetHostAndSave new host unexpectedly succeeded with directory path")
	}
	if _, ok := store.Host("host-new"); ok {
		t.Fatal("host-new retained after failed SetHostAndSave")
	}
}

func TestPlacementStoreRemoveHostAndSaveRollsBackOnFailure(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-a: %v", err)
	}
	if err := store.MarkConfigManagedHost("host-a", true); err != nil {
		t.Fatalf("MarkConfigManagedHost: %v", err)
	}
	if err := store.SetHostObservation("host-a", HostObservation{Status: HostStatusDegraded, FailureCount: 1}); err != nil {
		t.Fatalf("SetHostObservation: %v", err)
	}
	deleted, err := store.RemoveHostAndSave("host-a")
	if err == nil {
		t.Fatal("RemoveHostAndSave unexpectedly succeeded with directory path")
	}
	if !deleted {
		t.Fatal("deleted = false, want true for existing host")
	}
	host, ok := store.Host("host-a")
	if !ok {
		t.Fatal("host-a missing after failed RemoveHostAndSave")
	}
	if host.Endpoint != "http://host-a" {
		t.Fatalf("host-a after failed RemoveHostAndSave = %+v, want original host", host)
	}
	state := store.State()
	if !state.ConfigManagedHosts["host-a"] {
		t.Fatalf("ConfigManagedHosts = %+v, want host-a restored after failed RemoveHostAndSave", state.ConfigManagedHosts)
	}
	if state.HostObservations["host-a"].FailureCount != 1 {
		t.Fatalf("HostObservations[host-a] = %+v, want restored observation", state.HostObservations["host-a"])
	}

	deleted, err = store.RemoveHostAndSave("missing")
	if err != nil {
		t.Fatalf("RemoveHostAndSave missing returned error: %v", err)
	}
	if deleted {
		t.Fatal("deleted = true, want false for missing host")
	}
}

func TestPlacementStoreRemoveHostAndSaveCleansControlLoopState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placements.json")
	store := NewPlacementStore(path)
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-a: %v", err)
	}
	if err := store.MarkConfigManagedHost("host-a", true); err != nil {
		t.Fatalf("MarkConfigManagedHost: %v", err)
	}
	if err := store.SetHostObservation("host-a", HostObservation{Status: HostStatusUnhealthy, FailureCount: 3}); err != nil {
		t.Fatalf("SetHostObservation: %v", err)
	}
	if err := store.SetVMPlacement("vm-1", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	if err := store.MarkHostPlacementsSuspect("host-a", "host_unhealthy"); err != nil {
		t.Fatalf("MarkHostPlacementsSuspect: %v", err)
	}
	if err := store.SetControlLoopStatus(ControlLoopStatus{
		Running: true,
		Hosts:   map[string]HostObservation{"host-a": {Status: HostStatusUnhealthy, FailureCount: 3}},
	}); err != nil {
		t.Fatalf("SetControlLoopStatus: %v", err)
	}

	deleted, err := store.RemoveHostAndSave("host-a")
	if err != nil {
		t.Fatalf("RemoveHostAndSave: %v", err)
	}
	if !deleted {
		t.Fatal("deleted = false, want true")
	}
	state := store.State()
	if _, ok := state.Hosts["host-a"]; ok {
		t.Fatalf("Hosts[host-a] retained after delete: %+v", state.Hosts["host-a"])
	}
	if state.ConfigManagedHosts["host-a"] {
		t.Fatalf("ConfigManagedHosts = %+v, want host-a removed", state.ConfigManagedHosts)
	}
	if _, ok := state.HostObservations["host-a"]; ok {
		t.Fatalf("HostObservations[host-a] retained after delete: %+v", state.HostObservations["host-a"])
	}
	if _, ok := state.ControlLoopStatus.Hosts["host-a"]; ok {
		t.Fatalf("ControlLoopStatus.Hosts[host-a] retained after delete: %+v", state.ControlLoopStatus.Hosts["host-a"])
	}
	if _, ok := state.SuspectVMPlacements["vm-1"]; ok {
		t.Fatalf("SuspectVMPlacements[vm-1] retained after delete: %+v", state.SuspectVMPlacements["vm-1"])
	}
	if state.VMPlacements["vm-1"] != "host-a" {
		t.Fatalf("VMPlacements[vm-1] = %q, want retained host-a placement", state.VMPlacements["vm-1"])
	}
}

func TestPlacementStoreApplyHostObservationDoesNotResurrectDeletedHost(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	base := RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}
	if err := store.SetHost(base); err != nil {
		t.Fatalf("SetHost: %v", err)
	}
	store.RemoveHost("host-a")

	applied, err := store.ApplyHostObservation(base, RuntimeHost{
		Name:                   "host-a",
		Endpoint:               "http://stale-host-a",
		Healthy:                true,
		AvailableVMs:           7,
		AvailableSnapshotBytes: 8192,
	}, HostObservation{
		Status:                 HostStatusHealthy,
		AvailableVMs:           7,
		AvailableSnapshotBytes: 8192,
	})
	if err != nil {
		t.Fatalf("ApplyHostObservation: %v", err)
	}
	if applied {
		t.Fatal("applied = true, want false for deleted host")
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
}

func TestPlacementStoreApplyHostObservationPreservesStaticFieldsAndUpdatesCapabilities(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	base := RuntimeHost{
		Name:                   "host-a",
		Endpoint:               "http://old-host-a",
		Healthy:                false,
		AvailableVMs:           1,
		AvailableSnapshotBytes: 4096,
		EgressPolicies:         []EgressPolicy{EgressPolicyDenyAll},
		SmokeOnly:              false,
	}
	if err := store.SetHost(base); err != nil {
		t.Fatalf("SetHost base: %v", err)
	}
	if err := store.SetHost(RuntimeHost{
		Name:                   "host-a",
		Endpoint:               "http://new-host-a",
		Healthy:                false,
		AvailableVMs:           2,
		AvailableSnapshotBytes: 2048,
		EgressPolicies:         []EgressPolicy{EgressPolicyProfile},
		SmokeOnly:              true,
	}); err != nil {
		t.Fatalf("SetHost concurrent update: %v", err)
	}

	obs := HostObservation{
		Status:                 HostStatusHealthy,
		AvailableVMs:           9,
		AvailableSnapshotBytes: 16384,
		LastError:              "  ",
	}
	applied, err := store.ApplyHostObservation(base, RuntimeHost{
		Name:                   "host-a",
		Endpoint:               "http://old-host-a",
		Healthy:                true,
		AvailableVMs:           9,
		AvailableSnapshotBytes: 16384,
		EgressPolicies:         []EgressPolicy{EgressPolicyAllowAll},
		SmokeOnly:              false,
	}, obs)
	if err != nil {
		t.Fatalf("ApplyHostObservation: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true for existing host")
	}
	state := store.State()
	host := state.Hosts["host-a"]
	if host.Endpoint != "http://new-host-a" {
		t.Fatalf("endpoint = %q, want concurrent value", host.Endpoint)
	}
	if len(host.EgressPolicies) != 1 || host.EgressPolicies[0] != EgressPolicyAllowAll {
		t.Fatalf("egress policies = %+v, want observed allow_all policy", host.EgressPolicies)
	}
	if !host.SmokeOnly {
		t.Fatalf("smoke_only = false, want concurrent true")
	}
	if !host.Healthy || host.AvailableVMs != 9 || host.AvailableSnapshotBytes != 16384 {
		t.Fatalf("dynamic host fields = %+v, want healthy capacity from observation", host)
	}
	storedObs := state.HostObservations["host-a"]
	if storedObs.Status != HostStatusHealthy || storedObs.AvailableVMs != 9 || storedObs.AvailableSnapshotBytes != 16384 || storedObs.LastError != "" {
		t.Fatalf("stored observation = %+v, want normalized healthy observation", storedObs)
	}
	statusObs := state.ControlLoopStatus.Hosts["host-a"]
	if statusObs != storedObs {
		t.Fatalf("control loop status observation = %+v, want %+v", statusObs, storedObs)
	}
}

func TestPlacementStorePersistsControlLoopState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "placements.json")
	store := NewPlacementStore(path)
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-a: %v", err)
	}
	if err := store.MarkConfigManagedHost("host-a", true); err != nil {
		t.Fatalf("MarkConfigManagedHost: %v", err)
	}
	now := time.Date(2026, 5, 28, 1, 2, 3, 0, time.UTC)
	if err := store.SetHostObservation("host-a", HostObservation{
		Status:                 HostStatusDegraded,
		AvailableVMs:           2,
		AvailableSnapshotBytes: 4096,
		FailureCount:           1,
		LastFailureAt:          now,
		LastError:              "health returned 503",
	}); err != nil {
		t.Fatalf("SetHostObservation: %v", err)
	}
	if err := store.SetVMPlacement("vm-1", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	if err := store.MarkHostPlacementsSuspect("host-a", "host_degraded"); err != nil {
		t.Fatalf("MarkHostPlacementsSuspect: %v", err)
	}
	statusHosts := map[string]HostObservation{
		"host-a": {
			Status:       HostStatusDegraded,
			FailureCount: 1,
			LastError:    "health returned 503",
		},
	}
	if err := store.SetControlLoopStatus(ControlLoopStatus{
		Running:                  true,
		PollIntervalSeconds:      10,
		ReconcileIntervalSeconds: 30,
		FailureThreshold:         3,
		PersistenceDegraded:      true,
		LastError:                "replace placement store: permission denied",
		Hosts:                    statusHosts,
	}); err != nil {
		t.Fatalf("SetControlLoopStatus: %v", err)
	}
	statusHosts["host-a"] = HostObservation{Status: HostStatusHealthy}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	state := reloaded.State()
	if !state.ConfigManagedHosts["host-a"] {
		t.Fatalf("ConfigManagedHosts = %+v, want host-a=true", state.ConfigManagedHosts)
	}
	obs := state.HostObservations["host-a"]
	if obs.Status != HostStatusDegraded || obs.FailureCount != 1 || obs.LastError != "health returned 503" {
		t.Fatalf("HostObservations[host-a] = %+v", obs)
	}
	suspect := state.SuspectVMPlacements["vm-1"]
	if suspect.Host != "host-a" || suspect.Reason != "host_degraded" {
		t.Fatalf("SuspectVMPlacements[vm-1] = %+v", suspect)
	}
	if !state.ControlLoopStatus.Running || !state.ControlLoopStatus.PersistenceDegraded {
		t.Fatalf("ControlLoopStatus = %+v", state.ControlLoopStatus)
	}
	statusObs := state.ControlLoopStatus.Hosts["host-a"]
	if statusObs.Status != HostStatusDegraded || statusObs.FailureCount != 1 || statusObs.LastError != "health returned 503" {
		t.Fatalf("ControlLoopStatus.Hosts[host-a] = %+v, want cloned degraded observation", statusObs)
	}
}

func TestPlacementStoreRoutedFlockRegistryPersistsAndClones(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.json")
	store := NewPlacementStore(path)
	record := RoutedFlockRecord{
		FlockID:      "routed-flock-1",
		Task:         "review changes",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{{
			AgentID:  "worker-1",
			Role:     "worker",
			VMID:     "vm-worker",
			AgentURL: "http://10.0.0.2:3000",
			Host:     "host-a",
			Status:   "running",
		}},
		CreatedAt: time.Date(2026, 6, 6, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 6, 1, 1, 0, 0, time.UTC),
	}

	if err := store.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	record.Agents[0].AgentURL = "mutated"

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := reloaded.RoutedFlock("routed-flock-1")
	if !ok {
		t.Fatal("RoutedFlock ok = false, want true")
	}
	if got.Agents[0].AgentURL != "http://10.0.0.2:3000" {
		t.Fatalf("stored agent URL = %q, want original", got.Agents[0].AgentURL)
	}
	if host, ok := reloaded.VMHost("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("vm placement = %q,%v want host-a,true", host, ok)
	}
	got.Agents[0].Host = "mutated"
	again, _ := reloaded.RoutedFlock("routed-flock-1")
	if again.Agents[0].Host != "host-a" {
		t.Fatalf("RoutedFlock returned mutable state: %+v", again.Agents[0])
	}
}

func TestPlacementStoreRoutedFlockCleanupRemovesOnlyRequestedPlacements(t *testing.T) {
	store := NewPlacementStore("")
	record := RoutedFlockRecord{
		FlockID: "routed-flock-cleanup",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-ok", Host: "host-a", Status: "running"},
			{AgentID: "worker-2", Role: "worker", VMID: "vm-failed", Host: "host-b", Status: "running"},
		},
	}
	if err := store.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	record.Status = RoutedFlockStatusFailedCleanupPending
	record.Agents = []RoutedFlockAgent{{AgentID: "worker-2", Role: "worker", VMID: "vm-failed", Host: "host-b", Status: "cleanup_pending"}}
	if err := store.SaveRoutedFlockAndPlacements(record, []string{"vm-ok"}); err != nil {
		t.Fatalf("cleanup SaveRoutedFlockAndPlacements: %v", err)
	}
	if _, ok := store.VMHost("vm-ok"); ok {
		t.Fatal("vm-ok placement still exists, want removed")
	}
	if host, ok := store.VMHost("vm-failed"); !ok || host != "host-b" {
		t.Fatalf("vm-failed placement = %q,%v want host-b,true", host, ok)
	}
}

func TestPlacementStoreRoutedFlockRegistrySurvivesStaleUnrelatedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.json")
	initial := NewPlacementStore(path)
	if err := initial.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID: "routed-flock-survive",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{{
			AgentID: "worker-1",
			Role:    "worker",
			VMID:    "vm-worker",
			Host:    "host-a",
			Status:  "running",
		}},
	}, nil); err != nil {
		t.Fatalf("initial SaveRoutedFlockAndPlacements: %v", err)
	}

	stale := NewPlacementStore(path)
	if err := stale.Load(); err != nil {
		t.Fatalf("stale Load: %v", err)
	}

	fresh := NewPlacementStore(path)
	if err := fresh.Load(); err != nil {
		t.Fatalf("fresh Load: %v", err)
	}

	if err := stale.SetHostAndSave(RuntimeHost{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("stale SetHostAndSave: %v", err)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reloaded Load: %v", err)
	}
	record, ok := reloaded.RoutedFlock("routed-flock-survive")
	if !ok {
		t.Fatal("stale unrelated save erased routed flock")
	}
	if record.Agents[0].Host != "host-a" {
		t.Fatalf("routed flock agent host = %q, want host-a", record.Agents[0].Host)
	}
	if host, ok := reloaded.VMHost("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("routed flock VM placement = %q,%v want host-a,true", host, ok)
	}
	if host, ok := reloaded.Host("host-b"); !ok || host.Endpoint != "http://host-b" {
		t.Fatalf("unrelated host = %+v,%v want persisted host-b", host, ok)
	}
}

func TestPlacementStoreRoutedFlockDeletedPlacementsSurviveStaleGenericSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.json")
	initial := NewPlacementStore(path)
	ready := RoutedFlockRecord{
		FlockID: "routed-flock-delete",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-worker-1", Host: "host-a", Status: "running"},
			{AgentID: "worker-2", Role: "worker", VMID: "vm-worker-2", Host: "host-b", Status: "running"},
		},
	}
	if err := initial.SaveRoutedFlockAndPlacements(ready, nil); err != nil {
		t.Fatalf("initial SaveRoutedFlockAndPlacements: %v", err)
	}

	stale := NewPlacementStore(path)
	if err := stale.Load(); err != nil {
		t.Fatalf("stale Load: %v", err)
	}

	fresh := NewPlacementStore(path)
	if err := fresh.Load(); err != nil {
		t.Fatalf("fresh Load: %v", err)
	}
	deleted := ready
	deleted.Status = RoutedFlockStatusDeleted
	deleted.Agents = []RoutedFlockAgent{
		{AgentID: "worker-1", Role: "worker", VMID: "vm-worker-1", Host: "host-a", Status: "deleted"},
		{AgentID: "worker-2", Role: "worker", VMID: "vm-worker-2", Host: "host-b", Status: "deleted"},
	}
	if err := fresh.SaveRoutedFlockAndPlacements(deleted, []string{"vm-worker-1", "vm-worker-2"}); err != nil {
		t.Fatalf("fresh cleanup SaveRoutedFlockAndPlacements: %v", err)
	}

	if err := stale.SetHostAndSave(RuntimeHost{Name: "host-c", Endpoint: "http://host-c", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("stale SetHostAndSave: %v", err)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reloaded Load: %v", err)
	}
	record, ok := reloaded.RoutedFlock("routed-flock-delete")
	if !ok || record.Status != RoutedFlockStatusDeleted {
		t.Fatalf("routed flock = %+v,%v want deleted record", record, ok)
	}
	for _, vmID := range []string{"vm-worker-1", "vm-worker-2"} {
		if host, ok := reloaded.VMHost(vmID); ok {
			t.Fatalf("%s placement resurrected on %q after stale generic save", vmID, host)
		}
	}
}

func TestPlacementStoreRoutedFlockPartialCleanupSurvivesStaleGenericSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.json")
	initial := NewPlacementStore(path)
	ready := RoutedFlockRecord{
		FlockID: "routed-flock-partial-cleanup",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-ok", Role: "worker", VMID: "vm-ok", Host: "host-a", Status: "running"},
			{AgentID: "worker-failed", Role: "worker", VMID: "vm-failed", Host: "host-b", Status: "running"},
		},
	}
	if err := initial.SaveRoutedFlockAndPlacements(ready, nil); err != nil {
		t.Fatalf("initial SaveRoutedFlockAndPlacements: %v", err)
	}

	stale := NewPlacementStore(path)
	if err := stale.Load(); err != nil {
		t.Fatalf("stale Load: %v", err)
	}

	fresh := NewPlacementStore(path)
	if err := fresh.Load(); err != nil {
		t.Fatalf("fresh Load: %v", err)
	}
	cleanupPending := ready
	cleanupPending.Status = RoutedFlockStatusFailedCleanupPending
	cleanupPending.Agents = []RoutedFlockAgent{{
		AgentID: "worker-failed",
		Role:    "worker",
		VMID:    "vm-failed",
		Host:    "host-b",
		Status:  "cleanup_pending",
	}}
	if err := fresh.SaveRoutedFlockAndPlacements(cleanupPending, []string{"vm-ok"}); err != nil {
		t.Fatalf("fresh cleanup SaveRoutedFlockAndPlacements: %v", err)
	}

	if err := stale.SetHostAndSave(RuntimeHost{Name: "host-c", Endpoint: "http://host-c", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("stale SetHostAndSave: %v", err)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reloaded Load: %v", err)
	}
	record, ok := reloaded.RoutedFlock("routed-flock-partial-cleanup")
	if !ok || record.Status != RoutedFlockStatusFailedCleanupPending {
		t.Fatalf("routed flock = %+v,%v want failed_cleanup_pending record", record, ok)
	}
	if host, ok := reloaded.VMHost("vm-ok"); ok {
		t.Fatalf("vm-ok placement resurrected on %q after stale generic save", host)
	}
	if host, ok := reloaded.VMHost("vm-failed"); !ok || host != "host-b" {
		t.Fatalf("vm-failed placement = %q,%v want host-b,true", host, ok)
	}
}

// TestPlacementStoreRoutedFlockTokensSurviveStaleGenericSave is the token
// counterpart to TestPlacementStoreRoutedFlockPartialCleanupSurvivesStale-
// GenericSave: mergePersistedRoutedFlocks already rebuilds state.RoutedFlocks
// entirely from disk on every generic (non-flock) save, so a stale in-memory
// RoutedFlocks copy can never clobber a concurrently-persisted flock record.
// Before the fix, RoutedFlockRelayTokens/RoutedFlockCallTokens were NOT
// merged the same way: a stale store's generic save wrote back its own
// (missing) token-map copy, wiping a concurrent writer's just-persisted
// flock token even though that flock's RECORD survived untouched.
func TestPlacementStoreRoutedFlockTokensSurviveStaleGenericSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.json")

	initial := NewPlacementStore(path)
	recX := RoutedFlockRecord{
		FlockID: "routed-flock-x",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-x", Role: "worker", VMID: "vm-x", Host: "host-a", Status: "running"}},
	}
	recX.relayToken = "relay-x-secret"
	recX.callToken = "call-x-secret"
	if err := initial.SaveRoutedFlockAndPlacements(recX, nil); err != nil {
		t.Fatalf("initial SaveRoutedFlockAndPlacements: %v", err)
	}

	// stale loads while only flock X (and its tokens) exist on disk.
	stale := NewPlacementStore(path)
	if err := stale.Load(); err != nil {
		t.Fatalf("stale Load: %v", err)
	}

	// fresh persists flock Y (record + tokens) after stale's load -- stale's
	// in-memory token maps have no entry for Y.
	fresh := NewPlacementStore(path)
	recY := RoutedFlockRecord{
		FlockID: "routed-flock-y",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-y", Role: "worker", VMID: "vm-y", Host: "host-b", Status: "running"}},
	}
	recY.relayToken = "relay-y-secret"
	recY.callToken = "call-y-secret"
	if err := fresh.SaveRoutedFlockAndPlacements(recY, nil); err != nil {
		t.Fatalf("fresh SaveRoutedFlockAndPlacements: %v", err)
	}

	// stale performs an unrelated generic save (host state), never having
	// reloaded since fresh persisted flock Y.
	if err := stale.SetHostAndSave(RuntimeHost{Name: "host-c", Endpoint: "http://host-c", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("stale SetHostAndSave: %v", err)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reloaded Load: %v", err)
	}
	if _, ok := reloaded.RoutedFlock("routed-flock-x"); !ok {
		t.Fatal("routed-flock-x record lost after stale generic save")
	}
	if _, ok := reloaded.RoutedFlock("routed-flock-y"); !ok {
		t.Fatal("routed-flock-y record lost after stale generic save")
	}
	if tok, ok := reloaded.RoutedFlockRelayToken("routed-flock-x"); !ok || tok != "relay-x-secret" {
		t.Fatalf("routed-flock-x relay token = %q,%v want relay-x-secret,true", tok, ok)
	}
	if tok, ok := reloaded.RoutedFlockCallToken("routed-flock-x"); !ok || tok != "call-x-secret" {
		t.Fatalf("routed-flock-x call token = %q,%v want call-x-secret,true", tok, ok)
	}
	if tok, ok := reloaded.RoutedFlockRelayToken("routed-flock-y"); !ok || tok != "relay-y-secret" {
		t.Fatalf("routed-flock-y relay token = %q,%v want relay-y-secret,true (lost by stale generic save)", tok, ok)
	}
	if tok, ok := reloaded.RoutedFlockCallToken("routed-flock-y"); !ok || tok != "call-y-secret" {
		t.Fatalf("routed-flock-y call token = %q,%v want call-y-secret,true (lost by stale generic save)", tok, ok)
	}
}

func TestPlacementStoreListRoutedFlocksSortsAndClones(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID: "routed-flock-b",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-b", Role: "worker", VMID: "vm-b", Host: "host-b", Status: "running"}},
	}, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements b: %v", err)
	}
	if err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID: "routed-flock-a",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-a", Role: "worker", VMID: "vm-a", Host: "host-a", Status: "running"}},
	}, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements a: %v", err)
	}

	records := store.ListRoutedFlocks()
	if len(records) != 2 {
		t.Fatalf("ListRoutedFlocks len = %d, want 2", len(records))
	}
	if records[0].FlockID != "routed-flock-a" || records[1].FlockID != "routed-flock-b" {
		t.Fatalf("ListRoutedFlocks order = %+v, want routed-flock-a,routed-flock-b", records)
	}
	records[0].Agents[0].Host = "mutated"
	again := store.ListRoutedFlocks()
	if again[0].Agents[0].Host != "host-a" {
		t.Fatalf("ListRoutedFlocks returned mutable state: %+v", again[0].Agents[0])
	}
}

func TestPlacementStoreSaveRoutedFlockAndPlacementsPreservesOtherPersistedFlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.json")
	stale := NewPlacementStore(path)
	if err := stale.Load(); err != nil {
		t.Fatalf("stale Load: %v", err)
	}

	fresh := NewPlacementStore(path)
	if err := fresh.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID: "routed-flock-other",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-other", Role: "worker", VMID: "vm-other", Host: "host-other", Status: "running"}},
	}, nil); err != nil {
		t.Fatalf("fresh SaveRoutedFlockAndPlacements other: %v", err)
	}
	if err := fresh.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID: "routed-flock-current",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-current", Role: "worker", VMID: "vm-current-old", Host: "host-old", Status: "running"}},
	}, nil); err != nil {
		t.Fatalf("fresh SaveRoutedFlockAndPlacements current: %v", err)
	}

	if err := stale.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID: "routed-flock-current",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-current", Role: "worker", VMID: "vm-current-new", Host: "host-new", Status: "running"}},
	}, nil); err != nil {
		t.Fatalf("stale SaveRoutedFlockAndPlacements current: %v", err)
	}
	if host, ok := stale.VMHost("vm-other"); !ok || host != "host-other" {
		t.Fatalf("in-memory other routed flock VM placement = %q,%v want host-other,true", host, ok)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reloaded Load: %v", err)
	}
	other, ok := reloaded.RoutedFlock("routed-flock-other")
	if !ok || other.Agents[0].Host != "host-other" {
		t.Fatalf("other routed flock = %+v,%v want host-other,true", other, ok)
	}
	if host, ok := reloaded.VMHost("vm-other"); !ok || host != "host-other" {
		t.Fatalf("other routed flock VM placement = %q,%v want host-other,true", host, ok)
	}
	current, ok := reloaded.RoutedFlock("routed-flock-current")
	if !ok || current.Agents[0].VMID != "vm-current-new" || current.Agents[0].Host != "host-new" {
		t.Fatalf("current routed flock = %+v,%v want new current record", current, ok)
	}
}

func TestPlacementStoreSaveRoutedFlockAndPlacementsPreservesExternallyPersistedFlockPlacementMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.json")
	stale := NewPlacementStore(path)
	if err := stale.Load(); err != nil {
		t.Fatalf("stale Load: %v", err)
	}

	metricsStore := NewPlacementStore(path)
	if err := metricsStore.RecordFlockPlacementMetrics(FlockPlacementMetricObservation{
		Outcome: FlockPlacementOutcomeSuccess,
		Reason:  FlockPlacementReasonScheduled,
		At:      time.Date(2026, 6, 6, 2, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordFlockPlacementMetrics: %v", err)
	}

	if err := stale.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID: "routed-flock-current",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-current", Role: "worker", VMID: "vm-current", Host: "host-current", Status: "running"}},
	}, nil); err != nil {
		t.Fatalf("stale SaveRoutedFlockAndPlacements: %v", err)
	}
	requireStoredFlockPlacementAttempt(t, stale.State().FlockPlacementMetrics, FlockPlacementOutcomeSuccess, FlockPlacementReasonScheduled, 1)

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reloaded Load: %v", err)
	}
	requireStoredFlockPlacementAttempt(t, reloaded.State().FlockPlacementMetrics, FlockPlacementOutcomeSuccess, FlockPlacementReasonScheduled, 1)
}

func TestPlacementStoreSaveRoutedFlockAndPlacementsRollsBackOnFailure(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	if err := store.SetVMPlacement("vm-old", "host-old"); err != nil {
		t.Fatalf("SetVMPlacement vm-old: %v", err)
	}

	err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID: "routed-flock-failed-save",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-new", Role: "worker", VMID: "vm-new", Host: "host-new", Status: "running"}},
	}, []string{"vm-old"})
	if err == nil {
		t.Fatal("SaveRoutedFlockAndPlacements unexpectedly succeeded with directory path")
	}
	if _, ok := store.RoutedFlock("routed-flock-failed-save"); ok {
		t.Fatal("routed flock retained after failed save")
	}
	if _, ok := store.VMHost("vm-new"); ok {
		t.Fatal("new VM placement retained after failed save")
	}
	if host, ok := store.VMHost("vm-old"); !ok || host != "host-old" {
		t.Fatalf("old VM placement = %q,%v want host-old,true", host, ok)
	}
}

// TestPlacementStoreCallTokenLifecycle mirrors the relay-token store plumbing
// (see TestPlacementStoreRoutedFlockRegistryPersistsAndClones and the
// RoutedFlockRelayToken accessor tests in routed_flock_test.go /
// runtime_router_test.go): per-flock call tokens must persist across
// token-less re-saves and disk reload, stay redacted from State(), and be
// removable on demand.
func TestPlacementStoreCallTokenLifecycle(t *testing.T) {
	dir := t.TempDir()
	store := NewPlacementStore(filepath.Join(dir, "state.json"))
	rec := RoutedFlockRecord{FlockID: "routed-1", Status: RoutedFlockStatusCreating}
	rec.callToken = "ct-secret"
	if err := store.SaveRoutedFlockAndPlacements(rec, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 영속·carrier scrub 후에도 조회 가능
	if tok, ok := store.RoutedFlockCallToken("routed-1"); !ok || tok != "ct-secret" {
		t.Fatalf("call token = %q,%v want ct-secret,true", tok, ok)
	}
	// State() redaction
	if store.State().RoutedFlockCallTokens != nil {
		t.Fatal("State() must redact RoutedFlockCallTokens")
	}
	// 빈 carrier 재저장이 기존 entry를 지우지 않음
	rec2 := RoutedFlockRecord{FlockID: "routed-1", Status: RoutedFlockStatusReady}
	if err := store.SaveRoutedFlockAndPlacements(rec2, nil); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	if tok, _ := store.RoutedFlockCallToken("routed-1"); tok != "ct-secret" {
		t.Fatalf("token lost on token-less re-save: %q", tok)
	}
	// 디스크 reload 후 생존 (clone/normalize 경로 검증)
	store2 := NewPlacementStore(filepath.Join(dir, "state.json"))
	if err := store2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if tok, ok := store2.RoutedFlockCallToken("routed-1"); !ok || tok != "ct-secret" {
		t.Fatalf("reloaded call token = %q,%v", tok, ok)
	}
	// revoke
	if err := store2.removeRoutedFlockCallToken("routed-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := store2.RoutedFlockCallToken("routed-1"); ok {
		t.Fatal("call token survived removal")
	}
}
