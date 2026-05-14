package anvilmcp

type Scheduler struct {
	hosts  []RuntimeHost
	quotas map[string]TenantQuota
	usage  map[string]TenantUsage
}

type ScheduleDecision struct {
	Allowed      bool          `json:"allowed"`
	Reason       string        `json:"reason"`
	TenantID     string        `json:"tenant_id"`
	Host         RuntimeHost   `json:"host,omitempty"`
	Quota        QuotaDecision `json:"quota,omitempty"`
	EgressPolicy EgressPolicy  `json:"egress_policy"`
	Requested    TenantUsage   `json:"requested"`
	CurrentUsage TenantUsage   `json:"current_usage"`
	Limit        TenantQuota   `json:"limit"`
}

func NewScheduler(hosts []RuntimeHost, quotas map[string]TenantQuota, usage map[string]TenantUsage) *Scheduler {
	hostCopy := append([]RuntimeHost(nil), hosts...)
	quotaCopy := make(map[string]TenantQuota, len(quotas))
	for tenantID, quota := range quotas {
		quotaCopy[tenantID] = quota
	}
	usageCopy := make(map[string]TenantUsage, len(usage))
	for tenantID, current := range usage {
		usageCopy[tenantID] = current
	}
	return &Scheduler{
		hosts:  hostCopy,
		quotas: quotaCopy,
		usage:  usageCopy,
	}
}

func (s *Scheduler) Schedule(req ScheduleRequest, requested TenantUsage) (ScheduleDecision, error) {
	tenantID, err := NormalizeTenantID(req.TenantID)
	if err != nil {
		return ScheduleDecision{}, err
	}
	egressPolicy, err := NormalizeEgressPolicy(string(req.EgressPolicy))
	if err != nil {
		return ScheduleDecision{}, err
	}
	if requested.SnapshotBytes == 0 && req.RequestedSnapshotBytes > 0 {
		requested.SnapshotBytes = req.RequestedSnapshotBytes
	}

	limit := s.quotas[tenantID]
	current := s.usage[tenantID]
	quotaDecision, err := CheckTenantQuota(limit, current, requested)
	if err != nil {
		return ScheduleDecision{}, err
	}
	base := ScheduleDecision{
		TenantID:     tenantID,
		EgressPolicy: egressPolicy,
		Quota:        quotaDecision,
		Requested:    requested,
		CurrentUsage: current,
		Limit:        limit,
	}
	if !quotaDecision.Allowed {
		base.Allowed = false
		base.Reason = quotaDecision.Reason
		return base, nil
	}

	req.TenantID = tenantID
	req.EgressPolicy = egressPolicy
	host, err := SelectRuntimeHost(s.hosts, req)
	if err != nil {
		base.Allowed = false
		base.Reason = "no_eligible_host"
		return base, nil
	}
	base.Allowed = true
	base.Reason = "scheduled"
	base.Host = host
	return base, nil
}
