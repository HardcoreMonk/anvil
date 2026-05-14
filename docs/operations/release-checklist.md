# anvil 릴리즈 운영 체크리스트

## 목적

이 체크리스트는 같은 저장소에서 관리되는 두 종류의 공개 릴리즈를 구분한다.
`ephemera` runtime 릴리즈는 Firecracker MicroVM 기반 실행 엔진 자체의 source
snapshot을 공개하는 절차이고, `anvil` integration 릴리즈는 IronClaw가
`cmd/anvil-mcp`를 통해 ephemera runtime을 호출하는 통합 표면을 공개하는 절차다.

`ephemera`는 이미 릴리즈된 기반 runtime 이름이다. `anvil`은 IronClaw와
ephemera를 결합하는 통합 프로젝트 이름이다. 릴리즈 제목, tag prefix, GitHub
Release 본문에서 두 이름을 섞어 쓰지 않는다.

## 릴리즈 종류

| 종류 | Tag 예시 | 공개 대상 | 기준 내용 |
|---|---|---|---|
| ephemera runtime | `v0.2.0` | Firecracker MicroVM runtime source snapshot | `cmd/goose-daemon`, `cmd/goose-agent`, `cmd/micro-init`, `internal/storage`, `internal/network`, `internal/vm` |
| anvil integration | `anvil-v0.1.0` | IronClaw 통합 MCP adapter와 운영 계약 | `cmd/anvil-mcp`, `internal/anvilmcp`, workspace copy-in/out, snapshot MCP tools, daemon env alias, IronClaw E2E 검증 |
| anvil runtime foundation | 다음 `anvil-v*` tag | scheduler, network policy, observability foundation | `cmd/anvil-scheduler`, `internal/anvilmcp`, daemon tenant/audit/metrics API, profile egress, optional trace export |

## 게시 전 확인 명령

```bash
git fetch --tags origin
git tag --list 'v*' --sort=version:refname
git tag --list 'anvil-v*' --sort=version:refname
```

### GitHub Release 상태 확인

```bash
gh release view anvil-v0.1.0 --json tagName,targetCommitish,publishedAt,url,isDraft,isPrerelease
```

`anvil-v0.1.0` GitHub Release가 이미 존재하면 release body를 갱신하는 작업도
외부 상태를 바꾸는 작업이다. 따라서 `gh release edit`을 실행하기 전에 tag name,
target commit, release body source를 먼저 보여 주고 사용자의 명시적 승인을
받아야 한다.

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

`anvil-v0.1.0` Release 본문 초안은 이미 게시된 첫 통합 release의 historical body다.
현재 mainline의 scheduler, profile egress, `/metrics/vms`, optional trace export
변경은 [RELEASE_NOTES.md](../../RELEASE_NOTES.md)의 `Unreleased` section을 기준으로
다음 release body에 반영한다.

## `anvil-v0.1.0` GitHub Release 본문 초안

아래 본문은 `anvil-v0.1.0` GitHub Release를 게시할 때 사용하는 초안이다.
게시 전 target commit이 `RELEASE_NOTES.md`와 이 체크리스트를 포함하는지 확인한다.

```markdown
# anvil-v0.1.0 - IronClaw integration over ephemera runtime v0.2.0

`anvil-v0.1.0`은 IronClaw가 ephemera Firecracker runtime을 MCP tool로 호출할 수
있게 하는 첫 통합 릴리즈다. 이 릴리즈는 ephemera runtime `v0.2.0`을 기반으로
하며, VM lifecycle 의미는 ephemera daemon API가 소유하고 anvil은 얇은 stdio MCP
adapter로 그 기능을 IronClaw에 노출한다.

## 포함 내용

- `cmd/anvil-mcp`: IronClaw용 stdio MCP server entrypoint.
- `internal/anvilmcp`: 설정 로더, daemon HTTP client, session alias 저장소,
  MCP tool handler.
- VM lifecycle MCP tools:
  - `anvil_spawn_vm`
  - `anvil_run_task`
  - `anvil_get_vm_health`
  - `anvil_stop_vm`
  - `anvil_delete_vm`
- Workspace copy-in/out MCP tools:
  - `anvil_copy_in`
  - `anvil_copy_out`
  - VM 내부 `/workspace` 기준 단일 file copy를 지원한다.
  - text와 base64 encoding, overwrite 정책, 4 MiB 단일 file 제한을 적용한다.
- Snapshot MCP tools:
  - `anvil_create_snapshot`
  - `anvil_list_snapshots`
  - `anvil_restore_snapshot`
  - `anvil_delete_snapshot`
- daemon 환경 변수 alias:
  - `ANVIL_API_ADDR`, `ANVIL_API_PORT`
  - `ANVIL_API_TOKENS`, `ANVIL_API_TOKEN`
  - `ANVIL_PUBLIC_URL`, `ANVIL_AGENT_PORT`
  - canonical `EPHEMERA_*` 환경 변수와 호환된다.
- MCP adapter 환경 변수:
  - `ANVIL_DAEMON_URL`
  - `ANVIL_API_TOKEN`
  - `ANVIL_MCP_DEFAULT_TIMEOUT`
- 보안 경계:
  - daemon 직접 API의 `POST /vms`만 VM 접근에 필요한 `agent_token`을 반환한다.
  - restore 응답과 MCP output은 `agent_token`을 노출하지 않는다.
  - 외부 client는 daemon proxy와 control-plane token을 기준으로 통합한다.

## 검증

- `go test ./...`
- `go build ./cmd/goose-daemon`
- `go build ./cmd/anvil-mcp`
- `ironclaw mcp test anvil --no-onboard --cli-only`
- 실제 IronClaw agent가 `anvil_spawn_vm`, `anvil_copy_in`,
  `anvil_copy_out`, `anvil_get_vm_health`, `anvil_stop_vm`,
  `anvil_delete_vm`을 호출하는 E2E 검증.

## 운영 주의사항

- `anvil`은 IronClaw 통합 project이고, `ephemera`는 기반 Firecracker runtime이다.
- ephemera runtime release tag는 `v*` 형식을 사용하고, anvil integration release
  tag는 `anvil-v*` 형식을 사용한다.
- 공개 운영은 TLS 종료 reverse proxy 뒤에서 수행한다.
- 운영 모드에서는 `EPHEMERA_API_TOKENS` 또는 `ANVIL_API_TOKENS`를 설정한다.
- 로컬 `configs/goose-secrets.yaml`과 profile별 secret file은 release artifact에
  포함하지 않는다.
```

## 외부 효과 승인

다음 작업은 repository 외부 상태를 바꾸므로 사용자의 명시적 승인을 받은 뒤에만
수행한다.

- Git tag 생성
- Git tag push
- GitHub Release publish

승인 요청 전에는 반드시 다음 값을 먼저 보여 준다.

- tag name
- target commit
- release body source: `docs/operations/release-checklist.md` 안의 fenced
  `anvil-v0.1.0` GitHub Release 본문 초안 section

`gh release --notes-file`에는 전체 체크리스트 파일을 넘기지 않는다. 게시 전 fenced
draft만 별도의 검토된 notes file로 추출한 뒤 그 파일을 사용한다.

승인 없이 tag를 만들거나, tag를 push하거나, GitHub Release를 게시하지 않는다.
