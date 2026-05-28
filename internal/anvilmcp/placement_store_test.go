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

	deleted, err = store.RemoveHostAndSave("missing")
	if err != nil {
		t.Fatalf("RemoveHostAndSave missing returned error: %v", err)
	}
	if deleted {
		t.Fatal("deleted = true, want false for missing host")
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

func TestPlacementStoreApplyHostObservationPreservesConcurrentStaticHostFields(t *testing.T) {
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
	if len(host.EgressPolicies) != 1 || host.EgressPolicies[0] != EgressPolicyProfile {
		t.Fatalf("egress policies = %+v, want concurrent profile policy", host.EgressPolicies)
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
