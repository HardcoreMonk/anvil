# anvil MCP snapshot/restore tools v1 설계

작성일: 2026-05-11

## 목적

anvil MCP adapter에 ephemera daemon의 snapshot lifecycle API를 얇게 노출한다.
IronClaw는 MCP를 통해 실행 중인 VM의 snapshot을 만들고, snapshot 목록을 보고,
snapshot에서 새 VM을 restore하고, 더 이상 필요 없는 snapshot을 삭제할 수 있어야
한다.

이 설계는 기존 MCP v1 원칙을 유지한다. adapter는 ephemera daemon API 의미를
재해석하지 않고, daemon status/body와 VM lifecycle의 소유권을 보존한다.

## 범위

포함:

- `anvil_create_snapshot`
- `anvil_list_snapshots`
- `anvil_restore_snapshot`
- `anvil_delete_snapshot`
- restore 성공 후 optional `session_name -> restored_vm_id` bind
- MCP output에서 `agent_token` 제거
- README와 MCP architecture 문서 갱신
- unit test 중심의 검증

제외:

- snapshot alias 또는 `snapshot_name`
- session name으로 최신 snapshot 자동 선택
- workspace copy-in/copy-out
- persistent session database
- HTTP MCP transport
- daemon API 변경
- restore alias bind 실패 시 adapter의 자동 VM 삭제

## 도구 계약

### `anvil_create_snapshot`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set",
  "stop_after": false,
  "type": "full | diff | optional"
}
```

동작:

- `vm_id`가 있으면 우선 사용한다.
- `vm_id`가 없으면 `session_name`을 기존 VM session alias로 resolve한다.
- `session_name`을 알 수 없으면 daemon 호출 전에 validation error를 반환한다.
- `stop_after` 기본값은 `false`다.
- `type`은 생략, 빈 문자열, `full`, `diff`만 허용한다.
- daemon `POST /vms/{vm_id}/snapshot`을 호출한다.

출력:

```json
{
  "snapshot_id": "snap-...",
  "source_vm_id": "vm-...",
  "profile": "optional-profile",
  "snapshot_type": "full",
  "base_snapshot_id": "optional-base-snapshot",
  "created_at": "2026-05-11T00:00:00Z"
}
```

### `anvil_list_snapshots`

입력:

```json
{}
```

동작:

- daemon `GET /snapshots`를 호출한다.
- snapshot alias나 filtering은 v1에서 제공하지 않는다.

출력:

```json
{
  "snapshots": [
    {
      "snapshot_id": "snap-...",
      "source_vm_id": "vm-...",
      "profile": "optional-profile",
      "snapshot_type": "full",
      "base_snapshot_id": "optional-base-snapshot",
      "created_at": "2026-05-11T00:00:00Z"
    }
  ]
}
```

### `anvil_restore_snapshot`

입력:

```json
{
  "snapshot_id": "snap-...",
  "session_name": "optional-local-alias"
}
```

동작:

- `snapshot_id`는 필수다.
- `session_name`이 있으면 restore 전 duplicate 여부를 빠르게 검사한다.
- daemon `POST /snapshots/{snapshot_id}/restore`를 호출한다.
- restore 성공 후 `session_name`이 있으면 `session_name -> restored_vm_id`를
  bind한다.
- duplicate 사전 검사와 사후 bind 사이에는 race가 있을 수 있으므로 bind 실패를
  별도 실패 모드로 처리한다.
- restore 후 bind 실패 시 VM은 자동 삭제하지 않는다. error에는
  `restored_vm_id`를 포함해 사용자가 `anvil_delete_vm`으로 명시 cleanup할 수
  있게 한다.
- daemon 응답에 포함된 `agent_token`은 MCP output에서 제거한다.

출력:

```json
{
  "vm_id": "vm-...",
  "guest_ip": "10.0.1.x",
  "agent_url": "http://...",
  "profile": "optional-profile",
  "source_snapshot_id": "snap-...",
  "session_name": "optional-local-alias"
}
```

### `anvil_delete_snapshot`

입력:

```json
{
  "snapshot_id": "snap-..."
}
```

동작:

- `snapshot_id`는 필수다.
- daemon `DELETE /snapshots/{snapshot_id}`를 호출한다.
- full snapshot이 diff snapshot의 base라서 daemon이 `409`를 반환하면 adapter는
  status/body를 보존한 `DaemonError`로 전달한다.

출력:

```json
{
  "status_code": 200,
  "body": "{\"snapshot_id\":\"snap-...\",\"status\":\"deleted\"}"
}
```

## 내부 구조

### Daemon client

`internal/anvilmcp/daemon_client.go`에 다음 daemon method를 추가한다.

- `CreateSnapshot(ctx, vmID, req) (*SnapshotInfo, error)`
- `ListSnapshots(ctx) ([]SnapshotInfo, error)`
- `RestoreSnapshot(ctx, snapshotID) (*RestoreSnapshotResponse, error)`
- `DeleteSnapshot(ctx, snapshotID) (*RawDaemonResponse, error)`

`SnapshotInfo`는 daemon의 public snapshot response와 같은 field를 가진다.
`RestoreSnapshotResponse`는 daemon의 restore response를 decode하기 위해
`agent_token` field를 포함할 수 있지만, MCP output struct에는 포함하지 않는다.

### Tool layer

`internal/anvilmcp/tools.go`에 다음 handler를 추가한다.

- `MCPCreateSnapshot`
- `MCPListSnapshots`
- `MCPRestoreSnapshot`
- `MCPDeleteSnapshot`

기존 VM identity resolution은 그대로 재사용한다. snapshot identity는
`snapshot_id`만 사용한다.

restore alias bind race를 테스트할 수 있도록 `Tools` 내부의 session dependency는
작은 interface로 분리한다. 기본 구현은 기존 `SessionStore`를 사용한다.
필요한 interface method는 다음 범위로 제한한다.

- `Exists(sessionName string) bool`
- `Bind(sessionName, vmID string) error`
- `ResolveIdentity(vmID, sessionName string) (string, error)`
- `RemoveVM(vmID string)`

### Entrypoint

`cmd/anvil-mcp/main.go`는 새 tool 4개를 등록한다. 기존 5개 tool 이름과 동작은
변경하지 않는다.

## 오류 처리

- unknown `session_name`: daemon 호출 전 validation error
- invalid snapshot `type`: daemon 호출 전 validation error
- empty `snapshot_id`: daemon 호출 전 validation error
- daemon 4xx/5xx: 기존 `DaemonError`처럼 status/body 보존
- duplicate restore `session_name`: 가능하면 restore 전 validation error
- restore 후 alias bind 실패: error에 `restored_vm_id` 포함, 자동 VM 삭제 없음

restore 후 alias bind 실패 error message는 다음 정보를 포함해야 한다.

```text
failed to bind session "name" to restored VM "vm-..."; restored VM was not deleted
```

## 보안

- MCP output에는 `agent_token`을 노출하지 않는다.
- IronClaw는 control-plane token만 다루고, guest agent token 사용은 ephemera
  daemon proxy가 소유한다.
- snapshot metadata에는 token이 포함될 수 있으므로, MCP layer는 metadata file을
  직접 읽지 않는다.

## 테스트 전략

TDD로 구현한다.

Daemon client tests:

- `CreateSnapshot`이 `POST /vms/{id}/snapshot`에 `stop_after`, `type` JSON을 보낸다.
- `ListSnapshots`가 `GET /snapshots` 응답을 decode한다.
- `RestoreSnapshot`이 daemon response의 `agent_token`을 decode할 수 있다.
- `DeleteSnapshot`이 `DELETE /snapshots/{id}`를 호출하고 non-2xx body를 보존한다.

Tool tests:

- `MCPCreateSnapshot`이 `session_name`을 `vm_id`로 resolve해 daemon을 호출한다.
- `MCPCreateSnapshot`이 invalid `type`을 daemon 호출 전에 거부한다.
- `MCPListSnapshots`가 snapshot 배열을 wrapper object로 반환한다.
- `MCPRestoreSnapshot`이 restore 성공 후 `session_name -> restored_vm_id`를 bind한다.
- `MCPRestoreSnapshot`이 duplicate `session_name`을 restore 전 거부한다.
- `MCPRestoreSnapshot`의 사후 bind 실패 error가 `restored_vm_id`를 포함하고
  daemon delete를 호출하지 않는다.
- `MCPRestoreSnapshot` output에 `agent_token`이 없다.
- `MCPDeleteSnapshot`이 daemon `409`를 그대로 error로 전달한다.

검증 명령:

```bash
go test ./internal/anvilmcp
go test ./cmd/anvil-mcp
go test ./...
```

## 수용 기준

- 새 MCP tool 4개가 `cmd/anvil-mcp`에 등록된다.
- 기존 5개 MCP tool 동작은 유지된다.
- MCP output에 `agent_token`이 노출되지 않는다.
- snapshot alias는 도입하지 않는다.
- restore alias bind 실패 시 restored VM을 자동 삭제하지 않고 error에 VM ID를
  포함한다.
- README와 `docs/architecture/mcp-architecture.md`가 새 tool과 제한사항을 설명한다.
- `go test ./...`가 통과한다.
