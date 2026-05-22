# ADR 적용 상태 인덱스

> **대상:** anvil downstream repository
> **현행화 기준:** 2026-05-22
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

---

## 5. 새 ADR 작성 기준

다음 조건 중 하나에 해당하면 새 ADR을 추가한다.

- anvil 공개 표면 또는 제외 표면이 바뀐다.
- upstream ephemera 변경을 anvil 정책에 맞게 수정 채택하거나 제외한다.
- token, auth, MCP output, guest access 같은 보안 경계가 바뀐다.
- VM lifecycle, snapshot/restore, cleanup, watchdog, persistence 의미가 바뀐다.
- IronClaw와 anvil 사이의 tool 계약이 바뀐다.
- 기존 ADR을 대체하거나 폐기해야 한다.

새 ADR에는 반드시 상태, 날짜, 결정, 결과, 검증 기준을 적는다.
