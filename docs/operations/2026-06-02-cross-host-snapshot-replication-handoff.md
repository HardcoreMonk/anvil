# Cross-host Snapshot Replication 운영 인계

작성일: 2026-06-02

## 릴리즈 범위

- daemon snapshot bundle export/import API:
  - `POST /snapshots/{id}/export`
  - `POST /snapshots/import`
- MCP `anvil_replicate_snapshot`
- RuntimeRouter 기반 source export stream -> target import stream 연결
- scheduler `PlacementStoreState.SnapshotLocations` 갱신
- diff snapshot의 base full dependency 선복제

## 검증

- `go test ./internal/storage -count=1`: PASS
- `go test ./cmd/goose-daemon -count=1`: PASS
- `go test ./internal/anvilmcp -count=1`: PASS
- `go test ./cmd/anvil-mcp -count=1`: PASS
- `go test ./... -count=1`: PASS
- `go build ./cmd/goose-daemon`: PASS
- `go build ./cmd/anvil-mcp`: PASS
- `go build ./cmd/anvil-scheduler`: PASS
- `gofmt -w ...`: 변경 없음
- `git diff --check`: PASS
- documentation grep:
  `rg -n "anvil_replicate_snapshot|snapshots/.+export|snapshots/import|SnapshotLocations|cross-host snapshot replication" README.md RELEASE_NOTES.md docs/architecture docs/operations`: PASS

## Review 상태

- Storage bundle helper: spec/quality review 승인.
- Daemon import/export API: spec/quality review 승인.
- Daemon client streaming helper: spec/quality review 승인.
- RuntimeRouter replication orchestration: spec/quality review 승인.
- MCP tool/schema/production entrypoint: spec/quality review 승인.

## 운영 절차

1. source와 target scheduler host가 healthy인지 확인한다.
2. diff snapshot이면 `include_dependencies=true`를 사용한다.
3. MCP `anvil_replicate_snapshot`을 호출한다.
4. scheduler `/placements` 또는 state file의 `snapshot_locations[snapshot_id]`에
   target host가 추가됐는지 확인한다.
5. 실패 시 target daemon의 `snapshots/.import-*` staging directory가 남지 않았는지
   확인한다.

## 보안 조건

- replication response와 audit record에는 `agent_token`, authorization header,
  daemon raw body, raw `metadata.json` body를 넣지 않는다.
- export bundle의 `metadata.json`은 raw local metadata가 아니라 token을 제거한
  portable metadata다. `disk_path`와 `vsock_path`는 Firecracker restore 제약 때문에
  safe path로 검증한 뒤 보존한다.
- 복제된 snapshot restore는 target daemon이 새 `agent_token`을 생성해 guest agent에
  vsock으로 주입한다.
- MCP production entrypoint는 기존 tool을 계속 `ANVIL_DAEMON_URL` base daemon에
  위임한다. scheduler/router config는 `anvil_replicate_snapshot`에만 적용된다.
- host daemon client는 기존 `ANVIL_API_TOKEN`을 사용한다.

## 잔여 위험

- ~~첫 버전은 수동 동기 replication이다. background retry queue, metrics, alert는
  후속 후보로 남는다.~~ **해소 (2026-07-11, `feature/snapshot-replication-automation`
  slice)**: adapter reconcile 루프가 background retry queue 역할(desired N=2로
  bounded-cap 재시도 수렴)을 흡수했고, `anvil_scheduler_snapshot_replication_*`
  metric family + runbook 권장 alert 식이 추가됐다. 상세:
  [`docs/operations/2026-07-11-snapshot-replication-automation-handoff.md`](2026-07-11-snapshot-replication-automation-handoff.md).
- KVM/Firecracker 기반 real restore 검증은 이번 로컬 검증에 포함하지 않았다. release
  전 실제 운영 host에서 별도 확인해야 한다.
- source/target host degraded override는 지원하지 않는다.
- replicated snapshot delete를 모든 replica에 전파하는 정책은 이번 범위가 아니다.
  (2026-07-11 slice에서도 명시 비목표로 재확인 — `SnapshotLocations`는 add-only이며
  삭제가 다른 replica로 전파되지 않는다.)

## 현재 lifecycle 단계

구현, 테스트, 문서, review를 마쳤고 operate 단계로 진입할 수 있다.

## 다음 작업

- scheduler-aware cross-host flock placement 설계
- ~~replication metrics와 alert 설계~~ **해소 (2026-07-11)**: 위 "잔여 위험" 항목과
  동일 slice/handoff 참조.
- 운영 환경에서 KVM/Firecracker real restore smoke 검증
