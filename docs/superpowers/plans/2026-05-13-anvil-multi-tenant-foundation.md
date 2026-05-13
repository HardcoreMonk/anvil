# anvil Multi-Tenant Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 문서의 1-8 후속 항목을 최신 상태로 정리하고, multi-tenant runtime을 위한 quota, scheduler, egress, audit foundation을 코드와 문서에 추가한다.

**Architecture:** 이미 완료된 release/workspace/snapshot GC 항목은 `CONTEXT.md`와 운영 문서 상태를 최신화한다. 새 multi-tenant foundation은 `internal/anvilmcp`에 tenant identifier validation, quota decision, host selection, egress policy, append-only audit record helper를 추가하되 기존 daemon/MCP 동작은 바꾸지 않는다.

**Tech Stack:** Go 1.25, standard library, `golang.org/x/sys/unix`, 기존 `internal/anvilmcp` 패턴.

---

## File Structure

- Create: `internal/anvilmcp/tenant_policy.go`
  - tenant ID validation
  - quota/usage decision
  - egress policy validation
  - scheduler host selection primitive
  - append-only multi-tenant audit store
- Create: `internal/anvilmcp/tenant_policy_test.go`
  - TDD coverage for tenant validation, quota rejection, egress matching, host selection, audit JSONL safety
- Modify: `internal/anvilmcp/config.go`
  - optional config/env for default tenant ID and audit log path
- Modify: `internal/anvilmcp/config_test.go`
  - config file and env override tests
- Modify: `configs/anvil-mcp.yaml.example`
  - documented disabled-by-default tenant/audit options
- Modify: `CONTEXT.md`
  - completed release/workspace/snapshot follow-ups and remaining multi-host implementation next steps
- Modify: `README.md`
  - link and summarize multi-tenant foundation
- Modify: `docs/architecture/multi-tenant-roadmap.md`
  - mark foundation status and next integration boundaries

## Scope Check

이 plan은 scheduler service, tenant API, daemon header contract, host network enforcement, billing, UI를 구현하지 않는다. 기존 `anvil_*` MCP tool behavior와 ephemera daemon HTTP contract도 변경하지 않는다.

### Task 1: Red Tests for Multi-Tenant Foundation

- [ ] Add `internal/anvilmcp/tenant_policy_test.go` with tests for:
  - `NormalizeTenantID` trims valid IDs and rejects empty/path-like/oversized IDs.
  - `CheckTenantQuota` rejects the first exceeded quota with stable reason codes.
  - `NormalizeEgressPolicy` accepts `deny_all`, `profile`, `allow_all`.
  - `SelectRuntimeHost` chooses a healthy host that has VM capacity, requested snapshot bytes, and requested egress policy.
  - `AppendRuntimeAudit` writes JSONL with `0600`, rejects symlink paths, and never emits `agent_token`.
- [ ] Run `go test ./internal/anvilmcp -run 'TestNormalizeTenantID|TestCheckTenantQuota|TestSelectRuntimeHost|TestAppendRuntimeAudit' -count=1`.
- [ ] Confirm it fails to compile because the foundation types/functions do not exist.

### Task 2: Multi-Tenant Foundation Implementation

- [ ] Add `internal/anvilmcp/tenant_policy.go`.
- [ ] Implement `NormalizeTenantID`, `TenantQuota`, `TenantUsage`, `QuotaDecision`, `CheckTenantQuota`.
- [ ] Implement `EgressPolicy`, `NormalizeEgressPolicy`, `RuntimeHost`, `ScheduleRequest`, `SelectRuntimeHost`.
- [ ] Implement `RuntimeAuditRecord` and `AppendRuntimeAudit` with symlink-safe append-only JSONL semantics.
- [ ] Run `go test ./internal/anvilmcp -count=1`.

### Task 3: MCP Config Surface

- [ ] Add `DefaultTenantID` and `AuditLogPath` to `Config`.
- [ ] Add env vars `ANVIL_MCP_TENANT_ID` and `ANVIL_MCP_AUDIT_LOG`.
- [ ] Validate optional tenant ID with `NormalizeTenantID`.
- [ ] Update config tests and `configs/anvil-mcp.yaml.example`.
- [ ] Run `go test ./internal/anvilmcp -run TestLoadConfig -count=1`.

### Task 4: Release/Completed-Feature Status Cleanup

- [ ] Update `CONTEXT.md` so public release, workspace copy, and snapshot size/audit are no longer listed as unfinished follow-ups.
- [ ] Keep remaining work focused on multi-host scheduler integration, daemon tenant contract, host egress enforcement, and API examples.

### Task 5: Multi-Tenant Docs

- [ ] Update `README.md` with a short multi-tenant foundation section and config examples.
- [ ] Update `docs/architecture/multi-tenant-roadmap.md` to distinguish implemented foundation from non-goals.
- [ ] Ensure Korean prose and unchanged code/API names.

### Task 6: Full Verification

- [ ] Run `go test ./...`.
- [ ] Run `go test -race ./internal/anvilmcp -count=1`.
- [ ] Run `go build ./cmd/goose-daemon`.
- [ ] Run `go build ./cmd/anvil-mcp`.
- [ ] Run `git diff --check`.

### Task 7: Review for Security Invariants

- [ ] Search docs and new code for `agent_token`.
- [ ] Confirm audit records do not include `agent_token` or raw metadata.
- [ ] Confirm no daemon restore token exposure is normalized as policy.

### Task 8: Commit and Prepare PR

- [ ] Commit as `feat: add multi-tenant policy foundation`.
- [ ] Push branch.
- [ ] Create PR with summary and test plan.

## Self Review

- Spec coverage: all eight selected items are accounted for. Completed release/workspace/snapshot items are treated as status cleanup; multi-host/quota/egress/audit/API docs are advanced through a tested foundation.
- Placeholder scan: no TBD/TODO placeholders.
- Type consistency: function and type names are defined in Task 2 and referenced consistently by tests and docs.
