# IronClaw 본체 연동 점검

## 결론

현재 로컬 환경에서는 IronClaw 본체 기준 MCP tool call 검증을 완료할 수 없다.
`anvil-mcp` adapter와 Go MCP SDK smoke client 기준 검증은 완료되었지만,
IronClaw 실행 파일 또는 source checkout이 확인되지 않았다.

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

- `ironclaw` 실행 파일: 없음
- `/data/projects` 아래 IronClaw project checkout: 없음
- `/home/hardcoremonk` 아래 IronClaw project checkout: 없음
- 검색된 파일은 debate 상태 파일뿐이며 실행 가능한 IronClaw 본체가 아니다.

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

IronClaw 본체 기준 검증에는 다음 중 하나가 필요하다.

- `ironclaw` 실행 파일 설치
- IronClaw source checkout 경로
- IronClaw의 MCP server 등록 방식 또는 config file 위치

## 재개 조건

IronClaw 본체가 설치되면 다음 순서로 재검증한다.

1. IronClaw에서 `cmd/anvil-mcp`를 stdio MCP server로 등록한다.
2. `ANVIL_DAEMON_URL`과 `ANVIL_API_TOKEN`을 IronClaw 실행 환경에 주입한다.
3. root 권한으로 `anvil-daemon`을 실행한다.
4. IronClaw에서 `anvil_spawn_vm`, `anvil_copy_in`, `anvil_run_task`,
   `anvil_copy_out`, `anvil_delete_vm`을 호출한다.
5. tool response와 daemon log를 함께 보관한다.
