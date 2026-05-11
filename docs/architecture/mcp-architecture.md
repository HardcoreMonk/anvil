# anvil MCP Architecture

## Status

- Baseline: `v0.2.0`
- MCP version: v1 stdio adapter
- Entrypoint: `cmd/anvil-mcp`
- Runtime target: anvil control plane daemon HTTP API

MCP v1 is a thin bridge. It does not own VM lifecycle semantics; it maps MCP tool
calls to the daemon API and keeps a small in-memory session alias map.

## System View

```text
IronClaw or other MCP client
  |
  | stdio MCP transport
  v
cmd/anvil-mcp
  |
  | internal/anvilmcp.Tools
  v
internal/anvilmcp.DaemonClient
  |
  | HTTP + optional Bearer token
  v
anvil control plane daemon
  |
  | Firecracker, guest agent proxy, snapshots
  v
MicroVM runtime
```

The adapter is intentionally process-local and stateless except for optional
`session_name` aliases.

## Component Responsibilities

| Component | Files | Responsibility |
|---|---|---|
| MCP server entrypoint | `cmd/anvil-mcp/main.go` | Load config, create daemon client, create tool handlers, register MCP tools, run stdio transport |
| Config loader | `internal/anvilmcp/config.go` | Load defaults, optional YAML config, and environment overrides |
| Daemon client | `internal/anvilmcp/daemon_client.go` | Call the control plane daemon over HTTP and preserve daemon response bodies |
| Tool layer | `internal/anvilmcp/tools.go` | Validate MCP input, resolve VM identity, apply task timeout, map tools to daemon client methods |
| Session store | `internal/anvilmcp/session_store.go` | Maintain in-memory `session_name -> vm_id` aliases for one adapter process |
| Config example | `configs/anvil-mcp.yaml.example` | File-based adapter configuration template |

## Config Model

Default values:

| Field | Default |
|---|---|
| `daemon_url` | `http://127.0.0.1:3000` |
| `default_timeout_seconds` | `300` |
| Config file path | `configs/anvil-mcp.yaml` |

Load order:

```text
defaults
  -> optional YAML config file
  -> environment variables
```

Environment variables:

| Variable | Meaning |
|---|---|
| `ANVIL_MCP_CONFIG` | Override config file path |
| `ANVIL_DAEMON_URL` | Override daemon base URL |
| `ANVIL_API_TOKEN` | Bearer token used for daemon requests |
| `ANVIL_MCP_DEFAULT_TIMEOUT` | Default timeout for `anvil_run_task` in seconds |

Validation:

- `daemon_url` must be non-empty.
- `daemon_url` must use `http` or `https`.
- `daemon_url` must include a host.
- `default_timeout_seconds` must be positive.
- `ANVIL_MCP_DEFAULT_TIMEOUT` must parse as a positive integer.

## Tool Contract

| MCP tool | Daemon call | Purpose |
|---|---|---|
| `anvil_spawn_vm` | `POST /vms` | Create a VM and optionally bind a `session_name` alias |
| `anvil_run_task` | `POST /vms/{vm_id}/tasks` | Run a prompt in a VM through the daemon agent proxy |
| `anvil_get_vm_health` | `GET /vms/{vm_id}/health` | Return guest agent health through the daemon proxy |
| `anvil_stop_vm` | `POST /vms/{vm_id}/stop` | Ask the guest agent to stop gracefully |
| `anvil_delete_vm` | `DELETE /vms/{vm_id}` | Destroy VM resources and release matching session aliases |

### `anvil_spawn_vm`

Input:

```json
{
  "profile": "optional-profile-name",
  "session_name": "optional-local-alias"
}
```

Output:

```json
{
  "vm_id": "vm-...",
  "guest_ip": "10.0.1.x",
  "agent_url": "http://...",
  "profile": "optional-profile-name",
  "session_name": "optional-local-alias"
}
```

Behavior:

- Rejects duplicate `session_name` before calling the daemon.
- Calls `POST /vms`.
- If alias binding fails after daemon spawn, attempts best-effort daemon cleanup
  with `DELETE /vms/{vm_id}`.
- Does not expose `agent_token` in MCP output. The daemon proxy owns guest token use.

### `anvil_run_task`

Input:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set",
  "prompt": "required prompt",
  "timeout_seconds": 300
}
```

Behavior:

- Requires non-empty `prompt`.
- Rejects negative timeouts.
- Rejects timeouts above 24 hours.
- Resolves VM identity.
- `vm_id` takes priority over `session_name`.
- Uses `timeout_seconds` when provided, otherwise the configured default timeout.
- Calls `POST /vms/{vm_id}/tasks`.

Output:

```json
{
  "status_code": 200,
  "body": "{...daemon response body...}"
}
```

### `anvil_get_vm_health`

Input:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set"
}
```

Behavior:

- Resolves VM identity.
- Calls `GET /vms/{vm_id}/health`.
- Returns daemon status code and raw response body.

### `anvil_stop_vm`

Input:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set"
}
```

Behavior:

- Resolves VM identity.
- Calls `POST /vms/{vm_id}/stop`.
- Does not remove the session alias.
- Does not delete host-side VM resources.

This distinction matters: stop asks the guest agent HTTP server to shut down.
Delete destroys the VM resource from the host control plane.

### `anvil_delete_vm`

Input:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set"
}
```

Behavior:

- Resolves VM identity.
- Calls `DELETE /vms/{vm_id}`.
- Removes all local aliases that point to the deleted VM after a successful daemon
  response.

## Session Alias Model

`SessionStore` is an in-memory convenience map:

```text
session_name -> vm_id
```

Rules:

- Empty session names are invalid.
- Empty VM IDs are invalid.
- Duplicate session names are rejected.
- `vm_id` takes priority when both `vm_id` and `session_name` are provided.
- Unknown session names are rejected before a daemon call.
- `anvil_delete_vm` removes aliases for the deleted VM.
- `anvil_stop_vm` does not remove aliases.
- Aliases are lost when the `anvil-mcp` process exits.

The adapter does not persist session state to disk.

## Daemon Client Behavior

`DaemonClient` builds HTTP requests from the configured base URL and tool-specific
paths.

Request behavior:

- Adds `Authorization: Bearer <ANVIL_API_TOKEN>` when a token is configured.
- Adds `Content-Type: application/json` for requests with JSON bodies.
- Uses the incoming MCP call context, including task timeout when set.

Response behavior:

- For 2xx responses, returns status code and body.
- For non-2xx responses, returns `DaemonError` with the daemon status code and raw
  body.
- The tool layer does not rewrite daemon errors into a new domain model.

This keeps the adapter thin and makes daemon behavior visible to the MCP client.

## Security Model

| Concern | Current behavior |
|---|---|
| Daemon authentication | Adapter uses `ANVIL_API_TOKEN` as the daemon Bearer token |
| Guest agent token | Not exposed by MCP output; daemon proxy injects it |
| Session aliases | Process-local memory only |
| Secrets | Config file can contain `api_token`; local config files should stay out of git |
| Transport | MCP v1 uses stdio between client and adapter |

The adapter assumes the daemon URL and API token are trusted local/operator
configuration.

## Failure Behavior

| Failure | Result |
|---|---|
| Missing explicit config file | Config load error |
| Missing default config file | Allowed, defaults/env are used |
| Invalid daemon URL | Config load error |
| Duplicate session name | Tool validation error before daemon call |
| Unknown session name | Tool validation error before daemon call |
| Daemon 4xx/5xx | `DaemonError` with status and body |
| Daemon connection failure | Send request error |
| Spawn succeeds but alias binding fails | Best-effort VM delete, then return error |

## v1 Non-Goals

MCP v1 intentionally does not implement:

- Workspace copy-in/copy-out.
- Snapshot creation tools.
- Snapshot restore tools.
- Persistent session database.
- Automatic VM cleanup on adapter exit.
- HTTP MCP transport.
- Multi-daemon routing.
- Long-running async task orchestration.

These are candidates for future MCP v2 design work, not hidden behavior in v1.

## Source References

- `cmd/anvil-mcp/main.go`
- `internal/anvilmcp/config.go`
- `internal/anvilmcp/daemon_client.go`
- `internal/anvilmcp/session_store.go`
- `internal/anvilmcp/tools.go`
- `configs/anvil-mcp.yaml.example`
- `docs/superpowers/specs/2026-05-11-anvil-ironclaw-mcp-v1-design.md`
