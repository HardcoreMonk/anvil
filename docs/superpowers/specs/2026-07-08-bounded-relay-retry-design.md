# Bounded relay retry 설계 (동기, dial-실패 한정)

- 작성일: 2026-07-08
- 상태: 설계 확정 (구현 전)
- 선행: [`docs/operations/2026-07-08-cross-host-gtcall-handoff.md`](../../operations/2026-07-08-cross-host-gtcall-handoff.md),
  [`docs/operations/2026-07-06-cross-host-town-wall-handoff.md`](../../operations/2026-07-06-cross-host-town-wall-handoff.md)
  Follow-Up "bounded relay retry/buffer"
- 관련 코드: `cmd/goose-daemon/orchestrator_api.go`(`relayTownWallPost`,
  `forwardFlockCall`, `townWallHistory` relay 분기), `cmd/goose-daemon/api.go`
  (`newAgentHTTPClient`)

## 문제

cross-host wall/gtcall의 daemon-to-daemon hop(member→home, home→target)은
네트워크 순단·상대 daemon 재시작 순간에 즉시 실패하고(502), 복구는 agent의
수동 재시도에 의존한다. reconcile 루프(기본 60s)가 등록을 힐링하는 것과 같은
시나리오 창에서, hop 자체도 짧은 자동 재시도로 순단을 흡수해야 한다.

## 결정 (사용자 확정 2건)

1. **동기 bounded retry만** — 재시도는 guest 요청 창 안에서 동기로 끝난다.
   성공 응답 = 상대 daemon ack (전달 semantics 불변). 비동기 수락·버퍼·재전송은
   비범위 — 그 가치 창(home 장기 다운)은 mesh 슬라이스의 영역이고, 전달 보장
   약화·crash 유실·순서 이상을 수반하기 때문(수동 multi-host 검증과 mesh 설계
   후 재평가).
2. **다음 capability 우선순위** — mesh(home SPOF 제거)보다 retry를 먼저 한다.
   미검증 토대(실 2-daemon 수동 검증 대기) 위에 HA를 얹지 않는다.

## 재시도 규칙

- **dial-계열 transport 에러에만 재시도**: connection refused / no route /
  dial timeout 등 — **요청이 상대 서버에 도달하지 않았음이 보장되는 실패**만.
  판별은 `*net.OpError`의 `Op == "dial"`(체인 unwrap — `url.Error` 안쪽)으로
  한다.
- **재시도 금지**: 연결 수립 후 실패(reset, read/EOF — 상대가 이미 처리했을 수
  있음: wall post 중복, **call 프롬프트 이중 실행** 위험), HTTP 응답 일체
  (4xx/5xx는 상대 daemon의 답), ctx 취소/만료.
- **정책 고정** (설정화하지 않음 — YAGNI): 최대 2회 재시도(총 3 시도),
  backoff 1s → 2s. backoff 대기는 ctx-aware — `ctx.Done()` 시 즉시 중단하고
  마지막 에러로 종료. 기존 timeout 계단(guest 300s > member→home 290s >
  home→target 280s) 안에서 최악 +3s는 무시 가능.
- 재시도 시 slog 경고 1줄 — flock/host 식별자만 (daemon 주소·토큰 금지,
  `d5c7df0` redaction 규율).

## 적용 지점

member/hub daemon의 원격 hop 3곳 (`cmd/goose-daemon/orchestrator_api.go`):

1. `relayTownWallPost` (wall post relay, member→home)
2. `forwardFlockCall` (call hop — relay→home과 hub→target 양쪽)
3. `townWallHistory` relay 분기 (GET, 멱등 — 동일 규칙 적용)

SSE 스트림(`streamTownWallRelay`)의 재접속은 비범위(스트림은 클라이언트 재접속
semantics가 별도).

## 구현 형태

- helper 하나: `doWithDialRetry(ctx, client, build func() (*http.Request, error))
  (*http.Response, error)` — 시도마다 요청을 **재생성**(body Reader 소모 문제),
  dial-계열 판별 함수 `isDialError(err) bool` 분리(단위 테스트 대상).
- 세 적용 지점이 기존 `newAgentHTTPClient()` 호출을 helper 경유로 교체.
  응답 처리·에러 매핑(502 등)은 기존 그대로 — helper는 transport 계층만.

## 테스트 (TDD)

- `isDialError` 단위: dial refused(`*net.OpError{Op:"dial"}` 체인) → true;
  connection reset/EOF → false; `url.Error` wrap unwrap 확인; nil/무관 에러 →
  false.
- helper 단위(주입 fake RoundTripper): dial 에러 → 재시도(시도 수 상한 3),
  2번째 성공 시 성공 반환; HTTP 500 응답 → 재시도 없이 그 응답 반환;
  reset-계열 → 재시도 없이 에러; ctx cancel이 backoff 중 즉시 중단.
- 핸들러 통합: 닫힌 포트 home → (재시도 소진 후) 502, 벽시계 상한 단언(<10s);
  기존 relay/call 테스트 무변경 통과(semantics 불변).
- KVM e2e 무변경 (기존 gtcall/wall e2e가 semantics 불변을 회귀 확인).

## 문서 반영 (구현 시)

- runbook 순단 대응에 1-2줄 (자동 재시도 범위와 한계 — dial-실패만, 총 3 시도).
- wall/gtcall handoff의 "bounded relay retry/buffer" Follow-Up을 CLOSED로
  (비동기 버퍼는 mesh 이후 재평가로 명시).
- slice handoff 신설.

## 비목표

- 비동기 수락·버퍼·백그라운드 재전송 (mesh/수동 검증 이후 재평가).
- SSE 재접속, retry 정책 설정화(env), 신규 메트릭 family.
- reset-계열/응답 실패의 재시도 (이중 실행 위험 — 의도적 제외).
