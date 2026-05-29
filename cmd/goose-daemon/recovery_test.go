package main

import (
	"testing"

	"ephemera/internal/network"
	"ephemera/internal/storage"
)

// TestRecoverRestoredVM_SnapshotMissing_DropsState covers the v0.4.5 recovery
// branch for snapshot-restored VMs whose source snapshot is gone (deleted while
// the VM ran): the VM is unrecoverable, so its state.json is dropped and the
// vmID is surfaced via failed[] rather than silently left behind. Needs no KVM —
// it exercises the early "snapshot missing" path before any Firecracker work.
func TestRecoverRestoredVM_SnapshotMissing_DropsState(t *testing.T) {
	cp := newTestCP(t)
	cp.netManager = network.NewManager("10.0.1.", "10.0.1.1")
	cp.snapshots = make(map[string]storage.SnapshotMetadata) // empty: source snapshot absent

	st := storage.VMState{
		VMID:             "vm-restored-1",
		GuestIP:          "10.0.1.50",
		TapDevice:        "tap-test-99",
		MacAddr:          "AA:FC:00:00:00:99",
		SocketPath:       "/tmp/none-vm-restored-1.sock",
		DiskPath:         "/tmp/none-vm-restored-1.cow",
		DiskMode:         storage.DiskModeCOW,
		AgentURL:         "http://10.0.1.50:8080",
		SourceSnapshotID: "snap-gone", // not present in cp.snapshots
	}
	if err := storage.SaveVMState(cp.workDir, st); err != nil {
		t.Fatalf("seed SaveVMState: %v", err)
	}

	recovered := 0
	var failed []string
	cp.recoverRestoredVM(st, &recovered, &failed)

	if recovered != 0 {
		t.Errorf("recovered = %d, want 0", recovered)
	}
	if len(failed) != 1 || failed[0] != "vm-restored-1" {
		t.Errorf("failed = %v, want [vm-restored-1]", failed)
	}
	if _, err := storage.LoadVMState(cp.workDir, "vm-restored-1"); err == nil {
		t.Error("expected state.json to be dropped for an unrecoverable restored VM")
	}
}
