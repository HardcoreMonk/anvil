package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

	"ephemera/internal/anvilmcp"
	"ephemera/internal/orchestrator"
	"ephemera/internal/storage"
	"ephemera/internal/vm"
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

func TestCreateFlockRejectsInvalidTenantBeforeRegistration(t *testing.T) {
	cp := newTestCP(t)
	cp.flockMgr = orchestrator.NewFlockManager(cp.workDir)
	req := httptest.NewRequest(http.MethodPost, "/flocks", strings.NewReader(`{"task":"ship review","roles":["worker"],"tenant_id":"../bad"}`))
	rr := httptest.NewRecorder()

	cp.createFlock(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /flocks invalid tenant status = %d body = %s, want 400", rr.Code, rr.Body.String())
	}
	if got := len(cp.flockMgr.List()); got != 0 {
		t.Fatalf("registered flocks = %d, want 0 after invalid tenant_id", got)
	}
}

func TestCreateFlockRejectsInvalidEgressPolicyBeforeRegistration(t *testing.T) {
	cp := newTestCP(t)
	cp.flockMgr = orchestrator.NewFlockManager(cp.workDir)
	req := httptest.NewRequest(http.MethodPost, "/flocks", strings.NewReader(`{"task":"ship review","roles":["worker"],"egress_policy":"internet"}`))
	rr := httptest.NewRecorder()

	cp.createFlock(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /flocks invalid egress status = %d body = %s, want 400", rr.Code, rr.Body.String())
	}
	if got := len(cp.flockMgr.List()); got != 0 {
		t.Fatalf("registered flocks = %d, want 0 after invalid egress_policy", got)
	}
}

func TestCreateFlockRejectsInvalidTaskAndRolesBeforeRegistration(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantBody string
	}{
		{
			name:     "blank task",
			body:     `{"task":" ","roles":["worker"]}`,
			wantBody: "task must be non-empty",
		},
		{
			name:     "blank role",
			body:     `{"task":"ship review","roles":[" "]}`,
			wantBody: "roles[0] must be non-empty",
		},
		{
			name:     "slash role",
			body:     `{"task":"ship review","roles":["ops/admin"]}`,
			wantBody: "roles[0] must not contain path separators",
		},
		{
			name:     "backslash role",
			body:     `{"task":"ship review","roles":["ops\\admin"]}`,
			wantBody: "roles[0] must not contain path separators",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newTestCP(t)
			cp.flockMgr = orchestrator.NewFlockManager(cp.workDir)
			req := httptest.NewRequest(http.MethodPost, "/flocks", strings.NewReader(tc.body))
			rr := httptest.NewRecorder()

			cp.createFlock(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("POST /flocks status = %d body = %s, want 400", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), tc.wantBody) {
				t.Fatalf("POST /flocks body = %s, want %q", rr.Body.String(), tc.wantBody)
			}
			if got := len(cp.flockMgr.List()); got != 0 {
				t.Fatalf("registered flocks = %d, want 0 after invalid flock input", got)
			}
		})
	}
}

func TestTenantAPIUpsertsAndGetsTenant(t *testing.T) {
	cp := newTestCP(t)
	req := httptest.NewRequest(http.MethodPut, "/tenants/tenant-1", strings.NewReader(`{"quota":{"active_vms":2,"snapshot_bytes":4096}}`))
	rr := httptest.NewRecorder()
	cp.handleTenantItem(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /tenants/tenant-1 status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/tenants/tenant-1", nil)
	getRR := httptest.NewRecorder()
	cp.handleTenantItem(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET /tenants/tenant-1 status = %d body = %s, want 200", getRR.Code, getRR.Body.String())
	}
	var record TenantRecord
	if err := json.Unmarshal(getRR.Body.Bytes(), &record); err != nil {
		t.Fatalf("decode tenant record: %v", err)
	}
	if record.TenantID != "tenant-1" {
		t.Fatalf("tenant_id = %q, want tenant-1", record.TenantID)
	}
	if record.Quota.ActiveVMs != 2 || record.Quota.SnapshotBytes != 4096 {
		t.Fatalf("quota = %+v, want active=2 snapshot_bytes=4096", record.Quota)
	}
}

func TestTenantAPIListTenants(t *testing.T) {
	cp := newTestCP(t)
	cp.tenantStore = anvilmcp.NewQuotaStore(filepath.Join(cp.workDir, "tenants", "tenants.json"))
	if err := cp.tenantStore.SetTenantQuota("tenant-a", anvilmcp.TenantQuota{ActiveVMs: 1}); err != nil {
		t.Fatalf("SetTenantQuota tenant-a: %v", err)
	}
	if err := cp.tenantStore.SetTenantQuota("tenant-b", anvilmcp.TenantQuota{ActiveVMs: 2}); err != nil {
		t.Fatalf("SetTenantQuota tenant-b: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/tenants", nil)
	rr := httptest.NewRecorder()
	cp.handleTenants(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /tenants status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	var records []TenantRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &records); err != nil {
		t.Fatalf("decode tenant list: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("tenant list len = %d, want 2: %+v", len(records), records)
	}
	if records[0].TenantID != "tenant-a" || records[1].TenantID != "tenant-b" {
		t.Fatalf("tenant order = %+v, want tenant-a then tenant-b", records)
	}
}

func TestTenantAPIRejectsInvalidTenantBeforeMutation(t *testing.T) {
	cp := newTestCP(t)
	req := httptest.NewRequest(http.MethodPut, "/tenants/../bad", strings.NewReader(`{"quota":{"active_vms":2}}`))
	rr := httptest.NewRecorder()
	cp.handleTenantItem(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid tenant status = %d body = %s, want 400", rr.Code, rr.Body.String())
	}
	records := cp.ensureTenantStore().ListTenants()
	if len(records) != 0 {
		t.Fatalf("tenant records = %+v, want empty after invalid request", records)
	}
}

func TestCommandEgressEnforcerDenyAllAndCleanup(t *testing.T) {
	var commands [][]string
	enforcer := &commandEgressEnforcer{
		run: func(name string, args ...string) error {
			commands = append(commands, append([]string{name}, args...))
			return nil
		},
	}
	if err := enforcer.Apply("vm-1", "tap-vm-1", "10.0.1.10", "deny_all"); err != nil {
		t.Fatalf("Apply deny_all error = %v", err)
	}
	if err := enforcer.Cleanup("vm-1"); err != nil {
		t.Fatalf("Cleanup error = %v", err)
	}
	want := [][]string{
		{"iptables", "-I", "FORWARD", "-s", "10.0.1.10", "-j", "REJECT", "-m", "comment", "--comment", "anvil-egress-vm-1"},
		{"iptables", "-D", "FORWARD", "-s", "10.0.1.10", "-j", "REJECT", "-m", "comment", "--comment", "anvil-egress-vm-1"},
	}
	if len(commands) != len(want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	for i := range want {
		if strings.Join(commands[i], " ") != strings.Join(want[i], " ") {
			t.Fatalf("command[%d] = %#v, want %#v", i, commands[i], want[i])
		}
	}
}

func TestCommandEgressEnforcerAllowAllIsNoop(t *testing.T) {
	var calls int
	enforcer := &commandEgressEnforcer{
		run: func(name string, args ...string) error {
			calls++
			return nil
		},
	}
	if err := enforcer.Apply("vm-1", "tap-vm-1", "10.0.1.10", "allow_all"); err != nil {
		t.Fatalf("Apply allow_all error = %v", err)
	}
	if err := enforcer.Cleanup("vm-1"); err != nil {
		t.Fatalf("Cleanup allow_all error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("command calls = %d, want 0", calls)
	}
}

func TestRuntimeAuditAPIListFiltersAndRedacts(t *testing.T) {
	cp := newTestCP(t)
	cp.runtimeAuditPath = filepath.Join(cp.workDir, "audit", "runtime-audit.jsonl")
	if err := anvilmcp.AppendRuntimeAudit(cp.runtimeAuditPath, anvilmcp.RuntimeAuditRecord{
		Timestamp:       time.Date(2026, 5, 14, 1, 0, 0, 0, time.UTC),
		TenantID:        "tenant-1",
		VMID:            "vm-1",
		ToolName:        "anvil_spawn_vm",
		DaemonOperation: "POST /vms",
		ResultCode:      "error",
		Error:           "agent_token=secret must not leak",
	}); err != nil {
		t.Fatalf("append audit tenant-1: %v", err)
	}
	if err := anvilmcp.AppendRuntimeAudit(cp.runtimeAuditPath, anvilmcp.RuntimeAuditRecord{
		Timestamp:       time.Date(2026, 5, 14, 2, 0, 0, 0, time.UTC),
		TenantID:        "tenant-2",
		ToolName:        "anvil_spawn_vm",
		DaemonOperation: "POST /vms",
		ResultCode:      "success",
	}); err != nil {
		t.Fatalf("append audit tenant-2: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/audit/runtime?tenant_id=tenant-1&limit=10", nil)
	rr := httptest.NewRecorder()
	cp.handleRuntimeAudit(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /audit/runtime status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	var resp RuntimeAuditListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode audit response: %v", err)
	}
	if len(resp.Records) != 1 {
		t.Fatalf("records len = %d, want 1: %+v", len(resp.Records), resp.Records)
	}
	if resp.Records[0].TenantID != "tenant-1" {
		t.Fatalf("tenant_id = %q, want tenant-1", resp.Records[0].TenantID)
	}
	if strings.Contains(rr.Body.String(), "agent_token") || strings.Contains(rr.Body.String(), "secret") {
		t.Fatalf("audit response leaked token context: %s", rr.Body.String())
	}
}

func TestRuntimeAuditAPIPrune(t *testing.T) {
	cp := newTestCP(t)
	cp.runtimeAuditPath = filepath.Join(cp.workDir, "audit", "runtime-audit.jsonl")
	for _, tenantID := range []string{"tenant-1", "tenant-2", "tenant-3"} {
		if err := anvilmcp.AppendRuntimeAudit(cp.runtimeAuditPath, anvilmcp.RuntimeAuditRecord{
			TenantID:        tenantID,
			ToolName:        "anvil_spawn_vm",
			DaemonOperation: "POST /vms",
			ResultCode:      "success",
		}); err != nil {
			t.Fatalf("append audit %s: %v", tenantID, err)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/audit/runtime/prune", strings.NewReader(`{"keep_last":2}`))
	rr := httptest.NewRecorder()
	cp.handleRuntimeAuditPrune(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /audit/runtime/prune status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	var resp RuntimeAuditListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode prune response: %v", err)
	}
	if len(resp.Records) != 2 {
		t.Fatalf("records len = %d, want 2: %+v", len(resp.Records), resp.Records)
	}
}

func TestControlPlaneHealthEndpoint(t *testing.T) {
	cp := newTestCP(t)
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{VMID: "vm-1"}}
	cp.snapshots["snap-1"] = storage.SnapshotMetadata{SnapshotID: "snap-1"}
	cp.clients = []APIClient{{Name: "operator", Token: "secret-token"}}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	cp.handleHealth(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	var resp HealthResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if resp.Status != "ok" || resp.VMCount != 1 || resp.SnapshotCount != 1 || !resp.AuthEnabled {
		t.Fatalf("health response = %+v, want ok vm=1 snapshot=1 auth=true", resp)
	}
	if strings.Contains(rr.Body.String(), "secret-token") {
		t.Fatalf("health response leaked token: %s", rr.Body.String())
	}
}

func TestControlPlaneMetricsEndpoint(t *testing.T) {
	cp := newTestCP(t)
	cp.metrics.IncVMCreate()
	cp.metrics.IncVMDelete()
	cp.metrics.IncCleanupFailure()
	cp.metrics.IncAuthFailure()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	cp.handleMetrics(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"anvil_vm_create_total 1",
		"anvil_vm_delete_total 1",
		"anvil_cleanup_failure_total 1",
		"anvil_auth_failure_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "agent_token") || strings.Contains(body, "secret") {
		t.Fatalf("metrics leaked secret context: %s", body)
	}
}

func TestControlPlaneMetricsEndpointIncludesDurationsAndQueueDepth(t *testing.T) {
	cp := newTestCP(t)
	cp.metrics.ObserveDuration("vm_create", 1500*time.Millisecond)
	cp.metrics.ObserveDuration("snapshot_create", 2*time.Second)
	cp.metrics.IncQueueDepth()
	cp.metrics.IncQueueDepth()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	cp.handleMetrics(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"anvil_vm_create_duration_seconds_count 1",
		"anvil_vm_create_duration_seconds_sum 1.500000",
		"anvil_snapshot_create_duration_seconds_count 1",
		"anvil_snapshot_create_duration_seconds_sum 2.000000",
		"anvil_lifecycle_queue_depth 2",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
}

func TestControlPlanePerVMMetricsEndpoint(t *testing.T) {
	cp := newTestCP(t)
	startedAt := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	cp.vms["vm-1"] = &runningVM{
		VMInfo: VMInfo{
			VMID:         "vm-1",
			GuestIP:      "10.0.1.10",
			Profile:      "anthropic",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
		},
		agentToken: "secret-token",
		startedAt:  startedAt,
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics/vms", nil)
	rr := httptest.NewRecorder()
	cp.handleVMMetrics(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /metrics/vms status = %d body = %s, want 200", rr.Code, rr.Body.String())
	}
	var resp []VMMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode VM metrics: %v", err)
	}
	if len(resp) != 1 || resp[0].VMID != "vm-1" || resp[0].TenantID != "tenant-1" || !resp[0].StartedAt.Equal(startedAt) {
		t.Fatalf("vm metrics = %+v, want vm-1 tenant-1 started_at", resp)
	}
	if strings.Contains(rr.Body.String(), "secret-token") || strings.Contains(rr.Body.String(), "agent_token") {
		t.Fatalf("VM metrics leaked token: %s", rr.Body.String())
	}
}

func TestAuthMiddlewareIncrementsAuthFailure(t *testing.T) {
	var metrics controlPlaneMetrics
	handler := authMiddleware(
		func() []APIClient {
			return []APIClient{{Name: "operator", Token: "secret-token"}}
		},
		&metrics,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("handler should not run for unauthorized request")
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s, want 401", rr.Code, rr.Body.String())
	}
	if got := metrics.snapshot().authFailure; got != 1 {
		t.Fatalf("authFailure = %d, want 1", got)
	}
	if strings.Contains(rr.Body.String(), "secret-token") {
		t.Fatalf("auth failure response leaked token: %s", rr.Body.String())
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

func writeSnapshotFile(t *testing.T, cp *ControlPlane, snapshotID, name string, size int) {
	t.Helper()
	path := filepath.Join(storage.SnapshotDir(cp.workDir, snapshotID), name)
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0600); err != nil {
		t.Fatalf("write snapshot file %s: %v", path, err)
	}
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

func decodeRestoreErrorResponse(t *testing.T, rr *httptest.ResponseRecorder) RestoreErrorResponse {
	t.Helper()
	var resp RestoreErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode restore error response %q: %v", rr.Body.String(), err)
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

func TestListVMsIncludesTenantAndEgressPolicy(t *testing.T) {
	cp := newTestCP(t)
	cp.vms["vm-1"] = &runningVM{
		VMInfo: VMInfo{
			VMID:         "vm-1",
			GuestIP:      "10.0.1.2",
			AgentURL:     "http://10.0.1.2:8080",
			Profile:      "dev",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
		},
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	cp.handleVMs(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	var list []VMInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode VM list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("VM list length = %d, want 1", len(list))
	}
	if list[0].TenantID != "tenant-1" || list[0].EgressPolicy != "profile" {
		t.Fatalf("VM info = %+v, want tenant-1/profile", list[0])
	}
}

func TestCreateSnapshotRejectsTenantMismatchBeforePause(t *testing.T) {
	cp := newTestCP(t)
	cp.vms["vm-1"] = &runningVM{
		VMInfo: VMInfo{
			VMID:         "vm-1",
			TenantID:     "tenant-1",
			EgressPolicy: "deny_all",
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/snapshot", strings.NewReader(`{"tenant_id":"tenant-2"}`))
	rr := httptest.NewRecorder()
	cp.createSnapshot(rr, req, "vm-1")

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusForbidden)
	}
	if !strings.Contains(rr.Body.String(), "tenant_id does not match VM tenant") {
		t.Fatalf("body = %q, want tenant mismatch error", rr.Body.String())
	}
}

func TestVMRestoreResultOmitsAgentToken(t *testing.T) {
	data, err := json.Marshal(VMRestoreResult{
		VMInfo: VMInfo{
			VMID:         "vm-restored",
			GuestIP:      "10.0.1.9",
			AgentURL:     "http://10.0.1.9:8080",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
		},
		SourceSnapshotID: "snap-1",
	})
	if err != nil {
		t.Fatalf("marshal restore result: %v", err)
	}
	if strings.Contains(string(data), "agent_token") {
		t.Fatalf("restore result exposes agent_token: %s", string(data))
	}
	if !strings.Contains(string(data), `"tenant_id":"tenant-1"`) {
		t.Fatalf("restore result = %s, want tenant_id", string(data))
	}
}

func TestSnapshotInfoIncludesTenantAndEgressPolicy(t *testing.T) {
	info := snapshotInfoFrom(storage.SnapshotMetadata{
		SnapshotID:   "snap-1",
		SourceVMID:   "vm-1",
		TenantID:     "tenant-1",
		Profile:      "dev",
		EgressPolicy: "deny_all",
		SnapshotType: "full",
		CreatedAt:    time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
	})
	if info.TenantID != "tenant-1" || info.EgressPolicy != "deny_all" {
		t.Fatalf("snapshot info = %+v, want tenant-1/deny_all", info)
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

func TestPlanSnapshotGCMaxTotalBytesSelectsOldestUnprotected(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	cp := newTestCP(t)

	base := testSnapshotMeta("snap-base", "vm-1", "full", now.Add(-6*24*time.Hour))
	old := testSnapshotMeta("snap-old", "vm-2", "full", now.Add(-5*24*time.Hour))
	diff := testSnapshotMeta("snap-diff", "vm-1", "diff", now.Add(-4*24*time.Hour))
	diff.BaseSnapshotID = "snap-base"
	newer := testSnapshotMeta("snap-new", "vm-3", "full", now.Add(-1*time.Hour))

	for _, meta := range []storage.SnapshotMetadata{base, old, diff, newer} {
		addTestSnapshot(t, cp, meta)
	}
	writeSnapshotFile(t, cp, "snap-base", "rootfs.ext4", 20)
	writeSnapshotFile(t, cp, "snap-old", "rootfs.ext4", 8)
	writeSnapshotFile(t, cp, "snap-diff", "memory.bin", 1)
	writeSnapshotFile(t, cp, "snap-new", "rootfs.ext4", 7)

	got := cp.planSnapshotGC(SnapshotGCPolicy{
		OlderThanSeconds: int64((365 * 24 * time.Hour) / time.Second),
		MaxTotalBytes:    34,
	}, now)

	if got.Policy.MaxTotalBytes != 34 {
		t.Fatalf("policy max_total_bytes = %d, want 34", got.Policy.MaxTotalBytes)
	}
	if ids := strings.Join(snapshotIDs(got.Candidates), ","); ids != "snap-old" {
		t.Fatalf("candidate IDs = %s, want snap-old", ids)
	}
	candidate := got.Candidates[0]
	if candidate.Reason != "max_total_bytes" {
		t.Fatalf("candidate reason = %q, want max_total_bytes", candidate.Reason)
	}
	if candidate.SizeBytes != 10 {
		t.Fatalf("candidate size_bytes = %d, want 10", candidate.SizeBytes)
	}

	protected, ok := gcEntryByID(got.Protected, "snap-base")
	if !ok {
		t.Fatal("snap-base was not protected")
	}
	if protected.SizeBytes != 22 {
		t.Fatalf("protected size_bytes = %d, want 22", protected.SizeBytes)
	}
}

func TestPlanSnapshotGCSizeOnlyDoesNotSelectAllAgeCandidates(t *testing.T) {
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	cp := newTestCP(t)

	old := testSnapshotMeta("snap-old", "vm-1", "full", now.Add(-3*time.Hour))
	mid := testSnapshotMeta("snap-mid", "vm-1", "full", now.Add(-2*time.Hour))
	newer := testSnapshotMeta("snap-new", "vm-1", "full", now.Add(-1*time.Hour))
	for _, meta := range []storage.SnapshotMetadata{old, mid, newer} {
		addTestSnapshot(t, cp, meta)
		writeSnapshotFile(t, cp, meta.SnapshotID, "rootfs.ext4", 8)
	}

	got := cp.planSnapshotGC(SnapshotGCPolicy{
		MaxTotalBytes: 15,
	}, now)

	if ids := strings.Join(snapshotIDs(got.Candidates), ","); ids != "snap-old,snap-mid" {
		t.Fatalf("candidate IDs = %s, want snap-old,snap-mid", ids)
	}
	for _, candidate := range got.Candidates {
		if candidate.Reason != "max_total_bytes" {
			t.Fatalf("%s reason = %q, want max_total_bytes", candidate.SnapshotID, candidate.Reason)
		}
		if candidate.SizeBytes != 10 {
			t.Fatalf("%s size_bytes = %d, want 10", candidate.SnapshotID, candidate.SizeBytes)
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
		{
			name:       "negative max_total_bytes",
			body:       `{"max_total_bytes":-1}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   "max_total_bytes must be non-negative",
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

func TestHandleSnapshotGCAuditWrittenOnlyOnApply(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-old", "vm-1", "full", now.Add(-10*24*time.Hour))
	meta.AgentToken = "secret-token"
	addTestSnapshot(t, cp, meta)
	addTestSnapshot(t, cp, testSnapshotMeta("snap-new", "vm-1", "full", now.Add(-1*time.Hour)))
	auditPath := filepath.Join(cp.workDir, "snapshots", "gc-audit.jsonl")

	dryRunReq := httptest.NewRequest(http.MethodPost, "/snapshots/gc", bytes.NewReader([]byte(`{"older_than_seconds":604800}`)))
	dryRunRR := httptest.NewRecorder()
	cp.handleSnapshotGC(dryRunRR, dryRunReq)

	if dryRunRR.Code != http.StatusOK {
		t.Fatalf("dry-run status = %d, body = %q; want 200", dryRunRR.Code, dryRunRR.Body.String())
	}
	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run audit stat err = %v, want not exist", err)
	}

	applyReq := httptest.NewRequest(http.MethodPost, "/snapshots/gc", bytes.NewReader([]byte(`{"older_than_seconds":604800,"apply":true}`)))
	applyRR := httptest.NewRecorder()
	cp.handleSnapshotGC(applyRR, applyReq)

	if applyRR.Code != http.StatusOK {
		t.Fatalf("apply status = %d, body = %q; want 200", applyRR.Code, applyRR.Body.String())
	}
	resp := decodeGCResponse(t, applyRR)
	if len(resp.Candidates) != 1 || len(resp.Deleted) != 1 || len(resp.Errors) != 0 {
		t.Fatalf("gc counts candidates=%d deleted=%d errors=%d, want 1/1/0", len(resp.Candidates), len(resp.Deleted), len(resp.Errors))
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	if strings.Contains(string(data), "secret-token") || strings.Contains(string(data), "agent_token") {
		t.Fatalf("audit file includes sensitive metadata: %q", string(data))
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1: %q", len(lines), string(data))
	}
	var record struct {
		Applied         bool `json:"applied"`
		CandidatesCount int  `json:"candidates_count"`
		DeletedCount    int  `json:"deleted_count"`
		ErrorsCount     int  `json:"errors_count"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("parse audit line: %v", err)
	}
	if !record.Applied || record.CandidatesCount != 1 || record.DeletedCount != 1 || record.ErrorsCount != 0 {
		t.Fatalf("audit record = %+v, want applied=true counts 1/1/0", record)
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

func TestCreateSnapshotRejectsMalformedJSONWithJSONError(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/snapshot", strings.NewReader("{"))

	cp.createSnapshot(rr, req, "vm-1")

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusBadRequest)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(rr.Body.String(), "invalid JSON body") {
		t.Fatalf("body = %q, want invalid JSON body", rr.Body.String())
	}
}

func TestRestoreSnapshotMissingSnapshotReturnsJSONError(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()

	cp.restoreSnapshot(rr, "missing-snapshot")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusNotFound)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	resp := decodeRestoreErrorResponse(t, rr)
	if resp.Code != "snapshot_not_found" {
		t.Fatalf("code = %q, want snapshot_not_found", resp.Code)
	}
	if resp.SourceSnapshotID != "missing-snapshot" {
		t.Fatalf("source_snapshot_id = %q, want missing-snapshot", resp.SourceSnapshotID)
	}
	if resp.Error != "snapshot not found" {
		t.Fatalf("error = %q, want snapshot not found", resp.Error)
	}
}

func TestRestoreSnapshotRejectsTenantMismatchBeforeNetworkAllocation(t *testing.T) {
	cp := newTestCP(t)
	cp.provisioner = &storage.Provisioner{WorkspaceDir: t.TempDir()}
	snapshotID := "snap-tenant"
	meta := testSnapshotMeta(snapshotID, "vm-source", "full", time.Now().UTC())
	meta.TenantID = "tenant-1"
	meta.EgressPolicy = "profile"
	cp.snapshots[snapshotID] = meta
	cp.allocateForRestore = func(string, string) (string, string, error) {
		t.Fatal("allocateForRestore called before tenant mismatch rejection")
		return "", "", nil
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/snapshots/snap-tenant/restore", strings.NewReader(`{"tenant_id":"tenant-2"}`))
	cp.restoreSnapshotFromRequest(rr, req, snapshotID)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusForbidden)
	}
	resp := decodeRestoreErrorResponse(t, rr)
	if resp.Code != "tenant_mismatch" {
		t.Fatalf("code = %q, want tenant_mismatch", resp.Code)
	}
	if !strings.Contains(resp.Error, "tenant_id does not match snapshot tenant") {
		t.Fatalf("error = %q, want tenant mismatch", resp.Error)
	}
}

func TestRestoreSnapshotFirecrackerFailureCleansNetworkAndDMSnapshot(t *testing.T) {
	cp := newTestCP(t)
	cp.provisioner = &storage.Provisioner{WorkspaceDir: t.TempDir()}
	snapshotID := "snap-firecracker-fail"
	meta := testSnapshotMeta(snapshotID, "vm-source", "full", time.Now().UTC())
	meta.GuestIP = "10.0.1.2"
	meta.TapDevice = "tap9"
	meta.MacAddr = "AA:FC:00:00:00:09"
	meta.VsockPath = filepath.Join(t.TempDir(), "old.vsock")
	meta.DiskPath = filepath.Join(t.TempDir(), "source.ext4")
	meta.DiskCopyPath = filepath.Join(t.TempDir(), "rootfs.ext4")
	meta.MemFilePath = filepath.Join(t.TempDir(), "memory.bin")
	meta.StatFilePath = filepath.Join(t.TempDir(), "state.bin")
	cp.snapshots[snapshotID] = meta

	dmInfo := &storage.DMSnapshotInfo{
		DMDevice:       "dm-test",
		LoopDevice:     "/dev/loop-test-base",
		COWLoopDevice:  "/dev/loop-test-cow",
		ExceptionStore: filepath.Join(t.TempDir(), "restore.cow"),
		MountTarget:    meta.DiskPath,
	}
	var releasedTap, releasedIP string
	var tornDown *storage.DMSnapshotInfo

	cp.allocateForRestore = func(tapDeviceName, macAddr string) (string, string, error) {
		if tapDeviceName != meta.TapDevice {
			t.Fatalf("tapDeviceName = %q, want %q", tapDeviceName, meta.TapDevice)
		}
		if macAddr != meta.MacAddr {
			t.Fatalf("macAddr = %q, want %q", macAddr, meta.MacAddr)
		}
		return "tap-restored", "10.0.1.44", nil
	}
	cp.releaseNetwork = func(tapDevice, guestIP string) error {
		releasedTap = tapDevice
		releasedIP = guestIP
		return nil
	}
	cp.setupDMSnapshot = func(baseDiskPath, exceptionStorePath, mountTargetPath string) (*storage.DMSnapshotInfo, error) {
		if baseDiskPath != meta.DiskCopyPath {
			t.Fatalf("baseDiskPath = %q, want %q", baseDiskPath, meta.DiskCopyPath)
		}
		if mountTargetPath != meta.DiskPath {
			t.Fatalf("mountTargetPath = %q, want %q", mountTargetPath, meta.DiskPath)
		}
		return dmInfo, nil
	}
	cp.teardownDMSnapshot = func(info *storage.DMSnapshotInfo) {
		tornDown = info
	}
	cp.restoreMachine = func(ctx context.Context, cfg vm.VMConfig, memFilePath, snapshotPath string) (*firecracker.Machine, error) {
		if cfg.RootfsPath != meta.DiskPath {
			t.Fatalf("RootfsPath = %q, want %q", cfg.RootfsPath, meta.DiskPath)
		}
		if cfg.TapDevice != "tap-restored" {
			t.Fatalf("TapDevice = %q, want tap-restored", cfg.TapDevice)
		}
		if cfg.GuestIP != "10.0.1.44" {
			t.Fatalf("GuestIP = %q, want 10.0.1.44", cfg.GuestIP)
		}
		if memFilePath != meta.MemFilePath {
			t.Fatalf("memFilePath = %q, want %q", memFilePath, meta.MemFilePath)
		}
		if snapshotPath != meta.StatFilePath {
			t.Fatalf("snapshotPath = %q, want %q", snapshotPath, meta.StatFilePath)
		}
		return nil, errors.New("restore failed")
	}

	rr := httptest.NewRecorder()
	cp.restoreSnapshot(rr, snapshotID)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusInternalServerError)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	resp := decodeRestoreErrorResponse(t, rr)
	if resp.Code != "firecracker_restore_failed" {
		t.Fatalf("code = %q, want firecracker_restore_failed", resp.Code)
	}
	if resp.SourceSnapshotID != snapshotID {
		t.Fatalf("source_snapshot_id = %q, want %q", resp.SourceSnapshotID, snapshotID)
	}
	if releasedTap != "tap-restored" || releasedIP != "10.0.1.44" {
		t.Fatalf("released network = (%q, %q), want (tap-restored, 10.0.1.44)", releasedTap, releasedIP)
	}
	if tornDown != dmInfo {
		t.Fatalf("torn down dm snapshot = %#v, want %#v", tornDown, dmInfo)
	}
	if !cp.restoreMu.TryLock() {
		t.Fatal("restoreMu remained locked after restore failure")
	}
	cp.restoreMu.Unlock()
	if !cp.snapshotLifecycleMu.TryLock() {
		t.Fatal("snapshotLifecycleMu remained locked after restore failure")
	}
	cp.snapshotLifecycleMu.Unlock()
}

func TestRestoreSnapshotDMSnapshotFallbackReleasesNetworkOnlyAfterBindMountFailure(t *testing.T) {
	cp := newTestCP(t)
	cp.provisioner = &storage.Provisioner{WorkspaceDir: t.TempDir()}
	snapshotID := "snap-bind-fallback-fail"
	meta := testSnapshotMeta(snapshotID, "vm-source", "full", time.Now().UTC())
	meta.TapDevice = "tap9"
	meta.MacAddr = "AA:FC:00:00:00:09"
	meta.VsockPath = filepath.Join(t.TempDir(), "old.vsock")
	meta.DiskPath = filepath.Join(t.TempDir(), "source.ext4")
	meta.DiskCopyPath = filepath.Join(t.TempDir(), "rootfs.ext4")
	meta.MemFilePath = filepath.Join(t.TempDir(), "memory.bin")
	meta.StatFilePath = filepath.Join(t.TempDir(), "state.bin")
	cp.snapshots[snapshotID] = meta

	events := []string{}
	cp.allocateForRestore = func(string, string) (string, string, error) {
		events = append(events, "allocate")
		return "tap-restored", "10.0.1.44", nil
	}
	cp.setupDMSnapshot = func(string, string, string) (*storage.DMSnapshotInfo, error) {
		events = append(events, "dm-fail")
		return nil, errors.New("dm unavailable")
	}
	cp.setupBindMount = func(baseDiskPath, newDiskPath, mountTargetPath string) error {
		events = append(events, "bind-fail")
		if baseDiskPath != meta.DiskCopyPath {
			t.Fatalf("baseDiskPath = %q, want %q", baseDiskPath, meta.DiskCopyPath)
		}
		if !strings.HasPrefix(newDiskPath, cp.provisioner.WorkspaceDir) {
			t.Fatalf("newDiskPath = %q, want under %q", newDiskPath, cp.provisioner.WorkspaceDir)
		}
		if mountTargetPath != meta.DiskPath {
			t.Fatalf("mountTargetPath = %q, want %q", mountTargetPath, meta.DiskPath)
		}
		return errors.New("bind unavailable")
	}
	cp.releaseNetwork = func(tapDevice, guestIP string) error {
		events = append(events, "release")
		if tapDevice != "tap-restored" || guestIP != "10.0.1.44" {
			t.Fatalf("released network = (%q, %q), want (tap-restored, 10.0.1.44)", tapDevice, guestIP)
		}
		return nil
	}

	rr := httptest.NewRecorder()
	cp.restoreSnapshot(rr, snapshotID)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusInternalServerError)
	}
	resp := decodeRestoreErrorResponse(t, rr)
	if resp.Code != "firecracker_restore_failed" {
		t.Fatalf("code = %q, want firecracker_restore_failed", resp.Code)
	}
	if got := strings.Join(events, ","); got != "allocate,dm-fail,bind-fail,release" {
		t.Fatalf("events = %s, want allocate,dm-fail,bind-fail,release", got)
	}
	if !cp.restoreMu.TryLock() {
		t.Fatal("restoreMu remained locked after bind fallback failure")
	}
	cp.restoreMu.Unlock()
	if !cp.snapshotLifecycleMu.TryLock() {
		t.Fatal("snapshotLifecycleMu remained locked after bind fallback failure")
	}
	cp.snapshotLifecycleMu.Unlock()
}

func TestRestoreSnapshotUsesSnapshotLifecycleLock(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	done := make(chan struct{})

	cp.snapshotLifecycleMu.Lock()
	go func() {
		defer close(done)
		cp.restoreSnapshot(rr, "missing-snapshot")
	}()

	select {
	case <-done:
		t.Fatal("restoreSnapshot finished while snapshotLifecycleMu was held")
	case <-time.After(50 * time.Millisecond):
	}

	cp.snapshotLifecycleMu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("restoreSnapshot did not finish after snapshotLifecycleMu was released")
	}
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusNotFound)
	}
}

func TestDeleteSnapshotFailureDoesNotExposeSnapshotPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based delete failure is not reliable as root")
	}

	now := time.Now().UTC().Add(-2 * time.Hour)
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-fail", "vm-1", "full", now)
	addTestSnapshot(t, cp, meta)

	snapshotsDir := filepath.Join(cp.workDir, "snapshots")
	snapDir := storage.SnapshotDir(cp.workDir, "snap-fail")
	if err := os.Chmod(snapshotsDir, 0555); err != nil {
		t.Fatalf("chmod snapshots dir read-only: %v", err)
	}
	defer os.Chmod(snapshotsDir, 0755)

	rr := httptest.NewRecorder()
	cp.deleteSnapshot(rr, "snap-fail")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusInternalServerError)
	}
	if !strings.Contains(rr.Body.String(), "failed to delete snapshot snap-fail") {
		t.Fatalf("body = %q, want sanitized delete failure", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), cp.workDir) || strings.Contains(rr.Body.String(), snapDir) {
		t.Fatalf("body = %q, must not expose snapshot path %q", rr.Body.String(), snapDir)
	}

	req := httptest.NewRequest(http.MethodPost, "/snapshots/gc", strings.NewReader(`{"older_than_seconds":0,"apply":true}`))
	gcRR := httptest.NewRecorder()
	cp.handleSnapshotGC(gcRR, req)
	if gcRR.Code != http.StatusOK {
		t.Fatalf("gc status = %d, body = %q; want %d", gcRR.Code, gcRR.Body.String(), http.StatusOK)
	}
	if !strings.Contains(gcRR.Body.String(), "failed to delete snapshot snap-fail") {
		t.Fatalf("gc body = %q, want sanitized delete failure", gcRR.Body.String())
	}
	if strings.Contains(gcRR.Body.String(), cp.workDir) || strings.Contains(gcRR.Body.String(), snapDir) {
		t.Fatalf("gc body = %q, must not expose snapshot path %q", gcRR.Body.String(), snapDir)
	}
}
