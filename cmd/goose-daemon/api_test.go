package main

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

// ---- profileConfigPaths ----

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

func TestProfileConfigPaths_EmptyProfile_ReturnsDefaults(t *testing.T) {
	cp := newTestCP(t)
	cfg, sec, err := cp.profileConfigPaths("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != cp.gooseConfigPath {
		t.Errorf("expected default configPath %q, got %q", cp.gooseConfigPath, cfg)
	}
	if sec != cp.gooseSecretsPath {
		t.Errorf("expected default secretsPath %q, got %q", cp.gooseSecretsPath, sec)
	}
}

func TestProfileConfigPaths_ValidProfile_ReturnsPaths(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "anthropic")
	os.MkdirAll(profileDir, 0755)
	os.WriteFile(filepath.Join(profileDir, "goose.yaml"), []byte("GOOSE_PROVIDER: anthropic\n"), 0644)
	os.WriteFile(filepath.Join(profileDir, "goose-secrets.yaml"), []byte("ANTHROPIC_API_KEY: sk\n"), 0644)

	cfg, sec, err := cp.profileConfigPaths("anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != filepath.Join(profileDir, "goose.yaml") {
		t.Errorf("unexpected configPath: %q", cfg)
	}
	if sec != filepath.Join(profileDir, "goose-secrets.yaml") {
		t.Errorf("unexpected secretsPath: %q", sec)
	}
}

func TestProfileConfigPaths_MissingConfigYaml_Error(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "partial")
	os.MkdirAll(profileDir, 0755)
	// Only goose-secrets.yaml, no goose.yaml
	os.WriteFile(filepath.Join(profileDir, "goose-secrets.yaml"), []byte("KEY: x\n"), 0644)

	_, _, err := cp.profileConfigPaths("partial")
	if err == nil {
		t.Error("expected error for missing goose.yaml")
	}
}

func TestProfileConfigPaths_MissingSecretsYaml_Error(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "partial2")
	os.MkdirAll(profileDir, 0755)
	// Only goose.yaml, no goose-secrets.yaml
	os.WriteFile(filepath.Join(profileDir, "goose.yaml"), []byte("GOOSE_PROVIDER: test\n"), 0644)

	_, _, err := cp.profileConfigPaths("partial2")
	if err == nil {
		t.Error("expected error for missing goose-secrets.yaml")
	}
}

func TestProfileConfigPaths_PathTraversal_Rejected(t *testing.T) {
	cp := newTestCP(t)
	for _, evil := range []string{"../evil", "../../etc", "a/b", `a\b`} {
		_, _, err := cp.profileConfigPaths(evil)
		if err == nil {
			t.Errorf("expected error for path-traversal profile name %q", evil)
		}
	}
}

func TestProfileConfigPaths_DotDot_Rejected(t *testing.T) {
	cp := newTestCP(t)
	_, _, err := cp.profileConfigPaths("..")
	if err == nil {
		t.Error("expected error for profile name '..'")
	}
}

// ---- generateAgentToken ----

func TestGenerateAgentToken_Length(t *testing.T) {
	tok, err := generateAgentToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tok) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Errorf("token is not valid hex: %v", err)
	}
}

func TestGenerateAgentToken_Uniqueness(t *testing.T) {
	a, _ := generateAgentToken()
	b, _ := generateAgentToken()
	if a == b {
		t.Error("two tokens should not be identical (probabilistic)")
	}
}

func TestHandleVMWorkspaceProxiesQueryAuthAndBody(t *testing.T) {
	var gotBody string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/workspace" {
			t.Fatalf("path = %s, want /workspace", r.URL.Path)
		}
		if r.URL.Query().Get("path") != "notes/task.txt" {
			t.Fatalf("query path = %q, want notes/task.txt", r.URL.Query().Get("path"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("Authorization = %q, want Bearer agent-token", got)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotBody = string(data)
		_, _ = w.Write([]byte(`{"path":"notes/task.txt","bytes":5}`))
	}))
	defer agent.Close()

	_, portText, err := net.SplitHostPort(strings.TrimPrefix(agent.URL, "http://"))
	if err != nil {
		t.Fatalf("split agent URL: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse agent port: %v", err)
	}
	oldAgentPort := agentPort
	agentPort = port
	defer func() { agentPort = oldAgentPort }()

	cp := newTestCP(t)
	cp.agentHTTPClient = agent.Client()
	cp.vms["vm-1"] = &runningVM{
		VMInfo: VMInfo{
			VMID:    "vm-1",
			GuestIP: "127.0.0.1",
		},
		agentToken: "agent-token",
	}

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-1/workspace?path=notes/task.txt", strings.NewReader("hello"))
	rr := httptest.NewRecorder()
	cp.handleVM(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	if gotBody != "hello" {
		t.Fatalf("proxied body = %q, want hello", gotBody)
	}
}

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
		KeepLastPerVM:    1,
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
	if _, ok := cp.snapshots["snap-diff"]; ok {
		t.Fatal("snap-diff still exists in map after apply")
	}
	if _, err := os.Stat(storage.SnapshotDir(cp.workDir, "snap-diff")); !os.IsNotExist(err) {
		t.Fatalf("snap-diff directory stat err = %v, want not exist", err)
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
	if _, err := os.Stat(storage.SnapshotDir(cp.workDir, "snap-full")); err != nil {
		t.Fatalf("protected full snapshot directory missing: %v", err)
	}
}
