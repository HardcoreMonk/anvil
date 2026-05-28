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
git checkout -b sync/ephemera-v0.3.x origin/main
git merge --no-ff upstream/main
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

- branch: `sync/ephemera-v0.3.1`
- merge commit: `merge: sync ephemera v0.3.1`
- follow-up adaptation commit: `fix(runtime): adapt anvil to ephemera v0.3.1`
- docs commit: `docs: document ephemera v0.3.1 baseline`

sync PR은 upstream merge commit과 anvil adaptation commit을 분리한다. 이렇게 해야
upstream 변경 자체와 anvil에서 해결한 conflict/적응 작업을 review에서 구분할 수 있다.

## 현재 runtime baseline

2026-05-26 기준 `sync/ephemera-v0.3.6`은 upstream ephemera `v0.3.6` tag까지
병합한 runtime baseline을 사용한다. `v0.3.2`-`v0.3.5` 병합 commit은
`1ebe201 Merge upstream/main`이고, `v0.3.6`은 `v0.3.6` tag commit을 merge한다.

| tag | peeled commit | 요약 | anvil 현재 상태 |
|---|---|---|---|
| `v0.3.2` | `f5e0de694a5584acb1a20436a0b3ae912d862792` | live VM cold-restart, `vms/<vm_id>/state.json`, orphan cleanup, network re-reservation | 병합됨, `adapted` |
| `v0.3.3` | `3c24e6086e8b16380c94cf16d4d19dec960c9675` | watchdog dead-status persistence, per-agent restart, in-VM CP token auto-injection, real-LLM e2e | 병합됨, `adapted` |
| `v0.3.4` | `5580482c7911d184bcd950347f050642535c431a` | `EPHEMERA_API_TOKENS_FILE`, SIGHUP CP-token fan-out, watchdog tunables/auto-heal, Firecracker SIGHUP hot-fix | 병합됨, `adapted` |
| `v0.3.5` | `7a6d42dd56361719bc4fb592e75e0c8d8d9cf211` | `/metrics`, `/vms/{vm_id}/stats`, `log/slog`, observability demo | 병합됨, `adapted` |
| `v0.3.6` | `4bd5e8c3d94fbfb862de116caa7417f7b640b325` | autonomous webdev demo, in-VM `gtcall`, multi-line-safe `gtwall`, Goose JSON output parsing | sync branch에서 병합, `adapted` |

`v0.3.2`/`v0.3.3`의 세부 변경 근거와 anvil 채택 검토 포인트는
[`docs/analysis/08-v0.3.2-v0.3.3-upstream-change-review.md`](../analysis/08-v0.3.2-v0.3.3-upstream-change-review.md)를
historical analysis로 보존한다. 현재 채택 상태는
[`docs/PUBLIC_RELEASE_BOUNDARY.md`](../PUBLIC_RELEASE_BOUNDARY.md)와
[`docs/ADR_INDEX.md`](../ADR_INDEX.md)를 기준으로 한다.

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
