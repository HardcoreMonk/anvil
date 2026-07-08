# Bounded Relay Retry Handoff

- 작성일: 2026-07-08
- 대상 branch: `worktree-relay-retry`
- 대상: `cmd/goose-daemon`(`relay_retry.go` 신설, `orchestrator_api.go`의 relay
  hop 3곳)
- 상태: 구현·유닛(+`-race`)·기존 KVM e2e 회귀 게이트 완료. 설계:
  [`docs/superpowers/specs/2026-07-08-bounded-relay-retry-design.md`](../superpowers/specs/2026-07-08-bounded-relay-retry-design.md)
- 선행: [`docs/operations/2026-07-06-cross-host-town-wall-handoff.md`](2026-07-06-cross-host-town-wall-handoff.md),
  [`docs/operations/2026-07-08-cross-host-gtcall-handoff.md`](2026-07-08-cross-host-gtcall-handoff.md)
  (양쪽 Follow-Up "bounded relay retry/buffer" — 이 슬라이스가 그 후속을 닫는다)

## 무엇이 배포됐나

member/hub daemon의 daemon-to-daemon relay hop 3곳이 짧은 네트워크 순단을
동기 bounded retry로 자동 흡수한다:

1. `relayTownWallPost` (wall post relay, member→home)
2. `forwardFlockCall` (call hop — relay→home과 hub→target 양쪽)
3. `townWallHistory` relay 분기 (GET, 멱등)

세 hop 모두 기존 `newAgentHTTPClient().Do(...)` 직접 호출을
`doWithDialRetry(ctx, client, build)`(`cmd/goose-daemon/relay_retry.go`, 신설)
경유로 교체했을 뿐이다 — 헤더·payload·method·타임아웃(guest 300s > member→home
290s > hub→target 280s)은 변경 없음. SSE 스트림(`streamTownWallRelay`)은
비범위로 남았다(스트림 재접속 semantics는 별도 문제).

## 재시도 규칙과 안전 논거

- **dial-계열 transport 에러에만 재시도**: `isDialError(err)`가 `url.Error`/
  `net.OpError` 체인을 unwrap해 `Op == "dial"`인 경우만 true를 반환한다 —
  connection refused/no route/dial timeout 등, **요청이 상대 daemon에
  물리적으로 도달하지 않았음이 보장되는 실패**만 대상이다.
- **재시도 금지 (의도적)**: 연결 수립 후 실패(reset/EOF — 상대가 이미
  요청을 받아 처리했을 수 있음), HTTP 응답 일체(4xx/5xx는 상대 daemon의
  답이지 transport 실패가 아님), ctx 취소/만료.
- **안전 논거**: dial 실패만 재시도 대상으로 좁힌 것이 곧 안전 보증이다 —
  연결 자체가 서지 않았다는 것은 페이로드가 상대 프로세스에 전혀 도달하지
  않았다는 뜻이므로, 재시도가 wall post를 중복시키거나 call 프롬프트를
  이중 실행할 가능성이 구조적으로 배제된다. reset/EOF나 HTTP 응답을
  재시도 대상에서 뺀 것은 바로 이 반대 경우(상대가 이미 처리했을 수 있는
  경우)를 피하기 위함이다.
- **정책 고정** (설정화하지 않음, YAGNI): 최대 2회 재시도(총 3 시도),
  backoff 1s→2s, ctx-aware(`ctx.Done()` 시 즉시 중단, 마지막 에러로 종료).
  기존 timeout 계단 안에서 최악 +3s는 무시 가능한 수준이다.
- 재시도 시 `slog.Warn` 1줄 — `attempt` 번호만 기록한다. daemon 주소·토큰은
  이 helper가 flock/host 컨텍스트를 모르기도 하고, 알았더라도 기존 redaction
  규율(`d5c7df0`)을 지키기 위해 의도적으로 배제했다.

## Gate 결과

- **유닛**: Task 1 신설 `cmd/goose-daemon/relay_retry_test.go` 6건 —
  `TestIsDialError`, `TestDoWithDialRetry_RetriesDialThenSucceeds`,
  `TestDoWithDialRetry_StopsAtAttemptCap`,
  `TestDoWithDialRetry_NoRetryOnHTTPResponse`,
  `TestDoWithDialRetry_NoRetryOnResetError`,
  `TestDoWithDialRetry_CtxCancelAbortsBackoff` — 전부 PASS(`-race` 포함).
- **통합**: Task 2 신설 2건 — `TestPostToTownWall_RelayRetriesDialFailure`
  (`townwall_relay_test.go`, 닫힌 포트 → 1회 재시도 후 성공, home이 정확히
  1회만 POST 수신 — 재시도가 중복 전달을 만들지 않음을 증명)와
  `TestCallFlockAgent_RelayRetryExhausted502`
  (`flock_call_test.go`, 영구 닫힌 포트 → 재시도 소진 후 502, 벽시계
  <5s, 502 바디에 daemon 주소·양쪽 토큰 모두 미노출). 둘 다 `-race` x20
  반복에서 flake 없음.
- **회귀**: `go test ./cmd/goose-daemon -count=1 -race` 전체 패키지 green —
  기존 relay/call 테스트(`TestPostToTownWall_RelayForwardsToHome`,
  `TestCallFlockAgent_RelayForwardsToHome`,
  `TestCallFlockAgent_HopGuardNeverReforwards` 등) **단언 0 변경**으로
  markHop/depth/redaction/context-honoring semantics 불변을 확인.
  `go test ./cmd/... ./internal/... -count=1` 13개 테스트 패키지 전부
  `ok`(`cmd/micro-init`는 기존부터 테스트 파일 없음, 무관).
- **빌드**: `go build ./cmd/goose-daemon ./cmd/anvil-mcp
  ./cmd/anvil-scheduler ./cmd/ephemera-ctl` 4종 green.
- `git diff --check` clean(매 task 커밋 전 확인).
- **KVM e2e**: `scripts/anvil-cross-host-wall-e2e.sh`,
  `scripts/anvil-cross-host-gtcall-e2e.sh` 기존과 동일하게 통과 — 두
  스크립트 모두 정상 경로(dial 실패 없음)만 구동하므로 재시도가 발화하지
  않는다. 이는 회귀 없음의 증거이지 재시도 경로 자체의 검증은 아니다(재시도
  경로는 유닛/통합 테스트가 커버).

## Known limitations / 운영 주의

- **dial-계열 실패만 재시도**: reset/EOF, HTTP 응답, ctx 만료는 즉시 반환된다
  — 이런 실패의 복구는 여전히 agent 쪽 재시도에 의존한다(설계상 의도, 위
  안전 논거 참고).
- **SSE 비범위**: `streamTownWallRelay`의 스트림 재접속은 이 slice에 포함되지
  않는다.
- **비동기 버퍼 없음**: 재시도는 guest 요청 창 안에서 동기로 끝난다. home이
  장기간(재시도 창 3s를 넘어) 다운되면 요청은 그대로 실패 반환된다 — 로컬
  버퍼링·백그라운드 재전송은 이 slice의 비목표.
- **wall 경로 502 바디의 home URL 노출은 기존 성질**: `postToTownWall`과
  `townWallHistory`의 relay 분기는 `doWithDialRetry`가 반환한 `err`(내부에
  `*url.Error`가 home 주소를 담고 있음)를 502 응답 바디에 그대로 실어
  보낸다(`town wall relay to home failed: %w`, 또는 `err` 직접 전달) — 이
  slice가 도입한 문제가 아니라 기존 wall relay 에러 처리부터 있던 성질이며,
  이번 retry 전환은 그 wrapping을 바꾸지 않았다. **call 경로만 redaction
  계약**을 지킨다 — `callFlockAgent`는 `err`를 wrap하지 않고 flock ID만
  담은 opaque 메시지를 만들어(`TestCallFlockAgent_RelayRetryExhausted502`가
  주소·토큰 미노출을 단언), wall과 call 두 경로의 에러 노출 수준이
  정합하지 않는다. 정합은 후속(아래 Follow-Up).

## Next Action

release-gate 관점에서 이 슬라이스는 닫혔다(유닛 green, 통합 green, race
green, 기존 KVM e2e 회귀 green, `git diff --check` clean). 다음 우선순위는
설계 문서가 확정한 순서를 따른다:

1. **수동 multi-host 검증**: 실 2-daemon cross-host 환경에서 순단(네트워크
   차단/daemon 재시작)을 인위로 유발해 자동 재시도가 실제로 순단을 흡수하는
   지 확인한다(현재는 유닛/통합 fake transport로만 검증됨).
2. **mesh 설계**: home 단일 장애점 제거는 위 수동 검증으로 미검증 토대가
   해소된 뒤 착수한다(미검증 토대 위에 HA를 얹지 않는다는 원칙, 설계 문서
   참고).

## Follow-Up Tasks

- ~~**wall 경로 에러 redaction 정합**~~ — **CLOSED** (PR #24, `be73461`):
  wall relay hop 4곳(post, history, stream 요청 빌드, stream relay)이 전부
  call 경로와 동일하게 flock id만 노출하는 opaque 502 에러로 정합됐다 —
  daemon 주소는 더 이상 어떤 relay hop에서도 노출되지 않는다.
- **비동기 수락·버퍼·백그라운드 재전송**: home 장기 다운 시나리오는 이
  slice의 비목표로 남았다 — mesh/수동 multi-host 검증 이후 재평가한다(위
  Next Action 참고).
