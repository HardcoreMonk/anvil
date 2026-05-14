package anvilmcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQuotaStoreLoadMissingFileReturnsEmptyState(t *testing.T) {
	store := NewQuotaStore(filepath.Join(t.TempDir(), "quota.json"))
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	quotas, usage := store.SchedulerInputs()
	if len(quotas) != 0 || len(usage) != 0 {
		t.Fatalf("SchedulerInputs = %#v/%#v, want empty", quotas, usage)
	}
}

func TestQuotaStorePersistsQuotaAndUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	store := NewQuotaStore(path)
	if err := store.SetTenantQuota("tenant-1", TenantQuota{ActiveVMs: 2, SnapshotBytes: 4096}); err != nil {
		t.Fatalf("SetTenantQuota() error = %v", err)
	}
	if err := store.UpdateTenantUsage("tenant-1", TenantUsage{ActiveVMs: 1, SnapshotBytes: 1024}); err != nil {
		t.Fatalf("UpdateTenantUsage() error = %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	reloaded := NewQuotaStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reloaded Load() error = %v", err)
	}
	quotas, usage := reloaded.SchedulerInputs()
	if quotas["tenant-1"].ActiveVMs != 2 || quotas["tenant-1"].SnapshotBytes != 4096 {
		t.Fatalf("quota = %+v, want active=2 snapshot_bytes=4096", quotas["tenant-1"])
	}
	if usage["tenant-1"].ActiveVMs != 1 || usage["tenant-1"].SnapshotBytes != 1024 {
		t.Fatalf("usage = %+v, want active=1 snapshot_bytes=1024", usage["tenant-1"])
	}
}

func TestQuotaStoreRejectsNegativeResultingUsage(t *testing.T) {
	store := NewQuotaStore(filepath.Join(t.TempDir(), "quota.json"))
	if err := store.UpdateTenantUsage("tenant-1", TenantUsage{ActiveVMs: 1}); err != nil {
		t.Fatalf("initial UpdateTenantUsage() error = %v", err)
	}
	err := store.UpdateTenantUsage("tenant-1", TenantUsage{ActiveVMs: -2})
	if err == nil {
		t.Fatal("UpdateTenantUsage() error = nil, want negative usage rejection")
	}
	_, usage := store.SchedulerInputs()
	if usage["tenant-1"].ActiveVMs != 1 {
		t.Fatalf("usage after rejected update = %+v, want unchanged active_vms=1", usage["tenant-1"])
	}
}

func TestQuotaStoreRejectsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatalf("write invalid quota file: %v", err)
	}
	store := NewQuotaStore(path)
	if err := store.Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid JSON error")
	}
}

func TestQuotaStoreSchedulerInputsDriveQuotaDecision(t *testing.T) {
	store := NewQuotaStore(filepath.Join(t.TempDir(), "quota.json"))
	if err := store.SetTenantQuota("tenant-1", TenantQuota{ActiveVMs: 1}); err != nil {
		t.Fatalf("SetTenantQuota() error = %v", err)
	}
	if err := store.UpdateTenantUsage("tenant-1", TenantUsage{ActiveVMs: 1}); err != nil {
		t.Fatalf("UpdateTenantUsage() error = %v", err)
	}
	quotas, usage := store.SchedulerInputs()
	scheduler := NewScheduler([]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}}, quotas, usage)

	decision, err := scheduler.Schedule(ScheduleRequest{TenantID: "tenant-1", EgressPolicy: EgressPolicyProfile}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if decision.Allowed || decision.Reason != "quota_exceeded" {
		t.Fatalf("decision = %+v, want quota_exceeded", decision)
	}
}
