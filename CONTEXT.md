# anvil Context

## Purpose

`anvil` is the official product and project name for this repository. It provides a
Firecracker MicroVM based runtime for isolated AI agent execution and a thin MCP
adapter for IronClaw integration.

The GitHub repository and local directory can remain named `ephemera`. In current-facing
documentation, `ephemera` means repository identity, path identity, or historical source
material. It is not the product name.

## Source Of Truth

For documentation conflicts, use this order:

1. `AGENTS.md`
2. `CONTEXT.md`
3. `README.md`
4. `RELEASE_NOTES.md`
5. `docs/analysis/`
6. `docs/superpowers/`
7. `docs/lifecycle/` and `docs/operations/`

Runtime behavior is controlled by implementation and accepted lifecycle specs. The
computed lifecycle JSON files under `docs/lifecycle/runs/` do not override accepted
project-local evidence.

## Domain Glossary

| Term | Meaning | Owning Area |
|---|---|---|
| `anvil` | Official product/project name | Project-wide |
| `ephemera` | Repository/path/module legacy name | Repository metadata |
| Core runtime | Firecracker MicroVM based isolated agent runtime | `cmd/goose-daemon/`, `internal/storage/`, `internal/network/`, `internal/vm/` |
| Control plane daemon | Host daemon that manages VM lifecycle, snapshots, restore, and agent proxying | `cmd/goose-daemon/` |
| Guest agent | VM-side task runner exposed over HTTP inside the guest | `cmd/goose-agent/` |
| Guest init | VM-side PID 1 process that prepares mounts and supervises the guest agent | `cmd/micro-init/` |
| MCP adapter | Thin stdio bridge used by IronClaw to call the anvil daemon | `cmd/anvil-mcp/`, `internal/anvilmcp/` |
| Session alias | In-memory `session_name -> vm_id` convenience mapping in the MCP adapter | `internal/anvilmcp/` |
| Snapshot/restore | Daemon runtime capability for VM state persistence and restore | `cmd/goose-daemon/`, `internal/storage/`, `internal/vm/` |
| Profile | VM creation-time LLM config and secret selection | `configs/profiles/`, daemon VM create flow |

## Boundary Rules

- `docs/analysis/` is evidence, not canonical product truth.
- `docs/superpowers/` is lifecycle evidence, not runtime state.
- `docs/lifecycle/runs/*.json` is computed lifecycle snapshot output.
- `configs/*.example` files are non-secret examples only.
- Secret config files remain local and ignored.
- MCP v1 remains a thin runtime bridge.

## Frozen Runtime Contracts

This redesign must not change:

- daemon endpoint semantics, including `POST /vms`, `DELETE /vms/{vm_id}`,
  `POST /vms/{vm_id}/tasks`, `GET /vms/{vm_id}/health`,
  `POST /vms/{vm_id}/stop`, snapshot, and restore endpoints.
- the five MCP v1 tools: `anvil_spawn_vm`, `anvil_run_task`,
  `anvil_get_vm_health`, `anvil_stop_vm`, and `anvil_delete_vm`.
- config precedence: defaults, config file, then environment variables.
- session alias semantics: process-memory `session_name -> vm_id` mapping only.
- `anvil_stop_vm` and `anvil_delete_vm` distinction.
- snapshot/restore behavior.

## Follow-Up Candidates

- Public release and tag hygiene.
- MCP v2 workspace copy-in/out design.
- MCP snapshot/restore tools.
- HTTP MCP transport.
- Runtime module refactoring.
