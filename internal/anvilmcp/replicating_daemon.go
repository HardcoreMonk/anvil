package anvilmcp

import (
	"context"
	"fmt"
)

type ReplicatingDaemon struct {
	Daemon
	replicator snapshotReplicator
}

func NewReplicatingDaemon(base Daemon, replicator snapshotReplicator) *ReplicatingDaemon {
	return &ReplicatingDaemon{
		Daemon:     base,
		replicator: replicator,
	}
}

func (d *ReplicatingDaemon) ReplicateSnapshot(ctx context.Context, req SnapshotReplicationRequest) (*SnapshotReplicationResponse, error) {
	if d == nil || d.replicator == nil {
		return nil, fmt.Errorf("snapshot replicator is nil")
	}
	return d.replicator.ReplicateSnapshot(ctx, req)
}
