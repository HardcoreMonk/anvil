package anvilmcp

import (
	"path/filepath"
	"testing"
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
