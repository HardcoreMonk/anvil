# Svelte 5 event-idiom modernization — 설계

**날짜:** 2026-07-19
**상태:** 승인됨 (사용자 승인 2026-07-19)
**관련:** Svelte 5 core runes 마이그레이션(#87, spec `2026-07-19-svelte5-runes-migration-design.md`)의 후속 — 거기서 스코프 아웃한 event-idiom. operator Web UI `web/`, `svelte-check` 게이트 이미 존재.

## 목표

deprecated Svelte 4 event 관용구를 Svelte 5 관용구로 전환해 `on:`/`createEventDispatcher` **deprecation 경고를 0**으로 만든다 — `on:event`→event 속성, event modifier 인라인, `createEventDispatcher`→callback props. **behavior-preserving.**

## 세 변환 클래스

### 1. DOM 이벤트 `on:xxx` → `onxxx` (기계적)
`on:click`→`onclick`, `on:change`→`onchange`, `on:input`→`oninput`, `on:keydown`→`onkeydown`, `on:submit`→`onsubmit`. **핸들러 본문 불변**(둘 다 DOM event를 인자로 받음). event 속성은 legacy·runes 모드 양쪽에서 동작. 대상: `on:`를 쓰는 24개 `.svelte` 파일.

### 2. event modifier 인라인 (17 사이트 — Svelte 5는 modifier 제거)
- **`on:click|self={h}` (16×, 모달 backdrop 패턴)** → `onclick={(e) => e.target === e.currentTarget && h}` (또는 `h`가 함수면 `&& h()`). `|self` 의미(대상이 자기 자신일 때만) 정확 보존.
- **`on:submit|preventDefault={submit}` (1×, Login)** → `onsubmit={(e) => { e.preventDefault(); submit() }}`.
- **결정: 인라인**(공유 헬퍼/action 추출 안 함 — 추상화 미도입, behavior-preserving 최소 변경). 16× backdrop은 동일 패턴의 반복 인라인.

### 3. custom event → callback props (부모/자식 결합, 11 모달 + 6 부모)
- **자식**: `const dispatch = createEventDispatcher(); dispatch('close')` → `let { onclose, oncreated, ... } = $props(); onclose?.()`. 이벤트→콜백 이름: `close`→`onclose`, `created`→`oncreated`, `changed`→`onchanged`, `added`→`onadded`, `restored`→`onrestored`, `spawned`→`onspawned`.
- **부모**: `<Modal on:close={h} on:created={f} />` → `<Modal onclose={h} oncreated={f} />`.
- **detail-운반 케이스 1건**: `SnapshotModal`이 `dispatch('created', { stopAfter })` → `oncreated?.({ stopAfter })`; 부모 `VMDetail`의 `onSnapshotCreated` 핸들러는 **인자를 직접** 읽는다(Svelte4 `event.detail` 아님). 나머지 dispatch는 전부 무-detail → 콜백 무인자.

#### 모달 ↔ 부모 매핑 (결합 단위)
| 모달 | 부모 | 콜백 props |
|---|---|---|
| CreateFlockModal | Flocks | onclose, oncreated |
| BuiltinsModal | Settings | onclose |
| ChangeRoleModal | FlockDetail | onclose, onchanged |
| AddAgentModal | FlockDetail | onclose, onadded |
| SendTaskModal | FlockDetail | onclose |
| SnapshotModal | VMDetail | onclose, **oncreated({stopAfter})** |
| RestoreModal | Snapshots | onclose, onrestored |
| BroadcastModal | FlockDetail | onclose |
| SystemPromptModal | Settings | onclose |
| SpawnModal | VMList | onclose, onspawned |
| ProfileModal | Settings | onclose, oncreated |

**SpawnModal 뉘앙스**: 11개 dispatcher-모달 중 10개는 이미 runes(#87의 `$props` 보유) → callback prop을 기존 `$props()` 구조분해에 추가만 하면 됨. **`SpawnModal`만 아직 legacy** → callback prop `$props()` 도입으로 runes 모드 진입 → 그 컴포넌트의 지역 반응 `let`도 `$state` 필요(게이트가 `non_reactive_update`로 지목). 즉 SpawnModal 태스크는 event 전환 + runes 전환을 함께 완료한다.

## 도구

**Hand-migrate** — #87 core-runes와 일관(clean·controlled·in-scope), 변환이 균일(backdrop 1패턴, dispatcher→callback 1패턴). **`sv migrate` 미사용**(전 파일 over-reach + slots→snippets, 어차피 동일 리뷰 필요).

## 검증

- **`svelte-check`**: 이 작업은 **event-idiom deprecation 경고**(`event_directive_deprecated`(on:) + `createEventDispatcher` deprecation, 현재 ~104건)를 **0으로 제거**한다. 단 **`state_referenced_locally` 경고(~10건, #87의 const/init-reads-prop 유래, benign)는 잔존** — 이는 이 작업의 비목표이므로 **"0 total warnings"가 아니다**. 따라서 게이트는 blanket `--fail-on-warnings`가 **아니라**, event-idiom deprecation 코드를 error로 승격한다: 기존 `--compiler-warnings "non_reactive_update:error"`에 **`event_directive_deprecated:error`(+ createEventDispatcher deprecation 코드 — Task 1이 정확 코드명 확정)**를 추가 → on:/dispatch 회귀 시 exit1로 고정, benign `state_referenced_locally`는 허용. 태스크별: `npm run check` clean(승격된 코드 0)이며 대상 파일의 event-idiom deprecation 경고가 **엄격히 감소**.
- `npx vite build` OK; **`/ui` 수동 스모크**: 모달별 open/close(backdrop-click + Escape + ✕/Cancel), 도메인 이벤트(spawn→list refresh, snapshot create, flock create, add-agent, change-role, restore, broadcast, send-task), Login submit, `SnapshotModal.created({stopAfter})` 경로.
- 마지막에 `uidist` 재빌드+커밋(#87과 동일, `//go:embed`).

## 증분

컴포넌트별. **클래스 3(callback props)은 각 모달과 그 부모를 함께 전환**(자식 콜백 prop + 부모 호출부 동시) — 모달+부모를 한 태스크로 묶는다. 클래스 1·2(DOM on:→on + modifier)는 파일별 가능. 혼합 모드 안전(전 과정 앱 동작).

## 비목표

stores→`$state` 모듈, snippet/slot, 동작 변경, #87에서 끝낸 non-event runes 작업. `$store` 자동구독 유지.

## 리스크

low-moderate. DOM rename·backdrop 인라인은 기계적·균일. 결합된 callback-props가 주 care point(부모/자식 쌍 일치 + `{stopAfter}` detail 1건) — svelte-check가 시그니처 불일치 잡고 스모크가 이벤트 배선 확인. `|self` 인라인(`e.target === e.currentTarget`)은 Svelte `|self`의 정확한 의미.
