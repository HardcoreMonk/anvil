# VM 삭제 실패 cleanup grill-me 기록

**날짜:** 2026-08-13

**topic:** `vm-deletion-failure-cleanup`

**설계:**
[`2026-08-13-vm-deletion-failure-cleanup-design.md`](../specs/2026-08-13-vm-deletion-failure-cleanup-design.md)

## 압박 질문과 결정

### Q1. dm device가 남았는데 loop detach를 시도하는 것이 의미가 있는가?

성공 가능성은 낮지만 생략하면 cleanup 시도 자체가 0이다. dependency가 예상과 다르게
풀린 경우 성공할 수 있고, 실패해도 error aggregation이 정확한 잔여를 보여 준다.

### Q2. store를 unlink하면 orphan recovery가 더 어려워지는가?

full delete는 store 보존 의도가 없다. unlink는 path leak을 없애고 열린 inode는 kernel
reference 종료 후 회수된다. keep-store shutdown 경로는 unlink하지 않는다.

### Q3. 무한 retry가 더 안전하지 않은가?

HTTP delete와 daemon shutdown을 무한정 멈출 수 없다. 기존 10초 bounded retry를 유지하고
후속 cleanup·error reporting으로 진행한다.

### Q4. 실패를 무시하고 성공 metric을 올리는 문제는 없는가?

기존 `anvil_cleanup_failure_total`은 teardown error에서 증가한다. VM identity 삭제와
network release 완료 의미의 delete metric은 유지하되 handoff/release gate는 resource
inventory로 별도 판정한다.

### Q5. fake command test가 실제 kernel behavior를 증명하는가?

실제 busy semantics 자체가 아니라 control flow 불변식(실패 뒤 다음 단계 호출)을
결정적으로 증명한다. 정상 real KVM E2E와 host inventory를 두 번째 modality로 사용한다.

### Q6. bind-mount restore도 같은 결함이 있는가?

`TeardownBindMount`는 unmount 실패와 무관하게 disk remove를 계속한다. 이번 regression은
dm branch에 한정하되 KVM inventory에서 bind mount도 확인한다.

### Q7. TAP/IP release는 현재도 storage error 뒤 호출되지 않는가?

코드상 그렇지만 회귀 test가 없다. cleanup helper를 분리해 storage failure를 강제하고
network release 호출을 실행 증거로 고정한다.

### Q8. 실제 dm failure fault injection을 production host에서 해야 하는가?

의도적으로 kernel resource를 남기는 fault는 운영 host를 오염시킬 수 있다. deterministic
unit fault injection으로 failure branch를 검증하고, real KVM은 정상 경로 잔여 inventory를
검증한다. 실제 kernel refusal에서 “모든 제거 성공”은 보장하지 않고 error/blocker로 남긴다.

## Grill 결과

- 열린 질문: 없음
- blocker: 없음
- 고정 결정: bounded retry 유지, early return 제거, keep-store 보존, aggregate error,
  forced unit failure + real inventory

