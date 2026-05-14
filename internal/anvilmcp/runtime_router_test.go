package anvilmcp

import (
	"context"
	"errors"
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
	return nil, nil
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
