# `allow_hosts` 제거 handoff

## 문서 상태

- 날짜: 2026-08-13
- topic: `allow-hosts-removal`
- branch: `agent/next-release-gates`
- PR: [#111](https://github.com/HardcoreMonk/anvil/pull/111)
- code-bearing commit: `0aec994089459a64d7bf9e8584f59f4bf6243e4e`
- 설계:
  [`2026-08-13-allow-hosts-removal-design.md`](../superpowers/specs/2026-08-13-allow-hosts-removal-design.md)
- 계획:
  [`2026-08-13-allow-hosts-removal.md`](../superpowers/plans/2026-08-13-allow-hosts-removal.md)

## Release Scope

- `egressProfile.AllowHosts`와 iptables `-m string --string ... --algo bm` apply path 제거
- legacy `allow_hosts` key의 명시적 loud fail-closed rejection
- domain → `allow_sni`, IP/CIDR → `allow_cidrs` 문서화
- canonical context, README, release notes, architecture, ADR, operator guide 정합화

tag 또는 GitHub Release는 범위 밖이다.

## Verification

### TDD / effect-anchored evidence

구현 전 targeted test는 non-empty, empty, `null` 세 legacy profile을 모두 `ok=true`,
`err=nil`로 받아 실패했다. non-empty case에서는 기존 deprecation warning까지 발화해
legacy apply contract가 실제로 살아 있음을 보였다.

구현 후:

- `TestLoadEgressProfileRejectsRemovedAllowHosts`: non-empty/empty/`null`/mixed-case 모두 통과
- error는 `allow_hosts`, `allow_sni`, `allow_cidrs`를 포함하고 hostname value는 미포함
- `TestLoadEgressProfileKeepsUnrelatedUnknownFieldCompatibility`: 통과
- `TestLoadEgressProfileAndPlanAllowlistCommands`: CIDR/SNI/DNS current path 통과
- production source search: `AllowHosts`, `--string`, `--algo`, `-m string` 0건

### Repository gate

- `go test ./cmd/goose-daemon -count=1`: 통과
- `go test ./... -count=1`: 통과
- `go test -race ./... -count=1`: 통과
- named daemon/MCP/scheduler build: 통과
- `go vet ./...`, `go mod verify`, `gofmt -l .`: 통과/빈 출력
- `govulncheck ./...`: reachable vulnerability 0
- `bash scripts/secret-scan.sh`: tracked tree PASS
- Markdown relative links 155건: PASS
- `git diff --check`: PASS
- full KVM `e2e_test.sh`: `All test steps passed`
- PR #111 exact code-bearing SHA CI: Go/Web/secret 3 jobs green

## Audit

- removed key는 값 형태와 무관하게 typed validation/apply 전에 종료된다.
- error/log에 legacy hostname value를 반복하지 않는다.
- global `DisallowUnknownFields`를 도입하지 않아 unrelated unknown metadata behavior는 불변이다.
- profile/config example에서 `allow_hosts` 사용 0건이다.
- historical deprecation spec/plan과 과거 handoff의 당시 사실은 보존했다.
- tag 생성 없음.

## Blockers

- PR #111 merge가 아직 남아 있다.
- 다음 public version number는 upstream post-`v0.7.0` tag 부재로 미할당이다.
- deployment host credential/key/permission remediation이 외부 접근 부재로 미완료다.

## Warnings

- `allow_hosts`를 가진 외부 operator profile은 upgrade 후 VM spawn/profile load가 실패한다.
  key 삭제와 migration이 선행돼야 한다.

## Residual Risk

- `allow_sni`의 spoofing/fronting/ECH와 신뢰 golden-image 위협 모델 한계는 ADR-0002에
  그대로 남는다.
- 다른 unknown JSON field는 호환성 때문에 계속 무시된다. 전체 strict schema는 별도 결정이다.

## Current Lifecycle Stage

local implement, verification, code review, exact code-bearing SHA remote CI와 release
handoff 작성이 끝났다. PR merge 전이므로 `operate`에는 진입하지 않았다.

## Next Action

PR #111의 final head CI를 확인하고 병합한다.

## Follow-Up Tasks

1. 외부 profile에서 `allow_hosts` key 제거 여부를 배포 전 확인
2. 통합 PR review/merge
3. host security blocker와 upstream-derived version blocker 해소 전 tag 금지
