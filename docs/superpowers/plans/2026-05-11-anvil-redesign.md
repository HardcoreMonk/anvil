---
lifecycle_run: 2026-05-11-anvil-redesign
lifecycle_stage: superpowers:writing-plans
lifecycle_status: passed
generated_by: lifecycle-redesign-start
generated_at: 2026-05-11T00:00:00
redaction_applied: true
---
# anvil 프로젝트 재설계 구현 계획

> agentic worker용 지침: 이 계획은 task-by-task 실행을 전제로 작성되었다.
> 현재 문서는 구현 완료 후 한국어 보존 기록으로 재작성되었으며, 원래의 긴
> 코드/문서 스니펫 대신 승인된 범위, 작업 순서, 검증 기준을 남긴다.

## 목표

runtime 동작을 변경하지 않고 anvil의 공식 문서, 도메인 언어, lifecycle
artifact를 정렬한다.

## 아키텍처

이 작업은 문서 전용 재설계다.

- `AGENTS.md`: Codex 작업 계약.
- `CONTEXT.md`: domain glossary와 boundary map.
- `README.md`: 현재 사용자/개발자 진입점.
- `RELEASE_NOTES.md`: release별 변경 이력.
- `docs/superpowers/`, `docs/operations/`, `docs/lifecycle/`: 승인 gate와
  lifecycle evidence.

runtime code, daemon API, MCP tool 계약, config 우선순위, session alias 의미,
snapshot/restore semantics는 freeze한다.

## 기술 스택

- Markdown
- Codex lifecycle artifact
- 기존 Go test suite
- `lifecycle-lint.sh`

## 소스 참조

- 설계서: `docs/superpowers/specs/2026-05-11-anvil-redesign-design.md`
- Grill-me 기록: `docs/superpowers/grill-me/2026-05-11-anvil-redesign.md`
- Project guidance: `AGENTS.md`
- 사용자 문서: `README.md`, `RELEASE_NOTES.md`
- 분석 색인: `docs/analysis/README.md`
- Lifecycle lint: `/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh`

## 범위 점검

이 계획은 문서와 아키텍처 일관성만 다룬다. 다음 파일/계약은 변경하지 않는다.

- `cmd/**`
- `internal/**`
- `go.mod`
- `go.sum`
- runtime config example
- daemon endpoint semantics
- MCP tool behavior

runtime file 변경이 필요해지면 중단하고 사용자에게 scope 재승인을 받아야 한다.

## 파일 구조

생성:

- `CONTEXT.md`: 공식 domain glossary, module boundary map, naming policy,
  frozen behavior list.

수정:

- `AGENTS.md`: `CONTEXT.md`를 source-of-truth order에 추가하고 문서 전용
  재설계 guardrail 명시.
- `README.md`: 현재 제품명을 `anvil`로 정리하고 repository link는
  `HardcoreMonk/ephemera` 유지.
- `RELEASE_NOTES.md`: 현재 release wording은 `anvil`로 정리하고 historical
  readability 유지.
- `docs/analysis/README.md`: 0.1.0의 `ephemera` 코드 경로 표현은 historical evidence임을
  명시.
- `docs/superpowers/plans/2026-05-11-anvil-redesign.md`: 완료된 plan evidence 기록.
- `docs/operations/2026-05-11-anvil-redesign-handoff.md`: release/operate 인계 기록.
- `docs/lifecycle/runs/2026-05-11-anvil-redesign.json`: 승인 artifact에 맞춘 stage
  상태 snapshot.

수정 금지:

- `cmd/**`
- `internal/**`
- `go.mod`
- `go.sum`
- `configs/*.example`
- `configs/*.yaml`
- `.superpowers/**`

## 작업 계획

### 작업 1. 사전 점검과 domain context

- 구현 시작 commit을 기록한다.
- `CONTEXT.md`를 추가한다.
- `CONTEXT.md`에 목적, source-of-truth, domain glossary, boundary rule,
  frozen runtime contract, follow-up candidate를 포함한다.
- `CONTEXT.md`가 비어 있지 않고 핵심 section을 포함하는지 확인한다.

검증:

```bash
test -s CONTEXT.md
rg -n "Domain Glossary|Frozen Runtime Contracts|anvil|ephemera" CONTEXT.md
```

### 작업 2. Project guidance 정렬

- `AGENTS.md` source-of-truth order에 `CONTEXT.md`를 포함한다.
- generated lifecycle JSON이 accepted project-local evidence를 덮어쓰지 않는다고
  명시한다.
- runtime/API 변경 금지 guardrail을 workflow에 추가한다.

검증:

```bash
rg -n "CONTEXT.md|runtime|MCP|snapshot|restore" AGENTS.md
```

### 작업 3. README 제품 정체성 정렬

- README 제목과 소개를 `anvil` 중심으로 정리한다.
- repository 이름 `ephemera`는 저장소/경로 의미로만 설명한다.
- API endpoint, 환경 변수, 명령어 이름은 구현 계약 그대로 유지한다.
- MCP adapter 사용법과 Go 1.25+ 요구 사항을 반영한다.

검증:

```bash
rg -n "anvil|HardcoreMonk/ephemera|EPHEMERA_PUBLIC_URL|anvil-mcp" README.md
```

### 작업 4. Release note와 analysis index 정렬

- `RELEASE_NOTES.md` current-facing wording을 `anvil`로 정리한다.
- historical release 정보는 이해 가능한 형태로 유지한다.
- `docs/analysis/README.md`에 legacy naming policy를 명시한다.

검증:

```bash
rg -n "anvil|ephemera|historical|0.2.0" RELEASE_NOTES.md docs/analysis/README.md
```

### 작업 5. Lifecycle artifact와 handoff 정리

- redesign lifecycle artifact에 승인된 범위, 검증, risk, handoff 정보를 기록한다.
- `docs/operations/2026-05-11-anvil-redesign-handoff.md`를 operate 단계 기록으로
  정리한다.
- generated snapshot과 accepted docs의 역할 차이를 명시한다.

검증:

```bash
/data/projects/codex-zone/codex-project-mgmt/scripts/lifecycle-lint.sh anvil --run 2026-05-11-anvil-redesign
```

### 작업 6. 최종 검증

- whitespace/markdown diff를 점검한다.
- 기존 Go test를 실행한다.
- runtime diff guard로 `cmd/`, `internal/`, `go.mod`, `go.sum` 변경이 없는지
  확인한다.
- private config, secret, local artifact가 commit set에 포함되지 않았는지
  확인한다.

검증:

```bash
git diff --check
go test ./...
git diff --name-only -- cmd internal go.mod go.sum
git status --short
```

## 완료 기준

- `CONTEXT.md`가 존재하고 domain boundary와 frozen runtime contract를 포함한다.
- `AGENTS.md`, `README.md`, `RELEASE_NOTES.md`가 공식 제품명 `anvil`을 사용한다.
- legacy `ephemera`는 repository/path/module/historical context로만 사용된다.
- lifecycle artifact와 handoff가 승인 상태를 반영한다.
- runtime code 변경 없이 검증이 통과한다.

## 자체 검토

- 문서 전용 scope가 유지되었는가: 예.
- runtime behavior freeze가 명시되었는가: 예.
- source-of-truth order가 명확한가: 예.
- secret/private artifact가 제외되는가: 예.
- 후속 release/tag hygiene이 별도 작업으로 분리되었는가: 예.

## 계획 엔지니어링 검토

결과: 통과.

- 아키텍처 경계: documentation-only.
- Data flow impact: 없음.
- Test strategy: lifecycle lint, `git diff --check`, `go test ./...`, runtime diff guard.
- Blocking issue: 없음.

검토 중 반영된 사항:

1. release evidence가 code review 이전에 확정된 것처럼 보이지 않도록 handoff
   heading을 조정했다.
2. `docs/lifecycle/runs/*.json`은 computed snapshot이며 accepted docs를
   override하지 않는다고 명시했다.
3. runtime diff guard 대상에 `cmd/`, `internal/`, `go.mod`, `go.sum`을 포함했다.

## 생명주기 gate 근거

- Stage: `superpowers:writing-plans`
- 상태: `passed`
- Approved by: user
- 근거: 이 구현 계획은 plan-eng-review 준비가 완료된 상태로 기록되었다.
- Stage: `plan-eng-review`
- 상태: `passed`
- 근거: 위 계획 엔지니어링 검토 section.
