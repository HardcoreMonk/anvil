package anvilmcp

import (
	"context"
	"fmt"
	"sort"
)

type RuntimeRouterDaemon struct {
	router *RuntimeRouter
}

func NewRuntimeRouterDaemon(router *RuntimeRouter) *RuntimeRouterDaemon {
	return &RuntimeRouterDaemon{router: router}
}

func (d *RuntimeRouterDaemon) SpawnVM(ctx context.Context, req SpawnVMRequest) (*SpawnVMResponse, error) {
	if d == nil || d.router == nil {
		return nil, fmt.Errorf("runtime router daemon is nil")
	}
	resp, err := d.router.SpawnVM(ctx, req, TenantUsage{ActiveVMs: 1})
	if err != nil {
		return nil, err
	}
	return &resp.SpawnVMResponse, nil
}

func (d *RuntimeRouterDaemon) RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	daemon, err := d.daemonForVM(vmID)
	if err != nil {
		return nil, err
	}
	return daemon.RunTask(ctx, vmID, prompt)
}

func (d *RuntimeRouterDaemon) CopyIn(ctx context.Context, vmID, workspacePath, content string, overwrite bool) (*RawDaemonResponse, error) {
	daemon, err := d.daemonForVM(vmID)
	if err != nil {
		return nil, err
	}
	return daemon.CopyIn(ctx, vmID, workspacePath, content, overwrite)
}

func (d *RuntimeRouterDaemon) CopyOut(ctx context.Context, vmID, workspacePath string) (string, error) {
	daemon, err := d.daemonForVM(vmID)
	if err != nil {
		return "", err
	}
	return daemon.CopyOut(ctx, vmID, workspacePath)
}

func (d *RuntimeRouterDaemon) Health(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	daemon, err := d.daemonForVM(vmID)
	if err != nil {
		return nil, err
	}
	return daemon.Health(ctx, vmID)
}

func (d *RuntimeRouterDaemon) Stop(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	daemon, err := d.daemonForVM(vmID)
	if err != nil {
		return nil, err
	}
	return daemon.Stop(ctx, vmID)
}

func (d *RuntimeRouterDaemon) Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	if d == nil || d.router == nil {
		return nil, fmt.Errorf("runtime router daemon is nil")
	}
	return d.router.Delete(ctx, vmID)
}

func (d *RuntimeRouterDaemon) CreateSnapshot(ctx context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	if d == nil || d.router == nil {
		return nil, fmt.Errorf("runtime router daemon is nil")
	}
	return d.router.CreateSnapshot(ctx, vmID, req)
}

func (d *RuntimeRouterDaemon) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	daemons, err := d.hostDaemons()
	if err != nil {
		return nil, err
	}
	var snapshots []SnapshotInfo
	for _, item := range daemons {
		hostSnapshots, err := item.daemon.ListSnapshots(ctx)
		if err != nil {
			return nil, fmt.Errorf("list snapshots on runtime host %q: %w", item.name, err)
		}
		snapshots = append(snapshots, hostSnapshots...)
	}
	return snapshots, nil
}

func (d *RuntimeRouterDaemon) RestoreSnapshot(ctx context.Context, snapshotID string, req RestoreSnapshotRequest) (*RestoreSnapshotResponse, error) {
	if d == nil || d.router == nil {
		return nil, fmt.Errorf("runtime router daemon is nil")
	}
	resp, err := d.router.RestoreSnapshot(ctx, snapshotID, req, ScheduleRequest{
		TenantID:     req.TenantID,
		EgressPolicy: EgressPolicy(req.EgressPolicy),
	}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		return nil, err
	}
	return &resp.RestoreSnapshotResponse, nil
}

func (d *RuntimeRouterDaemon) DeleteSnapshot(ctx context.Context, snapshotID string) (*RawDaemonResponse, error) {
	if d == nil || d.router == nil {
		return nil, fmt.Errorf("runtime router daemon is nil")
	}
	if d.router.placementStore != nil {
		for _, hostName := range d.router.placementStore.SnapshotHosts(snapshotID) {
			daemon, ok := d.router.daemons[hostName]
			if ok && daemon != nil {
				return daemon.DeleteSnapshot(ctx, snapshotID)
			}
		}
	}
	daemon, hostName, err := d.firstDaemon()
	if err != nil {
		return nil, err
	}
	resp, err := daemon.DeleteSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("delete snapshot on runtime host %q: %w", hostName, err)
	}
	return resp, nil
}

func (d *RuntimeRouterDaemon) ReplicateSnapshot(ctx context.Context, req SnapshotReplicationRequest) (*SnapshotReplicationResponse, error) {
	if d == nil || d.router == nil {
		return nil, fmt.Errorf("runtime router daemon is nil")
	}
	return d.router.ReplicateSnapshot(ctx, req)
}

func (d *RuntimeRouterDaemon) CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	daemon, _, err := d.firstDaemon()
	if err != nil {
		return nil, err
	}
	return daemon.CreateFlock(ctx, req)
}

func (d *RuntimeRouterDaemon) ListFlocks(ctx context.Context) ([]FlockInfo, error) {
	daemon, _, err := d.firstDaemon()
	if err != nil {
		return nil, err
	}
	return daemon.ListFlocks(ctx)
}

func (d *RuntimeRouterDaemon) GetFlock(ctx context.Context, flockID string) (*FlockInfo, error) {
	daemon, _, err := d.firstDaemon()
	if err != nil {
		return nil, err
	}
	return daemon.GetFlock(ctx, flockID)
}

func (d *RuntimeRouterDaemon) DeleteFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error) {
	daemon, _, err := d.firstDaemon()
	if err != nil {
		return nil, err
	}
	return daemon.DeleteFlock(ctx, flockID)
}

func (d *RuntimeRouterDaemon) PostTownWall(ctx context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error) {
	daemon, _, err := d.firstDaemon()
	if err != nil {
		return nil, err
	}
	return daemon.PostTownWall(ctx, flockID, req)
}

func (d *RuntimeRouterDaemon) TownWallHistory(ctx context.Context, flockID string) ([]TownWallMessage, error) {
	daemon, _, err := d.firstDaemon()
	if err != nil {
		return nil, err
	}
	return daemon.TownWallHistory(ctx, flockID)
}

func (d *RuntimeRouterDaemon) daemonForVM(vmID string) (Daemon, error) {
	if d == nil || d.router == nil {
		return nil, fmt.Errorf("runtime router daemon is nil")
	}
	return d.router.daemonForVM(vmID)
}

type namedDaemon struct {
	name   string
	daemon Daemon
}

func (d *RuntimeRouterDaemon) hostDaemons() ([]namedDaemon, error) {
	if d == nil || d.router == nil {
		return nil, fmt.Errorf("runtime router daemon is nil")
	}
	d.router.mu.RLock()
	defer d.router.mu.RUnlock()
	items := make([]namedDaemon, 0, len(d.router.daemons))
	for name, daemon := range d.router.daemons {
		if daemon != nil {
			items = append(items, namedDaemon{name: name, daemon: daemon})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	if len(items) == 0 {
		return nil, fmt.Errorf("runtime router has no daemon clients")
	}
	return items, nil
}

func (d *RuntimeRouterDaemon) firstDaemon() (Daemon, string, error) {
	items, err := d.hostDaemons()
	if err != nil {
		return nil, "", err
	}
	return items[0].daemon, items[0].name, nil
}
