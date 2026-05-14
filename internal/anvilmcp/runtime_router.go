package anvilmcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type ScheduleDeniedError struct {
	Decision ScheduleDecision
}

func (e *ScheduleDeniedError) Error() string {
	return fmt.Sprintf("schedule denied: %s", e.Decision.Reason)
}

type RoutedSpawnVMResponse struct {
	SpawnVMResponse
	Host RuntimeHost `json:"host"`
}

type RoutedRestoreSnapshotResponse struct {
	RestoreSnapshotResponse
	Host RuntimeHost `json:"host"`
}

type RuntimeRouter struct {
	mu        sync.RWMutex
	scheduler *Scheduler
	daemons   map[string]Daemon
	placement map[string]string
}

func NewRuntimeRouter(scheduler *Scheduler, daemons map[string]Daemon) *RuntimeRouter {
	daemonCopy := make(map[string]Daemon, len(daemons))
	for name, daemon := range daemons {
		daemonCopy[strings.TrimSpace(name)] = daemon
	}
	return &RuntimeRouter{
		scheduler: scheduler,
		daemons:   daemonCopy,
		placement: make(map[string]string),
	}
}

func (r *RuntimeRouter) Placement(vmID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	host, ok := r.placement[strings.TrimSpace(vmID)]
	return host, ok
}

func (r *RuntimeRouter) SpawnVM(ctx context.Context, req SpawnVMRequest, requested TenantUsage) (*RoutedSpawnVMResponse, error) {
	decision, daemon, err := r.scheduleDaemon(ScheduleRequest{
		TenantID:     req.TenantID,
		Profile:      req.Profile,
		EgressPolicy: EgressPolicy(req.EgressPolicy),
	}, requested)
	if err != nil {
		return nil, err
	}
	req.TenantID = decision.TenantID
	req.EgressPolicy = string(decision.EgressPolicy)
	resp, err := daemon.SpawnVM(ctx, req)
	if err != nil {
		return nil, err
	}
	r.recordPlacement(resp.VMID, decision.Host.Name)
	return &RoutedSpawnVMResponse{SpawnVMResponse: *resp, Host: decision.Host}, nil
}

func (r *RuntimeRouter) RestoreSnapshot(ctx context.Context, snapshotID string, req RestoreSnapshotRequest, scheduleReq ScheduleRequest, requested TenantUsage) (*RoutedRestoreSnapshotResponse, error) {
	if scheduleReq.TenantID == "" {
		scheduleReq.TenantID = req.TenantID
	}
	if scheduleReq.EgressPolicy == "" {
		scheduleReq.EgressPolicy = EgressPolicy(req.EgressPolicy)
	}
	decision, daemon, err := r.scheduleDaemon(scheduleReq, requested)
	if err != nil {
		return nil, err
	}
	req.TenantID = decision.TenantID
	req.EgressPolicy = string(decision.EgressPolicy)
	resp, err := daemon.RestoreSnapshot(ctx, snapshotID, req)
	if err != nil {
		return nil, err
	}
	r.recordPlacement(resp.VMID, decision.Host.Name)
	return &RoutedRestoreSnapshotResponse{RestoreSnapshotResponse: *resp, Host: decision.Host}, nil
}

func (r *RuntimeRouter) RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	daemon, err := r.daemonForVM(vmID)
	if err != nil {
		return nil, err
	}
	return daemon.RunTask(ctx, vmID, prompt)
}

func (r *RuntimeRouter) Health(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	daemon, err := r.daemonForVM(vmID)
	if err != nil {
		return nil, err
	}
	return daemon.Health(ctx, vmID)
}

func (r *RuntimeRouter) CreateSnapshot(ctx context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	daemon, err := r.daemonForVM(vmID)
	if err != nil {
		return nil, err
	}
	return daemon.CreateSnapshot(ctx, vmID, req)
}

func (r *RuntimeRouter) Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	daemon, err := r.daemonForVM(vmID)
	if err != nil {
		return nil, err
	}
	resp, err := daemon.Delete(ctx, vmID)
	if err != nil {
		return nil, err
	}
	r.removePlacement(vmID)
	return resp, nil
}

func (r *RuntimeRouter) scheduleDaemon(req ScheduleRequest, requested TenantUsage) (ScheduleDecision, Daemon, error) {
	if r.scheduler == nil {
		return ScheduleDecision{}, nil, fmt.Errorf("runtime router scheduler is nil")
	}
	decision, err := r.scheduler.Schedule(req, requested)
	if err != nil {
		return ScheduleDecision{}, nil, err
	}
	if !decision.Allowed {
		return decision, nil, &ScheduleDeniedError{Decision: decision}
	}
	daemon, ok := r.daemons[decision.Host.Name]
	if !ok || daemon == nil {
		return decision, nil, fmt.Errorf("runtime host %q has no daemon client", decision.Host.Name)
	}
	return decision, daemon, nil
}

func (r *RuntimeRouter) daemonForVM(vmID string) (Daemon, error) {
	vmID = strings.TrimSpace(vmID)
	r.mu.RLock()
	hostName, ok := r.placement[vmID]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("vm %q has no runtime host placement", vmID)
	}
	daemon, ok := r.daemons[hostName]
	if !ok || daemon == nil {
		return nil, fmt.Errorf("runtime host %q has no daemon client", hostName)
	}
	return daemon, nil
}

func (r *RuntimeRouter) recordPlacement(vmID, hostName string) {
	vmID = strings.TrimSpace(vmID)
	hostName = strings.TrimSpace(hostName)
	if vmID == "" || hostName == "" {
		return
	}
	r.mu.Lock()
	r.placement[vmID] = hostName
	r.mu.Unlock()
}

func (r *RuntimeRouter) removePlacement(vmID string) {
	r.mu.Lock()
	delete(r.placement, strings.TrimSpace(vmID))
	r.mu.Unlock()
}
