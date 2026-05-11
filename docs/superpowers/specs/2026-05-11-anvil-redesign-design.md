---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: superpowers:brainstorming
lifecycle_status: draft
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# anvil Project Redesign Design

## Summary

This redesign aligns the anvil project documentation and architecture language after
the 0.2.0 runtime expansion and the IronClaw MCP v1 adapter work. It is a
documentation and architecture consistency pass only. It does not change daemon
behavior, MCP tool behavior, snapshot/restore behavior, API schemas, or runtime code.

The official product/project name is `anvil`. The GitHub repository and local path may
remain `ephemera`, but that name is treated as a repository name, not the product name.

## Approved Scope

The redesign scope is **documentation plus architecture consistency**.

In scope:

- Define the canonical document hierarchy for anvil.
- Add a project-level domain context document.
- Normalize official current-facing docs to use `anvil`.
- Preserve legacy `Ephemera` wording only as historical evidence in older analysis
  documents.
- Record lifecycle evidence for the redesign run.
- Keep verification focused on documentation consistency and existing Go tests.

Out of scope:

- Runtime code changes.
- Daemon API changes.
- MCP tool contract changes.
- Snapshot/restore behavior changes.
- Release/tag policy cleanup.
- New v2 feature design such as workspace sync, snapshot MCP tools, or HTTP MCP
  transport.

## Canonical Document Hierarchy

Project documentation follows this source-of-truth order:

1. `AGENTS.md` - Codex work rules, source-of-truth order, approval rules, and safety
   constraints.
2. `CONTEXT.md` - anvil domain glossary, module boundary map, and legacy term policy.
3. `README.md` - external user and developer entrypoint for product overview, build,
   run, API, and MCP usage.
4. `RELEASE_NOTES.md` - release-by-release change history.
5. `docs/analysis/` - evidence and analysis reports. These can preserve historical
   terms when they describe older states.
6. `docs/superpowers/` - lifecycle evidence such as specs, grill-me records, plans,
   and reviews.
7. `docs/lifecycle/` and `docs/operations/` - lifecycle run snapshots and operation
   handoffs.

When documents conflict, current-facing product docs use `anvil` as the product name.
`ephemera` is allowed when referring to the Git repository, Go module, local directory,
or historical source material.

## Domain Architecture Boundary

`CONTEXT.md` will become the canonical domain architecture document for this redesign.
It should define the following terms and boundaries.

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

Boundary rules:

- `docs/analysis/` is evidence, not canonical product truth.
- `docs/superpowers/` is lifecycle evidence, not runtime state.
- `configs/*.example` files are non-secret examples only.
- Secret config files remain local and ignored.
- MCP v1 remains a thin runtime bridge. Workspace sync, snapshot MCP tools,
  persistent sessions, and automatic VM cleanup stay out of scope for this redesign.

## Planned Documentation Changes

The implementation plan for this redesign should update only documentation and
governance artifacts:

- Add `CONTEXT.md`.
- Update `AGENTS.md` to include `CONTEXT.md` in the source-of-truth hierarchy and keep
  the redesign constraints explicit.
- Update `README.md` so the current product name and product description are `anvil`.
  Repository references can continue to point at `HardcoreMonk/ephemera`.
- Update `RELEASE_NOTES.md` so current release notes use `anvil` while historical
  release notes remain understandable.
- Update `docs/analysis/README.md` to state that legacy `Ephemera` terms are preserved
  as historical evidence.
- Update this redesign run's lifecycle artifacts with accepted decisions and review
  evidence.

## Verification

Required verification for the implementation plan:

```bash
/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh anvil --run 2026-05-11-anvil-redesign
git diff --check
go test ./...
```

The final summary must state that no runtime behavior was intentionally changed. If any
runtime file changes become necessary, this redesign scope must be re-approved before
implementation.

## Follow-Up Runs

These are explicit follow-up candidates, not part of this redesign:

- Release/tag hygiene for the public GitHub repository.
- MCP v2 design for workspace copy-in/out.
- MCP snapshot/restore tools.
- HTTP MCP transport.
- Runtime module refactoring.

## Approved Brainstorming Decisions

- Scope: documentation plus architecture consistency only.
- Canonical structure: role-separated docs with `CONTEXT.md` as domain glossary and
  boundary map.
- Naming policy: current-facing docs use `anvil`; `ephemera` remains repository/path
  metadata and historical evidence.
- Completion criteria: `AGENTS.md`, `CONTEXT.md`, `README.md`, lifecycle artifacts, and
  analysis index are consistent; runtime code is unchanged; verification commands pass.
- Approach: Canonical Docs First.

## Lifecycle Gate Evidence

- Stage: `superpowers:brainstorming`
- Status: `draft`
- Approved by: user approved scope, canonical document structure, domain architecture
  boundary, completion criteria, and Canonical Docs First approach in conversation.
- Evidence: This written spec is pending final user file review before implementation
  planning begins.
