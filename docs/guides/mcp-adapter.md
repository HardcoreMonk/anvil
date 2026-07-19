# IronClaw MCP 어댑터 가이드

> IronClaw용 `anvil_*` MCP tool 표면, multi-tenant foundation, scheduler service, routed flock을 다룹니다.
> 이 어댑터(`cmd/anvil-mcp`)는 VM 내부 agent용 runtime MCP Gateway(`EPHEMERA_MCP_*`)와 별개입니다.
> 프로젝트 개요는 [README](../../README.md), daemon REST API는 [api-reference.md](api-reference.md)를 참고하세요.

## IronClaw MCP 어댑터

`cmd/anvil-mcp`는 ephemera daemon API를 stdio MCP server로 노출한다.

```bash
go build -o anvil-mcp ./cmd/anvil-mcp
```

환경 변수 설정:

```bash
export ANVIL_DAEMON_URL=http://127.0.0.1:3000
export ANVIL_API_TOKEN="<daemon-bearer-token>"
export ANVIL_MCP_DEFAULT_TIMEOUT=300
# 선택 사항: multi-tenant foundation과 runtime audit
export ANVIL_MCP_TENANT_ID=tenant.alpha
export ANVIL_MCP_AUDIT_LOG=/var/lib/anvil-mcp/runtime-audit.jsonl
```

여기서 `ANVIL_API_TOKEN`은 `cmd/anvil-mcp` 프로세스가 daemon으로 보내는 outbound
Bearer token이다. goose-daemon 환경 변수에서는 같은 이름이
`EPHEMERA_API_TOKEN`의 fallback alias로, daemon이 client 요청에서 받아들이는
control-plane token을 뜻한다.

또는 설정 파일을 사용할 수 있다.

```bash
cp configs/anvil-mcp.yaml.example configs/anvil-mcp.yaml
export ANVIL_MCP_CONFIG=configs/anvil-mcp.yaml
```

MCP tool:

- `anvil_spawn_vm`:
  ephemera VM을 만들고 optional `session_name` alias를 연결한다.

- `anvil_run_task`:
  `vm_id` 또는 `session_name`으로 VM에 prompt를 실행한다.

- `anvil_copy_in`:
  `vm_id` 또는 `session_name`으로 VM `/workspace`에 단일 file을 쓴다.

- `anvil_copy_out`:
  `vm_id` 또는 `session_name`으로 VM `/workspace`의 단일 file을 읽는다.

- `anvil_get_vm_health`:
  VM agent health를 확인한다.

- `anvil_stop_vm`:
  guest agent에 graceful stop을 요청한다.

- `anvil_delete_vm`:
  host VM 리소스를 삭제하고 session alias를 해제한다.

- `anvil_create_snapshot`:
  `vm_id` 또는 `session_name`으로 VM snapshot을 생성한다.

- `anvil_list_snapshots`:
  daemon이 알고 있는 snapshot 목록을 조회한다.

- `anvil_restore_snapshot`:
  `snapshot_id`에서 새 VM을 restore하고 optional `session_name` alias를 연결한다.

- `anvil_delete_snapshot`:
  `snapshot_id`로 snapshot을 삭제한다.

- `anvil_replicate_snapshot`:
  `snapshot_id`, `source_host`, `target_host`, `include_dependencies`로 host 간
  snapshot bundle을 복제한다. diff snapshot에서 `include_dependencies=true`이면
  base full snapshot을 먼저 target host로 복제한 뒤 diff를 복제한다.

- `anvil_spawn_flock`:
  `task`, `roles`, optional `tenant_id`, optional `egress_policy`로 Goosetown
  flock을 생성한다. blank `task`, empty role, `/` 또는 `\`가 포함된 role은
  daemon VM spawn 전에 거부된다.

- `anvil_create_routed_flock_members`:
  `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`와 persistent scheduler state가
  설정된 경우에만 활성화되는 experimental tool이다. scheduler `POST /schedule/flock`
  plan으로 role별 host를 고른 뒤 host daemon `POST /vms`로 member VM만 생성한다.
  이어서 home host(`roles[0]` 배치 호스트) daemon에 `POST /flocks/{id}/distributed`로
  hub flock을, 나머지 멤버 host daemon에 `POST /flocks/{id}/relay`로 relay flock을
  등록해 공유 Town Wall을 구성한다.

- `anvil_list_flocks` / `anvil_get_flock` / `anvil_delete_flock`:
  live flock 목록, 단일 flock metadata와 agent 상태 조회, flock 소속 VM 삭제를 처리한다.
  일반 Goosetown flock은 daemon `GET/DELETE /flocks` 의미를 따른다. members-only
  routed flock은 `scheduler_state_path` registry의 visible record를 list/get에 합치고,
  delete는 registry의 member placement를 따라 host별 daemon `DELETE /vms`로 라우팅한다.
  delete와 create 실패 rollback은 hub/relay flock 등록도 함께 해제하고
  (`deregisterRoutedFlockWall`) 해당 flock의 per-flock `relay_token`을 revoke한다.

- `anvil_post_townwall` / `anvil_get_townwall_history`:
  flock Town Wall에 message를 append하고 stdio-compatible history를 조회한다.

Goosetown MCP tool은 additive extension이며 기존 VM/snapshot tool 계약을
대체하지 않는다. VM `session_name` alias는 flock alias로 재사용하지 않고, flock
작업은 명시적인 `flock_id`를 사용한다.

daemon direct `POST /flocks` 응답도 `agent_token`/`agent_tokens`를 노출하지 않는다.
upstream ephemera `v0.3.1`의 `agent_tokens` 응답 추가는 anvil 보안 불변 조건에 맞춰
downstream에서 채택하지 않는다.

MCP adapter는 얇은 runtime bridge다. 현재 workspace copy는 VM 내부
`/workspace` 기준 단일 file copy-in/copy-out만 지원한다. 기본 encoding은
`text`이고, binary payload는 `encoding: "base64"`로 전달한다. 단일 파일 크기는
4 MiB로 제한하며, copy-in은 기본적으로 기존 파일을 덮어쓰지 않는다.
`overwrite: true`를 명시해야 교체한다. directory sync, snapshot alias,
HTTP MCP transport는 제공하지 않는다. Restore 응답은 daemon direct response와
MCP output 모두 `agent_token`을 노출하지 않는다.
Restore 후 `session_name` bind가 실패하면 adapter는 restored VM을 자동 삭제하지
않고 error에 restored VM ID를 포함한다.

Multi-tenant foundation은 MCP adapter boundary에서 optional `tenant_id`와
`ANVIL_MCP_TENANT_ID` 기본값을 검증한 뒤 `POST /vms`,
`POST /vms/{id}/snapshot`, `POST /snapshots/{id}/restore` daemon body로 전달한다.
`egress_policy`는 `deny_all`, `profile`, `allow_all` 중 하나이며 VM/snapshot
metadata에 보존된다. `ANVIL_MCP_AUDIT_LOG`를 설정하면 성공/실패 tool call에 대해
tenant ID, VM ID, session alias, tool name, daemon operation, result code,
timestamp, sanitized error만 JSONL로 append한다. 이 audit record에는 snapshot
metadata, daemon raw body, `agent_token`을 저장하지 않는다.
`ANVIL_MCP_AUDIT_LOG`를 켠 상태에서는 tool input `tenant_id` 또는
`ANVIL_MCP_TENANT_ID`가 필요하다.

현재 control-plane foundation은 host inventory polling, `cmd/anvil-scheduler`,
scheduler-backed `RuntimeRouter`, JSON quota store, persistent placement/snapshot
locality store, daemon `/tenants`, `/audit/runtime`, `/health`, `/metrics`,
`/metrics/vms`를 제공한다. router는 snapshot locality preferred host, retry/failover,
placement reconciliation helper를 제공한다. `anvil_replicate_snapshot`은
RuntimeRouter가 source daemon의 `POST /snapshots/{id}/export` stream을 target
daemon의 `POST /snapshots/import`로 전달하고, 성공한 target host만 scheduler
`SnapshotLocations`에 기록한다. operator-facing response와 audit record는
`agent_token`, authorization header, daemon raw body, raw `metadata.json` body를
포함하지 않는다.

MCP production config에서 기존 VM/snapshot tool은 `ANVIL_DAEMON_URL` direct daemon
동작을 유지한다. snapshot replication과 scheduler-aware flock placement만 router를
사용하며, router는 `scheduler_state_path` 또는 `ANVIL_MCP_SCHEDULER_STATE`,
`scheduler_hosts_file` 또는 `ANVIL_MCP_SCHEDULER_HOSTS_FILE`이 설정된 경우
활성화된다. `ANVIL_MCP_SCHEDULER_STATE` 또는
`ANVIL_MCP_SCHEDULER_HOSTS_FILE`로 router config가 제공되면 `anvil_spawn_flock`은
기존 scheduler-aware single-host placement를 계속 사용한다. roles 수만큼 active VM
capacity/quota를 확인한 뒤 하나의 healthy host를 선택하고, daemon
`POST /flocks`는 그 host에서 기존 single-host 의미로 실행한다.
`ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`와 persistent
`scheduler_state_path`가 함께 설정되면 experimental
`anvil_create_routed_flock_members`도 사용할 수 있다. 이 tool은
`POST /schedule/flock` plan으로 role별 host를 정하고, 각 host daemon의 `POST /vms`를
호출해 role VM을 생성한 뒤 member placement를 `scheduler_state_path`의 routed flock
registry에 기록한다. 반환 `mode`는 `cross_host_members_only`,
`town_wall_enabled=true`다. 2026-07-06 cross-host shared Town Wall slice부터 routed
member는 `.ephemera-flock`/`.ephemera-cp-token`이 주입돼(`POST /vms`) guest flock
context를 갖추고, `roles[0]` 배치 호스트가 home으로 선정돼 home daemon에 hub
flock(공유 `TownWall`)을, 나머지 멤버 host daemon에 relay flock(post/wall/history를
home으로 forward/proxy)을 등록한다(`internal/anvilmcp/routed_flock.go`
`TownWallEnabled` 설정부). 2026-07-08 cross-host gtcall slice부터 daemon
`POST /flocks/{id}/call`로 routed flock의 임의 member가 다른 임의 member를 호출할
수 있다(member→home→target 2-hop; `relay_token`은 guest 능력 토큰으로 그 flock의
wall과 `call` 진입을 모두 admit하고, 별도 `call_token`이 daemon-to-daemon call
hop만 admit한다 — wall 경로는 거부). cross-host broadcast fan-out만 이 표면 범위
밖 비목표로 남는다. daemon-to-daemon relay hop은 dial-실패에 한정해 동기
bounded retry(총 3시도, 1s/2s)로 짧은 네트워크 순단을 자동 흡수한다.
`scheduler_quota_store_path` 또는 `ANVIL_MCP_SCHEDULER_QUOTA_STORE`는 scheduler quota
store를 함께 지정할 때 사용한다. host daemon client 인증에는 `ANVIL_API_TOKEN`을
사용한다.

MCP router 관련 설정:

| 설정 | 의미 |
|---|---|
| `scheduler_state_path` / `ANVIL_MCP_SCHEDULER_STATE` | router placement, snapshot locality, routed flock registry를 저장하는 persistent JSON path |
| `scheduler_hosts_file` / `ANVIL_MCP_SCHEDULER_HOSTS_FILE` | router가 사용할 runtime host inventory JSON path |
| `scheduler_quota_store_path` / `ANVIL_MCP_SCHEDULER_QUOTA_STORE` | optional tenant quota JSON path |
| `cross_host_flock_create_mode` / `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only` | members-only routed flock create opt-in. persistent `scheduler_state_path`가 필요하며 기본 `anvil_spawn_flock`을 대체하지 않는다 |
| `reconcile_interval` / `ANVIL_MCP_RECONCILE_INTERVAL` | `members_only` 모드에서 `ReconcilePlacements`를 재실행하는 주기(`time.ParseDuration` 형식, 기본 `60s`, `0`=off). daemon 재시작 후 hub/relay wall 등록과 relay-token admission을 자동 복구한다 |

예시:

```bash
anvil_replicate_snapshot \
  snapshot_id=snap-1 \
  source_host=host-a \
  target_host=host-b \
  include_dependencies=true
```

Scheduler service를 별도 process로 실행할 때는 다음 환경 변수를 사용한다.

```bash
go build -o anvil-scheduler ./cmd/anvil-scheduler

ANVIL_SCHEDULER_ADDR=127.0.0.1:3010 \
ANVIL_SCHEDULER_STATE=/var/lib/anvil/scheduler.json \
ANVIL_SCHEDULER_QUOTA_STORE=/var/lib/anvil/tenants.json \
ANVIL_SCHEDULER_POLL_INTERVAL=10s \
ANVIL_SCHEDULER_RECONCILE_INTERVAL=30s \
ANVIL_SCHEDULER_FAILURE_THRESHOLD=3 \
./anvil-scheduler
```

control loop과 host bootstrap 관련 scheduler 환경 변수:

| Env var | 목적 |
|---|---|
| `ANVIL_SCHEDULER_HOSTS_FILE` | config-managed runtime host bootstrap JSON 파일. 설정한 파일은 존재해야 한다 |
| `ANVIL_SCHEDULER_POLL_INTERVAL` | host observation poll 주기 |
| `ANVIL_SCHEDULER_RECONCILE_INTERVAL` | placement reconciliation 주기 |
| `ANVIL_SCHEDULER_HOST_TIMEOUT` | daemon `/health`, `/vms` 요청별 timeout |
| `ANVIL_SCHEDULER_FAILURE_THRESHOLD` | host를 `unhealthy`로 전이하기 전 연속 실패 횟수 |
| `ANVIL_SCHEDULER_API_TOKEN` | ephemera daemon에 전달할 bearer token |
| `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE` | `true`면 scheduler state 저장 장애 중 신규 scheduling을 503으로 차단 |

현재 daemon `/health`는 scheduler capacity 필드를 제공하지 않으므로 hosts file에서
`available_vms`, `available_snapshot_bytes`, `egress_policies`를 함께 지정한다.

`--verify`와 standalone smoke harness는 host에 `curl`, `python3`가 있어야
실행된다. systemd 설치 host에서는 dry-run, start, smoke verify를 같은 installer로
수행한다.

```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --verify
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

이미 실행 중인 scheduler만 확인할 때는 standalone smoke harness를 사용한다.

```bash
bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010
```

smoke harness는 기본 host id를 `anvil-scheduler-smoke-*`로 생성하고,
`GET /hosts`로 같은 host id의 기존 inventory record가 없는지 먼저 확인한다.
충돌이 있으면 `PUT /hosts`를 실행하기 전에 실패하므로 운영 host record를 덮어쓰지
않는다. 등록한 fake host는 `DELETE /hosts/{name}`로 제거하며 cleanup 실패는 smoke
성공으로 취급하지 않는다. smoke harness가 등록하는 fake host는 `smoke_only: true`이며,
`PreferredHosts`에 해당 host id를 명시한 smoke 요청에서만 선택된다. smoke harness는
`PreferredHosts`가 없는 추가 `/schedule/spawn`도 실행해 smoke-only host가 일반
fallback placement 후보로 선택되지 않는지 확인한다. `POST /schedule/flock`도
dry-run으로 호출해 planner response가 `agents`, `requested`,
`host_status_summary` 계약을 유지하는지 확인한다.

`POST /schedule/flock`을 수동 확인할 때는 다음처럼 dry-run 요청을 보낸다. 이 요청은
VM을 생성하지 않는다.

```bash
curl -sS -X POST http://127.0.0.1:3010/schedule/flock \
  -H 'Content-Type: application/json' \
  --data '{"tenant_id":"tenant-1","egress_policy":"profile","roles":["worker","reviewer"]}'
```

Scheduler service API는 operator가 host inventory와 placement 상태를 관리하는
얇은 control-plane surface다.

| Endpoint | 목적 |
|---|---|
| `GET /health` | scheduler process 상태 확인 |
| `GET/PUT /hosts` | runtime host inventory 조회/등록 |
| `DELETE /hosts/{name}` | smoke/운영 정리용 runtime host inventory 제거. 없는 host 삭제는 idempotent success로 처리 |
| `GET /placements` | host, VM placement, snapshot location state 조회 |
| `GET /control-loop/status` | scheduler control loop 실행 상태, host observation, degraded/unhealthy 판단 조회 |
| `POST /reconcile` | 현재 placement state 반환. router reconciliation은 daemon `GET /vms` 기반 helper가 수행 |
| `POST /schedule/spawn` | spawn 요청의 host decision 반환 |
| `POST /schedule/flock` | flock roles를 host별 agent placement plan으로 dry-run한다. VM은 생성하지 않는다. |
| `POST /schedule/restore?snapshot_id=...` | snapshot locality를 반영한 restore host decision 반환 |

Scheduler service는 operator JSON endpoint와 별도로 scheduler 전용 Prometheus text
`GET /metrics`를 제공한다. 이 endpoint는 daemon `/metrics`와 다른 surface이며
`anvil_scheduler_*` namespace로 control loop running flag, persistence degraded
flag, host status count, suspect placement count, last poll/reconcile timestamp,
flock placement, snapshot replication, tenant quota metric family를 반환한다 —
전체 목록·라벨은 canonical [`docs/operations/observability.md`](../operations/observability.md)의
"Scheduler metrics endpoint" 절을 참조한다(이 페이지는 신규 metric family가
추가돼도 갱신을 보장하지 않는다). scheduler에는 자체 인증 계층이 없으므로 기존
scheduler 운영 경계처럼 loopback/private network 또는 reverse proxy policy 뒤에서만
노출한다.

`deny_all` egress policy는 host `iptables` reject rule로 강제한다. `profile` policy는
`configs/profiles/{profile}/egress.json`,
`EPHEMERA_EGRESS_PROFILE_DIR`, `ANVIL_EGRESS_PROFILE_DIR` 아래의 profile별
`egress.json`이 있을 때 allow CIDR/host/SNI/DNS rule을 적용하고, policy 파일이
없으면 기존 profile 호환성을 위해 no-op이다. 예시:

```json
{
  "allow_cidrs": ["203.0.113.10/32"],
  "allow_hosts": ["api.anthropic.com"],
  "allow_sni": ["api.anthropic.com"],
  "dns_servers": ["1.1.1.1"]
}
```

`allow_sni`는 파싱된 ClientHello SNI 기준 :443 도메인 allowlist다(TCP+QUIC/UDP
모두 적용). `allow_hosts`의 packet-string 매치보다 정밀하며, 신규 profile은
`allow_sni`를 쓴다. 상세는 [security-and-resilience.md](security-and-resilience.md)와
[ADR-0002](../adr/0002-egress-sni-transparent-filter.md)를 참고한다.

Optional trace export는 `ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT` 또는
`OTEL_EXPORTER_OTLP_ENDPOINT`를 설정하면 lifecycle span을 `{endpoint}/v1/traces`로
보낸다. trace attribute는 token/secret 계열 값을 제거한 뒤 전송한다.

정확한 입력/출력 계약은 `docs/architecture/mcp-architecture.md`를 참조한다.

문서 기준 MCP smoke test는 실제 daemon과 `anvil-mcp` stdio server를 함께
사용한다. 일반 CI에서는 KVM/root가 필요한 daemon 실행을 요구하지 않고
`go test ./...`, `go build ./cmd/anvil-mcp`, `go build ./cmd/anvil-scheduler` 같은
CI-safe 검증만 수행한다.
MCP smoke는 Firecracker를 실행할 수 있는 host에서 별도로 수행한다.

먼저 root 권한으로 daemon을 실행한다.

```bash
sudo ANVIL_API_ADDR=127.0.0.1:3000 ./anvil-daemon
```

다른 터미널에서 smoke wrapper를 실행한다. wrapper는
`go build -o /tmp/anvil-mcp ./cmd/anvil-mcp`로 adapter를 빌드한 뒤 smoke client가
해당 binary를 stdio MCP server로 실행하게 한다.

```bash
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
```

`lifecycle`은 기본 모드이며 내부적으로
`go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -expect-output ""`를
실행한다. `semantic`은
`go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -expect-output "anvil-smoke-ok"`를
실행한다.

두 모드 모두 `anvil_spawn_vm`, `anvil_copy_in`, `anvil_copy_out`,
`anvil_run_task`, `anvil_get_vm_health`, `anvil_stop_vm`,
`anvil_delete_vm` 순서로 tool call을 수행한다. `lifecycle`은 workspace copy
round-trip과 VM cleanup 경로를 확인하되 `anvil_run_task` 응답 body의 의미적
marker는 검사하지 않는다. `semantic`은 같은 flow에 더해 `anvil-smoke-ok`
포함 여부를 확인한다.

`flock` 모드는 daemon이 이미 실행 중인 상태에서 MCP를 통해
`anvil_spawn_flock`, `anvil_list_flocks`, `anvil_post_townwall`,
`anvil_get_townwall_history`, `anvil_delete_flock` 경로를 확인한다. Town Wall
SSE stream은 MCP smoke 대상이 아니며, history 조회로 stdio-compatible inspection을
수행한다.

daemon은 smoke 실행 전에 이미 떠 있어야 하며 `ANVIL_DAEMON_URL`과 필요한 경우
`ANVIL_API_TOKEN`으로 adapter가 daemon에 도달할 수 있어야 한다. daemon 실행에는
`/dev/kvm`, root 권한, Firecracker 실행 가능 host가 필요하다. `semantic`은 유효한
LLM credential과 provider 응답까지 요구한다. `lifecycle`은 의미적 marker 검사만
끄므로, 선택한 daemon/profile의 `anvil_run_task` 경로가 2xx로 완료될 수 있어야
한다.
