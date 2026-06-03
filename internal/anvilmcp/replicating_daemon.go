package anvilmcp

import (
	"context"
	"fmt"
)

type ReplicatingDaemon struct {
	Daemon
	replicator  snapshotReplicator
	flockRouter flockCreator
}

type flockCreator interface {
	CreateFlock(context.Context, FlockCreateRequest) (*FlockCreateResponse, error)
}

func NewReplicatingDaemon(base Daemon, replicator snapshotReplicator) *ReplicatingDaemon {
	daemon := &ReplicatingDaemon{
		Daemon:     base,
		replicator: replicator,
	}
	if router, ok := replicator.(flockCreator); ok {
		daemon.flockRouter = router
	}
	return daemon
}

func (d *ReplicatingDaemon) ReplicateSnapshot(ctx context.Context, req SnapshotReplicationRequest) (*SnapshotReplicationResponse, error) {
	if d == nil || d.replicator == nil {
		return nil, fmt.Errorf("snapshot replicator is nil")
	}
	return d.replicator.ReplicateSnapshot(ctx, req)
}

func (d *ReplicatingDaemon) CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	if d != nil && d.flockRouter != nil {
		return d.flockRouter.CreateFlock(ctx, req)
	}
	return d.Daemon.CreateFlock(ctx, req)
}
