package anvilmcp

import (
	"encoding/json"
	"testing"
)

func TestSchedulerScheduleFlockDistributesAcrossHosts(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		map[string]TenantQuota{"tenant-1": {ActiveVMs: 3}},
		map[string]TenantUsage{"tenant-1": {ActiveVMs: 0}},
	)

	plan, err := scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     " tenant-1 ",
		EgressPolicy: EgressPolicyProfile,
		Roles:        []string{"worker", "reviewer"},
	})
	if err != nil {
		t.Fatalf("ScheduleFlock() error = %v", err)
	}
	if !plan.Allowed {
		t.Fatalf("ScheduleFlock() denied: %+v", plan)
	}
	if plan.TenantID != "tenant-1" || plan.EgressPolicy != EgressPolicyProfile {
		t.Fatalf("normalized plan tenant/egress = %q/%q", plan.TenantID, plan.EgressPolicy)
	}
	if plan.Requested.ActiveVMs != 2 {
		t.Fatalf("requested active VMs = %d, want 2", plan.Requested.ActiveVMs)
	}
	if plan.HostStatusSummary.Healthy != 2 {
		t.Fatalf("host status summary = %+v, want two healthy hosts", plan.HostStatusSummary)
	}
	if len(plan.Agents) != 2 {
		t.Fatalf("agent placements = %+v, want two agents", plan.Agents)
	}
	if plan.Agents[0].AgentID != "worker-1" || plan.Agents[0].Role != "worker" || plan.Agents[0].Host.Name != "host-a" {
		t.Fatalf("first agent = %+v, want worker-1 on host-a", plan.Agents[0])
	}
	if plan.Agents[1].AgentID != "reviewer-1" || plan.Agents[1].Role != "reviewer" || plan.Agents[1].Host.Name != "host-b" {
		t.Fatalf("second agent = %+v, want reviewer-1 on host-b", plan.Agents[1])
	}
}

func TestSchedulerScheduleFlockPacksWhenSingleHostHasCapacity(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	plan, err := scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
		Roles:        []string{"worker", "reviewer"},
	})
	if err != nil {
		t.Fatalf("ScheduleFlock() error = %v", err)
	}
	if !plan.Allowed || len(plan.Agents) != 2 {
		t.Fatalf("plan = %+v, want allowed two-agent plan", plan)
	}
	if plan.Agents[0].Host.Name != "host-a" || plan.Agents[1].Host.Name != "host-a" {
		t.Fatalf("agent hosts = %q/%q, want deterministic packing on host-a", plan.Agents[0].Host.Name, plan.Agents[1].Host.Name)
	}
}

func TestSchedulerScheduleFlockRejectsQuotaBeforePlacement(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 10, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}},
		map[string]TenantUsage{"tenant-1": {ActiveVMs: 1}},
	)

	plan, err := scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
		Roles:        []string{"worker"},
	})
	if err != nil {
		t.Fatalf("ScheduleFlock() error = %v", err)
	}
	if plan.Allowed {
		t.Fatalf("plan allowed = true, want quota denial: %+v", plan)
	}
	if plan.Reason != FlockPlacementReasonQuotaExceeded {
		t.Fatalf("plan reason = %q, want quota_exceeded", plan.Reason)
	}
	if len(plan.Agents) != 0 {
		t.Fatalf("denied plan agents = %+v, want none", plan.Agents)
	}
	if plan.Quota.Resource != "active_vms" {
		t.Fatalf("quota decision = %+v, want active_vms resource", plan.Quota)
	}
	assertPlanJSONAgentsEmptyArray(t, plan)
}

func TestSchedulerScheduleFlockRejectsInvalidRoles(t *testing.T) {
	scheduler := NewScheduler(nil, nil, nil)

	_, err := scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
		Roles:        []string{"bad/role"},
	})
	if err == nil {
		t.Fatal("ScheduleFlock() error = nil, want invalid role error")
	}
}

func TestSchedulerScheduleFlockFiltersByEgressPolicy(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyDenyAll}},
			{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyAllowAll}},
		},
		nil,
		nil,
	)

	plan, err := scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyAllowAll,
		Roles:        []string{"worker"},
	})
	if err != nil {
		t.Fatalf("ScheduleFlock() error = %v", err)
	}
	if !plan.Allowed || len(plan.Agents) != 1 || plan.Agents[0].Host.Name != "host-b" {
		t.Fatalf("plan = %+v, want worker on host-b", plan)
	}
}

func TestSchedulerScheduleFlockSequencesRepeatedRoles(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 3, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	plan, err := scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
		Roles:        []string{"worker", "worker", "reviewer"},
	})
	if err != nil {
		t.Fatalf("ScheduleFlock() error = %v", err)
	}
	if !plan.Allowed || len(plan.Agents) != 3 {
		t.Fatalf("plan = %+v, want allowed three-agent plan", plan)
	}
	for i, want := range []string{"worker-1", "worker-2", "reviewer-1"} {
		if plan.Agents[i].AgentID != want {
			t.Fatalf("agent %d id = %q, want %q", i, plan.Agents[i].AgentID, want)
		}
	}
}

func TestSchedulerScheduleFlockDeniesWhenPlanReservationExhaustsCapacity(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	plan, err := scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
		Roles:        []string{"worker", "reviewer"},
	})
	if err != nil {
		t.Fatalf("ScheduleFlock() error = %v", err)
	}
	if plan.Allowed {
		t.Fatalf("plan allowed = true, want no eligible host after reservation: %+v", plan)
	}
	if plan.Reason != FlockPlacementReasonNoEligibleHost {
		t.Fatalf("plan reason = %q, want no_eligible_host", plan.Reason)
	}
	if len(plan.Agents) != 0 {
		t.Fatalf("denied plan agents = %+v, want none", plan.Agents)
	}
	assertPlanJSONAgentsEmptyArray(t, plan)
}

func assertPlanJSONAgentsEmptyArray(t *testing.T, plan FlockPlacementPlan) {
	t.Helper()

	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("Marshal(FlockPlacementPlan) error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("Unmarshal(FlockPlacementPlan JSON) error = %v", err)
	}
	rawAgents, ok := fields["agents"]
	if !ok {
		t.Fatalf("plan JSON = %s, want agents field", payload)
	}
	var agents []FlockAgentPlacement
	if err := json.Unmarshal(rawAgents, &agents); err != nil {
		t.Fatalf("Unmarshal agents JSON %s error = %v", rawAgents, err)
	}
	if len(agents) != 0 {
		t.Fatalf("agents JSON = %s, want empty array", rawAgents)
	}
}
