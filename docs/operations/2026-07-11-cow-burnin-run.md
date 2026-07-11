# default COW 전환 burn-in 1차 — 수행 기록 (2026-07-11)

- 상태: **FAIL — default COW flip 보류, 신규 결함 D4 회부.** 2026-07-11 사용자
  결정("burn-in 후 전환")의 burn-in 입력값. flip은 D4 해소·재-burn-in 통과까지
  진행하지 않는다.
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
- 가설(미확증, n=1): D3 계열의 coarse-hole 상호작용 또는 loop/dm-snapshot ×
  ZFS 128K 상호작용. plain 경로는 같은 merged 산출물을 직접 복사 사용해 통과했으므로
  dm-snapshot base 경로 특이성이 유력 후보. **ext4 host에서의 cow gate 재현 여부가
  1차 분별 실험**(upstream 일반 결함 vs ZFS-조합 결함).
- 재현: 위 명령 1회 재실행으로 결정성 확인 필요(현재 n=1).

## 후속

1. **D4 RCA slice** — 분별 실험(ext4 cow gate, ZFS tmp 경로 격리) → 원인 확정 →
   최소 수정(후보: merge 산출물 경로를 4K dataset로 / D3 가드의 출력·base 경로
   확장 / dm 정렬). 별도 승인.
2. D4 해소 후 **cow burn-in 재실행** → green이면 default flip slice 진행
   (2026-07-11 결정의 조건부 이행).
3. host-a `~/anvil` 배포본이 feature 브랜치 소스 상태 — 다음 서버 세션에서 main
   재배포로 원복(스케줄러 서비스·snapshots mount는 보존됨).
