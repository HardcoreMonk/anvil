# Snapshot Replication 자동화 — 실 2-daemon(비-stub) 수동 검증 수행 기록 (2026-07-11)

- 상태: **PASS — snapshot replication 자동 sweep의 실 multi-host(비-stub) 수동
  검증 완료.** 자동 복제(원본+복제 1)·down 시 dial-cap giving-up(전송 미발화)·
  복귀 시 revival reset·자동 수렴·metric 전이·redaction·정리 전부 **두 개의 실
  anvil-daemon** 사이에서 관측. 신규 결함 회부 없음. (KVM e2e가 단일 물리 호스트
  + python stub target으로 sweep 로직을 검증한 것과 달리, 여기서는 host-b가 실
  daemon으로서 import bundle을 실제로 수신·저장·재서빙한다.)
- 절차: 신설(이 문서). 구조는 §6b failover 수행 기록과 동형 —
  [2026-07-11-6b-failover-verification-run.md](2026-07-11-6b-failover-verification-run.md).
- slice handoff: [2026-07-11-snapshot-replication-automation-handoff.md](2026-07-11-snapshot-replication-automation-handoff.md)
  (Follow-Up 1 "실 multi-host 수동 검증 수행" — 이 run이 완료).
- 선행 baseline: [2026-06-02-cross-host-snapshot-replication-handoff.md](2026-06-02-cross-host-snapshot-replication-handoff.md)
  (수동·동기 `anvil_replicate_snapshot`).
- 대상 커밋: **`aa2b0b0`** (main, PR #43 `feature/snapshot-replication-automation`
  병합 — `aa2b0b0b1092b4012d5d8ac0b900bb98682237e1`). 양 host 동일 소스에서
  host별 빌드 — `anvil-daemon` sha256
  **`17c8d2da241de9a0fc089437c2414d98e2487e09ede19ace660c8c181fc658b3`** 양 host
  바이트 동일(재현 빌드 확인).

## 환경 (계획 대비 편차 포함)

| | host-a (snapshot source) | host-b (replication target) | 워크스테이션 (control plane) |
|---|---|---|---|
| 주소 | 192.168.1.19 (PureCVisor-PROD-1) | 192.168.1.20 (PureCVisor-Prod-2) | 192.168.1.18 |
| OS / kernel | Ubuntu 24.04.4 / 6.8.0-111 | Ubuntu 24.04.4 / 6.8.0-134 | — |
| 루트 fs | root-on-ZFS + `rpool/anvil-snapshots`(recordsize **4K**)→`~/anvil/snapshots` | 동일 | ext4 |
| Go | 1.26.2 (`/opt/anvil-go`) | 동일 | 1.26.2 |
| 역할 | daemon (auth-on, bind `0.0.0.0:3000`, root — 실 KVM) | 동일 | adapter(`cmd/anvil-mcp`) + reconcile 드라이버 + read-only `/metrics` 렌더러 |

- **재배포**: 양 host `~/anvil`을 `aa2b0b0`으로 `rsync -a --delete`
  (예외: `snapshots/`·`configs/goose.yaml`·`configs/goose-secrets.yaml`·root-owned
  런타임 dir `artifacts/`·`flocks/`·`audit/`·`vms/`·`tmp/`·`*.log`·`.git`) →
  host별 빌드 → `DEPLOY_RECORD.txt` 갱신(commit `aa2b0b0`, sha256 위). ZFS
  snapshot mountpoint 불파괴 규칙 준수(검증 전/후 mount PRESENT, snapshots dir 정리).
  - **브리프 대비 사실 정정**: 브리프는 host-a만 feature 소스 원복 대상으로
    적었으나, 재배포 직전 grep 확인 결과 **양 host 모두** `~/anvil` 소스에
    `reconcileSnapshotReplication`가 없어(둘 다 `0feb9fb`-era, pre-sweep) **양쪽
    다 aa2b0b0 재배포가 필요**했다. 둘 다 재배포함.
  - host-a `~/anvil/quantify-out/daemon.log`(root-owned, D4 RCA 워커 잔재,
    Jul 10)는 rsync `--delete`가 삭제하지 못해 그대로 두었다(범위 밖·무해).
- **host-a 상주 `anvil-scheduler.service` 보존 + aa2b0b0 정합**: 소스가 aa2b0b0로
  이동했으므로 스케줄러 바이너리도 재빌드해 정합화 — `/usr/local/bin/anvil-scheduler`
  `188b35b4…` → **`13fbde1e12a0a0c2f9735c8af9c98fa49f40d1cae56bf1141c597f1a5dc66aed`**
  로 교체 후 `systemctl restart` → active+enabled, control loop running,
  `/health {"status":"ok"}` 확인. 이전 바이너리는 `/usr/local/bin/anvil-scheduler.pre-aa2b0b0.bak`
  로 백업. 이 스케줄러 인벤토리는 host-b만 가리키고(source host-a 미인지) sweep는
  add-only이므로 이번 검증(원본이 host-a에 생성)과 간섭 없음 — 검증 내내 상주 유지.
- **adapter 구성**: `cross_host_flock_create_mode=members_only`,
  `ANVIL_MCP_RECONCILE_INTERVAL=10s`(기본 60s 대신 — 절차 대기 단축 허용 편차),
  persistent `scheduler_state_path`(placements.json), `hosts.json`=
  `[{host-a→.19:3000, cap 8, egress deny_all/profile/allow_all}, {host-b→.20:3000, 동일}]`
  둘 다 `healthy:true`. **단일 operator bearer**(40-hex)가 양 daemon 인증. adapter는
  MCP stdio 서버라 reconcile 드라이버(MCP 세션 hold, tool call 없음)로 loop을 상주시켰다.
- **전송 위상(중요)**: `ReplicateSnapshot`은 **adapter가 relay** 한다 — source
  daemon의 `ExportSnapshot` 스트림을 워크스테이션이 읽어 target daemon의
  `/snapshots/import`로 POST. 두 daemon은 서로 직접 통신하지 않는다(redaction·도달성
  분석의 근거).
- **기동 편차(결함 아님)**: 재배포가 `micro-init`/`goose-agent` stamp를
  무효화해 daemon 첫 부팅이 이들을 재빌드 + golden image를 재bake했다. 최초 daemon
  기동을 `go` 없는 PATH로 실행해 `micro-init` 재빌드가 실패(`exec: "go": not found`)
  → PATH에 `/opt/anvil-go/go/bin` 추가 후 정상. golden image rebake 완료까지 첫
  기동 ~수분 소요(1회성).

## 결과 매트릭스

| 단계 | 결과 | 증거 요지 |
|---|---|---|
| 재배포 (양 host 동일 소스, host별 빌드) | **PASS** | `anvil-daemon` sha256 `17c8d2da…` 양 host 바이트 동일, `DEPLOY_RECORD` commit=`aa2b0b0`. scheduler `13fbde1e…` active+enabled |
| ① daemon 기동 (auth-on 양쪽) | **PASS** | 무토큰 `/vms` **401**, op-token **200**, `/health auth_enabled:true` 양 host, 워크스테이션→LAN `:3000` 도달 |
| ② adapter 구성 (`members_only` + persistent placements) | **PASS** | placements.json에 host-a·host-b, 초기 `snapshot_locations={}`, queue_depth 0 |
| ②-복제 host-a 1 VM spawn → snapshot 생성 → **host-b 자동 복제** | **PASS** | snap1(full, on-disk 646M / logical ~1.8G) 발견 `[host-a]`(len1) → sweep가 host-a→host-b 복제. host-b(실 daemon) `GET /snapshots`에 snap1 **재서빙**, on-disk bundle 바이트 동일(`memory.bin 1073741824`·`rootfs.ext4 786743296`·`state.bin 14203` + import `manifest.json`). placements `[host-a,host-b]`(len2), metrics `attempts{replicated,scheduled}=1`, queue_depth→0 |
| ③-down host-b 정지 → 새 snapshot(snap3) 생성 → dial-cap giving-up | **PASS** | host-b daemon 정지(`:3000` closed) 후 snap3 생성. queue_depth **1**, `attempts{dial_failed,target_unreachable}` **1→2→3**(21:39:01/11/21, 10s cadence), cap 3 도달 시 `giving_up` **0→1** + 로그 `giving up on target host "host-b" after 3 dial failures`(21:39:21). **전송 미발화**(snap3 `replicated` 로그 없음; host-b 재기동 로그 `loaded existing snapshots count=2` = snap3 미수신). giving-up 후 dial_failed **3에서 동결**(target exclude) → 이후 pass는 `no_candidate,no_eligible_host` |
| ③-복귀 host-b 재기동 → revival reset → 자동 수렴 | **PASS** | host-b 재기동(control plane ready 21:42:31) → `giving_up`→0·dial 카운터 리셋(제외됐던 target이 다시 선정돼 복제 발화한 것 자체가 reset 증거) → snap3 host-b로 자동 복제(21:44:30), host-b가 snap3 재서빙, placements len2, queue_depth 0 |
| ④ metric 관측 (read-only `RenderSchedulerMetrics`) | **PASS** | 종료 시 `attempts{replicated,scheduled}=3`·`{dial_failed,target_unreachable}=3`·`{no_candidate,no_eligible_host}=19`, `latency_seconds{phase="total"}` count=3 sum=346.13s, queue_depth 0/1·giving_up 0/1 전이 관측 |
| ⑦ redaction 스팟 | **PASS** | adapter stderr + **양 daemon 로그** + 렌더된 `/metrics` 3면 전부: operator token **0**, 상대 daemon 주소(192.168.1.19/20) **0**, 64-hex **0**. 두 daemon은 서로 주소를 로그하지 않음(adapter relay) |
| ⑧ 정리 + 원복 | **PASS** | snap1/2/3 `DELETE /snapshots` **200** 양 host, VM `DELETE /vms` 200, `/snapshots` **[]** 양 host. adapter+양 daemon 정지, 서버는 `aa2b0b0` 배포 상태 유지, scheduler active, ZFS snapshots mount PRESENT |

## 전환 창 실측

- **복제 지연(full snapshot, adapter-relay 위상)**: snap1 = **120.7s**
  (`latency_seconds{phase="total"}` count=1 시점 sum). 3건 누적 sum 346.13s →
  **건당 ~113–121s**. bundle은 on-disk 646M / logical ~1.8G(memory.bin 1G +
  rootfs.ext4 786M). 지연은 대부분 순수 전송량 × relay(read-then-write) 위상 —
  reconcile_interval(10s)과 무관하게 payload가 지배.
- **down → giving-up 전이**: snap3 생성(21:38:58) → 첫 dial(21:39:01, 첫 reconcile
  tick) → 3연속 dial-failure(10s cadence) → **cap 3 도달·giving-up 발화(21:39:21)
  = 생성 후 23s**. interval 환산: 첫 dial 후 `3 × reconcile_interval` = **30s
  상한 안**(관측 3-pass = 20s). **default 60s 환산** = `3 × 60s ≈ 3분` 상한
  (runbook "자동 복제 sweep" alert 권고와 정합).
- **복귀 수렴**: host-b ready(21:42:31) → snap3 재복제 완료(21:44:30) =
  **~119s**. revival 감지는 host-b 복귀 후 **첫 reconcile pass(≤10s)** 내(그
  pass가 곧바로 복제 발화), 나머지는 full-snapshot 전송(~115–120s)이 지배.

## 관측 방법 편차 (특성 — 결함 아님)

`queue_depth`/`giving_up` gauge는 reconcile pass **당 1회**(sweep 말미)만
republish된다. sweep의 `ReplicateSnapshot`은 pass 내 **동기 블로킹**이므로
(full snapshot ~120s), 대형 전송이 진행 중인 pass 동안 디스크의 gauge는 **직전
pass 값을 유지**한다. 두 발현:

1. **②-복제**: 새로 발견된 snap1이 `[host-a]`(len1, under-replicated)인 첫 pass가
   곧바로 120s 전송에 진입하므로, 그 pass가 끝날 때까지 placements.json의
   `queue_depth`는 직전 값(0)을 유지한다 — 진행 중인 under-replicated 상태가
   gauge에 즉시 뜨지 않음. 전송 완료 후 후속 pass에서 0(수렴)으로 정착.
2. **③-복귀**: revival reset은 pass 시작부(in-memory)에서 일어나지만, 같은 pass가
   120s 전송을 블로킹하므로 디스크 `giving_up`의 1→0 반영이 **전송 완료 시점
   (21:44:30)에 관측**된다(in-memory reset 자체는 이미 21:42:3x에 발생 — 제외됐던
   target이 재선정돼 복제가 발화한 사실이 reset의 직접 증거).

**down 단계에는 블로킹 전송이 없다**(target 미도달 → dial short-circuit) →
`queue_depth=1`·`giving_up=1`이 즉시·명확히 surface. 즉 이 편차는 "대형 전송이
동기로 pass를 점유 + gauge는 pass당 1회 publish"라는 설계의 자연스러운 귀결이며,
모든 **종단 상태**(수렴 후 0, giving-up 시 1, 복귀 후 0)는 정확하다. 운영
관측에서 "진행 중 복제"를 실시간으로 보려면 gauge를 pass 중간에도 갱신하거나
in-flight 카운터를 별도 노출해야 하나, 이는 별도 결정 사안(후속 2).

## 결함 회부

### 신규: 없음

sweep 경로(discover / 자동 복제 / dial-cap giving-up·전송 미발화 / revival reset /
no_candidate / redaction)에서 신규 결함 없음. 모든 semantics가 설계·유닛·KVM
e2e와 일치했고, **실 daemon-to-daemon 전송**(host-b가 bundle을 실제 수신·저장·
재서빙)이 stub 검증을 넘어 확증됐다.

## 후속 작업

1. **C slice handoff Follow-Up 1 완료** — 이 run으로 "실 multi-host 수동 검증
   수행" 해소. handoff의 관련 "미수행" 문구를 PASS로 갱신(이 커밋에 포함).
2. **(관측성, 소소·별도 결정)** 진행 중(in-flight) 복제의 실시간 가시성 —
   위 "관측 방법 편차" 참조. gauge pass-중 갱신 또는 in-flight 카운터 노출은
   YAGNI 여부 포함 별도 판단. 현 종단-상태 정확성은 충분.
3. **zone `~/projects/claude-zone/docs/FOLLOWUP.md` P1-09 "C replication 자동화"**
   — 이 run(PASS)로 갱신(zone repo는 이 branch 밖 — 트리거만 기록).

## zone 연동

- 검증 인프라 사실(서버 접속·Go 경로·ZFS 4K dataset·op-token 방식·adapter relay
  위상)은 세션 메모리 `anvil-test-servers`/`anvil-session-workflow`와 정합.
- 서버는 배포본(`aa2b0b0`) 유지 상태로 남김(양 daemon 정지, snapshot/VM 소멸,
  `snapshots` mount·`configs` 보존, host-a `anvil-scheduler.service` active 유지).
