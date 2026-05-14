package anvilmcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHostInventoryPollMarksHealthyHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("path = %s, want /health", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","available_vms":3,"available_snapshot_bytes":4096,"egress_policies":["profile","deny_all"]}`))
	}))
	defer server.Close()

	inventory := NewHostInventory([]RuntimeHost{
		{Name: "host-a", Endpoint: server.URL, AvailableVMs: 1},
	}, HostInventoryOptions{HTTPClient: server.Client()})

	if err := inventory.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce() error = %v", err)
	}

	hosts := inventory.RuntimeHosts()
	if len(hosts) != 1 {
		t.Fatalf("hosts len = %d, want 1", len(hosts))
	}
	if !hosts[0].Healthy {
		t.Fatalf("host healthy = false, want true: %+v", hosts[0])
	}
	if hosts[0].AvailableVMs != 3 {
		t.Fatalf("AvailableVMs = %d, want 3", hosts[0].AvailableVMs)
	}
	if hosts[0].AvailableSnapshotBytes != 4096 {
		t.Fatalf("AvailableSnapshotBytes = %d, want 4096", hosts[0].AvailableSnapshotBytes)
	}
	if len(hosts[0].EgressPolicies) != 2 || hosts[0].EgressPolicies[0] != EgressPolicyProfile || hosts[0].EgressPolicies[1] != EgressPolicyDenyAll {
		t.Fatalf("EgressPolicies = %#v, want [profile deny_all]", hosts[0].EgressPolicies)
	}
}

func TestHostInventoryPollMarksUnreachableHostUnhealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"down"}`, http.StatusServiceUnavailable)
	}))
	server.Close()

	inventory := NewHostInventory([]RuntimeHost{
		{Name: "host-a", Endpoint: server.URL, Healthy: true, AvailableVMs: 1},
	}, HostInventoryOptions{HTTPClient: server.Client(), Timeout: 50 * time.Millisecond})

	if err := inventory.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce() error = %v", err)
	}

	hosts := inventory.RuntimeHosts()
	if len(hosts) != 1 {
		t.Fatalf("hosts len = %d, want 1", len(hosts))
	}
	if hosts[0].Healthy {
		t.Fatalf("host healthy = true, want false: %+v", hosts[0])
	}

	scheduler := NewScheduler(hosts, nil, nil)
	decision, err := scheduler.Schedule(ScheduleRequest{TenantID: "tenant-1", EgressPolicy: EgressPolicyProfile}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if decision.Allowed || decision.Reason != "no_eligible_host" {
		t.Fatalf("decision = %+v, want no_eligible_host denial", decision)
	}
}

func TestHostInventoryPollForwardsBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer poll-token" {
			t.Fatalf("Authorization = %q, want Bearer poll-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","available_vms":1,"available_snapshot_bytes":1024,"egress_policies":["profile"]}`))
	}))
	defer server.Close()

	inventory := NewHostInventory([]RuntimeHost{
		{Name: "host-a", Endpoint: server.URL},
	}, HostInventoryOptions{HTTPClient: server.Client(), APIToken: "poll-token"})

	if err := inventory.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce() error = %v", err)
	}
	if !inventory.RuntimeHosts()[0].Healthy {
		t.Fatal("host was not marked healthy")
	}
}
