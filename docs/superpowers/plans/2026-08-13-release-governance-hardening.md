# Release governance hardening 구현 계획

**날짜:** 2026-08-13

**상태:** 승인된 spec 기반 실행 계획

**Spec:**
[`2026-08-13-release-governance-hardening-design.md`](../specs/2026-08-13-release-governance-hardening-design.md)

**Grill:**
[`2026-08-13-release-governance-hardening.md`](../grill-me/2026-08-13-release-governance-hardening.md)

## Engineering Review

### Dependency와 workflow

package override는 lockfile에 resolved graph를 고정하고 `npm ci`로 재현한다. GitHub Actions
jobs는 contents read만 필요하며 secret write/PR write 권한이 없다.

### Governance ordering

workflow commit → PR exact checks → review/merge → main checks 확인 → branch protection 적용
순서다. protection을 먼저 적용해 sole-owner deadlock을 만들지 않는다.

### Rollback

dependency override/workflow는 git revert 가능하다. protection은 GitHub API로 이전 설정을
복원할 수 있다. tag는 만들지 않아 immutable rollback 문제가 없다.

### Execution Environment Constraints

- local Node 22/npm 9와 CI Node 22
- npm registry/advisory DB network 필요
- GitHub admin token과 branch protection API 필요
- upstream tag 조회는 `git ls-remote --tags upstream`, force fetch 금지

### Engineering gate

dependency compatibility, job least privilege, required context ordering, sole-owner deadlock,
version alignment와 rollback을 검토했다. 구현 blocker 없음.

## Task 1. npm advisory를 prove-broken 후 제거

1. 기존 `npm audit --json`에서 High 2/Moderate 2를 기록한다.
2. `package.json`에 최소 transitive overrides를 추가한다.
3. `npm install`로 lockfile을 갱신한다.
4. `npm ci`, check, build, audit moderate를 실행한다.
5. `npm ls`로 patched resolved versions를 확인한다.

## Task 2. Web/secret CI

1. `.github/workflows/ci.yml`에 Node 22 `web-and-security` job을 추가한다.
2. npm cache dependency path를 `web/package-lock.json`으로 고정한다.
3. `web` working-directory에서 ci/check/build/audit를 실행한다.
4. 별도 `secret-scan` job에서 strict tracked-tree scanner를 실행한다.
5. permissions `contents: read` 외 권한을 추가하지 않는다.

## Task 3. version/canonical docs

1. `CONTEXT.md`, `README.md`, `RELEASE_NOTES.md`에 next-upstream exact transform을 기록한다.
2. current next number가 unassigned blocker임을 쓴다.
3. downstream-only patch/minor 추측을 금지한다.

## Task 4. local/remote 검증과 PR

1. Go 전체와 named builds, race/vet/gofmt/secret scan
2. Web clean install/check/build/audit
3. workflow syntax와 `git diff --check`
4. intentional commit/push, PR 생성
5. exact SHA의 세 Actions job + CodeRabbit 결과 확인
6. actionable review 처리 후 merge

## Task 5. branch protection

1. collaborator inventory를 재확인한다.
2. `main`에 strict required checks 4개, admin enforce, approval 1, stale review dismiss,
   last-push approval, conversation resolution, force-push/delete 금지를 적용한다.
3. GET API로 실제 설정을 read-back한다.
4. reviewer 부재를 handoff follow-up으로 기록한다.

## Task 6. no-tag audit와 handoff

1. origin/upstream tag inventory를 재확인한다.
2. head points-at tag가 없음을 확인한다.
3. `docs/operations/2026-08-13-release-governance-hardening-handoff.md`에 필수 fields와
   remaining host/version blockers를 기록한다.

## 완료 조건

- spec 수용 기준 1–8 evidence 기록
- npm moderate+ advisory 0
- exact SHA jobs/CodeRabbit green
- PR merged 뒤 protection read-back 일치
- no tag

