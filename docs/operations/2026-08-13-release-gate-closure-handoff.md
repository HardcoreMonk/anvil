# Release gate closure handoff

## 문서 상태

- 날짜: 2026-08-13
- topic: `release-gate-closure`
- branch: `agent/release-gate-closure`
- 기준 parent: `main@3033cddac5a6764c5d1cb12221e3a2d88b1928db`
- draft PR: [#110](https://github.com/HardcoreMonk/anvil/pull/110)
- merge: 완료, `main@794d0ae4debc02612106b647e680084167922ab5`
- code-bearing commit: `576165429b1ecee8b25697b103b533e452a9cb98`
- 설계:
  [`2026-08-13-release-gate-closure-design.md`](../superpowers/specs/2026-08-13-release-gate-closure-design.md)
- 계획:
  [`2026-08-13-release-gate-closure.md`](../superpowers/plans/2026-08-13-release-gate-closure.md)

## Release Scope

이 handoff는 정식 제품 release가 아니라 다음 release의 P0 gate closure 결과를 기록한다.

포함:

- 현재 공정 분석 보고서 사실 재검토
- `docs/analysis/README.md` baseline과 신규 보고서 색인 갱신
- OpenAI·Google non-leak sentinel 2개의 strict source scanner 충돌 해소
- MCP flock smoke의 stale hard-coded author를 spawn roster ID로 교정
- local Go/Web/secret/KVM/MCP 검증
- exact branch SHA GitHub CI 준비

제외:

- tag 생성과 GitHub Release publish
- `allow_hosts` 실제 제거
- branch ruleset/CI surface 확대
- npm dependency upgrade
- deployment host의 credential/key/permission 변경

## Verification

### Secret control — effect-anchored evidence

| 단계 | 명령/관측 | 결과 |
|---|---|---|
| prove broken | `bash scripts/secret-scan.sh` | tracked test file 지목, exit 1 |
| 원인 분리 | raw source pattern probe | 같은 파일의 OpenAI·Google sentinel 2개 확인 |
| runtime 양성대조 | 두 config non-leak targeted Go test | 통과 |
| scanner 독립채널 | 수정 후 `bash scripts/secret-scan.sh` | tracked tree PASS, exit 0 |
| 음성대조 | `git diff -- scripts/secret-scan.sh` | scanner pattern/fail branch diff 없음 |

runtime sentinel은 compile-time concatenation 뒤 기존과 동일한 provider-shaped 값이다.
scanner allowlist는 추가하지 않았다.

### Go와 repository

- `go test ./... -count=1`: 통과
- `go test -race ./... -count=1`: 통과
- `go build ./...`: 통과
- `go build -o anvil-daemon ./cmd/goose-daemon/`: 통과
- `go build ./cmd/anvil-mcp`: 통과
- `go build ./cmd/anvil-scheduler`: 통과
- `go vet ./...`: 통과
- `go mod verify`: 통과
- `gofmt -l .`: 출력 없음
- `govulncheck ./...`: reachable vulnerability 0
- bash release/E2E script syntax: 통과
- 변경 문서 relative-link scan: 통과
- `git diff --check`: 통과

### Web

- `npm run check`: 오류 0, 기존 `state_referenced_locally` warning 10
- `npm run build`: 통과, embedded `uidist` deterministic
- `npm audit --audit-level=high`: exit 1, High 2 + Moderate 2
- `npm audit --omit=dev`: exit 1, Moderate 2

### KVM full E2E

첫 실행은 `sudo` secure PATH에 Go가 없어 stale `goose-agent` build preflight에서 종료됐다.
제품 test 단계 전 환경 실패이며, explicit `/usr/local/go/bin` PATH로 재실행했다.

재실행:

```bash
sudo -n env PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  bash e2e_test.sh
```

결과: `All test steps passed`.

AGENTS의 표준 표기인 `sudo bash e2e_test.sh`와 동형이며, 실제 실행은 sudo secure PATH에
Go를 명시하기 위해 위 `sudo -n env PATH=... bash e2e_test.sh`를 사용했다.

확인된 주요 경로:

- VM spawn/task/stream/delete
- full/diff snapshot, restore, dependency delete guard
- COW spawn/recovery/orphan cleanup
- restored-VM recovery와 live snapshot reference `409`
- flock lifecycle, Town Wall, dynamic membership, pause/resume/watchdog
- auth token rotation, audit, TTL, metrics
- MCP gateway/stdio health, privilege drop, rlimit, process reap

환경변수 기반 선택적 real-LLM step 71, MCP gateway Tier B와 stdio MCP Tier B는 skip됐다.
기본 task와 별도 MCP semantic smoke에서는 실제 `anvil-smoke-ok` response를 확인했다.

### MCP smoke

| mode | 최초 결과 | 최종 결과 |
|---|---|---|
| `lifecycle` | live daemon 없이 호출 시 연결 실패 | daemon 기동 후 통과 |
| `semantic` | live daemon 없이 호출 시 연결 실패 | daemon 기동 후 `anvil-smoke-ok` 통과 |
| `flock` | roster 밖 `orchestrator` post가 `403` | actual `orchestrator-1` roster ID로 수정 후 통과 |

flock prove-broken은 daemon authorship guard가 실제로 roster 밖 author를 거부한다는
독립 보안 채널이기도 하다. 수정은 smoke caller에만 적용했고 daemon guard와 MCP schema는
변경하지 않았다.

### Remote CI

- GitHub CLI auth: 확인됨(`repo`, `workflow` scope)
- PR #109 과거 실패 원인: hosted runner 통신/할당 실패, code failure 아님
- PR #110 code-bearing exact SHA: `576165429b1ecee8b25697b103b533e452a9cb98`
- GitHub Actions:
  [run 31624693889](https://github.com/HardcoreMonk/anvil/actions/runs/31624693889)
- 결과: Gofmt, Build, Vet, Test, Govulncheck 전부 통과
- 이 handoff 증적 추가는 documentation-only 후속 commit이다. 자기 commit SHA를 문서에
  재귀적으로 고정할 수 없으므로 PR #110의 latest head check를 최종 exact-SHA canonical
  external evidence로 사용한다. merge/release 전 latest check가 green이어야 한다.

## Audit

- 실제 credential 값 출력 없음
- local `configs/goose-secrets.yaml`: mode 0600, 값 미열람
- current tracked-tree secret scan: PASS
- history scan: 과거 secret-like commit warning 유지; history rewrite 안 함
- ignored/local scan: local secrets file warning 유지
- scanner 정규식/실패 동작: 불변
- full E2E 전 삭제 대상 `vms/`, `snapshots/`, `flocks/`, `/tmp/goose-workspaces/`:
  모두 empty 확인
- E2E/MCP 종료 후 resource별 probe: VM state 0, TAP/IP lease 0, project bind mount 0,
  `cow-vm-*` dm-snapshot 0, anvil workspace-backed loop device 0, sparse `.cow` store 0.
  root-owned Town Wall log 10개와 0-byte workspace placeholder 7개는 실행 전 empty 상태와
  ID를 대조한 뒤 이번 test artifact만 비재귀 삭제했고, `vms/`, `snapshots/`, `flocks/`
  entry 0 및 `/tmp/goose-workspaces/` 부재를 재확인했다.
- PR #110 사후 CodeRabbit review에서 dm remove failure 뒤 후속 loop/store cleanup이
  중단되는 source path를 발견했다. 이 정상-path inventory만으로 failure-path invariant를
  과장하지 않고, `vm-deletion-failure-cleanup` full lifecycle의 forced failure test와
  수정으로 별도 폐쇄한다. 해당 lifecycle이 green이기 전 release gate는 열린 상태다.
- tag/release/deployment mutation: 없음

## Blockers

1. 다음 anvil version은 post-`v0.7.0` upstream tag가 없어 의도적으로 미할당이다.
2. deployment host credential/key/permission remediation은 인증·도달 경로 부재로
   완료되지 않았다.

## Warnings

- strict `main` branch protection은 최종 bootstrap 증적 직후 외부 설정으로 적용한다.
- owner가 한 명뿐이므로 approval 1/admin enforcement 적용 뒤 두 번째 eligible reviewer가
  추가될 때까지 새 PR merge가 차단된다.
- PR #109 merge 당시 actionable documentation review comment 2개가 미해결이었다.
- `CONTEXT.md` 마지막 문장 절단, `RELEASE_NOTES.md` release workflow 이력 drift,
  Svelte migration spec의 끊어진 ADR 링크가 남아 있다.
- Svelte check warning 10건의 허용 정책이 명시적으로 정규화되지 않았다.
- 선택적 real-LLM/MCP Tier B E2E 일부가 환경변수 부재로 skip됐다.

## Residual Risk

- COW diff-restore의 Firecracker/KVM resume race는 upstream residual risk이며 `plain`
  default/COW opt-in을 유지한다.
- single-host에서는 cross-host wall/gtcall/failover를 완전히 재현할 수 없다.
- history scan warning은 의도된 test fixture와 과거 기록을 포함하며 current tree PASS를
  대체하지도, current leak를 뜻하지도 않는다.
- release workflow는 tag push와 public immutable publish가 결합되어 있다.

## Code Review

2026-08-13 staged diff를 requirement fit, security, cleanup, maintainability, test
adequacy 관점에서 검토했다.

- blocker/Important finding: 없음
- scanner allowlist 또는 regex 완화: 없음
- runtime/API/schema 변경: 없음
- sentinel runtime value와 leak assertion: 유지
- flock smoke: spawn response의 canonical roster ID를 사용하며 member 부재 시 post 전 실패
- cleanup: 실패 경로의 deferred flock delete 유지
- 문서: historical ephemera 제목을 보존하고 current anvil 분석만 추가
- 검증 유효성: secret control은 prove-broken, runtime unit test, independent raw-source scan,
  음성대조를 모두 가짐. MCP flock은 실제 daemon의 roster guard와 history를 관측

CodeRabbit 완료 리뷰는 merge 직후 도착했다. 4건 중 zone-relative lifecycle link 지적은
실제 target 존재로 invalid 처리했고, named build와 수용 기준 11 누락은 문서 보정했다.
dm removal failure 뒤 loop/store cleanup 중단 Major finding은 별도
[`vm-deletion-failure-cleanup`](2026-08-13-vm-deletion-failure-cleanup-handoff.md)
full lifecycle/TDD로 수정·검증했다.

## Current Lifecycle Stage

후속 PR #111에서 deprecated contract 제거, dependency/CI 보강, cleanup failure-path 수정이
`main`에 병합됐고 merge commit CI까지 통과했다. host와 upstream version input blocker가
남아 있어 public `release` 또는 `operate`에는 진입하지 않았다.

## Next Action

1. strict `main` protection을 적용하고 read-back
2. host와 upstream version input blocker가 남아 있으므로 tag는 생성하지 않음

## Follow-Up Tasks

1. upstream에 post-`v0.7.0` tag가 생긴 뒤 다음 anvil version 확정
2. deployment host security operations 종료
3. branch protection/required review 설정
4. 두 번째 eligible reviewer 추가
