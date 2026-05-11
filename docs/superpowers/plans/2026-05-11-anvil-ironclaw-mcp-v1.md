# anvil IronClaw MCP v1 구현 계획

> agentic worker용 지침: 원본 계획은 task-by-task 실행과 체크박스 tracking을
> 전제로 작성되었다. 현재 문서는 구현 완료 후 한국어 보존 기록으로 재작성되어
> 핵심 범위, 파일 구조, 검증 기준, 완료 evidence를 남긴다.

## 목표

`cmd/anvil-mcp`를 구현한다. 이 binary는 IronClaw가 ephemera 0.2.0 daemon API를
통해 VM 생성, task 실행, health 조회, agent stop, VM 삭제를 수행할 수 있게 하는
Go stdio MCP server다.

## 아키텍처

`anvil-mcp`는 얇은 runtime bridge다.

- IronClaw는 stdio MCP로 Go binary와 통신한다.
- adapter는 config를 load한다.
- optional in-memory `session_name -> vm_id` map을 유지한다.
- ephemera daemon에는 HTTP JSON으로 요청한다.
- v1은 workspace copy-in/out, snapshot tool, persistent session, HTTP MCP
  transport, quota, automatic cleanup을 의도적으로 제외한다.

## 기술 스택

- Go
- `github.com/modelcontextprotocol/go-sdk/mcp`
- standard `net/http`
- `gopkg.in/yaml.v2`
- ephemera daemon 0.2.0 HTTP API
- Go `testing`
- `net/http/httptest`

## 소스 참조

- Spec: `docs/superpowers/specs/2026-05-11-anvil-ironclaw-mcp-v1-design.md`
- daemon API: `cmd/goose-daemon/api.go`
- config pattern: `cmd/goose-daemon/config.go`, `cmd/goose-daemon/config_test.go`
- 공식 MCP Go SDK docs: `https://go.sdk.modelcontextprotocol.io/`
- 공식 MCP Go SDK repository: `https://github.com/modelcontextprotocol/go-sdk`

## 범위 점검

이 계획은 anvil-owned MCP adapter 하나만 다룬다. 다음은 구현하지 않는다.

- IronClaw-side code
- workspace sync
- snapshot tool
- restore tool
- persistent session storage
- daemon API 변경

위 항목은 별도 future spec에서 다룬다.

## 파일 구조

생성:

- `cmd/anvil-mcp/main.go`: binary entrypoint. config load, daemon client/session
  store/tool server 생성, MCP tool 등록, stdio transport 실행.
- `internal/anvilmcp/config.go`: config default, YAML loading, environment override,
  URL validation, timeout parsing.
- `internal/anvilmcp/config_test.go`: config precedence와 validation test.
- `internal/anvilmcp/session_store.go`: concurrency-safe optional
  `session_name -> vm_id` alias map.
- `internal/anvilmcp/session_store_test.go`: bind, duplicate, resolve, remove test.
- `internal/anvilmcp/daemon_client.go`: ephemera daemon 0.2.0 endpoint용 작은 HTTP client.
- `internal/anvilmcp/daemon_client_test.go`: request shape, auth header, daemon error
  preservation, timeout context propagation test.
- `internal/anvilmcp/tools.go`: MCP input/output struct와 tool handler.
- `internal/anvilmcp/tools_test.go`: tool handler validation, session resolution,
  timeout behavior, delete mapping cleanup test.
- `configs/anvil-mcp.yaml.example`: adapter config 예시.

수정:

- `go.mod`: Go version 상향 및 공식 MCP Go SDK 추가.
- `go.sum`: `go mod tidy` 결과 반영.
- `README.md`: `anvil-mcp` build/config/use section 및 최소 Go version 갱신.
- `RELEASE_NOTES.md`: 미릴리즈 MCP adapter 추가 내용 기록.

수정 금지:

- `cmd/goose-daemon/api.go`
- `cmd/goose-agent/main.go`
- snapshot/restore code

## 작업 계획

### 작업 1. Toolchain과 MCP SDK dependency

- local Go toolchain이 MCP SDK 요구사항을 만족하는지 확인한다.
- `go.mod`의 Go version을 1.25 이상으로 맞춘다.
- `github.com/modelcontextprotocol/go-sdk`를 추가한다.
- 기존 project build/test가 깨지지 않는지 확인한다.
- README prerequisite에 Go 1.25+ 요구사항을 반영한다.

검증:

```bash
go version
go list -m -json github.com/modelcontextprotocol/go-sdk@v1.6.0
go test ./...
```

### 작업 2. Config loader

- default daemon URL은 `http://127.0.0.1:3000`.
- default timeout은 `300` seconds.
- `configs/anvil-mcp.yaml`을 optional config file로 지원한다.
- `ANVIL_MCP_CONFIG`, `ANVIL_DAEMON_URL`, `ANVIL_API_TOKEN`,
  `ANVIL_MCP_DEFAULT_TIMEOUT` 환경 변수를 지원한다.
- 환경 변수는 config file보다 우선한다.
- URL scheme/host와 positive timeout을 validation한다.

검증:

```bash
go test ./internal/anvilmcp -run TestLoadConfig
```

### 작업 3. Session store

- process-local `session_name -> vm_id` map을 구현한다.
- empty session name과 empty VM ID를 거부한다.
- duplicate session name을 거부한다.
- `vm_id` 우선 resolution을 구현한다.
- delete 성공 시 해당 VM을 가리키는 alias를 제거한다.

검증:

```bash
go test ./internal/anvilmcp -run TestSessionStore
```

### 작업 4. Daemon HTTP client

- daemon base URL과 path를 결합해 request를 만든다.
- `ANVIL_API_TOKEN`이 있으면 Bearer token을 주입한다.
- JSON request에는 `Content-Type: application/json`을 설정한다.
- daemon response status/body를 보존한다.
- non-2xx response는 status/body가 있는 `DaemonError`로 반환한다.
- MCP call context와 timeout을 request에 전달한다.

검증:

```bash
go test ./internal/anvilmcp -run TestDaemonClient
```

### 작업 5. MCP tool handler

- `anvil_spawn_vm`
- `anvil_run_task`
- `anvil_get_vm_health`
- `anvil_stop_vm`
- `anvil_delete_vm`

각 handler는 input validation, VM identity resolution, daemon client call,
response mapping을 담당한다. `anvil_stop_vm`은 alias를 제거하지 않고,
`anvil_delete_vm`은 성공 후 alias를 제거한다.

검증:

```bash
go test ./internal/anvilmcp -run TestTools
```

### 작업 6. MCP stdio server entrypoint

- `cmd/anvil-mcp/main.go`에서 config를 load한다.
- daemon client, session store, tool handler를 생성한다.
- 공식 MCP Go SDK로 tool을 등록한다.
- stdio transport를 실행한다.
- startup failure는 stderr와 non-zero exit로 처리한다.

검증:

```bash
go build ./cmd/anvil-mcp
```

### 작업 7. Config example과 README 사용법

- `configs/anvil-mcp.yaml.example`을 추가한다.
- README에 build, environment variable, config file, MCP tool 목록을 추가한다.
- v1 비목표를 문서화한다.

검증:

```bash
test -s configs/anvil-mcp.yaml.example
rg -n "anvil-mcp|ANVIL_DAEMON_URL|anvil_spawn_vm" README.md
```

### 작업 8. 최종 검증과 release note

- `RELEASE_NOTES.md` 미릴리즈 section에 MCP adapter 추가를 기록한다.
- 전체 Go test를 실행한다.
- daemon과 adapter build를 확인한다.
- manual smoke test 절차를 남긴다.

검증:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
git diff --check
```

## 수동 smoke test

real Firecracker 환경에서 다음 순서로 확인한다.

```text
start ephemera daemon
start anvil-mcp
call anvil_spawn_vm
call anvil_run_task
call anvil_get_vm_health
call anvil_stop_vm
call anvil_delete_vm
```

이 smoke test는 `/dev/kvm`, root 권한, 로컬 LLM API key가 필요하므로 일반 CI의
unit test와 분리한다.

## 완료 기준

- `cmd/anvil-mcp`가 build된다.
- `internal/anvilmcp` unit test가 통과한다.
- daemon API 또는 snapshot/restore runtime code를 변경하지 않는다.
- MCP output에 guest `agent_token`을 노출하지 않는다.
- stop과 delete semantics가 구분된다.
- README와 release note가 MCP v1 범위와 비목표를 설명한다.

## 자체 검토 checklist

- config precedence가 default -> config file -> environment override 순서인가: 예.
- session alias가 persistent state로 오해되지 않게 문서화했는가: 예.
- daemon error status/body를 보존하는가: 예.
- timeout 발생 시 VM을 자동 삭제하지 않는가: 예.
- MCP v1 비목표가 README와 설계서에 남아 있는가: 예.
- full test/build 검증이 가능한 명령으로 남아 있는가: 예.
