# Anvil Scheduler Operations Hardening Design

## 목표

`anvil-v0.3.1` 이후 scheduler control loop를 운영 품질로 굳힌다. 이번 범위는
후속 개발 목록의 1-3번이다.

1. `cmd/anvil-scheduler` full-process integration test
2. 실제 systemd host scheduler 운영 배포 검증
3. scheduler observability metrics/alerts surface

이 작업은 scheduler process와 운영 검증 경로를 강화하는 additive change다. VM
lifecycle, Firecracker/KVM runtime, snapshot/restore storage semantics, flock runtime
wire contract는 바꾸지 않는다.

## 비목표

- upstream ephemera `v0.4.0`-`v0.5.0` sync 또는 adoption review
- cross-host snapshot replication
- scheduler-aware cross-host flock placement
- daemon self-registration, host별 scheduler token, scheduler auth layer
- multi-node HA, leader election, external database-backed state store
- scheduler algorithm, quota model, placement decision priority 변경
- UI dashboard
- OpenClaw compatibility layer

## 현재 기반

현재 `main`에는 scheduler 운영 기반이 이미 있다.

- `cmd/anvil-scheduler`는 `ANVIL_SCHEDULER_*` 환경 변수, hosts file, persistent
  `PlacementStore`, `QuotaStore`를 읽어 HTTP service와 control loop를 실행한다.
- `internal/anvilmcp.SchedulerControlLoop`는 host `/health` polling과 `GET /vms`
  reconciliation을 수행한다.
- `GET /control-loop/status`는 사람이 확인할 JSON 상태를 반환한다.
- `scripts/anvil-scheduler-smoke.sh`는 실행 중인 scheduler의 `/health`,
  `/control-loop/status`, `/hosts`, `/schedule/spawn`, `/placements`를 확인한다.
- `scripts/install-anvil-scheduler-systemd.sh --verify`는 smoke harness를 연결한다.

빠진 부분은 세 가지다.

- 실제 process startup에서 env, hosts file, stale state, fake daemon, HTTP server,
  scheduling이 함께 맞물리는 regression test가 없다.
- scheduler 자체 metrics endpoint가 없어 degraded/unhealthy host, persistence
  degraded, suspect placement 같은 상태를 alert로 걸기 어렵다.
- 실제 systemd 배포 검증이 release/operate handoff에 명시적으로 닫혀 있지 않다.

## Domain Architecture

이번 설계의 domain boundary는 다음과 같다.

| 용어 | 의미 | 구현 경계 |
|---|---|---|
| Scheduler process | `cmd/anvil-scheduler` binary와 HTTP server/control loop startup | `cmd/anvil-scheduler` |
| Scheduler service | operator HTTP API: `/health`, `/hosts`, `/placements`, `/control-loop/status`, `/schedule/*`, 신규 `/metrics` | `internal/anvilmcp.SchedulerService` |
| Control-loop state | poll/reconcile 실행 상태, host observation, suspect placement, persistence degraded flag | `internal/anvilmcp.PlacementStoreState` |
| Scheduler metrics | control-loop state를 Prometheus text format으로 바꾼 read-only operator surface | 신규 `internal/anvilmcp` helper |
| Full-process integration test | scheduler binary/process를 실제로 띄우고 fake daemon과 HTTP API를 검증하는 test | `cmd/anvil-scheduler` test |
| Systemd verification | installer와 smoke harness가 실제 service install/start/verify 경로를 닫는 운영 검증 | `scripts/*`, `deploy/systemd/*`, operations docs |

Scheduler metrics는 daemon metrics와 다른 surface다. daemon `/metrics`는 ephemera
runtime lifecycle metric을 제공하고, scheduler `/metrics`는 scheduler control plane
상태만 제공한다. metric namespace는 `anvil_scheduler_*`를 사용한다.

## Architecture

### 1. Full-process integration test

`cmd/anvil-scheduler`에 process-level integration test를 추가한다. test는 scheduler
binary 또는 test subprocess를 실제 process처럼 시작하고 다음 조건을 검증한다.

- `ANVIL_SCHEDULER_ADDR`는 test 전용 loopback port를 사용한다.
- `ANVIL_SCHEDULER_HOSTS_FILE`은 fake daemon endpoint와 configured capacity를 담는다.
- `ANVIL_SCHEDULER_STATE`는 stale zero capacity state를 포함할 수 있다.
- fake daemon `/health`는 current daemon shape인 `{"status":"ok","vm_count":...}`만
  반환해도 scheduler가 hosts file capacity를 보존한다.
- `/control-loop/status`는 `running: true`를 반환한다.
- `/schedule/spawn`은 configured host를 선택한다.
- `/metrics`는 scheduler state metric을 반환한다.

이 test는 KVM, Firecracker, root 권한, LLM secret을 요구하지 않는다. scheduler와 fake
daemon 사이의 HTTP/API 계약만 검증한다.

### 2. Scheduler metrics endpoint

`GET /metrics`를 `SchedulerService`에 추가한다. 응답은 Prometheus text format이다.
읽기 전용이며 placement store를 저장하지 않는다.

초기 metric family:

- `anvil_scheduler_control_loop_running`
- `anvil_scheduler_persistence_degraded`
- `anvil_scheduler_host_status_count{status="healthy|degraded|unhealthy|unknown"}`
- `anvil_scheduler_suspect_vm_placements`
- `anvil_scheduler_last_poll_completed_timestamp_seconds`
- `anvil_scheduler_last_reconcile_completed_timestamp_seconds`
- `anvil_scheduler_poll_interval_seconds`
- `anvil_scheduler_reconcile_interval_seconds`

`last_*_timestamp_seconds`는 값이 없으면 `0`을 반환한다. host name, endpoint, raw error,
authorization header, daemon raw response, `agent_token`은 metric label/value에 넣지
않는다.

후속 후보 metric은 별도 release에서 다룬다.

- poll/reconcile latency histogram
- poll/reconcile failure counter
- host별 allowlisted cardinality labels
- scheduler host registration audit metric

### 3. Systemd 운영 배포 검증

운영 검증은 두 단계다.

1. repository 검증: installer dry-run, smoke harness unit test, shell syntax check,
   scheduler process integration test
2. 실제 host 검증: `sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify`

실제 host 검증은 `/etc/anvil`, `/var/lib/anvil`, systemd unit state를 바꿀 수 있으므로
실행 직전에 명시적 escalation 승인을 요구한다. 승인되면 `--start --verify`로 service를
기동하고 smoke harness가 `/metrics`까지 확인한다.

## Endpoint Contract

### `GET /metrics`

성공:

```text
# HELP anvil_scheduler_control_loop_running Scheduler control loop running flag.
# TYPE anvil_scheduler_control_loop_running gauge
anvil_scheduler_control_loop_running 1
# HELP anvil_scheduler_persistence_degraded Scheduler persistence degraded flag.
# TYPE anvil_scheduler_persistence_degraded gauge
anvil_scheduler_persistence_degraded 0
# HELP anvil_scheduler_host_status_count Scheduler host count by status.
# TYPE anvil_scheduler_host_status_count gauge
anvil_scheduler_host_status_count{status="healthy"} 1
anvil_scheduler_host_status_count{status="degraded"} 0
anvil_scheduler_host_status_count{status="unhealthy"} 0
anvil_scheduler_host_status_count{status="unknown"} 0
```

Method가 `GET`이 아니면 `405`를 반환한다.

Scheduler service에는 현재 인증 계층이 없다. 따라서 `/metrics`도 scheduler의 기존
운영 경계와 동일하게 loopback/private network 또는 reverse proxy policy 뒤에서만
노출한다.

## Data Flow

### Startup integration path

1. test가 fake daemon을 loopback에서 띄운다.
2. test가 hosts file과 stale state file을 temp directory에 쓴다.
3. test가 scheduler process를 test loopback address로 시작한다.
4. scheduler가 hosts file을 읽고 configured host를 store에 적용한다.
5. control loop poll이 fake daemon `/health`를 읽는다.
6. test가 scheduler `/control-loop/status`, `/schedule/spawn`, `/metrics`를 poll한다.
7. test가 scheduler process를 종료하고 temp artifact를 정리한다.

### Metrics path

1. client가 `GET /metrics`를 호출한다.
2. service가 `PlacementStore.State()` snapshot을 읽는다.
3. helper가 `ControlLoopStatus`, `HostObservations`, `Hosts`,
   `SuspectVMPlacements`를 metric line으로 변환한다.
4. response는 `text/plain; version=0.0.4` compatible content type을 사용한다.

## Error Handling

- Full-process integration test는 scheduler startup timeout을 명확한 test failure로
  처리한다.
- Port collision은 test 전용 loopback port 할당으로 피한다.
- Scheduler process 종료 실패는 test cleanup failure로 기록한다.
- `/metrics`는 malformed state를 만들지 않는다. nil map과 zero timestamp는 정상
  response로 렌더링한다.
- `scripts/anvil-scheduler-smoke.sh`는 `/metrics` 실패를 `metrics_failed`로 보고한다.
- systemd `--verify` 실패 시 runbook은 `health_failed`, `control_loop_status_failed`,
  `metrics_failed`, `host_put_failed`, `schedule_spawn_failed` 순서로 triage한다.

## Security

- `agent_token`은 scheduler surface에 노출하지 않는다.
- `/metrics`에는 host endpoint, authorization header, daemon raw body, raw error text,
  snapshot metadata를 넣지 않는다.
- Scheduler는 기본적으로 `127.0.0.1:3010`에 bind한다.
- 실제 배포 검증은 root/systemd state를 바꾸므로 escalation 승인을 받아 실행한다.
- local secret file과 runtime artifact는 커밋하지 않는다.

## Testing Strategy

TDD 순서:

1. `internal/anvilmcp`에 `/metrics` route와 renderer failing test를 추가한다.
2. test가 `/metrics` 미구현으로 실패하는 것을 확인한다.
3. 최소 renderer와 route를 구현한다.
4. `scripts/anvil-scheduler-smoke.sh` fake scheduler test에 metrics check를 추가한다.
5. smoke script가 metrics를 확인하도록 구현한다.
6. `cmd/anvil-scheduler` full-process integration failing test를 추가한다.
7. scheduler startup helper/test harness를 구현한다.
8. docs/runbook/observability/release checklist/follow-up 문서를 갱신한다.
9. 실제 systemd host 검증을 승인받아 실행한다.

필수 로컬 검증:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerService.*Metrics|TestRenderSchedulerMetrics' -count=1
go test ./scripts -run 'TestAnvilSchedulerSmoke' -count=1
go test ./cmd/anvil-scheduler -run 'TestSchedulerProcess' -count=1
go test ./... -count=1
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
bash -n scripts/anvil-scheduler-smoke.sh
bash -n scripts/install-anvil-scheduler-systemd.sh
```

실제 운영 host 검증:

```bash
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

이 명령은 실행 전 escalation 승인을 요구한다.

## Plan Grilling

- 이 작업이 scheduler algorithm을 바꾸는가? 아니다. read-only metrics와 process test,
  smoke check만 추가한다.
- `/metrics`에 인증을 붙일 것인가? 이번 범위에서는 아니다. scheduler 자체가 현재 인증
  없는 loopback/private service이므로 같은 경계를 유지한다.
- daemon `/metrics`와 충돌하는가? 충돌하지 않는다. 별도 process와
  `anvil_scheduler_*` namespace를 사용한다.
- full-process test가 flaky해질 위험은? startup readiness poll, temp loopback port,
  bounded timeout으로 줄인다.
- 실제 systemd 검증은 자동으로 실행할 것인가? 아니다. root/systemd state를 바꾸므로
  마지막에 별도 승인으로 실행한다.

## Plan Design Review

UI가 없는 운영 surface이므로 정보 구조와 gate clarity를 기준으로 검토한다.

- 사람이 보는 상태는 `/control-loop/status`, alert가 보는 상태는 `/metrics`로 분리한다.
- smoke harness는 단계별 실패 이름을 유지하고 `metrics_failed`만 추가한다.
- runbook은 dry-run, local smoke, actual systemd verify를 분리해서 운영자가 어느 단계가
  host state를 바꾸는지 알 수 있게 한다.
- metric label은 cardinality와 secret exposure를 피하기 위해 status-only로 시작한다.

Design-blocking issue는 없다.

## 완료 기준

- scheduler full-process integration test가 KVM/root 없이 통과한다.
- scheduler `/metrics`가 Prometheus text format으로 control-loop 상태를 반환한다.
- smoke harness와 installer verify가 `/metrics`를 확인한다.
- 실제 systemd host에서 승인 후 `--start --verify`가 통과하거나, 실패 시 단계명과
  residual risk가 handoff에 기록된다.
- README, runbook, observability, release checklist, follow-up 문서가 현재 구현과
  일치한다.
- `agent_token` 또는 local secret이 새 output, metrics, docs, artifacts에 노출되지
  않는다.

## Spec Freeze Snapshot

- Topic: `scheduler-operations-hardening`
- Date: 2026-06-02
- Scope: 후속 개발 1-3번. Scheduler full-process integration test, 실제 systemd
  deployment verification, scheduler metrics/alerts surface.
- Accepted architecture: Scheduler `/metrics`는 daemon metrics와 분리된
  `anvil_scheduler_*` Prometheus text endpoint다. `GET /control-loop/status`는 사람이
  보는 JSON status로 유지한다. Process integration test는 fake daemon과 temp state를
  사용하고 KVM/root를 요구하지 않는다.
- Non-goals: upstream sync, snapshot replication, cross-host flock placement, scheduler
  auth layer, algorithm/quota decision 변경, UI.
- Security boundary: scheduler metrics에는 token, endpoint, raw error, raw daemon
  response, snapshot metadata, `agent_token`을 넣지 않는다.
- Environment assumptions: local Go 1.25 toolchain, loopback HTTP test 가능, actual
  systemd verification에는 root 권한과 systemd host가 필요하다.
- Required verification: focused Go tests, `go test ./... -count=1`, three binary builds,
  shell syntax checks, approval-gated `sudo bash scripts/install-anvil-scheduler-systemd.sh
  --start --verify`.
- Open risk: actual systemd verification may fail due to host-specific permissions,
  existing service state, or `/var/lib/anvil` ownership. Failure is not hidden; it must be
  recorded in operation handoff with next action.
