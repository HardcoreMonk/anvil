package anvilmcp

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestReplicatingDaemonCreateFlockDelegatesToRouterWhenAvailable(t *testing.T) {
	base := &replicatingDaemonBaseFake{
		createFlockResp: &FlockCreateResponse{FlockID: "base-flock"},
	}
	router := &replicatingDaemonRouterFake{
		createFlockResp: &FlockCreateResponse{FlockID: "router-flock"},
	}
	daemon := NewReplicatingDaemon(base, router)

	resp, err := daemon.CreateFlock(context.Background(), FlockCreateRequest{
		Task:  "x",
		Roles: []string{"worker"},
	})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if resp.FlockID != "router-flock" {
		t.Fatalf("CreateFlock flock id = %q, want router-flock", resp.FlockID)
	}
	if router.createFlockCalls != 1 {
		t.Fatalf("router CreateFlock calls = %d, want 1", router.createFlockCalls)
	}
	if base.createFlockCalls != 0 {
		t.Fatalf("base CreateFlock calls = %d, want 0", base.createFlockCalls)
	}
}

func TestReplicatingDaemonCreateFlockFallsBackToBase(t *testing.T) {
	base := &replicatingDaemonBaseFake{
		createFlockResp: &FlockCreateResponse{FlockID: "base-flock"},
	}
	replicator := &replicatingDaemonSnapshotOnlyFake{}
	daemon := NewReplicatingDaemon(base, replicator)

	resp, err := daemon.CreateFlock(context.Background(), FlockCreateRequest{
		Task:  "x",
		Roles: []string{"worker"},
	})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if resp.FlockID != "base-flock" {
		t.Fatalf("CreateFlock flock id = %q, want base-flock", resp.FlockID)
	}
	if base.createFlockCalls != 1 {
		t.Fatalf("base CreateFlock calls = %d, want 1", base.createFlockCalls)
	}
}

func TestReplicatingDaemonCreateRoutedFlockMembersDisabledWithoutController(t *testing.T) {
	daemon := NewReplicatingDaemon(&replicatingDaemonBaseFake{}, &replicatingDaemonSnapshotOnlyFake{})

	_, err := daemon.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:  "review changes",
		Roles: []string{"worker"},
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want disabled")
	}
	if got := err.Error(); got != "routed flock members create is disabled; set ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only with persistent scheduler state" {
		t.Fatalf("CreateRoutedFlockMembers error = %q", got)
	}
}

func TestReplicatingDaemonDeleteFlockRoutesRoutedFlock(t *testing.T) {
	base := &replicatingDaemonBaseFake{}
	router := &replicatingDaemonRoutedFake{
		routed:     map[string]bool{"routed-flock-1": true},
		deleteResp: &RawDaemonResponse{StatusCode: 200, Body: `{"status":"deleted"}`},
	}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	resp, err := daemon.DeleteFlock(context.Background(), "routed-flock-1")
	if err != nil {
		t.Fatalf("DeleteFlock returned error: %v", err)
	}
	if resp.StatusCode != 200 || router.deleteCalls != 1 || router.deleteFlockID != "routed-flock-1" {
		t.Fatalf("resp/delete calls/id = %+v/%d/%q, want routed delete", resp, router.deleteCalls, router.deleteFlockID)
	}
	if base.deleteFlockCalls != 0 {
		t.Fatalf("base delete calls = %d, want 0", base.deleteFlockCalls)
	}
}

func TestReplicatingDaemonTownWallRejectsRoutedFlockWithoutBaseFallback(t *testing.T) {
	base := &replicatingDaemonBaseFake{}
	router := &replicatingDaemonRoutedFake{routed: map[string]bool{"routed-flock-1": true}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	_, postErr := daemon.PostTownWall(context.Background(), "routed-flock-1", TownWallPostRequest{AgentID: "worker-1", Body: "hello"})
	if postErr == nil {
		t.Fatal("PostTownWall error = nil, want unsupported")
	}
	if got := postErr.Error(); got != `Town Wall is not supported for routed members-only flock "routed-flock-1"` {
		t.Fatalf("PostTownWall error = %q", got)
	}
	if base.postTownWallCalls != 0 {
		t.Fatalf("base post calls = %d, want 0", base.postTownWallCalls)
	}

	_, historyErr := daemon.TownWallHistory(context.Background(), "routed-flock-1")
	if historyErr == nil {
		t.Fatal("TownWallHistory error = nil, want unsupported")
	}
	if got := historyErr.Error(); got != `Town Wall is not supported for routed members-only flock "routed-flock-1"` {
		t.Fatalf("TownWallHistory error = %q", got)
	}
	if base.townWallHistoryCalls != 0 {
		t.Fatalf("base history calls = %d, want 0", base.townWallHistoryCalls)
	}
}

func TestReplicatingDaemonGetFlockReturnsRoutedRecord(t *testing.T) {
	base := &replicatingDaemonBaseFake{getFlockResp: &FlockInfo{FlockID: "base-flock"}}
	createdAt := time.Date(2026, 6, 6, 1, 2, 3, 0, time.UTC)
	router := &replicatingDaemonRoutedFake{records: []RoutedFlockRecord{{
		FlockID:      "routed-flock-1",
		Task:         "review changes",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Status:       RoutedFlockStatusReady,
		CreatedAt:    createdAt,
		Agents: []RoutedFlockAgent{{
			AgentID:  "worker-1",
			Role:     "worker",
			VMID:     "vm-worker",
			AgentURL: "http://10.0.0.2:3000",
			Status:   "running",
		}},
	}}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	info, err := daemon.GetFlock(context.Background(), "routed-flock-1")
	if err != nil {
		t.Fatalf("GetFlock returned error: %v", err)
	}
	if base.getFlockCalls != 0 {
		t.Fatalf("base get calls = %d, want 0", base.getFlockCalls)
	}
	if info.FlockID != "routed-flock-1" || info.Task != "review changes" || info.TenantID != "tenant-1" || info.EgressPolicy != "profile" || !info.CreatedAt.Equal(createdAt) {
		t.Fatalf("routed flock info = %+v", info)
	}
	agent := info.Agents["worker-1"]
	if agent.AgentID != "worker-1" || agent.Role != "worker" || agent.VMID != "vm-worker" || agent.AgentURL != "http://10.0.0.2:3000" || agent.Status != "running" {
		t.Fatalf("routed agent info = %+v", agent)
	}
}

func TestReplicatingDaemonListFlocksIncludesBaseFlocksAndRoutedRecords(t *testing.T) {
	createdAt := time.Date(2026, 6, 6, 4, 5, 6, 0, time.UTC)
	base := &replicatingDaemonBaseFake{listFlockResp: []FlockInfo{{FlockID: "base-flock"}}}
	router := &replicatingDaemonRoutedFake{records: []RoutedFlockRecord{{
		FlockID:      "routed-flock-1",
		Task:         "review changes",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Status:       RoutedFlockStatusReady,
		CreatedAt:    createdAt,
		Agents: []RoutedFlockAgent{{
			AgentID:  "worker-1",
			Role:     "worker",
			VMID:     "vm-worker",
			AgentURL: "http://10.0.0.2:3000",
			Status:   "running",
		}},
	}}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	flocks, err := daemon.ListFlocks(context.Background())
	if err != nil {
		t.Fatalf("ListFlocks returned error: %v", err)
	}
	if len(flocks) != 2 {
		t.Fatalf("flocks = %+v, want base+routed", flocks)
	}
	if flocks[0].FlockID != "base-flock" {
		t.Fatalf("base flock = %+v", flocks[0])
	}
	if flocks[1].FlockID != "routed-flock-1" || flocks[1].Task != "review changes" || !flocks[1].CreatedAt.Equal(createdAt) {
		t.Fatalf("routed flock info = %+v", flocks[1])
	}
	if flocks[1].Agents["worker-1"].VMID != "vm-worker" {
		t.Fatalf("routed flock agents = %+v", flocks[1].Agents)
	}
}

type replicatingDaemonBaseFake struct {
	createFlockCalls int
	createFlockReq   FlockCreateRequest
	createFlockResp  *FlockCreateResponse

	listFlockResp        []FlockInfo
	getFlockCalls        int
	getFlockID           string
	getFlockResp         *FlockInfo
	deleteFlockCalls     int
	deleteFlockID        string
	postTownWallCalls    int
	townWallHistoryCalls int
}

func (f *replicatingDaemonBaseFake) SpawnVM(context.Context, SpawnVMRequest) (*SpawnVMResponse, error) {
	return &SpawnVMResponse{}, nil
}

func (f *replicatingDaemonBaseFake) RunTask(context.Context, string, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) CopyIn(context.Context, string, string, string, bool) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) CopyOut(context.Context, string, string) (string, error) {
	return "", nil
}

func (f *replicatingDaemonBaseFake) Health(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) Stop(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) Delete(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) CreateSnapshot(context.Context, string, CreateSnapshotRequest) (*SnapshotInfo, error) {
	return &SnapshotInfo{}, nil
}

func (f *replicatingDaemonBaseFake) ListSnapshots(context.Context) ([]SnapshotInfo, error) {
	return []SnapshotInfo{}, nil
}

func (f *replicatingDaemonBaseFake) RestoreSnapshot(context.Context, string, RestoreSnapshotRequest) (*RestoreSnapshotResponse, error) {
	return &RestoreSnapshotResponse{}, nil
}

func (f *replicatingDaemonBaseFake) DeleteSnapshot(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) CreateFlock(_ context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	f.createFlockCalls++
	f.createFlockReq = req
	if f.createFlockResp != nil {
		return f.createFlockResp, nil
	}
	return &FlockCreateResponse{FlockID: "base-flock"}, nil
}

func (f *replicatingDaemonBaseFake) ListFlocks(context.Context) ([]FlockInfo, error) {
	if f.listFlockResp != nil {
		return append([]FlockInfo(nil), f.listFlockResp...), nil
	}
	return []FlockInfo{}, nil
}

func (f *replicatingDaemonBaseFake) GetFlock(_ context.Context, flockID string) (*FlockInfo, error) {
	f.getFlockCalls++
	f.getFlockID = flockID
	if f.getFlockResp != nil {
		info := *f.getFlockResp
		return &info, nil
	}
	return &FlockInfo{}, nil
}

func (f *replicatingDaemonBaseFake) DeleteFlock(_ context.Context, flockID string) (*RawDaemonResponse, error) {
	f.deleteFlockCalls++
	f.deleteFlockID = flockID
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) PostTownWall(context.Context, string, TownWallPostRequest) (*TownWallMessage, error) {
	f.postTownWallCalls++
	return &TownWallMessage{}, nil
}

func (f *replicatingDaemonBaseFake) TownWallHistory(context.Context, string) ([]TownWallMessage, error) {
	f.townWallHistoryCalls++
	return []TownWallMessage{}, nil
}

type replicatingDaemonRouterFake struct {
	createFlockCalls int
	createFlockReq   FlockCreateRequest
	createFlockResp  *FlockCreateResponse
}

func (f *replicatingDaemonRouterFake) ReplicateSnapshot(context.Context, SnapshotReplicationRequest) (*SnapshotReplicationResponse, error) {
	return &SnapshotReplicationResponse{}, nil
}

func (f *replicatingDaemonRouterFake) CreateFlock(_ context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	f.createFlockCalls++
	f.createFlockReq = req
	if f.createFlockResp != nil {
		return f.createFlockResp, nil
	}
	return &FlockCreateResponse{FlockID: "router-flock"}, nil
}

type replicatingDaemonSnapshotOnlyFake struct{}

func (f *replicatingDaemonSnapshotOnlyFake) ReplicateSnapshot(context.Context, SnapshotReplicationRequest) (*SnapshotReplicationResponse, error) {
	return &SnapshotReplicationResponse{}, nil
}

type replicatingDaemonRoutedFake struct {
	routed        map[string]bool
	records       []RoutedFlockRecord
	createCalls   int
	deleteCalls   int
	deleteFlockID string
	deleteResp    *RawDaemonResponse
}

func (f *replicatingDaemonRoutedFake) CreateRoutedFlockMembers(context.Context, FlockCreateRequest) (*RoutedFlockCreateOutput, error) {
	f.createCalls++
	return nil, fmt.Errorf("not used")
}

func (f *replicatingDaemonRoutedFake) IsRoutedFlock(flockID string) bool {
	if f.routed != nil {
		return f.routed[flockID]
	}
	for _, record := range f.records {
		if record.FlockID == flockID {
			return true
		}
	}
	return false
}

func (f *replicatingDaemonRoutedFake) DeleteRoutedFlock(_ context.Context, flockID string) (*RawDaemonResponse, error) {
	f.deleteCalls++
	f.deleteFlockID = flockID
	if f.deleteResp != nil {
		return f.deleteResp, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *replicatingDaemonRoutedFake) GetRoutedFlock(flockID string) (RoutedFlockRecord, bool) {
	for _, record := range f.records {
		if record.FlockID == flockID {
			return record, true
		}
	}
	return RoutedFlockRecord{}, false
}

func (f *replicatingDaemonRoutedFake) ListRoutedFlocks() []RoutedFlockRecord {
	return append([]RoutedFlockRecord(nil), f.records...)
}
