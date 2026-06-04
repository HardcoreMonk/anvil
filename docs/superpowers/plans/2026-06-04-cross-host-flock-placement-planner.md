# Cross-host Flock Placement Planner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a scheduler dry-run planner that can distribute flock agent slots across runtime hosts without creating VMs.

**Architecture:** Keep daemon `POST /flocks` and MCP `anvil_spawn_flock` runtime behavior unchanged. Add an internal `Scheduler.ScheduleFlock` planner that validates quota once for the full flock and reserves host capacity per agent slot. Expose it through scheduler service `POST /schedule/flock` for operator dry-run visibility.

**Tech Stack:** Go standard library, existing `internal/anvilmcp` scheduler/tenant policy helpers, existing scheduler service HTTP tests, markdown docs.

---

## File Structure

- Create `internal/anvilmcp/flock_placement_plan.go`: cross-host flock planner request/response types, agent placement type, role-to-agent-ID helper, per-plan host reservation logic, bounded denial mapping.
- Create `internal/anvilmcp/flock_placement_plan_test.go`: focused planner tests for cross-host distribution, same-host packing, quota denial, invalid role validation, egress filtering, and in-plan reservation.
- Modify `internal/anvilmcp/scheduler_service.go`: add `POST /schedule/flock`, request decoding, persistence degraded gate reuse, host status summary injection.
- Modify `internal/anvilmcp/scheduler_service_test.go`: service endpoint tests for distributed plan, quota denial, method validation, persistence gate, invalid body.
- Modify `README.md`: document scheduler `POST /schedule/flock` as dry-run only.
- Modify `docs/architecture/mcp-architecture.md`: clarify that cross-host planner exists but `anvil_spawn_flock` still creates single-host flocks until create slice lands.
- Modify `docs/architecture/multi-tenant-roadmap.md`: move cross-host planner to current foundation and keep true create/Town Wall as future work.
- Modify `RELEASE_NOTES.md`: add Unreleased planner entry and verification commands.

---

### Task 1: Add Flock Placement Planner Types And Tests

**Files:**
- Create: `internal/anvilmcp/flock_placement_plan.go`
- Create: `internal/anvilmcp/flock_placement_plan_test.go`

- [ ] **Step 1: Write failing planner tests**

Create `internal/anvilmcp/flock_placement_plan_test.go`:

```go
package anvilmcp

import "testing"

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
}
```

- [ ] **Step 2: Run planner tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerScheduleFlock' -count=1
```

Expected: FAIL with undefined identifiers such as `FlockPlacementPlanRequest` and missing `ScheduleFlock`.

- [ ] **Step 3: Add planner implementation**

Create `internal/anvilmcp/flock_placement_plan.go`:

```go
package anvilmcp

import "strconv"

type FlockPlacementPlanRequest struct {
	TenantID     string       `json:"tenant_id"`
	EgressPolicy EgressPolicy `json:"egress_policy"`
	Roles        []string     `json:"roles"`
}

type FlockPlacementPlan struct {
	Allowed           bool                  `json:"allowed"`
	Reason            string                `json:"reason"`
	TenantID          string                `json:"tenant_id"`
	EgressPolicy      EgressPolicy          `json:"egress_policy"`
	Agents            []FlockAgentPlacement `json:"agents"`
	HostStatusSummary HostStatusSummary     `json:"host_status_summary"`
	Quota             QuotaDecision         `json:"quota,omitempty"`
	Requested         TenantUsage           `json:"requested"`
	CurrentUsage      TenantUsage           `json:"current_usage"`
	Limit             TenantQuota           `json:"limit"`
}

type FlockAgentPlacement struct {
	AgentID string      `json:"agent_id"`
	Role    string      `json:"role"`
	Host    RuntimeHost `json:"host"`
}

func (s *Scheduler) ScheduleFlock(req FlockPlacementPlanRequest) (FlockPlacementPlan, error) {
	tenantID, err := NormalizeTenantID(req.TenantID)
	if err != nil {
		return FlockPlacementPlan{}, err
	}
	egressPolicy, err := NormalizeEgressPolicy(string(req.EgressPolicy))
	if err != nil {
		return FlockPlacementPlan{}, err
	}
	roles, err := normalizeFlockRoles(req.Roles)
	if err != nil {
		return FlockPlacementPlan{}, err
	}

	requested := TenantUsage{ActiveVMs: int64(len(roles))}
	limit := s.quotas[tenantID]
	current := s.usage[tenantID]
	quotaDecision, err := CheckTenantQuota(limit, current, requested)
	if err != nil {
		return FlockPlacementPlan{}, err
	}

	base := FlockPlacementPlan{
		TenantID:     tenantID,
		EgressPolicy: egressPolicy,
		Quota:        quotaDecision,
		Requested:    requested,
		CurrentUsage: current,
		Limit:        limit,
		Agents:       []FlockAgentPlacement{},
	}
	if !quotaDecision.Allowed {
		base.Allowed = false
		base.Reason = normalizeScheduleDecisionReason(quotaDecision.Reason)
		return base, nil
	}

	reserved := make(map[string]int64)
	roleSeq := make(map[string]int, len(roles))
	agents := make([]FlockAgentPlacement, 0, len(roles))
	for _, role := range roles {
		roleSeq[role]++
		agentID := flockAgentID(role, roleSeq[role])
		host, err := selectHostForFlockAgent(s.hosts, ScheduleRequest{
			TenantID:           tenantID,
			EgressPolicy:       egressPolicy,
			RequestedActiveVMs: 1,
		}, reserved)
		if err != nil {
			base.Allowed = false
			base.Reason = FlockPlacementReasonNoEligibleHost
			base.Agents = []FlockAgentPlacement{}
			return base, nil
		}
		reserved[host.Name]++
		agents = append(agents, FlockAgentPlacement{
			AgentID: agentID,
			Role:    role,
			Host:    host,
		})
	}

	base.Allowed = true
	base.Reason = FlockPlacementReasonScheduled
	base.Agents = agents
	return base, nil
}

func flockAgentID(role string, sequence int) string {
	return role + "-" + strconv.Itoa(sequence)
}

func selectHostForFlockAgent(hosts []RuntimeHost, req ScheduleRequest, reserved map[string]int64) (RuntimeHost, error) {
	adjusted := make([]RuntimeHost, 0, len(hosts))
	for _, host := range hosts {
		host = cloneRuntimeHost(host)
		if used := reserved[host.Name]; used > 0 {
			host.AvailableVMs -= used
		}
		adjusted = append(adjusted, host)
	}
	return SelectRuntimeHost(adjusted, req)
}
```

- [ ] **Step 4: Run planner tests and verify they pass**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerScheduleFlock' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit planner helper**

Run:

```bash
git add internal/anvilmcp/flock_placement_plan.go internal/anvilmcp/flock_placement_plan_test.go
git commit -m "feat: add cross-host flock placement planner"
```

---

### Task 2: Expose Scheduler `/schedule/flock` Dry-run Endpoint

**Files:**
- Modify: `internal/anvilmcp/scheduler_service.go`
- Modify: `internal/anvilmcp/scheduler_service_test.go`

- [ ] **Step 1: Write failing scheduler service endpoint tests**

Append these tests to `internal/anvilmcp/scheduler_service_test.go` before `schedulerServiceListHosts`:

```go
func TestSchedulerServiceSchedulesFlockAcrossHosts(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetHost(RuntimeHost{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusHealthy})
	_ = store.SetHostObservation("host-b", HostObservation{Status: HostStatusHealthy})
	quota := NewQuotaStore(filepath.Join(t.TempDir(), "tenants.json"))
	if err := quota.SetTenantQuota("tenant-1", TenantQuota{ActiveVMs: 2}); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, QuotaStore: quota})

	req := httptest.NewRequest(http.MethodPost, "/schedule/flock", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","roles":["worker","reviewer"]}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /schedule/flock status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var plan FlockPlacementPlan
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if !plan.Allowed || len(plan.Agents) != 2 {
		t.Fatalf("plan = %+v, want allowed two-agent plan", plan)
	}
	if plan.Agents[0].Host.Name != "host-a" || plan.Agents[1].Host.Name != "host-b" {
		t.Fatalf("agent hosts = %q/%q, want host-a/host-b", plan.Agents[0].Host.Name, plan.Agents[1].Host.Name)
	}
	if plan.HostStatusSummary.Healthy != 2 {
		t.Fatalf("host status summary = %+v, want two healthy hosts", plan.HostStatusSummary)
	}
}

func TestSchedulerServiceFlockScheduleDeniesQuota(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	quota := NewQuotaStore(filepath.Join(t.TempDir(), "tenants.json"))
	if err := quota.SetTenantQuota("tenant-1", TenantQuota{ActiveVMs: 1}); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, QuotaStore: quota})

	req := httptest.NewRequest(http.MethodPost, "/schedule/flock", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","roles":["worker","reviewer"]}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /schedule/flock status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var plan FlockPlacementPlan
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if plan.Allowed || plan.Reason != FlockPlacementReasonQuotaExceeded {
		t.Fatalf("plan = %+v, want quota denial", plan)
	}
}

func TestSchedulerServiceFlockScheduleRejectsWrongMethodBeforePersistenceGate(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetControlLoopStatus(ControlLoopStatus{PersistenceDegraded: true})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: true})

	req := httptest.NewRequest(http.MethodGet, "/schedule/flock", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /schedule/flock status = %d body=%s, want 405", rr.Code, rr.Body.String())
	}
}

func TestSchedulerServiceFlockScheduleRequiresPersistenceWhenConfigured(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetControlLoopStatus(ControlLoopStatus{PersistenceDegraded: true, LastError: "save failed"})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: true})

	req := httptest.NewRequest(http.MethodPost, "/schedule/flock", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","roles":["worker"]}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /schedule/flock status = %d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "scheduler persistence degraded") {
		t.Fatalf("body = %q, want persistence degraded message", rr.Body.String())
	}
}

func TestSchedulerServiceFlockScheduleRejectsInvalidBody(t *testing.T) {
	service := NewSchedulerService(SchedulerServiceOptions{})

	req := httptest.NewRequest(http.MethodPost, "/schedule/flock", strings.NewReader(`{`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /schedule/flock invalid body status = %d body=%s, want 400", rr.Code, rr.Body.String())
	}
}
```

- [ ] **Step 2: Run service tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerService.*FlockSchedule|TestSchedulerServiceSchedulesFlockAcrossHosts' -count=1
```

Expected: FAIL because `/schedule/flock` is not registered and `FlockPlacementPlan` is not wired through the service.

- [ ] **Step 3: Add scheduler flock request type**

In `internal/anvilmcp/scheduler_service.go`, add this type after `schedulerRequest`:

```go
type schedulerFlockRequest struct {
	TenantID     string       `json:"tenant_id"`
	EgressPolicy EgressPolicy `json:"egress_policy"`
	Roles        []string     `json:"roles"`
}
```

- [ ] **Step 4: Register `/schedule/flock`**

In `SchedulerService.Handler`, add this route after the existing schedule routes:

```go
mux.HandleFunc("/schedule/flock", s.handleScheduleFlock)
```

- [ ] **Step 5: Implement `handleScheduleFlock`**

Add this method after `handleSchedule`:

```go
func (s *SchedulerService) handleScheduleFlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.requirePersistence && s.placements.State().ControlLoopStatus.PersistenceDegraded {
		http.Error(w, "scheduler persistence degraded", http.StatusServiceUnavailable)
		return
	}
	var req schedulerFlockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid flock schedule body", http.StatusBadRequest)
		return
	}
	state := s.placements.State()
	hosts := runtimeHostsFromPlacementState(state)
	quotas, usage := s.quotas.SchedulerInputs()
	plan, err := NewScheduler(hosts, quotas, usage).ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     req.TenantID,
		EgressPolicy: req.EgressPolicy,
		Roles:        req.Roles,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	plan.HostStatusSummary = SummarizeHostStatuses(hosts, state.HostObservations)
	writeSchedulerJSON(w, plan)
}
```

- [ ] **Step 6: Run focused service tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerService.*FlockSchedule|TestSchedulerServiceSchedulesFlockAcrossHosts' -count=1
```

Expected: PASS.

- [ ] **Step 7: Run package tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit scheduler service endpoint**

Run:

```bash
git add internal/anvilmcp/scheduler_service.go internal/anvilmcp/scheduler_service_test.go
git commit -m "feat: expose flock placement dry-run schedule"
```

---

### Task 3: Document Planner-only Cross-host Status

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture/mcp-architecture.md`
- Modify: `docs/architecture/multi-tenant-roadmap.md`
- Modify: `RELEASE_NOTES.md`

- [ ] **Step 1: Update README scheduler API table**

In `README.md`, in the scheduler service API table near `POST /schedule/spawn` and `POST /schedule/restore`, add:

```markdown
| `POST /schedule/flock` | flock roles를 host별 agent placement plan으로 dry-run한다. VM은 생성하지 않는다. |
```

- [ ] **Step 2: Update README flock placement paragraph**

In `README.md`, near the paragraph that currently says `true cross-host flock member 분산... 후속 범위다`, replace the final sentence with:

```markdown
`POST /schedule/flock`은 cross-host agent placement plan을 dry-run으로 반환하지만,
`anvil_spawn_flock` runtime create path는 아직 scheduler-aware single-host placement를
사용한다. true cross-host member VM 생성, coordinator Town Wall, cross-host `gtcall`은
후속 범위다.
```

- [ ] **Step 3: Update MCP architecture**

In `docs/architecture/mcp-architecture.md`, add this note near the `anvil_spawn_flock` router mode section:

```markdown
Scheduler service `POST /schedule/flock`은 cross-host flock placement plan을 dry-run으로
계산한다. 이 endpoint는 operator planning surface이며 VM을 만들지 않는다. MCP
`anvil_spawn_flock`은 cross-host create slice가 구현되기 전까지 기존 scheduler-aware
single-host create path를 유지한다.
```

- [ ] **Step 4: Update multi-tenant roadmap**

In `docs/architecture/multi-tenant-roadmap.md`, update the scheduler foundation bullets with:

```markdown
- `POST /schedule/flock` dry-run planner는 role별 agent slot을 host capacity에 맞춰
  분산 배치할 수 있는지 계산한다. 실제 cross-host VM 생성과 coordinator Town Wall은
  별도 create slice로 남아 있다.
```

- [ ] **Step 5: Update release notes**

In `RELEASE_NOTES.md`, under the top `# Unreleased — Scheduler flock placement metrics`
section and its `## 추가됨` heading, add these bullets before the existing metrics bullets:

```markdown
- scheduler service에 `POST /schedule/flock` dry-run endpoint를 추가한다. 이 endpoint는
  flock roles를 host별 agent placement plan으로 계산하지만 VM을 생성하지 않는다.
- cross-host planner는 tenant quota를 roles 수 기준으로 한 번 검증하고, host capacity는
  agent slot별 reservation으로 초과하지 않게 계산한다.
```

- [ ] **Step 6: Run docs diff check**

Run:

```bash
git diff -- README.md docs/architecture/mcp-architecture.md docs/architecture/multi-tenant-roadmap.md RELEASE_NOTES.md
git diff --check
```

Expected: docs mention dry-run only and `git diff --check` prints no errors.

- [ ] **Step 7: Commit docs**

Run:

```bash
git add README.md docs/architecture/mcp-architecture.md docs/architecture/multi-tenant-roadmap.md RELEASE_NOTES.md
git commit -m "docs: document flock placement planner"
```

---

### Task 4: Final Verification

**Files:**
- Verify all changed files from Tasks 1-3.

- [ ] **Step 1: Run focused planner and service tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerScheduleFlock|TestSchedulerService.*FlockSchedule|TestSchedulerServiceSchedulesFlockAcrossHosts' -count=1
```

Expected: PASS.

- [ ] **Step 2: Run package tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-scheduler -count=1
```

Expected: PASS.

- [ ] **Step 3: Run broad tests**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 4: Run build verification**

Run:

```bash
go build ./cmd/anvil-scheduler
go build ./cmd/anvil-mcp
go build ./cmd/goose-daemon
```

Expected: all commands exit 0.

- [ ] **Step 5: Run diff and status checks**

Run:

```bash
git diff --check
git status --short --branch
```

Expected: `git diff --check` prints no errors. `git status` shows `main...origin/main` with local commits ahead until push.

- [ ] **Step 6: Push and verify CI**

Run:

```bash
git push origin main
gh run list --branch main --limit 5
```

Expected: a new CI run for the final commit appears and completes successfully.

---

## Self-review Notes

- Spec coverage: this plan implements only the planner slice from `docs/superpowers/specs/2026-06-04-cross-host-flock-placement-design.md`: `ScheduleFlock`, role-level reservation, bounded denied reasons, scheduler dry-run endpoint, and docs. It intentionally excludes daemon agent spawn, coordinator Town Wall, routed flock registry, lifecycle routing, rollback, and guest callback URL injection.
- Security: the planner returns `RuntimeHost` values through scheduler operator API, matching existing `/schedule/spawn` trust boundary. No MCP user-facing output, token, daemon raw body, or agent token field is added.
- Runtime behavior: `anvil_spawn_flock` remains scheduler-aware single-host create. This plan does not change daemon `POST /flocks`.
