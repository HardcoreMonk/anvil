# Scheduler-aware Flock Placement 운영 인계

작성일: 2026-06-03

## 릴리즈 범위

- MCP `anvil_spawn_flock`은 scheduler router config가 있을 때 scheduler-aware
  single-host placement를 사용한다.
- roles 수를 active VM 요청량으로 계산해 tenant quota와 host capacity를 확인한다.
- 선택된 daemon host의 기존 `POST /flocks`를 호출한다.
- 반환된 flock member VM ID를 scheduler `PlacementStore.VMPlacements`에 기록한다.

## 제외 범위

- daemon direct `POST /flocks` 계약 변경
- flock member의 cross-host 분산 배치
- cross-host Town Wall forwarding
- cross-host `gtcall`
- partial flock 허용

## 검증

- `go test ./internal/anvilmcp -count=1`
- `go test ./cmd/anvil-mcp -count=1`
- `go test ./... -count=1`
- `go build ./cmd/anvil-mcp`
- `go build ./cmd/anvil-scheduler`
- `go build ./cmd/goose-daemon`
- `bash -n scripts/anvil-mcp-e2e.sh`
- `git diff --check`

## 보안 조건

- `anvil_spawn_flock` 응답은 `agent_token` 또는 `agent_tokens`를 노출하지 않는다.
- scheduler placement state에는 VM ID와 host name만 저장한다.
- host endpoint, authorization header, bearer token, daemon raw body는 MCP output에
  넣지 않는다.

## 잔여 위험

- 첫 버전은 single-host flock placement다.
- placement save 실패 시 flock은 이미 생성된 상태일 수 있으며, 운영자는 daemon
  `DELETE /flocks/{id}`로 정리해야 한다.
- KVM/Firecracker full e2e는 이번 unit verification의 필수 조건이 아니다.

## 다음 작업

- true cross-host flock placement 설계
- cross-host Town Wall/`gtcall` routing 설계
- scheduler flock placement metrics 검토
