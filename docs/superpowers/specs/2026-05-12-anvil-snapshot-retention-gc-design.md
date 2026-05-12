# anvil Snapshot Retention/GC 수동 API 설계

## 1. 목적

anvil/ephemera runtime에 snapshot retention과 garbage collection을 위한 수동
daemon API를 추가한다. 첫 버전은 자동 삭제를 하지 않고 운영자가 명시적으로
`POST /snapshots/gc`를 호출했을 때만 GC plan을 계산하거나 적용한다.

이 기능의 목표는 오래된 snapshot이 `snapshots/` 디렉터리에 무기한 쌓이는 문제를
줄이되, 기존 snapshot/restore invariant를 보존하는 것이다. 특히 diff snapshot이
참조하는 full snapshot은 삭제하지 않고, 삭제 전 dry-run 결과를 통해 운영자가
영향을 확인할 수 있어야 한다.

## 2. 범위

포함 범위:

- daemon HTTP endpoint `POST /snapshots/gc`
- request 기반 GC 후보 계산
- dry-run 기본 동작
- `apply: true`일 때 후보 snapshot 일괄 삭제
- source VM별 최신 N개 snapshot 보호
- diff dependency 때문에 삭제할 수 없는 full snapshot 보호
- 삭제 결과와 보호 사유를 포함한 JSON 응답
- README, RELEASE_NOTES, architecture 문서 갱신
- VM/Firecracker 없이 실행 가능한 unit test

제외 범위:

- daemon 시작 시 자동 GC
- 주기적 background GC worker
- snapshot size 기반 policy
- snapshot name/alias
- multi-host snapshot catalog
- tenant별 quota/policy engine
- MCP tool 표면 추가

## 3. API 계약

Endpoint:

```text
POST /snapshots/gc
```

Request:

```json
{
  "older_than_seconds": 604800,
  "keep_last_per_vm": 1,
  "apply": false
}
```

Field 의미:

| field | type | 기본값 | 의미 |
|---|---|---:|---|
| `older_than_seconds` | integer | `0` | 이 나이보다 오래된 snapshot만 삭제 후보로 본다. `0`이면 age 조건을 사용하지 않는다. |
| `keep_last_per_vm` | integer | `0` | `source_vm_id`별 최신 N개 snapshot을 보호한다. `0`이면 최신 보존 조건을 사용하지 않는다. |
| `apply` | boolean | `false` | `false`이면 dry-run plan만 반환한다. `true`이면 삭제 후보를 실제 삭제한다. |

Validation:

- request body는 생략 가능하다.
- malformed JSON은 `400` JSON error를 반환한다.
- `older_than_seconds < 0`은 `400`이다.
- `keep_last_per_vm < 0`은 `400`이다.
- `older_than_seconds == 0`이고 `keep_last_per_vm == 0`이면 전체 snapshot이
  후보가 될 수 있으므로 허용한다. 단 diff dependency 보호 규칙은 계속 적용한다.

Response:

```json
{
  "applied": false,
  "requested_at": "2026-05-12T00:00:00Z",
  "policy": {
    "older_than_seconds": 604800,
    "keep_last_per_vm": 1
  },
  "candidates": [
    {
      "snapshot_id": "snap-1",
      "source_vm_id": "vm-1",
      "snapshot_type": "diff",
      "base_snapshot_id": "snap-base",
      "created_at": "2026-05-01T00:00:00Z",
      "reason": "older_than"
    }
  ],
  "protected": [
    {
      "snapshot_id": "snap-base",
      "source_vm_id": "vm-1",
      "snapshot_type": "full",
      "created_at": "2026-05-01T00:00:00Z",
      "reason": "referenced_by_diff",
      "referenced_by": ["snap-1"]
    }
  ],
  "deleted": [],
  "errors": []
}
```

`apply: true`인 경우 `deleted`에는 삭제 성공 항목이 들어간다. 개별 삭제 실패는
`errors`에 기록하고 API는 `207 Multi-Status` 대신 `200 OK`를 반환한다. 기존
daemon API가 operation-level JSON body로 상태를 표현하는 패턴에 맞춘다.

full snapshot과 그 full을 참조하는 diff snapshot이 동시에 오래된 경우에도 첫 GC
apply에서는 diff snapshot만 삭제 후보가 되고 full snapshot은 보호된다. 그 다음
GC 호출에서 reverse reference가 사라진 full snapshot이 삭제 후보가 된다. 한 번의
GC 호출 안에서 graph를 재계산하며 연쇄 삭제하지 않는 보수적 정책이다.

## 4. 후보 계산 규칙

GC planner는 현재 메모리에 로드된 `cp.snapshots`를 기준으로 계산한다.

1. snapshot 목록을 복사한 뒤 `CreatedAt` 기준 오래된 순서로 정렬한다.
2. `source_vm_id`별로 최신순 snapshot 목록을 만들고 `keep_last_per_vm`개를
   `protected`로 표시한다.
3. 모든 diff snapshot의 `base_snapshot_id`를 모아 full snapshot의 reverse
   reference map을 만든다.
4. reverse reference가 있는 snapshot은 `protected`로 표시한다.
5. 보호되지 않은 snapshot 중 age 조건을 통과한 snapshot을 `candidates`로 표시한다.
6. age 조건이 비활성화된 경우 보호되지 않은 모든 snapshot을 `candidates`로 본다.

보호 사유 우선순위:

1. `referenced_by_diff`
2. `keep_last_per_vm`

한 snapshot이 여러 이유로 보호될 수 있지만 응답에는 가장 위험도가 높은 사유 하나를
표시한다. `referenced_by_diff`는 삭제하면 restore contract가 깨지는 hard
protection이므로 최우선이다.

## 5. 적용 규칙

`apply: false`:

- 파일 시스템을 변경하지 않는다.
- `cp.snapshots` map을 변경하지 않는다.
- `candidates`와 `protected`만 반환한다.

`apply: true`:

- planner가 계산한 `candidates`만 삭제한다.
- 삭제는 `deleteSnapshot` handler의 HTTP writer를 재사용하지 않고 내부 helper로
  분리한다.
- helper는 기존 `DELETE /snapshots/{snapshot_id}`와 같은 보호 의미를 유지한다.
- 삭제 성공 시 `cp.snapshots`에서 제거하고 `storage.DeleteSnapshot`으로
  snapshot directory를 삭제한다.
- directory 삭제 실패는 `errors`에 기록한다. map에서는 제거하지 않는다.
- 실행 중 restore와 직접 충돌하지 않도록 `snapshotsMu`를 짧게 잡고, file 삭제는
  lock 밖에서 수행한다.

기존 `DELETE /snapshots/{snapshot_id}`는 새 helper를 사용하도록 정리하되 외부
응답 형식은 유지한다.

## 6. 오류 처리

| 상황 | HTTP status | body |
|---|---:|---|
| malformed JSON | `400` | `{"error":"invalid JSON body: unexpected EOF"}` |
| negative `older_than_seconds` | `400` | `{"error":"older_than_seconds must be non-negative"}` |
| negative `keep_last_per_vm` | `400` | `{"error":"keep_last_per_vm must be non-negative"}` |
| unsupported method on `/snapshots/gc` | `405` | 기존 style의 method error |
| dry-run 후보 없음 | `200` | 빈 `candidates`, `deleted`, `errors` |
| apply 중 일부 삭제 실패 | `200` | `errors`에 snapshot별 error 기록 |

삭제 실패를 전체 API 실패로 올리지 않는 이유는 대량 GC에서 성공한 삭제와 실패한
삭제를 모두 운영자가 볼 수 있어야 하기 때문이다.

## 7. 보안과 불변 조건

- `agent_token`은 GC 응답에 노출하지 않는다.
- GC 응답은 `SnapshotInfo` 수준의 public metadata만 포함한다.
- diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
- 실행 중인 원본 VM의 snapshot restore 금지 규칙은 변경하지 않는다.
- VM 삭제 cleanup 경로, COW restore cleanup 경로, TAP/IP lifecycle은 변경하지
  않는다.
- API 인증은 기존 `authMiddleware`를 그대로 적용한다.

## 8. 테스트 전략

Unit test는 `cmd/goose-daemon/api_test.go`에 추가한다.

테스트 항목:

- dry-run은 snapshot map과 directory를 삭제하지 않는다.
- `older_than_seconds`보다 오래된 snapshot만 후보가 된다.
- `keep_last_per_vm`가 source VM별 최신 snapshot을 보호한다.
- diff가 참조 중인 full snapshot은 후보가 되지 않고 `referenced_by_diff`로
  보호된다.
- `apply: true`는 후보 snapshot을 map과 disk에서 제거한다.
- malformed JSON과 negative policy는 `400`을 반환한다.
- 기존 `DELETE /snapshots/{id}`가 diff dependency 보호를 계속 유지한다.

검증 명령:

```bash
go test ./cmd/goose-daemon
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
```

통합 테스트 `sudo bash e2e_test.sh`는 `/dev/kvm`, root 권한, Firecracker 실행 가능
host, LLM API key가 필요하므로 이 구현의 기본 검증에는 포함하지 않는다. runtime
release 전에 별도 운영 환경에서 실행한다.

## 9. 문서 갱신

다음 문서를 갱신한다.

- `README.md`: snapshot lifecycle API에 `POST /snapshots/gc` 추가
- `RELEASE_NOTES.md`: 수동 snapshot retention/GC 추가 기록
- `docs/architecture/service-logic.md`: GC 계산/적용 로직과 invariant 설명
- `docs/architecture/runtime-architecture.md`: host disk snapshot lifecycle 설명 보강

`CONTEXT.md`의 후속 후보는 이 기능 구현 후 “snapshot retention/GC” 완료 항목으로
정리할 수 있다. 단 `multi-host runtime`, `scheduler`, `quota`, `audit storage`는
계속 후속 후보로 남긴다.

## 10. 구현 순서

1. GC request/response type과 planner helper를 `cmd/goose-daemon/api.go`에 추가한다.
2. planner unit test를 먼저 작성하고 실패를 확인한다.
3. `NewControlPlane`에 `mux.HandleFunc("/snapshots/gc", cp.handleSnapshotGC)`를
   추가한다. Go `ServeMux`는 더 구체적인 pattern을 우선하므로 기존
   `/snapshots/` item route와 충돌하지 않는다.
4. dry-run endpoint test를 작성하고 실패를 확인한다.
5. apply helper와 기존 delete helper 공통화를 구현한다.
6. apply endpoint test를 작성하고 실패를 확인한다.
7. 문서를 갱신한다.
8. 전체 Go test/build 검증을 실행한다.
