# Cross-host Snapshot Replication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add manual operator-triggered cross-host snapshot replication so a snapshot can be exported from one ephemera daemon host, imported into another, and recorded in scheduler snapshot locality.

**Architecture:** Snapshot artifact ownership stays inside daemon/storage. `cmd/goose-daemon` exposes authenticated export/import endpoints backed by tar bundle helpers in `internal/storage`; `internal/anvilmcp.RuntimeRouter` streams source export into target import and updates `PlacementStore.SnapshotLocations`; `cmd/anvil-mcp` exposes the operator surface as `anvil_replicate_snapshot`.

**Tech Stack:** Go standard library (`archive/tar`, `crypto/sha256`, `io`, `net/http`, `httptest`), existing ephemera daemon API, existing anvil MCP tool registration, existing scheduler `PlacementStore`.

---

## File Structure

- Create `internal/storage/snapshot_bundle.go`: bundle manifest types, export helper, import helper, checksum validation, temp staging cleanup.
- Create `internal/storage/snapshot_bundle_test.go`: storage-level bundle export/import/idempotency/conflict/dependency tests.
- Modify `cmd/goose-daemon/api.go`: register `/snapshots/import`, route `/snapshots/{id}/export`, wire handlers to storage helpers.
- Modify `cmd/goose-daemon/api_test.go`: daemon API tests for export/import, diff base protection, malformed bundle handling.
- Modify `internal/anvilmcp/daemon_client.go`: streaming export client, import client, import response type.
- Modify `internal/anvilmcp/daemon_client_test.go`: verify export stream and import request behavior.
- Create `internal/anvilmcp/snapshot_replication.go`: router-level request/response types and `RuntimeRouter.ReplicateSnapshot`.
- Modify `internal/anvilmcp/runtime_router.go`: keep existing routing behavior; only add small helpers if needed by replication.
- Modify `internal/anvilmcp/scheduler.go`: add host lookup helper used by replication validation.
- Modify `internal/anvilmcp/runtime_router_test.go`: replication orchestration tests.
- Modify `internal/anvilmcp/tools.go`: `ReplicateSnapshotInput`, `Tools.ReplicateSnapshot`, `MCPReplicateSnapshot`.
- Modify `internal/anvilmcp/tools_test.go`: MCP/tool tests for success, unsupported daemon, secret omission.
- Modify `internal/anvilmcp/ironclaw_schema.go` and tests: expose schema for `anvil_replicate_snapshot`.
- Modify `cmd/anvil-mcp/main.go` and `cmd/anvil-mcp/main_test.go`: register the new tool.
- Modify `README.md`, `RELEASE_NOTES.md`, `docs/architecture/service-logic.md`, `docs/architecture/runtime-architecture.md`, `docs/operations/runbook.md`, `docs/operations/2026-05-29-anvil-follow-up-development.md`: document operator flow and status.

## Task 1: Storage Snapshot Bundle Helpers

**Files:**
- Create: `internal/storage/snapshot_bundle.go`
- Create: `internal/storage/snapshot_bundle_test.go`
- Read: `internal/storage/snapshot.go`

- [ ] **Step 1: Write failing storage export/import tests**

Create `internal/storage/snapshot_bundle_test.go` with these tests:

```go
package storage

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSnapshotFixture(t *testing.T, workDir, snapshotID, snapshotType, baseID string) SnapshotMetadata {
	t.Helper()
	snapDir := SnapshotDir(workDir, snapshotID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatalf("MkdirAll snapshot dir: %v", err)
	}
	for name, body := range map[string]string{
		"memory.bin": "memory-" + snapshotID,
		"state.bin":  "state-" + snapshotID,
		"rootfs.ext4": "disk-" + snapshotID,
	} {
		if err := os.WriteFile(filepath.Join(snapDir, name), []byte(body), 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	meta := SnapshotMetadata{
		SnapshotID:     snapshotID,
		SourceVMID:     "vm-1",
		TenantID:       "tenant-1",
		Profile:        "dev",
		EgressPolicy:   "profile",
		SnapshotType:   snapshotType,
		BaseSnapshotID: baseID,
		GuestIP:        "10.0.1.10",
		TapDevice:      "tap1",
		VsockPath:      "/tmp/vsock-" + snapshotID + ".sock",
		MacAddr:        "AA:FC:00:00:00:01",
		AgentToken:     "secret-token",
		DiskPath:       "/tmp/goose-workspaces/vm-1.ext4",
		MemFilePath:    filepath.Join(snapDir, "memory.bin"),
		StatFilePath:   filepath.Join(snapDir, "state.bin"),
		DiskCopyPath:   filepath.Join(snapDir, "rootfs.ext4"),
		CreatedAt:      time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
	}
	if err := SaveMetadata(snapDir, meta); err != nil {
		t.Fatalf("SaveMetadata: %v", err)
	}
	return meta
}

func TestExportSnapshotBundleContainsManifestAndArtifacts(t *testing.T) {
	workDir := t.TempDir()
	writeSnapshotFixture(t, workDir, "snap-1", "full", "")
	var buf bytes.Buffer
	manifest, err := ExportSnapshotBundle(workDir, "snap-1", &buf)
	if err != nil {
		t.Fatalf("ExportSnapshotBundle returned error: %v", err)
	}
	if manifest.SnapshotID != "snap-1" || manifest.SnapshotType != "full" {
		t.Fatalf("manifest identity = %q/%q, want snap-1/full", manifest.SnapshotID, manifest.SnapshotType)
	}
	seen := map[string]bool{}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		seen[hdr.Name] = true
	}
	for _, name := range []string{"manifest.json", "metadata.json", "memory.bin", "state.bin", "rootfs.ext4"} {
		if !seen[name] {
			t.Fatalf("tar entry %q missing; seen=%v", name, seen)
		}
	}
}

func TestImportSnapshotBundleRebasesPathsAndPublishesAtomically(t *testing.T) {
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	writeSnapshotFixture(t, sourceDir, "snap-1", "full", "")
	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceDir, "snap-1", &buf); err != nil {
		t.Fatalf("ExportSnapshotBundle: %v", err)
	}
	result, err := ImportSnapshotBundle(targetDir, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshotBundle returned error: %v", err)
	}
	if result.Status != SnapshotImportStatusImported {
		t.Fatalf("status = %q, want %q", result.Status, SnapshotImportStatusImported)
	}
	meta, err := LoadMetadata(SnapshotDir(targetDir, "snap-1"))
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.MemFilePath != filepath.Join(SnapshotDir(targetDir, "snap-1"), "memory.bin") {
		t.Fatalf("MemFilePath = %q, want target path", meta.MemFilePath)
	}
	if _, err := os.Stat(filepath.Join(SnapshotDir(targetDir, "snap-1"), "manifest.json")); err != nil {
		t.Fatalf("manifest not published: %v", err)
	}
}

func TestImportSnapshotBundleRejectsDiffWithoutBaseAndCleansTemp(t *testing.T) {
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	writeSnapshotFixture(t, sourceDir, "snap-diff", "diff", "snap-base")
	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceDir, "snap-diff", &buf); err != nil {
		t.Fatalf("ExportSnapshotBundle: %v", err)
	}
	_, err := ImportSnapshotBundle(targetDir, bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrDiffBaseMissing) {
		t.Fatalf("error = %v, want ErrDiffBaseMissing", err)
	}
	matches, globErr := filepath.Glob(filepath.Join(targetDir, "snapshots", ".import-*"))
	if globErr != nil {
		t.Fatalf("Glob: %v", globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary imports left behind: %v", matches)
	}
}

func TestImportSnapshotBundleIdempotentAndConflict(t *testing.T) {
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	writeSnapshotFixture(t, sourceDir, "snap-1", "full", "")
	var first bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceDir, "snap-1", &first); err != nil {
		t.Fatalf("ExportSnapshotBundle first: %v", err)
	}
	if _, err := ImportSnapshotBundle(targetDir, bytes.NewReader(first.Bytes())); err != nil {
		t.Fatalf("ImportSnapshotBundle first: %v", err)
	}
	var second bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceDir, "snap-1", &second); err != nil {
		t.Fatalf("ExportSnapshotBundle second: %v", err)
	}
	result, err := ImportSnapshotBundle(targetDir, bytes.NewReader(second.Bytes()))
	if err != nil {
		t.Fatalf("ImportSnapshotBundle identical returned error: %v", err)
	}
	if result.Status != SnapshotImportStatusAlreadyPresent {
		t.Fatalf("status = %q, want %q", result.Status, SnapshotImportStatusAlreadyPresent)
	}
	if err := os.WriteFile(filepath.Join(SnapshotDir(targetDir, "snap-1"), "rootfs.ext4"), []byte("different"), 0600); err != nil {
		t.Fatalf("mutate target rootfs: %v", err)
	}
	_, err = ImportSnapshotBundle(targetDir, bytes.NewReader(second.Bytes()))
	if !errors.Is(err, ErrSnapshotBundleConflict) {
		t.Fatalf("error = %v, want ErrSnapshotBundleConflict", err)
	}
}

func TestSnapshotExportManifestDoesNotExposeAgentToken(t *testing.T) {
	workDir := t.TempDir()
	writeSnapshotFixture(t, workDir, "snap-1", "full", "")
	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(workDir, "snap-1", &buf); err != nil {
		t.Fatalf("ExportSnapshotBundle: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("manifest.json not found")
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Name != "manifest.json" {
			continue
		}
		var manifest SnapshotExportManifest
		if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		raw, _ := json.Marshal(manifest)
		if bytes.Contains(raw, []byte("secret-token")) {
			t.Fatalf("manifest exposes agent token: %s", raw)
		}
		return
	}
}
```

- [ ] **Step 2: Run storage tests and verify they fail**

Run:

```bash
go test ./internal/storage -run 'Test(ExportSnapshotBundle|ImportSnapshotBundle|SnapshotExportManifest)' -count=1
```

Expected: FAIL to compile with undefined `ExportSnapshotBundle`, `ImportSnapshotBundle`, `SnapshotExportManifest`, `SnapshotImportStatusImported`, `ErrDiffBaseMissing`, and `ErrSnapshotBundleConflict`.

- [ ] **Step 3: Implement bundle helpers**

Create `internal/storage/snapshot_bundle.go` with these public contracts and implementation responsibilities:

```go
package storage

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const SnapshotBundleContentType = "application/vnd.anvil.snapshot-bundle"

const (
	SnapshotImportStatusImported       = "imported"
	SnapshotImportStatusAlreadyPresent = "already_present"
)

var (
	ErrSnapshotBundleInvalid  = errors.New("invalid snapshot bundle")
	ErrSnapshotBundleConflict = errors.New("snapshot bundle conflicts with existing snapshot")
	ErrDiffBaseMissing        = errors.New("diff base snapshot missing")
)

type SnapshotBundleFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type SnapshotExportManifest struct {
	SnapshotID     string               `json:"snapshot_id"`
	SourceVMID     string               `json:"source_vm_id"`
	TenantID       string               `json:"tenant_id,omitempty"`
	Profile        string               `json:"profile,omitempty"`
	EgressPolicy   string               `json:"egress_policy,omitempty"`
	SnapshotType   string               `json:"snapshot_type"`
	BaseSnapshotID string               `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	Files          []SnapshotBundleFile `json:"files"`
}

type SnapshotImportResult struct {
	SnapshotID     string           `json:"snapshot_id"`
	SnapshotType   string           `json:"snapshot_type"`
	BaseSnapshotID string           `json:"base_snapshot_id,omitempty"`
	Status         string           `json:"status"`
	Skipped        bool             `json:"skipped"`
	Manifest       SnapshotExportManifest `json:"-"`
	Metadata       SnapshotMetadata `json:"-"`
}
```

Implementation requirements:

- `ExportSnapshotBundle(workDir, snapshotID string, dst io.Writer) (SnapshotExportManifest, error)` loads metadata from `SnapshotDir(workDir, snapshotID)`, writes `manifest.json`, `metadata.json`, `memory.bin`, `state.bin`, and `rootfs.ext4` into a tar stream, and returns the public manifest.
- The manifest must not include `AgentToken`, `DiskPath`, `MemFilePath`, `StatFilePath`, `DiskCopyPath`, `VsockPath`, `GuestIP`, `TapDevice`, or `MacAddr`.
- The manifest `Files` list must include checksums for `metadata.json`, `memory.bin`, `state.bin`, and `rootfs.ext4`; sort entries by path for deterministic output.
- `ImportSnapshotBundle(workDir string, src io.Reader) (SnapshotImportResult, error)` reads the tar stream into a temp directory under `filepath.Join(workDir, "snapshots")`.
- Reject tar entries with empty names, absolute paths, `..`, directories, symlinks, hardlinks, or paths outside the five allowed file names.
- Decode `manifest.json` and `metadata.json`; require matching `snapshot_id`, `snapshot_type`, `base_snapshot_id`, and `created_at`.
- If importing a diff snapshot, require `SnapshotDir(workDir, meta.BaseSnapshotID)` to exist before publish.
- Rebase `meta.MemFilePath`, `meta.StatFilePath`, and `meta.DiskCopyPath` to the target snapshot directory before calling `SaveMetadata`.
- Leave `meta.DiskPath`, `meta.VsockPath`, `meta.TapDevice`, and `meta.MacAddr` unchanged because Firecracker snapshot state depends on those original values.
- Store the incoming `manifest.json` in the final snapshot directory for idempotency checks.
- Existing snapshot idempotency checks compare public manifest identity and artifact checksums. Do not compare raw `metadata.json` after import because target import rewrites local path fields.
- Use `os.Rename(stageDir, finalDir)` for publish. On every error before publish, remove the stage directory.

- [ ] **Step 4: Run storage tests and verify they pass**

Run:

```bash
go test ./internal/storage -run 'Test(ExportSnapshotBundle|ImportSnapshotBundle|SnapshotExportManifest)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Run full storage package**

Run:

```bash
go test ./internal/storage -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit storage bundle helpers**

```bash
git add internal/storage/snapshot_bundle.go internal/storage/snapshot_bundle_test.go
git commit -m "feat: add snapshot bundle import export"
```

## Task 2: Daemon Snapshot Export/Import API

**Files:**
- Modify: `cmd/goose-daemon/api.go`
- Modify: `cmd/goose-daemon/api_test.go`
- Uses: `internal/storage/snapshot_bundle.go`

- [ ] **Step 1: Write failing daemon API tests**

Append tests to `cmd/goose-daemon/api_test.go`:

```go
func TestHandleSnapshotExportStreamsBundle(t *testing.T) {
	cp := newTestControlPlane(t)
	meta := addSnapshotBundleFixture(t, cp, "snap-1", "full", "")
	req := httptest.NewRequest(http.MethodPost, "/snapshots/snap-1/export", nil)
	rr := httptest.NewRecorder()
	cp.handleSnapshotItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != storage.SnapshotBundleContentType {
		t.Fatalf("content-type = %q, want %q", ct, storage.SnapshotBundleContentType)
	}
	imported, err := storage.ImportSnapshotBundle(t.TempDir(), bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("import exported bundle: %v", err)
	}
	if imported.SnapshotID != meta.SnapshotID {
		t.Fatalf("snapshot_id = %q, want %q", imported.SnapshotID, meta.SnapshotID)
	}
}

func TestHandleSnapshotImportPublishesSnapshot(t *testing.T) {
	source := t.TempDir()
	writeStorageSnapshotBundleFixture(t, source, "snap-1", "full", "")
	var bundle bytes.Buffer
	if _, err := storage.ExportSnapshotBundle(source, "snap-1", &bundle); err != nil {
		t.Fatalf("ExportSnapshotBundle: %v", err)
	}
	cp := newTestControlPlane(t)
	req := httptest.NewRequest(http.MethodPost, "/snapshots/import", bytes.NewReader(bundle.Bytes()))
	req.Header.Set("Content-Type", storage.SnapshotBundleContentType)
	rr := httptest.NewRecorder()
	cp.handleSnapshotImport(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s, want 201", rr.Code, rr.Body.String())
	}
	cp.snapshotsMu.RLock()
	_, ok := cp.snapshots["snap-1"]
	cp.snapshotsMu.RUnlock()
	if !ok {
		t.Fatal("imported snapshot not added to cp.snapshots")
	}
}

func TestHandleSnapshotImportRejectsDiffWithoutBase(t *testing.T) {
	source := t.TempDir()
	writeStorageSnapshotBundleFixture(t, source, "snap-diff", "diff", "snap-base")
	var bundle bytes.Buffer
	if _, err := storage.ExportSnapshotBundle(source, "snap-diff", &bundle); err != nil {
		t.Fatalf("ExportSnapshotBundle: %v", err)
	}
	cp := newTestControlPlane(t)
	req := httptest.NewRequest(http.MethodPost, "/snapshots/import", bytes.NewReader(bundle.Bytes()))
	req.Header.Set("Content-Type", storage.SnapshotBundleContentType)
	rr := httptest.NewRecorder()
	cp.handleSnapshotImport(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s, want 409", rr.Code, rr.Body.String())
	}
	cp.snapshotsMu.RLock()
	_, ok := cp.snapshots["snap-diff"]
	cp.snapshotsMu.RUnlock()
	if ok {
		t.Fatal("diff snapshot added despite missing base")
	}
}

func TestImportedDiffProtectsBaseSnapshotDelete(t *testing.T) {
	source := t.TempDir()
	writeStorageSnapshotBundleFixture(t, source, "snap-base", "full", "")
	writeStorageSnapshotBundleFixture(t, source, "snap-diff", "diff", "snap-base")
	cp := newTestControlPlane(t)
	for _, snapshotID := range []string{"snap-base", "snap-diff"} {
		var bundle bytes.Buffer
		if _, err := storage.ExportSnapshotBundle(source, snapshotID, &bundle); err != nil {
			t.Fatalf("ExportSnapshotBundle %s: %v", snapshotID, err)
		}
		req := httptest.NewRequest(http.MethodPost, "/snapshots/import", bytes.NewReader(bundle.Bytes()))
		req.Header.Set("Content-Type", storage.SnapshotBundleContentType)
		rr := httptest.NewRecorder()
		cp.handleSnapshotImport(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("import %s status = %d body=%s, want 201", snapshotID, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	cp.deleteSnapshot(rr, "snap-base")
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete base status = %d body=%s, want 409", rr.Code, rr.Body.String())
	}
}
```

Add test helpers in the same file. Use the storage helper body from Task 1, but return both `storage.SnapshotMetadata` and write files into `storage.SnapshotDir(workDir, snapshotID)`.

- [ ] **Step 2: Run daemon tests and verify they fail**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestHandleSnapshot(Export|Import)|TestImportedDiffProtectsBaseSnapshotDelete' -count=1
```

Expected: FAIL to compile with undefined `handleSnapshotImport` and missing export routing.

- [ ] **Step 3: Register daemon routes**

In `NewControlPlane`, add the exact route before `/snapshots/`:

```go
internalMux.HandleFunc("/snapshots", cp.handleSnapshots)
internalMux.HandleFunc("/snapshots/gc", cp.handleSnapshotGC)
internalMux.HandleFunc("/snapshots/import", cp.handleSnapshotImport)
internalMux.HandleFunc("/snapshots/", cp.handleSnapshotItem)
```

In `handleSnapshotItem`, route export before delete:

```go
if strings.HasSuffix(path, "/export") {
	snapID := strings.TrimSuffix(path, "/export")
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	cp.handleSnapshotExport(w, r, snapID)
	return
}
```

- [ ] **Step 4: Implement daemon handlers**

Add handlers near the existing snapshot handlers:

```go
func (cp *ControlPlane) handleSnapshotExport(w http.ResponseWriter, r *http.Request, snapID string) {
	defer cp.observeLifecycle("snapshot_export")()
	snapID = strings.TrimSpace(snapID)
	if snapID == "" {
		writeJSONError(w, http.StatusBadRequest, "snapshot_id is required")
		return
	}
	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()
	cp.snapshotsMu.RLock()
	_, ok := cp.snapshots[snapID]
	cp.snapshotsMu.RUnlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, "snapshot_not_found")
		return
	}
	w.Header().Set("Content-Type", storage.SnapshotBundleContentType)
	if _, err := storage.ExportSnapshotBundle(cp.workDir, snapID, w); err != nil {
		slog.Warn("snapshot export failed", "snapshot_id", snapID, "err", err)
	}
}

func (cp *ControlPlane) handleSnapshotImport(w http.ResponseWriter, r *http.Request) {
	defer cp.observeLifecycle("snapshot_import")()
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]); ct != storage.SnapshotBundleContentType {
		writeJSONError(w, http.StatusBadRequest, "invalid snapshot bundle content type")
		return
	}
	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()
	result, err := storage.ImportSnapshotBundle(cp.workDir, r.Body)
	if err != nil {
		status := http.StatusInternalServerError
		msg := "snapshot_import_failed"
		switch {
		case errors.Is(err, storage.ErrSnapshotBundleInvalid):
			status = http.StatusBadRequest
			msg = "invalid_snapshot_bundle"
		case errors.Is(err, storage.ErrDiffBaseMissing):
			status = http.StatusConflict
			msg = "diff_base_missing"
		case errors.Is(err, storage.ErrSnapshotBundleConflict):
			status = http.StatusConflict
			msg = "snapshot_conflict"
		}
		writeJSONError(w, status, msg)
		return
	}
	cp.snapshotsMu.Lock()
	cp.snapshots[result.SnapshotID] = result.Metadata
	cp.snapshotsMu.Unlock()
	status := http.StatusCreated
	if result.Status == storage.SnapshotImportStatusAlreadyPresent {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}
```

If `cmd/goose-daemon/api.go` does not already import `errors`, add it.

- [ ] **Step 5: Run daemon focused tests**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestHandleSnapshot(Export|Import)|TestImportedDiffProtectsBaseSnapshotDelete' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run daemon package tests**

Run:

```bash
go test ./cmd/goose-daemon -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit daemon API**

```bash
git add cmd/goose-daemon/api.go cmd/goose-daemon/api_test.go
git commit -m "feat: expose snapshot replication bundle api"
```

## Task 3: Daemon Client Streaming Methods

**Files:**
- Modify: `internal/anvilmcp/daemon_client.go`
- Modify: `internal/anvilmcp/daemon_client_test.go`

- [ ] **Step 1: Write failing client tests**

Add tests to `internal/anvilmcp/daemon_client_test.go`:

```go
func TestDaemonClientExportSnapshotReturnsStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/snapshots/snap-1/export" {
			t.Fatalf("request = %s %s, want POST /snapshots/snap-1/export", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", storage.SnapshotBundleContentType)
		_, _ = w.Write([]byte("bundle-bytes"))
	}))
	defer server.Close()
	client := NewDaemonClient(Config{DaemonURL: server.URL, APIToken: "token-1"}, server.Client())
	stream, err := client.ExportSnapshot(context.Background(), "snap-1")
	if err != nil {
		t.Fatalf("ExportSnapshot returned error: %v", err)
	}
	defer stream.Body.Close()
	body, err := io.ReadAll(stream.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "bundle-bytes" {
		t.Fatalf("body = %q, want bundle-bytes", body)
	}
}

func TestDaemonClientImportSnapshotSendsBundleContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/snapshots/import" {
			t.Fatalf("request = %s %s, want POST /snapshots/import", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != storage.SnapshotBundleContentType {
			t.Fatalf("Content-Type = %q, want %q", got, storage.SnapshotBundleContentType)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request: %v", err)
		}
		if string(body) != "bundle-bytes" {
			t.Fatalf("request body = %q, want bundle-bytes", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"snapshot_id":"snap-1","snapshot_type":"full","status":"imported","skipped":false}`))
	}))
	defer server.Close()
	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	resp, err := client.ImportSnapshot(context.Background(), strings.NewReader("bundle-bytes"))
	if err != nil {
		t.Fatalf("ImportSnapshot returned error: %v", err)
	}
	if resp.SnapshotID != "snap-1" || resp.Status != "imported" {
		t.Fatalf("response = %+v, want snap-1/imported", resp)
	}
}
```

- [ ] **Step 2: Run client tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestDaemonClient(ExportSnapshot|ImportSnapshot)' -count=1
```

Expected: FAIL to compile with undefined `ExportSnapshot`, `ImportSnapshot`, and `SnapshotImportResponse`.

- [ ] **Step 3: Implement client stream contracts**

In `internal/anvilmcp/daemon_client.go`, import `io` is already present. Add:

```go
type SnapshotExportStream struct {
	Body        io.ReadCloser
	ContentType string
}

type SnapshotImportResponse struct {
	SnapshotID     string `json:"snapshot_id"`
	SnapshotType   string `json:"snapshot_type"`
	BaseSnapshotID string `json:"base_snapshot_id,omitempty"`
	Status         string `json:"status"`
	Skipped        bool   `json:"skipped"`
}
```

Add methods:

```go
func (c *DaemonClient) ExportSnapshot(ctx context.Context, snapshotID string) (*SnapshotExportStream, error) {
	snapshotID = strings.TrimSpace(snapshotID)
	if snapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/snapshots/"+url.PathEscape(snapshotID)+"/export", nil)
	if err != nil {
		return nil, fmt.Errorf("create daemon export request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send daemon export request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read daemon export error response: %w", readErr)
		}
		return nil, &DaemonError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	return &SnapshotExportStream{Body: resp.Body, ContentType: resp.Header.Get("Content-Type")}, nil
}

func (c *DaemonClient) ImportSnapshot(ctx context.Context, body io.Reader) (*SnapshotImportResponse, error) {
	_, responseBody, err := c.doRaw(ctx, http.MethodPost, "/snapshots/import", body, storage.SnapshotBundleContentType)
	if err != nil {
		return nil, err
	}
	var resp SnapshotImportResponse
	if err := json.Unmarshal([]byte(responseBody), &resp); err != nil {
		return nil, fmt.Errorf("decode import snapshot response: %w", err)
	}
	return &resp, nil
}
```

Add `ephemera/internal/storage` to imports.

- [ ] **Step 4: Run client tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestDaemonClient(ExportSnapshot|ImportSnapshot)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit client methods**

```bash
git add internal/anvilmcp/daemon_client.go internal/anvilmcp/daemon_client_test.go
git commit -m "feat: add snapshot transfer daemon client"
```

## Task 4: Runtime Router Replication Orchestration

**Files:**
- Create: `internal/anvilmcp/snapshot_replication.go`
- Modify: `internal/anvilmcp/scheduler.go`
- Modify: `internal/anvilmcp/runtime_router_test.go`

- [ ] **Step 1: Extend router fake daemon for transfer calls**

In `internal/anvilmcp/runtime_router_test.go`, add fields to `routerFakeDaemon`:

```go
snapshotList       []SnapshotInfo
exportCalls        []string
exportBodies       map[string]string
exportErr          error
importCalls        []string
importErrForBody   map[string]error
importStatusForBody map[string]string
```

Add methods:

```go
func (f *routerFakeDaemon) ExportSnapshot(_ context.Context, snapshotID string) (*SnapshotExportStream, error) {
	f.exportCalls = append(f.exportCalls, snapshotID)
	if f.exportErr != nil {
		return nil, f.exportErr
	}
	body := "bundle:" + snapshotID
	if f.exportBodies != nil {
		if configured, ok := f.exportBodies[snapshotID]; ok {
			body = configured
		}
	}
	return &SnapshotExportStream{Body: io.NopCloser(strings.NewReader(body)), ContentType: "application/vnd.anvil.snapshot-bundle"}, nil
}

func (f *routerFakeDaemon) ImportSnapshot(_ context.Context, body io.Reader) (*SnapshotImportResponse, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	text := string(data)
	f.importCalls = append(f.importCalls, text)
	if f.importErrForBody != nil {
		if err, ok := f.importErrForBody[text]; ok {
			return nil, err
		}
	}
	status := "imported"
	if f.importStatusForBody != nil {
		if configured, ok := f.importStatusForBody[text]; ok {
			status = configured
		}
	}
	snapshotID := strings.TrimPrefix(text, "bundle:")
	return &SnapshotImportResponse{SnapshotID: snapshotID, SnapshotType: "full", Status: status, Skipped: status == "already_present"}, nil
}
```

Update `ListSnapshots` fake method:

```go
func (f *routerFakeDaemon) ListSnapshots(context.Context) ([]SnapshotInfo, error) {
	return append([]SnapshotInfo(nil), f.snapshotList...), nil
}
```

- [ ] **Step 2: Write failing router replication tests**

Append tests to `internal/anvilmcp/runtime_router_test.go`:

```go
func TestRuntimeRouterReplicateSnapshotRecordsTargetLocation(t *testing.T) {
	store := NewPlacementStore("")
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler([]RuntimeHost{
			{Name: "host-a", Healthy: true, AvailableSnapshotBytes: 1 << 30},
			{Name: "host-b", Healthy: true, AvailableSnapshotBytes: 1 << 30},
		}, nil, nil),
		map[string]Daemon{"host-a": source, "host-b": target},
		RuntimeRouterOptions{PlacementStore: store},
	)
	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID: "snap-1", SourceHost: "host-a", TargetHost: "host-b", IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "replicated" {
		t.Fatalf("status = %q, want replicated", resp.Status)
	}
	if got := strings.Join(target.importCalls, ","); got != "bundle:snap-1" {
		t.Fatalf("target imports = %q, want bundle:snap-1", got)
	}
	if hosts := store.SnapshotHosts("snap-1"); strings.Join(hosts, ",") != "host-b" {
		t.Fatalf("snapshot hosts = %v, want host-b", hosts)
	}
}

func TestRuntimeRouterReplicateDiffIncludesBaseFirst(t *testing.T) {
	store := NewPlacementStore("")
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
		{SnapshotID: "snap-base", SnapshotType: "full"},
		{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
	}}
	target := &routerFakeDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler([]RuntimeHost{
			{Name: "host-a", Healthy: true, AvailableSnapshotBytes: 1 << 30},
			{Name: "host-b", Healthy: true, AvailableSnapshotBytes: 1 << 30},
		}, nil, nil),
		map[string]Daemon{"host-a": source, "host-b": target},
		RuntimeRouterOptions{PlacementStore: store},
	)
	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID: "snap-diff", SourceHost: "host-a", TargetHost: "host-b", IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if got := strings.Join(source.exportCalls, ","); got != "snap-base,snap-diff" {
		t.Fatalf("export calls = %q, want snap-base,snap-diff", got)
	}
	if got := strings.Join(resp.Replicated, ","); got != "snap-base,snap-diff" {
		t.Fatalf("replicated = %q, want snap-base,snap-diff", got)
	}
}

func TestRuntimeRouterReplicateDiffWithoutDependencyFails(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"}}}
	target := &routerFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler([]RuntimeHost{{Name: "host-a", Healthy: true}, {Name: "host-b", Healthy: true}}, nil, nil),
		map[string]Daemon{"host-a": source, "host-b": target},
	)
	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID: "snap-diff", SourceHost: "host-a", TargetHost: "host-b", IncludeDependencies: false,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "failed" || len(resp.Errors) != 1 {
		t.Fatalf("response = %+v, want failed with one error", resp)
	}
	if len(source.exportCalls) != 0 || len(target.importCalls) != 0 {
		t.Fatalf("export/import calls = %v/%v, want none", source.exportCalls, target.importCalls)
	}
}

func TestRuntimeRouterReplicateDoesNotRecordFailedDiffLocation(t *testing.T) {
	store := NewPlacementStore("")
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
		{SnapshotID: "snap-base", SnapshotType: "full"},
		{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
	}}
	target := &routerFakeDaemon{importErrForBody: map[string]error{"bundle:snap-diff": errors.New("import failed")}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler([]RuntimeHost{{Name: "host-a", Healthy: true}, {Name: "host-b", Healthy: true}}, nil, nil),
		map[string]Daemon{"host-a": source, "host-b": target},
		RuntimeRouterOptions{PlacementStore: store},
	)
	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID: "snap-diff", SourceHost: "host-a", TargetHost: "host-b", IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "partial" {
		t.Fatalf("status = %q, want partial", resp.Status)
	}
	if hosts := store.SnapshotHosts("snap-base"); strings.Join(hosts, ",") != "host-b" {
		t.Fatalf("base hosts = %v, want host-b", hosts)
	}
	if hosts := store.SnapshotHosts("snap-diff"); len(hosts) != 0 {
		t.Fatalf("diff hosts = %v, want empty", hosts)
	}
}

func TestRuntimeRouterReplicateRejectsUnhealthyHostBeforeDaemonCall(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler([]RuntimeHost{{Name: "host-a", Healthy: false}, {Name: "host-b", Healthy: true}}, nil, nil),
		map[string]Daemon{"host-a": source, "host-b": target},
	)
	_, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID: "snap-1", SourceHost: "host-a", TargetHost: "host-b", IncludeDependencies: true,
	})
	if err == nil {
		t.Fatal("ReplicateSnapshot error = nil, want source_host_unavailable")
	}
	if len(source.exportCalls) != 0 || len(target.importCalls) != 0 {
		t.Fatalf("daemon calls = %v/%v, want none", source.exportCalls, target.importCalls)
	}
}
```

Update imports in `runtime_router_test.go` with `io` and `strings`.

- [ ] **Step 3: Run router tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterReplicate' -count=1
```

Expected: FAIL to compile with undefined `SnapshotReplicationRequest` and `ReplicateSnapshot`.

- [ ] **Step 4: Add scheduler host lookup**

In `internal/anvilmcp/scheduler.go`, add:

```go
func (s *Scheduler) RuntimeHost(name string) (RuntimeHost, bool) {
	name = strings.TrimSpace(name)
	if s == nil || name == "" {
		return RuntimeHost{}, false
	}
	for _, host := range s.hosts {
		if host.Name == name {
			return cloneRuntimeHost(host), true
		}
	}
	return RuntimeHost{}, false
}
```

Add `strings` to imports if `scheduler.go` does not already import it.

- [ ] **Step 5: Implement `snapshot_replication.go`**

Create `internal/anvilmcp/snapshot_replication.go`:

```go
package anvilmcp

import (
	"context"
	"fmt"
	"io"
	"strings"
)

type SnapshotReplicationRequest struct {
	SnapshotID          string `json:"snapshot_id"`
	SourceHost          string `json:"source_host"`
	TargetHost          string `json:"target_host"`
	IncludeDependencies bool   `json:"include_dependencies"`
}

type SnapshotReplicationResponse struct {
	SnapshotID string   `json:"snapshot_id"`
	SourceHost string   `json:"source_host"`
	TargetHost string   `json:"target_host"`
	Status     string   `json:"status"`
	Replicated []string `json:"replicated"`
	Skipped    []string `json:"skipped"`
	Errors     []string `json:"errors"`
}

type snapshotTransferDaemon interface {
	ListSnapshots(context.Context) ([]SnapshotInfo, error)
	ExportSnapshot(context.Context, string) (*SnapshotExportStream, error)
	ImportSnapshot(context.Context, io.Reader) (*SnapshotImportResponse, error)
}
```

Implement `RuntimeRouter.ReplicateSnapshot` with these exact semantics:

- Trim `snapshot_id`, `source_host`, `target_host`.
- Reject empty fields with `fmt.Errorf("snapshot_id is required")`, `fmt.Errorf("source_host is required")`, or `fmt.Errorf("target_host is required")`.
- Reject same source/target with `fmt.Errorf("same_host")`.
- Resolve both hosts through `r.scheduler.RuntimeHost`.
- Reject missing hosts with `fmt.Errorf("source_host_not_found")` or `fmt.Errorf("target_host_not_found")`.
- Reject `!host.Healthy` with `fmt.Errorf("source_host_unavailable")` or `fmt.Errorf("target_host_unavailable")`.
- Type assert both daemons to `snapshotTransferDaemon`; reject unsupported with `fmt.Errorf("runtime host %q does not support snapshot replication", hostName)`.
- Use `source.ListSnapshots(ctx)` to find requested snapshot and its base.
- Use `target.ListSnapshots(ctx)` to check which snapshots are already present.
- If requested snapshot is diff and base is absent on target:
  - when `IncludeDependencies` is false, return response with `Status: "failed"` and one `diff_base_missing` error.
  - when true, replicate base first.
- For each snapshot to replicate, stream:

```go
stream, err := source.ExportSnapshot(ctx, snapshotID)
if err != nil {
	resp.Status = statusForFailure(resp)
	resp.Errors = append(resp.Errors, fmt.Sprintf("%s export failed: %v", snapshotID, err))
	return resp, nil
}
defer stream.Body.Close()
importResp, err := target.ImportSnapshot(ctx, stream.Body)
if err != nil {
	resp.Status = statusForFailure(resp)
	resp.Errors = append(resp.Errors, fmt.Sprintf("%s import failed: %v", snapshotID, err))
	return resp, nil
}
```

- Treat `importResp.Status == "already_present"` as skipped and still record location.
- After each successful or already-present import, call `r.placementStore.SetSnapshotLocation(snapshotID, targetHost)` and `Save` when `placementStore != nil`.
- If a base succeeds but diff fails, status is `partial`.
- If every needed snapshot succeeds or is already present, status is `replicated`.

Add focused helpers in the same file:

```go
func statusForFailure(resp *SnapshotReplicationResponse) string {
	if len(resp.Replicated) > 0 || len(resp.Skipped) > 0 {
		return "partial"
	}
	return "failed"
}

func snapshotInfoByID(snapshots []SnapshotInfo, snapshotID string) (SnapshotInfo, bool) {
	for _, snapshot := range snapshots {
		if strings.TrimSpace(snapshot.SnapshotID) == snapshotID {
			return snapshot, true
		}
	}
	return SnapshotInfo{}, false
}
```

- [ ] **Step 6: Run router tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterReplicate' -count=1
```

Expected: PASS.

- [ ] **Step 7: Run all anvilmcp tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit router replication**

```bash
git add internal/anvilmcp/snapshot_replication.go internal/anvilmcp/scheduler.go internal/anvilmcp/runtime_router_test.go
git commit -m "feat: orchestrate snapshot replication"
```

## Task 5: MCP Tool and Schema

**Files:**
- Modify: `internal/anvilmcp/tools.go`
- Modify: `internal/anvilmcp/tools_test.go`
- Modify: `internal/anvilmcp/ironclaw_schema.go`
- Modify: `internal/anvilmcp/ironclaw_schema_test.go`
- Modify: `cmd/anvil-mcp/main.go`
- Modify: `cmd/anvil-mcp/main_test.go`

- [ ] **Step 1: Write failing tool tests**

In `internal/anvilmcp/tools_test.go`, extend the fake daemon with:

```go
replicateSnapshotCalls int
replicateSnapshotReq   SnapshotReplicationRequest
replicateSnapshotResp  *SnapshotReplicationResponse
replicateSnapshotErr   error
```

Add method:

```go
func (f *fakeDaemon) ReplicateSnapshot(_ context.Context, req SnapshotReplicationRequest) (*SnapshotReplicationResponse, error) {
	f.replicateSnapshotCalls++
	f.replicateSnapshotReq = req
	if f.replicateSnapshotErr != nil {
		return nil, f.replicateSnapshotErr
	}
	if f.replicateSnapshotResp != nil {
		return f.replicateSnapshotResp, nil
	}
	return &SnapshotReplicationResponse{
		SnapshotID: req.SnapshotID,
		SourceHost: req.SourceHost,
		TargetHost: req.TargetHost,
		Status: "replicated",
		Replicated: []string{req.SnapshotID},
	}, nil
}
```

Append tests:

```go
func TestToolsReplicateSnapshotCallsReplicator(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewMemorySessionStore(), time.Second)
	out, err := tools.ReplicateSnapshot(context.Background(), ReplicateSnapshotInput{
		SnapshotID: "snap-1", SourceHost: "host-a", TargetHost: "host-b", IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if daemon.replicateSnapshotCalls != 1 {
		t.Fatalf("replicate calls = %d, want 1", daemon.replicateSnapshotCalls)
	}
	if !daemon.replicateSnapshotReq.IncludeDependencies {
		t.Fatal("IncludeDependencies = false, want true")
	}
	if out.Status != "replicated" || out.SnapshotID != "snap-1" {
		t.Fatalf("output = %+v, want replicated snap-1", out)
	}
}

func TestToolsReplicateSnapshotRejectsUnsupportedDaemon(t *testing.T) {
	tools := NewTools(&fakeDaemonWithoutReplicator{}, NewMemorySessionStore(), time.Second)
	_, err := tools.ReplicateSnapshot(context.Background(), ReplicateSnapshotInput{SnapshotID: "snap-1", SourceHost: "host-a", TargetHost: "host-b"})
	if err == nil {
		t.Fatal("ReplicateSnapshot error = nil, want unsupported daemon error")
	}
}

func TestToolsMCPReplicateSnapshotOutputOmitsSecrets(t *testing.T) {
	daemon := &fakeDaemon{replicateSnapshotResp: &SnapshotReplicationResponse{
		SnapshotID: "snap-1",
		SourceHost: "host-a",
		TargetHost: "host-b",
		Status: "replicated",
		Replicated: []string{"snap-1"},
	}}
	tools := NewTools(daemon, NewMemorySessionStore(), time.Second)
	_, out, err := tools.MCPReplicateSnapshot(context.Background(), nil, ReplicateSnapshotInput{SnapshotID: "snap-1", SourceHost: "host-a", TargetHost: "host-b"})
	if err != nil {
		t.Fatalf("MCPReplicateSnapshot returned error: %v", err)
	}
	raw, _ := json.Marshal(out)
	for _, forbidden := range []string{"agent_token", "Authorization", "Bearer", "metadata.json", "http://host-a"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("replication output exposes %q: %s", forbidden, raw)
		}
	}
}
```

If `fakeDaemonWithoutReplicator` does not exist, add a minimal struct that embeds or duplicates the existing fake daemon methods except `ReplicateSnapshot`.

- [ ] **Step 2: Write failing registration/schema tests**

Update `cmd/anvil-mcp/main_test.go` expected map with:

```go
"anvil_replicate_snapshot": "Replicate a snapshot from one scheduler runtime host to another and update snapshot locality.",
```

Update `internal/anvilmcp/ironclaw_schema_test.go` to assert `anvil_replicate_snapshot` exists and has required `snapshot_id`, `source_host`, and `target_host` fields plus optional `include_dependencies`.

- [ ] **Step 3: Run MCP-focused tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestTools.*ReplicateSnapshot|Test.*IronClaw' -count=1
go test ./cmd/anvil-mcp -run TestToolRegistrations -count=1
```

Expected: FAIL with undefined `ReplicateSnapshotInput`, `MCPReplicateSnapshot`, and missing tool registration/schema.

- [ ] **Step 4: Implement tool input and methods**

In `internal/anvilmcp/tools.go`, add:

```go
type ReplicateSnapshotInput struct {
	SnapshotID          string `json:"snapshot_id"`
	SourceHost          string `json:"source_host"`
	TargetHost          string `json:"target_host"`
	IncludeDependencies bool   `json:"include_dependencies,omitempty"`
}

type snapshotReplicator interface {
	ReplicateSnapshot(context.Context, SnapshotReplicationRequest) (*SnapshotReplicationResponse, error)
}
```

Add methods near snapshot tools:

```go
func (t *Tools) ReplicateSnapshot(ctx context.Context, input ReplicateSnapshotInput) (*SnapshotReplicationResponse, error) {
	tenantID, err := t.resolveTenantID("")
	if err != nil {
		return nil, err
	}
	snapshotID := strings.TrimSpace(input.SnapshotID)
	sourceHost := strings.TrimSpace(input.SourceHost)
	targetHost := strings.TrimSpace(input.TargetHost)
	if snapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	if sourceHost == "" {
		return nil, fmt.Errorf("source_host is required")
	}
	if targetHost == "" {
		return nil, fmt.Errorf("target_host is required")
	}
	replicator, ok := t.daemon.(snapshotReplicator)
	if !ok {
		err := fmt.Errorf("configured daemon does not support snapshot replication")
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_replicate_snapshot", "POST /snapshots/{snapshot_id}/export -> POST /snapshots/import", err)
	}
	out, err := replicator.ReplicateSnapshot(ctx, SnapshotReplicationRequest{
		SnapshotID: snapshotID,
		SourceHost: sourceHost,
		TargetHost: targetHost,
		IncludeDependencies: input.IncludeDependencies,
	})
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_replicate_snapshot", "POST /snapshots/{snapshot_id}/export -> POST /snapshots/import", err)
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_replicate_snapshot", "POST /snapshots/{snapshot_id}/export -> POST /snapshots/import"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPReplicateSnapshot(ctx context.Context, req *mcp.CallToolRequest, input ReplicateSnapshotInput) (*mcp.CallToolResult, SnapshotReplicationResponse, error) {
	out, err := t.ReplicateSnapshot(ctx, input)
	if err != nil || out == nil {
		return nil, SnapshotReplicationResponse{}, err
	}
	return nil, *out, nil
}
```

- [ ] **Step 5: Register schema and MCP tool**

In `internal/anvilmcp/ironclaw_schema.go`, add:

```go
toolInputSchemaFromStruct("anvil_replicate_snapshot", ReplicateSnapshotInput{}),
```

near the snapshot tools.

In `cmd/anvil-mcp/main.go`, add a registration after `anvil_delete_snapshot`:

```go
{
	name:        "anvil_replicate_snapshot",
	description: "Replicate a snapshot from one scheduler runtime host to another and update snapshot locality.",
	register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
		mcp.AddTool(server, tool, tools.MCPReplicateSnapshot)
	},
},
```

- [ ] **Step 6: Run MCP-focused tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestTools.*ReplicateSnapshot|Test.*IronClaw' -count=1
go test ./cmd/anvil-mcp -run TestToolRegistrations -count=1
```

Expected: PASS.

- [ ] **Step 7: Run full MCP packages**

Run:

```bash
go test ./internal/anvilmcp ./cmd/anvil-mcp -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit MCP tool**

```bash
git add internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go internal/anvilmcp/ironclaw_schema.go internal/anvilmcp/ironclaw_schema_test.go cmd/anvil-mcp/main.go cmd/anvil-mcp/main_test.go
git commit -m "feat: add snapshot replication mcp tool"
```

## Task 6: Documentation

**Files:**
- Modify: `README.md`
- Modify: `RELEASE_NOTES.md`
- Modify: `docs/architecture/service-logic.md`
- Modify: `docs/architecture/runtime-architecture.md`
- Modify: `docs/operations/runbook.md`
- Modify: `docs/operations/2026-05-29-anvil-follow-up-development.md`

- [ ] **Step 1: Update README API and MCP sections**

Add daemon API lines near snapshot routes:

```text
POST   /snapshots/{id}/export       -> snapshot bundle export
POST   /snapshots/import            -> snapshot bundle import
```

Add MCP tool line near snapshot tools:

```text
- anvil_replicate_snapshot: source_host의 snapshot artifact를 target_host로 수동 복제하고 scheduler snapshot locality를 갱신한다.
```

Add operator example:

```bash
anvil_replicate_snapshot \
  snapshot_id=snap-1 \
  source_host=host-a \
  target_host=host-b \
  include_dependencies=true
```

Explain that diff replication with `include_dependencies=true` copies the base full snapshot before the diff snapshot.

- [ ] **Step 2: Update release notes**

Add under Unreleased:

```markdown
## 추가됨

- 수동 cross-host snapshot replication:
  - daemon `POST /snapshots/{id}/export`
  - daemon `POST /snapshots/import`
  - MCP `anvil_replicate_snapshot`
  - 성공한 target host를 scheduler `SnapshotLocations`에 기록해 restore locality에 반영한다.

## 보안/운영 강화

- replication response, audit, metrics에는 `agent_token`, authorization header, daemon raw body, `metadata.json` raw body를 노출하지 않는다.
- diff snapshot replication은 target에 base full snapshot이 없으면 `include_dependencies=true`일 때 base를 먼저 복제한다.
```

- [ ] **Step 3: Update architecture docs**

In `docs/architecture/service-logic.md`, add a section:

```markdown
## Cross-host snapshot replication

수동 replication은 MCP/runtime router가 source daemon의 `POST /snapshots/{id}/export`
stream을 target daemon의 `POST /snapshots/import` request body로 연결한다. daemon은
artifact bundle 생성과 import staging을 소유하고, scheduler는 성공한 snapshot
location만 `PlacementStoreState.SnapshotLocations`에 기록한다.

diff snapshot은 target에 `base_snapshot_id`가 없으면 import를 거부한다. router는
`include_dependencies=true` 요청에서 base full snapshot을 먼저 복제한 뒤 diff
snapshot을 복제한다.
```

In `docs/architecture/runtime-architecture.md`, add:

```markdown
Snapshot import는 `snapshots/.import-<snapshot_id>-<nonce>/` temporary directory에
bundle을 먼저 풀고 manifest/checksum/metadata 검증이 끝난 뒤
`snapshots/<snapshot_id>/`로 atomic rename한다. import는 target host의
`metadata.json` 안 `memory.bin`, `state.bin`, `rootfs.ext4` path를 target snapshot
directory로 rebase한다.
```

- [ ] **Step 4: Update operations docs**

In `docs/operations/runbook.md`, add a short runbook:

```markdown
## Snapshot cross-host replication

1. source/target scheduler host가 healthy인지 확인한다.
2. diff snapshot이면 `include_dependencies=true`를 사용한다.
3. `anvil_replicate_snapshot`을 호출한다.
4. `/placements` 또는 scheduler state에서 `snapshot_locations[snapshot_id]`에 target
   host가 추가됐는지 확인한다.
5. 실패 시 target daemon의 `.import-*` directory가 남지 않았는지 확인한다.
```

In `docs/operations/2026-05-29-anvil-follow-up-development.md`, mark item 4 as implementation in progress and point to the plan/spec paths.

- [ ] **Step 5: Run docs grep checks**

Run:

```bash
rg -n "anvil_replicate_snapshot|snapshots/.+export|snapshots/import|SnapshotLocations|cross-host snapshot replication" README.md RELEASE_NOTES.md docs/architecture docs/operations
```

Expected: output includes all modified docs and no unrelated legacy-only references.

- [ ] **Step 6: Commit docs**

```bash
git add README.md RELEASE_NOTES.md docs/architecture/service-logic.md docs/architecture/runtime-architecture.md docs/operations/runbook.md docs/operations/2026-05-29-anvil-follow-up-development.md
git commit -m "docs: document snapshot replication operations"
```

## Task 7: Full Verification

**Files:**
- No source edits expected.

- [ ] **Step 1: Run focused tests**

```bash
go test ./internal/storage -count=1
go test ./cmd/goose-daemon -count=1
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-mcp -count=1
```

Expected: all PASS.

- [ ] **Step 2: Run full tests**

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 3: Run required builds**

```bash
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

Expected: all commands exit 0.

- [ ] **Step 4: Check formatting and whitespace**

```bash
gofmt -w internal/storage/snapshot_bundle.go internal/storage/snapshot_bundle_test.go cmd/goose-daemon/api.go cmd/goose-daemon/api_test.go internal/anvilmcp/daemon_client.go internal/anvilmcp/daemon_client_test.go internal/anvilmcp/snapshot_replication.go internal/anvilmcp/scheduler.go internal/anvilmcp/runtime_router_test.go internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go internal/anvilmcp/ironclaw_schema.go internal/anvilmcp/ironclaw_schema_test.go cmd/anvil-mcp/main.go cmd/anvil-mcp/main_test.go
git diff --check
```

Expected: no output from `git diff --check`.

- [ ] **Step 5: Check secret exposure**

```bash
rg -n "agent_token|Authorization: Bearer|metadata\\.json raw|secret-token" internal/anvilmcp cmd/goose-daemon internal/storage README.md RELEASE_NOTES.md docs/architecture docs/operations
```

Expected: matches only invariant statements, test fixture assertions, or existing safe references. No new operator-facing response examples include token values.

- [ ] **Step 6: Commit verification fixes if needed**

If gofmt or verification fixes changed files:

```bash
git add internal cmd README.md RELEASE_NOTES.md docs
git commit -m "chore: finalize snapshot replication verification"
```

Expected: commit only if verification changed tracked files.

## Task 8: Implementation Handoff

**Files:**
- Create: `docs/operations/2026-06-02-cross-host-snapshot-replication-handoff.md`
- Modify: `RELEASE_NOTES.md` if verification results need to be recorded.

- [ ] **Step 1: Write handoff**

Create `docs/operations/2026-06-02-cross-host-snapshot-replication-handoff.md`:

```markdown
# Cross-host Snapshot Replication 운영 인계

작성일: 2026-06-02

## 릴리즈 범위

- daemon snapshot bundle export/import API
- MCP `anvil_replicate_snapshot`
- scheduler `SnapshotLocations` 갱신
- diff snapshot base dependency 복제

## 검증

- `go test ./internal/storage -count=1`: PASS
- `go test ./cmd/goose-daemon -count=1`: PASS
- `go test ./internal/anvilmcp -count=1`: PASS
- `go test ./cmd/anvil-mcp -count=1`: PASS
- `go test ./... -count=1`: PASS
- `go build ./cmd/goose-daemon`: PASS
- `go build ./cmd/anvil-mcp`: PASS
- `go build ./cmd/anvil-scheduler`: PASS
- `git diff --check`: PASS

## 잔여 위험

- 첫 버전은 수동 동기 replication이다. background retry queue와 metrics는 후속 후보로 남는다.
- KVM/Firecracker 기반 real restore 검증은 release 전에 별도 운영 환경에서 수행한다.
- source/target host degraded override는 지원하지 않는다.

## 다음 작업

- scheduler-aware cross-host flock placement 설계
- replication metrics와 alert 설계
```

- [ ] **Step 2: Commit handoff**

```bash
git add docs/operations/2026-06-02-cross-host-snapshot-replication-handoff.md RELEASE_NOTES.md
git commit -m "docs: record snapshot replication handoff"
```

- [ ] **Step 3: Final status check**

```bash
git status --short --branch
git log --oneline --decorate -8
```

Expected: clean working tree on `feature/cross-host-snapshot-replication`.
