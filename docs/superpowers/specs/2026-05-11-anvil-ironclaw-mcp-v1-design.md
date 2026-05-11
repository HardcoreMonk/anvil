# anvil IronClaw MCP v1 설계

작성일: 2026-05-11
기준 프로젝트: `anvil`
기준 저장소: `https://github.com/HardcoreMonk/ephemera/`
기준 릴리즈: `0.2.0`, commit `abcaa86`

## 1. 목적

ephemera 0.2.0은 단일 host에서 VM 생성, task 실행, agent proxy, VM별 token,
snapshot/restore 기반을 제공한다. IronClaw 통합 v1의 목표는 이 runtime
capability를 MCP tool로 노출해 IronClaw가 anvil VM lifecycle과 task 실행을
사용할 수 있게 하는 것이다.

v1은 **thin runtime bridge**로 설계한다. MCP adapter는 ephemera daemon API를 얇게
감싸고, IronClaw의 workspace/session 제품 의미를 과하게 선점하지 않는다.

## 2. 결정 사항

- 위치: anvil repository 내부 `cmd/anvil-mcp`
- 구현 언어: Go
- MCP transport: v1은 stdio
- daemon 연결: local과 remote 모두 지원
- 기본 daemon URL: `http://127.0.0.1:3000`
- 상태 관리: stateless 기본, optional `session_name -> vm_id` 메모리 mapping
- task 실행: 동기식, `timeout_seconds` 지원
- cleanup: 명시적 `anvil_delete_vm` 호출만 VM 삭제
- profile: `anvil_spawn_vm(profile)`에서만 지정
- error 처리: daemon HTTP status/body를 최대한 보존
- workspace copy-in/out: v1 제외, v2 설계 경계로 명시
- snapshot tool: v1 제외, v2 후보
- HTTP MCP transport: v1 제외, v2 후보

## 3. 비목표

v1은 다음을 구현하지 않는다.

- IronClaw workspace file을 VM으로 copy-in/copy-out
- snapshot, restore, checkpoint tool
- persistent session database
- 자동 VM cleanup
- VM quota, user policy, audit policy
- daemon error를 adapter 고유 taxonomy로 재해석
- MCP HTTP transport
- ephemera daemon API 변경

## 4. 아키텍처

```text
IronClaw
  MCP client
    |
    | stdio MCP
    v
cmd/anvil-mcp
  Go binary
  MCP tool adapter
  config loader
  in-memory session map
  daemon HTTP client
    |
    | HTTP JSON
    v
ephemera daemon 0.2.0
  VM lifecycle
  agent proxy
  task execution
```

### 4.1 책임 경계

`anvil-mcp`가 소유하는 것:

- MCP tool schema
- stdio MCP request/response 처리
- daemon URL/token 설정 로딩
- ephemera daemon HTTP 호출
- optional `session_name -> vm_id` 메모리 mapping
- synchronous task timeout
- adapter 수준 input validation

`anvil-mcp`가 소유하지 않는 것:

- workspace persistence
- file transfer protocol
- snapshot lifecycle
- VM scheduling
- quota, policy, audit
- daemon 내부 VM lifecycle semantics
- daemon error 의미 재정의

## 5. 도구 계약

| Tool | 역할 |
|---|---|
| `anvil_spawn_vm` | VM을 생성하고 optional `session_name` alias를 저장한다. |
| `anvil_run_task` | `vm_id` 또는 `session_name`으로 VM task를 실행한다. |
| `anvil_get_vm_health` | VM agent health를 조회한다. |
| `anvil_stop_vm` | VM agent에 graceful stop을 요청한다. |
| `anvil_delete_vm` | VM을 삭제하고 연결된 session mapping을 제거한다. |

### 5.1 `anvil_spawn_vm`

입력:

```json
{
  "profile": "optional string",
  "session_name": "optional string"
}
```

동작:

- ephemera daemon `POST /vms` 호출.
- `profile`이 있으면 daemon request에 전달.
- `session_name`이 있으면 adapter memory에 `session_name -> vm_id` 저장.
- 같은 `session_name`이 이미 있으면 validation error 반환.
- MCP 응답에는 `agent_token`을 포함하지 않는다.

출력:

```json
{
  "vm_id": "string",
  "guest_ip": "string",
  "agent_url": "string",
  "profile": "optional string",
  "session_name": "optional string"
}
```

### 5.2 `anvil_run_task`

입력:

```json
{
  "vm_id": "optional string",
  "session_name": "optional string",
  "prompt": "string",
  "timeout_seconds": "optional integer"
}
```

동작:

- `vm_id`가 있으면 우선 사용.
- `vm_id`가 없으면 `session_name`으로 lookup.
- 둘 다 없으면 validation error.
- `prompt`가 비어 있으면 validation error.
- daemon proxy endpoint `POST /vms/{vm_id}/tasks` 호출.
- 호출은 동기식.
- timeout에 도달하면 adapter timeout error 반환.

출력은 daemon task response body를 그대로 반환한다.

### 5.3 `anvil_get_vm_health`

- `vm_id`가 있으면 우선 사용하고, 없으면 `session_name`으로 lookup한다.
- daemon proxy endpoint `GET /vms/{vm_id}/health`를 호출한다.
- daemon health response body를 그대로 반환한다.

### 5.4 `anvil_stop_vm`

- `vm_id`가 있으면 우선 사용하고, 없으면 `session_name`으로 lookup한다.
- daemon proxy endpoint `POST /vms/{vm_id}/stop`을 호출한다.
- VM 삭제와 session mapping 삭제는 수행하지 않는다.
- daemon stop response body를 그대로 반환한다.

### 5.5 `anvil_delete_vm`

- `vm_id`가 있으면 우선 사용하고, 없으면 `session_name`으로 lookup한다.
- daemon endpoint `DELETE /vms/{vm_id}`를 호출한다.
- 성공 시 해당 `vm_id`와 연결된 session mapping을 제거한다.
- daemon delete response body를 그대로 반환한다.

## 6. 설정

설정 우선순위:

1. 환경 변수
2. config file
3. 기본값

환경 변수:

- `ANVIL_DAEMON_URL`: ephemera daemon base URL
- `ANVIL_API_TOKEN`: daemon Bearer token
- `ANVIL_MCP_DEFAULT_TIMEOUT`: 기본 task timeout, seconds
- `ANVIL_MCP_CONFIG`: config file path

기본값:

- daemon URL: `http://127.0.0.1:3000`
- default timeout: `300`
- API token: empty

Config file shape:

```yaml
daemon_url: http://127.0.0.1:3000
api_token: ""
default_timeout_seconds: 300
```

환경 변수 값이 있으면 config file 값을 덮어쓴다.

## 7. 상태 관리

v1 adapter는 persistent database를 갖지 않는다.

- 상태는 process memory의 `session_name -> vm_id` map뿐이다.
- adapter 재시작 시 session alias는 손실된다.
- tool input에 `vm_id`가 있으면 항상 `vm_id`가 우선한다.
- `session_name`은 편의 alias이며 권위 있는 runtime identity가 아니다.
- adapter 재시작 후에도 IronClaw가 `vm_id`를 알고 있으면 VM을 계속 제어할 수 있다.
- delete 성공 시 관련 mapping을 제거한다.
- stop 성공만으로 mapping을 제거하지 않는다.

## 8. 오류 처리

Daemon error:

- HTTP status code를 보존한다.
- response body를 MCP error message 또는 details에 포함한다.
- adapter가 retryable 여부를 임의로 판단하지 않는다.

Adapter error:

- invalid config
- daemon URL parse failure
- daemon unreachable
- task timeout
- invalid MCP input
- unknown `session_name`
- duplicate `session_name`

Timeout:

- `timeout_seconds`가 있으면 해당 값을 사용한다.
- 없으면 `ANVIL_MCP_DEFAULT_TIMEOUT` 또는 config/default 값을 사용한다.
- timeout은 adapter error로 반환한다.
- timeout 발생 시 v1 adapter는 VM을 자동 stop/delete하지 않는다.

## 9. 보안

- daemon API token은 config 또는 환경 변수에서 읽고 outbound HTTP request의
  bearer token으로 사용한다.
- token은 MCP response에 포함하지 않는다.
- token은 log에 출력하지 않는다.
- `session_name`은 인증 경계가 아니라 단순 alias다.
- remote daemon 연결 시 TLS는 배포 환경의 URL 구성에 맡긴다. v1 adapter는
  HTTPS URL을 그대로 지원한다.

## 10. 테스트 전략

Unit test:

- config loading precedence
- daemon URL parsing
- default timeout parsing
- session map insert, lookup, duplicate, delete
- VM identity resolution, `vm_id` priority
- daemon client request shape
- daemon error preservation with `httptest`
- task timeout
- MCP tool input validation

Smoke test:

```text
start ephemera daemon
start anvil-mcp
call anvil_spawn_vm
call anvil_run_task
call anvil_get_vm_health
call anvil_stop_vm
call anvil_delete_vm
```

v1 smoke test는 real Firecracker 환경이 필요하므로 일반 CI unit test와 분리한다.

## 11. v2 확장 후보

- workspace copy-in/copy-out
- session persistence
- snapshot/checkpoint tool
- restore session tool
- HTTP MCP transport
- one-shot ephemeral task tool
- structured adapter error code
- audit event emission
- IronClaw workspace metadata binding

## 12. 승인된 설계 요약

v1은 ephemera daemon 0.2.0 API를 Go 기반 MCP stdio server로 얇게 감싸는 통합이다.
IronClaw는 이 adapter를 실행해 VM 생성, task 실행, health 조회, stop, delete를
수행한다. adapter는 optional `session_name` alias를 제공하지만 VM lifecycle
의미를 숨기지 않는다. workspace file movement와 snapshot/session persistence는
v2 설계 영역으로 남긴다.
