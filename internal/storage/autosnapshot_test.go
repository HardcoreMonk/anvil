package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAutoSnapshotDirAndPaths(t *testing.T) {
	work := "/work"
	wantDir := filepath.Join(work, "vms", "vm-1", "auto")
	if dir := AutoSnapshotDir(work, "vm-1"); dir != wantDir {
		t.Errorf("AutoSnapshotDir = %q, want %q", dir, wantDir)
	}
	mem, stat := AutoSnapshotPaths(work, "vm-1")
	if mem != filepath.Join(wantDir, "memory.bin") {
		t.Errorf("mem path = %q", mem)
	}
	if stat != filepath.Join(wantDir, "state.bin") {
		t.Errorf("stat path = %q", stat)
	}
}

func TestAutoSnapshotExistsAndRemove(t *testing.T) {
	work := t.TempDir()
	vmID := "vm-snap"

	if AutoSnapshotExists(work, vmID) {
		t.Fatal("expected no auto-snapshot in an empty workdir")
	}

	// state.bin alone must NOT satisfy the predicate — memory.bin is the
	// load-bearing artifact a warm restore reads.
	dir := AutoSnapshotDir(work, vmID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	memPath, statPath := AutoSnapshotPaths(work, vmID)
	if err := os.WriteFile(statPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if AutoSnapshotExists(work, vmID) {
		t.Error("state.bin alone should not satisfy AutoSnapshotExists")
	}

	// memory.bin present → snapshot exists.
	if err := os.WriteFile(memPath, []byte("mem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !AutoSnapshotExists(work, vmID) {
		t.Error("expected AutoSnapshotExists true once memory.bin is present")
	}

	// RemoveAutoSnapshot clears the dir and is idempotent on a second call.
	RemoveAutoSnapshot(work, vmID)
	if AutoSnapshotExists(work, vmID) {
		t.Error("expected snapshot gone after RemoveAutoSnapshot")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected auto dir removed, stat err = %v", err)
	}
	RemoveAutoSnapshot(work, vmID) // must not panic on a missing dir
}
