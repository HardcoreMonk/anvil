# Svelte 5 runes migration (core) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Migrate the 19 operator-UI components under `web/src/components/` that use legacy Svelte 4 reactivity to Svelte 5 runes (`$props`/`$state`/`$derived`/`$effect`/`$bindable`), behavior-preserving, gated by a new `svelte-check` step.

**Architecture:** Hand-migrate component-by-component (grouped into tasks). Each component enters runes mode atomically: `export let`→`$props` (+ `$bindable` for parent-bound props), `$:`→`$derived`/`$effect`, and every reactive local `let`→`$state`. A new `svelte-check` gate (`--compiler-warnings non_reactive_update:error`) mechanically catches a missed `$state` (promoted to a failing error). A missed `$bindable` is a Svelte *runtime* error the static gate does NOT catch — the 2 bindable props are specified exactly and verified by the Task 7 manual smoke. Mixed legacy+runes across components is fully supported, so the app builds and works at every step. Stores, `on:` directives, `createEventDispatcher`, and `$store` auto-subscription are unchanged.

**Tech Stack:** Svelte 5.56.4 (pinned in `web/package-lock.json`), Vite 8, `svelte-check` (added here), vanilla JS (no TypeScript source).

## Global Constraints

- **Scope = core runes only.** Convert `export let`→`$props`, `$:`→`$derived`/`$effect`, reactive local `let`→`$state`, parent-bound props→`$bindable`. **Do NOT** change `on:` event directives, `createEventDispatcher`, `$store` auto-subscription (`$auth`/`$view`/`$toasts`), or migrate stores to `$state` modules. These work in runes mode (deprecation warnings for `on:`/dispatch are acceptable and out of scope).
- **Only the 19 listed components** are migrated. Leave the other ~11 (no `export let`, no `$:`) as legacy — mixed mode is supported.
- **Bindable props (exactly two):** `BuiltinPicker.selected` and `ModelPicker.value` are two-way bound by parents → declare with `$bindable()`. No other prop is parent-bound.
- **`$state` is intrinsic:** in runes mode a plain top-level `let` that is reassigned and read in the template/derived is NOT reactive — it must be `let x = $state(init)`. The `svelte-check` gate names any you miss (`non_reactive_update`). `const` and event-handler functions are not `$state`.
- **DOM-safe (ADR-013):** unaffected — no `{@html}`/`innerHTML`/`document.write` exists or may be introduced. Template syntax is compliant.
- **`npm ci` first:** a checkout's `node_modules` may be stale (e.g. Svelte 4.2.20) vs the pinned 5.56.4 — the app won't build until `npm ci`. `node_modules` is gitignored.
- **`uidist` is tracked + `//go:embed`'d:** `cmd/goose-daemon/uidist/` (3 files) is the daemon-embedded bundle. `vite build` outputs there. Rebuild + commit it ONCE at the end (Task 7); do not commit intermediate rebuilds.
- **web/ is not in CI:** verification is local (`npm run check`, `npx vite build`, manual `/ui` smoke).
- **The gate's must-fix set** (this is the DoD for every migration task): **0 errors + 0 `non_reactive_update` warnings** (the `check` script promotes `non_reactive_update` to a failing error via `--compiler-warnings`, so `npm run check` exits non-zero on a missed `$state`). Because the scope KEEPS `on:` directives and `createEventDispatcher` (event idiom out of scope), svelte-check WILL emit **deprecation warnings** for them in runes-mode components — these are **EXPECTED, allowed, and will increase** as components migrate (they do not fail the gate). Never treat a deprecation warning as a defect and never "fix" it by converting `on:`→`onevent` (out of scope). **A missed `$bindable` is a Svelte runtime error the static gate does NOT catch** — it is verified by the Task 7 manual smoke (only 2 props, specified exactly in Task 6).
- **Per-task verification:** `npm run check` at the must-fix set above; commit source only (not `node_modules`, not `uidist`).

## File Structure

- Modify: `web/package.json`, `web/package-lock.json` (add `svelte-check` + `typescript` devDeps + `check` script) — Task 1.
- Create: `web/jsconfig.json` (svelte-check config) — Task 1.
- Modify: 19 files under `web/src/components/*.svelte` — Tasks 2-6.
- Modify (rebuild, commit once): `cmd/goose-daemon/uidist/*` — Task 7.

The 19 components, grouped by shape:
- **props-only** (export let, no `$:`, 11): `AddAgentModal`, `ChangeRoleModal`, `ActivityFeed`, `SystemPromptModal`, `SnapshotModal`, `BuiltinsModal`, `SendTaskModal`, `RestoreModal`, `VMDetail`, `TaskPanel`, `BuiltinPicker`.
- **`$:`-only** (no export let, 4): `Settings`, `CreateFlockModal`, `Monitoring`, `WatchdogPanel`.
- **both props + `$:`** (4): `FlockDetail`, `ModelPicker`, `ProfileModal`, `BroadcastModal`.

---

### Task 1: `svelte-check` gate + baseline

**Files:**
- Modify: `web/package.json`, `web/package-lock.json`
- Create: `web/jsconfig.json`

**Interfaces:**
- Produces: `npm run check` (in `web/`) → runs `svelte-check`; the per-task gate for Tasks 2-7.

- [ ] **Step 1: Install deps (fixes any stale `node_modules`) and add the checker**

Run (in `web/`):
```bash
cd /data/projects/claude-zone/anvil/web
npm ci                                   # install pinned svelte 5.56.4 (node_modules may be stale)
node -p "require('svelte/package.json').version"   # expect 5.56.4
npm install --save-dev svelte-check typescript
```
Expected: `svelte/package.json` version prints `5.56.4`; `npm install` adds `svelte-check` + `typescript` to `devDependencies` and updates `package-lock.json`.

- [ ] **Step 2: Create `web/jsconfig.json`**

`svelte-check` needs a tsconfig/jsconfig. Create `web/jsconfig.json`:
```json
{
  "compilerOptions": {
    "moduleResolution": "bundler",
    "target": "esnext",
    "module": "esnext",
    "checkJs": false,
    "allowJs": true
  },
  "include": ["src/**/*.js", "src/**/*.svelte"]
}
```
(`checkJs:false` keeps this a Svelte-correctness gate, not a full JS type-check of the vanilla JS — we want rune/binding/template errors, not to newly type-check untyped `.js`.)

- [ ] **Step 3: Add the `check` script to `web/package.json`**

In `web/package.json` `"scripts"`, add `check` alongside the existing `dev`/`build`/`preview`:
```json
  "scripts": {
    "dev": "vite",
    "build": "vite build",
    "preview": "vite preview",
    "check": "svelte-check --tsconfig ./jsconfig.json"
  },
```

- [ ] **Step 4: Establish the baseline on the current (legacy) code**

Run:
```bash
cd /data/projects/claude-zone/anvil/web
npm run check 2>&1 | tail -20
npx vite build 2>&1 | tail -5
```
Expected: `vite build` succeeds (writes `../cmd/goose-daemon/uidist/`). `npm run check` prints a summary like `svelte-check found N errors and M warnings`. **Record N (errors) as the baseline — expected 0** (legacy Svelte 4 code is valid). If N>0, list the errors in the report; they are pre-existing/out of scope, and every later task must keep **errors** at that baseline (never increase). Note `M` (warnings) is informational only — it will **grow** as components migrate (each runes-mode component that keeps `on:`/dispatch adds deprecation warnings, which are expected per Global Constraints). The number that must stay clean is: errors + `non_reactive_update` + non-bindable-binding warnings (all expected 0). Discard the `uidist` rebuild from this step: `git checkout -- cmd/goose-daemon/uidist` (Task 7 owns the committed rebuild).

- [ ] **Step 5: Commit (source config only — not `node_modules`, not `uidist`)**

```bash
cd /data/projects/claude-zone/anvil
git checkout -- cmd/goose-daemon/uidist 2>/dev/null || true
git add web/package.json web/package-lock.json web/jsconfig.json
git commit -m "chore(web): add svelte-check gate (npm run check) for the runes migration"
```
Expected: `git status` shows no `uidist`/`node_modules` changes staged; commit contains exactly the 3 files.

---

### Task 2: Migrate props-only modals — group A (5 components)

**Files (Modify):** `web/src/components/AddAgentModal.svelte`, `ChangeRoleModal.svelte`, `ActivityFeed.svelte`, `SystemPromptModal.svelte`, `SnapshotModal.svelte`

**Interfaces:**
- Consumes: `npm run check` (Task 1).
- Produces: these 5 components in runes mode. None has a parent-bound prop, so no `$bindable`.

Per-component prop conversions (replace the `export let` line(s) with a single `$props()` destructure at the same spot):

| Component | Before | After |
|---|---|---|
| `AddAgentModal` | `export let flockId` | `let { flockId } = $props()` |
| `ChangeRoleModal` | `export let flockId` / `export let agent` | `let { flockId, agent } = $props()` |
| `ActivityFeed` | `export let flockId` | `let { flockId } = $props()` |
| `SystemPromptModal` | `export let name` | `let { name } = $props()` |
| `SnapshotModal` | `export let vmId` | `let { vmId } = $props()` |

Preserve the trailing `// ...` comments by moving them onto the destructure or the line above.

- [ ] **Step 1: Convert props in all 5 components**

For each component, replace its `export let` declaration(s) with the single `$props()` destructure from the table above.

- [ ] **Step 2: Convert every reactive local `let` to `$state` in these 5 components**

In each of the 5 files, find every top-level `let x` (in `<script>`) that is **reassigned** anywhere and **read in the template or a `$:`/derived** — convert `let x = init` → `let x = $state(init)`. (Form fields, `busy`/`loading` flags, `error` strings, result holders, etc.) Do NOT convert `const`, imported bindings, or `let` that is only ever read (never reassigned). Do not guess — Step 3's `npm run check` names each missed one.

- [ ] **Step 3: Run the gate and fix every runes warning**

Run:
```bash
cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -30
```
Expected (the must-fix set): 0 errors, **0 `non_reactive_update` warnings**, **0 binding warnings**. If svelte-check reports `` `x` is updated, but is not declared with `$state(...)` `` for any variable, wrap that `let` in `$state()` and re-run until clean. Errors stay at the Task-1 baseline (0). `on:`/dispatch **deprecation** warnings are expected and ignored (do not "fix" them).

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/AddAgentModal.svelte web/src/components/ChangeRoleModal.svelte web/src/components/ActivityFeed.svelte web/src/components/SystemPromptModal.svelte web/src/components/SnapshotModal.svelte
git commit -m "refactor(web): migrate AddAgent/ChangeRole/ActivityFeed/SystemPrompt/Snapshot modals to runes"
```

---

### Task 3: Migrate props-only modals — group B (5 components)

**Files (Modify):** `web/src/components/BuiltinsModal.svelte`, `SendTaskModal.svelte`, `RestoreModal.svelte`, `VMDetail.svelte`, `TaskPanel.svelte`

**Interfaces:**
- Consumes: `npm run check` (Task 1). `BuiltinsModal` renders `<BuiltinPicker bind:selected …>` — that `bind:` on the *child* is unchanged here; `BuiltinPicker` itself gets `$bindable` in Task 6. `BuiltinsModal`'s own props (`name`, `options`) are NOT parent-bound → plain `$props`.
- Produces: these 5 components in runes mode.

Per-component prop conversions:

| Component | Before | After |
|---|---|---|
| `BuiltinsModal` | `export let name` / `export let options = []` | `let { name, options = [] } = $props()` |
| `SendTaskModal` | `export let agent` | `let { agent } = $props()` |
| `RestoreModal` | `export let snapshot` | `let { snapshot } = $props()` |
| `VMDetail` | `export let vm` | `let { vm } = $props()` |
| `TaskPanel` | `export let vmId` | `let { vmId } = $props()` |

- [ ] **Step 1: Convert props in all 5 components** (table above).

- [ ] **Step 2: Convert every reactive local `let`→`$state`** in these 5 files (same rule as Task 2 Step 2).

- [ ] **Step 3: Run the gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -30`
Expected (must-fix set): 0 errors, 0 `non_reactive_update`, 0 binding warnings. Fix any `non_reactive_update` by wrapping in `$state()`. `on:`/dispatch deprecation warnings are expected/ignored.

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/BuiltinsModal.svelte web/src/components/SendTaskModal.svelte web/src/components/RestoreModal.svelte web/src/components/VMDetail.svelte web/src/components/TaskPanel.svelte
git commit -m "refactor(web): migrate Builtins/SendTask/Restore/VMDetail/TaskPanel to runes"
```

---

### Task 4: Migrate `$:`-only components (4 components)

**Files (Modify):** `web/src/components/Settings.svelte`, `CreateFlockModal.svelte`, `Monitoring.svelte`, `WatchdogPanel.svelte`

**Interfaces:**
- Consumes: `npm run check` (Task 1). These have no `export let` — they enter runes mode via `$derived`/`$state`. `Settings` renders `<ModelPicker bind:value={p.model}>` — the `bind:` is unchanged here (ModelPicker gets `$bindable` in Task 6).
- Produces: these 4 in runes mode.

`$:` conversions (all are derived assignments → `$derived`):

| File:line | Before | After |
|---|---|---|
| `Settings:25` | `$: availableProviders = providers.filter((p) => p.available)` | `let availableProviders = $derived(providers.filter((p) => p.available))` |
| `CreateFlockModal:28` | `$: defaultProfile = profiles.length ? profiles[0].name : 'default'` | `let defaultProfile = $derived(profiles.length ? profiles[0].name : 'default')` |
| `CreateFlockModal:69` | `$: tokenRows = result ? Object.entries(result.agent_tokens || {}) : []` | `let tokenRows = $derived(result ? Object.entries(result.agent_tokens || {}) : [])` |
| `Monitoring:13` | `$: src = enabled ? …` (multi-line) | `let src = $derived(enabled ? … )` (keep the full expression) |
| `WatchdogPanel:27` | `$: failEntries = status && status.vm_fail_counts ? Object.entries(status.vm_fail_counts) : []` | `let failEntries = $derived(status && status.vm_fail_counts ? Object.entries(status.vm_fail_counts) : [])` |
| `WatchdogPanel:28` | `$: deadList = status && status.vm_dead_marked ? status.vm_dead_marked : []` | `let deadList = $derived(status && status.vm_dead_marked ? status.vm_dead_marked : [])` |

Note `Monitoring:13` is a multi-line expression — wrap the entire right-hand side (through its last line) in `$derived( … )`.

- [ ] **Step 1: Convert each `$:` to `$derived`** (table above). Note: `Settings`/`CreateFlockModal` reference stores (`$auth` etc.) elsewhere — leave those unchanged.

- [ ] **Step 2: Convert every reactive local `let`→`$state`** in these 4 files. In runes mode any reassigned template-read `let` (e.g. `CreateFlockModal`'s form fields + `result`, `Settings`' editable rows, `Monitoring`'s toggles, `WatchdogPanel`'s local state) must be `$state`. The gate names misses.

- [ ] **Step 3: Run the gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -30`
Expected (must-fix set): 0 errors, 0 `non_reactive_update`, 0 binding warnings. A `$derived` whose source is itself a `$state` updates automatically — verify no `non_reactive_update` remains. `on:`/dispatch deprecation warnings expected/ignored.

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/Settings.svelte web/src/components/CreateFlockModal.svelte web/src/components/Monitoring.svelte web/src/components/WatchdogPanel.svelte
git commit -m "refactor(web): migrate Settings/CreateFlock/Monitoring/Watchdog to runes ($derived/$state)"
```

---

### Task 5: Migrate props + `$:` components (3 components)

**Files (Modify):** `web/src/components/FlockDetail.svelte`, `ProfileModal.svelte`, `BroadcastModal.svelte`

**Interfaces:**
- Consumes: `npm run check` (Task 1). `ProfileModal` renders `<ModelPicker bind:value={model}>` and `<BuiltinPicker bind:selected={selectedBuiltins}>` — the `bind:`s are unchanged here (children get `$bindable` in Task 6). `ProfileModal`'s own props (`providers`/`presets`/`builtins`) are NOT parent-bound → plain `$props`.
- Produces: these 3 in runes mode.

Prop conversions:

| Component | Before | After |
|---|---|---|
| `FlockDetail` | `export let flock` | `let { flock } = $props()` |
| `ProfileModal` | `export let providers = []` / `export let presets = []` / `export let builtins = []` | `let { providers = [], presets = [], builtins = [] } = $props()` |
| `BroadcastModal` | `export let flockId` | `let { flockId } = $props()` |

`$:` conversions (all derived → `$derived`):

| File:line | After |
|---|---|
| `FlockDetail:29` | `let agents = $derived(detail && detail.agents ? Object.values(detail.agents) : [])` |
| `ProfileModal:29` | `let suggested = $derived((providers.find((p) => p.id === provider) || {}).suggested_models || [])` |
| `ProfileModal:32` | `let activePreset = $derived(presets.find((p) => p.vcpu_count === vcpu && p.mem_size_mib === mem) || null)` |
| `BroadcastModal:56` | `let resultRows = $derived(result ? Object.entries(result.results || {}) : [])` |

Note `ProfileModal`'s `$derived`s reference `provider`/`vcpu`/`mem` — those are local editable state and must be `$state` (Step 2), so the `$derived` recomputes when they change.

- [ ] **Step 1: Convert props** (table).
- [ ] **Step 2: Convert `$:`→`$derived`** (table).
- [ ] **Step 3: Convert every reactive local `let`→`$state`** — especially `ProfileModal`'s `provider`/`vcpu`/`mem`/`model`/`selectedBuiltins`/`busy` (they feed the `$derived`s and `bind:`s), `FlockDetail`'s `detail`/loading state, `BroadcastModal`'s form + `result`.
- [ ] **Step 4: Run the gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -30`
Expected (must-fix set): 0 errors, 0 `non_reactive_update`, 0 binding warnings. `on:`/dispatch deprecation warnings expected/ignored.

- [ ] **Step 5: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/FlockDetail.svelte web/src/components/ProfileModal.svelte web/src/components/BroadcastModal.svelte
git commit -m "refactor(web): migrate FlockDetail/ProfileModal/BroadcastModal to runes"
```

---

### Task 6: Migrate the bindable + effect components (2 components)

**Files (Modify):** `web/src/components/BuiltinPicker.svelte`, `web/src/components/ModelPicker.svelte`

**Interfaces:**
- Consumes: `npm run check` (Task 1). These two have props that parents two-way bind (`BuiltinsModal`/`ProfileModal` bind `BuiltinPicker.selected`; `Settings`/`ProfileModal` bind `ModelPicker.value`) — those props MUST be `$bindable()` or the parents' `bind:` errors.
- Produces: these 2 in runes mode; the two `$bindable` props preserved so parent `bind:` keeps working.

**`BuiltinPicker.svelte`** prop conversion:
```
export let options = []      →   (see combined destructure below)
export let selected = []     →   selected is $bindable
export let disabled = false
```
Combined:
```js
let { options = [], selected = $bindable([]), disabled = false } = $props()
```
Then convert any reactive local `let` in `BuiltinPicker` to `$state` (the gate names them).

**`ModelPicker.svelte`** — the highest-attention component (bindable `value` + a `$:` side-effect + a reactive local `let custom`). Current `<script>`:
```js
export let models = []
export let value = ''
export let id = undefined
const CUSTOM = '__custom__'
let custom = !!value && !models.includes(value)
$: if (value && models.includes(value)) custom = false
function onSelect(e) { … value = … ; custom = … }
```
Runes version of the `<script>` (template unchanged — it already uses `bind:value` on the input and `value={…}` / `on:change` on the select):
```js
let { models = [], value = $bindable(''), id = undefined } = $props()

const CUSTOM = '__custom__'

// custom = show the free-text input because the value is not a suggested model.
let custom = $state(!!value && !models.includes(value))

// When the suggested list changes so the value IS a suggested model, drop back
// to the dropdown. (Svelte 4 ran this pre-render via `$:`; $effect runs post-render
// — behavior-equivalent here: it only flips custom→false, is idempotent, reads
// value/models and writes custom, so it cannot loop. Verified by the /ui smoke.)
$effect(() => {
  if (value && models.includes(value)) custom = false
})

function onSelect(e) {
  if (e.target.value === CUSTOM) {
    custom = true
    value = ''
  } else {
    custom = false
    value = e.target.value
  }
}
```
- `value` is `$bindable('')` (parent binds it; `onSelect` and the `<input bind:value>` write it).
- `custom` is `$state(...)` (reassigned in `$effect` and `onSelect`, read in the template).
- `models`/`id` are plain props. `CUSTOM` stays `const`.
- The template (`<select>`/`{#each}`/`{#if custom}`/`<input bind:value>`) is unchanged.

- [ ] **Step 1: Migrate `BuiltinPicker.svelte`** — the combined `$props` destructure with `selected = $bindable([])`; convert its reactive locals to `$state`.

- [ ] **Step 2: Migrate `ModelPicker.svelte`** — replace the `<script>` body with the runes version above (props with `$bindable value`, `custom = $state(...)`, `$: →$effect`). Leave the template untouched.

- [ ] **Step 3: Run the gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -30`
Expected (must-fix set): 0 errors, 0 `non_reactive_update` (e.g. for `custom`). **NOTE:** svelte-check does NOT catch a missed `$bindable` (binding to a non-bindable prop is a Svelte *runtime* error, not a static one — confirmed empirically). So apply the `$bindable()` on `selected`/`value` **exactly as specified above**; its correctness is verified at runtime by the Task 7 manual smoke (in ProfileModal/Settings, pick a model and toggle "custom"; in Builtins, toggle a builtin — the parent's `bind:` must round-trip). (`on:change` deprecation warning in ModelPicker is expected/ignored.)

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/BuiltinPicker.svelte web/src/components/ModelPicker.svelte
git commit -m "refactor(web): migrate BuiltinPicker/ModelPicker to runes (\$bindable + \$effect)"
```

---

### Task 7: Rebuild `uidist`, full build, and smoke checklist

**Files (Modify):** `cmd/goose-daemon/uidist/index.html`, `cmd/goose-daemon/uidist/assets/*` (regenerated by the build)

**Interfaces:**
- Consumes: all 19 migrated components + the Task-1 gate.
- Produces: the rebuilt embedded bundle matching the runes source; a recorded smoke result.

- [ ] **Step 1: Full check + build**

Run:
```bash
cd /data/projects/claude-zone/anvil/web
npm run check 2>&1 | tail -5      # whole tree: 0 errors, 0 non_reactive_update, 0 binding warnings (on:/dispatch deprecation warnings expected)
npx vite build 2>&1 | tail -6     # regenerates ../cmd/goose-daemon/uidist/
```
Expected: check clean; build succeeds and writes `cmd/goose-daemon/uidist/`.

- [ ] **Step 2: Confirm the Go daemon still builds with the new embedded bundle**

Run: `cd /data/projects/claude-zone/anvil && go build ./cmd/goose-daemon`
Expected: builds (the `//go:embed uidist` picks up the rebuilt files).

- [ ] **Step 3: Manual `/ui` smoke (record results in the report)**

Serve the daemon UI (or `cd web && npm run preview` for the static bundle) and click through, confirming no console errors and unchanged behavior:
- Login screen renders / auth flow.
- Spawn a VM (SpawnModal), VM detail (VMDetail), task panel (TaskPanel).
- Flock create (CreateFlockModal → token rows), flock detail (FlockDetail → agents), add agent (AddAgentModal), change role (ChangeRoleModal), broadcast (BroadcastModal → result rows).
- Snapshot (SnapshotModal), restore (RestoreModal).
- Settings (Settings → provider list), Profile modal (ProfileModal → **ModelPicker**: pick a suggested model, switch to "custom", type a custom value, switch provider — confirm the custom input toggles correctly; this exercises the `$effect` + `$bindable`), Builtins (BuiltinsModal → **BuiltinPicker** bind:selected).
- Monitoring tab (Monitoring → Grafana iframe), Watchdog panel (WatchdogPanel).

Record pass/fail per area in the report. Any behavior change or console error is a defect to fix (likely a missed `$state`).

- [ ] **Step 4: Commit the rebuilt bundle**

```bash
cd /data/projects/claude-zone/anvil
git add cmd/goose-daemon/uidist
git commit -m "build(web): rebuild embedded uidist from runes-migrated components"
```
Expected: commit contains only `cmd/goose-daemon/uidist/*`.

---

## Notes for the executor

- **`npm run check` is the correctness engine.** The transformation is largely mechanical; the gate catches the two easy-to-miss classes: a reactive `let` not wrapped in `$state` (`non_reactive_update` warning) and a bound prop not marked `$bindable` (binding error). Treat **every** such warning/error as must-fix — a task is not done while any remains.
- **Do not run `sv migrate`** — it overshoots to the full Svelte-5 idiom (`on:`→`onevent`, dispatchers→callbacks) that is explicitly out of scope.
- **Do not touch** `on:` directives, `createEventDispatcher`, `$store` (`$auth`/`$view`/`$toasts`) usage, or the ~11 non-migrated components.
- **`node_modules`/`uidist` hygiene:** never `git add web/node_modules`; only Task 7 commits `uidist`. If an intermediate `vite build` dirties `uidist`, `git checkout -- cmd/goose-daemon/uidist` before committing source.
- **Model routing (SDD):** Tasks 2-3 (mechanical props+$state) → cheap model. Tasks 4-5 ($derived + $state) → standard. Task 6 ($bindable + $effect, behavioral) → standard/most-capable. Task 1 (tooling) → standard. Task 7 (build + smoke) → standard. Final whole-branch review → most capable.
- **No KVM/Go-test impact:** this is frontend-only; Go tests are unaffected. The Go `gofmt` CI gate doesn't touch `.svelte`/`.js`.
