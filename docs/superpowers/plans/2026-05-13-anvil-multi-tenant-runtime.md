# anvil Multi-Tenant Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** multi-tenant foundation을 실제 runtime 계약 쪽으로 확장해 scheduler service, daemon tenant contract, quota enforcement, egress policy, audit failure/retention, restore token removal을 구현한다.

**Architecture:** `internal/anvilmcp`에는 in-process scheduler service와 runtime audit 조회/보관 helper를 둔다. `cmd/goose-daemon`은 `tenant_id`를 VM/snapshot/restore contract에 명시적으로 반영하고, restore 응답에서 `agent_token`을 제거한다. Host egress enforcement는 daemon이 선택된 policy를 VM metadata에 고정하고, 실제 packet filtering은 후속 host network layer로 남긴다.

**Tech Stack:** Go 1.25, standard library, existing ephemera daemon and MCP adapter packages.

---

## File Structure

- Create: `internal/anvilmcp/scheduler.go`
  - host inventory + quota policy 기반 schedule decision service
  - egress policy compatibility check
  - request-before-daemon quota enforcement helper
- Modify: `internal/anvilmcp/tenant_policy.go`
  - runtime audit failure path, read, retention pruning helpers
- Modify: `internal/anvilmcp/tools.go`
  - audit failure 기록과 quota/scheduler extension point 정리
- Modify: `cmd/goose-daemon/api.go`
  - `tenant_id`, `egress_policy` request/response/metadata contract
  - restore response `agent_token` 제거
- Modify: `internal/storage/snapshot.go`
  - snapshot metadata tenant/egress fields
- Modify tests in `internal/anvilmcp/*_test.go`, `cmd/goose-daemon/api_test.go`
- Modify Korean docs: `CONTEXT.md`, `README.md`, `docs/architecture/*.md`, operations docs as needed.

## Tasks

### Task 1: Merge prerequisite PR

- [x] Merge PR #10 and verify `origin/main` includes multi-tenant foundation.

### Task 2: Scheduler Service

- [x] Write failing tests for scheduler quota rejection, healthy host selection, unsupported egress rejection, and deterministic denial codes.
- [x] Implement `Scheduler` with host inventory, tenant quota/usage maps, and `Schedule`.
- [x] Run `go test ./internal/anvilmcp -run 'TestScheduler' -count=1`.

### Task 3: Daemon Tenant Contract

- [x] Write failing daemon tests for `tenant_id` on `POST /vms`, snapshot inheritance, snapshot mismatch rejection, and restore inheritance.
- [x] Add `TenantID` and `EgressPolicy` fields to VM/snapshot request/response metadata.
- [x] Validate `tenant_id` and `egress_policy` before mutating runtime state.
- [x] Run `go test ./cmd/goose-daemon -run 'Test.*Tenant|Test.*Egress' -count=1`.

### Task 4: Quota Enforcement Connection

- [x] Add scheduler tests showing quota is checked before daemon host selection.
- [x] Ensure denied requests return `quota_exceeded` with resource/limit/used/requested.
- [x] Run `go test ./internal/anvilmcp -run TestScheduler -count=1`.

### Task 5: Host Egress Policy Contract

- [x] Add tests for `EgressPolicy` in daemon VM info and snapshot metadata.
- [x] Persist chosen egress policy in VM and snapshot metadata.
- [x] Document that host packet filtering is still a host network layer follow-up.

### Task 6: Runtime Audit Expansion

- [x] Write tests for failure audit records, `ReadRuntimeAudit`, and retention pruning.
- [x] Implement failure result codes and retention helper without recording raw daemon body or `agent_token`.
- [x] Run `go test ./internal/anvilmcp -run 'Test.*RuntimeAudit' -count=1`.

### Task 7: Docs

- [x] Update Korean docs for tenant contract, quota denial, scheduler selection, egress contract, audit read/retention, and restore token removal.

### Task 8: Restore Token Removal

- [x] Write failing daemon/client tests proving direct restore response omits `agent_token`.
- [x] Remove `agent_token` from daemon `POST /snapshots/{id}/restore` response structs and MCP daemon client restore response.
- [x] Keep internal restored VM agent token from snapshot metadata for daemon proxy only.

### Task 9: Verification and PR

- [x] Run `go test ./...`.
- [x] Run `go test -race ./cmd/goose-daemon -count=1`.
- [x] Run `go test -race ./internal/anvilmcp -count=1`.
- [x] Run `go build ./cmd/goose-daemon`.
- [x] Run `go build ./cmd/anvil-mcp`.
- [x] Run `git diff --check`.
- [x] Search for `agent_token` and verify no new public output/audit records expose it.
- [ ] Commit, push, create PR.
