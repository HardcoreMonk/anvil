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
scripts/anvil-mcp-e2e.sh flock
```

`lifecycle`은 `anvil_run_task` 응답의 의미적 marker assertion만 끈다. 이 모드도
`anvil_run_task`를 호출하므로 task call이 완료될 수 있는 daemon/profile/provider
경로는 필요하다. `semantic`은 같은 flow에 더해 응답 body의 `anvil-smoke-ok`
marker를 확인한다. 두 모드 모두 실제 daemon-backed 검증이므로 KVM/root가 가능한
host와 daemon에 도달 가능한 `ANVIL_DAEMON_URL`/`ANVIL_API_TOKEN` 설정이 필요하며,
`semantic`은 기대한 marker를 반환할 수 있는 유효한 LLM credential까지 요구한다.
`flock`은 후속 Goosetown MCP surface 보강으로 추가된 모드이며
`anvil_spawn_flock`, `anvil_list_flocks`, `anvil_post_townwall`,
`anvil_get_townwall_history`, `anvil_delete_flock`을 확인한다.

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

## v0.7.0 재검증 (2026-07-12)

E2E 데모 asset refresh(`docs/e2e-demo-refresh`) 과정에서 IronClaw 실세션을 재시도했다.
대상은 untagged `main`(post-`anvil-v0.7.0`, commit `9a9af7d`). 결과는 2026-05-12 관찰과 일치한다.

- 재현 환경: 로컬 `ironclaw`(`~/.local/bin/ironclaw`), backend `gemini` / `gemini-2.5-flash`,
  `anvil` stdio MCP server를 이번 worktree의 fresh `anvil-mcp`로 repoint, `anvil-daemon`
  auth-off `127.0.0.1:3000`.
- `ironclaw mcp test anvil`: 연결 성공, `anvil_*` tool **19개** 조회(2026-05-12에는 11개 —
  Goosetown/snapshot replication 등 surface 확장).
- 기본(full) tool inventory로 `ironclaw --cli-only --no-onboard --auto-approve -m <lifecycle
  prompt>` 실행 시 2026-05-12와 **동일 시그니처** 재현:
  `Invalid value at 'tools[0].function_declarations[29].parameters.properties[..].value.type', ""`
  (Gemini 400 `INVALID_ARGUMENT`). anvil tool이 아니라 IronClaw 내장 tool inventory 전체를
  Gemini에 노출한 데서 발생한다.
- 완화책 적용(anvil 전용 profile): `ironclaw config set tool_permissions.<31개 내장 tool>
  disabled` + env `SKILLS_ENABLED / WASM_ENABLED / BUILDER_ENABLED / GATEWAY_ENABLED /
  HEARTBEAT_ENABLED = false`. → **스키마 오류 해소**. tool declaration 배열이 Gemini 검증을
  통과했고 요청이 다음 단계로 진행됐다.
- 다음 단계에서 **credential 오류**로 차단: `API key not valid. Please pass a valid API key.`
  (Gemini 400, `reason: API_KEY_INVALID`). `~/.ironclaw` DB에 저장된 Gemini API key가
  2026-07-12 시점 무효/만료 상태다. 프로젝트 내 대체 가능한 유효 Google/Gemini key는 없다
  (`configs/goose-secrets.yaml`는 provider openai). 따라서 이번 세션에서는 IronClaw
  agent-driven full lifecycle 재녹화를 완료하지 못했다.
- 데모 (a)는 fallback으로 유지: 실제 `ironclaw mcp test anvil` 핸드셰이크(장면 1) + 동일
  anvil MCP tool surface를 `anvil-mcp-smoke`로 구동한 실 VM lifecycle(장면 2)을 녹화.
  README 캡션에서 두 장면의 주체를 분리 서술한다.

### `TestCurrentAnvilToolInputsAreGeminiCompatible` 커버리지 판단

`internal/anvilmcp/ironclaw_schema_test.go`의 이 테스트는
`ValidateIronClawToolInputSchemas(CurrentIronClawToolInputSchemas())`로 **anvil tool input
스키마**가 빈 Gemini type을 만들지 않는지만 검증한다. 이번 실패는 IronClaw **내장** tool
inventory에서 발생하므로 이 테스트 범위 밖이며, 테스트가 실패 모드를 놓친 것이 아니다 —
anvil surface는 정상 커버된다. IronClaw 내장 tool의 Gemini 스키마 호환은 anvil 테스트로
막을 수 없고, upstream 수정 또는 anvil 전용 profile 유지로만 다룰 수 있다.

### 잔여 후속

1. `~/.ironclaw` Gemini API key rotation/refresh 후 anvil 전용 profile로 agent-driven full
   lifecycle을 재녹화한다(스키마 통과는 이번에 확인됨).
2. IronClaw upstream이 전체 built-in tool inventory의 Gemini function schema 변환을 고치면
   기본 inventory 상태로 회귀 테스트한다.

이번 세션의 `~/.ironclaw` 변경(tool_permissions 31개 `disabled`, `mcp-servers.json` repoint)은
검증 종료 후 원값으로 **전부 원복**했다(원복 후 per-key diff 일치 확인).

### 갱신된 credential 재검증 (2026-07-12 후속)

만료 key를 **갱신된 credential**로 교체(값은 `~/.ironclaw`에만 보관, 문서·repo·report 어디에도
미기록)한 뒤 anvil 전용 profile로 재시도했다. 이때 위 "잔여 후속 1"의 낙관적 서술
("스키마 통과는 이번에 확인됨")이 **부정확**했음이 드러났다 — 정정한다.

- credential 갱신으로 `API_KEY_INVALID`는 해소됐다(Gemini auth 통과).
- 그러나 valid key 상태에서 **새 스키마 오류**가 표면화됐다. 이번에는 IronClaw 내장 tool이
  아니라 **anvil 자신의 flock-create tool 2종**(`anvil_spawn_flock`,
  `anvil_create_routed_flock_members`)에서 발생한다:
  `GenerateContentRequest.tools[0].function_declarations[..].parameters.properties[roles].items:
  field predicate failed: $type == Type.ARRAY`. 두 tool의 `roles []string` 필드가 만든 MCP
  wire schema를 Gemini function declaration 검증이 거부한다. round-2의 `API_KEY_INVALID`가
  이 오류를 **가려서**, "anvil-only profile이 스키마를 완전히 해소했다"는 판단은 절반만
  맞았다(내장 tool 오류는 해소, anvil flock tool 오류는 미해소).
- 원인 상세: `TestCurrentAnvilToolInputsAreGeminiCompatible`는 **이상화된**
  `CurrentIronClawToolInputSchemas()`(roles=ARRAY / items=STRING)만 검증하며 이는 통과한다.
  실제 wire schema는 go-sdk `mcp.AddTool`이 `SpawnFlockInput{}`를 reflection해 생성하는데, 그
  형태가 Gemini에 거부된다. 즉 테스트의 이상화 표현과 실제 wire schema가 flock `roles`에서
  괴리된다 — 테스트가 실제 wire schema를 커버하지 않는다.
- **실증(PoC)**: 위 2개 flock-create tool만 제외한 로컬 임시 빌드(커밋/기록 안 함, 검증 후 폐기)
  로는 IronClaw agent-driven **full VM lifecycle이 실제로 성공**한다. gemini-2.5-flash가 자기
  판단으로 tool을 순차 호출: `anvil_spawn_vm` → `anvil_run_task`(출력 `anvil-smoke-ok`) →
  `anvil_create_snapshot` → (restore가 `409 source_vm_running`을 반환하자 agent가 스스로
  `anvil_stop_vm`→`anvil_delete_vm`로 원본을 제거) → `anvil_restore_snapshot` →
  `anvil_get_vm_health`(idle) → `anvil_delete_vm`(복원본) → `anvil_delete_snapshot` →
  `LIFECYCLE OK`. VM lifecycle tool 자체는 Gemini 호환이며 agent가 자율 구동한다.
- **데모 격상 blocker**: 배포되는 `anvil-mcp`는 broken flock tool 2종을 항상 노출하고, IronClaw는
  MCP tool 전체를 LLM에 전달한다(`tool_permissions=disabled`는 실행 gate일 뿐 LLM 노출을
  제거하지 않음 — 확인함). 따라서 재현 가능한 정직한 real-agent GIF는 anvil-mcp 수정
  (flock `roles` wire schema를 Gemini-safe하게 고치거나, 2개 flock-create tool을 opt-in gate)
  이 선행돼야 한다. 이는 anvil behavior/스키마 변경으로 데모 asset 범위를 벗어나므로 여기서는
  **수정하지 않고 보고만** 한다.
- 이번 후속의 `~/.ironclaw` 변경도 전부 원복했다(tool_permissions 31 built-in per-key diff 일치
  + `config set`이 만든 flock MCP-tool 2행을 DB `settings`에서 삭제, `mcp-servers.json` 원복).
  **단, 갱신된 credential은 사용자 의도대로 `~/.ironclaw/.env`에 유지**한다.

#### 잔여 후속 (갱신)

1. anvil-mcp의 `anvil_spawn_flock` / `anvil_create_routed_flock_members` `roles` wire schema를
   Gemini function declaration 호환으로 수정(또는 2 tool을 gate)한다. 완료되면 배포 adapter로
   IronClaw agent-driven full lifecycle을 재녹화해 `docs/assets/ironclaw-e2e.gif`를 교체한다.
2. `TestCurrentAnvilToolInputsAreGeminiCompatible`가 이상화 표현이 아닌 **실제 MCP wire schema**
   (SDK reflection 산출물)를 검증하도록 보강해 이 괴리를 회귀 방지한다.

### D5 fix로 전체 표면 실세션 성공 (2026-07-12)

위 "잔여 후속 (갱신)" 2건을 D5 slice로 처리했다.

- **근본 원인 확정**: jsonschema-go는 nilable Go kind(slice/map/pointer)를 null union으로
  렌더링한다 — `[]string roles`가 `{"type":["null","array"],"items":{"type":"string"}}`로
  나온다. Gemini function declaration 검증은 이 union을 `Type.ARRAY`로 매핑하지 못해
  `properties[roles].items: field predicate failed: $type == Type.ARRAY`로 전체 tool 배열을
  거부한다(문제는 items type 누락이 아니라 property의 null-union type).
- **fix**: `NormalizeSchemaForGemini`가 생성 시점에서 `["null", X]`를 단일 type `X`로 축약한다
  (roles뿐 아니라 모든 `[]T`/nilable 필드 커버). `AddToolGeminiSafe`가 모든 등록 tool의 input
  schema에 적용한다. 단일-type array + typed items는 표준 JSON Schema라 비-Gemini MCP
  클라이언트에 additive(하위호환). tool 의미·필드는 불변.
- **가드**: `TestAnvilToolInputWireSchemasAreGeminiCompatible`가 이상화 표현이 아닌 **실제 SDK
  wire schema**(19 tool 전체)를 검증한다 — union type이나 items type 누락을 잡는다. fix 전
  flock 2종에서 RED, fix 후 GREEN. 관측된 Gemini 거부 시그니처를 테스트 주석에 보존.
- **검증(갱신된 credential로, 값 미기록)**:
  - `go test -race ./internal/... ./cmd/...`: anvilmcp/cmd 전부 green (무관한 기존
    flaky `TestDistributedTokens_ConcurrentRefill`은 -count=1 재실행 시 안정 통과).
  - `scripts/anvil-mcp-e2e.sh semantic` + `flock`: 둘 다 `smoke test passed`.
  - **결정판**: IronClaw 실세션을 **전체 19-tool 표면**(flock 제외 없이, 완화책+갱신 key)으로
    구동 → gemini-2.5-flash가 자율로 spawn → run_task(`anvil-smoke-ok`) → create_snapshot →
    delete(source) → restore → get_vm_health(idle) → delete → delete_snapshot →
    `LIFECYCLE OK`. **스키마 오류 0**, `INVALID_ARGUMENT`/`function_declarations`/`roles.items`
    재현 없음.
  - full KVM gate 1회(adapter 변경 회귀) 실행.
- **데모 격상**: 위 실세션 로그로 `docs/assets/ironclaw-e2e.gif` 재녹화, README/runtime-usage
  캡션을 "IronClaw 본체가 anvil MCP tool로 실제 VM lifecycle 구동"으로 단순화.
- IronClaw 내장 tool inventory 전체는 여전히 Gemini 비호환이므로 anvil 전용 profile(31 built-in
  `disabled`)은 실세션에 계속 필요하다 — 이는 IronClaw upstream 사안이며 D5 범위 밖. 세션 후
  tool_permissions는 원복, 갱신 credential은 `~/.ironclaw`에 유지.
