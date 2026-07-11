package anvilmcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type RoutedFlockCreateOutput struct {
	FlockID         string             `json:"flock_id"`
	Task            string             `json:"task"`
	TenantID        string             `json:"tenant_id,omitempty"`
	EgressPolicy    string             `json:"egress_policy,omitempty"`
	Mode            string             `json:"mode"`
	Status          string             `json:"status"`
	TownWallEnabled bool               `json:"town_wall_enabled"`
	Agents          []RoutedFlockAgent `json:"agents"`
}

const (
	routedFlockAgentStatusCleanupPending = "cleanup_pending"
	routedFlockReasonCleanupFailed       = "cleanup_failed"
)

type routedFlockCreateFailureMetric struct {
	Outcome             string
	Reason              string
	PlanLatency         time.Duration
	AgentSpawnLatency   time.Duration
	RegistrySaveLatency time.Duration
	TotalStart          time.Time
}

func (r *RuntimeRouter) CreateRoutedFlockMembers(ctx context.Context, req FlockCreateRequest) (*RoutedFlockCreateOutput, error) {
	if r == nil {
		return nil, fmt.Errorf("runtime router is nil")
	}
	if r.scheduler == nil {
		return nil, fmt.Errorf("runtime router scheduler is nil")
	}
	if r.placementStore == nil || strings.TrimSpace(r.placementStore.path) == "" {
		return nil, fmt.Errorf("routed flock create requires persistent placement store")
	}

	totalStart := time.Now()
	planStart := time.Now()
	plan, err := r.scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     req.TenantID,
		EgressPolicy: EgressPolicy(req.EgressPolicy),
		Roles:        req.Roles,
	})
	planLatency := time.Since(planStart)
	if err != nil {
		r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
			Outcome: FlockPlacementOutcomeSchedulerError,
			Reason:  FlockPlacementReasonInvalidRequest,
			Latencies: map[string]time.Duration{
				FlockPlacementPhasePlan:  planLatency,
				FlockPlacementPhaseTotal: time.Since(totalStart),
			},
		})
		return nil, err
	}
	if !plan.Allowed {
		r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
			Outcome: FlockPlacementOutcomeCrossHostDenied,
			Reason:  normalizeScheduleDecisionReason(plan.Reason),
			Latencies: map[string]time.Duration{
				FlockPlacementPhasePlan:  planLatency,
				FlockPlacementPhaseTotal: time.Since(totalStart),
			},
		})
		return nil, &ScheduleDeniedError{Decision: scheduleDecisionFromFlockPlan(plan)}
	}

	now := time.Now().UTC()
	record := RoutedFlockRecord{
		FlockID:      fmt.Sprintf("routed-flock-%d", now.UnixNano()),
		Task:         strings.TrimSpace(req.Task),
		TenantID:     plan.TenantID,
		EgressPolicy: string(plan.EgressPolicy),
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusCreating,
		Agents:       []RoutedFlockAgent{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if record.Task == "" {
		return nil, fmt.Errorf("task must be non-empty")
	}
	if len(plan.Agents) == 0 {
		return nil, fmt.Errorf("routed flock create failed: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonNoEligibleHost))
	}

	// Home host is deterministic: the host where roles[0] (plan.Agents[0]) is
	// placed. The home daemon owns the shared Town Wall hub; every other member
	// host relays to it under one per-flock secret.
	homeHost := strings.TrimSpace(plan.Agents[0].Host.Name)
	relayToken, err := newRelayToken()
	if err != nil {
		return nil, fmt.Errorf("routed flock create failed: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonUnknown))
	}
	callToken, err := newCallToken()
	if err != nil {
		return nil, fmt.Errorf("routed flock create failed: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonUnknown))
	}
	record.HomeHost = homeHost
	// Persist the relay secret from this first save (before hub/relay registration
	// and any spawn) so a crash mid-create leaves the store able to reconcile: a
	// live token registered on a daemon with none in the store makes
	// ReconcilePlacements skip the record. The store copies the token into its
	// redacted RoutedFlockRelayTokens map; we scrub the local carrier right after
	// the save so later mid-loop/rollback saves carry an empty token, which
	// preserves the persisted entry instead of re-writing (or re-adding) it.
	record.relayToken = relayToken
	// Call token mirrors the relay token exactly: persisted on this same first
	// save (dedicated RoutedFlockCallTokens map) and scrubbed from the local
	// carrier right after, for the same crash-recovery/reconcile reasons.
	record.callToken = callToken

	var registrySaveLatency time.Duration
	registrySaveStart := time.Now()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		registrySaveLatency += time.Since(registrySaveStart)
		r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
			Outcome: FlockPlacementOutcomeCrossHostRegistryError,
			Reason:  FlockPlacementReasonPlacementSaveFailed,
			Latencies: map[string]time.Duration{
				FlockPlacementPhasePlan:         planLatency,
				FlockPlacementPhaseRegistrySave: registrySaveLatency,
				FlockPlacementPhaseTotal:        time.Since(totalStart),
			},
		})
		return nil, err
	}
	registrySaveLatency += time.Since(registrySaveStart)
	record.relayToken = ""
	record.callToken = ""

	// Register the shared Town Wall hub on the home daemon: the full roster plus
	// the per-flock relay secret it will admit for the flock's wall sub-paths.
	roster := make([]RosterMember, 0, len(plan.Agents))
	for _, a := range plan.Agents {
		roster = append(roster, RosterMember{AgentID: strings.TrimSpace(a.AgentID), Host: strings.TrimSpace(a.Host.Name)})
	}
	homeDaemon, ok := r.daemons[homeHost]
	if !ok || homeDaemon == nil {
		return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
			Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
			Reason:              FlockPlacementReasonDaemonCreateFailed,
			PlanLatency:         planLatency,
			RegistrySaveLatency: registrySaveLatency,
			TotalStart:          totalStart,
		})
	}
	if err := homeDaemon.RegisterDistributedFlock(ctx, record.FlockID, DistributedFlockRequest{Roster: roster, RelayToken: relayToken, CallToken: callToken}); err != nil {
		return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
			Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
			Reason:              FlockPlacementReasonDaemonCreateFailed,
			PlanLatency:         planLatency,
			RegistrySaveLatency: registrySaveLatency,
			TotalStart:          totalStart,
		})
	}
	homeAddr := r.daemonAddr(homeHost)

	var agentSpawnLatency time.Duration
	// relayHosts tracks member hosts whose relay flock registered successfully so
	// rollback can deregister them even when the member's later SpawnVM fails before
	// the member ever enters record.Agents (which deregisterRoutedFlockWall walks).
	var relayHosts []string
	for _, planned := range plan.Agents {
		hostName := strings.TrimSpace(planned.Host.Name)
		daemon, ok := r.daemons[hostName]
		if !ok || daemon == nil {
			return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
				Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
				Reason:              FlockPlacementReasonDaemonCreateFailed,
				PlanLatency:         planLatency,
				AgentSpawnLatency:   agentSpawnLatency,
				RegistrySaveLatency: registrySaveLatency,
				TotalStart:          totalStart,
			}, relayHosts...)
		}

		// Register a relay flock on each member host so its guests' wall traffic
		// forwards to the home daemon. Skip the home host: it already owns the hub.
		if hostName != homeHost {
			if err := daemon.RegisterRelayFlock(ctx, record.FlockID, RelayFlockRequest{HomeAddr: homeAddr, RelayToken: relayToken, CallToken: callToken}); err != nil {
				return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
					Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
					Reason:              FlockPlacementReasonDaemonCreateFailed,
					PlanLatency:         planLatency,
					AgentSpawnLatency:   agentSpawnLatency,
					RegistrySaveLatency: registrySaveLatency,
					TotalStart:          totalStart,
				}, relayHosts...)
			}
			relayHosts = append(relayHosts, hostName)
		}

		spawnStart := time.Now()
		resp, err := daemon.SpawnVM(ctx, SpawnVMRequest{
			Profile:      planned.Role,
			TenantID:     plan.TenantID,
			EgressPolicy: string(plan.EgressPolicy),
			// Flock identity: the daemon injects .ephemera-flock / .ephemera-cp-token
			// so gtwall works in-VM. The guest CP token is the per-flock relay token,
			// which the home daemon admits only for this flock's wall sub-paths.
			FlockID:           record.FlockID,
			AgentID:           strings.TrimSpace(planned.AgentID),
			ControlPlaneToken: relayToken,
		})
		agentSpawnLatency += time.Since(spawnStart)
		if err != nil {
			return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
				Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
				Reason:              FlockPlacementReasonDaemonCreateFailed,
				PlanLatency:         planLatency,
				AgentSpawnLatency:   agentSpawnLatency,
				RegistrySaveLatency: registrySaveLatency,
				TotalStart:          totalStart,
			}, relayHosts...)
		}
		if resp == nil {
			return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
				Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
				Reason:              FlockPlacementReasonDaemonNilResponse,
				PlanLatency:         planLatency,
				AgentSpawnLatency:   agentSpawnLatency,
				RegistrySaveLatency: registrySaveLatency,
				TotalStart:          totalStart,
			}, relayHosts...)
		}
		vmID := strings.TrimSpace(resp.VMID)
		if vmID == "" {
			return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
				Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
				Reason:              FlockPlacementReasonDaemonInvalidResponse,
				PlanLatency:         planLatency,
				AgentSpawnLatency:   agentSpawnLatency,
				RegistrySaveLatency: registrySaveLatency,
				TotalStart:          totalStart,
			}, relayHosts...)
		}

		record.Agents = append(record.Agents, RoutedFlockAgent{
			AgentID:  planned.AgentID,
			Role:     planned.Role,
			VMID:     vmID,
			AgentURL: strings.TrimSpace(resp.AgentURL),
			Host:     hostName,
			Status:   "running",
		})
		record.UpdatedAt = time.Now().UTC()

		registrySaveStart = time.Now()
		if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
			registrySaveLatency += time.Since(registrySaveStart)
			return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
				Outcome:             FlockPlacementOutcomeCrossHostRegistryError,
				Reason:              FlockPlacementReasonPlacementSaveFailed,
				PlanLatency:         planLatency,
				AgentSpawnLatency:   agentSpawnLatency,
				RegistrySaveLatency: registrySaveLatency,
				TotalStart:          totalStart,
			}, relayHosts...)
		}
		registrySaveLatency += time.Since(registrySaveStart)
		r.recordRoutedFlockAgentPlacement(record.Agents[len(record.Agents)-1])
	}

	// Re-register the hub with the VMID/Addr-enriched roster: the initial
	// pre-spawn registration cannot know VM ids, and the hub needs them (plus
	// each member host daemon's address) to resolve and hop cross-host calls.
	// The daemon's idempotent hub path updates the roster in place (Task 1).
	enriched := make([]RosterMember, 0, len(record.Agents))
	for _, a := range record.Agents {
		enriched = append(enriched, RosterMember{
			AgentID: strings.TrimSpace(a.AgentID),
			Host:    strings.TrimSpace(a.Host),
			VMID:    strings.TrimSpace(a.VMID),
			Addr:    r.daemonAddr(strings.TrimSpace(a.Host)),
		})
	}
	if err := homeDaemon.RegisterDistributedFlock(ctx, record.FlockID, DistributedFlockRequest{Roster: enriched, RelayToken: relayToken, CallToken: callToken}); err != nil {
		return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
			Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
			Reason:              FlockPlacementReasonDaemonCreateFailed,
			PlanLatency:         planLatency,
			AgentSpawnLatency:   agentSpawnLatency,
			RegistrySaveLatency: registrySaveLatency,
			TotalStart:          totalStart,
		}, relayHosts...)
	}

	// Re-register each non-home member host's relay flock with that host's own
	// local agents (agent_id + vm_id): this lets a hopped /call landing on the
	// member daemon resolve the target locally instead of 404ing (2026-07-08
	// design correction). Grouped per host so a host with multiple agents gets
	// one re-registration carrying its full local roster.
	memberAgents := make(map[string][]RosterMember)
	var memberHostOrder []string
	for _, a := range record.Agents {
		host := strings.TrimSpace(a.Host)
		if host == "" || host == homeHost {
			continue
		}
		if _, seen := memberAgents[host]; !seen {
			memberHostOrder = append(memberHostOrder, host)
		}
		memberAgents[host] = append(memberAgents[host], RosterMember{
			AgentID: strings.TrimSpace(a.AgentID),
			VMID:    strings.TrimSpace(a.VMID),
		})
	}
	for _, host := range memberHostOrder {
		daemon, ok := r.daemons[host]
		if !ok || daemon == nil {
			return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
				Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
				Reason:              FlockPlacementReasonDaemonCreateFailed,
				PlanLatency:         planLatency,
				AgentSpawnLatency:   agentSpawnLatency,
				RegistrySaveLatency: registrySaveLatency,
				TotalStart:          totalStart,
			}, relayHosts...)
		}
		if err := daemon.RegisterRelayFlock(ctx, record.FlockID, RelayFlockRequest{
			HomeAddr:   homeAddr,
			RelayToken: relayToken,
			CallToken:  callToken,
			Agents:     memberAgents[host],
		}); err != nil {
			return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
				Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
				Reason:              FlockPlacementReasonDaemonCreateFailed,
				PlanLatency:         planLatency,
				AgentSpawnLatency:   agentSpawnLatency,
				RegistrySaveLatency: registrySaveLatency,
				TotalStart:          totalStart,
			}, relayHosts...)
		}
	}

	record.Status = RoutedFlockStatusReady
	// The relay secret was already persisted on the first save (above); the store
	// preserves it across these token-less saves, so no re-assignment is needed.
	record.UpdatedAt = time.Now().UTC()
	registrySaveStart = time.Now()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		registrySaveLatency += time.Since(registrySaveStart)
		return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
			Outcome:             FlockPlacementOutcomeCrossHostRegistryError,
			Reason:              FlockPlacementReasonPlacementSaveFailed,
			PlanLatency:         planLatency,
			AgentSpawnLatency:   agentSpawnLatency,
			RegistrySaveLatency: registrySaveLatency,
			TotalStart:          totalStart,
		}, relayHosts...)
	}
	registrySaveLatency += time.Since(registrySaveStart)

	r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
		Outcome: FlockPlacementOutcomeCrossHostSuccess,
		Reason:  FlockPlacementReasonScheduled,
		Latencies: map[string]time.Duration{
			FlockPlacementPhasePlan:         planLatency,
			FlockPlacementPhaseAgentSpawn:   agentSpawnLatency,
			FlockPlacementPhaseRegistrySave: registrySaveLatency,
			FlockPlacementPhaseTotal:        time.Since(totalStart),
		},
	})
	return routedFlockCreateOutput(record), nil
}

func routedFlockCreateOutput(record RoutedFlockRecord) *RoutedFlockCreateOutput {
	record = cloneRoutedFlockRecord(record)
	return &RoutedFlockCreateOutput{
		FlockID:         record.FlockID,
		Task:            record.Task,
		TenantID:        record.TenantID,
		EgressPolicy:    record.EgressPolicy,
		Mode:            record.Mode,
		Status:          record.Status,
		TownWallEnabled: true,
		Agents:          append([]RoutedFlockAgent(nil), record.Agents...),
	}
}

// newFlockSecret returns a fresh per-flock secret (32 random bytes, hex). It
// backs both the relay token and the call token: they are independent
// secrets minted the same way but scoped to different admission paths (wall
// vs /call).
func newFlockSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate flock secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// newRelayToken returns a fresh per-flock relay secret (32 random bytes, hex).
func newRelayToken() (string, error) {
	return newFlockSecret()
}

// newCallToken returns a fresh per-flock call-hop secret (32 random bytes,
// hex). Admitted for /call only, never wall paths.
func newCallToken() (string, error) {
	return newFlockSecret()
}

func (r *RuntimeRouter) recordRoutedFlockMetric(obs FlockPlacementMetricObservation) {
	if r == nil || r.placementStore == nil {
		return
	}
	_ = r.placementStore.RecordFlockPlacementMetrics(obs)
}

func (r *RuntimeRouter) recordRoutedFlockAgentPlacement(agent RoutedFlockAgent) {
	vmID := strings.TrimSpace(agent.VMID)
	hostName := strings.TrimSpace(agent.Host)
	if vmID == "" || hostName == "" {
		return
	}
	r.mu.Lock()
	r.placement[vmID] = hostName
	r.mu.Unlock()
}

func scheduleDecisionFromFlockPlan(plan FlockPlacementPlan) ScheduleDecision {
	return ScheduleDecision{
		Allowed:           plan.Allowed,
		Reason:            plan.Reason,
		TenantID:          plan.TenantID,
		HostStatusSummary: plan.HostStatusSummary,
		Quota:             plan.Quota,
		EgressPolicy:      plan.EgressPolicy,
		Requested:         plan.Requested,
		CurrentUsage:      plan.CurrentUsage,
		Limit:             plan.Limit,
	}
}

// deregisterRoutedFlockWall best-effort tears down the cross-host shared Town
// Wall registrations for a routed flock: the hub flock on the home daemon and the
// relay flock on each member host that was registered. The home daemon's flock
// delete also revokes the admitted relay and call tokens, so a rolled-back or
// deleted flock's secrets can no longer authenticate a wall hop or a /call hop.
// The persisted relay and call tokens are stripped from the store too, so no
// stale secret lingers on disk. Every step is best-effort: failures are
// swallowed so deregistration never masks the original create/delete outcome.
func (r *RuntimeRouter) deregisterRoutedFlockWall(ctx context.Context, record RoutedFlockRecord, extraHosts ...string) {
	if r == nil {
		return
	}
	flockID := strings.TrimSpace(record.FlockID)
	if flockID == "" {
		return
	}
	seen := make(map[string]bool)
	hosts := make([]string, 0, len(record.Agents)+len(extraHosts)+1)
	if home := strings.TrimSpace(record.HomeHost); home != "" {
		hosts = append(hosts, home)
		seen[home] = true
	}
	for _, agent := range record.Agents {
		host := strings.TrimSpace(agent.Host)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	// extraHosts covers member hosts whose relay flock registered but whose member
	// never entered record.Agents (e.g. a spawn-failed member during create rollback).
	for _, host := range extraHosts {
		host = strings.TrimSpace(host)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	for _, host := range hosts {
		daemon, ok := r.daemons[host]
		if !ok || daemon == nil {
			continue
		}
		// DELETE /flocks/{id} deregisters the hub (home) or relay (member) flock.
		// Best-effort: ignore the error so a retried create still starts clean.
		_, _ = daemon.DeleteFlock(ctx, flockID)
	}
	if r.placementStore != nil {
		_ = r.placementStore.removeRoutedFlockRelayToken(flockID)
		_ = r.placementStore.removeRoutedFlockCallToken(flockID)
	}
}

func rollbackRoutedFlockCreate(ctx context.Context, r *RuntimeRouter, record RoutedFlockRecord, metric routedFlockCreateFailureMetric, extraHosts ...string) error {
	reason := sanitizeRoutedFlockErrorReason(metric.Reason)
	rollbackStart := time.Now()
	// Best-effort deregistration of the shared Town Wall before VM teardown, so a
	// retried create starts clean and no stale hub/relay registration or relay
	// token lingers. extraHosts carries member hosts whose relay flock registered
	// but which never made it into record.Agents (spawn-failed members). Failures
	// here must not change the rollback outcome.
	r.deregisterRoutedFlockWall(ctx, record, extraHosts...)
	removeVMIDs, pendingAgents := r.deleteRoutedFlockAgents(ctx, record.Agents)
	rollbackLatency := time.Since(rollbackStart)
	r.removeRoutedFlockPlacements(removeVMIDs)

	if len(pendingAgents) == 0 {
		record.Status = RoutedFlockStatusDeleted
		record.Agents = []RoutedFlockAgent{}
		record.UpdatedAt = time.Now().UTC()
		if err := r.placementStore.SaveRoutedFlockAndPlacements(record, removeVMIDs); err != nil {
			preserveRoutedFlockCleanupStateInMemory(r.placementStore, record, removeVMIDs)
			r.recordRoutedFlockMetric(metric.observation(FlockPlacementOutcomeCrossHostRollbackError, reason, rollbackLatency))
			return sanitizedRoutedFlockCreateError(record.FlockID, reason, true)
		}
		r.recordRoutedFlockMetric(metric.observation(metric.Outcome, reason, rollbackLatency))
		return sanitizedRoutedFlockCreateError(record.FlockID, reason, false)
	}

	record.Status = RoutedFlockStatusFailedCleanupPending
	record.Agents = pendingAgents
	record.UpdatedAt = time.Now().UTC()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, removeVMIDs); err != nil {
		preserveRoutedFlockCleanupStateInMemory(r.placementStore, record, removeVMIDs)
		r.recordRoutedFlockMetric(metric.observation(FlockPlacementOutcomeCrossHostRollbackError, reason, rollbackLatency))
		return sanitizedRoutedFlockCreateError(record.FlockID, reason, true)
	}
	r.recordRoutedFlockMetric(metric.observation(FlockPlacementOutcomeCrossHostRollbackError, reason, rollbackLatency))
	return sanitizedRoutedFlockCreateError(record.FlockID, reason, true)
}

func (m routedFlockCreateFailureMetric) observation(outcome, reason string, rollbackLatency time.Duration) FlockPlacementMetricObservation {
	if outcome == "" {
		outcome = FlockPlacementOutcomeCrossHostSpawnError
	}
	if reason == "" {
		reason = FlockPlacementReasonUnknown
	}
	latencies := map[string]time.Duration{
		FlockPlacementPhasePlan:         m.PlanLatency,
		FlockPlacementPhaseAgentSpawn:   m.AgentSpawnLatency,
		FlockPlacementPhaseRegistrySave: m.RegistrySaveLatency,
		FlockPlacementPhaseRollback:     rollbackLatency,
	}
	if !m.TotalStart.IsZero() {
		latencies[FlockPlacementPhaseTotal] = time.Since(m.TotalStart)
	}
	return FlockPlacementMetricObservation{
		Outcome:   outcome,
		Reason:    reason,
		Latencies: latencies,
	}
}

func sanitizedRoutedFlockCreateError(flockID, reason string, cleanupPending bool) error {
	reason = sanitizeRoutedFlockErrorReason(reason)
	if cleanupPending {
		return fmt.Errorf("routed flock create cleanup pending: flock_id=%q reason=%s", strings.TrimSpace(flockID), reason)
	}
	return fmt.Errorf("routed flock create failed: flock_id=%q reason=%s", strings.TrimSpace(flockID), reason)
}

func sanitizeRoutedFlockErrorReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case FlockPlacementReasonScheduled:
		return FlockPlacementReasonScheduled
	case FlockPlacementReasonQuotaExceeded:
		return FlockPlacementReasonQuotaExceeded
	case FlockPlacementReasonNoEligibleHost:
		return FlockPlacementReasonNoEligibleHost
	case FlockPlacementReasonInvalidRequest:
		return FlockPlacementReasonInvalidRequest
	case FlockPlacementReasonDaemonCreateFailed:
		return FlockPlacementReasonDaemonCreateFailed
	case FlockPlacementReasonDaemonNilResponse:
		return FlockPlacementReasonDaemonNilResponse
	case FlockPlacementReasonDaemonInvalidResponse:
		return FlockPlacementReasonDaemonInvalidResponse
	case FlockPlacementReasonPlacementSaveFailed:
		return FlockPlacementReasonPlacementSaveFailed
	case routedFlockReasonCleanupFailed:
		return routedFlockReasonCleanupFailed
	default:
		return FlockPlacementReasonUnknown
	}
}

func (r *RuntimeRouter) DeleteRoutedFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error) {
	if r == nil {
		return nil, fmt.Errorf("runtime router is nil")
	}
	if r.placementStore == nil {
		return nil, fmt.Errorf("routed flock %q not found", strings.TrimSpace(flockID))
	}
	record, ok := r.placementStore.RoutedFlock(flockID)
	if !ok {
		return nil, fmt.Errorf("routed flock %q not found", strings.TrimSpace(flockID))
	}

	record.Status = RoutedFlockStatusDeleting
	record.UpdatedAt = time.Now().UTC()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		return nil, fmt.Errorf("routed flock delete failed: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonPlacementSaveFailed))
	}

	// Best-effort deregistration of the shared Town Wall (hub on home, relay on
	// each member) and removal of the persisted relay token, before VM teardown.
	r.deregisterRoutedFlockWall(ctx, record)

	removeVMIDs, pendingAgents := r.deleteRoutedFlockAgents(ctx, record.Agents)
	r.removeRoutedFlockPlacements(removeVMIDs)
	if len(pendingAgents) > 0 {
		record.Status = RoutedFlockStatusFailedCleanupPending
		record.Agents = pendingAgents
		record.UpdatedAt = time.Now().UTC()
		if err := r.placementStore.SaveRoutedFlockAndPlacements(record, removeVMIDs); err != nil {
			preserveRoutedFlockCleanupStateInMemory(r.placementStore, record, removeVMIDs)
			return nil, fmt.Errorf("routed flock delete cleanup pending: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonPlacementSaveFailed))
		}
		return nil, fmt.Errorf("routed flock delete cleanup pending: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(routedFlockReasonCleanupFailed))
	}

	record.Status = RoutedFlockStatusDeleted
	record.Agents = []RoutedFlockAgent{}
	record.UpdatedAt = time.Now().UTC()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, removeVMIDs); err != nil {
		record.Status = RoutedFlockStatusFailedCleanupPending
		record.UpdatedAt = time.Now().UTC()
		preserveRoutedFlockCleanupStateInMemory(r.placementStore, record, removeVMIDs)
		return nil, fmt.Errorf("routed flock delete cleanup pending: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonPlacementSaveFailed))
	}
	body, err := json.Marshal(map[string]string{
		"status":   "deleted",
		"flock_id": record.FlockID,
	})
	if err != nil {
		return nil, fmt.Errorf("routed flock delete failed: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonUnknown))
	}
	return &RawDaemonResponse{StatusCode: 200, Body: string(body)}, nil
}

func (r *RuntimeRouter) deleteRoutedFlockAgents(ctx context.Context, agents []RoutedFlockAgent) ([]string, []RoutedFlockAgent) {
	if r == nil {
		pending := make([]RoutedFlockAgent, 0, len(agents))
		for _, agent := range agents {
			pending = append(pending, cleanupPendingRoutedFlockAgent(agent))
		}
		return nil, pending
	}
	removeVMIDs := make([]string, 0, len(agents))
	pendingAgents := make([]RoutedFlockAgent, 0)
	for _, agent := range agents {
		vmID := strings.TrimSpace(agent.VMID)
		hostName := strings.TrimSpace(agent.Host)
		if vmID == "" || hostName == "" {
			pendingAgents = append(pendingAgents, cleanupPendingRoutedFlockAgent(agent))
			continue
		}
		daemon, ok := r.daemons[hostName]
		if !ok || daemon == nil {
			pendingAgents = append(pendingAgents, cleanupPendingRoutedFlockAgent(agent))
			continue
		}
		resp, err := daemon.Delete(ctx, vmID)
		switch {
		case err != nil && !routedFlockVMAlreadyGone(err):
			// A genuine teardown failure (unreachable daemon, 5xx, auth, etc.):
			// the VM may still be standing, so keep it pending for retry.
			pendingAgents = append(pendingAgents, cleanupPendingRoutedFlockAgent(agent))
			continue
		case err == nil && resp == nil:
			// Ambiguous success with no response body: treat conservatively as
			// pending. The real daemon client never returns (nil, nil).
			pendingAgents = append(pendingAgents, cleanupPendingRoutedFlockAgent(agent))
			continue
		}
		// The delete succeeded, or the VM was already absent (404) — both mean the
		// VM is torn down, so record it for placement removal instead of misreporting
		// the already-reached end-state as a pending cleanup (defect D2). Deregistering
		// the shared wall (DeleteFlock) cascades this teardown on the daemon, which is
		// why every routed delete otherwise hit an already-gone VM and 404ed.
		removeVMIDs = append(removeVMIDs, vmID)
	}
	return removeVMIDs, pendingAgents
}

func preserveRoutedFlockCleanupStateInMemory(store *PlacementStore, record RoutedFlockRecord, removeVMIDs []string) {
	if store == nil {
		return
	}
	record = normalizeRoutedFlockRecord(record)
	store.mu.Lock()
	defer store.mu.Unlock()
	store.ensureMaps()
	applyRoutedFlockAndPlacements(&store.state, record, removeVMIDs)
}

func cleanupPendingRoutedFlockAgent(agent RoutedFlockAgent) RoutedFlockAgent {
	agent.Status = routedFlockAgentStatusCleanupPending
	return agent
}

// routedFlockVMAlreadyGone reports whether a daemon VM-delete error means the VM
// is already absent (HTTP 404). DELETE /vms/{id} that 404s is the success
// end-state of teardown — commonly because deregistering the shared wall
// (DeleteFlock) already cascaded the VM teardown on the daemon — so it must be
// treated as a completed teardown, not a pending cleanup (defect D2). Any other
// error (unreachable daemon, 5xx, auth) is a genuine, retryable failure.
func routedFlockVMAlreadyGone(err error) bool {
	var daemonErr *DaemonError
	if errors.As(err, &daemonErr) {
		return daemonErr.StatusCode == http.StatusNotFound
	}
	return false
}

func (r *RuntimeRouter) removeRoutedFlockPlacements(vmIDs []string) {
	if r == nil || len(vmIDs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, vmID := range vmIDs {
		delete(r.placement, strings.TrimSpace(vmID))
	}
}

func (r *RuntimeRouter) IsRoutedFlock(flockID string) bool {
	_, ok := r.GetRoutedFlock(flockID)
	return ok
}

func (r *RuntimeRouter) GetRoutedFlock(flockID string) (RoutedFlockRecord, bool) {
	if r == nil || r.placementStore == nil {
		return RoutedFlockRecord{}, false
	}
	return r.placementStore.RoutedFlock(flockID)
}

func (r *RuntimeRouter) ListRoutedFlocks() []RoutedFlockRecord {
	if r == nil || r.placementStore == nil {
		return []RoutedFlockRecord{}
	}
	return r.placementStore.ListRoutedFlocks()
}

// PostRoutedTownWall posts to a routed flock's shared Town Wall. Routed members
// relay to the wall hub owned by the home daemon (Tasks 3-5), so this helper
// proxies the post to the home host's daemon rather than hard-erroring. The relay
// token is never referenced here: the home daemon authenticates the runtime's own
// control-plane session, and the per-flock relay secret stays server-side.
func (r *RuntimeRouter) PostRoutedTownWall(ctx context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error) {
	record, ok := r.GetRoutedFlock(flockID)
	if !ok {
		return nil, fmt.Errorf("routed flock %q not found", strings.TrimSpace(flockID))
	}
	homeDaemon, err := r.homeDaemonForRoutedFlock(record)
	if err != nil {
		return nil, err
	}
	return homeDaemon.PostTownWall(ctx, strings.TrimSpace(flockID), req)
}

// RoutedTownWallHistory reads a routed flock's shared Town Wall history from the
// home daemon that owns the hub (see PostRoutedTownWall).
func (r *RuntimeRouter) RoutedTownWallHistory(ctx context.Context, flockID string) ([]TownWallMessage, error) {
	record, ok := r.GetRoutedFlock(flockID)
	if !ok {
		return nil, fmt.Errorf("routed flock %q not found", strings.TrimSpace(flockID))
	}
	homeDaemon, err := r.homeDaemonForRoutedFlock(record)
	if err != nil {
		return nil, err
	}
	return homeDaemon.TownWallHistory(ctx, strings.TrimSpace(flockID))
}

// homeDaemonForRoutedFlock resolves the daemon of the flock's home host, which
// owns the shared Town Wall hub. It returns a sanitized error (host name only, no
// secrets) when the record carries no home host or the host has no daemon client.
func (r *RuntimeRouter) homeDaemonForRoutedFlock(record RoutedFlockRecord) (Daemon, error) {
	homeHost := strings.TrimSpace(record.HomeHost)
	if homeHost == "" {
		return nil, fmt.Errorf("routed flock %q has no home host", strings.TrimSpace(record.FlockID))
	}
	daemon, ok := r.daemons[homeHost]
	if !ok || daemon == nil {
		return nil, fmt.Errorf("routed flock %q home host %q has no daemon client", strings.TrimSpace(record.FlockID), homeHost)
	}
	return daemon, nil
}
