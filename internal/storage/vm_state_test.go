package storage

import (
	"testing"
	"time"
)

func TestVMState_SaveLoadRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	state := VMState{
		VMID:       "vm-rt",
		GuestIP:    "10.0.1.42",
		TapDevice:  "tap42",
		MacAddr:    "AA:FC:00:00:00:2A",
		VsockPath:  "/tmp/firecracker-vsock-vm-rt.sock",
		SocketPath: "/tmp/firecracker-vm-rt.sock",
		AgentToken: "deadbeef",
		DiskPath:   "/tmp/goose-workspaces/vm-rt.ext4",
		DiskMode:   DiskModePlain,
		Profile:    "researcher",
		VcpuCount:  2,
		MemSizeMib: 2048,
		FlockID:    "flock-1",
		AgentID:    "researcher-1",
		AgentURL:   "http://10.0.1.42:8080",
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
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
	if loaded.SchemaVersion != vmStateSchemaVersion {
		t.Errorf("schema version not set, got %d", loaded.SchemaVersion)
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
