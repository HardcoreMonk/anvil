# anvil Runtime Control Plane and Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 문서의 2-8 후속 항목인 scheduler inventory, multi-host routing, quota persistence, tenant API, host egress enforcement, runtime audit API, metrics/health를 구현한다.

**Architecture:** PR #11의 multi-tenant foundation 위에 얇은 control-plane layer를 추가한다. MCP adapter 쪽에는 host inventory polling, route decision, quota store를 두고, daemon 쪽에는 host-local tenant/egress/audit/metrics endpoint와 강제 지점을 둔다. 실제 KVM lifecycle 소유권은 계속 ephemera daemon에 남긴다.

**Tech Stack:** Go 1.25, standard library HTTP/JSON, existing `cmd/goose-daemon`, `internal/anvilmcp`, JSON file persistence.

---

## File Structure

- Create: `internal/anvilmcp/host_inventory.go`
  - daemon host polling, health snapshot, capacity update, scheduler input materialization.
- Create: `internal/anvilmcp/host_inventory_test.go`
  - health polling, unhealthy exclusion, token forwarding, deterministic host snapshot tests.
- Create: `internal/anvilmcp/quota_store.go`
  - JSON-backed tenant quota/usage persistence for scheduler decisions.
- Create: `internal/anvilmcp/quota_store_test.go`
  - load/save, atomic update, missing file, invalid JSON, per-tenant usage tests.
- Create: `internal/anvilmcp/runtime_router.go`
  - scheduler-backed routing for spawn/restore and VM-to-host placement tracking.
- Create: `internal/anvilmcp/runtime_router_test.go`
  - routes before daemon mutation, records placement, rejects quota/no-host decisions.
- Modify: `internal/anvilmcp/daemon_client.go`
  - add daemon `/health`, `/metrics`, `/tenants`, `/audit/runtime` client methods.
- Modify: `cmd/goose-daemon/api.go`
  - add top-level `/health`, `/metrics`, `/tenants`, `/tenants/{id}`, `/audit/runtime`, and egress enforcement hooks.
- Modify: `cmd/goose-daemon/api_test.go`
  - daemon endpoint and enforcement tests.
- Modify: `docs/architecture/*.md`, `docs/operations/*.md`, `README.md`, `CONTEXT.md`
  - update implemented/non-goal state and usage examples.

## Tasks

### Task 1: Branch Baseline

- [x] Commit, push, and create PR for completed multi-tenant runtime contract.
- [x] Create follow-up branch from the PR baseline: `feature/runtime-control-plane-observability`.

### Task 2: Scheduler Host Inventory and Health Polling

- [x] Write failing tests in `internal/anvilmcp/host_inventory_test.go`:
  - healthy daemon response updates `RuntimeHost.Healthy=true`.
  - unreachable daemon marks host unhealthy and excludes it from `Scheduler.Schedule`.
  - Bearer token is forwarded to health polling requests.
- [x] Implement `HostInventory`, `HostProbe`, `PollOnce(ctx)`, and `RuntimeHosts()`.
- [x] Run `go test ./internal/anvilmcp -run 'TestHostInventory|TestScheduler' -count=1`.

### Task 3: Multi-Host Runtime Routing

- [x] Write failing tests in `internal/anvilmcp/runtime_router_test.go`:
  - spawn calls scheduler before daemon client.
  - quota denied spawn returns `quota_exceeded` and does not call a daemon.
  - successful spawn records `vm_id -> host`.
  - snapshot/delete/health/task use recorded VM placement.
  - restore records the restored VM placement on the selected host.
- [x] Implement `RuntimeRouter` with in-memory placement map and daemon client registry.
- [x] Run `go test ./internal/anvilmcp -run 'TestRuntimeRouter|TestScheduler' -count=1`.

### Task 4: Quota State Persistence

- [x] Write failing tests in `internal/anvilmcp/quota_store_test.go`:
  - missing file loads empty state.
  - saved quota/usage survives reload.
  - atomic update rejects negative usage.
  - quota decision uses persisted usage.
- [x] Implement JSON-backed `QuotaStore` with `Load`, `Save`, `SetTenantQuota`, `UpdateTenantUsage`, and `SchedulerInputs`.
- [x] Run `go test ./internal/anvilmcp -run 'TestQuotaStore|TestScheduler' -count=1`.

### Task 5: Tenant API

- [x] Write failing daemon tests:
  - `PUT /tenants/{tenant_id}` validates tenant ID and quota body.
  - `GET /tenants` lists tenants without secrets.
  - `GET /tenants/{tenant_id}` returns quota and current usage.
  - invalid tenant IDs return HTTP 400 before state mutation.
- [x] Implement daemon-local JSON tenant store under `workDir/tenants/tenants.json`.
- [x] Register `/tenants` and `/tenants/{id}` handlers.
- [x] Run `go test ./cmd/goose-daemon -run 'TestTenant' -count=1`.

### Task 6: Host Egress Enforcement

- [x] Write failing daemon tests:
  - `deny_all` adds a host-local deny decision before VM start.
  - `allow_all` leaves the default runtime path unchanged.
  - unsupported policy returns HTTP 400 before network/disk mutation.
- [x] Implement an injectable `egressEnforcer` interface in `ControlPlane`.
- [x] Wire enforcement before VM start and restore resume; cleanup enforcement state on VM destroy.
- [x] Keep actual packet filtering implementation conservative and host-local, with a no-op default for non-root unit tests.
- [x] Run `go test ./cmd/goose-daemon -run 'Test.*Egress' -count=1`.

### Task 7: Runtime Audit Operating API

- [x] Write failing daemon/MCP tests:
  - `GET /audit/runtime` reads JSONL records with tenant filter and limit.
  - `POST /audit/runtime/prune` applies keep-last/max-age policy.
  - responses never include raw daemon bodies, snapshot metadata, or `agent_token`.
- [x] Implement daemon or adapter-facing API wrappers around `ReadRuntimeAudit` and `PruneRuntimeAudit`.
- [x] Run `go test ./internal/anvilmcp ./cmd/goose-daemon -run 'Test.*RuntimeAudit|Test.*AuditAPI' -count=1`.

### Task 8: Metrics and Health Observability

- [x] Write failing daemon tests:
  - `GET /health` returns daemon status, VM count, snapshot count, and auth mode.
  - `GET /metrics` returns Prometheus text without secrets.
  - metrics include VM lifecycle counters, cleanup failure counters, and auth failure counters.
- [x] Add in-memory metrics counters to `ControlPlane`.
- [x] Increment counters on VM create/restore/delete, snapshot create/delete/GC, VM restore, cleanup warning, and auth failure.
- [x] Update `docs/operations/observability.md` from "not implemented" to endpoint usage.
- [x] Run `go test ./cmd/goose-daemon -run 'Test.*Health|Test.*Metrics' -count=1`.

### Task 9: Final Verification

- [x] Run `go test -count=1 ./...`.
- [x] Run `go build ./cmd/goose-daemon`.
- [x] Run `go build ./cmd/anvil-mcp`.
- [x] Run `bash -n e2e_test.sh`.
- [x] Run `bash -n scripts/build_image.sh`.
- [x] Run `git diff --check`.
- [x] Search for `agent_token` and verify no new public output/audit/metrics endpoint exposes it.
- [x] Run full KVM e2e: `go build -o anvil-daemon ./cmd/goose-daemon/` and `sudo bash e2e_test.sh`.
- [x] Commit, push, and create follow-up PR.

## Self-Review

- Spec coverage: user-selected items 1-8 map to Tasks 1-8. Task 9 covers verification and PR handoff.
- Placeholder scan: no TBD/TODO placeholders are used as implementation instructions.
- Type consistency: new `HostInventory`, `QuotaStore`, and `RuntimeRouter` build on existing `RuntimeHost`, `TenantQuota`, `TenantUsage`, `Scheduler`, and `DaemonClient` types from PR #11.
