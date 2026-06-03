package anvilmcp

import (
	"context"
	"testing"
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

type replicatingDaemonBaseFake struct {
	createFlockCalls int
	createFlockReq   FlockCreateRequest
	createFlockResp  *FlockCreateResponse
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
	return []FlockInfo{}, nil
}

func (f *replicatingDaemonBaseFake) GetFlock(context.Context, string) (*FlockInfo, error) {
	return &FlockInfo{}, nil
}

func (f *replicatingDaemonBaseFake) DeleteFlock(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) PostTownWall(context.Context, string, TownWallPostRequest) (*TownWallMessage, error) {
	return &TownWallMessage{}, nil
}

func (f *replicatingDaemonBaseFake) TownWallHistory(context.Context, string) ([]TownWallMessage, error) {
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
