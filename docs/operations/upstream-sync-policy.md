# anvil upstream sync 정책

## 목적

`HardcoreMonk/anvil`은 `steve-seungeui/ephemera`의 fork로 유지한다. ephemera는
Firecracker runtime engine upstream이고, anvil은 그 runtime을 IronClaw 실행 계층으로
통합하는 downstream product fork다.

이 문서는 ephemera 버전업을 anvil에 반영할 때의 branch, tag, commit 관리 기준을
정한다.

## Remote 계약

```bash
origin   git@github.com:HardcoreMonk/anvil.git
upstream https://github.com/steve-seungeui/ephemera.git
```

`upstream`은 읽기 전용으로 취급한다. 로컬에서는 accidental push를 막기 위해 push URL을
비활성 값으로 둔다.

```bash
git remote add upstream https://github.com/steve-seungeui/ephemera.git
git remote set-url upstream https://github.com/steve-seungeui/ephemera.git
git remote set-url --push upstream DISABLED
```

## Sync branch 규칙

ephemera upstream 반영은 `main`에서 직접 하지 않는다. 항상 전용 sync branch를 만든다.

```bash
git fetch upstream main
git ls-remote --tags upstream
git fetch upstream tag v0.4.5
git checkout -b sync/ephemera-v0.4-runtime-core origin/main
git merge --no-ff v0.4.5
```

특정 upstream release 기준으로 맞출 때는 먼저 tag를 remote에서 확인한다.

```bash
git ls-remote --tags upstream
```

기존 local `v*` tag와 upstream tag가 충돌할 수 있으므로 `git fetch --tags --force`로
local tag를 덮어쓰지 않는다. 필요한 경우 upstream tag의 peeled commit SHA를 확인한 뒤
그 commit을 merge한다.

## Conflict 처리 기준

conflict가 runtime engine 영역에서 발생하면 ephemera upstream 계약을 우선한다.

우선권이 높은 영역:

- `cmd/goose-daemon`
- `cmd/goose-agent`
- `cmd/micro-init`
- `internal/storage`
- `internal/network`
- `internal/vm`
- `EPHEMERA_*` 환경 변수와 `goose-*` 이름을 쓰는 public contract

anvil 영역은 upstream runtime 계약에 맞춰 적응한다.

- `cmd/anvil-mcp`
- `cmd/anvil-scheduler`
- `internal/anvilmcp`
- `ANVIL_*` alias와 IronClaw MCP tool contract
- 운영 문서와 release note

## Commit과 PR 규칙

upstream sync PR은 다음 형태를 권장한다.

- branch: `sync/ephemera-v0.4-runtime-core`
- merge commit: `merge: sync ephemera v0.4 runtime core`
- follow-up adaptation commit: `fix(runtime): adapt anvil to ephemera v0.4 runtime core`
- docs commit: `docs: document ephemera v0.4 runtime baseline`

sync PR은 upstream merge commit과 anvil adaptation commit을 분리한다. 이렇게 해야
upstream 변경 자체와 anvil에서 해결한 conflict/적응 작업을 review에서 구분할 수 있다.

## 현재 runtime baseline

anvil main runtime baseline은 upstream ephemera `v0.4.5` tag까지 병합·적응한 runtime을
포함한다. `v0.3.2`-`v0.3.6`은 이전 release(`anvil-v0.3.x`)에서 채택한 baseline이고,
`v0.4.0`-`v0.4.5`는 이 v0.4 sync로 병합·적응해 full KVM gate로 검증한 baseline이다.
2026-07-02 기준 upstream `main`과 최신 upstream tag는 `v0.7.0`까지 진행되어 있다.
`v0.3.2`-`v0.3.5` 병합 commit은 `1ebe201 Merge upstream/main`이고, `v0.3.6`은
`v0.3.6` tag commit을 merge한다.

| tag | peeled commit | 요약 | anvil 현재 상태 |
|---|---|---|---|
| `v0.3.2` | `f5e0de694a5584acb1a20436a0b3ae912d862792` | live VM cold-restart, `vms/<vm_id>/state.json`, orphan cleanup, network re-reservation | 병합됨, `adapted` |
| `v0.3.3` | `3c24e6086e8b16380c94cf16d4d19dec960c9675` | watchdog dead-status persistence, per-agent restart, in-VM CP token auto-injection, real-LLM e2e | 병합됨, `adapted` |
| `v0.3.4` | `5580482c7911d184bcd950347f050642535c431a` | `EPHEMERA_API_TOKENS_FILE`, SIGHUP CP-token fan-out, watchdog tunables/auto-heal, Firecracker SIGHUP hot-fix | 병합됨, `adapted` |
| `v0.3.5` | `7a6d42dd56361719bc4fb592e75e0c8d8d9cf211` | `/metrics`, `/vms/{vm_id}/stats`, `log/slog`, observability demo | 병합됨, `adapted` |
| `v0.3.6` | `4bd5e8c3d94fbfb862de116caa7417f7b640b325` | autonomous webdev demo, in-VM `gtcall`, multi-line-safe `gtwall`, Goose JSON output parsing | sync branch에서 병합, `adapted` |

`v0.4.0`-`v0.4.5` 채택 상태(peeled upstream SHA 대신 이 v0.4 sync의 merge/adapt
commit 기준으로 기록한다):

| tag | 상태 | anvil adaptation 요지 |
|---|---|---|
| `v0.4.0` | 병합됨, `adapted` | storage/recovery core. `EPHEMERA_AUTOSNAPSHOT=true` auto-snapshot은 opt-in·disk-expensive로 두고 public support로 승격하지 않는다. |
| `v0.4.1` | 병합됨, `adapted` | client identity, `GET /audit`, per-token TTL, `ephemera-ctl` operator CLI(IronClaw MCP 대체 아님). |
| `v0.4.2` | 병합됨, `adapted`, default cow deferred | COW probe/fallback + COW+Diff snapshot. `EPHEMERA_DISK_MODE=cow`는 명시적 opt-in이고 default 전환은 KVM burn-in 뒤 결정한다. |
| `v0.4.3` | 병합(`aab3299`), `adapted` | dynamic flock membership, pause/resume, per-flock `max_agents`, Town Wall filter/rotation. |
| `v0.4.4` | 병합(`7d65c12`)/적응(`2ffd282`), `adapted`, broadcast MCP exposure deferred | streaming `/tasks`(buffered 기본 계약 유지), `EPHEMERA_MAX_TASK_DEPTH`/`508` depth guard, `GET /watchdog/status`, goose-agent slog. flock broadcast는 daemon API/`ephemera-ctl` CLI로만 채택하고 `anvil_*` MCP tool로 노출하지 않는다. |
| `v0.4.5` | 병합(`8bd84ec`)/적응(`8daf6f3`), `adapted` | snapshot-restore auto-recovery(`recoverRestoredVM`/`reRestoreMachine`). restore state에 `tenant_id`/`egress_policy` persist, 응답 token redaction 유지. divergence는 아래 참조. |

의도적 divergence (`adapted`): anvil은 live·persisted restored VM이 참조하는 source
snapshot의 `DELETE`를 `409`로 막는다. upstream e2e 46c는 이 `DELETE`를 `200`으로
허용하고 restored VM을 orphan으로 두지만, anvil은 VM을 먼저 삭제한 뒤 snapshot을
삭제하도록 요구한다. e2e 46c는 이 divergence에 맞게 조정했고(commit `63df804`,
`e2e_test.sh`에 divergence 주석) 같은 divergence를 [`docs/ADR_INDEX.md`](../ADR_INDEX.md)
Section 4에도 `v0.4.5` `adapted`로 기록한다.

`v0.3.2`/`v0.3.3`의 세부 변경 근거와 anvil 채택 검토 포인트는
[`docs/analysis/08-v0.3.2-v0.3.3-upstream-change-review.md`](../analysis/08-v0.3.2-v0.3.3-upstream-change-review.md)를
historical analysis로 보존한다. 현재 채택 상태는
[`docs/PUBLIC_RELEASE_BOUNDARY.md`](../PUBLIC_RELEASE_BOUNDARY.md)와
[`docs/ADR_INDEX.md`](../ADR_INDEX.md)를 기준으로 한다.

다음 sync 순서는 다음 기준으로 관리한다.

1. `v0.4.0`-`v0.4.5` runtime 안정화 변경은
   [`docs/superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md`](../superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md)의
   계획대로 이 v0.4 sync에서 병합/적응·검증을 마쳤다(위 채택 상태 표 참조).
   남은 항목은 `v0.4.2` default COW 전환과 `v0.4.4` flock broadcast의 MCP tool
   노출이며, 각각 KVM burn-in과 tenant/rate/audit 설계 뒤 결정한다.
2. `v0.5.0`-`v0.7.0`은 product/operator Web UI, MCP Gateway,
   installer/transcript/hardening 계열 변경으로 분류하고, `v0.4.x` sync 이후 별도
   adoption review 문서를 먼저 작성한다.
3. upstream `v0.7.0`의 kernel SHA 검증, `waitForAgent` per-probe timeout,
   `EPHEMERA_HOME`은 baseline sync와 독립적인 hardening backport로 이미 반영됐지만,
   이것을 `v0.7.0` 전체 채택으로 표기하지 않는다.

## 정체성/namespace 규칙

- `EPHEMERA_*`, `goose-*`, `ephemera_*`는 runtime compatibility namespace다.
- anvil product 표면은 `anvil_*` MCP tool, scheduler, tenant/egress, workload
  automation으로 설명한다.
- upstream runtime release note를 anvil product release처럼 제목 변경하지 않는다.
- anvil release note에는 "upstream runtime baseline"과 "anvil product changes"를
  분리해 적는다.

## Tag와 release 규칙

- ephemera runtime tag는 `v*` 형식을 유지한다.
- anvil product release tag는 `anvil-v*` 형식을 사용한다.
- anvil release note에는 기준 ephemera upstream version과 anvil 변경분을 분리해 적는다.
- 기존 `v*` tag를 anvil 의미로 재사용하지 않는다.

## 검증

sync PR은 최소 다음 검증을 통과해야 한다.

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
git diff --check
```

runtime 계약 변경이 VM lifecycle, snapshot, network, guest image에 닿으면 KVM 통합
테스트도 별도로 수행한다.

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
```

upstream runtime 변경을 anvil MCP surface로 새로 노출하거나 tool contract가 바뀌면
daemon-backed MCP smoke도 함께 수행한다.

```bash
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
```
