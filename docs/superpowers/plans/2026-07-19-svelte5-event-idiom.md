# Svelte 5 event-idiom modernization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the deprecated Svelte 4 event idiom across `web/src` — `on:event`→event attributes, event modifiers inlined, `createEventDispatcher`→callback props — bringing `event_directive_deprecated` warnings to zero, behavior-preserving.

**Architecture:** Hand-migrate, each file touched once. Two independent transform classes are per-file mechanical (DOM `on:`→`on`; inline the 17 removed event modifiers). The third (custom events→callback props) couples each modal with its parent, so tasks are grouped by parent-cluster. The existing `svelte-check` gate catches signature errors; a final task promotes `event_directive_deprecated` to a failing error and greps to prove `createEventDispatcher` is gone.

**Tech Stack:** Svelte 5.56.4, Vite 8, `svelte-check` (gate already present from #87). node_modules already installed.

## Global Constraints

- **Behavior-preserving.** No user-visible behavior change. Every event that fired before fires after, with the same handler behavior.
- **Transform class 1 — DOM events → event attributes:** `on:click={h}`→`onclick={h}`, `on:change`→`onchange`, `on:input`→`oninput`, `on:keydown`→`onkeydown`, `on:submit`→`onsubmit`. Handler body UNCHANGED (both receive the DOM event). Works in legacy and runes mode. **A bare `on:xxx` (no modifier) is a plain rename; a modifier site is NOT** (class 2).
- **Transform class 2 — event modifiers (Svelte 5 removed them, must inline), 17 sites:**
  - `on:click|self={h}` (16×, modal-backdrop) → `onclick={(e) => e.target === e.currentTarget && h}` where `h` is the existing handler expression (e.g. `close` → `&& close()`; `() => (x = null)` → `&& (x = null)`). This is the exact `|self` semantics (fire only when the event target is the element itself).
  - `on:submit|preventDefault={submit}` (1×, `Login.svelte`) → `onsubmit={(e) => { e.preventDefault(); submit() }}`.
- **Transform class 3 — custom events → callback props (coupled, 11 modals + 6 parents):**
  - Child: remove `createEventDispatcher` from the `svelte` import + delete `const dispatch = createEventDispatcher()`; add the callback names to `$props()`; replace each `dispatch('x'[, detail])` with `onx?.([detail])`. Names: `close`→`onclose`, `created`→`oncreated`, `changed`→`onchanged`, `added`→`onadded`, `restored`→`onrestored`, `spawned`→`onspawned`.
  - Parent: `<Modal on:close={h} on:created={f} />` → `<Modal onclose={h} oncreated={f} />`.
  - **The one detail-carrying case:** `SnapshotModal` `dispatch('created', { stopAfter })` → `oncreated?.({ stopAfter })`; its parent `VMDetail`'s `onSnapshotCreated` reads the argument directly (it was already `event.detail`-free — a callback prop passes `{stopAfter}` as the first arg; if the handler ignored the payload before, it still works).
- **`SpawnModal` completes its runes conversion:** it is the one dispatcher-modal still in legacy mode. Adding callback props via `$props()` puts it in runes mode, so its reactive local `let`s must become `$state` (the gate's `non_reactive_update:error` names any miss).
- **The gate.** During migration, `npm run check` uses the existing `--compiler-warnings "non_reactive_update:error"`; the per-task DoD is **0 errors** and the migrated files' `event_directive_deprecated` count strictly decreases (deprecation warnings are still allowed mid-migration). The FINAL task promotes `event_directive_deprecated:error` (now repo-wide 0) and asserts `grep -rn createEventDispatcher web/src` is empty. `createEventDispatcher` itself emits NO svelte-check warning, so grep — not the gate — proves its removal. Benign `state_referenced_locally` warnings (~10, from #87) persist and are NOT promoted (out of scope).
- **DO NOT** change store/`$store` usage, template structure beyond the event attributes, or migrate non-event runes concerns. No `sv migrate`.
- **`uidist` is tracked + `//go:embed`'d** (`cmd/goose-daemon/uidist/`) — rebuild + commit ONLY in the final task. If an intermediate `vite build` dirties it, `git checkout -- cmd/goose-daemon/uidist`.
- **Verification per task:** `cd web && npm run check` (0 errors); commit source only (not node_modules/uidist).

## File Structure

- **Task 1 (DOM-only files — no `createEventDispatcher`, not a modal-parent):** `App.svelte`, `System.svelte`, `AuditLog.svelte`, `Login.svelte` (has the `|preventDefault`), `TaskPanel.svelte`, `ModelPicker.svelte`, `ActivityFeed.svelte`. DOM `on:`→`on` + modifiers only.
- **Tasks 2-7 (parent-clusters — each: DOM `on:`→`on` + backdrop `|self` inline + `createEventDispatcher`→callback props for its modals + parent `on:X`→`onX`):**
  - T2: `Flocks.svelte` + `CreateFlockModal.svelte`
  - T3: `Settings.svelte` + `ProfileModal.svelte` + `SystemPromptModal.svelte` + `BuiltinsModal.svelte`
  - T4: `FlockDetail.svelte` + `AddAgentModal.svelte` + `BroadcastModal.svelte` + `SendTaskModal.svelte` + `ChangeRoleModal.svelte`
  - T5: `VMDetail.svelte` + `SnapshotModal.svelte`
  - T6: `Snapshots.svelte` + `RestoreModal.svelte`
  - T7: `VMList.svelte` + `SpawnModal.svelte` (SpawnModal also → runes/`$state`)
- **Task 8:** gate promotion + `createEventDispatcher` grep-assert + `uidist` rebuild + build + `/ui` smoke.

Every file with `on:` appears in exactly one task; each modal's callback-props change and its parent's call-site change live in the same task (no cross-task coupling).

---

### Task 1: DOM-only files — `on:`→`on` + modifiers

**Files (Modify):** `web/src/App.svelte`, `web/src/components/System.svelte`, `web/src/components/AuditLog.svelte`, `web/src/components/Login.svelte`, `web/src/components/TaskPanel.svelte`, `web/src/components/ModelPicker.svelte`, `web/src/components/ActivityFeed.svelte` (7 files — the `on:`-using files that are neither modals nor modal-parents; `Monitoring`/`WatchdogPanel` have no `on:`)

**Interfaces:** none consumed/produced (these files dispatch no custom events and are not modal parents). Pure class-1/2 transforms.

- [ ] **Step 1: Rename every bare DOM `on:xxx={h}` → `onxxx={h}`**

In each of the 7 files, for every `on:click`/`on:change`/`on:input`/`on:keydown` **without** a `|modifier`, drop the colon: `on:click={handler}` → `onclick={handler}`. Handler expressions are unchanged. Find them: `grep -nE 'on:[a-z]+=' <file>`. Do NOT touch any `on:xxx|mod` (Step 2) and do NOT touch `on:<customEvent>` on a capitalized component (there are none in these files).

- [ ] **Step 2: Inline the one modifier site (`Login.svelte`)**

`Login.svelte` has `<form on:submit|preventDefault={submit}>`. Replace with:
```svelte
<form onsubmit={(e) => { e.preventDefault(); submit() }}>
```
(These 7 files have no `on:click|self` — those are all in the modal/parent files, Tasks 2-7. Confirm with `grep -rnE 'on:[a-z]+\|' <the 7 files>` → only the Login line.)

- [ ] **Step 3: Gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -5`
Expected: **0 errors**. `event_directive_deprecated` warnings for these 7 files are gone (repo-wide count drops). `on:` deprecation warnings from the not-yet-migrated files remain (expected). No new `non_reactive_update`.

- [ ] **Step 4: Verify no bare DOM `on:` remains in these 7 files**

Run: `grep -rnE 'on:(click|change|input|keydown|submit)' web/src/App.svelte web/src/components/System.svelte web/src/components/AuditLog.svelte web/src/components/Login.svelte web/src/components/TaskPanel.svelte web/src/components/ModelPicker.svelte web/src/components/ActivityFeed.svelte`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/App.svelte web/src/components/System.svelte web/src/components/AuditLog.svelte web/src/components/Login.svelte web/src/components/TaskPanel.svelte web/src/components/ModelPicker.svelte web/src/components/ActivityFeed.svelte
git commit -m "refactor(web): DOM on: -> event attributes in non-modal files (Login preventDefault inlined)"
```

---

### Task 2: Flocks + CreateFlockModal (callback props)

**Files (Modify):** `web/src/components/Flocks.svelte`, `web/src/components/CreateFlockModal.svelte`

**Interfaces:**
- Produces: `CreateFlockModal` gains `$props()` callbacks `onclose`, `oncreated` (called with no args). Its parent `Flocks` passes them.

- [ ] **Step 1: `CreateFlockModal.svelte` — DOM `on:`→`on` + backdrop `|self` + callback props**

- Rename bare DOM `on:xxx`→`onxxx` (Step-1 rule from Task 1).
- Backdrop: `on:click|self={close}` → `onclick={(e) => e.target === e.currentTarget && close()}`; the sibling `on:keydown={(e) => e.key === 'Escape' && close()}` → `onkeydown={(e) => e.key === 'Escape' && close()}` (plain rename).
- Callback props: in the `svelte` import remove `createEventDispatcher`; delete `const dispatch = createEventDispatcher()`; add `onclose, oncreated` to the existing `$props()` destructure (CreateFlockModal is already runes from #87). Replace `dispatch('close')`→`onclose?.()` and `dispatch('created')`→`oncreated?.()`. (`function close() { dispatch('close') }` → `function close() { onclose?.() }`.)

- [ ] **Step 2: `Flocks.svelte` — parent call site + its own DOM `on:`**

- Rename `Flocks`'s own bare DOM `on:xxx`→`onxxx`; inline any `on:click|self`/`|preventDefault` it has (grep `on:[a-z]+\|` in Flocks — the `<CreateFlockModal>` line at ~87 uses custom events, handled next).
- Line ~87: `<CreateFlockModal on:close={() => (showCreate = false)} on:created={refresh} />` → `<CreateFlockModal onclose={() => (showCreate = false)} oncreated={refresh} />`.

- [ ] **Step 3: Gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -5`
Expected: 0 errors. No `createEventDispatcher` warning class exists, so confirm removal by grep: `grep -n createEventDispatcher web/src/components/CreateFlockModal.svelte` → empty.

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/Flocks.svelte web/src/components/CreateFlockModal.svelte
git commit -m "refactor(web): Flocks/CreateFlockModal event idiom -> event attrs + callback props"
```

---

### Task 3: Settings + ProfileModal + SystemPromptModal + BuiltinsModal (callback props)

**Files (Modify):** `web/src/components/Settings.svelte`, `web/src/components/ProfileModal.svelte`, `web/src/components/SystemPromptModal.svelte`, `web/src/components/BuiltinsModal.svelte`

**Interfaces:**
- Produces: `ProfileModal` callbacks `onclose`, `oncreated`; `SystemPromptModal` `onclose`; `BuiltinsModal` `onclose`. Parent `Settings` passes them.

- [ ] **Step 1: The 3 modals — DOM `on:`→`on` + backdrop `|self` + callback props**

For each of `ProfileModal`, `SystemPromptModal`, `BuiltinsModal` (all already runes):
- Rename bare DOM `on:xxx`→`onxxx`.
- Backdrop: `on:click|self={close}` → `onclick={(e) => e.target === e.currentTarget && close()}`; `on:keydown={(e) => e.key === 'Escape' && close()}` → `onkeydown={...}` (plain rename).
- Remove `createEventDispatcher` import + `const dispatch = ...`; add callbacks to `$props()`: ProfileModal `onclose, oncreated`; SystemPromptModal `onclose`; BuiltinsModal `onclose`. Replace `dispatch('close')`→`onclose?.()`, `dispatch('created')`→`oncreated?.()`, `function close()`→`onclose?.()`.

- [ ] **Step 2: `Settings.svelte` — parent call sites + its own DOM `on:`**

- Rename Settings's own bare DOM `on:xxx`→`onxxx`; inline any modifier it has.
- `<ProfileModal ... on:created={() => { showCreate = false; load() }} on:close={() => (showCreate = false)} />` → `onclose={...}` / `oncreated={...}` (line ~154-156).
- `<SystemPromptModal name={systemPromptName} on:close={() => (systemPromptName = null)} />` → `onclose={...}` (line ~160).
- `<BuiltinsModal name={builtinsName} options={builtins} on:close={() => (builtinsName = null)} />` → `onclose={...}` (line ~164).

- [ ] **Step 3: Gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -5`
Expected: 0 errors. `grep -n createEventDispatcher web/src/components/ProfileModal.svelte web/src/components/SystemPromptModal.svelte web/src/components/BuiltinsModal.svelte` → empty.

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/Settings.svelte web/src/components/ProfileModal.svelte web/src/components/SystemPromptModal.svelte web/src/components/BuiltinsModal.svelte
git commit -m "refactor(web): Settings + Profile/SystemPrompt/Builtins modals -> event attrs + callback props"
```

---

### Task 4: FlockDetail + AddAgentModal + BroadcastModal + SendTaskModal + ChangeRoleModal (callback props)

**Files (Modify):** `web/src/components/FlockDetail.svelte`, `web/src/components/AddAgentModal.svelte`, `web/src/components/BroadcastModal.svelte`, `web/src/components/SendTaskModal.svelte`, `web/src/components/ChangeRoleModal.svelte`

**Interfaces:**
- Produces: `AddAgentModal` `onclose, onadded`; `BroadcastModal` `onclose`; `SendTaskModal` `onclose`; `ChangeRoleModal` `onclose, onchanged`. Parent `FlockDetail` passes them.

- [ ] **Step 1: The 4 modals — DOM `on:`→`on` + backdrop `|self` + callback props** (all already runes)

For each modal: rename bare DOM `on:xxx`→`onxxx`; backdrop `on:click|self={close}`→`onclick={(e) => e.target === e.currentTarget && close()}` + `on:keydown` rename; remove `createEventDispatcher`/`const dispatch`; add callbacks to `$props()` — AddAgentModal `onclose, onadded`; BroadcastModal `onclose`; SendTaskModal `onclose`; ChangeRoleModal `onclose, onchanged`. Replace dispatches: `dispatch('close')`→`onclose?.()`, `dispatch('added')`→`onadded?.()`, `dispatch('changed')`→`onchanged?.()`.

- [ ] **Step 2: `FlockDetail.svelte` — parent call sites + its own DOM `on:` + its 3 backdrop `|self`**

- Rename FlockDetail's bare DOM `on:xxx`→`onxxx`.
- FlockDetail has THREE confirm-dialog backdrops with `on:click|self` (lines ~198/211/224): `on:click|self={() => (confirmingRemove = null)}` → `onclick={(e) => e.target === e.currentTarget && (confirmingRemove = null)}` (and similarly `confirmingRestart`, `confirmingDelete`); their sibling `on:keydown` → plain rename.
- Modal call sites (lines ~185/188/191/194): `on:close`→`onclose`, `on:added={refreshDetail}`→`onadded={refreshDetail}`, `on:changed={refreshDetail}`→`onchanged={refreshDetail}`.

- [ ] **Step 3: Gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -5`
Expected: 0 errors. `grep -rn createEventDispatcher web/src/components/AddAgentModal.svelte web/src/components/BroadcastModal.svelte web/src/components/SendTaskModal.svelte web/src/components/ChangeRoleModal.svelte` → empty.

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/FlockDetail.svelte web/src/components/AddAgentModal.svelte web/src/components/BroadcastModal.svelte web/src/components/SendTaskModal.svelte web/src/components/ChangeRoleModal.svelte
git commit -m "refactor(web): FlockDetail + AddAgent/Broadcast/SendTask/ChangeRole -> event attrs + callback props"
```

---

### Task 5: VMDetail + SnapshotModal (callback props, the `{stopAfter}` detail case)

**Files (Modify):** `web/src/components/VMDetail.svelte`, `web/src/components/SnapshotModal.svelte`

**Interfaces:**
- Produces: `SnapshotModal` callbacks `onclose` (no arg), `oncreated` called **with `{ stopAfter }`**. Parent `VMDetail`'s `onSnapshotCreated` receives that object as its first argument.

- [ ] **Step 1: `SnapshotModal.svelte` — DOM `on:`→`on` + backdrop `|self` + callback props (detail)** (already runes)

- Rename bare DOM `on:xxx`→`onxxx`; backdrop `on:click|self={close}`→`onclick={(e) => e.target === e.currentTarget && close()}` + `on:keydown` rename.
- Remove `createEventDispatcher` import + `const dispatch = createEventDispatcher()`; add `onclose, oncreated` to `$props()`. Replace `dispatch('created', { stopAfter })` → `oncreated?.({ stopAfter })`; `dispatch('close')` → `onclose?.()`; `function close() { dispatch('close') }` → `function close() { onclose?.() }`.

- [ ] **Step 2: `VMDetail.svelte` — parent call site (+ its confirm backdrop + DOM `on:`)**

- Rename VMDetail's bare DOM `on:xxx`→`onxxx`; its confirm backdrop `on:click|self={() => (confirmingDelete = false)}` (line ~128) → `onclick={(e) => e.target === e.currentTarget && (confirmingDelete = false)}` + `on:keydown` rename.
- Line ~143: `<SnapshotModal vmId={vm.vm_id} on:close={() => (showSnapshot = false)} on:created={onSnapshotCreated} />` → `<SnapshotModal vmId={vm.vm_id} onclose={() => (showSnapshot = false)} oncreated={onSnapshotCreated} />`.
- **`onSnapshotCreated` reads `e.detail.stopAfter` today — update it.** The callback prop passes `{ stopAfter }` as the first arg (no `CustomEvent` wrapper). Change (line ~62):
```js
function onSnapshotCreated(e) {
  // stop_after destroyed the VM — leave the now-dead detail page.
  if (e.detail && e.detail.stopAfter) {
    stopPolling()
    view.set({ name: 'list' })
  }
}
```
to:
```js
function onSnapshotCreated(payload) {
  // stop_after destroyed the VM — leave the now-dead detail page.
  if (payload && payload.stopAfter) {
    stopPolling()
    view.set({ name: 'list' })
  }
}
```

- [ ] **Step 3: Gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -5`
Expected: 0 errors. `grep -n createEventDispatcher web/src/components/SnapshotModal.svelte` → empty. `grep -n 'event.detail\|\.detail' web/src/components/VMDetail.svelte` → empty (no stale detail access).

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/VMDetail.svelte web/src/components/SnapshotModal.svelte
git commit -m "refactor(web): VMDetail/SnapshotModal -> event attrs + callback props (created payload)"
```

---

### Task 6: Snapshots + RestoreModal (callback props)

**Files (Modify):** `web/src/components/Snapshots.svelte`, `web/src/components/RestoreModal.svelte`

**Interfaces:**
- Produces: `RestoreModal` callbacks `onclose`, `onrestored` (no args). Parent `Snapshots` passes them.

- [ ] **Step 1: `RestoreModal.svelte`** (already runes) — DOM `on:`→`on`; backdrop `on:click|self={close}`→`onclick={(e) => e.target === e.currentTarget && close()}` + `on:keydown` rename; remove `createEventDispatcher`/`const dispatch`; add `onclose, onrestored` to `$props()`; `dispatch('restored')`→`onrestored?.()`, `dispatch('close')`→`onclose?.()`.

- [ ] **Step 2: `Snapshots.svelte`** — rename its bare DOM `on:xxx`→`onxxx`; its delete-confirm backdrop `on:click|self={() => (deleteTarget = null)}` (line ~104) → `onclick={(e) => e.target === e.currentTarget && (deleteTarget = null)}` + `on:keydown` rename; line ~100 `<RestoreModal ... on:close={() => (restoreTarget = null)} on:restored={refresh} />` → `onclose={...}` / `onrestored={refresh}`.

- [ ] **Step 3: Gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -5`
Expected: 0 errors. `grep -n createEventDispatcher web/src/components/RestoreModal.svelte` → empty.

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/Snapshots.svelte web/src/components/RestoreModal.svelte
git commit -m "refactor(web): Snapshots/RestoreModal -> event attrs + callback props"
```

---

### Task 7: VMList + SpawnModal (callback props; SpawnModal also → runes)

**Files (Modify):** `web/src/components/VMList.svelte`, `web/src/components/SpawnModal.svelte`

**Interfaces:**
- Produces: `SpawnModal` callbacks `onclose`, `onspawned` (no args). Parent `VMList` passes them.

- [ ] **Step 1: `SpawnModal.svelte` — DOM `on:`→`on` + backdrop `|self` + callback props + `$state` for locals**

`SpawnModal` is the one dispatcher-modal still in LEGACY mode (no `$props` yet). It has **no `export let`** (self-contained) — so the `$props()` destructure is NEW and holds only the callbacks:
- Add `let { onclose, onspawned } = $props()` (near the top of `<script>`).
- Change the import `import { onMount, createEventDispatcher } from 'svelte'` → `import { onMount } from 'svelte'`; delete `const dispatch = createEventDispatcher()`.
- **Because it now uses `$props()` it is in runes mode** — its reactive locals must be `$state`: `let profiles = []` → `let profiles = $state([])`, `let selected = ''` → `$state('')`, `let busy = false` → `$state(false)`, `let result = null` → `$state(null)`, and any other reassigned + template-read `let` (grep `^  let ` in SpawnModal). The gate's `non_reactive_update:error` names each miss — wrap and re-run until 0. (Leave non-reactive `const`/handles as-is.)
- Rename bare DOM `on:xxx`→`onxxx`; backdrop `on:click|self={close}`→`onclick={(e) => e.target === e.currentTarget && close()}` + `on:keydown` rename.
- Replace `dispatch('spawned')`→`onspawned?.()`, `dispatch('close')`→`onclose?.()`.

- [ ] **Step 2: `VMList.svelte`** — rename its bare DOM `on:xxx`→`onxxx`; line ~75 `<SpawnModal on:close={() => (showSpawn = false)} on:spawned={refresh} />` → `<SpawnModal onclose={() => (showSpawn = false)} onspawned={refresh} />`.

- [ ] **Step 3: Gate (watch for SpawnModal `non_reactive_update`)**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -8`
Expected: 0 errors. If `` `x` is updated, but is not declared with `$state(...)` `` appears for a SpawnModal local, wrap it in `$state()` and re-run. `grep -n createEventDispatcher web/src/components/SpawnModal.svelte` → empty. `grep -n 'export let\|on:' web/src/components/SpawnModal.svelte` → empty (fully runes + no on:).

- [ ] **Step 4: Commit**

```bash
cd /data/projects/claude-zone/anvil
git add web/src/components/VMList.svelte web/src/components/SpawnModal.svelte
git commit -m "refactor(web): VMList/SpawnModal -> event attrs + callback props (SpawnModal to runes)"
```

---

### Task 8: Promote the gate, prove removal, rebuild uidist, smoke

**Files (Modify):** `web/package.json` (gate), `cmd/goose-daemon/uidist/*` (rebuilt)

- [ ] **Step 1: Prove the event idiom is fully gone**

Run:
```bash
cd /data/projects/claude-zone/anvil/web
grep -rnE 'on:[a-zA-Z]+' src --include='*.svelte' || echo "NO on: directives remain"
grep -rn 'createEventDispatcher' src --include='*.svelte' || echo "NO createEventDispatcher remains"
```
Expected: both print the "NO ... remain" line (zero matches). If any remain, they belong to an earlier task — fix there.

- [ ] **Step 2: Promote `event_directive_deprecated` to a failing error in the gate**

In `web/package.json`, change the `check` script's `--compiler-warnings` list from `"non_reactive_update:error"` to `"non_reactive_update:error,event_directive_deprecated:error"`:
```json
    "check": "svelte-check --tsconfig ./jsconfig.json --output human --compiler-warnings \"non_reactive_update:error,event_directive_deprecated:error\""
```

- [ ] **Step 3: Whole-tree check with the tightened gate**

Run: `cd /data/projects/claude-zone/anvil/web && npm run check 2>&1 | tail -6`
Expected: **0 errors** (no `event_directive_deprecated` since all `on:` are gone; no `non_reactive_update`). Remaining warnings are only the benign `state_referenced_locally` (~10, from #87) — these do NOT fail the gate. If any `event_directive_deprecated` error appears, an `on:` was missed — fix it.

- [ ] **Step 4: Build + rebuild the embedded uidist + Go build**

Run:
```bash
cd /data/projects/claude-zone/anvil/web && npx vite build 2>&1 | tail -4
cd /data/projects/claude-zone/anvil && go build ./cmd/goose-daemon && echo "daemon builds"
```
Expected: vite build succeeds (writes `cmd/goose-daemon/uidist/`); Go daemon builds with the rebuilt embed.

- [ ] **Step 5: Manual `/ui` smoke (record results in the report)**

Serve the UI (`cd web && npm run preview`, or the daemon) and confirm no console errors and unchanged behavior:
- Each modal opens and closes THREE ways: backdrop click (the inlined `|self`), Escape key, and the ✕/Cancel button — for SpawnModal, CreateFlockModal, AddAgentModal, ChangeRoleModal, SendTaskModal, BroadcastModal, SnapshotModal, RestoreModal, SystemPromptModal, BuiltinsModal, ProfileModal.
- Each domain callback fires and refreshes the parent: spawn a VM (→ VMList refresh), create a snapshot (→ VMDetail `onSnapshotCreated`, confirm the `{stopAfter}` path), create a flock (→ Flocks refresh), add an agent / change a role (→ FlockDetail refresh), restore a snapshot (→ Snapshots refresh), broadcast, send a task.
- Login form submit (the inlined `|preventDefault`) — the page must NOT navigate/reload.
- The confirm-dialog backdrops (FlockDetail remove/restart/delete, VMDetail delete, Snapshots delete) close on backdrop click.
Record pass/fail per area; any behavior change is a defect to fix.

- [ ] **Step 6: Commit gate + uidist**

```bash
cd /data/projects/claude-zone/anvil
git add web/package.json cmd/goose-daemon/uidist
git commit -m "build(web): promote event_directive_deprecated to gate error + rebuild uidist (event idiom done)"
```

---

## Notes for the executor

- **`npm run check` is the safety net for classes 1 & 3 signatures** (a bad event attribute or a `$props`/binding error → error) but NOT for `createEventDispatcher` removal (no warning) — use the grep asserts.
- **The `|self` inline is exact:** `e.target === e.currentTarget` fires only when the click is on the element itself (the backdrop), never a bubbled child click — identical to Svelte's `|self`.
- **Do NOT** touch `$store`/stores, template structure, or the ~11 non-migrated (no-event) runes concerns. Do NOT run `sv migrate`.
- **uidist hygiene:** only Task 8 commits `uidist`. If an intermediate `vite build` (none needed before Task 8) dirties it, `git checkout -- cmd/goose-daemon/uidist`.
- **Model routing (SDD):** Task 1 (mechanical DOM rename) → cheap. Tasks 2-6 (callback props, coupled) → standard. Task 7 (SpawnModal runes + callbacks) → standard. Task 8 (gate + smoke) → standard. Final whole-branch review → most capable.
- **No Go/KVM impact:** frontend-only; the Go gofmt CI gate is untouched (no `.go` changed except the daemon re-embedding uidist, which is a build artifact).
