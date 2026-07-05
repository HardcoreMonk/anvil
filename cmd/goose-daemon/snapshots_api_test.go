package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ephemera/internal/storage"
)

// These tests exercise the snapshot HTTP handlers without KVM: they only touch
// the in-memory cp.snapshots / cp.vms maps and routing, returning before any
// dm-snapshot or Firecracker work. Successful create/restore (which need a live
// VM + Firecracker) stay in the e2e gate. Uses newMetricsTestCP from
// metrics_handler_test.go (initializes vms, snapshots, and metrics).

func TestSnapshots_List_EmptyReturnsJSONArray(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	cp.handleSnapshots(rec, httptest.NewRequest(http.MethodGet, "/snapshots", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("unexpected Content-Type %q", ct)
	}
	// Must be an empty JSON array, never null — the UI relies on Array.isArray.
	if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
		t.Errorf("expected [], got %q", body)
	}
}

func TestSnapshots_List_SeededShape(t *testing.T) {
	cp := newMetricsTestCP(t)
	created := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	cp.snapshots["snap-1"] = storage.SnapshotMetadata{
		SnapshotID:   "snap-1",
		SourceVMID:   "vm-1",
		Profile:      "default",
		SnapshotType: "full",
		CreatedAt:    created,
	}
	cp.snapshots["snap-2"] = storage.SnapshotMetadata{
		SnapshotID:     "snap-2",
		SourceVMID:     "vm-1",
		SnapshotType:   "diff",
		BaseSnapshotID: "snap-1",
		CreatedAt:      created.Add(time.Minute),
	}

	rec := httptest.NewRecorder()
	cp.handleSnapshots(rec, httptest.NewRequest(http.MethodGet, "/snapshots", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var list []SnapshotInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(list))
	}
	byID := map[string]SnapshotInfo{}
	for _, s := range list {
		byID[s.SnapshotID] = s
	}
	if s := byID["snap-2"]; s.SnapshotType != "diff" || s.BaseSnapshotID != "snap-1" || s.SourceVMID != "vm-1" {
		t.Errorf("snap-2 mapping wrong: %+v", s)
	}
	if s := byID["snap-1"]; s.SnapshotType != "full" || s.Profile != "default" {
		t.Errorf("snap-1 mapping wrong: %+v", s)
	}
}

func TestSnapshots_List_RejectsNonGET(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	cp.handleSnapshots(rec, httptest.NewRequest(http.MethodPost, "/snapshots", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestSnapshots_Delete_NotFound(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	cp.handleSnapshotItem(rec, httptest.NewRequest(http.MethodDelete, "/snapshots/snap-missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestSnapshots_Delete_BaseWithDiffConflict(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.snapshots["snap-base"] = storage.SnapshotMetadata{SnapshotID: "snap-base", SnapshotType: "full"}
	cp.snapshots["snap-diff"] = storage.SnapshotMetadata{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"}

	rec := httptest.NewRecorder()
	cp.handleSnapshotItem(rec, httptest.NewRequest(http.MethodDelete, "/snapshots/snap-base", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "delete the diff first") {
		t.Errorf("expected base-dependency message, got %q", rec.Body.String())
	}
	// A refused delete must leave the base catalogued.
	if _, ok := cp.snapshots["snap-base"]; !ok {
		t.Errorf("base snapshot was removed despite 409")
	}
}

func TestSnapshots_Delete_Success(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.workDir = t.TempDir()
	cp.snapshots["snap-1"] = storage.SnapshotMetadata{SnapshotID: "snap-1", SnapshotType: "full"}

	rec := httptest.NewRecorder()
	cp.handleSnapshotItem(rec, httptest.NewRequest(http.MethodDelete, "/snapshots/snap-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"deleted"`) {
		t.Errorf("expected deleted status, got %q", rec.Body.String())
	}
	if _, ok := cp.snapshots["snap-1"]; ok {
		t.Errorf("snapshot still present after delete")
	}
}

func TestSnapshots_Restore_NotFound(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	cp.handleSnapshotItem(rec, httptest.NewRequest(http.MethodPost, "/snapshots/snap-missing/restore", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestSnapshots_Restore_SourceVMRunningConflict(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.snapshots["snap-1"] = storage.SnapshotMetadata{SnapshotID: "snap-1", SourceVMID: "vm-1"}
	cp.vms["vm-1"] = &runningVM{} // source still running → restore must be refused

	rec := httptest.NewRecorder()
	cp.handleSnapshotItem(rec, httptest.NewRequest(http.MethodPost, "/snapshots/snap-1/restore", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "still running") {
		t.Errorf("expected still-running message, got %q", rec.Body.String())
	}
}

// TestRunningVM_RootfsPath locks the disk-path selection used by createSnapshot.
// The restored-VM case is the regression guard: before the fix, createSnapshot
// reconstructed /tmp/goose-workspaces/{vmID}.ext4, which does not exist for a
// restored VM (its rootfs is the dm-snapshot device at the source VM's path), so
// snapshotting a restored VM failed with "failed to copy disk: ... no such file".
func TestRunningVM_RootfsPath(t *testing.T) {
	cases := []struct {
		name string
		vm   *runningVM
		want string
	}{
		{
			name: "plain spawn uses diskPath",
			vm:   &runningVM{diskPath: "/tmp/goose-workspaces/vm-1.ext4"},
			want: "/tmp/goose-workspaces/vm-1.ext4",
		},
		{
			name: "cow spawn uses dm-snapshot mount target",
			vm: &runningVM{
				diskPath:   "/tmp/goose-workspaces/vm-2.cow",
				dmSnapshot: &storage.DMSnapshotInfo{MountTarget: "/tmp/goose-workspaces/vm-2.ext4"},
			},
			want: "/tmp/goose-workspaces/vm-2.ext4",
		},
		{
			name: "restored cow vm keeps source disk path",
			vm: &runningVM{
				diskPath:   "/tmp/goose-workspaces/vm-restored.cow",
				dmSnapshot: &storage.DMSnapshotInfo{MountTarget: "/tmp/goose-workspaces/vm-source.ext4"},
			},
			want: "/tmp/goose-workspaces/vm-source.ext4",
		},
		{
			name: "legacy bind-mount restore uses bind target",
			vm: &runningVM{
				diskPath:        "/tmp/goose-workspaces/vm-legacy.ext4",
				bindMountTarget: "/tmp/goose-workspaces/vm-source.ext4",
			},
			want: "/tmp/goose-workspaces/vm-source.ext4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vm.rootfsPath(); got != tc.want {
				t.Errorf("rootfsPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSnapshots_ItemRouting_MethodGuards(t *testing.T) {
	cp := newMetricsTestCP(t)
	// /restore requires POST.
	rec := httptest.NewRecorder()
	cp.handleSnapshotItem(rec, httptest.NewRequest(http.MethodGet, "/snapshots/snap-1/restore", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("restore GET: expected 405, got %d", rec.Code)
	}
	// A bare item requires DELETE.
	rec = httptest.NewRecorder()
	cp.handleSnapshotItem(rec, httptest.NewRequest(http.MethodGet, "/snapshots/snap-1", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("item GET: expected 405, got %d", rec.Code)
	}
}
