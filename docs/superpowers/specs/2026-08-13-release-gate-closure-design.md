# Release gate closure 설계

**날짜:** 2026-08-13

**상태:** 승인됨 — 사용자가 공정 보고서의 후속 작업 1·2·3 전체 실행을 승인

**topic:** `release-gate-closure`

## 목표

2026-08-13 공정 상태 보고서의 사실관계를 재검토하고 분석 색인을 갱신한 뒤, 현재
`main`의 P0 release blocker를 안전한 범위에서 순서대로 닫는다. 검증할 수 없는
blocker는 완료로 과장하지 않고 operation handoff에 남긴다.

## 포함 범위

1. 공정 보고서의 Git/GitHub/lifecycle/build/security 사실 재검토와 정정
2. `docs/analysis/README.md`의 stale upstream baseline 및 신규 보고서 색인 갱신
3. tracked tree secret scan을 깨뜨리는 provider-key sentinel 충돌 해소
4. 변경된 정확한 SHA의 로컬 Go/Web/secret 검증
5. 변경된 정확한 SHA의 GitHub Actions CI 검증
6. 가능한 KVM full E2E와 MCP smoke 검증
7. 다음 release version/scope의 결정 가능성 검토
8. `Release Scope`, 검증, blocker, residual risk와 현재 lifecycle stage를 포함한
   operation handoff 작성

## 비목표

- 정식 tag 생성 또는 GitHub Release publish
- Git history rewrite로 과거 secret-like 문자열 제거
- 실제 credential 출력·복사·변경
- `main` branch ruleset 도입, CI에 Web/secret gate 추가(noted P1)
- npm audit dependency upgrade(noted P1)
- `allow_hosts` 제거를 이 작업에 암묵적으로 포함
- deployment host의 password/key/permission 변경

## Domain Architecture

이 변경은 daemon API, MCP tool, storage format, runtime process 경계를 바꾸지 않는다.
영향 경계는 다음 세 개다.

1. **Test sentinel 경계**
   - `TestConfigProvidersNeverExposeKeyValues`는 실행 시 credential-shaped 값을 실제
     secrets file에 넣고 응답 비노출을 검증한다.
   - source tree scanner는 연속된 credential-shaped literal을 실제 secret과 구분할
     수 없으므로 현재 release gate를 실패시킨다.
2. **Secret scanner 경계**
   - `scripts/secret-scan.sh`의 pattern과 fail-closed tracked-tree 동작은 유지한다.
   - path/file allowlist를 추가하지 않는다. allowlist는 같은 파일에 나중에 들어온
     실제 secret을 숨길 수 있다.
3. **Release evidence 경계**
   - local validation, GitHub CI, KVM E2E, immutable publish는 서로 다른 evidence다.
   - 하나의 green 결과가 다른 채널의 결손을 대체하지 않는다.

architecture pack은 runtime/service/MCP 경계 변경이 없으므로 수정하지 않는다.

### 검증 중 발견된 MCP smoke drift

full KVM 이후 `scripts/anvil-mcp-e2e.sh flock`을 실행하자 smoke가
`agent_id="orchestrator"`를 보냈고, 강화된 daemon authorship guard는 실제 roster ID가
아니라서 `403`으로 거부했다. spawn response의 실제 member는 role별 sequence가 붙은
`orchestrator-1` 형태다.

이는 production daemon이나 MCP API 결함이 아니라 smoke caller가 과거 계약에 고정된
검증 코드 drift다. 이번 release gate를 green으로 만들려면 smoke가 spawn response에서
role이 `orchestrator`인 실제 `AgentID`를 선택하도록 최소 수정한다. public API와 daemon
authorship guard는 변경하지 않는다.

## 설계 결정

### D1. scanner가 아니라 fixture source spelling을 수정한다

sentinel을 두 compile-time string fragment로 구성한다. Go runtime에서 완성되는 값과
non-leak assertion은 동일하지만 source tree에는 scanner pattern과 일치하는 연속
literal이 남지 않는다.

이 방식은 다음 속성을 가진다.

- scanner pattern과 tracked-tree fail-closed 동작 불변
- 실제 secret에 대한 allowlist 없음
- runtime sentinel의 provider-like shape 유지
- 기존 unit test가 값 조립과 response non-leak를 계속 검증
- `bash scripts/secret-scan.sh`가 변경 전 실패, 변경 후 tracked tree PASS를 보여
  falsifiable regression evidence가 됨

### D2. history/local warning을 tracked-tree 실패와 구분한다

Git history에는 과거 fixture literal이 남는다. history rewrite는 fork/upstream 정책과
배치되고 보안상 이득도 없다. 기본 scan의 history warning은 보존하고, current tracked
tree가 green인지 별도로 판정한다. local secrets file warning도 값은 출력하지 않는다.

### D3. release version을 임의로 만들지 않는다

현재 정책은 `anvil-v0.7.0`부터 upstream ephemera version과 정렬한다. 조사 시점
upstream latest는 여전히 `v0.7.0`이다. 또한 `allow_hosts`는 “다음 tagged anvil
release에서 제거”하기로 이미 결정됐지만 제거가 구현되지 않았다.

따라서 이 작업은 `anvil-v0.7.1` 또는 `anvil-v0.8.0`을 임의 확정하지 않는다. 다음
version은 upstream tag 또는 version-policy 결정과 `allow_hosts` 제거 lifecycle이
끝난 뒤 확정한다.

### D4. CI 검증은 변경 SHA를 대상으로 한다

기존 `main@3033cdd`의 runner failure를 재실행하는 것만으로 새 diff가 검증되지는
않는다. 변경을 branch에 commit/push하고 PR Actions가 그 exact SHA에서 green인지
확인한다. runner failure는 code failure와 구분하되 green evidence로 간주하지 않는다.

### D5. KVM과 publish를 분리한다

KVM full E2E는 tag push 전에 실행한다. 성공해도 tag는 만들지 않는다. tag 생성은
Immutable Release publish와 결합된 별도 비가역 승인이다.

### D6. MCP flock smoke는 실제 roster ID를 소비한다

`scripts/anvil-mcp-smoke.go`는 이미 `anvil_spawn_flock`의 structured `Agents`를
decode한다. 별도 ID 추측이나 `role + "-1"` 재구성 대신 response에서
`Role == "orchestrator"`인 `AgentID`를 선택해 `anvil_post_townwall` 입력과 assertion에
사용한다. roster member가 없으면 post 전 명시적으로 실패한다.

## Plan Design Review

- **발견성:** 분석 보고서를 `docs/analysis/README.md`에서 직접 찾을 수 있게 한다.
- **gate 명확성:** tracked/history/local scan 결과와 release blocker를 표로 분리한다.
- **operator 오류 방지:** “local green = CI green” 또는 “CI green = KVM green”으로
  오해하지 않도록 독립 evidence를 유지한다.
- **비가역성:** tag/publish는 명시적으로 비목표이며 자동 실행하지 않는다.
- **visual/UI:** UI 변경이 없어 visual review는 해당 없음. IA와 gate clarity review로
  대체했으며 blocking issue가 없다.

## 수용 기준

1. runtime sentinel 값과 non-leak test 의미가 유지된다.
2. `bash scripts/secret-scan.sh`가 current tracked tree를 PASS하고 exit 0한다.
3. scanner pattern 또는 fail-closed tracked-tree 동작은 약해지지 않는다.
4. `go test ./cmd/goose-daemon -run TestConfigProvidersNeverExposeKeyValues -count=1`
   및 전체 Go 검증이 통과한다.
5. 분석 색인이 upstream `v0.7.0` adopted/adapted baseline과 신규 보고서를 가리킨다.
6. 공정 보고서는 review 상태, 새로운 증거와 남은 blocker를 구분한다.
7. 변경 exact SHA의 GitHub CI 결과를 기록한다.
8. KVM E2E가 불가능하거나 실패하면 이유와 영향을 blocker로 기록한다.
9. version을 정책 근거 없이 발명하지 않는다.
10. operation handoff가 필수 release-to-operate 필드를 모두 포함하고, blocker가 있으면
    `operate` 진입을 선언하지 않는다.
11. MCP flock smoke가 roster에 없는 author의 `403`을 숨기지 않고, 실제 spawn roster
    member로 post/history/delete 전체 경로를 통과한다.

## Spec Freeze Snapshot

- **Approved objective:** 보고서 검토·색인 갱신·P0 순차 처리
- **Domain boundary:** test fixture source, strict scanner, independent release evidence
- **Security decision:** scanner allowlist 금지; compile-time fragment로 source collision만 제거
- **Version decision:** upstream/version policy와 `allow_hosts` 제거 전까지 TBD
- **Publish decision:** tag/release 금지
- **Acceptance:** current tree secret PASS, unit/full local checks, exact SHA CI, KVM result,
  complete pre-release handoff
- **Environment:** x86_64 Linux, `/dev/kvm` 존재, local secrets 값 비출력, GitHub CLI 인증됨
- **Known risks:** hosted runner 가용성, external LLM/network, KVM/Firecracker COW resume race,
  deployment host remediation
- **Open design questions:** 없음. 실행 결과에 따라 release blockers만 남을 수 있음
