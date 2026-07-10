# D1: routed flock 재시작 복구 — 구현 플랜

- 결함 원문: [../../operations/2026-07-10-cross-host-verification-run-handoff.md](../../operations/2026-07-10-cross-host-verification-run-handoff.md) D1
- 브랜치: `fix/routed-flock-restart-recovery`

## 결함 요약 (RCA 완료)

daemon 재시작 시 hub/relay flock이 **일반 local flock으로 강등 복구**된다.
`registerDistributedFlock`/`registerRelayFlock`은 이미 멱등으로 설계돼 있고
(reconcile re-POST가 admission을 복원하는 것이 문서화된 의도 —
orchestrator_api.go:1479-1492 주석), kind 가드(1498, 1548)는 정상 local flock
보호용으로 올바르다. **버그는 영속 계층**: `FlockMetadata`에 Kind/HomeAddr/
Roster가 없고(`persistence.go:15`), `RegisterHub`/`RegisterRelay`(flock.go:450,
492)는 Persist를 호출하지 않는다 → `LoadFromDisk`(flock.go:565)가 kind 없이
복원 → 재등록이 409 → relay hop 401 영구화.

## 수정 설계

**영속 스키마에 분산 메타데이터(비밀 제외)를 추가하고, 등록 경로에서 Persist,
LoadFromDisk에서 복원한다.** 가드·재등록 핸들러는 무변경 — kind가 살아나면
기존 멱등 경로가 그대로 self-heal을 완성한다.

1. `internal/orchestrator/persistence.go` — `FlockMetadata`에 추가:
   - `Kind string \`json:"kind,omitempty"\``
   - `HomeAddr string \`json:"home_addr,omitempty"\``
   - `Roster []RosterMember \`json:"roster,omitempty"\``
   - **RelayToken/CallToken은 절대 추가하지 않는다** (admission 비밀은
     daemon 디스크에 영속 금지 — 기존 보안 불변식. reconcile re-POST가
     adapter placement store의 토큰으로 재주입하는 것이 설계된 복구 경로).
   - `currentSchemaVersion`은 1 유지 (additive optional 필드 — 구버전 파일은
     zero value로 local flock으로 읽히며 이는 기존 동작과 동일). 로드 시
     version 검증 로직이 strict라면 확인 후 그대로 두기.
2. `internal/orchestrator/flock.go`:
   - `ToMetadata()`: Kind/HomeAddr/Roster 복사 (Roster는 방어적 복사).
   - `LoadFromDisk()`: 세 필드 복원.
   - `RegisterHub`, `RegisterRelay`: 등록 직후 `f.Persist(fm.workDir)` —
     실패는 기존 local flock 생성 경로(createFlock)의 persist 실패 처리
     관례를 그대로 따른다 (조사 후 동일하게: 로그 후 진행이면 로그 후 진행).
   - `UpdateHubRoster`: roster 갱신 성공 시 재-Persist (post-spawn enriched
     roster·reconcile 갱신이 재시작을 넘어 살아남도록).
   - relay 재등록(overwrite) 경로도 Persist를 타는지 확인 (RegisterRelay
     재호출이므로 자동 충족).
3. 삭제 경로: flock delete가 `workDir/flocks/{id}/` (metadata.json 포함)를
   제거하는지 확인 — 이미 제거한다면 무변경.
4. `cmd/goose-daemon/recovery.go`: 무변경 목표. `reconcileRecoveredFlockAgent`의
   NewUnregistered 폴백은 "메타데이터가 정말 없는" flock 전용으로 유지.
   LoadFromDisk가 먼저 kind를 복원하므로 routed flock은 폴백에 도달하지 않는다
   (LoadFromDisk가 recovery보다 먼저 실행되는지 daemon 기동 순서 확인 — 아니면
   순서 보장 필요).

### 복구 후 상태 계약 (문서화 대상)

- **hub**: kind=hub + wall(파일에서 재부착, 히스토리 보존) + roster 복원.
  admission(cp.relayTokens/callTokens)은 in-memory라 비어 있음 → reconcile
  re-POST(≤60s)가 setRelayToken/setCallToken으로 복원. 그 사이 relay hop은
  401 (수용된 degraded window).
- **relay**: kind=relay + HomeAddr 복원. f.RelayToken/CallToken 비어 있음 →
  outbound hop은 re-POST까지 실패 (동일 window).

## TDD 테스트 목록 (RED 먼저)

`internal/orchestrator`:
1. `TestFlockMetadata_RoundTripPreservesDistributedFields` — RegisterHub →
   Persist → 새 FlockManager LoadFromDisk → Kind/Roster 보존. RegisterRelay →
   동일 → Kind/HomeAddr 보존.
2. `TestFlockMetadata_NeverPersistsTokens` — RegisterHub/RegisterRelay 후
   metadata.json 파일 바이트에 relay/call 토큰 값 sentinel 부재.
3. `TestLoadFromDisk_LegacyMetadataLoadsAsLocal` — kind 없는 구버전 파일 →
   local flock (회귀 가드).

`cmd/goose-daemon` (handler 레벨, httptest — 재시작은 "같은 workDir로 새
ControlPlane/FlockManager 구성 + LoadFromDisk"로 시뮬레이션):
4. `TestRegisterDistributedFlock_SurvivesDaemonRestart` — 등록 → 재시작 시뮬 →
   re-POST /distributed → **201** (409 아님) + relay token으로 wall post 200
   (admission 복원) + 기존 TOWN_WALL.log 히스토리 보존.
5. `TestRegisterRelayFlock_SurvivesDaemonRestart` — 등록 → 재시작 시뮬 →
   re-POST /relay → **201** + HomeAddr/kind 복원 확인.
6. `TestRegisterDistributedFlock_StillRefusesGenuineLocalFlock` — local flock
   생성(POST /flocks 경로 또는 fm.Register) 후 /distributed → **409 유지**
   (가드 회귀).

## 검증 게이트

- `go build ./cmd/... ./internal/... ./scripts/...` + `go vet` clean
- `go test ./cmd/... ./internal/...` 전체 green + 대상 패키지 `-race`
- 커밋 관례: `fix(runtime)`/`test(runtime)`, **git trailer 금지**
- 최종 수락: 실 2-daemon 환경(192.168.1.19/.20)에서 runbook ⑥만 재검증
  (home 재시작 → reconcile ≤60s → wall/call 재동작; member 재시작 → 동일)
