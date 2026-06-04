# Scheduler Flock Placement Metrics 설계

작성일: 2026-06-04

## 1. 목적

`anvil-v0.3.2`는 MCP `anvil_spawn_flock`을 scheduler-aware single-host placement로
확장했다. 현재 scheduler `/metrics`는 control loop, host status, suspect placement
상태만 보여 준다. 운영자는 flock placement가 scheduler에서 거부됐는지, daemon flock
create에서 실패했는지, placement save에서 실패했는지, 그리고 각 단계가 얼마나 걸리는지
볼 수 없다.

이번 작업은 scheduler flock placement metrics를 추가한다. 범위는 counters와 latency
histograms까지 포함한다. true cross-host flock placement는 별도 설계로 남긴다.

## 2. 범위

포함 범위:

- `RuntimeRouter.CreateFlock`의 scheduler-aware path만 계측한다.
- scheduler `/metrics`에 `anvil_scheduler_flock_placement_*` metrics를 추가한다.
- 결과 counter는 `outcome`, `reason` bounded labels로 집계한다.
- latency histogram은 `phase` bounded label로 집계한다.
- 마지막 성공/실패 timestamp gauge를 노출한다.
- metrics aggregate state는 `PlacementStoreState`에 저장한다.
- metrics 기록 실패는 observability 실패로만 취급하고, flock 생성의 user-facing 결과를
  바꾸지 않는다.
- unit test와 focused package test를 추가한다.
- release note, observability 문서, 운영 handoff를 갱신한다.

제외 범위:

- host 이름, host endpoint, `tenant_id`, `flock_id`, `vm_id`를 metrics label로 사용하지
  않는다.
- raw daemon error body, authorization header, `agent_token`, snapshot metadata를 metrics
  state나 exposition에 저장하지 않는다.
- scheduler service에 새 event ingestion API를 추가하지 않는다.
- daemon direct `POST /flocks`를 계측하지 않는다.
- Prometheus client dependency를 추가하지 않는다.
- cross-host member placement, role별 placement, partial flock rollback은 다루지 않는다.

## 3. 선택한 접근

선택한 접근은 `PlacementStoreState` 기반 aggregate metrics다.

현재 MCP router와 scheduler service는 같은 scheduler state file을 기준으로 동작한다.
MCP router는 `RuntimeRouter.CreateFlock`에서 placement를 기록하고, scheduler service는
`GET /metrics`에서 `PlacementStoreState`를 읽어 text exposition을 렌더링한다. 따라서
flock placement metrics도 같은 state에 aggregate로 저장하는 방식이 가장 작은 변경이다.

대안과 판단:

- router process memory metrics: 구현은 가장 쉽지만 scheduler `/metrics`에서 볼 수 없고
  process 재시작 시 사라진다.
- scheduler service event API: 구조는 명확하지만 새 HTTP API, auth, retry, persistence
  계약이 생긴다.
- `PlacementStoreState` aggregate: 기존 persistence와 `/metrics` 경계를 재사용하고,
  bounded aggregate만 저장해 정보 노출과 cardinality를 제어할 수 있다.

## 4. Metrics 계약

추가 metric namespace는 `anvil_scheduler_flock_placement_*`다.

### Result counter

```text
anvil_scheduler_flock_placement_attempts_total{outcome="success",reason="scheduled"} 3
anvil_scheduler_flock_placement_attempts_total{outcome="denied",reason="quota_exceeded"} 1
```

허용 outcome:

- `success`
- `denied`
- `scheduler_error`
- `daemon_error`
- `daemon_nil_response`
- `placement_save_error`

허용 reason:

- `scheduled`
- `quota_exceeded`
- `no_eligible_host`
- `invalid_request`
- `daemon_create_failed`
- `daemon_nil_response`
- `placement_save_failed`
- `unknown`

Scheduler denial reason이 이미 bounded value면 그대로 사용한다. 그 외 error text는 raw
문자열을 쓰지 않고 위 reason 중 하나로 매핑한다.

### Latency histogram

```text
anvil_scheduler_flock_placement_latency_seconds_bucket{phase="schedule",le="0.005"} 1
anvil_scheduler_flock_placement_latency_seconds_bucket{phase="schedule",le="+Inf"} 3
anvil_scheduler_flock_placement_latency_seconds_sum{phase="schedule"} 0.042
anvil_scheduler_flock_placement_latency_seconds_count{phase="schedule"} 3
```

허용 phase:

- `schedule`: scheduler decision을 얻는 시간
- `daemon_create`: 선택된 daemon의 `CreateFlock` 호출 시간
- `placement_save`: flock member VM placement state 저장 시간
- `total`: `RuntimeRouter.CreateFlock` 전체 시간

bucket upper bounds는 초 단위로 고정한다.

```text
0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, +Inf
```

### Timestamp gauges

```text
anvil_scheduler_flock_placement_last_success_timestamp_seconds 1780550000
anvil_scheduler_flock_placement_last_failure_timestamp_seconds 1780550100
```

성공은 scheduler decision, daemon create, placement save가 모두 완료된 경우다. 실패는
denial과 error outcome을 모두 포함한다. zero state에서는 두 gauge 모두 `0`이다.

## 5. State model

`PlacementStoreState`에 aggregate field를 추가한다.

```go
type FlockPlacementMetricsState struct {
    AttemptsByOutcomeReason map[string]int64 `json:"attempts_by_outcome_reason,omitempty"`
    LatencyByPhase          map[string]LatencyHistogramState `json:"latency_by_phase,omitempty"`
    LastSuccessAt           time.Time `json:"last_success_at,omitempty"`
    LastFailureAt           time.Time `json:"last_failure_at,omitempty"`
}

type LatencyHistogramState struct {
    Buckets map[string]int64 `json:"buckets,omitempty"`
    SumSeconds float64 `json:"sum_seconds,omitempty"`
    Count int64 `json:"count,omitempty"`
}
```

구현 시 key format은 internal helper가 소유한다. 외부 JSON 소비자가 key format에 의존하지
않도록 문서화하지 않는다. metrics exposition에서는 key를 다시 `outcome`, `reason`,
`phase` labels로 렌더링한다.

State normalization은 기존 `normalizePlacementStoreState`와
`clonePlacementStoreState`에 포함한다. 누락된 field는 empty aggregate로 취급한다.

## 6. Data flow

1. `RuntimeRouter.CreateFlock`가 시작 시간을 기록한다.
2. `scheduleDaemon` 호출 시간을 `schedule` phase로 측정한다.
3. scheduler가 deny/error를 반환하면 bounded outcome/reason과 `total` latency를
   best-effort로 기록하고 기존 error를 반환한다.
4. daemon `CreateFlock` 호출 시간을 `daemon_create` phase로 측정한다.
5. daemon error 또는 nil response는 bounded outcome/reason과 latency를 best-effort로
   기록하고 기존 error를 반환한다.
6. placement 저장 시간을 `placement_save` phase로 측정한다.
7. placement save가 성공하면 `success/scheduled` counter, phase histograms, last success
   timestamp를 best-effort로 기록한다.
8. placement save가 실패하면 `placement_save_error/placement_save_failed` counter,
   phase histograms, last failure timestamp를 best-effort로 기록한 뒤 기존 sanitized error
   흐름을 유지한다.

Metrics 기록은 `PlacementStore` method로 캡슐화한다. metrics 저장 실패는 원래
`CreateFlock` 결과를 덮어쓰지 않는다.

## 7. Error handling

- `NormalizeTenantID`, `NormalizeEgressPolicy`, requested VM validation error는
  `scheduler_error/invalid_request`로 집계한다.
- `ScheduleDeniedError`는 `denied/<decision reason>`으로 집계한다.
- daemon call error는 `daemon_error/daemon_create_failed`로 집계한다.
- nil daemon response는 `daemon_nil_response/daemon_nil_response`로 집계한다.
- placement save error는 `placement_save_error/placement_save_failed`로 집계한다.
- metrics save error는 user-facing error에 포함하지 않는다.
- raw error string은 metrics label이나 state key로 쓰지 않는다.

## 8. Security and cardinality

- Metrics labels는 bounded enum만 사용한다.
- `tenant_id`, `flock_id`, `vm_id`, role name, host name, host endpoint는 label로 쓰지
  않는다.
- Metrics state와 output에 token-like strings가 들어가지 않도록 renderer test를 둔다.
- `reason`은 scheduler decision reason 중 허용 목록만 통과시키고, unknown value는
  `unknown`으로 접는다.
- Histogram phase도 허용 목록만 렌더링한다.

## 9. Test plan

Focused unit tests:

- `RenderSchedulerMetrics`가 flock placement counter, histogram, timestamp gauges를
  렌더링한다.
- zero state는 counter 없이 timestamp `0`, histogram count `0`을 안정적으로 렌더링한다.
- renderer output에 host endpoint, `tenant_id`, `flock_id`, `vm_id`, `agent_token`이
  포함되지 않는다.
- `PlacementStore` metrics record method가 outcome/reason counter와 latency buckets를
  누적한다.
- `RuntimeRouter.CreateFlock` success path가 schedule, daemon_create, placement_save,
  total latency와 `success/scheduled`를 기록한다.
- scheduler denial path가 daemon을 호출하지 않고 `denied/<reason>`을 기록한다.
- daemon error path가 placement를 기록하지 않고 `daemon_error/daemon_create_failed`를
  기록한다.
- nil response path가 `daemon_nil_response/daemon_nil_response`를 기록한다.

Command verification:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-scheduler -count=1
go test ./cmd/anvil-mcp -count=1
go test ./... -count=1
go build ./cmd/anvil-scheduler
go build ./cmd/anvil-mcp
go build ./cmd/goose-daemon
git diff --check
```

Optional smoke:

```bash
scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010
ANVIL_MCP_TENANT_ID=tenant-1 \
ANVIL_MCP_SCHEDULER_STATE=/tmp/anvil-mcp-router-state.json \
ANVIL_MCP_SCHEDULER_HOSTS_FILE=/tmp/anvil-mcp-router-hosts.json \
ANVIL_DAEMON_URL=http://127.0.0.1:3000 \
scripts/anvil-mcp-e2e.sh flock
```

Optional smoke는 `/dev/kvm`, daemon, scheduler가 준비된 host에서만 실행한다.

## 10. Documentation updates

- `RELEASE_NOTES.md`: 새 Unreleased 항목에 scheduler flock placement metrics 추가.
- `docs/operations/observability.md`: 새 metrics 이름, labels, 보안 경계 설명.
- `docs/operations/2026-06-04-flock-placement-metrics-handoff.md`: 검증 결과와 잔여 위험
  기록.
- 필요하면 `docs/architecture/multi-tenant-roadmap.md`에서 scheduler flock placement
  metrics 후속 항목을 완료로 이동한다.

## 11. Residual risk

- `PlacementStoreState`는 여러 process가 같은 JSON file을 갱신하는 기존 모델을 유지한다.
  이번 작업은 그 모델을 개선하지 않는다.
- Metrics는 aggregate counter이므로 개별 flock 실패 원인을 추적하는 audit log를
  대체하지 않는다.
- Histogram은 process-local live scrape가 아니라 persisted aggregate다. State file을
  삭제하면 counter와 histogram은 초기화된다.
- Host별 편향은 이번 metrics에서 직접 볼 수 없다. host label은 cardinality와 정보 노출
  관리를 별도로 설계한 뒤 추가한다.
