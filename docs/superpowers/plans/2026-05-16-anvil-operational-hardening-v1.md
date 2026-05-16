# Anvil Operational Hardening v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** purecvisor에서 가져올 운영 패턴 1-5를 anvil/ephemera runtime에 맞게 job/audit, verification policy, deep health/metrics, owner-scope, network preflight/cleanup 표면으로 반영한다.

**Architecture:** v1은 기존 ephemera daemon API 응답 호환성을 깨지 않는다. 먼저 daemon 내부에 operation state foundation을 추가하고, 기존 동기 lifecycle 작업에 `job_id`, audit event, health/metrics 관측을 붙인다. RBAC/owner-scope와 network preflight는 별도 task로 분리해 token 노출 정책과 Firecracker/TAP cleanup 계약을 보존한다.

**Tech Stack:** Go standard library, `net/http`, existing `cmd/goose-daemon`, `internal/network`, `internal/storage`, `internal/anvilmcp`, shell-based full KVM E2E.

**Lifecycle Status:** Approved for implementation. `docs/superpowers/grill-me/2026-05-16-anvil-operational-hardening-v1.md` is marked `passed`; implement this plan task-by-task with the required sub-skill.

---

## Scope

이 계획은 purecvisor의 운영 패턴을 anvil에 맞게 흡수한다.

1. **Job + audit + completion tracking**
   - 긴 lifecycle 작업을 `job_id`로 추적한다.
   - 기존 sync API body는 유지하고 `X-Anvil-Job-ID` header와 `/jobs` 조회를 추가한다.
   - `Prefer: respond-async` 또는 `?async=true` 기반 비동기 API 전환은 foundation이 안정화된 뒤 별도 ADR에서 결정한다.

2. **Verification policy**
   - 성공 HTTP 응답만으로 완료를 판정하지 않는 검증 정책을 문서화한다.
   - 영속 상태, cleanup, audit, metrics, E2E 증거를 함께 확인한다.

3. **Deep health / metrics**
   - daemon-level `/health`와 `/metrics`를 추가한다.
   - v1에서는 control-plane auth middleware 뒤에 둔다. 별도 unauth readiness endpoint는 추가하지 않는다.

4. **RBAC / owner-scope**
   - 기존 `EPHEMERA_API_TOKENS=name:token`은 admin 호환으로 유지한다.
   - 새 `name:role:token` 형식을 추가하고 `operator`는 owner가 일치하는 VM/snapshot만 조작하게 한다.

5. **Network preflight / cleanup observability**
   - `goose-br0`, TAP, IP pool, NAT, ip_forward 상태를 조회한다.
   - destructive cleanup은 자동 실행하지 않는다. v1은 status와 dry-run cleanup plan을 먼저 제공한다.

Out of scope:

- purecvisor의 libvirt/QEMU, LXC, ZFS zvol clone, multi-node HA, live migration, OVN multi-node automation
- `POST /vms` 외부의 `agent_token` 노출
- MCP adapter가 daemon lifecycle 의미를 직접 소유하는 구조
- 기본 API 응답 body를 breaking change로 바꾸는 async 전환

---

## Files

Create:

- `internal/ops/job.go`: in-memory job store, status transition, completion result.
- `internal/ops/job_test.go`: job lifecycle tests.
- `internal/ops/audit.go`: in-memory audit ring and structured event type.
- `internal/ops/audit_test.go`: audit retention and filtering tests.
- `internal/ops/metrics.go`: daemon metric counters and Prometheus text rendering.
- `internal/ops/metrics_test.go`: metrics rendering tests.
- `cmd/goose-daemon/ops_api_test.go`: daemon HTTP tests for `/jobs`, `/audit`, `/health`, `/metrics`.
- `cmd/goose-daemon/authz_test.go`: token role parsing and owner-scope tests.
- `internal/network/status.go`: network status snapshot and dry-run cleanup plan.
- `internal/network/status_test.go`: network status and cleanup plan tests with fake executor.
- `docs/DEVELOPMENT_VERIFICATION_POLICY.md`: anvil verification policy.
- `docs/adr/0002-operational-hardening-v1.md`: job/audit/health/RBAC/network scope ADR.

Modify:

- `cmd/goose-daemon/config.go`: parse `APIClient` role while preserving old token format.
- `cmd/goose-daemon/api.go`: attach authenticated client to request context, add ops endpoints, emit job/audit/metrics, enforce owner-scope.
- `cmd/goose-daemon/main.go`: initialize ops stores and pass them into `ControlPlane`.
- `internal/network/manager.go`: expose non-mutating status from IP/TAP pool.
- `README.md`: document ops endpoints, verification policy, RBAC token format.
- `CONTEXT.md`: add job/audit/owner-scope/runtime observability contracts.
- `docs/PUBLIC_RELEASE_BOUNDARY.md`: move audit/metrics/job store from conditional to public surface once implemented.
- `docs/ADR_INDEX.md`: register ADR-0002.
- `e2e_test.sh`: add full KVM checks for job/audit/health/metrics/network status without leaking `agent_token`.

---

### Task 1: Operation State Foundation

**Files:**
- Create: `internal/ops/job.go`
- Create: `internal/ops/job_test.go`
- Create: `internal/ops/audit.go`
- Create: `internal/ops/audit_test.go`
- Create: `internal/ops/metrics.go`
- Create: `internal/ops/metrics_test.go`

- [ ] **Step 1: Add failing tests for job lifecycle**

Test cases:

- `TestJobStoreStartCompleteFail`
- `TestJobStoreListSortedNewestFirst`
- `TestJobStoreRetention`

Expected model:

```go
type JobStatus string

const (
	JobAccepted JobStatus = "accepted"
	JobRunning  JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed   JobStatus = "failed"
)

type Job struct {
	ID        string          `json:"job_id"`
	Type      string          `json:"type"`
	Target    string          `json:"target,omitempty"`
	Owner     string          `json:"owner,omitempty"`
	Status    JobStatus       `json:"status"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   *time.Time      `json:"ended_at,omitempty"`
	Error     string          `json:"error,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}
```

Run:

```bash
go test ./internal/ops
```

Expected: tests fail because `internal/ops` does not exist.

- [ ] **Step 2: Implement `JobStore`**

Implementation requirements:

- `NewJobStore(limit int, now func() time.Time) *JobStore`
- `Start(jobType, target, owner string) Job`
- `MarkRunning(jobID string)`
- `Complete(jobID string, result any)`
- `Fail(jobID string, err error)`
- `Get(jobID string) (Job, bool)`
- `List() []Job`
- in-memory only for v1
- generated ID format: `job-<unix_nano>`
- no token or secret values in `Result`

Run:

```bash
go test ./internal/ops
```

Expected: PASS.

- [ ] **Step 3: Add audit ring tests**

Test cases:

- `TestAuditStoreRecordAndList`
- `TestAuditStoreLimit`
- `TestAuditEventDoesNotRequireSecrets`

Expected model:

```go
type AuditEvent struct {
	ID        string    `json:"event_id"`
	Time      time.Time `json:"time"`
	Client    string    `json:"client,omitempty"`
	Role      string    `json:"role,omitempty"`
	Action    string    `json:"action"`
	Target    string    `json:"target,omitempty"`
	Result    string    `json:"result"`
	JobID     string    `json:"job_id,omitempty"`
	Error     string    `json:"error,omitempty"`
}
```

- [ ] **Step 4: Implement `AuditStore`**

Requirements:

- `NewAuditStore(limit int, now func() time.Time) *AuditStore`
- `Record(event AuditEvent) AuditEvent`
- `List(limit int) []AuditEvent`
- enforce max retention in memory
- default limit in daemon: 1000 events

Run:

```bash
go test ./internal/ops
```

Expected: PASS.

- [ ] **Step 5: Add metrics tests**

Required metrics:

- `anvil_jobs_total{type,status}`
- `anvil_audit_events_total{result}`
- `anvil_vms_running`
- `anvil_snapshots_total`
- `anvil_network_ips_allocated`
- `anvil_network_taps_allocated`
- `anvil_cleanup_failures_total{resource}`

- [ ] **Step 6: Implement metrics renderer**

Requirements:

- render Prometheus text format
- no external dependency
- deterministic label ordering in tests
- no `agent_token` or bearer token value in metric labels

Run:

```bash
go test ./internal/ops
```

Expected: PASS.

- [ ] **Step 7: Commit foundation**

```bash
git add internal/ops
git commit -m "feat: add operation state foundation"
```

---

### Task 2: Daemon Job, Audit, And Completion Endpoints

**Files:**
- Modify: `cmd/goose-daemon/api.go`
- Modify: `cmd/goose-daemon/main.go`
- Create: `cmd/goose-daemon/ops_api_test.go`

- [ ] **Step 1: Add failing daemon endpoint tests**

Test cases:

- `GET /jobs` returns `[]` before operations.
- `GET /jobs/{job_id}` returns one job after a tracked operation.
- `GET /audit?limit=10` returns latest audit events.
- existing `POST /vms` response body shape remains compatible.
- `POST /vms` response includes `X-Anvil-Job-ID`.

Run:

```bash
go test ./cmd/goose-daemon -run 'TestOps'
```

Expected: FAIL because endpoints do not exist.

- [ ] **Step 2: Add ops stores to `ControlPlane`**

Extend constructor with:

```go
jobs    *ops.JobStore
audit   *ops.AuditStore
metrics *ops.Metrics
```

Use defaults in `main.go`:

```go
jobs := ops.NewJobStore(1000, time.Now)
audit := ops.NewAuditStore(1000, time.Now)
metrics := ops.NewMetrics()
```

- [ ] **Step 3: Add `/jobs`, `/jobs/{id}`, `/audit` handlers**

Routing:

```go
mux.HandleFunc("/jobs", cp.handleJobs)
mux.HandleFunc("/jobs/", cp.handleJobItem)
mux.HandleFunc("/audit", cp.handleAudit)
```

Behavior:

- `GET /jobs`: returns latest jobs.
- `GET /jobs/{job_id}`: returns `404 {"error":"job not found"}` if absent.
- `GET /audit?limit=N`: returns latest N audit events.
- all endpoints remain behind existing control-plane auth middleware.

- [ ] **Step 4: Track existing sync lifecycle operations**

Add job/audit tracking to:

- `POST /vms`
- `DELETE /vms/{vm_id}`
- `POST /vms/{vm_id}/snapshot`
- `POST /snapshots/{snapshot_id}/restore`
- `DELETE /snapshots/{snapshot_id}`

Rules:

- preserve existing response body.
- add `X-Anvil-Job-ID` header on tracked operations.
- record audit `result=ok` only after final state is known.
- record audit `result=fail` on every error branch.
- never copy `agent_token` into job result, audit event, or metric labels.

- [ ] **Step 5: Verify**

```bash
go test ./cmd/goose-daemon -run 'TestOps'
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit daemon ops endpoints**

```bash
git add cmd/goose-daemon internal/ops
git commit -m "feat: track daemon operations"
```

---

### Task 3: Verification Policy

**Files:**
- Create: `docs/DEVELOPMENT_VERIFICATION_POLICY.md`
- Modify: `README.md`
- Modify: `docs/PUBLIC_RELEASE_BOUNDARY.md`
- Modify: `docs/ADR_INDEX.md`
- Create: `docs/adr/0002-operational-hardening-v1.md`

- [ ] **Step 1: Write verification policy document**

Required sections:

- 문서 목적
- 검증 단계
- 기능별 최소 검증 기준
- `agent_token` 노출 회귀 검사
- full KVM E2E 필요 조건
- cleanup 검증 조건
- job/audit/metrics 검증 조건

Core rule:

```text
성공 HTTP 응답은 기능 완료 증거가 아니다. 최종 daemon state, guest reachability,
host resource cleanup, audit event, metrics, E2E evidence가 함께 맞아야 완료로 본다.
```

- [ ] **Step 2: Register ADR-0002**

ADR-0002 decision:

- v1은 in-memory job/audit/metrics로 시작한다.
- persistent audit store는 `deferred`다.
- sync API 호환성을 유지한다.
- unauth `/metrics`는 v1에서 제외한다.
- owner-scope는 existing token format을 깨지 않는 방식으로 추가한다.

- [ ] **Step 3: Update public boundary**

Move this row from conditional to public when implementation lands:

```markdown
| audit/metrics/job store | daemon 작업 추적, audit event, Prometheus metrics, deep health |
```

Keep persistent audit storage and async API conversion as conditional/deferred.

- [ ] **Step 4: Verify docs**

```bash
rg -n "TODO|TBD|FIXME" docs/DEVELOPMENT_VERIFICATION_POLICY.md docs/adr/0002-operational-hardening-v1.md
git diff --check
```

Expected:

- no `TODO/TBD/FIXME` output
- `git diff --check` exits 0

- [ ] **Step 5: Commit docs**

```bash
git add docs/DEVELOPMENT_VERIFICATION_POLICY.md docs/PUBLIC_RELEASE_BOUNDARY.md docs/ADR_INDEX.md docs/adr/0002-operational-hardening-v1.md README.md
git commit -m "docs: add anvil verification policy"
```

---

### Task 4: Deep Health And Metrics

**Files:**
- Modify: `cmd/goose-daemon/api.go`
- Modify: `internal/network/manager.go`
- Modify: `internal/ops/metrics.go`
- Create: `cmd/goose-daemon/health_metrics_test.go`

- [ ] **Step 1: Add failing health and metrics tests**

Test cases:

- `GET /health` returns daemon status JSON.
- health includes `vms_running`, `snapshots_total`, `auth_enabled`, `network`.
- `GET /metrics` returns Prometheus text.
- metrics contain no `agent_token` string.

Expected health shape:

```json
{
  "service": "anvil-daemon",
  "status": "ok",
  "runtime": "ephemera",
  "vms_running": 0,
  "snapshots_total": 0,
  "auth_enabled": true,
  "network": {
    "bridge": "goose-br0",
    "subnet": "10.0.1.0/24",
    "allocated_ips": 0
  }
}
```

- [ ] **Step 2: Add health handler**

Route:

```go
mux.HandleFunc("/health", cp.handleDaemonHealth)
```

Rules:

- This is daemon health, not guest health.
- Guest health remains `GET /vms/{vm_id}/health`.
- v1 keeps `/health` behind existing auth middleware.

- [ ] **Step 3: Add metrics handler**

Route:

```go
mux.HandleFunc("/metrics", cp.handleMetrics)
```

Rules:

- content type: `text/plain; version=0.0.4`
- include VM/snapshot/job/audit/network counters.
- do not include client token, agent token, prompt, workspace content.

- [ ] **Step 4: Verify**

```bash
go test ./cmd/goose-daemon -run 'TestHealth|TestMetrics'
go test ./...
```

Expected: PASS.

- [ ] **Step 5: Commit health and metrics**

```bash
git add cmd/goose-daemon internal/ops internal/network
git commit -m "feat: expose daemon health and metrics"
```

---

### Task 5: RBAC And Owner-Scope

**Files:**
- Modify: `cmd/goose-daemon/config.go`
- Modify: `cmd/goose-daemon/api.go`
- Create: `cmd/goose-daemon/authz_test.go`
- Modify: `docs/adr/0002-operational-hardening-v1.md`
- Modify: `README.md`

- [ ] **Step 1: Add failing token parser tests**

Required compatibility:

| Env value | Parsed client |
|---|---|
| `alice:tok1` | name `alice`, role `admin`, token `tok1` |
| `alice:operator:tok1` | name `alice`, role `operator`, token `tok1` |
| `tok1` through `EPHEMERA_API_TOKEN` | name `default`, role `admin`, token `tok1` |

Allowed roles:

- `admin`
- `operator`
- `viewer`

Invalid role entries are ignored and logged.

- [ ] **Step 2: Attach auth client to request context**

Current auth middleware logs matched client but does not pass it downstream. Add:

```go
type authContextKey struct{}

type AuthContext struct {
	Name string
	Role string
}
```

Rules:

- no-token mode uses `{Name:"anonymous", Role:"admin"}` for backward-compatible local dev.
- matched client is available to handlers.

- [ ] **Step 3: Add owner to VM and snapshot metadata**

Add to public VM/snapshot JSON:

```go
Owner string `json:"owner,omitempty"`
```

Rules:

- existing clients can ignore the new field.
- new VMs get owner from authenticated client name.
- restored VMs inherit owner from snapshot unless admin override is later added.

- [ ] **Step 4: Enforce owner-scope**

Rules:

- `admin`: full access.
- `operator`: can list and mutate only owned VMs/snapshots.
- `viewer`: can list/read only owned VMs/snapshots, cannot mutate.
- no endpoint may return `agent_token` outside `POST /vms`.

Apply to:

- `GET /vms`
- `DELETE /vms/{vm_id}`
- `POST /vms/{vm_id}/snapshot`
- `GET /snapshots`
- `POST /snapshots/{snapshot_id}/restore`
- `DELETE /snapshots/{snapshot_id}`
- `GET /jobs`
- `GET /audit`

- [ ] **Step 5: Verify**

```bash
go test ./cmd/goose-daemon -run 'TestAuth|TestOwner|TestRBAC'
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit RBAC**

```bash
git add cmd/goose-daemon README.md docs/adr/0002-operational-hardening-v1.md
git commit -m "feat: add owner scoped daemon access"
```

---

### Task 6: Network Preflight And Cleanup Observability

**Files:**
- Modify: `internal/network/manager.go`
- Create: `internal/network/status.go`
- Create: `internal/network/status_test.go`
- Modify: `cmd/goose-daemon/api.go`
- Create: `cmd/goose-daemon/network_api_test.go`
- Modify: `README.md`

- [ ] **Step 1: Add fake executor seam for network status tests**

Do not shell out directly from tests. Introduce a package-level executor interface:

```go
type commandRunner interface {
	Run(name string, args ...string) ([]byte, error)
}
```

Production implementation wraps `exec.Command(name, args...).CombinedOutput()`.

- [ ] **Step 2: Add network status model**

Expected JSON:

```go
type Status struct {
	BridgeName      string   `json:"bridge_name"`
	Subnet          string   `json:"subnet"`
	GatewayIP       string   `json:"gateway_ip"`
	AllocatedIPs    int      `json:"allocated_ips"`
	FreeIPs         int      `json:"free_ips"`
	AllocatedTAPs   int      `json:"allocated_taps"`
	FreeTAPIDs      []int    `json:"free_tap_ids,omitempty"`
	IPForward       string   `json:"ip_forward"`
	NATRulePresent  bool     `json:"nat_rule_present"`
	StaleTAPDevices []string `json:"stale_tap_devices,omitempty"`
}
```

- [ ] **Step 3: Add dry-run cleanup plan**

Expected JSON:

```go
type CleanupPlan struct {
	DryRun          bool     `json:"dry_run"`
	StaleTAPDevices []string `json:"stale_tap_devices"`
	Commands        []string `json:"commands"`
}
```

Rules:

- default is dry-run only.
- destructive cleanup requires explicit future ADR and separate approval.
- do not delete active `runningVM.tapDevice`.

- [ ] **Step 4: Add daemon endpoints**

Routes:

```go
mux.HandleFunc("/network/status", cp.handleNetworkStatus)
mux.HandleFunc("/network/cleanup-plan", cp.handleNetworkCleanupPlan)
```

Both remain behind control-plane auth.

- [ ] **Step 5: Verify**

```bash
go test ./internal/network
go test ./cmd/goose-daemon -run 'TestNetwork'
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit network observability**

```bash
git add internal/network cmd/goose-daemon README.md
git commit -m "feat: expose network preflight status"
```

---

### Task 7: Full KVM E2E Coverage

**Files:**
- Modify: `e2e_test.sh`
- Modify: `docs/DEVELOPMENT_VERIFICATION_POLICY.md`
- Modify: `README.md`

- [ ] **Step 1: Add E2E assertions**

Add steps after the current VM lifecycle checks:

- daemon `/health` returns `status=ok`
- `/metrics` contains `anvil_vms_running`
- `POST /vms` returns `X-Anvil-Job-ID`
- `/jobs/{job_id}` reaches `succeeded`
- `/audit` contains matching lifecycle event
- `/network/status` reports allocated IP/TAP while VM is running
- after `DELETE /vms/{id}`, `/network/status` allocated IP count decreases
- `agent_token` does not appear in `/jobs`, `/audit`, `/metrics`, `/network/status`

- [ ] **Step 2: Run unit tests**

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Build daemon**

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
```

Expected: binary builds successfully.

- [ ] **Step 4: Run full KVM E2E**

```bash
sudo bash e2e_test.sh
```

Expected:

- all existing steps pass
- new operational hardening steps pass
- final VM count is zero
- final snapshot count is zero
- no leaked `agent_token` outside allowed `POST /vms` responses

- [ ] **Step 5: Commit E2E coverage**

```bash
git add e2e_test.sh README.md docs/DEVELOPMENT_VERIFICATION_POLICY.md
git commit -m "test: cover operational hardening e2e"
```

---

## Execution Order

Recommended order:

1. Task 1: operation state foundation
2. Task 2: daemon job/audit endpoints
3. Task 4: health/metrics
4. Task 6: network status
5. Task 5: RBAC/owner-scope
6. Task 3: verification policy and ADR updates
7. Task 7: full KVM E2E

RBAC is intentionally after health/metrics/network status because auth context changes can make debugging early instrumentation harder. Documentation should be finalized after the concrete API surface lands, but ADR-0002 can be drafted early and updated before commit.

---

## Risk Controls

- Existing API body compatibility must be preserved unless a separate ADR approves a breaking change.
- `agent_token` must never appear in `/jobs`, `/audit`, `/metrics`, `/health`, `/network/status`, MCP output, or replay output.
- Network cleanup must be dry-run only in this plan.
- `/health` and `/metrics` stay behind current control-plane auth in v1.
- No local secret or runtime artifact is committed.

---

## Final Verification

Run before declaring the plan implemented:

```bash
! rg -n 'secret-token|Bearer [A-Za-z0-9._~+/=-]{8,}' docs/DEVELOPMENT_VERIFICATION_POLICY.md docs/adr README.md
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
sudo bash e2e_test.sh
git diff --check
```

Expected:

- docs contain policy discussion but no real token values
- unit tests pass
- both binaries build
- full KVM E2E passes
- whitespace check passes
