# Anvil — Codex project guidance

이 repo는 Firecracker MicroVM 기반 ephemeral AI agent runtime과 Anvil MCP adapter를
관리한다. GitHub repository 이름은 `ephemera`지만 Codex zone의 프로젝트 이름은
`Anvil`로 취급한다.

## Source Of Truth
문서 간 설명이 충돌하면 아래 순서를 따른다.

1. `AGENTS.md`
2. `README.md`
3. `RELEASE_NOTES.md`
4. `docs/`
5. `.superpowers/` 로컬 brainstorming 산출물

Claude Code 호환 `CLAUDE.md`는 현재 운영하지 않는다. 이 repo에서 Codex 기준 문서는
`AGENTS.md`다.

## Project Shape
| 경로 | 역할 |
|---|---|
| `cmd/goose-daemon/` | host control plane daemon |
| `cmd/goose-agent/` | guest VM 안에서 task를 실행하는 agent |
| `cmd/micro-init/` | guest init process |
| `cmd/anvil-mcp/` | stdio MCP adapter entrypoint |
| `internal/anvilmcp/` | MCP config, daemon client, session alias, tool handlers |
| `internal/storage/`, `internal/network/`, `internal/vm/` | VM provisioning support |
| `configs/*.example` | non-secret example configs only |
| `docs/superpowers/` | accepted spec/plan artifacts |

## Workflow
- 한국어로 작업하되 코드, 명령어, API field, file path는 영문을 유지한다.
- 신규 runtime/API/MCP 설계는 `brainstorming -> domain-architecture -> grill-me ->
  writing-plans -> plan-eng-review -> implement -> code-review -> release -> operate`
  흐름을 따른다. 실제 spec/plan은 `docs/superpowers/` 아래에 둔다.
- `.superpowers/`는 로컬 brainstorming runtime 산출물이다. 내부 IP와 임시 server
  metadata가 들어갈 수 있으므로 commit하지 않는다.
- `docs/analysis/`는 분석 보고서 초안이다. release 문서로 승격할 때만 정리해서
  commit한다.
- commit, push, VM 실행, sudo가 필요한 Firecracker smoke는 사용자가 명시적으로
  요청했을 때만 수행한다.

## Commands
기본 검증:

```bash
go test ./...
go vet ./...
go build ./...
```

Anvil MCP adapter만 빠르게 확인:

```bash
go test ./internal/anvilmcp
go build ./cmd/anvil-mcp
```

Firecracker/daemon end-to-end smoke는 KVM, root 권한, host networking 상태에 영향을
줄 수 있으므로 별도 승인 후 실행한다.

## Invariants
- MCP adapter v1은 thin runtime bridge다. workspace copy-in/out, snapshot/restore
  tool, persistent session DB, automatic VM cleanup은 v1 범위 밖이다.
- `anvil_delete_vm`만 VM 삭제와 session alias 해제를 수행한다.
- `anvil_stop_vm`은 graceful stop 요청이며 VM resource 삭제로 해석하지 않는다.
- daemon HTTP status/body는 가능한 한 그대로 보존한다.
- config precedence는 default < config file < environment variable 순서를 유지한다.

## Security
- secrets, API token, SSH key, private config를 commit하지 않는다.
- `configs/anvil-mcp.yaml`, `configs/goose.yaml`, `configs/goose-secrets.yaml`은 로컬
  secret config로 취급한다.
- private IP, host-specific endpoint, local brainstorming server metadata는 문서로
  승격하기 전에 공개 가능성을 검토한다.
- daemon/control-plane token과 guest agent token은 역할을 섞지 않는다.
