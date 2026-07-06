package anvilmcp

import (
	"context"
	"fmt"
	"strings"
)

type ReplicatingDaemon struct {
	Daemon
	replicator   snapshotReplicator
	flockRouter  flockCreator
	routedFlocks routedFlockController
}

type flockCreator interface {
	CreateFlock(context.Context, FlockCreateRequest) (*FlockCreateResponse, error)
}

type routedFlockController interface {
	CreateRoutedFlockMembers(context.Context, FlockCreateRequest) (*RoutedFlockCreateOutput, error)
	IsRoutedFlock(string) bool
	DeleteRoutedFlock(context.Context, string) (*RawDaemonResponse, error)
	GetRoutedFlock(string) (RoutedFlockRecord, bool)
	ListRoutedFlocks() []RoutedFlockRecord
	PostRoutedTownWall(context.Context, string, TownWallPostRequest) (*TownWallMessage, error)
	RoutedTownWallHistory(context.Context, string) ([]TownWallMessage, error)
}

type ReplicatingDaemonOptions struct {
	RoutedFlocks routedFlockController
}

func NewReplicatingDaemon(base Daemon, replicator snapshotReplicator) *ReplicatingDaemon {
	return NewReplicatingDaemonWithOptions(base, replicator, ReplicatingDaemonOptions{})
}

func NewReplicatingDaemonWithOptions(base Daemon, replicator snapshotReplicator, opts ReplicatingDaemonOptions) *ReplicatingDaemon {
	daemon := &ReplicatingDaemon{
		Daemon:       base,
		replicator:   replicator,
		routedFlocks: opts.RoutedFlocks,
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

func (d *ReplicatingDaemon) CreateRoutedFlockMembers(ctx context.Context, req FlockCreateRequest) (*RoutedFlockCreateOutput, error) {
	if d == nil || d.routedFlocks == nil {
		return nil, fmt.Errorf("routed flock members create is disabled; set ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only with persistent scheduler state")
	}
	return d.routedFlocks.CreateRoutedFlockMembers(ctx, req)
}

func (d *ReplicatingDaemon) DeleteFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error) {
	routedFlockID := strings.TrimSpace(flockID)
	if d != nil && d.routedFlocks != nil && d.routedFlocks.IsRoutedFlock(routedFlockID) {
		return d.routedFlocks.DeleteRoutedFlock(ctx, routedFlockID)
	}
	return d.Daemon.DeleteFlock(ctx, flockID)
}

func (d *ReplicatingDaemon) GetFlock(ctx context.Context, flockID string) (*FlockInfo, error) {
	routedFlockID := strings.TrimSpace(flockID)
	if d != nil && d.routedFlocks != nil {
		if record, ok := d.routedFlocks.GetRoutedFlock(routedFlockID); ok && routedFlockRecordVisible(record) {
			info := flockInfoFromRoutedRecord(record)
			return &info, nil
		}
	}
	return d.Daemon.GetFlock(ctx, flockID)
}

func (d *ReplicatingDaemon) ListFlocks(ctx context.Context) ([]FlockInfo, error) {
	base, err := d.Daemon.ListFlocks(ctx)
	if err != nil {
		return nil, err
	}
	if d == nil || d.routedFlocks == nil {
		return base, nil
	}
	for _, record := range d.routedFlocks.ListRoutedFlocks() {
		if !routedFlockRecordVisible(record) {
			continue
		}
		base = append(base, flockInfoFromRoutedRecord(record))
	}
	return base, nil
}

// PostTownWall routes a routed flock's wall post to the routed controller, which
// proxies to the home daemon that owns the shared wall hub (Task 6). Non-routed
// flocks continue to hit the base daemon directly.
func (d *ReplicatingDaemon) PostTownWall(ctx context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error) {
	routedFlockID := strings.TrimSpace(flockID)
	if d != nil && d.routedFlocks != nil && d.routedFlocks.IsRoutedFlock(routedFlockID) {
		return d.routedFlocks.PostRoutedTownWall(ctx, routedFlockID, req)
	}
	return d.Daemon.PostTownWall(ctx, flockID, req)
}

// TownWallHistory reads a routed flock's shared wall from the home daemon via the
// routed controller; non-routed flocks read from the base daemon.
func (d *ReplicatingDaemon) TownWallHistory(ctx context.Context, flockID string) ([]TownWallMessage, error) {
	routedFlockID := strings.TrimSpace(flockID)
	if d != nil && d.routedFlocks != nil && d.routedFlocks.IsRoutedFlock(routedFlockID) {
		return d.routedFlocks.RoutedTownWallHistory(ctx, routedFlockID)
	}
	return d.Daemon.TownWallHistory(ctx, flockID)
}

func routedFlockRecordVisible(record RoutedFlockRecord) bool {
	return record.Status != RoutedFlockStatusDeleted
}

func flockInfoFromRoutedRecord(record RoutedFlockRecord) FlockInfo {
	agents := make(map[string]FlockAgentInfo, len(record.Agents))
	for _, agent := range record.Agents {
		agents[agent.AgentID] = FlockAgentInfo{
			AgentID:  agent.AgentID,
			Role:     agent.Role,
			VMID:     agent.VMID,
			AgentURL: agent.AgentURL,
			Status:   agent.Status,
		}
	}
	return FlockInfo{
		FlockID:      record.FlockID,
		Task:         record.Task,
		TenantID:     record.TenantID,
		EgressPolicy: record.EgressPolicy,
		Agents:       agents,
		CreatedAt:    record.CreatedAt,
	}
}
