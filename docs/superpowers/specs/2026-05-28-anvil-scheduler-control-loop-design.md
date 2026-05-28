# Anvil Scheduler Control Loop Design

## 목표

anvil scheduler를 단순 HTTP scheduling service에서 운영자가 장시간 믿고 돌릴 수
있는 scheduler control plane process로 확장한다. 이번 작업은 anvil 자체 기능
개선이며, upstream ephemera `v0.4.0` PR-A storage/recovery 변경은 범위에서 제외한다.

핵심 목표는 host inventory polling과 placement reconciliation을 하나의 scheduler
control loop로 묶는 것이다. scheduler는 daemon host의 현재 상태를 주기적으로 관측하고,
VM placement state가 실제 daemon 상태와 얼마나 확실히 일치하는지 운영 API로 보여준다.

## 비목표

- upstream ephemera `v0.4.0` PR-A 병합 또는 adoption review
- cross-host snapshot replication
- scheduler-aware cross-host flock placement
- daemon self-registration 또는 host별 scheduler token
- multi-node HA, leader election, external database-backed state store
- UI, billing, OpenClaw compatibility layer
- Firecracker/KVM integration behavior 변경

## 현재 기반

현재 코드에는 control loop를 만들 수 있는 조각이 이미 있다.

- `HostInventory.PollOnce`: daemon host의 `/health`를 호출해 `healthy`,
  `available_vms`, `available_snapshot_bytes`, `egress_policies`를 갱신한다.
- `RuntimeRouter.ReconcilePlacements`: daemon `GET /vms`를 읽어 VM placement map을
  실제 live VM 목록 기준으로 교체한다.
- `PlacementStore`: host, VM placement, snapshot location을 JSON state로 저장한다.
- `cmd/anvil-scheduler`: persistent state와 quota store를 읽어 scheduler HTTP
  service를 실행한다.

빠진 부분은 위 기능들을 `cmd/anvil-scheduler` 안에서 주기적으로 실행하고, 실패 상태를
운영자가 볼 수 있는 명시적인 model로 저장하는 control loop다.

## Architecture

설계의 중심은 "선언된 host 의도"와 "관측된 runtime 상태"를 분리하는 것이다.

### HostConfig

`HostConfig`는 config file에서 온 정적 운영 의도다.

- `name`
- `endpoint`
- `egress_policies`
- `smoke_only`

정적 값은 scheduler가 재시작되더라도 config가 우선한다. 같은 host name이 persisted
state와 config file에 동시에 있으면 config file의 정적 값이 이긴다.
config file에서 온 host는 `config_managed=true`로 표시한다.

### HostObservation

`HostObservation`은 control loop가 poll 결과로 갱신하는 동적 상태다.

- `status`: `healthy`, `degraded`, `unhealthy`
- `available_vms`
- `available_snapshot_bytes`
- `failure_count`
- `last_success_at`
- `last_failure_at`
- `last_error`

동적 값은 daemon `/health` 응답과 poll failure에서만 갱신한다. 사람이 config file에
적는 값이 아니다.

### PlacementStore 확장

`PlacementStore`는 기존 state를 유지하되 control loop state를 추가로 보존한다.

- 기존 `hosts`
- 기존 `vm_placements`
- 기존 `snapshot_locations`
- 신규 `config_managed_hosts`
- 신규 `host_observations`
- 신규 `suspect_vm_placements`
- 신규 `control_loop_status`

기존 state file을 읽을 수 있어야 하므로 신규 field는 optional로 취급한다. 누락된
map은 load 시 빈 map으로 normalize한다.

### SchedulerControlLoop

`SchedulerControlLoop`는 scheduler process 내부에서 실행된다.

- poll tick: host `/health` polling
- reconcile tick: daemon `GET /vms` 기반 placement reconciliation
- status update: last run time, last error, persistence health 기록

HTTP service와 같은 process 안에서 동작하지만, control loop failure가 HTTP service를
죽이지 않는다. startup config parse 실패와 state load 실패만 fatal이다.

## Data Flow

### Startup

1. `cmd/anvil-scheduler`가 기존 scheduler config를 읽는다.
2. `ANVIL_SCHEDULER_HOSTS_FILE`이 있으면 host bootstrap config를 읽는다.
3. `ANVIL_SCHEDULER_STATE`의 persisted state를 읽는다.
4. 같은 host name이 config와 state에 모두 있으면 정적 값은 config가 이긴다.
5. poll로 관측할 dynamic fields는 기존 observation을 보존하되, config가 바뀐 host는
   다음 poll에서 갱신한다.
6. HTTP service와 control loop를 시작한다.

### Poll Tick

1. 각 host의 `endpoint + /health`를 호출한다.
2. 성공하면 host는 `healthy`가 된다.
3. 성공한 host는 `failure_count=0`, `last_success_at=now`, capacity fields를 갱신한다.
4. 첫 실패부터 host는 `degraded`가 된다.
5. failure count가 threshold 이상이면 host는 `unhealthy`가 된다.
6. poll 결과는 `PlacementStore`에 저장한다.

### Reconcile Tick

1. `healthy` host에 대해서만 daemon `GET /vms`를 호출한다.
2. 응답한 host의 VM placement는 확정 상태로 갱신한다.
3. 응답하지 않는 host의 기존 placement는 삭제하지 않는다.
4. `degraded` 또는 `unhealthy` host의 VM placement는 `suspect_vm_placements`에 표시한다.
5. host가 다시 `healthy`가 되면 해당 host의 `GET /vms` 결과로 stale placement를
   정리한다.

### Scheduling Request

`POST /schedule/spawn`과 `POST /schedule/restore`는 `healthy` host만 후보로 사용한다.
`degraded` 또는 `unhealthy` host는 `PreferredHosts`에 들어 있어도 선택하지 않는다.
선택 실패 사유는 기존 `no_eligible_host`를 유지하되, response에는
`host_status_summary`를 추가한다. 이 summary는 `healthy`, `degraded`, `unhealthy`
host count를 담는다.

## Host State Model

Host status는 세 단계다.

| Status | 의미 | 신규 placement 후보 |
|---|---|---|
| `healthy` | 최근 poll 성공. capacity와 policy 정보가 신뢰 가능하다. | 포함 |
| `degraded` | 첫 poll 실패 이후 threshold 미만 실패 상태. 일시 장애일 수 있다. | 제외 |
| `unhealthy` | 연속 실패가 threshold 이상이다. 운영자가 봐야 하는 장애 상태다. | 제외 |

poll이 다시 성공하면 `healthy`로 복구한다. 복구 시 `failure_count`는 `0`으로 reset한다.

## Endpoint 변경

### `GET /placements`

기존 response는 유지하고 신규 field를 additive로 추가한다.

```json
{
  "hosts": {},
  "vm_placements": {},
  "snapshot_locations": {},
  "config_managed_hosts": {},
  "host_observations": {},
  "suspect_vm_placements": {}
}
```

`suspect_vm_placements`는 VM ID를 key로 하고, 마지막으로 알려진 host와 이유를 값으로
둔다. 이 정보는 운영자가 장애 중에 "이 VM은 어느 host에 있었는가"를 판단하는 데
사용한다.

### `GET /control-loop/status`

control loop 자체의 상태를 반환한다.

```json
{
  "running": true,
  "poll_interval_seconds": 10,
  "reconcile_interval_seconds": 30,
  "failure_threshold": 3,
  "persistence_degraded": false,
  "last_poll_started_at": "2026-05-28T00:00:00Z",
  "last_poll_completed_at": "2026-05-28T00:00:01Z",
  "last_reconcile_started_at": "2026-05-28T00:00:00Z",
  "last_reconcile_completed_at": "2026-05-28T00:00:02Z",
  "last_error": "",
  "hosts": {}
}
```

`last_error`는 sanitized message만 담는다. token, raw daemon body, snapshot metadata,
`agent_token`은 저장하거나 반환하지 않는다.

### `GET /hosts`

host static config와 observation을 함께 볼 수 있게 한다. 기존 client가 깨지지 않도록
기존 `RuntimeHost` field는 유지한다. `observation`과 `config_managed`는 새 field로
붙인다.

### `PUT /hosts`

runtime host upsert는 계속 지원한다. 같은 host name이 config file에 있으면 restart 시
config의 정적 값이 다시 이긴다. 즉 `/hosts`는 운영 중 긴급 수정 또는 config 밖 host
등록에 적합하지만, config-managed host의 장기 truth source는 config file이다.

### `DELETE /hosts/{name}`

config-managed host 삭제는 거부한다. runtime-added host 삭제는 허용한다. config-managed
host는 config file에서 제거하고 service를 재시작해야 사라진다.

## Configuration

기존 환경 변수는 유지한다.

- `ANVIL_SCHEDULER_ADDR`
- `ANVIL_SCHEDULER_STATE`
- `ANVIL_SCHEDULER_QUOTA_STORE`

신규 환경 변수는 additive다.

| Env | 기본값 | 의미 |
|---|---:|---|
| `ANVIL_SCHEDULER_HOSTS_FILE` | empty | scheduler bootstrap host JSON file |
| `ANVIL_SCHEDULER_POLL_INTERVAL` | `10s` | host health poll interval |
| `ANVIL_SCHEDULER_RECONCILE_INTERVAL` | `30s` | placement reconciliation interval |
| `ANVIL_SCHEDULER_HOST_TIMEOUT` | `3s` | daemon host HTTP timeout |
| `ANVIL_SCHEDULER_FAILURE_THRESHOLD` | `3` | `unhealthy` 전이까지 필요한 연속 실패 횟수 |
| `ANVIL_SCHEDULER_API_TOKEN` | empty | daemon host 호출용 bearer token |
| `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE` | `false` | persistence degraded 상태에서 schedule 거부 여부 |

Hosts file은 JSON을 사용한다. Go 표준 라이브러리로 처리할 수 있고, 새 dependency를
추가하지 않는다.

```json
{
  "hosts": [
    {
      "name": "host-a",
      "endpoint": "http://127.0.0.1:3000",
      "egress_policies": ["profile", "deny_all"],
      "smoke_only": false
    }
  ]
}
```

## Error Handling

### Startup Failure

다음 오류는 startup fatal이다.

- hosts file parse 실패
- persisted placement state load 실패
- quota store load 실패

잘못된 config나 깨진 state로 scheduler가 시작되면 잘못된 placement decision을 만들 수
있으므로 즉시 종료한다.

### Poll Failure

single host poll failure는 scheduler service를 죽이지 않는다.

- 첫 실패: `degraded`
- threshold 이상 연속 실패: `unhealthy`
- 성공: `healthy`

각 host의 failure count와 last error를 observation에 저장한다. error는 sanitized
message만 저장한다.

### Reconciliation Failure

reconciliation 실패는 기존 placement를 삭제하지 않는다. 실패한 host의 placement는
`suspect`로 표시하고 다음 tick에서 재시도한다.

### Store Save Failure

store save failure는 process fatal이 아니다.

- in-memory state는 유지한다.
- `/control-loop/status`에 `persistence_degraded=true`를 표시한다.
- `last_error`에 sanitized save error를 기록한다.
- `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE=true`이면 `/schedule/*`는 거부한다.
- 기본값에서는 scheduling을 계속 허용한다.

## Security

- `ANVIL_SCHEDULER_API_TOKEN`은 daemon host 호출에만 사용한다.
- host별 token은 이번 범위에서 제외한다.
- control loop status에는 token, authorization header, raw daemon body를 저장하지 않는다.
- `agent_token`은 기존 불변 조건대로 `POST /vms` 응답 외에는 노출하지 않는다.
- scheduler service 자체에는 bearer auth를 추가하지 않는다. 기존 운영 문서대로 loopback
  또는 private network/reverse proxy policy 뒤에서 운영한다.

## Testing Strategy

### Unit Tests

- config host와 persisted host 병합
- poll 성공 시 `healthy` 전이와 capacity 갱신
- 첫 실패 시 `degraded`, threshold 도달 시 `unhealthy`
- `degraded`와 `unhealthy` host가 schedule 후보에서 제외되는지
- unhealthy host placement가 삭제되지 않고 `suspect`로 표시되는지
- 복구 후 reconciliation으로 stale placement가 정리되는지
- store save 실패 시 `persistence_degraded=true`

### SchedulerService Tests

- `GET /placements`가 observations와 suspect placements를 반환
- `GET /control-loop/status`가 last tick, host status, last error를 반환
- config-managed host delete가 거부되는지
- runtime-added host delete가 허용되는지
- `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE=true`일 때 persistence degraded 상태에서
  schedule을 거부하는지

### Binary Config Tests

- 신규 env parsing
- hosts file parse
- interval, timeout, threshold 기본값
- invalid duration 또는 threshold 값은 startup config error

### Smoke Script Tests

- `scripts/anvil-scheduler-smoke.sh`가 `/control-loop/status`를 확인
- fake scheduler test에서 기존 `/health`, `/hosts`, `/schedule/spawn`,
  `/placements`와 신규 `/control-loop/status` response shape를 검증

### Verification Gates

```bash
go test ./...
go build ./cmd/anvil-scheduler
bash -n scripts/anvil-scheduler-smoke.sh
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --no-build --no-enable --verify
```

이번 범위는 scheduler control loop와 daemon API mocking이 핵심이다. Firecracker/KVM
통합 검증은 필요하지 않다.

## Rollout

1. 기본값에서는 `ANVIL_SCHEDULER_HOSTS_FILE`이 없으면 기존 persisted hosts만 사용한다.
2. control loop는 기본 실행한다. `ANVIL_SCHEDULER_POLL_INTERVAL`과
   `ANVIL_SCHEDULER_RECONCILE_INTERVAL`은 tick 주기만 조정한다.
3. control loop status endpoint와 placements extension은 additive API다.
4. runbook과 release checklist는 scheduler 운영 검증에 `/control-loop/status` 확인을
   추가한다.

## 설계 결정 요약

- 접근안은 store-backed control loop를 선택한다.
- host source는 config bootstrap + `/hosts` API를 사용한다.
- 같은 host name 충돌 시 정적 값은 config가 이긴다.
- 첫 poll 실패부터 `degraded`, threshold 이상 실패 시 `unhealthy`로 전이한다.
- `degraded/unhealthy` host는 신규 placement에서 제외한다.
- 기존 VM placement는 장애 중 삭제하지 않고 `suspect`로 표시한다.
- `GET /placements`와 `GET /control-loop/status`를 모두 제공한다.
- startup config/state load 실패는 fatal, runtime poll/reconcile/store save 실패는
  service를 죽이지 않는다.
