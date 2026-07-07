# Cross-host shared Town Wall for anvil routed flocks — design

## 상태

- 작성일: 2026-07-06
- 대상: `internal/anvilmcp` routed flock + `cmd/goose-daemon` Town Wall
- 전제 baseline: `anvil-v0.7.0`(main HEAD). upstream ephemera `v0.4.0`-`v0.7.0` parity 편입 완료
- 범위 결정(brainstorming): **공유 Town Wall(broadcast/observe)만**. cross-host gtcall과
  broadcast fan-out은 비범위(YAGNI).
- 트래픽 모델 결정: **daemon-to-daemon relay, guest는 로컬 유지**(격리 불변).
- 토폴로지 결정: **home-host hub**(단일 canonical wall + relay).

## 배경 / 문제

anvil은 IronClaw의 tool call을 Firecracker MicroVM 실행으로 변환하는 실행 계층이다.
단일 호스트 flock은 Town Wall(per-flock append-only 로그 + monotonic seq + in-process
구독자 + SSE)로 agent들이 협업한다. guest는 `gtwall`→in-VM agent→**로컬 호스트**
daemon(`10.0.1.1:3000`)의 `/flocks/{id}/post`로만 접촉하며 호스트 밖 네트워크로 나가지
않는다.

cross-host routed flock(`ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`)은 현재
멤버 VM을 여러 호스트에 **spawn만** 하고 통신 수단이 없다. 구체적으로:

- `SpawnVMRequest`(`internal/anvilmcp/daemon_client.go`)에 FlockID/AgentID/
  ControlPlaneToken이 없어 routed 멤버는 `.ephemera-flock`·`.ephemera-cp-token`을
  받지 못한다 → 멤버 안에서 `gtwall`이 "not part of a flock"으로 종료.
- `RoutedFlockCreateOutput.TownWallEnabled`가 hardcoded `false`, post/history는
  hard-error(`routed_flock.go`, `replicating_daemon.go`).
- Town Wall은 한 daemon의 workDir 파일 + in-process 채널로, 호스트를 넘는 relay·
  aggregation·coordinator가 없다.

목표: **여러 호스트에 흩어진 flock 멤버가 하나의 공유 Town Wall에 post하고 관찰**할 수
있게 한다. 단일 호스트 flock의 wall 의미(순서·history·SSE)를 cross-host에서 보존하되,
guest의 bridge-only 격리와 anvil의 credential 경계를 깨지 않는다.

## 비목표

- cross-host gtcall(호스트 넘는 지정 agent 호출).
- cross-host broadcast fan-out.
- mesh 복제 wall / 다중 home HA(진화 경로로만 열어둠).
- Town Wall을 IronClaw-facing `anvil_*` MCP 도구로 승격(계속 runtime/operator 표면).
- guest VM이 호스트 밖 네트워크로 직접 나가는 모델.

## 아키텍처 (home-host hub)

세 daemon 역할. guest는 전부 로컬 daemon만 접촉.

```
[member VM/host B]  gtwall→ 로컬 daemon B (relay flock F)
                                   │ daemon→daemon relay (F 전용 relay_token)
                                   ▼
[home host A]  daemon A (hub flock F: canonical TownWall 소유)
                                   ▲
[member VM/host C]  gtwall→ 로컬 daemon C (relay flock F) ──┘
```

- **Home daemon(호스트 A)**: flock F의 canonical Town Wall 소유. 기존 `internal/
  orchestrator/townwall.go`의 `TownWall`(파일 로그 + monotonic seq + 구독자 + SSE)를
  그대로 사용. 멤버 VM을 로컬에 두지 않아도 F를 flockMgr에 **hub flock**으로 등록해
  `/flocks/F/post`·`/wall`·`/wall/history`·SSE를 서빙.
- **Member daemon(호스트 B/C)**: F를 **relay flock**으로 등록 — 자기 wall이 없고,
  `/flocks/F/post`를 받으면 home으로 forward, `/wall`·`history`·SSE는 home으로 proxy.
- **Guest(member VM)**: 지금과 동일. `gtwall`→in-VM agent→로컬 `10.0.1.1:3000`. 원격
  주소를 모르고 네트워크로 안 나감. relay는 로컬 daemon이 처리.

**home 호스트 선정**: `CreateRoutedFlockMembers`가 결정한다. **기본 규칙: 첫 번째
요청 role(`roles[0]`)이 배치된 호스트를 home으로 삼는다** — 결정론적이고 추가 설정
불요. (설정 가능한 coordinator role 지정은 진화 경로이며 이번 범위 밖.)
`PlacementStore`의 `RoutedFlockRecord.HomeHost`로 영속.

**순서/일관성**: 모든 post가 home의 단일 wall에서 직렬화 → monotonic seq·순서 자명.

**진화 경로**: SPOF(home 다운→wall 불가)는 1차 수용. hub flock 등록을 replica set으로
확장하면 mesh로 승격 가능(비범위).

## 컴포넌트

### (a) 계약/모델 변경
- `SpawnVMRequest`(`internal/anvilmcp/daemon_client.go`)에 `FlockID`, `AgentID`,
  `ControlPlaneToken` 추가. provisioner(`internal/storage/provisioner.go`)는 이 필드가
  있으면 `.ephemera-flock`·`.ephemera-cp-token`을 이미 기록 → 공급만 추가.
- daemon flock 모델에 **flock kind** 도입: `local`(기존, 불변), `hub`(canonical wall
  소유 + 원격 roster), `relay`(wall 없음, home로 forward/proxy). `relay` flock은 얇은
  구조 `{flockID, homeAddr, relayToken}`.
- `RoutedFlockRecord`(`internal/anvilmcp/placement_store.go`)에 `HomeHost` 추가.
  **relay_token은 PlacementStore에 영속**(reconcile 재등록에 필요)하되, **모든 직렬화
  출력·MCP output·audit·로그에서 redact**한다(아래 보안). 즉 저장은 하되 노출은 안 함.

### (b) 새 daemon 엔드포인트 (internal mux, control-plane bearer, anvil_* 미노출)
- `POST /flocks/{id}/distributed` (home daemon): hub flock 등록 — 실제 `TownWall`
  생성, roster = 원격 `{agent_id, host}[]`, 로컬 VM 불요, relay_token 수용 목록에 등록.
- `POST /flocks/{id}/relay` (member daemon): relay flock 등록 — `{home_addr,
  relay_token}` 보관.
- 두 엔드포인트는 create 시 anvil control-plane이 호출. 멱등(재등록 안전) — reconcile
  에서 재호출.

### (c) 핸들러 변경 (`cmd/goose-daemon/orchestrator_api.go`)
- `postToTownWall`, `townWallHistory`, `streamTownWall`: flock kind가 `relay`면 home
  으로 forward/proxy(relay_token), `hub`/`local`이면 기존 로컬 wall 처리. 분기 최소.

### (d) anvil 오케스트레이션 (`internal/anvilmcp/routed_flock.go`)
- `CreateRoutedFlockMembers`: home 선정 → relay_token 생성 → home daemon에 hub flock
  등록 → 멤버 호스트마다 relay flock 등록 + `SpawnVM{FlockID, AgentID,
  ControlPlaneToken}` → `TownWallEnabled=true`, `HomeHost` 영속.
- `PostRoutedTownWall`/`RoutedTownWallHistory`의 hard-error를 제거하고 home로 위임.

### (e) daemon-to-daemon 인증
- flock별 **relay_token**을 anvil control-plane이 생성 → 멤버 daemon(home 인증용) +
  home daemon(수신 허용용)에 등록. flock 범위로 스코프. relay는 `{agent_id, body}`만
  전달(broadcast metadata-only 규율과 동일, per-VM agent 토큰·peer CP 토큰 미노출).

### (f) guest 측
- 변경 없음. `scripts/gtwall`·`cmd/goose-agent` townwall forward·provisioner 주입은
  이미 지원. routed spawn이 필드만 공급.

## 데이터 흐름

### Post (호스트 B 멤버)
1. 멤버 goose가 `gtwall "msg"` → `.ephemera-flock`·`.ephemera-agent-token` →
   `POST localhost:8080/townwall/post {body}` (기존)
2. in-VM agent → `POST 10.0.1.1:3000/flocks/F/post {agent_id, body}` + CP토큰 (기존)
3. member daemon B: `flockMgr.Get(F)` → relay flock → `POST {homeAddr}/flocks/F/post
   {agent_id, body}` + relay_token
4. home daemon A: `flockMgr.Get(F)` → hub flock → relay_token 검증 →
   `f.TownWall.Post(agent_id, body)`: 파일 append + monotonic seq + 로컬 구독자/SSE
5. 성공 응답 역방향

### Read (history/SSE)
- `GET /flocks/F/wall/history` → member daemon B relay flock → home로 proxy(relay_token)
- SSE `/flocks/F/wall` → member daemon B가 home SSE를 통과-proxy

### Home-로컬 멤버
- home 호스트가 멤버도 실행하면 그 post는 agent→로컬 daemon A→hub flock 직접(relay hop
  없음) → 같은 wall. 원격/home 멤버가 하나의 wall로 수렴.

### Create (오케스트레이션)
1. `ScheduleFlock`이 role별 호스트 선정
2. home = role[0] 배치 호스트(또는 설정 coordinator)
3. relay_token 생성
4. `POST {homeDaemon}/flocks/F/distributed {roster, relay_token}`
5. 멤버마다 `POST {memberDaemon}/flocks/F/relay {homeAddr, relay_token}` →
   `SpawnVM{FlockID=F, AgentID, ControlPlaneToken}`
6. record `ready`, `TownWallEnabled=true`, `HomeHost` 영속

## 오류 처리 / 장애

- **Home 호스트 다운**: wall 불가(SPOF, 1차 수용). relay post 실패 → gtwall 실패 보고
  (non-fatal, agent 계속). `TOWN_WALL.log` 파일은 home 디스크에 남아 history 보존.
  hub/relay flock 등록은 in-memory → anvil control-plane reconcile(`runtime_router.
  ReconcilePlacements` 확장)이 `HomeHost`+roster로 재등록.
- **relay 네트워크 순단**: post 오류 반환, v1은 로컬 버퍼링 없음(agent 재시도/계속).
  후속: bounded relay 재시도.
- **member 호스트 다운**: 해당 멤버만 소실, home wall·타 멤버 무영향.
- **relay_token 불일치/불량 post**: home daemon이 401 거부(flock 범위 스코프).
- **부분 create 실패**: 기존 rollback(`failed_cleanup_pending`)에 hub/relay 등록 해제
  추가.

## 보안 경계

- **guest는 bridge-only 유지** — 로컬 `10.0.1.1`에만 post, 신규 네트워크 노출 없음.
- **relay_token**: flock별·daemon 간 전용. `{agent_id, body}`만 전달 — per-VM agent
  토큰·peer CP 토큰 미노출. `PlacementStore`에 보관하되 **MCP output·audit·로그에서
  redact**(agent_token과 동급 규율).
- **home daemon이 relayed post 인증**(relay_token) → 불량 호스트가 자기가 멤버를 안 둔
  flock wall 오염 불가.
- 새 `/distributed`·`/relay` 엔드포인트는 internal mux(bearer auth) 뒤, **anvil_* MCP
  도구로 미노출**. wall은 runtime/operator 표면이지 IronClaw 표면이 아니다.
- **cross-host 도달성**: member daemon→home daemon은 control-plane 포트로 상호 도달
  필요 → 신뢰 네트워크(private) 전제. 외부 노출은 기존 reverse-proxy/TLS 정책 뒤에서만.

## 테스트 전략

### 유닛 (KVM-free, httptest mock daemon)
- relay flock forwarding: mock home으로 forward, 본문 `{agent_id, body}`뿐(credential
  미포함) 단언.
- hub flock: wall 등록, relayed post 수용(유효 relay_token→200/무효→401), history/SSE.
- `SpawnVMRequest` 확장: routed spawn이 FlockID/AgentID/CP토큰 주입 → `injectVMFiles`가
  `.ephemera-flock`·`.ephemera-cp-token` 기록 단언.
- 오케스트레이션: home 선정·hub/relay 등록·정체성 spawn → `TownWallEnabled=true`·
  `HomeHost` 영속.
- relay_token redaction sentinel: MCP output·audit·직렬화 응답에 부재 단언.
- reconcile: home "재시작" → PlacementStore에서 hub flock 재등록.
- rollback: 부분 실패 → hub/relay 등록 해제.
- 보안 가드(anvil sentinel): relayed post에 per-VM 토큰 없음, 새 엔드포인트 401 without
  bearer, **cross-host wall이 IronClaw 스키마에 anvil_* 도구 미추가**(기존
  schema-exclusion 가드 확장).

### KVM e2e (게이트, non-LLM)
- real member daemon 1대 + **stub home HTTP 서버**로: 실제 flock 멤버 VM이 `gtwall`
  post → member daemon → relay → stub home 도달 단언. guest→agent→daemon→relay 실경로를
  실 VM으로 검증. wall post는 LLM 불요라 provider-key skip에 안 걸림.
- 완전한 2-데몬 cross-host 통합(단일 호스트 bridge/IP 충돌)은 **수동 멀티호스트 검증**
  으로 분리 명시(anvil skip/제한 기록 규율).

### TDD
- 행동 변경(relay forward, hub accept/reject, 토큰 redaction)은 RED→GREEN.

## 영향 파일 (구현 계획 입력)
- `internal/anvilmcp/daemon_client.go` (SpawnVMRequest)
- `internal/anvilmcp/routed_flock.go` (오케스트레이션, TownWallEnabled)
- `internal/anvilmcp/placement_store.go` (HomeHost, relay token 참조)
- `internal/anvilmcp/runtime_router.go` (reconcile 재등록)
- `internal/anvilmcp/replicating_daemon.go` (post/history 위임)
- `cmd/goose-daemon/orchestrator_api.go` (hub/relay 엔드포인트, 핸들러 분기)
- `internal/orchestrator/townwall.go` (hub roster; 파일/seq는 재사용)
- `internal/storage/provisioner.go` (routed 멤버 주입 — 필드 지원 확인)
- `e2e_test.sh` 또는 신규 `scripts/anvil-cross-host-wall-e2e.sh`
- 테스트: `internal/anvilmcp/*_test.go`, `cmd/goose-daemon/*_test.go`

## 공개 경계 / ADR 후보
- Town Wall은 runtime/operator 표면 유지, IronClaw `anvil_*` 미노출 — `docs/PUBLIC_
  RELEASE_BOUNDARY.md` 확인.
- relay_token redaction·cross-host wall 신뢰 네트워크 전제는 ADR 후보(`docs/ADR_
  INDEX.md`).
