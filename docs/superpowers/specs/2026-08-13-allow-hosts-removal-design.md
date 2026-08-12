# `allow_hosts` 제거 설계

**날짜:** 2026-08-13

**상태:** 승인됨 — 2026-07-18 deprecation 설계와 사용자의 후속 작업 전체 실행 승인에 근거

**topic:** `allow-hosts-removal`

**선행 설계:**
[`2026-07-18-allow-hosts-deprecation-design.md`](./2026-07-18-allow-hosts-deprecation-design.md)

## 목표

fragmentation으로 우회 가능한 iptables substring matcher인 `allow_hosts`를 runtime
contract에서 제거한다. 기존 profile이 이 필드를 계속 포함하면 조용히 규칙을 잃지
않도록 profile load를 명시적으로 실패시키고, operator에게 `allow_sni`와
`allow_cidrs` 마이그레이션 경로를 알려 준다.

## 포함 범위

1. `egressProfile.AllowHosts`와 관련 validation/apply rule 제거
2. JSON 최상위 `allow_hosts` key의 값이 빈 배열 또는 `null`이어도 loud failure
3. domain은 `allow_sni`, IP/CIDR은 `allow_cidrs`로 이동하는 운영 문서 갱신
4. `CONTEXT.md`, `README.md`, `RELEASE_NOTES.md`, service architecture와 ADR 정합화
5. 제거 전 실패·제거 후 거부·현행 profile 정상 동작을 구분하는 TDD 회귀 검증

## 비목표

- 다른 미지 JSON 필드를 일괄 거부하는 schema strictness 확대
- `allow_sni`/`allow_cidrs` semantics 변경
- profile 자동 변환 또는 운영 환경 파일의 자동 수정
- 다음 tag 생성이나 version 번호 발행
- historical lifecycle 문서의 과거 사실 재작성

## Domain Architecture

### 계약 경계

- `egressProfile`은 daemon 내부의 현재 egress 계약만 표현한다.
- `loadEgressProfile`은 disk JSON과 현재 계약 사이의 anti-corruption boundary다.
- 제거된 `allow_hosts` 감지는 apply/validation보다 앞선 decode 단계에서 수행한다.
- `planProfileEgressCommands`는 `allow_cidrs`, `allow_sni`, `dns_servers`만 소비한다.

공개 HTTP/MCP API, guest protocol, storage format은 바뀌지 않는다. operator가 작성하는
`configs/profiles/<name>/egress.json` contract만 바뀌므로
`docs/architecture/service-logic.md`를 갱신한다.

### 용어

- **removed legacy key:** 현재 schema에는 없지만 과거에 안전하지 않은 의미를 가졌던
  `allow_hosts`
- **loud fail-closed:** key를 발견하면 profile load를 error로 끝내 VM 생성/egress 적용을
  진행하지 않는 동작
- **explicit legacy rejection:** 모든 unknown field가 아니라 `allow_hosts`만 raw JSON key로
  탐지해 거부하는 decode 방식

## 설계 결정

### D1. 모든 `allow_hosts` key를 명시적으로 거부한다

`json.Unmarshal` 전에 최상위 object를 `map[string]json.RawMessage`로 읽어
`allow_hosts` 존재 여부를 확인한다. 값의 길이나 타입을 보지 않으므로 다음 모두 실패한다.

```json
{"allow_hosts": ["api.example.com"]}
{"allow_hosts": []}
{"allow_hosts": null}
```

오류는 제거된 field 이름과 두 대체 field를 포함한다. host value는 log/error에
반복하지 않는다.

### D2. 모든 unknown field를 거부하지 않는다

`json.Decoder.DisallowUnknownFields`는 제거 목표보다 넓은 behavior change다. 이미 존재하는
다른 unknown metadata를 갑자기 거부할 수 있으므로 이번에는 explicit legacy rejection을
사용한다. 일반 schema strictness는 별도 lifecycle 대상이다.

### D3. substring iptables rule을 완전히 제거한다

`-m string --string ... --algo bm` command 생성 경로와 `AllowHosts` validation을 제거한다.
현재 profile 예시는 `allow_hosts`를 사용하지 않으므로 repository-owned config migration은
필요 없다.

### D4. historical 기록은 보존하고 canonical 문서를 현재화한다

과거 deprecation spec/plan은 당시 사실이므로 바꾸지 않는다. 현재 진실 기준인
`CONTEXT.md`, `README.md`, `RELEASE_NOTES.md`, architecture/ADR와 운영 guide에는 제거 완료와
마이그레이션 계약을 기록한다.

### D5. version은 이 변경에서 발행하지 않는다

제거는 다음 tagged release에 포함될 breaking candidate지만, tag/version은 upstream
alignment gate와 다른 blockers가 닫힌 뒤 결정한다. 이번 diff에 tag를 만들지 않는다.

## Plan Design Review

- **발견성:** README의 egress 설명과 runbook에서 제거 사실·대체 필드를 직접 찾을 수 있다.
- **오류 예방:** 빈 `allow_hosts`도 받아들이지 않아 오래된 template이 잠복하지 않는다.
- **오류 메시지:** 제거 field와 `allow_sni`/`allow_cidrs`를 함께 명시한다.
- **상태 명확성:** “deprecated”와 “removed”를 canonical 문서에서 혼용하지 않는다.
- **visual/UI:** UI 변경이 없어 visual review는 해당 없다. operator IA와 오류 발견성
  review로 대체했고 blocking issue가 없다.

## 수용 기준

1. `egressProfile`에 `AllowHosts`가 없다.
2. `allow_hosts` key는 non-empty, empty, `null` 모두 profile load error가 된다.
3. 오류는 `allow_hosts`, `allow_sni`, `allow_cidrs`를 명시하고 값은 노출하지 않는다.
4. 제거 후 command plan에 iptables string matcher가 생성되는 코드가 없다.
5. `allow_sni`, `allow_cidrs`, `dns_servers` profile은 기존대로 load/validate/plan된다.
6. 다른 unknown field의 기존 ignore behavior는 이번 변경으로 바뀌지 않는다.
7. 변경 전 테스트가 legacy profile을 허용해 실패하고, 변경 후 통과하는 prove-broken-first
   evidence가 있다.
8. Go 전체 test/build/vet/race/gofmt와 secret scan이 통과한다.
9. 진실 기준 문서와 operator 문서가 제거 계약으로 정합화된다.
10. operation handoff가 verification, blocker, residual risk를 기록한다.

## Spec Freeze Snapshot

- **Approved objective:** deprecated `allow_hosts` 실제 제거와 loud fail-closed migration
- **Domain boundary:** disk JSON decode boundary와 host egress command planner
- **Decode decision:** explicit top-level `allow_hosts` rejection; global unknown rejection 아님
- **Migration:** domain → `allow_sni`, IP/CIDR → `allow_cidrs`
- **Security invariant:** packet substring matcher 완전 제거, legacy key 조용한 무시 금지
- **Non-goals:** 자동 profile 수정, 다른 schema strictness, tag/version 발행
- **Acceptance:** key 형태 3종 거부, 현행 fields 정상, string matcher source/plan 부재,
  전체 검증 green
- **Environment:** Linux/Go unit test는 KVM 불필요; full KVM은 최종 release gate에서 별도
- **Open design questions:** 없음

