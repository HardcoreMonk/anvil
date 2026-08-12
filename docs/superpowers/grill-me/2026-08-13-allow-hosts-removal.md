# `allow_hosts` 제거 grill-me 기록

**날짜:** 2026-08-13

**topic:** `allow-hosts-removal`

**설계:**
[`2026-08-13-allow-hosts-removal-design.md`](../specs/2026-08-13-allow-hosts-removal-design.md)

## 압박 질문과 결정

### Q1. struct field만 지우면 충분한가?

아니다. Go `json.Unmarshal`은 unknown field를 무시하므로 오래된 profile이 성공한 것처럼
보이면서 의도한 allow rule만 잃는다. `allow_hosts` raw key를 decode boundary에서
명시적으로 거부한다.

### Q2. 빈 배열은 실제 rule을 만들지 않는데 허용해도 되는가?

허용하지 않는다. 빈 key도 stale template/automation의 증거이며 다음 편집에서 위험한
값이 다시 들어갈 수 있다. key 존재 자체를 migration failure로 본다.

### Q3. `DisallowUnknownFields`가 더 단순하지 않은가?

더 넓은 계약 변경이다. 제거된 단일 위험 field를 닫는 목적과 무관한 metadata까지
거부할 수 있다. 이번에는 explicit legacy rejection을 사용하고 전체 strict schema는
별도 결정으로 남긴다.

### Q4. parse error 전에 migration error를 내면 malformed JSON을 잘못 분류할 수 있는가?

raw map unmarshal 자체가 먼저 JSON 문법을 검증한다. 문법이 깨졌으면 parse error를,
유효한 object에 `allow_hosts`가 있으면 removal error를 반환한다.

### Q5. JSON root가 배열이나 scalar이면 어떻게 되는가?

현재 typed struct unmarshal의 오류 semantics를 유지해야 한다. raw legacy probe와 typed
unmarshal을 모두 통과해야 하며, root-shape 회귀 테스트는 기존/추가 test로 보호한다.

### Q6. migration error에 host value를 넣어야 디버깅이 쉽지 않은가?

값은 불필요하다. field 이름과 대체 field만으로 수정 가능하며 hostname이 내부 목적지를
노출할 수 있으므로 value는 반복하지 않는다.

### Q7. 기존 `allow_hosts` validation helper를 남겨 호환 계층으로 쓸 이유가 있는가?

없다. apply path가 제거된 helper는 잘못된 호환 기대와 dead code만 남긴다. shared domain
charset helper는 `allow_sni` 전용 설명으로 정리한다.

### Q8. 과거 spec과 ADR의 deprecation 문장을 모두 제거해야 하는가?

과거 lifecycle 기록은 당시 사실이므로 보존한다. ADR은 현재 decision status를 업데이트하고
canonical/current 운영 문서는 “removed”로 맞춘다.

### Q9. 이 변경만으로 다음 tag를 만들어도 되는가?

아니다. upstream version alignment, Web dependency, deployment host, branch governance가
독립 gate다. 제거 완료는 필요조건이지 release 승인 자체가 아니다.

### Q10. rollback은 무엇인가?

코드 rollback은 field/apply/warning을 되살리지만 보안상 열화다. release 전 발견된
호환성 문제는 profile을 `allow_sni`/`allow_cidrs`로 마이그레이션하는 것이 우선이다.
이미 배포된 artifact rollback은 별도 release 승인 없이 수행하지 않는다.

## Grill 결과

- 열린 설계 질문: 없음
- design blocker: 없음
- 핵심 고정 결정: explicit key rejection, empty/null 포함, global strictness 확대 금지,
  value 비노출, tag 금지

