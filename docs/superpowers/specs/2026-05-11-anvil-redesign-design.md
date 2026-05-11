---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: superpowers:brainstorming
lifecycle_status: passed
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

## Domain Architecture Pass

The accepted domain boundary is documentation-facing and does not require runtime code
changes. Domain terms determine documentation ownership and future planning boundaries,
not new package moves in this run.

Accepted boundaries:

- Product identity is `anvil`; repository identity can remain `ephemera`.
- Core runtime remains owned by the daemon, storage, network, and VM packages.
- Guest runtime remains owned by `goose-agent` and `micro-init`.
- IronClaw integration remains owned by the MCP adapter packages.
- Session alias means MCP adapter process memory only; it is not persistent session
  state.
- Snapshot/restore remains a daemon capability; it is not part of MCP v1.
- `docs/analysis/` remains evidence and does not override `AGENTS.md`, `CONTEXT.md`,
  README, or accepted lifecycle specs.

No ADR is required for this pass because the redesign does not introduce a
hard-to-reverse runtime or public API decision.

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

## Plan Design Review

Result: passed, no blocking issues.

This redesign has no UI scope: no screens, pages, components, frontend framework
changes, or visual design system changes are planned. No mockups were generated.
`DESIGN.md` is absent and not required because the Codex zone registry marks this
project as `design_md: false`.

Design review was reduced to information architecture, document discoverability, and
lifecycle gate clarity.

| Pass | Rating | Result |
|---|---:|---|
| Information Architecture | 10/10 | Canonical document order is explicit and puts `CONTEXT.md` in the right role. |
| Interaction State Coverage | 9/10 | Lifecycle gate states are explicit; computed lifecycle JSON is identified as snapshot output. |
| User Journey | 9/10 | Developer readers enter through README, Codex enters through `AGENTS.md`, and design/planning enters through `docs/superpowers/`. |
| AI Slop Risk | 10/10 | No generated UI or generic visual pattern is in scope. |
| Design System Alignment | 9/10 | No project visual design system is required for this backend/docs redesign. Markdown conventions are sufficient. |
| Responsive And Accessibility | 9/10 | Markdown headings, tables, and fenced commands are scan-friendly across terminal and web renderers. |
| Unresolved Design Decisions | 10/10 | No unresolved IA or gate decisions remain before implementation planning. |

Not in scope for this design review:

- Visual mockups or product UI.
- `DESIGN.md` creation.
- Runtime behavior, daemon API, or MCP tool changes.
- Public release/tag hygiene.

What already exists:

- `AGENTS.md` project guidance.
- `docs/analysis/` evidence documents.
- Accepted MCP v1 spec and plan under `docs/superpowers/`.
- Redesign spec, grill-me record, lifecycle snapshot, and handoff draft.

No `TODOS.md` update is needed because no deferred design debt was found.

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
- Status: `passed`
- Approved by: user approved scope, canonical document structure, domain architecture
  boundary, completion criteria, and Canonical Docs First approach in conversation.
- Evidence: User reviewed and approved this written spec in conversation.
- Stage: `domain-architecture`
- Status: `passed`
- Approved by: user approved `CONTEXT.md` as the domain glossary and boundary map and
  approved the accepted domain terms above.
- Evidence: Domain Architecture Pass section in this spec.
