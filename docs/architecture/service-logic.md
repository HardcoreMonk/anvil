# ephemera 서비스 로직

## 상태

- 기준 버전: upstream ephemera `v0.5.5` + anvil runtime control-plane updates
- 범위: ephemera daemon HTTP 동작, VM lifecycle, agent proxy, snapshot lifecycle,
  Goosetown flock/Town Wall, tenant/egress/audit/observability endpoint, guest agent 동작
- 제외 범위: IronClaw MCP client 동작. 해당 내용은
  [mcp-architecture.md](mcp-architecture.md)를 참조한다.

이 문서는 각 service operation이 수행하는 일과 반드시 지켜야 할 invariant를
설명한다. 파일 수준 런타임 구조는
[runtime-architecture.md](runtime-architecture.md)에 정리한다.

## 서비스 경계

control plane daemon은 하나의 HTTP service를 노출한다.

| API group | 소유자 | 목적 |
|---|---|---|
| `/health` | `cmd/goose-daemon/api.go` | daemon 상태, VM 수, snapshot 수, auth 활성 여부 |
| `/metrics` | `cmd/goose-daemon/metrics_handler.go` | Prometheus text 형식 daemon metrics. 기본 unauth, `EPHEMERA_METRICS_REQUIRE_AUTH=true`일 때 Bearer token 필요 |
| `/metrics/vms` | `cmd/goose-daemon/api.go` | 실행 중인 VM별 legacy JSON metadata metrics |
| `/watchdog/status` | `cmd/goose-daemon/api.go` | health watchdog tunable과 per-VM fail/dead 상태 read-only 조회 (count/ID/config만) |
| `/tenants` | `cmd/goose-daemon/api.go` | tenant quota/usage 목록 |
| `/tenants/{tenant_id}` | `cmd/goose-daemon/api.go` | tenant quota/usage 조회와 quota 설정 |
| `/audit/runtime` | `cmd/goose-daemon/api.go` | runtime audit 조회 |
| `/audit/runtime/prune` | `cmd/goose-daemon/api.go` | runtime audit 보관 정책 적용 |
| `/vms` | `cmd/goose-daemon/api.go` | VM 생성, 목록, 삭제. `?stats=true`면 per-VM stats inline |
| `/vms/{vm_id}/stats` | `cmd/goose-daemon/stats_handler.go` | VM별 cpu/mem/net/uptime/agent_busy point-in-time stats |
| `/vms/{vm_id}/tasks` | `cmd/goose-daemon/api.go` | guest agent로 task 실행 proxy. `?stream=1`이면 NDJSON progress+result streaming, 기본은 buffered `{"output","error"}`. `EPHEMERA_MAX_TASK_DEPTH` depth guard(`508`) 적용 |
| `/vms/{vm_id}/workloads/run` | `cmd/goose-daemon/api.go` | guest agent로 script-only workload 실행 proxy |
| `/vms/{vm_id}/workspace` | `cmd/goose-daemon/api.go` | guest `/workspace` 단일 파일 read/write proxy |
| `/vms/{vm_id}/health` | `cmd/goose-daemon/api.go` | guest health proxy |
| `/vms/{vm_id}/stop` | `cmd/goose-daemon/api.go` | guest agent에 stop 요청 |
| `/vms/{vm_id}/snapshot` | `cmd/goose-daemon/api.go` | full 또는 diff VM snapshot 생성 |
| `/snapshots` | `cmd/goose-daemon/api.go` | 저장된 snapshot 목록 |
| `/snapshots/gc` | `cmd/goose-daemon/api.go` | snapshot retention GC dry-run/apply |
| `/snapshots/{id}/export` | `cmd/goose-daemon/api.go` | streamable snapshot bundle export |
| `/snapshots/import` | `cmd/goose-daemon/api.go` | snapshot bundle staging/validation/import |
| `/snapshots/{id}/restore` | `cmd/goose-daemon/api.go` | snapshot에서 VM restore |
| `/snapshots/{id}` | `cmd/goose-daemon/api.go` | snapshot 삭제 |
| `/flocks` | `cmd/goose-daemon/orchestrator_api.go` | Goosetown flock 생성과 live flock 목록 |
| `/flocks/{flock_id}` | `cmd/goose-daemon/orchestrator_api.go` | flock metadata 조회와 소속 VM teardown |
| `/flocks/{flock_id}/post` | `cmd/goose-daemon/orchestrator_api.go` | Town Wall message append |
| `/flocks/{flock_id}/wall` | `cmd/goose-daemon/orchestrator_api.go` | Town Wall SSE stream |
| `/flocks/{flock_id}/wall/history` | `cmd/goose-daemon/orchestrator_api.go` | Town Wall history 조회 |
| `/flocks/{flock_id}/broadcast` | `cmd/goose-daemon/orchestrator_api.go` | flock 전 member agent에 prompt scatter-gather. daemon-only endpoint이며 `anvil_*` MCP tool로 노출하지 않는다 |
| `/ui/` | `cmd/goose-daemon/config_api.go`, `cmd/goose-daemon/uidist/` | embedded operator Svelte SPA(정적 bundle + login page). auth chain 밖에 두는 유일한 surface. runtime/operator surface이며 IronClaw MCP surface가 아니다 |
| `/config/profiles`, `/config/profiles/{name}` | `cmd/goose-daemon/config_api.go` | profile `GOOSE_PROVIDER`/`GOOSE_MODEL`·sizing·`system.md`(`64 KiB` cap) read/write. `goose-secrets.yaml`은 read/write하지 않음(sentinel test). delete in-use → `409`, default profile 예약, traversal 거부. auth 설정 시 bearer 뒤 |
| `/config/providers` | `cmd/goose-daemon/config_api.go` | provider별 API key 존재 여부만 반환(key 값 비노출, sentinel test) |
| `/config/clients` | `cmd/goose-daemon/config_api.go` | control-plane client 이름과 만료만 반환(token 값 비노출, sentinel test) |

VM 내부의 `goose-agent`는 다음 endpoint를 제공한다.

| Endpoint | Auth | 목적 |
|---|---|---|
| `POST /tasks` | VM별 Bearer token | Goose prompt 실행 |
| `POST /workloads/run` | VM별 Bearer token | `/workspace/workloads/*.sh` script를 timeout/output limit 안에서 실행 |
| `PUT /workspace?path=...` | VM별 Bearer token | `/workspace` 아래 단일 파일 쓰기 |
| `GET /workspace?path=...` | VM별 Bearer token | `/workspace` 아래 단일 파일 읽기 |
| `GET /health` | 없음 | `idle` 또는 `busy` 반환 |
| `POST /stop` | VM별 Bearer token | agent HTTP server graceful stop |
| `POST /townwall/post` | VM별 Bearer token | flock context를 읽어 host Town Wall에 message 전달 |

외부 caller는 private guest IP에 직접 접근하기보다 control plane proxy endpoint를
사용해야 한다.

## Runtime 설정 alias

daemon은 기존 `EPHEMERA_*` 환경 변수를 canonical 계약으로 유지하면서 다음
`ANVIL_*` alias를 fallback으로 인식한다.

| Canonical | Alias |
|---|---|
| `EPHEMERA_API_ADDR` | `ANVIL_API_ADDR` |
| `EPHEMERA_API_PORT` | `ANVIL_API_PORT` |
| `EPHEMERA_API_TOKENS` | `ANVIL_API_TOKENS` |
| `EPHEMERA_API_TOKEN` | `ANVIL_API_TOKEN` |
| `EPHEMERA_AGENT_PORT` | `ANVIL_AGENT_PORT` |
| `EPHEMERA_PUBLIC_URL` | `ANVIL_PUBLIC_URL` |
| `EPHEMERA_EGRESS_PROFILE_DIR` | `ANVIL_EGRESS_PROFILE_DIR` |

Canonical-only upstream runtime 설정:

| 변수 | 의미 |
|---|---|
| `EPHEMERA_API_TOKENS_FILE` | `name:token` entry file. 파일 source는 SIGHUP 때 다시 읽히므로 hot rotation의 권장 경로 |
| `EPHEMERA_HOME` | daemon work directory. `artifacts/`, `configs/`, `snapshots/` 같은 runtime path의 기준 directory |
| `EPHEMERA_MAX_TASK_DEPTH` | nested `/tasks` dispatch depth 한계, 기본 `5`. 한계 도달 시 `508`, `X-Ephemera-Task-Depth`를 `depth+1`로 forwarding. 신설 canonical env, ANVIL alias 없음 |
| `EPHEMERA_WATCHDOG_INTERVAL_SEC` | watchdog poll cadence, 기본 `5` |
| `EPHEMERA_WATCHDOG_TIMEOUT_SEC` | watchdog per-probe HTTP timeout, 기본 `1` |
| `EPHEMERA_WATCHDOG_THRESHOLD` | dead marking 전 연속 실패 횟수, 기본 `3` |
| `EPHEMERA_WATCHDOG_AUTO_HEAL` | `true`면 dead agent가 다시 응답할 때 `ready`로 auto-heal, 기본 `false` |
| `EPHEMERA_METRICS_REQUIRE_AUTH` | `true`면 `/metrics`도 control-plane Bearer token 필요, 기본 `false` |
| `EPHEMERA_LOG_FORMAT` | `text` 또는 `json`, 기본 `text` |
| `EPHEMERA_LOG_LEVEL` | `debug`, `info`, `warn`, `error`, 기본 `warn` |

추가 optional 운영 hook:

| 변수 | 의미 |
|---|---|
| `ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT` | daemon lifecycle trace export endpoint. `OTEL_EXPORTER_OTLP_ENDPOINT`보다 우선 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OpenTelemetry 호환 endpoint fallback |

각 설정은 canonical 값이 비어 있을 때만 alias 값을 사용한다. 이 규칙은 기존
ephemera 배포의 동작을 보존하면서 anvil 운영 문서에서 `ANVIL_*` 이름을 사용할 수
있게 한다.

## 제어 평면 인증

operator Web UI(`v0.5.0`)에서 auth 밖에 두는 surface는 `/ui/`(정적 bundle + login
page)뿐이다. `/config/profiles`·`/config/providers`·`/config/clients`를 포함한 모든
data API는 auth 설정 시 bearer token 뒤에 유지한다(guard `config_api_anvil_test.go`).
`/config/*` surface는 `goose-secrets.yaml` 값(API key)이나 control-plane token 값을
응답에 담지 않는다(sentinel test).

모든 control-plane route는 `authMiddleware`로 감싼다.

```text
incoming request
  -> cp.getClients()로 현재 API client list 읽기
  -> client가 설정되어 있지 않으면 요청 허용
  -> Authorization header를 모든 등록 token과 비교
  -> 인증 실패 시 401 JSON body 반환
  -> matched client name을 log에 남기고 route handler 호출
```

token 비교는 constant-time comparison을 사용하고 첫 후보에서 멈추지 않는다.
partial token match가 timing으로 새지 않게 하기 위한 선택이다.

`SIGHUP`은 `ControlPlane.ReloadClients`를 호출한다. daemon 재시작이나 실행 중
VM 중단 없이 API client list를 다시 로드한다. `EPHEMERA_API_TOKENS_FILE`을 쓰는
경우 파일을 매번 다시 읽기 때문에 true hot rotation이 가능하다. env var source는
process exec 시점에 고정되므로 SIGHUP이 env 값 변경을 볼 수 없다.

환경 변수 precedence:

```text
EPHEMERA_API_TOKENS_FILE
  -> EPHEMERA_API_TOKENS
  -> ANVIL_API_TOKENS
  -> EPHEMERA_API_TOKEN
  -> ANVIL_API_TOKEN
  -> 인증 비활성화
```

`EPHEMERA_*`는 ephemera runtime의 canonical 설정이고 `ANVIL_*`는 anvil 운영자를
위한 alias다. canonical 값이 있으면 alias 값보다 우선한다.

reload 후 daemon은 새 `apiClients[0].Token`을 running VM의 vsock으로 fan-out해
guest 내부 `/root/.ephemera-cp-token`을 갱신한다. 이 propagation은 best-effort이며
v0.3.4+ guest agent가 있어야 성공한다. 실패한 VM은 log와
`ephemera_cp_token_propagated_total{outcome="fail"}`로 관측한다.

## Health, metrics, tenant, audit 로직

Daemon health:

```text
GET /health
  -> cp.vms count와 cp.snapshots count 조회
  -> {"status":"ok","vm_count":N,"snapshot_count":M,"auth_enabled":true|false} 반환
```

Metrics:

```text
GET /metrics
  -> Prometheus exposition format 0.0.4 반환
  -> ephemera_* counter/gauge/histogram 반환
  -> legacy anvil_* lifecycle line append
  -> 기본 unauthenticated, EPHEMERA_METRICS_REQUIRE_AUTH=true면 Bearer token 필요

GET /metrics/vms
  -> 실행 중인 VM별 vm_id, guest_ip, profile, tenant_id, egress_policy, started_at 반환

GET /vms/{vm_id}/stats
  -> host /proc, TAP statistics, guest /health probe를 조합
  -> vm_id, cpu_percent, mem_used_mib, mem_total_mib, uptime_seconds,
     network_rx_bytes, network_tx_bytes, agent_busy 반환

GET /vms?stats=true
  -> VMInfo 목록에 stats block을 inline으로 포함
```

Metrics response에는 `agent_token`, daemon raw body, snapshot metadata를 포함하지
않는다.

Tenant quota API:

```text
GET /tenants
  -> tenant quota/usage records 반환

GET /tenants/{tenant_id}
  -> tenant_id 검증 후 해당 tenant quota/usage 반환

PUT /tenants/{tenant_id}
  -> {"quota": {...}} body decode
  -> quota 값 검증
  -> workDir/tenants/tenants.json에 0600 JSON state 저장
```

Runtime audit API:

```text
GET /audit/runtime?tenant_id=...&limit=N
  -> workDir/audit/runtime-audit.jsonl read
  -> tenant filter와 limit 적용
  -> sanitized records 반환

POST /audit/runtime/prune
  -> keep_last 또는 max_age_seconds 보관 정책 적용
  -> 남은 sanitized records 반환
```

Runtime audit record는 symlink path append를 거부하고 `0600`으로 저장된다. 조회
응답은 secret, daemon raw body, snapshot metadata, `agent_token`을 포함하지 않는다.

## Egress policy와 trace export

`egress_policy`는 `deny_all`, `profile`, `allow_all`만 허용한다.

- `deny_all`: VM guest IP 기준 default reject rule을 적용한다.
- `profile`: `configs/profiles/{profile}/egress.json`,
  `EPHEMERA_EGRESS_PROFILE_DIR`, `ANVIL_EGRESS_PROFILE_DIR` 아래의 profile별
  `egress.json`을 읽고 allow CIDR, allow host string match, DNS server allowlist와
  default reject rule을 적용한다. policy 파일이 없으면 no-op이다.
- `allow_all`: 기존 NAT outbound 동작을 유지한다.

선택된 policy는 VM/snapshot/restore metadata에 보존된다. host rule cleanup은
`destroyVM` 경로에서 VM resource cleanup과 함께 수행된다.

`ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT` 또는 `OTEL_EXPORTER_OTLP_ENDPOINT`가 설정되면
daemon lifecycle span을 `{endpoint}/v1/traces`로 HTTP POST한다. exporter는 token,
secret, authorization 계열 attribute를 제거한 뒤 전송한다.

## VM 생성 로직

Route: `POST /vms`

입력:

```json
{
  "profile": "optional-profile-name",
  "tenant_id": "optional-tenant",
  "egress_policy": "profile"
}
```

흐름:

```text
spawnVM()
  -> optional JSON body decode
  -> profile name trim
  -> tenant_id와 egress_policy 검증
  -> config/secrets path 해석
       empty profile -> configs/goose.yaml + configs/goose-secrets.yaml
       named profile -> configs/profiles/<name>/{goose.yaml,goose-secrets.yaml}
       slash/backslash가 있는 profile name 거부
  -> 32-byte random agent token 생성
  -> TAP, guest IP, MAC 할당
  -> 선택된 egress policy를 host-local network rule로 적용
  -> golden image를 /tmp/goose-workspaces/<vm_id>.ext4로 clone
  -> disk를 한 번 mount해 config, secrets, token, timezone 주입
  -> Firecracker API socket과 vsock UDS path 생성
  -> vm.StartMachine()으로 Firecracker 시작
  -> cp.vms에 VM 등록
  -> 최대 60초 동안 http://<guest_ip>:8080/health poll
  -> vm_id, guest_ip, agent_url, profile, agent_token 반환
```

실패 cleanup:

| 실패 지점 | Cleanup |
|---|---|
| Network allocation 실패 | `500` 반환 |
| Egress policy 적용 실패 | TAP/IP 반환 |
| Disk clone 실패 | egress cleanup, TAP/IP 반환 |
| Disk preparation 실패 | cloned disk 삭제, egress cleanup, TAP/IP 반환 |
| Firecracker start 실패 | cloned disk 삭제, egress cleanup, TAP/IP 반환 |
| Agent readiness 실패 | `cp.destroyVM`으로 VM 제거 |

응답의 `agent_token`은 민감 정보다. control plane은 proxy 호출을 위해 token을
메모리에 보관한다.

`tenant_id`와 `egress_policy`는 public response와 snapshot metadata에 보존된다.
`agent_token`은 `POST /vms` 성공 response에서만 노출된다.

## VM 목록 로직

Route: `GET /vms`

```text
listVMs()
  -> cp.vms를 lock 아래에서 읽기
  -> []VMInfo 반환
```

목록 응답에는 `agent_token`을 포함하지 않는다.

## Goosetown flock/Town Wall 로직

Route: `POST /flocks`

입력:

```json
{
  "task": "release readiness review",
  "roles": ["orchestrator", "researcher", "worker", "reviewer"],
  "tenant_id": "optional-tenant",
  "egress_policy": "profile"
}
```

흐름:

```text
createFlock()
  -> JSON body decode
  -> task trim, blank task 거부
  -> roles 개수 1..20 확인
  -> role trim, empty role과 path separator 포함 role 거부
  -> tenant_id와 egress_policy 검증
  -> flock ID와 flocks/<flock_id>/TOWN_WALL.log 준비
  -> role별 agent_id 번호를 부여하며 spawnVMInternal() 호출
       profile별 config/secrets/system prompt/sizing 적용
       tenant_id, egress_policy, flock_id, agent_id 전달
  -> 일부 VM spawn 실패 시 이미 생성한 VM을 destroyVM으로 정리하고 flock registry 제거
  -> 초기 Town Wall message append
  -> flocks/<flock_id>/metadata.json 원자적 저장
  -> flock_id, task, tenant/egress, agents, townwall_url, post_url 반환
```

`POST /flocks`는 daemon direct API에서도 validation을 spawn 전에 수행한다. blank
`task`, empty role, `/` 또는 `\`가 포함된 role은 host resource를 만들지 않고
`400`으로 거부한다. anvil downstream에서는 `POST /flocks` 응답에
`agent_token`/`agent_tokens`를 포함하지 않는다.

Route: `GET /flocks`, `GET /flocks/{flock_id}`

```text
list/get flock
  -> host-local FlockManager registry snapshot 반환
  -> agent map에는 agent_id, role, vm_id, agent_url, status 포함
```

Route: `DELETE /flocks/{flock_id}`

```text
deleteFlock()
  -> FlockManager에서 live flock 제거
  -> flock agent VM들을 병렬 destroyVM()으로 teardown
  -> flocks/<flock_id>/metadata.json 삭제
  -> {"status":"deleted","flock_id":"..."} 반환
```

Route: `POST /flocks/{flock_id}/post`, `GET /flocks/{flock_id}/wall/history`,
`GET /flocks/{flock_id}/wall`

```text
Town Wall
  -> post는 append-only log에 timestamp, agent_id, body 기록
  -> message는 per-flock monotonic seq 포함
  -> history는 전체 log를 JSON 배열로 반환
  -> SSE stream은 기존 history를 먼저 emit하고 새 message를 subscriber로 전달
```

Town Wall body는 사용자가 제공한 message다. runtime audit record에는 body를 저장하지
않지만, Town Wall history와 `TOWN_WALL.log`에는 그대로 남으므로 secret 전달 채널로
사용하지 않는다.

daemon startup은 `flocks/*/metadata.json`을 scan해 flock registry와 Town Wall log를
복구한 뒤, `vms/<vm_id>/state.json`이 남아 있는 spawn-path VM을 cold-restart한다.
복구 성공한 flock member VM은 `cp.vms`에 다시 등록되므로 proxy endpoint와 watchdog
probe 대상이 된다. spawn-path VM은 plain·COW disk mode 모두 cold-restart되고,
snapshot-restored VM은 `v0.4.5`부터 `source_snapshot_id`를 담은 `state.json`으로
source snapshot에서 re-restore(`recoverRestoredVM`/`reRestoreMachine`)된다.
bind-mount-fallback restore와 source snapshot이 삭제된 restored VM은 복구 대상이
아니며 drop되어 surface된다.

## VM 삭제 로직

Route: `DELETE /vms/{vm_id}`

```text
stopVM()
  -> vm_id 존재 확인
  -> cp.destroyVM(vm_id)
  -> {"status":"stopped","vm_id":"..."} 반환
```

실제 teardown은 `destroyVM`이 수행한다.

```text
destroyVM()
  -> cp.vms lock 아래에서 VM 제거
  -> StopVMM()
       Firecracker가 SIGTERM 전송
       micro-init이 signal 수신
       micro-init이 goose-agent 종료 요청
       micro-init이 poweroff(2) 호출
  -> Firecracker socket/log/vsock file 삭제
  -> COW-restored VM이면 TeardownDMSnapshot()
  -> legacy bind restore이면 TeardownBindMount()
  -> 일반 VM이면 cloned ext4 disk 삭제
  -> TAP/IP를 network.Manager로 반환
```

## Agent proxy 로직

Routes:

- `POST /vms/{vm_id}/tasks`
- `POST /vms/{vm_id}/workloads/run`
- `GET /vms/{vm_id}/health`
- `POST /vms/{vm_id}/stop`

```text
proxyAgentEndpoint()
  -> vm_id로 실행 중인 VM 찾기
  -> private target URL http://<guest_ip>:8080/<agent_path> 구성
  -> incoming context와 body로 새 request 생성
  -> Content-Type 보존
  -> /health가 아니면 "Authorization: Bearer <agent_token>" 주입
  -> cp.agentHTTPClient(request마다 fresh dial, `DisableKeepAlives`)로 request 전송
  -> response header, status code, body를 caller에게 복사
```

`cp.agentHTTPClient`는 connection pooling을 끈다(`DisableKeepAlives`, `64ec57c`).
guest IP는 VM destroy/create/restore 사이에 재활용되는데, 공유 keep-alive client가
destroy된 VM으로 향하던 stale pooled connection을 재사용하면(특히 `v0.5.x`
`gracefulAgentStop`이 넓힌 window에서) restored VM의 첫 proxied `/tasks`가 hang하거나
`502`(peer RST)로 실패한다. proxied body는 rewindable하지 않아 net/http가 재시도할 수
없으므로, request마다 새로 dial해 이 재사용을 원천 차단한다. 이는 v0.2.0부터 잠재하던
upstream pooled-client 결함을 고친 것으로, upstream connection pooling과의 의도적
divergence이며 upstream 기여 후보다.

proxy는 외부 caller에게 하나의 인증 모델만 노출한다. caller는 control plane에만
인증하고, daemon이 필요한 guest agent token을 내부적으로 주입한다.

## Snapshot 유형 선택

`resolveSnapshotType(req.Type, vmID)`는 다음 규칙을 적용한다.

| 요청 type | 결과 |
|---|---|
| `"full"` | full snapshot 생성 |
| `"diff"`이고 기존 full base 있음 | latest full snapshot을 참조하는 diff snapshot 생성 |
| `"diff"`이지만 full base 없음 | error 반환 |
| 비어 있거나 unknown이고 full base 없음 | full snapshot 생성 |
| 비어 있거나 unknown이고 full base 있음 | latest full snapshot을 참조하는 diff snapshot 생성 |

latest full snapshot은 같은 `source_vm_id`를 가진 snapshot 중 `CreatedAt` 기준으로
선택한다.

## Snapshot 생성 로직

Route: `POST /vms/{vm_id}/snapshot`

입력:

```json
{
  "stop_after": false,
  "type": "full | diff | optional",
  "tenant_id": "optional-tenant"
}
```

흐름:

```text
createSnapshot()
  -> optional body parse
  -> request tenant_id가 VM tenant_id와 충돌하면 reject
  -> 실행 중인 VM 찾기
  -> full/diff type과 base snapshot ID 결정
  -> snapshots/<snapshot_id>/ 생성
  -> VM pause
  -> CreateSnapshot(memory.bin, state.bin)
       diff snapshot은 Firecracker SnapshotType="Diff" 전달
  -> pause 상태에서 /tmp/goose-workspaces/<vm_id>.ext4를 rootfs.ext4로 copy
  -> stop_after=false이면 VM resume
  -> stop_after=true이면 source VM destroy
  -> metadata.json 작성
  -> cp.snapshots에 metadata 추가
  -> public SnapshotInfo 반환
```

중요 invariant:

- disk copy는 VM이 pause된 상태에서 수행한다.
- diff snapshot도 rootfs는 full copy한다. memory만 sparse/diff다.
- `metadata.json`은 tenant ID, egress policy, original TAP name, MAC, vsock path,
  agent token, disk path, memory path, state path, base snapshot ID를 보존한다.
  `v0.5.3`부터 per-VM `VcpuCount`/`MemSizeMib`도 기록하며, legacy snapshot(0 값)은
  restore 시 historical 2 vCPU / 2048 MiB로 fallback한다.
- snapshot API response에는 `agent_token`을 노출하지 않는다.

### Snapshot token 수명 주기

snapshot metadata는 guest agent token을 저장한다. restore된 VM이 기존
`goose-agent` 인증 계약을 그대로 유지해야 하므로, snapshot에서 복원한 VM은
metadata에 있던 원래 token을 계속 사용한다.

공개 API 응답은 이 값을 노출하지 않는 것이 정책이며, 허용된 노출 지점은
`POST /vms` 응답뿐이다. `POST /snapshots/{id}/restore`, snapshot 생성, snapshot
목록, snapshot GC, MCP output, audit output은 실제 `agent_token`을 포함하지 않는다.

snapshot metadata를 반출하거나 백업 workflow에서 신뢰된 host 경계 밖으로
보낼 때는 먼저 `scripts/snapshot-metadata-scrub.go`로 `agent_token`을 제거한다.

```bash
go run ./scripts/snapshot-metadata-scrub.go -input snapshots/snap-.../metadata.json > metadata.scrubbed.json
```

token 회전은 생성 시점에만 명확하다. 새 VM은 새 guest agent token을 받지만,
기존 snapshot restore는 snapshot metadata의 원래 token을 유지한다. 이미 만들어진
snapshot의 token을 회전하려면 향후 guest-side rekey와 metadata rewrite 설계가
필요하며, 이 문서의 구현 범위에는 포함되지 않는다.

## Snapshot restore 로직

Route: `POST /snapshots/{id}/restore`

입력:

```json
{
  "tenant_id": "optional-tenant",
  "egress_policy": "profile"
}
```

```text
restoreSnapshot()
  -> cp.snapshots에서 snapshot metadata load
  -> request tenant_id/egress_policy가 metadata와 충돌하면 reject
  -> source VM이 아직 실행 중이면 reject
  -> 새 VM ID 할당
  -> stale Firecracker socket 제거
  -> snapshot metadata의 original vsock UDS path 제거
  -> AllocateForRestore(original TAP, original MAC)
       original TAP name + 사용 가능한 guest IP 반환
  -> cp.restoreMu lock
  -> SetupDMSnapshot(rootfs.ext4, <new_vm_id>.cow, original disk path) 시도
       read-only loop for base rootfs 생성
       sparse exception store 생성
       dm-snapshot device 생성
       original disk path 위로 bind mount
  -> dm-snapshot 실패 시 network release 후 restoreLegacyBindMount() fallback
  -> snapshot이 diff이면 base snapshot metadata load
  -> MergeMemoryDiff(base.memory.bin, diff.memory.bin, tmp/<new_vm_id>-merged.bin)
  -> RestoreMachine(memory file, state.bin)
  -> Firecracker가 disk path를 연 뒤 cp.restoreMu unlock
  -> 임시 merged memory file 삭제
  -> ReconfigureGuestIP(original vsock path, new IP, gateway)
  -> restored VM을 cp.vms에 등록
  -> 최대 30초 동안 guest agent health 대기
  -> source_snapshot_id를 포함한 VMRestoreResult 반환
```

restore된 VM은 guest agent 연속성을 위해 snapshot metadata의 original agent token을
내부적으로 유지한다. 외부 client는 guest agent token이 아니라 control-plane
token과 daemon proxy를 사용해야 한다. restore success response에는
`source_snapshot_id`, restored VM info, tenant ID, egress policy만 포함하고
`agent_token`/`agent_tokens`/`Authorization`/`Bearer`는 포함하지 않는다.

`v0.4.5`부터 restore는 `source_snapshot_id`, `tenant_id`, `egress_policy`
attribution을 담은 `vms/<new_vm_id>/state.json`을 persist한다. daemon restart 시
`recoverRestoredVM`이 이 state를 읽어 source snapshot에서 auto-re-restore하며,
graceful shutdown은 dm device와 transient exception store만 정리하고 `state.json`은
남긴다. restored VM은 opt-in memory auto-snapshot에서 제외된다(재기동 시 source에서
re-restore하므로 `auto/` image가 쓰이지 않는다). restore 시 복원되는 상태는
snapshot-time memory·disk이며 post-restore write는 재기동 후 보존되지 않는다.

restore 실패는 `Content-Type: application/json`인 `RestoreErrorResponse`를
반환한다.

```json
{
  "error": "snapshot not found",
  "code": "snapshot_not_found",
  "source_snapshot_id": "snap-..."
}
```

`source_snapshot_id`는 restore 요청의 snapshot ID이며, stable `code`는 다음
값 중 하나다.

| code | 상태 |
|---|---|
| `snapshot_not_found` | 요청한 snapshot metadata 없음 |
| `source_vm_running` | source VM이 아직 실행 중 |
| `network_unavailable` | restore용 network allocation 실패 |
| `diff_base_missing` | diff snapshot의 base metadata 없음 |
| `memory_merge_failed` | diff memory merge 실패 |
| `firecracker_restore_failed` | disk setup 또는 Firecracker restore 실패 |
| `guest_reconfigure_failed` | guest IP reconfiguration 실패 |
| `agent_not_ready` | restored VM agent readiness 실패 |

실패 cleanup:

| 실패 지점 | Cleanup |
|---|---|
| Network allocation 실패 | `network_unavailable`, `409` 반환 |
| dm-snapshot setup 실패 | network release 후 bind-mount fallback 시도 |
| dm-snapshot과 bind-mount fallback 모두 실패 | network release, `firecracker_restore_failed` 반환 |
| Diff base 없음 | COW teardown, network release, `diff_base_missing`, `409` 반환 |
| Diff merge 실패 | COW teardown, network release, `memory_merge_failed` 반환 |
| Firecracker restore 실패 | COW teardown, network release, `firecracker_restore_failed` 반환 |
| Guest IP reconfiguration 실패 | Stop VMM, COW teardown, network release, `guest_reconfigure_failed` 반환 |
| Agent readiness 실패 | restored VM destroy, `agent_not_ready` 반환 |

## 기존 bind-mount restore fallback

`SetupDMSnapshot`이 실패하면 daemon은 `restoreLegacyBindMount`로 복원한다.

```text
  -> snapshot rootfs.ext4를 /tmp/goose-workspaces/<new_vm_id>.ext4로 copy
  -> state.bin의 original disk path 위로 해당 file bind mount
  -> 필요 시 diff memory merge
  -> RestoreMachine()
  -> ReconfigureGuestIP()
  -> later teardown을 위해 bindMountTarget이 있는 VM 등록
```

이 fallback은 COW restore보다 느리고 disk를 더 많이 사용하지만,
dm-snapshot을 사용할 수 없는 host에서도 restore 기능을 유지한다.

## Cross-host snapshot replication

MCP `anvil_replicate_snapshot`은 `snapshot_id`, `source_host`, `target_host`,
`include_dependencies` 입력을 받아 RuntimeRouter를 통해 host 간 snapshot bundle을
복제한다. 기존 MCP production config에서 일반 VM/snapshot tool은
`ANVIL_DAEMON_URL` direct daemon 동작을 유지하고, replication만 scheduler state 또는
hosts file config가 있을 때 router를 사용한다.

흐름:

```text
anvil_replicate_snapshot
  -> source_host와 target_host client 선택
  -> snapshot metadata에서 full/diff와 base_snapshot_id 확인
  -> diff이고 include_dependencies=true이면 base full을 먼저 같은 흐름으로 복제
  -> source daemon POST /snapshots/{id}/export 호출
       Content-Type: application/vnd.anvil.snapshot-bundle
  -> export response body를 target daemon POST /snapshots/import로 stream 전달
  -> target daemon이 staging, validation, atomic publish 수행
  -> import 성공 후 scheduler PlacementStoreState.SnapshotLocations에 target_host 기록
```

artifact bundle 생성, staging directory, metadata validation, publish semantics는
daemon이 소유한다. RuntimeRouter는 bundle body를 해석하거나 raw `metadata.json`을
operator surface에 노출하지 않고 source export stream을 target import request로
전달한다. scheduler state는 성공한 target host만 기록한다. source export 또는 target
import가 실패하면 `SnapshotLocations`를 갱신하지 않는다.

diff snapshot은 target host에 base full snapshot이 있어야 restore 가능하다.
`include_dependencies=false`일 때 base full이 target에 없으면 replication 또는 이후
restore가 dependency 오류로 실패할 수 있다. `include_dependencies=true`는 base full을
먼저 복제한 뒤 diff를 복제하므로 restore scheduler가 target host를 안전하게 선택할 수
있다.

보안 불변 조건:

- operator-facing replication response와 audit record는 `agent_token`을 포함하지 않는다.
- authorization header와 `ANVIL_API_TOKEN` 값은 response/audit/log에 기록하지 않는다.
- daemon raw body와 raw `metadata.json` body를 MCP output에 포함하지 않는다.
- `POST /snapshots/{id}/export` bundle의 `metadata.json`은 raw local metadata가 아니라
  token을 제거한 portable metadata다. Firecracker restore가 요구하는 `disk_path`와
  `vsock_path`는 safe path로 검증한 뒤 보존한다.
- 복제된 snapshot restore는 source host token을 재사용하지 않고 target daemon이 새
  agent token을 vsock으로 주입한다.

## Snapshot 삭제 로직

Route: `DELETE /snapshots/{id}`

```text
deleteSnapshot()
  -> cp.snapshots에서 base_snapshot_id == requested ID인 diff 검색
  -> 있으면 409 반환
  -> live 또는 persisted restored VM state가 이 snapshot을 source로 참조하면
     409 ("referenced by restored VM ...") 반환
  -> cp.snapshots에서 snapshot metadata 제거
  -> snapshots/<id>/를 disk에서 삭제
  -> {"status":"deleted","snapshot_id":"..."} 반환
```

이 규칙은 diff snapshot이 아직 필요로 하는 full snapshot을, 그리고 live·persisted
restored VM이 재기동 recovery에 필요로 하는 source snapshot을 삭제하지 못하게
막는다. upstream e2e 46c는 live restored VM이 참조하는 snapshot의 `DELETE`를 `200`으로
허용하고 VM을 orphan으로 두지만, anvil은 이 `409` 보호를 유지하고 restored VM을
먼저 삭제하도록 요구한다(의도적 divergence, `docs/ADR_INDEX.md`·
`docs/operations/upstream-sync-policy.md`에 `adapted`로 기록).

## Snapshot GC 로직

Route: `POST /snapshots/gc`

Request 예시:

```json
{
  "older_than_seconds": 604800,
  "keep_last_per_vm": 1,
  "max_total_bytes": 10737418240,
  "apply": false
}
```

Flow:

```text
handleSnapshotGC()
  -> optional JSON body parse
  -> negative older_than_seconds / keep_last_per_vm / max_total_bytes 거부
  -> cp.snapshots metadata를 복사해 CreatedAt 기준 정렬
  -> 각 storage.SnapshotDir(workDir, snapshotID)를 walk해 file Info().Size() 합산
  -> diff snapshot의 base_snapshot_id reverse reference map 생성
  -> referenced full snapshot 보호
  -> source_vm_id별 최신 keep_last_per_vm개 보호
  -> age 조건을 통과하고 보호되지 않은 snapshot을 candidates로 분류
  -> max_total_bytes > 0이면 전체 size에서 기존 candidates size를 뺀 projected
     remaining total을 계산
  -> projected remaining total이 max_total_bytes보다 크면 보호되지 않고 아직 후보가
     아닌 snapshot을 오래된 순서로 추가 후보 처리(reason=max_total_bytes)
  -> apply=false이면 plan만 반환
  -> apply=true이면 candidates를 storage.DeleteSnapshot으로 삭제하고 cp.snapshots에서 제거
  -> apply=true이면 snapshots/gc-audit.jsonl에 JSONL audit record append
```

Size 계산은 planning 보조 정보다. walk 중 파일이 사라지거나 stat할 수 없는 경우 해당
파일은 무시하고 planner는 실패하지 않는다. `candidates`, `protected`, `deleted` entry는
계산 가능한 경우 `size_bytes`를 포함한다. 이미 age 후보인 snapshot은 size pressure가
있어도 기존 `older_than` reason을 유지한다.

Audit record는 timestamp, applied, policy, candidates_count, deleted_count,
errors_count만 포함한다. snapshot metadata 전체를 기록하지 않으므로 `agent_token`이나
profile별 secret 값이 audit file에 들어가지 않는다. audit append 실패는 HTTP 200
response 안의 `errors`에 `snapshot_id: ""`, `error: "write GC audit: ..."` 형태로
추가하며 삭제 결과는 유지한다.

불변 조건:

- `apply` 기본값은 `false`다.
- 응답에 `agent_token`을 포함하지 않는다.
- diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
- live·persisted restored VM state가 참조하는 source snapshot은 GC 후보에서
  제외한다(`DELETE`와 동일한 restored-VM dependency 보호).
- `max_total_bytes`를 만족하지 못하더라도 diff snapshot이 참조 중인 full snapshot은
  보호 상태로 남긴다.
- 같은 GC 호출에서 diff를 삭제한 뒤 해당 full을 연쇄 삭제하지 않는다.
- 하나의 create/restore/delete/GC lifecycle operation만 동시에 실행된다. snapshot
  graph locking이 설계되기 전까지 restore/delete race와 진행 중인 restore 파일
  읽기 중 diff base 삭제를 피하기 위한 보수적 직렬화다.

## Guest agent 로직

`goose-agent`는 각 VM 내부에서 실행된다.

Startup:

```text
main()
  -> /root/.ephemera-agent-token 읽기
  -> vsock CHANGE_IP listener 시작
  -> /tasks, /workloads/run, /workspace, /stop, /health 등록
  -> 기본 :8080 listen
```

Task 실행:

```text
POST /tasks
  -> POST method 요구
  -> {"prompt":"..."} decode
  -> 빈 prompt 거부
  -> busy이면 503 반환
  -> busy=true
  -> prompt를 stdin으로 넘겨 /usr/local/bin/goose run -i - 실행
  -> {"output":"..."} 또는 {"output":"...","error":"..."} 반환
  -> busy=false
```

Script workload 실행:

```text
POST /workloads/run
  -> {"script":"workloads/name.sh","timeout_seconds":600} decode
  -> relative path, workloads/ prefix, .sh suffix, traversal reject
  -> busy이면 503 반환
  -> bash /workspace/workloads/name.sh 실행
  -> stdout/stderr/exit_code/duration_ms/timed_out 반환
```

Workspace file copy:

```text
PUT /workspace?path=<relative-path>[&overwrite=true]
  -> VM /workspace 기준 relative path 검증
  -> body가 4 MiB를 초과하면 413 JSON error
  -> overwrite=false 기본값이면 기존 file 존재 시 409 JSON error
  -> parent directory 생성
  -> raw bytes 저장
  -> {"path":"...","bytes":N} 반환

GET /workspace?path=<relative-path>
  -> VM /workspace 기준 relative path 검증
  -> file 없음이면 404 JSON error
  -> file이 4 MiB를 초과하면 413 JSON error
  -> raw bytes 반환
```

Health:

```text
GET /health
  -> {"status":"idle"} 또는 {"status":"busy"} 반환
```

Stop:

```text
POST /stop
  -> {"status":"stopping"} 반환
  -> 200 ms 뒤 HTTP server graceful shutdown
```

Vsock IP reconfiguration:

```text
CHANGE_IP <cidr_ip> <gateway>
  -> ip addr flush dev eth0
  -> ip addr add <cidr_ip> dev eth0
  -> ip link set eth0 up
  -> ip route replace default via <gateway>
  -> OK 또는 ERROR 반환
```

## Guest init 로직

`micro-init`은 VM 내부 PID 1이다.

```text
micro-init
  -> /proc, /sys, /dev, /dev/pts mount
  -> HOME, USER, PATH 설정
  -> /usr/local/bin/goose-agent 시작
  -> goose-agent exit 또는 SIGTERM/SIGINT 대기
  -> signal 수신 시 goose-agent에 SIGTERM 전송
  -> sync
  -> poweroff(2)
```

이 흐름은 PID 1이 단순 종료되면서 발생할 수 있는 guest kernel panic을 피한다.

## 오류 모델

- Control-plane auth 실패: `401`, body `{"error":"unauthorized"}`
- VM 없음: 일반적으로 `404`
- Snapshot base dependency conflict(diff가 참조하는 full, 또는 live·persisted
  restored VM이 참조하는 source snapshot): `409`
- Nested task depth 초과(`EPHEMERA_MAX_TASK_DEPTH` 이상): `508 Loop Detected`
- Snapshot restore 실패: `{"error":"...","code":"...","source_snapshot_id":"..."}`
  JSON body와 함께 `snapshot_not_found`, `source_vm_running`,
  `network_unavailable`, `diff_base_missing`, `memory_merge_failed`,
  `firecracker_restore_failed`, `guest_reconfigure_failed`, `agent_not_ready`
  중 하나를 반환한다.
- Invalid profile 또는 invalid snapshot type request: `400`
- Workspace copy error: `{"error":"..."}` JSON body와 함께 `400`, `404`,
  `409`, `413`, `500` 중 하나
- Host/runtime setup 실패: 일반적으로 `500`
- Agent proxy connection 실패: `502`

일부 legacy path는 아직 plain text body를 반환한다. MCP v1은 daemon status와
body를 보존하며, 모든 response를 새 domain model로 정규화하지 않는다.

## 운영 불변 조건

- 실행 중인 VM disk를 `destroyVM` 밖에서 삭제하거나 변경하지 않는다.
- list/snapshot/restore response에 guest `agent_token`을 노출하지 않는다.
- source VM이 실행 중인 snapshot은 restore하지 않는다.
- diff가 참조하는 full snapshot은 삭제하지 않는다.
- VM 생성 또는 restore 실패 시 항상 TAP/IP를 반환한다.
- VM 삭제 시 dm-snapshot, loop device, bind mount, sparse COW file을 정리한다.
- MCP layer에서 `anvil_stop_vm`과 `anvil_delete_vm` 의미를 구분한다.
  stop은 guest agent 중지 요청이고, delete는 host VM resource 삭제다.

## 소스 참조

- `cmd/goose-daemon/api.go`
- `cmd/goose-daemon/config.go`
- `cmd/goose-daemon/egress_policy.go`
- `cmd/goose-daemon/otel.go`
- `cmd/goose-agent/main.go`
- `cmd/micro-init/main.go`
- `internal/anvilmcp/tenant_policy.go`
- `internal/anvilmcp/quota_store.go`
- `internal/storage/snapshot.go`
- `internal/storage/provisioner.go`
- `internal/network/manager.go`
- `internal/vm/machine.go`
