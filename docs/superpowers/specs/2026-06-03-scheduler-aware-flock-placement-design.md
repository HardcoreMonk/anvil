# Scheduler-aware Flock Placement 작은 시작 설계

작성일: 2026-06-03

## 1. 목적

anvil v0.3.6 기반 라인은 이미 scheduler, placement store, runtime router, cross-host
snapshot replication을 갖고 있다. 하지만 Goosetown flock 생성은 여전히 단일 daemon
`POST /flocks` 호출에 위임된다. 이 때문에 `anvil_spawn_flock`으로 만든 multi-agent
flock은 scheduler host capacity, tenant quota, egress policy, placement state를
활용하지 못한다.

이번 작업은 "작게 시작"한다. daemon `POST /flocks` 의미를 바꾸지 않고, MCP
`anvil_spawn_flock`이 scheduler router config가 있을 때만 scheduler-aware 경로를
사용하게 한다. 목표는 cross-host flock placement의 운영 가치를 검증하는 첫 단계이며,
v0.5.0 Web UI/API 변경은 범위에서 제외한다.

## 2. 범위

포함 범위:

- `main`의 v0.3.6 기반 anvil 라인에서 작업한다.
- MCP `anvil_spawn_flock` 호출만 scheduler-aware로 확장한다.
- scheduler router config가 없으면 기존처럼 base daemon `POST /flocks`를 그대로
  호출한다.
- scheduler router config가 있으면 `RuntimeRouter`가 role별 VM을 host별로 schedule하고,
  성공한 VM placement를 `PlacementStore.VMPlacements`에 기록한다.
- flock metadata와 Town Wall은 한 host의 daemon이 계속 소유한다.
- 첫 버전은 coordinator host를 하나 선택하고, 모든 flock member VM도 그 host에서
  생성한다. 즉 "scheduler-aware single-host flock placement"가 작은 시작이다.
- scheduler decision은 tenant quota, host health, available VM capacity, egress policy를
  기존 `Scheduler.Schedule` 규칙으로 판단한다.
- scheduler 또는 daemon create 실패 시 placement를 기록하지 않는다. daemon create 성공
  후 placement save가 실패하면 flock은 삭제하지 않고 sanitized error로 보고한다.
- MCP output, audit, metrics에는 `agent_token`, authorization header, daemon raw body를
  노출하지 않는다.
- unit test와 MCP smoke-compatible test를 추가한다.
- README, `docs/architecture/mcp-architecture.md`, `docs/architecture/multi-tenant-roadmap.md`,
  `RELEASE_NOTES.md`, 운영 handoff 문서를 갱신한다.

제외 범위:

- daemon direct `POST /flocks`의 wire contract 변경
- daemon 내부 flock spawn 구현 변경
- flock member를 여러 host에 분산 배치하는 true cross-host flock
- cross-host Town Wall forwarding, SSE fan-out, in-VM `gtcall` cross-host routing
- role별 preferred/excluded host 입력
- partial flock 허용
- long-running placement job store
- v0.5.0 Web UI 반영
- KVM/Firecracker full e2e 필수 실행

## 3. 선택한 접근

선택안은 MCP/router 계층의 scheduler-aware single-host flock placement다.

`cmd/anvil-mcp`는 scheduler config가 있으면 이미 `RuntimeRouter`를 만들지만, 현재
`NewReplicatingDaemon(base, router)` 래퍼는 snapshot replication만 router에 위임하고
나머지 daemon method는 base daemon에 위임한다. 따라서 `anvil_spawn_flock`은 scheduler
config가 있어도 base daemon에 직접 붙는다.

이번 작업은 다음 두 경계를 추가한다.

1. `RuntimeRouter.CreateFlock(ctx, req)`:
   - `roles` 수만큼 `TenantUsage{ActiveVMs: len(roles)}`를 요청량으로 계산한다.
   - 기존 `ScheduleRequest`에 `tenant_id`, `egress_policy`를 채워 scheduler decision을
     얻는다.
   - 선택된 host daemon의 `CreateFlock`을 호출한다.
   - 응답의 agent VM ID를 모두 선택된 host에 placement로 기록한다.
   - placement save 실패는 flock 생성 성공 후의 운영 상태 기록 실패로 취급하되,
     error message에 raw daemon body나 token을 넣지 않는다.

2. router-aware wrapper:
   - `NewReplicatingDaemon` 또는 새 wrapper가 `CreateFlock`만 router로 위임한다.
   - 기존 snapshot replication 위임은 유지한다.
   - router가 없는 config에서는 기존 base daemon 동작이 유지된다.

이 방식은 daemon API와 guest runtime을 건드리지 않는다. flock/Town Wall owner가 단일
daemon으로 유지되므로 v0.3.6의 in-VM `gtwall`, `gtcall`, watchdog, metadata persistence,
per-agent restart 동작과 충돌하지 않는다.

## 4. Data flow

### Router config 없음

1. IronClaw가 MCP `anvil_spawn_flock`을 호출한다.
2. `Tools.SpawnFlock`가 입력을 검증한다.
3. `DaemonClient.CreateFlock`이 base daemon `POST /flocks`를 호출한다.
4. 기존 응답 shape를 그대로 반환한다.

### Router config 있음

1. `cmd/anvil-mcp`가 scheduler state/hosts/quota store를 읽는다.
2. host별 `DaemonClient`와 `RuntimeRouter`를 만든다.
3. wrapper daemon이 `CreateFlock`을 `RuntimeRouter.CreateFlock`에 위임한다.
4. `RuntimeRouter.CreateFlock`이 roles 수만큼 active VM 요청량을 계산한다.
5. scheduler가 healthy host 중 capacity, egress, quota를 만족하는 host를 고른다.
6. 선택된 host daemon의 `POST /flocks`를 한 번 호출한다.
7. daemon이 기존 방식으로 single-host flock을 만든다.
8. router가 응답 agents의 `vm_id`를 `PlacementStore.VMPlacements`에 기록하고 save한다.
9. MCP output은 기존 `SpawnFlockOutput`을 반환한다. host endpoint와 token은 반환하지
   않는다.

## 5. Error handling

- scheduler가 quota 초과, host 없음, unhealthy host만 존재 등의 이유로 거부하면 daemon
  호출 전에 실패한다.
- 선택된 host daemon `POST /flocks`가 실패하면 placement를 기록하지 않는다.
- `POST /flocks`가 성공했지만 placement 기록 중 일부가 실패하면:
  - first implementation은 flock을 삭제하지 않는다. VM은 이미 정상 생성됐고 flock owner
    daemon이 cleanup 책임을 갖기 때문이다.
  - MCP error는 "flock created but placement save failed" 형태로 반환할 수 있다.
  - error에는 `flock_id`와 sanitized context만 넣고 agent token, authorization header,
    daemon raw body는 넣지 않는다.
- placement 기록 후 MCP audit 성공 기록이 실패하면 기존 MCP audit 실패 처리 방식을
  따른다.
- retry/failover는 첫 버전에서 사용하지 않는다. flock create는 여러 VM을 한 번에 만들기
  때문에 host A 실패 후 host B 재시도 시 partial cleanup 의미가 커진다. 재시도 정책은
  후속 cross-host flock 설계에서 다룬다.

## 6. Security and invariants

- `POST /vms` 응답 외에는 `agent_token`을 노출하지 않는다.
- `anvil_spawn_flock` 응답에는 `agent_token` 또는 `agent_tokens`를 넣지 않는다.
- runtime audit에는 tool name, tenant, sanitized daemon operation만 남긴다.
- scheduler state에는 `vm_id -> host_name` placement만 저장한다.
- host endpoint, bearer token, daemon raw response body, snapshot metadata body는 MCP
  output과 scheduler metrics에 넣지 않는다.
- daemon `POST /flocks`가 기존처럼 VM lifecycle과 cleanup 의미를 소유한다.

## 7. Test plan

Focused unit tests:

- `RuntimeRouter.CreateFlock` schedules using `len(roles)` as active VM request.
- unhealthy host는 flock create 후보에서 제외된다.
- quota 초과 시 daemon `CreateFlock`을 호출하지 않는다.
- successful flock create records every returned agent VM placement.
- placement save failure is reported without token/raw body leakage.
- wrapper daemon delegates `CreateFlock` to router when router config exists.
- no-router path still calls base daemon `CreateFlock`.
- `Tools.SpawnFlock` output still omits `agent_token`.

Command verification:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-mcp -count=1
go test ./... -count=1
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
go build ./cmd/goose-daemon
bash -n scripts/anvil-mcp-e2e.sh
git diff --check
```

Manual/KVM verification is optional for this first step because daemon runtime behavior is not
changed. Before release, an operator can still run `scripts/anvil-mcp-e2e.sh flock` against a
single configured scheduler host to prove MCP compatibility.

## 8. Documentation updates

- `README.md`: `anvil_spawn_flock` scheduler-aware single-host placement behavior.
- `docs/architecture/mcp-architecture.md`: previous "no scheduler-aware placement" note를
  "MCP-only single-host scheduler placement available; true cross-host flock deferred"로
  갱신.
- `docs/architecture/multi-tenant-roadmap.md`: scheduler-aware flock placement v1 완료
  범위와 남은 true cross-host 범위 정리.
- `RELEASE_NOTES.md`: Unreleased 항목에 scheduler-aware flock placement 추가.
- `docs/operations/YYYY-MM-DD-scheduler-aware-flock-placement-handoff.md`: 검증과 잔여
  위험 기록.

## 9. Follow-up

이번 작은 시작 이후의 후속 후보:

- role별 preferred/excluded host 입력
- flock member를 여러 host에 분산 배치하는 true cross-host flock
- cross-host Town Wall forwarding과 SSE fan-out
- cross-host `gtcall` routing
- partial flock 허용 또는 member별 rollback 정책
- scheduler flock placement metrics
