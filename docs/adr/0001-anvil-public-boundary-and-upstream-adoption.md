# ADR-0001: anvil 공개 경계와 upstream ephemera 채택 상태 관리

> **상태:** accepted
> **날짜:** 2026-05-16
> **대상:** anvil downstream repository

---

## 맥락

anvil은 `steve-seungeui/ephemera`에서 시작한 downstream fork이며, upstream
ephemera는 Firecracker runtime engine 역할을 계속 수행한다. anvil은 이 runtime을
IronClaw 전용 MCP execution layer로 사용하는 별도 통합 프로젝트다.

따라서 upstream ephemera 변경은 계속 병합해야 하지만, 모든 upstream public API를
anvil 공개 표면으로 그대로 받아들이면 안 된다. 대표적으로 upstream ephemera
v0.3.1의 운영 안정성 변경은 anvil에 유용하지만, flock `agent_tokens` 응답 노출은
anvil의 `agent_token` 노출 정책과 충돌한다.

기존 README와 RELEASE_NOTES만으로는 다음 판단을 안정적으로 남기기 어렵다.

- 어떤 기능이 anvil 공개 기능인지
- 어떤 upstream 변경을 그대로 채택했는지
- 어떤 변경을 anvil 정책에 맞게 수정했는지
- 어떤 변경을 보류하거나 제외했는지

---

## 결정

anvil은 공개 경계와 설계 결정을 다음 문서로 관리한다.

1. `docs/PUBLIC_RELEASE_BOUNDARY.md`
   - anvil 공개 포함/조건부 포함/제외 표면을 정의한다.
   - upstream ephemera 변경 채택 상태 값을 정의한다.
   - 릴리즈 전 경계 확인 항목을 제공한다.

2. `docs/ADR_INDEX.md`
   - 현재 적용되는 ADR과 역사 기록 ADR을 구분한다.
   - 개별 ADR보다 최신 적용 상태를 요약한다.

3. `docs/adr/*.md`
   - 장기 설계 결정의 원문을 보존한다.
   - 공개 경계, token/auth, MCP tool 계약, runtime lifecycle 의미 변경은 ADR로 남긴다.

upstream ephemera 변경은 다음 상태로 분류한다.

- `adopted`: anvil 정책과 충돌하지 않아 그대로 채택
- `adapted`: runtime 가치는 채택하되 public/API 보안 표면은 수정
- `excluded`: anvil 공개 경계와 충돌해 제외
- `deferred`: 가치가 있으나 현재 release scope 밖
- `historical`: 과거 분석 근거로만 유지

`agent_token` 노출 정책은 유지한다. `POST /vms` 응답 외부에서 `agent_token`을
새로 노출하는 upstream 변경은 anvil에서 `adapted` 또는 `excluded`로 처리한다.

---

## 결과

긍정적 결과:

- upstream ephemera 병합 시 runtime 변경과 anvil 공개 표면 변경을 분리해서 판단할 수 있다.
- README가 제품 설명에 집중하고, 공개 경계 판단은 별도 문서가 담당한다.
- token/auth, MCP tool surface, runtime lifecycle 같은 장기 계약 변경 이력이 남는다.

비용:

- 공개 기능을 추가할 때 README/RELEASE_NOTES 외에 공개 경계와 ADR 적용 상태도 확인해야 한다.
- upstream merge마다 채택 상태를 명시해야 하므로 문서 관리 비용이 늘어난다.

---

## 검증 기준

- `docs/PUBLIC_RELEASE_BOUNDARY.md`가 anvil 공개 포함/조건부 포함/제외 표면을 설명한다.
- `docs/ADR_INDEX.md`가 ADR-0001의 현재 상태를 `accepted`로 표시한다.
- `CONTEXT.md`와 `README.md`가 새 문서 체계를 참조한다.
- `agent_token` 노출 정책이 기존 불변 조건과 충돌하지 않는다.
