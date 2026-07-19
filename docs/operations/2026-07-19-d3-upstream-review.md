# D3 (coarse-hole diff corruption) — fc/OpenZFS upstream review

**날짜:** 2026-07-19
**상태:** VALIDATED — anvil의 D3 처리가 upstream fc/OpenZFS 의미론과 정합. **코드 변경 불요, 백로그 항목 종결.**
**관련:** D3 RCA `2026-07-10-cross-host-verification-run-handoff.md §D3`, 방어 코드 `internal/storage/hole_granularity.go`·`snapshot.go`, 운영 절차 `disaster-recovery.md`(§coarse-hole D3), PR #36. D4 대응 문서 `2026-07-13-d4-firecracker-upstream-report.md`.

## 목적

백로그 "fc upstream/OpenZFS 참고 보고 검토(D3의 fc diff 'sparseness=의미' 상호작용)"에 대해, anvil의 D3 방어 모델을 **upstream Firecracker diff-snapshot 포맷 + OpenZFS/ZFS hole-보고 의미론**에 비추어 검증한다. D3는 이미 방어 확정(PR #36); 이 문서는 그 방어가 upstream 사실과 정합하고 충분·내구적인지 확인해 항목을 종결한다.

## anvil D3 모델 (검증 대상)

fc는 diff snapshot을 **sparse 파일**로 쓰며 hole = clean(미더티) 4KiB 페이지 = base에서 읽어야 하는 영역이다. anvil `overlaySparseDiff`가 `SEEK_DATA`/`SEEK_HOLE`로 diff의 data extent만 base 위에 overlay한다. ZFS는 hole 경계를 **recordsize(>4K) 정렬**로만 보고하므로, recordsize>4K에서 written 영역을 과대보고 → 미기록 record padding이 live base memory를 덮어써 guest triple-fault(D3). 방어: `ProbeHoleGranularity`로 fs granularity 측정(coarse=>4K 또는 오류) → 창설측 diff→full 강등(`applyD3DiffGuard`), 판독측 overlay 거부(`refusing overlay ... (see D3)`), 운영상 recordsize=4K dataset 권장.

## 검증 결과 (upstream 대조)

| # | 질문 | 결과 | 근거 |
|---|---|---|---|
| Q1 | fc diff = 4KiB dirty-page sparse, hole=clean? `HoleGranularityFine=4096` 정확? | ✅ VALIDATED | fc `snapshot-support.md`: diff mem file는 dirty 페이지만 담은 **sparse** 파일; `track_dirty_pages`로 KVM dirty-log 사용. dirty 단위 = 4KiB guest page(KVM dirty-tracking은 4K 페이지 단위; fc hugepages 문서). fc 자체 merge 도구(`snapshot-editor`/`rebase-snap`)도 **동일 SEEK_DATA/SEEK_HOLE overlay** 사용 → 취약성은 fc diff 포맷 고유(anvil-특유 아님). |
| Q2 | ZFS SEEK_HOLE = recordsize 정렬 → recordsize>4K는 coarse? | ✅ VALIDATED | record가 할당 단위(sub-record hole 없음); `zfs_holey→dmu_offset_next→dnode_next_offset`가 block 단위 탐색. FreeBSD **D36194**: ZFS hole 단위 = dataset recordsize(`_PC_MIN_HOLE_SIZE`). |
| Q3 | SEEK_HOLE가 미-sync dirty를 hole로 안 보고? probe fsync+retry가 옳은 가드? | ✅ VALIDATED | `zfs_dmu_offset_next_sync` man: "holes will not be reported in recently dirtied files". probe fsync+retry / D4 `out.Sync()`가 정확한 belt-and-suspenders(OpenZFS #15526 corruption-bug window도 방어). |
| Q4 | 방어가 sound·complete? probe fail-safe? 엣지? | ✅ VALIDATED-with-notes | lseek(2) 계약이 바로 loophole(hole granularity는 fs-정의, DATA가 non-zero 보장 없음). probe는 모든 불확실(생성/write/truncate/fsync 오류, SEEK_HOLE ENOTSUP, interior hole 없음)에서 256K(coarse) 반환 = **fail-safe**. 엣지 전부 처리: recordsize==4K→4096(허용), <4K→4096(허용), ≥256K/no-hole/ENOTSUP/거짓보고fs→coarse(안전). |
| Q5 | upstream 이동으로 완화 가능? 아니면 근본적? | ✅ 근본적, 내구적 | recordsize 과대보고는 ZFS block-pointer 할당 모델 고유 — sub-record hole 없음, 어떤 ZFS 버전도 finer 미보고·계획 없음. 유일 완화 = anvil이 이미 권장하는 recordsize=4K dataset. Q3 dirty-보고는 upstream 기본이 `zfs_dmu_offset_next_sync=1`로 flip(fsync+retry는 이제 종종 redundant지만 `=0` 운영자·regression window 방어로 **유지가 정확**). fc는 여전히 diff를 4K sparse로 씀 → `HoleGranularityFine=4096` 유지 정확. |

## Notes (검증 완료)

- **Note 1 — read-side 가드는 diff가 *현재* 얹힌 fs를 probe하지, *태생* fs가 아니다.** coarse-born diff(과대보고 extent가 내용에 baked)가 fine fs로 재구현되면 read probe가 통과해 corruption 가능. **검증: anvil은 coarse-born diff를 절대 *생성*하지 않는다** — snapshot 생성 경로는 단일(`api.go:3252` `resolveSnapshotType` 직후 `:3264` `applyD3DiffGuard`)이며 coarse fs에서 항상 diff→full 강등. 따라서 all-anvil(post-PR#36) fleet에서 이 시나리오는 불가. 잔여는 legacy(pre-#36) 또는 non-anvil 생성 diff뿐 — 이론적 residual, 실무 gap 아님. read-side 가드가 의존하는 불변식(창설측 강등의 보편성)은 확인됨.
- **Note 2 (무시할 수준)** — 창설측 probe는 daemon lifetime당 1회 캐시(`snapshotsDirCoarse`). 런타임 recordsize 변경은 재시작까지 미감지. 단 read-side probe는 overlay마다 실행(비캐시)이라 **restore는 항상 재검증**됨.

## 부차 발견 (검토 밖·선택)

- **dead code**: `storage.HoleGranularityCoarse` 헬퍼는 production 호출자 0(데몬은 `snapshotsDirCoarse`+`ProbeHoleGranularity` 직접, `overlaySparseDiff`는 `g` 직접 비교). 오해 소지 있는 미사용 D3 헬퍼 — 선택적 cleanup 후보(헬퍼+해당 test 제거). 별도 작업.
- **upstream 기여 후보**: fc 자체 `rebase-snap`/`snapshot-editor`도 recordsize>4K ZFS에서 동일 취약 — fc 문서에 "diff snapshot은 4K-hole fs를 요구" 노트 제보 가능(선택, anvil 책임 아님).

## 결론

anvil의 D3 처리(probe→창설 강등/판독 거부 + recordsize=4K 권장)는 upstream fc(4K sparse dirty diff)·OpenZFS(recordsize hole granularity, dirty-sync 보고) 의미론과 **모든 지점에서 정합**하며, fail-safe하고 근본적으로 내구적이다. **코드 변경 불요.** 백로그 "fc/OpenZFS D3 리뷰" **종결(VALIDATED)**.

(전체 소스별 상세·인용은 검토 세션 산출물에 보존; 이 문서가 durable 요약이다. 검토자는 fc 소스의 literal `4096` 상수·`dnode_next_offset` 라인 단위는 code-summary read로 확인, 권위 소스로 교차검증 — honest flag.)
