# Release governance hardening grill-me 기록

**날짜:** 2026-08-13

**topic:** `release-governance-hardening`

**설계:**
[`2026-08-13-release-governance-hardening-design.md`](../specs/2026-08-13-release-governance-hardening-design.md)

## 압박 질문과 결정

### Q1. `npm audit fix --force`를 쓰면 더 간단한가?

direct `svelte-i18n`을 3.x로 downgrade하는 major change를 제안한다. Svelte 5 app behavior
risk가 크므로 patched transitive override와 check/build를 선택한다.

### Q2. audit가 dev-server advisory인데 production blocker인가?

현재 repository가 source와 build toolchain을 배포·개발 contract로 포함하고, High
postcss/nanoid도 함께 있다. CI가 moderate부터 0을 요구해 supply-chain baseline을 단순하게
유지한다.

### Q3. Web과 secret을 Go job에 합치면 안 되는가?

가능하지만 failure ownership과 required context가 흐려진다. 독립 job은 병렬화되고
branch rule에서 각 control의 존재를 확인할 수 있다.

### Q4. CodeRabbit가 approval 1건을 대신할 수 있는가?

아니다. 현재 bot은 status/comment review이며 GitHub approving review가 아니다. strict
human approval을 만족하려면 두 번째 eligible collaborator가 필요하다.

### Q5. owner 한 명인데 admin enforce 1 approval은 지나치지 않은가?

사용자는 required review를 요청했고 강한 release posture가 목적이다. protection은
reversible하며 repository admin이 reviewer를 추가하거나 rule을 변경할 수 있다. silent
bypass보다 명시적 잠금과 follow-up을 선택한다.

### Q6. 다음 version을 `anvil-v0.8.0`으로 확정하면 안 되는가?

upstream alignment policy와 충돌한다. upstream에는 post-v0.7.0 tag가 없다. 실제 번호를
추측하지 않고 first-next-upstream transform을 확정하는 것이 현재 가능한 유일한 정합
결정이다.

### Q7. branch rule을 PR 전에 켜면 어떻게 되는가?

현재 collaborator 구조에서는 이 PR 자체를 merge할 수 없다. 변경 PR을 merge한 뒤 exact
checks가 존재할 때 protection을 적용한다.

### Q8. tag가 우발적으로 생기지 않았는지 어떻게 확인하는가?

작업 전후 `git tag --points-at HEAD`, `git ls-remote --tags origin/upstream`을 비교하고
handoff에 no-new-tag를 기록한다.

## Grill 결과

- 열린 질문: 없음
- blocker: branch protection 적용은 변경 PR merge 후 수행
- 고정 결정: audit moderate 0, 독립 jobs, admin-enforced 1 approval, upstream-derived version,
  no tag

