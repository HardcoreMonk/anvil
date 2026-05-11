---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: grill-me
lifecycle_status: passed
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# Grill-Me Draft: anvil

Ask one question at a time. For each question, include Codex's recommended answer and the repo evidence behind it.

## Domain Architecture Questions

### Q1. Public behavior and API freeze

**Context:** This redesign is documentation and architecture consistency only. While
updating README, CONTEXT, release notes, and lifecycle docs, wording could accidentally
reinterpret runtime behavior, daemon API behavior, or MCP tool semantics.

**Question:** Which behavior, schema, or public API must remain unchanged during this
redesign?

**Recommended Answer:** All runtime behavior and public API contracts remain unchanged.
The freeze includes daemon endpoints such as `POST /vms`, `/vms/{id}/tasks`,
snapshot/restore endpoints, the five `cmd/anvil-mcp` MCP tool contracts, config
precedence, session alias semantics, and the distinction between `anvil_stop_vm` and
`anvil_delete_vm`.

**User Answer:** Approved.

**Decision:** Current implementation and accepted MCP v1 spec remain authoritative for
runtime behavior. This redesign may clarify documentation but must not introduce daemon
API, MCP tool, config, session, snapshot, restore, stop, or delete behavior changes.

**Spec Update:** Keep runtime/API changes in the redesign spec's out-of-scope section
and keep verification focused on docs plus existing tests.

**Next Branch:** Confirm source-of-truth behavior when generated lifecycle artifacts
conflict with project guidance or implementation facts.

### Q2. Source-of-truth conflict rule

**Context:** This redesign has project guidance, future domain context, README,
analysis documents, lifecycle specs and plans, and lifecycle run JSON snapshots. These
documents can conflict, especially when generated snapshots lag behind accepted
project-local evidence.

**Question:** Which existing source is canonical when generated lifecycle artifacts
conflict with guidance or code?

**Recommended Answer:** Use `AGENTS.md -> CONTEXT.md -> README.md ->
RELEASE_NOTES.md -> docs/analysis -> docs/superpowers` for documentation conflicts.
For actual runtime behavior, the implementation and the accepted MCP v1 spec remain
authoritative. `docs/lifecycle/runs/*.json` is a computed lifecycle snapshot, not a
source of truth.

**User Answer:** Approved.

**Decision:** `docs/lifecycle/runs/*.json` may be updated to match accepted gate
evidence, but it does not override `AGENTS.md`, `CONTEXT.md`, README, implementation
facts, or accepted specs.

**Spec Update:** Keep `docs/lifecycle/` in the canonical hierarchy as lifecycle
snapshot output, below accepted project docs and lifecycle evidence.

**Next Branch:** Confirm release and operate blockers for this documentation-only
redesign.

### Q3. Release and operate blocker criteria

**Context:** This redesign only permits documentation and architecture consistency
changes. The implementation plan needs explicit blockers so documentation cleanup does
not expand into runtime behavior, release policy, or secret-handling changes.

**Question:** What verification failure should block release or operate entry?

**Recommended Answer:** Block release or operate entry if any of these occur:
runtime code diff appears; `CONTEXT.md` is missing or has an empty domain boundary;
current-facing documents do not use `anvil` as the product name; lifecycle lint fails;
`git diff --check` fails; `go test ./...` fails; or secrets, tokens, private config,
or local brainstorming metadata enter the commit set.

**User Answer:** Approved.

**Decision:** The redesign cannot release or enter operate unless it remains
documentation-only, has a non-empty `CONTEXT.md` domain boundary, normalizes current
product naming to `anvil`, passes lifecycle lint, passes markdown/whitespace diff
checks, passes existing Go tests, and excludes private/local artifacts.

**Spec Update:** The implementation plan must include these blockers in verification
and release criteria.

**Next Branch:** Grill-me complete. Remaining questions are implementation planning
details.

## Release Control Questions

- Which generated artifacts can become accepted records after review, and which remain computed snapshots?
- What verification failure should block release or operate entry?

## Lifecycle Gate Evidence

- Stage: `grill-me`
- Status: `passed`
- Approved by: user approved Q1 public behavior/API freeze, Q2 source-of-truth
  conflict rule, and Q3 release/operate blocker criteria in conversation.
- Evidence: Q1-Q3 decisions recorded in this file.
