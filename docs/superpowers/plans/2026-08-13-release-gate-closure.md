# Release gate closure 구현 계획

**날짜:** 2026-08-13

**상태:** 승인된 spec 기반 실행 계획

**Spec:**
[`2026-08-13-release-gate-closure-design.md`](../specs/2026-08-13-release-gate-closure-design.md)

**Grill:**
[`2026-08-13-release-gate-closure.md`](../grill-me/2026-08-13-release-gate-closure.md)

## Goal

strict tracked-tree secret scan의 false-positive 충돌을 scanner 약화 없이 제거하고,
분석 문서의 사실·색인을 현재 상태로 맞춘 뒤 exact SHA local/remote/KVM evidence와
pre-release handoff를 만든다.

## Engineering Review

### Architecture와 data flow

- daemon/runtime/API data flow 변경 없음
- test source fragment → compile-time concatenation → local secrets fixture → handler read →
  availability-only response의 기존 경로 유지
- scanner input은 raw tracked source이므로 fragment 사이를 결합하지 않음

### Security review

- `scripts/secret-scan.sh:29`의 credential pattern 불변
- `scripts/secret-scan.sh:43-49` tracked-tree fail-closed 불변
- path, line, file allowlist 추가 금지
- actual secret 값 출력 금지

### Rollback

- test constant를 기존 단일 literal로 되돌리면 runtime behavior는 같지만 secret scan은
  다시 실패한다.
- 문서 변경은 해당 diff를 수동 반전할 수 있다.
- tag, release, deployment 변경을 하지 않으므로 외부 runtime rollback은 없음

### Execution Environment Constraints

- Linux x86_64, Go toolchain `/usr/local/go/bin/go`
- Web Node/npm은 `web/package-lock.json` 기준
- KVM 검증은 `/dev/kvm`, root, Firecracker, loop/dm/TAP, external network와 local
  `configs/goose-secrets.yaml` 필요
- GitHub Actions는 hosted runner availability에 의존
- Immutable Release 때문에 tag 생성 금지

### Engineering gate

설계·security·rollback·환경 제약을 검토했다. 구현을 막는 engineering issue는 없다.
환경 실패는 handoff blocker로 남긴다.

## Task 1. Prove-broken secret gate와 최소 수정

**Consumes:**

- `scripts/secret-scan.sh:29` `patterns`
- `scripts/secret-scan.sh:43-49` tracked-tree fail behavior
- `cmd/goose-daemon/config_api_anvil_test.go:188-220`
  `TestConfigProvidersNeverExposeKeyValues`

**Files:**

- Modify: `cmd/goose-daemon/config_api_anvil_test.go`

1. 변경 전 `bash scripts/secret-scan.sh`가 tracked file을 지목하고 exit 1하는 증거를
   보존한다.
2. `providerKeyValue`를 compile-time 두 fragment로 구성하고 scanner를 약화하지 않는
   이유를 주석으로 기록한다.
3. targeted unit test를 실행해 runtime sentinel/non-leak 계약이 유지되는지 확인한다.
4. scanner를 다시 실행해 tracked tree PASS와 exit 0을 확인한다.

**적대적 수용 기준:**

- 선행조건: 분할 전 scanner가 실제로 실패
- 적대적 probe: test source에서 연속 provider-shaped literal 부재 확인
- 독립 채널: Go handler unit test + Git raw source scanner
- 음성대조: scanner regex와 fail branch diff가 없음을 `git diff`로 확인

## Task 2. 분석 보고서 검토와 색인 정합화

**Consumes:**

- `docs/analysis/12-anvil-project-process-status-review-2026-08-13.md`
- `docs/analysis/README.md:3-12`, `:53-64`
- `docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`

**Files:**

- Modify: `docs/analysis/12-anvil-project-process-status-review-2026-08-13.md`
- Modify: `docs/analysis/README.md`

1. 보고서에 검토 상태와 branch/CI/review 추가 증거를 기록한다.
2. 분석 README baseline을 upstream `v0.7.0` adopted/adapted 상태로 갱신한다.
3. 11번 parity review와 12번 공정 보고서를 색인에 추가한다.
4. historical analysis의 ephemera 제목은 anvil로 바꾸지 않는다.

## Task 2a. MCP flock smoke의 roster-author drift 수정

**Consumes:**

- `scripts/anvil-mcp-smoke.go:41-48` `spawnFlockOutput.Agents`
- `scripts/anvil-mcp-smoke.go:211-300` `runFlockSmoke`
- daemon `POST /flocks/{id}/post`의 roster authorship guard

**Files:**

- Modify: `scripts/anvil-mcp-smoke.go`

1. 실제 daemon에서 변경 전 flock smoke가 roster 밖 `orchestrator`로 `403`을 받는
   prove-broken evidence를 보존한다.
2. `spawned.Agents`에서 `Role == "orchestrator"`인 `AgentID`를 선택한다.
3. member를 찾지 못하면 post 전에 명시적 error를 반환한다.
4. post input과 response assertion 모두 선택된 ID를 사용한다.
5. live daemon에서 flock smoke를 재실행하고 post/history/delete를 확인한다.

**적대적 수용 기준:**

- 선행조건: 실제 spawn roster의 ID와 hard-coded author가 다름을 403으로 실증
- 적대적 probe: roster member가 없으면 fail-open하지 않고 post 전 실패
- 독립 채널: daemon의 roster guard + MCP structured spawn response/history
- 음성대조: daemon authorship validation과 MCP public schema에는 diff 없음

## Task 3. 로컬 검증

1. targeted daemon test
2. `bash scripts/secret-scan.sh`
3. Go test/race/build/vet/module/gofmt/govulncheck
4. Web check/build/audit
5. bash syntax와 Markdown relative-link scan
6. `git diff --check`

실패는 코드, dependency, local environment, known warning으로 분류한다.

## Task 4. Exact SHA remote CI

1. 변경을 `chore/release-gate-closure`에 intentional commit한다.
2. branch를 origin에 push하고 draft PR을 연다.
3. GitHub app으로 PR metadata/patch context를 확인한다.
4. `gh pr checks`와 Actions log로 exact SHA 결과를 확인한다.
5. runner failure와 code failure를 구분한다.

PR 생성은 exact SHA CI를 얻기 위한 범위 내 외부 변경이다. merge는 이 계획에 포함하지
않는다.

## Task 5. KVM과 MCP release-candidate gate

선행조건과 local secret file 존재·permission을 값 출력 없이 확인한다.

```bash
sudo bash e2e_test.sh
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
```

각 smoke가 독립 daemon lifecycle을 요구하는지 script 계약을 먼저 읽고 안전한 순서로
실행한다. 실패 시 device/network/provider/runtime 원인을 구분하고 생성 resource cleanup을
확인한다.

## Task 6. Code review와 pre-release handoff

1. actual diff를 requirement/security/maintenance/test adequacy 관점에서 review한다.
2. secret control이 inert하거나 test가 vacuous하지 않은지 확인한다.
3. 다음 version을 정책 근거 없이 확정하지 않는다.
4. `docs/operations/2026-08-13-release-gate-closure-handoff.md`에 다음 필드를 쓴다.
   - Release Scope
   - Verification
   - Audit
   - Blockers
   - Warnings
   - Residual Risk
   - Current Lifecycle Stage
   - Next Action
   - Follow-Up Tasks
5. blocker가 하나라도 남으면 `release`/`operate` 진입을 선언하지 않는다.

## 완료 조건

- spec 수용 기준 1-10의 상태가 handoff에 실제 evidence와 함께 기록됨
- report/index/fixture diff가 review됨
- exact SHA CI와 KVM 결과가 성공 또는 명확한 blocker로 분류됨
- tag/release가 생성되지 않음
