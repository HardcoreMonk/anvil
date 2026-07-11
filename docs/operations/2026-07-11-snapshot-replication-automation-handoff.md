# Snapshot Replication 자동화 Handoff

- 작성일: 2026-07-11
- 대상 branch: `feature/snapshot-replication-automation`
- 대상: `internal/anvilmcp`(`snapshot_replication.go` sweep 확장,
  `runtime_router.go` reconcile 배선 + in-memory 카운터 필드, `placement_store.go`
  metric 영속, `scheduler_metrics.go` `/metrics` 렌더), `scripts/anvil-snapshot-replication-e2e.sh`
- 상태: 구현·유닛(+`-race`)·KVM e2e 게이트 완료(2연속 green). **실 multi-host(비-stub)
  수동 검증은 미수행**(Follow-Up). 설계:
  [`docs/superpowers/specs/2026-07-11-snapshot-replication-automation-design.md`](../superpowers/specs/2026-07-11-snapshot-replication-automation-design.md)
- 선행: [`docs/operations/2026-06-02-cross-host-snapshot-replication-handoff.md`](2026-06-02-cross-host-snapshot-replication-handoff.md)
  (수동 동기 replication baseline — 이 slice가 "잔여 위험"/"다음 작업"의
  background retry queue·metrics·alert 항목을 해소)

## 무엇이 main에 있나

이 slice는 아직 `feature/snapshot-replication-automation` branch에만 있다 —
main 병합은 "최종 검증(전체 슬라이스)" 통과와 PR 승인 이후에 이뤄진다(자체
머지 금지). 이 handoff는 branch 상태를 기록한다.

baseline(2026-06-02, main에 이미 있음)의 수동·동기 `anvil_replicate_snapshot`
위에 **선언적 reconcile sweep**을 추가한다.

### sweep 메커니즘 (adapter reconcile 확장)

`ReconcilePlacements`(`runtime_router.go:289`)가 매 주기 `reconcileSnapshotReplication`을
호출해 다음을 수행한다.

1. **discover**: probe-reachable daemon마다 `ListSnapshots`를 호출해 실제
   위치를 `PlacementStore.SnapshotLocations`에 add-only union으로 반영한다.
2. **revival reset**: 이전에 dial-실패로 카운트되거나 giving-up 표시된
   (snapshot,target) 쌍 중 이번 pass에서 대상이 reachable로 관측되면 카운터를
   즉시 지운다.
3. **drift heal**: `len(SnapshotHosts) < N`(**상수 N=2**, 원본+복제 1)인
   스냅샷마다 reachable source를 찾고 `SelectRuntimeHost`(tenant/egress carry,
   기존 hosts + terminal/giving-up 대상 exclude)로 target을 고른 뒤 기존
   `ReplicateSnapshot`을 1회 재사용해 시도한다.
   - dial 실패는 in-memory 카운터++, **연속 3회(상수 cap)** 도달 시 giving-up.
   - non-dial 실패(D3 coarse-fs 거부, tenant 불일치 등)는 **terminal**로
     분류돼 그 (snapshot,target) 쌍이 즉시 exclude된다. **이 exclude는
     in-memory(프로세스 수명 한정)라 adapter(`cmd/anvil-mcp`) 재시작이 re-arm**한다 — 영속
     차단 목록이 아니다(무한 재시도 금지라는 스펙 요구와, 복제 GC 비목표라는
     제약을 동시에 만족).
4. **카운터 GC**: dial-failure/giving-up/terminal 세 counter map 모두
   **positive-evidence 규칙**으로만 청소한다 — 어떤 (snapshot,target)의
   기록된 host가 이번 pass에 probe-reachable이면서 그 host의 `ListSnapshots`에
   해당 스냅샷이 없을 때만(진짜 삭제가 관측됐을 때만) 삭제한다. 기록된 host가
   전부 이번 pass에 unreachable이면 아무 것도 건드리지 않는다(naive
   "이번 pass에 안 보이면 삭제" 규칙은 소스가 일시적으로 다 죽었을 때
   포화된 giving-up 마크를 잘못 지워 죽은 대상 재선정을 유발한다 — Task 3
   리뷰에서 발견·수정).
5. **관측**: `queue_depth`/`giving_up` gauge를 매 pass republish한다.

### metric family

`PlacementStore.RecordSnapshotReplicationMetrics`(flock placement metric과
동형)가 `anvil_scheduler_snapshot_replication_*`를 **PlacementStore state에
영속**한다(카운터형이라 파일 성장 유계, flock placement metric과 정합).
scheduler `/metrics`(`renderSnapshotReplicationMetrics`)가 노출:

- `..._attempts_total{outcome,reason}` (counter) — `replicated`/`already_present`/
  `dial_failed`/`terminal_rejected`/`error`/`no_candidate`.
- `..._latency_seconds{phase}` (histogram) — **`phase="total"`만**. 스펙
  D-4는 export/import/total 세 phase를 열거했지만, sweep가 `ReplicateSnapshot`을
  통째로 재사용하는 이상 adapter는 스트림 내부 export/import sub-timing을
  관측할 수단이 없다 — sub-phase 계측을 넣으려면 diff-deps/idempotency/
  location-record 로직을 sweep에서 중복 구현해야 해서 D-3(재사용) 원칙에
  위배된다. 이 편차는 자체 리뷰에서 기록됐고 필요 대두 시 별도 결정 사안이다.
- `..._queue_depth` (gauge) — desired 미달 스냅샷 수.
- `..._giving_up` (gauge) — dial-cap 포화 스냅샷 수.
- `..._last_success_timestamp_seconds` / `..._last_failure_timestamp_seconds` (gauge).

### alert 경계 (D-5)

anvil 경계는 metric 노출 + `docs/operations/runbook.md`의 권장 alert 식
문서화까지다. adapter 안에는 alerting/notification 코드가 없다 — 실
alerting은 zone `project-dashboard`가 scrape/발화한다(YAGNI + thin-adapter
불변식).

## 보안 경계

- metric label은 `outcome`/`reason`/`phase`뿐이다 — host 주소, 토큰, 스냅샷
  비밀은 어떤 label에도 없다(`TestPlacementStoreSanitizesSnapshotReplicationMetricLabels`,
  `TestRenderSchedulerMetricsIncludesSnapshotReplicationMetrics`가 junk
  label 유출 부재를 단언).
- reconcile 로그는 flock/host reconcile 관례와 동일하게 스냅샷 id + host
  **이름**만 남긴다(`TestReconcilePlacements_RunsSnapshotReplicationSweep`가
  daemon 주소 부재를 단언).
- 복제 자체의 redaction(`safeReplicationError`, `sanitizeSnapshotReplicationError`)은
  기존 수동 경로 로직을 그대로 재사용한다 — 새 코드가 별도 경로를 추가하지
  않는다.

## Gate 결과

- **유닛(fake daemon)**: `internal/anvilmcp/snapshot_replication_sweep_test.go`에
  10종(`TestSnapshotReplication_ReplicatesUnderReplicatedSnapshot`,
  `...GivesUpAfterDialFailureCap`, `...DialCounterResetsWhenTargetRevives`,
  `...NoCandidateSurfacesQueueDepth`, `...RestartResetsInMemoryCounters`,
  `...RespectsHostEligibility`, `...TerminalRejectionIsNotRetried`,
  `TestReconcilePlacements_RunsSnapshotReplicationSweep`,
  `...CounterGCRequiresPositiveEvidence`,
  `TestClassifySnapshotReplication_AlreadyPresentDeletesCounters`) — 대상
  후보 0/재시작/tenant 적격성/dial cap+복귀/terminal 비재시도/positive-evidence
  GC를 모두 고정. `internal/anvilmcp/snapshot_replication_metrics_test.go`에
  5종(영속 record/gauge/redaction/`Save` cross-family 보존). `scheduler_metrics_test.go`에
  2종(render + `/metrics` 엔드포인트 노출). 전부 green(Task 1-4 리뷰 Approved).
- **race**: `go test -race ./internal/anvilmcp/` — Task 3(`a3b9d72`)·Task
  4(`6bdd6ef`) 시점 각각 green 확인(`ok ephemera/internal/anvilmcp
  2.0xxs`), 전체 `go test ./...` 회귀도 Task 3에서 전 패키지 green 확인.
  Task 6(문서 전용) 시점에는 재실행하지 않았다 — "최종 검증(전체 슬라이스)"
  단계에서 HEAD(`098ddee`) 기준 재확인이 필요하다.
- **빌드**: `go build ./...`, `go vet ./...`, `gofmt -l`(대상 파일) — Task
  3/4 리포트 기준 clean.
- **KVM e2e**(`scripts/anvil-snapshot-replication-e2e.sh`, Task 5,
  `098ddee`): **2연속 green**. 관측 증거: `attempts{outcome="dial_failed"}=4`,
  `attempts{outcome="replicated"}=2`, `queue_depth` 1→0 수렴, `giving_up`
  포화 관측 후 대상 복귀로 0 복귀, redaction 스팟 체크 clean(스텁 주소
  `127.0.0.1`·토큰 미노출). Review Approved — 자체 리뷰 편차 5건(latency
  phase=total만, terminal 오분류 창, uniform N=2 discovery, Task 2→3 terminal
  재배치, Phase 0 target-down 시퀀싱) 전부 수용, "Phase0 target-down
  시퀀싱은 스펙 정합 우수"로 평가됨.
- **회귀(미실행, 이 handoff 작성 시점)**: `scripts/anvil-cross-host-failover-e2e.sh`,
  `scripts/anvil-cross-host-wall-e2e.sh` — reconcile 경로를 공유 확장했으므로
  회귀 필수. "최종 검증(전체 슬라이스)" 단계 항목이며 이 handoff 작성
  시점(Task 6, 문서 전용)에는 아직 실행하지 않았다.
- **범위 밖(이 handoff 작성 시점 기준)**: 전체 KVM 게이트(`e2e_test.sh`),
  secret-scan, PR 생성은 "최종 검증(전체 슬라이스)" 단계에서 별도 실행 — 이
  handoff는 문서 작업(Task 6) 완료 시점 기록이다.

## Known limitations / 운영 주의

1. **latency phase = `total`만.** 위 "metric family" 절 참고 — export/import
   sub-timing은 D-3 재사용 원칙상 관측 불가.
2. ~~**터미널 오분류 창.**~~ **해소 (Follow-Up 0, `fix/replication-failure-classes`
   branch — 2026-07-11, PR 대기 · main 미병합).** 과거 서술: probe
   reachable 직후 대상이 죽어 `ReplicateSnapshot`이 `(nil,error)`(list
   dial)로 실패하면 재시도 대상인 `error`로 가지만, `(resp,nil)`
   non-success면 전부 `terminal`(exclude)로 갔다 — `ReplicateSnapshot`은
   source-측 export 실패도 같은 `(resp, non-success)` 모양으로 반환하므로
   **source 문제가 무고한 target을 terminal로 오염**시킬 수 있었고,
   pass마다 다음 후보가 차례로 오염되면 그 스냅샷은 no_candidate로
   정체됐다(원인 해소 후에도 adapter 재시작 전까지). metric reason도
   `terminal_rejected`로 일괄 귀속돼 source/일시 장애가 target 거부로
   오표기됐다.

   수정: `ReplicateSnapshot`이 `SnapshotReplicationResponse`에
   `FailureStage`(`source`/`target`/`internal`)·`FailureRejected`(bool)
   필드를 추가해(하위호환 — 기존 `Status`/`Errors` 의미 불변) 각 실패
   지점에서 어느 쪽 책임인지 타입으로 표시한다: export 실패·export
   stream close 실패·source 카탈로그의 diff base 누락은 항상
   `FailureStage=source`; router-local `record_location_failed`은
   `FailureStage=internal`; target-측(import 실패, target 카탈로그의
   diff base 누락)만 `FailureStage=target`이고, 그 중에서도 target의
   명시적 거부(`POST /snapshots/import`의 HTTP 4xx —
   `invalid_snapshot_bundle`/`diff_base_missing`/`snapshot_conflict`,
   `cmd/goose-daemon/api.go` `handleSnapshotImport`)만
   `FailureRejected=true`. `classifySnapshotReplication`은 이제
   `FailureStage==target && FailureRejected`일 때만 terminal 처리하고,
   나머지(source/internal/target-측 5xx·network·decode 등 불명확한
   실패)는 전부 재시도 가능한 `error`로 분류한다(target-측 5xx도
   보수적으로 재시도 — terminal은 확실한 거부만). 이는 원래 설계(D-2:
   "HTTP 4xx/검증 실패... terminal", D-6: D3/tenant/검증 거부만
   terminal-fail)와 정합이다 — 구현이 그 경계를 `(resp,nil)` 전체로
   과확장했던 것이 버그였다.

   metric reason도 분리했다: `error` outcome은 이제 `export_failed`
   (source-측)/`import_failed`(target-측 비확정 실패)/기존
   `transfer_error`(internal, 또는 repErr!=nil의 사전-전송 실패)로
   나뉘고, `terminal_rejected` outcome은 기존 `rejected` reason 그대로
   명시적 거부에만 붙는다(`normalizeSnapshotReplicationReason` 화이트
   리스트 갱신). 유닛 3종 신규/갱신
   (`TestSnapshotReplication_ExportFailureDoesNotTerminalTarget`,
   `...AmbiguousImportFailureIsNotTerminal`, 기존
   `...TerminalRejectionIsNotRetried`을 실제 데몬 거부 형태
   (`*DaemonError` 4xx)로 갱신) — `go test -race ./internal/anvilmcp/`
   green, 전체 `go test -race ./internal/... ./cmd/...` 회귀도 green
   (KVM e2e는 이 follow-up 범위 밖 — 로컬 KVM을 다른 슬라이스가 점유해
   controller가 KVM 확보 후 별도 재확인). 상세:
   `internal/anvilmcp/snapshot_replication.go`,
   `snapshot_replication_metrics.go`, `runtime_router.go`.
3. **uniform N=2 discovery.** discovery가 모든 reachable host의 모든
   스냅샷을 add-only 수집하므로 base/throwaway 스냅샷도 N=2로 수렴한다 —
   첫 sweep 트래픽이 클 수 있으나 bounded/idempotent.
4. **queue_depth가 삭제된-but-기록잔류 스냅샷도 계상한다.** `SnapshotLocations`가
   add-only이고 `DELETE /snapshots/{id}`가 이를 정리하지 않아, 스냅샷이
   모든 host에서 실제 삭제된 뒤에도 옛 위치 기록이 남아 `queue_depth`에
   잡힌다. 재시작으로도 해소되지 않는다(영속 상태 자체의 특성). 상세 +
   운영 대응: `docs/operations/runbook.md`의 "자동 복제 sweep" 절, "queue_depth
   캐비앗" 문단.
5. **실 multi-host(비-stub) 수동 검증 미수행.** KVM e2e는 단일 물리 호스트 +
   python stub target(failover/wall e2e와 동일한 bridge-collision 회피
   구조)으로 sweep 로직·metric·redaction을 검증했지만, 두 개의 실제 daemon
   사이에서 실제 네트워크 순단·복귀를 관측하는 수동 검증(failover 슬라이스의
   §6b에 해당하는 절차)은 아직 없다.

## Next Action

1. "최종 검증(전체 슬라이스)" 수행: `go build ./... && go vet ./... &&
   gofmt -l . | grep -v '^web/' ; go test -race ./internal/... ./cmd/...`,
   기존 cross-host e2e 회귀(`anvil-cross-host-failover-e2e.sh`,
   `anvil-cross-host-wall-e2e.sh`), 전체 KVM 게이트(`e2e_test.sh`),
   secret-scan.
2. PR 생성(`feature/snapshot-replication-automation` → `main`). **자체 머지
   금지** — 머지는 사용자 승인으로만.
3. PR 승인·머지 후: 실 multi-host 수동 검증 절차를 별도로 설계해 수행(현재
   `docs/operations/2026-07-08-cross-host-manual-verification.md`에는
   snapshot replication 전용 섹션이 없다 — 신설 여부는 별도 결정).

## Follow-Up Tasks

0. ~~**(최종 whole-branch 리뷰 파생) transfer 실패 분류 정밀화**~~ **완료
   (branch `fix/replication-failure-classes`, 2026-07-11, 사용자 승인
   하 착수 — PR 대기 · main 미병합).** export/일시 전송 실패를 재시도
   가능한 `error`로, 진짜 target 거부(HTTP 4xx)만 terminal로 라우팅:
   `ReplicateSnapshot`이 typed `FailureStage`/`FailureRejected`를
   `SnapshotReplicationResponse`에 추가(하위호환)해 분류하고, metric
   reason도 `export_failed`/`import_failed`/`rejected`로 분리. 상세는
   Known limitations #2(해소로 갱신) 참고. 이 branch 자체는 아직 별도
   PR·리뷰·머지 전이므로, "최종 검증(전체 슬라이스)" Next Action의
   범위에는 아직 들어가지 않는다 — 별도 PR로 진행.
1. **실 multi-host 수동 검증 수행** — KVM e2e(단일 호스트 + stub)가 아닌
   두 개의 실 daemon 사이에서 스냅샷 down/복귀·`queue_depth`/`giving_up`
   전이를 관측. 절차 신설 필요(§6b 유사 구조), 별도 승인 후 착수.
2. **zone `~/projects/claude-zone/docs/FOLLOWUP.md` P1-09 "C replication
   자동화" 항목 갱신** — zone repo는 이 anvil branch 밖이므로 이 handoff에는
   트리거만 기록한다. 현재 그 항목은 "설계 리뷰 승인 ... 플랜 초안 작성
   중"으로 남아 있어 stale — Tasks 1-6 구현·문서 완료(PR 대기)로 갱신
   필요.
3. **release 단계 zone 인벤토리 동기화** — PR 머지 후 release 단계에서
   `ops/units.yaml`/`ops/projects.yaml`/`wiki/entities/`(필요 시) 갱신.
   이 slice는 기존 `anvil-scheduler`/adapter unit·경로를 재사용하므로 신규
   systemd unit은 없지만, feature 목록 갱신 여부는 release 단계에서 판단.
4. **queue_depth 삭제-스냅샷 잔류 계상**(Known limitations #4) — 지금은
   runbook 캐비앗 문서화로 대응한다. 실사용에서 문제가 되면 `SnapshotLocations`
   삭제-전파(replica GC) 자체를 별도 slice로 검토할지 결정 — 이 slice의
   명시 비목표를 재litigate하는 것이므로 신중한 별도 승인 필요.
