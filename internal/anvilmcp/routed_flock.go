package anvilmcp

import (
	"context"
	"encoding/json"
	"fmt"
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
	routedTownWallUnsupportedMessage     = "Town Wall is not supported for routed members-only flock"
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

	var agentSpawnLatency time.Duration
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
			})
		}

		spawnStart := time.Now()
		resp, err := daemon.SpawnVM(ctx, SpawnVMRequest{
			Profile:      planned.Role,
			TenantID:     plan.TenantID,
			EgressPolicy: string(plan.EgressPolicy),
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
			})
		}
		if resp == nil {
			return nil, rollbackRoutedFlockCreate(ctx, r, record, routedFlockCreateFailureMetric{
				Outcome:             FlockPlacementOutcomeCrossHostSpawnError,
				Reason:              FlockPlacementReasonDaemonNilResponse,
				PlanLatency:         planLatency,
				AgentSpawnLatency:   agentSpawnLatency,
				RegistrySaveLatency: registrySaveLatency,
				TotalStart:          totalStart,
			})
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
			})
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
			})
		}
		registrySaveLatency += time.Since(registrySaveStart)
		r.recordRoutedFlockAgentPlacement(record.Agents[len(record.Agents)-1])
	}

	record.Status = RoutedFlockStatusReady
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
		})
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
		TownWallEnabled: false,
		Agents:          append([]RoutedFlockAgent(nil), record.Agents...),
	}
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

func rollbackRoutedFlockCreate(ctx context.Context, r *RuntimeRouter, record RoutedFlockRecord, metric routedFlockCreateFailureMetric) error {
	reason := sanitizeRoutedFlockErrorReason(metric.Reason)
	rollbackStart := time.Now()
	removeVMIDs, pendingAgents := r.deleteRoutedFlockAgents(ctx, record.Agents)
	rollbackLatency := time.Since(rollbackStart)
	r.removeRoutedFlockPlacements(removeVMIDs)

	if len(pendingAgents) == 0 {
		record.Status = RoutedFlockStatusDeleted
		record.Agents = []RoutedFlockAgent{}
		record.UpdatedAt = time.Now().UTC()
		if err := r.placementStore.SaveRoutedFlockAndPlacements(record, removeVMIDs); err != nil {
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

	removeVMIDs, pendingAgents := r.deleteRoutedFlockAgents(ctx, record.Agents)
	r.removeRoutedFlockPlacements(removeVMIDs)
	if len(pendingAgents) > 0 {
		record.Status = RoutedFlockStatusFailedCleanupPending
		record.Agents = pendingAgents
		record.UpdatedAt = time.Now().UTC()
		if err := r.placementStore.SaveRoutedFlockAndPlacements(record, removeVMIDs); err != nil {
			return nil, fmt.Errorf("routed flock delete cleanup pending: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonPlacementSaveFailed))
		}
		return nil, fmt.Errorf("routed flock delete cleanup pending: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(routedFlockReasonCleanupFailed))
	}

	record.Status = RoutedFlockStatusDeleted
	record.Agents = []RoutedFlockAgent{}
	record.UpdatedAt = time.Now().UTC()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, removeVMIDs); err != nil {
		return nil, fmt.Errorf("routed flock delete failed: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(FlockPlacementReasonPlacementSaveFailed))
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
		if _, err := daemon.Delete(ctx, vmID); err != nil {
			pendingAgents = append(pendingAgents, cleanupPendingRoutedFlockAgent(agent))
			continue
		}
		removeVMIDs = append(removeVMIDs, vmID)
	}
	return removeVMIDs, pendingAgents
}

func cleanupPendingRoutedFlockAgent(agent RoutedFlockAgent) RoutedFlockAgent {
	agent.Status = routedFlockAgentStatusCleanupPending
	return agent
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

func (r *RuntimeRouter) PostRoutedTownWall(_ context.Context, flockID string, _ TownWallPostRequest) (*TownWallMessage, error) {
	if !r.IsRoutedFlock(flockID) {
		return nil, fmt.Errorf("routed flock %q not found", strings.TrimSpace(flockID))
	}
	return nil, fmt.Errorf("%s: flock_id=%q", routedTownWallUnsupportedMessage, strings.TrimSpace(flockID))
}

func (r *RuntimeRouter) RoutedTownWallHistory(_ context.Context, flockID string) ([]TownWallMessage, error) {
	if !r.IsRoutedFlock(flockID) {
		return nil, fmt.Errorf("routed flock %q not found", strings.TrimSpace(flockID))
	}
	return nil, fmt.Errorf("%s: flock_id=%q", routedTownWallUnsupportedMessage, strings.TrimSpace(flockID))
}
