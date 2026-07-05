# ADR 적용 상태 인덱스

> **대상:** anvil downstream repository
> **현행화 기준:** 2026-07-02
> **목적:** anvil에서 장기 유지할 설계 결정과 upstream ephemera 변경 채택 상태를 추적한다.

---

## 1. 읽는 규칙

`docs/adr/`의 ADR은 설계 결정 원문이다. 이 인덱스는 각 ADR이 현재 anvil에
어떻게 적용되는지를 요약한다.

문서가 충돌하면 다음 순서를 따른다.

1. `CONTEXT.md`
2. `docs/PUBLIC_RELEASE_BOUNDARY.md`
3. `docs/ADR_INDEX.md`
4. 개별 ADR 원문
5. `README.md`
6. `RELEASE_NOTES.md`

개별 ADR 원문과 공개 경계가 충돌하면 공개 경계를 우선한다. ADR을 바꾸지 않고
역사 기록으로 보존해야 할 때는 이 인덱스에서 상태를 `superseded` 또는
`historical`로 바꾼다.

---

## 2. 상태 값

| 상태 | 의미 |
|---|---|
| `accepted` | 현재 anvil에 적용되는 결정 |
| `adapted` | 원칙은 유지하지만 anvil 정책에 맞게 수정 적용 |
| `superseded` | 더 최신 ADR 또는 공개 경계 문서가 대체 |
| `historical` | 과거 판단 근거로만 보존 |
| `rejected` | 검토했지만 현재 anvil 경계와 맞지 않아 채택하지 않음 |

---

## 3. 현재 적용 상태

| ADR | 상태 | 적용 요약 |
|---|---|---|
| [ADR-0001](adr/0001-anvil-public-boundary-and-upstream-adoption.md) | accepted | `PUBLIC_RELEASE_BOUNDARY.md`와 ADR_INDEX를 도입하고, upstream ephemera 변경을 `adopted/adapted/excluded/deferred/historical`로 분류한다. |

---

## 4. upstream runtime baseline 채택 상태

다음 upstream tag는 anvil `main`에 병합되어 runtime baseline으로 사용된다. 분류는
`PUBLIC_RELEASE_BOUNDARY.md`의 공개 표면과 보안 경계를 기준으로 한다.

| upstream tag | 상태 | 적용 전 검토 요약 |
|---|---|---|
| `v0.3.2` | adapted | live VM cold-restart와 `vms/<vm_id>/state.json`은 채택한다. `agent_token` persistence는 host-local recovery state로 취급하고 MCP output, audit, metrics, replay fixture에는 노출하지 않는다. |
| `v0.3.3` | adapted | watchdog dead persistence, per-agent restart, in-VM CP token auto-injection은 채택한다. `/root/.ephemera-cp-token`은 secret file로 취급하고 standalone VM에는 주입하지 않는 경계를 유지한다. |
| `v0.3.4` | adapted | `EPHEMERA_API_TOKENS_FILE`, SIGHUP CP-token vsock fan-out, watchdog tunables/auto-heal, Firecracker SIGHUP forwarding hot-fix는 채택한다. true hot rotation은 token file과 v0.3.4+ guest agent가 있을 때만 보장한다. |
| `v0.3.5` | adapted | `/metrics`, `/vms/{vm_id}/stats`, `log/slog`, observability demo는 채택한다. `ephemera_*` metric namespace와 `EPHEMERA_*` env는 runtime compatibility namespace로 유지한다. |
| `v0.3.6` | adapted | autonomous webdev demo, in-VM `gtcall`, multi-line-safe `gtwall`, Goose JSON output parsing은 채택한다. `gtcall`은 peer `agent_token`을 VM 내부에 노출하지 않고 control-plane proxy token injection 경계를 유지한다. |
| `v0.4.0` | adapted | storage/recovery core(memory auto-snapshot, diff/COW rootfs, spawn-path cold-restart)는 채택한다. `EPHEMERA_AUTOSNAPSHOT=true` auto-snapshot은 opt-in·disk-expensive로 두고 public support로 승격하지 않는다. |
| `v0.4.1` | adapted | client identity, daemon access audit(`GET /audit`), per-token TTL/rotation은 채택한다. `ephemera-ctl`은 runtime operator CLI로 유지하고 IronClaw MCP tool을 대체하거나 anvil MCP public surface로 승격하지 않는다. |
| `v0.4.2` | adapted, default cow deferred | COW probe/fallback과 COW+Diff snapshot은 채택한다. `EPHEMERA_DISK_MODE=cow`는 anvil에서 명시적 opt-in이며 default 전환은 KVM burn-in 뒤 결정한다. |
| `v0.4.3` | adapted | dynamic flock membership, pause/resume, per-flock `max_agents`, Town Wall filter/rotation single-host lifecycle은 채택한다. routed members-only cross-host flock에는 그대로 적용하지 않는다. |
| `v0.4.4` | adapted, broadcast MCP exposure deferred | streaming `/tasks`(buffered 기본 계약 유지), nested depth guard(`EPHEMERA_MAX_TASK_DEPTH`, `508`), `GET /watchdog/status`, goose-agent slog는 채택한다. flock broadcast는 daemon API/CLI로만 두고 `anvil_*` MCP tool 노출은 tenant/rate/audit 설계 전까지 deferred다(guard로 고정). |
| `v0.4.5` | adapted | snapshot-restore auto-recovery(`recoverRestoredVM`/`reRestoreMachine`)를 채택하고 restore state에 `tenant_id`/`egress_policy`를 persist하되 응답 token redaction은 유지한다. anvil은 live·persisted restored VM이 참조하는 source snapshot의 `DELETE`를 `409`로 막아 upstream e2e 46c의 `200` orphan 동작과 의도적으로 divergent하다(먼저 VM을 삭제한 뒤 snapshot 삭제). |
| `v0.5.0` | adapted | operator Web UI(Svelte SPA, EN/KO, `cmd/goose-daemon/uidist/` embedded), `/config/profiles`, multi-turn goose session, graceful VM delete를 채택한다. Web UI는 runtime/operator surface이고 IronClaw MCP surface가 아니다 — `/ui/`(정적 bundle + login)만 auth 밖, 모든 data API는 bearer 뒤(guard `config_api_anvil_test.go`). `/config/profiles`는 `goose-secrets.yaml`을 절대 read/write하지 않는다(sentinel test). `cmd/anvil-mcp`는 그대로이고 `VMInfo`는 additive `provider`/`model` 필드만 추가한다. |
| `v0.5.1`-`v0.5.5` | adapted | `/config/providers`(key 존재 여부만), `/config/clients`(이름+만료만)를 sentinel test로 secret 비노출 확인. `system.md`-only prompt 편집(`64 KiB` cap), profile delete in-use → `409`, default profile 예약, traversal 거부. sizing preset + per-VM `VcpuCount`/`MemSizeMib`(snapshot metadata에 기록, legacy → 2/2048 fallback). Town Wall `SystemAuthor` author migration. restore agent-wait 30s→60s. |
| `v0.6.0` | adapted | runtime MCP Gateway(`internal/mcpgateway`, daemon `/config/mcp`·`/config/mcp/servers`·`/config/mcp/builtins` handler, `configs/mcp/*.example`, Web UI MCP console). **runtime/operator surface이고 IronClaw MCP surface가 아니다 — `cmd/anvil-mcp`를 대체하지 않는다.** 경계는 구조적으로 강제: caller profile은 source IP↔VM registry로 server-side 판정(unknown → `403`), backend credential은 host-side(`configs/mcp/secrets.yaml`, gitignored)에만 있고 VM에는 gateway URL만 주입(`VMPrepareOptions`에 credential 필드 없음), `audit/mcp.jsonl`은 metadata-only(고정 key set, `Err`도 omit), profile policy는 `servers.yaml`을 좁히기만 하고 넓힐 수 없다. anvil boundary guard 4종: IronClaw schema/adapter가 gateway tool 제외, audit metadata-only sentinel, `/config/mcp*` bearer 없으면 `401`, VM은 URL(credential 아님)만 받고 policy는 widen 불가. |
| `v0.6.1`, `v0.6.2`, `v0.6.4` | adapted | (upstream에 `v0.6.3` 없음) `EPHEMERA_NET_ANTISPOOF` 기본 on(ebtables best-effort); per-(VM,server) token-bucket rate limit(`EPHEMERA_MCP_RATE` 기본 `0`=unlimited, `EPHEMERA_MCP_BURST`); resources/prompts가 tools와 policy·rate bucket 공유(anvil guard); audit `kind` field; `GET /config/mcp/servers`는 transport/command + `has_credential`만 노출(leak guard + sentinel); stdio backend child env를 `[PATH,HOME,LANG]`+`spec.Env`로 재구성(`EPHEMERA_*` canary test), credential은 `credential_env`로만(argv 아님), root면 `nobody`로 실행 + `/var/lib/ephemera/mcp-stdio` scratch, shutdown이 stdio process group reap(pgid recycling-safe). |
| `v0.7.0` | adapted | end-user installer(`install.sh`/`uninstall.sh`/`INSTALL.md`/`ephemera.service.in`), release workflow(`scripts/build_release.sh`), conversation transcript restore를 채택한다. installer는 runtime/operator surface이고 systemd service는 canonical `ephemera` 이름을 유지한다(rule-permitted, anvil alias wrapper 없음). transcript restore는 daemon proxy `GET /vms/{id}/sessions/{name}/transcript`(bearer)로 노출하며, agent export는 read-only `goose session export`(model call 없음)이고 응답 schema `{turns:[{role,text}]}`는 auth-free여서 Web UI가 daemon token 없이 렌더한다. 4개 transcript-safety guard: bearer 없으면 `401`, payload는 provider key/CP token/`agent_token` sentinel-free, cache-hit는 agent spawn 없이 serve, export argv는 `session export -n {name} --format json`이며 run-token 거부. release build integrity: `build_release.sh`가 kernel/firecracker를 `main.go` pin과 `sha256sum -c`로 검증. 사전 backport 3종(kernel SHA atomic temp+rename, `resolveWorkDir`/`EPHEMERA_HOME`, `waitForAgent` per-probe)이 v0.7.0 reconcile에서 single definition으로 남았고(anvil이 stricter, net Go diff doc-comment-only), 기존 anvil adaptation은 rollback 없음. |

runtime MCP Gateway 경계(요약): `EPHEMERA_MCP_*`(gateway, runtime/operator)와
`ANVIL_MCP_*`(`cmd/anvil-mcp` IronClaw adapter 설정)는 서로 다른 namespace·surface다.
gateway는 adapter를 대체하지 않으며, IronClaw tool 목록에 gateway tool을 추가하지
않는다(guard `TestToolRegistrationsExcludeGatewayTools`,
`TestCurrentIronClawSchemasExcludeGatewayNamespacedTools`).

`v0.4.0`-`v0.7.0`은 anvil main runtime baseline으로 병합·적응되었고 full KVM gate로
검증됐다(`v0.4.x`는 Task 8, `v0.5.x`는 Task 4, `v0.6.x`는 Task 6, `v0.7.0`은 Task 7
문서 반영 기준). 이로써 upstream parity scope(`v0.4.0`-`v0.7.0`) 코드 편입이 완료됐다.
상세 병합 근거는
[`docs/analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md`](analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md),
Phase 2 handoff([`docs/operations/2026-07-05-ephemera-v0.5-operator-sync-handoff.md`](operations/2026-07-05-ephemera-v0.5-operator-sync-handoff.md)),
Phase 3 handoff([`docs/operations/2026-07-05-ephemera-v0.6-mcp-gateway-sync-handoff.md`](operations/2026-07-05-ephemera-v0.6-mcp-gateway-sync-handoff.md)),
Phase 4 handoff([`docs/operations/2026-07-06-ephemera-v0.7-parity-sync-handoff.md`](operations/2026-07-06-ephemera-v0.7-parity-sync-handoff.md))에
보존한다.

sizing 결정: `v0.5.3`부터 anvil은 upstream default VM sizing `1` vCPU / `1024` MiB를
채택한다(이전 2/2048, KVM 근거로 승인, full e2e 3× `316✓`). snapshot metadata가 per-VM
sizing을 기록하고 legacy snapshot은 2/2048로 fallback한다. flock member spawn이
per-profile `EPHEMERA_VCPU_COUNT`/`EPHEMERA_MEM_SIZE_MIB` override를 무시하고
`LookupProfile` default로만 sizing하는 upstream-inherited gap은 follow-up이다.

keep-alive divergence(`adapted`): `v0.5.x` `gracefulAgentStop`이 v0.2.0부터 잠재하던
upstream shared pooled agent proxy client 결함을 드러냈다(guest IP 재활용 시 stale
keep-alive connection 재사용 → restored VM `/tasks` hang/`502`). `64ec57c`가 request마다
fresh dial(`DisableKeepAlives`)하고 connection-reuse guard test를 추가한다. upstream
connection pooling과의 의도적 divergence이며 upstream 기여 후보다. 같은 divergence를
[`docs/operations/upstream-sync-policy.md`](operations/upstream-sync-policy.md)에도
기록한다.

---

## 5. 다음 upstream sync 후보 예비 분류

`v0.4.0`-`v0.7.0`은 모두 Section 4의 baseline 채택 상태로 이동했다. 2026-07-02 기준
upstream `main`과 최신 upstream tag는 `v0.7.0`이며, anvil은 이 관찰 범위 전체를 병합·
적응했다 — upstream parity scope 코드 편입이 완료돼 현재 pending sync 후보는 없다.

`v0.7.0` 이후 upstream 태그가 새로 관찰되면 이 인덱스에 pre-review 분류로 다시 추가하고,
sync 전 backlog triage와 별도 analysis 문서/sync branch 검증 뒤 채택 상태를 확정한다.
현재 남은 작업은 tag 채택이 아니라 release-gate 항목(valid provider key `semantic` run,
audit-writer sentinel, stdio stderr scrub, `credential_env` reserved names, production-mux
auth assert)과 deferred 항목(`v0.4.4` broadcast MCP 노출, `v0.4.2` default COW,
auto-snapshot public support, flock per-profile sizing, runtime MCP Gateway의 IronClaw
표면 승격 금지)이다.

---

## 6. 새 ADR 작성 기준

다음 조건 중 하나에 해당하면 새 ADR을 추가한다.

- anvil 공개 표면 또는 제외 표면이 바뀐다.
- upstream ephemera 변경을 anvil 정책에 맞게 수정 채택하거나 제외한다.
- token, auth, MCP output, guest access 같은 보안 경계가 바뀐다.
- VM lifecycle, snapshot/restore, cleanup, watchdog, persistence 의미가 바뀐다.
- IronClaw와 anvil 사이의 tool 계약이 바뀐다.
- 기존 ADR을 대체하거나 폐기해야 한다.

새 ADR에는 반드시 상태, 날짜, 결정, 결과, 검증 기준을 적는다.
