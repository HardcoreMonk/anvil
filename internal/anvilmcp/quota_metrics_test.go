package anvilmcp

import (
	"strings"
	"testing"
)

func TestRenderQuotaMetrics_LinesAndOrder(t *testing.T) {
	agg := QuotaAggregate{
		TenantsTotal: 3,
		Resources: []ResourceQuotaAggregate{
			{Resource: "snapshot_bytes", UsageTotal: 1389, LimitTotal: 400, Near: 2, Over: 1},
			{Resource: "snapshot_count", UsageTotal: 7, LimitTotal: 0, Near: 0, Over: 0},
			{Resource: "active_vms", UsageTotal: 0, LimitTotal: 0, Near: 0, Over: 0},
			{Resource: "concurrent_tasks", UsageTotal: 0, LimitTotal: 0, Near: 0, Over: 0},
			{Resource: "retained_audit_records", UsageTotal: 0, LimitTotal: 0, Near: 0, Over: 0},
		},
	}
	out := RenderQuotaMetrics(agg)

	wantLines := []string{
		"# HELP anvil_scheduler_quota_usage_total",
		"# TYPE anvil_scheduler_quota_usage_total gauge",
		`anvil_scheduler_quota_usage_total{resource="snapshot_bytes"} 1389`,
		`anvil_scheduler_quota_limit_total{resource="snapshot_bytes"} 400`,
		`anvil_scheduler_quota_tenants_near{resource="snapshot_bytes"} 2`,
		`anvil_scheduler_quota_tenants_over{resource="snapshot_bytes"} 1`,
		`anvil_scheduler_quota_usage_total{resource="snapshot_count"} 7`,
		"anvil_scheduler_quota_tenants_total 3",
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("missing line %q in:\n%s", w, out)
		}
	}
	// Fixed resource order: snapshot_bytes usage line precedes active_vms usage line.
	if strings.Index(out, `usage_total{resource="snapshot_bytes"}`) >= strings.Index(out, `usage_total{resource="active_vms"}`) {
		t.Errorf("resource order wrong (snapshot_bytes must precede active_vms):\n%s", out)
	}
	// Policy: no tenant/PII label ever appears.
	if strings.Contains(out, "tenant=") || strings.Contains(out, "tenant_id") {
		t.Errorf("quota metrics must not carry a tenant label:\n%s", out)
	}
}
