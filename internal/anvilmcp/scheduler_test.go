package anvilmcp

import (
	"encoding/json"
	"testing"
)

func TestSchedulerRejectsQuotaBeforeHostSelection(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{
				Name:                   "host-a",
				Endpoint:               "http://host-a:3000",
				Healthy:                true,
				AvailableVMs:           10,
				AvailableSnapshotBytes: 1 << 30,
				EgressPolicies:         []EgressPolicy{EgressPolicyProfile},
			},
		},
		map[string]TenantQuota{
			"tenant-1": {ActiveVMs: 1},
		},
		map[string]TenantUsage{
			"tenant-1": {ActiveVMs: 1},
		},
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
	}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if decision.Allowed {
		t.Fatal("Schedule() allowed = true, want quota denial")
	}
	if decision.Reason != "quota_exceeded" || decision.Quota.Resource != "active_vms" {
		t.Fatalf("decision = %+v, want active_vms quota_exceeded", decision)
	}
	if decision.Host.Name != "" {
		t.Fatalf("denied decision host = %+v, want empty host", decision.Host)
	}
}

func TestSchedulerSelectsEligibleHost(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{
				Name:                   "host-a",
				Endpoint:               "http://host-a:3000",
				Healthy:                false,
				AvailableVMs:           10,
				AvailableSnapshotBytes: 1 << 30,
				EgressPolicies:         []EgressPolicy{EgressPolicyAllowAll},
			},
			{
				Name:                   "host-b",
				Endpoint:               "http://host-b:3000",
				Healthy:                true,
				AvailableVMs:           1,
				AvailableSnapshotBytes: 4096,
				EgressPolicies:         []EgressPolicy{EgressPolicyProfile},
			},
		},
		map[string]TenantQuota{
			"tenant-1": {ActiveVMs: 2, SnapshotBytes: 8192},
		},
		map[string]TenantUsage{
			"tenant-1": {ActiveVMs: 0, SnapshotBytes: 1024},
		},
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:               "tenant-1",
		RequestedSnapshotBytes: 2048,
		EgressPolicy:           EgressPolicyProfile,
	}, TenantUsage{ActiveVMs: 1, SnapshotBytes: 2048})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Schedule() allowed = false, decision = %+v", decision)
	}
	if decision.Host.Name != "host-b" {
		t.Fatalf("selected host = %q, want host-b", decision.Host.Name)
	}
	if decision.EgressPolicy != EgressPolicyProfile {
		t.Fatalf("egress policy = %q, want profile", decision.EgressPolicy)
	}
}

func TestSchedulerRejectsUnsupportedEgress(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{
				Name:                   "host-a",
				Endpoint:               "http://host-a:3000",
				Healthy:                true,
				AvailableVMs:           1,
				AvailableSnapshotBytes: 1 << 20,
				EgressPolicies:         []EgressPolicy{EgressPolicyDenyAll},
			},
		},
		nil,
		nil,
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:               "tenant-1",
		RequestedSnapshotBytes: 1024,
		EgressPolicy:           EgressPolicyAllowAll,
	}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if decision.Allowed {
		t.Fatal("Schedule() allowed = true, want no_host denial")
	}
	if decision.Reason != "no_eligible_host" {
		t.Fatalf("decision reason = %q, want no_eligible_host", decision.Reason)
	}
}

func TestSchedulerPrefersSnapshotLocalityHost(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:       "tenant-1",
		EgressPolicy:   EgressPolicyProfile,
		PreferredHosts: []string{"host-b"},
	}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Schedule() denied: %+v", decision)
	}
	if decision.Host.Name != "host-b" {
		t.Fatalf("selected host = %q, want preferred host-b", decision.Host.Name)
	}
}

func TestSchedulerIgnoresSmokeOnlyHostsForFallback(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			runtimeHostFromJSON(t, `{"name":"smoke-host","endpoint":"http://smoke-host","healthy":true,"available_vms":1,"available_snapshot_bytes":1048576,"egress_policies":["profile"],"smoke_only":true}`),
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
	}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Schedule() denied: %+v", decision)
	}
	if decision.Host.Name != "host-a" {
		t.Fatalf("selected host = %q, want non-smoke host-a", decision.Host.Name)
	}
}

func TestSchedulerSelectsSmokeOnlyHostWhenExplicitlyPreferred(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			runtimeHostFromJSON(t, `{"name":"smoke-host","endpoint":"http://smoke-host","healthy":true,"available_vms":1,"available_snapshot_bytes":1048576,"egress_policies":["profile"],"smoke_only":true}`),
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:       "tenant-1",
		EgressPolicy:   EgressPolicyProfile,
		PreferredHosts: []string{"smoke-host"},
	}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Schedule() denied: %+v", decision)
	}
	if decision.Host.Name != "smoke-host" {
		t.Fatalf("selected host = %q, want explicitly preferred smoke-host", decision.Host.Name)
	}
}

func TestSchedulerSkipsExcludedHostsForFailover(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:      "tenant-1",
		EgressPolicy:  EgressPolicyProfile,
		ExcludedHosts: []string{"host-a"},
	}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if !decision.Allowed || decision.Host.Name != "host-b" {
		t.Fatalf("decision = %+v, want host-b after excluding host-a", decision)
	}
}

func TestSchedulerRequiresEnoughAvailableVMsForRequestedActiveVMs(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 3, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
	}, TenantUsage{ActiveVMs: 2})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Schedule() denied: %+v", decision)
	}
	if decision.Host.Name != "host-b" {
		t.Fatalf("selected host = %q, want host-b because host-a has only 1 available VM", decision.Host.Name)
	}
}

func runtimeHostFromJSON(t *testing.T, body string) RuntimeHost {
	t.Helper()
	var host RuntimeHost
	if err := json.Unmarshal([]byte(body), &host); err != nil {
		t.Fatalf("decode runtime host JSON: %v", err)
	}
	return host
}
