package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ephemera/internal/storage"
)

// TestSizingPresetRegistry_Invariants locks the three advertised tiers. The
// "standard" tier must stay in sync with the daemon's default sizing (vm package:
// defaultVcpuCount / defaultMemSizeMib = 1 / 1024) — if that default changes, change
// this expectation too.
func TestSizingPresetRegistry_Invariants(t *testing.T) {
	byID := map[string]SizingPreset{}
	for _, p := range sizingPresetRegistry {
		byID[p.ID] = p
	}
	for _, id := range []string{"light", "standard", "advanced"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("preset %q missing from registry", id)
		}
	}
	if got := byID["light"]; got.VcpuCount != 1 || got.MemSizeMib != 512 {
		t.Errorf("light = %d/%d, want 1/512", got.VcpuCount, got.MemSizeMib)
	}
	if got := byID["standard"]; got.VcpuCount != 1 || got.MemSizeMib != 1024 {
		t.Errorf("standard = %d/%d, want 1/1024 (must match the daemon default)", got.VcpuCount, got.MemSizeMib)
	}
	if got := byID["advanced"]; got.VcpuCount != 2 || got.MemSizeMib != 2048 {
		t.Errorf("advanced = %d/%d, want 2/2048", got.VcpuCount, got.MemSizeMib)
	}
	// Every preset must be writable through the profile sizing validator, or the UI
	// would offer a preset the API then rejects.
	for _, p := range sizingPresetRegistry {
		if err := validateSizing(p.VcpuCount, p.MemSizeMib); err != nil {
			t.Errorf("preset %q outside validateSizing bounds: %v", p.ID, err)
		}
	}
}

// TestHandleConfigPresets_Shape verifies GET returns the full registry (lightest
// first) and that non-GET methods are rejected.
func TestHandleConfigPresets_Shape(t *testing.T) {
	cp := newTestCP(t)

	rr := httptest.NewRecorder()
	cp.handleConfigPresets(rr, httptest.NewRequest(http.MethodGet, "/config/presets", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var list []SizingPreset
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != len(sizingPresetRegistry) {
		t.Fatalf("got %d presets, want %d", len(list), len(sizingPresetRegistry))
	}
	if list[0].ID != "light" || list[len(list)-1].ID != "advanced" {
		t.Errorf("preset order not stable (lightest first): %+v", list)
	}

	rr = httptest.NewRecorder()
	cp.handleConfigPresets(rr, httptest.NewRequest(http.MethodPost, "/config/presets", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rr.Code)
	}
}

// TestSnapshotSizing_Fallback covers the restore-time sizing resolution: legacy
// snapshots (zero fields) map to the historical 2/2048; sized snapshots keep their
// own values.
func TestSnapshotSizing_Fallback(t *testing.T) {
	if v, m := snapshotSizing(storage.SnapshotMetadata{}); v != 2 || m != 2048 {
		t.Errorf("legacy fallback = %d/%d, want 2/2048", v, m)
	}
	if v, m := snapshotSizing(storage.SnapshotMetadata{VcpuCount: 1, MemSizeMib: 512}); v != 1 || m != 512 {
		t.Errorf("sized = %d/%d, want 1/512", v, m)
	}
	// A partially-zero record falls back per field.
	if v, m := snapshotSizing(storage.SnapshotMetadata{MemSizeMib: 1024}); v != 2 || m != 1024 {
		t.Errorf("partial = %d/%d, want 2/1024", v, m)
	}
}
