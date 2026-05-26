package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreateSnapshot_InsufficientStorage_Returns507 verifies the v0.4.0 disk
// pre-flight: when the free-space margin cannot be satisfied, createSnapshot
// answers 507 before pausing the VM (so a nil machine is never touched).
func TestCreateSnapshot_InsufficientStorage_Returns507(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.workDir = t.TempDir() // valid dir so statfs succeeds and reports real free space
	cp.vms["vm-snap"] = &runningVM{
		VMInfo:     VMInfo{VMID: "vm-snap"},
		memSizeMib: 2048,
		// machine is intentionally nil — the pre-flight returns before any use.
	}

	// Demand an impossibly large margin so the real filesystem always falls short.
	orig := diskMinFreeMiB
	diskMinFreeMiB = 1 << 40 // MiB → ~1 EiB once shifted; exceeds any real disk
	defer func() { diskMinFreeMiB = orig }()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-snap/snapshot", nil)
	cp.createSnapshot(rec, req, "vm-snap")

	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 Insufficient Storage, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestCreateSnapshot_VMNotFound_Returns404 guards the lookup path that precedes
// the pre-flight (a missing VM must still 404, not 507).
func TestCreateSnapshot_VMNotFound_Returns404(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.workDir = t.TempDir()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms/missing/snapshot", nil)
	cp.createSnapshot(rec, req, "missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
