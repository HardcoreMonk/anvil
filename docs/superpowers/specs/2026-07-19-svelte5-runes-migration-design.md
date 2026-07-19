# Svelte 5 runes migration (core, behavior-preserving) — 설계

**날짜:** 2026-07-19
**상태:** 승인됨 (사용자 승인 2026-07-19)
**관련:** operator Web UI (`web/`, goose-daemon `/ui/`에 임베드), [ADR-013](../adr/013-dom-safe-frontend-zone-wide.md) DOM-safe frontend, PR #39(svelte5/vite8 도입, legacy-compat 유지)

## 목표

anvil operator Web UI의 **legacy Svelte 4 반응성을 Svelte 5 runes로 전환**한다 — 범위는 **core runes**(`$props`/`$derived`/`$effect`/`$bindable`). behavior-preserving 리팩터이며, 신규 `svelte-check` 게이트 + 수동 `/ui` 스모크로 검증한다.

## 배경

- `web/`는 30개 `.svelte` 컴포넌트. 앱 엔트리(`main.js`)는 이미 Svelte 5 `mount()` API를 쓰나 컴포넌트는 전부 **legacy 모드**(runes 미사용).
- 프론트엔드 **테스트 없음**(vitest/testing-library/test 스크립트 부재) → behavior-preserving 검증이 최대 리스크.
- Svelte 5는 컴포넌트 단위로 legacy/runes 모드가 갈리며 **혼합 모드를 완전 지원**한다. runes 모드에서 `$:`는 컴파일 에러 → 한 컴포넌트의 props와 반응성은 **동시에**(atomic) 전환해야 한다.
- `on:` 이벤트 디렉티브, `createEventDispatcher`, `$store` 자동구독은 runes 모드에서도 **동작**(deprecation 경고만) → core-runes 범위에서 그대로 유지.

## 스코프

**포함:** `export let`→`$props`(+ 부모 `bind:` 프롭은 `$bindable`), `$:`→`$derived`/`$effect`. `svelte-check` 게이트 신설. **전환 대상 = export let 또는 `$:`를 쓰는 19개 컴포넌트**(union).

**제외(비목표):** `on:`→`onevent`, `createEventDispatcher`→callback props, stores→`$state` 모듈, snippet 전환, vitest/컴포넌트 테스트, props/`$:`가 없는 나머지 ~11개 컴포넌트(전환할 runes 구문 없음 → legacy 유지).

## 전환 대상 (19 컴포넌트, union of export let ∪ `$:`)

```
ActivityFeed, AddAgentModal, BroadcastModal, BuiltinPicker, BuiltinsModal,
ChangeRoleModal, CreateFlockModal, FlockDetail, ModelPicker, Monitoring,
ProfileModal, RestoreModal, SendTaskModal, Settings, SnapshotModal,
SystemPromptModal, TaskPanel, VMDetail, WatchdogPanel
```
(모두 `web/src/components/*.svelte`.)

## 전환 규칙 (컴포넌트 단위 atomic)

1. `export let x` → `let { x } = $props()`; 기본값 있으면 `let { x = d } = $props()`. 한 컴포넌트의 모든 prop을 하나의 `$props()` 구조분해로 모은다.
2. **bindable props**(부모가 `bind:`로 양방향 바인딩): 반드시 `$bindable()`. 확인된 2건:
   - `BuiltinPicker.selected` (부모: BuiltinsModal, ProfileModal) → `let { selected = $bindable() } = $props()`.
   - `ModelPicker.value` (부모: Settings, ProfileModal) → `let { value = $bindable() } = $props()`.
   - (svelte-check가 non-bindable prop 바인딩을 에러로 잡으므로 누락은 게이트에서 검출된다.)
3. `$: x = expr`(derived, 10건) → `let x = $derived(expr)`.
4. `$:` side-effect(1건, `ModelPicker.svelte:19` `$: if (value && models.includes(value)) custom = false`) → `$effect(() => { if (value && models.includes(value)) custom = false })`. **개별 리뷰**: effect는 렌더 후 실행되고 effect 내 state 변경이 재렌더를 유발하므로, 이 케이스가 `$effect`로 정확히 보존되는지 확인하고(필요 시 파생/이벤트 기반으로 재구성) 무한 루프가 없음을 확인한다.
5. **불변**: `$store` 자동구독(`$auth`/`$view`), `on:` 디렉티브, `createEventDispatcher`, 모든 템플릿 구문(`{#if}`/`{#each}`/`{expr}`) 그대로 둔다.

### `$:` 인벤토리 (전건, 파생/이펙트 판정)

| 파일:줄 | 문 | 판정 |
|---|---|---|
| Settings:25 | `availableProviders = providers.filter(...)` | $derived |
| CreateFlockModal:28 | `defaultProfile = profiles.length ? ... : 'default'` | $derived |
| CreateFlockModal:69 | `tokenRows = result ? Object.entries(...) : []` | $derived |
| Monitoring:13 | `src = enabled ? ...` | $derived |
| FlockDetail:29 | `agents = detail && detail.agents ? Object.values(...) : []` | $derived |
| ModelPicker:19 | `if (value && models.includes(value)) custom = false` | **$effect (리뷰)** |
| ProfileModal:29 | `suggested = (providers.find(...)||{}).suggested_models || []` | $derived |
| ProfileModal:32 | `activePreset = presets.find(...) || null` | $derived |
| WatchdogPanel:27 | `failEntries = status && ... ? Object.entries(...) : []` | $derived |
| WatchdogPanel:28 | `deadList = status && ... ? ... : []` | $derived |
| BroadcastModal:56 | `resultRows = result ? Object.entries(...) : []` | $derived |

## 도구

**Hand-migrate.** `sv migrate svelte-5`는 full idiom(`on:`→`onevent`, dispatcher→callback 포함)으로 overshoot → 스코프 밖 변경을 되돌리는 비용이 소규모 수동 전환(~23 props + 11 반응문)보다 크다. 수동으로 전환한다.

## 검증 (테스트 없음 → svelte-check + 수동 스모크)

1. **svelte-check 게이트 신설**: `web/package.json`에 devDep `svelte-check` + `typescript` 추가, 최소 `web/jsconfig.json` 생성, `"check": "svelte-check --tsconfig ./jsconfig.json"` 스크립트 추가. 이는 영구 정확성 게이트(rune 오용·bad binding·미정의 참조 검출).
2. **태스크별**: `npm run check` clean + `npx vite build` 성공(daemon이 `../cmd/goose-daemon/uidist`를 임베드).
3. **개별 리뷰**: 각 `$:`→derived/effect 전환(특히 ModelPicker effect), 각 bindable prop.
4. **수동 스모크(끝에 1회)**: `/ui`에서 핵심 플로우 — login, spawn, flock create/detail, snapshot, settings, model/profile picker(bindable 경로).

## 증분

컴포넌트별로 몇 개씩 SDD 태스크로 묶는다(leaf/단순 모달 먼저, `ModelPicker`+바인더는 집중 태스크, `Settings`/`FlockDetail`/`Monitoring` 등). 앱은 전 과정 동작(혼합 모드). 단일 브랜치/PR.

## DOM-safe (ADR-013)

무관·불변 — `src`에 `{@html}`/`innerHTML`/`document.write` 없음(확인됨). Svelte 템플릿 구문은 ADR-013이 규율하는 수동 DOM 조립/HTML 리터럴이 아니다. 마이그레이션이 새로 도입하지도 않는다.

## 리스크

낮음. 대부분(`$props`)은 기계적·컴파일러 검증. 반응 표면 11문 중 10건 clean `$derived`, 1건 `$effect`(리뷰). 이벤트 불변이라 부모/자식 결합 없음. 판단 지점은 bindable 2건 + effect 1건뿐이며 svelte-check + 리뷰가 커버.

## 비목표 (재확인)

`on:`→`onevent`, `createEventDispatcher`→callback props, stores→`$state`, snippet, vitest/컴포넌트 테스트, props/`$:` 없는 컴포넌트 전환.
