# default COW 전환 burn-in 1차 — 수행 기록 (2026-07-11)

- 상태: **run 1 FAIL → D4 회부 → 1차 fix(fsync) 불충분 → D4 REOPENED → host-b 재현으로
  일반 결함 확정 (미해결, runtime 계층).** ⚠️ 아래 "D4 — CLOSED" 서술은 **정정됨**:
  1차 fix green 은 n=1 우연, default-cow-flip 재검증·host-b 에서 동일 GPF 재발. 재-RCA·
  host-b 분별·runtime probe(음성)는 아래 "D4 — REOPENED (round 2)" 참조.
  anvil 저장소·복원-설정 레버로는 미해결 — 원인은 heavy 동시-부하 하 diff-restore resume
  경합(KVM/Firecracker/host). **default COW flip 은 여전히 보류.**
- 환경: host-a(192.168.1.19, root-on-ZFS 128K + `rpool/anvil-snapshots` 4K
  dataset → `~/anvil/snapshots`), full KVM gate `e2e_test.sh`, 소스
  `feature/deferred-decisions`(sizing fix 포함, main f43e8e8 + 2 커밋).
- 대조군 (plain, 동일 host/소스): **GATE_EXIT=0, 333✓/0✗** — sizing fix 회귀 없음.

## COW gate 결과

`sudo env "PATH=/opt/anvil-go/go/bin:..." EPHEMERA_DISK_MODE=cow bash e2e_test.sh`
(gate가 daemon에 process env를 상속시킴 — cow 도달 증거 아래):

- **GATE_EXIT=1, 329✓/4✗** — 실패는 steps 31–34 한 묶음뿐:
  - 31 `POST /snapshots/{id}/restore` (diff snapshot) → **500** (기대 201)
  - 32 diff-restore agent `/health` → 404 (VM 미기동), 33/34 후속 연쇄
- **COW 실동작 증거**: fallback WARN 0건(probe 통과, `useCOW=true`), dm-snapshot
  store 생성 관측, 명시적 COW steps **44a–g 전부 PASS**(cow spawn ×2,
  cold-restart, SIGKILL crash 후 orphan cow device reclaim, delete→plain 복귀).
- **차등 증거**: 동일 step 31이 plain gate에서는 PASS — 실패는 cow-spawn과 상관.

## D4 (신규 회부) — cow-스폰 VM의 diff-snapshot restore가 guest kernel panic

- 증상: cow-스폰 VM의 diff snapshot을 restore하면 guest가 부팅 ~5.4s에
  `Kernel panic - not syncing: Fatal exception in interrupt` → 60s agent-wait
  timeout → restore 500. COW의 spawn/재시작/crash-복구/reclaim 경로는 전부 정상 —
  **diff-restore × cow-spawn 조합에 고립된 실패.**
- daemon 로그 trace: `restore: setting up dm-snapshot cow
  base=~/anvil/tmp/vm-…-rootfs-merged.ext4 store=/tmp/goose-workspaces/vm-….cow`
  — **merge 중간산출물(`rootfs-merged.ext4`)이 `~/anvil/tmp`(root pool,
  recordsize 128K)에 놓이고 cow restore는 이를 dm-snapshot base로 사용**한다.
  4K `~/anvil/snapshots` dataset 밖이다.
- 가설(미확증, n=1 — **RCA로 대체됨, 아래 CLOSED 절 참조**): 당시에는 coarse-hole
  상호작용/“plain은 직접 복사 사용” 등을 후보로 봤으나, RCA 과정에서 plain restore도
  동일하게 dm-snapshot merged 경로를 쓰는 것으로 정정됐고 recordsize·콘텐츠 가설은
  반증됐다 — 확정 원인은 fsync 부재 durability race(CLOSED 절).
- 재현: 위 명령 1회 재실행으로 결정성 확인 필요(현재 n=1).

## D4 — CLOSED (2026-07-11, `fix/d4-cow-diff-restore`) — ⚠️ SUPERSEDED, 결론 오류

> **정정(2026-07-12)**: 이 절의 "원인 확정 + 최소 수정 완료" 결론은 **틀렸다**.
> `out.Sync()` fsync 는 불충분했고 (default-cow-flip 재검증에서 동일 GPF 재발), 아래
> "REOPENED (round 2)" 가 실제 상태다. 이 절은 round-1 조사 기록으로만 보존한다.

원인 확정 + 최소 수정 완료. burn-in 의 D4 가설(merge 산출물 128K 경로/dm 정렬)은
**부분적으로만 맞았다** — 128K 경로 자체가 아니라 그 산출물의 **내구성(durability)**
문제였다.

- **증상 정밀화**: diff restore 시 guest 가 resume 후 ~5.4–5.8s 에
  `general protection fault … RIP: inet_bind2_bucket_find` (커널 네트워크 bhash2,
  non-canonical 포인터) → `Kernel panic - not syncing: Fatal exception in interrupt`.
  rootfs 는 정상(EXT4 mount OK, 이 케이스 `rootfs diff changed_bytes=0`) — 손상은
  restore 된 guest 메모리로 **보이지만** 실제로는 rootfs 블록 오독의 2차 효과.
- **분별/재현 매트릭스**:
  - ext4 full cow gate → **통과**(334✓/0✗). ZFS 필요.
  - host-a ZFS, 최소 시나리오(spawn→full→diff→restore ×4) → **통과**. 전체 게이트
    부하 필요.
  - host-a ZFS **full cow gate** → 재현(step 31 restore **500**), 결정적(burn-in +
    재검증 2회).
- **데이터-레벨 격리**: 실제 `storage.MergeRootfsDiff` 로 128K 상 병합 산출물을
  loop 로 재독 → **byte-perfect**(reflink/sync-freshness/unlink 변형 전부 MATCH).
  merged memory 는 `/dev/shm`(tmpfs). ⇒ 병합 *내용* 결함 아님. (블록클론 버그도
  아님 — ZFS 2.2.2 fix 존재.)
- **확정 실험**: restore 경로에서 guest 로드 직전 전역 `sync()`+지연을 넣은 진단
  빌드로 full cow gate → **green**(334✓/0✗), 병합 메모리 sha 는 pre/post-sync
  동일(MATCH=true). ⇒ 병합 콘텐츠는 정확하고, **타이밍/내구성** 문제임이 확정.
- **근본 원인**: 병합 산출물(dm-snapshot origin = merged rootfs)이
  `reflinkOrSparseCopy`+`overlaySparseDiff` 로 **디스크에 커밋되기 전** losetup 이
  read-only loop 로 연다. ZFS 에서 전체 게이트 동시 I/O 부하 시 loop 의 버퍼드
  read 가 아직 커밋 안 된 파일 데이터와 어긋나 guest 가 손상된 rootfs 블록을 읽고
  GPF. (ext4 loop/page-cache 는 fsync 없이도 coherent, idle ZFS 도 마스킹 → ZFS +
  부하에서만 노출.)
- **수정(최소)**: `internal/storage/snapshot.go` `overlaySparseDiff` 가 반환 전
  `out.Sync()` — 병합 산출물이 loop 에 물리기 전 커밋/캐시 코히어런트 보장.
  (tmpfs 대상인 merged memory 에는 사실상 no-op.)
- **검증**: 로컬 ext4 cow gate **334✓/0✗**(회귀 없음), host-a full cow gate
  **334✓/0✗**(재현 해소), `go test ./internal/storage` green, `go vet` clean.
  기존 유닛 `TestWriteRootfsDiffIdentical`(changed_bytes=0 = D4 시나리오) 통과.

## D4 — REOPENED (round 2, 2026-07-12, `fix/d4-round2-cow-loop-coherence`) — 미해결/BLOCKED

`feat/default-cow-flip`(로컬 ext4 게이트 통과)의 host-a **default-cow full gate** 에서
step 31 diff-restore 500 → 동일 `inet_bind2_bucket_find` GPF 재발. 1차 fix(`out.Sync()`)는
**존재**했다 → fsync 는 충분하지 않았다. 이번엔 충분성 기준을 **n≥2 연속 green** 으로 잡고
재-RCA 수행. **결론: 저장소-계층 결함이 아니다. runtime/host 수준의 간헐적 손상.**

- **재현 결정성**: host-a full cow gate 3회 fail(repro #1 329✓/5✗, sync-fix gate, fix2 gate#2),
  전부 step 31 restore 500 + `inet_bind2_bucket_find` non-canonical GPF ~6s. 확정적 재현.
- **결정적 해부(round-2 핵심)**: 실패 restore 의 merged memory 를 캡처(D4-CAP: skip-delete +
  hardlink, hot-path 지연 0) → `MergeMemoryDiff` idle 재계산과 **byte-identical**(sha 일치).
  ⇒ **Firecracker 가 로드하는 메모리는 정확하다.** 손상은 **load 이후** guest 커널 RAM 에서 발생.
  guest 콘솔에 EXT4/블록 I/O 에러 전무 → rootfs 내용도 정상.
- **가설별 실험(전부 n=2 미달/무효)**:
  - **fsync** (`overlaySparseDiff out.Sync`, main 반영): FAIL(재발).
  - **guest resume 직전 global `sync()`**: FAIL. (round-1 의 green 은 sync 가 아니라 그
    진단이 동반한 ~3–5s 대량 I/O **지연** 때문이었음 — 빠른 sync 는 무효.)
  - **`losetup --direct-io=on`**(origin+cow loop): FAIL. ZFS 2.2.2 는 O_DIRECT 미완성
    (제대로 된 direct I/O 는 ZFS 2.3+) → loop 는 버퍼드 유지, 무효.
  - **merged rootfs → tmpfs**(`/dev/shm`): gate#1 334✓/0✗ **green**, gate#2 329✓/5✗ **FAIL**.
    즉 origin+memory 를 **둘 다 ZFS 밖(tmpfs)** 에 둬도 재발 → ZFS 데이터-경로가 원인이 아님.
    (2/3 통과로 rate 를 낮출 여지는 있으나 n=2 기준 불충족.)
  - **loop coherence / loop-number-reuse-stale-cache 합성 실험**(부하 하): 0 divergence.
    **swap**: 비-ZFS raw 파티션·미사용. **KSM**: run=1 이나 pages_shared=0(무활동). 전부 무관.
- **패턴**: 손상이 **항상 bhash2(inet_bind2_bucket)** — restore 직후 daemon 이 vsock 로
  guest IP 재구성할 때 커널 네트워크 스택이 touch 하는 구조. 간헐적, **부하 상관**(ZFS 게이트가
  느려 동시 microVM 수↑ → 실패율↑; ext4 게이트는 빨라 통과), **큰 지연이 마스킹**.
- **함의**: 원인은 **heavy 동시 microVM churn 하의 runtime**(KVM/Firecracker restore, 또는
  host). host-a RAM 은 **non-ECC**(silent bit-flip 미검출) — hardware 배제 불가하나, 손상이
  항상 bhash2 라는 **결정적 위치**성은 순수 랜덤 배드램보다 SW/레이아웃-특이 이슈를 시사.
- **상태**: **저장소-계층 최소 수정으로는 해결 불가(n≥2 green 달성 실패). 커밋한 코드 fix 없음.**

### host-b 분별 실험 (2026-07-12) — 결론: **일반 결함**(host-a 하드웨어 특이 아님)

목적: host-a(non-ECC) 하드웨어 특이 vs 일반 KVM/Firecracker×부하 결함 분리.

- **배포**: host-b(192.168.1.20, PureCVisor-Prod-2) — host-a 와 **동일 레이아웃**
  (ZFS rpool 128K + `rpool/anvil-snapshots` 4K, **RAM 도 non-ECC**). flip 소스
  (5beb744) rsync + **host-a flip 데몬 바이너리(sha256 e340cb5f…) 그대로 복사**(빌드
  환경 변수 제거). snapshots 4K mount·configs 보존. DEPLOY_RECORD 기록.
- **재현**: host-b default-cow full gate → **step 31 diff-restore 500 + 동일
  `inet_bind2_bucket_find` non-canonical GPF ~7s** 재발(gate#1 + 실험 gate 2회 = 3회
  전부). ⇒ **양 host 동일 코드·동일 GPF 재현 = 일반 결함.** host-a 하드웨어(배드램)
  단일 원인 배제(양쪽 non-ECC 인데 둘 다 재발은 배드램 우연 일치보다 SW/일반 결함).
- **runtime probe (i) vsock IP-reconfig 타이밍**: resume 직후 reconfig 를 **4000ms
  지연**(env `EPHEMERA_D4_RECONFIG_DELAY_MS`) → GPF **그대로**. ⇒ reconfig 타이밍은
  원인 아님(게스트 agent 가 스스로 :8080 bind → 지연된 reconfig 이전에 bhash2 touch).
- **runtime probe (ii) restore TrackDirtyPages**: 복원 VM 의 `TrackDirtyPages` 를
  **끔**(env `EPHEMERA_D4_NO_DIRTY_TRACK`) → GPF **그대로**. ⇒ 복원 dirty-tracking 도
  원인 아님.
- **정밀화**: full-snapshot restore 는 통과, **diff-restore 만** 실패. diff 경로는
  base(1GB) memory copy + overlay 로 **더 느리다** → ZFS 유발 동시-부하 하에서 resume
  순간의 **경합 창(window)**이 커져 게스트 커널 RAM 이 (로드된 뒤) 오손됨. ext4/full 은
  빨라 창이 작아 통과. 큰 지연(round-1 ~3–5s)이 마스킹한 것도 동일(부하/창 축소).
- **결론**: 원인은 **heavy 동시 microVM 부하 하 diff-restore resume 경합** —
  KVM/Firecracker/host runtime 계층. **anvil 저장소·복원-설정 레버로는 인과 미확정·미해결.**
  제안한 두 probe 모두 음성. **커밋 fix 없음.**

### memory-immutability 가설 + fc CHANGELOG 대조 (2026-07-12) — 둘 다 음성

fc 문서 계약("snapshot **memory file 은 resume 후에도 page cache 로 guest RAM 을 backing
하므로 immutable 이어야 한다; 외부 수정 시 guest memory 오손**")에 기반해 두 단서를 검증.

- **단서 1 — 병합 memory 산출물이 live guest 를 backing 하는 동안 후속 writer 가 덮는가**:
  - **코드 감사**: 병합 memory/rootfs 경로는 `pickMerged*Path(workDir, newVMID)` 로
    **restore 마다 newVMID(=`vm-<UnixNano>`) 고유**. 유일한 writer 는 load *이전*의
    merge(`copyFile` O_TRUNC + `overlaySparseDiff` O_WRONLY), 유일한 remover 는 `os.Remove`
    (unlink=안전). rootfs 산출물은 losetup 직후 즉시 unlink; memory 산출물은 handler
    return 시 defer-unlink(더 늦으나 여전히 unlink). ⇒ **live 산출물에 쓰는 자연 경로 없음**
    (경로 고유 + defer-unlink). (recovery 경로는 `vmID` 사용하나 재시작 직후라 live VM 없음.)
  - **표적 기전 재현**(host-b): 경로를 **고정**(env)하고 defer-unlink 를 **끔**(env) →
    VM-A 를 fixed 경로 backing 으로 live/health 200 확인 후 그 파일을 **외부에서 200MB
    덮어씀** → VM-A **health 200 유지·GPF 없음**. (fc 는 MAP_PRIVATE/COW 로 로드해 실행
    중 guest 가 이미 fault-in/COW 한 페이지는 파일 수정 영향 없음.) ⇒ **기전 미성립**.
  - 순차 2회 재현도 A 의 defer-unlink 가 B 이전에 경로를 비워 충돌 미발생.
  ⇒ **단서 1 반증**(자연 충돌 없음 + 외부 write 로 running guest 미오손).
- **단서 2 — fc "diff snapshot memory corruption(multiple memory slots)" fix**:
  CHANGELOG **#5705 = v1.15.0**: ">3GiB 또는 memory-hotplug(=multiple memory slots) x86 VM
  의 **diff snapshot memory 파일 오손**" fix. **우리 fc = v1.15.1**(양 host `--version`
  확인) → **이미 포함**. 게다가 우리 VM 은 **≤2GiB(default 1024, 캡처 merged=정확히 1GiB)
  = 단일 memory slot** → 애초에 해당 버그 조건 미충족. 1.16.x 까지 관련 추가 fix 없음.
  ⇒ **단서 2 반증**(fix 포함 + 조건 미충족).
- **잔여**: byte-match(캡처 merged == idle re-merge)는 병합 *결정성*만 증명 → diff *생성*
  자체가 부하 하에서 stale/incomplete 할 가능성(단일-slot fc diff 생성 결함)은 남으나
  #5705(멀티-slot) 외 알려진 fc 버그·fix 없음, 그리고 diff 는 ext4 에서 통과 → 순수
  생성-결함보다 resume-순간 runtime 경합 유력. **anvil-측 원인 없음 재확인.**

## 후속

1. ⚠️ **[정정]** round-1 의 "burn-in 재실행 green → flip 가능" 결론은 **무효**.
   D4 는 미해결이며 **default COW flip 은 계속 보류**. (round-1 green 은 n=1 우연.)
2. ✅ **[완료] 호스트-특이 vs 일반 분별**: host-b(192.168.1.20)에서 동일 flip 바이너리
   재현 = **일반 결함 확정**(위 "host-b 분별 실험"). host-a 배드램 단일 원인 배제.
   memtest 류(재부팅 필요)는 여전히 잔여 하드웨어 배제용 사용자 옵션이나 우선순위 하락.
3. ✅ **[완료·음성] runtime probe (i)(ii) + memory-immutability 단서 1·2**: vsock reconfig
   지연·복원 TrackDirtyPages off·병합 memory 경로 충돌/외부 write·fc #5705 대조 **전부 음성**
   (위 참조). anvil-측 원인 없음 재확인.
4. **잔여 조사(모두 Firecracker/KVM 계층, anvil 밖)**: (a) 단일-slot VM 의 diff-snapshot
   **생성**이 부하 하에서 stale/incomplete 해지는지(fc 계측 필요; 알려진 fc 버그·fix 없음),
   (b) resume-순간 동시-부하 KVM 경합. **완화**(anvil-측, 근본 수정 아님): diff-restore
   resume 창 축소(1GB memory copy 가속/제거, restore 동시성 축소, bounded quiescence)를
   **host-a·host-b 각 n≥2 + ext4 회귀**로 A/B. **fc 업그레이드(≥1.16.1 또는 최신)는 별도
   결정** — 단일-slot 관련 fix 는 없으나 상위 버전 소거법으로 값싸게 시험 가치.
5. host-a·host-b 배포본 모두 **feat/default-cow-flip 빌드로 원복 완료**(host-b sha256
   e340cb5f = host-a). round-2 잔재(테스트 daemon/loop/dm/캡처) 제거. 스케줄러·snapshots
   4K mount 보존.
6. 저장소-계층·복원-설정 레버로는 인과 미확정·미해결 — 커밋한 코드 fix **없음**(round-2
   브랜치는 본 문서 정정·확장만 포함).

## upstream 함의 (기여 후보)

restore가 merged rootfs를 loop/dm-snapshot origin으로 쓰는 구조는 upstream을
미러링한다(`api.go` restore 경로 주석). merge/D3 배관 자체는 anvil-specific이라
단정할 수 없으나, upstream ephemera에 동형의 "fsync 없는 merged 산출물 → loop"
경로가 있으면 같은 잠재 race가 존재한다 — **upstream 확인 후 기여 후보**로 기록
(keep-alive proxy fix와 같은 계열의 후보 목록).
