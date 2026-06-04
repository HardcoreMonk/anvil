package anvilmcp

import "strconv"

type FlockPlacementPlanRequest struct {
	TenantID     string       `json:"tenant_id"`
	EgressPolicy EgressPolicy `json:"egress_policy"`
	Roles        []string     `json:"roles"`
}

type FlockPlacementPlan struct {
	Allowed           bool                  `json:"allowed"`
	Reason            string                `json:"reason"`
	TenantID          string                `json:"tenant_id"`
	EgressPolicy      EgressPolicy          `json:"egress_policy"`
	Agents            []FlockAgentPlacement `json:"agents"`
	HostStatusSummary HostStatusSummary     `json:"host_status_summary"`
	Quota             QuotaDecision         `json:"quota,omitempty"`
	Requested         TenantUsage           `json:"requested"`
	CurrentUsage      TenantUsage           `json:"current_usage"`
	Limit             TenantQuota           `json:"limit"`
}

type FlockAgentPlacement struct {
	AgentID string      `json:"agent_id"`
	Role    string      `json:"role"`
	Host    RuntimeHost `json:"host"`
}

func (s *Scheduler) ScheduleFlock(req FlockPlacementPlanRequest) (FlockPlacementPlan, error) {
	tenantID, err := NormalizeTenantID(req.TenantID)
	if err != nil {
		return FlockPlacementPlan{}, err
	}
	egressPolicy, err := NormalizeEgressPolicy(string(req.EgressPolicy))
	if err != nil {
		return FlockPlacementPlan{}, err
	}
	roles, err := normalizeFlockRoles(req.Roles)
	if err != nil {
		return FlockPlacementPlan{}, err
	}

	var hosts []RuntimeHost
	var limit TenantQuota
	var current TenantUsage
	if s != nil {
		hosts = make([]RuntimeHost, 0, len(s.hosts))
		for _, host := range s.hosts {
			hosts = append(hosts, cloneRuntimeHost(host))
		}
		limit = s.quotas[tenantID]
		current = s.usage[tenantID]
	}
	requested := TenantUsage{ActiveVMs: int64(len(roles))}
	quotaDecision, err := CheckTenantQuota(limit, current, requested)
	if err != nil {
		return FlockPlacementPlan{}, err
	}

	base := FlockPlacementPlan{
		TenantID:          tenantID,
		EgressPolicy:      egressPolicy,
		Agents:            []FlockAgentPlacement{},
		HostStatusSummary: SummarizeHostStatuses(hosts, nil),
		Requested:         requested,
		CurrentUsage:      current,
		Limit:             limit,
		Quota:             quotaDecision,
	}
	if !quotaDecision.Allowed {
		base.Allowed = false
		base.Reason = FlockPlacementReasonQuotaExceeded
		return base, nil
	}

	roleCounts := make(map[string]int, len(roles))
	agents := make([]FlockAgentPlacement, 0, len(roles))
	for _, role := range roles {
		scheduleReq := ScheduleRequest{
			TenantID:           tenantID,
			RequestedActiveVMs: 1,
			EgressPolicy:       egressPolicy,
		}
		host, err := SelectRuntimeHost(hosts, scheduleReq)
		if err != nil {
			base.Allowed = false
			base.Reason = FlockPlacementReasonNoEligibleHost
			return base, nil
		}

		roleCounts[role]++
		agents = append(agents, FlockAgentPlacement{
			AgentID: role + "-" + strconv.Itoa(roleCounts[role]),
			Role:    role,
			Host:    host,
		})
		reserveFlockPlanHostCapacity(hosts, host.Name)
	}

	base.Allowed = true
	base.Reason = FlockPlacementReasonScheduled
	base.Agents = agents
	return base, nil
}

func reserveFlockPlanHostCapacity(hosts []RuntimeHost, hostName string) {
	for i := range hosts {
		if hosts[i].Name == hostName {
			hosts[i].AvailableVMs--
			return
		}
	}
}
