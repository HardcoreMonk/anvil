package main

import (
	"path/filepath"
	"testing"
	"time"

	"ephemera/internal/anvilmcp"
)

func TestLoadSchedulerConfigDefaultsAndEnv(t *testing.T) {
	t.Setenv("ANVIL_SCHEDULER_ADDR", "")
	t.Setenv("ANVIL_SCHEDULER_STATE", "")
	t.Setenv("ANVIL_SCHEDULER_QUOTA_STORE", "")
	cfg, err := loadSchedulerConfig()
	if err != nil {
		t.Fatalf("loadSchedulerConfig defaults: %v", err)
	}
	if cfg.Addr != defaultSchedulerAddr {
		t.Fatalf("default addr = %q, want %q", cfg.Addr, defaultSchedulerAddr)
	}
	if cfg.PollInterval != 10*time.Second || cfg.ReconcileInterval != 30*time.Second || cfg.HostTimeout != 3*time.Second || cfg.FailureThreshold != 3 {
		t.Fatalf("default control loop config = %+v", cfg)
	}

	hostsFile := filepath.Join(t.TempDir(), "hosts.json")
	t.Setenv("ANVIL_SCHEDULER_ADDR", "0.0.0.0:3999")
	t.Setenv("ANVIL_SCHEDULER_STATE", "/var/lib/anvil/scheduler.json")
	t.Setenv("ANVIL_SCHEDULER_QUOTA_STORE", "/var/lib/anvil/tenants.json")
	t.Setenv("ANVIL_SCHEDULER_HOSTS_FILE", hostsFile)
	t.Setenv("ANVIL_SCHEDULER_POLL_INTERVAL", "2s")
	t.Setenv("ANVIL_SCHEDULER_RECONCILE_INTERVAL", "7s")
	t.Setenv("ANVIL_SCHEDULER_HOST_TIMEOUT", "500ms")
	t.Setenv("ANVIL_SCHEDULER_FAILURE_THRESHOLD", "5")
	t.Setenv("ANVIL_SCHEDULER_API_TOKEN", "scheduler-token")
	t.Setenv("ANVIL_SCHEDULER_REQUIRE_PERSISTENCE", "true")
	cfg, err = loadSchedulerConfig()
	if err != nil {
		t.Fatalf("loadSchedulerConfig env: %v", err)
	}
	if cfg.Addr != "0.0.0.0:3999" || cfg.PlacementPath != "/var/lib/anvil/scheduler.json" || cfg.QuotaStorePath != "/var/lib/anvil/tenants.json" {
		t.Fatalf("cfg paths = %+v", cfg)
	}
	if cfg.HostsFile != hostsFile || cfg.PollInterval != 2*time.Second || cfg.ReconcileInterval != 7*time.Second || cfg.HostTimeout != 500*time.Millisecond || cfg.FailureThreshold != 5 {
		t.Fatalf("cfg control loop = %+v", cfg)
	}
	if cfg.APIToken != "scheduler-token" || !cfg.RequirePersistence {
		t.Fatalf("cfg security/persistence = %+v", cfg)
	}
}

func TestLoadSchedulerConfigRejectsInvalidValues(t *testing.T) {
	t.Setenv("ANVIL_SCHEDULER_POLL_INTERVAL", "nope")
	if _, err := loadSchedulerConfig(); err == nil {
		t.Fatal("invalid poll interval error = nil")
	}

	t.Setenv("ANVIL_SCHEDULER_POLL_INTERVAL", "")
	t.Setenv("ANVIL_SCHEDULER_FAILURE_THRESHOLD", "0")
	if _, err := loadSchedulerConfig(); err == nil {
		t.Fatal("invalid failure threshold error = nil")
	}
}

func TestApplyConfiguredHostsMarksManagedAndPreservesObservation(t *testing.T) {
	store := anvilmcp.NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(anvilmcp.RuntimeHost{Name: "host-a", Endpoint: "http://old-host", Healthy: true, AvailableVMs: 9})
	_ = store.SetHostObservation("host-a", anvilmcp.HostObservation{Status: anvilmcp.HostStatusDegraded, FailureCount: 1})

	err := applyConfiguredHosts(store, []anvilmcp.RuntimeHost{{Name: "host-a", Endpoint: "http://new-host", EgressPolicies: []anvilmcp.EgressPolicy{anvilmcp.EgressPolicyProfile}}})
	if err != nil {
		t.Fatalf("applyConfiguredHosts: %v", err)
	}
	state := store.State()
	if state.Hosts["host-a"].Endpoint != "http://new-host" {
		t.Fatalf("host endpoint = %q, want config endpoint", state.Hosts["host-a"].Endpoint)
	}
	if state.HostObservations["host-a"].FailureCount != 1 {
		t.Fatalf("observation = %+v, want preserved failure count", state.HostObservations["host-a"])
	}
	if !state.ConfigManagedHosts["host-a"] {
		t.Fatalf("config managed hosts = %+v, want host-a", state.ConfigManagedHosts)
	}
}

func TestApplyConfiguredHostsNoopsWithoutHosts(t *testing.T) {
	store := anvilmcp.NewPlacementStore(t.TempDir())

	if err := applyConfiguredHosts(store, nil); err != nil {
		t.Fatalf("applyConfiguredHosts nil hosts: %v", err)
	}
}
