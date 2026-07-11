# Cross-host snapshot replication 자동화 설계 (background retry queue + metrics + alert)

- 작성일: 2026-07-11
- 상태: **설계 확정 (2026-07-11 사용자 설계 리뷰 승인)** — 골격(adapter reconcile
  확장·sweep 중심·bounded 카운터·terminal 분류) + replica factor **N=2 상수** +
  세부 3건(coarse-fs terminal-fail 수용 / metric PlacementStore 영속 / alert 경계 =
  metric+runbook 문서) 일괄 승인. 보강 1건: giving-up 카운터는 대상 host 복귀
  관측 시 리셋(failover 재평가 패턴). 아래 "결정" 절의 권고가 곧 확정안이다.
- 선행 baseline (이미 shipped): 수동 동기 replication + snapshot locality preference
  (`CONTEXT.md:52-54`, `:228`; handoff `docs/operations/2026-06-02-cross-host-snapshot-replication-handoff.md`).
- 배경 backlog: `CONTEXT.md:433-434` — "cross-host snapshot replication 자동화(background
  retry queue·metrics·alert — 수동 동기 replication과 snapshot locality preference는
  baseline 포함)". 세 항목 모두 아직 미구현. handoff `:63,:77`이 "후속 후보"로 명시.
- 관련 코드: `internal/anvilmcp/snapshot_replication.go`, `runtime_router.go`,
  `placement_store.go`, `home_failover.go`, `scheduler_metrics.go`,
  `internal/storage/snapshot.go`(D3 read-side guard), `cmd/goose-daemon/relay_retry.go`.
- 설계 선례: home-failover 슬라이스(`docs/superpowers/specs/2026-07-08-home-failover-design.md`),
  bounded relay retry(`docs/superpowers/specs/2026-07-08-bounded-relay-retry-design.md`).

---

## 문제

현재 cross-host snapshot replication은 **수동·동기**다:
`anvil_replicate_snapshot` MCP 호출 → `Tools.ReplicateSnapshot`
(`internal/anvilmcp/tools.go:784`) → `RuntimeRouter.ReplicateSnapshot`
(`snapshot_replication.go:34`)가 source→target을 export/import 스트림으로 한 번
연결하고, 성공 시 `recordSnapshotLocation`(`snapshot_replication.go:176`) →
`PlacementStore.SetSnapshotLocation`(`placement_store.go:678`) + `Save`(`:172`)로
locality를 갱신한다. 이 위치 정보는 이후 restore 스케줄링에서 locality preference로
소비된다(`runtime_router.go:126`, `scheduler_service.go:166` → `SelectRuntimeHost`
`tenant_policy.go:155`, 선호 패스 `:198-207`).

한계:
- **일회성**: 대상 호스트가 down이면 그 순간 실패로 끝난다. 재시도·복구 없음
  (handoff `:63` "첫 버전은 수동 동기 replication이다. background retry queue,
  metrics, alert는 후속 후보로 남는다.").
- **관측 불가**: 복제 성공/실패/지연/미달 복제본 수를 노출하는 metric이 없다
  (handoff `:77` "replication metrics와 alert 설계"가 미완).
- **수동 트리거**: 스냅샷을 만들어도 복제본이 자동으로 생기지 않아 단일 호스트
  소실 시 스냅샷이 사라진다.

목표: 스냅샷이 목표 복제본 수를 갖도록 **선언적·수렴적으로** 유지하는 자동화 계층을
adapter 기존 규율(reconcile idempotency, bounded retry, 단일 control plane) 안에서
추가하고, 상태를 scheduler `/metrics`로 노출한다. anvil 경계에서는 **metric 노출 +
문서화**까지가 alert의 몫이다(실 alerting은 zone 대시보드 소비).

---

## 결정 (2026-07-11 확정 — 옵션 분석은 기록 보존)

### D-1. 큐의 소유자 → **권고: adapter(RuntimeRouter) reconcile 루프 확장**

| 옵션 | 근거 | 판정 |
|------|------|------|
| **(A) adapter / RuntimeRouter** | PlacementStore(`SnapshotLocations` 소유, `placement_store.go:109`), `daemons` map, `reconcileMu` 직렬화, "desired state로 heal" 패턴(`reconcileRoutedFlockWalls` `runtime_router.go:403`, `failoverRoutedFlock` `home_failover.go:75`)을 이미 전부 가진 유일 지점. failover와 동일한 "adapter=단일 control plane" 불변식(`AGENTS.md:88-89`, `CONTEXT.md:11-14,288-293`). | **권고** |
| (B) scheduler service | `SchedulerService`는 PlacementStore 위 얇은 stateless HTTP 표면(`scheduler_service.go`)이며 cross-host 전송용 daemon client를 소유하지 않는다. | 기각 |
| (C) daemon | daemon은 자기 host 아티팩트만 소유. cross-host 토폴로지 결정은 thin-adapter 불변식 위반(`AGENTS.md:88-89`). | 기각 |

권고 근거: 자동 복제는 "스냅샷 S는 N개 host에 있어야 한다"는 **desired state**이고,
reconcile이 실제 위치(`SnapshotHosts`)와 desired set의 drift를 계산해 heal하는 형태가
failover의 "desired HomeHost 영속 → reconcile heal"과 정확히 대칭이다.

### D-2. 재시도 정책 → **권고: reconcile-idempotent 재시도 + bounded in-memory 실패 카운터**

| 옵션 | 근거 | 판정 |
|------|------|------|
| (A) in-request bounded retry만 (relay retry식 3회) | host-down 창(threshold×interval)을 넘겨 살아남지 못함 — 자동화의 핵심 목적 미달. relay retry는 `relayRetryAttempts=3`, backoff `{1s,2s}`, dial-only(`cmd/goose-daemon/relay_retry.go:20,22`)로 **단일 요청 내부** 성질. | 부분 재사용 |
| **(B) reconcile 주기마다 drift heal 재시도 + bounded 연속-실패 카운터** | import은 이미 idempotent("already_present"→`Skipped`, `snapshot_replication.go:160-163`)이므로 재-시도 안전. failover의 `homeFailures map[string]int`(reconcileMu 가드, `runtime_router.go:47`)와 `homeFailureThreshold=3`(상수, `home_failover.go:29`) 패턴을 그대로 복제. | **권고** |
| (C) 영속 job queue + worker (async 버퍼) | **이미 기각된 선례** — async relay buffer 2026-07-11 사용자 결정으로 기각 확정(`CONTEXT.md:435-440`). 재-litigate 금지. | 기각 |

권고 상세:
- **영속되는 것**: desired replica set(어느 스냅샷이 몇 개 복제본을 원하는지)과
  각 스냅샷의 현재 위치(`SnapshotLocations` 이미 영속·atomic write chmod 0600,
  `placement_store.go:288-318`). 재시작 생존 = 영속된 desired/actual 비교로 자동 재개.
- **영속 안 하는 것**: (snapshot,target)별 **연속 실패 카운터**는 in-memory
  (reconcileMu 가드). failover의 `homeFailures`와 동일 — 재시작 시 0으로 리셋되어
  다음 주기에 한 번 더 시도할 뿐, 무해하고 idempotent가 흡수.
- **dial-class 게이팅**: `isDialError`(`home_failover.go:17`) 재사용. dial 실패만
  "재시도할 일시 장애". HTTP 4xx/검증 실패(D3 거부, tenant 불일치 등)는 **terminal**
  — 무한 재시도 금지, 별도 metric reason으로 surface하고 카운터에서 제외.
- **bounded cap**: K=3회 연속 dial 실패 시 "degraded/giving-up"으로 표시하고 hammering
  중단, metric으로 노출. 상수(YAGNI, `homeFailureThreshold`와 동일 방침).
- **giving-up 리셋 (설계 리뷰 보강)**: giving-up은 영구 아님 — reconcile probe가
  해당 대상 host의 복귀(reachable)를 관측하면 그 (snapshot,target) 카운터를 리셋해
  다음 sweep가 재시도한다. failover의 "포화 카운터가 후보 복귀 시 즉시 발화"와
  동일한 재평가 규율.

### D-3. 트리거 & 대상 선정 → **권고: reconcile sweep가 desired-set 보증, 선정은 locality/용량 재사용**

- **트리거**: 두 지점 후보 — (a) `CreateSnapshot`(`tools.go:595`) 성공 시 desired
  intent 기록, (b) reconcile sweep가 "복제본 < N인 모든 스냅샷"을 매 주기 heal.
  권고는 **(b) 중심 + (a)는 즉시성 optional**. (b)만으로도 수렴 보장되고 트리거가
  reconcile 단일 경로로 모임(failover와 동형). `CreateSnapshot`은 `t.daemon` 위 thin
  tool이라 host 인벤토리·용량·locality를 모른다 — 선정 로직을 여기 넣으면 thin-adapter
  경계가 흐려짐.
- **대상 선정 규칙**: source 제외, healthy·용량 충족·tenant/egress 적격 host 중
  locality 우선. `SelectRuntimeHost`(`tenant_policy.go:155`) 적격성 로직 재사용
  (SmokeOnly host는 preferred일 때만 적격 `:186`). coarse-fs 대상은 사전 제외 검토
  (D-6 참조).
- **desired replica factor**: 옵션 — (a) 전역 상수(예: 총 2본 = 원본+복제 1) vs
  (b) per-tenant/per-snapshot 설정. 확정 **(a) 상수 N=2**(원본+복제 1 — YAGNI, threshold류와 동일,
  `ADR_INDEX.md:48`). 실제 요구 발생 시에만 설정화.
- **수동 API 관계**: `anvil_replicate_snapshot`은 명령형 escape hatch로 유지(현행대로
  location도 갱신). 자동화는 그 위의 선언적 desired-state 계층 — 둘은 같은
  `SetSnapshotLocation`/`SnapshotHosts` 상태를 공유해 상호 정합.

### D-4. Metrics → **권고: scheduler `/metrics`에, flock placement metric 패턴 재사용**

- **어디에**: scheduler `/metrics`(`scheduler_service.go:49,69-76`), Prometheus text
  0.0.4(`scheduler_metrics.go:10`). daemon 아님 — 큐 소유자가 adapter이고 상태는
  PlacementStore에 있으므로 노출도 scheduler service가 맡는 것이 정합.
- **무엇을** (`anvil_scheduler_flock_placement_*` 미러):
  - `anvil_scheduler_snapshot_replication_queue_depth` (gauge) — desired 미달 스냅샷 수.
  - `anvil_scheduler_snapshot_replication_attempts_total{outcome,reason}` (counter) —
    replicated/already_present/dial_failed/terminal_rejected 등. outcome/reason 어휘는
    `flock_placement_metrics.go:11-31` 패턴 재사용.
  - `anvil_scheduler_snapshot_replication_latency_seconds{phase}` (histogram) —
    **`total`만** (플랜 검수 정정 2026-07-11: D-3의 `ReplicateSnapshot` 재사용
    지시상 adapter가 export/import sub-timing을 관측할 수단이 없음 — sub-phase
    계측은 재사용 원칙을 깨는 로직 중복이라 기각, 필요 대두 시 별도 결정).
    버킷은 `flockPlacementLatencyBuckets`(`:43`) 재사용.
  - `..._last_success/last_failure_timestamp_seconds` (gauge).
  - `..._giving_up` (gauge) — bounded cap 도달 스냅샷 수(D-2).
- **기록 경로**: `PlacementStore.RecordSnapshotReplicationMetrics`(신설, `:147`
  `RecordFlockPlacementMetrics` 미러), state는 PlacementStore JSON에 영속(disk
  read-merge 패턴 `flock_placement_metrics.go:157-165`). 렌더는 `renderFlockPlacementMetrics`
  (`scheduler_metrics.go:37`)와 동형 함수 추가.
- **확정 (2026-07-11)**: replication metric은 PlacementStore state에 **영속** —
  flock placement metric과 정합, 카운터형이라 파일 성장 유계.

### D-5. Alert → **권고: anvil 경계 = metric 노출 + 문서화까지**

- anvil은 Prometheus metric을 노출하고, zone `project-dashboard`가 scrape/alert하는
  기존 분업을 유지. adapter 안에 alerting/notification 코드를 넣지 않는다(thin-adapter +
  YAGNI).
- 산출물: (1) D-4 gauge/counter 노출, (2) runbook에 권장 alert 식 문서화(queue_depth가
  지속 >0, giving_up >0, last_success staleness), (3) in-adapter 알림 없음.

### D-6. D3 가드 / tenant / redaction 상호작용

- **D3**: read-side 가드가 이미 복제/임포트 유입 coarse diff를 방어
  (`internal/storage/snapshot.go:435 overlaySparseDiff`, 조건 `:437`
  `holeGranularityFn(dir) > HoleGranularityFine`(=4096, `hole_granularity.go:15`),
  에러 `:439` "...refusing overlay to avoid guest memory corruption (see D3)";
  runbook `docs/operations/runbook.md:435-471`, 특히 `:451` "복제/임포트로 유입된
  coarse diff까지 방어"). 자동화는 D3 거부를 **terminal(비재시도) 실패**로 분류
  — 같은 coarse-fs 대상에 재시도 무의미. 별도 reason(`coarse_fs_rejected`)으로 metric.
  가능하면 선정 단계에서 coarse 대상 사전 제외(관측 가능 시), 아니면 terminal-fail 수용.
  (참고: creating-side는 이미 diff→full 강등 `runbook.md:444-448`; read-side가 잡는
  케이스는 fine source → coarse target의 cross-fs 복제.)
- **tenant/egress**: 복제는 스냅샷의 tenant 범위를 보존해야 한다. 수동 경로는
  `tools.go:785`에서 tenant 해석. 자동 경로도 스냅샷 소유 tenant를 동등하게 carry —
  tenant-blind 실행 금지. 대상 선정은 tenant/egress 적격성 존중(`SelectRuntimeHost`가
  이미 TenantID/EgressPolicy carry).
- **redaction**: 큐/metric/log 출력은 기존 redaction 규율 준수 —
  `safeReplicationError`(`snapshot_replication.go:196`),
  `sanitizeSnapshotReplicationError`(`tools.go:873`) 재사용. metric label은
  저-cardinality identifier(outcome/reason/phase)만, host 주소·토큰·스냅샷 비밀 금지.
  reconcile 로그는 flock/host 식별자만(`reconcileRoutedFlockWalls` 규율,
  `runtime_router.go:401-402`).

---

## 메커니즘 (권고안 기준 요약)

1. **Desired state**: 스냅샷별 목표 복제본 수(상수 N). 실제 위치는 기존
   `SnapshotLocations`(영속). desired가 명시 저장이 필요하면 PlacementStore state에
   최소 필드 추가(모든 스냅샷 균일 N이면 저장 불필요, sweep가 `SnapshotHosts` 길이로
   판정).
2. **Reconcile sweep** (`ReconcilePlacements` `runtime_router.go:289` 확장, reconcileMu
   직렬화): 각 스냅샷에 대해 `len(SnapshotHosts) < N`이면 대상 선정 → export/import
   1회 시도(`ReplicateSnapshot` 재사용) → 성공 시 `SetSnapshotLocation`+`Save`,
   metric 기록.
3. **실패 처리**: dial 실패 → in-memory (snapshot,target) 카운터++, cap 도달 시
   giving-up 표시. non-dial/terminal(D3·tenant·검증) → 즉시 terminal reason 기록,
   재시도 안 함. 성공/도달 시 카운터 0.
4. **관측**: `PlacementStore.RecordSnapshotReplicationMetrics` → scheduler `/metrics`.
5. **수렴**: 매 주기 idempotent 재평가 — host 복귀 시 다음 sweep가 자동 복구
   (failover와 동일 수렴 규율).

---

## 경계 사례

- **대상 후보 0** (모든 타 host down / 단일-host 클러스터) → no-op, queue_depth로 surface,
  다음 주기 재평가.
- **재시작 중 부분 복제** → desired/actual 영속 비교로 재개; in-memory 카운터만 리셋.
- **동시 수동 + 자동** → 같은 `SnapshotHosts` 상태 공유, import idempotent라 이중 복제도
  "already_present"로 흡수.
- **D3 coarse 대상** → terminal-fail, 무한 재시도 없음(D-6).
- **스냅샷 삭제 후** → 자동화는 "≥N 보장"만; 복제본 GC/전파는 비목표(현행 미구현,
  handoff `:68`). 삭제된 스냅샷은 sweep 대상에서 제외(카운터 sweep은 failover의
  `homeFailures` 청소 패턴 `runtime_router.go:480-486` 재사용).
- **diff 스냅샷** → base full 선복제 의존성은 기존 `ReplicateSnapshot`
  `IncludeDependencies` 로직 재사용(`snapshot_replication.go:107-126`).
- **다중 adapter 인스턴스** → PlacementStore last-writer-wins + 선정 결정론으로 수렴
  (failover 경계 사례와 동일 논거).

---

## 테스트 (구현 시, TDD)

- **유닛(fake daemon)**: (a) 복제본 미달 스냅샷 → sweep가 대상 선정 후 import 발화 +
  `SetSnapshotLocation` 단언; (b) dial 실패 K회 → giving-up 표시, K-1회는 재시도 지속;
  (c) 성공 개재 시 카운터 리셋; (d) D3/terminal 실패 → 재시도 0회 + terminal reason
  metric; (e) 후보 0 → no-op + queue_depth; (f) 재시작(카운터 리셋) 후 수렴;
  (g) tenant/egress 부적격 대상 제외.
- **metrics 유닛**: `RenderSchedulerMetrics` 확장 렌더 문자열이 Prometheus 0.0.4
  형식·label cardinality 규약 준수(`scheduler_metrics_test.go` 패턴).
- **redaction 유닛**: metric label/log에 host 주소·토큰 부재 단언(기존 sanitize 테스트
  패턴).
- **KVM e2e**: 실 호스트에서 스냅샷 생성 → 대상 down → 복귀 후 자동 복제 관측,
  `/metrics`에서 attempts_total·last_success 확인.

---

## 문서 반영 (구현 시)

- `CONTEXT.md:433-434` backlog 항목을 "구현됨"으로 이동, 완료 상태 목록(`:209-228`) 갱신.
- runbook: 자동 복제 sweep 동작·bounded cap·권장 alert 식(D-5) 문서화. D3 상호작용
  주석(`runbook.md:435-471`)과 교차 참조.
- handoff `2026-06-02-cross-host-snapshot-replication-handoff.md`의 "잔여 위험/다음 작업"
  해소 반영. 새 slice handoff 작성.
- ADR_INDEX: 자동화 계층의 정책 상수(replica factor·retry cap) YAGNI 근거 기록
  (`ADR_INDEX.md:48` 패턴).
- PUBLIC_RELEASE_BOUNDARY: 새 metric 표면이 공개 경계에 미치는 영향 검토.

---

## 비목표 (명시)

- **무손실 보장 / 동기 복제 ack**: 복제는 best-effort eventual. 스냅샷 생성이 복제본
  완료를 기다리지 않는다. (강한 durability는 별개 문제 — YAGNI, home-failover의
  "wall 손실 수용" 정신과 정합.)
- **cross-region**: host는 동일 LAN(고정 `10.0.1.0/24` net 계약 `CONTEXT.md:156-164`).
  WAN/region 토폴로지·지연 보상 없음.
- **복제 토폴로지 자동 재배치 / 복제본 GC**: host join/leave 시 replica 이동·리밸런스,
  스냅샷 삭제 시 복제본 전파 삭제 — 미포함. replicated-delete 전파는 이미 미구현 명시
  (handoff `:68`). "≥N 보장"만.
- **unbounded async job queue / worker pool**: 기각 선례(async relay buffer,
  `CONTEXT.md:435-440`). reconcile-idempotent + bounded로 대체.
- **정책 설정화**(replica factor / retry cap / threshold 설정 노출): YAGNI, 상수 유지
  (`homeFailureThreshold` 방침, `ADR_INDEX.md:48`).
- **in-adapter alerting/notification**: metric 노출 + 문서화까지가 anvil 경계(D-5).
  실 alerting은 zone 대시보드 소비.
- **cross-tenant 복제**: tenant 경계 보존, 다른 tenant로의 복제 없음.

---

## 결정 기록 (2026-07-11 사용자 설계 리뷰)

초안의 미결 질문 7건 전부 해소:

1. 큐 소유자 = **adapter reconcile 확장** 승인.
2. 재시도 = **reconcile-idempotent + in-memory bounded 카운터, cap 3** 승인 +
   보강: **대상 host 복귀 관측 시 giving-up 리셋** (D-2 참조).
3. 트리거 = **sweep 중심(b) 단독** — CreateSnapshot 훅(a) 병행 없음 (YAGNI).
4. replica factor = **전역 상수 N=2**.
5. coarse-fs 대상 = **사전 제외 없이 terminal-fail 수용** (원격 fs granularity를
   adapter가 관측할 수단 없음 — D3 read 가드가 방어).
6. metric 영속 = **PlacementStore 영속** (flock placement 정합).
7. alert 경계 = **metric 노출 + runbook 권장 alert 식 문서화까지** (실 alerting은
   zone 대시보드 몫).
