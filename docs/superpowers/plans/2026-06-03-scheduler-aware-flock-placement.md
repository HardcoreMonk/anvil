# Scheduler-aware Flock Placement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make MCP `anvil_spawn_flock` use scheduler-aware single-host placement on the v0.3.6 anvil line when scheduler router config is enabled.

**Architecture:** Keep daemon `POST /flocks` unchanged. Extend the MCP/runtime-router layer so a router-backed MCP daemon schedules one host for the whole flock, delegates `CreateFlock` to that host, and records returned member VM placements. This is not true cross-host flock placement; all flock members remain on one selected daemon host.

**Tech Stack:** Go, `internal/anvilmcp`, stdlib tests, existing scheduler/placement store/runtime router, markdown docs.

---

## File Structure

- Modify `internal/anvilmcp/tenant_policy.go`: add requested active VM capacity awareness to `ScheduleRequest`/`SelectRuntimeHost`.
- Modify `internal/anvilmcp/scheduler.go`: pass `requested.ActiveVMs` into host selection.
- Modify `internal/anvilmcp/scheduler_test.go`: prove host capacity rejects a host that cannot fit all requested flock member VMs.
- Modify `internal/anvilmcp/runtime_router.go`: add `CreateFlock` and a placement recording helper for multiple VM IDs.
- Modify `internal/anvilmcp/runtime_router_test.go`: add fake flock fields and router flock placement tests.
- Modify `internal/anvilmcp/replicating_daemon.go`: let the wrapper delegate `CreateFlock` to the router when the replicator supports it.
- Create `internal/anvilmcp/replicating_daemon_test.go`: cover router delegation and base fallback.
- Modify `internal/anvilmcp/tools_test.go`: add an MCP output redaction regression for `SpawnFlock`.
- Modify `README.md`, `docs/architecture/mcp-architecture.md`, `docs/architecture/multi-tenant-roadmap.md`, `RELEASE_NOTES.md`: document the MCP-only scheduler-aware single-host flock placement scope.
- Create `docs/operations/2026-06-03-scheduler-aware-flock-placement-handoff.md`: record verification and residual risk.

---

### Task 1: Make Scheduler Host Capacity Aware Of Requested Active VMs

**Files:**
- Modify: `internal/anvilmcp/tenant_policy.go`
- Modify: `internal/anvilmcp/scheduler.go`
- Test: `internal/anvilmcp/scheduler_test.go`

- [ ] **Step 1: Write the failing scheduler capacity test**

Append this test to `internal/anvilmcp/scheduler_test.go` before `runtimeHostFromJSON`:

```go
func TestSchedulerRequiresEnoughAvailableVMsForRequestedActiveVMs(t *testing.T) {
	scheduler := NewScheduler(
		[]RuntimeHost{
			{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 3, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
		},
		nil,
		nil,
	)

	decision, err := scheduler.Schedule(ScheduleRequest{
		TenantID:     "tenant-1",
		EgressPolicy: EgressPolicyProfile,
	}, TenantUsage{ActiveVMs: 2})
	if err != nil {
		t.Fatalf("Schedule() error = %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("Schedule() denied: %+v", decision)
	}
	if decision.Host.Name != "host-b" {
		t.Fatalf("selected host = %q, want host-b because host-a has only 1 available VM", decision.Host.Name)
	}
}
```

- [ ] **Step 2: Run the focused test and verify it fails**

Run:

```bash
go test ./internal/anvilmcp -run TestSchedulerRequiresEnoughAvailableVMsForRequestedActiveVMs -count=1
```

Expected: FAIL because the current host selector accepts `host-a` when `AvailableVMs > 0`, ignoring `requested.ActiveVMs`.

- [ ] **Step 3: Add requested active VM capacity to `ScheduleRequest`**

In `internal/anvilmcp/tenant_policy.go`, update `ScheduleRequest`:

```go
type ScheduleRequest struct {
	TenantID               string       `json:"tenant_id"`
	Profile                string       `json:"profile,omitempty"`
	RequestedSnapshotBytes int64        `json:"requested_snapshot_bytes,omitempty"`
	RequestedActiveVMs     int64        `json:"requested_active_vms,omitempty"`
	EgressPolicy           EgressPolicy `json:"egress_policy"`
	PreferredHosts         []string     `json:"preferred_hosts,omitempty"`
	ExcludedHosts          []string     `json:"excluded_hosts,omitempty"`
}
```

- [ ] **Step 4: Pass requested active VMs from scheduler to host selection**

In `internal/anvilmcp/scheduler.go`, just before `host, err := SelectRuntimeHost(s.hosts, req)`, set the additive field:

```go
	req.RequestedActiveVMs = requested.ActiveVMs
	host, err := SelectRuntimeHost(s.hosts, req)
```

- [ ] **Step 5: Enforce the active VM capacity in `SelectRuntimeHost`**

In `internal/anvilmcp/tenant_policy.go`, update `SelectRuntimeHost` after the snapshot byte validation:

```go
	if req.RequestedActiveVMs < 0 {
		return RuntimeHost{}, fmt.Errorf("requested_active_vms must be non-negative")
	}
	requestedActiveVMs := req.RequestedActiveVMs
	if requestedActiveVMs == 0 {
		requestedActiveVMs = 1
	}
```

Then replace this condition inside `eligible`:

```go
		if !host.Healthy || host.AvailableVMs <= 0 {
			return false
		}
```

with:

```go
		if !host.Healthy || host.AvailableVMs < requestedActiveVMs {
			return false
		}
```

- [ ] **Step 6: Run the focused scheduler tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerRequiresEnoughAvailableVMsForRequestedActiveVMs|TestSchedulerSelectsEligibleHost|TestSchedulerPrefersSnapshotLocalityHost|TestSchedulerSkipsExcludedHostsForFailover' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit scheduler capacity semantics**

```bash
git add internal/anvilmcp/tenant_policy.go internal/anvilmcp/scheduler.go internal/anvilmcp/scheduler_test.go
git commit -m "fix: schedule against requested vm capacity"
```

---

### Task 2: Add RuntimeRouter CreateFlock Tests

**Files:**
- Modify: `internal/anvilmcp/runtime_router_test.go`
- Modify: `internal/anvilmcp/runtime_router.go`

- [ ] **Step 1: Extend `routerFakeDaemon` with flock fields**

In `internal/anvilmcp/runtime_router_test.go`, add these fields to `routerFakeDaemon`:

```go
	createFlockCalls int
	createFlockReq   FlockCreateRequest
	createFlockResp  *FlockCreateResponse
	createFlockErr   error
```

- [ ] **Step 2: Add `CreateFlock` to `routerFakeDaemon`**

In `internal/anvilmcp/runtime_router_test.go`, place this method after `DeleteSnapshot`:

```go
func (f *routerFakeDaemon) CreateFlock(_ context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	f.createFlockCalls++
	f.createFlockReq = req
	if f.createFlockErr != nil {
		return nil, f.createFlockErr
	}
	if f.createFlockResp != nil {
		return f.createFlockResp, nil
	}
	return &FlockCreateResponse{
		FlockID:      "flock-1",
		Task:         req.Task,
		TenantID:     req.TenantID,
		EgressPolicy: req.EgressPolicy,
		Agents: []FlockAgentInfo{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-worker-1", AgentURL: "http://10.0.1.10:8080", Status: "ready"},
			{AgentID: "reviewer-1", Role: "reviewer", VMID: "vm-reviewer-1", AgentURL: "http://10.0.1.11:8080", Status: "ready"},
		},
		TownWallURL: "http://host/flocks/flock-1/wall",
		PostURL:     "http://host/flocks/flock-1/post",
	}, nil
}
```

- [ ] **Step 3: Write the scheduler-aware flock placement test**

Append this test after `TestRuntimeRouterSpawnRecordsPlacementAndRoutesVMCalls`:

```go
func TestRuntimeRouterCreateFlockSchedulesByRoleCountAndRecordsPlacements(t *testing.T) {
	hostA := &routerFakeDaemon{}
	hostB := &routerFakeDaemon{}
	store := NewPlacementStore("")
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	resp, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "build town",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if resp.FlockID != "flock-1" {
		t.Fatalf("flock id = %q, want flock-1", resp.FlockID)
	}
	if hostA.createFlockCalls != 0 || hostB.createFlockCalls != 1 {
		t.Fatalf("create flock calls hostA/hostB = %d/%d, want 0/1", hostA.createFlockCalls, hostB.createFlockCalls)
	}
	if hostB.createFlockReq.TenantID != "tenant-1" || hostB.createFlockReq.EgressPolicy != "profile" {
		t.Fatalf("create flock req tenant/egress = %q/%q, want tenant-1/profile", hostB.createFlockReq.TenantID, hostB.createFlockReq.EgressPolicy)
	}
	state := store.State()
	if state.VMPlacements["vm-worker-1"] != "host-b" || state.VMPlacements["vm-reviewer-1"] != "host-b" {
		t.Fatalf("vm placements = %+v, want both member VMs on host-b", state.VMPlacements)
	}
}
```

- [ ] **Step 4: Write the quota denial test**

Append this test after the previous one:

```go
func TestRuntimeRouterCreateFlockRejectsQuotaBeforeDaemonCall(t *testing.T) {
	daemon := &routerFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 3, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}},
			map[string]TenantUsage{"tenant-1": {ActiveVMs: 0}},
		),
		map[string]Daemon{"host-a": daemon},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "build town",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateFlock error = nil, want quota denial")
	}
	var denied *ScheduleDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error type = %T, want ScheduleDeniedError", err)
	}
	if denied.Decision.Reason != "quota_exceeded" {
		t.Fatalf("denial reason = %q, want quota_exceeded", denied.Decision.Reason)
	}
	if daemon.createFlockCalls != 0 {
		t.Fatalf("daemon CreateFlock calls = %d, want 0", daemon.createFlockCalls)
	}
}
```

- [ ] **Step 5: Write the placement save failure redaction test**

Append this test after the quota test:

```go
func TestRuntimeRouterCreateFlockReportsPlacementSaveFailureWithoutSecrets(t *testing.T) {
	daemon := &routerFakeDaemon{
		createFlockResp: &FlockCreateResponse{
			FlockID: "flock-secret",
			Agents: []FlockAgentInfo{
				{AgentID: "worker-1", Role: "worker", VMID: "vm-secret", AgentURL: "http://10.0.1.10:8080", Status: "ready"},
			},
		},
	}
	store := NewPlacementStore(t.TempDir())
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "build town",
		Roles:        []string{"worker"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateFlock error = nil, want placement save failure")
	}
	message := err.Error()
	if !strings.Contains(message, "flock created but placement save failed") {
		t.Fatalf("error = %q, want placement save failure context", message)
	}
	for _, secret := range []string{"agent_token", "Authorization", "Bearer", "secret-token"} {
		if strings.Contains(message, secret) {
			t.Fatalf("placement error leaked %q: %s", secret, message)
		}
	}
}
```

- [ ] **Step 6: Run the new tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterCreateFlock' -count=1
```

Expected: FAIL because `RuntimeRouter` does not yet implement `CreateFlock`.

---

### Task 3: Implement RuntimeRouter CreateFlock

**Files:**
- Modify: `internal/anvilmcp/runtime_router.go`
- Test: `internal/anvilmcp/runtime_router_test.go`

- [ ] **Step 1: Add `CreateFlock` to `RuntimeRouter`**

In `internal/anvilmcp/runtime_router.go`, add this method after `RestoreSnapshot`:

```go
func (r *RuntimeRouter) CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	requestedActiveVMs := int64(len(req.Roles))
	if requestedActiveVMs <= 0 {
		requestedActiveVMs = 1
	}
	scheduleReq := ScheduleRequest{
		TenantID:           req.TenantID,
		EgressPolicy:       EgressPolicy(req.EgressPolicy),
		RequestedActiveVMs: requestedActiveVMs,
	}
	decision, daemon, err := r.scheduleDaemon(scheduleReq, TenantUsage{ActiveVMs: requestedActiveVMs})
	if err != nil {
		return nil, err
	}
	req.TenantID = decision.TenantID
	req.EgressPolicy = string(decision.EgressPolicy)
	resp, err := daemon.CreateFlock(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("daemon create flock returned nil response")
	}
	if err := r.recordFlockPlacements(resp, decision.Host.Name); err != nil {
		flockID := ""
		flockID = strings.TrimSpace(resp.FlockID)
		return nil, fmt.Errorf("flock created but placement save failed: flock_id=%q: %w", flockID, err)
	}
	return resp, nil
}
```

- [ ] **Step 2: Add batch placement recording helper**

In `internal/anvilmcp/runtime_router.go`, add this helper after `recordPlacement`:

```go
func (r *RuntimeRouter) recordFlockPlacements(resp *FlockCreateResponse, hostName string) error {
	hostName = strings.TrimSpace(hostName)
	if resp == nil || hostName == "" {
		return nil
	}
	vmIDs := make([]string, 0, len(resp.Agents))
	for _, agent := range resp.Agents {
		vmID := strings.TrimSpace(agent.VMID)
		if vmID != "" {
			vmIDs = append(vmIDs, vmID)
		}
	}
	if len(vmIDs) == 0 {
		return nil
	}
	r.mu.Lock()
	for _, vmID := range vmIDs {
		r.placement[vmID] = hostName
	}
	r.mu.Unlock()

	if r.placementStore == nil {
		return nil
	}
	for _, vmID := range vmIDs {
		if err := r.placementStore.SetVMPlacement(vmID, hostName); err != nil {
			return err
		}
	}
	return r.placementStore.Save()
}
```

- [ ] **Step 3: Run runtime router flock tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestRuntimeRouterCreateFlock|TestSchedulerRequiresEnoughAvailableVMsForRequestedActiveVMs' -count=1
```

Expected: PASS.

- [ ] **Step 4: Run full anvilmcp tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit router flock placement**

```bash
git add internal/anvilmcp/runtime_router.go internal/anvilmcp/runtime_router_test.go
git commit -m "feat: route flock creation through scheduler"
```

---

### Task 4: Delegate Flock Creation Through Router-Aware MCP Daemon Wrapper

**Files:**
- Modify: `internal/anvilmcp/replicating_daemon.go`
- Create: `internal/anvilmcp/replicating_daemon_test.go`

- [ ] **Step 1: Create wrapper delegation tests**

Create `internal/anvilmcp/replicating_daemon_test.go`:

```go
package anvilmcp

import (
	"context"
	"testing"
)

type wrapperFlockDaemon struct {
	createFlockCalls int
	createFlockReq   FlockCreateRequest
}

func (d *wrapperFlockDaemon) CreateFlock(_ context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	d.createFlockCalls++
	d.createFlockReq = req
	return &FlockCreateResponse{FlockID: "base-flock", Task: req.Task}, nil
}

type wrapperFlockRouter struct {
	createFlockCalls int
	createFlockReq   FlockCreateRequest
}

func (r *wrapperFlockRouter) ReplicateSnapshot(context.Context, SnapshotReplicationRequest) (*SnapshotReplicationResponse, error) {
	return &SnapshotReplicationResponse{Status: "replicated"}, nil
}

func (r *wrapperFlockRouter) CreateFlock(_ context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	r.createFlockCalls++
	r.createFlockReq = req
	return &FlockCreateResponse{FlockID: "router-flock", Task: req.Task}, nil
}

func TestReplicatingDaemonCreateFlockDelegatesToRouterWhenAvailable(t *testing.T) {
	base := &fakeDaemon{}
	router := &wrapperFlockRouter{}
	wrapped := NewReplicatingDaemon(base, router)

	resp, err := wrapped.CreateFlock(context.Background(), FlockCreateRequest{Task: "build", Roles: []string{"worker"}})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if resp.FlockID != "router-flock" {
		t.Fatalf("flock id = %q, want router-flock", resp.FlockID)
	}
	if router.createFlockCalls != 1 || base.createFlockCalls != 0 {
		t.Fatalf("create flock calls router/base = %d/%d, want 1/0", router.createFlockCalls, base.createFlockCalls)
	}
	if router.createFlockReq.Task != "build" {
		t.Fatalf("router request task = %q, want build", router.createFlockReq.Task)
	}
}

func TestReplicatingDaemonCreateFlockFallsBackToBase(t *testing.T) {
	base := &fakeDaemon{}
	wrapped := NewReplicatingDaemon(base, nil)

	resp, err := wrapped.CreateFlock(context.Background(), FlockCreateRequest{Task: "build", Roles: []string{"worker"}})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if resp.FlockID != "flock-1" {
		t.Fatalf("flock id = %q, want base fake flock-1", resp.FlockID)
	}
	if base.createFlockCalls != 1 {
		t.Fatalf("base CreateFlock calls = %d, want 1", base.createFlockCalls)
	}
}
```

- [ ] **Step 2: Run wrapper tests and verify the router test fails**

Run:

```bash
go test ./internal/anvilmcp -run 'TestReplicatingDaemonCreateFlock' -count=1
```

Expected: FAIL for `TestReplicatingDaemonCreateFlockDelegatesToRouterWhenAvailable`, because `ReplicatingDaemon` currently falls through to the embedded base daemon.

- [ ] **Step 3: Add a flock creator interface and wrapper field**

In `internal/anvilmcp/replicating_daemon.go`, add this interface after `ReplicatingDaemon` imports:

```go
type flockCreator interface {
	CreateFlock(context.Context, FlockCreateRequest) (*FlockCreateResponse, error)
}
```

Update the struct:

```go
type ReplicatingDaemon struct {
	Daemon
	replicator  snapshotReplicator
	flockRouter flockCreator
}
```

- [ ] **Step 4: Wire router support in `NewReplicatingDaemon`**

Replace `NewReplicatingDaemon` with:

```go
func NewReplicatingDaemon(base Daemon, replicator snapshotReplicator) *ReplicatingDaemon {
	wrapped := &ReplicatingDaemon{
		Daemon:     base,
		replicator: replicator,
	}
	if creator, ok := replicator.(flockCreator); ok {
		wrapped.flockRouter = creator
	}
	return wrapped
}
```

- [ ] **Step 5: Override `CreateFlock`**

In `internal/anvilmcp/replicating_daemon.go`, add this method before `ReplicateSnapshot`:

```go
func (d *ReplicatingDaemon) CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	if d != nil && d.flockRouter != nil {
		return d.flockRouter.CreateFlock(ctx, req)
	}
	return d.Daemon.CreateFlock(ctx, req)
}
```

- [ ] **Step 6: Run wrapper tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestReplicatingDaemonCreateFlock|TestRuntimeRouterCreateFlock' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit wrapper delegation**

```bash
git add internal/anvilmcp/replicating_daemon.go internal/anvilmcp/replicating_daemon_test.go
git commit -m "feat: delegate flock creation to router"
```

---

### Task 5: Add MCP SpawnFlock Output Redaction Regression

**Files:**
- Modify: `internal/anvilmcp/tools_test.go`

- [ ] **Step 1: Add output redaction test**

Append this test near the existing flock tool tests in `internal/anvilmcp/tools_test.go`:

```go
func TestToolsMCPSpawnFlockOutputOmitsSecretsAndHostEndpoints(t *testing.T) {
	daemon := &fakeDaemon{
		createFlockResp: &FlockCreateResponse{
			FlockID:      "flock-1",
			Task:         "build town",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
			Agents: []FlockAgentInfo{
				{AgentID: "worker-1", Role: "worker", VMID: "vm-1", AgentURL: "http://10.0.1.10:8080", Status: "ready"},
			},
			TownWallURL: "http://127.0.0.1:3000/flocks/flock-1/wall",
			PostURL:     "http://127.0.0.1:3000/flocks/flock-1/post",
		},
	}
	tools := NewToolsWithOptions(daemon, nil, time.Second, ToolsOptions{DefaultTenantID: "tenant-1"})

	_, out, err := tools.MCPSpawnFlock(context.Background(), nil, SpawnFlockInput{
		Task:         "build town",
		Roles:        []string{"worker"},
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatalf("MCPSpawnFlock returned error: %v", err)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{"agent_token", "agent_tokens", "Authorization", "Bearer", "http://host-a", "secret-token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("MCP spawn flock output leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "flock-1") || !strings.Contains(text, "vm-1") {
		t.Fatalf("MCP spawn flock output = %s, want flock and VM identity", text)
	}
}
```

- [ ] **Step 2: Run the redaction test**

Run:

```bash
go test ./internal/anvilmcp -run TestToolsMCPSpawnFlockOutputOmitsSecretsAndHostEndpoints -count=1
```

Expected: PASS. If it fails, fix only the output struct or MCP wrapper behavior that caused the leak.

- [ ] **Step 3: Commit redaction regression**

```bash
git add internal/anvilmcp/tools_test.go
git commit -m "test: guard flock mcp output redaction"
```

---

### Task 6: Documentation And Operations Handoff

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture/mcp-architecture.md`
- Modify: `docs/architecture/multi-tenant-roadmap.md`
- Modify: `RELEASE_NOTES.md`
- Create: `docs/operations/2026-06-03-scheduler-aware-flock-placement-handoff.md`

- [ ] **Step 1: Update README MCP scheduler section**

In `README.md`, update the scheduler/runtime router paragraph near the MCP adapter section to include:

```markdown
`ANVIL_MCP_SCHEDULER_STATE` 또는 `ANVIL_MCP_SCHEDULER_HOSTS_FILE`로 router config가
제공되면 `anvil_spawn_flock`은 scheduler-aware single-host placement를 사용한다.
roles 수만큼 active VM capacity/quota를 확인한 뒤 하나의 healthy host를 선택하고,
daemon `POST /flocks`는 그 host에서 기존 single-host 의미로 실행한다. true cross-host
flock member 분산, cross-host Town Wall, cross-host `gtcall`은 후속 범위다.
```

- [ ] **Step 2: Update MCP architecture exclusions**

In `docs/architecture/mcp-architecture.md`, replace the sentence that says scheduler-aware cross-host flock placement is absent with:

```markdown
MCP router config가 있을 때 `anvil_spawn_flock`은 scheduler-aware single-host
placement를 사용할 수 있다. flock member를 여러 runtime host에 분산하는 true
cross-host flock placement, cross-host Town Wall forwarding, cross-host `gtcall`은
아직 제외 범위다.
```

- [ ] **Step 3: Update multi-tenant roadmap**

In `docs/architecture/multi-tenant-roadmap.md`, add a small status note in the scheduler/flock section:

```markdown
2026-06-03 작은 시작 범위에서 MCP `anvil_spawn_flock`은 scheduler-aware single-host
placement를 사용하도록 확장됐다. 이 v1은 roles 수 기반 active VM quota/capacity를
확인하고 선택된 하나의 daemon host에 기존 `POST /flocks`를 위임한다. member별
cross-host 분산 배치와 cross-host Town Wall/`gtcall`은 계속 후속 후보로 남는다.
```

- [ ] **Step 4: Update RELEASE_NOTES**

Under `# Unreleased — Scheduler 운영 강화`, add:

```markdown
- MCP `anvil_spawn_flock` scheduler-aware single-host placement:
  - scheduler router config가 있을 때 roles 수 기반 active VM quota/capacity로 host를
    선택한다.
  - 선택된 host의 기존 daemon `POST /flocks`를 호출하고, 반환된 member VM placement를
    scheduler `PlacementStore`에 기록한다.
  - daemon direct `POST /flocks` wire contract와 `agent_token` 비노출 조건은 유지한다.
```

- [ ] **Step 5: Create operations handoff**

Create `docs/operations/2026-06-03-scheduler-aware-flock-placement-handoff.md`:

```markdown
# Scheduler-aware Flock Placement 운영 인계

작성일: 2026-06-03

## 릴리즈 범위

- MCP `anvil_spawn_flock`은 scheduler router config가 있을 때 scheduler-aware
  single-host placement를 사용한다.
- roles 수를 active VM 요청량으로 계산해 tenant quota와 host capacity를 확인한다.
- 선택된 daemon host의 기존 `POST /flocks`를 호출한다.
- 반환된 flock member VM ID를 scheduler `PlacementStore.VMPlacements`에 기록한다.

## 제외 범위

- daemon direct `POST /flocks` 계약 변경
- flock member의 cross-host 분산 배치
- cross-host Town Wall forwarding
- cross-host `gtcall`
- partial flock 허용

## 검증

- `go test ./internal/anvilmcp -count=1`
- `go test ./cmd/anvil-mcp -count=1`
- `go test ./... -count=1`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `go build ./cmd/goose-daemon`
- `bash -n scripts/anvil-mcp-e2e.sh`
- `git diff --check`

## 보안 조건

- `anvil_spawn_flock` 응답은 `agent_token` 또는 `agent_tokens`를 노출하지 않는다.
- scheduler placement state에는 VM ID와 host name만 저장한다.
- host endpoint, authorization header, bearer token, daemon raw body는 MCP output에
  넣지 않는다.

## 잔여 위험

- 첫 버전은 single-host flock placement다.
- placement save 실패 시 flock은 이미 생성된 상태일 수 있으며, 운영자는 daemon
  `DELETE /flocks/{id}`로 정리해야 한다.
- KVM/Firecracker full e2e는 이번 unit verification의 필수 조건이 아니다.

## 다음 작업

- true cross-host flock placement 설계
- cross-host Town Wall/`gtcall` routing 설계
- scheduler flock placement metrics 검토
```

- [ ] **Step 6: Run documentation checks**

Run:

```bash
rg -n "scheduler-aware single-host|anvil_spawn_flock|cross-host Town Wall|agent_token" README.md docs/architecture/mcp-architecture.md docs/architecture/multi-tenant-roadmap.md RELEASE_NOTES.md docs/operations/2026-06-03-scheduler-aware-flock-placement-handoff.md
git diff --check
```

Expected: `rg` finds the new documented contracts, and `git diff --check` exits 0.

- [ ] **Step 7: Commit docs**

```bash
git add README.md docs/architecture/mcp-architecture.md docs/architecture/multi-tenant-roadmap.md RELEASE_NOTES.md docs/operations/2026-06-03-scheduler-aware-flock-placement-handoff.md
git commit -m "docs: document scheduler-aware flock placement"
```

---

### Task 7: Final Verification

**Files:**
- All files modified by previous tasks.

- [ ] **Step 1: Format Go files**

Run:

```bash
gofmt -w internal/anvilmcp/tenant_policy.go internal/anvilmcp/scheduler.go internal/anvilmcp/scheduler_test.go internal/anvilmcp/runtime_router.go internal/anvilmcp/runtime_router_test.go internal/anvilmcp/replicating_daemon.go internal/anvilmcp/replicating_daemon_test.go internal/anvilmcp/tools_test.go
```

Expected: command exits 0.

- [ ] **Step 2: Run targeted tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerRequiresEnoughAvailableVMsForRequestedActiveVMs|TestRuntimeRouterCreateFlock|TestReplicatingDaemonCreateFlock|TestToolsMCPSpawnFlockOutputOmitsSecretsAndHostEndpoints' -count=1
```

Expected: PASS.

- [ ] **Step 3: Run package tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-mcp -count=1
```

Expected: PASS.

- [ ] **Step 4: Run repository tests**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Run build and shell syntax checks**

Run:

```bash
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
go build ./cmd/goose-daemon
bash -n scripts/anvil-mcp-e2e.sh
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 6: Check branch status**

Run:

```bash
git status --short --branch
```

Expected: clean working tree on `feature/scheduler-aware-flock-placement`.
