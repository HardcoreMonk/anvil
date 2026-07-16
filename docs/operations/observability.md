# anvil 관측성 운영 메모

현재 anvil/ephemera 운영 관측성은 daemon structured log, top-level daemon
`/health`, Prometheus text 형식의 `/metrics`, legacy `/metrics/vms`, per-VM
`/vms/{vm_id}/stats`, `GET /vms?stats=true`, VM/guest health endpoint, API 상태
응답, runtime audit API, snapshot GC audit 파일을 중심으로 한다.

## 현재 log

`goose-daemon`은 `log/slog` 기반 structured log를 stdout/stderr에 출력한다.
service manager를 사용한다면 해당 manager의 log 수집 설정으로 stdout/stderr를
보관한다.

운영 log 설정:

- `EPHEMERA_LOG_FORMAT=text`: 기본값. `key=value` 형태의 text log를 출력한다.
- `EPHEMERA_LOG_FORMAT=json`: log aggregation pipeline용 JSON log를 출력한다.
- `EPHEMERA_LOG_LEVEL=debug|info|warn|error`: 기본값은 `warn`.

시작 시 확인할 log:

- control plane listen address와 auth mode
- 등록된 endpoint 목록
- `EPHEMERA_PUBLIC_URL`이 설정된 경우 agent URL base
- bootstrap, Firecracker, network, storage warning

runtime 중 확인할 log:

- VM 생성, restore, delete 실패
- guest `/health` readiness timeout
- TAP/IP allocation 또는 cleanup warning
- dm-snapshot, loop device, bind mount, COW cleanup warning
- snapshot GC apply error

## Health endpoint

daemon 자체 상태는 top-level `/health` endpoint로 확인한다. 응답에는 `status`,
실행 중 VM 수, snapshot 수, control-plane auth 활성 여부가 들어가며 token 값은
포함하지 않는다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/health
```

VM health는 daemon proxy를 통해 확인한다. 공개 배포에서는 TLS reverse proxy의 외부
URL을 사용하고, 내부 host 점검에서는 localhost daemon URL을 사용할 수 있다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/vms/$VM_ID/health
```

guest 내부 endpoint는 `goose-agent`의 `/health`다. 운영 client는 VM private IP에
직접 접근하지 말고 daemon의 `/vms/{id}/health` proxy를 우선 사용한다.

runtime scheduler service를 별도 process로 운영하면 scheduler 자체 health와
placement state를 함께 본다.

```bash
curl http://127.0.0.1:3010/health
curl http://127.0.0.1:3010/placements
```

## Scheduler metrics endpoint

runtime scheduler service는 scheduler control-plane 상태를 Prometheus text 형식으로
노출한다.

```bash
curl http://127.0.0.1:3010/metrics
```

현재 scheduler metric family:

- `anvil_scheduler_control_loop_running`
- `anvil_scheduler_persistence_degraded`
- `anvil_scheduler_host_status_count{status="healthy|degraded|unhealthy|unknown"}`
- `anvil_scheduler_suspect_vm_placements`
- `anvil_scheduler_last_poll_completed_timestamp_seconds`
- `anvil_scheduler_last_reconcile_completed_timestamp_seconds`
- `anvil_scheduler_poll_interval_seconds`
- `anvil_scheduler_reconcile_interval_seconds`
- `anvil_scheduler_flock_placement_attempts_total{outcome,reason}`
- `anvil_scheduler_flock_placement_latency_seconds{phase}`
- `anvil_scheduler_flock_placement_last_success_timestamp_seconds`
- `anvil_scheduler_flock_placement_last_failure_timestamp_seconds`

metric label에는 host name, endpoint, raw daemon response, authorization header,
`agent_token`을 넣지 않는다. scheduler service에는 자체 인증 계층이 없으므로
loopback/private network 또는 reverse proxy policy 뒤에서만 scrape한다.

### Scheduler flock placement metrics

MCP router의 scheduler-aware `anvil_spawn_flock` path는 flock placement 결과와 단계별
latency를 scheduler state에 aggregate로 기록한다. scheduler service는 같은 state를
읽어 `/metrics`에 노출한다.

- `anvil_scheduler_flock_placement_attempts_total{outcome,reason}`은 bounded outcome과
  reason별 placement 시도 수를 센다.
- `anvil_scheduler_flock_placement_latency_seconds{phase}`는 `schedule`,
  `daemon_create`, `placement_save`, `total` phase latency를 persisted histogram
  aggregate로 기록한다.
- `anvil_scheduler_flock_placement_last_success_timestamp_seconds`는 scheduler decision,
  daemon create, placement save가 모두 완료된 마지막 성공 시각이다.
- `anvil_scheduler_flock_placement_last_failure_timestamp_seconds`는 scheduler denial
  또는 error outcome이 발생한 마지막 시각이다.

Members-only routed flock create는 같은 metric family에 cross-host 전용 bounded enum을
추가로 기록한다. 이 path는 `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`가 켜진
MCP router에서 `anvil_create_routed_flock_members`를 호출할 때만 사용된다. invalid
request처럼 scheduler 단계에서 공통 처리되는 오류는 기존 `scheduler_error` outcome을
그대로 사용할 수 있다.

- outcome enum: `cross_host_success`, `cross_host_denied`,
  `cross_host_spawn_error`, `cross_host_rollback_error`,
  `cross_host_registry_error`
- phase enum: `plan`, `agent_spawn`, `registry_save`, `rollback`, `total`

flock placement metrics label은 의도적으로 bounded enum만 사용한다. `tenant_id`,
`flock_id`, `vm_id`, host name, host endpoint, daemon raw error text, authorization
header, `agent_token` 같은 값은 label이나 state에 넣지 않는다.

## Goosetown 상태 확인

Goosetown flock은 live registry와 Town Wall log를 함께 본다.

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/flocks
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:3000/flocks/$FLOCK_ID
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/flocks/$FLOCK_ID/wall/history
```

`GET /flocks/{flock_id}`의 agent map에서 `agent_id`, `role`, `vm_id`, `agent_url`,
`status`를 확인하고, 각 member VM은 `/vms/{vm_id}/health` proxy로 추가 점검한다.
Town Wall SSE stream은 실시간 관찰에 사용할 수 있지만 MCP smoke에서는 history
endpoint를 사용한다.

현재 runtime baseline은 upstream ephemera `v0.3.6`이며, `v0.3.2` 이후 spawn-path
VM은 `vms/<vm_id>/state.json`을 기반으로 daemon restart 뒤 cold-restart된다. 이때
VM ID, IP, TAP, MAC, agent token, agent URL은 유지되지만 memory state와 진행 중인
task는 보존되지 않는다.

watchdog은 flock member health 실패를 `status=dead`와 Town Wall notice로 드러내며,
`v0.3.3` 이후 dead status는 `flocks/<flock_id>/metadata.json`에 persist된다.
`EPHEMERA_WATCHDOG_AUTO_HEAL=true`가 아닌 기본 설정에서는 once-dead agent를 자동으로
`ready`로 되돌리지 않는다.

COW-mode VM과 snapshot-restored VM은 daemon restart 뒤 자동 복구 범위가 아니다.
이 경우에는 snapshot에서 다시 restore하거나 해당 workload를 재생성한다.

## Snapshot GC audit

`POST /snapshots/gc`를 `apply:true`로 호출하면 daemon은
`snapshots/gc-audit.jsonl`에 JSONL record를 append한다. dry-run은 audit record를 쓰지
않는다.

record는 count-only 성격이다.

- `timestamp`
- `applied`
- `policy.older_than_seconds`
- `policy.keep_last_per_vm`
- `policy.max_total_bytes`
- `candidates_count`
- `deleted_count`
- `errors_count`

audit 파일에는 snapshot metadata 전체, path 세부 정보, `agent_token`이 들어가지
않는다. 파일 권한은 append 시 `0600`으로 조정된다.

최근 audit 확인:

```bash
tail -n 20 snapshots/gc-audit.jsonl
```

## Runtime audit API

runtime audit JSONL은 운영 API로 조회/보관 정리할 수 있다. 응답은 record 배열만
반환하며 daemon raw body, snapshot metadata, `agent_token`을 포함하지 않는다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:3000/audit/runtime?tenant_id=tenant.alpha&limit=50"

curl -X POST http://127.0.0.1:3000/audit/runtime/prune \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"keep_last":1000,"max_age_seconds":2592000}'
```

## Metrics endpoint

`GET /metrics`는 Prometheus text 형식의 host-local metric을 반환한다. upstream
runtime baseline의 canonical metric namespace는 `ephemera_*`다. anvil 기존 scraper
호환을 위해 legacy `anvil_*` lifecycle line도 같은 response 끝에 append된다.

현재 제공하는 주요 metric family:

- `ephemera_vm_spawn_total{outcome="ok|fail"}`
- `ephemera_vm_destroy_total{outcome="ok|fail"}`
- `ephemera_snapshot_create_total{type="full|diff"}`
- `ephemera_snapshot_restore_total{outcome="ok|fail"}`
- `ephemera_snapshot_gc_total`
- `ephemera_flock_spawn_total`
- `ephemera_flock_destroy_total`
- `ephemera_watchdog_dead_total`
- `ephemera_watchdog_heal_total`
- `ephemera_sighup_reload_total`
- `ephemera_cp_token_propagated_total{outcome="ok|fail"}`
- `ephemera_egress_sni_verdict_total{proto="tcp|udp|unknown",outcome="allowed|denied|dropped"}` —
  egress SNI 필터의 :443 판정. `proto`로 TCP:443과 QUIC/UDP:443을 구분한다(`unknown`은
  proto 분기 이전 no-payload drop 전용). additive label이라 `sum without(proto)(...)`가
  기존 total과 같다 — 단일 outcome series를 그리던 패널은 이제 proto별로 분리된다.
- `ephemera_cleanup_failure_total`
- `ephemera_auth_failure_total`
- `ephemera_auth_total{outcome="ok|denied|expired|relay|call"}` — `relay`는
  per-flock `relay_token`(guest 능력 토큰)이 wall/call 진입으로 admit된 경우,
  `call`은 cross-host gtcall daemon-to-daemon hop이 per-flock `call_token`으로
  admit된 경우
- `ephemera_lifecycle_queue_depth`
- `ephemera_vm_count`
- `ephemera_flock_count`
- `ephemera_snapshot_count`
- `ephemera_api_clients_count`
- `ephemera_vm_spawn_duration_seconds`
- `ephemera_snapshot_restore_duration_seconds`
- `ephemera_watchdog_probe_duration_seconds`

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/metrics
```

`/metrics`는 Prometheus scrape 관례에 맞춰 기본적으로 unauthenticated다.
`EPHEMERA_METRICS_REQUIRE_AUTH=true`를 설정하면 다른 control-plane endpoint와 같은
Bearer token 인증 뒤에 놓인다. localhost 밖으로 노출하는 배포에서는 이 값을 켜거나
network-level isolation을 둔다.

구조화된 legacy per-VM metadata는 `/metrics/vms`에서 JSON으로 확인한다. 응답에는
VM ID, guest IP, profile, tenant ID, egress policy, host-local start time만 포함하며
`agent_token`은 포함하지 않는다.

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/metrics/vms
```

## Per-VM stats endpoint

`GET /vms/{vm_id}/stats`는 한 VM의 point-in-time resource snapshot을 JSON으로
반환한다. `GET /vms?stats=true`는 VM 목록 response에 같은 `stats` block을 inline으로
붙인다.

주요 field:

- `vm_id`
- `cpu_percent`
- `mem_used_mib`
- `mem_total_mib`
- `uptime_seconds`
- `network_rx_bytes`
- `network_tx_bytes`
- `agent_busy`

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:3000/vms/$VM_ID/stats

curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:3000/vms?stats=true"
```

stats response는 host `/proc`, TAP statistics, guest `/health` probe를 조합한다. PID
resolution race나 agent busy probe 실패는 log warning으로 남기고 가능한 field만
반환한다. token, daemon raw body, snapshot metadata는 포함하지 않는다.

## Trace export

`ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT` 또는 `OTEL_EXPORTER_OTLP_ENDPOINT`를 설정하면
daemon lifecycle span을 `{endpoint}/v1/traces`로 전송한다. 현재 exporter는 host-local
lifecycle event를 JSON payload로 보내는 optional 운영 hook이며, token/secret 계열
attribute는 전송 전에 제거한다.

```bash
ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 ./anvil-daemon
```

## 현재 없는 것

다음 기능은 아직 구현되어 있지 않다.

- OpenTelemetry SDK/protobuf 기반 exporter
- label cardinality를 제어한 상세 cleanup failure breakdown
- snapshot storage quota dashboard

현재 운영 판단은 daemon structured log, `/health`, `/metrics`, `GET /vms`,
`GET /vms?stats=true`, `GET /vms/{vm_id}/stats`, `GET /snapshots`, `GET /flocks`,
Town Wall history, `/metrics/vms`, VM health endpoint, `snapshots/gc-audit.jsonl`,
runtime audit API, optional trace export를 조합해서 수행한다.

## 향후 metrics 후보

구현 후보 metrics:

- snapshot total bytes와 GC candidate/deleted count
- TAP/IP allocation failure count
- dm-snapshot, loop device, bind mount cleanup failure 상세 label
- proxy task request count, latency, error count

이 항목들은 현재 counter보다 더 세밀한 운영 권장 지표다.
