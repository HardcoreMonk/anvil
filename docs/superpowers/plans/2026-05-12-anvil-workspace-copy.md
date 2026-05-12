# Anvil Workspace Copy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add single-file workspace copy-in/copy-out between anvil MCP clients and running ephemera VMs.

**Architecture:** The daemon extends the existing VM agent proxy with `/workspace`, while the guest `goose-agent` owns `/workspace` path validation and file IO. The MCP adapter exposes `anvil_copy_in` and `anvil_copy_out` using `vm_id` or `session_name`, reusing the current session store and daemon client patterns.

**Tech Stack:** Go, `net/http`, Model Context Protocol Go SDK, existing ephemera daemon and guest agent.

---

### Task 1: Guest Agent Workspace Endpoint

**Files:**
- Modify: `cmd/goose-agent/main.go`
- Modify: `cmd/goose-agent/main_test.go`

- [ ] Write failing tests for workspace path validation, PUT/GET round trip, traversal rejection, and missing file handling.
- [ ] Run `go test ./cmd/goose-agent -count=1` and confirm the new tests fail because `/workspace` behavior does not exist.
- [ ] Implement `/workspace` handler with `/workspace` root, safe relative path normalization, parent directory creation, and `GET`/`PUT` method handling.
- [ ] Run `go test ./cmd/goose-agent -count=1` and confirm it passes.

### Task 2: Daemon Workspace Proxy

**Files:**
- Modify: `cmd/goose-daemon/api.go`

- [ ] Add route handling for `/vms/{vm_id}/workspace` before generic `DELETE /vms/{vm_id}` handling.
- [ ] Reuse `proxyAgentEndpoint` so daemon injects the per-VM agent token.
- [ ] Ensure `PUT` and `GET` are the only accepted methods.
- [ ] Run `go test ./cmd/goose-daemon -count=1`.

### Task 3: MCP Daemon Client and Tools

**Files:**
- Modify: `internal/anvilmcp/daemon_client.go`
- Modify: `internal/anvilmcp/daemon_client_test.go`
- Modify: `internal/anvilmcp/tools.go`
- Modify: `internal/anvilmcp/tools_test.go`

- [ ] Write failing daemon client tests for `PUT /vms/{id}/workspace?path=...` and `GET /vms/{id}/workspace?path=...`.
- [ ] Write failing tool tests for `CopyIn` and `CopyOut` using `session_name`.
- [ ] Add daemon interface methods `CopyIn` and `CopyOut`.
- [ ] Implement MCP input/output types, path validation, and tool methods.
- [ ] Run `go test ./internal/anvilmcp -count=1`.

### Task 4: MCP Registry and Smoke

**Files:**
- Modify: `cmd/anvil-mcp/main.go`
- Modify: `cmd/anvil-mcp/main_test.go`
- Modify: `scripts/anvil-mcp-smoke.go`

- [ ] Write failing registry test expectations for `anvil_copy_in` and `anvil_copy_out`.
- [ ] Register both tools with concise descriptions.
- [ ] Extend the smoke script to call `anvil_copy_in` and `anvil_copy_out` after spawn.
- [ ] Run `go test ./cmd/anvil-mcp -count=1`.

### Task 5: Documentation and Verification

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture/mcp-architecture.md`
- Modify: `docs/architecture/service-logic.md`

- [ ] Document the daemon workspace endpoint and MCP tools in Korean.
- [ ] Run `go test ./...`.
- [ ] Run `go build -o anvil-mcp ./cmd/anvil-mcp`.
- [ ] Run `go build -o anvil-daemon ./cmd/goose-daemon`.
- [ ] Run strict MCP smoke with Google provider when daemon is available.
- [ ] Commit and push.

## Self Review

- Spec coverage: each requirement in the workspace copy design maps to Tasks 1-5.
- Placeholder scan: no task uses unresolved placeholders.
- Type consistency: tool names are `anvil_copy_in` and `anvil_copy_out`; daemon route is `/vms/{vm_id}/workspace?path=...`.
