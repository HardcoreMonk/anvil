package anvilmcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
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
	mu sync.RWMutex

	// reconcileMu serializes ReconcilePlacements end-to-end so the periodic
	// loop and manual calls never run concurrently (placement replace + wall
	// re-registration is not safe to interleave with itself).
	reconcileMu sync.Mutex

	scheduler      *Scheduler
	daemons        map[string]Daemon
	placement      map[string]string
	placementStore *PlacementStore
	maxAttempts    int

	// homeFailures counts CONSECUTIVE dial-class home failures per routed
	// flock id for failover detection. Guarded by reconcileMu: it is only
	// ever touched inside ReconcilePlacements, which reconcileMu serializes.
	homeFailures map[string]int

	// reconcileLogf reports reconcile/failover events (flock/host identifiers
	// only — never tokens or daemon addresses). Set by StartReconcileLoop;
	// nil-safe via logf.
	reconcileLogf func(format string, args ...any)
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
		homeFailures:   make(map[string]int),
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

func (r *RuntimeRouter) CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	totalStart := time.Now()
	requestedActiveVMs := int64(len(req.Roles))
	if requestedActiveVMs == 0 {
		requestedActiveVMs = 1
	}
	scheduleReq := ScheduleRequest{
		TenantID:           req.TenantID,
		RequestedActiveVMs: requestedActiveVMs,
		EgressPolicy:       EgressPolicy(req.EgressPolicy),
	}
	scheduleStart := time.Now()
	decision, daemon, err := r.scheduleDaemon(scheduleReq, TenantUsage{ActiveVMs: requestedActiveVMs})
	scheduleLatency := time.Since(scheduleStart)
	if err != nil {
		r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
			Outcome: flockPlacementOutcomeForScheduleError(err),
			Reason:  flockPlacementReasonForScheduleError(err),
			Latencies: map[string]time.Duration{
				FlockPlacementPhaseSchedule: scheduleLatency,
				FlockPlacementPhaseTotal:    time.Since(totalStart),
			},
		})
		return nil, err
	}
	req.TenantID = decision.TenantID
	req.EgressPolicy = string(decision.EgressPolicy)
	daemonCreateStart := time.Now()
	resp, err := daemon.CreateFlock(ctx, req)
	daemonCreateLatency := time.Since(daemonCreateStart)
	if err != nil {
		r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
			Outcome: FlockPlacementOutcomeDaemonError,
			Reason:  FlockPlacementReasonDaemonCreateFailed,
			Latencies: map[string]time.Duration{
				FlockPlacementPhaseSchedule:     scheduleLatency,
				FlockPlacementPhaseDaemonCreate: daemonCreateLatency,
				FlockPlacementPhaseTotal:        time.Since(totalStart),
			},
		})
		return nil, err
	}
	if resp == nil {
		nilResponseErr := fmt.Errorf("runtime daemon CreateFlock returned nil response")
		r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
			Outcome: FlockPlacementOutcomeDaemonNilResponse,
			Reason:  FlockPlacementReasonDaemonNilResponse,
			Latencies: map[string]time.Duration{
				FlockPlacementPhaseSchedule:     scheduleLatency,
				FlockPlacementPhaseDaemonCreate: daemonCreateLatency,
				FlockPlacementPhaseTotal:        time.Since(totalStart),
			},
		})
		return nil, nilResponseErr
	}
	placementSaveStart := time.Now()
	err = r.recordFlockPlacements(resp, decision.Host.Name)
	placementSaveLatency := time.Since(placementSaveStart)
	if err != nil {
		r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
			Outcome: FlockPlacementOutcomePlacementSaveError,
			Reason:  FlockPlacementReasonPlacementSaveFailed,
			Latencies: map[string]time.Duration{
				FlockPlacementPhaseSchedule:      scheduleLatency,
				FlockPlacementPhaseDaemonCreate:  daemonCreateLatency,
				FlockPlacementPhasePlacementSave: placementSaveLatency,
				FlockPlacementPhaseTotal:         time.Since(totalStart),
			},
		})
		flockID := strings.TrimSpace(resp.FlockID)
		return nil, fmt.Errorf("flock created but placement save failed: flock_id=%q: %w", flockID, err)
	}
	r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
		Outcome: FlockPlacementOutcomeSuccess,
		Reason:  FlockPlacementReasonScheduled,
		Latencies: map[string]time.Duration{
			FlockPlacementPhaseSchedule:      scheduleLatency,
			FlockPlacementPhaseDaemonCreate:  daemonCreateLatency,
			FlockPlacementPhasePlacementSave: placementSaveLatency,
			FlockPlacementPhaseTotal:         time.Since(totalStart),
		},
	})
	return resp, nil
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

// hostProbe records one reconcile pass's reachability observation for a host.
// reachable is true only when the host's daemon answered ListVMs this pass;
// dialFailed is true when ListVMs failed with a dial-class transport error
// (host down), the only failure class that counts toward home failover.
type hostProbe struct {
	reachable  bool
	dialFailed bool
}

func (r *RuntimeRouter) ReconcilePlacements(ctx context.Context) error {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	next := make(map[string]string)
	probes := make(map[string]hostProbe)
	var errs []error
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
			// Per-host fault isolation: one unreachable daemon must not abort
			// placement reconciliation and wall healing for every other host
			// (failover detection depends on reconcile continuing while the
			// home is down). Carry the failed host's existing placements over
			// unchanged — replacing them from a partial view would orphan its
			// VMs until the host returns.
			probes[hostName] = hostProbe{dialFailed: isDialError(err)}
			errs = append(errs, fmt.Errorf("list vms on runtime host %q failed", hostName))
			r.mu.RLock()
			for vmID, host := range r.placement {
				if host == hostName {
					next[vmID] = host
				}
			}
			r.mu.RUnlock()
			continue
		}
		probes[hostName] = hostProbe{reachable: true}
		for _, vm := range vms {
			if vmID := strings.TrimSpace(vm.VMID); vmID != "" {
				next[vmID] = hostName
			}
		}
	}
	r.mu.Lock()
	r.placement = next
	r.mu.Unlock()
	if r.placementStore != nil {
		if err := r.placementStore.ReplaceVMPlacements(next); err != nil {
			errs = append(errs, err)
		} else if err := r.placementStore.Save(); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, r.reconcileRoutedFlockWalls(ctx, probes))
	return errors.Join(errs...)
}

// StartReconcileLoop runs ReconcilePlacements once immediately and then every
// interval until ctx is cancelled. interval <= 0 disables the loop entirely
// (including the immediate run). Failures are logged through logf (flock/host
// identifiers only — relay tokens and daemon addresses never appear) and the
// loop keeps running: reconcile must never block or kill the adapter.
func (r *RuntimeRouter) StartReconcileLoop(ctx context.Context, interval time.Duration, logf func(format string, args ...any)) {
	if r == nil || interval <= 0 {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r.reconcileLogf = logf
	run := func() {
		if err := r.ReconcilePlacements(ctx); err != nil {
			logf("anvil-mcp: reconcile placements: %v", err)
		}
	}
	go func() {
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}

// logf reports a reconcile/failover event through the configured sink, if any.
// nil-safe: before StartReconcileLoop wires reconcileLogf (and in unit tests
// that inject it directly) a nil sink simply drops the line.
func (r *RuntimeRouter) logf(format string, args ...any) {
	if r.reconcileLogf != nil {
		r.reconcileLogf(format, args...)
	}
}

// reconcileRoutedFlockWalls re-registers the shared Town Wall hub and relay
// flocks for every persisted routed flock, healing the in-memory flock
// registrations a home/member daemon loses on restart. It re-issues
// RegisterDistributedFlock to the home daemon (VMID/Addr-enriched roster from
// the record's agents, relay + call token from the persisted store) and
// RegisterRelayFlock to each member host that is not the home host (that
// host's own local agents, so a hopped /call resolves locally). Registration
// endpoints are idempotent (Task 3), so re-issuing when already present is
// safe. A record with no persisted call token (pre-existing records) still
// reconciles with an empty CallToken — backward compatible, not skipped.
//
// Flocks that are deleted/deleting or carry no home host are skipped. A single
// flock's (or member's) failure is collected and reconcile continues so one
// unreachable daemon cannot block healing the rest. Error strings are bounded to
// flock/host identifiers only: the relay/call tokens and daemon addresses are
// never surfaced (redaction discipline — the tokens flow daemon-to-daemon only).
func (r *RuntimeRouter) reconcileRoutedFlockWalls(ctx context.Context, probes map[string]hostProbe) error {
	if r == nil || r.placementStore == nil {
		return nil
	}
	var errs []error
	live := make(map[string]bool)
	for _, record := range r.placementStore.ListRoutedFlocks() {
		flockID := strings.TrimSpace(record.FlockID)
		homeHost := strings.TrimSpace(record.HomeHost)
		if homeHost == "" {
			continue
		}
		if record.Status == RoutedFlockStatusDeleted || record.Status == RoutedFlockStatusDeleting {
			continue
		}
		relayToken, ok := r.placementStore.RoutedFlockRelayToken(flockID)
		if !ok || relayToken == "" {
			// No persisted relay secret (e.g. a record that never reached ready):
			// re-registering without it would admit an unauthenticated hub, so skip.
			continue
		}
		// Call token is optional for backward compatibility: older records saved
		// before the call token existed have none persisted. Re-registration
		// continues with an empty CallToken in that case (relay token alone still
		// heals the wall); it does not skip the record.
		callToken, _ := r.placementStore.RoutedFlockCallToken(flockID)
		live[flockID] = true

		// ── home failover detection (2026-07-08 spec) ─────────────────────
		// Only dial-class failures count: the probe's ListVMs dial failure
		// short-circuits (a doomed hub POST adds nothing but latency), and a
		// hub POST that fails with a dial error counts the same way. Any
		// successful hub registration resets the consecutive counter. HTTP
		// errors mean the host is alive: collected as heal errors, counter
		// untouched.
		homeDown := probes[homeHost].dialFailed
		if !homeDown {
			hubErr := r.registerRoutedHub(ctx, record, relayToken, callToken)
			switch {
			case hubErr == nil:
				r.homeFailures[flockID] = 0
			case isDialError(hubErr):
				homeDown = true
			default:
				errs = append(errs, fmt.Errorf("reconcile routed flock %q: hub re-registration on home host %q failed", flockID, homeHost))
				continue
			}
		}
		if homeDown {
			r.homeFailures[flockID]++
			if r.homeFailures[flockID] >= homeFailureThreshold && record.Status == RoutedFlockStatusReady {
				if newHome, ok := r.electNewHome(record, probes); ok {
					switched, err := r.failoverRoutedFlock(ctx, record, newHome, relayToken, callToken)
					if switched {
						r.homeFailures[flockID] = 0
					}
					if err != nil {
						errs = append(errs, err)
					}
					continue
				}
				// No reachable candidate: no-op, counter stays saturated so the
				// next pass with a revived member fires immediately (spec).
			}
			errs = append(errs, fmt.Errorf("reconcile routed flock %q: home host %q unreachable", flockID, homeHost))
			continue
		}
		errs = append(errs, r.registerRoutedRelays(ctx, record, relayToken, callToken)...)
	}
	// Sweep counters for flocks that no longer exist (deleted/removed) so the
	// map cannot grow unboundedly across flock lifecycles.
	for id := range r.homeFailures {
		if !live[id] {
			delete(r.homeFailures, id)
		}
	}
	return errors.Join(errs...)
}

// registerRoutedHub re-issues the hub registration for record on its current
// HomeHost: the VMID/Addr-enriched roster from the record's agents plus the
// persisted relay/call tokens. Error strings carry flock/host identifiers only.
func (r *RuntimeRouter) registerRoutedHub(ctx context.Context, record RoutedFlockRecord, relayToken, callToken string) error {
	flockID := strings.TrimSpace(record.FlockID)
	homeHost := strings.TrimSpace(record.HomeHost)
	homeDaemon, ok := r.daemons[homeHost]
	if !ok || homeDaemon == nil {
		return fmt.Errorf("reconcile routed flock %q: home host %q has no daemon client", flockID, homeHost)
	}
	roster := make([]RosterMember, 0, len(record.Agents))
	for _, a := range record.Agents {
		roster = append(roster, RosterMember{
			AgentID: strings.TrimSpace(a.AgentID),
			Host:    strings.TrimSpace(a.Host),
			VMID:    strings.TrimSpace(a.VMID),
			Addr:    r.daemonAddr(strings.TrimSpace(a.Host)),
		})
	}
	return homeDaemon.RegisterDistributedFlock(ctx, flockID, DistributedFlockRequest{Roster: roster, RelayToken: relayToken, CallToken: callToken})
}

// registerRoutedRelays re-issues relay registrations on every member host that
// is not the record's current HomeHost, each carrying that host's own local
// agents so a hopped /call resolves locally. Failures are collected per host
// (identifiers only) so one unreachable member cannot block the rest.
func (r *RuntimeRouter) registerRoutedRelays(ctx context.Context, record RoutedFlockRecord, relayToken, callToken string) []error {
	flockID := strings.TrimSpace(record.FlockID)
	homeHost := strings.TrimSpace(record.HomeHost)
	homeAddr := r.daemonAddr(homeHost)
	memberAgents := make(map[string][]RosterMember)
	for _, a := range record.Agents {
		host := strings.TrimSpace(a.Host)
		if host == "" || host == homeHost {
			continue
		}
		memberAgents[host] = append(memberAgents[host], RosterMember{
			AgentID: strings.TrimSpace(a.AgentID),
			VMID:    strings.TrimSpace(a.VMID),
		})
	}
	var errs []error
	relayed := map[string]bool{homeHost: true}
	for _, a := range record.Agents {
		host := strings.TrimSpace(a.Host)
		if host == "" || relayed[host] {
			continue
		}
		relayed[host] = true
		daemon, ok := r.daemons[host]
		if !ok || daemon == nil {
			errs = append(errs, fmt.Errorf("reconcile routed flock %q: member host %q has no daemon client", flockID, host))
			continue
		}
		if err := daemon.RegisterRelayFlock(ctx, flockID, RelayFlockRequest{
			HomeAddr:   homeAddr,
			RelayToken: relayToken,
			CallToken:  callToken,
			Agents:     memberAgents[host],
		}); err != nil {
			errs = append(errs, fmt.Errorf("reconcile routed flock %q: relay re-registration on member host %q failed", flockID, host))
		}
	}
	return errs
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

func flockPlacementOutcomeForScheduleError(err error) string {
	var denied *ScheduleDeniedError
	if errors.As(err, &denied) {
		return FlockPlacementOutcomeDenied
	}
	return FlockPlacementOutcomeSchedulerError
}

func flockPlacementReasonForScheduleError(err error) string {
	var denied *ScheduleDeniedError
	if errors.As(err, &denied) {
		return normalizeScheduleDecisionReason(denied.Decision.Reason)
	}
	return FlockPlacementReasonInvalidRequest
}

func normalizeScheduleDecisionReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case FlockPlacementReasonQuotaExceeded:
		return FlockPlacementReasonQuotaExceeded
	case FlockPlacementReasonNoEligibleHost:
		return FlockPlacementReasonNoEligibleHost
	default:
		return FlockPlacementReasonUnknown
	}
}

// daemonAddr returns the reachable base URL of a host's daemon, taken from the
// scheduler's host inventory (RuntimeHost.Endpoint) — the same source the
// runtime uses to reach each daemon. It is sent to member daemons as the home
// daemon address for wall relaying; it is never persisted in the routed-flock
// record or surfaced in MCP output.
func (r *RuntimeRouter) daemonAddr(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || r == nil || r.scheduler == nil {
		return ""
	}
	for _, h := range r.scheduler.hosts {
		if strings.TrimSpace(h.Name) == host {
			return strings.TrimSpace(h.Endpoint)
		}
	}
	return ""
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

func (r *RuntimeRouter) recordFlockPlacements(resp *FlockCreateResponse, hostName string) error {
	hostName = strings.TrimSpace(hostName)
	if resp == nil || hostName == "" {
		return nil
	}
	vmIDs := make([]string, 0, len(resp.Agents))
	for _, agent := range resp.Agents {
		vmID := strings.TrimSpace(agent.VMID)
		if vmID != "" {
			vmIDs = append(vmIDs, vmID)
		}
	}
	if len(vmIDs) == 0 {
		return nil
	}
	r.mu.Lock()
	for _, vmID := range vmIDs {
		r.placement[vmID] = hostName
	}
	r.mu.Unlock()
	if r.placementStore == nil {
		return nil
	}
	for _, vmID := range vmIDs {
		if err := r.placementStore.SetVMPlacement(vmID, hostName); err != nil {
			return err
		}
	}
	return r.placementStore.Save()
}

func (r *RuntimeRouter) recordFlockPlacementMetric(obs FlockPlacementMetricObservation) {
	if r == nil || r.placementStore == nil {
		return
	}
	_ = r.placementStore.RecordFlockPlacementMetrics(obs)
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
