# VM 삭제 실패 cleanup 설계

**날짜:** 2026-08-13

**상태:** 승인됨 — PR #110 사후 CodeRabbit review와 프로젝트 불변 조건에 따른 보안 수정

**topic:** `vm-deletion-failure-cleanup`

## 목표

명시적 VM 삭제 중 dm-snapshot device 제거가 실패해도 loop device detach, sparse COW
exception-store 제거, TAP/IP release가 중단되지 않게 한다. 모든 cleanup 단계의 오류를
모아 반환·계측하되 한 단계 실패가 뒤 단계를 건너뛰는 control flow를 금지한다.

## 문제 증거

현재 `internal/storage.teardownDMSnapshot`은 `dmsetup remove --retry`가 retry window 안에
성공하지 못하면 즉시 return한다. 따라서 다음 `losetup -d` 2회와
`os.Remove(ExceptionStore)`가 시도조차 되지 않는다. `destroyVMUnderSnapshotLock`은 storage
호출 뒤 TAP/IP를 release하지만, 내부 loop/store leak을 식별 가능한 실행 test가 없다.

## 포함 범위

1. dm removal failure 뒤에도 COW/base loop detach와 store removal 시도
2. 모든 단계 error aggregation 유지
3. `KeepStore` 경로는 store를 보존하면서 kernel object cleanup을 계속 시도
4. forced storage failure 뒤 ControlPlane의 TAP/IP release 지속을 test seam으로 검증
5. 정상 KVM E2E 종료 뒤 TAP/IP, bind mount, dm device, loop, `.cow` 잔여 inventory 확인
6. PR #110 release-gate plan/handoff에 실제 named build와 cleanup evidence 보강

## 비목표

- kernel이 busy resource 제거를 거부할 때 성공을 가장하는 것
- unrelated orphan cleanup 구조 재작성
- VM delete HTTP response contract 변경
- tag/release 생성

## Domain Architecture

- `ControlPlane.destroyVMUnderSnapshotLock`: VM identity 제거, VMM stop, host resource
  cleanup을 orchestration한다.
- `storage.TeardownDMSnapshot`: mount → dm device → COW/base loop → optional store 순서의
  best-effort cleanup owner다.
- `network.Manager.Release`: TAP/IP lease cleanup owner다.

storage failure가 network boundary로 전파되어 cleanup을 중단하면 안 된다. 반대로
ControlPlane이 storage 내부 단계 성공을 추측하지 않고 반환 error를 계측한다.
HTTP/MCP surface와 storage format은 바뀌지 않아 architecture pack 추가 수정은 필요 없다.

## 설계 결정

### D1. early return을 제거하고 오류를 누적한다

dm removal retry가 소진되면 error를 `errs`에 추가한 뒤 loop detach와 store removal을
계속 시도한다. 각 단계 error도 `errors.Join`으로 반환한다.

### D2. dependency 실패 뒤 시도는 의도된 best-effort다

dm device가 loop를 계속 참조하면 `losetup -d`가 실패할 수 있다. 그래도 명령을 생략하는
것보다 시도하고 각 실패를 독립 증거로 남기는 편이 cleanup invariant에 맞다.
store unlink는 path 잔여를 없애며 열린 inode의 실제 공간은 kernel reference가 사라질 때
회수된다. 반환 error가 있으므로 성공으로 과장하지 않는다.

### D3. deterministic fake operations로 forced failure를 실증한다

private helper에 command runner, file remover, clock/sleep을 주입한다. production wrapper는
`exec.Command`, `os.Remove`, real clock을 사용한다. test는 dm removal을 강제로 계속
실패시키고 이후 두 loop detach와 store remove가 호출됐는지 순서와 error로 관측한다.

### D4. ControlPlane의 독립 TAP/IP continuation을 별도 test한다

post-VMM host cleanup을 작은 helper로 추출한다. injected storage teardown이 실패해도
injected network release가 호출되는지 검증한다. storage와 network self-report 하나에
의존하지 않는다.

### D5. 운영 gate는 잔여 resource가 하나라도 있으면 열어 둔다

KVM E2E 후 host inventory에서 project-owned TAP/IP, bind mount, dm-snapshot, loop,
sparse `.cow`를 각각 확인한다. 잔여가 있으면 release blocker다.

## Plan Design Review

- operator는 하나의 “cleanup passed” 문장 대신 resource type별 결과를 본다.
- error aggregation은 첫 실패와 후속 실패를 모두 보존한다.
- cleanup failure metric은 기존대로 증가한다.
- UI 변경 없음. gate clarity와 failure discoverability review로 대체했으며 blocker 없음.

## 수용 기준

1. forced `dmsetup remove` failure 뒤 COW/base `losetup -d`가 모두 호출된다.
2. full teardown이면 store removal이 호출되고, keep-store면 호출되지 않는다.
3. dm/loop/store errors가 반환 error에 함께 포함된다.
4. storage teardown error 뒤에도 TAP/IP release가 호출된다.
5. 정상 삭제의 기존 ordering과 metrics가 유지된다.
6. Go test/race/build/vet/gofmt와 KVM E2E가 통과한다.
7. E2E 후 TAP/IP, bind mount, dm device, loop device, sparse `.cow` 잔여가 없다.
8. named daemon/MCP/scheduler build 증거가 기록된다.
9. tag/release를 생성하지 않는다.

## Spec Freeze Snapshot

- **Objective:** deletion failure path에서 모든 host resource cleanup 계속 시도
- **Boundaries:** ControlPlane orchestration, storage dm teardown, network TAP/IP release
- **Control decision:** early return 제거 + aggregate errors
- **Test decision:** forced fake dm failure + CP network continuation + real KVM inventory
- **KeepStore:** file preservation 불변
- **Release rule:** resource 잔여 하나라도 blocker, tag 금지
- **Open questions:** 없음

