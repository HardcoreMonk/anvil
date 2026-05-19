# ADR 적용 상태 인덱스

> **대상:** anvil downstream repository
> **현행화 기준:** 2026-05-19
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

## 4. upstream 채택 대기 상태

다음 upstream tag는 확인됐지만 아직 anvil `main`에 병합되지 않았다. 새 sync branch에서
병합할 때 공개 표면과 보안 경계를 검토해 `adopted`, `adapted`, `excluded`,
`deferred` 중 하나로 다시 확정한다.

| upstream tag | 상태 | 적용 전 검토 요약 |
|---|---|---|
| `v0.3.2` | deferred | live VM cold-restart가 `agent_token`을 `vms/<vm_id>/state.json`에 보존하고 daemon restart semantics를 바꾼다. file mode, audit/replay/MCP output redaction, disaster recovery 문서 갱신이 필요하다. |
| `v0.3.3` | deferred | per-agent restart와 in-VM CP token auto-injection이 token/auth 경계에 닿는다. MCP tool로 노출할지, guest disk에 들어가는 CP token의 rotation/blast radius를 어떻게 문서화할지 결정해야 한다. |

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
