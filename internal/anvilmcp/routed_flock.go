package anvilmcp

import (
	"context"
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
			err := fmt.Errorf("runtime host %q has no daemon client", hostName)
			r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
				Outcome: FlockPlacementOutcomeCrossHostSpawnError,
				Reason:  FlockPlacementReasonDaemonCreateFailed,
				Latencies: map[string]time.Duration{
					FlockPlacementPhasePlan:         planLatency,
					FlockPlacementPhaseAgentSpawn:   agentSpawnLatency,
					FlockPlacementPhaseRegistrySave: registrySaveLatency,
					FlockPlacementPhaseTotal:        time.Since(totalStart),
				},
			})
			return nil, rollbackRoutedFlockCreate(ctx, r, record, err)
		}

		spawnStart := time.Now()
		resp, err := daemon.SpawnVM(ctx, SpawnVMRequest{
			Profile:      planned.Role,
			TenantID:     plan.TenantID,
			EgressPolicy: string(plan.EgressPolicy),
		})
		agentSpawnLatency += time.Since(spawnStart)
		if err != nil {
			r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
				Outcome: FlockPlacementOutcomeCrossHostSpawnError,
				Reason:  FlockPlacementReasonDaemonCreateFailed,
				Latencies: map[string]time.Duration{
					FlockPlacementPhasePlan:         planLatency,
					FlockPlacementPhaseAgentSpawn:   agentSpawnLatency,
					FlockPlacementPhaseRegistrySave: registrySaveLatency,
					FlockPlacementPhaseTotal:        time.Since(totalStart),
				},
			})
			return nil, rollbackRoutedFlockCreate(ctx, r, record, err)
		}
		if resp == nil {
			err := fmt.Errorf("runtime daemon SpawnVM returned nil response for routed flock agent %q", planned.AgentID)
			r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
				Outcome: FlockPlacementOutcomeCrossHostSpawnError,
				Reason:  FlockPlacementReasonDaemonNilResponse,
				Latencies: map[string]time.Duration{
					FlockPlacementPhasePlan:         planLatency,
					FlockPlacementPhaseAgentSpawn:   agentSpawnLatency,
					FlockPlacementPhaseRegistrySave: registrySaveLatency,
					FlockPlacementPhaseTotal:        time.Since(totalStart),
				},
			})
			return nil, rollbackRoutedFlockCreate(ctx, r, record, err)
		}

		record.Agents = append(record.Agents, RoutedFlockAgent{
			AgentID:  planned.AgentID,
			Role:     planned.Role,
			VMID:     strings.TrimSpace(resp.VMID),
			AgentURL: strings.TrimSpace(resp.AgentURL),
			Host:     hostName,
			Status:   "running",
		})
		record.UpdatedAt = time.Now().UTC()

		registrySaveStart = time.Now()
		if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
			registrySaveLatency += time.Since(registrySaveStart)
			r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
				Outcome: FlockPlacementOutcomeCrossHostRegistryError,
				Reason:  FlockPlacementReasonPlacementSaveFailed,
				Latencies: map[string]time.Duration{
					FlockPlacementPhasePlan:         planLatency,
					FlockPlacementPhaseAgentSpawn:   agentSpawnLatency,
					FlockPlacementPhaseRegistrySave: registrySaveLatency,
					FlockPlacementPhaseTotal:        time.Since(totalStart),
				},
			})
			return nil, rollbackRoutedFlockCreate(ctx, r, record, err)
		}
		registrySaveLatency += time.Since(registrySaveStart)
		r.recordRoutedFlockAgentPlacement(record.Agents[len(record.Agents)-1])
	}

	record.Status = RoutedFlockStatusReady
	record.UpdatedAt = time.Now().UTC()
	registrySaveStart = time.Now()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		registrySaveLatency += time.Since(registrySaveStart)
		r.recordRoutedFlockMetric(FlockPlacementMetricObservation{
			Outcome: FlockPlacementOutcomeCrossHostRegistryError,
			Reason:  FlockPlacementReasonPlacementSaveFailed,
			Latencies: map[string]time.Duration{
				FlockPlacementPhasePlan:         planLatency,
				FlockPlacementPhaseAgentSpawn:   agentSpawnLatency,
				FlockPlacementPhaseRegistrySave: registrySaveLatency,
				FlockPlacementPhaseTotal:        time.Since(totalStart),
			},
		})
		return nil, rollbackRoutedFlockCreate(ctx, r, record, err)
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

func rollbackRoutedFlockCreate(_ context.Context, _ *RuntimeRouter, _ RoutedFlockRecord, cause error) error {
	return cause
}
