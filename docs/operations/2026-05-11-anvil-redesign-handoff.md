---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: release
lifecycle_status: draft
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# Operation Handoff Draft: anvil

## Release Scope Candidate

- Documentation-only redesign for anvil project identity, domain glossary, canonical
  document hierarchy, and lifecycle evidence.
- Runtime behavior, daemon API, MCP tool contracts, config precedence, session alias
  semantics, and snapshot/restore behavior are unchanged.

## Current Lifecycle Stage

Operate has not been entered. This handoff remains pre-release until implementation,
code-review, and final verification complete. `plan-eng-review` has passed.

## Verification Candidates

- `/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh anvil --run 2026-05-11-anvil-redesign`
- `git diff --check`
- `go test ./...`
- Runtime diff guard against `cmd/`, `internal/`, `go.mod`, and `go.sum`

## Audit Candidates

- Confirm generated artifacts do not contain secrets or stale internal references.
- Confirm canonical docs and computed snapshots are clearly separated.

## Blockers

- Implementation pending.
- `code-review` pending.
- Final verification pending.

## Warnings

- Scan summaries are bounded and redacted.
- Generated drafts may omit project-specific decisions until manually completed.

## Residual Risk Candidates

- Legacy docs may be stale.
- Scan summaries are bounded and redacted.

## Next Action

- Complete the design brief, then rerun lifecycle lint before release or operate entry.

## Lifecycle Gate Evidence

- Stage: `release`
- Status: `draft`
- Approved by: `not-approved`
- Evidence: Generated draft artifact. This gate is not passed yet.
