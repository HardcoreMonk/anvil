package anvilmcp

import (
	"fmt"
	"strings"
)

// RenderQuotaMetrics renders the fleet-aggregate tenant quota gauges in Prometheus
// text format, matching RenderSchedulerMetrics's imperative style. Labels are the
// bounded `resource` enum only — never tenant/host identity (scheduler metric-label
// policy). Resources render in QuotaAggregate.Resources order (snapshot first).
func RenderQuotaMetrics(agg QuotaAggregate) string {
	var out strings.Builder

	writeQuotaResourceGauge(&out, "anvil_scheduler_quota_usage_total",
		"Fleet-summed tenant quota usage by resource.", agg,
		func(r ResourceQuotaAggregate) int64 { return r.UsageTotal })
	writeQuotaResourceGauge(&out, "anvil_scheduler_quota_limit_total",
		"Fleet-summed tenant quota limit by resource (limit>0 tenants only).", agg,
		func(r ResourceQuotaAggregate) int64 { return r.LimitTotal })
	writeQuotaResourceGauge(&out, "anvil_scheduler_quota_tenants_near",
		"Tenants at or above the near-quota threshold (>=90%, not over) by resource.", agg,
		func(r ResourceQuotaAggregate) int64 { return int64(r.Near) })
	writeQuotaResourceGauge(&out, "anvil_scheduler_quota_tenants_over",
		"Tenants over their quota (usage>limit) by resource.", agg,
		func(r ResourceQuotaAggregate) int64 { return int64(r.Over) })

	writeSchedulerGauge(&out, "anvil_scheduler_quota_tenants_total",
		"Total tenants tracked in the quota store.", float64(agg.TenantsTotal))

	return out.String()
}

// writeQuotaResourceGauge writes one resource-labeled gauge family: a single
// HELP/TYPE header, then one line per resource in aggregate order.
func writeQuotaResourceGauge(out *strings.Builder, name, help string, agg QuotaAggregate, pick func(ResourceQuotaAggregate) int64) {
	fmt.Fprintf(out, "# HELP %s %s\n", name, help)
	fmt.Fprintf(out, "# TYPE %s gauge\n", name)
	for _, r := range agg.Resources {
		fmt.Fprintf(out, "%s{resource=\"%s\"} %d\n", name, r.Resource, pick(r))
	}
}
