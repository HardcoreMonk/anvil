# Cross-host Snapshot Replication 수동 API 설계

작성일: 2026-06-02

## 1. 목적

anvil scheduler는 이미 `PlacementStoreState.SnapshotLocations`를 통해 snapshot
locality를 restore scheduling에 반영할 준비가 되어 있다. 하지만 현재 snapshot
artifact는 단일 ephemera daemon host의 `snapshots/{snapshot_id}` directory에만
존재하므로, source host 장애나 capacity 부족 상황에서 다른 host로 restore할 수 없다.

이 설계는 operator가 명시적으로 요청하는 cross-host snapshot replication을 추가한다.
첫 버전은 자동 policy나 background worker를 만들지 않고, source daemon에서 target
daemon으로 snapshot artifact를 안전하게 복제한 뒤 scheduler catalog에 location을
기록하는 동기 operation으로 제한한다.

## 2. 범위

포함 범위:

- source daemon snapshot export API
- target daemon snapshot import API
- full snapshot과 diff snapshot artifact bundle 검증
- diff snapshot dependency가 target에 없을 때 `include_dependencies=true`로 base
  full snapshot을 먼저 복제하는 orchestration
- target import의 temporary directory staging과 atomic publish
- MCP/runtime router의 수동 replication orchestration
- operator trigger surface인 `anvil_replicate_snapshot` MCP tool
- 성공 시 `PlacementStore.SnapshotLocations` 갱신
- restore scheduler가 기존 `/schedule/restore?snapshot_id=...` 흐름으로 복제된 host를
  선호하도록 유지
- README, RELEASE_NOTES, architecture, operations 후속 문서 갱신
- VM/Firecracker 없이 실행 가능한 storage, daemon API, router unit test

제외 범위:

- 자동 replication policy
- background replication worker 또는 long-running job queue
- degraded/unhealthy source host override
- cross-region WAN transport 최적화
- encrypted artifact-at-rest 재암호화
- snapshot alias/name 변경
- tenant별 replication quota/policy engine
- flock placement 연동
- Prometheus metrics 필수 추가
- KVM 기반 real restore 통합 테스트

## 3. 선택한 접근

선택안은 daemon-to-daemon import/export API다.

source daemon은 snapshot artifact를 export하고, target daemon은 import와 local storage
publish를 소유한다. MCP/runtime router는 어떤 host에서 어떤 host로 복제할지
orchestration하고, 성공 이후 scheduler placement catalog만 갱신한다.

이 접근은 snapshot artifact 소유권을 daemon/storage 경계 안에 유지한다. MCP가 host
filesystem path, local permissions, partial copy cleanup, secret-bearing metadata를
직접 다루지 않으므로 기존 trust boundary와 더 잘 맞는다.

## 4. 책임 경계

### Source daemon

- `POST /snapshots/{snapshot_id}/export`를 제공한다.
- snapshot metadata와 artifact file을 하나의 streamable bundle로 내보낸다.
- snapshot이 diff이면 `base_snapshot_id`를 manifest에 포함한다.
- export 중 `agent_token`, authorization header, daemon raw body는 log, metrics, audit,
  response summary에 남기지 않는다.
- snapshot이 없으면 `404 snapshot_not_found`를 반환한다.

### Target daemon

- `POST /snapshots/import`를 제공한다.
- import body를 `snapshots/.import-<snapshot_id>-<nonce>/`에 먼저 쓴다.
- manifest, file list, size, checksum, metadata consistency를 검증한다.
- 검증 성공 후 `snapshots/{snapshot_id}`로 atomic rename한다.
- 같은 `snapshot_id`가 이미 있고 manifest/checksum이 같으면 idempotent success를
  반환한다.
- 같은 `snapshot_id`가 이미 있지만 metadata나 artifact checksum이 다르면
  `409 snapshot_conflict`를 반환한다.
- import 실패 또는 stream 중단 시 temporary directory를 삭제한다.

### MCP/runtime router

- `ReplicateSnapshot(source_host, target_host, snapshot_id, include_dependencies)`를
  제공한다.
- source/target host가 scheduler inventory에 있고 서로 다른 host인지 확인한다.
- source host가 healthy가 아니면 기본적으로 요청을 거부한다.
- diff snapshot이고 target에 base full snapshot이 없으며
  `include_dependencies=true`이면 base full snapshot을 먼저 복제한다.
- 각 snapshot import가 성공하거나 idempotent `already_present`로 확인된 뒤 해당
  snapshot에 대해 `PlacementStore.SetSnapshotLocation`과 `Save`를 호출한다.
- 실패한 snapshot의 `SnapshotLocations`는 갱신하지 않는다.

## 5. API 계약

### Snapshot export

Endpoint:

```text
POST /snapshots/{snapshot_id}/export
```

Request:

```json
{}
```

Response:

- 성공: `200 OK`, `application/vnd.anvil.snapshot-bundle`
- 실패:
  - `404 snapshot_not_found`
  - `409 snapshot_export_busy`
  - `500 snapshot_export_failed`

첫 구현의 bundle format은 tar stream을 사용한다. tar 안에는 다음 entry가 들어간다.

```text
manifest.json
metadata.json
memory.bin
state.bin
rootfs.ext4
```

`manifest.json` 예시:

```json
{
  "snapshot_id": "snap-1",
  "snapshot_type": "diff",
  "base_snapshot_id": "snap-base",
  "created_at": "2026-06-02T00:00:00Z",
  "files": [
    {"path": "metadata.json", "size_bytes": 1024, "sha256": "..."},
    {"path": "memory.bin", "size_bytes": 4096, "sha256": "..."},
    {"path": "state.bin", "size_bytes": 8192, "sha256": "..."},
    {"path": "rootfs.ext4", "size_bytes": 734003200, "sha256": "..."}
  ]
}
```

### Snapshot import

Endpoint:

```text
POST /snapshots/import
```

Request:

- `application/vnd.anvil.snapshot-bundle`
- body는 export bundle stream이다.

Response:

```json
{
  "snapshot_id": "snap-1",
  "snapshot_type": "diff",
  "base_snapshot_id": "snap-base",
  "status": "imported",
  "skipped": false
}
```

Status values:

- `imported`: target에 새 snapshot을 publish했다.
- `already_present`: 같은 content가 이미 있어 no-op 처리했다.

Failure:

- `400 invalid_snapshot_bundle`
- `409 diff_base_missing`
- `409 snapshot_conflict`
- `500 snapshot_import_failed`

### `anvil_replicate_snapshot`

첫 구현의 operator trigger는 MCP tool `anvil_replicate_snapshot`이다. 별도 scheduler
HTTP endpoint는 첫 버전에 추가하지 않는다.

Tool input:

```go
type ReplicateSnapshotInput struct {
	SnapshotID          string `json:"snapshot_id"`
	SourceHost          string `json:"source_host"`
	TargetHost          string `json:"target_host"`
	IncludeDependencies bool   `json:"include_dependencies"`
}
```

Runtime router 내부 계약도 같은 field를 사용한다.

```go
type SnapshotReplicationRequest struct {
	SnapshotID          string `json:"snapshot_id"`
	SourceHost          string `json:"source_host"`
	TargetHost          string `json:"target_host"`
	IncludeDependencies bool   `json:"include_dependencies"`
}
```

응답:

```json
{
  "snapshot_id": "snap-1",
  "source_host": "host-a",
  "target_host": "host-b",
  "status": "replicated",
  "replicated": ["snap-base", "snap-1"],
  "skipped": [],
  "errors": []
}
```

MCP tool 응답은 `SnapshotReplicationResponse`를 그대로 사용한다. 응답에는 host
endpoint, authorization header, daemon raw response, `metadata.json` raw body를 넣지
않는다.

## 6. Dependency 규칙

full snapshot:

- 단독으로 export/import할 수 있다.
- target에 이미 있으면 checksum 일치 여부로 idempotency를 판단한다.

diff snapshot:

- `base_snapshot_id`를 반드시 유지한다.
- target daemon은 diff import 전에 base full snapshot이 local catalog에 있는지
  확인한다.
- base가 없으면 import는 `409 diff_base_missing`을 반환한다.
- router는 `include_dependencies=true`일 때 source에서 base full snapshot을 먼저
  export/import한다.
- 복제 순서는 항상 `base full -> diff`다.

GC/delete invariant:

- target daemon에서도 기존 `DELETE /snapshots/{id}`와 `POST /snapshots/gc`의
  "diff가 참조 중인 full snapshot 삭제 금지" 조건을 그대로 적용한다.
- 한 번의 replication 요청에서 base와 diff 중 diff import가 실패하면 base는 그대로
  둘 수 있다. base full만 존재하는 상태는 restore contract를 깨지 않는다.

## 7. Data model

기존 `PlacementStoreState.SnapshotLocations map[string][]string`를 계속 사용한다.

새 장기 상태는 첫 버전에 추가하지 않는다. replication은 동기 operation이며, 성공 또는
실패가 HTTP/MCP 응답에 즉시 담긴다. 장기 audit이나 retry queue가 필요하면 후속 작업으로
별도 job store를 설계한다.

추가 타입:

```go
type SnapshotExportManifest struct {
	SnapshotID     string                 `json:"snapshot_id"`
	SnapshotType   string                 `json:"snapshot_type"`
	BaseSnapshotID string                 `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
	Files          []SnapshotBundleFile    `json:"files"`
}

type SnapshotBundleFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type SnapshotReplicationResponse struct {
	SnapshotID  string   `json:"snapshot_id"`
	SourceHost  string   `json:"source_host"`
	TargetHost  string   `json:"target_host"`
	Status      string   `json:"status"`
	Replicated  []string `json:"replicated"`
	Skipped     []string `json:"skipped"`
	Errors      []string `json:"errors"`
}
```

`SnapshotMetadata` 자체는 기존 restore에 필요한 `AgentToken`, path 정보를 포함한다.
따라서 manifest와 public response는 `SnapshotInfo` 수준의 public field만 노출한다.
bundle 내부 `metadata.json`은 daemon-to-daemon auth 경계 안에서만 흐르며, 로그와
operator-facing JSON에는 그대로 출력하지 않는다.

## 8. Error handling

| 상황 | 처리 |
|---|---|
| source snapshot 없음 | router response에 error, location 미갱신 |
| source host unhealthy/degraded | `source_host_unavailable`, export 시작 전 거부 |
| target host unhealthy/degraded | `target_host_unavailable`, import 시작 전 거부 |
| source와 target이 같음 | `400 same_host` |
| target에 같은 snapshot/content 있음 | `already_present`, location 갱신 가능 |
| target에 같은 ID/different content 있음 | `409 snapshot_conflict`, location 미갱신 |
| diff base 없음, dependencies disabled | `409 diff_base_missing`, 실패한 diff location 미갱신 |
| dependency base import 성공 후 diff import 실패 | 성공한 base full location만 기록하고 diff location은 기록하지 않는다. 응답에는 `status: "partial"`과 실패한 diff error를 포함한다. |
| import stream 중단 | temp directory 삭제, location 미갱신 |
| `PlacementStore.Save` 실패 | replication은 target에 남아 있지만 scheduler catalog 갱신 실패로 응답은 실패 처리한다. operator는 retry로 idempotent catalog update를 시도할 수 있다. |

## 9. Security and invariants

- `agent_token`은 `POST /vms` 응답 외에는 operator-facing 응답에 노출하지 않는다.
- export/import API는 기존 daemon control-plane auth 뒤에 둔다.
- router/MCP 응답에는 host endpoint, authorization header, daemon raw response,
  `metadata.json` raw body를 넣지 않는다.
- snapshot bundle은 daemon-to-daemon trusted channel에서만 전송한다. 공개 인터넷
  노출은 TLS 종료 reverse proxy와 network policy 뒤에서만 허용한다.
- 실행 중인 원본 VM의 snapshot restore 금지 조건은 변경하지 않는다.
- VM deletion cleanup, TAP/IP cleanup, dm-snapshot cleanup, bind mount cleanup은
  변경하지 않는다.
- diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
- import는 publish 전까지 canonical `snapshots/{snapshot_id}` path를 만들지 않는다.

## 10. Testing strategy

Storage tests:

- export manifest가 `metadata.json`, `memory.bin`, `state.bin`, `rootfs.ext4`를
  포함한다.
- checksum mismatch를 감지한다.
- import는 temp directory에 쓴 뒤 atomic rename한다.
- failed import는 temp directory를 삭제한다.
- existing identical snapshot은 idempotent success가 된다.
- existing conflicting snapshot은 `snapshot_conflict`가 된다.

Daemon API tests:

- full snapshot export/import 성공.
- diff snapshot import는 base가 없으면 `diff_base_missing`.
- diff snapshot import는 base가 있으면 성공.
- import된 diff가 참조하는 base full은 `DELETE /snapshots/{base}`에서 보호된다.
- malformed bundle은 `400`.

Router tests:

- full snapshot replication 성공 시 target host가 `SnapshotLocations`에 기록된다.
- diff replication with dependencies는 `base full -> diff` 순서로 호출한다.
- `include_dependencies=false`이고 target에 base가 없으면 실패한다.
- import 실패 시 `SnapshotLocations`를 갱신하지 않는다.
- identical existing target snapshot은 skipped로 응답하고 location을 기록한다.
- unhealthy source/target host는 daemon call 전 거부된다.

Verification commands:

```bash
go test ./internal/storage
go test ./cmd/goose-daemon
go test ./internal/anvilmcp
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
git diff --check
```

KVM/Firecracker 기반 restore 검증은 runtime release 전에 별도 운영 환경에서 수행한다.
이번 첫 구현의 기본 gate에는 포함하지 않는다.

## 11. Documentation updates

다음 문서를 갱신한다.

- `README.md`: snapshot API와 scheduler restore locality 설명에 수동 replication 추가
- `RELEASE_NOTES.md`: Unreleased 항목에 수동 cross-host snapshot replication 기록
- `docs/architecture/service-logic.md`: export/import, dependency, GC invariant 설명
- `docs/architecture/runtime-architecture.md`: snapshot directory import staging과 atomic
  publish 설명
- `docs/operations/runbook.md`: operator replication 절차와 failure recovery
- `docs/operations/2026-05-29-anvil-follow-up-development.md`: 4번 진행 상태 갱신

## 12. 후속 후보

- background replication worker
- policy 기반 N-host replication
- replication metrics와 alert
- replication audit log와 retry queue
- degraded source override
- remote transport compression/rate limiting
- scheduler-aware cross-host flock placement 연동
