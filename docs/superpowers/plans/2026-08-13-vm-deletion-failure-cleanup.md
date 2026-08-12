# VM 삭제 실패 cleanup 구현 계획

**날짜:** 2026-08-13

**상태:** 승인된 security invariant 기반 실행 계획

**Spec:**
[`2026-08-13-vm-deletion-failure-cleanup-design.md`](../specs/2026-08-13-vm-deletion-failure-cleanup-design.md)

**Grill:**
[`2026-08-13-vm-deletion-failure-cleanup.md`](../grill-me/2026-08-13-vm-deletion-failure-cleanup.md)

## Engineering Review

### Data flow와 ordering

VMM stop → socket/vsock/egress cleanup → storage teardown(all attempts) → TAP/IP release 순서를
유지한다. storage 내부는 unmount → bounded dm remove → loop detach 2개 → optional store
remove이며, 실패는 진행 제어가 아니라 반환 evidence다.

### Security와 rollback

resource leak을 줄이는 fail-cleanup change다. rollback은 early return을 되살려 보안 열화가
되므로 compatibility rollback 사유가 없다. HTTP/schema/data format은 불변이다.

### Execution Environment Constraints

- deterministic unit fault는 KVM/root 불필요
- real inventory는 Linux x86_64, `/dev/kvm`, root, dm/loop/TAP tools 필요
- host-wide resource probe는 anvil prefix/workspace로 scope를 제한
- tag/publish 금지

### Engineering gate

dependency ordering, bounded wait, keep-store, metrics, test seam과 host 안전을 검토했다.
구현 blocker 없음.

## Task 1. storage early-return regression을 test-first로 고정

**Consumes:**

- `internal/storage/snapshot.go:354` `teardownDMSnapshot`
- `internal/storage/snapshot.go:339` `TeardownDMSnapshot`
- `internal/storage/snapshot.go:347` `TeardownDMSnapshotKeepStore`

**Files:**

- Modify: `internal/storage/snapshot_test.go`
- Modify: `internal/storage/snapshot.go`

1. command/remove/clock를 기록하는 fake operations를 만든다.
2. dm remove를 강제로 timeout 실패시킨다.
3. 구현 전 loop/store 호출 누락으로 test가 실패하는 prove-broken evidence를 기록한다.
4. early return을 제거하고 후속 단계 error를 누적한다.
5. full/keep-store 양쪽을 검증한다.

**적대적 수용 기준:** forced dm failure, 두 loop detach 호출, full store remove 호출,
keep-store 미호출, 모든 error join.

## Task 2. ControlPlane TAP/IP continuation 회귀

**Consumes:**

- `cmd/goose-daemon/api.go:1806` `destroyVMUnderSnapshotLock`
- `cmd/goose-daemon/api.go:1840` COW teardown branch
- `cmd/goose-daemon/api.go:1854` network release

1. VMM stop 이후 host-resource cleanup을 helper로 추출한다.
2. delete-only dm teardown test seam을 둔다.
3. storage error를 강제하고 cleanup failure metric과 network release 호출을 검증한다.
4. bind/plain disk branches와 ordering은 보존한다.

## Task 3. PR #110 review 문서 보정

1. plan 완료 기준을 spec 1–11로 고친다.
2. 세 named build와 `sudo bash e2e_test.sh`를 plan/handoff에 명시한다.
3. resource type별 inventory와 failure gate를 handoff에 기록한다.
4. zone-relative governance 링크 지적은 실제 path probe를 근거로 오탐 처리한다.

## Task 4. 검증

1. targeted forced-failure tests
2. `go test ./... -count=1`, `go test -race ./... -count=1`
3. `go build -o anvil-daemon ./cmd/goose-daemon/`
4. `go build ./cmd/anvil-mcp`
5. `go build ./cmd/anvil-scheduler`
6. `go vet ./...`, `gofmt -l .`, secret scan
7. `sudo bash e2e_test.sh`
8. scoped TAP/IP, bind mount, dm, loop, `.cow` post-inventory
9. `git diff --check`와 code review

## Task 5. handoff

`docs/operations/2026-08-13-vm-deletion-failure-cleanup-handoff.md`에 필수 operate fields와
잔여 kernel refusal risk를 기록한다. cleanup 잔여 또는 test 실패가 있으면 release/operate
진입을 선언하지 않는다.

## 완료 조건

- spec 수용 기준 1–9 evidence 기록
- forced failure 뒤 모든 후속 cleanup 시도
- CP network continuation 실행 검증
- real KVM inventory clean
- review blocker 0, tag 없음

