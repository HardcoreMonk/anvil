# anvil Snapshot Retention/GC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `POST /snapshots/gc` 수동 daemon API를 추가해 snapshot GC dry-run과 명시적 apply를 제공한다.

**Architecture:** GC는 `cmd/goose-daemon/api.go` 안에서 현재 `cp.snapshots` metadata map을 복사해 plan을 계산한다. `apply: false`는 파일 시스템과 map을 변경하지 않고, `apply: true`는 보호되지 않은 후보만 `storage.DeleteSnapshot`으로 삭제한 뒤 map에서 제거한다. 기존 diff dependency 보호 의미는 `DELETE /snapshots/{id}`와 GC apply 양쪽에서 유지한다.

**Tech Stack:** Go 1.25, standard library `net/http`, `encoding/json`, `sort`, `time`, existing `internal/storage` snapshot metadata helpers.

---

## File Structure

- Modify: `cmd/goose-daemon/api.go`
  - GC request/response type
  - deterministic GC planner
  - `POST /snapshots/gc` handler
  - GC apply helper
  - existing snapshot delete helper reuse
- Modify: `cmd/goose-daemon/api_test.go`
  - VM/Firecracker 없이 동작하는 planner, dry-run, apply, validation test
- Modify: `README.md`
  - daemon snapshot lifecycle API에 `POST /snapshots/gc` 설명 추가
- Modify: `RELEASE_NOTES.md`
  - 수동 snapshot retention/GC 추가 기록
- Modify: `docs/architecture/service-logic.md`
  - GC 계산/적용 흐름과 invariant 설명
- Modify: `docs/architecture/runtime-architecture.md`
  - snapshot disk lifecycle에 수동 GC 설명 추가
- Modify: `CONTEXT.md`
  - 후속 후보에 snapshot retention/GC가 이번 작업으로 진행됐음을 반영

## Scope Check

이 plan은 단일 daemon API와 문서 갱신만 포함한다. 자동 GC worker, MCP tool 추가, tenant quota, snapshot size 기반 policy, multi-host catalog는 포함하지 않는다.

### Task 1: Planner Unit Test

**Files:**
- Modify: `cmd/goose-daemon/api_test.go`
- Test: `cmd/goose-daemon/api_test.go`

- [ ] **Step 1: Add imports and test helpers**

Modify the import block in `cmd/goose-daemon/api_test.go` to this complete block:

```go
import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ephemera/internal/storage"
)
```

Update `newTestCP` so snapshot tests start with an initialized snapshot map:

```go
func newTestCP(t *testing.T) *ControlPlane {
	t.Helper()
	tmp := t.TempDir()
	defaultCfg := filepath.Join(tmp, "goose.yaml")
	defaultSec := filepath.Join(tmp, "goose-secrets.yaml")
	os.WriteFile(defaultCfg, []byte("GOOSE_PROVIDER: default\n"), 0644)
	os.WriteFile(defaultSec, []byte("DEFAULT_KEY: x\n"), 0644)
	return &ControlPlane{
		vms:              make(map[string]*runningVM),
		snapshots:        make(map[string]storage.SnapshotMetadata),
		workDir:          tmp,
		gooseConfigPath:  defaultCfg,
		gooseSecretsPath: defaultSec,
	}
}
```

Append these helpers below `newTestCP`:

```go
func testSnapshotMeta(snapshotID, sourceVMID, snapshotType string, createdAt time.Time) storage.SnapshotMetadata {
	return storage.SnapshotMetadata{
		SnapshotID:   snapshotID,
		SourceVMID:   sourceVMID,
		SnapshotType: snapshotType,
		CreatedAt:    createdAt,
	}
}

func addTestSnapshot(t *testing.T, cp *ControlPlane, meta storage.SnapshotMetadata) {
	t.Helper()
	snapDir := storage.SnapshotDir(cp.workDir, meta.SnapshotID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "metadata.json"), []byte(`{}`), 0600); err != nil {
		t.Fatalf("create snapshot metadata: %v", err)
	}
	cp.snapshots[meta.SnapshotID] = meta
}

func snapshotIDs(entries []SnapshotGCEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.SnapshotID)
	}
	return ids
}

func gcEntryByID(entries []SnapshotGCEntry, snapshotID string) (SnapshotGCEntry, bool) {
	for _, entry := range entries {
		if entry.SnapshotID == snapshotID {
			return entry, true
		}
	}
	return SnapshotGCEntry{}, false
}

func decodeGCResponse(t *testing.T, rr *httptest.ResponseRecorder) SnapshotGCResponse {
	t.Helper()
	var resp SnapshotGCResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GC response %q: %v", rr.Body.String(), err)
	}
	return resp
}
```

- [ ] **Step 2: Write the failing planner test**

Append this test to `cmd/goose-daemon/api_test.go`:

```go
func TestPlanSnapshotGCProtectsReferencedAndKeepLast(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	cp := newTestCP(t)

	fullOld := testSnapshotMeta("snap-full-old", "vm-1", "full", now.Add(-10*24*time.Hour))
	diffOld := testSnapshotMeta("snap-diff-old", "vm-1", "diff", now.Add(-9*24*time.Hour))
	diffOld.BaseSnapshotID = "snap-full-old"
	fullRecent := testSnapshotMeta("snap-full-recent", "vm-1", "full", now.Add(-1*time.Hour))
	otherOld := testSnapshotMeta("snap-other-old", "vm-2", "full", now.Add(-8*24*time.Hour))
	otherRecent := testSnapshotMeta("snap-other-recent", "vm-2", "full", now.Add(-30*time.Minute))

	for _, meta := range []storage.SnapshotMetadata{fullOld, diffOld, fullRecent, otherOld, otherRecent} {
		cp.snapshots[meta.SnapshotID] = meta
	}

	got := cp.planSnapshotGC(SnapshotGCPolicy{
		OlderThanSeconds: int64((7 * 24 * time.Hour) / time.Second),
		KeepLastPerVM:   1,
	}, now)

	if ids := strings.Join(snapshotIDs(got.Candidates), ","); ids != "snap-diff-old,snap-other-old" {
		t.Fatalf("candidate IDs = %s, want snap-diff-old,snap-other-old", ids)
	}

	base, ok := gcEntryByID(got.Protected, "snap-full-old")
	if !ok {
		t.Fatal("snap-full-old was not protected")
	}
	if base.Reason != "referenced_by_diff" {
		t.Fatalf("snap-full-old reason = %q, want referenced_by_diff", base.Reason)
	}
	if strings.Join(base.ReferencedBy, ",") != "snap-diff-old" {
		t.Fatalf("snap-full-old referenced_by = %v, want [snap-diff-old]", base.ReferencedBy)
	}

	for _, snapshotID := range []string{"snap-full-recent", "snap-other-recent"} {
		entry, ok := gcEntryByID(got.Protected, snapshotID)
		if !ok {
			t.Fatalf("%s was not protected", snapshotID)
		}
		if entry.Reason != "keep_last_per_vm" {
			t.Fatalf("%s reason = %q, want keep_last_per_vm", snapshotID, entry.Reason)
		}
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run:

```bash
go test ./cmd/goose-daemon -run TestPlanSnapshotGCProtectsReferencedAndKeepLast -count=1
```

Expected: FAIL to compile with errors mentioning undefined `SnapshotGCEntry`, `SnapshotGCResponse`, `SnapshotGCPolicy`, or `planSnapshotGC`.

- [ ] **Step 4: Commit the red test**

```bash
git add cmd/goose-daemon/api_test.go
git commit -m "test: define snapshot gc planner behavior"
```

### Task 2: Planner Implementation

**Files:**
- Modify: `cmd/goose-daemon/api.go`
- Test: `cmd/goose-daemon/api_test.go`

- [ ] **Step 1: Add `sort` import**

Update the import block in `cmd/goose-daemon/api.go` by adding `sort`:

```go
import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)
```

- [ ] **Step 2: Add GC types**

Insert these types immediately after `SnapshotRequest` in `cmd/goose-daemon/api.go`:

```go
// SnapshotGCRequest is the optional body for POST /snapshots/gc.
type SnapshotGCRequest struct {
	OlderThanSeconds int64 `json:"older_than_seconds"`
	KeepLastPerVM    int   `json:"keep_last_per_vm"`
	Apply            bool  `json:"apply"`
}

// SnapshotGCPolicy is echoed in GC responses without the apply flag.
type SnapshotGCPolicy struct {
	OlderThanSeconds int64 `json:"older_than_seconds"`
	KeepLastPerVM    int   `json:"keep_last_per_vm"`
}

// SnapshotGCEntry is the public representation of one GC decision.
type SnapshotGCEntry struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Reason         string    `json:"reason"`
	ReferencedBy   []string  `json:"referenced_by,omitempty"`
}

// SnapshotGCError records a per-snapshot apply failure.
type SnapshotGCError struct {
	SnapshotID string `json:"snapshot_id"`
	Error      string `json:"error"`
}

// SnapshotGCResponse is returned by POST /snapshots/gc for dry-run and apply.
type SnapshotGCResponse struct {
	Applied     bool              `json:"applied"`
	RequestedAt time.Time         `json:"requested_at"`
	Policy      SnapshotGCPolicy  `json:"policy"`
	Candidates  []SnapshotGCEntry `json:"candidates"`
	Protected   []SnapshotGCEntry `json:"protected"`
	Deleted     []SnapshotGCEntry `json:"deleted"`
	Errors      []SnapshotGCError `json:"errors"`
}

const (
	snapshotGCReasonOlderThan        = "older_than"
	snapshotGCReasonReferencedByDiff = "referenced_by_diff"
	snapshotGCReasonKeepLastPerVM    = "keep_last_per_vm"
)
```

- [ ] **Step 3: Add planner helpers**

Insert these helpers below `snapshotInfoFrom`:

```go
func snapshotGCEntryFrom(meta storage.SnapshotMetadata, reason string, referencedBy []string) SnapshotGCEntry {
	refs := append([]string(nil), referencedBy...)
	sort.Strings(refs)
	return SnapshotGCEntry{
		SnapshotID:     meta.SnapshotID,
		SourceVMID:     meta.SourceVMID,
		Profile:        meta.Profile,
		SnapshotType:   meta.SnapshotType,
		BaseSnapshotID: meta.BaseSnapshotID,
		CreatedAt:      meta.CreatedAt,
		Reason:         reason,
		ReferencedBy:   refs,
	}
}

func sortSnapshotsOldestFirst(snapshots []storage.SnapshotMetadata) {
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].CreatedAt.Equal(snapshots[j].CreatedAt) {
			return snapshots[i].SnapshotID < snapshots[j].SnapshotID
		}
		return snapshots[i].CreatedAt.Before(snapshots[j].CreatedAt)
	})
}

func (cp *ControlPlane) snapshotMetadataList() []storage.SnapshotMetadata {
	cp.snapshotsMu.RLock()
	defer cp.snapshotsMu.RUnlock()

	list := make([]storage.SnapshotMetadata, 0, len(cp.snapshots))
	for _, meta := range cp.snapshots {
		list = append(list, meta)
	}
	sortSnapshotsOldestFirst(list)
	return list
}

func (cp *ControlPlane) planSnapshotGC(policy SnapshotGCPolicy, now time.Time) SnapshotGCResponse {
	snapshots := cp.snapshotMetadataList()
	referencedBy := make(map[string][]string)
	for _, meta := range snapshots {
		if meta.BaseSnapshotID != "" {
			referencedBy[meta.BaseSnapshotID] = append(referencedBy[meta.BaseSnapshotID], meta.SnapshotID)
		}
	}
	for id := range referencedBy {
		sort.Strings(referencedBy[id])
	}

	protected := make(map[string]SnapshotGCEntry)
	for _, meta := range snapshots {
		if refs, ok := referencedBy[meta.SnapshotID]; ok {
			protected[meta.SnapshotID] = snapshotGCEntryFrom(meta, snapshotGCReasonReferencedByDiff, refs)
		}
	}

	if policy.KeepLastPerVM > 0 {
		byVM := make(map[string][]storage.SnapshotMetadata)
		for _, meta := range snapshots {
			byVM[meta.SourceVMID] = append(byVM[meta.SourceVMID], meta)
		}
		for _, group := range byVM {
			sort.Slice(group, func(i, j int) bool {
				if group[i].CreatedAt.Equal(group[j].CreatedAt) {
					return group[i].SnapshotID > group[j].SnapshotID
				}
				return group[i].CreatedAt.After(group[j].CreatedAt)
			})
			for i := 0; i < len(group) && i < policy.KeepLastPerVM; i++ {
				meta := group[i]
				if _, exists := protected[meta.SnapshotID]; !exists {
					protected[meta.SnapshotID] = snapshotGCEntryFrom(meta, snapshotGCReasonKeepLastPerVM, nil)
				}
			}
		}
	}

	resp := SnapshotGCResponse{
		RequestedAt: now,
		Policy:      policy,
		Candidates:  []SnapshotGCEntry{},
		Protected:   []SnapshotGCEntry{},
		Deleted:     []SnapshotGCEntry{},
		Errors:      []SnapshotGCError{},
	}

	cutoff := now.Add(-time.Duration(policy.OlderThanSeconds) * time.Second)
	for _, meta := range snapshots {
		if entry, ok := protected[meta.SnapshotID]; ok {
			resp.Protected = append(resp.Protected, entry)
			continue
		}
		if policy.OlderThanSeconds == 0 || !meta.CreatedAt.After(cutoff) {
			resp.Candidates = append(resp.Candidates, snapshotGCEntryFrom(meta, snapshotGCReasonOlderThan, nil))
		}
	}
	return resp
}
```

- [ ] **Step 4: Run planner test to verify it passes**

Run:

```bash
go test ./cmd/goose-daemon -run TestPlanSnapshotGCProtectsReferencedAndKeepLast -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit planner implementation**

```bash
git add cmd/goose-daemon/api.go cmd/goose-daemon/api_test.go
git commit -m "feat: plan snapshot gc candidates"
```

### Task 3: Dry-Run Endpoint Tests

**Files:**
- Modify: `cmd/goose-daemon/api_test.go`
- Test: `cmd/goose-daemon/api_test.go`

- [ ] **Step 1: Write dry-run and validation tests**

Update the import block in `cmd/goose-daemon/api_test.go` by adding `bytes`:

```go
import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ephemera/internal/storage"
)
```

Append these tests to `cmd/goose-daemon/api_test.go`:

```go
func TestHandleSnapshotGCDryRunDoesNotDelete(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)
	addTestSnapshot(t, cp, testSnapshotMeta("snap-old", "vm-1", "full", now.Add(-10*24*time.Hour)))
	addTestSnapshot(t, cp, testSnapshotMeta("snap-new", "vm-1", "full", now.Add(-1*time.Hour)))

	req := httptest.NewRequest(http.MethodPost, "/snapshots/gc", bytes.NewReader([]byte(`{"older_than_seconds":604800}`)))
	rr := httptest.NewRecorder()
	cp.handleSnapshotGC(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	resp := decodeGCResponse(t, rr)
	if resp.Applied {
		t.Fatal("dry-run response applied = true, want false")
	}
	if ids := strings.Join(snapshotIDs(resp.Candidates), ","); ids != "snap-old" {
		t.Fatalf("candidate IDs = %s, want snap-old", ids)
	}
	if len(resp.Deleted) != 0 {
		t.Fatalf("deleted count = %d, want 0", len(resp.Deleted))
	}
	if _, ok := cp.snapshots["snap-old"]; !ok {
		t.Fatal("dry-run removed snap-old from map")
	}
	if _, err := os.Stat(storage.SnapshotDir(cp.workDir, "snap-old")); err != nil {
		t.Fatalf("dry-run removed snap-old directory: %v", err)
	}
}

func TestHandleSnapshotGCRejectsInvalidPolicy(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "malformed json",
			body:       `{`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "invalid JSON body",
		},
		{
			name:       "negative older_than_seconds",
			body:       `{"older_than_seconds":-1}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "older_than_seconds must be non-negative",
		},
		{
			name:       "negative keep_last_per_vm",
			body:       `{"keep_last_per_vm":-1}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "keep_last_per_vm must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := newTestCP(t)
			req := httptest.NewRequest(http.MethodPost, "/snapshots/gc", bytes.NewReader([]byte(tt.body)))
			rr := httptest.NewRecorder()
			cp.handleSnapshotGC(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), tt.wantStatus)
			}
			if !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want substring %q", rr.Body.String(), tt.wantBody)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestHandleSnapshotGC(DryRunDoesNotDelete|RejectsInvalidPolicy)' -count=1
```

Expected: FAIL to compile with undefined `handleSnapshotGC`.

- [ ] **Step 3: Commit the red endpoint tests**

```bash
git add cmd/goose-daemon/api_test.go
git commit -m "test: define snapshot gc dry run api"
```

### Task 4: Dry-Run Endpoint Implementation

**Files:**
- Modify: `cmd/goose-daemon/api.go`
- Test: `cmd/goose-daemon/api_test.go`

- [ ] **Step 1: Register the specific route**

In `NewControlPlane`, insert the GC route between `/snapshots` and `/snapshots/`:

```go
	mux.HandleFunc("/snapshots", cp.handleSnapshots)
	mux.HandleFunc("/snapshots/gc", cp.handleSnapshotGC)
	mux.HandleFunc("/snapshots/", cp.handleSnapshotItem)
```

In `Start`, add this log line after the existing `GET /snapshots` log:

```go
	log.Printf("  POST   /snapshots/gc                     — plan/apply snapshot retention GC")
```

- [ ] **Step 2: Add JSON error helper and handler**

Insert this helper near the snapshot handlers:

```go
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
```

Insert this handler above `handleSnapshots`:

```go
// POST /snapshots/gc
func (cp *ControlPlane) handleSnapshotGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req SnapshotGCRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
			return
		}
	}
	if req.OlderThanSeconds < 0 {
		writeJSONError(w, http.StatusBadRequest, "older_than_seconds must be non-negative")
		return
	}
	if req.KeepLastPerVM < 0 {
		writeJSONError(w, http.StatusBadRequest, "keep_last_per_vm must be non-negative")
		return
	}

	policy := SnapshotGCPolicy{
		OlderThanSeconds: req.OlderThanSeconds,
		KeepLastPerVM:    req.KeepLastPerVM,
	}
	resp := cp.planSnapshotGC(policy, time.Now().UTC())
	resp.Applied = req.Apply

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
```

- [ ] **Step 3: Run dry-run endpoint tests**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestHandleSnapshotGC(DryRunDoesNotDelete|RejectsInvalidPolicy)' -count=1
```

Expected: PASS.

- [ ] **Step 4: Run full daemon package tests**

Run:

```bash
go test ./cmd/goose-daemon -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit dry-run endpoint**

```bash
git add cmd/goose-daemon/api.go cmd/goose-daemon/api_test.go
git commit -m "feat: add snapshot gc dry run endpoint"
```

### Task 5: Apply and Delete Tests

**Files:**
- Modify: `cmd/goose-daemon/api_test.go`
- Test: `cmd/goose-daemon/api_test.go`

- [ ] **Step 1: Write apply and delete protection tests**

Append these tests to `cmd/goose-daemon/api_test.go`:

```go
func TestHandleSnapshotGCApplyDeletesCandidates(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)
	addTestSnapshot(t, cp, testSnapshotMeta("snap-old", "vm-1", "full", now.Add(-10*24*time.Hour)))
	addTestSnapshot(t, cp, testSnapshotMeta("snap-new", "vm-1", "full", now.Add(-1*time.Hour)))

	req := httptest.NewRequest(http.MethodPost, "/snapshots/gc", bytes.NewReader([]byte(`{"older_than_seconds":604800,"apply":true}`)))
	rr := httptest.NewRecorder()
	cp.handleSnapshotGC(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	resp := decodeGCResponse(t, rr)
	if !resp.Applied {
		t.Fatal("apply response applied = false, want true")
	}
	if ids := strings.Join(snapshotIDs(resp.Deleted), ","); ids != "snap-old" {
		t.Fatalf("deleted IDs = %s, want snap-old", ids)
	}
	if len(resp.Errors) != 0 {
		t.Fatalf("errors = %#v, want empty", resp.Errors)
	}
	if _, ok := cp.snapshots["snap-old"]; ok {
		t.Fatal("snap-old still exists in map after apply")
	}
	if _, err := os.Stat(storage.SnapshotDir(cp.workDir, "snap-old")); !os.IsNotExist(err) {
		t.Fatalf("snap-old directory stat err = %v, want not exist", err)
	}
	if _, ok := cp.snapshots["snap-new"]; !ok {
		t.Fatal("snap-new was removed from map")
	}
	if _, err := os.Stat(storage.SnapshotDir(cp.workDir, "snap-new")); err != nil {
		t.Fatalf("snap-new directory missing: %v", err)
	}
}

func TestHandleSnapshotGCApplyKeepsReferencedFullUntilNextRun(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)
	full := testSnapshotMeta("snap-full", "vm-1", "full", now.Add(-10*24*time.Hour))
	diff := testSnapshotMeta("snap-diff", "vm-1", "diff", now.Add(-9*24*time.Hour))
	diff.BaseSnapshotID = "snap-full"
	addTestSnapshot(t, cp, full)
	addTestSnapshot(t, cp, diff)

	req := httptest.NewRequest(http.MethodPost, "/snapshots/gc", bytes.NewReader([]byte(`{"older_than_seconds":604800,"apply":true}`)))
	rr := httptest.NewRecorder()
	cp.handleSnapshotGC(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	resp := decodeGCResponse(t, rr)
	if ids := strings.Join(snapshotIDs(resp.Deleted), ","); ids != "snap-diff" {
		t.Fatalf("deleted IDs = %s, want snap-diff", ids)
	}
	if _, ok := cp.snapshots["snap-full"]; !ok {
		t.Fatal("referenced full snapshot was removed in same GC run")
	}
	if _, err := os.Stat(storage.SnapshotDir(cp.workDir, "snap-full")); err != nil {
		t.Fatalf("referenced full snapshot directory missing: %v", err)
	}
}

func TestDeleteSnapshotStillProtectsDiffBase(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)
	full := testSnapshotMeta("snap-full", "vm-1", "full", now.Add(-10*24*time.Hour))
	diff := testSnapshotMeta("snap-diff", "vm-1", "diff", now.Add(-9*24*time.Hour))
	diff.BaseSnapshotID = "snap-full"
	addTestSnapshot(t, cp, full)
	addTestSnapshot(t, cp, diff)

	req := httptest.NewRequest(http.MethodDelete, "/snapshots/snap-full", nil)
	rr := httptest.NewRecorder()
	cp.deleteSnapshot(rr, "snap-full")

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %q; want 409", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "base for diff snapshot snap-diff") {
		t.Fatalf("body = %q, want diff dependency error", rr.Body.String())
	}
	if _, ok := cp.snapshots["snap-full"]; !ok {
		t.Fatal("protected full snapshot was removed from map")
	}
	if req.Method != http.MethodDelete {
		t.Fatalf("request method changed to %s", req.Method)
	}
}
```

- [ ] **Step 2: Run tests to verify apply tests fail**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestHandleSnapshotGCApply|TestDeleteSnapshotStillProtectsDiffBase' -count=1
```

Expected: FAIL with apply tests reporting an empty `Deleted` list or unchanged snapshot map because `apply: true` does not delete candidates yet.

- [ ] **Step 3: Commit the red apply tests**

```bash
git add cmd/goose-daemon/api_test.go
git commit -m "test: define snapshot gc apply behavior"
```

### Task 6: Apply and Delete Implementation

**Files:**
- Modify: `cmd/goose-daemon/api.go`
- Test: `cmd/goose-daemon/api_test.go`

- [ ] **Step 1: Add snapshot delete helper**

Insert this helper above `deleteSnapshot`:

```go
func (cp *ControlPlane) deleteSnapshotByID(snapID string) (storage.SnapshotMetadata, int, error) {
	cp.snapshotsMu.RLock()
	for id, snap := range cp.snapshots {
		if snap.BaseSnapshotID == snapID {
			cp.snapshotsMu.RUnlock()
			return storage.SnapshotMetadata{}, http.StatusConflict, fmt.Errorf("cannot delete: snapshot %s is the base for diff snapshot %s — delete the diff first", snapID, id)
		}
	}
	meta, ok := cp.snapshots[snapID]
	cp.snapshotsMu.RUnlock()
	if !ok {
		return storage.SnapshotMetadata{}, http.StatusNotFound, fmt.Errorf("snapshot not found")
	}

	snapDir := storage.SnapshotDir(cp.workDir, snapID)
	if err := storage.DeleteSnapshot(snapDir); err != nil {
		return meta, http.StatusInternalServerError, fmt.Errorf("delete snapshot dir %s: %w", snapDir, err)
	}

	cp.snapshotsMu.Lock()
	delete(cp.snapshots, snapID)
	cp.snapshotsMu.Unlock()
	return meta, http.StatusOK, nil
}
```

- [ ] **Step 2: Add GC apply helper**

Insert this helper below `handleSnapshotGC`:

```go
func (cp *ControlPlane) applySnapshotGC(resp *SnapshotGCResponse) {
	for _, candidate := range resp.Candidates {
		meta, _, err := cp.deleteSnapshotByID(candidate.SnapshotID)
		if err != nil {
			resp.Errors = append(resp.Errors, SnapshotGCError{
				SnapshotID: candidate.SnapshotID,
				Error:      err.Error(),
			})
			continue
		}
		resp.Deleted = append(resp.Deleted, snapshotGCEntryFrom(meta, candidate.Reason, nil))
	}
}
```

Update `handleSnapshotGC` so it calls the helper before encoding:

```go
	resp := cp.planSnapshotGC(policy, time.Now().UTC())
	resp.Applied = req.Apply
	if req.Apply {
		cp.applySnapshotGC(&resp)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
```

- [ ] **Step 3: Refactor existing DELETE handler**

Replace `deleteSnapshot` with this complete function:

```go
// DELETE /snapshots/{snapshot_id}
func (cp *ControlPlane) deleteSnapshot(w http.ResponseWriter, snapID string) {
	meta, status, err := cp.deleteSnapshotByID(snapID)
	if err != nil {
		if status == http.StatusNotFound {
			http.Error(w, `{"error":"snapshot not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), status)
		return
	}

	log.Printf("Snapshot [%s] (%s, from VM %s) deleted.", snapID, meta.SnapshotType, meta.SourceVMID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "snapshot_id": snapID})
}
```

- [ ] **Step 4: Run apply and delete tests**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestHandleSnapshotGCApply|TestDeleteSnapshotStillProtectsDiffBase' -count=1
```

Expected: PASS.

- [ ] **Step 5: Run daemon package tests**

Run:

```bash
go test ./cmd/goose-daemon -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit apply implementation**

```bash
git add cmd/goose-daemon/api.go cmd/goose-daemon/api_test.go
git commit -m "feat: apply snapshot gc deletions"
```

### Task 7: Documentation Updates

**Files:**
- Modify: `README.md`
- Modify: `RELEASE_NOTES.md`
- Modify: `docs/architecture/service-logic.md`
- Modify: `docs/architecture/runtime-architecture.md`
- Modify: `CONTEXT.md`

- [ ] **Step 1: Update README API list**

In `README.md`, update the daemon API block near the snapshot endpoints so it includes GC:

```text
  GET    /snapshots            -> snapshot 목록
  POST   /snapshots/gc         -> snapshot GC dry-run/apply
  POST   /snapshots/{id}/restore
                                -> snapshot에서 VM 복원
  DELETE /snapshots/{id}       -> snapshot 삭제
```

- [ ] **Step 2: Add README GC usage section**

Insert this section after the existing `### Snapshot 삭제` section in `README.md`:

````markdown
### Snapshot GC dry-run/apply

`POST /snapshots/gc`는 snapshot retention plan을 계산한다. 기본값은 dry-run이며
파일을 삭제하지 않는다.

```bash
curl -X POST http://localhost:3000/snapshots/gc \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $EPHEMERA_API_TOKEN" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1}'
```

실제 삭제는 `apply: true`를 명시해야 수행된다.

```bash
curl -X POST http://localhost:3000/snapshots/gc \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $EPHEMERA_API_TOKEN" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"apply":true}'
```

diff snapshot이 참조 중인 full snapshot은 항상 보호된다. full과 diff가 모두 오래된
경우 첫 GC apply에서는 diff만 삭제되고, 다음 GC 호출에서 full이 삭제 후보가 된다.
````

- [ ] **Step 3: Update RELEASE_NOTES**

Under the top `## 추가됨` section in `RELEASE_NOTES.md`, add:

```markdown
- ephemera daemon `POST /snapshots/gc`: 수동 snapshot retention/GC API.
  - 기본 dry-run mode로 삭제 후보와 보호 사유를 반환한다.
  - `apply: true`일 때만 후보 snapshot directory를 삭제한다.
  - diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
```

- [ ] **Step 4: Update service logic architecture**

In `docs/architecture/service-logic.md`, add the route row in the service boundary table:

```markdown
| `/snapshots/gc` | `cmd/goose-daemon/api.go` | snapshot retention GC dry-run/apply |
```

Insert this section after `## Snapshot 삭제 로직`:

````markdown
## Snapshot GC 로직

Route: `POST /snapshots/gc`

입력:

```json
{
  "older_than_seconds": 604800,
  "keep_last_per_vm": 1,
  "apply": false
}
```

흐름:

```text
handleSnapshotGC()
  -> optional JSON body parse
  -> negative older_than_seconds / keep_last_per_vm 거부
  -> cp.snapshots metadata를 복사해 CreatedAt 기준 정렬
  -> diff snapshot의 base_snapshot_id reverse reference map 생성
  -> referenced full snapshot 보호
  -> source_vm_id별 최신 keep_last_per_vm개 보호
  -> age 조건을 통과하고 보호되지 않은 snapshot을 candidates로 분류
  -> apply=false이면 plan만 반환
  -> apply=true이면 candidates를 storage.DeleteSnapshot으로 삭제하고 cp.snapshots에서 제거
```

중요 invariant:

- `apply` 기본값은 `false`다.
- GC 응답에는 `agent_token`을 포함하지 않는다.
- diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
- 같은 GC 호출 안에서 diff 삭제 후 full 삭제까지 연쇄 수행하지 않는다.
````

- [ ] **Step 5: Update runtime architecture**

In `docs/architecture/runtime-architecture.md`, add this paragraph after the snapshot directory layout:

```markdown
Snapshot directory는 `POST /snapshots/gc`로 수동 정리할 수 있다. GC는 먼저
`cp.snapshots` metadata graph를 기준으로 삭제 후보와 보호 사유를 계산한다.
`apply: false`가 기본값이므로 dry-run은 host disk를 변경하지 않는다.
`apply: true`일 때만 보호되지 않은 후보 snapshot directory를 삭제한다.
diff snapshot이 참조 중인 full snapshot은 base dependency가 사라질 때까지
보호된다.
```

- [ ] **Step 6: Update CONTEXT follow-up list**

In `CONTEXT.md`, update `## 후속 후보` to keep future work accurate:

```markdown
## 후속 후보

- 공개 tag/release 정리: Git tag와 GitHub Release page 상태를 함께 관리
- MCP v2에서 persistent session 지원
- snapshot retention/GC 이후: size 기반 policy, 자동 background GC, retention audit 추가
- multi-host runtime, scheduler, quota, audit storage 추가
```

- [ ] **Step 7: Run docs grep sanity check**

Run:

```bash
rg -n "snapshots/gc|snapshot retention|GC" README.md RELEASE_NOTES.md docs/architecture/service-logic.md docs/architecture/runtime-architecture.md CONTEXT.md
```

Expected: output shows the new API in all five files.

- [ ] **Step 8: Commit documentation**

```bash
git add README.md RELEASE_NOTES.md docs/architecture/service-logic.md docs/architecture/runtime-architecture.md CONTEXT.md
git commit -m "docs: document snapshot gc api"
```

### Task 8: Final Verification

**Files:**
- Verify: `cmd/goose-daemon/api.go`
- Verify: `cmd/goose-daemon/api_test.go`
- Verify: `README.md`
- Verify: `RELEASE_NOTES.md`
- Verify: `docs/architecture/service-logic.md`
- Verify: `docs/architecture/runtime-architecture.md`
- Verify: `CONTEXT.md`

- [ ] **Step 1: Run daemon tests**

Run:

```bash
go test ./cmd/goose-daemon -count=1
```

Expected: PASS.

- [ ] **Step 2: Run all Go tests**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Build daemon**

Run:

```bash
go build ./cmd/goose-daemon
```

Expected: command exits 0 and writes no test failure output.

- [ ] **Step 4: Build MCP adapter**

Run:

```bash
go build ./cmd/anvil-mcp
```

Expected: command exits 0 and writes no test failure output.

- [ ] **Step 5: Check diff for unintended files**

Run:

```bash
git status --short
```

Expected: only pre-existing unrelated workspace changes remain, plus no unstaged files from this GC implementation.

- [ ] **Step 6: Final implementation commit if verification changed generated files**

If verification produced no source changes, do not create an empty commit. If formatting or generated files changed, commit only the relevant GC implementation files:

```bash
git add cmd/goose-daemon/api.go cmd/goose-daemon/api_test.go README.md RELEASE_NOTES.md docs/architecture/service-logic.md docs/architecture/runtime-architecture.md CONTEXT.md
git commit -m "chore: finalize snapshot gc verification"
```

Expected: either no commit is needed, or the commit includes only GC-related files.

## Self-Review

- Spec coverage: endpoint, dry-run, apply, validation, dependency protection, keep-last policy, docs, and verification are covered by Tasks 1-8.
- Type consistency: request/response names are consistent across code snippets and tests: `SnapshotGCRequest`, `SnapshotGCPolicy`, `SnapshotGCEntry`, `SnapshotGCError`, `SnapshotGCResponse`.
- Scope control: no MCP tool, automatic worker, size policy, quota, or multi-host behavior is included.
- Test discipline: each behavior-changing task starts with a failing test command before implementation.
