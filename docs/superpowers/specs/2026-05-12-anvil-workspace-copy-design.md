# Anvil Workspace Copy Design

## 목표

anvil MCP v2의 첫 기능으로 실행 중인 ephemera VM과 IronClaw 쪽 작업 공간 사이에 단일 파일을 넣고 빼는 `workspace copy-in/copy-out` 흐름을 추가한다.

## 범위

- VM 내부 기준 workspace root는 `/workspace`이다.
- v1 구현은 단일 파일만 다룬다.
- directory sync, tar archive, binary chunk streaming, checksum negotiation, snapshot alias, HTTP MCP transport는 이번 범위에서 제외한다.

## HTTP 계약

daemon은 기존 VM proxy 패턴을 확장해 다음 endpoint를 제공한다.

- `PUT /vms/{vm_id}/workspace?path=<relative-path>`
- `GET /vms/{vm_id}/workspace?path=<relative-path>`

daemon은 VM 존재 여부와 agent token 주입만 담당한다. 실제 파일 path 검증과 read/write는 guest `goose-agent`가 수행한다.

## Guest Agent 계약

`goose-agent`는 `/workspace` endpoint를 추가한다.

- `PUT`은 request body를 `/workspace/<path>`에 저장한다.
- `GET`은 `/workspace/<path>` 내용을 `application/octet-stream`으로 반환한다.
- parent directory는 `PUT` 시 자동 생성한다.
- `path`가 비어 있거나 절대경로이거나 `..`을 포함하거나 clean 결과가 `.`이면 `400`을 반환한다.
- file이 없으면 `404`를 반환한다.

## MCP 계약

MCP tool은 두 개를 추가한다.

- `anvil_copy_in`
  - 입력: `vm_id` 또는 `session_name`, `path`, `content`
  - 출력: daemon raw response (`status_code`, `body`)
- `anvil_copy_out`
  - 입력: `vm_id` 또는 `session_name`, `path`
  - 출력: `path`, `content`

`path`는 VM `/workspace` 내부 상대경로이다. `content`는 v1에서 UTF-8 text로 취급한다.

## 보안

- 외부 caller는 daemon token만 사용한다.
- daemon은 기존 agent proxy처럼 per-VM agent token을 주입한다.
- guest agent가 path traversal을 거부해 VM 내부 `/workspace` 밖으로 나가지 못하게 한다.
- MCP layer도 빈 path와 traversal path를 사전 거부해 빠른 feedback을 제공한다.

## 테스트 전략

- `cmd/goose-agent` unit test: path 검증, PUT/GET round trip, traversal reject, missing file `404`.
- `internal/anvilmcp` unit test: session name resolve, daemon client URL/query/body 계약, invalid path reject.
- `cmd/anvil-mcp` unit test: tool registry에 `anvil_copy_in`, `anvil_copy_out` 포함.
- smoke script 확장: copy-in으로 파일을 넣고, copy-out으로 같은 내용을 회수한다.
