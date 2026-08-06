package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVMState_SaveLoadRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	state := VMState{
		VMID:             "vm-rt",
		GuestIP:          "10.0.1.42",
		TapDevice:        "tap42",
		MacAddr:          "AA:FC:00:00:00:2A",
		VsockPath:        "/tmp/firecracker-vsock-vm-rt.sock",
		SocketPath:       "/tmp/firecracker-vm-rt.sock",
		AgentToken:       "deadbeef",
		DiskPath:         "/tmp/goose-workspaces/vm-rt.ext4",
		DiskMode:         DiskModePlain,
		Profile:          "researcher",
		TenantID:         "tenant-1",
		EgressPolicy:     "deny_all",
		SourceSnapshotID: "snap-source",
		VcpuCount:        2,
		MemSizeMib:       2048,
		FlockID:          "flock-1",
		AgentID:          "researcher-1",
		AgentURL:         "http://10.0.1.42:8080",
		CreatedAt:        time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveVMState(tmp, state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadVMState(tmp, "vm-rt")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.VMID != state.VMID || loaded.GuestIP != state.GuestIP || loaded.AgentToken != state.AgentToken {
		t.Errorf("round-trip mismatch: got %+v", loaded)
	}
	if loaded.FlockID != "flock-1" || loaded.AgentID != "researcher-1" {
		t.Errorf("flock association not preserved: %+v", loaded)
	}
	if loaded.TenantID != "tenant-1" || loaded.EgressPolicy != "deny_all" || loaded.SourceSnapshotID != "snap-source" {
		t.Errorf("anvil metadata not preserved: %+v", loaded)
	}
	if loaded.SchemaVersion != vmStateSchemaVersion {
		t.Errorf("schema version not set, got %d", loaded.SchemaVersion)
	}
}

// TestVMState_CPTokenManagedRoundTrip pins that the "this guest holds the
// daemon's own operator bearer" provenance bit survives a save/load cycle, so
// recovery after a daemon restart can keep rotating the VMs it is allowed to.
func TestVMState_CPTokenManagedRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	if err := SaveVMState(tmp, VMState{VMID: "vm-managed", DiskMode: DiskModePlain, CPTokenManaged: true}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadVMState(tmp, "vm-managed")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.CPTokenManaged {
		t.Errorf("CPTokenManaged not preserved: %+v", loaded)
	}

	if err := SaveVMState(tmp, VMState{VMID: "vm-unmanaged", DiskMode: DiskModePlain}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err = LoadVMState(tmp, "vm-unmanaged")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CPTokenManaged {
		t.Errorf("CPTokenManaged set on a VM that never had one: %+v", loaded)
	}
}

// TestVMState_LegacyStateDecodesCPTokenUnmanaged fixes the upgrade semantics: a
// state.json written before the field existed must decode as UNMANAGED, so a
// pre-existing plain VM is never handed the operator bearer by the first SIGHUP
// after the upgrade.
func TestVMState_LegacyStateDecodesCPTokenUnmanaged(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "vms", "vm-legacy")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := `{"schema_version":1,"vm_id":"vm-legacy","guest_ip":"10.0.1.9","disk_mode":"plain","flock_id":"flock-1","agent_id":"researcher-1"}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	loaded, err := LoadVMState(tmp, "vm-legacy")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CPTokenManaged {
		t.Fatalf("legacy state.json decoded as CP-token-managed; it must default to unmanaged")
	}
}

func TestVMState_SaveSetsDefaultSchemaVersion(t *testing.T) {
	tmp := t.TempDir()
	if err := SaveVMState(tmp, VMState{VMID: "vm-default", DiskMode: DiskModePlain}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadVMState(tmp, "vm-default")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.SchemaVersion != vmStateSchemaVersion {
		t.Errorf("expected schema version %d, got %d", vmStateSchemaVersion, loaded.SchemaVersion)
	}
}

func TestListVMState_SortedByVMID(t *testing.T) {
	tmp := t.TempDir()
	// Save out of order to confirm sort.
	for _, id := range []string{"vm-c", "vm-a", "vm-b"} {
		if err := SaveVMState(tmp, VMState{VMID: id, DiskMode: DiskModePlain}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := ListVMState(tmp)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(list))
	}
	if list[0].VMID != "vm-a" || list[1].VMID != "vm-b" || list[2].VMID != "vm-c" {
		t.Errorf("not sorted by VMID: %v", list)
	}
}

func TestDeleteVMState_IdempotentOnMissing(t *testing.T) {
	tmp := t.TempDir()
	if err := DeleteVMState(tmp, "never-existed"); err != nil {
		t.Errorf("delete on missing should be no-op, got %v", err)
	}
}

func TestListVMState_EmptyWorkdir(t *testing.T) {
	tmp := t.TempDir()
	list, err := ListVMState(tmp)
	if err != nil {
		t.Fatalf("List on empty: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("empty workdir should yield no entries, got %d", len(list))
	}
}

func TestVMState_COWModeRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	state := VMState{VMID: "vm-cow", DiskMode: DiskModeCOW, DiskPath: "/cow/path"}
	if err := SaveVMState(tmp, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadVMState(tmp, "vm-cow")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DiskMode != DiskModeCOW {
		t.Errorf("disk_mode not preserved: got %q", loaded.DiskMode)
	}
}
