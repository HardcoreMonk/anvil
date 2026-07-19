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

func TestQuotaAggregate_EmptyStore(t *testing.T) {
	agg := NewQuotaStore("").QuotaAggregate()
	if agg.TenantsTotal != 0 {
		t.Fatalf("TenantsTotal = %d, want 0", agg.TenantsTotal)
	}
	if len(agg.Resources) != 5 {
		t.Fatalf("Resources len = %d, want 5", len(agg.Resources))
	}
	wantOrder := []string{"snapshot_bytes", "snapshot_count", "active_vms", "concurrent_tasks", "retained_audit_records"}
	for i, r := range agg.Resources {
		if r.Resource != wantOrder[i] {
			t.Fatalf("Resources[%d] = %q, want %q", i, r.Resource, wantOrder[i])
		}
		if r.UsageTotal != 0 || r.LimitTotal != 0 || r.Near != 0 || r.Over != 0 {
			t.Fatalf("empty %s = %+v, want all zero", r.Resource, r)
		}
	}
}

// resourceAgg returns the ResourceQuotaAggregate for a resource label (test helper).
func resourceAgg(t *testing.T, agg QuotaAggregate, resource string) ResourceQuotaAggregate {
	t.Helper()
	for _, r := range agg.Resources {
		if r.Resource == resource {
			return r
		}
	}
	t.Fatalf("resource %q not found in aggregate", resource)
	return ResourceQuotaAggregate{}
}

func TestQuotaAggregate_NearOverUnderAndUnlimited(t *testing.T) {
	s := NewQuotaStore("")
	// under: 50% of a 100-byte snapshot limit.
	mustSetQuota(t, s, "tenant.under", TenantQuota{SnapshotBytes: 100})
	mustAddUsage(t, s, "tenant.under", TenantUsage{SnapshotBytes: 50})
	// near: exactly 90%.
	mustSetQuota(t, s, "tenant.near", TenantQuota{SnapshotBytes: 100})
	mustAddUsage(t, s, "tenant.near", TenantUsage{SnapshotBytes: 90})
	// at100: exactly at limit -> near (not over).
	mustSetQuota(t, s, "tenant.at100", TenantQuota{SnapshotBytes: 100})
	mustAddUsage(t, s, "tenant.at100", TenantUsage{SnapshotBytes: 100})
	// over: above limit.
	mustSetQuota(t, s, "tenant.over", TenantQuota{SnapshotBytes: 100})
	mustAddUsage(t, s, "tenant.over", TenantUsage{SnapshotBytes: 150})
	// unlimited: limit 0, usage present -> counts in usage_total only.
	mustAddUsage(t, s, "tenant.unlimited", TenantUsage{SnapshotBytes: 999})

	agg := s.QuotaAggregate()
	if agg.TenantsTotal != 5 {
		t.Fatalf("TenantsTotal = %d, want 5", agg.TenantsTotal)
	}
	sb := resourceAgg(t, agg, "snapshot_bytes")
	if sb.UsageTotal != 50+90+100+150+999 {
		t.Fatalf("UsageTotal = %d, want %d", sb.UsageTotal, 50+90+100+150+999)
	}
	if sb.LimitTotal != 100+100+100+100 { // unlimited (limit 0) excluded
		t.Fatalf("LimitTotal = %d, want 400", sb.LimitTotal)
	}
	if sb.Near != 2 { // near + at100
		t.Fatalf("Near = %d, want 2", sb.Near)
	}
	if sb.Over != 1 {
		t.Fatalf("Over = %d, want 1", sb.Over)
	}
	// A resource nobody set a limit or usage for stays zero.
	av := resourceAgg(t, agg, "active_vms")
	if av.UsageTotal != 0 || av.LimitTotal != 0 || av.Near != 0 || av.Over != 0 {
		t.Fatalf("active_vms = %+v, want all zero", av)
	}
}

func TestQuotaAggregate_JustBelowNearNotCounted(t *testing.T) {
	s := NewQuotaStore("")
	// 89% of 100 -> below the 0.9 threshold -> neither near nor over.
	mustSetQuota(t, s, "tenant.x", TenantQuota{SnapshotBytes: 100})
	mustAddUsage(t, s, "tenant.x", TenantUsage{SnapshotBytes: 89})
	sb := resourceAgg(t, s.QuotaAggregate(), "snapshot_bytes")
	if sb.Near != 0 || sb.Over != 0 {
		t.Fatalf("89%% -> Near=%d Over=%d, want 0/0", sb.Near, sb.Over)
	}
}

// mustSetQuota / mustAddUsage are test helpers (fail on error).
func mustSetQuota(t *testing.T, s *QuotaStore, tenantID string, q TenantQuota) {
	t.Helper()
	if err := s.SetTenantQuota(tenantID, q); err != nil {
		t.Fatalf("SetTenantQuota(%s): %v", tenantID, err)
	}
}

func mustAddUsage(t *testing.T, s *QuotaStore, tenantID string, u TenantUsage) {
	t.Helper()
	if err := s.UpdateTenantUsage(tenantID, u); err != nil {
		t.Fatalf("UpdateTenantUsage(%s): %v", tenantID, err)
	}
}
