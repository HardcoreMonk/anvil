# anvil 프로젝트 공정 상태 고강도 조사 보고서

## 문서 상태

- 조사 기준일: 2026-08-13 (Asia/Seoul)
- 조사 대상: `HardcoreMonk/anvil`의 로컬 `main`, GitHub 원격 상태, 릴리스·CI,
  lifecycle 산출물, 로컬 빌드·테스트·보안 게이트
- 기준 커밋: `3033cddac5a6764c5d1cb12221e3a2d88b1928db`
- 조사 방식: read-only 상태 조사 및 비파괴 로컬 검증
- 사실 재검토: 2026-08-13 `release-gate-closure` lifecycle에서 GitHub app/CLI와
  source-level probe로 재검토. 핵심 판정 승인, 아래 두 항목 정정
  - secret scan 원인은 한 개가 아니라 같은 test file의 OpenAI·Google sentinel 2개
  - PR #109에는 merge 당시 미해결 CodeRabbit documentation comment 2개가 존재
- 실행 갱신: 같은 날 사용자가 후속 작업 1–7 전체 실행을 승인했다. 원 조사 snapshot은
  보존하고 §14에 PR #110 merge 이후 gate closure 진행 상태를 추가했다.
- 문서 목적: 다음 정식 태그 이전에 현재 공정 단계, 증적 공백, release blocker와
  후속 작업을 하나의 기준 보고서로 고정
- 비밀정보 처리: 실제 credential, 호스트 주소, password, private key는 기록하지 않음

## 1. 집행 요약

현재 프로젝트는 제품 기준선과 개발 기준선의 공정 상태가 분리되어 있다.

| 대상 | 공정 판정 | 근거 |
|---|---|---|
| 공개 릴리스 `anvil-v0.7.0` | `operate` | 공개 tag/release와 과거 release handoff가 존재 |
| 현재 `main@3033cdd` | 실질적으로 release 준비 상태 | 구현과 로컬 검증은 대부분 완료 |
| 현재 `main`의 보수적 공식 단계 | `code-review` | aggregate release handoff와 정확한 HEAD의 green 원격 CI가 없음 |
| 다음 정식 tag | release gate 미통과 | secret scan 실패, KVM 최종 검증·공식 handoff 미완료 |
| 코드 건강도 | 양호 | Go build/test/race/vet, Web check/build 통과 |
| 릴리스·거버넌스 건강도 | 취약 | branch rule 부재, 증적 drift, CI surface 부족 |
| 운영 보안 잔여 | 미완료 | 배포 호스트의 credential·permission 후속 조치 잔존 |

핵심 결론은 다음과 같다.

> `anvil-v0.7.0`은 `operate` 상태다. 현재 `main`은 구현과 로컬 검증이 대부분
> 끝난 release candidate 성격이지만, lifecycle 증적과 release gate가 충족되지
> 않아 `release` 또는 `operate` 진입을 선언할 수 없다.

따라서 **현재 상태에서 정식 tag를 생성해서는 안 된다.** Immutable Releases가
활성화되어 있고 tag push가 자동 publish를 유발하므로, tag 생성은 사실상 되돌리기
어려운 외부 상태 변경이다.

## 2. 판정 기준과 조사 방법

### 2.1 진실 기준

프로젝트 지침에 따라 충돌하는 정보는 다음 순서로 판정했다.

1. [`CONTEXT.md`](../../CONTEXT.md)
2. [`README.md`](../../README.md)
3. [`RELEASE_NOTES.md`](../../RELEASE_NOTES.md)
4. [`docs/architecture/`](../architecture/)
5. [`docs/analysis/`](./)
6. 과거 spec, plan, handoff와 기타 초안

공정 단계는 zone 기준
[`codex-lifecycle-control-plane.md`](../../../docs/governance/codex-lifecycle-control-plane.md)를
적용했다. 표준 순서는 다음과 같다.

```text
intake
  -> superpowers:brainstorming
  -> domain-architecture
  -> grill-me
  -> plan-design-review
  -> superpowers:writing-plans
  -> plan-eng-review
  -> implement
  -> code-review
  -> release
  -> operate
```

ADR lifecycle은 이 project pipeline과 별개이며, pipeline stage 통과만으로 ADR이
승인됐다고 판단하지 않았다.

### 2.2 조사 범위

다음 증거를 상호 교차검증했다.

- Git branch, tag, commit ancestry, working tree, remote와 fork 관계
- GitHub PR, Actions check, release, ruleset, issue/discussion 설정
- spec, plan, grill-me, code-review, release/operations handoff의 수량과 연결성
- Go 전체 package build/test/race/vet와 module 검증
- Web type/check/build와 dependency audit
- release secret scan과 shell script 구문
- 로컬 KVM·runtime artifact·service 상태
- private audit evidence의 공개 경계와 미종결 운영 조치

통합 KVM E2E는 root 권한, Firecracker 실행, TAP·device-mapper·loop device, 네트워크,
LLM API key를 사용하는 침습적 검증이므로 이번 read-only 조사에서는 실행하지 않았다.

## 3. Git·fork·upstream 상태

### 3.1 로컬과 원격 정합성

- 현재 branch: `main`
- 현재 HEAD: `3033cddac5a6764c5d1cb12221e3a2d88b1928db`
- `main`과 `origin/main`: 일치
- 조사 종료 시 working tree: clean
- `origin`: `https://github.com/HardcoreMonk/anvil/`
- `upstream`: `https://github.com/steve-seungeui/ephemera`

fork network를 유지해야 한다는 프로젝트 불변 조건은 지켜지고 있다. standalone
repository로 detach된 정황은 없다.

### 3.2 upstream sync 상태

`git ls-remote --tags upstream`과 upstream `main` 조회 결과, upstream 최신 tag
`v0.7.0`의 peeled commit과 upstream `main`이 모두 `8db2fb4...`를 가리킨다.

따라서 조사 시점에는 새 upstream runtime tag나 `main` 변경을 anvil로 sync해야 하는
즉시 backlog가 없다.

### 3.3 공개 릴리스와 미태그 변경량

- 최신 공개 제품 릴리스: `anvil-v0.7.0`
- 공개일: 2026-07-06
- 현재 HEAD는 `anvil-v0.7.0` 이후 Git ancestry 기준 416 commit 앞섬
- 변경 범위: 256 files, 약 `+46,454 / -4,528`

이 변경량은 작은 patch release 후보가 아니다. 개별 PR의 merge 여부만으로 전체
release scope가 설명되지 않으므로, 다음 tag 전에 aggregate release note와 handoff가
필요하다.

[`RELEASE_NOTES.md`](../../RELEASE_NOTES.md)는 미태그 변경을 구분하지만 최상단
`anvil-v0.7.0 current release` 아래 tagged/untagged 정보가 함께 누적되어 있어, 현재
HEAD 전체를 특정 다음 버전과 직접 연결하지는 못한다.

## 4. GitHub 공정과 CI 상태

### 4.1 PR과 backlog 표면

- 열린 PR: 0
- Issues: 비활성화
- Discussions: 비활성화
- milestone: 없음

따라서 열린 PR이 없다는 사실을 “잔여 작업이 없다”는 뜻으로 사용할 수 없다. 실제
backlog와 residual risk는 문서 및 private audit evidence에 분산되어 있다.

### 4.2 branch 보호와 review 강제성

GitHub 조회 결과 `main`에 branch protection이나 repository ruleset이 없다.

그 결과 다음 항목이 원격에서 강제되지 않는다.

- merge 전 required CI
- 최소 승인 수
- stale approval dismissal
- 직접 push 제한
- tag/release 전 별도 승인

최근 PR #91-#109에서도 공식 `reviewDecision` 또는 approval 증적이 거의 없다. 일부
자동화 review comment는 존재하지만, 이는 사람의 lifecycle approval을 대체하지
않는다.

PR #109에는 merge 당시 CodeRabbit의 actionable documentation comment 2개가
미해결 상태로 남아 있었다. 하나는 변경 설명의 한국어 문서 규칙, 다른 하나는 CP token
fan-out 대상이 `cpTokenManaged`와 non-empty `vsockPath`를 모두 요구한다는 조건이다.
따라서 PR #109를 review debt가 전혀 없는 merge로 보지 않는다.

### 4.3 정확한 HEAD의 CI 공백

현재 HEAD와 연결된 green GitHub status context는 없다.

- PR #109의 CI 실패는 hosted runner 통신 상실로 발생했으며 코드 실패 증거는 아님
- PR #108 이후 `main` run은 job을 획득하지 못하고 취소됨
- 최근 release rehearsal도 hosted runner를 할당받지 못해 취소됨
- PR #107과 #108 이전의 주요 구현 CI에는 green 결과가 존재

즉, 로컬 검증 결과가 강하더라도 **정확한 release candidate가 원격의 재현 가능한
green check를 가졌다는 증거는 없다.** runner 장애는 결함 판정을 완화할 수 있지만
release evidence를 대신하지는 못한다.

참고 Actions 실행:

- [PR #109 CI runner failure](https://github.com/HardcoreMonk/anvil/actions/runs/31125006706)
- [최근 release probe runner failure](https://github.com/HardcoreMonk/anvil/actions/runs/31128017386)
- [과거 release workflow 성공](https://github.com/HardcoreMonk/anvil/actions/runs/31102901745)

### 4.4 release workflow와 immutable 상태

[`release.yml`](../../.github/workflows/release.yml)은 tag push를 publish trigger로
사용한다. 과거 `anvil-v0.0.0-ci-probe`에서 FULL/SLIM archive와 SHA256 asset 생성까지
성공했다.

GitHub API 기준 Immutable Releases는 다음 상태다.

- `enabled: true`
- `enforced_by_owner: false`

repository 수준 기능은 활성화되어 있으나 owner-wide enforcement는 아니다. 과거
probe 성공 때문에 [`RELEASE_NOTES.md`](../../RELEASE_NOTES.md)의 “release workflow가
한 번도 실행되지 않았다”는 기록은 현재 사실과 일치하지 않는다.

## 5. lifecycle 산출물 조사

### 5.1 산출물 수량

조사 시점 파일명 기준 집계는 다음과 같다.

| 산출물 | 수량 |
|---|---:|
| spec | 32 |
| plan | 40 |
| grill-me 기록 | 2 |
| `*handoff.md` | 23 |

문서 수는 많지만 lifecycle 단계 진입을 증명하는 형식이 일관되지 않다.

### 5.2 handoff 단계 표기

23개 handoff 중 `Current Lifecycle Stage` 또는 명시적인 현재 lifecycle 단계가 있는
문서는 3개뿐이다.

- 2026-05-11 redesign handoff
- 2026-06-02 cross-host snapshot replication handoff
- 2026-06-02 scheduler operations hardening handoff

나머지 20개는 내용상 완료 또는 운영 진입을 설명하더라도 현 lifecycle contract가
요구하는 명시적 stage 증거가 없다. 과거 문서가 현 표준보다 먼저 작성됐을 가능성은
고려해야 하지만, 현재 상태를 공식적으로 재구성하는 데는 여전히 증적 공백이다.

### 5.3 spec-plan 연결성

- 같은 날짜·topic 규칙으로 직접 연결되지 않는 plan: 10개
- 같은 날짜·topic 규칙으로 직접 연결되지 않는 spec: 2개

날짜가 다른 후속 plan 또는 의도적인 topic rename일 수 있으므로 이를 곧바로 절차
위반으로 판정하지는 않았다. 다만 도구나 신규 참여자가 spec → review → plan →
implementation → handoff 계보를 자동으로 추적하기 어렵다.

### 5.4 현재 `main`의 공식 단계 판정

개별 변경은 구현과 PR merge가 끝났고 로컬 검증도 통과했다. 그러나 현재 `main`
전체에 대해 다음 필수 증거가 없다.

- 확정된 다음 제품 버전
- aggregate release scope
- 정확한 HEAD의 green 원격 CI
- 정확한 HEAD의 KVM full E2E
- release blocker와 residual risk의 최종 disposition
- `Current Lifecycle Stage: release` 또는 `operate` handoff

그러므로 실제 작업 성숙도는 release 준비에 가깝지만, 증적 기반 공식 단계는
`code-review`에 두는 것이 안전하다.

## 6. 로컬 코드 검증 결과

### 6.1 Go 검증

저장소의 [`go.mod`](../../go.mod)는 Go `1.25.12` toolchain을 사용한다. 기본 PATH에
`go`가 없었으나 `/usr/local/go/bin/go`로 pinned toolchain을 정상 사용했다.

| 검증 | 결과 |
|---|---|
| `go test ./... -count=1` | 통과 |
| `go test -race ./... -count=1` | 통과 |
| `go build ./...` | 통과 |
| `go vet ./...` | 통과 |
| `go mod verify` | 통과, 모든 module 검증 |
| `gofmt -l .` | 출력 없음 |
| `govulncheck ./...` | reachable vulnerability 0 |

`govulncheck`은 required module에 존재하는 2개 vulnerability를 알렸지만 anvil 코드의
실제 호출 경로는 찾지 못했다. 조사 시점 Go vulnerability DB는 2026-08-11 갱신분이다.

### 6.2 Web 검증

| 검증 | 결과 |
|---|---|
| `npm run check` | 오류 0, Svelte warning 10 |
| `npm run build` | 통과 |
| build 후 working tree | clean |

경고는 5개 파일의 Svelte `state_referenced_locally` 유형이다. build를 막지는 않지만
허용된 warning인지 일시적으로 남은 migration debt인지 CI 정책에 기록되어 있지 않다.

### 6.3 npm dependency audit

`npm audit --audit-level=high` 결과는 exit 1이며 총 4건이다.

| 심각도 | 수량 | 주요 범위 | 판정 |
|---|---:|---|---|
| High | 2 | `nanoid`, `postcss` | dev/build chain, release 전 우선 조치 |
| Moderate | 2 | `esbuild` 계열 | production dependency에도 잔존 |

`npm audit --omit=dev`에서는 Moderate 2건만 남는다. 따라서 High 2건이 runtime bundle에
직접 포함된다고 단정할 수는 없지만, build supply chain 위험으로 별도 disposition이
필요하다.

현재 [`ci.yml`](../../.github/workflows/ci.yml)은 Go format/build/vet/test와
`govulncheck`만 수행한다. Web check/build/audit는 CI에서 강제되지 않는다.

### 6.4 shell과 repository hygiene

- installer, release build, E2E 관련 bash script 구문 검사: 통과
- 현재 working tree `git diff --check`: 통과
- `anvil-v0.7.0..HEAD` 범위에는 generated minified JS와 일부 문서의 trailing/EOF
  whitespace 4건이 존재

범위 내 whitespace는 runtime blocker는 아니지만, 정식 release diff의 hygiene
경고로 분류한다.

## 7. 보안 게이트 조사

### 7.1 secret scan 실패

[`scripts/secret-scan.sh`](../../scripts/secret-scan.sh) 실행 결과:

- tracked tree: 실패
- Git history: secret-like 과거 변경 warning
- ignored/local file: 예상된 local secret file warning

tracked tree 실패 위치는
[`cmd/goose-daemon/config_api_anvil_test.go`](../../cmd/goose-daemon/config_api_anvil_test.go)의
OpenAI profile-secrets non-leak sentinel과 Google provider-key non-leak sentinel 2개다.
값은 실제 credential이 아니라 테스트를 위해 secret과 유사한 형태로 만든 문자열이다.

그러나 scanner는 이 의도된 fixture들을 구별할 allowlist나 fixture convention이 없기
때문에 exit 1을 반환한다. 따라서 판정은 다음과 같다.

- 실제 credential 유출 증거: 없음
- test fixture가 검출된 사실: 맞음
- scanner 정책과 fixture 계약의 불일치: 있음
- 현재 release secret gate: **실패**

이를 단순 false positive로 무시하면 향후 실제 secret까지 수동 예외로 처리할 위험이
있다. secret-shaped fixture를 보존하면서 scanner가 좁고 감사 가능한 방식으로 이를
구별하게 해야 한다.

### 7.2 private audit와 공개 경계

private `anvil-dev` 측에는 2026-08 security audit 증적이 별도 유지된다. 공개하면
미해결 vulnerability 또는 host exposure 정보를 노출할 수 있어 public `main`과
분리한 정책은 타당하다.

비밀 내용을 제외한 상태 요약은 다음과 같다.

- HIGH 5건: 종료
- MEDIUM 구현 항목: 사실상 종료
- Immutable Release 항목: repository 수준 활성화
- code/documentation audit program: 종료
- 배포 호스트 credential rotation: 미완료
- 배포 호스트 SSH key rollout: 미완료
- 배포 호스트 permission remediation: 미완료
- LOW/INFO 개별 disposition: 일부 미정규화

private plan의 일부 상단 상태 문구와 checklist box는 이후 완료 기록과 일치하지 않는
stale 표현을 포함한다. 공개 문서에 상세 vulnerability를 복제하지 말고, public
release handoff에는 sanitized completion status와 residual operations action만
기록해야 한다.

## 8. runtime·운영 상태와 잔존 위험

### 8.1 조사 workstation의 운영 상태

- `ephemera` systemd unit: 비활성 또는 미설치
- `anvil-scheduler` systemd unit: 비활성 또는 미설치
- 로컬 3000/3010 health endpoint: serving 상태 아님
- `/dev/kvm`: 존재하고 접근 가능
- Firecracker artifact: pinned `v1.16.1` 실행 파일 확인
- guest kernel artifact: 존재

이는 배포 환경 장애를 뜻하지 않는다. 이 checkout이 현재 production service를
serve하지 않는 개발·검증 workstation이라는 뜻이다.

### 8.2 KVM full E2E 공백

PR #107 구현 commit에는 full KVM 결과 `334✓/0✗`가 기록되어 있고 이후 PR #108,
#109는 문서 변경이다. 코드 회귀 가능성은 낮지만, Immutable Release 직전 검증은
정확한 tag candidate에서 다시 수행해야 한다.

필수 조건은 다음과 같다.

- `/dev/kvm`
- root 권한
- Firecracker 실행 가능 호스트
- local `configs/goose-secrets.yaml`
- 외부 LLM API 접근

### 8.3 COW diff-restore 잔존 위험

COW diff-restore의 guest kernel panic은 부하 중 Firecracker/KVM resume race로 분류되어
있다. 현재 정책은 다음과 같다.

- 기본 disk mode: `plain`
- COW: 명시적 opt-in
- Firecracker `v1.16.1`: 실패율 완화
- 근본 해결: upstream 추적

관찰 실패율은 환경에 따라 약 15-25%로 기록되어 있다. 이는 미구현 기능이라기보다
수용된 platform residual risk다. 다음 release handoff에서 숨기지 말고 운영 제약으로
명시해야 한다.

### 8.4 cross-host 검증 제약

single-host 환경에서는 guest gateway/bridge 충돌 때문에 cross-host wall, `gtcall`,
failover E2E를 동시에 재현할 수 없다. 관련 검증은 실제 분리된 두 host 또는 이를
격리하는 전용 network namespace 설계가 필요하다.

## 9. 문서 drift와 추적성 문제

### 9.1 최상위 진실 문서의 불완전성

[`CONTEXT.md`](../../CONTEXT.md)의 마지막 부분은 문장 중간에서 끝난다. 가장 높은
진실 기준 문서가 syntactically incomplete하므로 마지막 residual risk와 후속 결정이
누락됐을 가능성을 배제할 수 없다.

### 9.2 release workflow 이력 불일치

[`RELEASE_NOTES.md`](../../RELEASE_NOTES.md)는 release workflow가 실행된 적 없다고
기록하지만 GitHub에는 성공한 CI probe가 있다. 과거 문장의 시점을 명시하거나 현재
사실로 교정해야 한다.

### 9.3 끊어진 내부 링크

[`2026-07-19-svelte5-runes-migration-design.md`](../superpowers/specs/2026-07-19-svelte5-runes-migration-design.md)는
존재하지 않는 `docs/adr/013-dom-safe-frontend-zone-wide.md`를 참조한다.

### 9.4 분석 색인의 stale baseline

[`docs/analysis/README.md`](README.md)는 anvil runtime baseline을 upstream `v0.3.6`,
`v0.7.0` 미병합으로 설명한다. 그러나 이후 parity review와 현재 Git state는 upstream
`v0.7.0` 채택·적응 완료를 가리킨다. 진실 기준 우선순위상 상위 문서가 이 설명을
덮어쓰지만, 분석 진입자가 잘못된 baseline을 읽을 가능성이 있다.

## 10. 위험 분류

### 10.1 P0 — release blocker

1. secret scan이 tracked test fixture 2개에서 exit 1
2. 정확한 HEAD의 green GitHub CI 부재
3. 정확한 tag candidate의 KVM full E2E 부재
4. 다음 version과 aggregate release scope 미확정
5. release/operate 진입을 선언하는 공식 handoff 부재
6. `allow_hosts` 제거가 “다음 tagged anvil release” 계약이지만 아직 미구현
7. upstream version 정렬 정책상 upstream `v0.7.0` 이후의 다음 anvil version 근거 부재

### 10.2 P1 — release 전 disposition 필요

1. npm audit High 2건과 Moderate 2건
2. branch protection/required review 부재
3. Web check/build/audit와 secret scan이 CI에 없음
4. 배포 호스트 credential·SSH key·permission 조치 미완료
5. `CONTEXT.md`, `RELEASE_NOTES.md`, 분석 색인의 drift
6. Svelte warning 10건의 허용 정책 미정규화

### 10.3 P2 — 수용 가능하지만 명시해야 하는 잔존 위험

1. COW diff-restore resume race
2. single-host cross-host E2E 제약
3. release 범위 내 committed whitespace 4건
4. historical handoff의 lifecycle stage 누락
5. LOW/INFO security finding의 개별 disposition 정규화 부족

## 11. 권고 release gate

다음 조건을 모두 만족하기 전에는 정식 tag를 생성하지 않는다.

| Gate | 통과 조건 | 현재 상태 |
|---|---|---|
| Source | clean tree, exact release SHA 고정 | 부분 통과 |
| Go | build/test/race/vet/govulncheck green | 통과 |
| Web | check/build green, audit disposition | 부분 통과 |
| Secret | tracked tree strict scan green | 실패 |
| Remote CI | exact SHA GitHub Actions green | 실패 |
| KVM | exact SHA full E2E green | 미실행 |
| Security ops | host follow-up 완료 또는 승인된 명시적 exception | 미통과 |
| Documentation | version/scope/release notes 정합 | 미통과 |
| Lifecycle | release handoff와 `Current Lifecycle Stage` 명시 | 미통과 |
| Publish | immutable tag/release 승인 | 대기 |

## 12. 후속 작업

### P0 — 다음 tag 이전

1. 두 secret-shaped sentinel의 runtime 값을 유지하되 source spelling을 분할해 scanner
   allowlist 없이 tracked tree를 green으로 만든다.
2. strict tracked-tree와 history scan을 재실행하고 결과를 release handoff에 기록한다.
3. 정확한 release SHA에서 GitHub CI를 green으로 재실행한다.
4. 동일 SHA로 `sudo bash e2e_test.sh`와 MCP lifecycle/semantic/flock smoke를 실행한다.
5. 다음 제품 version, 포함 PR/commit, 공개 기능, 제외·defer 항목을 확정한다.
6. residual risk, blocker, warning, rollback/cleanup, `Current Lifecycle Stage`를 포함한
   aggregate release handoff를 작성한다.

### P1 — release governance 보강

1. `main`에 required CI, 최소 review, direct-push 제한을 적용한다.
2. CI에 Web check/build/audit와 secret scan을 추가한다.
3. npm High 2건을 우선 해소하고 Moderate 2건은 fix/defer 근거를 기록한다.
4. 배포 호스트의 password/key rotation과 permission remediation을 종료한다.
5. `CONTEXT.md` 절단부, `RELEASE_NOTES.md` 실행 이력, 끊어진 ADR 링크를 교정한다.
6. `docs/analysis/README.md`의 upstream baseline과 문서 색인을 현재 사실로 갱신한다.

### P2 — 운영·추적성 개선

1. 기존 주요 handoff에 현재 stage와 replacement/supersession 관계를 보강한다.
2. spec-plan-review-handoff의 stable ID 또는 상호 링크 규칙을 도입한다.
3. COW opt-in 위험과 cross-host 검증 제약을 운영 handoff에 계속 명시한다.
4. LOW/INFO audit finding을 accept/fix/defer/invalid 중 하나로 정규화한다.

## 13. 최종 판정

현재 `main`은 코드 건강도만 보면 release candidate에 가깝다. 그러나 프로젝트의
표준 공정은 코드가 빌드되는 것만으로 `release`나 `operate` 진입을 허용하지 않는다.
정확한 SHA의 재현 가능한 검증, secret gate, 운영 후속 조치, release scope와 handoff가
함께 닫혀야 한다.

최종 판정은 다음과 같다.

```text
released baseline: anvil-v0.7.0 = operate
current main:       code-review -> release 경계
release gate:       failed
tag authorization:  hold
```

구현 자체보다 릴리스 공정의 강제성·재현성·증적 일치가 현재 가장 큰 프로젝트 위험이다.

## 14. 후속 작업 1–7 실행 갱신

### 14.1 완료 또는 코드상 폐쇄

| 항목 | 결과 | 증거 |
|---|---|---|
| PR #110 review/merge | 완료 | exact head `c394f7d...`의 Go CI green, unresolved thread 0에서 merge commit `794d0ae...`로 `main` 병합. CodeRabbit 완료 리뷰는 merge 직후 회수 |
| strict secret gate | 완료 | tracked tree PASS, scanner allowlist/regex 완화 없음 |
| KVM release-candidate gate | 완료 | full E2E `All test steps passed`, lifecycle/semantic/flock smoke는 선행 handoff에서 통과 |
| `allow_hosts` 제거 | 완료·`main` 병합 | field/validation/iptables string matcher 제거, non-empty/empty/`null`/mixed-case key loud rejection, unrelated unknown metadata 호환 |
| VM 삭제 실패 cleanup | 완료·`main` 병합 | CodeRabbit 사후 Major finding. forced dm failure 뒤 양 loop/store/TAP-IP cleanup continuation test, KVM resource inventory clean |
| npm audit | 폐쇄 | High 2/Moderate 2 → 0, clean install/check/build 통과 |
| Web/secret CI | 완료·`main` 강제 표면 편입 | `web-and-security`, `secret-scan` 독립 job, `contents: read`, merge commit CI green |
| version policy | 결정 규칙 확정 | upstream 첫 post-`v0.7.0` tag `vX.Y.Z` → `anvil-vX.Y.Z`; downstream-only 번호 금지 |

### 14.2 PR #110 사후 review disposition

CodeRabbit가 merge 완료 직후 4개 actionable comment를 게시했다.

1. lifecycle 링크 복구 지적: **invalid**. 이 프로젝트 지침은 zone 상대 경로를 사용하며
   `../../../docs/governance/codex-lifecycle-control-plane.md`는 실제 zone 문서로 해석돼
   존재한다. changed-document relative-link scan도 통과했다.
2. named Go build evidence 누락: **valid**, plan/handoff 보정과 실제 세 build 실행.
3. spec 수용 기준 11 누락: **valid**, plan 완료 조건을 1–11로 보정.
4. dm remove failure 뒤 loop/store cleanup 중단: **valid Major**, 별도
   `vm-deletion-failure-cleanup` full lifecycle/TDD로 수정.

### 14.3 아직 열린 blocker

1. **Host A1/A2:** 비밀 주소를 출력하지 않는 재점검에서 A1 endpoint 한 대는 tcp/22에
   도달하지만 현재 개발 키를 거부하고, 나머지 두 배포 endpoint는 tcp/22 도달 불가였다.
   password rotation, key rollout, permission remediation을 실행할 인증 경로가 없다.
2. **Next version number:** upstream latest/main이 계속 `v0.7.0`이라 결정 규칙의 입력이
   없다. 번호는 의도적으로 미할당이다.
3. **Remote integration:** PR #111은 merge commit `60ce239ce68555a419994f37c431dcb377825e1f`로
   병합됐다. 해당 merge commit의 CI run `31630049807`에서 Go/Web/secret 3개 job이
   모두 green이다. CodeRabbit는 rate limit로 실제 review를 수행하지 못해 approval로
   세지 않았고 별도 manual actual-diff review에서 blocking/Important finding은 없었다.
4. **Branch protection bootstrap:** 이 최종 증적 commit 직후 strict status checks,
   approval 1, conversation resolution, admin enforcement를 적용·read-back한다. collaborator가
   owner 1명뿐이라 적용 후 두 번째 eligible reviewer가 추가될 때까지 새 PR merge는 차단된다.

### 14.4 갱신 판정

```text
released baseline:     anvil-v0.7.0 = operate
main after PR #111:    implementation + release-gate CI merged
governance bootstrap: protection apply/read-back pending
host security gate:    blocked (access unavailable)
next version number:   blocked (no post-v0.7.0 upstream tag)
tag authorization:     hold
```

코드·CI 측 P0 대부분은 폐쇄됐지만 host security와 upstream version input은 아직 외부
blocker다. 따라서 후속 작업 7의 “blocker 종료 전 tag 금지”는 계속 유효하다.
