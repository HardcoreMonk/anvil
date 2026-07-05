# ANNOTATIONS — v0.4.x 학습 주석 브랜치

## 이 브랜치의 목적

`annotate/v0.4.5` 브랜치는 **참고 전용(reference-only)** 이다. 절대 `main`이나 다른
작업 브랜치에 병합하지 않는다. 목적은 딱 하나: anvil이 upstream ephemera
`v0.4.0`부터 `v0.4.5`까지를 흡수했던 시점의 코드를 읽는 사람이, 코드를 바꾸지 않고도
"이 코드가 왜 이렇게 동작하는가"를 한국어 주석만 보고 이해할 수 있게 하는 것이다.

기준 커밋: `8daf6f3` — `feat(runtime): adapt ephemera v0.4.5 restore recovery`
(anvil이 upstream ephemera `v0.4.0`–`v0.4.5`를 적응 완료한 시점).

이 브랜치의 모든 커밋은 **주석 추가(및 이 파일 하나) 외의 변경을 포함하지 않는다.**
Go 코드는 `//` 라인만, 셸 스크립트는 `#` 라인만 추가했다. 기존 코드 라인은 한 글자도
바뀌지 않았다 — 재정렬, 재포맷, 공백 변경, 삭제 전부 없음.

## 시리즈 개요: v0.4.0 storage/recovery → v0.4.5 restore recovery

upstream ephemera `v0.4.x` 시리즈는 "런타임 안정화"에 집중한 시리즈다. 각 태그가
다룬 주제:

| 태그 | 주제 | anvil 분류 |
|---|---|---|
| v0.4.0 | disk pre-flight, COW cold-restart, true rootfs diff snapshot | adapted |
| v0.4.1 | client identity, access audit, per-token TTL, `ephemera-ctl` | adapted |
| v0.4.2 | dm-snapshot COW spawn (probe/fallback), COW+diff 조합 | adapted (default flip은 deferred) |
| v0.4.3 | 동적 flock 멤버십(add/remove/role), pause/resume, Town Wall 필터 | adapted |
| v0.4.4 | streaming `/tasks`, nested-task depth guard, watchdog status, flock broadcast | adapted (broadcast MCP 노출은 deferred) |
| v0.4.5 | 스냅샷-restore VM의 데몬 재시작 자동 복구 | adapted |

즉 이 시리즈는 "VM을 어떻게 디스크에 싸게 복제하고(storage), 데몬이 죽었다 살아나도
어떻게 그 VM을 되살리는가(recovery)"라는 하나의 축으로 요약된다. v0.4.0이 COW 클론과
콜드 리스타트를 놓았고, v0.4.5가 거기에 "스냅샷에서 restore된 VM"까지 복구 대상에
포함시키며 이 축을 완성했다.

anvil은 이 기능들의 런타임 가치는 그대로 채택하되(`adapted`), 다음 지점에서 upstream과
의도적으로 다르게 동작한다(자세한 근거는 `docs/analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md`,
cross-phase 종합은 `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md` 참고):

- **토큰 redaction**: `agent_token`은 `POST /vms` 응답 외에는 절대 노출하지 않는다
  (flock add-agent 응답에서도 생략).
- **tenant/egress 보존**: `state.json`/`SnapshotMetadata`에 anvil이 추가한
  `TenantID`/`EgressPolicy` 필드가 재시작·복구·restore 전 구간에서 유지된다.
- **409 삭제 보호**: diff의 base, 그리고 살아있는(라이브 + 영속) restored VM이 참조하는
  source snapshot은 삭제/GC 모두에서 `409`로 막는다 — upstream e2e 46c가 허용하는 고아
  스냅샷 `200` 삭제를 anvil은 의도적으로 거부한다.
- **broadcast의 MCP 비노출**: daemon HTTP API로는 채택하되 `anvil_*` MCP tool로는
  아직 노출하지 않는다(tenant/rate/audit 설계 전까지 deferred).

## 주석 단 파일 목록

| 파일 | 한 줄 요약 |
|---|---|
| `internal/storage/provisioner.go` | golden image 자가 빌드, COW 클론(`CloneDiskCOW`), VM별 파일 주입(`injectVMFiles`)의 디스크 준비 계층. |
| `internal/storage/vm_state.go` | 콜드 리스타트 복구 계약의 소스 오브 트루스인 `state.json`의 스키마와 원자적 저장/조회. |
| `internal/storage/snapshot.go` | 스냅샷 메타데이터, dm-snapshot COW / bind-mount 두 가지 restore 메커니즘, diff 병합. |
| `cmd/goose-daemon/recovery.go` | 데몬 재시작 시 spawn VM 콜드 리스타트 + COW 재구성 + v0.4.5 restored VM 재복구(`recoverRestoredVM`/`reRestoreMachine`). |
| `cmd/goose-daemon/api.go` | restore 핸들러(`meta.DiskPath`/`meta.VsockPath` 계약), snapshot 삭제/GC의 409 참조 보호, depth guard, `DestroyAll` graceful-shutdown 분기. |
| `cmd/goose-daemon/orchestrator_api.go` | 단일 호스트 flock 라이프사이클(add/remove/role/pause/resume/broadcast) HTTP 계층. |
| `internal/orchestrator/watchdog.go` | flock 멤버 VM `/health` 폴러 — paused 예외 처리, dead 마킹, `GET /watchdog/status`. |
| `cmd/goose-agent/main.go` | in-guest 에이전트: buffered `/tasks` 기본 계약 vs `?stream=1` NDJSON, task depth 전달. |
| `scripts/gtcall` | golden image 안에서 flock 동료를 호출하는 CLI — depth 헤더 재전송. |

## 참고 자료

- `docs/analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md` — 이 시리즈의
  anvil 예비 채택 분류(이 워크트리 기준).
- `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md` — 최종 브랜치
  (`anvil-ephemera-parity`)의 cross-phase parity matrix. v0.4.x 항목의 최종 확정
  근거(merge/adapt commit, guard test 이름)를 함께 담고 있다.
- `RELEASE_NOTES.md`의 `v0.4.1`–`v0.4.5` 섹션 — 각 태그의 상세 변경 목록과 검증 커맨드.

## 읽는 순서 제안

1. `internal/storage/vm_state.go` — `state.json`이 무엇을 기록하는지부터 파악한다.
2. `internal/storage/provisioner.go` → `internal/storage/snapshot.go` — 디스크가
   어떻게 준비되고(spawn) 어떻게 스냅샷/restore되는지.
3. `cmd/goose-daemon/recovery.go` — 데몬이 죽었다 살아날 때 위 두 파일의 정보를 어떻게
   조합해 VM을 되살리는지.
4. `cmd/goose-daemon/api.go` — 위 복구 계약이 실제 HTTP 핸들러(restore/snapshot
   delete/GC)에서 어떻게 강제되는지.
5. `cmd/goose-daemon/orchestrator_api.go` + `internal/orchestrator/watchdog.go` —
   여러 VM을 flock으로 묶었을 때의 라이프사이클과 헬스 모니터링.
6. `cmd/goose-agent/main.go` + `scripts/gtcall` — VM 내부에서 이 모든 것을 사용하는
   쪽의 시점.
