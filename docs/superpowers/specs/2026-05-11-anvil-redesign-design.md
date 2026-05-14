---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: superpowers:brainstorming
lifecycle_status: passed
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# anvil 프로젝트 재설계 설계서

## 요약

이 재설계는 ephemera 0.2.0 runtime 확장과 IronClaw MCP v1 adapter 작업 이후
anvil 프로젝트 문서와 아키텍처 언어를 정렬한다. 범위는 문서와 아키텍처
일관성에만 한정한다. daemon 동작, MCP tool 동작, snapshot/restore 동작,
API schema, runtime code는 변경하지 않는다.

`anvil`은 IronClaw와 ephemera를 결합하는 새 프로젝트다. `ephemera`는
이미 0.1.0/0.2.0이 릴리즈된 기반 runtime이며, GitHub 저장소와 Go 모듈 이름도
`ephemera`를 유지한다.

## 승인된 범위

재설계 범위는 **문서와 아키텍처 일관성**이다.

범위에 포함:

- anvil/ephemera/IronClaw 경계를 반영한 공식 문서 계층 정의.
- project-level domain context 문서 추가.
- README는 anvil 결합 프로젝트 관점으로 정리.
- ephemera 0.1.0/0.2.0 분석 문서는 ephemera runtime 문서임을 제목과 설명에
  명시.
- 재설계 run의 lifecycle evidence 기록.
- 문서 일관성과 기존 Go test 중심의 검증 유지.

범위에서 제외:

- runtime code 변경.
- daemon API 변경.
- MCP tool contract 변경.
- snapshot/restore 동작 변경.
- release/tag policy cleanup.
- workspace sync, snapshot MCP tool, HTTP MCP transport 같은 v2 기능 설계.

## 공식 문서 계층

프로젝트 문서는 다음 source-of-truth 순서를 따른다.

1. `AGENTS.md`: Codex 작업 규칙, source-of-truth 순서, 승인 규칙, 안전 제약.
2. `CONTEXT.md`: anvil/ephemera/IronClaw 경계, module boundary map, legacy term policy.
3. `README.md`: anvil 결합 프로젝트 개요, build, run, API, MCP 사용법.
4. `RELEASE_NOTES.md`: ephemera release와 anvil 통합 변경 이력.
5. `docs/analysis/`: ephemera 0.1.0/0.2.0 근거와 분석 보고서.
6. `docs/superpowers/`: spec, grill-me record, plan, review 등 lifecycle evidence.
7. `docs/lifecycle/`, `docs/operations/`: lifecycle run snapshot과 operation handoff.

문서가 충돌하면 `anvil`은 IronClaw+ephemera 결합 프로젝트, `ephemera`는 기반
runtime 릴리즈로 구분한다. ephemera 릴리즈 분석 문서의 제목과 본문을 anvil로
덮어쓰지 않는다.

## 도메인 아키텍처 경계

`CONTEXT.md`는 이 재설계의 공식 domain architecture 문서다. 다음 term과 경계를
정의한다.

| 용어 | 의미 | 소유 영역 |
|---|---|---|
| `anvil` | IronClaw와 ephemera를 결합하는 새 프로젝트 이름 | project-wide |
| `ephemera` | Firecracker MicroVM 기반 격리 runtime. 0.1.0/0.2.0 릴리즈 기준 구현 | `cmd/goose-daemon/`, `internal/*` |
| Core runtime | Firecracker MicroVM 기반 격리 agent runtime | `cmd/goose-daemon/`, `internal/storage/`, `internal/network/`, `internal/vm/` |
| Control plane daemon | VM lifecycle, snapshot, restore, agent proxy를 관리하는 host daemon | `cmd/goose-daemon/` |
| Guest agent | VM 내부 HTTP task runner | `cmd/goose-agent/` |
| Guest init | mount 준비와 guest agent 감시를 담당하는 VM 내부 PID 1 | `cmd/micro-init/` |
| MCP adapter | IronClaw가 ephemera daemon을 호출하는 얇은 stdio bridge | `cmd/anvil-mcp/`, `internal/anvilmcp/` |
| Session alias | MCP adapter의 in-memory `session_name -> vm_id` 편의 mapping | `internal/anvilmcp/` |
| Snapshot/restore | VM state persistence와 restore를 담당하는 daemon runtime capability | `cmd/goose-daemon/`, `internal/storage/`, `internal/vm/` |
| Profile | VM 생성 시 LLM config/secret을 선택하는 단위 | `configs/profiles/`, daemon VM create flow |

경계 규칙:

- `docs/analysis/`는 ephemera 릴리즈 evidence이며 anvil 프로젝트 문서와 역할이 다르다.
- `docs/superpowers/`는 lifecycle evidence이며 runtime state가 아니다.
- `configs/*.example` 파일은 secret이 없는 예시다.
- secret config file은 local에 남기고 git에서 제외한다.
- MCP v1은 얇은 runtime bridge다. workspace sync, snapshot MCP tool,
  persistent session, automatic VM cleanup은 이 재설계 범위 밖이다.

## 도메인 아키텍처 통과

승인된 domain boundary는 문서-facing 경계이며 runtime code 변경을 요구하지
않는다. domain term은 문서 ownership과 향후 planning boundary를 정하며, 이번
run에서 package 이동을 만들지 않는다.

승인된 경계:

- anvil은 IronClaw+ephemera 결합 프로젝트이고, ephemera는 기반 runtime이다.
- core runtime은 ephemera daemon, storage, network, VM package가 소유한다.
- guest runtime은 `goose-agent`와 `micro-init`이 소유한다.
- IronClaw 통합은 MCP adapter package가 소유한다.
- session alias는 MCP adapter process memory이며 persistent session state가 아니다.
- snapshot/restore는 daemon capability이며 MCP v1 범위가 아니다.
- `docs/analysis/`는 evidence이며 `AGENTS.md`, `CONTEXT.md`, README, 승인된
  lifecycle spec을 덮어쓰지 않는다.

이번 재설계는 되돌리기 어려운 runtime 또는 public API 결정을 도입하지 않으므로
별도 ADR은 필요하지 않다.

## 계획된 문서 변경

구현 plan은 문서와 governance artifact만 갱신한다.

- `CONTEXT.md` 추가.
- `AGENTS.md`에 `CONTEXT.md`를 source-of-truth hierarchy에 포함하고 재설계
  제약을 명시.
- `README.md`에서 anvil을 IronClaw+ephemera 결합 프로젝트로 설명. repository
  link는 `HardcoreMonk/anvil` 유지 가능.
- `RELEASE_NOTES.md`에서 ephemera release와 anvil 통합 미릴리즈 변경을 구분.
- `docs/analysis/README.md`에 분석 문서가 ephemera 0.1.0/0.2.0 runtime 분석임을
  명시.
- 이 redesign run의 lifecycle artifact에 승인 decision과 review evidence 기록.

## 검증

구현 plan에 필요한 검증:

```bash
/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh anvil --run 2026-05-11-anvil-redesign
git diff --check
go test ./...
```

최종 summary는 runtime 동작을 의도적으로 바꾸지 않았다고 명시해야 한다. runtime
file 변경이 필요해지면 구현 전에 redesign scope를 다시 승인받아야 한다.

## 후속 실행

다음은 이번 재설계 범위 밖의 명시적 follow-up 후보이다.

- public GitHub repository release/tag hygiene.
- workspace copy-in/out을 위한 MCP v2 설계.
- MCP snapshot/restore tool.
- HTTP MCP transport.
- runtime module refactoring.

## 계획 설계 검토

결과: 통과, blocking issue 없음.

이 재설계에는 UI 범위가 없다. screen, page, component, frontend framework 변경,
visual design system 변경을 계획하지 않았다. mockup도 생성하지 않았다.
Codex zone registry에서 이 project는 `design_md: false`이므로 `DESIGN.md`는
없고 필요하지 않다.

design review는 information architecture, document discoverability,
lifecycle gate clarity로 축소했다.

| 항목 | 점수 | 결과 |
|---|---:|---|
| 정보 구조 | 10/10 | canonical document order가 명확하며 `CONTEXT.md`가 적절한 역할을 맡는다. |
| 상태 범위 | 9/10 | lifecycle gate state가 명확하고 computed lifecycle JSON이 snapshot output임을 표시한다. |
| 사용자 흐름 | 9/10 | 개발자는 README, Codex는 `AGENTS.md`, design/planning은 `docs/superpowers/`로 진입한다. |
| AI 일반화 위험 | 10/10 | 생성 UI나 generic visual pattern이 범위에 없다. |
| 디자인 시스템 정렬 | 9/10 | backend/docs 재설계에 별도 visual design system은 필요 없고 Markdown convention으로 충분하다. |
| 반응형과 접근성 | 9/10 | Markdown heading, table, fenced command는 terminal과 web renderer에서 scan하기 쉽다. |
| 미해결 설계 결정 | 10/10 | 구현 planning 전 unresolved IA 또는 gate decision이 없다. |

Design review 범위 밖:

- visual mockup 또는 product UI.
- `DESIGN.md` 생성.
- runtime behavior, daemon API, MCP tool 변경.
- public release/tag hygiene.

이미 존재하는 근거:

- `AGENTS.md` project guidance.
- `docs/analysis/` evidence documents.
- `docs/superpowers/` 아래 승인된 MCP v1 spec과 plan.
- redesign spec, grill-me record, lifecycle snapshot, handoff draft.

deferred design debt가 없으므로 `TODOS.md` 갱신은 필요하지 않다.

## 승인된 브레인스토밍 결정

- 범위: 문서와 아키텍처 일관성만.
- 공식 구조: `CONTEXT.md`를 domain glossary와 boundary map으로 둔 역할 분리 문서.
- naming policy: 현재 사용자-facing 문서는 `anvil` 사용. `ephemera`는
  repository/path metadata와 historical evidence로 유지.
- 완료 기준: `AGENTS.md`, `CONTEXT.md`, `README.md`, lifecycle artifact,
  analysis index가 일관되고 runtime code는 변경되지 않으며 검증 명령이 통과.
- 접근: Canonical Docs First.

## 생명주기 gate 근거

- Stage: `superpowers:brainstorming`
- 상태: `passed`
- Approved by: 사용자가 scope, canonical document structure, domain architecture
  boundary, completion criteria, Canonical Docs First 접근을 대화에서 승인했다.
- 근거: 사용자가 대화에서 이 written spec을 검토하고 승인했다.
- Stage: `domain-architecture`
- 상태: `passed`
- Approved by: 사용자가 `CONTEXT.md`를 domain glossary와 boundary map으로 승인했고
  위 accepted domain term을 승인했다.
- 근거: 이 spec의 Domain Architecture Pass section.
