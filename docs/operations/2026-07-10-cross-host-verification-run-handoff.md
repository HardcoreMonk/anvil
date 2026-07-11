# Cross-host 실 2-daemon 수동 검증 — 수행 기록 (2026-07-10)

- 상태: **완전 통과 (당일 갱신).** 최초 수행에서 ⑥ 재시작 복구만 FAIL → D1 fix
  (PR #30) + 재검증 중 발견된 잔여 D1b fix (PR #31) 머지 후 **⑥ 재검증 전 항목
  PASS** — 그것도 runbook보다 강한 시나리오(양 daemon **동시** 재시작)로:
  reconcile 무에러, wall 양방향, gtcall 양방향(home→member outbound 포함),
  히스토리 재시작 넘어 보존. runbook 판정 기준 전체 충족 — **failover 구현
  게이트 해제.**
- 절차 원문: [2026-07-08-cross-host-manual-verification.md](2026-07-08-cross-host-manual-verification.md)
- 작업 묶음 진입점: [2026-07-08-routed-flock-stack-handoff.md](2026-07-08-routed-flock-stack-handoff.md)
- 대상 커밋: `5d4fac5` (main, 양 host 동일 소스에서 host별 빌드 — sha256 동일 확인)

## 환경 (계획 대비 편차 포함)

| | host-a (home) | host-b (member) | 워크스테이션 (control plane) |
|---|---|---|---|
| 주소 | 192.168.1.19 | 192.168.1.20 | 192.168.1.18 |
| OS | Ubuntu **24.04.4** (계획: 26.04 — runbook 배포판 중립 명시로 진행) | 동일 | — |
| CPU | i9-11900 | i9-11900 | i9-12900H |
| 루트 fs | **root-on-ZFS** (rpool, recordsize 128K) | 동일 | ext4 |
| Go | 1.26.2 (`/opt/anvil-go`, 기존 1.23.6 보존) | 동일 | 1.26.2 |

- 단일 host smoke: 최초 실행에서 diff-snapshot 복원 4건 실패 → RCA 후 완화 적용
  (아래 D3) → **재실행 시 양 host "All test steps passed ✓" (전체 594 체크 green)**.

## 결과 매트릭스

| 절차 | 결과 | 증거 요지 |
|---|---|---|
| ① daemon 기동 (auth-on 양쪽) | **PASS** | health `auth_enabled:true`, 무토큰 401 |
| ② adapter 구성 (`members_only` + persistent state) | **PASS** | hosts.json 용량으로 host별 배치 제어 |
| ③ routed flock 생성 | **PASS** | roles[0]→host-a(home), `town_wall_enabled=true`, 두 host VM 분산 |
| ④ wall 양방향 | **PASS** (2회) | home `TOWN_WALL.log`에 양쪽 기록, 양 daemon `wall/history` 바이트 동일. relay hop 487ms vs 로컬 74ms |
| ⑤ gtcall 4방향 | **PASS** | ① member→home ② home→member(2nd hop) ③ member→같은 host member(C1 경로) 전부 PONG 왕복(1.1~1.6s), ④ 미존재 agent → daemon 404 + guest 진단에 주소·토큰 무노출 |
| ⑥ 재시작 복구 | **FAIL** | home·member 재시작 각각에서 wall/call 영구 불능 — 결함 D1 |
| ⑦ redaction 스팟 | **PASS** | 양 daemon stderr에 64-hex 토큰 0건·상대 daemon 주소 0건, `GET /flocks/{id}`에 토큰/HomeAddr 부재, store 레코드 토큰 scrub 확인 |
| ⑧ 정리 + revoke | **PASS** | relay token: delete 전 200 → 후 **401**, 양쪽 flock/VM 소멸 (단 D2 오보고 동반) |

판정 기준상 "재시작 복구" 실패로 **전체 검증은 미통과** — runbook 규정에 따라
결함 회부. **home 재선출 failover 구현 착수는 D1 수정·재검증까지 보류** (재시작
복구가 안 되는 상태에서 재선출은 성립 불가; D1은 failover의 선행 조건).

## 결함 회부

### ~~D1~~ — **CLOSED** (PR #30 `df667c8` + PR #31 `15bd204`, 2026-07-10 당일)

- **PR #30**: `FlockMetadata`에 kind/home_addr/roster 영속(토큰 비영속 불변식
  유지, Addr scrub) + LoadFromDisk 복원 → 멱등 재등록 경로 성립. 리뷰 반영:
  Persist↔delete 직렬화(유령 metadata 방지), watchdog nil-wall 가드.
- **PR #31 (D1b)**: 재검증에서 발견 — 멱등 재등록이 admission map만 채우고 hub
  struct 토큰(`f.CallToken`)을 재충전하지 않아 home 재시작 후 **outbound 2nd
  hop이 빈 bearer 발신 → member 401**. `UpdateHubTokens` 재충전 + 가변화된 hub
  토큰·roster의 raw read를 locked accessor(`DistributedTokens`/`RosterSnapshot`)
  로 전환(-race 검증).
- **재검증 (15bd204 배포, 양 daemon 동시 재시작)**: reconcile 무에러 / wall
  양방향 PASS / gtcall member→home·home→member PASS / history parity 5msgs
  (재시작 넘어 보존). ⑥ CLOSED.

원문 (기록 보존):

### D1 (주요) — daemon 재시작 시 routed flock 분산 상태 유실 + 재등록 비멱등

증상: home 또는 member daemon 재시작 후 해당 flock의 cross-host wall/call이
영구 불능 (adapter reconcile로도 복구 안 됨). home·member 대칭 재현 (각 1회).

체인 (audit/adapter 로그 근거):

1. daemon cold-restart recovery가 vm state에서 flock 메타데이터를 재생성하되
   **일반 local flock으로 강등** (`"task": "recovered flock ..."` — hub/relay
   kind, roster, relay/call token admission 전부 유실).
2. adapter `ReconcilePlacements`의 재등록(`POST /flocks/{id}/distributed` 또는
   `/relay`)이 **409** — 동명 flock 존재 시 재등록 거부 (비멱등).
   audit: `10:21:19 POST /flocks/.../distributed status:409` (operator).
3. 이후 상대 host relay hop 전부 **401** — admission 부재.
   audit: `10:22:21 POST /flocks/.../post status:401`, `/call status:401`
   (remote 192.168.1.20).

관련 기존 관찰: gtcall handoff Follow-Up의 "`UpdateHubRoster` bool 반환 미사용"
(TOCTOU) — 같은 재등록 경로의 방어 부족 계열. 수정 방향(설계는 fix slice에서):
(a) recovery가 분산 kind + admission을 복원하거나, (b) 재등록을 멱등(upsert)으로
바꿔 reconcile self-heal이 성립하게 하거나, 양쪽 모두.

참고: PR #21(reconcile loop)의 "daemon 재시작 자가 치유"는 단일 host stub 환경
검증이었다 — **정확히 이 수동 검증이 잡도록 설계된 부류의 결함**이며, 이번
검증의 핵심 수확.

### D2 (부차) — routed flock delete의 cleanup_failed 오보고

`anvil_delete_flock`이 3회 모두 `"routed flock delete cleanup pending:
reason=cleanup_failed"` (isError)를 반환했으나, 실제로는 양 daemon의 flock/VM
teardown과 token revoke까지 전부 성공. 보고/멱등성 결함 — 운영자가 성공한
삭제를 실패로 오인하게 만든다.

> RCA/fix slice (`fix/d2-delete-cleanup-misreport`, 머지 전): delete가 shared
> wall 을 먼저 deregister(`DeleteFlock`)하는데 daemon `deleteFlock`이 그 flock의
> 멤버 VM 까지 cascade teardown 하므로, 뒤이은 per-VM `DELETE /vms/{id}`가
> 항상 404 를 받고 `deleteRoutedFlockAgents`가 이를 실패로 오분류 → 3회 모두
> 결정적으로 재현. fix: 404(=이미 소멸=teardown 성공)를 pending 이 아닌 완료로
> 취급, 진짜 실패(비-404)는 계속 cleanup_failed 유지.

### D3 (부차·플랫폼) — diff snapshot merge가 ZFS에서 guest 메모리 오염

- 증상: diff snapshot 복원 시 fc가 resume 직후 사망
  (`fc_vcpu 0:ERROR Unexpected exit reason on vcpu run: Shutdown` = guest triple
  fault). 단일 host smoke 4건 실패의 원인.
- RCA (수치 확정): `overlaySparseDiff`(`internal/storage/snapshot.go:429`)가
  "diff 파일의 SEEK_DATA 영역 = 실제 기록된 dirty 페이지"를 가정하나, ZFS는
  hole을 **recordsize(기본 128K) 단위**로만 보고 → fc가 실기록한 217페이지가
  1536페이지 extent로 부풀려지고, 미기록 제로 패딩 ~1300페이지가 base 메모리의
  유효 데이터를 덮어씀. 같은 pause에서 diff·full을 연속 덤프해 비교(churn 0)로
  증명. ext4 대조군은 234페이지 중 불일치 1(정상). fc 1.15.1/1.16.1 동일,
  KVM pml 유무 무관 — fc·ZFS·KVM 각자는 정상, 조합 계약 위반.
- 같은 노출: `MergeRootfsDiff`가 읽는 `rootfs.diff`(anvil 자체 기록 sparse 파일)
  — 이번 smoke에선 changed_bytes=0이라 미발화.
- 운영 완화 (적용 완료): 양 host에 `rpool/anvil-snapshots` dataset
  (**recordsize=4K**)을 `~/anvil/snapshots`에 마운트 → smoke 전체 green.
- 코드 후속 (**완료**, `fix/d3-zfs-diff-guard`): snapshot 디렉토리 hole-granularity
  probe(`storage.ProbeHoleGranularity` — 4K sparse 테스트 파일 + SEEK_HOLE)를
  daemon에 추가. **창설측 강등**(coarse면 diff→full, metadata·응답 `snapshot_type`
  정직 기록, daemon 수명당 warning 1회, probe 1회 캐시) + **판독측 거부**
  (`overlaySparseDiff` 진입 시 diff 디렉토리 probe, coarse면 명확한 에러로 overlay
  거부 — 복제/임포트 유입분까지 방어). 두 가드 모두 fine fs(ext4)에서는 no-op —
  KVM e2e 게이트 전 594 체크 green(diff 생성·restore 포함 불변) 재확인. 4K
  recordsize dataset 운영 완화는 diff 효율을 위해 계속 권장. 상세: runbook "Diff
  snapshot 안전성" 절 + disaster-recovery "coarse-hole ... (D3)" 절.

## 후속 작업 (우선순위순 — 2026-07-10 갱신)

1. ~~D1 fix slice~~ / ~~⑥ 재검증~~ — **완료** (PR #30·#31, 위 D1 CLOSED 참조).
2. ~~failover 구현 slice 착수 승인 요청~~ — **완료**: 승인 → 구현 완료
   (2026-07-11, `feature/home-failover`). 상세:
   [2026-07-11-home-failover-handoff.md](2026-07-11-home-failover-handoff.md).
3. ~~D3 코드 완화~~ — **완료** (`fix/d3-zfs-diff-guard`): hole-granularity probe +
   창설측 diff→full 강등 + 판독측 overlay 거부. 위 D3 "코드 후속" 참조.
4. **D2 fix** — delete cleanup 보고 정합.
5. fc upstream/OpenZFS 참고 보고 검토 (D3는 fc diff 포맷의 "sparseness=의미"
   계약이 coarse-granularity fs에서 깨지는 일반 문제 — fc 문서 개선 제안 후보).
6. (리뷰 파생, 소소) reconcile 틱마다의 무변경 Persist dirty-check /
   relay 멤버 상태 이벤트의 로컬(비서빙) wall 기록 설계 재평가 /
   `flockAgentLocalVM`의 `f.Agents` raw map 읽기 정리 (기존 클래스).

## zone 연동

- `~/projects/claude-zone/docs/FOLLOWUP.md` **P3-09**: 트리거(서버 2대) 충족,
  검증 수행, D1 회부로 갱신 — failover는 D1 이후로 보류.
- 검증 인프라 사실(서버 접속·Go 경로·ZFS dataset)은 세션 메모리
  `anvil-test-servers`에 영속.
