package storage

import (
	"testing"
	"time"
)

// TestSnapshotMetadata_SizingRoundTrip ensures the per-snapshot VM sizing survives
// save/load so a restore can report the VM's true sizing via the stats API.
func TestSnapshotMetadata_SizingRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	meta := SnapshotMetadata{
		SnapshotID: "snap-rt",
		SourceVMID: "vm-rt",
		VcpuCount:  2,
		MemSizeMib: 2048,
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveMetadata(tmp, meta); err != nil {
		t.Fatalf("SaveMetadata: %v", err)
	}
	loaded, err := LoadMetadata(tmp)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if loaded.VcpuCount != 2 || loaded.MemSizeMib != 2048 {
		t.Errorf("sizing not preserved: got %d/%d, want 2/2048", loaded.VcpuCount, loaded.MemSizeMib)
	}
}

// TestSnapshotMetadata_LegacyZeroSizing documents that a snapshot written before
// sizing was recorded loads zero fields (omitempty), which the restore path maps to
// the historical 2/2048 default.
func TestSnapshotMetadata_LegacyZeroSizing(t *testing.T) {
	tmp := t.TempDir()
	if err := SaveMetadata(tmp, SnapshotMetadata{SnapshotID: "snap-legacy", SourceVMID: "vm-legacy"}); err != nil {
		t.Fatalf("SaveMetadata: %v", err)
	}
	loaded, err := LoadMetadata(tmp)
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if loaded.VcpuCount != 0 || loaded.MemSizeMib != 0 {
		t.Errorf("legacy sizing should be zero, got %d/%d", loaded.VcpuCount, loaded.MemSizeMib)
	}
}
