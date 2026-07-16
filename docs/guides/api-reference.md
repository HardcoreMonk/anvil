# anvil control plane API 참조

> ephemera control plane REST API 전체 참조입니다. 프로젝트 개요는 [README](../../README.md),
> IronClaw용 `anvil_*` MCP 어댑터는 [mcp-adapter.md](mcp-adapter.md), 인증·보안 정책은
> [security-and-resilience.md](security-and-resilience.md)를 참고하세요.

## API 참조

token이 설정되어 있으면 모든 control-plane endpoint는
`Authorization: Bearer <token>`을 요구한다.

### VM 생성

```text
POST /vms
Content-Type: application/json

{ "profile": "anthropic", "tenant_id": "tenant.alpha", "egress_policy": "profile" }
```

`profile`을 생략하면 기본 `configs/goose.yaml`과
`configs/goose-secrets.yaml`을 사용한다. `tenant_id`와 `egress_policy`는 optional
계약이다. `tenant_id`는 ASCII letter/digit으로 시작해야 하며 letter, digit, `.`,
`_`, `-`만 허용한다. `egress_policy`는 `deny_all`, `profile`, `allow_all` 중 하나다.

```bash
curl -X POST http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"profile": "anthropic", "tenant_id": "tenant.alpha", "egress_policy": "profile"}'
```

```json
{
  "vm_id": "vm-1778227813435",
  "guest_ip": "10.0.1.10",
  "agent_url": "http://10.0.1.10:8080",
  "profile": "anthropic",
  "tenant_id": "tenant.alpha",
  "egress_policy": "profile",
  "agent_token": "3f9a2c..."
}
```

보안 불변 조건은 `POST /vms` 외 응답에서 `agent_token`을 노출하지 않는 것이다.
daemon의 `POST /snapshots/{id}/restore`, MCP output, audit record는
`agent_token`을 노출하지 않는다.

### VM 목록

```bash
curl http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN"
```

### VM 삭제

```bash
curl -X DELETE http://localhost:3000/vms/vm-1778227813435 \
  -H "Authorization: Bearer $TOKEN"
```

### Snapshot 생성

```text
POST /vms/{vm_id}/snapshot
Content-Type: application/json

{
  "stop_after": false,
  "type": ""
}
```

`type`이 비어 있으면 자동 선택한다.

| 조건 | 결과 |
|---|---|
| 해당 VM의 기존 Full snapshot 없음 | `full` |
| 해당 VM의 기존 Full snapshot 있음 | `diff` |

```bash
curl -X POST http://localhost:3000/vms/vm-1778227813435/snapshot \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json"
```

### Snapshot 목록

```bash
curl http://localhost:3000/snapshots \
  -H "Authorization: Bearer $TOKEN"
```

### Snapshot bundle export/import

```text
POST /snapshots/{id}/export       -> snapshot bundle export
POST /snapshots/import            -> snapshot bundle import
```

`POST /snapshots/{id}/export`는 `application/vnd.anvil.snapshot-bundle` content type의
streamable bundle을 반환한다. `POST /snapshots/import`는 target host에서 bundle을
staging, validation, atomic publish 순서로 반입한다. cross-host 복제는 MCP
`anvil_replicate_snapshot`을 통해 source export stream을 target import로 전달한다.
diff snapshot 복제 시 `include_dependencies=true`를 사용하면 base full snapshot을
먼저 복제하고 diff를 복제한다.

### Snapshot 복원

```bash
curl -X POST http://localhost:3000/snapshots/snap-1778229000000/restore \
curl -X DELETE http://localhost:3000/snapshots/snap-1778227847573 \
  -H "Authorization: Bearer $TOKEN"
```

> **Dependency rule**: A Full snapshot that is the base for one or more Diff snapshots cannot be deleted (returns `409 Conflict`). Delete all referencing Diff snapshots first.

---

### Per-VM Stats (v0.3.5)

```
GET /vms/{vm_id}/stats
```

Returns a point-in-time snapshot of host-observable VM stats. Authentication uses the **control plane Bearer token**.

```json
{
  "vm_id": "vm-1778227000000",
  "cpu_percent": 12.4,
  "mem_used_mib": 187,
  "mem_total_mib": 2048,
  "uptime_seconds": 312,
  "network_rx_bytes": 12849,
  "network_tx_bytes": 5328,
  "agent_busy": false
}
```

| Field | Source |
|-------|--------|
| `cpu_percent` | 100 ms sample of `/proc/<firecracker_pid>/stat` (utime+stime). 100 = one full host core. |
| `mem_used_mib` | `VmRSS:` line of `/proc/<firecracker_pid>/status`. |
| `mem_total_mib` | VM spawn sizing (mirrors `VMState.MemSizeMib`). |
| `uptime_seconds` | `time.Since(spawned_at)` — `VMState.CreatedAt` for recovered VMs. |
| `network_rx_bytes`, `network_tx_bytes` | `/sys/class/net/<tap>/statistics/{tx,rx}_bytes`, swapped to VM perspective. |
| `agent_busy` | 1 s `GET /health` against the in-VM agent; `true` when `status == "busy"`. |

Per-VM stats failures (firecracker PID not resolvable, `/proc` race, agent unreachable) degrade fields to zero and emit a slog `Warn`; the endpoint still returns 200 so dashboards see partial data instead of intermittent errors.

For bulk dashboards, `GET /vms?stats=true` returns the standard `[]VMInfo` list with an embedded `stats` field on each element.

> The endpoint emits a snapshot. Streaming (`text/event-stream`) is on the v0.4.3 roadmap.

---

### Metrics (v0.3.5)

```
GET /metrics
```

Returns the control plane's Prometheus exposition payload (text format version 0.0.4). Unauthenticated by default; set `EPHEMERA_METRICS_REQUIRE_AUTH=true` to require a Bearer token like the other endpoints.

Exposed series (additive — never breaks the wire format on minor bumps):

| Family | Type | Labels | Notes |
|--------|------|--------|-------|
| `ephemera_vm_spawn_total` | counter | `outcome=ok\|fail` | every `spawnVMInternal` exit |
| `ephemera_vm_destroy_total` | counter | `outcome=ok` | `destroyVM` after teardown |
| `ephemera_snapshot_create_total` | counter | `type=full\|diff` | success path of `createSnapshot` |
| `ephemera_snapshot_restore_total` | counter | `outcome` | dm-snapshot and bind-mount fallback both contribute |
| `ephemera_snapshot_gc_total` | counter | — | successful `POST /snapshots/gc` applications |
| `ephemera_auto_snapshot_total` | counter | `outcome=ok\|fail` | graceful-shutdown memory auto-snapshot (`EPHEMERA_AUTOSNAPSHOT`, v0.4.0) |
| `ephemera_auto_restore_total` | counter | `outcome=ok\|fail` | recovery warm-restore attempt (v0.4.0) |
| `ephemera_auth_total` | counter | `outcome=ok\|denied\|expired` | per-request API auth decision (v0.4.1) |
| `ephemera_auth_failure_total` | counter | — | failed API Bearer-token authentication attempts |
| `ephemera_cleanup_failure_total` | counter | — | cleanup failures while releasing VM resources |
| `ephemera_flock_spawn_total` / `_destroy_total` | counter | — | success path of `createFlock` / `deleteFlock` |
| `ephemera_watchdog_dead_total` / `_heal_total` | counter | — | dyingThreshold and autoHeal transitions |
| `ephemera_sighup_reload_total` | counter | — | after `ReloadClients` completes |
| `ephemera_cp_token_propagated_total` | counter | `outcome` | per-VM vsock fan-out result |
| `ephemera_mcp_tool_calls_total` | counter | `server`, `outcome=ok\|fail\|forbidden\|rate_limited` | MCP gateway tool calls by backend server (v0.6.0) |
| `ephemera_egress_sni_verdict_total` | counter | `proto=tcp\|udp\|unknown`, `outcome=allowed\|denied\|dropped` | :443 egress SNI filter verdicts; `unknown` is the pre-classify no-payload drop (ADR-0002) |
| `ephemera_vm_count` / `_flock_count` / `_snapshot_count` / `_api_clients_count` | gauge | — | re-read on each scrape (GaugeFunc) |
| `ephemera_lifecycle_queue_depth` | gauge | — | current in-flight lifecycle operations |
| `ephemera_vm_spawn_duration_seconds` | histogram | — | wall-clock spawn time |
| `ephemera_snapshot_restore_duration_seconds` | histogram | — | wall-clock restore time |
| `ephemera_watchdog_probe_duration_seconds` | histogram | — | per-probe `/health` duration |

---

### Access Audit Log (v0.4.1)

```
GET /audit?limit=100&client=alice&status=200&method=GET
Authorization: Bearer <token>
```

Returns the most recent access-log records (newest first) as a JSON array. All
query params are optional: `limit` (default 100, max 1000), `client`, `status`,
`method`.

```json
[
  { "ts": "2026-05-27T06:11:05Z", "client": "alice", "method": "GET",
    "path": "/vms", "status": 200, "duration_ms": 3, "remote_addr": "127.0.0.1:54xxx", "bytes": 412 }
]
```

Records are appended to `{workDir}/audit/access.jsonl` (size-rotated; see the
`EPHEMERA_AUDIT_*` env vars) and **never contain tokens, the `Authorization`
header, request/response bodies, or the query string**. Unauthenticated requests
are logged with `client` = `"-"`; `/metrics` is excluded. This endpoint is
authenticated (and is itself audited). See also [Access audit log](security-and-resilience.md#access-audit-log-v041) under Security.

---

### Agent Proxy (via Control Plane)

The control plane proxies the three agent endpoints, making them accessible to external clients without direct access to the private VM subnet. Authentication uses the **control plane Bearer token** — the agent token is injected internally.

```
POST /vms/{vm_id}/tasks    → proxied to goose-agent /tasks
GET  /vms/{vm_id}/health   → proxied to goose-agent /health  (no auth required)
POST /vms/{vm_id}/stop     → proxied to goose-agent /stop
```

`POST /vms/{vm_id}/tasks`의 response shape는 `{ "output", "error" }`를 유지한다.
`v0.3.6` baseline부터 `output`은 Goose `--output-format json` envelope에서 추출한
assistant text다. agent는 Goose startup banner 앞부분을 건너뛰고 assistant text
block을 이어 붙이며, envelope parsing에 실패하면 raw stdout으로 fallback한다.

> The agent's `/townwall/post` is **not** proxied. It is an in-VM convenience used by the bundled `gtwall` CLI, which already has the flock context. External callers should `POST /flocks/{id}/post` directly — they already know the flock ID and can pick the `agent_id` themselves.

When `EPHEMERA_PUBLIC_URL` is configured, `agent_url` in VM responses points directly to the proxy base (`{EPHEMERA_PUBLIC_URL}/vms/{vm_id}`), so clients can use it as-is:

```bash
export EPHEMERA_PUBLIC_URL=https://api.example.com
# agent_url in POST /vms response will be: https://api.example.com/vms/vm-...

curl -X POST "$AGENT_URL/tasks" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tenant_id": "tenant.alpha", "egress_policy": "profile"}'
```

source VM이 아직 실행 중이면 restore는 거부된다. restore된 VM은 새 VM ID와
새 IP를 받지만 snapshot의 agent token은 daemon 내부 proxy용으로만 유지한다.
restore request의 `tenant_id` 또는 `egress_policy`가 snapshot metadata와 충돌하면
daemon은 restore를 거부한다. restore success response에는 `agent_token`이 없다.

snapshot `metadata.json`은 restore 인증 계약을 보존하기 위해 `agent_token`을
담고 있다. metadata를 반출하거나 백업 산출물이 신뢰된 host 경계 밖으로 나가기
전에는 scrubber로 token을 제거한다.
단, `POST /snapshots/{id}/export` replication bundle 안의 `metadata.json`은 raw local
metadata file이 아니라 `agent_token`을 제거한 portable copy다. Firecracker restore
state가 요구하는 `disk_path`, `vsock_path`는 safe workspace/tmp path인지 import 때
검증한 뒤 보존하고, target daemon은 `mem_file_path`, `state_file_path`,
`disk_copy_path`를 target snapshot directory로 다시 기록한다. 복제된 snapshot을
restore할 때는 target daemon이 새 `agent_token`을 생성해 vsock control channel로
guest agent에 주입하므로 source host의 token을 bundle에 싣지 않는다.

```bash
go run ./scripts/snapshot-metadata-scrub.go -input snapshots/snap-1778229000000/metadata.json > metadata.scrubbed.json
```

restore 실패는 JSON error body를 반환한다.

```json
{
  "error": "snapshot not found",
  "code": "snapshot_not_found",
  "source_snapshot_id": "snap-1778229000000"
}
```

`code`는 안정적인 machine-readable 값이다.

| code | 의미 |
|---|---|
| `snapshot_not_found` | 요청한 snapshot metadata가 없다 |
| `source_vm_running` | source VM이 아직 실행 중이라 restore할 수 없다 |
| `network_unavailable` | restore용 TAP/IP allocation에 실패했다 |
| `diff_base_missing` | diff snapshot의 base full snapshot이 없다 |
| `memory_merge_failed` | diff memory merge에 실패했다 |
| `firecracker_restore_failed` | disk setup 또는 Firecracker restore에 실패했다 |
| `guest_reconfigure_failed` | restore 후 guest IP 재설정에 실패했다 |
| `agent_not_ready` | restore된 VM의 `goose-agent` health 대기가 실패했다 |

현재 snapshot lifecycle은 보수적으로 직렬화되어 하나의 create/restore/delete/GC
lifecycle operation만 동시에 실행된다.

### Snapshot 삭제

```bash
curl -X DELETE http://localhost:3000/snapshots/snap-1778229000000 \
  -H "Authorization: Bearer $TOKEN"
```

diff snapshot이 참조 중인 full snapshot은 삭제할 수 없다.

### Snapshot GC dry-run/apply

`POST /snapshots/gc`는 snapshot retention plan을 계산한다. 기본값은 dry-run이며
파일을 삭제하지 않는다. `older_than_seconds`와 `keep_last_per_vm`에 더해
`max_total_bytes`를 지정할 수 있다. `max_total_bytes` 기본값 `0`은 비활성화이며,
양수이면 모든 snapshot directory의 apparent file size를 합산한 뒤 projected
remaining total이 한도 이하가 될 때까지 보호되지 않은 snapshot을 오래된 순서로
추가 후보에 넣는다.

```bash
curl -X POST http://localhost:3000/snapshots/gc \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $EPHEMERA_API_TOKEN" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"max_total_bytes":10737418240}'
```

실제 삭제는 `apply: true`를 명시해야 수행된다.

```bash
curl -X POST http://localhost:3000/snapshots/gc \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $EPHEMERA_API_TOKEN" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"max_total_bytes":10737418240,"apply":true}'
```

diff snapshot이 참조 중인 full snapshot은 항상 보호된다. full과 diff가 모두 오래된
경우 첫 GC apply에서는 diff만 삭제되고, 다음 GC 호출에서 full이 삭제 후보가 된다.
`candidates`, `protected`, `deleted` entry는 계산 가능한 경우 `size_bytes`를 포함한다.
`max_total_bytes` 때문에 추가된 후보의 `reason`은 `max_total_bytes`다. `apply: true`
호출은 삭제 시도 후 `snapshots/gc-audit.jsonl`에 JSONL audit record를 1줄 append한다.
audit record에는 timestamp, applied, policy, candidates/deleted/errors count만 들어가며
snapshot metadata나 `agent_token`은 기록하지 않는다. dry-run은 audit record를 쓰지
않는다.

### Agent proxy 사용

```bash
curl http://localhost:3000/vms/$VM_ID/health \
  -H "Authorization: Bearer $TOKEN"

curl -X POST http://localhost:3000/vms/$VM_ID/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"hello from inside the VM"}'

curl -X POST http://localhost:3000/vms/$VM_ID/stop \
  -H "Authorization: Bearer $TOKEN"
# Filters (v0.4.3, combinable): ?agent_id=worker-1 · ?since= / ?until= (RFC3339) · ?contains=text
curl "http://localhost:3000/flocks/$FLOCK_ID/wall/history?agent_id=worker-1&contains=build" \
  -H "Authorization: Bearer $TOKEN"
```

외부 client는 control-plane token만 사용한다. daemon이 guest agent token을
내부적으로 주입한다.

```json
[
  { "timestamp":"...","agent_id":"orchestrator","body":"Flock spawned with 5 agents: [...]" },
  { "timestamp":"...","agent_id":"researcher-1","body":"Found existing dark mode CSS variables" }
]
```

#### List flocks

```bash
curl http://localhost:3000/flocks -H "Authorization: Bearer $TOKEN"
```

#### Describe a flock

```bash
curl http://localhost:3000/flocks/$FLOCK_ID -H "Authorization: Bearer $TOKEN"
```

#### Add, remove, or change an agent (v0.4.3)

```bash
# Add an agent — role-N id auto-assigned; anvil omits guest token fields from the response
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/agents \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"role":"worker"}'

# Change an agent's role — VM recreated under the new role (agent_id + token kept)
curl -X PATCH http://localhost:3000/flocks/$FLOCK_ID/agents/worker-2 \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"role":"reviewer"}'

# Remove an agent — VM torn down; removing the last agent leaves an empty flock
curl -X DELETE http://localhost:3000/flocks/$FLOCK_ID/agents/worker-2 \
  -H "Authorization: Bearer $TOKEN"
```

#### Pause or resume a flock (v0.4.3)

```bash
# Pause all members (runtime-only — a daemon restart brings them back running)
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/pause -H "Authorization: Bearer $TOKEN"
# Resume all members
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/resume -H "Authorization: Bearer $TOKEN"
```

> A per-flock agent cap is set at creation: `POST /flocks {"…", "max_agents": N}` (default 20), enforced on create and on add.

#### Tear down a flock

```bash
curl -X DELETE http://localhost:3000/flocks/$FLOCK_ID \
  -H "Authorization: Bearer $TOKEN"
# {"status":"deleted","flock_id":"flock-..."}
```

Destroys every member VM in parallel and removes the flock from the registry. The Town Wall log on disk (`flocks/<id>/TOWN_WALL.log`) is left in place as an audit artifact.

#### Restart a single agent (v0.3.3)

```bash
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/agents/reviewer-1/restart \
  -H "Authorization: Bearer $TOKEN"
# {"vm_id":"vm-...","guest_ip":"10.0.1.7","agent_url":"http://10.0.1.7:8080","profile":"reviewer"}
```

Tears down the named agent's VM and respawns it with the same `agent_id`, role, and `agent_token` (callers that cached the token keep working). The new VM has a fresh `vm_id` / `guest_ip` / `agent_url`; the agent status resets to `ready`. On spawn failure the agent is left `Status=dead` and persisted, so callers see the truth without needing to poll.
