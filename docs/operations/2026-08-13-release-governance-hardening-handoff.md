# Release governance hardening handoff

## 문서 상태

- 날짜: 2026-08-13
- topic: `release-governance-hardening`
- branch: `agent/next-release-gates`
- PR: [#111](https://github.com/HardcoreMonk/anvil/pull/111)
- code-bearing commit: `0aec994089459a64d7bf9e8584f59f4bf6243e4e`
- evidence commit: `72f721331274101f2ea276f84caebe62f5c147ae`
- merge commit: `60ce239ce68555a419994f37c431dcb377825e1f`
- 설계:
  [`2026-08-13-release-governance-hardening-design.md`](../superpowers/specs/2026-08-13-release-governance-hardening-design.md)
- 계획:
  [`2026-08-13-release-governance-hardening.md`](../superpowers/plans/2026-08-13-release-governance-hardening.md)

## Release Scope

- npm transitive advisory remediation
- Node 22 Web check/build/audit CI와 tracked-tree secret CI
- upstream-derived next version selection rule
- PR merge 후 strict `main` branch protection 적용
- no-tag audit

## Verification

### npm prove-broken / fixed

변경 전 `npm audit --json`:

- High 2: `nanoid`, `postcss`
- Moderate 2: `esbuild`, direct `svelte-i18n` effect

patched graph:

- `esbuild@0.28.2` override, Vite와 dedupe
- `postcss@8.5.26`
- `nanoid@3.3.18`
- direct `svelte-i18n@4.0.1` 유지

결과:

- `npm ci`: 통과, vulnerabilities 0
- `npm run check`: error 0, 기존 warning 10
- `npm run build`: 통과, embedded bundle deterministic
- `npm audit --audit-level=moderate`: 0건, exit 0

### CI / repository

- `.github/workflows/ci.yml` PyYAML parse: PASS
- job `web-and-security`: clean install/check/build/moderate audit
- job `secret-scan`: tracked-tree scanner
- workflow permissions: `contents: read`
- Go full/race/named builds/vet/gofmt/govulncheck: 통과
- Markdown relative links 155건, `git diff --check`: PASS

PR #111 final head `72f721331274101f2ea276f84caebe62f5c147ae`의
`build-and-test`, `web-and-security`, `secret-scan`은 모두 green이다. CodeRabbit status는
service review-rate-limit으로 pass됐지만 실제 review는 수행되지 않았다. 이를 approval로
세지 않았고 manual actual-diff review에서 blocking/Important finding은 없었다. PR은 merge
commit `60ce239ce68555a419994f37c431dcb377825e1f`로 병합됐다.

merge commit의 GitHub Actions run `31630049807`에서도 다음 세 job이 모두 통과했다.

- `build-and-test`
- `web-and-security`
- `secret-scan`

이 문서 갱신은 관리자까지 강제하는 보호 규칙을 활성화하기 직전의 마지막 bootstrap
commit이다. 최종 외부 설정의 진실 기준은 이 commit 직후 수행하는 GitHub branch
protection API read-back이다.

## Audit

- direct Svelte package downgrade 없음.
- audit ignore/allowlist 없음.
- CI에 write permission 또는 secret 전달 없음.
- upstream `git ls-remote --tags upstream`: latest `v0.7.0`.
- upstream main: `8db2fb4...`, `v0.7.0` exact.
- tag 생성 없음.

## Blockers

1. next public version number는 post-`v0.7.0` upstream tag 부재로 미할당이다.
2. deployment host A1/A2는 인증/도달 경로 부재로 미완료다.
3. strict branch protection 적용과 read-back이 남았다.

## Warnings

- Svelte `state_referenced_locally` warning 10건은 기존 상태이며 CI는 error로 취급하지 않는다.
- collaborator는 repository owner 1명뿐이다. admin-enforced approval 1을 적용하면 두 번째
  eligible reviewer가 추가될 때까지 이후 PR merge가 차단된다.

## Residual Risk

- npm override는 upstream direct dependency range 밖의 `esbuild`를 강제한다. clean
  install/check/build로 현재 compatibility를 검증했지만 direct package가 dependency를
  갱신하면 override를 재검토해야 한다.
- npm advisory DB 변화는 향후 CI를 새로 실패시킬 수 있으며 이는 의도된 fail-closed다.

## Current Lifecycle Stage

implement/verification/code review, exact-SHA remote CI, PR merge, merge-commit CI가
완료됐다. external protection 적용 직전이며 host/version blocker 때문에 public release
`operate`에는 진입하지 않았다.

## Next Action

이 bootstrap 문서를 병합한 뒤 `main`에 strict status check, approval 1,
conversation-resolution, admin enforcement를 적용하고 API로 read-back한다.

## Follow-Up Tasks

1. `main` protection 적용/read-back
2. 두 번째 eligible reviewer 추가
3. host A1/A2와 upstream version input 전 tag 금지
