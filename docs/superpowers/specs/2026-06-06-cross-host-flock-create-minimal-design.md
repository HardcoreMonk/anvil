# Cross-host Flock Create 최소 Slice 설계

작성일: 2026-06-06

## 1. 목적

현재 `anvil_spawn_flock`은 scheduler-aware single-host placement를 사용한다. roles 수만큼
tenant quota와 host capacity를 확인한 뒤 하나의 daemon host에서 기존 `POST /flocks`를
실행한다. 2026-06-04 작업으로 `Scheduler.ScheduleFlock`과 scheduler service
`POST /schedule/flock` dry-run planner는 준비됐지만, 실제 VM 생성은 아직 cross-host로
분산하지 않는다.

이번 설계의 목적은 true cross-host flock으로 가기 위한 첫 create slice를 작게 자르는
것이다. 이 slice는 role별 VM을 여러 runtime host에 실제 생성하고, placement/registry와
rollback을 검증한다. 반면 Town Wall coordinator, cross-host `gtcall`, guest flock
context 주입은 아직 구현하지 않는다.

## 2. 현재 사실

- `Scheduler.ScheduleFlock`은 `FlockPlacementPlan`으로 role별 host placement를 dry-run
  계산한다.
- scheduler service `POST /schedule/flock`은 operator planning surface이며 VM을 만들지
  않는다.
- `RuntimeRouter.CreateFlock`은 현재 roles 전체를 한 host에 배치하고 해당 daemon의 기존
  `POST /flocks`를 호출한다.
- daemon `POST /flocks`는 host-local `FlockManager`, Town Wall, watchdog, metadata
  persistence를 모두 소유한다.
- `Daemon` interface는 이미 `SpawnVM`, `Delete`, `CreateFlock`, `DeleteFlock`을 갖고
  있다.
- `SpawnFlockOutput`은 `townwall_url`, `post_url`을 포함한다. 따라서 Town Wall이 없는
  cross-host members-only flock을 기존 `anvil_spawn_flock` 응답처럼 노출하면 사용자가
  기능이 완성된 flock으로 오해할 수 있다.
- `PlacementStoreState`는 VM placement와 flock placement metrics를 저장하지만, routed
  flock registry는 아직 없다.

## 3. 선택안

### 선택: members-only routed flock

router가 `ScheduleFlock` plan에 따라 host별 daemon `POST /vms`를 호출하고, 생성된 VM을
하나의 routed flock registry record로 묶는다. 이 record는 lifecycle cleanup과 inspection을
위한 downstream scheduler state이며, daemon `FlockManager`나 Town Wall을 만들지 않는다.

장점:

- 이미 있는 daemon `POST /vms`와 `DELETE /vms/{vm_id}` 계약만 사용하므로 daemon API
  변경이 작다.
- 실패 시 생성한 VM 목록을 host별로 알고 있어 rollback을 검증하기 쉽다.
- 기존 daemon `POST /flocks` wire contract와 single-host `anvil_spawn_flock` 동작을
  유지한다.
- cross-host capacity와 quota 효과를 실제 VM 생성으로 검증할 수 있다.

단점:

- first slice의 routed flock은 Town Wall, in-VM `/townwall/post`, `gtcall`을 제공하지
  않는다.
- 기존 `SpawnFlockOutput`과 의미가 다르므로 기본 `anvil_spawn_flock`에 바로 연결하면
  혼동이 크다.
- `List/Get/Delete`에 registry-aware routing을 최소한으로 추가해야 create 성공 후 리소스
  누수를 피할 수 있다.

### 비선택: full coordinator flock

`/flocks/{id}/init`, `/flock-agents`, `/flocks/{id}/agents/register`, coordinator Town
Wall, guest callback URL 주입까지 한 번에 구현한다. 최종 목표에는 맞지만 첫 slice로는
rollback, guest behavior, route fan-out, persistence 변경이 너무 크다.

### 비선택: host별 `POST /flocks` subgroup

roles를 host별로 나눠 각 host daemon의 기존 `POST /flocks`를 호출한다. 빠르지만 flock ID와
Town Wall이 host별로 분리되어 하나의 flock 의미가 깨진다. 이 방식은 cross-host create처럼
보이지만 실제로는 여러 host-local flock을 만든다.

## 4. 범위

포함 범위:

- 명시적 opt-in 경로에서만 members-only cross-host create를 활성화한다.
- `RuntimeRouter`가 `ScheduleFlock` plan을 사용해 role별 host daemon `SpawnVM`을 호출한다.
- role별 deterministic `agent_id`를 유지한다. 예: `worker-1`, `worker-2`, `reviewer-1`.
- 생성 성공한 VM은 `PlacementStore`에 `vm_id -> host`로 기록한다.
- routed flock registry를 `PlacementStoreState`에 추가한다.
- create 실패 시 이미 생성한 VM을 host별 daemon `Delete`로 rollback한다.
- routed flock delete는 registry의 member VM을 host별 daemon `Delete`로 정리한다.
- cleanup 실패가 있으면 registry를 `failed_cleanup_pending`으로 남긴다.
- user-facing error, audit, metrics에는 daemon raw body, host endpoint, authorization
  header, `agent_token`을 저장하지 않는다.

제외 범위:

- 기존 daemon `POST /flocks` wire contract 변경.
- 기존 `anvil_spawn_flock` 기본 동작 변경.
- Town Wall 생성, post/history routing, SSE forwarding.
- cross-host `gtcall`.
- guest `/root/.ephemera-flock` 또는 `/root/.ephemera-cp-token` 주입 변경.
- daemon `FlockManager`에 routed flock을 등록하는 동작.
- VM restart/watchdog 같은 flock member 고급 lifecycle.

## 5. 활성화 모델

기본값은 기존 single-host flock create다. members-only cross-host create는 명시적으로 켜야
한다.

설정:

- `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`

동작:

- 설정이 없으면 `anvil_spawn_flock`은 현재처럼 scheduler-aware single-host
  `RuntimeRouter.CreateFlock` 경로를 유지한다.
- `members_only`가 설정되면 새 routed create path를 사용한다.
- routed create path는 `PlacementStore` persistence path가 없으면 실패한다. create 성공 후
  registry/delete가 불가능한 상태로 VM을 만들지 않는다.

## 6. API와 output 계약

첫 slice에서 기존 `anvil_spawn_flock` output shape를 그대로 재사용하지 않는다. Town Wall이
없는 상태에서 `townwall_url`과 `post_url`을 반환하면 잘못된 사용을 유도한다.

최소 slice는 내부 `RuntimeRouter` method와 MCP tool을 분리한다.

- 내부 method: `RuntimeRouter.CreateRoutedFlockMembers`
- MCP tool: `anvil_create_routed_flock_members`

이 MCP tool은 실험적 operator tool로 문서화한다. 기존 `anvil_spawn_flock`은 기본
single-host flock create path로 유지한다.

Output shape:

```go
type RoutedFlockCreateOutput struct {
    FlockID         string              `json:"flock_id"`
    Task            string              `json:"task"`
    TenantID        string              `json:"tenant_id,omitempty"`
    EgressPolicy    string              `json:"egress_policy,omitempty"`
    Mode            string              `json:"mode"`
    Status          string              `json:"status"`
    TownWallEnabled bool                `json:"town_wall_enabled"`
    Agents          []RoutedFlockAgent  `json:"agents"`
}
```

`Mode`는 `cross_host_members_only`다. `TownWallEnabled`는 항상 `false`다. 이 output은
`agent_token`, host endpoint, daemon raw body를 포함하지 않는다.

## 7. Registry 모델

`PlacementStoreState`에 routed flock registry를 추가한다.

```go
type RoutedFlockRecord struct {
    FlockID      string             `json:"flock_id"`
    Task         string             `json:"task"`
    TenantID     string             `json:"tenant_id,omitempty"`
    EgressPolicy string             `json:"egress_policy,omitempty"`
    Mode         string             `json:"mode"`
    Status       string             `json:"status"`
    Agents       []RoutedFlockAgent `json:"agents"`
    CreatedAt    time.Time          `json:"created_at"`
    UpdatedAt    time.Time          `json:"updated_at"`
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

저장 규칙:

- create 시작 전 registry `creating` record를 저장한다.
- agent spawn 성공마다 VM placement와 registry agent를 저장한다.
- registry save 실패는 create 성공으로 처리하지 않는다.
- 모든 agent가 생성되면 `ready`로 저장한다.
- cleanup 실패가 있으면 `failed_cleanup_pending`으로 저장한다.
- registry에는 host name만 저장한다. host endpoint와 authorization header는 저장하지
  않는다.

## 8. Create 흐름

1. input `task`, `tenant_id`, `egress_policy`, `roles`를 기존 helper 의미로 검증한다.
2. `Scheduler.ScheduleFlock`으로 full flock quota와 role별 host capacity를 검증한다.
3. plan denied면 daemon 호출 전에 실패한다.
4. `routed-flock-<timestamp>` 형식의 flock ID를 생성한다.
5. `PlacementStore`에 `creating` registry record를 저장한다.
6. plan agent 순서대로 target host daemon `SpawnVM`을 호출한다.
7. `SpawnVMRequest.Profile`은 agent role을 사용한다.
8. `SpawnVMRequest.TenantID`와 `EgressPolicy`는 normalized plan 값을 사용한다.
9. spawn 성공마다 returned `VMID`, `AgentURL`, host name을 registry와 VM placement에 저장한다.
10. 모든 spawn이 끝나면 registry status를 `ready`로 저장한다.
11. `RoutedFlockCreateOutput`을 반환한다.

이 흐름은 daemon `POST /flocks`를 호출하지 않는다.

## 9. Rollback과 delete 흐름

Create 실패 시:

1. 아직 spawn하지 않은 agent는 건드리지 않는다.
2. 이미 spawn 성공한 VM은 해당 host daemon `Delete(ctx, vmID)`로 삭제한다.
3. delete 성공한 VM은 placement와 registry agent에서 제거하거나 status를 cleanup 완료로
   반영한다.
4. 모든 cleanup이 성공하면 registry record를 `deleted`로 저장하거나 제거한다.
5. cleanup 실패가 있으면 registry status를 `failed_cleanup_pending`으로 저장한다.
6. 반환 error는 host name, VM ID, bounded cleanup reason만 포함한다.

Delete 요청 시:

1. flock ID가 routed registry에 있는지 확인한다.
2. 없으면 기존 daemon `DeleteFlock` fallback을 유지한다.
3. routed flock이면 registry status를 `deleting`으로 저장한다.
4. registry agent의 host/VMID 기준으로 host별 daemon `Delete`를 호출한다.
5. 성공한 VM placement는 제거한다.
6. 모든 delete가 성공하면 registry status를 `deleted`로 저장하거나 record를 제거한다.
7. 일부 실패하면 실패 VM placement는 유지하고 registry status를 `failed_cleanup_pending`으로 둔다.

`PostTownWall`과 `TownWallHistory`는 members-only routed flock에 대해 unsupported error를
반환한다. 이 error는 사용자에게 Town Wall이 이 slice의 범위가 아님을 명확히 알려야 한다.

## 10. 보안

- `agent_token`은 `POST /vms` 응답 외에는 노출하지 않는 불변 조건을 유지한다.
- routed flock output, registry, metrics, audit에는 `agent_token`을 저장하지 않는다.
- daemon raw body는 error에 그대로 넣지 않는다.
- host endpoint와 authorization header는 registry와 user-facing output에 저장하지 않는다.
- scheduler metrics label에는 tenant ID, flock ID, VM ID, host name을 넣지 않는다.
- opt-in flag가 없으면 기존 production path는 바뀌지 않는다.

## 11. Metrics와 audit

기존 flock placement metrics enum을 재사용하되, 첫 slice에서 새 high-cardinality label은
추가하지 않는다.

추가할 bounded outcome:

- `cross_host_success`
- `cross_host_denied`
- `cross_host_spawn_error`
- `cross_host_rollback_error`
- `cross_host_registry_error`

추가할 phase:

- `plan`
- `agent_spawn`
- `registry_save`
- `rollback`
- `total`

첫 slice는 위 bounded outcome/phase enum을 추가한다. 기존 single-host metrics 의미는 유지하고,
members-only routed create path에서만 새 enum을 사용한다. 어떤 경우에도 host name, tenant ID,
flock ID, VM ID는 metric label로 사용하지 않는다.

## 12. 테스트 계획

Router/package tests:

- 두 host가 각각 `AvailableVMs=1`이고 roles가 2개면 각 host daemon `SpawnVM`이 한 번씩
  호출된다.
- quota denied면 daemon `SpawnVM` 호출이 0회다.
- 두 번째 agent spawn 실패 시 첫 번째 VM이 rollback delete된다.
- registry save 실패 시 성공 응답을 반환하지 않고 생성 VM을 rollback한다.
- cleanup 실패 시 registry status가 `failed_cleanup_pending`이 된다.
- routed flock delete는 registry agent의 host별 daemon `Delete`를 호출한다.
- routed flock delete 일부 실패 시 성공한 VM placement만 제거하고 실패 VM placement는 유지한다.
- routed members-only flock에 대한 Town Wall post/history는 unsupported error를 반환한다.
- output/error/audit/metrics에 `agent_token`, authorization header, host endpoint,
  daemon raw body가 포함되지 않는다.

Daemon tests:

- first slice는 daemon `POST /flocks`를 변경하지 않는다는 regression test를 유지한다.
- `DaemonClient.SpawnVM`과 `Delete` 계약만 사용하므로 새 daemon endpoint test는 추가하지 않는다.

MCP/tool tests:

- opt-in flag가 없으면 기존 `anvil_spawn_flock`이 single-host path를 유지한다.
- members-only tool 또는 opt-in path는 `town_wall_enabled=false`를 반환한다.
- `townwall_url`/`post_url`을 성공한 것처럼 반환하지 않는다.

Verification commands:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-mcp -count=1
go test ./cmd/goose-daemon -count=1
go test ./... -count=1
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
go build ./cmd/goose-daemon
git diff --check
```

KVM smoke는 members-only create implementation 후 별도 operator gate로 실행한다.

```bash
scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010
scripts/anvil-mcp-e2e.sh flock
```

`scripts/anvil-mcp-e2e.sh flock`은 기존 single-host flock smoke다. members-only routed
flock smoke는 구현 후 별도 mode로 추가한다.

## 13. 구현 plan 작성 시 분할 기준

구현 plan은 다음 순서로 나눈다.

1. `PlacementStoreState` routed flock registry model과 persistence tests.
2. `RuntimeRouter` members-only create planner/spawn/registry success path.
3. create failure rollback과 sanitized error tests.
4. routed flock delete path.
5. MCP opt-in tool 또는 flag-gated path와 docs.
6. metrics/audit 보강.
7. smoke와 release 문서.

각 단계는 독립 테스트와 commit을 가져야 한다. Town Wall coordinator, cross-host `gtcall`,
guest callback URL 주입은 이 plan의 다음 단계로 끌어오지 않는다.
