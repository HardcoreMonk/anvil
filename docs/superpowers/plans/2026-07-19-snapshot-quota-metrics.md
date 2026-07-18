# Snapshot (tenant) quota metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose fleet-aggregate tenant quota usage/limit and near/over-quota tenant counts (per resource) on the scheduler's existing Prometheus `/metrics`, so operators observe quota headroom and alert on near-overflow via Grafana — with no per-tenant labels, no new frontend, no new API.

**Architecture:** Three units in `internal/anvilmcp`, following the scheduler's existing imperative metric-render style. `QuotaStore.QuotaAggregate()` does one locked pass over tenants → a tenant-anonymous rollup; `RenderQuotaMetrics()` renders it in Prometheus text format; `handleMetrics` reloads the quota store (for freshness — the daemon writes it out-of-process) and appends the quota block after the existing scheduler metrics.

**Tech Stack:** Go 1.25 (go.mod-pinned), `internal/anvilmcp` scheduler service, Prometheus text exposition (hand-rendered, no client library), `net/http`.

## Global Constraints

- **Aggregate only — no per-tenant identity.** Metric labels are the bounded `resource` enum ONLY. Never emit `tenant_id`, host, or any PII label (scheduler metric-label policy, `docs/operations/observability.md`). The scheduler `/metrics` is unauthenticated.
- **Metric names (exact):** `anvil_scheduler_quota_usage_total{resource}`, `anvil_scheduler_quota_limit_total{resource}`, `anvil_scheduler_quota_tenants_near{resource}`, `anvil_scheduler_quota_tenants_over{resource}`, and `anvil_scheduler_quota_tenants_total` (no label). All gauges.
- **`resource` bounded enum, fixed render order:** `snapshot_bytes`, `snapshot_count`, `active_vms`, `concurrent_tasks`, `retained_audit_records`.
- **Semantics:** `over` = `usage > limit` (strict); `near` = `limit > 0 && usage <= limit && float64(usage) >= nearQuotaThreshold*float64(limit)` (so exactly 100% is *near*, above is *over* — mutually exclusive). `limit <= 0` (unset/unlimited): excluded from `limit_total`, `near`, `over`; its usage still counts in `usage_total`. `nearQuotaThreshold = 0.9` (constant, not configurable).
- **Freshness:** `handleMetrics` calls `s.quotas.Load()` before aggregating (the daemon writes the quota-store file out-of-process; the scheduler only reads it and never holds unsaved quota, so reloading is safe — mirrors the existing `s.placements.Load()` in the same handler).
- **No behavior change to scheduling.** Only additive read-only metrics.
- **Verification per task:** `go test ./internal/anvilmcp/... -race`, `go build ./...`, `go vet ./...`, `gofmt -l .` (must print nothing — the CI gofmt gate added in PR #83 now fails the build on drift). Go 1.25 and the local toolchain agree on gofmt; run `gofmt -w` on any file you touch before committing.

## File Structure

- `internal/anvilmcp/quota_store.go` (modify) — add the aggregate types, the `quotaResources` table, `nearQuotaThreshold`, and the `QuotaAggregate()` method. This file already owns `QuotaStore` and its locking, so the aggregate (which reads `s.state` under the store lock) lives here.
- `internal/anvilmcp/quota_metrics.go` (create) — the Prometheus render of the aggregate. Kept separate from `scheduler_metrics.go` so the quota render has one clear responsibility, mirroring how the render helpers there are organized.
- `internal/anvilmcp/scheduler_service.go` (modify `handleMetrics`, lines 69-77) — reload + append the quota block.
- Tests: `internal/anvilmcp/quota_store_test.go` (exists, append), `internal/anvilmcp/quota_metrics_test.go` (create), `internal/anvilmcp/scheduler_service_test.go` (exists, append integration test).
- Docs: `docs/operations/observability.md`, `docs/operations/runbook.md`, `CONTEXT.md`.

---

### Task 1: `QuotaStore.QuotaAggregate()` — tenant-anonymous rollup

**Files:**
- Modify: `internal/anvilmcp/quota_store.go` (add types + `quotaResources` + `nearQuotaThreshold` + `QuotaAggregate` method; no new imports needed — `quota_store.go` already imports `encoding/json`, `fmt`, `os`, `path/filepath`, `sort`, `sync`, and the additions use only built-ins).
- Test: `internal/anvilmcp/quota_store_test.go` (append).

**Interfaces:**
- Consumes: existing `TenantQuota` / `TenantUsage` structs (`internal/anvilmcp/tenant_policy.go`), each with `int64` fields `ActiveVMs`, `SnapshotCount`, `SnapshotBytes`, `ConcurrentTasks`, `RetainedAuditRecords`; existing `QuotaStore` with `mu sync.RWMutex` and `state.Tenants map[string]TenantQuotaState` (each `TenantQuotaState{Quota TenantQuota; Usage TenantUsage}`).
- Produces (for Task 2):
  - `type ResourceQuotaAggregate struct { Resource string; UsageTotal int64; LimitTotal int64; Near int; Over int }`
  - `type QuotaAggregate struct { Resources []ResourceQuotaAggregate; TenantsTotal int }`
  - `func (s *QuotaStore) QuotaAggregate() QuotaAggregate` — `Resources` has one entry per `quotaResources` in fixed order.
  - `const nearQuotaThreshold = 0.9`

- [ ] **Step 1: Write the failing tests**

Append to `internal/anvilmcp/quota_store_test.go`:

```go
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
```

> Note: if `quota_store_test.go` already defines helpers named `mustSetQuota`/`mustAddUsage`/`resourceAgg`, reuse the existing ones and drop the duplicate definitions above (Go will error on redeclaration). Grep the file first: `grep -n "func mustSetQuota\|func mustAddUsage\|func resourceAgg" internal/anvilmcp/quota_store_test.go`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/anvilmcp/ -run TestQuotaAggregate 2>&1 | head -20`
Expected: compile failure — `QuotaAggregate`, `ResourceQuotaAggregate`, `agg.Resources`, `s.QuotaAggregate` undefined. (RED.)

- [ ] **Step 3: Implement the aggregate in `quota_store.go`**

Add to `internal/anvilmcp/quota_store.go` (place after the `QuotaStoreState` type, before `type QuotaStore struct`, or anywhere at file scope — keep it grouped):

```go
// nearQuotaThreshold is the usage/limit ratio at or above which a tenant is
// counted "near" its quota (but not yet over). Fixed by design (not configurable).
const nearQuotaThreshold = 0.9

// quotaResource maps one bounded quota dimension to its Prometheus `resource`
// label and its accessors on TenantQuota/TenantUsage. The slice order below is the
// fixed metric render order (snapshot dimensions first).
type quotaResource struct {
	label string
	limit func(TenantQuota) int64
	usage func(TenantUsage) int64
}

var quotaResources = []quotaResource{
	{"snapshot_bytes", func(q TenantQuota) int64 { return q.SnapshotBytes }, func(u TenantUsage) int64 { return u.SnapshotBytes }},
	{"snapshot_count", func(q TenantQuota) int64 { return q.SnapshotCount }, func(u TenantUsage) int64 { return u.SnapshotCount }},
	{"active_vms", func(q TenantQuota) int64 { return q.ActiveVMs }, func(u TenantUsage) int64 { return u.ActiveVMs }},
	{"concurrent_tasks", func(q TenantQuota) int64 { return q.ConcurrentTasks }, func(u TenantUsage) int64 { return u.ConcurrentTasks }},
	{"retained_audit_records", func(q TenantQuota) int64 { return q.RetainedAuditRecords }, func(u TenantUsage) int64 { return u.RetainedAuditRecords }},
}

// ResourceQuotaAggregate is the fleet-aggregate quota rollup for one resource.
type ResourceQuotaAggregate struct {
	Resource   string
	UsageTotal int64 // sum of usage across all tenants
	LimitTotal int64 // sum of limits across tenants with limit > 0
	Near       int   // tenants with limit>0 and nearQuotaThreshold <= usage/limit <= 1.0
	Over       int   // tenants with usage > limit (limit > 0)
}

// QuotaAggregate is the tenant-anonymous, fleet-wide quota rollup backing the
// scheduler quota metrics. It carries no per-tenant identity (metric-label policy:
// bounded labels only).
type QuotaAggregate struct {
	Resources    []ResourceQuotaAggregate // one per quotaResources, in fixed order
	TenantsTotal int
}

// QuotaAggregate computes the fleet-wide rollup under the store read lock.
func (s *QuotaStore) QuotaAggregate() QuotaAggregate {
	s.mu.RLock()
	defer s.mu.RUnlock()

	agg := QuotaAggregate{
		Resources:    make([]ResourceQuotaAggregate, len(quotaResources)),
		TenantsTotal: len(s.state.Tenants),
	}
	for i, res := range quotaResources {
		r := ResourceQuotaAggregate{Resource: res.label}
		for _, tenant := range s.state.Tenants {
			usage := res.usage(tenant.Usage)
			limit := res.limit(tenant.Quota)
			r.UsageTotal += usage
			if limit <= 0 {
				continue // unset/unlimited: no limit_total, near, or over.
			}
			r.LimitTotal += limit
			switch {
			case usage > limit:
				r.Over++
			case float64(usage) >= nearQuotaThreshold*float64(limit):
				r.Near++
			}
		}
		agg.Resources[i] = r
	}
	return agg
}
```

- [ ] **Step 4: Run the tests to verify GREEN**

Run: `go test ./internal/anvilmcp/ -run TestQuotaAggregate -race`
Expected: PASS (all three tests).

- [ ] **Step 5: Format, vet, commit**

```bash
cd /data/projects/claude-zone/anvil
gofmt -w internal/anvilmcp/quota_store.go internal/anvilmcp/quota_store_test.go
go vet ./internal/anvilmcp/ && gofmt -l internal/anvilmcp/quota_store.go internal/anvilmcp/quota_store_test.go
git add internal/anvilmcp/quota_store.go internal/anvilmcp/quota_store_test.go
git commit -m "feat(scheduler): QuotaStore.QuotaAggregate fleet quota rollup (no tenant labels)"
```
Expected: `gofmt -l` prints nothing; commit succeeds.

---

### Task 2: `RenderQuotaMetrics` + `handleMetrics` wiring

**Files:**
- Create: `internal/anvilmcp/quota_metrics.go`.
- Modify: `internal/anvilmcp/scheduler_service.go` (`handleMetrics`, lines 69-77).
- Test: `internal/anvilmcp/quota_metrics_test.go` (create) + `internal/anvilmcp/scheduler_service_test.go` (append integration test).

**Interfaces:**
- Consumes (from Task 1): `QuotaAggregate{Resources []ResourceQuotaAggregate; TenantsTotal int}`, `ResourceQuotaAggregate{Resource string; UsageTotal, LimitTotal int64; Near, Over int}`, `(*QuotaStore).QuotaAggregate()`. Also the existing same-package helper `writeSchedulerGauge(out *strings.Builder, name, help string, value float64)` (in `scheduler_metrics.go`) and `NewQuotaStore(path string) *QuotaStore` / `SetTenantQuota` / `UpdateTenantUsage` / `Save` (in `quota_store.go`), and `NewSchedulerService(SchedulerServiceOptions{QuotaStore, PlacementStore}) *SchedulerService` with `.Handler() http.Handler` (in `scheduler_service.go`).
- Produces: `func RenderQuotaMetrics(agg QuotaAggregate) string`.

- [ ] **Step 1: Write the failing render test**

Create `internal/anvilmcp/quota_metrics_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/anvilmcp/ -run TestRenderQuotaMetrics 2>&1 | head -15`
Expected: compile failure — `RenderQuotaMetrics` undefined. (RED.)

- [ ] **Step 3: Implement `quota_metrics.go`**

Create `internal/anvilmcp/quota_metrics.go`:

```go
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
```

- [ ] **Step 4: Run the render test to verify GREEN**

Run: `go test ./internal/anvilmcp/ -run TestRenderQuotaMetrics -race`
Expected: PASS.

- [ ] **Step 5: Write the failing integration test**

Append to `internal/anvilmcp/scheduler_service_test.go`:

```go
func TestSchedulerHandleMetrics_IncludesQuota(t *testing.T) {
	// Seed a disk-backed quota store, then Save() — handleMetrics reloads from disk
	// (the daemon writes this file out-of-process), so the metrics must reflect it.
	dir := t.TempDir()
	q := NewQuotaStore(filepath.Join(dir, "quota.json"))
	if err := q.SetTenantQuota("tenant.alpha", TenantQuota{SnapshotBytes: 100}); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	if err := q.UpdateTenantUsage("tenant.alpha", TenantUsage{SnapshotBytes: 95}); err != nil {
		t.Fatalf("UpdateTenantUsage: %v", err)
	}
	if err := q.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	svc := NewSchedulerService(SchedulerServiceOptions{QuotaStore: q})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	wantLines := []string{
		`anvil_scheduler_quota_usage_total{resource="snapshot_bytes"} 95`,
		`anvil_scheduler_quota_limit_total{resource="snapshot_bytes"} 100`,
		`anvil_scheduler_quota_tenants_near{resource="snapshot_bytes"} 1`,
		"anvil_scheduler_quota_tenants_total 1",
		"anvil_scheduler_control_loop_running", // existing scheduler metric still present
	}
	for _, w := range wantLines {
		if !strings.Contains(body, w) {
			t.Errorf("missing line %q in:\n%s", w, body)
		}
	}
}
```

> Note: ensure `scheduler_service_test.go` imports `net/http`, `net/http/httptest`, `path/filepath`, and `strings`. If any is missing, add it (the test will not compile otherwise). Grep first: `head -20 internal/anvilmcp/scheduler_service_test.go`.

- [ ] **Step 6: Run the integration test to verify it fails**

Run: `go test ./internal/anvilmcp/ -run TestSchedulerHandleMetrics_IncludesQuota 2>&1 | head -20`
Expected: FAIL — the quota lines are absent (handleMetrics does not yet render/reload quota). (RED.)

- [ ] **Step 7: Wire `handleMetrics`**

In `internal/anvilmcp/scheduler_service.go`, replace the body of `handleMetrics` (lines 69-77) so it reloads the quota store and appends the quota block:

```go
func (s *SchedulerService) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	_ = s.placements.Load()
	_ = s.quotas.Load() // reload: the daemon writes the quota store out-of-process.
	w.Header().Set("Content-Type", schedulerMetricsContentType)
	_, _ = w.Write([]byte(RenderSchedulerMetrics(s.placements.State())))
	_, _ = w.Write([]byte(RenderQuotaMetrics(s.quotas.QuotaAggregate())))
}
```

- [ ] **Step 8: Run the integration test to verify GREEN**

Run: `go test ./internal/anvilmcp/ -run TestSchedulerHandleMetrics_IncludesQuota -race`
Expected: PASS.

- [ ] **Step 9: Full package test, format, vet, commit**

```bash
cd /data/projects/claude-zone/anvil
gofmt -w internal/anvilmcp/quota_metrics.go internal/anvilmcp/quota_metrics_test.go internal/anvilmcp/scheduler_service.go internal/anvilmcp/scheduler_service_test.go
go test ./internal/anvilmcp/... -race && go vet ./internal/anvilmcp/ && gofmt -l internal/anvilmcp/
git add internal/anvilmcp/quota_metrics.go internal/anvilmcp/quota_metrics_test.go internal/anvilmcp/scheduler_service.go internal/anvilmcp/scheduler_service_test.go
git commit -m "feat(scheduler): expose aggregate quota metrics on /metrics"
```
Expected: package tests PASS, `gofmt -l internal/anvilmcp/` prints nothing.

---

### Task 3: Documentation

**Files:**
- Modify: `docs/operations/observability.md` (scheduler metric family list).
- Modify: `docs/operations/runbook.md` (near/over-quota response).
- Modify: `CONTEXT.md` (close the backlog item).

**Interfaces:** none (docs only). Metric names/semantics must match Global Constraints verbatim.

- [ ] **Step 1: Add the metric family to `observability.md`**

In `docs/operations/observability.md`, the scheduler metric family list ends with `anvil_scheduler_snapshot_replication_last_failure_timestamp_seconds` (line ~93). Insert the following bullets immediately after that line (before the blank line preceding the "metric label에는 host name..." redaction note):

```markdown
- `anvil_scheduler_quota_usage_total{resource}` / `anvil_scheduler_quota_limit_total{resource}` —
  fleet-summed tenant quota usage / limit by resource (`resource ∈ snapshot_bytes,
  snapshot_count, active_vms, concurrent_tasks, retained_audit_records`). `limit_total`은
  limit>0 테넌트만 합산(무제한=0은 제외). fleet 활용률 = `usage_total / limit_total`.
- `anvil_scheduler_quota_tenants_near{resource}` / `anvil_scheduler_quota_tenants_over{resource}` —
  resource별 near(사용률 ≥90%, 한도 이하)·over(사용>한도) 테넌트 수. **near/over 상호 배타**(정확히 100%는 near). alert: `..._tenants_over{resource="snapshot_bytes"} > 0`(critical), `..._tenants_near{...} > 0`(warning).
- `anvil_scheduler_quota_tenants_total` — quota store에 추적 중인 테넌트 총수(라벨 없음).
- **정책**: quota metric은 aggregate만 노출하며 `tenant_id`/host 라벨을 넣지 않는다(무인증 `/metrics` + bounded-enum 규율). "어느 테넌트가 near/over인지"는 metric에 없다 — host의 quota store JSON(`SchedulerQuotaStorePath`/`ANVIL_SCHEDULER_QUOTA_STORE`)을 직접 조회한다(runbook 참조). near 임계값은 0.9 고정.
```

- [ ] **Step 2: Add the near/over-quota response to `runbook.md`**

Append a subsection to `docs/operations/runbook.md` (place it near other scheduler/observability procedures; use the file's existing heading level for procedures):

```markdown
## Quota near/over 알림 대응

`anvil_scheduler_quota_tenants_near`/`_over`(scheduler `/metrics`)가 0보다 크면 해당 resource에서 한도에 근접/초과한 테넌트가 있다는 뜻이다. metric은 aggregate라 "어느 테넌트"는 담지 않는다.

1. 어느 resource인지 확인: alert의 `resource` 라벨(예: `snapshot_bytes`).
2. 어느 테넌트인지 확인: scheduler host에서 quota store JSON을 조회한다.
   ```bash
   # 경로는 ANVIL_SCHEDULER_QUOTA_STORE / cfg.scheduler_quota_store_path
   jq '.tenants | to_entries[] | {tenant: .key, usage: .value.usage, quota: .value.quota}' "$ANVIL_SCHEDULER_QUOTA_STORE"
   ```
   `usage.<resource>`가 `quota.<resource>`에 근접(≥90%)/초과한 테넌트를 찾는다.
3. 조치: 해당 테넌트의 quota 상향(daemon quota API), 오래된 snapshot GC(`POST /snapshots/gc`), 또는 워크로드 축소. quota store는 daemon이 기록하고 scheduler는 읽기 전용이므로 quota 변경은 daemon 경로로 한다.
```

- [ ] **Step 3: Close the backlog item in `CONTEXT.md`**

In `CONTEXT.md`, find the backlog line `- snapshot storage quota dashboard` (grep: `grep -n "snapshot storage quota dashboard" CONTEXT.md`) and replace it with:

```markdown
- ~~snapshot storage quota dashboard~~ — **2026-07-19 종결**: scheduler `/metrics`에 aggregate
  quota metric family(`anvil_scheduler_quota_{usage_total,limit_total,tenants_near,tenants_over}{resource}`
  + `tenants_total`) 추가 → Grafana 대시보드·near-overflow alert. per-tenant 라벨은 비목표(무인증
  `/metrics` + bounded-enum 정책; "어느 테넌트"는 quota store JSON 조회, runbook). spec/plan:
  `docs/superpowers/specs/2026-07-19-snapshot-quota-metrics-design.md`,
  `docs/superpowers/plans/2026-07-19-snapshot-quota-metrics.md`.
```

- [ ] **Step 4: Verify docs + commit**

```bash
cd /data/projects/claude-zone/anvil
grep -n "anvil_scheduler_quota_usage_total" docs/operations/observability.md
grep -n "Quota near/over" docs/operations/runbook.md
grep -n "2026-07-19 종결" CONTEXT.md
git add docs/operations/observability.md docs/operations/runbook.md CONTEXT.md
git commit -m "docs: scheduler quota metrics catalog, runbook response, backlog close"
```
Expected: all three greps match; commit succeeds. (No code changed, so no gofmt/test gate needed — but `gofmt -l .` at branch tip must still be empty from the code tasks.)

---

## Notes for the executor

- **No KVM e2e required** — this is a read-only metrics addition to the scheduler HTTP handler; no packet path, no VM lifecycle change. Package unit + handler integration tests cover it.
- **CI gofmt gate is live** (PR #83): every `.go` file you touch must be `gofmt`-clean before commit. The commands above run `gofmt -w` then `gofmt -l` on exactly the touched files.
- **Model routing (SDD):** Task 1 (aggregate — subtle near/over/limit-0 semantics) → standard model. Task 2 (render + wiring — mostly transcription) → cheap/standard. Task 3 (docs) → cheap. Final whole-branch review → most capable.
