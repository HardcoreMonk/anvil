package main

import "testing"

func TestLoadSchedulerConfigDefaultsAndEnv(t *testing.T) {
	t.Setenv("ANVIL_SCHEDULER_ADDR", "")
	t.Setenv("ANVIL_SCHEDULER_STATE", "")
	t.Setenv("ANVIL_SCHEDULER_QUOTA_STORE", "")
	cfg := loadSchedulerConfig()
	if cfg.Addr != defaultSchedulerAddr {
		t.Fatalf("default addr = %q, want %q", cfg.Addr, defaultSchedulerAddr)
	}

	t.Setenv("ANVIL_SCHEDULER_ADDR", "0.0.0.0:3999")
	t.Setenv("ANVIL_SCHEDULER_STATE", "/var/lib/anvil/scheduler.json")
	t.Setenv("ANVIL_SCHEDULER_QUOTA_STORE", "/var/lib/anvil/tenants.json")
	cfg = loadSchedulerConfig()
	if cfg.Addr != "0.0.0.0:3999" || cfg.PlacementPath != "/var/lib/anvil/scheduler.json" || cfg.QuotaStorePath != "/var/lib/anvil/tenants.json" {
		t.Fatalf("cfg = %+v", cfg)
	}
}
