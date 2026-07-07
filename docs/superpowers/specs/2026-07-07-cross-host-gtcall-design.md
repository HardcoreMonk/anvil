# Cross-host gtcall 설계 (home 경유 2-hop + call_token)

- 작성일: 2026-07-07
- 상태: 설계 확정 (구현 전)
- 선행: [`docs/operations/2026-07-06-cross-host-town-wall-handoff.md`](../../operations/2026-07-06-cross-host-town-wall-handoff.md)
  ("cross-host `gtcall`" follow-up),
  [`docs/operations/2026-07-07-reconcile-loop-handoff.md`](../../operations/2026-07-07-reconcile-loop-handoff.md)
- 관련 코드: `scripts/gtcall`, `cmd/goose-daemon/orchestrator_api.go`,
  `cmd/goose-daemon/api.go`(authMiddleware), `internal/orchestrator/flock.go`,
  `internal/anvilmcp/routed_flock.go`, `internal/anvilmcp/placement_store.go`

## 문제

단일 host flock의 `gtcall <agent_id> <prompt>`는 guest가 로컬 daemon에
① `GET /flocks/{id}`로 agent_id→vm_id를 해석하고 ② `POST /vms/{vm_id}/tasks`로
프롬프트를 보내는 2-step 계약이다(daemon이 대상 VM의 `agent_token`을 로컬에서
주입 — 호출자는 peer credential을 모른다). routed flock에서는 대상 agent가 다른
host에 있을 수 있어 두 단계 모두 로컬 daemon에서 실패한다. cross-host shared
Town Wall(PR #19)은 broadcast/observe만 다뤘고 지정 agent 호출은 비범위였다.

## 결정 (사용자 확정 3건)

1. **완전 parity**: routed flock의 어느 member든 다른 어느 member를 호출할 수
   있다. guest는 bridge-only 유지(로컬 daemon만 접촉). 단일 host flock 경로는
   semantics 불변.
2. **별도 `call_token`**: daemon-to-daemon call hop은 relay_token이 아니라
   per-flock **call 전용 secret**으로 인증한다. relay_token 유출의 blast
   radius(wall append)와 call_token 유출의 blast radius(임의 member 프롬프트
   실행)를 분리한다.
3. **home 경유 2-hop**: member → home → target host. member daemon이 아는 원격
   주소는 지금처럼 `HomeAddr` 하나뿐이고, 해석(agent→host,vm)은 canonical
   roster를 가진 hub 한 곳에서 한다. home SPOF는 wall이 이미 가진 성질과 동일
   (1차 수용). LLM 지연(수십 초)이 지배적이라 hop 추가 비용은 무시 가능.

## Guest 계약 — 새 단일 endpoint

daemon에 **`POST /flocks/{id}/call`** (body `{agent_id, prompt}`)을 신설하고
`scripts/gtcall`을 이 경로로 전환한다.

- **local/hub flock**: 로컬 roster/Agents에서 agent_id 해석 후 기존
  `/vms/{vm_id}/tasks` dispatch 로직 재사용(agent_token 주입은 지금처럼 로컬).
- **relay flock**: `{agent_id, prompt}`를 home daemon의 같은 경로로 forward.
- 기존 2-step 경로(`GET /flocks/{id}` + `POST /vms/{vm_id}/tasks`)는 하위호환
  으로 유지한다 — 제거·약화 없음. 단일 host에서 새 경로와 기존 경로의 응답
  semantics는 동일하다(TaskResult.output 텍스트).
- `scripts/gtcall` 변경은 provisioner `EnsureGoldenImage`의 build-input 감지가
  자동으로 golden image를 rebuild하므로 별도 운영 절차가 없다.

## 해석과 실행 (2-hop)

- hub 등록(`POST /flocks/{id}/distributed`)의 roster member를
  `{AgentID, Host}` → `{AgentID, Host, VMID, Addr}`로 확장한다. home이
  agent→(host, vm_id)를 canonical하게 해석한다. `Addr`(해당 host daemon의
  control-plane 주소)는 home→target 2번째 hop에 필요하다 — home daemon은
  daemon 주소록을 갖지 않으므로(주소록은 anvilmcp control plane 소유) 등록
  시점에 배포한다. `Addr`는 daemon 내부 용도이며 로그·에러 문자열·직렬화
  표면에는 노출하지 않는다(기존 redaction 규율).
- 최초 hub 등록은 spawn 전이라 VMID가 없다 — spawn 완료 후 `record.Agents`
  기반의 VMID 포함 roster로 **재등록**하고, daemon의 hub idempotent 경로가
  재등록 시 Roster를 갱신하도록 수정한다(현행은 token만 재주입하고 roster를
  버림). reconcile 재등록도 같은 VMID 포함 roster를 보낸다.
- home의 로컬 대상 판정은 host 이름 비교가 아니라 **roster VMID가 자기 로컬
  VM registry에 존재하는지**로 한다(daemon은 자기 control-plane host 이름을
  모른다).
- home 수신 call: target이 home 자신의 host면 로컬 dispatch, 아니면 target
  host daemon의 `POST /flocks/{id}/call`로 2번째 hop.
- **루프 방지**: hop 요청에 forward 표식 헤더 `X-Ephemera-Call-Hop: 1`을
  붙이고, 이 표식이 있는 요청은 어느 daemon에서도 다시 forward하지 않는다
  (로컬 해석 실패 시 즉시 에러).
- **depth 가드**: `X-Ephemera-Task-Depth`를 전 hop에서 그대로 전파해 기존
  `EPHEMERA_MAX_TASK_DEPTH`(기본 5, 한계 시 `508`)가 cross-host에서도 성립한다.

## 보안

- **`call_token`**: per-flock call 전용 secret. relay_token과 완전히 나란한
  규율 —
  - `PlacementStore` 영속(전용 map, `State()` 등 모든 직렬화 표면에서 redaction),
  - authMiddleware가 **해당 flock의 `/flocks/{id}/call` 경로만** admit
    (control-plane bearer로 승격 금지, wall 경로 거부),
  - admit 시 `countAuth("call")` + synthetic identity `call:<flockID>` 기록,
  - hub/relay 등록 요청에 relay_token과 함께 배포, reconcile이 재주입,
    rollback/delete가 revoke.
  - relay_token은 call 경로를 admit하지 않고, call_token은 wall 경로를 admit
    하지 않는다(상호 배타 — 테스트로 고정).
- 대상 VM의 `agent_token`은 target host daemon 로컬에서만 주입된다. wire를
  건너는 것은 `{agent_id, prompt}` + depth/hop 헤더뿐 — per-VM token, CP
  token, provider key 미노출(sentinel 테스트).
- guest는 신규 네트워크 노출 없음(로컬 daemon만). 신규 `anvil_*` MCP tool
  없음 — 기존 IronClaw schema-exclusion guard가 계속 통과해야 한다.
- 에러 문자열은 flock/host/agent 식별자만 — daemon 주소·토큰을 bound out
  (reconcile-loop slice에서 확립한 규율, `d5c7df0` 참조).

## 에러 처리·한계

- home 불가 → member daemon이 `502` + host-식별자-only 진단. target 불가 →
  home이 `502` 중계. agent_id 미존재 → `404`. retry 없음(agent 재시도 위임 —
  wall과 동일).
- timeout 계단: guest→local `300s`(현행 gtcall 상속) > member→home `290s` >
  home→target `280s` — 안쪽 hop이 먼저 끊겨 바깥 hop이 진단을 남길 수 있게.
- **home SPOF**(1차 수용, wall과 동일 — mesh 진화 경로 공유), cross-host
  broadcast fan-out은 계속 비범위.

## 테스트 (TDD)

- 유닛: roster VMID 해석; local/hub/relay 분기; hop 표식 루프 방지; call_token
  admission 경로 한정(call만 admit, wall 거부, 타 flock 거부, relay_token으로
  call 경로 접근 거부); redaction guard(응답/로그에 토큰·주소 무노출); depth
  헤더 전파; timeout 계단.
- KVM e2e: `scripts/anvil-cross-host-wall-e2e.sh` 패턴 재사용 — real member
  VM에서 `gtcall` → member daemon → stub home 수신 payload가
  `{agent_id, prompt}`+헤더뿐임을 sentinel로 검증하고, stub이 응답을 되돌려
  guest까지 왕복을 확인.

## 문서 반영 (구현 시)

- `docs/PUBLIC_RELEASE_BOUNDARY.md`·`docs/ADR_INDEX.md`에 call_token 행,
- `CONTEXT.md` 용어집(call_token)·완료 목록, `README.md`, runbook,
- town wall handoff의 "cross-host `gtcall`" follow-up CLOSED 표기,
- slice handoff 신설.

## 비목표

- cross-host broadcast fan-out, home SPOF 제거(mesh), call retry/queue.
- 기존 2-step gtcall 계약의 제거 또는 변경.
- call 결과의 wall 게시 등 부수 동작(호출은 순수 왕복).
