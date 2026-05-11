# anvil IronClaw MCP 아키텍처

## 상태

- 기준 버전: `v0.2.0`
- MCP 버전: v1 stdio adapter
- Entrypoint: `cmd/anvil-mcp`
- 런타임 대상: ephemera control plane daemon HTTP API

MCP v1은 IronClaw와 ephemera runtime을 연결하는 얇은 bridge다. VM lifecycle의
의미를 직접 소유하지 않고, MCP tool call을 ephemera daemon API로 매핑한다.
adapter process 안에는 작은 in-memory
`session_name` alias map만 유지한다.

## 시스템 관점

```text
IronClaw 또는 다른 MCP client
  |
  | stdio MCP transport
  v
cmd/anvil-mcp
  |
  | internal/anvilmcp.Tools
  v
internal/anvilmcp.DaemonClient
  |
  | HTTP + optional Bearer token
  v
ephemera control plane daemon
  |
  | Firecracker, guest agent proxy, snapshots
  v
MicroVM runtime
```

adapter는 optional `session_name` alias를 제외하면 process-local stateless
component로 동작한다.

## 구성 요소 책임

| 구성 요소 | 파일 | 책임 |
|---|---|---|
| MCP server entrypoint | `cmd/anvil-mcp/main.go` | config load, daemon client 생성, tool handler 생성, MCP tool 등록, stdio transport 실행 |
| Config loader | `internal/anvilmcp/config.go` | 기본값, optional YAML config, 환경 변수 override load |
| Daemon client | `internal/anvilmcp/daemon_client.go` | control plane daemon HTTP 호출, daemon response body 보존 |
| Tool layer | `internal/anvilmcp/tools.go` | MCP input validation, VM identity resolve, task timeout 적용, tool을 daemon client method로 매핑 |
| Session store | `internal/anvilmcp/session_store.go` | 한 adapter process 안에서 `session_name -> vm_id` alias 유지 |
| Config example | `configs/anvil-mcp.yaml.example` | 파일 기반 adapter 설정 template |

## 설정 모델

기본값:

| Field | 기본값 |
|---|---|
| `daemon_url` | `http://127.0.0.1:3000` |
| `default_timeout_seconds` | `300` |
| Config file path | `configs/anvil-mcp.yaml` |

Load 순서:

```text
defaults
  -> optional YAML config file
  -> environment variables
```

환경 변수:

| 변수 | 의미 |
|---|---|
| `ANVIL_MCP_CONFIG` | config file path override |
| `ANVIL_DAEMON_URL` | daemon base URL override |
| `ANVIL_API_TOKEN` | daemon request에 사용할 Bearer token |
| `ANVIL_MCP_DEFAULT_TIMEOUT` | `anvil_run_task` 기본 timeout, 초 단위 |

Validation:

- `daemon_url`은 비어 있으면 안 된다.
- `daemon_url` scheme은 `http` 또는 `https`여야 한다.
- `daemon_url`에는 host가 있어야 한다.
- `default_timeout_seconds`는 양수여야 한다.
- `ANVIL_MCP_DEFAULT_TIMEOUT`은 양의 정수로 parse되어야 한다.

## 도구 계약

| MCP tool | Daemon call | 목적 |
|---|---|---|
| `anvil_spawn_vm` | `POST /vms` | VM 생성 및 optional `session_name` alias binding |
| `anvil_run_task` | `POST /vms/{vm_id}/tasks` | daemon agent proxy를 통해 VM에서 prompt 실행 |
| `anvil_get_vm_health` | `GET /vms/{vm_id}/health` | daemon proxy를 통해 guest agent health 반환 |
| `anvil_stop_vm` | `POST /vms/{vm_id}/stop` | guest agent에 graceful stop 요청 |
| `anvil_delete_vm` | `DELETE /vms/{vm_id}` | VM resource 삭제 및 관련 session alias 해제 |
| `anvil_create_snapshot` | `POST /vms/{vm_id}/snapshot` | VM full 또는 diff snapshot 생성 |
| `anvil_list_snapshots` | `GET /snapshots` | 저장된 snapshot 목록 조회 |
| `anvil_restore_snapshot` | `POST /snapshots/{snapshot_id}/restore` | snapshot에서 새 VM restore 및 optional alias binding |
| `anvil_delete_snapshot` | `DELETE /snapshots/{snapshot_id}` | snapshot 삭제 |

### `anvil_spawn_vm`

입력:

```json
{
  "profile": "optional-profile-name",
  "session_name": "optional-local-alias"
}
```

출력:

```json
{
  "vm_id": "vm-...",
  "guest_ip": "10.0.1.x",
  "agent_url": "http://...",
  "profile": "optional-profile-name",
  "session_name": "optional-local-alias"
}
```

동작:

- daemon 호출 전에 duplicate `session_name`을 거부한다.
- `POST /vms`를 호출한다.
- daemon spawn 이후 alias binding이 실패하면 best-effort로
  `DELETE /vms/{vm_id}` cleanup을 시도한다.
- MCP output에는 `agent_token`을 노출하지 않는다. guest token 사용은 daemon
  proxy가 소유한다.

### `anvil_run_task`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set",
  "prompt": "required prompt",
  "timeout_seconds": 300
}
```

동작:

- 비어 있지 않은 `prompt`를 요구한다.
- 음수 timeout을 거부한다.
- 24시간을 초과하는 timeout을 거부한다.
- VM identity를 resolve한다.
- `vm_id`와 `session_name`이 모두 있으면 `vm_id`가 우선한다.
- `timeout_seconds`가 있으면 그 값을, 없으면 config 기본 timeout을 사용한다.
- `POST /vms/{vm_id}/tasks`를 호출한다.

출력:

```json
{
  "status_code": 200,
  "body": "{...daemon response body...}"
}
```

### `anvil_get_vm_health`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set"
}
```

동작:

- VM identity를 resolve한다.
- `GET /vms/{vm_id}/health`를 호출한다.
- daemon status code와 raw response body를 반환한다.

### `anvil_stop_vm`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set"
}
```

동작:

- VM identity를 resolve한다.
- `POST /vms/{vm_id}/stop`을 호출한다.
- session alias는 제거하지 않는다.
- host-side VM resource도 삭제하지 않는다.

이 구분이 중요하다. stop은 guest agent HTTP server의 종료 요청이고, delete는
control plane의 VM resource 삭제다.

### `anvil_delete_vm`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set"
}
```

동작:

- VM identity를 resolve한다.
- `DELETE /vms/{vm_id}`를 호출한다.
- daemon response가 성공이면 삭제된 VM을 가리키는 모든 local alias를 제거한다.

### `anvil_create_snapshot`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set",
  "stop_after": false,
  "type": "full"
}
```

동작:

- VM identity를 resolve한다.
- `type`은 생략, 빈 문자열, `full`, `diff`만 허용한다.
- `type`은 대소문자 차이를 정규화해 daemon에는 소문자로 전달한다.
- `POST /vms/{vm_id}/snapshot`을 호출한다.
- daemon snapshot response를 그대로 구조화해 반환한다.

### `anvil_list_snapshots`

입력:

```json
{}
```

동작:

- `GET /snapshots`를 호출한다.
- snapshot alias나 filtering은 제공하지 않는다.
- 출력은 `{ "snapshots": [...] }` wrapper object다.

### `anvil_restore_snapshot`

입력:

```json
{
  "snapshot_id": "snap-1",
  "session_name": "optional-local-alias"
}
```

동작:

- 빈 `snapshot_id`를 daemon 호출 전에 거부한다.
- `session_name`이 있으면 restore 전에 duplicate 여부를 검사한다.
- `POST /snapshots/{snapshot_id}/restore`를 호출한다.
- restore 성공 후 `session_name -> restored_vm_id`를 bind한다.
- duplicate 사전 검사와 사후 bind 사이의 race로 bind가 실패할 수 있다.
- 사후 bind 실패 시 restored VM은 자동 삭제하지 않는다. error는 restored VM ID와 직접 cleanup 필요성을 포함한다.
- MCP output에는 daemon restore response의 `agent_token`을 노출하지 않는다.

출력:

```json
{
  "vm_id": "vm-restored",
  "guest_ip": "10.0.1.9",
  "agent_url": "http://192.168.3.73:3000/vms/vm-restored/agent",
  "profile": "dev",
  "source_snapshot_id": "snap-1",
  "session_name": "optional-local-alias"
}
```

### `anvil_delete_snapshot`

입력:

```json
{
  "snapshot_id": "snap-1"
}
```

동작:

- 빈 `snapshot_id`를 daemon 호출 전에 거부한다.
- `DELETE /snapshots/{snapshot_id}`를 호출한다.
- diff snapshot이 참조 중인 full snapshot 삭제처럼 daemon이 non-2xx를 반환하면 status code와 body를 `DaemonError`로 보존한다.

## 세션 alias 모델

`SessionStore`는 in-memory convenience map이다.

```text
session_name -> vm_id
```

규칙:

- 빈 session name은 invalid다.
- 빈 VM ID는 invalid다.
- duplicate session name은 거부한다.
- `vm_id`와 `session_name`이 모두 제공되면 `vm_id`가 우선한다.
- 알 수 없는 session name은 daemon call 전에 거부한다.
- `anvil_delete_vm`은 삭제된 VM의 alias를 제거한다.
- `anvil_stop_vm`은 alias를 제거하지 않는다.
- `anvil_restore_snapshot`은 restore 성공 후 optional alias를 새 VM ID에 연결한다.
- restore 후 alias bind가 실패하면 restored VM은 자동 삭제되지 않는다.
- `anvil-mcp` process가 종료되면 alias는 사라진다.

adapter는 session state를 disk에 저장하지 않는다.

## Daemon client 동작

`DaemonClient`는 설정된 base URL과 tool별 path로 HTTP request를 만든다.

Request 동작:

- token이 설정되어 있으면 `Authorization: Bearer <ANVIL_API_TOKEN>`을 추가한다.
- JSON body가 있는 request에는 `Content-Type: application/json`을 추가한다.
- MCP call context를 사용하며, task timeout이 설정되면 context deadline에
  반영한다.

Response 동작:

- 2xx response는 status code와 body를 반환한다.
- non-2xx response는 daemon status code와 raw body를 담은 `DaemonError`를
  반환한다.
- tool layer는 daemon error를 새 domain model로 다시 쓰지 않는다.

이 선택은 adapter를 얇게 유지하고 daemon 동작을 MCP client에 그대로 보이게
한다.

## 보안 모델

| 관심사 | 현재 동작 |
|---|---|
| Daemon 인증 | adapter가 `ANVIL_API_TOKEN`을 daemon Bearer token으로 사용 |
| Guest agent token | MCP output에는 노출하지 않고 daemon proxy가 주입한다. Restore response decode용 내부 struct에만 존재한다. |
| Session alias | process-local memory에만 저장 |
| Secrets | config file은 `api_token`을 담을 수 있으므로 local config를 git에 넣지 않는다. |
| Transport | MCP v1은 client와 adapter 사이에서 stdio를 사용 |

adapter는 daemon URL과 API token을 신뢰된 local/operator configuration으로
가정한다.

## Smoke test

문서 기준 smoke test는 IronClaw 본체 설치를 전제로 하지 않는다. Go MCP SDK
client가 `cmd/anvil-mcp`를 stdio MCP server로 실행하고, 실제 ephemera daemon을
대상으로 tool call을 순서대로 수행한다.

```bash
sudo ANVIL_API_ADDR=127.0.0.1:3000 ./anvil-daemon
```

다른 터미널:

```bash
go run scripts/anvil-mcp-smoke.go -session smoke
```

검증 순서:

```text
start ephemera daemon
start anvil-mcp through MCP CommandTransport
call anvil_spawn_vm
call anvil_run_task
call anvil_get_vm_health
call anvil_stop_vm
call anvil_delete_vm
```

기본 smoke test는 `anvil_run_task` 응답에 `anvil-smoke-ok`가 포함되는지 확인한다.
LLM provider credential이 유효하지 않은 환경에서 MCP lifecycle transport만
확인하려면 `-expect-output ""`를 사용한다. 이 경우 `anvil_run_task`가 provider
error를 반환해도 MCP tool path, VM lifecycle, cleanup 경로를 확인할 수 있다.

## 실패 동작

| 실패 | 결과 |
|---|---|
| 명시 config file 없음 | config load error |
| 기본 config file 없음 | 허용. defaults/env 사용 |
| invalid daemon URL | config load error |
| duplicate session name | daemon call 전 tool validation error |
| unknown session name | daemon call 전 tool validation error |
| daemon 4xx/5xx | status와 body를 포함한 `DaemonError` |
| daemon connection failure | request 전송 error |
| spawn 성공 후 alias binding 실패 | best-effort VM delete 후 error 반환 |
| restore 성공 후 alias binding 실패 | restored VM은 자동 삭제되지 않는다. error는 restored VM ID를 포함하며, 필요하면 operator/user가 `anvil_delete_vm`으로 명시적으로 정리해야 한다. |

## v1 비목표

MCP v1은 의도적으로 다음을 구현하지 않는다.

- workspace copy-in/copy-out
- snapshot alias 또는 snapshot name
- session alias 영속화
- session name으로 최신 snapshot 자동 선택
- HTTP MCP transport
- daemon API 의미 재해석

위 항목은 v1의 숨은 동작이 아니라 향후 MCP v2 설계 후보로 남긴다.

## 소스 참조

- `cmd/anvil-mcp/main.go`
- `internal/anvilmcp/config.go`
- `internal/anvilmcp/daemon_client.go`
- `internal/anvilmcp/session_store.go`
- `internal/anvilmcp/tools.go`
- `configs/anvil-mcp.yaml.example`
- `scripts/anvil-mcp-smoke.go`
- `docs/superpowers/specs/2026-05-11-anvil-ironclaw-mcp-v1-design.md`
