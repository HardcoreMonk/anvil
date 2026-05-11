---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: superpowers:writing-plans
lifecycle_status: passed
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# anvil Project Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Align anvil's canonical documentation, domain language, and lifecycle artifacts without changing runtime behavior.

**Architecture:** This is a documentation-only redesign. `AGENTS.md` remains the Codex work contract, `CONTEXT.md` becomes the domain glossary and boundary map, README becomes the current-facing user/developer entrypoint, and lifecycle files record the accepted gates. Runtime code, daemon APIs, MCP tool contracts, config precedence, session alias behavior, and snapshot/restore semantics remain frozen.

**Tech Stack:** Markdown, Codex lifecycle artifacts, existing Go test suite for regression verification.

---

## Source References

- Design spec: `docs/superpowers/specs/2026-05-11-anvil-redesign-design.md`
- Grill-me record: `docs/superpowers/grill-me/2026-05-11-anvil-redesign.md`
- Project guidance: `AGENTS.md`
- Existing user docs: `README.md`, `RELEASE_NOTES.md`
- Analysis index: `docs/analysis/README.md`
- Lifecycle lint: `/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh`

## Scope Check

This plan covers one subsystem: documentation and architecture consistency. It must not edit `cmd/`, `internal/`, `go.mod`, `go.sum`, runtime config examples, daemon endpoints, or MCP tool behavior. If any runtime file needs to change, stop and return to the redesign spec for user re-approval.

## File Structure

Create:

- `CONTEXT.md`: canonical domain glossary, module boundary map, naming policy, and frozen behavior list.

Modify:

- `AGENTS.md`: add `CONTEXT.md` to source-of-truth order; clarify documentation-only redesign constraints.
- `README.md`: change current-facing product identity from Ephemera to anvil; update GitHub links to `HardcoreMonk/ephemera`; keep environment variable names unchanged.
- `RELEASE_NOTES.md`: make current release wording use anvil while preserving historical readability.
- `docs/analysis/README.md`: clarify that 0.1.0 `Ephemera` wording is historical evidence.
- `docs/superpowers/plans/2026-05-11-anvil-redesign.md`: mark completed plan evidence.
- `docs/operations/2026-05-11-anvil-redesign-handoff.md`: update release/operate draft with actual verification and residual risk after implementation.
- `docs/lifecycle/runs/2026-05-11-anvil-redesign.json`: update stored stage states to match accepted artifacts.

Do not modify:

- `cmd/**`
- `internal/**`
- `go.mod`
- `go.sum`
- `configs/*.example`
- `configs/*.yaml`
- `.superpowers/**`

---

### Task 1: Preflight And Domain Context

**Files:**
- Create: `CONTEXT.md`

- [ ] **Step 1: Record the implementation start commit**

Run:

```bash
git rev-parse --short HEAD > /tmp/anvil-redesign-start.sha
cat /tmp/anvil-redesign-start.sha
```

Expected:

```text
<short commit sha>
```

- [ ] **Step 2: Create `CONTEXT.md`**

Create `CONTEXT.md` with this content:

```markdown
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
```

- [ ] **Step 3: Verify context is non-empty**

Run:

```bash
test -s CONTEXT.md
rg -n "Domain Glossary|Frozen Runtime Contracts|anvil|ephemera" CONTEXT.md
```

Expected:

```text
CONTEXT.md contains the required sections and both naming terms.
```

- [ ] **Step 4: Commit**

Run:

```bash
git add CONTEXT.md
git commit -m "docs: add anvil context glossary"
```

---

### Task 2: Project Guidance Alignment

**Files:**
- Modify: `AGENTS.md`

- [ ] **Step 1: Update source-of-truth order**

In `AGENTS.md`, replace the current `## Source Of Truth` numbered list with:

```markdown
1. `AGENTS.md`
2. `CONTEXT.md`
3. `README.md`
4. `RELEASE_NOTES.md`
5. `docs/analysis/`
6. `docs/superpowers/`
7. `docs/lifecycle/`, `docs/operations/`
8. `.superpowers/` local brainstorming artifacts
```

Add this paragraph after the list:

```markdown
`CONTEXT.md` owns domain glossary, module boundary, and legacy naming policy.
`docs/lifecycle/runs/*.json` is computed lifecycle snapshot output and does not
override accepted project-local evidence.
```

- [ ] **Step 2: Clarify redesign guardrails**

Add this bullet under `## Workflow`:

```markdown
- 문서/아키텍처 정합성 redesign은 runtime code, daemon API, MCP tool contract,
  snapshot/restore behavior를 변경하지 않는다. 해당 변경이 필요하면 새 spec으로
  scope를 다시 승인받는다.
```

- [ ] **Step 3: Verify guidance references `CONTEXT.md`**

Run:

```bash
rg -n "CONTEXT.md|docs/lifecycle|runtime code|MCP tool contract" AGENTS.md
```

Expected: all four terms appear.

- [ ] **Step 4: Commit**

Run:

```bash
git add AGENTS.md
git commit -m "docs: align anvil project guidance"
```

---

### Task 3: README Product Identity

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update README title, badges, and intro**

Replace the top block through the first paragraph with:

```markdown
# anvil

[![CI](https://github.com/HardcoreMonk/ephemera/actions/workflows/ci.yml/badge.svg)](https://github.com/HardcoreMonk/ephemera/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/HardcoreMonk/ephemera)](https://github.com/HardcoreMonk/ephemera/releases)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Firecracker](https://img.shields.io/badge/Firecracker-v1.15.1-FF4500?logo=amazonaws&logoColor=white)](https://github.com/firecracker-microvm/firecracker)

**Enterprise Control Plane for Ephemeral AI Agents via Firecracker MicroVMs**

anvil orchestrates isolated, KVM-backed MicroVM environments for agentic AI workloads. Each VM runs [Goose](https://github.com/aaif-goose/goose) as an autonomous agent inside a minimal Debian guest, fully contained within hardware VM boundaries and wiped on termination.

The repository is still named `ephemera`; the official project and product name is `anvil`.
```

- [ ] **Step 2: Update current-facing product labels**

In current-facing README prose and diagrams, replace human-facing `Ephemera` product
labels with `anvil`. Do not rename environment variables such as
`EPHEMERA_PUBLIC_URL`, `EPHEMERA_API_TOKEN`, or `EPHEMERA_API_TOKENS`.
Do not rename the repository directory `ephemera` or the Go module. Do update local
example binary names from `ephemera-daemon` to `anvil-daemon`.

Required replacements:

```text
Ephemera Control Plane -> anvil Control Plane
Ephemera will -> anvil will
Ephemera has -> anvil has
Ephemera uses -> anvil uses
ephemera-daemon -> anvil-daemon
```

- [ ] **Step 3: Update clone/build commands**

Replace the clone block with:

```bash
git clone https://github.com/HardcoreMonk/ephemera.git
cd ephemera
go build -o anvil-daemon ./cmd/goose-daemon/
```

- [ ] **Step 4: Verify remaining legacy terms are intentional**

Run:

```bash
rg -n "\\bEphemera\\b|steve-seungeui|ephemera-daemon" README.md
```

Expected: no output.

Run:

```bash
rg -n "EPHEMERA_" README.md
```

Expected: environment variable references still appear.

- [ ] **Step 5: Commit**

Run:

```bash
git add README.md
git commit -m "docs: update README for anvil identity"
```

---

### Task 4: Release Notes And Analysis Index

**Files:**
- Modify: `RELEASE_NOTES.md`
- Modify: `docs/analysis/README.md`

- [ ] **Step 1: Update `RELEASE_NOTES.md` current-facing naming**

Replace:

```markdown
**Ephemera** completes the single-host feature set.
```

with:

```markdown
**anvil** completes the single-host feature set.
```

Replace:

```markdown
**Ephemera** is an enterprise control plane for running ephemeral AI agents inside Firecracker MicroVMs.
```

with:

```markdown
**anvil** is an enterprise control plane for running ephemeral AI agents inside Firecracker MicroVMs.
```

- [ ] **Step 2: Add naming note to release notes**

Add this paragraph below `# Unreleased`:

```markdown
Project naming note: `anvil` is the official product/project name. `ephemera` remains
the GitHub repository and Go module name.
```

- [ ] **Step 3: Clarify analysis index historical wording**

In `docs/analysis/README.md`, extend the 0.1.0 paragraph to:

```markdown
0.1.0 문서는 초기 소스 분석 결과다. 문서 제목과 일부 표현에는 당시 코드베이스 명칭인 Ephemera가 남아 있다. 이 표현은 historical evidence로 보존하며, 현재 공식 제품명은 `anvil`이다.
```

- [ ] **Step 4: Verify naming policy**

Run:

```bash
rg -n "\\*\\*Ephemera\\*\\*|Project naming note|historical evidence" RELEASE_NOTES.md docs/analysis/README.md
```

Expected:

```text
No **Ephemera** entries.
Project naming note appears.
historical evidence appears.
```

- [ ] **Step 5: Commit**

Run:

```bash
git add RELEASE_NOTES.md docs/analysis/README.md
git commit -m "docs: clarify anvil release naming"
```

---

### Task 5: Lifecycle Artifacts And Handoff

**Files:**
- Modify: `docs/operations/2026-05-11-anvil-redesign-handoff.md`
- Modify: `docs/lifecycle/runs/2026-05-11-anvil-redesign.json`

- [ ] **Step 1: Update operation handoff release scope**

Keep the `## Release Scope Candidate` heading and replace only its content with:

```markdown
- Documentation-only redesign for anvil project identity, domain glossary, canonical
  document hierarchy, and lifecycle evidence.
- Runtime behavior, daemon API, MCP tool contracts, config precedence, session alias
  semantics, and snapshot/restore behavior are unchanged.
```

- [ ] **Step 2: Update handoff verification section**

Keep the `## Verification Candidates` heading and replace only its content with:

```markdown
- `/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh anvil --run 2026-05-11-anvil-redesign`
- `git diff --check`
- `go test ./...`
- Runtime diff guard against `cmd/`, `internal/`, `go.mod`, and `go.sum`
```

Do not introduce exact `## Release Scope` or `## Verification` headings in this task.
The lifecycle linter treats those headings as release evidence, which must wait until
after implementation, code review, and final verification.

- [ ] **Step 3: Update handoff blockers and current stage**

Set `## Current Lifecycle Stage` to:

```markdown
Operate has not been entered. This handoff remains pre-release until implementation,
code-review, and final verification complete. `plan-eng-review` has passed.
```

Set `## Blockers` to:

```markdown
- Implementation pending.
- `code-review` pending.
- Final verification pending.
```

- [ ] **Step 4: Update lifecycle JSON stages**

In `docs/lifecycle/runs/2026-05-11-anvil-redesign.json`, set:

```json
"superpowers:writing-plans": "passed",
"plan-eng-review": "passed"
```

Keep these stages pending or draft:

```json
"implement": "pending",
"code-review": "pending",
"release": "draft",
"operate": "draft"
```

- [ ] **Step 5: Run lifecycle lint**

Run:

```bash
/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh anvil --run 2026-05-11-anvil-redesign
```

Expected:

```text
errors: 0
warnings: 0
```

- [ ] **Step 6: Commit**

Run:

```bash
git add docs/operations/2026-05-11-anvil-redesign-handoff.md docs/lifecycle/runs/2026-05-11-anvil-redesign.json
git commit -m "docs: update anvil redesign lifecycle state"
```

---

### Task 6: Final Verification

**Files:**
- Read-only verification across the repository.

- [ ] **Step 1: Check for forbidden runtime diffs**

Run:

```bash
START=$(cat /tmp/anvil-redesign-start.sha)
git diff --name-only "$START"..HEAD -- cmd internal go.mod go.sum 'configs/*.example' | tee /tmp/anvil-redesign-runtime-diff.txt
test ! -s /tmp/anvil-redesign-runtime-diff.txt
```

Expected: no output and exit code 0.

- [ ] **Step 2: Check lifecycle state**

Run:

```bash
/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh anvil --run 2026-05-11-anvil-redesign
```

Expected:

```text
errors: 0
warnings: 0
```

- [ ] **Step 3: Check markdown whitespace**

Run:

```bash
git diff --check
```

Expected: no output.

- [ ] **Step 4: Run Go tests**

Run:

```bash
go test ./...
```

Expected:

```text
ok or [no test files] for all packages
```

- [ ] **Step 5: Confirm product naming**

Run:

```bash
rg -n "\\bEphemera\\b|steve-seungeui|ephemera-daemon" README.md RELEASE_NOTES.md
rg -n "Project naming note|Domain Glossary|Frozen Runtime Contracts" RELEASE_NOTES.md CONTEXT.md
```

Expected:

```text
First command has no output.
Second command finds the naming note and CONTEXT sections.
```

- [ ] **Step 6: Commit verification-only lifecycle updates if needed**

If the operation handoff needs final verification output, edit only
`docs/operations/2026-05-11-anvil-redesign-handoff.md` and
`docs/lifecycle/runs/2026-05-11-anvil-redesign.json`. Keep release/verification
headings in candidate form until code-review and release approval are complete, then
run:

```bash
git add docs/operations/2026-05-11-anvil-redesign-handoff.md docs/lifecycle/runs/2026-05-11-anvil-redesign.json
git commit -m "docs: record anvil redesign verification"
```

---

## Self-Review

Spec coverage:

- Canonical document hierarchy: Task 1 and Task 2.
- `CONTEXT.md` domain glossary and boundary map: Task 1.
- README current-facing naming: Task 3.
- Release notes and analysis historical evidence: Task 4.
- Lifecycle and handoff evidence: Task 5.
- Runtime behavior freeze and blockers: Task 1, Task 5, Task 6.

Placeholder scan:

- No prohibited placeholder steps are present.

Scope check:

- This plan remains documentation-only. Runtime code and public API changes are
  explicitly forbidden and verified in Task 6.

## Plan Eng Review

Result: passed, no unresolved blockers.

Review focus:

- Architecture boundary: documentation-only. No runtime package, daemon API, MCP tool,
  config, or snapshot behavior changes are allowed.
- Data flow: documentation source-of-truth flows from `AGENTS.md` to `CONTEXT.md` to
  README/release notes, while lifecycle JSON remains computed snapshot output.
- Failure modes: accidental runtime edits, premature release gate evidence, stale
  product naming, missing `CONTEXT.md`, and private/local artifact leakage.
- Verification: lifecycle lint, markdown diff check, Go tests, runtime diff guard, and
  product naming checks.
- Rollback: all planned changes are documentation commits and can be reverted without
  guest/runtime state migration.

Findings fixed in this review:

1. Handoff headings originally risked creating release evidence before code-review.
   The plan now keeps `Release Scope Candidate` and `Verification Candidates` headings
   until release approval.
2. README identity checks were too narrow. The plan now checks for remaining
   `Ephemera`, `steve-seungeui`, and `ephemera-daemon` current-facing references.
3. Runtime diff guard now quotes the `configs/*.example` pathspec so shell expansion
   cannot alter the intended git pathspec.

No ADR is required because this review does not introduce a hard-to-reverse runtime,
API, or data model decision.

## Lifecycle Gate Evidence

- Stage: `superpowers:writing-plans`
- Status: `passed`
- Approved by: plan derived from approved redesign spec, domain-architecture pass,
  grill-me Q1-Q3 decisions, and plan-design-review.
- Evidence: This implementation plan is complete and ready for `plan-eng-review`
  before execution.
- Stage: `plan-eng-review`
- Status: `passed`
- Approved by: engineering review completed in this session.
- Evidence: Plan Eng Review section above.
