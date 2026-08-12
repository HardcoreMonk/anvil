# `allow_hosts` 제거 구현 계획

**날짜:** 2026-08-13

**상태:** 승인된 spec 기반 실행 계획

**Spec:**
[`2026-08-13-allow-hosts-removal-design.md`](../specs/2026-08-13-allow-hosts-removal-design.md)

**Grill:**
[`2026-08-13-allow-hosts-removal.md`](../grill-me/2026-08-13-allow-hosts-removal.md)

## Goal

legacy `allow_hosts`를 runtime과 command planner에서 제거하고, 오래된 profile이 조용히
무시되지 않도록 decode boundary에서 migration error로 거부한다.

## Engineering Review

### Architecture와 data flow

`egress.json` bytes → raw top-level key probe → typed `egressProfile` unmarshal → validation →
iptables command plan 순서를 사용한다. legacy key는 typed decode 전 종료되므로 apply에
도달하지 않는다. HTTP/MCP/guest boundary에는 변화가 없다.

### Security review

- substring matcher source와 emitted command를 모두 제거한다.
- error는 field/migration만 기록하고 host value는 기록하지 않는다.
- key 값이 empty/null이어도 거부한다.
- 현행 allowlist fields의 fail-closed default REJECT는 유지한다.

### Test validity

co-generated test만으로 끝내지 않는다. prove-broken-first test, raw source search,
command plan negative assertion, compiler/전체 unit test를 독립 채널로 사용한다.

### Rollback

git diff를 수동 반전할 수 있으나 legacy matcher 복원은 보안 열화다. operator 호환 문제는
profile migration으로 해결하고, release 후 rollback은 별도 승인 대상으로 둔다.

### Execution Environment Constraints

- Go unit/race/build/vet는 Linux에서 KVM 없이 수행 가능
- full E2E는 `/dev/kvm`, root, Firecracker와 local secrets가 필요하며 최종 release gate에서
  별도 수행
- profile path와 env 이름의 기존 `EPHEMERA_*`/`ANVIL_*` contract는 유지
- tag/publish 금지

### Engineering gate

architecture, error ordering, security, test falsifiability, rollback과 환경 제약을 검토했다.
구현을 막는 issue는 없다.

## Task 1. 제거 계약을 테스트로 먼저 고정

**Consumes:**

- `cmd/goose-daemon/egress_policy.go:48` `loadEgressProfile`
- `cmd/goose-daemon/egress_policy_test.go:11` profile load/plan test
- `cmd/goose-daemon/egress_policy_test.go:212` deprecation warning test

**Files:**

- Modify: `cmd/goose-daemon/egress_policy_test.go`

1. `allow_hosts` non-empty/empty/null을 table test로 load한다.
2. 모두 `ok=false`와 migration error를 기대한다.
3. error가 제거 field와 두 대체 field를 포함하고 host value를 포함하지 않는지 확인한다.
4. current fields profile이 계속 load/plan되는 test로 양성대조한다.
5. 다른 unknown field가 기존대로 무시되는 음성대조를 추가한다.
6. 구현 전 targeted test가 실패하는 prove-broken 결과를 기록한다.

**적대적 수용 기준:**

- 선행조건: 기존 runtime이 non-empty/empty/null legacy key를 허용함
- probe: 값 형태 3종과 secret-like hostname 비노출 assertion
- 독립 채널: loader error + command source/plan negative search
- 음성대조: `allow_sni`/`allow_cidrs`와 unrelated unknown key 정상

## Task 2. 최소 removal 구현

**Consumes:**

- `cmd/goose-daemon/egress_policy.go:38-43` `egressProfile`
- `cmd/goose-daemon/egress_policy.go:48-87` loader/deprecation warning
- `cmd/goose-daemon/egress_policy.go:90-119` validation
- `cmd/goose-daemon/egress_policy.go:208-213` string matcher plan

**Files:**

- Modify: `cmd/goose-daemon/egress_policy.go`

1. `AllowHosts`, warning, validator와 apply loop를 제거한다.
2. raw top-level object에서 `allow_hosts` key를 찾는 decode helper를 추가한다.
3. field와 migration target만 포함한 error를 반환한다.
4. typed unmarshal과 기존 validation error wrapping을 유지한다.
5. `validateDomainCharset` 설명을 `allow_sni` 현재 계약에 맞춘다.

## Task 3. canonical/운영 문서 정합화

**Consumes:**

- `CONTEXT.md`의 egress 상태와 backlog
- `README.md` egress/operator surface
- `RELEASE_NOTES.md` unreleased/current change 기록
- `docs/architecture/service-logic.md` egress policy flow
- `docs/adr/0002-egress-sni-transparent-filter.md` OQ8
- `docs/PUBLIC_RELEASE_BOUNDARY.md`, `docs/operations/runbook.md`,
  `docs/operations/security-policy.md`, `docs/guides/mcp-adapter.md`

1. canonical 문서에서 deprecated/예정 표현을 removed/loud rejection으로 갱신한다.
2. migration mapping과 key-existence rejection을 operator 문서에 쓴다.
3. historical spec/plan은 수정하지 않는다.
4. `README.md`, architecture, release notes를 반드시 포함한다.

## Task 4. 검증과 code review

1. targeted removal tests
2. `go test ./cmd/goose-daemon -count=1`
3. `go test ./... -count=1`
4. `go test -race ./... -count=1`
5. `go build ./...`, `go vet ./...`, `gofmt -l .`
6. `bash scripts/secret-scan.sh`
7. `rg`로 production `allow_hosts`/iptables string matcher 부재 확인
8. Markdown relative-link scan과 `git diff --check`
9. actual diff를 requirement/security/regression/test adequacy 관점에서 review한다.

## Task 5. release-to-operate handoff

`docs/operations/2026-08-13-allow-hosts-removal-handoff.md`에 Release Scope,
Verification, Audit, Blockers, Warnings, Residual Risk, Current Lifecycle Stage,
Next Action, Follow-Up Tasks를 기록한다. 이 변경만으로 tag 또는 operate 진입을 선언하지
않는다.

## 완료 조건

- spec 수용 기준이 실행 증거와 함께 handoff에 기록됨
- legacy key가 값 형태와 무관하게 loud failure
- matcher source/apply path 제거
- canonical/current 문서 정합화
- blocking code review finding 0
- tag/release 생성 없음

