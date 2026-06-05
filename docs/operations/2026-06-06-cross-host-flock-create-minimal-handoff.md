# Cross-host Flock Create Minimal Handoff

- 작성일: 2026-06-06
- 상태: opt-in experimental MCP tool. 기본 `anvil_spawn_flock` 동작은 변경하지 않는다.

## 활성화 조건

`anvil_create_routed_flock_members`는 다음 조건이 모두 맞을 때만 사용한다.

- `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`
- persistent `scheduler_state_path` 또는 `ANVIL_MCP_SCHEDULER_STATE`
- router host inventory를 제공하는 `scheduler_hosts_file` 또는
  `ANVIL_MCP_SCHEDULER_HOSTS_FILE`

기본 `anvil_spawn_flock`은 계속 scheduler-aware single-host placement이며, 기존 daemon
`POST /flocks` 의미를 유지한다.

## 지원 범위

- `POST /schedule/flock` plan 기반 role별 VM cross-host spawn
- host daemon `POST /vms`를 통한 member VM 생성
- `scheduler_state_path` routed flock registry 저장
- member spawn 실패 시 이미 생성한 VM rollback
- routed registry 기반 `anvil_delete_flock` delete routing
- members-only routed flock에 대한 Town Wall unsupported error
- 성공 output: `mode=cross_host_members_only`, `town_wall_enabled=false`, no
  `townwall_url`, no `post_url`

## 미지원

- Town Wall
- `/flocks/{id}/post`
- SSE/history
- cross-host `gtcall`
- guest flock context injection
- daemon `FlockManager` registration

## 운영 주의

`failed_cleanup_pending`은 일부 VM cleanup 또는 registry save가 실패해 수동 확인이
필요한 상태다. operator는 routed registry의 남은 member VM을 확인하고 해당 host
daemon에서 VM 삭제 상태를 점검해야 한다.

error reason이 `placement_save_failed`이면 persistent `scheduler_state_path`가 최신
cleanup 상태를 반영하지 못했을 수 있다. 이 경우 반환된 `flock_id`, 직전 operation
log, host daemon `/vms` 상태를 대조해 이미 삭제된 VM과 남은 VM을 직접 확인한다.

output, registry, audit, metrics에는 authorization header, host endpoint, daemon raw
body, `agent_token`을 저장하지 않는다. routed member의 `agent_url`은 VM 접근 정보로
반환될 수 있지만 daemon raw response나 bearer token을 포함하면 안 된다.
