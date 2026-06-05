# Cross-host Flock Create Minimal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in members-only cross-host flock create slice that spawns role VMs across scheduler-selected runtime hosts, persists routed flock membership, and cleans up created VMs on failure.

**Architecture:** Keep daemon `POST /flocks` and default MCP `anvil_spawn_flock` behavior unchanged. Add a routed flock registry to `PlacementStore`, implement `RuntimeRouter.CreateRoutedFlockMembers` with `ScheduleFlock` plus per-host `SpawnVM`, and route delete/Town Wall calls through a small optional `ReplicatingDaemon` interface when `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only` is enabled. The first slice returns `town_wall_enabled=false` and does not create Town Wall, `gtcall`, guest flock context, or daemon `FlockManager` state.

**Tech Stack:** Go standard library, existing `internal/anvilmcp` scheduler/router/store helpers, existing MCP stdio tool registration, markdown docs.

---

## File Structure

- Create `internal/anvilmcp/routed_flock.go`: routed flock output types, registry helpers, router create/delete/list/get/unsupported Town Wall methods.
- Modify `internal/anvilmcp/placement_store.go`: add `PlacementStoreState.RoutedFlocks`, clone/normalize support, and atomic routed flock registry/placement save helpers.
- Modify `internal/anvilmcp/flock_placement_metrics.go`: add bounded cross-host outcome/phase constants and normalizer acceptance.
- Modify `internal/anvilmcp/runtime_router_test.go`: router tests for cross-host success, quota denial, rollback, registry failure, cleanup pending, delete, unsupported Town Wall, and no secret leakage.
- Modify `internal/anvilmcp/tools.go`: add `RoutedFlockCreateOutput`, `Tools.CreateRoutedFlockMembers`, and `MCPCreateRoutedFlockMembers`.
- Modify `internal/anvilmcp/tools_test.go`: MCP tool behavior tests for disabled mode, success output, no Town Wall URLs, audit, and existing `anvil_spawn_flock` regression.
- Modify `internal/anvilmcp/ironclaw_schema.go` and `internal/anvilmcp/ironclaw_schema_test.go`: add `anvil_create_routed_flock_members` input schema.
- Modify `internal/anvilmcp/config.go` and `internal/anvilmcp/config_test.go`: add `cross_host_flock_create_mode` and `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`.
- Modify `internal/anvilmcp/replicating_daemon.go` and `internal/anvilmcp/replicating_daemon_test.go`: add optional routed flock controller for create-members, delete, list/get, and Town Wall unsupported routing.
- Modify `cmd/anvil-mcp/main.go` and `cmd/anvil-mcp/main_test.go`: register the new MCP tool and wire routed controller only when opt-in mode and persistent scheduler state are configured.
- Modify `configs/anvil-mcp.yaml.example`: document the new opt-in setting.
- Modify `README.md`, `docs/architecture/mcp-architecture.md`, `docs/architecture/multi-tenant-roadmap.md`, `docs/operations/observability.md`, `docs/operations/release-checklist.md`, and `RELEASE_NOTES.md`: document the members-only create slice, security constraints, metrics, and verification.

---

### Task 1: Add Routed Flock Registry Persistence

**Files:**
- Modify: `internal/anvilmcp/placement_store.go`
- Test: `internal/anvilmcp/placement_store_test.go`

- [ ] **Step 1: Write failing registry persistence tests**

Append these tests to `internal/anvilmcp/placement_store_test.go`:

```go
func TestPlacementStoreRoutedFlockRegistryPersistsAndClones(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.json")
	store := NewPlacementStore(path)
	record := RoutedFlockRecord{
		FlockID:      "routed-flock-1",
		Task:         "review changes",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{{
			AgentID:  "worker-1",
			Role:     "worker",
			VMID:     "vm-worker",
			AgentURL: "http://10.0.0.2:3000",
			Host:     "host-a",
			Status:   "running",
		}},
		CreatedAt: time.Date(2026, 6, 6, 1, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 6, 1, 1, 0, 0, time.UTC),
	}

	if err := store.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	record.Agents[0].AgentURL = "mutated"

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := reloaded.RoutedFlock("routed-flock-1")
	if !ok {
		t.Fatal("RoutedFlock ok = false, want true")
	}
	if got.Agents[0].AgentURL != "http://10.0.0.2:3000" {
		t.Fatalf("stored agent URL = %q, want original", got.Agents[0].AgentURL)
	}
	if host, ok := reloaded.VMHost("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("vm placement = %q,%v want host-a,true", host, ok)
	}
	got.Agents[0].Host = "mutated"
	again, _ := reloaded.RoutedFlock("routed-flock-1")
	if again.Agents[0].Host != "host-a" {
		t.Fatalf("RoutedFlock returned mutable state: %+v", again.Agents[0])
	}
}

func TestPlacementStoreRoutedFlockCleanupRemovesOnlyRequestedPlacements(t *testing.T) {
	store := NewPlacementStore("")
	record := RoutedFlockRecord{
		FlockID: "routed-flock-cleanup",
		Mode:    RoutedFlockModeCrossHostMembersOnly,
		Status:  RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-ok", Host: "host-a", Status: "running"},
			{AgentID: "worker-2", Role: "worker", VMID: "vm-failed", Host: "host-b", Status: "running"},
		},
	}
	if err := store.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	record.Status = RoutedFlockStatusFailedCleanupPending
	record.Agents = []RoutedFlockAgent{{AgentID: "worker-2", Role: "worker", VMID: "vm-failed", Host: "host-b", Status: "cleanup_pending"}}
	if err := store.SaveRoutedFlockAndPlacements(record, []string{"vm-ok"}); err != nil {
		t.Fatalf("cleanup SaveRoutedFlockAndPlacements: %v", err)
	}
	if _, ok := store.VMHost("vm-ok"); ok {
		t.Fatal("vm-ok placement still exists, want removed")
	}
	if host, ok := store.VMHost("vm-failed"); !ok || host != "host-b" {
		t.Fatalf("vm-failed placement = %q,%v want host-b,true", host, ok)
	}
}
```

- [ ] **Step 2: Run the registry tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestPlacementStoreRoutedFlock' -count=1
```

Expected: FAIL with undefined identifiers such as `RoutedFlockRecord`, `RoutedFlockAgent`, and `SaveRoutedFlockAndPlacements`.

- [ ] **Step 3: Add routed flock registry types and state field**

In `internal/anvilmcp/placement_store.go`, add these declarations near the existing placement state types:

```go
const (
	RoutedFlockModeCrossHostMembersOnly = "cross_host_members_only"

	RoutedFlockStatusCreating             = "creating"
	RoutedFlockStatusReady                = "ready"
	RoutedFlockStatusDeleting             = "deleting"
	RoutedFlockStatusDeleted              = "deleted"
	RoutedFlockStatusFailedCleanupPending = "failed_cleanup_pending"
)

type RoutedFlockRecord struct {
	FlockID      string             `json:"flock_id"`
	Task         string             `json:"task"`
	TenantID     string             `json:"tenant_id,omitempty"`
	EgressPolicy string             `json:"egress_policy,omitempty"`
	Mode         string             `json:"mode"`
	Status       string             `json:"status"`
	Agents       []RoutedFlockAgent `json:"agents"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
}

type RoutedFlockAgent struct {
	AgentID  string `json:"agent_id"`
	Role     string `json:"role"`
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
	Host     string `json:"host"`
	Status   string `json:"status"`
}
```

Add `RoutedFlocks` to `PlacementStoreState`:

```go
type PlacementStoreState struct {
	Hosts                 map[string]RuntimeHost          `json:"hosts"`
	VMPlacements          map[string]string               `json:"vm_placements"`
	SnapshotLocations     map[string][]string             `json:"snapshot_locations"`
	ConfigManagedHosts    map[string]bool                 `json:"config_managed_hosts,omitempty"`
	HostObservations      map[string]HostObservation      `json:"host_observations,omitempty"`
	SuspectVMPlacements   map[string]SuspectVMPlacement   `json:"suspect_vm_placements,omitempty"`
	ControlLoopStatus     ControlLoopStatus               `json:"control_loop_status,omitempty"`
	FlockPlacementMetrics FlockPlacementMetricsState      `json:"flock_placement_metrics,omitempty"`
	RoutedFlocks          map[string]RoutedFlockRecord    `json:"routed_flocks,omitempty"`
}
```

- [ ] **Step 4: Add normalize, clone, and store methods**

In `normalizePlacementStoreState`, initialize routed flocks:

```go
if state.RoutedFlocks == nil {
	state.RoutedFlocks = make(map[string]RoutedFlockRecord)
}
for flockID, record := range state.RoutedFlocks {
	normalized := normalizeRoutedFlockRecord(record)
	if normalized.FlockID == "" {
		delete(state.RoutedFlocks, flockID)
		continue
	}
	state.RoutedFlocks[normalized.FlockID] = normalized
	if normalized.FlockID != flockID {
		delete(state.RoutedFlocks, flockID)
	}
}
```

In `clonePlacementStoreState`, clone routed flocks:

```go
out.RoutedFlocks = make(map[string]RoutedFlockRecord, len(state.RoutedFlocks))
for flockID, record := range state.RoutedFlocks {
	out.RoutedFlocks[flockID] = cloneRoutedFlockRecord(record)
}
```

Add helper functions and methods below `cloneRuntimeHost`:

```go
func (s *PlacementStore) SaveRoutedFlockAndPlacements(record RoutedFlockRecord, removeVMIDs []string) error {
	record = normalizeRoutedFlockRecord(record)
	if record.FlockID == "" {
		return fmt.Errorf("flock_id must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	previous := clonePlacementStoreState(s.state)
	s.state.RoutedFlocks[record.FlockID] = record
	for _, vmID := range removeVMIDs {
		delete(s.state.VMPlacements, strings.TrimSpace(vmID))
	}
	for _, agent := range record.Agents {
		vmID := strings.TrimSpace(agent.VMID)
		host := strings.TrimSpace(agent.Host)
		if vmID != "" && host != "" && agent.Status != "deleted" {
			s.state.VMPlacements[vmID] = host
		}
	}
	if err := s.saveLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func (s *PlacementStore) RoutedFlock(flockID string) (RoutedFlockRecord, bool) {
	flockID = strings.TrimSpace(flockID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.state.RoutedFlocks[flockID]
	return cloneRoutedFlockRecord(record), ok
}

func (s *PlacementStore) ListRoutedFlocks() []RoutedFlockRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.state.RoutedFlocks))
	for flockID := range s.state.RoutedFlocks {
		names = append(names, flockID)
	}
	sort.Strings(names)
	out := make([]RoutedFlockRecord, 0, len(names))
	for _, flockID := range names {
		out = append(out, cloneRoutedFlockRecord(s.state.RoutedFlocks[flockID]))
	}
	return out
}

func normalizeRoutedFlockRecord(record RoutedFlockRecord) RoutedFlockRecord {
	record.FlockID = strings.TrimSpace(record.FlockID)
	record.Task = strings.TrimSpace(record.Task)
	record.TenantID = strings.TrimSpace(record.TenantID)
	record.EgressPolicy = strings.TrimSpace(record.EgressPolicy)
	record.Mode = strings.TrimSpace(record.Mode)
	if record.Mode == "" {
		record.Mode = RoutedFlockModeCrossHostMembersOnly
	}
	record.Status = normalizeRoutedFlockStatus(record.Status)
	agents := make([]RoutedFlockAgent, 0, len(record.Agents))
	for _, agent := range record.Agents {
		agent.AgentID = strings.TrimSpace(agent.AgentID)
		agent.Role = strings.TrimSpace(agent.Role)
		agent.VMID = strings.TrimSpace(agent.VMID)
		agent.AgentURL = strings.TrimSpace(agent.AgentURL)
		agent.Host = strings.TrimSpace(agent.Host)
		agent.Status = strings.TrimSpace(agent.Status)
		if agent.AgentID == "" || agent.Role == "" {
			continue
		}
		agents = append(agents, agent)
	}
	record.Agents = agents
	return record
}

func normalizeRoutedFlockStatus(status string) string {
	switch strings.TrimSpace(status) {
	case RoutedFlockStatusCreating:
		return RoutedFlockStatusCreating
	case RoutedFlockStatusReady:
		return RoutedFlockStatusReady
	case RoutedFlockStatusDeleting:
		return RoutedFlockStatusDeleting
	case RoutedFlockStatusDeleted:
		return RoutedFlockStatusDeleted
	case RoutedFlockStatusFailedCleanupPending:
		return RoutedFlockStatusFailedCleanupPending
	default:
		return RoutedFlockStatusCreating
	}
}

func cloneRoutedFlockRecord(record RoutedFlockRecord) RoutedFlockRecord {
	record.Agents = append([]RoutedFlockAgent(nil), record.Agents...)
	return record
}
```

- [ ] **Step 5: Run the registry tests and commit**

Run:

```bash
go test ./internal/anvilmcp -run 'TestPlacementStoreRoutedFlock' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/anvilmcp/placement_store.go internal/anvilmcp/placement_store_test.go
git commit -m "feat: persist routed flock registry"
```

---

### Task 2: Implement Routed Flock Create Success Path

**Files:**
- Create: `internal/anvilmcp/routed_flock.go`
- Modify: `internal/anvilmcp/flock_placement_metrics.go`
- Modify: `internal/anvilmcp/runtime_router_test.go`

- [ ] **Step 1: Write the two-host success test**

Add these imports to `internal/anvilmcp/runtime_router_test.go`:

```go
"encoding/json"
"path/filepath"
```

Extend `routerFakeDaemon` in `internal/anvilmcp/runtime_router_test.go` so it can return ordered spawn responses:

```go
spawnResponses []*SpawnVMResponse
spawnReqs      []SpawnVMRequest
```

Replace the first lines of `SpawnVM` with:

```go
f.spawnCalls++
f.spawnReq = req
f.spawnReqs = append(f.spawnReqs, req)
```

Before the default response, add:

```go
if len(f.spawnResponses) > 0 {
	resp := f.spawnResponses[0]
	f.spawnResponses = f.spawnResponses[1:]
	return resp, nil
}
```

Append this test:

```go
func TestRuntimeRouterCreateRoutedFlockMembersSpawnsAcrossHosts(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	hostA := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-worker", AgentURL: "http://10.0.0.2:3000", AgentToken: "secret-worker",
	}}}
	hostB := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-reviewer", AgentURL: "http://10.0.0.3:3000", AgentToken: "secret-reviewer",
	}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a.internal", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b.internal", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     " tenant-1 ",
		EgressPolicy: " PROFILE ",
	})
	if err != nil {
		t.Fatalf("CreateRoutedFlockMembers returned error: %v", err)
	}
	if !strings.HasPrefix(out.FlockID, "routed-flock-") {
		t.Fatalf("flock id = %q, want routed-flock-*", out.FlockID)
	}
	if out.Mode != RoutedFlockModeCrossHostMembersOnly || out.Status != RoutedFlockStatusReady || out.TownWallEnabled {
		t.Fatalf("output mode/status/townwall = %q/%q/%v", out.Mode, out.Status, out.TownWallEnabled)
	}
	if hostA.spawnCalls != 1 || hostB.spawnCalls != 1 {
		t.Fatalf("spawn calls hostA/hostB = %d/%d, want 1/1", hostA.spawnCalls, hostB.spawnCalls)
	}
	if hostA.spawnReqs[0].Profile != "worker" || hostB.spawnReqs[0].Profile != "reviewer" {
		t.Fatalf("profiles = %q/%q, want worker/reviewer", hostA.spawnReqs[0].Profile, hostB.spawnReqs[0].Profile)
	}
	for _, req := range []SpawnVMRequest{hostA.spawnReqs[0], hostB.spawnReqs[0]} {
		if req.TenantID != "tenant-1" || req.EgressPolicy != "profile" {
			t.Fatalf("spawn request = %+v, want normalized tenant/egress", req)
		}
	}
	if len(out.Agents) != 2 {
		t.Fatalf("agents = %+v, want two", out.Agents)
	}
	if out.Agents[0].AgentID != "worker-1" || out.Agents[0].Host != "host-a" || out.Agents[0].VMID != "vm-worker" {
		t.Fatalf("first agent = %+v", out.Agents[0])
	}
	if out.Agents[1].AgentID != "reviewer-1" || out.Agents[1].Host != "host-b" || out.Agents[1].VMID != "vm-reviewer" {
		t.Fatalf("second agent = %+v", out.Agents[1])
	}
	for _, forbidden := range []string{"secret-worker", "secret-reviewer", "host-a.internal", "host-b.internal", "agent_token"} {
		data, _ := json.Marshal(out)
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("output leaks %q: %s", forbidden, data)
		}
	}
	if record, ok := store.RoutedFlock(out.FlockID); !ok || record.Status != RoutedFlockStatusReady {
		t.Fatalf("registry = %+v,%v want ready record", record, ok)
	}
	if host, ok := store.VMHost("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("vm-worker placement = %q,%v want host-a,true", host, ok)
	}
	if host, ok := store.VMHost("vm-reviewer"); !ok || host != "host-b" {
		t.Fatalf("vm-reviewer placement = %q,%v want host-b,true", host, ok)
	}
}
```

- [ ] **Step 2: Run the success test and verify it fails**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterCreateRoutedFlockMembersSpawnsAcrossHosts' -count=1
```

Expected: FAIL with `CreateRoutedFlockMembers undefined`.

- [ ] **Step 3: Add routed metric constants, create output, and router implementation**

In `internal/anvilmcp/flock_placement_metrics.go`, add these constants. Task 3 will add the normalizer acceptance and assertions.

```go
FlockPlacementOutcomeCrossHostSuccess       = "cross_host_success"
FlockPlacementOutcomeCrossHostDenied        = "cross_host_denied"
FlockPlacementOutcomeCrossHostSpawnError    = "cross_host_spawn_error"
FlockPlacementOutcomeCrossHostRollbackError = "cross_host_rollback_error"
FlockPlacementOutcomeCrossHostRegistryError = "cross_host_registry_error"

FlockPlacementPhasePlan         = "plan"
FlockPlacementPhaseAgentSpawn   = "agent_spawn"
FlockPlacementPhaseRegistrySave = "registry_save"
FlockPlacementPhaseRollback     = "rollback"
```

Create `internal/anvilmcp/routed_flock.go`:

```go
package anvilmcp

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type RoutedFlockCreateOutput struct {
	FlockID         string             `json:"flock_id"`
	Task            string             `json:"task"`
	TenantID        string             `json:"tenant_id,omitempty"`
	EgressPolicy    string             `json:"egress_policy,omitempty"`
	Mode            string             `json:"mode"`
	Status          string             `json:"status"`
	TownWallEnabled bool               `json:"town_wall_enabled"`
	Agents          []RoutedFlockAgent `json:"agents"`
}

func (r *RuntimeRouter) CreateRoutedFlockMembers(ctx context.Context, req FlockCreateRequest) (*RoutedFlockCreateOutput, error) {
	if r == nil {
		return nil, fmt.Errorf("runtime router is nil")
	}
	if r.scheduler == nil {
		return nil, fmt.Errorf("runtime router scheduler is nil")
	}
	if r.placementStore == nil || strings.TrimSpace(r.placementStore.path) == "" {
		return nil, fmt.Errorf("persistent placement store is required for routed flock create")
	}
	totalStart := time.Now()
	planStart := time.Now()
	plan, err := r.scheduler.ScheduleFlock(FlockPlacementPlanRequest{
		TenantID:     req.TenantID,
		EgressPolicy: EgressPolicy(req.EgressPolicy),
		Roles:        req.Roles,
	})
	planLatency := time.Since(planStart)
	if err != nil {
		r.recordRoutedFlockMetric(FlockPlacementOutcomeSchedulerError, FlockPlacementReasonInvalidRequest, map[string]time.Duration{
			FlockPlacementPhasePlan:  planLatency,
			FlockPlacementPhaseTotal: time.Since(totalStart),
		})
		return nil, err
	}
	if !plan.Allowed {
		r.recordRoutedFlockMetric(FlockPlacementOutcomeCrossHostDenied, normalizeScheduleDecisionReason(plan.Reason), map[string]time.Duration{
			FlockPlacementPhasePlan:  planLatency,
			FlockPlacementPhaseTotal: time.Since(totalStart),
		})
		return nil, &ScheduleDeniedError{Decision: ScheduleDecision{Allowed: false, Reason: plan.Reason}}
	}

	now := time.Now().UTC()
	record := RoutedFlockRecord{
		FlockID:      fmt.Sprintf("routed-flock-%d", now.UnixNano()),
		Task:         strings.TrimSpace(req.Task),
		TenantID:     plan.TenantID,
		EgressPolicy: string(plan.EgressPolicy),
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusCreating,
		Agents:       []RoutedFlockAgent{},
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if strings.TrimSpace(record.Task) == "" {
		return nil, fmt.Errorf("task must be non-empty")
	}
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		r.recordRoutedFlockMetric(FlockPlacementOutcomeCrossHostRegistryError, FlockPlacementReasonPlacementSaveFailed, map[string]time.Duration{
			FlockPlacementPhasePlan:         planLatency,
			FlockPlacementPhaseRegistrySave: time.Since(now),
			FlockPlacementPhaseTotal:        time.Since(totalStart),
		})
		return nil, fmt.Errorf("save routed flock registry: %w", err)
	}

	spawnStart := time.Now()
	for _, agentPlan := range plan.Agents {
		daemon, ok := r.daemons[agentPlan.Host.Name]
		if !ok || daemon == nil {
			err := fmt.Errorf("runtime host %q has no daemon client", agentPlan.Host.Name)
			return nil, r.rollbackRoutedFlockCreate(ctx, record, nil, err, totalStart, planLatency, time.Since(spawnStart))
		}
		resp, err := daemon.SpawnVM(ctx, SpawnVMRequest{
			Profile:      agentPlan.Role,
			TenantID:     plan.TenantID,
			EgressPolicy: string(plan.EgressPolicy),
		})
		if err != nil {
			return nil, r.rollbackRoutedFlockCreate(ctx, record, nil, err, totalStart, planLatency, time.Since(spawnStart))
		}
		if resp == nil || strings.TrimSpace(resp.VMID) == "" {
			return nil, r.rollbackRoutedFlockCreate(ctx, record, nil, fmt.Errorf("runtime daemon SpawnVM returned empty vm_id"), totalStart, planLatency, time.Since(spawnStart))
		}
		record.Agents = append(record.Agents, RoutedFlockAgent{
			AgentID:  agentPlan.AgentID,
			Role:     agentPlan.Role,
			VMID:     strings.TrimSpace(resp.VMID),
			AgentURL: strings.TrimSpace(resp.AgentURL),
			Host:     agentPlan.Host.Name,
			Status:   "running",
		})
		record.UpdatedAt = time.Now().UTC()
		if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
			return nil, r.rollbackRoutedFlockCreate(ctx, record, nil, err, totalStart, planLatency, time.Since(spawnStart))
		}
	}

	record.Status = RoutedFlockStatusReady
	record.UpdatedAt = time.Now().UTC()
	registryStart := time.Now()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		return nil, r.rollbackRoutedFlockCreate(ctx, record, nil, err, totalStart, planLatency, time.Since(spawnStart))
	}
	r.recordRoutedFlockMetric(FlockPlacementOutcomeCrossHostSuccess, FlockPlacementReasonScheduled, map[string]time.Duration{
		FlockPlacementPhasePlan:         planLatency,
		FlockPlacementPhaseAgentSpawn:   time.Since(spawnStart),
		FlockPlacementPhaseRegistrySave: time.Since(registryStart),
		FlockPlacementPhaseTotal:        time.Since(totalStart),
	})
	return routedFlockCreateOutput(record), nil
}

func routedFlockCreateOutput(record RoutedFlockRecord) *RoutedFlockCreateOutput {
	record = cloneRoutedFlockRecord(record)
	return &RoutedFlockCreateOutput{
		FlockID:         record.FlockID,
		Task:            record.Task,
		TenantID:        record.TenantID,
		EgressPolicy:    record.EgressPolicy,
		Mode:            record.Mode,
		Status:          record.Status,
		TownWallEnabled: false,
		Agents:          record.Agents,
	}
}

func (r *RuntimeRouter) recordRoutedFlockMetric(outcome, reason string, latencies map[string]time.Duration) {
	r.recordFlockPlacementMetric(FlockPlacementMetricObservation{
		Outcome:   outcome,
		Reason:    reason,
		Latencies: latencies,
	})
}
```

Keep `rollbackRoutedFlockCreate` as a stub for this task:

```go
func (r *RuntimeRouter) rollbackRoutedFlockCreate(ctx context.Context, record RoutedFlockRecord, removeVMIDs []string, cause error, totalStart time.Time, planLatency time.Duration, spawnLatency time.Duration) error {
	return cause
}
```

- [ ] **Step 4: Run the success test and commit**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterCreateRoutedFlockMembersSpawnsAcrossHosts|TestPlacementStoreRoutedFlock' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/anvilmcp/routed_flock.go internal/anvilmcp/flock_placement_metrics.go internal/anvilmcp/runtime_router_test.go
git commit -m "feat: create routed flock members across hosts"
```

---

### Task 3: Add Rollback, Delete, Unsupported Town Wall, And Metrics

**Files:**
- Modify: `internal/anvilmcp/routed_flock.go`
- Modify: `internal/anvilmcp/flock_placement_metrics.go`
- Modify: `internal/anvilmcp/runtime_router_test.go`

- [ ] **Step 1: Write rollback, delete, and unsupported tests**

Append these focused tests to `internal/anvilmcp/runtime_router_test.go`:

```go
func TestRuntimeRouterCreateRoutedFlockMembersDeniedBeforeDaemonCall(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	daemon := &routerFakeDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}},
			map[string]TenantUsage{"tenant-1": {ActiveVMs: 0}},
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task: "review", Roles: []string{"worker", "reviewer"}, TenantID: "tenant-1", EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want quota denial")
	}
	if daemon.spawnCalls != 0 {
		t.Fatalf("spawn calls = %d, want 0", daemon.spawnCalls)
	}
	requireFlockPlacementMetricAttempt(t, store.State().FlockPlacementMetrics, FlockPlacementOutcomeCrossHostDenied, FlockPlacementReasonQuotaExceeded, 1)
}

func TestRuntimeRouterCreateRoutedFlockMembersRollsBackOnSecondSpawnFailure(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	hostA := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{VMID: "vm-worker", AgentURL: "http://10.0.0.2:3000", AgentToken: "secret-token"}}}
	hostB := &routerFakeDaemon{spawnErr: errors.New("daemon failed: agent_token raw body")}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a.internal", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b.internal", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task: "review", Roles: []string{"worker", "reviewer"}, TenantID: "tenant-1", EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want spawn failure")
	}
	if hostA.deleteCalls != 1 || hostA.deleteVMID != "vm-worker" {
		t.Fatalf("rollback delete = %d/%q, want 1/vm-worker", hostA.deleteCalls, hostA.deleteVMID)
	}
	if _, ok := store.VMHost("vm-worker"); ok {
		t.Fatal("vm-worker placement still exists after rollback")
	}
	for _, forbidden := range []string{"agent_token", "secret-token", "host-a.internal", "host-b.internal"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error leaks %q: %q", forbidden, err.Error())
		}
	}
	requireFlockPlacementMetricAttempt(t, store.State().FlockPlacementMetrics, FlockPlacementOutcomeCrossHostSpawnError, FlockPlacementReasonDaemonCreateFailed, 1)
}

func TestRuntimeRouterCreateRoutedFlockMembersLeavesCleanupPendingOnRollbackDeleteFailure(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	hostA := &routerFakeDaemon{
		spawnResponses: []*SpawnVMResponse{{VMID: "vm-worker", AgentURL: "http://10.0.0.2:3000"}},
		deleteErr:      errors.New("delete failed"),
	}
	hostB := &routerFakeDaemon{spawnErr: errors.New("spawn failed")}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task: "review", Roles: []string{"worker", "reviewer"}, TenantID: "tenant-1", EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want cleanup pending")
	}
	var pending RoutedFlockRecord
	for _, record := range store.ListRoutedFlocks() {
		pending = record
	}
	if pending.Status != RoutedFlockStatusFailedCleanupPending {
		t.Fatalf("registry status = %q, want failed_cleanup_pending", pending.Status)
	}
	if host, ok := store.VMHost("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("vm-worker placement = %q,%v want host-a,true", host, ok)
	}
	requireFlockPlacementMetricAttempt(t, store.State().FlockPlacementMetrics, FlockPlacementOutcomeCrossHostRollbackError, FlockPlacementReasonDaemonCreateFailed, 1)
}

func TestRuntimeRouterDeleteRoutedFlockDeletesMemberVMs(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	record := RoutedFlockRecord{
		FlockID: "routed-flock-delete", Task: "review", Mode: RoutedFlockModeCrossHostMembersOnly, Status: RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-worker", Host: "host-a", Status: "running"},
			{AgentID: "reviewer-1", Role: "reviewer", VMID: "vm-reviewer", Host: "host-b", Status: "running"},
		},
	}
	if err := store.SaveRoutedFlockAndPlacements(record, nil); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	hostA := &routerFakeDaemon{}
	hostB := &routerFakeDaemon{}
	router := NewRuntimeRouterWithOptions(NewScheduler(nil, nil, nil), map[string]Daemon{"host-a": hostA, "host-b": hostB}, RuntimeRouterOptions{PlacementStore: store})

	resp, err := router.DeleteRoutedFlock(context.Background(), "routed-flock-delete")
	if err != nil {
		t.Fatalf("DeleteRoutedFlock returned error: %v", err)
	}
	if resp.StatusCode != 200 || !strings.Contains(resp.Body, `"status":"deleted"`) {
		t.Fatalf("delete response = %+v, want deleted", resp)
	}
	if hostA.deleteCalls != 1 || hostB.deleteCalls != 1 {
		t.Fatalf("delete calls hostA/hostB = %d/%d, want 1/1", hostA.deleteCalls, hostB.deleteCalls)
	}
	if _, ok := store.VMHost("vm-worker"); ok {
		t.Fatal("vm-worker placement still exists")
	}
	updated, _ := store.RoutedFlock("routed-flock-delete")
	if updated.Status != RoutedFlockStatusDeleted {
		t.Fatalf("record status = %q, want deleted", updated.Status)
	}
}

func TestRuntimeRouterRoutedFlockTownWallUnsupported(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	if err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{FlockID: "routed-flock-wall", Mode: RoutedFlockModeCrossHostMembersOnly, Status: RoutedFlockStatusReady}, nil); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	router := NewRuntimeRouterWithOptions(NewScheduler(nil, nil, nil), nil, RuntimeRouterOptions{PlacementStore: store})
	if _, err := router.PostRoutedTownWall(context.Background(), "routed-flock-wall", TownWallPostRequest{AgentID: "worker-1", Body: "hello"}); err == nil {
		t.Fatal("PostRoutedTownWall error = nil, want unsupported")
	} else if !strings.Contains(err.Error(), "Town Wall is not supported for routed members-only flock") {
		t.Fatalf("PostRoutedTownWall error = %q", err.Error())
	}
	if _, err := router.RoutedTownWallHistory(context.Background(), "routed-flock-wall"); err == nil {
		t.Fatal("RoutedTownWallHistory error = nil, want unsupported")
	}
}
```

- [ ] **Step 2: Add metrics normalizer support**

In `internal/anvilmcp/flock_placement_metrics.go`, extend `normalizeFlockPlacementOutcome` and `isAllowedFlockPlacementPhase` to accept the cross-host constants introduced in Task 2. Keep existing outcome and phase values unchanged.

- [ ] **Step 3: Implement rollback, delete, list/get, and unsupported methods**

Replace the rollback stub in `internal/anvilmcp/routed_flock.go` and add these methods:

```go
func (r *RuntimeRouter) rollbackRoutedFlockCreate(ctx context.Context, record RoutedFlockRecord, removeVMIDs []string, cause error, totalStart time.Time, planLatency time.Duration, spawnLatency time.Duration) error {
	rollbackStart := time.Now()
	var cleanupFailed bool
	cleaned := make([]string, 0, len(record.Agents))
	pending := make([]RoutedFlockAgent, 0, len(record.Agents))
	for _, agent := range record.Agents {
		daemon := r.daemons[agent.Host]
		if daemon == nil {
			cleanupFailed = true
			agent.Status = "cleanup_pending"
			pending = append(pending, agent)
			continue
		}
		if _, err := daemon.Delete(ctx, agent.VMID); err != nil {
			cleanupFailed = true
			agent.Status = "cleanup_pending"
			pending = append(pending, agent)
			continue
		}
		cleaned = append(cleaned, agent.VMID)
	}
	if cleanupFailed {
		record.Status = RoutedFlockStatusFailedCleanupPending
		record.Agents = pending
		record.UpdatedAt = time.Now().UTC()
		_ = r.placementStore.SaveRoutedFlockAndPlacements(record, cleaned)
		r.recordRoutedFlockMetric(FlockPlacementOutcomeCrossHostRollbackError, FlockPlacementReasonDaemonCreateFailed, map[string]time.Duration{
			FlockPlacementPhasePlan:       planLatency,
			FlockPlacementPhaseAgentSpawn: spawnLatency,
			FlockPlacementPhaseRollback:   time.Since(rollbackStart),
			FlockPlacementPhaseTotal:      time.Since(totalStart),
		})
		return fmt.Errorf("routed flock create failed and cleanup is pending: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(cause))
	}
	record.Status = RoutedFlockStatusDeleted
	record.Agents = nil
	record.UpdatedAt = time.Now().UTC()
	_ = r.placementStore.SaveRoutedFlockAndPlacements(record, cleaned)
	r.recordRoutedFlockMetric(FlockPlacementOutcomeCrossHostSpawnError, FlockPlacementReasonDaemonCreateFailed, map[string]time.Duration{
		FlockPlacementPhasePlan:       planLatency,
		FlockPlacementPhaseAgentSpawn: spawnLatency,
		FlockPlacementPhaseRollback:   time.Since(rollbackStart),
		FlockPlacementPhaseTotal:      time.Since(totalStart),
	})
	return fmt.Errorf("routed flock create failed: flock_id=%q reason=%s", record.FlockID, sanitizeRoutedFlockErrorReason(cause))
}

func (r *RuntimeRouter) DeleteRoutedFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error) {
	record, ok := r.GetRoutedFlock(flockID)
	if !ok {
		return nil, fmt.Errorf("routed flock %q not found", strings.TrimSpace(flockID))
	}
	record.Status = RoutedFlockStatusDeleting
	record.UpdatedAt = time.Now().UTC()
	_ = r.placementStore.SaveRoutedFlockAndPlacements(record, nil)

	cleaned := make([]string, 0, len(record.Agents))
	pending := make([]RoutedFlockAgent, 0, len(record.Agents))
	for _, agent := range record.Agents {
		daemon := r.daemons[agent.Host]
		if daemon == nil {
			agent.Status = "cleanup_pending"
			pending = append(pending, agent)
			continue
		}
		if _, err := daemon.Delete(ctx, agent.VMID); err != nil {
			agent.Status = "cleanup_pending"
			pending = append(pending, agent)
			continue
		}
		cleaned = append(cleaned, agent.VMID)
	}
	if len(pending) > 0 {
		record.Status = RoutedFlockStatusFailedCleanupPending
		record.Agents = pending
		record.UpdatedAt = time.Now().UTC()
		_ = r.placementStore.SaveRoutedFlockAndPlacements(record, cleaned)
		return nil, fmt.Errorf("routed flock cleanup pending: flock_id=%q failed=%d", record.FlockID, len(pending))
	}
	record.Status = RoutedFlockStatusDeleted
	record.Agents = nil
	record.UpdatedAt = time.Now().UTC()
	if err := r.placementStore.SaveRoutedFlockAndPlacements(record, cleaned); err != nil {
		return nil, fmt.Errorf("save routed flock delete state: %w", err)
	}
	return &RawDaemonResponse{StatusCode: 200, Body: fmt.Sprintf(`{"status":"deleted","flock_id":%q}`, record.FlockID)}, nil
}

func (r *RuntimeRouter) IsRoutedFlock(flockID string) bool {
	_, ok := r.GetRoutedFlock(flockID)
	return ok
}

func (r *RuntimeRouter) GetRoutedFlock(flockID string) (RoutedFlockRecord, bool) {
	if r == nil || r.placementStore == nil {
		return RoutedFlockRecord{}, false
	}
	record, ok := r.placementStore.RoutedFlock(flockID)
	return record, ok
}

func (r *RuntimeRouter) ListRoutedFlocks() []RoutedFlockRecord {
	if r == nil || r.placementStore == nil {
		return nil
	}
	return r.placementStore.ListRoutedFlocks()
}

func (r *RuntimeRouter) PostRoutedTownWall(context.Context, string, TownWallPostRequest) (*TownWallMessage, error) {
	return nil, fmt.Errorf("Town Wall is not supported for routed members-only flock")
}

func (r *RuntimeRouter) RoutedTownWallHistory(context.Context, string) ([]TownWallMessage, error) {
	return nil, fmt.Errorf("Town Wall is not supported for routed members-only flock")
}

func sanitizeRoutedFlockErrorReason(err error) string {
	if err == nil {
		return FlockPlacementReasonUnknown
	}
	return FlockPlacementReasonDaemonCreateFailed
}
```

- [ ] **Step 4: Run routed router tests and commit**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterCreateRoutedFlockMembers|TestRuntimeRouterDeleteRoutedFlock|TestRuntimeRouterRoutedFlockTownWall' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/anvilmcp/routed_flock.go internal/anvilmcp/flock_placement_metrics.go internal/anvilmcp/runtime_router_test.go
git commit -m "feat: rollback and delete routed flocks"
```

---

### Task 4: Route Existing Flock Operations Through ReplicatingDaemon

**Files:**
- Modify: `internal/anvilmcp/replicating_daemon.go`
- Modify: `internal/anvilmcp/replicating_daemon_test.go`

- [ ] **Step 1: Write routed operation routing tests**

Add `fmt` to the imports in `internal/anvilmcp/replicating_daemon_test.go`.

Append tests to `internal/anvilmcp/replicating_daemon_test.go`:

```go
func TestReplicatingDaemonDeleteFlockRoutesRoutedFlock(t *testing.T) {
	base := &replicatingDaemonBaseFake{}
	router := &replicatingDaemonRoutedFake{
		routed: map[string]bool{"routed-flock-1": true},
		deleteResp: &RawDaemonResponse{StatusCode: 200, Body: `{"status":"deleted"}`},
	}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	resp, err := daemon.DeleteFlock(context.Background(), "routed-flock-1")
	if err != nil {
		t.Fatalf("DeleteFlock returned error: %v", err)
	}
	if resp.StatusCode != 200 || router.deleteCalls != 1 {
		t.Fatalf("resp/delete calls = %+v/%d, want routed delete", resp, router.deleteCalls)
	}
	if base.deleteFlockCalls != 0 {
		t.Fatalf("base delete calls = %d, want 0", base.deleteFlockCalls)
	}
}

func TestReplicatingDaemonTownWallRejectsRoutedFlockWithoutBaseFallback(t *testing.T) {
	base := &replicatingDaemonBaseFake{}
	router := &replicatingDaemonRoutedFake{routed: map[string]bool{"routed-flock-1": true}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	if _, err := daemon.PostTownWall(context.Background(), "routed-flock-1", TownWallPostRequest{AgentID: "worker-1", Body: "hello"}); err == nil {
		t.Fatal("PostTownWall error = nil, want unsupported")
	}
	if base.postTownWallCalls != 0 {
		t.Fatalf("base post calls = %d, want 0", base.postTownWallCalls)
	}
	if _, err := daemon.TownWallHistory(context.Background(), "routed-flock-1"); err == nil {
		t.Fatal("TownWallHistory error = nil, want unsupported")
	}
	if base.townWallHistoryCalls != 0 {
		t.Fatalf("base history calls = %d, want 0", base.townWallHistoryCalls)
	}
}

func TestReplicatingDaemonListFlocksIncludesRoutedRecords(t *testing.T) {
	base := &replicatingDaemonBaseFake{listFlockResp: []FlockInfo{{FlockID: "base-flock"}}}
	router := &replicatingDaemonRoutedFake{records: []RoutedFlockRecord{{
		FlockID: "routed-flock-1",
		Task:    "review",
		Status:  RoutedFlockStatusReady,
		Agents:  []RoutedFlockAgent{{AgentID: "worker-1", Role: "worker", VMID: "vm-worker", AgentURL: "http://10.0.0.2:3000", Status: "running"}},
	}}}
	daemon := NewReplicatingDaemonWithOptions(base, nil, ReplicatingDaemonOptions{RoutedFlocks: router})

	flocks, err := daemon.ListFlocks(context.Background())
	if err != nil {
		t.Fatalf("ListFlocks returned error: %v", err)
	}
	if len(flocks) != 2 {
		t.Fatalf("flocks = %+v, want base+routed", flocks)
	}
	if flocks[1].FlockID != "routed-flock-1" || flocks[1].Agents["worker-1"].VMID != "vm-worker" {
		t.Fatalf("routed flock info = %+v", flocks[1])
	}
}
```

Extend fakes with the fields/methods needed by the tests:

```go
// Add these fields to replicatingDaemonBaseFake.
listFlockResp        []FlockInfo
deleteFlockCalls     int
deleteFlockID        string
postTownWallCalls    int
townWallHistoryCalls int

// Replace these existing methods so the counters and configured list response work.
func (f *replicatingDaemonBaseFake) ListFlocks(context.Context) ([]FlockInfo, error) {
	if f.listFlockResp != nil {
		return append([]FlockInfo(nil), f.listFlockResp...), nil
	}
	return []FlockInfo{}, nil
}

func (f *replicatingDaemonBaseFake) DeleteFlock(_ context.Context, flockID string) (*RawDaemonResponse, error) {
	f.deleteFlockCalls++
	f.deleteFlockID = flockID
	return &RawDaemonResponse{}, nil
}

func (f *replicatingDaemonBaseFake) PostTownWall(context.Context, string, TownWallPostRequest) (*TownWallMessage, error) {
	f.postTownWallCalls++
	return &TownWallMessage{}, nil
}

func (f *replicatingDaemonBaseFake) TownWallHistory(context.Context, string) ([]TownWallMessage, error) {
	f.townWallHistoryCalls++
	return []TownWallMessage{}, nil
}

type replicatingDaemonRoutedFake struct {
	routed      map[string]bool
	records     []RoutedFlockRecord
	deleteCalls int
	deleteResp  *RawDaemonResponse
}

func (f *replicatingDaemonRoutedFake) CreateRoutedFlockMembers(context.Context, FlockCreateRequest) (*RoutedFlockCreateOutput, error) {
	return nil, fmt.Errorf("not used")
}

func (f *replicatingDaemonRoutedFake) IsRoutedFlock(flockID string) bool {
	if f.routed != nil {
		return f.routed[flockID]
	}
	for _, record := range f.records {
		if record.FlockID == flockID {
			return true
		}
	}
	return false
}

func (f *replicatingDaemonRoutedFake) DeleteRoutedFlock(context.Context, string) (*RawDaemonResponse, error) {
	f.deleteCalls++
	if f.deleteResp != nil {
		return f.deleteResp, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *replicatingDaemonRoutedFake) GetRoutedFlock(flockID string) (RoutedFlockRecord, bool) {
	for _, record := range f.records {
		if record.FlockID == flockID {
			return record, true
		}
	}
	return RoutedFlockRecord{}, false
}

func (f *replicatingDaemonRoutedFake) ListRoutedFlocks() []RoutedFlockRecord {
	return append([]RoutedFlockRecord(nil), f.records...)
}
```

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
go test ./internal/anvilmcp -run 'TestReplicatingDaemon.*Routed|TestReplicatingDaemonListFlocksIncludesRoutedRecords' -count=1
```

Expected: FAIL with undefined `NewReplicatingDaemonWithOptions`, `ReplicatingDaemonOptions`, or missing fake fields.

- [ ] **Step 3: Implement routed controller routing**

In `internal/anvilmcp/replicating_daemon.go`, add:

```go
type routedFlockController interface {
	CreateRoutedFlockMembers(context.Context, FlockCreateRequest) (*RoutedFlockCreateOutput, error)
	IsRoutedFlock(string) bool
	DeleteRoutedFlock(context.Context, string) (*RawDaemonResponse, error)
	GetRoutedFlock(string) (RoutedFlockRecord, bool)
	ListRoutedFlocks() []RoutedFlockRecord
}

type ReplicatingDaemonOptions struct {
	RoutedFlocks routedFlockController
}
```

Extend `ReplicatingDaemon`:

```go
routedFlocks routedFlockController
```

Replace `NewReplicatingDaemon` with a wrapper:

```go
func NewReplicatingDaemon(base Daemon, replicator snapshotReplicator) *ReplicatingDaemon {
	return NewReplicatingDaemonWithOptions(base, replicator, ReplicatingDaemonOptions{})
}

func NewReplicatingDaemonWithOptions(base Daemon, replicator snapshotReplicator, opts ReplicatingDaemonOptions) *ReplicatingDaemon {
	daemon := &ReplicatingDaemon{
		Daemon:       base,
		replicator:   replicator,
		routedFlocks: opts.RoutedFlocks,
	}
	if router, ok := replicator.(flockCreator); ok {
		daemon.flockRouter = router
	}
	return daemon
}
```

Add routed methods:

```go
func (d *ReplicatingDaemon) CreateRoutedFlockMembers(ctx context.Context, req FlockCreateRequest) (*RoutedFlockCreateOutput, error) {
	if d == nil || d.routedFlocks == nil {
		return nil, fmt.Errorf("routed flock members create is disabled; set ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only with persistent scheduler state")
	}
	return d.routedFlocks.CreateRoutedFlockMembers(ctx, req)
}

func (d *ReplicatingDaemon) DeleteFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error) {
	if d != nil && d.routedFlocks != nil && d.routedFlocks.IsRoutedFlock(flockID) {
		return d.routedFlocks.DeleteRoutedFlock(ctx, flockID)
	}
	return d.Daemon.DeleteFlock(ctx, flockID)
}

func (d *ReplicatingDaemon) GetFlock(ctx context.Context, flockID string) (*FlockInfo, error) {
	if d != nil && d.routedFlocks != nil {
		if record, ok := d.routedFlocks.GetRoutedFlock(flockID); ok {
			info := flockInfoFromRoutedRecord(record)
			return &info, nil
		}
	}
	return d.Daemon.GetFlock(ctx, flockID)
}

func (d *ReplicatingDaemon) ListFlocks(ctx context.Context) ([]FlockInfo, error) {
	base, err := d.Daemon.ListFlocks(ctx)
	if err != nil {
		return nil, err
	}
	if d == nil || d.routedFlocks == nil {
		return base, nil
	}
	for _, record := range d.routedFlocks.ListRoutedFlocks() {
		base = append(base, flockInfoFromRoutedRecord(record))
	}
	return base, nil
}

func (d *ReplicatingDaemon) PostTownWall(ctx context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error) {
	if d != nil && d.routedFlocks != nil && d.routedFlocks.IsRoutedFlock(flockID) {
		return nil, fmt.Errorf("Town Wall is not supported for routed members-only flock %q", flockID)
	}
	return d.Daemon.PostTownWall(ctx, flockID, req)
}

func (d *ReplicatingDaemon) TownWallHistory(ctx context.Context, flockID string) ([]TownWallMessage, error) {
	if d != nil && d.routedFlocks != nil && d.routedFlocks.IsRoutedFlock(flockID) {
		return nil, fmt.Errorf("Town Wall is not supported for routed members-only flock %q", flockID)
	}
	return d.Daemon.TownWallHistory(ctx, flockID)
}

func flockInfoFromRoutedRecord(record RoutedFlockRecord) FlockInfo {
	agents := make(map[string]FlockAgentInfo, len(record.Agents))
	for _, agent := range record.Agents {
		agents[agent.AgentID] = FlockAgentInfo{
			AgentID:  agent.AgentID,
			Role:     agent.Role,
			VMID:     agent.VMID,
			AgentURL: agent.AgentURL,
			Status:   agent.Status,
		}
	}
	return FlockInfo{
		FlockID:      record.FlockID,
		Task:         record.Task,
		TenantID:     record.TenantID,
		EgressPolicy: record.EgressPolicy,
		Agents:       agents,
		CreatedAt:    record.CreatedAt,
	}
}
```

- [ ] **Step 4: Run routed daemon tests and commit**

Run:

```bash
go test ./internal/anvilmcp -run 'TestReplicatingDaemon' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/anvilmcp/replicating_daemon.go internal/anvilmcp/replicating_daemon_test.go
git commit -m "feat: route routed flock lifecycle operations"
```

---

### Task 5: Add MCP Tool, Config Flag, And anvil-mcp Wiring

**Files:**
- Modify: `internal/anvilmcp/config.go`
- Modify: `internal/anvilmcp/config_test.go`
- Modify: `internal/anvilmcp/tools.go`
- Modify: `internal/anvilmcp/tools_test.go`
- Modify: `internal/anvilmcp/ironclaw_schema.go`
- Modify: `internal/anvilmcp/ironclaw_schema_test.go`
- Modify: `cmd/anvil-mcp/main.go`
- Modify: `cmd/anvil-mcp/main_test.go`
- Modify: `configs/anvil-mcp.yaml.example`

- [ ] **Step 1: Write config and tool tests**

In `internal/anvilmcp/config_test.go`, extend file/env tests to include `cross_host_flock_create_mode: members_only` and `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE: members_only`, then assert:

```go
if cfg.CrossHostFlockCreateMode != "members_only" {
	t.Errorf("CrossHostFlockCreateMode = %q, want members_only", cfg.CrossHostFlockCreateMode)
}
```

Add a rejection test:

```go
func TestLoadConfigRejectsInvalidCrossHostFlockCreateMode(t *testing.T) {
	env := map[string]string{"ANVIL_MCP_CROSS_HOST_FLOCK_CREATE": "full"}
	_, err := LoadConfig(testConfigSource(env, nil))
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want invalid cross-host flock mode")
	}
	if !strings.Contains(err.Error(), "ANVIL_MCP_CROSS_HOST_FLOCK_CREATE") {
		t.Fatalf("LoadConfig() error = %q, want env var name", err)
	}
}
```

In `internal/anvilmcp/tools_test.go`, add:

```go
func TestToolsCreateRoutedFlockMembersRequiresRoutedDaemon(t *testing.T) {
	tools := newTools(&fakeDaemon{}, NewSessionStore(), time.Second)
	_, err := tools.CreateRoutedFlockMembers(context.Background(), SpawnFlockInput{
		Task: "review", Roles: []string{"worker"}, TenantID: "tenant-1", EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want disabled")
	}
	if !strings.Contains(err.Error(), "routed flock members create is disabled") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestToolsCreateRoutedFlockMembersReturnsMembersOnlyOutput(t *testing.T) {
	daemon := &fakeRoutedFlockDaemon{
		output: &RoutedFlockCreateOutput{
			FlockID: "routed-flock-1", Task: "review", TenantID: "tenant-1", EgressPolicy: "profile",
			Mode: RoutedFlockModeCrossHostMembersOnly, Status: RoutedFlockStatusReady, TownWallEnabled: false,
			Agents: []RoutedFlockAgent{{AgentID: "worker-1", Role: "worker", VMID: "vm-worker", AgentURL: "http://10.0.0.2:3000", Host: "host-a", Status: "running"}},
		},
	}
	tools := newTools(daemon, NewSessionStore(), time.Second)
	out, err := tools.CreateRoutedFlockMembers(context.Background(), SpawnFlockInput{
		Task: " review ", Roles: []string{" worker "}, TenantID: " tenant-1 ", EgressPolicy: " PROFILE ",
	})
	if err != nil {
		t.Fatalf("CreateRoutedFlockMembers returned error: %v", err)
	}
	if out.TownWallEnabled || out.Mode != RoutedFlockModeCrossHostMembersOnly {
		t.Fatalf("output = %+v, want members-only without Town Wall", out)
	}
	if daemon.routedReq.Task != "review" || daemon.routedReq.Roles[0] != "worker" || daemon.routedReq.EgressPolicy != "profile" {
		t.Fatalf("routed request = %+v, want normalized input", daemon.routedReq)
	}
	data, _ := json.Marshal(out)
	if strings.Contains(string(data), "townwall_url") || strings.Contains(string(data), "post_url") || strings.Contains(string(data), "agent_token") {
		t.Fatalf("routed output has forbidden fields: %s", data)
	}
}
```

Add fake:

```go
type fakeRoutedFlockDaemon struct {
	fakeDaemon
	routedReq FlockCreateRequest
	output    *RoutedFlockCreateOutput
	err       error
}

func (f *fakeRoutedFlockDaemon) CreateRoutedFlockMembers(_ context.Context, req FlockCreateRequest) (*RoutedFlockCreateOutput, error) {
	f.routedReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.output, nil
}
```

In `cmd/anvil-mcp/main_test.go`, update the registration `want` map with:

```go
"anvil_create_routed_flock_members": "Create an experimental members-only routed flock by spawning role VMs across scheduler runtime hosts without Town Wall.",
```

Add wiring test:

```go
func TestNewMCPDaemonRequiresPersistentStateForMembersOnlyRoutedFlocks(t *testing.T) {
	_, err := newMCPDaemon(anvilmcp.Config{
		DaemonURL:                 "http://127.0.0.1:3000",
		CrossHostFlockCreateMode:  "members_only",
		SchedulerHostsFile:        filepath.Join(t.TempDir(), "hosts.json"),
	}, http.DefaultClient)
	if err == nil {
		t.Fatal("newMCPDaemon error = nil, want persistent scheduler state requirement")
	}
	if !strings.Contains(err.Error(), "scheduler_state_path") {
		t.Fatalf("newMCPDaemon error = %q, want scheduler_state_path", err)
	}
}
```

Add `strings` to the imports in `cmd/anvil-mcp/main_test.go`.

- [ ] **Step 2: Run tests and verify failures**

Run:

```bash
go test ./internal/anvilmcp -run 'TestLoadConfig.*CrossHost|TestToolsCreateRoutedFlockMembers|TestCurrentIronClawSchemasIncludeGoosetownTools|TestCurrentAnvilToolInputsAreGeminiCompatible' -count=1
go test ./cmd/anvil-mcp -run 'TestToolRegistrations|TestNewMCPDaemonRequiresPersistentStateForMembersOnlyRoutedFlocks' -count=1
```

Expected: FAIL with missing config field, missing tool method, missing registration, and missing schema.

- [ ] **Step 3: Implement config parsing**

In `internal/anvilmcp/config.go`, add:

```go
envCrossHostFlockCreate = "ANVIL_MCP_CROSS_HOST_FLOCK_CREATE"
```

Add field:

```go
CrossHostFlockCreateMode string `yaml:"cross_host_flock_create_mode"`
```

Read env override:

```go
if v := getenv(envCrossHostFlockCreate); v != "" {
	cfg.CrossHostFlockCreateMode = v
}
```

Normalize and validate:

```go
cfg.CrossHostFlockCreateMode = strings.TrimSpace(cfg.CrossHostFlockCreateMode)
if cfg.CrossHostFlockCreateMode != "" && cfg.CrossHostFlockCreateMode != "members_only" {
	label := "cross_host_flock_create_mode"
	if getenv(envCrossHostFlockCreate) != "" {
		label = envCrossHostFlockCreate
	}
	return Config{}, fmt.Errorf("%s must be empty or members_only", label)
}
```

- [ ] **Step 4: Implement MCP tool method and schema**

In `internal/anvilmcp/tools.go`, add optional interface:

```go
type routedFlockMembersCreator interface {
	CreateRoutedFlockMembers(context.Context, FlockCreateRequest) (*RoutedFlockCreateOutput, error)
}
```

Add methods near `SpawnFlock`:

```go
func (t *Tools) CreateRoutedFlockMembers(ctx context.Context, input SpawnFlockInput) (*RoutedFlockCreateOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	egressPolicy, err := NormalizeEgressPolicy(input.EgressPolicy)
	if err != nil {
		return nil, err
	}
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return nil, fmt.Errorf("task must be non-empty")
	}
	roles, err := normalizeFlockRoles(input.Roles)
	if err != nil {
		return nil, err
	}
	creator, ok := t.daemon.(routedFlockMembersCreator)
	if !ok {
		return nil, fmt.Errorf("routed flock members create is disabled; set ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only with persistent scheduler state")
	}
	out, err := creator.CreateRoutedFlockMembers(ctx, FlockCreateRequest{
		Task:         task,
		Roles:        roles,
		TenantID:     tenantID,
		EgressPolicy: string(egressPolicy),
	})
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_create_routed_flock_members", "POST /vms routed flock members", err)
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_create_routed_flock_members", "POST /vms routed flock members"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPCreateRoutedFlockMembers(ctx context.Context, req *mcp.CallToolRequest, input SpawnFlockInput) (*mcp.CallToolResult, RoutedFlockCreateOutput, error) {
	out, err := t.CreateRoutedFlockMembers(ctx, input)
	if err != nil || out == nil {
		return nil, RoutedFlockCreateOutput{}, err
	}
	return nil, *out, nil
}
```

In `internal/anvilmcp/ironclaw_schema.go`, add:

```go
toolInputSchemaFromStruct("anvil_create_routed_flock_members", SpawnFlockInput{}),
```

In `internal/anvilmcp/ironclaw_schema_test.go`, add `"anvil_create_routed_flock_members"` to the Goosetown schema list.

- [ ] **Step 5: Register the tool and wire routed controller**

In `cmd/anvil-mcp/main.go`, add tool registration after `anvil_spawn_flock`:

```go
{
	name:        "anvil_create_routed_flock_members",
	description: "Create an experimental members-only routed flock by spawning role VMs across scheduler runtime hosts without Town Wall.",
	register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
		mcp.AddTool(server, tool, tools.MCPCreateRoutedFlockMembers)
	},
},
```

In `newMCPDaemon`, reject invalid members-only config before constructing the router:

```go
if cfg.CrossHostFlockCreateMode == "members_only" && strings.TrimSpace(cfg.SchedulerStatePath) == "" {
	return nil, fmt.Errorf("scheduler_state_path or ANVIL_MCP_SCHEDULER_STATE is required when ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only")
}
```

Replace the final wrapper:

```go
opts := anvilmcp.ReplicatingDaemonOptions{}
if cfg.CrossHostFlockCreateMode == "members_only" {
	opts.RoutedFlocks = router
}
return anvilmcp.NewReplicatingDaemonWithOptions(base, router, opts), nil
```

Add `strings` import to `cmd/anvil-mcp/main.go` if needed.

In `configs/anvil-mcp.yaml.example`, document:

```yaml
# cross_host_flock_create_mode: members_only
```

- [ ] **Step 6: Run tool/config tests and commit**

Run:

```bash
go test ./internal/anvilmcp -run 'TestLoadConfig|TestToolsCreateRoutedFlockMembers|TestCurrentAnvilToolInputsAreGeminiCompatible|TestCurrentIronClawSchemasIncludeGoosetownTools|TestSpawnFlockRolesSchemaDescribesStringItems' -count=1
go test ./cmd/anvil-mcp -run 'TestToolRegistrations|TestNewMCPDaemon' -count=1
```

Expected: PASS.

Commit:

```bash
git add internal/anvilmcp/config.go internal/anvilmcp/config_test.go internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go internal/anvilmcp/ironclaw_schema.go internal/anvilmcp/ironclaw_schema_test.go cmd/anvil-mcp/main.go cmd/anvil-mcp/main_test.go configs/anvil-mcp.yaml.example
git commit -m "feat: expose routed flock members MCP tool"
```

---

### Task 6: Documentation And Operations Handoff

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture/mcp-architecture.md`
- Modify: `docs/architecture/multi-tenant-roadmap.md`
- Modify: `docs/operations/observability.md`
- Modify: `docs/operations/release-checklist.md`
- Modify: `RELEASE_NOTES.md`
- Create: `docs/operations/2026-06-06-cross-host-flock-create-minimal-handoff.md`

- [ ] **Step 1: Update README runtime docs**

In `README.md`, near the scheduler-aware `anvil_spawn_flock` section, replace the future-only text with:

```markdown
`ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`를 함께 설정하면 experimental
`anvil_create_routed_flock_members` tool을 사용할 수 있다. 이 tool은
`POST /schedule/flock` plan에 따라 role VM을 여러 runtime host의 daemon `POST /vms`로
생성하고, `scheduler_state_path`의 routed flock registry에 member VM placement를
기록한다. output은 `mode=cross_host_members_only`, `town_wall_enabled=false`를
반환하며 `townwall_url`/`post_url`을 반환하지 않는다. 이 first slice는 Town Wall,
cross-host `gtcall`, guest flock context 주입을 제공하지 않는다.
```

Add the new env var to the config list:

```markdown
- `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`: persistent scheduler state가 있을 때
  members-only routed flock create tool을 활성화한다.
```

- [ ] **Step 2: Update architecture docs**

In `docs/architecture/mcp-architecture.md`, update the router mode section:

```markdown
MCP router config와 `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`가 함께 설정되면
`anvil_create_routed_flock_members`는 cross-host members-only create를 수행한다. 이
경로는 daemon `POST /flocks`를 호출하지 않고 host별 daemon `POST /vms`만 호출한다.
routed registry는 delete routing과 inspection을 위한 downstream state이며 daemon
`FlockManager` 또는 Town Wall state가 아니다.
```

In `docs/architecture/multi-tenant-roadmap.md`, move members-only routed create from future to current foundation and keep Town Wall coordinator as future:

```markdown
2026-06-06 members-only routed flock create slice는 quota/capacity 검증 후 role VM을
cross-host로 생성하고 registry/rollback/delete를 검증한다. coordinator Town Wall,
cross-host `gtcall`, guest `/root/.ephemera-flock` 주입은 다음 단계로 남긴다.
```

- [ ] **Step 3: Update observability and release docs**

In `docs/operations/observability.md`, add:

```markdown
members-only routed flock create는 flock placement metrics에 bounded outcome
`cross_host_success`, `cross_host_denied`, `cross_host_spawn_error`,
`cross_host_rollback_error`, `cross_host_registry_error`를 기록한다. phase label은
`plan`, `agent_spawn`, `registry_save`, `rollback`, `total`만 사용한다. tenant ID,
flock ID, VM ID, host name, host endpoint, authorization header, `agent_token`은
metric label이나 state에 기록하지 않는다.
```

In `RELEASE_NOTES.md` Unreleased section, add:

```markdown
- experimental MCP `anvil_create_routed_flock_members` tool을 추가한다. 이 tool은
  `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`와 persistent scheduler state가 있을
  때 role VM을 여러 runtime host에 생성하고 routed registry에 기록한다.
- routed members-only flock delete는 registry의 member VM을 host별 daemon `DELETE /vms`
  로 정리한다. cleanup 일부 실패 시 registry status는 `failed_cleanup_pending`으로 남긴다.
```

In `docs/operations/release-checklist.md`, add the verification commands from Task 7.

- [ ] **Step 4: Add operation handoff**

Create `docs/operations/2026-06-06-cross-host-flock-create-minimal-handoff.md`:

```markdown
# Cross-host Flock Create Minimal Handoff

작성일: 2026-06-06

## 상태

- `anvil_create_routed_flock_members`는 opt-in experimental MCP tool이다.
- 활성화 조건은 `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`,
  `ANVIL_MCP_SCHEDULER_STATE`, runtime host inventory다.
- 기존 `anvil_spawn_flock`은 scheduler-aware single-host create를 유지한다.

## 지원 범위

- 지원: role별 VM cross-host spawn, routed registry, rollback, routed delete,
  unsupported Town Wall error.
- 미지원: Town Wall, `/flocks/{id}/post`, SSE/history, cross-host `gtcall`,
  guest flock context 주입, daemon `FlockManager` registration.

## 운영 주의

- `failed_cleanup_pending` record가 있으면 registry의 VM ID와 host name으로 수동
  cleanup을 확인한다.
- output, registry, audit, metrics에는 `agent_token`, authorization header, host
  endpoint, daemon raw body를 남기지 않는다.
```

- [ ] **Step 5: Run documentation checks and commit**

Run:

```bash
rg -n "anvil_create_routed_flock_members|ANVIL_MCP_CROSS_HOST_FLOCK_CREATE|cross_host_members_only|failed_cleanup_pending|agent_token" README.md docs/architecture/mcp-architecture.md docs/architecture/multi-tenant-roadmap.md docs/operations/observability.md docs/operations/release-checklist.md RELEASE_NOTES.md docs/operations/2026-06-06-cross-host-flock-create-minimal-handoff.md
git diff --check
```

Expected: `rg` prints the new documentation references, and `git diff --check` exits 0.

Commit:

```bash
git add README.md docs/architecture/mcp-architecture.md docs/architecture/multi-tenant-roadmap.md docs/operations/observability.md docs/operations/release-checklist.md RELEASE_NOTES.md docs/operations/2026-06-06-cross-host-flock-create-minimal-handoff.md
git commit -m "docs: document routed flock members create"
```

---

### Task 7: Full Verification And Release Gate

**Files:**
- Verify: whole repository

- [ ] **Step 1: Run focused package tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-mcp -count=1
go test ./cmd/goose-daemon -count=1
```

Expected: all three commands exit 0.

- [ ] **Step 2: Run full test suite**

Run:

```bash
go test ./... -count=1
```

Expected: exit 0.

- [ ] **Step 3: Run builds**

Run:

```bash
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
go build ./cmd/goose-daemon
```

Expected: all builds exit 0.

- [ ] **Step 4: Run static diff check**

Run:

```bash
git diff --check
```

Expected: exit 0.

- [ ] **Step 5: Confirm existing single-host flock smoke remains the default**

Run only when local daemon/MCP smoke prerequisites are present:

```bash
scripts/anvil-mcp-e2e.sh flock
```

Expected: existing `anvil_spawn_flock` smoke still passes and returns Town Wall URLs. This confirms the default single-host path was not replaced by members-only routed create.

- [ ] **Step 6: Commit any verification-only doc updates**

If release docs were updated with actual verification results, commit:

```bash
git add RELEASE_NOTES.md docs/operations/release-checklist.md docs/operations/2026-06-06-cross-host-flock-create-minimal-handoff.md
git commit -m "docs: record routed flock create verification"
```

Skip this commit if no files changed.

---

## Self-Review Checklist

- Spec coverage:
  - `ScheduleFlock` drives role placement: Task 2.
  - role VMs spawn via daemon `SpawnVM`: Task 2.
  - deterministic `agent_id`: Task 2 via existing `ScheduleFlock` plan.
  - persistent routed registry and VM placement: Task 1 and Task 2.
  - rollback on create failure: Task 3.
  - cleanup failure leaves `failed_cleanup_pending`: Task 3.
  - routed delete: Task 3 and Task 4.
  - Town Wall unsupported for routed flock: Task 3 and Task 4.
  - existing daemon `POST /flocks` and `anvil_spawn_flock` default remain unchanged: Task 5 and Task 7.
  - no `agent_token`, auth header, host endpoint, daemon raw body in output/audit/metrics: Task 2, Task 3, Task 5, Task 6.
  - bounded metrics outcomes/phases: Task 3 and Task 6.
- Placeholder scan:
  - 금지 placeholder와 unresolved name을 남기지 않았다.
- Type consistency:
  - `RoutedFlockRecord`, `RoutedFlockAgent`, `RoutedFlockCreateOutput`, `CreateRoutedFlockMembers`, `DeleteRoutedFlock`, and `ReplicatingDaemonOptions.RoutedFlocks` are introduced before later tasks reference them.
  - The MCP tool reuses `SpawnFlockInput` intentionally so input schema stays identical to `anvil_spawn_flock` while output differs.
