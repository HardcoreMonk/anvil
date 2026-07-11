# default COW 전환 burn-in 1차 — 수행 기록 (2026-07-11)

- 상태: **run 1 FAIL → D4 회부 → D4 CLOSED (`fix/d4-cow-diff-restore`).** 재-burn-in
  (host-a full cow gate, fix 적용) **334✓/0✗ green** — 아래 "D4 — CLOSED" 참조.
  2026-07-11 사용자 결정("burn-in 후 전환")의 조건(“D4 해소·재-burn-in 통과”) 충족;
  flip slice 는 별도 승인 하에 진행 가능.
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

## D4 — CLOSED (2026-07-11, `fix/d4-cow-diff-restore`)

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

## 후속

1. **cow burn-in 재실행 1차 = 위 host-a full cow gate 334✓/0✗ green** (fix 반영
   단일 실행 — 반복 soak가 필요하면 flip slice에서 추가 판단). default COW
   flip slice 진행 가능(2026-07-11 결정의 조건부 이행) — 별도 승인 하에.
2. host-a `~/anvil` 배포본은 **aa2b0b0(main) 로 원복 완료**(fix 는 아직 branch —
   merge+release 시 재배포로 반영). 스케줄러 서비스·snapshots mount 보존됨.
3. 내구성 결함은 ZFS+부하 종속이라 고전적 실패-우선 유닛테스트가 불가 — 검증은
   재현 게이트로 수행. 병합 *정확성* 회귀는 기존 유닛으로 커버.

## upstream 함의 (기여 후보)

restore가 merged rootfs를 loop/dm-snapshot origin으로 쓰는 구조는 upstream을
미러링한다(`api.go` restore 경로 주석). merge/D3 배관 자체는 anvil-specific이라
단정할 수 없으나, upstream ephemera에 동형의 "fsync 없는 merged 산출물 → loop"
경로가 있으면 같은 잠재 race가 존재한다 — **upstream 확인 후 기여 후보**로 기록
(keep-alive proxy fix와 같은 계열의 후보 목록).
