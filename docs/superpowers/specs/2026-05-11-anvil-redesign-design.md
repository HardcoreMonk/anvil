---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: superpowers:brainstorming
lifecycle_status: draft
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# Existing Project Redesign Design Brief Draft: anvil

## Context

This artifact starts an existing-project redesign lifecycle from a bounded repository scan. It is not an approved redesign spec; it is a draft brief that must be completed with human-reviewed domain architecture, grill-me decisions, and plan reviews before implementation.

## Problem

- Existing projects often contain current guidance, legacy notes, generated lifecycle records, and code facts in the same tree.
- A file list alone does not decide which source is canonical, which facts are stale, or which domain terms should shape code boundaries.
- Generated artifacts must stay draft until their lifecycle gate evidence is explicitly accepted.

## Goals

- Create a reviewable redesign starting point with current document, package, and context signals.
- Force `domain-architecture` before `grill-me` so domain language can constrain folders, modules, and public interfaces.
- Separate candidate evidence from approved decisions, release criteria, and operate handoff.

## Non-Goals

- No runtime feature, schema, deployment, or API behavior changes are implied by this generated draft.
- No FE/BE skill refactoring is in scope unless a later approved plan adds it explicitly.
- No generated artifact becomes canonical project truth without human review and gate evidence.

## Evidence From Current Repo

### Document Signals

- `AGENTS.md`: Anvil — Codex project guidance, Source Of Truth, Project Shape, Workflow
- `README.md`: Ephemera, Architecture, VM Provisioning Flow, Snapshot/Restore Flow
- `docs/analysis/01-source-line-analysis.md`: Ephemera 소스 줄 단위 분석 보고서, 전체 구조, `go.mod`, `cmd/goose-daemon/main.go`
- `docs/analysis/02-junior-developer-report.md`: Ephemera 주니어 개발자 실무 투입 보고서, 한 문장 요약, 아키텍처 분해, 프론트엔드
- `docs/analysis/03-non-technical-report.md`: Ephemera 비전공자용 설명 보고서, Ephemera가 하는 일, 등장인물, Control Plane
- `docs/analysis/04-v0.2.0-diff-from-v0.1.0.md`: anvil 0.2.0 변경 분석: 0.1.0 대비, 기준, 변경 규모, 핵심 변화
- `docs/analysis/05-source-line-analysis-v0.2.0.md`: anvil 0.2.0 소스 분석, 문서 목적, 전체 구조, `cmd/micro-init/main.go`
- `docs/analysis/06-junior-developer-report-v0.2.0.md`: anvil 0.2.0 주니어 개발자용 분석 보고서, 한 줄 요약, 먼저 알아야 할 배경, 꼭 이해해야 하는 새 구성요소
- `docs/analysis/07-non-technical-report-v0.2.0.md`: anvil 0.2.0 비기술 보고서, 요약, 무엇이 달라졌나, 1. VM을 더 안전하게 종료할 수 있다
- `docs/analysis/README.md`: anvil 분석 문서 색인, 기준 정보, 0.1.0 문서, 0.2.0 문서
- `docs/superpowers/plans/2026-05-11-anvil-ironclaw-mcp-v1.md`: anvil IronClaw MCP v1 Implementation Plan, Source References, Scope Check, File Structure
- `docs/superpowers/specs/2026-05-11-anvil-ironclaw-mcp-v1-design.md`: anvil IronClaw MCP v1 Design, 1. 목적, 2. 결정 사항, 3. 비목표

### Package And Automation Signals

- `go.mod`

### Context Document Signals

- `CONTEXT.md`: missing
- `CONTEXT-MAP.md`: missing
- `docs/adr`: missing

## Redaction Summary

- Redactions: `{"args": 0, "internal_ref": 79, "local_path": 0, "secret": 0}`

## Lifecycle Contract

- Reference: `docs/codex-lifecycle-control-plane.md` or the target project's equivalent contract.

## Evidence-Based Design Boundaries

| Boundary | Candidate Evidence | Required Design Output |
|---|---|---|
| Project guidance | `AGENTS.md`, `CONTEXT.md`, `CONTEXT-MAP.md` when present | Current rules, legacy rules, and unknowns separated |
| Long-lived decisions | `docs/adr/` when present | ADR candidates for any irreversible redesign decision |
| Runtime facts | Code, migrations, API docs, package manifests | Facts referenced by path rather than duplicated as new truth |
| Lifecycle records | `docs/superpowers/`, `docs/operations/`, `docs/lifecycle/runs/` | Process evidence, not canonical runtime state |

## Domain Architecture Draft

- Extract domain terms from current project guidance, context docs, ADRs, API docs, and model/schema code.
- Map each accepted term to owning folder, module boundary, public function/API signature, persistence boundary, and adapter boundary.
- Mark ambiguous synonyms, legacy terms, and rejected terms before `grill-me` questions begin.
- Record accepted new terms in `CONTEXT.md` or an ADR only after approval.

## Required Human Synthesis

- Replace candidate evidence with an approved information architecture and domain boundary map.
- Answer open decisions one at a time through `grill-me`, including Codex's recommended answer and evidence.
- Add `plan-design-review` and `plan-eng-review` conclusions before treating the plan as executable.
- Keep this run in `draft` until the lifecycle gate evidence names the approver and accepted evidence.

## Open Decisions

- Confirm whether this redesign is documentation-only, architecture-only, or runtime-affecting. Recommended default: documentation and architecture evidence only.
- Confirm which files are canonical sources of domain language. Recommended default: `AGENTS.md`, `CONTEXT.md`, `docs/adr/`, and code/schema paths.
- Confirm release and operate criteria before any implementation work starts.

## Lifecycle Gate Evidence

- Stage: `superpowers:brainstorming`
- Status: `draft`
- Approved by: `not-approved`
- Evidence: Generated draft artifact. This gate is not passed yet.
