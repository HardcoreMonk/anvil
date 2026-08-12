# VM 삭제 실패 cleanup handoff

## 문서 상태

- 날짜: 2026-08-13
- topic: `vm-deletion-failure-cleanup`
- branch: `agent/next-release-gates`
- PR: [#111](https://github.com/HardcoreMonk/anvil/pull/111)
- code-bearing commit: `0aec994089459a64d7bf9e8584f59f4bf6243e4e`
- merge commit: `60ce239ce68555a419994f37c431dcb377825e1f`
- trigger: merged PR #110에 대한 CodeRabbit 사후 Major review
- 설계:
  [`2026-08-13-vm-deletion-failure-cleanup-design.md`](../superpowers/specs/2026-08-13-vm-deletion-failure-cleanup-design.md)
- 계획:
  [`2026-08-13-vm-deletion-failure-cleanup.md`](../superpowers/plans/2026-08-13-vm-deletion-failure-cleanup.md)

## Release Scope

- `dmsetup remove --retry` 소진 뒤 early return 제거
- COW/base loop detach와 full-delete sparse `.cow` unlink 계속 시도
- 모든 storage cleanup error aggregation
- storage failure와 무관한 TAP/IP release orchestration 회귀 test
- PR #110 named build/acceptance/resource evidence 보정

## Verification

### Prove-broken-first / forced failure

초기 test compile은 `dmSnapshotTeardownOps`와 `teardownDMSnapshotWithOps`가 없어 실패했다.
기존 source control flow는 dm remove 실패 직후 return해 loop/store 호출에 도달하지 않았다.

구현 후 `TestTeardownDMSnapshotContinuesAfterDMRemoveFailure`가 다음을 강제·검증한다.

- dm remove는 timeout failure
- COW loop와 base loop detach 모두 호출
- full teardown은 store remove 호출, keep-store는 미호출
- dm, 양 loop, store failure detail이 joined error에 포함

`TestCleanupDeletedVMContinuesNetworkReleaseAfterDMSnapshotFailure`는 injected storage error 뒤
TAP/IP release seam이 호출되는 순서를 검증한다.

### Real KVM / independent inventory

`sudo -n env PATH=... bash e2e_test.sh`: `All test steps passed`.

- COW restore/delete: dm device 1개 활성 관측 후 delete, dm 0/store 제거 확인
- concurrent restore cleanup: `cow-vm-*` 0
- COW spawn graceful/crash recovery와 orphan reclaim: 통과
- disk-missing recovery: stale TAP release 확인

종료 후 독립 host inventory:

| Resource | 결과 |
|---|---:|
| `tap[0-9]+` interface | 0 |
| project bind mount | 0 |
| `cow-vm-*` dm-snapshot | 0 |
| project workspace-backed loop | 0 |
| sparse `.cow` store | 0 |
| `vms/`, `snapshots/` entry | 0 |

`goose-br0`의 link-down base route는 bridge baseline이며 guest TAP/IP lease가 아니다.
E2E가 남긴 root-owned Town Wall log 8개와 0-byte rootfs placeholder 7개는 실행 전 empty
상태와 대조해 이번 test artifact임을 확인한 뒤 삭제했다. 최종 `flocks/` entry 0,
`/tmp/goose-workspaces/` absent다.

### Repository gate

- targeted forced-failure tests: 통과
- Go full/race/named builds/vet/module verify/gofmt: 통과
- govulncheck reachable 0, tracked secret scan PASS
- full KVM E2E: 통과
- PR #111 exact code-bearing SHA Go/Web/secret CI: 통과
- `main` merge commit CI run `31630049807`: Go/Web/secret 3 jobs 통과
- `git diff --check`: 통과

## Audit

- bounded 10초 dm retry는 유지했다.
- failure를 성공으로 가장하지 않고 error와 cleanup failure metric을 보존한다.
- keep-store daemon shutdown contract는 store unlink를 하지 않는다.
- HTTP/MCP/schema/storage format 변경 없음.
- tag 생성 없음.

## Blockers

- 실제 kernel이 resource를 계속 busy로 유지하면 후속 detach도 실패할 수 있다. 이 경우
  error와 metric이 남으며 release gate는 host inventory clean 전까지 열려 있다.
- deployment host security operations가 미완료다.

## Warnings

- `TeardownBindMount`는 legacy best-effort void API라 unmount 실패 detail을 반환하지 않는다.
  정상 KVM inventory는 clean이지만 failure error aggregation은 dm path만 강화됐다.

## Residual Risk

- kernel refusal 자체를 소프트웨어가 강제로 성공시킬 수 없다. 이번 보장은 “모든 단계를
  계속 시도하고 잔여를 관측”하는 control-flow invariant다.

## Current Lifecycle Stage

implement, local/KVM verification, code review, exact code-bearing SHA remote CI와 `main`
병합 완료. 실제 배포는 host/version blocker 때문에 `operate` 미진입이다.

## Next Action

배포 전 host resource inventory를 재확인하고 host/version blocker를 해소한다.

## Follow-Up Tasks

1. 배포 전 host resource inventory 재확인
2. host/security/version blockers 전 tag 금지
