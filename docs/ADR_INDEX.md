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

---

## 5. 다음 upstream sync 후보 예비 분류

다음 upstream tag는 아직 anvil runtime baseline으로 병합되지 않았다. 2026-07-02
기준 upstream `main`과 최신 upstream tag는 `v0.7.0`이지만, anvil `main`의 runtime
baseline은 계속 `v0.3.6`이다. `v0.4.0`-`v0.4.5`의 예비 분류와 검토 근거는
[`docs/analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md`](analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md)에
기록한다.

| upstream tag | 예비 상태 | 적용 전 검토 요약 |
|---|---|---|
| `v0.4.0` | adapted | storage/recovery core는 채택 후보지만 rootfs diff snapshot, COW recovery, auto-snapshot을 anvil snapshot replication/GC와 맞춰야 한다. |
| `v0.4.1` | adapted | client identity, daemon access audit, token TTL은 채택 후보다. `ephemera-ctl`은 runtime operator CLI로 유지하고 anvil MCP public surface로 승격하지 않는다. |
| `v0.4.2` | adapted/deferred | COW probe/fallback과 COW+Diff support는 채택 후보지만 default `EPHEMERA_DISK_MODE=cow` 전환은 KVM burn-in 뒤 결정한다. |
| `v0.4.3` | adapted | single-host flock lifecycle은 채택 후보지만 routed members-only cross-host flock에는 그대로 적용하지 않는다. |
| `v0.4.4` | adapted/deferred | streaming task, watchdog status, depth guard는 채택 후보다. flock broadcast의 MCP public exposure는 tenant/rate/audit 설계 전까지 deferred다. |
| `v0.4.5` | adapted | snapshot-restored VM auto-recovery는 채택 후보지만 live restored VM의 source snapshot dependency를 GC가 보호해야 한다. |

`v0.5.0`-`v0.7.0`은 아직 상세 adoption review가 끝나지 않았다. 다음 분류는 sync
전 backlog triage 기준이며, 실제 채택 상태는 별도 analysis 문서와 sync branch 검증
뒤 확정한다.

| upstream tag 범위 | 현재 상태 | 적용 전 검토 요약 |
|---|---|---|
| `v0.5.0`-`v0.5.5` | pre-review | product/operator Web UI 계열로 보인다. anvil 공개 표면으로 승격하기 전 IronClaw 전용 경계, 인증, 운영 노출 범위를 별도로 판단해야 한다. |
| `v0.6.0`-`v0.6.4` | pre-review | MCP Gateway 계열로 보인다. anvil MCP adapter, IronClaw 통합 경계, multi-host runtime 계획과 겹칠 수 있어 보안/권한 설계 review가 필요하다. |
| `v0.7.0` | pre-review/backported-hardening | installer, transcript, hardening 계열로 보인다. kernel SHA 검증, `waitForAgent` per-probe timeout, `EPHEMERA_HOME`은 baseline sync와 독립 backport로 반영됐지만, tag 전체는 아직 채택하지 않았다. |

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
