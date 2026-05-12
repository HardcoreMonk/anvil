# IronClaw 본체 연동 점검

## 결론

초기 점검 시점에는 IronClaw 실행 파일 또는 source checkout이 확인되지 않아
IronClaw 본체 기준 검증을 진행할 수 없었다. 이후 로컬에 IronClaw `0.28.1`을
설치했고, IronClaw CLI에서 `anvil` stdio MCP server 등록과 `mcp test` 기반
연결/도구 목록 조회까지 확인했다.

아직 완료되지 않은 범위는 IronClaw agent가 실제 LLM workflow 안에서
`anvil_spawn_vm`, `anvil_run_task` 같은 tool을 선택해 호출하는 end-to-end 검증이다.
이 검증에는 `ironclaw onboard`를 통한 NEAR AI session 또는 다른 LLM provider
설정이 필요하다.

## 확인 일시

- 날짜: 2026-05-12
- 작업 디렉터리: `/data/projects/codex-zone/ephemera`

## 확인 명령

```bash
command -v ironclaw || true
find /data/projects -maxdepth 4 -iname '*ironclaw*' 2>/dev/null | head -n 80
find /home/hardcoremonk -maxdepth 4 -iname '*ironclaw*' 2>/dev/null | head -n 80
```

## 결과

- 초기 점검 결과:
  - `ironclaw` 실행 파일: 없음
  - `/data/projects` 아래 IronClaw project checkout: 없음
  - `/home/hardcoremonk` 아래 IronClaw project checkout: 없음
- 검색된 파일은 debate 상태 파일뿐이며 실행 가능한 IronClaw 본체가 아니다.

## 설치 결과

- 설치 일시: 2026-05-12
- 설치 버전: `ironclaw 0.28.1`
- 설치 위치:
  - `/home/hardcoremonk/.local/bin/ironclaw`
  - `/home/hardcoremonk/.local/bin/sandbox_daemon`
- 설치 방식:
  - GitHub release `ironclaw-v0.28.1`
  - `ironclaw-x86_64-unknown-linux-gnu.tar.gz` 다운로드
  - `.sha256` checksum 검증 후 `~/.local/bin`에 수동 설치
- PostgreSQL 구성:
  - Ubuntu package `postgresql-16`, `postgresql-16-pgvector`
  - database: `ironclaw`
  - owner role: `hardcoremonk`
  - extension: `vector`
- IronClaw bootstrap env:
  - `/home/hardcoremonk/.ironclaw/.env`
  - `DATABASE_URL`은 local PostgreSQL Unix socket을 사용한다.
  - `DATABASE_SSLMODE=disable`
  - `SECRETS_MASTER_KEY`는 IronClaw가 자동 생성했으며 문서에 값을 기록하지 않는다.
- IronClaw MCP server 등록:
  - name: `anvil`
  - transport: `stdio`
  - command: `/data/projects/codex-zone/ephemera/anvil-mcp`
  - env: `ANVIL_DAEMON_URL=http://127.0.0.1:3000`

## IronClaw 본체 검증 결과

검증 명령:

```bash
ironclaw --version
ironclaw doctor --no-onboard --cli-only
ironclaw mcp list --verbose --no-onboard --cli-only
ironclaw mcp test anvil --no-onboard --cli-only
```

결과:

- `ironclaw --version`: `ironclaw 0.28.1`
- `ironclaw doctor`: PostgreSQL 연결, MCP server config, secrets backing store 확인
- `ironclaw mcp list`: `anvil` stdio MCP server 등록 확인
- `ironclaw mcp test anvil`: 연결 성공, `anvil_*` tool 11개 조회 성공

## 검증 완료 범위

- `cmd/anvil-mcp` stdio MCP adapter build 가능
- Go MCP SDK smoke client가 `cmd/anvil-mcp`를 실행해 실제 daemon과 tool call 수행 가능
- 검증된 tool flow:
  - `anvil_spawn_vm`
  - `anvil_copy_in`
  - `anvil_copy_out`
  - `anvil_run_task`
  - `anvil_get_vm_health`
  - `anvil_stop_vm`
  - `anvil_delete_vm`

## Blocker

남은 blocker는 IronClaw 본체 설치가 아니라 interactive/onboarded runtime 상태다.

- NEAR AI session file이 없음: `/home/hardcoremonk/.ironclaw/session.json`
- `ironclaw onboard` 또는 대체 LLM provider 설정이 아직 완료되지 않았다.
- 따라서 IronClaw agent가 자연어 요청을 받아 실제 `anvil_*` MCP tool을 선택하고
  호출하는 end-to-end 검증은 아직 미완료다.

## 재개 조건

IronClaw onboarding 또는 LLM provider 설정이 완료되면 다음 순서로 재검증한다.

1. root 권한으로 `anvil-daemon`을 실행한다.
2. IronClaw에서 `anvil_spawn_vm`, `anvil_copy_in`, `anvil_run_task`,
   `anvil_copy_out`, `anvil_delete_vm`을 호출한다.
3. tool response와 daemon log를 함께 보관한다.
