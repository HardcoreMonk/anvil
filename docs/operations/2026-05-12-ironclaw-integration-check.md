# IronClaw 본체 연동 점검

## 결론

초기 점검 시점에는 IronClaw 실행 파일 또는 source checkout이 확인되지 않아
IronClaw 본체 기준 검증을 진행할 수 없었다. 이후 로컬에 IronClaw `0.28.1`을
설치했고, IronClaw CLI에서 `anvil` stdio MCP server 등록과 `mcp test` 기반
연결/도구 목록 조회까지 확인했다.

이후 `anvil-daemon`을 실행한 상태에서 IronClaw agent가 실제 LLM workflow 안에서
`anvil_*` MCP tool을 선택해 호출하는 end-to-end 검증까지 완료했다.
단, 기본 IronClaw tool inventory 전체를 Gemini backend에 노출하면 일부 IronClaw
내장 tool schema가 Gemini function declaration 검증에서 거부된다. 따라서 이번
검증은 임시 PostgreSQL DB에 anvil 외 내장 tool permission을 `disabled`로 설정한
anvil 전용 profile로 수행했다.

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
  - `LLM_BACKEND=gemini`
  - `GEMINI_MODEL=gemini-2.5-flash`
  - `SECRETS_MASTER_KEY`는 IronClaw가 자동 생성했으며 문서에 값을 기록하지 않는다.
  - Google Gemini API key는 onboarding 과정에서 입력했고 문서에 값을 기록하지 않는다.
- IronClaw onboarding:
  - 실행 명령: `ironclaw onboard --cli-only`
  - database: PostgreSQL
  - security: environment variable
  - provider: Google Gemini native API
  - model: `gemini-2.5-flash`
  - channel: CLI/TUI only
  - registry extension: none
  - Docker sandbox: disabled
  - heartbeat: disabled
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
ironclaw models status --json --no-onboard --cli-only
ironclaw mcp list --verbose --no-onboard --cli-only
ironclaw mcp test anvil --no-onboard --cli-only
```

결과:

- `ironclaw --version`: `ironclaw 0.28.1`
- `ironclaw doctor`: 실패 0개. PostgreSQL 연결, Gemini LLM config, MCP server
  config, secrets backing store 확인
- `ironclaw models status`: provider `gemini`, model `gemini-2.5-flash`
- `ironclaw mcp list`: `anvil` stdio MCP server 등록 확인
- `ironclaw mcp test anvil`: 연결 성공, `anvil_*` tool 11개 조회 성공

## IronClaw Agent E2E 결과

검증 환경:

- `anvil-daemon`: `ANVIL_API_ADDR=127.0.0.1:3000`
- IronClaw backend: `gemini`
- IronClaw model: `gemini-2.5-flash`
- MCP server: `anvil` stdio
- E2E 격리 방식: 임시 DB `ironclaw_e2e_anvil`
  - 기존 `ironclaw` DB 설정을 변경하지 않기 위해 별도 DB를 사용했다.
  - 임시 DB에는 anvil 외 IronClaw 내장 tool permission을 `disabled`로 설정했다.
  - 검증 후 임시 DB는 삭제했다.

사전 확인:

```bash
DATABASE_URL='postgresql:///ironclaw_e2e_anvil?host=/var/run/postgresql' \
DATABASE_BACKEND=postgres \
LLM_BACKEND=gemini \
GEMINI_MODEL=gemini-2.5-flash \
GEMINI_API_KEY='<redacted>' \
SKILLS_ENABLED=false \
WASM_ENABLED=false \
BUILDER_ENABLED=false \
GATEWAY_ENABLED=false \
HEARTBEAT_ENABLED=false \
ironclaw mcp test anvil --no-onboard --cli-only
```

결과:

- `anvil` MCP 연결 성공
- `anvil_*` tool 11개 조회 성공

최소 agent E2E:

- 요청: `anvil_spawn_vm` 후 `anvil_delete_vm`
- 결과: 성공
- 관찰:
  - 첫 tool call에서 모델이 지시하지 않은 `profile=minimal`을 붙여 daemon이
    `profile "minimal" not found`를 반환했다.
  - 같은 agent run 안에서 모델이 profile 없이 재시도했고, VM 생성과 삭제가
    정상 완료되었다.

전체 agent E2E:

- 요청 flow:
  - `anvil_spawn_vm`
  - `anvil_copy_in`
  - `anvil_copy_out`
  - `anvil_get_vm_health`
  - `anvil_stop_vm`
  - `anvil_delete_vm`
- session_name: `ironclaw-e2e-full-20260512`
- copy path: `e2e/input.txt`
- copy content: `ironclaw-anvil-e2e-input`
- 결과:
  - VM 생성 성공
  - `anvil_copy_in`은 첫 호출에서 잘못된 `encoding` 값으로 validation failure가
    1회 발생했으나, 같은 agent run 안에서 올바른 인자로 재호출해 성공했다.
  - `anvil_copy_out` 결과 content는 `ironclaw-anvil-e2e-input`와 일치했다.
  - health 결과는 `{"status":"idle"}`였다.
  - stop/delete 모두 HTTP 200으로 완료되었다.
  - 검증 후 `GET /vms` 결과는 빈 목록 `[]`였다.

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

반복 가능한 MCP smoke 명령은 wrapper로 고정한다. daemon은 별도 터미널에서
이미 실행 중이어야 한다.

```bash
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
```

`lifecycle`은 `anvil_run_task` 응답의 의미적 marker assertion만 끈다. 이 모드도
`anvil_run_task`를 호출하므로 task call이 완료될 수 있는 daemon/profile/provider
경로는 필요하다. `semantic`은 같은 flow에 더해 응답 body의 `anvil-smoke-ok`
marker를 확인한다. 두 모드 모두 실제 daemon-backed 검증이므로 KVM/root가 가능한
host와 daemon에 도달 가능한 `ANVIL_DAEMON_URL`/`ANVIL_API_TOKEN` 설정이 필요하며,
`semantic`은 기대한 marker를 반환할 수 있는 유효한 LLM credential까지 요구한다.

## 남은 운영상 주의점

설치, onboarding, MCP 등록, MCP smoke, IronClaw agent E2E는 완료되었다.
anvil MCP tool input struct는 `ValidateIronClawToolInputSchemas`로 Gemini function
declaration type이 비어 있지 않은지 정적 검증한다.

```bash
go test ./internal/anvilmcp ./cmd/anvil-mcp -run 'Test.*IronClaw|Test.*ToolRegistration' -count=1
```

남은 주의점은 IronClaw 기본 tool inventory 전체와 Gemini function schema의 호환성이다.

- 기본 설정으로 `ironclaw run`을 실행하면 Gemini가 다음 형태의 요청 오류를 반환한다.
  - `Invalid value at 'tools[0].function_declarations[...]...type', ""`
- 이 오류는 `ironclaw mcp test anvil` 실패가 아니다.
  - `ironclaw mcp test anvil`은 성공한다.
  - 실패는 LLM provider로 전달되는 전체 tool declaration 배열에서 발생한다.
- 임시 DB에서 anvil 외 내장 tool을 숨기면 IronClaw agent가 `anvil_*` tool을 실제로
  호출할 수 있다.
- 운영 profile에서는 anvil 전용 tool permission profile을 유지하거나, IronClaw
  upstream에서 전체 built-in tool inventory의 Gemini function schema 변환이 수정된
  버전으로 재검증해야 한다. anvil tool 자체의 input struct는 빈 type을 만들지 않는
  상태로 검증된다.

## 재개 조건

후속 검증은 다음 조건에서 진행한다.

1. 실제 운영 DB에 anvil 전용 tool permission profile을 적용한다.
2. `anvil_run_task`를 포함한 장시간 tool call을 IronClaw agent 기준으로 재검증한다.
3. Gemini schema 호환성 문제가 IronClaw upstream에서 해결되면 기본 tool inventory
   상태로 회귀 테스트한다.
