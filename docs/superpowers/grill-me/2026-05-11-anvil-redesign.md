---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: grill-me
lifecycle_status: passed
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# Grill-Me 초안: anvil

질문은 한 번에 하나씩 진행한다. 각 질문에는 Codex 권장 답변과 저장소 근거를
함께 남긴다.

## 도메인 아키텍처 질문

### Q1. 공개 동작과 API freeze

**맥락:** 이번 재설계는 문서와 아키텍처 일관성만 다룬다. README,
CONTEXT, release note, lifecycle 문서를 갱신하는 과정에서 runtime 동작,
daemon API, MCP tool 의미를 실수로 재해석할 수 있다.

**질문:** 이번 재설계 중 어떤 동작, schema, public API가 그대로 유지되어야
하는가?

**권장 답변:** 모든 runtime 동작과 public API 계약은 변경하지 않는다.
freeze 범위에는 `POST /vms`, `/vms/{id}/tasks`, snapshot/restore endpoint,
`cmd/anvil-mcp`의 다섯 MCP tool 계약, config 우선순위, session alias 의미,
`anvil_stop_vm`과 `anvil_delete_vm`의 구분이 포함된다.

**사용자 답변:** 승인됨.

**결정:** 현재 구현과 승인된 MCP v1 spec이 runtime 동작의 기준이다. 이번
재설계는 문서를 명확히 할 수 있지만 daemon API, MCP tool, config, session,
snapshot, restore, stop, delete 동작 변경을 도입하지 않는다.

**Spec 갱신:** redesign spec의 out-of-scope에 runtime/API 변경 금지를
유지하고, 검증은 문서와 기존 테스트 중심으로 둔다.

**다음 분기:** 생성된 lifecycle artifact가 project guidance 또는 구현 사실과
충돌할 때의 source-of-truth 규칙을 확인한다.

### Q2. Source-of-truth 충돌 규칙

**맥락:** 이 재설계에는 project guidance, domain context, README, analysis
문서, lifecycle spec/plan, lifecycle run JSON snapshot이 함께 존재한다. 특히
생성 snapshot은 승인된 project-local evidence보다 늦을 수 있다.

**질문:** 생성 lifecycle artifact가 guidance 또는 code와 충돌할 때 어떤
source를 기준으로 삼아야 하는가?

**권장 답변:** 문서 충돌은 `AGENTS.md -> CONTEXT.md -> README.md ->
RELEASE_NOTES.md -> docs/analysis -> docs/superpowers` 순서로 판단한다. 실제
runtime 동작은 구현과 승인된 MCP v1 spec을 기준으로 한다.
`docs/lifecycle/runs/*.json`은 계산된 lifecycle snapshot이며 source of truth가
아니다.

**사용자 답변:** 승인됨.

**결정:** `docs/lifecycle/runs/*.json`은 승인된 gate evidence에 맞춰 갱신할
수 있지만 `AGENTS.md`, `CONTEXT.md`, README, 구현 사실, 승인 spec을 덮어쓰지
않는다.

**Spec 갱신:** `docs/lifecycle/`은 canonical hierarchy 안에서 accepted project
docs와 lifecycle evidence 아래의 snapshot output으로 둔다.

**다음 분기:** 문서 전용 재설계의 release/operate blocker 기준을 확인한다.

### Q3. Release와 operate blocker 기준

**맥락:** 이번 재설계는 문서와 아키텍처 일관성 변경만 허용한다. 문서 정리가
runtime 동작, release policy, secret handling 변경으로 확장되지 않도록 명시적
blocker가 필요하다.

**질문:** 어떤 검증 실패가 release 또는 operate 진입을 막아야 하는가?

**권장 답변:** 다음 중 하나라도 발생하면 release 또는 operate 진입을
막는다. runtime code diff 발생, `CONTEXT.md` 누락 또는 domain boundary 공란,
README가 anvil 결합 프로젝트 관점을 반영하지 않음, ephemera 분석 문서가
ephemera 릴리즈 문서임을 드러내지 않음, lifecycle lint 실패,
`git diff --check` 실패, `go test ./...` 실패, secret/token/private config/local
brainstorming metadata가 commit set에 포함됨.

**사용자 답변:** 승인됨.

**결정:** 재설계는 문서 전용 범위를 유지하고, 비어 있지 않은 `CONTEXT.md`
domain boundary를 갖고, anvil/ephemera/IronClaw 경계를 정규화하며, lifecycle lint,
markdown/whitespace diff check, 기존 Go test를 통과하고, private/local artifact를
제외해야 release/operate에 진입할 수 있다.

**Spec 갱신:** 구현 plan은 verification과 release criteria에 이 blocker를
포함해야 한다.

**다음 분기:** grill-me 완료. 남은 질문은 구현 계획 세부 사항이다.

## Release control 질문

- 어떤 생성 artifact를 review 후 accepted record로 삼고, 어떤 artifact를
  computed snapshot으로 남길 것인가?
- 어떤 verification failure가 release 또는 operate 진입을 막아야 하는가?

## 생명주기 gate 근거

- Stage: `grill-me`
- 상태: `passed`
- Approved by: 사용자가 Q1 public behavior/API freeze, Q2 source-of-truth
  conflict rule, Q3 release/operate blocker criteria를 대화에서 승인했다.
- 근거: Q1-Q3 decision이 이 파일에 기록되어 있다.
