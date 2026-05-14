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
	mu             sync.RWMutex
	scheduler      *Scheduler
	daemons        map[string]Daemon
	placement      map[string]string
	placementStore *PlacementStore
	maxAttempts    int
}

type RuntimeRouterOptions struct {
	PlacementStore *PlacementStore
	MaxAttempts    int
}

func NewRuntimeRouter(scheduler *Scheduler, daemons map[string]Daemon) *RuntimeRouter {
	return NewRuntimeRouterWithOptions(scheduler, daemons, RuntimeRouterOptions{})
}

func NewRuntimeRouterWithOptions(scheduler *Scheduler, daemons map[string]Daemon, opts RuntimeRouterOptions) *RuntimeRouter {
	daemonCopy := make(map[string]Daemon, len(daemons))
	for name, daemon := range daemons {
		daemonCopy[strings.TrimSpace(name)] = daemon
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	return &RuntimeRouter{
		scheduler:      scheduler,
		daemons:        daemonCopy,
		placement:      initialPlacements(opts.PlacementStore),
		placementStore: opts.PlacementStore,
		maxAttempts:    maxAttempts,
	}
}

func (r *RuntimeRouter) Placement(vmID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	host, ok := r.placement[strings.TrimSpace(vmID)]
	return host, ok
}

func (r *RuntimeRouter) SpawnVM(ctx context.Context, req SpawnVMRequest, requested TenantUsage) (*RoutedSpawnVMResponse, error) {
	scheduleReq := ScheduleRequest{
		TenantID:     req.TenantID,
		Profile:      req.Profile,
		EgressPolicy: EgressPolicy(req.EgressPolicy),
	}
	var lastErr error
	for attempt := 0; attempt < r.maxAttempts; attempt++ {
		decision, daemon, err := r.scheduleDaemon(scheduleReq, requested)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		req.TenantID = decision.TenantID
		req.EgressPolicy = string(decision.EgressPolicy)
		resp, err := daemon.SpawnVM(ctx, req)
		if err == nil {
			r.recordPlacement(resp.VMID, decision.Host.Name)
			return &RoutedSpawnVMResponse{SpawnVMResponse: *resp, Host: decision.Host}, nil
		}
		lastErr = err
		scheduleReq.ExcludedHosts = append(scheduleReq.ExcludedHosts, decision.Host.Name)
	}
	return nil, lastErr
}

func (r *RuntimeRouter) RestoreSnapshot(ctx context.Context, snapshotID string, req RestoreSnapshotRequest, scheduleReq ScheduleRequest, requested TenantUsage) (*RoutedRestoreSnapshotResponse, error) {
	if scheduleReq.TenantID == "" {
		scheduleReq.TenantID = req.TenantID
	}
	if scheduleReq.EgressPolicy == "" {
		scheduleReq.EgressPolicy = EgressPolicy(req.EgressPolicy)
	}
	if len(scheduleReq.PreferredHosts) == 0 && r.placementStore != nil {
		scheduleReq.PreferredHosts = r.placementStore.SnapshotHosts(snapshotID)
	}
	var lastErr error
	for attempt := 0; attempt < r.maxAttempts; attempt++ {
		decision, daemon, err := r.scheduleDaemon(scheduleReq, requested)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		req.TenantID = decision.TenantID
		req.EgressPolicy = string(decision.EgressPolicy)
		resp, err := daemon.RestoreSnapshot(ctx, snapshotID, req)
		if err == nil {
			r.recordPlacement(resp.VMID, decision.Host.Name)
			return &RoutedRestoreSnapshotResponse{RestoreSnapshotResponse: *resp, Host: decision.Host}, nil
		}
		lastErr = err
		scheduleReq.ExcludedHosts = append(scheduleReq.ExcludedHosts, decision.Host.Name)
	}
	return nil, lastErr
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
	resp, err := daemon.CreateSnapshot(ctx, vmID, req)
	if err != nil {
		return nil, err
	}
	if hostName, ok := r.Placement(vmID); ok && r.placementStore != nil {
		_ = r.placementStore.SetSnapshotLocation(resp.SnapshotID, hostName)
		_ = r.placementStore.Save()
	}
	return resp, nil
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

func (r *RuntimeRouter) ReconcilePlacements(ctx context.Context) error {
	next := make(map[string]string)
	for hostName, daemon := range r.daemons {
		if daemon == nil {
			continue
		}
		lister, ok := daemon.(interface {
			ListVMs(context.Context) ([]VMInfo, error)
		})
		if !ok {
			continue
		}
		vms, err := lister.ListVMs(ctx)
		if err != nil {
			return fmt.Errorf("list vms on runtime host %q: %w", hostName, err)
		}
		for _, vm := range vms {
			vmID := strings.TrimSpace(vm.VMID)
			if vmID != "" {
				next[vmID] = hostName
			}
		}
	}
	r.mu.Lock()
	r.placement = next
	r.mu.Unlock()
	if r.placementStore != nil {
		if err := r.placementStore.ReplaceVMPlacements(next); err != nil {
			return err
		}
		return r.placementStore.Save()
	}
	return nil
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
	if r.placementStore != nil {
		_ = r.placementStore.SetVMPlacement(vmID, hostName)
		_ = r.placementStore.Save()
	}
}

func (r *RuntimeRouter) removePlacement(vmID string) {
	r.mu.Lock()
	delete(r.placement, strings.TrimSpace(vmID))
	r.mu.Unlock()
	if r.placementStore != nil {
		r.placementStore.RemoveVMPlacement(vmID)
		_ = r.placementStore.Save()
	}
}

func initialPlacements(store *PlacementStore) map[string]string {
	out := make(map[string]string)
	if store == nil {
		return out
	}
	state := store.State()
	for vmID, hostName := range state.VMPlacements {
		vmID = strings.TrimSpace(vmID)
		hostName = strings.TrimSpace(hostName)
		if vmID != "" && hostName != "" {
			out[vmID] = hostName
		}
	}
	return out
}
