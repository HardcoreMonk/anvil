package anvilmcp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type routerFakeDaemon struct {
	spawnCalls           int
	spawnReq             SpawnVMRequest
	spawnResp            *SpawnVMResponse
	spawnErr             error
	runTaskCalls         int
	runTaskVMID          string
	healthCalls          int
	healthVMID           string
	createSnapshotCalls  int
	createSnapshotVMID   string
	restoreSnapshotCalls int
	restoreSnapshotID    string
	restoreResp          *RestoreSnapshotResponse
	restoreErr           error
	deleteCalls          int
	deleteVMID           string
	listVMResp           []VMInfo
	snapshotList         []SnapshotInfo
	listSnapshotErr      error
	exportCalls          []string
	exportBodies         map[string]string
	exportStreams        map[string]io.ReadCloser
	exportErr            error
	importCalls          []string
	importErrForBody     map[string]error
	importStatusForBody  map[string]string
}

func (f *routerFakeDaemon) SpawnVM(_ context.Context, req SpawnVMRequest) (*SpawnVMResponse, error) {
	f.spawnCalls++
	f.spawnReq = req
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	if f.spawnResp != nil {
		return f.spawnResp, nil
	}
	return &SpawnVMResponse{VMID: "vm-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: req.TenantID, EgressPolicy: req.EgressPolicy}, nil
}

func (f *routerFakeDaemon) RunTask(_ context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	f.runTaskCalls++
	f.runTaskVMID = vmID
	return &RawDaemonResponse{StatusCode: 200, Body: `{"output":"ok"}`}, nil
}

func (f *routerFakeDaemon) CopyIn(context.Context, string, string, string, bool) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *routerFakeDaemon) CopyOut(context.Context, string, string) (string, error) {
	return "content", nil
}

func (f *routerFakeDaemon) Health(_ context.Context, vmID string) (*RawDaemonResponse, error) {
	f.healthCalls++
	f.healthVMID = vmID
	return &RawDaemonResponse{StatusCode: 200, Body: `{"status":"ok"}`}, nil
}

func (f *routerFakeDaemon) Stop(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *routerFakeDaemon) Delete(_ context.Context, vmID string) (*RawDaemonResponse, error) {
	f.deleteCalls++
	f.deleteVMID = vmID
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *routerFakeDaemon) CreateSnapshot(_ context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	f.createSnapshotCalls++
	f.createSnapshotVMID = vmID
	return &SnapshotInfo{SnapshotID: "snap-1", SourceVMID: vmID, TenantID: req.TenantID}, nil
}

func (f *routerFakeDaemon) ListSnapshots(context.Context) ([]SnapshotInfo, error) {
	if f.listSnapshotErr != nil {
		return nil, f.listSnapshotErr
	}
	return append([]SnapshotInfo(nil), f.snapshotList...), nil
}

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
	if f.exportStreams != nil {
		if configured, ok := f.exportStreams[snapshotID]; ok {
			return &SnapshotExportStream{Body: configured, ContentType: "application/vnd.anvil.snapshot-bundle"}, nil
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

func (f *routerFakeDaemon) RestoreSnapshot(_ context.Context, snapshotID string, req RestoreSnapshotRequest) (*RestoreSnapshotResponse, error) {
	f.restoreSnapshotCalls++
	f.restoreSnapshotID = snapshotID
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	if f.restoreResp != nil {
		return f.restoreResp, nil
	}
	return &RestoreSnapshotResponse{VMID: "vm-restored", GuestIP: "10.0.1.20", AgentURL: "http://10.0.1.20:8080", TenantID: req.TenantID, EgressPolicy: req.EgressPolicy, SourceSnapshotID: snapshotID}, nil
}

func (f *routerFakeDaemon) DeleteSnapshot(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *routerFakeDaemon) ListVMs(context.Context) ([]VMInfo, error) {
	return f.listVMResp, nil
}

type routerFlockFakeDaemon struct {
	routerFakeDaemon
	createFlockCalls int
	createFlockReq   FlockCreateRequest
	createFlockResp  *FlockCreateResponse
	createFlockErr   error
}

func (f *routerFlockFakeDaemon) CreateFlock(_ context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	f.createFlockCalls++
	f.createFlockReq = req
	if f.createFlockErr != nil {
		return nil, f.createFlockErr
	}
	if f.createFlockResp != nil {
		return f.createFlockResp, nil
	}
	return &FlockCreateResponse{FlockID: "flock-1", Agents: []FlockAgentInfo{}}, nil
}

func TestRuntimeRouterRejectsQuotaBeforeDaemonCall(t *testing.T) {
	daemon := &routerFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler([]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}}, map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}}, map[string]TenantUsage{"tenant-1": {ActiveVMs: 1}}),
		map[string]Daemon{"host-a": daemon},
	)

	_, err := router.SpawnVM(context.Background(), SpawnVMRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, TenantUsage{ActiveVMs: 1})
	if err == nil {
		t.Fatal("SpawnVM error = nil, want quota denial")
	}
	var denied *ScheduleDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error type = %T, want ScheduleDeniedError", err)
	}
	if denied.Decision.Reason != "quota_exceeded" {
		t.Fatalf("denial reason = %q, want quota_exceeded", denied.Decision.Reason)
	}
	if daemon.spawnCalls != 0 {
		t.Fatalf("daemon spawn calls = %d, want 0", daemon.spawnCalls)
	}
}

func TestRuntimeRouterCreateFlockSchedulesByRoleCountAndRecordsPlacements(t *testing.T) {
	hostA := &routerFlockFakeDaemon{}
	hostB := &routerFlockFakeDaemon{createFlockResp: &FlockCreateResponse{
		FlockID: "flock-1",
		Agents: []FlockAgentInfo{
			{AgentID: "agent-worker", Role: "worker", VMID: "vm-worker-1", Status: "running"},
			{AgentID: "agent-reviewer", Role: "reviewer", VMID: "vm-reviewer-1", Status: "running"},
		},
	}}
	router := NewRuntimeRouter(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
	)

	resp, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     " tenant-1 ",
		EgressPolicy: " PROFILE ",
	})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if resp.FlockID != "flock-1" {
		t.Fatalf("flock id = %q, want flock-1", resp.FlockID)
	}
	if hostA.createFlockCalls != 0 || hostB.createFlockCalls != 1 {
		t.Fatalf("CreateFlock calls hostA/hostB = %d/%d, want 0/1", hostA.createFlockCalls, hostB.createFlockCalls)
	}
	if hostB.createFlockReq.TenantID != "tenant-1" {
		t.Fatalf("daemon tenant = %q, want tenant-1", hostB.createFlockReq.TenantID)
	}
	if hostB.createFlockReq.EgressPolicy != "profile" {
		t.Fatalf("daemon egress policy = %q, want profile", hostB.createFlockReq.EgressPolicy)
	}
	for _, vmID := range []string{"vm-worker-1", "vm-reviewer-1"} {
		if host, ok := router.Placement(vmID); !ok || host != "host-b" {
			t.Fatalf("placement for %s = %q,%v want host-b,true", vmID, host, ok)
		}
	}
}

func TestRuntimeRouterCreateFlockRejectsQuotaBeforeDaemonCall(t *testing.T) {
	daemon := &routerFlockFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}},
			map[string]TenantUsage{"tenant-1": {ActiveVMs: 0}},
		),
		map[string]Daemon{"host-a": daemon},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateFlock error = nil, want quota denial")
	}
	var denied *ScheduleDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error type = %T, want ScheduleDeniedError", err)
	}
	if denied.Decision.Reason != "quota_exceeded" {
		t.Fatalf("denial reason = %q, want quota_exceeded", denied.Decision.Reason)
	}
	if daemon.createFlockCalls != 0 {
		t.Fatalf("daemon CreateFlock calls = %d, want 0", daemon.createFlockCalls)
	}
}

func TestRuntimeRouterCreateFlockReportsPlacementSaveFailureWithoutSecrets(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	daemon := &routerFlockFakeDaemon{createFlockResp: &FlockCreateResponse{
		FlockID: "flock-save-failure",
		Agents: []FlockAgentInfo{
			{AgentID: "agent-worker", Role: "worker", VMID: "vm-worker-1", AgentURL: "http://secret-token.example/agent", Status: "running"},
		},
		TownWallURL: "http://secret-token.example/townwall",
		PostURL:     "http://secret-token.example/post",
	}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateFlock error = nil, want placement save failure")
	}
	text := err.Error()
	if !strings.Contains(text, "flock created but placement save failed") {
		t.Fatalf("error = %q, want placement save failure context", text)
	}
	if !strings.Contains(text, "flock-save-failure") {
		t.Fatalf("error = %q, want flock id", text)
	}
	for _, forbidden := range []string{"agent_token", "Authorization", "Bearer", "secret-token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("error contains forbidden secret marker %q: %q", forbidden, text)
		}
	}
	if daemon.createFlockCalls != 1 {
		t.Fatalf("daemon CreateFlock calls = %d, want 1", daemon.createFlockCalls)
	}
}

func TestRuntimeRouterSpawnRecordsPlacementAndRoutesVMCalls(t *testing.T) {
	hostA := &routerFakeDaemon{}
	hostB := &routerFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: false, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
	)

	resp, err := router.SpawnVM(context.Background(), SpawnVMRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}
	if resp.Host.Name != "host-b" {
		t.Fatalf("host = %q, want host-b", resp.Host.Name)
	}
	if hostA.spawnCalls != 0 || hostB.spawnCalls != 1 {
		t.Fatalf("spawn calls hostA/hostB = %d/%d, want 0/1", hostA.spawnCalls, hostB.spawnCalls)
	}
	if host, ok := router.Placement(resp.VMID); !ok || host != "host-b" {
		t.Fatalf("placement = %q,%v want host-b,true", host, ok)
	}

	if _, err := router.Health(context.Background(), resp.VMID); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if hostB.healthCalls != 1 || hostB.healthVMID != resp.VMID {
		t.Fatalf("host-b health = %d/%q, want 1/%q", hostB.healthCalls, hostB.healthVMID, resp.VMID)
	}
	if _, err := router.CreateSnapshot(context.Background(), resp.VMID, CreateSnapshotRequest{TenantID: "tenant-1"}); err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if hostB.createSnapshotCalls != 1 || hostB.createSnapshotVMID != resp.VMID {
		t.Fatalf("host-b snapshot = %d/%q, want 1/%q", hostB.createSnapshotCalls, hostB.createSnapshotVMID, resp.VMID)
	}
}

func TestRuntimeRouterRestoreRecordsRestoredPlacement(t *testing.T) {
	daemon := &routerFakeDaemon{restoreResp: &RestoreSnapshotResponse{VMID: "vm-restored", GuestIP: "10.0.1.50", AgentURL: "http://10.0.1.50:8080", TenantID: "tenant-1", EgressPolicy: "profile", SourceSnapshotID: "snap-1"}}
	router := NewRuntimeRouter(
		NewScheduler([]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}}, nil, nil),
		map[string]Daemon{"host-a": daemon},
	)

	resp, err := router.RestoreSnapshot(context.Background(), "snap-1", RestoreSnapshotRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, ScheduleRequest{TenantID: "tenant-1", EgressPolicy: EgressPolicyProfile}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}
	if resp.Host.Name != "host-a" {
		t.Fatalf("host = %q, want host-a", resp.Host.Name)
	}
	if daemon.restoreSnapshotCalls != 1 || daemon.restoreSnapshotID != "snap-1" {
		t.Fatalf("restore calls = %d/%q, want 1/snap-1", daemon.restoreSnapshotCalls, daemon.restoreSnapshotID)
	}
	if host, ok := router.Placement("vm-restored"); !ok || host != "host-a" {
		t.Fatalf("placement = %q,%v want host-a,true", host, ok)
	}
}

func TestRuntimeRouterRestorePrefersSnapshotLocalityHost(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.SetSnapshotLocation("snap-1", "host-b"); err != nil {
		t.Fatalf("SetSnapshotLocation: %v", err)
	}
	hostA := &routerFakeDaemon{}
	hostB := &routerFakeDaemon{restoreResp: &RestoreSnapshotResponse{VMID: "vm-restored", SourceSnapshotID: "snap-1", TenantID: "tenant-1", EgressPolicy: "profile"}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	resp, err := router.RestoreSnapshot(context.Background(), "snap-1", RestoreSnapshotRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, ScheduleRequest{TenantID: "tenant-1", EgressPolicy: EgressPolicyProfile}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}
	if resp.Host.Name != "host-b" {
		t.Fatalf("restore host = %q, want locality host-b", resp.Host.Name)
	}
	if hostA.restoreSnapshotCalls != 0 || hostB.restoreSnapshotCalls != 1 {
		t.Fatalf("restore calls hostA/hostB = %d/%d, want 0/1", hostA.restoreSnapshotCalls, hostB.restoreSnapshotCalls)
	}
}

func TestRuntimeRouterSpawnRetriesOnNextEligibleHost(t *testing.T) {
	hostA := &routerFakeDaemon{spawnErr: errors.New("host-a unavailable")}
	hostB := &routerFakeDaemon{spawnResp: &SpawnVMResponse{VMID: "vm-b", TenantID: "tenant-1", EgressPolicy: "profile"}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{MaxAttempts: 2},
	)

	resp, err := router.SpawnVM(context.Background(), SpawnVMRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}
	if resp.Host.Name != "host-b" || resp.VMID != "vm-b" {
		t.Fatalf("resp = %+v, want host-b/vm-b", resp)
	}
	if hostA.spawnCalls != 1 || hostB.spawnCalls != 1 {
		t.Fatalf("spawn calls hostA/hostB = %d/%d, want 1/1", hostA.spawnCalls, hostB.spawnCalls)
	}
}

func TestRuntimeRouterReconcilePlacementsFromDaemonVMLists(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.SetVMPlacement("stale-vm", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	hostA := &routerFakeDaemon{listVMResp: []VMInfo{{VMID: "vm-a"}}}
	hostB := &routerFakeDaemon{listVMResp: []VMInfo{{VMID: "vm-b"}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	if err := router.ReconcilePlacements(context.Background()); err != nil {
		t.Fatalf("ReconcilePlacements: %v", err)
	}
	if _, ok := router.Placement("stale-vm"); ok {
		t.Fatal("stale-vm still has placement after reconcile")
	}
	if host, ok := router.Placement("vm-a"); !ok || host != "host-a" {
		t.Fatalf("vm-a placement = %q,%v want host-a,true", host, ok)
	}
	if host, ok := store.VMHost("vm-b"); !ok || host != "host-b" {
		t.Fatalf("store vm-b placement = %q,%v want host-b,true", host, ok)
	}
}

func TestRuntimeRouterReplicateSnapshotRecordsTargetLocation(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{}
	store := NewPlacementStore("")
	router := newSnapshotReplicationTestRouter(source, target, store, true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "replicated" {
		t.Fatalf("status = %q, want replicated", resp.Status)
	}
	if got := strings.Join(target.importCalls, ","); got != "bundle:snap-1" {
		t.Fatalf("target import calls = %q, want bundle:snap-1", got)
	}
	hosts := store.SnapshotHosts("snap-1")
	if got := strings.Join(hosts, ","); got != "host-b" {
		t.Fatalf("snapshot hosts = %q, want host-b", got)
	}
}

func TestRuntimeRouterReplicateDiffIncludesBaseFirst(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
		{SnapshotID: "snap-base", SnapshotType: "full"},
		{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
	}}
	target := &routerFakeDaemon{}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-diff",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if got := strings.Join(source.exportCalls, ","); got != "snap-base,snap-diff" {
		t.Fatalf("source export calls = %q, want snap-base,snap-diff", got)
	}
	if got := strings.Join(resp.Replicated, ","); got != "snap-base,snap-diff" {
		t.Fatalf("replicated = %q, want snap-base,snap-diff", got)
	}
	if resp.Status != "replicated" {
		t.Fatalf("status = %q, want replicated", resp.Status)
	}
}

func TestRuntimeRouterReplicateDiffValidatesListedBaseImport(t *testing.T) {
	t.Run("already present base is still imported and recorded", func(t *testing.T) {
		source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
			{SnapshotID: "snap-base", SnapshotType: "full"},
			{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
		}}
		target := &routerFakeDaemon{
			snapshotList:        []SnapshotInfo{{SnapshotID: "snap-base", SnapshotType: "full"}},
			importStatusForBody: map[string]string{"bundle:snap-base": "already_present"},
		}
		store := NewPlacementStore("")
		router := newSnapshotReplicationTestRouter(source, target, store, true, true)

		resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
			SnapshotID:          "snap-diff",
			SourceHost:          "host-a",
			TargetHost:          "host-b",
			IncludeDependencies: true,
		})
		if err != nil {
			t.Fatalf("ReplicateSnapshot returned error: %v", err)
		}
		if got := strings.Join(source.exportCalls, ","); got != "snap-base,snap-diff" {
			t.Fatalf("source export calls = %q, want snap-base,snap-diff", got)
		}
		if got := strings.Join(target.importCalls, ","); got != "bundle:snap-base,bundle:snap-diff" {
			t.Fatalf("target import calls = %q, want bundle:snap-base,bundle:snap-diff", got)
		}
		if got := strings.Join(resp.Skipped, ","); got != "snap-base" {
			t.Fatalf("skipped = %q, want snap-base", got)
		}
		if got := strings.Join(resp.Replicated, ","); got != "snap-diff" {
			t.Fatalf("replicated = %q, want snap-diff", got)
		}
		if got := strings.Join(store.SnapshotHosts("snap-base"), ","); got != "host-b" {
			t.Fatalf("base hosts = %q, want host-b", got)
		}
		if got := strings.Join(store.SnapshotHosts("snap-diff"), ","); got != "host-b" {
			t.Fatalf("diff hosts = %q, want host-b", got)
		}
	})

	t.Run("base conflict stops before diff transfer", func(t *testing.T) {
		source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
			{SnapshotID: "snap-base", SnapshotType: "full"},
			{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
		}}
		target := &routerFakeDaemon{
			snapshotList: []SnapshotInfo{{SnapshotID: "snap-base", SnapshotType: "full"}},
			importErrForBody: map[string]error{
				"bundle:snap-base": &DaemonError{StatusCode: 409, Body: "raw secret"},
			},
		}
		store := NewPlacementStore("")
		router := newSnapshotReplicationTestRouter(source, target, store, true, true)

		resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
			SnapshotID:          "snap-diff",
			SourceHost:          "host-a",
			TargetHost:          "host-b",
			IncludeDependencies: true,
		})
		if err != nil {
			t.Fatalf("ReplicateSnapshot returned error: %v", err)
		}
		if resp.Status != "failed" {
			t.Fatalf("status = %q, want failed", resp.Status)
		}
		if got := strings.Join(source.exportCalls, ","); got != "snap-base" {
			t.Fatalf("source export calls = %q, want snap-base", got)
		}
		if got := strings.Join(target.importCalls, ","); got != "bundle:snap-base" {
			t.Fatalf("target import calls = %q, want bundle:snap-base", got)
		}
		if len(resp.Errors) != 1 {
			t.Fatalf("errors = %+v, want one safe import error", resp.Errors)
		}
		errText := resp.Errors[0]
		for _, forbidden := range []string{"raw secret", "secret"} {
			if strings.Contains(errText, forbidden) {
				t.Fatalf("error %q contains forbidden raw body text %q", errText, forbidden)
			}
		}
		if !strings.Contains(errText, "import_failed") || !strings.Contains(errText, "status_code=409") {
			t.Fatalf("error = %q, want safe import failure with status code", errText)
		}
		if hosts := store.SnapshotHosts("snap-base"); len(hosts) != 0 {
			t.Fatalf("base hosts = %+v, want empty", hosts)
		}
		if hosts := store.SnapshotHosts("snap-diff"); len(hosts) != 0 {
			t.Fatalf("diff hosts = %+v, want empty", hosts)
		}
	})
}

func TestRuntimeRouterReplicateDiffWithoutDependencyFails(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"}}}
	target := &routerFakeDaemon{}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-diff",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: false,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "failed" {
		t.Fatalf("status = %q, want failed", resp.Status)
	}
	if len(resp.Errors) != 1 || resp.Errors[0] != "diff_base_missing" {
		t.Fatalf("errors = %+v, want [diff_base_missing]", resp.Errors)
	}
	if len(source.exportCalls) != 0 || len(target.importCalls) != 0 {
		t.Fatalf("export/import calls = %d/%d, want 0/0", len(source.exportCalls), len(target.importCalls))
	}
}

func TestRuntimeRouterReplicateDoesNotRecordFailedDiffLocation(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
		{SnapshotID: "snap-base", SnapshotType: "full"},
		{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
	}}
	target := &routerFakeDaemon{importErrForBody: map[string]error{"bundle:snap-diff": errors.New("import failed")}}
	store := NewPlacementStore("")
	router := newSnapshotReplicationTestRouter(source, target, store, true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-diff",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "partial" {
		t.Fatalf("status = %q, want partial", resp.Status)
	}
	if got := strings.Join(store.SnapshotHosts("snap-base"), ","); got != "host-b" {
		t.Fatalf("base hosts = %q, want host-b", got)
	}
	if hosts := store.SnapshotHosts("snap-diff"); len(hosts) != 0 {
		t.Fatalf("diff hosts = %+v, want empty", hosts)
	}
}

func TestRuntimeRouterReplicateRejectsUnhealthyHostBeforeDaemonCall(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), false, true)

	_, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err == nil || err.Error() != "source_host_unavailable" {
		t.Fatalf("error = %v, want source_host_unavailable", err)
	}
	if len(source.exportCalls) != 0 || len(target.importCalls) != 0 {
		t.Fatalf("export/import calls = %d/%d, want 0/0", len(source.exportCalls), len(target.importCalls))
	}
}

func TestRuntimeRouterReplicateScrubsImportDaemonErrorBody(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{importErrForBody: map[string]error{
		"bundle:snap-1": &DaemonError{StatusCode: 409, Body: "agent_token secret raw body"},
	}}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "failed" {
		t.Fatalf("status = %q, want failed", resp.Status)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %+v, want one safe import error", resp.Errors)
	}
	errText := resp.Errors[0]
	for _, forbidden := range []string{"agent_token", "secret", "raw body"} {
		if strings.Contains(errText, forbidden) {
			t.Fatalf("error %q contains forbidden raw body text %q", errText, forbidden)
		}
	}
	if !strings.Contains(errText, "import_failed") || !strings.Contains(errText, "status_code=409") {
		t.Fatalf("error = %q, want safe import failure with status code", errText)
	}
}

func TestRuntimeRouterReplicateDoesNotPreSkipTargetListPresence(t *testing.T) {
	t.Run("already present import records skipped locality", func(t *testing.T) {
		source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
		target := &routerFakeDaemon{
			snapshotList:        []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}},
			importStatusForBody: map[string]string{"bundle:snap-1": "already_present"},
		}
		store := NewPlacementStore("")
		router := newSnapshotReplicationTestRouter(source, target, store, true, true)

		resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
			SnapshotID:          "snap-1",
			SourceHost:          "host-a",
			TargetHost:          "host-b",
			IncludeDependencies: true,
		})
		if err != nil {
			t.Fatalf("ReplicateSnapshot returned error: %v", err)
		}
		if got := strings.Join(source.exportCalls, ","); got != "snap-1" {
			t.Fatalf("source export calls = %q, want snap-1", got)
		}
		if got := strings.Join(target.importCalls, ","); got != "bundle:snap-1" {
			t.Fatalf("target import calls = %q, want bundle:snap-1", got)
		}
		if got := strings.Join(resp.Skipped, ","); got != "snap-1" {
			t.Fatalf("skipped = %q, want snap-1", got)
		}
		if got := strings.Join(store.SnapshotHosts("snap-1"), ","); got != "host-b" {
			t.Fatalf("snapshot hosts = %q, want host-b", got)
		}
	})

	t.Run("conflict import does not record locality", func(t *testing.T) {
		source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
		target := &routerFakeDaemon{
			snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}},
			importErrForBody: map[string]error{
				"bundle:snap-1": &DaemonError{StatusCode: 409, Body: "conflict raw daemon body"},
			},
		}
		store := NewPlacementStore("")
		router := newSnapshotReplicationTestRouter(source, target, store, true, true)

		resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
			SnapshotID:          "snap-1",
			SourceHost:          "host-a",
			TargetHost:          "host-b",
			IncludeDependencies: true,
		})
		if err != nil {
			t.Fatalf("ReplicateSnapshot returned error: %v", err)
		}
		if resp.Status != "failed" {
			t.Fatalf("status = %q, want failed", resp.Status)
		}
		if got := strings.Join(source.exportCalls, ","); got != "snap-1" {
			t.Fatalf("source export calls = %q, want snap-1", got)
		}
		if got := strings.Join(target.importCalls, ","); got != "bundle:snap-1" {
			t.Fatalf("target import calls = %q, want bundle:snap-1", got)
		}
		if hosts := store.SnapshotHosts("snap-1"); len(hosts) != 0 {
			t.Fatalf("snapshot hosts = %+v, want empty", hosts)
		}
	})
}

func TestRuntimeRouterReplicateClosesExportStreamOnImportFailure(t *testing.T) {
	body := &routerTrackingReadCloser{Reader: strings.NewReader("bundle:snap-1")}
	source := &routerFakeDaemon{
		snapshotList:  []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}},
		exportStreams: map[string]io.ReadCloser{"snap-1": body},
	}
	target := &routerFakeDaemon{importErrForBody: map[string]error{"bundle:snap-1": errors.New("import failed")}}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "failed" {
		t.Fatalf("status = %q, want failed", resp.Status)
	}
	if !body.closed {
		t.Fatal("export stream was not closed after import failure")
	}
}

func TestRuntimeRouterReplicateScrubsSourceListDaemonErrorBody(t *testing.T) {
	source := &routerFakeDaemon{listSnapshotErr: &DaemonError{StatusCode: 500, Body: "agent_token secret raw body"}}
	target := &routerFakeDaemon{}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	_, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err == nil {
		t.Fatal("ReplicateSnapshot error = nil, want safe source list error")
	}
	errText := err.Error()
	for _, forbidden := range []string{"agent_token", "secret", "raw body"} {
		if strings.Contains(errText, forbidden) {
			t.Fatalf("error %q contains forbidden raw body text %q", errText, forbidden)
		}
	}
	if !strings.Contains(errText, "source_list_failed") || !strings.Contains(errText, "status_code=500") {
		t.Fatalf("error = %q, want safe source list failure with status code", errText)
	}
}

func TestRuntimeRouterReplicateReportsTransferWhenPlacementSaveFails(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{}
	store := NewPlacementStore(t.TempDir())
	router := newSnapshotReplicationTestRouter(source, target, store, true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "partial" {
		t.Fatalf("status = %q, want partial", resp.Status)
	}
	if got := strings.Join(resp.Replicated, ","); got != "snap-1" {
		t.Fatalf("replicated = %q, want snap-1", got)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "record_location_failed") {
		t.Fatalf("errors = %+v, want record_location_failed", resp.Errors)
	}
}

type routerTrackingReadCloser struct {
	*strings.Reader
	closed bool
}

func (r *routerTrackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func newSnapshotReplicationTestRouter(source, target *routerFakeDaemon, store *PlacementStore, sourceHealthy, targetHealthy bool) *RuntimeRouter {
	return NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: sourceHealthy, AvailableVMs: 1},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: targetHealthy, AvailableVMs: 1},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": source, "host-b": target},
		RuntimeRouterOptions{PlacementStore: store},
	)
}
