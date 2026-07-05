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

anvil main runtime baseline은 upstream ephemera `v0.7.0` tag까지 병합·적응한 runtime을
포함한다. `v0.3.2`-`v0.3.6`은 이전 release(`anvil-v0.3.x`)에서 채택한 baseline이고,
`v0.4.0`-`v0.4.5`는 v0.4 sync로, `v0.5.0`-`v0.5.5`는 v0.5 operator sync로,
`v0.6.0`-`v0.6.4`는 v0.6 MCP gateway sync로, `v0.7.0`은 v0.7 parity sync로 병합·적응해
full KVM gate로 검증한 baseline이다. 이로써 upstream parity scope(`v0.4.0`-`v0.7.0`)의
코드 편입이 완료됐다.
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

`v0.5.0`-`v0.5.5` operator sync 채택 상태(이 v0.5 sync의 merge/adapt commit 기준):

| tag | 상태 | anvil adaptation 요지 |
|---|---|---|
| `v0.5.0` | 병합(`884e832`)/적응(`726a59a`), `adapted` | operator Web UI(`cmd/goose-daemon/uidist/` embedded) + `/config/profiles` + multi-turn session + graceful delete. runtime/operator surface로만 채택(IronClaw MCP 아님). `/ui/`(정적 bundle + login)만 auth 밖, data API는 bearer 뒤. `/config/profiles`는 `goose-secrets.yaml` 비노출(sentinel). `cmd/anvil-mcp` 불변, `VMInfo` provider/model additive. |
| `v0.5.1`-`v0.5.5` | 병합(`bab1e9d`..`7f207a0`)/적응(`b0b4c48`, `225a845`), `adapted` | `/config/providers`(key 존재 여부)·`/config/clients`(이름+만료) secret 비노출, `system.md` 편집(`64 KiB`), profile delete in-use `409`/default 예약/traversal 거부, sizing preset + per-VM `VcpuCount`/`MemSizeMib`(snapshot metadata 기록, legacy 2/2048 fallback), `SystemAuthor` author migration, restore agent-wait 30s→60s. |

의도적 divergence (`adapted`, keep-alive): `v0.5.x` `gracefulAgentStop`이 v0.2.0부터
잠재하던 upstream shared pooled agent proxy client 결함을 드러냈다(guest IP 재활용 시
stale keep-alive connection 재사용 → restored VM `/tasks` hang/`502`). `64ec57c`가
request마다 fresh dial(`DisableKeepAlives`)하고 connection-reuse guard test를 추가한다.
upstream connection pooling과의 divergence이며 upstream 기여 후보다. 같은 divergence를
[`docs/ADR_INDEX.md`](../ADR_INDEX.md) Section 4에도 기록한다.

sizing 결정: `v0.5.3`부터 anvil은 upstream default VM sizing `1` vCPU / `1024` MiB를
채택한다(이전 2/2048, KVM 근거로 승인, full e2e 3× `316✓`). flock member spawn이
per-profile `EPHEMERA_VCPU_COUNT`/`EPHEMERA_MEM_SIZE_MIB` override를 무시하고
`LookupProfile` default로만 sizing하는 upstream-inherited gap은 follow-up이다.

`v0.6.0`-`v0.6.4` MCP gateway sync 채택 상태(이 v0.6 sync의 merge/adapt commit 기준;
upstream에 `v0.6.3` 없음):

| tag | 상태 | anvil adaptation 요지 |
|---|---|---|
| `v0.6.0` | 병합(`6e42d2b`)/적응(`cf2e87a`), `adapted` | runtime MCP Gateway(`internal/mcpgateway`, daemon `/config/mcp*` handler, `configs/mcp/*.example`, Web UI MCP console). **runtime/operator surface, IronClaw MCP surface 아님, `cmd/anvil-mcp` 대체 아님.** 경계 구조적 강제: source IP↔VM registry로 caller profile 판정(unknown `403`), backend credential host-side only(`configs/mcp/secrets.yaml` gitignored; VM엔 gateway URL만), `audit/mcp.jsonl` metadata-only(`Err`도 omit), profile policy는 widen 불가. anvil boundary guard 4종 추가. |
| `v0.6.1`/`v0.6.2`/`v0.6.4` | 병합(`9ba10f0`/`94fafdf`/`1bdd491`)/적응(`74c89c4`/`688e6ad`/`04e2a12`), `adapted` | `EPHEMERA_NET_ANTISPOOF` 기본 on(ebtables best-effort); per-(VM,server) token-bucket rate limit(`EPHEMERA_MCP_RATE` 기본 `0`, `EPHEMERA_MCP_BURST`); resources/prompts가 tools와 policy·rate bucket 공유(anvil guard); audit `kind` field; `GET /config/mcp/servers`는 transport/command + `has_credential`만(leak guard + sentinel); stdio backend child env `[PATH,HOME,LANG]`+`spec.Env` 재구성(`EPHEMERA_*` canary), credential은 `credential_env`로만(argv 아님), root면 `nobody` + `/var/lib/ephemera/mcp-stdio` scratch, shutdown이 process group reap. |

runtime MCP Gateway 경계: `EPHEMERA_MCP_*`(gateway, runtime/operator surface)와
`ANVIL_MCP_*`(`cmd/anvil-mcp` IronClaw adapter 설정)는 별개 namespace다. gateway는
adapter를 대체하지 않고 IronClaw tool 목록에 gateway tool을 추가하지 않는다(guard
`TestToolRegistrationsExcludeGatewayTools`). `EPHEMERA_MCP_BIND_IP`는 기본 안전한
bridge IP bind를 override할 수 있고 source-IP `403`이 defense-in-depth로 남으며, stdio
backend 운영 시 `nobody`/scratch default를 확인한다.

`v0.7.0` parity sync 채택 상태(이 v0.7 sync의 merge/adapt commit 기준):

| tag | 상태 | anvil adaptation 요지 |
|---|---|---|
| `v0.7.0` | 병합(`b2df010`)/적응(`7b3f009`), `adapted` | end-user installer(`install.sh`/`uninstall.sh`/`INSTALL.md`/`ephemera.service.in`), release workflow(`scripts/build_release.sh`), conversation transcript restore, upstream hardening reconcile. installer는 runtime/operator surface이고 systemd는 canonical `ephemera`(rule-permitted, alias wrapper 없음). transcript는 daemon proxy `GET /vms/{id}/sessions/{name}/transcript`(bearer), agent export read-only(model call 없음), 응답 `{turns:[{role,text}]}` auth-free. 4 transcript-safety guard(bearer 없으면 `401`, payload sentinel-free, cache-hit no-spawn, export argv `session export -n {name} --format json` run-token 거부). release build integrity: `build_release.sh`가 kernel/firecracker를 `main.go` pin과 `sha256sum -c`로 검증(FULL-tarball supply-chain gap 차단). |

backport reconciliation: 사전 독립 backport 3종(kernel SHA atomic temp+rename 무조건
검증, `resolveWorkDir`/`EPHEMERA_HOME`, `waitForAgent` per-probe timeout deadline cap)이
v0.7.0 reconcile에서 upstream 버전을 이기고 single definition으로 남았다(anvil이 stricter,
net Go diff는 doc-comment-only). 기존 anvil adaptation(agent-stamp mount skip,
restore-over-`meta.DiskPath`, proxy `DisableKeepAlives`)은 하나도 rollback되지 않았다.

`v0.3.2`/`v0.3.3`의 세부 변경 근거와 anvil 채택 검토 포인트는
[`docs/analysis/08-v0.3.2-v0.3.3-upstream-change-review.md`](../analysis/08-v0.3.2-v0.3.3-upstream-change-review.md)를
historical analysis로 보존한다. 현재 채택 상태는
[`docs/PUBLIC_RELEASE_BOUNDARY.md`](../PUBLIC_RELEASE_BOUNDARY.md)와
[`docs/ADR_INDEX.md`](../ADR_INDEX.md)를 기준으로 한다.

다음 sync 순서는 다음 기준으로 관리한다.

1. `v0.4.0`-`v0.4.5` runtime 안정화 변경은
   [`docs/superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md`](../superpowers/plans/2026-06-10-ephemera-v0.4-runtime-sync.md)
   계획대로 v0.4 sync에서, `v0.5.0`-`v0.5.5` operator support 변경은 v0.5 operator
   sync에서, `v0.6.0`-`v0.6.4` MCP gateway 변경은 v0.6 MCP gateway sync에서, `v0.7.0`
   installer/transcript/hardening 변경은 v0.7 parity sync에서 각각 병합/적응·검증을
   마쳤다(위 채택 상태 표 참조). 이로써 upstream parity scope 코드 편입이 완료됐다.
   남은 항목은 tag 채택이 아니라 `v0.4.2` default COW 전환, `v0.4.4` flock broadcast의
   MCP tool 노출, flock member spawn의 per-profile sizing 존중, release-gate 항목이며,
   각각 KVM burn-in과 tenant/rate/audit·sizing 경로 설계 뒤 결정한다.
2. `v0.7.0` 이후 upstream 태그는 2026-07-02 기준 아직 관찰되지 않았다. 새 태그가
   관찰되면 별도 sync branch에서 병합/적응하고 adoption review 문서를 먼저 작성한다.
3. `v0.7.0`의 kernel SHA 검증, `waitForAgent` per-probe timeout, `EPHEMERA_HOME`은 sync
   전 독립 hardening backport로 먼저 반영돼 있었고, v0.7.0 병합 시 reconcile돼 anvil
   backport가 single definition으로 남았다(위 backport reconciliation 참조).

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
