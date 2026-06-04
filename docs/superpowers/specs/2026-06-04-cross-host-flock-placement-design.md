# Scheduler-aware Cross-host Flock Placement 설계

작성일: 2026-06-04

## 1. 목적

`anvil-v0.3.2` 이후 `anvil_spawn_flock`은 scheduler-aware single-host placement를
사용한다. 현재 구현은 roles 수만큼 `RequestedActiveVMs`를 계산해 하나의 healthy host를
고르고, 그 host daemon의 기존 `POST /flocks`를 호출한다. 이 방식은 quota/capacity
검증과 placement 기록을 제공하지만, flock member를 여러 runtime host에 분산하지 않는다.

이번 설계의 목적은 true cross-host flock placement로 가는 계약을 잠그는 것이다. 작은
시작은 cross-host placement plan과 운영 가시성이다. 실제 VM 분산 생성은 daemon agent
spawn API, coordinator Town Wall, guest callback URL 주입이 준비된 뒤 진행한다.

## 2. 현재 사실

- `Scheduler.Schedule`은 단일 `ScheduleDecision.Host`만 반환한다.
- `SelectRuntimeHost`는 `RequestedActiveVMs` 전체를 한 host의 `AvailableVMs`와 비교한다.
- daemon `POST /flocks`는 host-local `FlockManager`가 flock ID, metadata, Town Wall,
  watchdog, delete를 모두 소유한다.
- flock VM은 provisioning 중 `/root/.ephemera-flock`에 `FLOCK_ID`, `AGENT_ID`를 받고,
  `/root/.ephemera-cp-token`으로 host daemon에 Town Wall post를 인증한다.
- VM 내부 `/townwall/post` forwarder의 기본 control-plane URL은
  `http://10.0.1.1:3000`이다. 즉 remote host에 배치된 agent는 기본값만으로 coordinator
  host의 Town Wall에 post할 수 없다.
- MCP `Tools`는 `CreateFlock` 외 `ListFlocks`, `GetFlock`, `DeleteFlock`,
  `PostTownWall`, `TownWallHistory`를 daemon interface에 직접 위임한다.
- scheduler state는 JSON file 기반이고, 이미 VM placement, snapshot locality,
  flock placement metrics를 저장한다.

## 3. 범위

포함 범위:

- role별 cross-host placement plan contract를 추가한다.
- planner는 tenant quota를 전체 roles 수 기준으로 한 번 검증하고, host capacity는
  role/agent slot별로 누적 예약해 분산 가능한 host list를 만든다.
- planner 결과는 bounded reason을 가진 allowed/denied decision으로 표현한다.
- scheduler service에 `POST /schedule/flock` dry-run endpoint를 추가할 수 있게 설계한다.
- cross-host VM 생성 단계는 coordinator host와 member host daemon 계약을 명확히 한다.
- cross-host flock registry를 `PlacementStoreState`에 추가하는 모델을 정의한다.
- router가 global flock lifecycle operation을 라우팅하는 책임을 정의한다.
- Town Wall callback URL과 CP token 주입 경계를 정의한다.
- 보안 불변 조건과 rollback 정책을 정의한다.

제외 범위:

- 이 설계 문서 작성 단계에서 코드 구현은 하지 않는다.
- daemon direct `POST /flocks`의 기존 wire contract를 변경하지 않는다.
- 임의 user input으로 remote control-plane URL을 지정하게 하지 않는다.
- `agent_token`, authorization header, daemon raw body를 scheduler state, MCP output,
  audit, metrics에 저장하지 않는다.
- cross-host `gtcall` routing은 후속 설계로 둔다.
- cross-host Town Wall SSE fan-out 최적화는 첫 true implementation 범위에서 제외한다.
- 완전한 multi-process scheduler state file locking은 별도 persistence hardening 과제로
  둔다. 다만 cross-host flock registry save는 실패 시 user-facing 성공으로 처리하지
  않는다.

## 4. 접근안

### 선택안: planner-first + coordinator Town Wall

첫 단계는 `FlockPlacementPlan`을 만들고 scheduler dry-run으로 검증한다. 다음 단계에서
router가 coordinator host를 고르고, member host daemon에 single flock-agent spawn을
요청한다. coordinator daemon은 global flock metadata와 Town Wall을 소유한다. remote
agent VM에는 coordinator URL과 CP token을 주입해 `/townwall/post`가 coordinator로
향하게 한다.

장점:

- single-host flock 동작과 daemon `POST /flocks` 계약을 깨지 않는다.
- placement plan, VM spawn, registry save, Town Wall ownership을 분리해 테스트할 수 있다.
- true cross-host member distribution과 cross-host Town Wall을 모두 만족한다.
- 실패 시 spawned VM 목록을 host별로 추적해 rollback할 수 있다.

단점:

- daemon에 single-agent spawn/register API가 필요하다.
- guest agent가 control-plane URL file을 읽는 작은 guest behavior change가 필요하다.
- router가 `List/Get/Delete/Post/History`까지 global flock registry 기준으로 라우팅해야
  한다.

### 대안 A: host별 `POST /flocks` subgroup

router가 role을 host별로 나누고 각 host daemon의 `POST /flocks`를 호출한다.

이 방식은 빠르지만 global flock ID가 여러 host-local flock ID로 쪼개진다. Town Wall도
host별로 분리되고 `anvil_get_flock`, `anvil_delete_flock`, in-VM `gtwall` 의미가
흐려진다. cross-host placement처럼 보이지만 true flock semantics가 아니다.

### 대안 B: daemon이 remote daemon을 직접 제어

coordinator daemon이 scheduler와 remote daemon clients를 갖고 다른 host까지 VM을 만든다.

이 방식은 daemon 책임이 scheduler/router와 섞인다. token 경계, host inventory, quota,
placement persistence가 daemon으로 역류하므로 anvil control-plane 분리 원칙과 맞지
않는다.

## 5. 선택한 설계

선택한 설계는 planner-first + coordinator Town Wall이다.

단계는 세 개로 나눈다.

1. Planner slice:
   - `ScheduleFlock` helper를 추가한다.
   - scheduler service는 `POST /schedule/flock` dry-run을 제공한다.
   - MCP router는 아직 VM을 만들지 않고 plan을 검증할 수 있다.
2. Cross-host create slice:
   - daemon에 coordinator flock init/register와 single flock-agent spawn API를 추가한다.
   - guest에 coordinator control-plane URL을 파일로 주입한다.
   - router가 plan대로 agent VM을 만들고 registry를 저장한다.
3. Lifecycle slice:
   - `ReplicatingDaemon` 또는 새 router-aware wrapper가 global flock의
     `List/Get/Delete/Post/History`를 router로 위임한다.
   - registry에 없는 flock은 기존 base daemon path로 fallback한다.

이 순서는 실패 blast radius를 작게 유지한다. planner slice만으로도 host capacity 편향,
quota 거부, no eligible host를 운영자가 확인할 수 있다. VM 생성 slice는 planner 계약이
검증된 뒤 붙인다.

## 6. Planner contract

새 internal type 예시:

```go
type FlockPlacementPlanRequest struct {
    TenantID     string
    EgressPolicy EgressPolicy
    Roles        []string
}

type FlockPlacementPlan struct {
    Allowed           bool
    Reason            string
    TenantID          string
    EgressPolicy      EgressPolicy
    Agents            []FlockAgentPlacement
    Requested         TenantUsage
    CurrentUsage      TenantUsage
    Limit             TenantQuota
    HostStatusSummary HostStatusSummary
}

type FlockAgentPlacement struct {
    AgentID string
    Role    string
    Host    RuntimeHost
}
```

Planner 규칙:

- `roles`는 기존 `normalizeFlockRoles`와 같은 의미로 검증한다.
- `agent_id`는 기존 daemon 규칙처럼 role별 sequence를 사용한다.
- tenant quota는 `ActiveVMs=len(roles)` 기준으로 한 번 검증한다.
- host selection은 agent slot마다 `RequestedActiveVMs=1`로 수행한다.
- plan 내부에서는 host별 reserved count를 누적해 같은 plan이 capacity를 초과하지 않게
  한다.
- preferred/excluded host 입력은 첫 planner slice에서 추가하지 않는다.
- denied reason은 `quota_exceeded`, `no_eligible_host`, `invalid_request`, `unknown` 중
  하나로 제한한다.

Scheduler service endpoint 예시:

```text
POST /schedule/flock
```

Request body:

```json
{
  "tenant_id": "tenant-1",
  "egress_policy": "profile",
  "roles": ["researcher", "worker", "reviewer"]
}
```

Response body는 `FlockPlacementPlan` JSON이다. Host endpoint는 scheduler의 기존
operator API와 같은 trust boundary 안에서만 노출된다. MCP user-facing response에는 plan의
host endpoint를 노출하지 않는다.

## 7. Cross-host create contract

### Coordinator flock init

coordinator daemon은 global flock ID와 Town Wall을 소유한다. 기존 `POST /flocks`는
변경하지 않고 새 internal endpoint를 둔다.

```text
POST /flocks/{flock_id}/init
```

Request:

```json
{
  "task": "review worker output",
  "tenant_id": "tenant-1",
  "egress_policy": "profile"
}
```

이 endpoint는 VM을 만들지 않는다. `FlockManager`에 empty flock을 만들고 metadata와
Town Wall을 준비한다.

### Member agent spawn

member host daemon은 caller가 지정한 global flock identity로 단일 agent VM을 만든다.

```text
POST /flock-agents
```

Request:

```json
{
  "flock_id": "flock-1780550000000000000",
  "agent_id": "worker-1",
  "role": "worker",
  "tenant_id": "tenant-1",
  "egress_policy": "profile",
  "control_plane_url": "https://anvil-runtime.example.com"
}
```

Response:

```json
{
  "agent_id": "worker-1",
  "role": "worker",
  "vm_id": "vm-1780550000000000001",
  "agent_url": "http://10.0.1.20:8080",
  "status": "ready"
}
```

`control_plane_url`은 operator config에서 온 값이어야 한다. user input을 그대로 전달하지
않는다. daemon은 `spawnVMForFlock` 경로를 재사용하되, `VMPrepareOptions`에
`ControlPlaneURL`을 추가해 guest에 `/root/.ephemera-cp-url`을 주입한다.

### Coordinator agent register

router는 member agent spawn 성공 후 coordinator daemon에 agent metadata를 등록한다.

```text
POST /flocks/{flock_id}/agents/register
```

Request:

```json
{
  "agent_id": "worker-1",
  "role": "worker",
  "vm_id": "vm-1780550000000000001",
  "agent_url": "http://10.0.1.20:8080",
  "status": "ready"
}
```

coordinator daemon은 `FlockManager.AddAgent`와 `Flock.Persist`를 수행한다. 이 API는
`agent_token`을 받거나 반환하지 않는다.

## 8. Registry state

`PlacementStoreState`에 routed flock registry를 추가한다.

```go
type RoutedFlockRecord struct {
    FlockID         string              `json:"flock_id"`
    Task            string              `json:"task"`
    TenantID        string              `json:"tenant_id,omitempty"`
    EgressPolicy    string              `json:"egress_policy,omitempty"`
    CoordinatorHost string              `json:"coordinator_host"`
    TownWallURL     string              `json:"townwall_url,omitempty"`
    PostURL         string              `json:"post_url,omitempty"`
    Status          string              `json:"status"`
    Agents          []RoutedFlockAgent  `json:"agents"`
    CreatedAt       time.Time           `json:"created_at"`
    UpdatedAt       time.Time           `json:"updated_at"`
}

type RoutedFlockAgent struct {
    AgentID  string `json:"agent_id"`
    Role     string `json:"role"`
    VMID     string `json:"vm_id"`
    AgentURL string `json:"agent_url"`
    Host     string `json:"host"`
    Status   string `json:"status"`
}
```

허용 status:

- `creating`
- `ready`
- `deleting`
- `deleted`
- `failed_cleanup_pending`

Registry 저장 규칙:

- cross-host create는 persistent placement store path가 없으면 활성화하지 않는다.
- router는 coordinator init 전에 `creating` record를 저장한다.
- agent spawn 성공마다 VM placement와 flock registry를 같은 placement store update 안에
  반영한다.
- registry save 실패는 성공 응답으로 처리하지 않는다.
- cleanup 후에도 실패가 남으면 `failed_cleanup_pending`으로 저장하고 sanitized error를
  반환한다.
- registry에는 host endpoint, bearer token, daemon raw body를 저장하지 않는다.

## 9. Router lifecycle

`RuntimeRouter.CreateFlock`은 cross-host mode가 켜졌을 때 다음 순서로 동작한다.

1. 입력을 검증하고 `FlockPlacementPlan`을 만든다.
2. plan이 denied면 daemon 호출 전에 실패한다.
3. coordinator host를 선택한다. 첫 구현은 plan의 첫 agent host를 coordinator로 사용한다.
4. global `flock_id`를 생성하고 registry `creating` record를 저장한다.
5. coordinator daemon에 `/flocks/{flock_id}/init`을 호출한다.
6. plan agent 순서대로 member host daemon에 `/flock-agents`를 호출한다.
7. 각 member spawn 성공 후 coordinator daemon에 `/agents/register`를 호출한다.
8. placement store에 `vm_id -> host`와 flock registry agent를 저장한다.
9. 모든 agent 등록이 끝나면 registry status를 `ready`로 저장한다.
10. existing `FlockCreateResponse` shape로 응답한다.

`ReplicatingDaemon`은 global flock registry를 조회할 수 있는 router interface를 감지해
다음 method를 위임한다.

- `CreateFlock`: cross-host mode면 router, 아니면 기존 single-host router/base daemon
- `ListFlocks`: registry flocks와 base daemon flocks를 병합
- `GetFlock`: registry에 있으면 router, 없으면 base daemon
- `DeleteFlock`: registry에 있으면 router cleanup, 없으면 base daemon
- `PostTownWall`: registry에 있으면 coordinator daemon, 없으면 base daemon
- `TownWallHistory`: registry에 있으면 coordinator daemon, 없으면 base daemon

## 10. Rollback

Create 실패 시 cleanup 순서:

1. 아직 spawn하지 않은 agent는 건드리지 않는다.
2. spawn 성공한 VM은 placement registry에 기록된 host daemon의 `DELETE /vms/{vm_id}`로
   삭제한다.
3. coordinator flock metadata는 `/flocks/{flock_id}` delete 또는 init rollback endpoint로
   제거한다.
4. cleanup이 모두 성공하면 registry record를 삭제하거나 `deleted`로 남긴다.
5. cleanup 실패가 하나라도 있으면 `failed_cleanup_pending`으로 남기고 실패 host와 VM ID만
   sanitized context로 보고한다.

Delete 실패 시 cleanup 의미:

- registry flock delete는 best-effort 병렬 VM delete를 사용한다.
- 일부 VM delete 실패 시 user-facing error를 반환하고 registry status를
  `failed_cleanup_pending`으로 둔다.
- 성공한 VM placement는 제거하고, 실패한 VM placement는 유지한다.

## 11. Security

- `agent_token`은 `POST /vms` 응답 외 어디에도 노출하지 않는 기존 불변 조건을 유지한다.
- cross-host daemon APIs는 기존 daemon auth boundary 뒤에서만 사용한다.
- `control_plane_url`은 operator config 또는 coordinator host public URL에서 파생한다.
  MCP user input으로 받지 않는다.
- CP token은 guest file에만 주입하고 MCP output, scheduler state, metrics, audit에 저장하지
  않는다.
- Registry에는 host name은 저장하지만 host endpoint와 authorization header는 저장하지
  않는다.
- daemon raw error body는 router error, audit, metrics에 그대로 넣지 않는다.
- scheduler metrics labels는 기존처럼 bounded enum만 사용한다.

## 12. Metrics

기존 `anvil_scheduler_flock_placement_*` metrics에 cross-host phase를 추가할 수 있다.

추가 phase 후보:

- `plan`
- `coordinator_init`
- `agent_spawn`
- `agent_register`
- `rollback`

추가 outcome 후보:

- `cross_host_success`
- `cross_host_denied`
- `coordinator_error`
- `agent_spawn_error`
- `agent_register_error`
- `rollback_error`

Host name, tenant ID, flock ID, VM ID는 metric label로 사용하지 않는다. host별 편향은
별도 low-cardinality 운영 report나 scheduler state inspection으로 다룬다.

## 13. Test plan

Planner tests:

- quota 초과 시 plan denied, agent placement 없음.
- 두 host가 각각 `AvailableVMs=1`이고 roles가 2개면 agent가 두 host에 분산된다.
- 한 host capacity가 충분하면 기존 deterministic ordering에 따라 같은 host에 배치된다.
- egress policy를 지원하지 않는 host는 제외된다.
- plan 내부 reservation으로 같은 host capacity를 초과하지 않는다.

Daemon tests:

- `/flocks/{id}/init`은 empty flock과 Town Wall을 만들고 VM을 spawn하지 않는다.
- `/flock-agents`는 `FLOCK_ID`, `AGENT_ID`, CP token, control-plane URL을 guest prepare
  options에 전달한다.
- `/flocks/{id}/agents/register`는 agent metadata를 추가하고 persist한다.
- 새 APIs는 `agent_token`을 응답하지 않는다.

Router tests:

- cross-host `CreateFlock`이 두 host daemon에 single-agent spawn을 호출하고 coordinator에
  agent를 등록한다.
- member spawn 실패 시 이미 만든 VM을 삭제하고 registry를 `failed_cleanup_pending` 또는
  removed 상태로 정리한다.
- registry save 실패 시 성공 응답을 반환하지 않는다.
- `Get/Delete/Post/History`는 registry flock이면 coordinator/placement host로 라우팅한다.
- registry에 없는 flock은 base daemon으로 fallback한다.

Verification commands:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/goose-daemon -count=1
go test ./cmd/anvil-mcp -count=1
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
git diff --check
```

KVM smoke는 true create slice 이후에 실행한다.

```bash
scripts/anvil-mcp-e2e.sh flock
scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010
```

## 14. Documentation updates

Planner slice:

- `README.md`: `POST /schedule/flock` dry-run과 single-host/cross-host 상태 구분.
- `docs/architecture/mcp-architecture.md`: `anvil_spawn_flock` router modes.
- `docs/architecture/multi-tenant-roadmap.md`: cross-host planner 완료, true create 잔여
  단계.
- `RELEASE_NOTES.md`: Unreleased에 planner 추가.

True create slice:

- `README.md`: coordinator Town Wall, member host spawn, registry cleanup 의미.
- `docs/operations/runbook.md`: `failed_cleanup_pending` 처리.
- `docs/operations/observability.md`: cross-host flock placement metrics phase/outcome.
- release handoff: KVM smoke 결과와 잔여 위험.

## 15. 잔여 위험

- cross-host flock registry는 state file persistence 신뢰도에 더 민감하다. 구현 전에
  placement store update 단위를 더 엄격히 검증해야 한다.
- guest가 coordinator URL에 도달하려면 host networking, reverse proxy, egress policy가
  맞아야 한다.
- cross-host `gtcall`은 이 설계에 포함하지 않으므로 agent-to-agent direct call 기대는
  별도 문서에서 다룬다.
- coordinator daemon 장애 시 Town Wall은 coordinator 복구에 의존한다. scheduler가
  coordinator failover를 자동 수행하는 것은 첫 true implementation 범위가 아니다.
