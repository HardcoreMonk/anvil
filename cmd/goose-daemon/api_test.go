package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	cp := &ControlPlane{
		vms:              make(map[string]*runningVM),
		snapshots:        make(map[string]storage.SnapshotMetadata),
		workDir:          tmp,
		gooseConfigPath:  defaultCfg,
		gooseSecretsPath: defaultSec,
		flockMgr:         orchestrator.NewFlockManager(tmp),
		agentHTTPClient:  &http.Client{Timeout: time.Second},
	}
	cp.metrics = newDaemonMetrics(cp)
	return cp
}

func newTestControlPlaneWithHandler(t *testing.T) *ControlPlane {
	t.Helper()
	tmp := t.TempDir()
	defaultCfg := filepath.Join(tmp, "goose.yaml")
	defaultSec := filepath.Join(tmp, "goose-secrets.yaml")
	if err := os.WriteFile(defaultCfg, []byte("GOOSE_PROVIDER: default\n"), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(defaultSec, []byte("DEFAULT_KEY: x\n"), 0644); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	cp := NewControlPlane(nil, nil, "", "", defaultCfg, defaultSec, tmp, filepath.Join(tmp, "snapshots"))
	cp.clients = []APIClient{{Name: "operator", Token: "secret-token"}}
	cp.agentHTTPClient = &http.Client{Timeout: time.Second}
	return cp
}

func authorizedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Authorization", "Bearer secret-token")
	return req
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

func TestFlockCreateResponseDoesNotExposeAgentTokens(t *testing.T) {
	data, err := json.Marshal(FlockCreateResponse{
		FlockID:      "flock-1",
		Task:         "ship review",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Agents: []*orchestrator.AgentInfo{{
			AgentID:  "worker-1",
			Role:     "worker",
			VMID:     "vm-1",
			AgentURL: "http://10.0.1.2:8080",
			Status:   orchestrator.AgentStatusReady,
		}},
		TownWallURL: "http://127.0.0.1:3000/flocks/flock-1/wall",
		PostURL:     "http://127.0.0.1:3000/flocks/flock-1/post",
	})
	if err != nil {
		t.Fatalf("marshal flock response: %v", err)
	}
	if strings.Contains(string(data), "agent_token") || strings.Contains(string(data), "agent_tokens") {
		t.Fatalf("flock create response exposes agent token field: %s", string(data))
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

func TestCommandEgressEnforcerProfileApplyFailureRollsBackAppliedRules(t *testing.T) {
	profileDir := t.TempDir()
	writeEgressProfileFixture(t, profileDir, "restricted")

	var commands [][]string
	enforcer := &commandEgressEnforcer{
		profileDir: profileDir,
		run: func(name string, args ...string) error {
			command := append([]string{name}, args...)
			commands = append(commands, command)
			if len(commands) == 2 {
				return errors.New("second insert failed")
			}
			return nil
		},
	}

	err := enforcer.ApplyWithProfile("vm-1", "tap-vm-1", "10.0.1.10", "profile", "restricted")
	if err == nil {
		t.Fatal("ApplyWithProfile returned nil error, want command failure")
	}
	if !strings.Contains(err.Error(), "second insert failed") {
		t.Fatalf("error = %v, want apply failure detail", err)
	}
	want := [][]string{
		{"iptables", "-I", "FORWARD", "-s", "10.0.1.10", "-j", "REJECT", "-m", "comment", "--comment", "anvil-egress-vm-1-default"},
		{"iptables", "-I", "FORWARD", "-s", "10.0.1.10", "-d", "203.0.113.10/32", "-j", "ACCEPT", "-m", "comment", "--comment", "anvil-egress-vm-1-cidr-0"},
		{"iptables", "-D", "FORWARD", "-s", "10.0.1.10", "-j", "REJECT", "-m", "comment", "--comment", "anvil-egress-vm-1-default"},
	}
	if len(commands) != len(want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
	for i := range want {
		if strings.Join(commands[i], " ") != strings.Join(want[i], " ") {
			t.Fatalf("command[%d] = %#v, want %#v", i, commands[i], want[i])
		}
	}
	if _, ok := enforcer.rules["vm-1"]; ok {
		t.Fatalf("partial failed apply left egress rule state: %+v", enforcer.rules["vm-1"])
	}
}

func TestCommandEgressEnforcerProfileApplyFailureReportsCleanupFailure(t *testing.T) {
	profileDir := t.TempDir()
	writeEgressProfileFixture(t, profileDir, "restricted")

	enforcer := &commandEgressEnforcer{
		profileDir: profileDir,
		run: func(name string, args ...string) error {
			if len(args) > 0 && args[0] == "-D" {
				return errors.New("cleanup failed")
			}
			if len(args) > 0 && args[0] == "-I" && strings.Contains(strings.Join(args, " "), "cidr-0") {
				return errors.New("insert failed")
			}
			return nil
		},
	}

	err := enforcer.ApplyWithProfile("vm-1", "tap-vm-1", "10.0.1.10", "profile", "restricted")
	if err == nil {
		t.Fatal("ApplyWithProfile returned nil error, want apply and cleanup failure")
	}
	if !strings.Contains(err.Error(), "insert failed") || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("error = %v, want both apply and cleanup failure details", err)
	}
	if _, ok := enforcer.rules["vm-1"]; ok {
		t.Fatalf("partial failed apply left egress rule state: %+v", enforcer.rules["vm-1"])
	}
}

func TestSpawnVMRollbackCleansEgressAndReleasesNetworkOnce(t *testing.T) {
	cp := newTestCP(t)
	workspace := t.TempDir()
	golden := filepath.Join(workspace, "golden.ext4")
	if err := os.WriteFile(golden, []byte("golden"), 0600); err != nil {
		t.Fatalf("write golden image: %v", err)
	}
	cp.provisioner = &storage.Provisioner{GoldenImagePath: golden, WorkspaceDir: workspace}
	var events []string
	recorder := &recordingEgressEnforcer{shared: &events}
	cp.egress = recorder
	cp.allocateNetwork = func() (string, string, string, error) {
		events = append(events, "allocate")
		return "tap-test", "10.0.1.77", "AA:FC:00:00:00:4D", nil
	}
	cp.releaseVMNetwork = func(tapDevice, guestIP string) error {
		events = append(events, "release:"+tapDevice+":"+guestIP)
		return nil
	}
	cp.cloneDisk = func(vmID string) (string, error) {
		events = append(events, "clone-fail")
		return "", errors.New("clone failed")
	}

	_, _, err := cp.spawnVMInternal(spawnVMOptions{
		Profile:      "dev",
		ConfigPath:   cp.gooseConfigPath,
		SecretsPath:  cp.gooseSecretsPath,
		EgressPolicy: "deny_all",
	})
	if err == nil {
		t.Fatal("spawnVMInternal returned nil error, want clone failure")
	}
	gotEvents := strings.Join(events, ",")
	if !strings.HasPrefix(gotEvents, "allocate,apply:") {
		t.Fatalf("events = %s, want allocate then egress apply", gotEvents)
	}
	if !strings.Contains(gotEvents, ",clone-fail,cleanup:") {
		t.Fatalf("events = %s, want clone failure followed by egress cleanup", gotEvents)
	}
	if !strings.HasSuffix(gotEvents, ",release:tap-test:10.0.1.77") {
		t.Fatalf("events = %s, want network release last", gotEvents)
	}
	if strings.Count(gotEvents, "release:") != 1 {
		t.Fatalf("events = %s, want exactly one network release", gotEvents)
	}
}

func TestRecoveryDropsRestoredCOWStateAfterSafeResourceCleanup(t *testing.T) {
	cp := newTestCP(t)
	workspace := t.TempDir()
	cp.provisioner = &storage.Provisioner{WorkspaceDir: workspace}

	vmID := "vm-restored"
	socketPath := filepath.Join(t.TempDir(), "firecracker-vm-restored.sock")
	vsockPath := filepath.Join(t.TempDir(), "vm-restored.vsock")
	if err := storage.SaveVMState(cp.workDir, storage.VMState{
		VMID:             vmID,
		GuestIP:          "10.0.1.99",
		TapDevice:        "tap99",
		MacAddr:          "AA:FC:00:00:00:63",
		SocketPath:       socketPath,
		VsockPath:        vsockPath,
		DiskPath:         filepath.Join(workspace, vmID+".ext4"),
		DiskMode:         storage.DiskModeCOW,
		SourceSnapshotID: "snap-source",
	}); err != nil {
		t.Fatalf("SaveVMState: %v", err)
	}
	memPath, _ := storage.AutoSnapshotPaths(cp.workDir, vmID)
	if err := os.MkdirAll(filepath.Dir(memPath), 0700); err != nil {
		t.Fatalf("mkdir auto snapshot dir: %v", err)
	}
	if err := os.WriteFile(memPath, []byte("memory"), 0600); err != nil {
		t.Fatalf("write auto snapshot: %v", err)
	}

	oldRemoveOrphans := removeOrphanCOWDevices
	oldKillStaleFirecracker := killStaleFirecracker
	oldRemoveStaleVMArtifacts := removeStaleVMArtifacts
	oldRemoveRestoredCOWDevice := removeRestoredCOWDevice
	var events []string
	removeOrphanCOWDevices = func(workspaceDir string, liveVMIDs map[string]struct{}) int {
		events = append(events, "orphan-sweep")
		if workspaceDir != workspace {
			t.Fatalf("workspaceDir = %q, want %q", workspaceDir, workspace)
		}
		if _, ok := liveVMIDs[vmID]; !ok {
			t.Fatalf("restored COW state %q was not protected in initial live set %v", vmID, liveVMIDs)
		}
		return 0
	}
	killStaleFirecracker = func(gotSocketPath string) error {
		events = append(events, "kill")
		if gotSocketPath != socketPath {
			t.Fatalf("KillStaleFirecracker socket = %q, want %q", gotSocketPath, socketPath)
		}
		if !storage.VMStateExists(cp.workDir, vmID) {
			t.Fatal("state was dropped before stale Firecracker cleanup")
		}
		return nil
	}
	removeStaleVMArtifacts = func(gotSocketPath, gotVsockPath, gotLogFifoPath string) {
		events = append(events, "artifacts")
		if gotSocketPath != socketPath {
			t.Fatalf("artifact socket = %q, want %q", gotSocketPath, socketPath)
		}
		if gotVsockPath != vsockPath {
			t.Fatalf("artifact vsock = %q, want %q", gotVsockPath, vsockPath)
		}
		wantLog := fmt.Sprintf("/tmp/fc-%s-log.fifo", vmID)
		if gotLogFifoPath != wantLog {
			t.Fatalf("artifact log fifo = %q, want %q", gotLogFifoPath, wantLog)
		}
		if !storage.VMStateExists(cp.workDir, vmID) {
			t.Fatal("state was dropped before stale artifact cleanup")
		}
	}
	removeRestoredCOWDevice = func(workspaceDir, gotVMID string) {
		events = append(events, "cow-cleanup")
		if workspaceDir != workspace {
			t.Fatalf("restored COW cleanup workspace = %q, want %q", workspaceDir, workspace)
		}
		if gotVMID != vmID {
			t.Fatalf("restored COW cleanup vmID = %q, want %q", gotVMID, vmID)
		}
		if !storage.VMStateExists(cp.workDir, vmID) {
			t.Fatal("state was dropped before restored COW cleanup")
		}
	}
	defer func() {
		removeOrphanCOWDevices = oldRemoveOrphans
		killStaleFirecracker = oldKillStaleFirecracker
		removeStaleVMArtifacts = oldRemoveStaleVMArtifacts
		removeRestoredCOWDevice = oldRemoveRestoredCOWDevice
	}()

	var releases []string
	cp.releaseVMNetwork = func(tapDevice, guestIP string) error {
		events = append(events, "release")
		releases = append(releases, tapDevice+" "+guestIP)
		if !storage.VMStateExists(cp.workDir, vmID) {
			t.Fatal("state was dropped before network release")
		}
		return nil
	}

	recovered, failed, err := cp.RecoverVMs()
	if err != nil {
		t.Fatalf("RecoverVMs: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recovered = %d, want 0", recovered)
	}
	if len(failed) != 1 || failed[0] != vmID {
		t.Fatalf("failed = %v, want [%s]", failed, vmID)
	}
	if len(releases) != 1 || releases[0] != "tap99 10.0.1.99" {
		t.Fatalf("network releases = %v, want restored VM release", releases)
	}
	if got, want := strings.Join(events, ","), "orphan-sweep,kill,artifacts,cow-cleanup,release"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if storage.VMStateExists(cp.workDir, vmID) {
		t.Fatal("restored VM state still exists after recovery drop")
	}
	if storage.AutoSnapshotExists(cp.workDir, vmID) {
		t.Fatal("restored VM auto snapshot still exists after recovery drop")
	}
}

func TestRecoveryNetworkReclaimFailureDropsAutoSnapshot(t *testing.T) {
	cp := newTestCP(t)
	workspace := t.TempDir()
	diskPath := filepath.Join(workspace, "vm-reclaim.ext4")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(diskPath, []byte("rootfs"), 0600); err != nil {
		t.Fatalf("write disk: %v", err)
	}
	vmID := "vm-reclaim"
	if err := storage.SaveVMState(cp.workDir, storage.VMState{
		VMID:      vmID,
		GuestIP:   "10.0.1.55",
		TapDevice: "tap55",
		MacAddr:   "AA:FC:00:00:00:37",
		DiskPath:  diskPath,
		DiskMode:  storage.DiskModePlain,
	}); err != nil {
		t.Fatalf("SaveVMState: %v", err)
	}
	memPath, _ := storage.AutoSnapshotPaths(cp.workDir, vmID)
	if err := os.MkdirAll(filepath.Dir(memPath), 0700); err != nil {
		t.Fatalf("mkdir auto snapshot dir: %v", err)
	}
	if err := os.WriteFile(memPath, []byte("memory"), 0600); err != nil {
		t.Fatalf("write auto snapshot: %v", err)
	}
	cp.reclaimNetwork = func(tapDevice, guestIP, macAddr string) error {
		return errors.New("tap already allocated")
	}

	recovered, failed, err := cp.RecoverVMs()
	if err != nil {
		t.Fatalf("RecoverVMs: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recovered = %d, want 0", recovered)
	}
	if len(failed) != 1 || failed[0] != vmID {
		t.Fatalf("failed = %v, want [%s]", failed, vmID)
	}
	if storage.VMStateExists(cp.workDir, vmID) {
		t.Fatal("VM state still exists after reclaim failure")
	}
	if storage.AutoSnapshotExists(cp.workDir, vmID) {
		t.Fatal("auto snapshot still exists after reclaim failure")
	}
}

func TestRegisterRecoveredVMRecreatesMissingFlockFromVMState(t *testing.T) {
	cp := newTestCP(t)
	state := storage.VMState{
		VMID:         "vm-recovered",
		GuestIP:      "10.0.1.77",
		AgentToken:   "agent-token",
		DiskPath:     filepath.Join(cp.workDir, "vm-recovered.ext4"),
		Profile:      "worker",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		FlockID:      "flock-recovered",
		AgentID:      "worker-1",
		CreatedAt:    time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC),
	}

	cp.registerRecoveredVM(state, nil, nil)

	flock, ok := cp.flockMgr.Get("flock-recovered")
	if !ok {
		t.Fatal("missing flock was not recreated from VM state")
	}
	agents := flock.Snapshot()
	if len(agents) != 1 {
		t.Fatalf("recovered agents = %d, want 1", len(agents))
	}
	agent := agents[0]
	if agent.AgentID != "worker-1" || agent.Role != "worker" || agent.VMID != "vm-recovered" || agent.AgentURL != "http://10.0.1.77:8080" || agent.Status != orchestrator.AgentStatusReady {
		t.Fatalf("recovered agent = %+v, want worker-1/worker/vm-recovered/ready", agent)
	}
	if flock.TenantID != "tenant-1" || flock.EgressPolicy != "profile" {
		t.Fatalf("recovered flock tenant/egress = %q/%q, want tenant-1/profile", flock.TenantID, flock.EgressPolicy)
	}
	meta, err := orchestrator.LoadFlockMetadata(cp.workDir, "flock-recovered")
	if err != nil {
		t.Fatalf("LoadFlockMetadata: %v", err)
	}
	if _, ok := meta.Agents["worker-1"]; !ok {
		t.Fatalf("persisted metadata missing recovered worker-1: %+v", meta.Agents)
	}
	if meta.Agents["worker-1"].AgentURL != "http://10.0.1.77:8080" {
		t.Fatalf("persisted recovered agent_url = %q, want private IP fallback", meta.Agents["worker-1"].AgentURL)
	}
}

func TestRegisterRecoveredVMAddsMissingAgentToExistingFlock(t *testing.T) {
	cp := newTestCP(t)
	flock, err := cp.flockMgr.Create("flock-existing", "existing task", "tenant-1", "profile", filepath.Join(cp.workDir, "flocks", "flock-existing", "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("Create flock: %v", err)
	}
	flock.AddAgent(&orchestrator.AgentInfo{AgentID: "orchestrator-1", Role: "orchestrator", VMID: "vm-orch", Status: orchestrator.AgentStatusReady})
	if err := flock.Persist(cp.workDir); err != nil {
		t.Fatalf("Persist existing flock: %v", err)
	}
	state := storage.VMState{
		VMID:         "vm-worker",
		GuestIP:      "10.0.1.78",
		AgentURL:     "http://10.0.1.78:8080",
		AgentToken:   "agent-token",
		DiskPath:     filepath.Join(cp.workDir, "vm-worker.ext4"),
		Profile:      "worker",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		FlockID:      "flock-existing",
		AgentID:      "worker-1",
		CreatedAt:    time.Date(2026, 7, 3, 1, 2, 3, 0, time.UTC),
	}

	cp.registerRecoveredVM(state, nil, nil)

	agents := map[string]*orchestrator.AgentInfo{}
	for _, agent := range flock.Snapshot() {
		copy := *agent
		agents[agent.AgentID] = &copy
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2: %+v", len(agents), agents)
	}
	worker := agents["worker-1"]
	if worker == nil {
		t.Fatalf("missing recovered worker-1: %+v", agents)
	}
	if worker.Role != "worker" || worker.VMID != "vm-worker" || worker.AgentURL != "http://10.0.1.78:8080" || worker.Status != orchestrator.AgentStatusReady {
		t.Fatalf("recovered worker = %+v, want worker/vm-worker/ready", worker)
	}
	meta, err := orchestrator.LoadFlockMetadata(cp.workDir, "flock-existing")
	if err != nil {
		t.Fatalf("LoadFlockMetadata: %v", err)
	}
	if _, ok := meta.Agents["worker-1"]; !ok {
		t.Fatalf("persisted metadata missing recovered worker-1: %+v", meta.Agents)
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
	cp := newTestCP(t)
	handler := authMiddleware(
		func() []APIClient {
			return []APIClient{{Name: "operator", Token: "secret-token"}}
		},
		cp.metrics.authTotal,
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
	if got := cp.metrics.authTotal.WithLabelValues("denied").Get(); got != 1 {
		t.Fatalf("auth denied count = %v, want 1", got)
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

func writeSnapshotBundleFixture(t *testing.T, workDir string, meta storage.SnapshotMetadata) storage.SnapshotMetadata {
	t.Helper()
	snapDir := storage.SnapshotDir(workDir, meta.SnapshotID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}
	meta.MemFilePath = filepath.Join(snapDir, "memory.bin")
	meta.StatFilePath = filepath.Join(snapDir, "state.bin")
	meta.DiskCopyPath = filepath.Join(snapDir, "rootfs.ext4")
	if meta.Profile == "" {
		meta.Profile = "dev"
	}
	if meta.DiskPath == "" {
		meta.DiskPath = filepath.Join(os.TempDir(), "goose-workspaces", meta.SnapshotID+".ext4")
	}
	if meta.VsockPath == "" {
		meta.VsockPath = filepath.Join(os.TempDir(), "firecracker-vsock-"+meta.SnapshotID+".sock")
	}
	for name, body := range map[string][]byte{
		"memory.bin":  []byte("memory:" + meta.SnapshotID),
		"state.bin":   []byte("state:" + meta.SnapshotID),
		"rootfs.ext4": []byte("rootfs:" + meta.SnapshotID),
	} {
		if err := os.WriteFile(filepath.Join(snapDir, name), body, 0600); err != nil {
			t.Fatalf("write snapshot fixture file %s: %v", name, err)
		}
	}
	if err := storage.SaveMetadata(snapDir, meta); err != nil {
		t.Fatalf("write snapshot metadata: %v", err)
	}
	return meta
}

func exportSnapshotBundleFixture(t *testing.T, workDir, snapshotID string) []byte {
	t.Helper()
	var bundle bytes.Buffer
	if _, err := storage.ExportSnapshotBundle(workDir, snapshotID, &bundle); err != nil {
		t.Fatalf("export snapshot bundle fixture: %v", err)
	}
	return bundle.Bytes()
}

type snapshotExportLockCheckingRecorder struct {
	*httptest.ResponseRecorder
	cp                *ControlPlane
	lockedDuringWrite bool
}

func (r *snapshotExportLockCheckingRecorder) Write(b []byte) (int, error) {
	if r.cp.snapshotLifecycleMu.TryLock() {
		r.cp.snapshotLifecycleMu.Unlock()
	} else {
		r.lockedDuringWrite = true
	}
	return r.ResponseRecorder.Write(b)
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

type recordingEgressEnforcer struct {
	events []string
	shared *[]string
}

func (e *recordingEgressEnforcer) Apply(vmID, tapDevice, guestIP, policy string) error {
	event := "apply:" + vmID + ":" + tapDevice + ":" + guestIP + ":" + policy
	e.events = append(e.events, event)
	if e.shared != nil {
		*e.shared = append(*e.shared, event)
	}
	return nil
}

func (e *recordingEgressEnforcer) Cleanup(vmID string) error {
	event := "cleanup:" + vmID
	e.events = append(e.events, event)
	if e.shared != nil {
		*e.shared = append(*e.shared, event)
	}
	return nil
}

type egressEnforcerFunc struct {
	apply   func(vmID, tapDevice, guestIP, policy string) error
	cleanup func(vmID string) error
}

func (e egressEnforcerFunc) Apply(vmID, tapDevice, guestIP, policy string) error {
	if e.apply != nil {
		return e.apply(vmID, tapDevice, guestIP, policy)
	}
	return nil
}

func (e egressEnforcerFunc) Cleanup(vmID string) error {
	if e.cleanup != nil {
		return e.cleanup(vmID)
	}
	return nil
}

func writeEgressProfileFixture(t *testing.T, baseDir, profileName string) {
	t.Helper()
	profileDir := filepath.Join(baseDir, profileName)
	if err := os.MkdirAll(profileDir, 0700); err != nil {
		t.Fatalf("mkdir egress profile dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "egress.json"), []byte(`{"allow_cidrs":["203.0.113.10/32"]}`), 0600); err != nil {
		t.Fatalf("write egress profile: %v", err)
	}
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

func TestWaitForAgentTimesOutHungHealthProbe(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer agent.Close()

	host, portText, err := net.SplitHostPort(strings.TrimPrefix(agent.URL, "http://"))
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

	start := time.Now()
	err = waitForAgent(host, 500*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForAgent returned nil error for a hung health probe")
	}
	if elapsed > 2800*time.Millisecond {
		t.Fatalf("waitForAgent elapsed = %v, want bounded by per-probe timeout", elapsed)
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

func TestPlanSnapshotGCKeepsSourceSnapshotForRestoredVM(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)

	sourceID := "snap-source"
	addTestSnapshot(t, cp, testSnapshotMeta(sourceID, "vm-source", "full", now.Add(-48*time.Hour)))
	addTestSnapshot(t, cp, testSnapshotMeta("snap-old", "vm-other", "full", now.Add(-72*time.Hour)))
	if err := storage.SaveVMState(cp.workDir, storage.VMState{
		VMID:             "vm-restored",
		DiskMode:         storage.DiskModeCOW,
		SourceSnapshotID: sourceID,
	}); err != nil {
		t.Fatalf("SaveVMState: %v", err)
	}

	got := cp.planSnapshotGC(SnapshotGCPolicy{
		OlderThanSeconds: int64((24 * time.Hour) / time.Second),
		KeepLastPerVM:    0,
	}, now)
	if _, ok := gcEntryByID(got.Candidates, sourceID); ok {
		t.Fatalf("source snapshot %q selected for GC while restored VM references it", sourceID)
	}
	if _, ok := gcEntryByID(got.Candidates, "snap-old"); !ok {
		t.Fatalf("unreferenced old snapshot not selected; candidates=%v", snapshotIDs(got.Candidates))
	}
}

func TestPlanSnapshotGCKeepsLiveRestoredSourceSnapshotWithoutVMState(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)

	sourceID := "snap-source"
	addTestSnapshot(t, cp, testSnapshotMeta(sourceID, "vm-source", "full", now.Add(-48*time.Hour)))
	addTestSnapshot(t, cp, testSnapshotMeta("snap-old", "vm-other", "full", now.Add(-72*time.Hour)))
	cp.vms["vm-restored"] = &runningVM{
		VMInfo:           VMInfo{VMID: "vm-restored"},
		sourceSnapshotID: sourceID,
	}

	got := cp.planSnapshotGC(SnapshotGCPolicy{
		OlderThanSeconds: int64((24 * time.Hour) / time.Second),
		KeepLastPerVM:    0,
	}, now)
	if _, ok := gcEntryByID(got.Candidates, sourceID); ok {
		t.Fatalf("live restored source snapshot %q selected for GC", sourceID)
	}
	if _, ok := gcEntryByID(got.Candidates, "snap-old"); !ok {
		t.Fatalf("unreferenced old snapshot not selected; candidates=%v", snapshotIDs(got.Candidates))
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

func TestApplySnapshotGCRechecksLiveRestoredSourceSnapshot(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-source", "vm-source", "full", now.Add(-72*time.Hour))
	addTestSnapshot(t, cp, meta)
	cp.vms["vm-restored"] = &runningVM{
		VMInfo:           VMInfo{VMID: "vm-restored"},
		sourceSnapshotID: "snap-source",
	}

	resp := SnapshotGCResponse{
		Applied:    true,
		Candidates: []SnapshotGCEntry{snapshotGCEntryFrom(meta, snapshotGCReasonOlderThan, nil, 0)},
	}
	cp.applySnapshotGC(&resp)

	if len(resp.Deleted) != 0 {
		t.Fatalf("deleted = %v, want no deletion of live restored source", snapshotIDs(resp.Deleted))
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Error, "referenced by restored VM") {
		t.Fatalf("errors = %+v, want restored VM dependency error", resp.Errors)
	}
	if _, ok := cp.snapshots["snap-source"]; !ok {
		t.Fatal("live restored source snapshot was removed from snapshot map")
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

func TestHandleSnapshotExportStreamsBundle(t *testing.T) {
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-1", "vm-1", "full", time.Now().UTC())
	meta = writeSnapshotBundleFixture(t, cp.workDir, meta)
	cp.snapshots[meta.SnapshotID] = meta

	req := httptest.NewRequest(http.MethodPost, "/snapshots/snap-1/export", nil)
	rr := httptest.NewRecorder()
	cp.handleSnapshotItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != storage.SnapshotBundleContentType {
		t.Fatalf("content-type = %q, want %q", got, storage.SnapshotBundleContentType)
	}
	result, err := storage.ImportSnapshotBundle(t.TempDir(), bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("import streamed bundle: %v", err)
	}
	if result.SnapshotID != "snap-1" || result.Status != storage.SnapshotImportStatusImported {
		t.Fatalf("import result = %+v, want snap-1 imported", result)
	}
}

func TestHandleSnapshotExportDoesNotHoldLifecycleLockWhileWriting(t *testing.T) {
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-1", "vm-1", "full", time.Now().UTC())
	meta = writeSnapshotBundleFixture(t, cp.workDir, meta)
	cp.snapshots[meta.SnapshotID] = meta

	req := httptest.NewRequest(http.MethodPost, "/snapshots/snap-1/export", nil)
	rr := &snapshotExportLockCheckingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		cp:               cp,
	}
	cp.handleSnapshotItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	if rr.lockedDuringWrite {
		t.Fatal("snapshotLifecycleMu was held while writing export response")
	}
}

func TestHandleSnapshotExportFailureDoesNotReturnBundleResponse(t *testing.T) {
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-corrupt", "vm-1", "full", time.Now().UTC())
	meta = writeSnapshotBundleFixture(t, cp.workDir, meta)
	cp.snapshots[meta.SnapshotID] = meta
	if err := os.Remove(filepath.Join(storage.SnapshotDir(cp.workDir, meta.SnapshotID), "rootfs.ext4")); err != nil {
		t.Fatalf("remove rootfs fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/snapshots/snap-corrupt/export", nil)
	rr := httptest.NewRecorder()
	cp.handleSnapshotItem(rr, req)

	if rr.Code == http.StatusOK {
		t.Fatalf("status = %d, body len = %d; want non-200", rr.Code, rr.Body.Len())
	}
	if got := rr.Header().Get("Content-Type"); strings.HasPrefix(got, storage.SnapshotBundleContentType) {
		t.Fatalf("content-type = %q, want non-bundle error response", got)
	}
}

func TestHandleSnapshotImportPublishesSnapshot(t *testing.T) {
	sourceDir := t.TempDir()
	meta := testSnapshotMeta("snap-import", "vm-1", "full", time.Now().UTC())
	writeSnapshotBundleFixture(t, sourceDir, meta)
	bundle := exportSnapshotBundleFixture(t, sourceDir, "snap-import")

	cp := newTestCP(t)
	req := httptest.NewRequest(http.MethodPost, "/snapshots/import", bytes.NewReader(bundle))
	req.Header.Set("Content-Type", storage.SnapshotBundleContentType)
	rr := httptest.NewRecorder()
	cp.handleSnapshotImport(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %q; want 201", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	got, ok := cp.snapshots["snap-import"]
	if !ok {
		t.Fatal("imported snapshot was not added to cp.snapshots")
	}
	if got.SnapshotID != "snap-import" || got.SnapshotType != "full" {
		t.Fatalf("snapshot metadata = %+v, want snap-import full", got)
	}
	if _, err := os.Stat(storage.SnapshotDir(cp.workDir, "snap-import")); err != nil {
		t.Fatalf("imported snapshot directory missing: %v", err)
	}
}

func TestSnapshotBundleRoutesThroughControlPlaneHandler(t *testing.T) {
	sourceDir := t.TempDir()
	meta := testSnapshotMeta("snap-route", "vm-1", "full", time.Now().UTC())
	writeSnapshotBundleFixture(t, sourceDir, meta)
	bundle := exportSnapshotBundleFixture(t, sourceDir, "snap-route")

	cp := newTestControlPlaneWithHandler(t)
	importReq := authorizedRequest(http.MethodPost, "/snapshots/import", bytes.NewReader(bundle))
	importReq.Header.Set("Content-Type", storage.SnapshotBundleContentType)
	importRR := httptest.NewRecorder()
	cp.srv.Handler.ServeHTTP(importRR, importReq)
	if importRR.Code != http.StatusCreated {
		t.Fatalf("import status = %d, body = %q; want 201", importRR.Code, importRR.Body.String())
	}
	if _, ok := cp.snapshots["snap-route"]; !ok {
		t.Fatal("mux import route did not publish snapshot")
	}

	exportReq := authorizedRequest(http.MethodPost, "/snapshots/snap-route/export", nil)
	exportRR := httptest.NewRecorder()
	cp.srv.Handler.ServeHTTP(exportRR, exportReq)
	if exportRR.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %q; want 200", exportRR.Code, exportRR.Body.String())
	}
	if got := exportRR.Header().Get("Content-Type"); got != storage.SnapshotBundleContentType {
		t.Fatalf("export content-type = %q, want %q", got, storage.SnapshotBundleContentType)
	}
}

func TestHandleSnapshotImportAcceptsParameterizedContentType(t *testing.T) {
	sourceDir := t.TempDir()
	meta := testSnapshotMeta("snap-content-type", "vm-1", "full", time.Now().UTC())
	writeSnapshotBundleFixture(t, sourceDir, meta)
	bundle := exportSnapshotBundleFixture(t, sourceDir, "snap-content-type")

	cp := newTestCP(t)
	req := httptest.NewRequest(http.MethodPost, "/snapshots/import", bytes.NewReader(bundle))
	req.Header.Set("Content-Type", "Application/Vnd.Anvil.Snapshot-Bundle; charset=binary")
	rr := httptest.NewRecorder()
	cp.handleSnapshotImport(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %q; want 201", rr.Code, rr.Body.String())
	}
	if _, ok := cp.snapshots["snap-content-type"]; !ok {
		t.Fatal("imported snapshot was not added to cp.snapshots")
	}
}

func TestHandleSnapshotImportRejectsInvalidContentType(t *testing.T) {
	for _, contentType := range []string{"", "%", "application/octet-stream"} {
		t.Run(contentType, func(t *testing.T) {
			cp := newTestCP(t)
			req := httptest.NewRequest(http.MethodPost, "/snapshots/import", strings.NewReader("not a bundle"))
			req.Header.Set("Content-Type", contentType)
			rr := httptest.NewRecorder()
			cp.handleSnapshotImport(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %q; want 400", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHandleSnapshotImportRejectsDiffWithoutBase(t *testing.T) {
	sourceDir := t.TempDir()
	diff := testSnapshotMeta("snap-diff", "vm-1", "diff", time.Now().UTC())
	diff.BaseSnapshotID = "snap-base"
	writeSnapshotBundleFixture(t, sourceDir, diff)
	bundle := exportSnapshotBundleFixture(t, sourceDir, "snap-diff")

	cp := newTestCP(t)
	req := httptest.NewRequest(http.MethodPost, "/snapshots/import", bytes.NewReader(bundle))
	req.Header.Set("Content-Type", storage.SnapshotBundleContentType)
	rr := httptest.NewRecorder()
	cp.handleSnapshotImport(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %q; want 409", rr.Code, rr.Body.String())
	}
	if _, ok := cp.snapshots["snap-diff"]; ok {
		t.Fatal("diff snapshot was added to cp.snapshots without its base")
	}
	if _, err := os.Stat(storage.SnapshotDir(cp.workDir, "snap-diff")); !os.IsNotExist(err) {
		t.Fatalf("diff snapshot directory stat err = %v, want not exist", err)
	}
}

func TestImportedDiffProtectsBaseSnapshotDelete(t *testing.T) {
	sourceDir := t.TempDir()
	base := testSnapshotMeta("snap-base", "vm-1", "full", time.Now().UTC().Add(-time.Minute))
	writeSnapshotBundleFixture(t, sourceDir, base)
	diff := testSnapshotMeta("snap-diff", "vm-1", "diff", time.Now().UTC())
	diff.BaseSnapshotID = "snap-base"
	writeSnapshotBundleFixture(t, sourceDir, diff)
	baseBundle := exportSnapshotBundleFixture(t, sourceDir, "snap-base")
	diffBundle := exportSnapshotBundleFixture(t, sourceDir, "snap-diff")

	cp := newTestCP(t)
	for _, bundle := range [][]byte{baseBundle, diffBundle} {
		req := httptest.NewRequest(http.MethodPost, "/snapshots/import", bytes.NewReader(bundle))
		req.Header.Set("Content-Type", storage.SnapshotBundleContentType)
		rr := httptest.NewRecorder()
		cp.handleSnapshotImport(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("import status = %d, body = %q; want 201", rr.Code, rr.Body.String())
		}
	}

	rr := httptest.NewRecorder()
	cp.deleteSnapshot(rr, "snap-base")

	if rr.Code != http.StatusConflict {
		t.Fatalf("delete status = %d, body = %q; want 409", rr.Code, rr.Body.String())
	}
	if _, ok := cp.snapshots["snap-base"]; !ok {
		t.Fatal("protected imported base snapshot was removed from map")
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

func TestDeleteSnapshotProtectsLiveRestoredSourceSnapshot(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-source", "vm-source", "full", now.Add(-10*24*time.Hour))
	addTestSnapshot(t, cp, meta)
	cp.vms["vm-restored"] = &runningVM{
		VMInfo:           VMInfo{VMID: "vm-restored"},
		sourceSnapshotID: "snap-source",
	}

	rr := httptest.NewRecorder()
	cp.deleteSnapshot(rr, "snap-source")

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %q; want 409", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "referenced by restored VM vm-restored") {
		t.Fatalf("body = %q, want restored VM dependency error", rr.Body.String())
	}
	if _, ok := cp.snapshots["snap-source"]; !ok {
		t.Fatal("protected source snapshot was removed from map")
	}
}

func TestDeleteSnapshotProtectsPersistedRestoredSourceSnapshot(t *testing.T) {
	now := time.Now().UTC()
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-source", "vm-source", "full", now.Add(-10*24*time.Hour))
	addTestSnapshot(t, cp, meta)
	if err := storage.SaveVMState(cp.workDir, storage.VMState{
		VMID:             "vm-restored",
		SourceSnapshotID: "snap-source",
	}); err != nil {
		t.Fatalf("SaveVMState: %v", err)
	}

	rr := httptest.NewRecorder()
	cp.deleteSnapshot(rr, "snap-source")

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %q; want 409", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "referenced by restored VM vm-restored") {
		t.Fatalf("body = %q, want restored VM state dependency error", rr.Body.String())
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
		// Firecracker LoadSnapshot opens the disk path baked into state.bin at snapshot
		// time (meta.DiskPath); the restored COW device MUST be bind-mounted over that
		// exact path, matching upstream and reRestoreMachine (recovery.go). Per-restore
		// isolation lives in the unique exception store, not the shared mount target.
		if mountTargetPath != meta.DiskPath {
			t.Fatalf("mountTargetPath = %q, want original snapshot DiskPath %q (Firecracker opens the recorded path)", mountTargetPath, meta.DiskPath)
		}
		if !strings.HasPrefix(exceptionStorePath, cp.provisioner.WorkspaceDir) || !strings.HasSuffix(exceptionStorePath, ".cow") {
			t.Fatalf("exceptionStorePath = %q, want per-restore .cow under %q", exceptionStorePath, cp.provisioner.WorkspaceDir)
		}
		dmInfo.MountTarget = mountTargetPath
		return dmInfo, nil
	}
	cp.teardownDMSnapshot = func(info *storage.DMSnapshotInfo) {
		tornDown = info
	}
	cp.restoreMachine = func(ctx context.Context, cfg vm.VMConfig, memFilePath, snapshotPath string) (*firecracker.Machine, error) {
		if cfg.RootfsPath != meta.DiskPath {
			t.Fatalf("RootfsPath = %q, want original snapshot DiskPath %q", cfg.RootfsPath, meta.DiskPath)
		}
		if cfg.VsockUDSPath != meta.VsockPath {
			t.Fatalf("VsockUDSPath = %q, want %q", cfg.VsockUDSPath, meta.VsockPath)
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

func TestRestoreSnapshotDiffMemoryTempRemovedWhenEgressFails(t *testing.T) {
	cp := newTestCP(t)
	cp.provisioner = &storage.Provisioner{WorkspaceDir: t.TempDir()}
	baseID := "snap-base"
	diffID := "snap-diff"
	baseDir := storage.SnapshotDir(cp.workDir, baseID)
	diffDir := storage.SnapshotDir(cp.workDir, diffID)
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		t.Fatalf("mkdir base snapshot: %v", err)
	}
	if err := os.MkdirAll(diffDir, 0700); err != nil {
		t.Fatalf("mkdir diff snapshot: %v", err)
	}
	baseMem := filepath.Join(baseDir, "memory.bin")
	diffMem := filepath.Join(diffDir, "memory.bin")
	if err := os.WriteFile(baseMem, bytes.Repeat([]byte{0x11}, 8192), 0600); err != nil {
		t.Fatalf("write base memory: %v", err)
	}
	if err := os.WriteFile(diffMem, bytes.Repeat([]byte{0x22}, 4096), 0600); err != nil {
		t.Fatalf("write diff memory: %v", err)
	}

	baseMeta := testSnapshotMeta(baseID, "vm-source", "full", time.Now().UTC())
	baseMeta.MemFilePath = baseMem
	baseMeta.DiskCopyPath = filepath.Join(baseDir, "rootfs.ext4")
	cp.snapshots[baseID] = baseMeta

	diffMeta := testSnapshotMeta(diffID, "vm-source", "diff", time.Now().UTC())
	diffMeta.BaseSnapshotID = baseID
	diffMeta.TapDevice = "tap9"
	diffMeta.MacAddr = "AA:FC:00:00:00:09"
	diffMeta.VsockPath = filepath.Join(t.TempDir(), "old.vsock")
	diffMeta.DiskPath = filepath.Join(t.TempDir(), "source.ext4")
	diffMeta.DiskCopyPath = filepath.Join(diffDir, "rootfs.ext4")
	diffMeta.MemFilePath = diffMem
	diffMeta.StatFilePath = filepath.Join(diffDir, "state.bin")
	diffMeta.EgressPolicy = "deny_all"
	cp.snapshots[diffID] = diffMeta

	var mergedMemPath string
	cp.allocateForRestore = func(string, string) (string, string, error) {
		return "tap-restored", "10.0.1.44", nil
	}
	cp.releaseNetwork = func(string, string) error {
		return nil
	}
	cp.setupDMSnapshot = func(baseDiskPath, exceptionStorePath, mountTargetPath string) (*storage.DMSnapshotInfo, error) {
		newVMID := strings.TrimSuffix(filepath.Base(exceptionStorePath), ".cow")
		mergedMemPath = pickMergedMemPath(cp.workDir, newVMID)
		return &storage.DMSnapshotInfo{
			DMDevice:       "dm-test",
			LoopDevice:     "/dev/loop-test-base",
			COWLoopDevice:  "/dev/loop-test-cow",
			ExceptionStore: exceptionStorePath,
			MountTarget:    mountTargetPath,
		}, nil
	}
	cp.teardownDMSnapshot = func(*storage.DMSnapshotInfo) {}
	cp.egress = egressEnforcerFunc{
		apply: func(vmID, tapDevice, guestIP, policy string) error {
			if mergedMemPath == "" {
				t.Fatal("merged memory path was not captured before egress apply")
			}
			if _, err := os.Stat(mergedMemPath); err != nil {
				t.Fatalf("merged memory temp missing before egress failure: %v", err)
			}
			return errors.New("egress failed")
		},
		cleanup: func(vmID string) error { return nil },
	}

	rr := httptest.NewRecorder()
	cp.restoreSnapshot(rr, diffID)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %q; want %d", rr.Code, rr.Body.String(), http.StatusInternalServerError)
	}
	resp := decodeRestoreErrorResponse(t, rr)
	if resp.Code != "egress_policy_failed" {
		t.Fatalf("code = %q, want egress_policy_failed", resp.Code)
	}
	if mergedMemPath == "" {
		t.Fatal("merged memory path was not captured")
	}
	if _, err := os.Stat(mergedMemPath); !os.IsNotExist(err) {
		t.Fatalf("merged memory temp still exists after egress failure: err=%v path=%s", err, mergedMemPath)
	}
}

// TestRestoreSnapshotPersistsRecoverableVMState covers the v0.4.5 behavior change:
// a dm-snapshot restore now persists a recovery record (state.json) carrying
// source_snapshot_id so a daemon restart re-restores the VM from its source
// snapshot (see recoverRestoredVM). This inverts the pre-v0.4.5 anvil guarantee
// (restore left no recoverable state). anvil adaptation: the persisted record also
// carries tenant_id / egress_policy attribution and the snapshot's baked agent
// token — re-restore reloads the snapshot's memory, so the recovered guest carries
// that token, not any post-restore rotation. The wire response still omits tokens
// (see TestVMRestoreResultOmitsAgentToken); the state.json is daemon-private.
func TestRestoreSnapshotPersistsRecoverableVMState(t *testing.T) {
	cp := newTestCP(t)
	cp.provisioner = &storage.Provisioner{WorkspaceDir: t.TempDir()}
	snapshotID := "snap-restore-live"
	meta := testSnapshotMeta(snapshotID, "vm-source", "full", time.Now().UTC())
	meta.GuestIP = "10.0.1.2"
	meta.TapDevice = "tap9"
	meta.MacAddr = "AA:FC:00:00:00:09"
	meta.VsockPath = filepath.Join(t.TempDir(), "old.vsock")
	meta.DiskPath = filepath.Join(t.TempDir(), "source.ext4")
	meta.DiskCopyPath = filepath.Join(t.TempDir(), "rootfs.ext4")
	meta.MemFilePath = filepath.Join(t.TempDir(), "memory.bin")
	meta.StatFilePath = filepath.Join(t.TempDir(), "state.bin")
	meta.AgentToken = "restored-token"
	meta.TenantID = "tenant-1"
	meta.EgressPolicy = "profile"
	cp.snapshots[snapshotID] = meta

	dmInfo := &storage.DMSnapshotInfo{
		DMDevice:       "dm-test",
		LoopDevice:     "/dev/loop-test-base",
		COWLoopDevice:  "/dev/loop-test-cow",
		ExceptionStore: filepath.Join(t.TempDir(), "restore.cow"),
	}
	cp.allocateForRestore = func(string, string) (string, string, error) {
		return "tap-restored", "10.0.1.44", nil
	}
	cp.setupDMSnapshot = func(baseDiskPath, exceptionStorePath, mountTargetPath string) (*storage.DMSnapshotInfo, error) {
		dmInfo.MountTarget = mountTargetPath
		return dmInfo, nil
	}
	cp.restoreMachine = func(ctx context.Context, cfg vm.VMConfig, memFilePath, snapshotPath string) (*firecracker.Machine, error) {
		return &firecracker.Machine{}, nil
	}
	cp.reconfigureGuestIP = func(vsockPath, ipCIDR, gateway string) error {
		return nil
	}
	cp.waitForAgent = func(guestIP string, timeout time.Duration) error {
		return nil
	}

	rr := httptest.NewRecorder()
	cp.restoreSnapshot(rr, snapshotID)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %q; want 201", rr.Code, rr.Body.String())
	}
	var resp VMRestoreResult
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode restore response: %v", err)
	}
	if resp.SourceSnapshotID != snapshotID {
		t.Fatalf("response source_snapshot_id = %q, want %q", resp.SourceSnapshotID, snapshotID)
	}

	restored := cp.vms[resp.VMID]
	if restored == nil {
		t.Fatalf("restored VM %q not registered", resp.VMID)
	}
	if restored.sourceSnapshotID != snapshotID {
		t.Fatalf("sourceSnapshotID = %q, want %q", restored.sourceSnapshotID, snapshotID)
	}

	// v0.4.5: the recovery record must be persisted so RecoverVMs re-restores it.
	if !storage.VMStateExists(cp.workDir, resp.VMID) {
		t.Fatalf("restored VM %q did not persist a recovery state.json", resp.VMID)
	}
	state, err := storage.LoadVMState(cp.workDir, resp.VMID)
	if err != nil {
		t.Fatalf("LoadVMState: %v", err)
	}
	if state.SourceSnapshotID != snapshotID {
		t.Errorf("persisted source_snapshot_id = %q, want %q", state.SourceSnapshotID, snapshotID)
	}
	if state.TenantID != "tenant-1" {
		t.Errorf("persisted tenant_id = %q, want tenant-1", state.TenantID)
	}
	if state.EgressPolicy != "profile" {
		t.Errorf("persisted egress_policy = %q, want profile", state.EgressPolicy)
	}
	if state.AgentToken != "restored-token" {
		t.Errorf("persisted agent_token = %q, want restored-token (snapshot's baked token)", state.AgentToken)
	}
	if state.DiskMode != storage.DiskModeCOW {
		t.Errorf("persisted disk_mode = %q, want %q", state.DiskMode, storage.DiskModeCOW)
	}
}

func TestAgentTokenForRestoredSnapshotUsesExistingToken(t *testing.T) {
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-token", "vm-source", "full", time.Now().UTC())
	meta.AgentToken = "existing-token"
	cp.setGuestAgentToken = func(vsockPath, token string) error {
		t.Fatal("setGuestAgentToken called for existing token")
		return nil
	}

	token, err := cp.agentTokenForRestoredSnapshot("snap-token", meta)
	if err != nil {
		t.Fatalf("agentTokenForRestoredSnapshot returned error: %v", err)
	}
	if token != "existing-token" {
		t.Fatalf("token = %q, want existing-token", token)
	}
}

func TestAgentTokenForRestoredSnapshotInjectsNewTokenWhenMissing(t *testing.T) {
	cp := newTestCP(t)
	meta := testSnapshotMeta("snap-token", "vm-source", "full", time.Now().UTC())
	meta.AgentToken = ""
	meta.VsockPath = filepath.Join(t.TempDir(), "rebased.vsock")

	var gotVsock, gotToken string
	cp.setGuestAgentToken = func(vsockPath, token string) error {
		gotVsock = vsockPath
		gotToken = token
		return nil
	}

	token, err := cp.agentTokenForRestoredSnapshot("snap-token", meta)
	if err != nil {
		t.Fatalf("agentTokenForRestoredSnapshot returned error: %v", err)
	}
	if token == "" {
		t.Fatal("token is empty, want generated token")
	}
	if token != gotToken {
		t.Fatalf("returned token = %q, injected token = %q", token, gotToken)
	}
	if gotVsock != meta.VsockPath {
		t.Fatalf("vsockPath = %q, want %q", gotVsock, meta.VsockPath)
	}
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
		// The bind mount target must be the original recorded disk path (meta.DiskPath)
		// that Firecracker opens on LoadSnapshot; per-restore isolation is the unique
		// newDiskPath bind source, not the target.
		if mountTargetPath != meta.DiskPath {
			t.Fatalf("mountTargetPath = %q, want original snapshot DiskPath %q (Firecracker opens the recorded path)", mountTargetPath, meta.DiskPath)
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

func TestDestroyVMWaitsForSnapshotLifecycleLock(t *testing.T) {
	cp := newTestCP(t)
	done := make(chan struct{})

	cp.snapshotLifecycleMu.Lock()
	go func() {
		defer close(done)
		cp.destroyVM("missing-vm")
	}()

	select {
	case <-done:
		t.Fatal("destroyVM finished while snapshotLifecycleMu was held")
	case <-time.After(50 * time.Millisecond):
	}

	cp.snapshotLifecycleMu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("destroyVM did not finish after snapshotLifecycleMu was released")
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

func TestHandleVMProxiesWorkloadRun(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workloads/run" {
			t.Fatalf("agent path = %q, want /workloads/run", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("Authorization = %q, want Bearer agent-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"script":"workloads/ok.sh","exit_code":0,"stdout":"ok","stderr":"","duration_ms":1,"timed_out":false}`))
	}))
	defer agent.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(agent.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	oldAgentPort := agentPort
	agentPort = parsedPort
	defer func() { agentPort = oldAgentPort }()

	cp := newTestCP(t)
	cp.agentHTTPClient = agent.Client()
	cp.vms["vm-1"] = &runningVM{
		VMInfo: VMInfo{
			VMID:    "vm-1",
			GuestIP: host,
		},
		agentToken: "agent-token",
	}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/workloads/run", strings.NewReader(`{"script":"workloads/ok.sh"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	cp.handleVM(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"exit_code":0`) {
		t.Fatalf("body = %q, want exit_code 0", rr.Body.String())
	}
}

func TestHandleVMWorkloadRunRequiresPost(t *testing.T) {
	cp := newTestCP(t)
	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/workloads/run", nil)
	rr := httptest.NewRecorder()

	cp.handleVM(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}
