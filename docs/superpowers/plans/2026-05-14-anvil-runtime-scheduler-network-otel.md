# anvil Runtime Scheduler, Network Policy, and Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 문서의 남은 1-8 후속 항목인 scheduler service, snapshot locality, failover/reconciliation, profile egress/DNS enforcement, per-VM metrics, OpenTelemetry trace, IronClaw schema verification을 구현한다.

**Architecture:** 기존 ephemera daemon lifecycle은 유지하고, `internal/anvilmcp`에 scheduler service와 persistent placement/inventory state를 둔다. daemon은 host-local egress policy와 observability endpoint를 확장하고, OpenTelemetry export는 optional HTTP exporter로 격리한다. IronClaw 호환성은 anvil tool input schema를 정적 검증하는 helper와 운영 문서로 관리한다.

**Tech Stack:** Go 1.25, standard library HTTP/JSON, existing `cmd/goose-daemon`, `internal/anvilmcp`, JSON file persistence, Prometheus text metrics.

---

## File Structure

- Create: `internal/anvilmcp/placement_store.go`
  - VM placement, snapshot locality, host inventory persistence.
- Create: `internal/anvilmcp/placement_store_test.go`
  - JSON persistence, deterministic ordering, reconciliation replacement tests.
- Modify: `internal/anvilmcp/scheduler.go`
  - preferred/excluded host selection for locality and failover.
- Modify: `internal/anvilmcp/scheduler_test.go`
  - locality preference and failover exclusion tests.
- Modify: `internal/anvilmcp/runtime_router.go`
  - retry/failover, snapshot locality recording, placement reconciliation.
- Modify: `internal/anvilmcp/runtime_router_test.go`
  - retry, restore locality, reconcile tests.
- Create: `internal/anvilmcp/scheduler_service.go`
  - HTTP scheduler service handlers for hosts, placements, reconcile, schedule.
- Create: `internal/anvilmcp/scheduler_service_test.go`
  - service API tests.
- Create: `cmd/anvil-scheduler/main.go`
  - thin scheduler service binary.
- Create: `cmd/anvil-scheduler/main_test.go`
  - env/config parsing tests.
- Create: `cmd/goose-daemon/egress_policy.go`
  - profile egress/DNS policy parser and iptables command planner.
- Create: `cmd/goose-daemon/egress_policy_test.go`
  - profile allowlist and DNS enforcement command tests.
- Modify: `cmd/goose-daemon/api.go`
  - profile-aware egress application, per-VM metrics, duration metrics, queue depth, OTEL exporter hooks.
- Modify: `cmd/goose-daemon/api_test.go`
  - metrics, per-VM metrics, trace export, egress integration tests.
- Create: `cmd/goose-daemon/otel.go`
  - optional OpenTelemetry-compatible JSON trace exporter.
- Create: `cmd/goose-daemon/otel_test.go`
  - exporter disabled/enabled tests.
- Create: `internal/anvilmcp/ironclaw_schema.go`
  - anvil tool input schema compatibility checks.
- Create: `internal/anvilmcp/ironclaw_schema_test.go`
  - empty type rejection and current tool input validation tests.
- Modify: `README.md`, `CONTEXT.md`, `docs/architecture/multi-tenant-roadmap.md`, `docs/operations/observability.md`, `docs/operations/2026-05-12-ironclaw-integration-check.md`
  - Korean docs update.

## Tasks

### Task 1: Scheduler Service and Persistent Inventory

- [x] Add failing placement/inventory persistence tests.
- [x] Implement `PlacementStore` with `Load`, `Save`, `SetHost`, `ListHosts`, `SetVMPlacement`, `SetSnapshotLocation`.
- [x] Add failing scheduler service API tests for `/health`, `/hosts`, `/placements`, `/schedule/spawn`.
- [x] Implement `SchedulerService` and thin `cmd/anvil-scheduler`.
- [x] Run `go test ./internal/anvilmcp ./cmd/anvil-scheduler -run 'TestPlacementStore|TestSchedulerService|TestSchedulerCommand' -count=1`.

### Task 2: Snapshot Locality

- [x] Add failing scheduler tests for preferred host selection.
- [x] Add failing router restore test that prefers hosts holding the source snapshot.
- [x] Implement `ScheduleRequest.PreferredHosts` and snapshot locality lookup.
- [x] Run `go test ./internal/anvilmcp -run 'TestScheduler.*Preferred|TestRuntimeRouter.*Locality' -count=1`.

### Task 3: Retry, Failover, and Placement Reconciliation

- [x] Add failing router tests for failed spawn/restore retry on another eligible host.
- [x] Add failing reconciliation test that replaces stale placement from daemon `GET /vms`.
- [x] Implement `RuntimeRouterOptions.MaxAttempts`, host exclusion, and `ReconcilePlacements`.
- [x] Run `go test ./internal/anvilmcp -run 'TestRuntimeRouter.*Retry|TestRuntimeRouter.*Reconcile' -count=1`.

### Task 4: Profile Egress Allowlist

- [x] Add failing daemon egress policy tests for profile allow CIDR/host rules.
- [x] Implement profile egress JSON parser and iptables command planner.
- [x] Wire `profile` policy to profile-aware enforcer while keeping missing profile policy as no-op.
- [x] Run `go test ./cmd/goose-daemon -run 'Test.*Egress' -count=1`.

### Task 5: DNS Policy Enforcement

- [x] Add failing daemon egress policy tests for DNS allow and DNS deny rules.
- [x] Implement DNS TCP/UDP 53 allowlist commands and default DNS reject.
- [x] Run `go test ./cmd/goose-daemon -run 'Test.*DNS|Test.*Egress' -count=1`.

### Task 6: Per-VM Metrics and Histograms

- [x] Add failing daemon tests for `/metrics/vms`, duration counters/sums, queue depth.
- [x] Implement in-memory VM metric records and lifecycle observation helpers.
- [x] Increment metrics around create/restore/delete/snapshot/proxy paths.
- [x] Run `go test ./cmd/goose-daemon -run 'Test.*Metrics|Test.*Health' -count=1`.

### Task 7: OpenTelemetry Trace Export

- [x] Add failing OTEL exporter tests.
- [x] Implement optional HTTP trace exporter controlled by `ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_ENDPOINT`.
- [x] Emit lifecycle spans without token or secret attributes.
- [x] Run `go test ./cmd/goose-daemon -run 'Test.*OTEL|Test.*Trace' -count=1`.

### Task 8: IronClaw Compatibility Revalidation

- [x] Add failing schema compatibility tests for empty Gemini function declaration types.
- [x] Implement static validation for anvil MCP input structs.
- [x] Update IronClaw operation doc with current revalidation command and remaining upstream condition.
- [x] Run `go test ./internal/anvilmcp ./cmd/anvil-mcp -run 'Test.*IronClaw|Test.*ToolRegistration' -count=1`.

### Task 9: Final Verification

- [x] Run `go test -count=1 ./...`.
- [x] Run `go build ./cmd/goose-daemon`.
- [x] Run `go build ./cmd/anvil-mcp`.
- [x] Run `go build ./cmd/anvil-scheduler`.
- [x] Run `bash -n e2e_test.sh`.
- [x] Run `bash -n scripts/build_image.sh`.
- [x] Run `git diff --check`.
- [x] Search for `agent_token` and verify no new metrics/audit/trace/schema endpoint exposes it.
- [x] Run full KVM e2e: `go build -o anvil-daemon ./cmd/goose-daemon/` and `sudo bash e2e_test.sh`.
- [x] Commit, push, and create PR.

## Self-Review

- Spec coverage: user-selected items 1-8 map to Tasks 1-8. Task 9 covers verification and handoff.
- Placeholder scan: no task defers behavior with hidden TODOs; every behavior has an explicit file and test command.
- Type consistency: new placement/scheduler/router types extend existing `RuntimeHost`, `ScheduleRequest`, `Daemon`, and daemon metrics contracts without renaming existing public API.
