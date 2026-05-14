# anvil IronClaw MCP 아키텍처

## 상태

- 기준 버전: `v0.2.0`
- MCP 버전: v1 stdio adapter
- Entrypoint: `cmd/anvil-mcp`
- 런타임 대상: ephemera control plane daemon HTTP API

MCP v1은 IronClaw와 ephemera runtime을 연결하는 얇은 bridge다. VM lifecycle의
의미를 직접 소유하지 않고, MCP tool call을 ephemera daemon API로 매핑한다.
adapter process 안에는 작은 `session_name` alias map만 유지하며, 설정된 경우에만
이 alias map을 local JSON file로 영속화한다.

이 adapter의 제품 통합 대상은 IronClaw 전용이다. Go MCP SDK smoke client는
검증 도구일 뿐이며, OpenClaw 또는 다른 orchestration product와의 연동 계약을
의미하지 않는다.

## 시스템 관점

```text
IronClaw
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
component로 동작한다. `session_store_path`가 비어 있으면 alias도 process-local
memory에만 남는다.

## 구성 요소 책임

| 구성 요소 | 파일 | 책임 |
|---|---|---|
| MCP server entrypoint | `cmd/anvil-mcp/main.go` | config load, daemon client 생성, tool handler 생성, MCP tool 등록, stdio transport 실행 |
| Config loader | `internal/anvilmcp/config.go` | 기본값, optional YAML config, 환경 변수 override load |
| Daemon client | `internal/anvilmcp/daemon_client.go` | control plane daemon HTTP 호출, daemon response body 보존 |
| Tool layer | `internal/anvilmcp/tools.go` | MCP input validation, VM identity resolve, task timeout 적용, tool을 daemon client method로 매핑 |
| Session store | `internal/anvilmcp/session_store.go` | `session_name -> vm_id` alias 유지, optional JSON load/save |
| Config example | `configs/anvil-mcp.yaml.example` | 파일 기반 adapter 설정 template |

## 설정 모델

기본값:

| Field | 기본값 |
|---|---|
| `daemon_url` | `http://127.0.0.1:3000` |
| `default_timeout_seconds` | `300` |
| `session_store_path` | 비어 있음 |
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
| `ANVIL_MCP_SESSION_STORE` | session alias 영속 JSON file path override |
| `ANVIL_MCP_TENANT_ID` | optional 기본 tenant identifier |
| `ANVIL_MCP_AUDIT_LOG` | optional runtime audit JSONL file path |

Validation:

- `daemon_url`은 비어 있으면 안 된다.
- `daemon_url` scheme은 `http` 또는 `https`여야 한다.
- `daemon_url`에는 host가 있어야 한다.
- `default_timeout_seconds`는 양수여야 한다.
- `ANVIL_MCP_DEFAULT_TIMEOUT`은 양의 정수로 parse되어야 한다.
- `default_tenant_id`와 `ANVIL_MCP_TENANT_ID`는 설정된 경우 ASCII letter/digit로
  시작하고 letter, digit, `.`, `_`, `-`만 포함해야 한다.

## 도구 계약

| MCP tool | Daemon call | 목적 |
|---|---|---|
| `anvil_spawn_vm` | `POST /vms` | VM 생성 및 optional `session_name` alias binding |
| `anvil_run_task` | `POST /vms/{vm_id}/tasks` | daemon agent proxy를 통해 VM에서 prompt 실행 |
| `anvil_copy_in` | `PUT /vms/{vm_id}/workspace?path=...` | VM `/workspace` 아래 단일 file 쓰기 |
| `anvil_copy_out` | `GET /vms/{vm_id}/workspace?path=...` | VM `/workspace` 아래 단일 file 읽기 |
| `anvil_get_vm_health` | `GET /vms/{vm_id}/health` | daemon proxy를 통해 guest agent health 반환 |
| `anvil_stop_vm` | `POST /vms/{vm_id}/stop` | guest agent에 graceful stop 요청 |
| `anvil_delete_vm` | `DELETE /vms/{vm_id}` | VM resource 삭제 및 관련 session alias 해제 |
| `anvil_create_snapshot` | `POST /vms/{vm_id}/snapshot` | VM full 또는 diff snapshot 생성 |
| `anvil_list_snapshots` | `GET /snapshots` | 저장된 snapshot 목록 조회 |
| `anvil_restore_snapshot` | `POST /snapshots/{snapshot_id}/restore` | snapshot에서 새 VM restore 및 optional alias binding |
| `anvil_delete_snapshot` | `DELETE /snapshots/{snapshot_id}` | snapshot 삭제 |

VM/snapshot 관련 tool input은 `anvil_list_snapshots`를 제외하고 optional
`tenant_id`를 받을 수 있다.
`tenant_id`가 비어 있으면 `ANVIL_MCP_TENANT_ID` 또는 `default_tenant_id`를 사용한다.
adapter는 이 값을 검증하고 runtime audit record와 daemon API body에 전달한다.
`tenant_id`를 header, profile, session alias, VM ID에 끼워 넣어 quota 근거로
사용하지 않는다. `anvil_spawn_vm`과 `anvil_restore_snapshot`은 optional
`egress_policy`도 받으며 값은 `deny_all`, `profile`, `allow_all` 중 하나다.
`ANVIL_MCP_AUDIT_LOG`를 켠 상태에서는 input `tenant_id` 또는 기본 tenant ID가
필요하다.

### `anvil_spawn_vm`

입력:

```json
{
  "profile": "optional-profile-name",
  "session_name": "optional-local-alias",
  "tenant_id": "optional-tenant",
  "egress_policy": "profile"
}
```

출력:

```json
{
  "vm_id": "vm-...",
  "guest_ip": "10.0.1.x",
  "agent_url": "http://...",
  "profile": "optional-profile-name",
  "tenant_id": "optional-tenant",
  "egress_policy": "profile",
  "session_name": "optional-local-alias"
}
```

동작:

- daemon 호출 전에 duplicate `session_name`을 거부한다.
- `POST /vms`를 호출한다.
- daemon spawn 이후 alias binding이 실패하면 best-effort로
  `DELETE /vms/{vm_id}` cleanup을 시도한다.
- `session_store_path`가 설정된 경우 alias binding 성공 후 JSON file에 저장한다.
- alias binding은 성공했지만 JSON 저장이 실패하면 error를 반환하고, 새로 만든
  VM은 best-effort로 삭제한다.
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

### `anvil_copy_in`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set",
  "path": "notes/task.txt",
  "content": "file content",
  "encoding": "text",
  "overwrite": false
}
```

동작:

- VM identity를 resolve한다.
- `path`는 VM 내부 `/workspace` 기준 상대경로다.
- 빈 path, 절대경로, `..` traversal, backslash가 포함된 path를 거부한다.
- `encoding`은 생략, `text`, `base64`만 허용한다. 생략하면 `text`다.
- `base64` copy-in은 content를 decode한 raw bytes를 VM에 저장한다.
- 단일 파일 payload는 4 MiB를 초과할 수 없다.
- `overwrite` 기본값은 `false`다. 기존 file이 있으면 guest agent가 `409`를 반환한다.
- `overwrite: true`이면 `PUT /vms/{vm_id}/workspace?path=...&overwrite=true`를 호출한다.
- daemon은 guest agent token을 주입하고, guest agent가 parent directory를 만든 뒤
  파일을 저장한다.
- workspace error body는 JSON `{"error":"..."}` 형식이다.

출력:

```json
{
  "status_code": 200,
  "body": "{\"path\":\"notes/task.txt\",\"bytes\":12}"
}
```

### `anvil_copy_out`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set",
  "path": "notes/task.txt",
  "encoding": "text"
}
```

동작:

- VM identity를 resolve한다.
- `path`는 `anvil_copy_in`과 같은 validation을 거친다.
- `encoding`은 생략, `text`, `base64`만 허용한다. 생략하면 `text`다.
- `base64` copy-out은 VM에서 읽은 raw bytes를 base64 string으로 반환한다.
- guest agent는 4 MiB를 초과하는 file read를 `413`으로 거부한다.
- `GET /vms/{vm_id}/workspace?path=...`를 호출한다.
- guest agent가 파일을 찾지 못하면 daemon error가 그대로 반환된다.

출력:

```json
{
  "path": "notes/task.txt",
  "content": "file content",
  "encoding": "text"
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
- `session_store_path`가 설정된 경우 alias 제거 후 JSON file에 저장한다.
- daemon delete는 성공했지만 JSON 저장이 실패하면 daemon delete status/body를
  포함한 error를 반환한다. 이때 제거했던 alias는 memory에 복원해 아직 갱신되지
  않은 disk store와 현재 process 상태를 맞춘다. VM을 재생성하지는 않는다.

### `anvil_create_snapshot`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set",
  "tenant_id": "optional-tenant",
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
- `stop_after: true`이고 `session_store_path`가 설정된 경우, VM alias 제거 후
  JSON file에 저장한다.
- `stop_after` snapshot은 성공했지만 JSON 저장이 실패하면 snapshot 성공 context를
  포함한 error를 반환한다. 이때 제거했던 alias는 memory에 복원해 아직 갱신되지
  않은 disk store와 현재 process 상태를 맞춘다.

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
  "session_name": "optional-local-alias",
  "tenant_id": "optional-tenant",
  "egress_policy": "profile"
}
```

동작:

- 빈 `snapshot_id`를 daemon 호출 전에 거부한다.
- `session_name`이 있으면 restore 전에 duplicate 여부를 검사한다.
- `POST /snapshots/{snapshot_id}/restore`를 호출한다.
- restore 성공 후 `session_name -> restored_vm_id`를 bind한다.
- duplicate 사전 검사와 사후 bind 사이의 race로 bind가 실패할 수 있다.
- 사후 bind 실패 시 restored VM은 자동 삭제하지 않는다. error는 restored VM ID와 직접 cleanup 필요성을 포함한다.
- `session_store_path`가 설정된 경우 alias binding 성공 후 JSON file에 저장한다.
- alias binding은 성공했지만 JSON 저장이 실패하면 restored VM ID를 포함한 error를
  반환한다. 이 경우 restored VM은 자동 삭제하지 않는다.
- daemon restore response와 MCP output에는 `agent_token`을 노출하지 않는다.

출력:

```json
{
  "vm_id": "vm-restored",
  "guest_ip": "10.0.1.9",
  "agent_url": "http://192.168.3.73:3000/vms/vm-restored/agent",
  "profile": "dev",
  "tenant_id": "optional-tenant",
  "egress_policy": "profile",
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

`SessionStore`는 기본적으로 in-memory convenience map이다.

```text
session_name -> vm_id
```

`session_store_path` 또는 `ANVIL_MCP_SESSION_STORE`가 설정되면 adapter 시작 시
해당 JSON file을 읽고, alias bind/remove 성공 후 같은 file에 다시 저장한다.
file이 없으면 빈 store로 시작한다. JSON이 손상되어 parse할 수 없으면 adapter
시작을 실패시킨다.

같은 `anvil-mcp` process 안에서는 alias read와 alias mutation+save가 같은 mutex로
직렬화된다. 따라서 tool handler는 bind/remove 저장 transaction 중간 상태를
읽지 않는다.

`session_store_path`는 single-writer 계약이다. 하나의 JSON file은 하나의
`anvil-mcp` process만 써야 한다. 여러 process를 동시에 실행해야 하면 file
locking 또는 catalog design이 추가되기 전까지 서로 다른 `session_store_path`를
사용해야 한다.

영속 file shape는 단순한 object다.

```json
{
  "sessions": {
    "work": "vm-1"
  }
}
```

저장 시 새로 만드는 parent directory는 `0700`, file은 `0600` 권한으로 만든다.
이미 존재하는 parent directory 권한은 이 adapter가 강화하지 않는다.

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
- `session_store_path`가 비어 있으면 `anvil-mcp` process 종료 시 alias는 사라진다.
- `session_store_path`가 설정되어 있으면 process 재시작 뒤에도 alias를 다시 load한다.

## Runtime audit 모델

`ANVIL_MCP_AUDIT_LOG` 또는 `audit_log_path`가 설정되면 adapter는 성공/실패 tool
call마다 JSONL record를 append한다. file은 `0600`으로 만들고 symlink path는
거부한다. `ReadRuntimeAudit`와 `PruneRuntimeAudit` helper는 JSONL 조회와
`keep_last`/`max_age_seconds` 보존 정책 적용을 담당한다.

Record field:

| Field | 의미 |
|---|---|
| `timestamp` | audit event 시각 |
| `tenant_id` | input `tenant_id` 또는 기본 tenant ID |
| `vm_id` | operation 대상 VM ID. 없으면 생략 |
| `session_alias` | input `session_name`. 없으면 생략 |
| `tool_name` | 호출된 MCP tool 이름 |
| `daemon_operation` | 매핑된 daemon API operation |
| `result_code` | `success` 또는 `error` |
| `error` | 실패 시 sanitized error. daemon raw body는 저장하지 않음 |

Runtime audit record에는 snapshot metadata, daemon raw body, secret, `agent_token`을
저장하지 않는다. 이 audit은 MCP boundary의 append-only 기록이며, multi-host
scheduler나 daemon tenant quota enforcement를 대체하지 않는다.
audit log가 설정됐는데 `tenant_id`와 기본 tenant ID가 모두 비어 있으면 adapter는
daemon call 전에 요청을 거부한다.

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
| Guest agent token | MCP output에는 노출하지 않고 daemon proxy가 주입한다. Restore response struct에도 존재하지 않는다. |
| Session alias | 기본값은 process-local memory다. `session_store_path` 설정 시 local JSON file에 `0600`으로 저장한다. |
| Tenant ID | optional `tenant_id` 또는 `ANVIL_MCP_TENANT_ID`를 검증하고 daemon spawn/snapshot/restore body와 audit record에 전달한다. |
| Runtime audit | optional JSONL append-only/read/prune record이며 `agent_token`, daemon raw body, raw metadata를 저장하지 않는다. |
| Secrets | config file은 `api_token`을 담을 수 있으므로 local config를 git에 넣지 않는다. |
| Transport | MCP v1은 client와 adapter 사이에서 stdio를 사용 |

adapter는 daemon URL과 API token을 신뢰된 local/operator configuration으로
가정한다.

## Smoke test

문서 기준 smoke test는 IronClaw 본체 설치를 전제로 하지 않는다. Go MCP SDK
client가 `cmd/anvil-mcp`를 stdio MCP server로 실행하고, 실제 ephemera daemon을
대상으로 tool call을 순서대로 수행한다. 일반 CI에서는 KVM/root가 필요한 daemon
실행을 요구하지 않고, `go test`와 `go build` 같은 build/test 검증만 수행한다.
MCP smoke는 Firecracker를 실행할 수 있는 host에서 반복 실행하는 별도 검증이다.

```bash
sudo ANVIL_API_ADDR=127.0.0.1:3000 ./anvil-daemon
```

다른 터미널에서는 wrapper를 사용한다. wrapper는 먼저
`go build -o /tmp/anvil-mcp ./cmd/anvil-mcp`로 adapter binary를 만든 뒤 smoke
client가 `/tmp/anvil-mcp`를 MCP `CommandTransport`로 실행하게 한다.

```bash
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
```

실행 모드:

| 모드 | 내부 smoke command | 목적 |
|---|---|---|
| `lifecycle` | `go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -expect-output ""` | MCP stdio 연결, tool 목록, VM 생성, workspace copy round-trip, task 호출, health, stop/delete cleanup을 확인한다. `anvil_run_task` 응답 body의 의미적 marker는 검사하지 않는다. |
| `semantic` | `go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -expect-output "anvil-smoke-ok"` | lifecycle 경로에 더해 `anvil_run_task` 응답 body에 `anvil-smoke-ok`가 포함되는지 확인한다. |

공통 검증 순서:

```text
start ephemera daemon
start anvil-mcp through MCP CommandTransport
call anvil_spawn_vm
call anvil_copy_in
call anvil_copy_out
call anvil_run_task
call anvil_get_vm_health
call anvil_stop_vm
call anvil_delete_vm
```

`scripts/anvil-mcp-e2e.sh`의 기본 모드는 `lifecycle`이다. `semantic` 모드와
`scripts/anvil-mcp-smoke.go`의 기본값은 기존처럼 `anvil-smoke-ok` marker를
검사한다. 두 모드 모두 daemon이 이미 실행 중이어야 하며, adapter 설정의
`ANVIL_DAEMON_URL`과 `ANVIL_API_TOKEN`이 daemon에 도달할 수 있어야 한다.
daemon 실행에는 `/dev/kvm`, root 권한, Firecracker 실행 가능 host가 필요하다.
`semantic` 모드는 정상 LLM credential과 provider 응답까지 요구한다.
`lifecycle` 모드는 의미적 marker assertion만 끄지만, 선택한 daemon/profile의
`anvil_run_task` 경로가 2xx로 완료될 수 있는 환경이어야 한다.

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
| spawn 성공 및 alias binding 성공 후 session store 저장 실패 | best-effort VM delete 후 저장 실패 error 반환 |
| restore 성공 후 alias binding 실패 | restored VM은 자동 삭제되지 않는다. error는 restored VM ID를 포함하며, 필요하면 operator/user가 `anvil_delete_vm`으로 명시적으로 정리해야 한다. |
| restore 성공 및 alias binding 성공 후 session store 저장 실패 | restored VM은 자동 삭제되지 않는다. error는 restored VM ID를 포함한다. |
| delete VM 성공 후 session store 저장 실패 | 제거했던 alias를 memory에 복원하고 delete status/body를 포함한 error 반환. VM 재생성은 시도하지 않는다. |
| `stop_after` snapshot 성공 후 session store 저장 실패 | 제거했던 alias를 memory에 복원하고 snapshot 성공 context를 포함한 error 반환. |

## MCP v2 후보 계약

이 작업에서 v2 후보로 고정하는 최소 계약은 session alias 영속화다.

- `session_store_path` 또는 `ANVIL_MCP_SESSION_STORE`로 local JSON file path를
  설정할 수 있다.
- path가 비어 있으면 v1과 같은 in-memory-only alias 동작을 유지한다.
- JSON shape는 `{ "sessions": { "name": "vm-id" } }`로 유지한다.
- `session_store_path`는 single-writer 계약이다. 여러 `anvil-mcp` process가 같은
  file을 동시에 쓰는 동작은 아직 보장하지 않는다.
- MCP v1 tool name과 기존 input field는 안정 계약으로 유지한다. 예를 들어
  `anvil_spawn_vm`, `anvil_run_task`, `vm_id`, `session_name`, `snapshot_id`의
  의미를 v2 후보 작업 때문에 바꾸지 않는다.
- MCP output에는 계속 `agent_token`을 노출하지 않는다.

아래 항목은 아직 설계 후보일 뿐이며, 이 작업에서는 runtime behavior를 구현하지
않는다.

- snapshot alias 또는 snapshot name
- session name으로 최신 snapshot을 찾는 lookup
- snapshot alias 저장을 위한 storage model

HTTP MCP transport도 이 작업 범위 밖이다. v2 후보 논의에서는 다룰 수 있지만,
현재 adapter는 stdio transport만 실행한다.

## 현재 비목표

현재 MCP adapter는 의도적으로 다음을 구현하지 않는다.

- directory sync 또는 archive 기반 workspace copy
- snapshot alias 또는 snapshot name
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
- `scripts/anvil-mcp-e2e.sh`
- `scripts/anvil-mcp-smoke.go`
- `docs/superpowers/specs/2026-05-11-anvil-ironclaw-mcp-v1-design.md`
