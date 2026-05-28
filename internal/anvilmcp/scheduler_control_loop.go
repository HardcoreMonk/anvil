package anvilmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type SchedulerControlLoopOptions struct {
	HTTPClient        *http.Client
	APIToken          string
	HostTimeout       time.Duration
	FailureThreshold  int
	PollInterval      time.Duration
	ReconcileInterval time.Duration
}

type SchedulerControlLoop struct {
	store             *PlacementStore
	http              *http.Client
	apiToken          string
	hostTimeout       time.Duration
	failureThreshold  int
	pollInterval      time.Duration
	reconcileInterval time.Duration
	mu                sync.Mutex
	persistenceError  string
}

const reconcileInventoryErrorPrefix = "list vms: "

func NewSchedulerControlLoop(store *PlacementStore, opts SchedulerControlLoopOptions) *SchedulerControlLoop {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	hostTimeout := opts.HostTimeout
	if hostTimeout <= 0 {
		hostTimeout = defaultHostInventoryTimeout
	}
	failureThreshold := opts.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 10 * time.Second
	}
	reconcileInterval := opts.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = 30 * time.Second
	}
	return &SchedulerControlLoop{
		store:             store,
		http:              httpClient,
		apiToken:          strings.TrimSpace(opts.APIToken),
		hostTimeout:       hostTimeout,
		failureThreshold:  failureThreshold,
		pollInterval:      pollInterval,
		reconcileInterval: reconcileInterval,
	}
}

func (l *SchedulerControlLoop) PollOnce(ctx context.Context) error {
	return l.pollOnce(ctx, false)
}

func (l *SchedulerControlLoop) ReconcileOnce(ctx context.Context) error {
	return l.reconcileOnce(ctx, false)
}

func (l *SchedulerControlLoop) Start(ctx context.Context) {
	go func() {
		_ = l.pollOnce(ctx, true)
		_ = l.reconcileOnce(ctx, true)

		pollTicker := time.NewTicker(l.pollInterval)
		defer pollTicker.Stop()
		reconcileTicker := time.NewTicker(l.reconcileInterval)
		defer reconcileTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				l.markStopped()
				return
			case <-pollTicker.C:
				_ = l.pollOnce(ctx, true)
			case <-reconcileTicker.C:
				_ = l.reconcileOnce(ctx, true)
			}
		}
	}()
}

func (l *SchedulerControlLoop) pollOnce(ctx context.Context, running bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store == nil {
		return fmt.Errorf("scheduler control loop requires placement store")
	}

	started := time.Now().UTC()
	state := l.store.State()
	for _, host := range state.Hosts {
		if ctx.Err() != nil {
			return nil
		}
		previous := state.HostObservations[host.Name]
		polled, obs, apply := l.pollHost(ctx, host, previous)
		if !apply {
			return nil
		}
		applied, err := l.store.ApplyHostObservation(host, polled, obs)
		if err != nil {
			return err
		}
		if !applied {
			continue
		}
		switch obs.Status {
		case HostStatusHealthy:
			l.store.ClearHostSuspectPlacements(polled.Name)
		case HostStatusDegraded:
			_ = l.store.MarkHostPlacementsSuspect(polled.Name, "host_degraded")
		case HostStatusUnhealthy:
			_ = l.store.MarkHostPlacementsSuspect(polled.Name, "host_unhealthy")
		}
	}
	completed := time.Now().UTC()
	l.persistenceError = ""
	_ = l.updateStatus(started, completed, false, running)
	if err := l.store.Save(); err != nil {
		l.recordPersistenceError(err)
	}
	return nil
}

func (l *SchedulerControlLoop) reconcileOnce(ctx context.Context, running bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store == nil {
		return fmt.Errorf("scheduler control loop requires placement store")
	}

	started := time.Now().UTC()
	state := l.store.State()
	nextPlacements := make(map[string]string)
	clearSuspectHosts := make([]string, 0, len(state.Hosts))

	for _, host := range state.Hosts {
		if ctx.Err() != nil {
			return nil
		}
		obs := state.HostObservations[host.Name]
		if obs.Status == "" {
			if host.Healthy {
				obs.Status = HostStatusHealthy
			} else {
				obs.Status = HostStatusUnhealthy
			}
		}
		if !shouldReconcileHostInventory(host, obs) {
			observedHost := host
			if obs.Status == HostStatusDegraded || obs.Status == HostStatusUnhealthy {
				observedHost.Healthy = false
			}
			applied, err := l.store.ApplyHostObservation(host, observedHost, obs)
			if err != nil {
				return err
			}
			if !applied {
				continue
			}
			reason := suspectReasonForHostStatus(obs.Status)
			preserveHostPlacements(nextPlacements, state.VMPlacements, host.Name)
			_ = l.store.MarkHostPlacementsSuspect(host.Name, reason)
			continue
		}

		vms, err := l.listHostVMs(ctx, host)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			now := time.Now().UTC()
			obs.FailureCount++
			obs.Status = l.hostStatusForFailureCount(obs.FailureCount)
			obs.LastFailureAt = now
			obs.LastError = reconcileInventoryLastError(err)
			observedHost := host
			observedHost.Healthy = false
			applied, err := l.store.ApplyHostObservation(host, observedHost, obs)
			if err != nil {
				return err
			}
			if !applied {
				continue
			}
			preserveHostPlacements(nextPlacements, state.VMPlacements, host.Name)
			_ = l.store.MarkHostPlacementsSuspect(host.Name, suspectReasonForHostStatus(obs.Status))
			continue
		}
		now := time.Now().UTC()
		observedHost := host
		observedHost.Healthy = true
		obs.Status = HostStatusHealthy
		obs.FailureCount = 0
		obs.LastSuccessAt = now
		obs.LastError = ""
		applied, err := l.store.ApplyHostObservation(host, observedHost, obs)
		if err != nil {
			return err
		}
		if !applied {
			continue
		}
		for _, vm := range vms {
			vmID := strings.TrimSpace(vm.VMID)
			if vmID != "" {
				nextPlacements[vmID] = host.Name
			}
		}
		clearSuspectHosts = append(clearSuspectHosts, host.Name)
	}

	_ = l.store.ReconcileVMPlacements(state.VMPlacements, nextPlacements)
	for _, hostName := range clearSuspectHosts {
		l.store.ClearHostSuspectPlacements(hostName)
	}
	completed := time.Now().UTC()
	l.persistenceError = ""
	_ = l.updateStatus(started, completed, true, running)
	if err := l.store.Save(); err != nil {
		l.recordPersistenceError(err)
	}
	return nil
}

func (l *SchedulerControlLoop) pollHost(ctx context.Context, host RuntimeHost, previous HostObservation) (RuntimeHost, HostObservation, bool) {
	health, err := l.getHostHealth(ctx, host)
	now := time.Now().UTC()
	if ctx.Err() != nil {
		return host, previous, false
	}
	if hasActiveReconcileInventoryFailure(previous) {
		host.Healthy = false
		return host, previous, true
	}
	if err != nil {
		previous.FailureCount++
		previous.LastFailureAt = now
		previous.LastError = sanitizeSchedulerError(err.Error())
		previous.Status = l.hostStatusForFailureCount(previous.FailureCount)
		host.Healthy = false
		return host, previous, true
	}

	host.Healthy = true
	host.AvailableVMs = health.AvailableVMs
	host.AvailableSnapshotBytes = health.AvailableSnapshotBytes
	host.EgressPolicies = append([]EgressPolicy(nil), health.EgressPolicies...)
	return host, HostObservation{
		Status:                 HostStatusHealthy,
		AvailableVMs:           health.AvailableVMs,
		AvailableSnapshotBytes: health.AvailableSnapshotBytes,
		LastSuccessAt:          now,
	}, true
}

func (l *SchedulerControlLoop) hostStatusForFailureCount(failureCount int) HostStatus {
	if failureCount >= l.failureThreshold {
		return HostStatusUnhealthy
	}
	return HostStatusDegraded
}

func shouldReconcileHostInventory(host RuntimeHost, obs HostObservation) bool {
	if obs.Status == HostStatusDegraded || obs.Status == HostStatusUnhealthy {
		return isReconcileInventoryFailure(obs)
	}
	return host.Healthy
}

func isReconcileInventoryFailure(obs HostObservation) bool {
	return strings.HasPrefix(obs.LastError, reconcileInventoryErrorPrefix)
}

func hasActiveReconcileInventoryFailure(obs HostObservation) bool {
	return (obs.Status == HostStatusDegraded || obs.Status == HostStatusUnhealthy) && isReconcileInventoryFailure(obs)
}

func (l *SchedulerControlLoop) getHostHealth(ctx context.Context, host RuntimeHost) (hostHealthResponse, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(host.Endpoint), "/")
	if endpoint == "" {
		return hostHealthResponse{}, fmt.Errorf("host endpoint is empty")
	}
	reqCtx, cancel := context.WithTimeout(ctx, l.hostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint+"/health", nil)
	if err != nil {
		return hostHealthResponse{}, err
	}
	if l.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.apiToken)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return hostHealthResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return hostHealthResponse{}, fmt.Errorf("health returned %d", resp.StatusCode)
	}
	var health hostHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return hostHealthResponse{}, err
	}
	if strings.ToLower(strings.TrimSpace(health.Status)) != "ok" {
		return hostHealthResponse{}, fmt.Errorf("health status %q", health.Status)
	}
	return health, nil
}

func (l *SchedulerControlLoop) listHostVMs(ctx context.Context, host RuntimeHost) ([]VMInfo, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(host.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("host endpoint is empty")
	}
	reqCtx, cancel := context.WithTimeout(ctx, l.hostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint+"/vms", nil)
	if err != nil {
		return nil, err
	}
	if l.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.apiToken)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list vms returned %d", resp.StatusCode)
	}
	var vms []VMInfo
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		return nil, err
	}
	return vms, nil
}

func (l *SchedulerControlLoop) updateStatus(started, completed time.Time, reconcile bool, running bool) error {
	if l.store == nil {
		return fmt.Errorf("scheduler control loop requires placement store")
	}
	state := l.store.State()
	status := state.ControlLoopStatus
	status.Running = running
	status.PollIntervalSeconds = int64(l.pollInterval / time.Second)
	status.ReconcileIntervalSeconds = int64(l.reconcileInterval / time.Second)
	status.FailureThreshold = l.failureThreshold
	status.PersistenceDegraded = l.persistenceError != ""
	status.LastError = l.persistenceError
	status.Hosts = state.HostObservations
	if reconcile {
		status.LastReconcileStartedAt = started
		status.LastReconcileCompletedAt = completed
	} else {
		status.LastPollStartedAt = started
		status.LastPollCompletedAt = completed
	}
	return l.store.SetControlLoopStatus(status)
}

func (l *SchedulerControlLoop) recordPersistenceError(err error) {
	if err == nil {
		l.persistenceError = ""
	} else {
		l.persistenceError = sanitizeSchedulerError(err.Error())
	}
	if l.store == nil {
		return
	}
	state := l.store.State()
	status := state.ControlLoopStatus
	status.PersistenceDegraded = l.persistenceError != ""
	status.LastError = l.persistenceError
	status.Hosts = state.HostObservations
	_ = l.store.SetControlLoopStatus(status)
}

func (l *SchedulerControlLoop) markStopped() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store == nil {
		return
	}
	state := l.store.State()
	status := state.ControlLoopStatus
	status.Running = false
	status.Hosts = state.HostObservations
	_ = l.store.SetControlLoopStatus(status)
	_ = l.store.Save()
}

func sanitizeSchedulerError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 240 {
		return value[:240]
	}
	return value
}

func reconcileInventoryLastError(err error) string {
	value := "unknown"
	if err != nil {
		value = sanitizeSchedulerError(err.Error())
	}
	return sanitizeSchedulerError(reconcileInventoryErrorPrefix + value)
}

func suspectReasonForHostStatus(status HostStatus) string {
	if status == HostStatusDegraded {
		return "host_degraded"
	}
	return "host_unhealthy"
}

func preserveHostPlacements(next, current map[string]string, hostName string) {
	for vmID, placedHost := range current {
		if placedHost == hostName {
			next[vmID] = hostName
		}
	}
}
