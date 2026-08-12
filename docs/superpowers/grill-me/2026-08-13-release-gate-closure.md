# Release gate closure grill-me 기록

**날짜:** 2026-08-13

**대상 spec:**
[`2026-08-13-release-gate-closure-design.md`](../specs/2026-08-13-release-gate-closure-design.md)

## 압박 질문과 결정

### Q1. 특정 test file을 scanner allowlist에 넣으면 가장 간단하지 않은가?

**기각.** 파일 전체 예외는 같은 파일에 실수로 추가된 실제 secret도 숨긴다. line marker
예외도 marker 남용과 scanner 복잡도를 만든다. scanner는 그대로 두고 test runtime 값만
compile-time fragment로 구성한다.

### Q2. 문자열을 분할하면 non-leak test가 약해지지 않는가?

**아니다.** Go constant concatenation으로 runtime 값은 이전과 byte-for-byte 동일하다.
테스트는 secrets file에 완성된 값을 쓰고, precondition으로 디스크 존재를 확인한 뒤,
HTTP response에 그 완성값이 없는지 검사한다.

### Q3. strict history scan까지 green으로 만들어야 하는가?

**아니다.** 과거 commit에는 의도된 fixture literal이 남고 이를 없애려면 history rewrite가
필요하다. fork/upstream 이력 보존 정책을 어긴다. release gate는 current tracked tree
PASS를 요구하고 history는 rotation 검토용 warning으로 보존한다.

### Q4. `main@3033cdd` CI만 재실행하면 충분한가?

**아니다.** 이번 작업은 diff를 만든다. exact changed SHA를 PR/push workflow로 검증해야
한다. 과거 runner failure 재실행은 원인 분류 증거일 뿐 새 변경의 CI가 아니다.

### Q5. hosted runner가 다시 실패하면 local green으로 대체할 수 있는가?

**릴리스에는 불가.** local 결과는 강한 보조 증거지만 remote reproducibility를 대체하지
않는다. runner failure로 분류하고 handoff blocker로 유지한다.

### Q6. KVM E2E 성공 뒤 바로 tag를 만들 것인가?

**아니다.** tag push는 public Immutable Release publish를 유발한다. 이 작업은 tag를
만들거나 push하지 않는다.

### Q7. 다음 version은 `anvil-v0.7.1`이 자연스럽지 않은가?

**확정하지 않는다.** 현 정책은 upstream ephemera version 정렬이고 upstream latest가
`v0.7.0`이다. 별도의 downstream patch version 정책이 없다. `allow_hosts` 제거 계약도
다음 tag 전에 이행돼야 한다.

### Q8. P0 closure에 `allow_hosts` 제거까지 포함해야 하는가?

**자동 확장하지 않는다.** public config behavior change이고 별도 full lifecycle/TDD가
필요하다. 대신 다음 tag blocker로 명시한다.

### Q9. local secret file warning을 없애기 위해 파일을 삭제할 것인가?

**아니다.** 통합 테스트 전제이며 `.gitignore`로 보호되는 operator-owned 파일이다. 값은
읽거나 출력하지 않고 존재·권한만 확인한다.

### Q10. PR #109의 미해결 자동 review 지적을 이번 범위에서 고칠 것인가?

**P0와 분리한다.** 사실 검토 결과로 보고서와 handoff warning에 남긴다. 두 지적은 문서
언어/정확성 문제이며 release 전 처리 권고지만 secret gate closure와 결합하지 않는다.

### Q11. flock smoke가 실패했으니 daemon의 authorship guard를 완화할 것인가?

**아니다.** roster 밖 author 거부는 PR #92 이후의 의도된 보안 계약이다. smoke가
hard-code한 `orchestrator`는 실제 `orchestrator-1` member가 아니다. spawn response에서
실제 roster ID를 소비하도록 smoke만 수정한다.

## Grill 결과

- 열린 설계 질문: 없음
- design blocker: 없음
- execution blocker 가능성: hosted runner, KVM/LLM/network 환경
- 비가역 작업: tag/release publish — 명시적 제외
