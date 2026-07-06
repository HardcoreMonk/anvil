# Cross-host Shared Town Wall Handoff

- 작성일: 2026-07-06
- 대상 branch: `feature/cross-host-town-wall`
- 대상: `internal/anvilmcp` routed flock + `cmd/goose-daemon` Town Wall
- 상태: 구현·유닛 검증·KVM e2e 게이트 완료. 설계:
  [`docs/superpowers/specs/2026-07-06-cross-host-shared-townwall-design.md`](../superpowers/specs/2026-07-06-cross-host-shared-townwall-design.md)
- 선행: [`docs/operations/2026-06-06-cross-host-flock-create-minimal-handoff.md`](2026-06-06-cross-host-flock-create-minimal-handoff.md)
  (members-only routed flock create — 이 슬라이스가 flock identity와 공유 wall을 그 위에
  더한다)

## 무엇이 배포됐나

여러 host에 흩어진 routed flock member가 **하나의 공유 Town Wall**에 post하고
관찰할 수 있다(home-host hub + daemon-to-daemon relay). guest는 계속
bridge-only(로컬 daemon만 접촉)로 격리된다. cross-host `gtcall`과 cross-host
broadcast fan-out은 이 슬라이스 범위 밖이다(비목표 유지).

### 토폴로지

- **home host**: `roles[0]`이 배치된 host. home daemon이 flock의 canonical
  `TownWall`을 hub flock으로 소유한다(파일 로그 + monotonic seq + 구독자 + SSE,
  `internal/orchestrator/townwall.go` 재사용).
- **member daemon**(home이 아닌 나머지 host): flock을 relay flock으로 등록한다.
  자기 wall이 없고 `/flocks/{id}/post`를 받으면 home으로 forward하고,
  `/wall`·`/wall/history`·SSE는 home으로 proxy한다.
- **guest**: 변경 없음. `gtwall` → in-VM agent → 로컬 daemon(`10.0.1.1:3000`)의
  `/flocks/{id}/post`만 접촉한다. 원격 주소를 모르고 네트워크로 나가지 않는다.

### flock identity 주입

routed member는 이제 `POST /vms`로 `FlockID`/`AgentID`/`ControlPlaneToken`을 받는다
(`internal/anvilmcp/daemon_client.go`의 `SpawnVMRequest` 확장). provisioner
(`internal/storage/provisioner.go`)가 이 필드로 `.ephemera-flock`/
`.ephemera-cp-token`을 기록해 `gtwall`이 더 이상 "not part of a flock"으로 종료하지
않는다. `CreateRoutedFlockMembers` 성공 output은 이제 `TownWallEnabled=true`.

## 보안 경계

- **relay_token 스코프**: flock별 `relay_token`이 daemon-to-daemon hop을 인증한다.
  `authMiddleware`는 이 token을 **해당 flock의 wall sub-path만**
  (`/flocks/{id}/(post|wall|wall/history)`) admit한다 — 일반 control-plane bearer로
  승격하지 않는다(Task 3에서 발견/차단한 escalation: 최초 구현은 `cp.clients`에
  merge해 host-boundary를 넘는 full admin bearer가 되는 결함이 있었음. flock-scoped
  전용 store로 고쳤다).
- **payload는 `{agent_id, body}`뿐** — per-VM `agent_token`, peer CP 토큰 미노출
  (broadcast metadata-only 규율과 동일).
- **`relay_token` redaction**: `PlacementStore`(`RoutedFlockRecord`/
  `RoutedFlockRelayTokens`)에 영속되지만 `State()`가 nil 처리해 모든 MCP output/
  audit/HTTP view에서 redact된다.
- **guest는 신규 네트워크 노출 없음** — 로컬 daemon만 접촉.
- **anvil_* MCP tool 미추가** — cross-host wall은 runtime/operator 표면 유지.
  `TestIronClawSchemasExcludeCrossHostWallTools`가 고정.
- **신뢰 네트워크 전제**: member↔home daemon은 control-plane 포트로 상호 도달
  가능해야 한다. 이 경로는 반드시 신뢰(private) 네트워크 위에 있어야 하며, 외부
  노출은 기존 reverse-proxy/TLS 정책 뒤에서만 한다.

## 장애/복구

- **reconcile**: daemon 재시작(process restart) 후 `ReconcilePlacements`가
  `PlacementStore`의 `HomeHost`+roster로 hub/relay 등록을 재수행하고
  relay_token admission을 복원한다. 저장된 relay token이 없는 record는 안전하게
  skip한다.
- **rollback/delete**: 부분 create 실패 시 rollback이, 그리고
  `anvil_delete_flock`이 hub/relay 등록을 해제하고 token을 revoke한다.
- **home 다운**: wall 불가(SPOF, 1차 수용 — 아래 진화 경로 참고). relay post는
  실패를 보고하지만 agent는 계속 실행한다(non-fatal). `TOWN_WALL.log`는 home
  디스크에 남아 history를 보존한다.
- **relay 네트워크 순단**: v1은 로컬 버퍼링 없음. post는 오류를 반환하고 agent가
  재시도/계속한다(follow-up: bounded relay retry).

## 커밋 (Task 1–9, `feature/cross-host-town-wall`)

```
f6f2bf1 feat(runtime): accept flock identity on POST /vms for routed members
0f6af84 feat(runtime): add hub/relay flock kinds for cross-host town wall
f6a6353 feat(runtime): hub/relay flock registration endpoints
54354e3 fix(runtime): scope relay tokens to flock wall paths, not full control plane
b58175f feat(runtime): relay town wall post/history/stream to home daemon
00615cc feat(runtime): wire shared town wall on routed flock create
91c02ee feat(runtime): enable routed flock town wall; guard token redaction and schema
5c0c728 fix(test): mock hub/relay registration endpoints in cmd/anvil-mcp integration test
65f6157 feat(runtime): reconcile re-registers cross-host town wall
262e14c feat(runtime): rollback deregisters cross-host town wall on failure
420f68f test(runtime): cross-host town wall relay e2e (real member + stub home)
```

`54354e3`는 Task 3 리뷰에서 발견된 보안 결함(위 relay_token escalation)의 same-task
수정이다.

## Gate 결과

- 유닛: `go test ./cmd/... ./internal/... -count=1` — Task 6부터 매 task마다 전체
  suite로 실행(Task 6에서 부분 suite만 돌려 cmd/anvil-mcp regression을 놓친 사고 이후
  프로세스 수정), 최종 상태 green.
- 빌드: `go build ./cmd/goose-daemon ./cmd/anvil-mcp ./cmd/anvil-scheduler
  ./cmd/ephemera-ctl` green.
- `git diff --check` clean.
- `bash -n scripts/anvil-cross-host-wall-e2e.sh` EXIT 0.
- **KVM e2e 게이트**(`scripts/anvil-cross-host-wall-e2e.sh`, Task 9): 컨트롤러가
  실행, **17/17 checks, EXIT 0**. 실 member VM에서 `gtwall` → in-VM agent → member
  daemon → relay → stub home 실경로 검증: body는 `{agent_id, body}`뿐, `Bearer
  relay_token`, `agent_token` 필드 부재 AND per-VM `agent_token` 값 부재(credential
  isolation을 실경로에서 증명). home은 stub(두 실 daemon이 한 host의 guest bridge
  subnet(`10.0.1.1`)에서 충돌하기 때문 — 완전한 2-daemon cross-host 통합은 수동
  multi-host 검증으로 분리 기록됨, 스크립트 헤더 주석 + 성공 시 echo).
- IronClaw schema-exclusion(`TestIronClawSchemasExcludeCrossHostWallTools`)과
  token-redaction guard 모두 pass.
- 기존 단일 호스트 flock e2e(`scripts/anvil-mcp-e2e.sh flock`, `e2e_test.sh`)는
  이번 슬라이스가 건드리지 않는 경로 — 회귀 없음.

## Known limitations / 운영 주의

- **SPOF**: home host가 다운되면 해당 flock의 wall 전체가 불가하다. 1차 수용된
  설계 결정이며, mesh 진화 경로가 있다(아래 follow-up).
- **relay retry 없음**: v1은 relay 네트워크 순단 시 로컬 버퍼링/재시도가 없다.
- **SSE relay non-200 content-type**: 기존 local-handler 패턴을 그대로 따라 relay
  경로에서도 non-200 응답에 `text/event-stream`이 남아있다(cosmetic, pre-existing
  패턴 상속).
- **member 최초 spawn 실패 시 관련 relay 등록이 rollback에서 누락될 수 있음**
  (아래 follow-up).
- **`ReconcilePlacements`에 production 호출자가 없음** — 현재 유닛 테스트로만
  exercise되고, 주기적 control loop 배선은 이번 슬라이스 범위 밖이다.

## Next Action

release-gate 관점에서 이 슬라이스는 닫혔다(유닛 green, KVM e2e green, 보안 가드
green). 다음으로 이어질 작업은 아래 Follow-Up Tasks 중 우선순위가 높은 항목
(member-deregistration gap, 그리고 `ReconcilePlacements` 주기적 control loop 배선)
부터 별도 slice로 계획한다. cross-host `gtcall`을 다음 capability로 검토한다.

## Follow-Up Tasks

- **SSE relay non-200 content-type**: relay 경로가 non-200 응답에도
  `text/event-stream`을 유지하는 기존 local-handler 패턴을 상속했다. polish 필요.
- **bounded relay retry/buffer**: v1은 relay 네트워크 순단 시 로컬 버퍼링이 없다.
  post 실패는 agent 재시도에 의존한다. bounded retry/buffer 설계가 후속이다.
- **mesh 진화(home SPOF 제거)**: 현재 home 단일 장애점을 1차 수용했다. hub flock
  등록을 replica set으로 확장하면 mesh로 승격 가능(설계 문서의 진화 경로).
- **cross-host `gtcall`**: 다음 capability 후보. 이 슬라이스는 공유 wall(broadcast/
  observe)만 다루고 지정 agent 호출은 비범위로 남겼다.
- **Task-8 member-deregistration gap**: 최초 spawn이 실패한 member는
  `record.Agents`에 들어가지 않아 rollback이 그 member의 relay flock 등록을 해제
  (`DELETE`)하지 못한다. 실질 위험은 낮다 — flock id가 timestamp 기반이라 충돌이
  없고 home 쪽 token이 revoke되어 stub이 실제 wall을 인증할 수 없지만, member
  daemon이 재시작될 때까지 in-memory relay flock stub이 leak된다. 수정 방향:
  rollback에 registered-hosts set을 스레딩해 spawn 성패와 무관하게 relay 등록을
  해제하도록 한다.
- **Task-7 comment correction**: `cmd/goose-daemon/orchestrator_api.go:1209`와
  `orchestrator_api_test.go:224`의 주석이 relay-token 소실 트리거를 "SIGHUP
  ReloadClients"로 잘못 지목한다. 실제 트리거는 **daemon process restart**다 —
  `ReloadClients`는 `cp.clients`만 교체하고 `flockMgr`/relay-token store는 건드리지
  않는다(recovery.go가 재시작 시 hub flock을 `NewUnregistered(Kind=FlockKindHub)`로
  재구성하지만 relay token store는 in-memory-only라 비게 된다). 코드 동작 자체는
  맞고(reconcile이 재등록으로 복구) 주석 문구만 고치면 된다.
- **`ReconcilePlacements` 주기적 control loop 배선**: 현재 유닛 테스트로만
  exercise되고 production 호출자가 없다(`grep -rn "ReconcilePlacements(" internal
  cmd`가 정의 외 호출부 없음을 확인). scheduler control loop 또는 daemon health
  polling 주기에 연결하는 배선이 후속이다.
