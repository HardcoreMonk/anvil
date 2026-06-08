# v0.5.3 — Sizing Presets + Flock Profile Workflow

**Ephemera** v0.5.3 lowers the default agent VM sizing from **2 vCPU / 2048 MiB** to **1 vCPU / 1024 MiB** (the new **Standard** preset) and adds three named sizing tiers — **Light** (1 vCPU / 512 MiB), **Standard** (1 vCPU / 1024 MiB), and **Advanced** (2 vCPU / 2048 MiB) — surfaced in the Settings profile editor. Rationale: Goose runs LLM inference remotely, so an agent VM mostly waits on network IO and runs tools; 2 GB was over-provisioned for chat/file-edit roles. It also reworks the **flock profile workflow** — Agent Groups now pick a profile per role (separate from the role label), that profile survives restart / role-change, the model field becomes a real dropdown, and an in-use profile can no longer be deleted out from under running agents. **No golden-image rebake** — only the daemon, `web/` bundle, docs, the webdev demo, and KVM-free tests change.

---

## What's New

### Lighter default + sizing presets

- The default for an agent VM created without explicit sizing is now **1 vCPU / 1024 MiB** (was 2 / 2048). This is the `vm` package default (`defaultVcpuCount` / `defaultMemSizeMib`) and the reserved `default` profile; existing profiles with explicit sizing are unaffected.
- New `GET /config/presets` returns the named tiers (Light / Standard / Advanced) from a backend registry (mirrors `GET /config/providers`). The **Settings** profile editor shows them as quick-select chips that fill the vCPU/memory fields, with free-form entry still available for custom sizing.
- Tune sizing from the live `GET /vms/{id}/stats` `mem_used_mib` reading: most chat/edit agents sit well under 512 MiB; raise a profile only if an agent runs build/test-heavy tools.

### Accurate restored-VM stats

- Snapshots now record the VM's sizing in metadata (`vcpu_count` / `mem_size_mib`), so a restored VM reports its true `mem_total_mib` instead of a hardcoded 2048. Legacy snapshots (no sizing recorded) fall back to the historical 2 vCPU / 2048 MiB. The memory file still governs the actual restored boot size.

### webdev demo modernized (hybrid Gemini + Groq, preset sizing)

- `webdev_demo.sh` (the 3-agent React + Vite flock demo) is rebuilt for the runtime-profile architecture. Role profiles now live in `configs/webdev-demo/profiles/{orchestrator,worker,reviewer}/` (system.md + goose.yaml); the script installs them into `configs/profiles/` for the run and removes them on cleanup (a same-named user profile is backed up to `{role}.webdev_bak` and restored). It runs **hybrid**: the orchestrator (tool-heavy multi-step loop) on **Google Gemini** `gemini-2.5-flash`, and worker/reviewer on **Groq** `openai/gpt-oss-20b` — with per-role preset sizing **1024 / 512 / 512 MiB (2 GiB total)**, fitting an **8 GiB laptop**; the memory floor drops from 6500 to 3584 MiB. Both API keys live in the global keychain (no per-role secrets). **Why hybrid**: Groq could not reliably drive the multi-turn tool loop — its Llama models reject the tool-call format ("Failed to call a function") and its `gpt-oss` reasoning models reject `reasoning_content` on follow-up tool turns (sometimes emitting the tool command as plain text); neither goose 1.36 nor 1.37 fixed it. Gemini handles multi-turn tool use reliably, while gpt-oss is fine for the worker/reviewer's single-shot generation. Independently, the **Groq provider default** (Settings UI + `configs/goose.yaml`) moves to `openai/gpt-oss-120b` (standard tool-call format, better than Llama for single-shot tool use). Override any demo role with `WEBDEV_ORCH_MODEL` / `WEBDEV_WORKER_MODEL` / `WEBDEV_REVIEWER_MODEL`; the script also re-dispatches the orchestrator up to `WEBDEV_ORCH_ATTEMPTS` times (default 4) and flags partial runs (unfilled PLACEHOLDER files).

### Flock role ↔ profile separation

- Creating an Agent Group now takes a **free-text role label and a profile** per agent. Previously the role string was overloaded as the profile name, so a role that didn't match a profile directory silently fell back to the default config. `POST /flocks` accepts an optional `profiles[]` array parallel to `roles[]` (empty entry → the role name is the profile, back-compat); each agent's profile is recorded on `AgentInfo.Profile`.
- **Fix:** restart / change-role / add-agent now respawn with the agent's **recorded profile** instead of re-deriving it from the role label — so a restarted agent keeps its sizing, model, and system prompt instead of falling back to default. Legacy flock records (no stored profile) fall back to the role name. The Change-role and Add-agent modals gained the same profile dropdown.

### Model input dropdown

- The model field in the New-profile modal and Settings was an `<input list>` + `<datalist>` (a type-to-filter bubble, unlike a normal dropdown). It is now a shared **`ModelPicker`** component: a `<select>` of the provider's suggested models plus a **"Custom model…"** option that reveals a free-text input for any model id.

### Profile delete guard

- Deleting a profile that running VMs were spawned from is now **refused (HTTP 409)** instead of silently leaving those agents to fall back to the default config on their next restart / role-change. The Settings UI shows an i18n notice (`settings.profileInUse`); remove or re-profile the agents first.

> **No rebake**: this release changes only the daemon (config / flock / orchestrator handlers), `web/` (and the regenerated `uidist/` bundle), docs, KVM-free Go tests, and the webdev demo (`webdev_demo.sh` + `configs/webdev-demo/`) — no `cmd/goose-agent`, `scripts/build_image.sh`, or `artifacts/*` changes, so the golden image is untouched. Snapshot restore sizing is exercised by the e2e/KVM gate; the preset registry/handler, snapshot-metadata round-trip, and default-sizing resolver are covered by KVM-free unit tests.

---

# v0.5.2 — Orchestration Web UI + Live Activity Feed (SSE)

**Ephemera** v0.5.2 adds the **Orchestration** console — browser-based Agent Group (flock) management — and a **live Activity Feed** over Server-Sent Events. Pure frontend: every flock endpoint already shipped in v0.4.x, so there are **no control-plane code changes and no golden-image rebake** (UI bundle + docs + tests only).

---

## What's New

### Orchestration console (Agent Groups)

- A new **Orchestration** screen lists Agent Groups (`GET /flocks`, 4s poll, newest-first) and creates them (`POST /flocks` — a task plus one role per VM; the one-time `agent_token` for each agent is shown once and not stored server-side).
- **Group detail** (`GET /flocks/{id}`, live poll) shows the agent table with per-agent **restart** / **remove** / **change role**, group **pause** / **resume**, **add agent**, **broadcast** a prompt to every member (per-agent sent / busy / failed results), and **delete group** — destructive actions behind confirm modals.

### Live Activity Feed (SSE)

- Group detail embeds an **Activity Feed** that streams the Town Wall live via `GET /flocks/{id}/wall` (Server-Sent Events): the full history replays on connect, then new messages append in real time. A composer posts to the feed (`POST /flocks/{id}/post`).
- Because `EventSource` cannot send the `Authorization` header, the feed reads the SSE stream over `fetch` (bearer-injected by `apiFetch`) with a hand-written `data:`-frame parser (`streamSSE` in `src/lib/stream.js`); it aborts on unmount and offers a manual **Reconnect** when the stream ends.

### Korean localization

- New `orchestration` / `createFlockModal` / `flockDetail` / `activityFeed` / `broadcastModal` / `changeRoleModal` / `addAgentModal` namespaces (오케스트레이션 / 에이전트 그룹 / 액티비티 피드 / 브로드캐스트), plus agent status terms (생성 중 / 준비됨 / 완료 / 중단됨 / 일시중지).

> **No rebake**: this release changes only `web/` (and the regenerated `uidist/` bundle), KVM-free Go tests, and docs — no `cmd/goose-agent`, `scripts/build_image.sh`, or `artifacts/*` changes, so the golden image is untouched. Spawn-dependent flows (create group / add agent / change role / restart / broadcast) are verified through the e2e/KVM gate; list / get / delete / pause / resume / post / wall-history and the SSE framing contract are covered by KVM-free handler tests.

---

# v0.5.1 — Per-Profile Sizing & Goose Stability

**Ephemera** v0.5.1 extends the v0.5.0 Web UI with **profile creation + per-VM sizing** and **snapshot/restore screens**, and hardens `goose-agent` for tight-budget and reasoning LLMs. Additive — the control-plane API adds `POST`/`DELETE /config/profiles` and `GET /config/providers`; existing routes are unchanged. **Golden-image rebake**: `cmd/goose-agent` changes (token cap, extension trim, `/nothink`, latest-turn output), so the daemon rebuilds the golden image on first start after the change.

---

## What's New

### Profile creation + per-VM vCPU/memory

- `POST /config/profiles` creates a user-defined profile (name + provider/model + optional vCPU/memory); `DELETE /config/profiles/{name}` removes one; `GET /config/providers` lists known providers and which have a keychain API key. The static `configs/profiles/*` role examples are removed — profiles are now created at runtime through the **Settings** UI.
- Per-VM sizing is stored as `EPHEMERA_VCPU_COUNT` / `EPHEMERA_MEM_SIZE_MIB` keys in the profile's `goose.yaml` (goose ignores them) and applied at spawn; unset falls back to the default **2 vCPU / 2048 MiB**. Groq's discontinued `mixtral-8x7b` is dropped from suggestions.

### Snapshot / restore Web console

- The console gains a **Snapshots** screen (`GET /snapshots` — type/base/created) with **restore** (`POST /snapshots/{id}/restore`) and delete behind confirm modals, plus a **Snapshots** section on **VM detail** to capture Full/Diff snapshots (optionally stop-after). The snapshot/restore control-plane API itself is unchanged.

### goose-agent hardening

- Loads **only** the `developer` builtin extension (`--no-profile --with-builtin developer`) and caps `GOOSE_MAX_TOKENS`, so a single request fits tight provider token-per-minute budgets — e.g. Groq's free tier, where the full default toolset otherwise overflows and the 413 rate-limit is misreported as a context overflow.
- Returns **only the latest turn's reply**, slicing goose's whole-transcript `--resume` output to the last user message — fixing the multi-turn "accumulating output" bug; the UI's brittle prefix-strip is removed.
- Prepends **`/nothink`** for qwen reasoning models, since goose replays their `reasoning_content` on resume and Groq rejects it with a 400.

### Korean UI vocabulary

- `ko.json`: 프로필 → **프로파일**, the Provider label → **프로바이더**, and snapshot wording (e.g. "스냅샷을 만든 뒤 원본 VM을 제거합니다").

### Stability

- Restore's `waitForAgent` budget is raised **30s → 60s** to match the spawn/recovery boot paths, fixing a concurrent-restore flake (two 2 GB VMs restored at once) under host memory pressure.

> **Caveats**: per-VM sizing applies to UI-created profiles and the spawn path — built-in flock roles keep their canonical sizing. The qwen `/nothink` workaround is partial: very long multi-turn reasoning-model sessions can still hit a `reasoning_content` 400 — start a fresh conversation, or use a non-reasoning model for heavy multi-turn.

---

# v0.5.0 — Web UI

**Ephemera** v0.5.0 adds a **browser console** served by the daemon itself, replacing the script-form external client for everyday use. The existing control-plane API is unchanged and fully backward compatible; the only new server routes are `/ui/` (the embedded UI) and `/config/profiles` (profile model/provider editing). **Golden-image rebake**: `cmd/goose-agent` gains optional goose-session support, so the daemon rebuilds the golden image on first start after the change.

---

## What's New

### Embedded Web console (`/ui/`)

- The daemon serves a Svelte + Vite single-page app at **`/ui/`** on the same address as the API (`go:embed`, same origin — no CORS). The build output (`cmd/goose-daemon/uidist/`) is committed, so `go build` needs no Node toolchain; rebuild only after editing `web/` (`cd web && npm run build`). `/ui/` is mounted **outside** the auth/audit chain (the login page + JS bundle must load token-free; the bundle has no secrets), while every data call still flows through Bearer auth. `/` redirects to `/ui/`.
- Screens: token **Login** (auto-skipped when auth is disabled), **VM list** (live stats + model), **Create VM** (profile dropdown, one-time `agent_token`), **VM detail** (live stats + a cancelable streaming conversation panel), **Settings** (per-profile model/provider).

### English / Korean localization

- All UI strings are externalized to `web/src/locales/{en,ko}.json` and rendered via `svelte-i18n`. The initial language follows the browser (`ko*` → Korean, else English); a nav toggle switches and persists the choice in `localStorage`. Server-originated error text is shown verbatim.
- UI vocabulary is generic IT (display labels only — API identifiers unchanged): **Platform Agent** (in-VM goose agent), **Agent Group** (flock), **Activity Feed** (Town Wall), **Create/Delete** (spawn/destroy).

### Profile model/provider editing (`/config/profiles`)

- `GET /config/profiles` lists each profile (`default` + `configs/profiles/*`) with its `provider`/`model`; `PUT /config/profiles/{name}` rewrites `GOOSE_PROVIDER`/`GOOSE_MODEL` in place — comments and the `extensions:` block are preserved, values are validated against newline injection, and **API keys (`goose-secrets.yaml`) are never read or written** through this surface. The Settings screen drives both.
- Config is injected at spawn, so an edit applies to the **next** VM from that profile; already-running VMs keep their spawn-time model. `VMInfo` now records each VM's `provider`/`model` (returned by `GET /vms`, shown in the UI) so the distinction is visible.

### Multi-turn agent conversation

- `POST /vms/{id}/tasks` accepts an optional `session`. With it, `goose-agent` runs `goose run --output-format json -n <session> [--resume] -i -` — the first turn creates the named session, later turns `--resume` it — so the agent keeps conversation context across turns. Omitting `session` preserves the original stateless one-shot behavior (`ephemera-ctl`, `gtcall`, depth-guard tests unaffected). The control plane proxies the request body verbatim, so no control-plane change was needed.

### Graceful VM delete; "stop agent" removed

- `DELETE /vms/{id}` now best-effort asks the in-VM agent to shut down cleanly (`POST /stop`, 2 s) before force-stopping Firecracker, then frees TAP/IP/disk and deregisters. The old UI "stop agent" action — which actually halted the whole guest (the agent is the VM's init) while leaving the VM registered and the stats poller spamming the log — was removed; **Delete is the single teardown**. The stats agent-busy probe-failure log was demoted to Debug so a down/halted VM can no longer spam.

> **Caveats**: the Web UI conversation transcript is in-memory (a page reload starts a fresh `session`; the goose session persists in the VM but is not re-displayed). `scripts/build_image.sh` now removes any stale tarball before download, hardening the golden-image build against a leftover at the fixed `/tmp` path.

---

# v0.4.5 — Snapshot-Restore Auto-Recovery

**Ephemera** v0.4.5 closes a recovery gap from the Known Limitations audit. Additive — no wire format changed; no golden-image rebake (daemon/storage only).

---

## What's New

### Snapshot-restored VMs are auto-recovered across a daemon restart

- A VM created via `POST /snapshots/{id}/restore` now persists a `state.json` carrying `source_snapshot_id`. On the next daemon start, `RecoverVMs` **re-restores it from that source snapshot** (it cannot cold-boot like a spawn VM) instead of dropping it — automating what previously required a manual re-restore.
- Semantics: the VM returns to its **snapshot-time** memory and disk. Writes made *after* the original restore are not preserved across the restart (identical to a manual re-restore); the COW exception store is recreated fresh so the re-loaded snapshot memory and disk stay consistent.
- Shutdown handling: graceful shutdown discards a restored VM's dm device + transient exception store but **keeps its `state.json`** (recovery re-restores fresh). Restored VMs are excluded from the opt-in memory auto-snapshot (they re-restore from source, so an `auto/` image would never be used).
- Caveats (documented, by design): **bind-mount-fallback** restores (when dm-snapshot tooling is unavailable) are not auto-recovered; and if the **source snapshot was deleted** while the restored VM ran, recovery drops the VM and surfaces it (via `failed[]` / a flock agent marked dead) rather than silently keeping it.

### Known-Limitations refresh

- Removed the "Snapshot-restored VMs are not auto-recovered" limitation (now resolved above).
- Reworded the CP-token-rotation limitation to "CP token hot-rotation requires `_TOKENS_FILE`": the old "needs v0.3.4 VMs" clause is obsolete — the golden image auto-rebakes on any `goose-agent` change, so every current VM carries the `SET_CP_TOKEN` vsock handler. The only real requirement is sourcing tokens from a file (env tokens are fixed at exec and cannot change on SIGHUP).

---

# v0.4.4 — Feature Extensions

**Ephemera** v0.4.4 is the last single-host cycle, rounding out the operator surface: **PR-A** flock broadcast + a watchdog status route; **PR-B** streaming `/tasks`, the `goose-agent` slog migration, and a nested-invocation depth guard. Additive — no wire format changed.

---

## What's New

### Flock broadcast (PR-A)

- `POST /flocks/{id}/broadcast` `{"body":"…"}` scatters one prompt to **every** member agent's `/tasks` endpoint in parallel and gathers each agent's result (scatter-gather). The response carries a `sent`/`skipped`/`failed` tally plus a per-agent `results` map (`status` = `ok`/`busy`/`error`, with `output`/`error`). Agents already running a task answer `503` and are reported `busy` (skipped); unreachable agents are `error`. The call blocks until every agent finishes, like calling `/tasks` on each — cancellation rides on the request context.
- The broadcast is also recorded once on the Town Wall (`orchestrator` author) so observers see it happened.
- `ephemera-ctl flock broadcast <flock_id> <message>` wraps the endpoint (trailing words are joined, so the message need not be quoted).

### Watchdog status route (PR-A)

- `GET /watchdog/status` exposes the health watchdog's tunables (`interval_sec`, `timeout_sec`, `dying_threshold`, `auto_heal`) and live per-VM state (`vm_fail_counts` — VMs with a non-zero consecutive-failure count; `vm_dead_marked` — VMs the watchdog has marked dead). Read-only, behind the same auth as the other internal routes. The watchdog has run since v0.3.4 but had no status route until now; the snapshot is taken under the same lock the polling loop uses, and the returned maps are copies.

### Streaming `/tasks` (PR-B)

- `POST /vms/{id}/tasks?stream=1` streams the task as **newline-delimited JSON** over chunked transfer instead of buffering the whole result: zero or more `{"type":"progress","text":"…"}` frames (relayed from goose's stderr activity, plus a 15s heartbeat) followed by exactly one `{"type":"result","output":"…","error":"…"}` frame that mirrors the legacy `TaskResult`. The default (no `stream=1`) path is **unchanged** — full backward compatibility. The control-plane proxy now flushes per chunk (the same `http.Flusher` plumbing the Town Wall SSE stream relies on), so the stream reaches the caller incrementally.
- **Caveat**: streaming commits a `200` before goose runs, so a goose failure can no longer be a `500` — the error rides in the `result` frame's `error` field. Streaming clients must inspect `result.error`, not the status code. The buffered path keeps its `500`.

### Nested-invocation depth guard (PR-B)

- Agent→agent dispatch (`gtcall`) is now loop-guarded. The control plane reads `X-Ephemera-Task-Depth` on every proxied `/tasks` hop (absent → 0), refuses a hop at/over `EPHEMERA_MAX_TASK_DEPTH` (default 5) with **`508 Loop Detected`**, and forwards `depth+1`. `goose-agent` injects the incoming depth into the goose subprocess environment (`EPHEMERA_TASK_DEPTH`), and `gtcall` re-sends it as the header, so depth accumulates across the whole nested call tree. Distinct from the agent's own `503` busy response.

### `goose-agent` slog migration (PR-B)

- The in-VM `goose-agent` moved off the plain `log` package to `log/slog`, mirroring the host daemon's `EPHEMERA_LOG_FORMAT` (text|json) / `EPHEMERA_LOG_LEVEL` handling (default level Warn). Completes the slog migration begun in v0.3.5.

> **Golden-image rebake**: PR-B edits `cmd/goose-agent` and `scripts/gtcall`, so the daemon rebuilds the golden image on first start after the change (mtime check in `EnsureGoldenImage`).

---

# v0.4.3 — Flock Lifecycle

**Ephemera** v0.4.3 makes flocks adjustable at runtime: **PR-A** dynamic agent membership (add/remove/role-change), **PR-B** flock pause/resume + per-flock agent cap, **PR-C** Town Wall history filters + log rotation. Additive — no wire format changed.

---

## What's New

### Dynamic flock agent management (PR-A)

- `POST /flocks/{id}/agents` `{"role":"worker"}` — spawn and attach a new agent. The `agent_id` follows the per-role `role-N` rule (max existing N + 1) and the one-time `agent_token` is returned. The 20-agent-per-flock cap is enforced.
- `DELETE /flocks/{id}/agents/{agent_id}` — tear down the agent's VM and remove it from the flock. Removing the last agent leaves an empty flock (recoverable via add; use `DELETE /flocks/{id}` for the whole flock).
- `PATCH /flocks/{id}/agents/{agent_id}` `{"role":"reviewer"}` — change an agent's role. Because role binds VM sizing + system prompt at spawn time, the VM is recreated under the new role (`agent_id` and `agent_token` preserved, like restart).
- `ephemera-ctl flock add-agent`/`rm-agent`/`set-role` wrap the three endpoints.
- Each membership change is posted to the Town Wall; the watchdog auto-discovers added VMs and forgets removed ones.

### Flock pause/resume + per-flock max_agents (PR-B)

- `POST /flocks/{id}/pause` / `POST /flocks/{id}/resume` — pause/resume **all** member VMs via Firecracker PauseVM/ResumeVM. **Runtime-only**: agent status flips to `paused`/`ready` and `Flock.Paused` toggles, but nothing is persisted (a daemon restart brings members back running). A partial pause failure rolls back (resumes already-paused members).
- The health watchdog **skips dead-marking `paused` agents** — a paused VM intentionally doesn't answer `/health`, so it must not be marked dead.
- `POST /flocks` accepts `max_agents` — a per-flock agent cap (default 20), enforced on create **and** on `POST /flocks/{id}/agents`. Persisted in metadata (backward-compatible; an absent/0 value falls back to the default).
- `ephemera-ctl flock pause`/`resume`; `flock create --max-agents N`.

### Town Wall query filters + log rotation (PR-C)

- `GET /flocks/{id}/wall/history` accepts filters: `agent_id` (exact), `since`/`until` (RFC3339, inclusive), `contains` (body substring). Combinable; an all-empty filter returns the full history.
- The Town Wall log now **rotates by size**: past `EPHEMERA_TOWNWALL_MAX_MIB` (default 10) the active log shifts to `.1` (…→`EPHEMERA_TOWNWALL_KEEP`, default 3) and a fresh file continues. `History` reflects the active file (rotated backups stay on disk, mirroring the audit log).

---

# v0.4.2 — COW Spawn by Default

**Ephemera** v0.4.2 promotes copy-on-write spawn disks to the default. New VMs now get a dm-snapshot view of the golden image instead of a 700 MiB full `io.Copy` clone — measured ~43% faster spawn (1.96s → 1.12s warm) with ~0 MiB initial per-VM disk. The daemon probes dm-snapshot support at startup and **auto-falls back to a full clone** when `losetup`/`dmsetup`/`dm_snapshot` are unavailable, so hosts without device-mapper keep working. Opt out explicitly with `EPHEMERA_DISK_MODE=plain` (or `full`). Additive — no wire format changed.

---

## What's New

### COW spawn rootfs is the default

- `EPHEMERA_DISK_MODE` now defaults to `cow`. The previous behavior (full byte-for-byte copy) is selected with `EPHEMERA_DISK_MODE=plain` or `full`.
- New `storage.DMSnapshotAvailable()` probes the host once at startup (tool presence + a `dmsetup version` device-mapper round-trip + the `dm_snapshot` module); on failure the daemon logs a warning and uses a full clone. The strategy is resolved once into `ControlPlane.useCOW` instead of re-reading the env on every spawn.
- Recovery is unaffected: each VM's `DiskMode` is recorded from the actual provisioning result, so plain and COW VMs both cold-restart correctly.
- COW spawn VMs now support **Diff snapshots**: `WriteRootfsDiff` reads the rootfs size via `blockdev` when the current rootfs is a dm-snapshot block device (whose `Stat().Size()` is 0), so a COW VM's 2nd-and-later snapshot is a sparse rootfs diff. Previously this hit a size-mismatch — the COW+Diff combination was untested while COW was opt-in.

---

# v0.4.1 — Operational Interfaces

**Ephemera** v0.4.1 makes the daemon operable as a service: authenticated **client identity** threaded into request handling, a per-request **access audit log** (`GET /audit`), **per-token TTL/rotation**, and a dependency-free **operator CLI** (`ephemera-ctl`). Additive — no wire format changed; the only behavior changes are that an expired token is now rejected (401) and the in-VM CP token is the first non-expired client.

---

## What's New

### Client identity in request context (F1)

- `authMiddleware` now surfaces the authenticated caller: the matched client name is threaded to handlers via the request context and to the (outer) audit middleware via a request-scoped holder. Timing-safety is preserved — every token is still compared with no early-exit, and the expiry check runs after the constant-time loop.

### Access audit log — `GET /audit` (F2)

- Every API request is appended as one JSON line to `{workDir}/audit/access.jsonl`: `{ts, client, method, path, status, duration_ms, remote_addr, bytes}`. The record **never contains tokens, the `Authorization` header, request/response bodies, or the query string**. Unauthenticated requests record `client="-"`; `/metrics` is excluded so scrapes don't flood the log.
- The file is size-rotated (`EPHEMERA_AUDIT_MAX_MIB`, default 100; `EPHEMERA_AUDIT_KEEP`, default 5). On by default; `EPHEMERA_AUDIT_DISABLE=true` turns it off.
- `GET /audit?limit=&client=&status=&method=` returns recent records as a JSON array (newest first; limit default 100, max 1000), itself authenticated.
- A new `statusRecorder` ResponseWriter wrapper captures status/bytes and **forwards `http.Flusher`** so the SSE Town Wall stream keeps working; the audit middleware wraps auth (outer) so it also records 401s and the final status.

### Per-token TTL + rotation (F3)

- Token entries gain an optional expiry: `name:token:expires` (RFC3339 or Unix seconds). A two-field `name:token` never expires (backward compatible). Tokens may contain `:` — the expiry is recognized only when the trailing colon-separated field parses as a timestamp.
- Expiry is enforced per request: an expired-but-matched token returns 401 (same body as an unknown token; distinguished only by the server log and `ephemera_auth_total{outcome="expired"}`). No background reaper.
- The in-VM control-plane token is now the **first non-expired** client (was blindly `clients[0]`), so an expired primary no longer breaks in-VM `/townwall/post`; if all tokens have expired, an empty (unauthenticated) token is propagated with a warning.
- New metric `ephemera_auth_total{outcome=ok|denied|expired}`; startup/SIGHUP banners log expired / expiring-24h counts.

### Operator CLI — `ephemera-ctl` (F4)

- New `cmd/ephemera-ctl`, a stdlib-only HTTP wrapper over the control-plane API: `vm spawn/ls/rm/health/stop/task/stats/snapshot`, `flock create/ls/get/rm/post/wall/restart`, `snapshot ls/restore/rm`, `audit`, and `metrics`. Built with `go build -o ephemera-ctl ./cmd/ephemera-ctl/`.
- Reads `EPHEMERA_CTL_URL` (default `http://127.0.0.1:3000`, never derived from the `0.0.0.0` bind addr) and a bearer from `--token` / `EPHEMERA_CTL_TOKEN` / `EPHEMERA_API_TOKEN`. Human-readable tables by default; `--json` for raw output. Non-2xx → server error to stderr + non-zero exit, so it composes in scripts.

### Tests

- Unit: token TTL parsing (2/3-field, colon-in-token ±expiry, RFC3339-with-internal-colons), `parseExpiry`, `firstActiveClient`, `countTokenExpiry`; `authMiddleware` outcomes + metric + no-early-exit; audit rotation / tail-filters / no-secret-leak; `statusRecorder` Flusher forwarding; client-identity context round-trip; CLI client round-trip (Bearer present/absent, non-2xx error, body encoding) + URL/token resolution + flag parsing.
- e2e steps 78–83: audit records an authenticated request (no token leak), audit captures a 401 (`client=-`), per-token TTL expiry → 401 while the primary still works, the SSE Town Wall stream survives the audit wrapper, and `ephemera-ctl` drives spawn/ls/rm + audit against the live daemon (bogus token → non-zero exit).

---

# v0.4.0 — Storage / Recovery Core

**Ephemera** v0.4.0 hardens the storage and recovery paths. Two headlines: **COW spawn cold-restart** — a VM started with `EPHEMERA_DISK_MODE=cow` now survives a daemon restart with the same identity, closing the v0.3.2 limitation where COW VMs were skipped on recovery — and opt-in **memory auto-snapshot** (`EPHEMERA_AUTOSNAPSHOT=true`), which warm-restores a VM's in-flight memory across a graceful daemon bounce instead of cold-booting. It also adds true rootfs diff snapshots, a disk-space pre-flight, orphan-device reclaim, atomic spawn-failure rollback, and a Firecracker signal-forwarding fix. Additive: no wire format changed.

---

## What's New

### COW spawn cold-restart (item A)

- A COW VM's writes accumulate in a sparse dm-snapshot exception store (`<workspace>/<vm_id>.cow`). On graceful shutdown, `DestroyAll` now releases the dm-snapshot kernel objects (mount, dm device, loop devices) via the new `TeardownDMSnapshotKeepStore` but **preserves the exception store + `state.json`**. On the next start, `RecoverVMs` re-layers that store over the golden image (`SetupDMSnapshot`) and cold-restarts the VM — same `vm_id`, IP, TAP, MAC, and agent token.
- A crashed previous run can leave a live `cow-<vm_id>` dm device for a VM whose `state.json` survived; recovery clears it first with the new store-preserving `ReclaimCOWDeviceKeepStore` before re-creating the snapshot, so `dmsetup create` never collides.
- The graceful-shutdown preserve-vs-teardown decision keys on `storage.VMStateExists`: only state-backed spawn VMs are preserved. Snapshot-restored VMs (`POST /snapshots/{id}/restore`) persist no `state.json` and their base is a snapshot copy, not the golden image, so they remain out of scope and are torn down fully.
- **Assumption:** the golden image is unchanged between spawn and restart. A golden-image rebuild invalidates the block-level COW layering; a missing store is logged and the VM boots from the pristine golden image (writes lost) rather than failing recovery.

### True rootfs diff snapshots (item C)

- A diff snapshot now stores only the **changed rootfs blocks** (`rootfs.diff` — a sparse 4 KiB-block delta vs its base, computed by `WriteRootfsDiff`) instead of a full ~570 MB `rootfs.ext4` copy, mirroring the existing memory diff (a few MB vs a full copy). On restore, `MergeRootfsDiff` overlays the delta onto the base's full rootfs into a transient `merged.ext4` (under `{workDir}/tmp`) that becomes the dm-snapshot read-only origin; it is unlinked right after `losetup` (blocks freed at VM destroy) and never placed on `/dev/shm`.
- Full snapshots now copy the rootfs with `cp --reflink=auto --sparse=always` — an instant CoW clone on btrfs/XFS, and a sparse copy that skips the golden image's ~22% holes on ext4.
- **Backward-compatible:** restore keys the rootfs-merge branch on a new `rootfs_diff_path` metadata field, so existing full snapshots and pre-v0.4.0 diff snapshots (which carry a full `rootfs.ext4`) restore byte-identically. A diff's base is always a full snapshot (no diff chains); the existing 409-on-delete dependency guard protects the base.

### Storage hardening (items F + E)

- **Disk-space pre-flight** — clone/snapshot operations check free space against `EPHEMERA_DISK_MIN_FREE_MIB` (default 1024) and return `507 Insufficient Storage` instead of failing mid-write.
- **Orphan COW inventory** — `RemoveOrphanCOWDevices` runs in the recovery preamble and reclaims dm-snapshot/loop devices + exception stores left by a crashed run whose `state.json` did not survive.

### Memory auto-snapshot (item B)

- New opt-in `EPHEMERA_AUTOSNAPSHOT=true`: on **graceful** shutdown, `DestroyAll` snapshots each recoverable VM's live memory+state into `vms/<id>/auto/{memory.bin,state.bin}` (via the extracted `snapshotVMMemory` helper — `PauseVM` + `CreateSnapshot`) *before* `StopVMM`. On the next start, `RecoverVMs` **warm-restores** from that snapshot with `vm.RestoreMachine` (memory preserved) instead of cold-booting, reusing the **same** `vm_id`/IP/TAP/MAC/token so the guest network state baked into the snapshot stays valid (no `ReconfigureGuestIP`).
- The snapshot is **one-shot**: deleted after every restore attempt so a later bounce never rolls the VM back to a stale image; the next graceful shutdown writes a fresh one. It is **best-effort**: a failed snapshot, restore, or post-restore agent handshake logs and falls back to the existing cold boot (plain rootfs clone or COW store re-layer), so warm restore is strictly additive. A COW VM's dm-snapshot is set up once and shared by both paths, so a warm-restore failure falls through to cold boot without a `dmsetup` collision.
- **Limits (documented):** warm restore requires a graceful shutdown — a SIGKILL/crash runs no `DestroyAll`, so the VM cold-boots as before. Auto-snapshot has no disk pre-flight (best-effort); a full memory image is ~VM RAM per recoverable VM (a 5-agent flock ≈ 10 GB), hence opt-in. New metrics `ephemera_auto_snapshot_total{outcome}` and `ephemera_auto_restore_total{outcome}`.

### Partial spawn / recovery failure rollback (item D)

- **Spawn rollback** — `spawnVMInternal` now unwinds partial allocations through a single deferred LIFO cleanup stack, armed as each resource (network, then disk) is acquired, replacing four duplicated inline rollback blocks. It disarms (`committed = true`) the instant the VM is registered in `cp.vms`, after which `destroyVM` owns cleanup — so a future early-return added mid-spawn can no longer leak a TAP/IP or disk clone.
- **Recovery artifacts-missing** — when a VM's `state.json` is present but its rootfs has vanished, `RecoverVMs` now releases the stale host TAP the prior run left, drops the state + any orphaned `auto/` snapshot, marks the flock agent `dead`, and surfaces the VM in the `failed` list — instead of silently dropping it. `destroyVM` also clears `auto/` so an explicit delete leaves no orphaned memory image.

### Firecracker signal-forwarding fix

- `internal/vm/machine.go`'s `forwardSignals` now omits `SIGINT` and `SIGTERM` (in addition to the v0.3.4 `SIGHUP` omission), forwarding only `SIGQUIT`/`SIGABRT` to Firecracker children. The daemon already traps `SIGINT`/`SIGTERM` and stops each child explicitly via `StopVMM`, so forwarding was redundant — and it would race the item B auto-snapshot, killing a child mid-`CreateSnapshot`. `SIGQUIT`/`SIGABRT` stay forwarded because the daemon does not trap them (forwarding then prevents orphaned children on an abnormal exit).

### Tests

- e2e steps **40–46** exercise COW graceful cold-restart (store preserved, dm device re-created, same `vm_id`, `/health` 200), a SIGKILL crash that reclaims an orphaned COW device while cold-restarting the survivor, and restoration of plain disk mode.
- e2e step **26b** asserts a diff snapshot's `rootfs.diff` is a sparse delta far smaller than the full rootfs; diff-restore steps (28–29) exercise the rootfs merge end-to-end. New `internal/storage` unit tests cover the `WriteRootfsDiff`/`MergeRootfsDiff` round-trip (byte-exact, sparse, size-mismatch guard).
- e2e step **76** exercises memory auto-snapshot: a graceful bounce under `EPHEMERA_AUTOSNAPSHOT=true` writes `auto/{memory,state}.bin`, the next start warm-restores (same `vm_id`, `/health` 200, `vm warm-restored` log), and the one-shot snapshot is deleted — also gating the signal fix (a forwarded SIGTERM would kill Firecracker mid-snapshot). e2e step **77** SIGKILLs the daemon, deletes a flock member's rootfs, and asserts the disk-missing recovery drops state, releases the stale TAP, marks the agent `dead`, and surfaces it via `vms not cold-restarted`.
- New unit tests: `AutoSnapshotDir/Paths/Exists` + `RemoveAutoSnapshot` (storage), `Manager.Release` idempotent on a never-allocated TAP (network), and the `EPHEMERA_AUTOSNAPSHOT` `envBool` mapping (daemon).

---

# v0.3.6 — Autonomous Multi-Agent Webdev Demo

**Ephemera** v0.3.6 turns the multi-agent flock from a spawn primitive into a demonstrated autonomous collaboration: `webdev_demo.sh` stands up an orchestrator + worker + reviewer team that designs, generates, reviews, and publishes a complete React + Vite site — every file authored inside the VMs, with the host acting only as a passive Town Wall harvester. Getting there required three pieces of in-VM tooling, all included here. The release is additive — no wire format changed and the `/tasks` response shape is unchanged; the only behavior change to an existing endpoint is that `/tasks` output is now cleaner and no longer truncated.

---

## What's New

### `webdev_demo.sh` — autonomous 3-agent flock

- A one-shot operator demo (manual gate, like `observability_demo.sh`): preflight → swap per-role `*.webdev.{md,yaml}` overrides → spawn an `orchestrator + worker + reviewer` flock → background SSE harvester on the Town Wall → one `POST /vms/{orchestrator}/tasks` that drives the whole job → `npm install` + `vite build` → `vite preview` on `:5173` until `Ctrl-C`.
- The orchestrator drives a single Goose session through ~13 tool calls: for each of `src/App.jsx`, `src/main.jsx`, `src/index.css`, `index.html` it calls `gtcall worker-1` to generate the file, `gtwall` to publish it to the Town Wall, and a best-effort `gtcall reviewer-1` review note — then posts `<<<DONE>>>`. All four `<<<FILE:>>>` posts are authored by `orchestrator-1`; there is no host authorship.
- Per-role models: orchestrator `gemini-2.5-flash` (must drive the loop without stalling), worker + reviewer `gemini-2.5-flash-lite` (single-shot). Assumes a paid-tier Gemini key — the free tier's shared 20 RPM cap across all models cannot sustain multi-turn orchestration.
- See [Multi-Agent Webdev Demo](README.md#multi-agent-webdev-demo-v036) in the README.

### In-VM agent-to-agent dispatch — `gtcall`

- New `gtcall <agent_id> "<prompt>"` CLI baked into the golden image alongside `gtwall`. It resolves the peer's `vm_id` from `GET /flocks/{id}` and posts to the control plane's `POST /vms/{vm_id}/tasks` proxy, which injects the peer's agent token — so a calling agent never needs peer credentials.
- The request body is built with `jq -n --arg`, so arbitrary multi-line prompts (newlines, quotes, backticks) dispatch safely, replacing the fragile LLM-authored `curl`/quoting that earlier demo attempts choked on.

### goose-agent `--output-format json` — clean, untruncated task output

- `cmd/goose-agent/main.go` now runs `goose run --output-format json` and returns the assistant text extracted from the envelope (`extractGooseJSONText`): it slices from the first `{` to skip Goose's startup banner, concatenates every assistant `text` block, and falls back to raw stdout if the envelope cannot be parsed.
- This is the only working escape from goose-cli's `streaming_buffer.rs` truncation, which otherwise caps fenced code at 50 lines and spills the overflow into an in-VM `/tmp/goose-*.txt` the host caller cannot reach (neither `--debug` nor `GOOSE_SHOW_FULL_OUTPUT` disables it). The `{ "output": ..., "error": ... }` response shape is unchanged.
- 4 new unit tests cover the happy path, multi-block concatenation, non-JSON fallback, and banner-prefix skip.

### `gtwall` multi-line fix

- `gtwall` now builds its JSON body with `jq -n --arg b "$1" '{body: $b}'` instead of a hand-rolled `sed` escape. The old escape did not handle newlines, so a multi-line body (a whole source file) became invalid JSON and the in-VM agent rejected it with HTTP 400 (curl `-f` → exit 22). Single-line posts are unaffected; multi-line posts now succeed. Latent for the entire v0.3.x line because gtwall had only ever posted single-line status messages.

### Golden image: `curl` + `jq`

- `scripts/build_image.sh` now installs `curl` and `jq` into the Debian Bookworm golden image (~6 MiB), which `gtcall`/`gtwall` and any future in-VM scripting rely on. `scripts/gtcall` is added to `EnsureGoldenImage`'s staleness input list so editing it triggers a rebuild.

---

## Compatibility

- **Additive.** No control-plane wire format changed. `/tasks` returns the same `{ "output", "error" }` shape (the value is now cleaner). `gtwall`'s single-line behavior is unchanged.
- **Golden-image rebuild.** The in-VM changes (`goose-agent`, `build_image.sh`, `gtwall`, new `gtcall`) trigger one automatic golden-image rebuild on the next daemon start via the mtime staleness check.
- **No new daemon dependency.** `jq` / `curl` are added inside the guest image only; the host already required `jq` since v0.3.5.

---

# v0.3.5 — Observability Trio

**Ephemera** v0.3.5 lays down the observability primitives every later cycle will depend on: a Prometheus `/metrics` endpoint, a per-VM `/stats` snapshot endpoint, and a `log/slog` migration of every daemon-side log call. The release is additive — defaults preserve every v0.3.4 behavior, no wire format changed, no external dependency was added.

---

## What's New

### Prometheus `/metrics` endpoint

- New unauthenticated `GET /metrics` returns the daemon's Prometheus text exposition payload (format 0.0.4). `EPHEMERA_METRICS_REQUIRE_AUTH=true` gates the endpoint behind the same Bearer auth as the rest of the API (defaults follow the standard scrape model — network-level isolation expected).
- Exposition formatter is hand-written (`internal/metrics/registry.go`, `internal/metrics/exposition.go`) so the project keeps its zero-runtime-dependency policy. Supports counters (`Counter`, `CounterVec` with label vectors), gauges (`Gauge`, `GaugeFunc` re-evaluated on every scrape), and histograms with configurable buckets (default `{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}`). All collectors are race-safe via `sync/atomic`.
- Counters: `ephemera_vm_spawn_total{outcome}`, `ephemera_vm_destroy_total{outcome}`, `ephemera_snapshot_create_total{type}`, `ephemera_snapshot_restore_total{outcome}`, `ephemera_flock_spawn_total`, `ephemera_flock_destroy_total`, `ephemera_watchdog_dead_total`, `ephemera_watchdog_heal_total`, `ephemera_sighup_reload_total`, `ephemera_cp_token_propagated_total{outcome}`.
- Gauges (GaugeFunc — re-read on every scrape): `ephemera_vm_count`, `ephemera_flock_count`, `ephemera_snapshot_count`, `ephemera_api_clients_count`.
- Histograms: `ephemera_vm_spawn_duration_seconds`, `ephemera_snapshot_restore_duration_seconds`, `ephemera_watchdog_probe_duration_seconds` (per-probe bucket set tuned to `{0.05, 0.1, 0.25, 0.5, 1, 2, 5}`).
- The mux is now two-level: `externalMux.Handle("/metrics", …)` is registered before the catch-all `externalMux.Handle("/", authMiddleware(internalMux))`, so ServeMux's most-specific-pattern rule routes scrape traffic past the bearer check by default. Watchdog observations flow into the registry through `Watchdog.OnDead` / `OnHeal` / `OnProbeDuration` callbacks — `internal/orchestrator/` deliberately does not import `internal/metrics/`.

### Structured logging via `log/slog`

- `go.mod` bumped from `go 1.18` to `go 1.21` to access `log/slog` from the standard library. CI's `go-version-file: go.mod` clause picks the new toolchain automatically; no `.github/workflows/` edits needed.
- Every `log.Printf` / `log.Println` / `log.Fatalf` call in `cmd/goose-daemon/`, `internal/storage/`, `internal/orchestrator/`, and `internal/network/` (138 sites total) was migrated to `slog.Info` / `slog.Warn` / `slog.Error`. Messages were rewritten as short lowercase phrases (`"recovery: disk missing"`, `"snapshot: copying disk"`); inline format args became structured fields (`vm_id`, `flock_id`, `agent_id`, `snapshot_id`, `err`, …).
- Two new env knobs: `EPHEMERA_LOG_FORMAT=text|json` (default `text`) selects TextHandler or JSONHandler; `EPHEMERA_LOG_LEVEL=debug|info|warn|error` (default `warn`) sets the minimum emitted level. The default preserves the previous `log.Printf` tone — every lifecycle event is emitted at warn-or-higher so operators see it without configuration.
- `cmd/goose-agent/` (in-VM) deliberately retains its existing `log.Printf` output this cycle — touching the agent would trigger a golden-image rebuild, which is deferred to v0.4.3 (the streaming-tasks cycle that already touches in-VM code).

### Per-VM `/stats` endpoint

- New `GET /vms/{vm_id}/stats` returns `{vm_id, cpu_percent, mem_used_mib, mem_total_mib, uptime_seconds, network_rx_bytes, network_tx_bytes, agent_busy}` as a point-in-time snapshot. Authentication uses the same control-plane Bearer token as every other VM endpoint.
- `cpu_percent` is sampled over 100 ms from `/proc/<firecracker_pid>/stat` (utime + stime). The Firecracker PID is resolved by tracing the socket path through `/proc/net/unix` (path → inode) and `/proc/<pid>/fd` (inode → process) — `firecracker-go-sdk` v1.0.0 does not expose the child PID directly. The resolved PID is cached on `runningVM.fcPID` (atomic) and re-validated on every stats request via `/proc/<pid>/comm`.
- `mem_used_mib` from `VmRSS:` in `/proc/<pid>/status`; `mem_total_mib` from the per-VM spawn sizing (mirrored on `runningVM.memSizeMib`).
- `network_rx_bytes` / `network_tx_bytes` from `/sys/class/net/<tap>/statistics/{tx,rx}_bytes`, swapped to the VM's perspective (host TAP `rx_bytes` = VM `tx_bytes`).
- `uptime_seconds` from a new `runningVM.spawnedAt` (UTC) that mirrors the already-persisted `VMState.CreatedAt`. Recovered VMs inherit their original spawn time across daemon restarts.
- `agent_busy` from a 1 s `GET /health` against the in-VM agent's existing endpoint, parsed as `{status: "idle"|"busy"}`. Direct HTTP (not via the proxy) so the response body can be JSON-decoded.
- `GET /vms?stats=true` returns the full list with embedded `stats` blocks for bulk dashboards. Per-VM stats failures (PID not resolvable, `/proc` race, agent unreachable) degrade to zero values and emit a slog `Warn`; the response never partial-errors.

### Tests

- `internal/metrics/registry_test.go` adds `TestRegistry_Counter_Increments`, `TestRegistry_CounterVec_LabelsSeparate`, `TestRegistry_Gauge_SetReturnsLastValue`, `TestRegistry_GaugeFunc_CallsFunctionEachWrite`, `TestRegistry_Histogram_BucketsCumulative`, `TestRegistry_WriteTo_FormatMatchesSpec` (golden text output), `TestRegistry_WriteTo_EscapesLabelValues`, `TestRegistry_Concurrent_NoRace`, `TestRegistry_DuplicateName_Panics`, `TestRegistry_InvalidName_Panics`, `TestFormatFloat_IntegralVsFractional`.
- `cmd/goose-daemon/metrics_handler_test.go` covers content-type, method enforcement, default-unauth, counter updates, and gauge func observation.
- `cmd/goose-daemon/stats_handler_test.go` covers `/proc/<pid>/stat` parsing, `VmRSS` parsing, TAP statistics, the `/health` probe (busy / timeout), the 404 and method-not-allowed handler paths, and `?stats=true` partial-failure behaviour.
- `cmd/goose-daemon/main_test.go` validates the slog format / level handler-selection logic.
- `e2e_test.sh` step 61 (`/metrics` endpoint format) and step 62 (`/vms/{vm_id}/stats` schema + `?stats=true` inline form) live between the existing rotation cleanup (`58c.vii`) and the rotation-daemon shutdown (`60`).

---

## Upgrade Notes

- **Go 1.21 toolchain required for local builds.** CI's `setup-go@v5` step picks this up from `go.mod` automatically, but contributors building locally must have `go1.21+` installed (`go version` to check). No source changes outside of the new `log/slog` imports rely on 1.21-specific stdlib.
- **Wire compatibility is unchanged.** Every pre-existing endpoint behaves identically; the only additions are `GET /metrics` and `GET /vms/{vm_id}/stats` plus the optional `?stats=true` query on `GET /vms`.
- **Log format change is opt-in.** The default `EPHEMERA_LOG_FORMAT=text` matches the previous `log.Printf` style closely enough that existing log scrapers / journals see the same kind of output (with structured fields appended). Set `EPHEMERA_LOG_FORMAT=json` only when an aggregator expects JSON. `EPHEMERA_LOG_LEVEL` defaults to `warn` so existing baseline noise stays the same; lower it to `info` or `debug` for troubleshooting.
- **No new env var is required for default operation.** `EPHEMERA_METRICS_REQUIRE_AUTH`, `EPHEMERA_LOG_FORMAT`, `EPHEMERA_LOG_LEVEL` are all optional with sensible defaults.

---

## Changed / new files

- `go.mod` — `go 1.21` (bump). `go.sum` regenerated by `go mod tidy`.
- `internal/metrics/registry.go` — new. Registry + Counter/CounterVec/Gauge/GaugeFunc/Histogram.
- `internal/metrics/exposition.go` — new. Prometheus text format 0.0.4 encoder.
- `internal/metrics/registry_test.go` — new. 11 tests.
- `cmd/goose-daemon/metrics_handler.go` — new. `daemonMetrics` bundle + `handleMetrics`.
- `cmd/goose-daemon/metrics_handler_test.go` — new.
- `cmd/goose-daemon/stats_handler.go` — new. `VMStats` + `VMInfoWithStats` + `handleVMStats` + `?stats=true` branch helper.
- `cmd/goose-daemon/stats_handler_test.go` — new.
- `cmd/goose-daemon/stats_collector.go` — new. PID resolution, `/proc` parsing, TAP stats, agent-busy probe.
- `cmd/goose-daemon/main_test.go` — new. slog handler tests.
- `cmd/goose-daemon/main.go` — `initSlog`, full `log` → `slog` migration, `fatal` helper.
- `cmd/goose-daemon/config.go` — `metricsRequireAuth` var; `log` → `slog`.
- `cmd/goose-daemon/api.go` — `ControlPlane.metrics`, `runningVM` adds `memSizeMib` / `spawnedAt` / `fcPID`, two-mux split, `/stats` routing, `?stats=true` branch, spawn / destroy / snapshot / restore / SIGHUP / CP-token counter wiring, `log` → `slog`.
- `cmd/goose-daemon/orchestrator_api.go` — `flockSpawn` / `flockDestroy` counters, `log` → `slog`.
- `cmd/goose-daemon/recovery.go` — `log` → `slog`, `runningVM.spawnedAt` / `memSizeMib` initialized from `VMState.CreatedAt` / `MemSizeMib`.
- `internal/orchestrator/watchdog.go` — `OnDead` / `OnHeal` / `OnProbeDuration` callback fields; per-probe timer wraps `checkOne`. `log` → `slog`.
- `internal/storage/provisioner.go` — `log` → `slog` (29 sites).
- `internal/network/manager.go` — `log` → `slog` (6 sites).
- `README.md` — `EPHEMERA_METRICS_REQUIRE_AUTH` / `EPHEMERA_LOG_FORMAT` / `EPHEMERA_LOG_LEVEL` rows; Per-VM Stats and Metrics endpoint subsections under API Reference; new Observability section under Resilience; Known Limitations row for external metrics retention; Key Features row for the observability trio.
- `CONTRIBUTING.md` — three new "extra care" items: metrics counter discipline, slog message convention, per-VM stats PID resolution.

---

# v0.3.4 — Operational Convenience

**Ephemera** v0.3.4 closes the last three operator-touch corners after v0.3.3: rotating the control-plane bearer no longer requires per-VM restarts, the watchdog cadence is overridable from the environment, and an opt-in self-heal returns recovered agents to `ready` for operators who prefer liveness over the sticky-dead default. The release is additive — defaults preserve every v0.3.3 behavior, no wire format changed, and every existing env var keeps working.

---

## What's New

### CP token hot rotation via vsock

- New `EPHEMERA_API_TOKENS_FILE` env (in `cmd/goose-daemon/config.go`) names a file path that `loadAPIClients` reads on every call. Operators edit the file, send `SIGHUP`, and the daemon picks up the new contents. Env-only deployments still work as before — file precedence is `EPHEMERA_API_TOKENS_FILE` → `EPHEMERA_API_TOKENS` → `EPHEMERA_API_TOKEN`. `parseAPIClients` is factored out of the legacy parse loop and accepts both comma- and newline-separated entries.
- `ReloadClients` (`cmd/goose-daemon/api.go`) now fans the new `apiClients[0].Token` out to every running VM that has a vsock UDS path. Snapshot taken under `cp.mu.RLock()`, dispatch in parallel goroutines, per-VM 4 s budget (20 × 200 ms — see hot-fix below for why this matches `ReconfigureGuestIP`), per-VM failure logged but never propagated. A final log line summarizes ok/total counts.
- `internal/vm/machine.go` extracts a generic `vsockSendCommand` from the existing `vsockSendChangeIP`; `ReconfigureGuestIP` becomes a thin wrapper with its original 20 × 200 ms retry budget preserved. New sibling `SetGuestCPToken` uses the same 20 × 200 ms budget. `vsockSendOnce` also stat-preflights the UDS path so a dead Firecracker surfaces as "vsock UDS missing" rather than the misleading "connection refused".
- `cmd/goose-agent/main.go`'s `handleVsockConn` now dispatches on the leading verb. The new `SET_CP_TOKEN <token>` command writes `/root/.ephemera-cp-token` via tmp-and-rename (mode 0600), so a concurrent `loadCPToken` reader never observes a partial write. `/townwall/post` continues to call `loadCPToken` per request, so the next forwarder call sees the rotated bearer without any caching change.

### Env-tunable watchdog timings

- `cmd/goose-daemon/config.go` exposes three new ints: `EPHEMERA_WATCHDOG_INTERVAL_SEC` (default 5), `EPHEMERA_WATCHDOG_TIMEOUT_SEC` (default 1), `EPHEMERA_WATCHDOG_THRESHOLD` (default 3). All three reuse the existing `envInt` helper.
- `Watchdog.Configure(interval, httpTimeout, threshold, autoHeal)` (`internal/orchestrator/watchdog.go`) is the new public entry point. The struct fields remain unexported so external callers cannot bypass the `interval >= httpTimeout` clamp logic; in-package tests continue to set fields directly.
- The startup log line now reflects the resolved values: `Watchdog started (interval=5s, timeout=1s, threshold=3, auto_heal=false)`.

### Opt-in watchdog self-heal

- New `envBool` helper (case-insensitive, accepts `1/true/yes/on` and `0/false/no/off`) backs `EPHEMERA_WATCHDOG_AUTO_HEAL` (default `false`).
- When `autoHeal=true`, `Watchdog.onSuccess` clears the `deadMarked` bit on the first successful probe of a previously-dead VM, flips the agent's status back to `ready`, persists the change via `Flock.Persist`, and posts an `<orchestrator> <id> recovered - auto-healed to ready` notice to the flock's Town Wall.
- Default (`false`) preserves the v0.3.1+ sticky-dead contract — `TestWatchdog_MarksDeadAfterThreshold` and `TestWatchdog_PersistsDeadStatus` are unchanged and still pass.

### Tests

- `TestWatchdog_Configure_AppliesTunables` and `TestWatchdog_AutoHeal_ResetsDeadMark` in `internal/orchestrator/watchdog_test.go` cover the new behaviors. The auto-heal test asserts the Town Wall recovery notice, the in-memory `Status=ready`, and the on-disk metadata round-trip.
- `e2e_test.sh` step 58c (sub-steps 58c.i–58c.vii) exercises the full rotation flow against a real Firecracker VM: spawn TOKENS_FILE daemon with v1 → spawn flock → SIGHUP after editing file to v2 → assert post-rotation `/townwall/post` 200, v1 operator bearer 401, both posts in Town Wall, and the `SIGHUP: CP token propagated` log line. Pre-flight cleanup also removes a leftover `/tmp/ephemera-tokens.txt`. Step 60 is renamed to the rotation-daemon shutdown. 58c.iii also `ls -la`s the vsock UDS before SIGHUP so operators reading a future failure can tell whether the file exists, and 58c.iii's assertion parses the `propagated to N/M` count line rather than just substring-grepping for the prefix (a silent "0/1" used to pass).

### Hot-fix: SDK signal forwarding (post-release)

The initial v0.3.4 cycle merged with a critical bug that the e2e gate did not catch on the first run: `firecracker-go-sdk` v1.0.0's default `ForwardSignals` list **includes `SIGHUP`**, and the SDK installs a goroutine that forwards every received signal to its Firecracker child via `cmd.Process.Signal(sig)`. The daemon uses `SIGHUP` for its own token reload, so sending `kill -HUP <daemon_pid>` triggered the SDK's forwarder, killed every running Firecracker (exit status 156), and the vsock fan-out then failed with `connection refused` on a UDS whose listener had just died. The bug existed latently since v0.2.0 (SDK upgrade), but v0.3.4 was the first release whose post-SIGHUP code actually depended on Firecracker still being alive.

Fix in `internal/vm/machine.go`: define a package-level `forwardSignals = []os.Signal{os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGABRT}` (SIGHUP deliberately omitted) and set `firecracker.Config.ForwardSignals: forwardSignals` in both `StartMachine` and `RestoreMachine`. Shutdown signals are still forwarded so `Ctrl-C` / `systemctl stop` propagate cleanly; only the daemon's own reload signal is intercepted before reaching Firecracker.

Two safety nets also landed alongside: `SetGuestCPToken`'s retry budget moved from 3 × 300 ms to 20 × 200 ms to match `ReconfigureGuestIP`, and `vsockSendOnce` now `os.Stat()`s the UDS path before dialing so a dead Firecracker is reported as `vsock UDS missing at <path>` instead of the more ambiguous `connection refused`.

**Operational impact for prior versions**: if you ran `kill -HUP` against any v0.2.0–v0.3.3 daemon (e.g. for the documented "token hot reload"), every Firecracker was silently killed at that moment. The v0.3.3 effect was undetectable because no post-SIGHUP code touched the children; nonetheless any VMs in flight became unreachable. Upgrading to the hot-fixed v0.3.4 is the recommended remediation.

---

## Upgrade Notes

- **Wire compatibility**: unchanged. All existing API responses, env vars, and on-disk files remain valid.
- **No default behavior change**: every new env var is default-preserving (`5s / 1s / 3` watchdog timings unchanged; `auto_heal=false` matches sticky-dead). Existing deployments observe no behavior difference until they opt in.
- **CP token hot rotation requires two prerequisites**: (1) the daemon must source tokens from `EPHEMERA_API_TOKENS_FILE`, since env vars are fixed at exec time and SIGHUP cannot change them; (2) the VMs must run a v0.3.4+ `goose-agent` (the `SET_CP_TOKEN` vsock handler did not exist before). Mixed-version fleets keep working — older VMs log a propagation failure and operators can fall back to `POST /flocks/{id}/agents/{agent_id}/restart` for those.
- **New vsock command**: `SET_CP_TOKEN <token>` joins `CHANGE_IP <cidr_ip> <gateway>` on the existing AF_VSOCK port 1234 channel. No new port, no new transport.

---

## Changed / new files

- `cmd/goose-daemon/config.go` — `EPHEMERA_API_TOKENS_FILE` source, `parseAPIClients` helper, four watchdog env vars, `envBool` helper
- `cmd/goose-daemon/api.go` — `ReloadClients` token fan-out (`propagateCPTokenToVMs`), `Configure` call after `NewWatchdog`, startup log line carries resolved tunable values
- `cmd/goose-agent/main.go` — `handleVsockConn` dispatch on verb (`CHANGE_IP` / `SET_CP_TOKEN`), `writeCPTokenAtomic`
- `internal/vm/machine.go` — `vsockSendCommand` / `vsockSendOnce` (with `os.Stat` preflight) extracted from `vsockSendChangeIP`; `ReconfigureGuestIP` becomes a thin wrapper; new `SetGuestCPToken` (20 × 200 ms retry); new `forwardSignals` package var explicitly omitting `SIGHUP`; both `StartMachine` and `RestoreMachine` set `firecracker.Config.ForwardSignals`
- `internal/orchestrator/watchdog.go` — `Watchdog.autoHeal`, `Watchdog.Configure`, opt-in `onSuccess` heal path
- `internal/orchestrator/watchdog_test.go` — `TestWatchdog_Configure_AppliesTunables`, `TestWatchdog_AutoHeal_ResetsDeadMark`
- `e2e_test.sh` — step 58c (sub-steps i–vii) for the rotation flow; step 60 renamed; pre-flight removes `/tmp/ephemera-tokens.txt`
- `README.md`, `CONTRIBUTING.md`, `RELEASE_NOTES.md`

---

# v0.3.3 — Operational Polish

**Ephemera** v0.3.3 closes the four rough edges left by the v0.3.1 / v0.3.2 hardening pass without changing any wire formats. Watchdog-detected `dead` markings now survive daemon restart and cold-restart; a single failed agent can be replaced in place without recreating the whole flock; the in-VM `/townwall/post` forwarder authenticates against an auth-on control plane without any per-VM operator setup; and the end-to-end test gained a real-LLM round-trip scenario so the `gtwall` chain stops being a code-only contract. The release is a strict superset of v0.3.2 — every existing API response, env var, and on-disk file remains valid.

---

## What's New

### Watchdog dead-status persistence

- `Flock.Persist(workDir)` is the new single entry point for flock-metadata writes (`internal/orchestrator/flock.go`). Holds a per-flock `writeMu` around `ToMetadata` + `SaveFlockMetadata`'s tmp+rename, so concurrent writers (createFlock, watchdog, recovery, per-agent restart) cannot tear each other's writes
- `Watchdog.onFailure` (`internal/orchestrator/watchdog.go`) now calls `Persist` immediately after flipping `Status` to `"dead"` — the change lands on disk before the next probe cycle
- `cmd/goose-daemon/recovery.go` persists both transitions: `markFlockAgentDead` writes the dead state when a VM can't be cold-restarted, and the success path persists `Status=ready` so a future `LoadFromDisk` does not resurrect a previously-dead agent
- `cmd/goose-daemon/orchestrator_api.go`'s `createFlock` swapped its raw `SaveFlockMetadata` call for `flock.Persist(cp.workDir)`; the raw API now carries a comment forbidding direct daemon-side calls

### Per-agent restart endpoint

- `POST /flocks/{flock_id}/agents/{agent_id}/restart` (handled by `restartAgent` in `orchestrator_api.go`) tears down one flock member's VM and respawns it with the same `agent_id`, role, and `agent_token` — callers that cached the token keep working without re-auth
- `spawnVMOptions.AgentToken` is the new optional field that drives token reuse: empty means generate fresh (standalone spawn path), non-empty means reuse verbatim (restart path)
- `Flock.UpdateAgentVM(agentID, newVMID, newAgentURL)` swaps the VM identity in place and resets `Status` to `ready`
- `Watchdog.ForgetVM(vmID)` clears cached `failCount` / `deadMarked` for a destroyed vmID so a recycled ID does not inherit stale state
- On spawn failure the agent is left `Status=dead` and persisted, so external callers see the truth without polling

### Auto-injected control-plane token

- `ControlPlane.controlPlaneTokenForVM()` returns `apiClients[0].Token` under `clientsMu` (so SIGHUP-driven `ReloadClients` is safe). Empty when auth is disabled — in that mode in-VM forwarders call CP unauthenticated, preserving the v0.3.2 dev/test flow
- `spawnVMForFlock` and `restartAgent` thread the token through `spawnVMOptions.ControlPlaneToken` → `VMPrepareOptions.ControlPlaneToken` → `injectVMFiles`, which writes it to `/root/.ephemera-cp-token` (mode 0600). Standalone `POST /vms` deliberately does NOT inject (no `/townwall/post` use case)
- `cmd/goose-agent/main.go` gains `loadCPToken()` — prefers the new file, falls back to the legacy `EPHEMERA_CONTROL_PLANE_TOKEN` env var so older golden images keep working

### Real-LLM Town Wall round-trip e2e

- `e2e_test.sh` step 59 spawns a researcher under the auth-on daemon, sends `/tasks` with an explicit `gtwall` instruction, and verifies `ROUNDTRIP_OK` reaches Town Wall via the system-prompt → Goose CLI → `gtwall` → in-VM forwarder → CP chain
- Skip-by-default: when none of `GOOGLE_API_KEY` / `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` is in the environment the step reports `ok "Skipped"` so CI without LLM credentials stays green
- Secret handling: profile files are backed up to `/tmp/researcher-{secrets,goose}.bak` before sed-injection and restored via `trap EXIT`; pre-flight cleanup also restores them in case a previous run was SIGKILLed mid-step

### Refactor for testability

- `internal/storage/provisioner.go`'s file-writing logic moved into `injectVMFiles(mntDir, opts)`, decoupling it from the loop-mount lifecycle. New mount-free unit tests cover the v0.3.3 `ControlPlaneToken` injection without needing root or `/dev/kvm`

---

## Upgrade Notes

- **Wire compatibility**: unchanged. All existing API responses, env vars, and on-disk files remain valid.
- **New on-disk file inside each flock VM**: `/root/.ephemera-cp-token` (mode 0600). Only present when `EPHEMERA_API_TOKENS` is set on the host.
- **`flocks/<id>/metadata.json` is written more often** (every dead transition + every per-agent restart). Atomic tmp+rename keeps the file always parseable; expect a handful of additional writes per long-running flock, not a steady-state burden.
- **CP token rotation is still a manual operation**: `apiClients[0]` is captured at spawn time. After SIGHUP-driven token rotation, run `POST /flocks/{id}/agents/{agent_id}/restart` (or DELETE + recreate the flock) on affected VMs. See README Known Limitations.

---

## Changed / new files

- `internal/orchestrator/flock.go` — `Flock.writeMu`, `Flock.Persist`, `Flock.UpdateAgentVM`
- `internal/orchestrator/watchdog.go` — `Watchdog.onFailure` persists; new `Watchdog.ForgetVM`
- `internal/orchestrator/persistence.go` — concurrency note updated; `SaveFlockMetadata` documented as raw primitive (callers go through `Flock.Persist`)
- `internal/orchestrator/persistence_test.go` — `TestFlock_Persist_StatusRoundTrip`, `TestFlock_Persist_ConcurrentSafe`
- `internal/orchestrator/watchdog_test.go` — `TestWatchdog_PersistsDeadStatus`, `TestWatchdog_ForgetVM_ClearsState`
- `internal/orchestrator/flock_test.go` — `TestFlock_UpdateAgentVM`
- `cmd/goose-daemon/recovery.go` — both status transitions persisted
- `cmd/goose-daemon/orchestrator_api.go` — `restartAgent`, `agents/{id}/restart` route, `controlPlaneTokenForVM` plumbed into `spawnVMForFlock`
- `cmd/goose-daemon/api.go` — `controlPlaneTokenForVM`, `spawnVMOptions.AgentToken` + `.ControlPlaneToken`, route log entry for restart
- `internal/storage/provisioner.go` — `VMPrepareOptions.ControlPlaneToken`, `injectVMFiles` refactor, `/root/.ephemera-cp-token` (mode 0600)
- `internal/storage/provisioner_test.go` — `TestInjectVMFiles_ControlPlaneToken`, `TestInjectVMFiles_EmptyControlPlaneToken_SkipsFile`
- `cmd/goose-agent/main.go` — `cpTokenPath` constant, `loadCPToken`, `/townwall/post` reads via `loadCPToken`
- `e2e_test.sh` — steps 57i, 57j, 58b, 58b.i, 59a–59e, 60 (+ pre-flight LLM-secret restore)
- `README.md`, `CONTRIBUTING.md`, `RELEASE_NOTES.md`

---

# v0.3.2 — Live VM Cold-Restart

**Ephemera** v0.3.2 closes the biggest operational gap left by v0.3.1: when the daemon stops and restarts, the VMs that were running come back up automatically with the same identity. Flock metadata recovery (v0.3.1) is no longer read-mostly — every recovered flock's member VMs are cold-restarted from their existing rootfs clones, with the same `vm_id`, IP, TAP device, MAC, agent token, and `agent_url` preserved. Memory state is not snapshotted; in-flight `/tasks` work is lost, but the system surface, network identity, and audit history are all stable across the restart. v0.3.x API responses remain fully backward compatible.

---

## What's New

### Live VM cold-restart

- Every successful `POST /vms` (and every flock member) writes `vms/<vm_id>/state.json` atomically (tmp + rename) with the VM's network identity, agent token, disk path, profile, and flock association
- On daemon startup, after flock metadata is rescanned, `ControlPlane.RecoverVMs` iterates the persisted state:
  1. **Orphan cleanup** — any leftover Firecracker process bound to the persisted API socket is sent SIGTERM, then SIGKILL after a 1.5 s grace; stale socket / log FIFO / vsock UDS files are removed (`internal/storage/orphan.go`)
  2. **Network re-reservation** — the original TAP name and MAC are recreated, the original IP is re-marked in-use in the pool (`network.Manager.ReclaimAllocation`); if the IP is no longer free, the entry is dropped and the agent is marked `dead`
  3. **Cold boot** — `vm.StartMachine` is called against the same rootfs clone with the persisted vCPU / memory sizing; `goose-agent` is waited for on `/health` up to 60 s
  4. **Flock association** — for flock VMs, the agent status is flipped back to `ready` on success or `dead` (with an `<orchestrator>` Town Wall notice) on failure
- `cp.vms` is repopulated, so `/tasks`, `/health`, `/stop`, and proxy endpoints all work against recovered VMs immediately after the daemon comes back up
- `destroyVM` deletes the per-VM state file before tearing down the Firecracker process, so a crash mid-teardown does not resurrect a VM that the operator was already removing

### Graceful shutdown feeds cold-restart

`ControlPlane.DestroyAll` (deferred from `main` when the daemon receives SIGTERM/SIGINT) was rewritten to support cold-restart. Previously it called `destroyVM` per VM, which removed `state.json` and the rootfs ext4 — so the next start had nothing to recover. v0.3.2 splits the two paths:

- **Graceful daemon shutdown** stops every Firecracker process via `StopVMM`, removes per-VM transient files (API socket, log FIFO, vsock UDS), and releases TAP/IP back to the pool — but **preserves `state.json` and the rootfs ext4**. Cold-restart picks them up on the next start.
- **Explicit `DELETE /vms/{id}`** still routes through `destroyVM` and does a full cleanup (state.json removed, rootfs removed); the VM is gone permanently.
- **SIGKILL / crash** leaves everything on disk as-is; cold-restart's orphan cleanup handles any leftover Firecracker process and then proceeds identically to the graceful path.
- **COW-restored and snapshot-restored VMs** are torn down fully in `DestroyAll` (their `state.json` is also removed) because v0.3.2 does not recover them; leaving the dm-snapshot device or bind mount would leak kernel resources.

### What is preserved vs. lost

| Preserved | Lost |
|-----------|------|
| `vm_id`, `guest_ip`, `tap_device`, `mac_addr` | In-flight `/tasks` work (memory is not snapshotted) |
| `agent_token`, `agent_url`, `profile` | Goose conversation context (in-VM, in-memory) |
| Disk contents (rootfs clone is reused, not recreated) | Watchdog `status=dead` markings (revert to `ready` until next probe) |
| Flock membership, Town Wall history, `seq` numbering | COW-mode VMs and snapshot-restored VMs (out of scope for v0.3.2) |

### Out of scope for v0.3.2

- **COW-mode VMs** (`EPHEMERA_DISK_MODE=cow`) are skipped during recovery (logged on startup). dm-snapshot orphan cleanup is deferred to a later release; workaround is to re-spawn the agent.
- **Snapshot-restored VMs** (`POST /snapshots/{id}/restore`) are not auto-recovered — restore from the snapshot again after the daemon comes back.

### New / changed files

- `internal/storage/vm_state.go` — `VMState` struct, `SaveVMState` / `LoadVMState` / `DeleteVMState` / `ListVMState` (atomic tmp+rename; `schema_version: 1`)
- `internal/storage/orphan.go` — `KillStaleFirecracker(socketPath)` + `RemoveStaleVMArtifacts`
- `internal/network/manager.go` — `ReclaimAllocation(tapName, guestIP, macAddr)`: re-reserve the exact original allocation (vs. `AllocateForRestore` which picks any free IP for vsock-reconfig snapshot restore)
- `cmd/goose-daemon/recovery.go` — `ControlPlane.RecoverVMs()` and `markFlockAgentDead`
- `cmd/goose-daemon/api.go` — `spawnVMInternal` writes state at the end of every spawn; `destroyVM` deletes state at the start of teardown; `NewControlPlane` calls `RecoverVMs` after `flockMgr.LoadFromDisk`; **`DestroyAll` rewritten** to stop Firecracker without dropping state.json or rootfs ext4 (see "Graceful shutdown feeds cold-restart")
- New e2e sub-steps `57e` (cold-restart preserves VM IDs) and `57f` (recovered VM `/health` responds); existing 57e/57f renumbered to 57g/57h. Pre-flight cleans `vms/vm-*` alongside `flocks/flock-*` and `snapshots/snap-*`.
- `vms/` and `flocks/` added to `.gitignore` (both were undocumented gaps)

---

## Upgrade Notes

- **Fully backward compatible** with v0.3.x clients — `state.json` is new on-disk surface only; no API response shape changes
- **First boot after upgrade**: existing VMs spawned by a v0.3.x daemon do *not* have `state.json` on disk, so they will not be cold-restarted. The new behavior takes effect for VMs spawned by v0.3.2 or later
- **Callers that rely on at-most-once `/tasks` semantics** should idempotency-key their task IDs or re-poll for completion across a daemon restart — memory is not preserved, so any in-flight task is lost
- **`EPHEMERA_DISK_MODE=cow` users**: keep using COW for spawn-time disk savings, but a daemon restart will not recover those VMs in v0.3.2. Plain disk mode (default) is fully covered.

---

# v0.3.1 — Goosetown Operational Hardening

**Ephemera** v0.3.1 hardens the Goosetown layer introduced in v0.3.0 for long-running workloads. Three operational risks present in v0.3.0 are addressed: VM death goes from silent to actively surfaced, flock state survives daemon restarts, and Town Wall subscribers can now detect message gaps. The release also folds in Phase 5 verification follow-up that completes the in-VM `gtwall` chain validation, plus a `CONTRIBUTING.md`. All v0.3.0 API responses remain backward compatible — only additive fields and one new internal behavior surface (watchdog).

---

## What's New

### VM health watchdog

- A background goroutine polls every flock-member VM's `/health` endpoint every 5 seconds (1 s per-probe HTTP timeout)
- After 3 consecutive failures the agent's status transitions to `"dead"` in the flock registry and a notice is auto-posted to the Town Wall as `<orchestrator>`: `worker-1 unresponsive after 3 health probes - marked dead`
- A revived VM is **not** auto-marked back to `ready` — operators clear dead state by deleting the flock or the individual VM
- Standalone (non-flock) VMs are not watched (locator returns `ok=false`)
- Watchdog is stopped before the HTTP server during shutdown to prevent it from observing a half-torn-down `cp.vms`

### Flock state persistence

- `POST /flocks` writes `flocks/<flock-id>/metadata.json` atomically (tmp + rename) before returning the response
- `DELETE /flocks/{id}` removes the metadata file (the `TOWN_WALL.log` is kept as an audit artifact)
- Daemon startup scans `flocks/*/metadata.json` and re-registers every flock in memory; the Town Wall log is reopened in append mode so full history (and `seq` numbering) continues across restarts
- Recovered flocks are read-mostly: their VM IDs no longer correspond to live Firecracker processes (those died with the previous daemon), so `/tasks` against them will fail; `/post`, `/wall`, `/wall/history`, and `DELETE` continue to work
- Schema versioned (`schema_version: 1`) for future migrations
- Live VM auto-restart is deferred to v0.3.2

### Monotonic SSE sequence numbers

- Every Town Wall `Message` now carries `seq` (uint64) starting at 1 per flock and incrementing on each post
- `seq` is preserved across daemon restarts by initializing from `len(History())` when the wall is reopened
- Subscribers detecting gaps in `seq` can fall back to `/flocks/{id}/wall/history` to recover the missing range
- The on-disk log format is unchanged (`[ts] <agent> body`); seq is wire-format-only and is reassigned 1..N from line order on each `History` read

### Phase 5 verification follow-up

These items extend the v0.3.0 work to cover what the original e2e couldn't:

- `agent_tokens` in `POST /flocks` response — additive map of `agent_id → bearer token`, lets callers authenticate to in-VM agent endpoints (matches the existing `/vms` spawn pattern)
- Per-role `agent_id` indexing — `roles=["orchestrator","researcher","researcher","worker","reviewer"]` now produces `orchestrator-1, researcher-1, researcher-2, worker-1, reviewer-1` (matches the README example; previous global indexing produced `researcher-2, researcher-3, worker-4, reviewer-5`)
- New e2e step **54b** validates the in-VM `/townwall/post` chain end-to-end (the same path `gtwall` takes), including escaped quote/backslash JSON round-trip and 401 on unauthenticated probe
- `/townwall/post` is documented as intentionally **not** proxied through the control plane — external callers should `POST /flocks/{id}/post` directly

### Daemon and tooling hardening

- mtime-based auto-rebuild for `goose-agent`, `micro-init`, and the golden image — editing in-VM Go code or `build_image.sh` no longer requires a manual `rm artifacts/...`
- Daemon `log.Fatalf` on `ListenAndServe` error so a `bind: address already in use` doesn't leave a silent zombie process
- e2e pre-flight kills any stale `ephemera-daemon` from a prior interrupted run, cleans `flocks/flock-*` workdir entries, and waits up to 600 s on cold-start (handles golden-image rebuild)
- Daemons in the e2e are started with `EPHEMERA_API_ADDR=0.0.0.0:3000` so the bridge gateway IP `10.0.1.1` accepts in-VM `/townwall/post` forwards

### Documentation

- New `CONTRIBUTING.md` focused on this project's specific gotchas (host vs in-VM binary classes, gitignored profile yaml, root/KVM e2e requirements, snapshot lifecycle, golden image bake cost, in-VM auth)
- README **Resilience** section documents the watchdog, persistence scope, and seq gap-detection pattern
- README env-var table notes `EPHEMERA_API_ADDR=0.0.0.0:3000` is required for flock /townwall/post forwarding

---

## Upgrade Notes

- **Fully backward compatible** with v0.3.0 clients — `seq`, `agent_tokens`, and the new `metadata.json` are all additive; existing endpoints unchanged
- **No image rebuild required** unless the daemon detects stale in-VM binaries — the new staleness check handles that automatically on the next start
- **First boot after upgrade**: existing `flocks/<id>/TOWN_WALL.log` files are preserved; flocks created before v0.3.1 (which have no `metadata.json`) are *not* recovered automatically. Going forward, every flock is recoverable
- **Production with `EPHEMERA_API_TOKENS` set**: the in-VM `/townwall/post` forwarder still relies on `EPHEMERA_CONTROL_PLANE_TOKEN` being set inside the VM; auto-injection is on the v0.3.2 roadmap

---

# v0.3.0 — Goosetown: Multi-Agent Orchestration

**Ephemera** now runs heterogeneous groups of role-specialized MicroVMs as a single addressable unit ("flocks"). One `POST /flocks` call spawns an orchestrator, researchers, workers, and reviewers in parallel, each sized to its role and sharing an append-only **Town Wall** log for coordination. Every v0.2.0 endpoint behaves exactly as before — the new surface is purely additive, so existing clients continue to work without changes.

---

## What's New

### Multi-Agent Flocks

- `POST /flocks` — spawn N role-specialized VMs in one call (max 20 per flock); returns flock metadata, agent records, `townwall_url`, and `post_url`
- `GET /flocks` — list all live flocks
- `GET /flocks/{id}` — describe a flock and its agents
- `DELETE /flocks/{id}` — tear down every member VM **in parallel** (~1 s for a 5-agent flock instead of ~5 s sequential) and unregister the flock
- Partial-spawn safety: if any VM in the flock fails to come up, all previously spawned VMs are torn down and the flock is removed before the error is returned

### Town Wall — Per-Flock Append-Only Log

- `POST /flocks/{id}/post` — append a message (`{agent_id, body}`) to the flock's shared log
- `GET /flocks/{id}/wall` — **SSE stream** that emits full history once, then forwards every new post live
- `GET /flocks/{id}/wall/history` — one-shot dump of the log as JSON
- Backed by `flocks/<flock-id>/TOWN_WALL.log` on disk (kept as an audit artifact after `DELETE`)
- Mutex-serialized writes with a buffered subscriber fan-out — slow subscribers are dropped from the current message rather than blocking the writer

### Per-Profile vCPU / Memory Sizing

- `vm.VMConfig` now accepts `VcpuCount` and `MemSizeMib` (zero falls back to the legacy 2 vCPU / 2048 MiB defaults)
- Built-in role profiles map to canonical sizing:

  | Role | vCPU | Memory (MiB) | Profile dir |
  |------|------|--------------|-------------|
  | `researcher` | 1 | 512 | `researcher/` |
  | `reviewer` | 1 | 512 | `reviewer/` |
  | `worker` | 2 | 2048 | `worker/` |
  | `orchestrator` | 2 | 2048 | `orchestrator/` |
  | `builder` | 4 | 4096 | `worker/` |

- Unknown profile names continue to spawn at the default sizing and resolve `configs/profiles/{name}/`, so the v0.2.0 profile contract is preserved

### Role System Prompts

- Each profile directory can ship a `system.md` that ships role instructions
- The control plane injects `system.md` content into the VM as `/root/.goose-system-prompt` at provision time
- In-VM `goose-agent` prepends it to every `/tasks` prompt as `[SYSTEM INSTRUCTIONS]\n...\n\n[USER TASK]\n...` so the role stays in-character even when the orchestrator dispatches plain user prompts
- Four shipped role prompts: `researcher` (read-only exploration), `worker` (implementation), `reviewer` (adversarial review), `orchestrator` (delegation + synthesis)

### In-VM Flock Context + `gtwall` CLI

- For flock members, `PrepareVM` writes `/root/.ephemera-flock` (`FLOCK_ID`, `AGENT_ID`) so the in-VM agent knows its identity
- New `goose-agent` endpoint: `POST /townwall/post` reads the flock context and forwards the message to the host control plane's `POST /flocks/{id}/post`
- New shell wrapper `scripts/gtwall` (installed at `/usr/local/bin/gtwall` in the golden image): one-liner posting from anywhere in the VM — `gtwall "Claiming src/styles/theme.css"`

### Optional COW Spawn Disks (`EPHEMERA_DISK_MODE=cow`)

- Setting `EPHEMERA_DISK_MODE=cow` provisions each new VM via `dm-snapshot` over the golden image instead of a 700 MiB full copy
- Initial extra disk usage drops from ~700 MiB to ~0 MiB per VM; writes accumulate in a sparse `.cow` exception store
- Default behavior is unchanged when the variable is unset, so it doubles as a safe rollback path
- Reuses the existing `SetupDMSnapshot` / `TeardownDMSnapshot` plumbing introduced in v0.2.0 for restore

### Faster Diff Snapshot Restore

- Memory merge during diff restore now writes the temporary 2 GiB `merged.bin` to `/dev/shm` (tmpfs) when available, avoiding disk I/O on the hot path
- Falls back to `{workDir}/tmp` when `/dev/shm` is not a writable directory (e.g. minimal containers)

### Spawning Internals — `spawnVMInternal`

- Extracted the spawn pipeline (network alloc → disk clone → config inject → Firecracker start → register → wait) into a shared `spawnVMInternal` helper
- Both the public `POST /vms` handler and the new flock spawner reuse it, so any future spawn change applies to both paths uniformly
- All cleanup paths consistently undo every resource they allocated before returning

### Configuration

- New env var: `EPHEMERA_DISK_MODE` — set to `cow` to enable the dm-snapshot-backed spawn path described above
- Inside a flock VM: `EPHEMERA_CONTROL_PLANE` (optional, default `http://10.0.1.1:3000`) — overrides the control plane URL used by `/townwall/post` forwarding for testing
- Inside a flock VM: `EPHEMERA_CONTROL_PLANE_TOKEN` (optional) — bearer token attached to the forwarded Town Wall post when the control plane runs with auth enabled

### Testing

- `e2e_test.sh` grows from 50 to **58 steps** with a new Goosetown block:
  - 51 prep `configs/profiles/*/{goose.yaml,goose-secrets.yaml}` from `.example` files
  - 52 spawn a 5-agent flock (orchestrator / researcher × 2 / worker / reviewer) and validate the returned IDs, URLs, and agent count
  - 53 confirm `/vms` reflects all flock members
  - 54 post to the Town Wall through the control plane
  - 55 assert `/flocks/{id}/wall/history` returns ≥ 2 entries
  - 56 verify `GET /flocks` lists the new flock
  - 57 `DELETE /flocks/{id}` and assert every VM is torn down and the flock unregisters
  - 58 daemon graceful shutdown (renumbered from former 50)
- New unit tests in `internal/orchestrator/`: Town Wall post/history, subscriber delivery, concurrent posting, line parsing, flock create/get/delete, agent status update, lock-safe JSON marshaling

### Documentation

- README adds an "Architecture" entry per flock endpoint, a "Flock API" section with curl examples (Spawn / Post / SSE Stream / History / List / Describe / Destroy), a built-in role profile table, and an updated Testing section showing the actual passing e2e output for steps 51–58

---

## Upgrade Notes

- **Backward compatible**: `POST /vms`, `POST /vms/{id}/snapshot`, `POST /snapshots/{id}/restore`, and the agent proxy endpoints behave exactly as in v0.2.0
- **Golden image**: rebuild to ship `gtwall` and `iproute2` inside the VM — `rm artifacts/golden-image.ext4 && sudo ./ephemera-daemon`. Existing v0.2.0 images keep working for non-flock VMs
- **Role profiles**: built-in role names ship `*.yaml.example` files only. Before spawning flocks, copy them and fill in API keys: `cp configs/profiles/<role>/goose.yaml{.example,}` and same for `goose-secrets.yaml`
- **COW spawn**: opt in with `EPHEMERA_DISK_MODE=cow`. Unset (default) keeps the v0.2.0 full-clone behavior — useful as a single-flag rollback if any platform-specific dm-snapshot issue surfaces

---

# v0.2.0 — Single-Host Feature Complete

**Ephemera** completes the single-host feature set. Every limitation noted in v0.1.0 is resolved: guests shut down gracefully, agent authentication is enforced, tokens reload without a restart, and VMs can be snapshotted and restored in seconds. A new control plane proxy makes goose-agent accessible from external clients without direct access to the private VM subnet.

---

## What's New

### Graceful Guest Shutdown

- `micro-init` (PID 1) now traps `SIGTERM` and calls `sync` + `poweroff(2)` — the guest powers off cleanly with no kernel panic
- `reboot=k` kernel argument lets Firecracker exit cleanly (exit code 0) on guest power-off

### Per-VM Agent Authentication

- Control plane generates a 32-byte random Bearer token per VM at spawn time
- Token written to `/root/.ephemera-agent-token` (mode `0600`) inside the VM disk
- `POST /tasks` and `POST /stop` require `Authorization: Bearer <agent_token>`
- `GET /health` remains unauthenticated (used by the control plane health poller)
- Token returned once in `POST /vms` and `POST /snapshots/{id}/restore` responses; preserved across snapshot/restore cycles

### Per-VM LLM Profiles

- Each VM spawn can specify a named profile via `{"profile": "anthropic"}` in the request body
- Profiles stored under `configs/profiles/<name>/goose.yaml` + `goose-secrets.yaml`
- Enables running multiple VMs with different providers, models, or API keys simultaneously
- Omitting `profile` uses the default `configs/goose.yaml`

### API Token Hot Reload (SIGHUP)

- `kill -HUP <daemon_pid>` re-reads `EPHEMERA_API_TOKENS` / `EPHEMERA_API_TOKEN` and swaps the in-memory client list
- Running VMs are not affected; no downtime required to add, rotate, or revoke tokens

### MicroVM Snapshot & Restore

- `POST /vms/{vm_id}/snapshot` — freezes VM memory state and copies rootfs to `snapshots/<id>/`
- `POST /snapshots/{id}/restore` — resumes a VM from snapshot in ~5 s (vs ~60 s cold boot); preserves agent token
- `DELETE /vms/{vm_id}` with `stop_after: true` destroys the source VM immediately after snapshot
- `GET /snapshots` — list all stored snapshots
- `DELETE /snapshots/{id}` — delete snapshot files

### Diff Snapshots (Multi-Checkpoint)

- First snapshot of a VM → **Full** (2 GB `memory.bin`)
- Subsequent snapshots → **Diff** (sparse `memory.bin`, dirty pages only — typically 1–400 MB)
- Type auto-detected; `"type": "full"` or `"type": "diff"` can override
- `base_snapshot_id` links each Diff to its Full base
- Deleting a Full that has referencing Diffs returns `409 Conflict`
- Restore from Diff merges base + diff memory in-memory; temp file cleaned up immediately after Firecracker opens it

### Post-Restore IP Reconfiguration (vsock)

- Each restored VM receives a fresh IP from the pool; the original IP is freed
- Guest network stack updated in-place via vsock (`CHANGE_IP <cidr> <gw>`) — no reboot required
- `goose-agent` binds `AF_VSOCK` port 1234 inside the VM for reconfiguration commands

### Concurrent Restore

- Multiple VMs can be restored simultaneously from different snapshots
- Each restore gets its own disk COW device; bind-mount stacking ensures Firecracker opens the correct device

### COW Rootfs Restore (dm-snapshot)

- Restore no longer copies the 700 MB rootfs — instead creates a Linux dm-snapshot COW device
- Base disk (`rootfs.ext4`) is read-only and shared; per-VM writes go to a sparse exception store (~0 MB initial)
- On VM delete: dm device removed, loop devices detached, exception store deleted automatically
- Falls back to full copy if dm-snapshot is unavailable

### Control Plane Agent Proxy

- Three new proxy endpoints route agent traffic through the control plane — no direct VM subnet access needed:
  - `POST /vms/{vm_id}/tasks` → goose-agent `/tasks`
  - `GET  /vms/{vm_id}/health` → goose-agent `/health`
  - `POST /vms/{vm_id}/stop`  → goose-agent `/stop`
- Callers authenticate with the control plane Bearer token only; agent token is injected internally
- `EPHEMERA_PUBLIC_URL` env var: when set, `agent_url` in VM responses points to the proxy path (`{url}/vms/{vm_id}`) instead of the private VM IP

### End-to-End Test Suite

- `e2e_test.sh` — 50-step integration test covering the full VM and snapshot lifecycle
- Validates: VM spawn, parallel tasks, snapshot/restore, concurrent restore, diff snapshots, COW rootfs, agent proxy, `EPHEMERA_PUBLIC_URL` behavior

---

# v0.1.0 — Initial Implementation

**Ephemera** is an enterprise control plane for running ephemeral AI agents inside Firecracker MicroVMs. This first release delivers a fully working end-to-end implementation: from spinning up an isolated KVM-backed VM to executing Goose AI tasks via HTTP and cleaning up all host resources on teardown.

---

## What's New

### Control Plane API

- `POST /vms` — spawn a MicroVM; blocks until goose-agent is ready (~60 s), returns `vm_id`, `guest_ip`, `agent_url`
- `GET /vms` — list running VMs
- `DELETE /vms/{vm_id}` — stop VM and release all host resources (TAP device, disk clone, IP)

### goose-agent (in-VM HTTP agent)

- Runs inside each MicroVM as PID 1 via `micro-init`
- `POST /tasks {"prompt":"..."}` — execute a Goose task, return output
- `GET /health` — `idle` | `busy` status
- `POST /stop` — graceful agent shutdown

### Self-bootstrapping

- Firecracker v1.15.1 downloaded and SHA256-verified automatically on first run
- Linux 6.1.155 kernel downloaded automatically
- Golden image (Debian Bookworm minbase + Goose + goose-agent) built via `debootstrap` on first run
- `goose-agent` binary compiled from source before image build

### Guest OS — Minimal Debian Bookworm

- Replaced initial Ubuntu 22.04 skeleton with Debian Bookworm `--variant=minbase`
- No SSH, no init daemon, no DHCP client — `micro-init` mounts virtual filesystems and execs goose-agent directly as PID 1
- Network configured via Linux kernel `ip=` boot parameter (live before any user-space process starts)
- Host timezone mirrored into VM via `/etc/localtime` symlink injection at provisioning time

### Runtime Config Injection

- `configs/goose.yaml` (provider, model, extensions) and `configs/goose-secrets.yaml` (API keys) injected into each VM's disk at provisioning time — no image rebuild needed to change provider or model
- Supports Google, Anthropic, OpenAI, Ollama, and all other Goose-compatible providers
- Keyring-free operation (`GOOSE_DISABLE_KEYRING: true`) for headless VM environments

### Network & Storage

- Linux bridge `goose-br0` with iptables MASQUERADE for VM-to-internet connectivity
- IP pool (10.0.1.2–254) sorted and recycled across VM lifecycle
- TAP device IDs recycled via free-list after VM destruction
- All host resources guaranteed to be released on teardown

### Security

- Per-client Bearer token authentication (`EPHEMERA_API_TOKENS=alice:token1,bob:token2`)
- Timing-safe token comparison via `crypto/subtle.ConstantTimeCompare`
- Each request logged with authenticated client name for audit trail
- Control plane binds to `127.0.0.1:3000` by default (localhost only)
- TLS supported via Caddy or Nginx reverse proxy (see README)
- Single-token fallback (`EPHEMERA_API_TOKEN`) for backward compatibility

---

## Prerequisites

- Ubuntu 22.04 or 24.04 host (bare metal or nested virtualization)
- `/dev/kvm` accessible
- Go 1.18+
- `sudo apt install -y curl debootstrap e2fsprogs util-linux`
