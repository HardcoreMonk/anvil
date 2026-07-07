package anvilmcp

import (
	"context"
	"fmt"
	"strings"
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

func TestReplicatingDaemonDirectRoutedOperationsTrimFlockID(t *testing.T) {
	base := &replicatingDaemonBaseFake{}
	router := &replicatingDaemonRoutedFake{
		routed:     map[string]bool{"routed-flock-1": true},
		deleteResp: &RawDaemonResponse{StatusCode: 200, Body: `{"status":"deleted"}`},
	}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	resp, err := daemon.DeleteFlock(context.Background(), " routed-flock-1 ")
	if err != nil {
		t.Fatalf("DeleteFlock returned error: %v", err)
	}
	if resp.StatusCode != 200 || router.deleteCalls != 1 || router.deleteFlockID != "routed-flock-1" {
		t.Fatalf("resp/delete calls/id = %+v/%d/%q, want routed delete with trimmed id", resp, router.deleteCalls, router.deleteFlockID)
	}
	if base.deleteFlockCalls != 0 {
		t.Fatalf("base delete calls = %d, want 0", base.deleteFlockCalls)
	}

	_, err = daemon.PostTownWall(context.Background(), " routed-flock-1 ", TownWallPostRequest{AgentID: "worker-1", Body: "hello"})
	if err != nil {
		t.Fatalf("PostTownWall error = %v, want nil (delegates to routed controller)", err)
	}
	if router.postWallCalls != 1 || router.postWallFlockID != "routed-flock-1" {
		t.Fatalf("routed post calls/id = %d/%q, want 1/routed-flock-1 (trimmed)", router.postWallCalls, router.postWallFlockID)
	}
	if base.postTownWallCalls != 0 {
		t.Fatalf("base post calls = %d, want 0", base.postTownWallCalls)
	}
}

// TestReplicatingDaemonTownWallDelegatesRoutedFlockToRouter locks in Task 6: a
// routed flock's wall post/history are delegated to the routed controller (which
// proxies to the home daemon) and never fall through to the base daemon.
func TestReplicatingDaemonTownWallDelegatesRoutedFlockToRouter(t *testing.T) {
	base := &replicatingDaemonBaseFake{}
	router := &replicatingDaemonRoutedFake{routed: map[string]bool{"routed-flock-1": true}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	_, postErr := daemon.PostTownWall(context.Background(), "routed-flock-1", TownWallPostRequest{AgentID: "worker-1", Body: "hello"})
	if postErr != nil {
		t.Fatalf("PostTownWall error = %v, want nil (delegates to routed controller)", postErr)
	}
	if router.postWallCalls != 1 || router.postWallFlockID != "routed-flock-1" {
		t.Fatalf("routed post calls/id = %d/%q, want 1/routed-flock-1", router.postWallCalls, router.postWallFlockID)
	}
	if base.postTownWallCalls != 0 {
		t.Fatalf("base post calls = %d, want 0 (routed flock never hits base)", base.postTownWallCalls)
	}

	_, historyErr := daemon.TownWallHistory(context.Background(), "routed-flock-1")
	if historyErr != nil {
		t.Fatalf("TownWallHistory error = %v, want nil (delegates to routed controller)", historyErr)
	}
	if router.historyCalls != 1 || router.historyFlockID != "routed-flock-1" {
		t.Fatalf("routed history calls/id = %d/%q, want 1/routed-flock-1", router.historyCalls, router.historyFlockID)
	}
	if base.townWallHistoryCalls != 0 {
		t.Fatalf("base history calls = %d, want 0 (routed flock never hits base)", base.townWallHistoryCalls)
	}
}

// TestReplicatingDaemonTownWallFallsBackToBaseForNonRoutedFlock keeps the base
// path intact: a flock the routed controller does not own is served by the base
// daemon exactly as before.
func TestReplicatingDaemonTownWallFallsBackToBaseForNonRoutedFlock(t *testing.T) {
	base := &replicatingDaemonBaseFake{}
	router := &replicatingDaemonRoutedFake{routed: map[string]bool{"routed-flock-1": true}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	if _, err := daemon.PostTownWall(context.Background(), "base-flock", TownWallPostRequest{AgentID: "a", Body: "b"}); err != nil {
		t.Fatalf("PostTownWall(base-flock) error = %v, want nil", err)
	}
	if _, err := daemon.TownWallHistory(context.Background(), "base-flock"); err != nil {
		t.Fatalf("TownWallHistory(base-flock) error = %v, want nil", err)
	}
	if base.postTownWallCalls != 1 || base.townWallHistoryCalls != 1 {
		t.Fatalf("base post/history calls = %d/%d, want 1/1", base.postTownWallCalls, base.townWallHistoryCalls)
	}
	if router.postWallCalls != 0 || router.historyCalls != 0 {
		t.Fatalf("routed controller calls = %d/%d, want 0/0 for non-routed flock", router.postWallCalls, router.historyCalls)
	}
}

func TestReplicatingDaemonGetFlockFallsBackToBaseForDeletedRoutedRecord(t *testing.T) {
	base := &replicatingDaemonBaseFake{getFlockResp: &FlockInfo{
		FlockID: "routed-flock-1",
		Task:    "base flock",
		Agents:  map[string]FlockAgentInfo{"base-agent": {AgentID: "base-agent", VMID: "base-vm"}},
	}}
	router := &replicatingDaemonRoutedFake{records: []RoutedFlockRecord{{
		FlockID: "routed-flock-1",
		Status:  RoutedFlockStatusDeleted,
	}}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	info, err := daemon.GetFlock(context.Background(), "routed-flock-1")
	if err != nil {
		t.Fatalf("GetFlock returned error: %v", err)
	}
	if base.getFlockCalls != 1 || base.getFlockID != "routed-flock-1" {
		t.Fatalf("base get calls/id = %d/%q, want fallback to base", base.getFlockCalls, base.getFlockID)
	}
	if info.Task != "base flock" || info.Agents["base-agent"].VMID != "base-vm" {
		t.Fatalf("flock info = %+v, want base response", info)
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
	}, {
		FlockID: "cleanup-flock",
		Task:    "cleanup failed members",
		Status:  RoutedFlockStatusFailedCleanupPending,
		Agents: []RoutedFlockAgent{{
			AgentID: "worker-2",
			Role:    "worker",
			VMID:    "vm-cleanup",
			Status:  "cleanup_pending",
		}},
	}, {
		FlockID: "deleted-flock",
		Task:    "deleted members",
		Status:  RoutedFlockStatusDeleted,
	}}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	flocks, err := daemon.ListFlocks(context.Background())
	if err != nil {
		t.Fatalf("ListFlocks returned error: %v", err)
	}
	if len(flocks) != 3 {
		t.Fatalf("flocks = %+v, want base+visible routed records", flocks)
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
	if flocks[2].FlockID != "cleanup-flock" || flocks[2].Agents["worker-2"].Status != "cleanup_pending" {
		t.Fatalf("cleanup pending flock = %+v", flocks[2])
	}
	for _, flock := range flocks {
		if flock.FlockID == "deleted-flock" {
			t.Fatalf("deleted routed flock was listed: %+v", flock)
		}
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

func (f *replicatingDaemonBaseFake) RegisterDistributedFlock(context.Context, string, DistributedFlockRequest) error {
	return nil
}

func (f *replicatingDaemonBaseFake) RegisterRelayFlock(context.Context, string, RelayFlockRequest) error {
	return nil
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
	routed          map[string]bool
	records         []RoutedFlockRecord
	createCalls     int
	deleteCalls     int
	deleteFlockID   string
	deleteResp      *RawDaemonResponse
	postWallCalls   int
	postWallFlockID string
	postWallReq     TownWallPostRequest
	historyCalls    int
	historyFlockID  string
}

func (f *replicatingDaemonRoutedFake) CreateRoutedFlockMembers(context.Context, FlockCreateRequest) (*RoutedFlockCreateOutput, error) {
	f.createCalls++
	return nil, fmt.Errorf("not used")
}

func (f *replicatingDaemonRoutedFake) IsRoutedFlock(flockID string) bool {
	flockID = strings.TrimSpace(flockID)
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
	flockID = strings.TrimSpace(flockID)
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

func (f *replicatingDaemonRoutedFake) PostRoutedTownWall(_ context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error) {
	f.postWallCalls++
	f.postWallFlockID = flockID
	f.postWallReq = req
	return &TownWallMessage{AgentID: req.AgentID, Body: req.Body}, nil
}

func (f *replicatingDaemonRoutedFake) RoutedTownWallHistory(_ context.Context, flockID string) ([]TownWallMessage, error) {
	f.historyCalls++
	f.historyFlockID = flockID
	return []TownWallMessage{{AgentID: "worker-1", Body: "routed-history"}}, nil
}
