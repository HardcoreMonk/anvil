# Ephemera Web UI

A single-page app (Svelte + Vite) that the daemon serves from its own binary
under `/ui/`. It is the browser-based replacement for the script-form External
Client (`ephemera-ctl` + curl): system management through agent usage in one
place. Introduced in v0.5.0 (VM lifecycle); later cycles add tasks, snapshots,
flocks, system management, and embedded Grafana monitoring.

## How it is served

The daemon embeds the built bundle via `//go:embed all:uidist` in
`cmd/goose-daemon/ui.go` and serves it at `/ui/` (same origin as the API, so no
CORS). `/ui/` is mounted **outside** the auth/audit middleware — the login page
and JS bundle must load before the user has a token — while every data API call
the app makes (`/vms`, …) still flows through the normal Bearer auth.

## Auth model

- On load the app probes `GET /vms` with **no** header. `200` → the server has
  no clients configured (auth disabled), so the login screen is skipped.
- `401` → auth is enabled. The login screen takes a Bearer token, validates it
  against `GET /vms`, and stores it in `sessionStorage` (or `localStorage` with
  "remember me"). It is sent as `Authorization: Bearer <token>` on every call;
  any `401` clears it and returns to login.

## UI terminology

The UI uses generic IT vocabulary, not the internal/backend product names. This
applies to **display labels only** — API routes, JSON field names, and env vars
keep their original identifiers and must never be renamed to match the UI.

| Backend / internal | UI label |
|---|---|
| goose agent | **Platform Agent** |
| flock | **Agent Group** |
| Town Wall | **Activity Feed** |
| Goosetown | **Orchestration** |
| spawn (a VM) | **Create** |
| destroy (a VM) | **Delete** |

"Platform Agent" is used only as a noun (section headings, confirmation dialogs,
first reference). Concise property labels stay short — `Agent URL`, `Agent token`,
`Stop agent`, the `agent: idle` badge — rather than repeating "Platform Agent".

Identifiers that stay unchanged regardless of the UI label: routes `/vms`,
`/flocks`, `/flocks/{id}/wall`, `/post`; fields `vm_id`, `agent_token`,
`agent_url`, `agent_id`, `flock_id`, `townwall_url`, `max_agents`; every
`EPHEMERA_*` env var.

## Localization (i18n)

The UI ships English + Korean via [`svelte-i18n`](https://github.com/kaisermann/svelte-i18n).

- Setup lives in `src/lib/i18n.js`: it `addMessages('en'|'ko', …)` from
  `src/locales/*.json` (bundled synchronously, so `$_` resolves immediately) and
  `init()`s with `fallbackLocale: 'en'`.
- **Initial locale**: a saved choice in `localStorage['ephemera_locale']` wins;
  otherwise the browser language (`ko*` → Korean, everything else → English).
- The nav language switch (`EN | 한국어`) calls `setLocale()`, which flips the
  store and persists the choice; `<html lang>` is kept in sync.
- Components read strings with `$_('namespace.key')` in markup and
  `get(_)('namespace.key', { values })` in script (confirm/toast). Interpolation
  uses ICU placeholders, e.g. `"VM {id} created"`.

**Adding or changing a string:** edit **both** `src/locales/en.json` and
`src/locales/ko.json` under the same key (a missing `ko` key falls back to `en`).
Markup inside a message (e.g. a bolded span) is split across sibling keys
(`…Pre`/`…Bold`/`…Post`) and reassembled in the template to avoid `{@html}`.

**Not localized:** server-originated error text (the daemon's `{"error":…}`
surfaced via toasts), the `EPHEMERA` brand, and the version badge.

## Profile & model configuration

Each VM's LLM provider/model comes from a **profile** — `configs/goose.yaml` (the
`default` profile) or `configs/profiles/{name}/goose.yaml` — injected into the
VM's rootfs at spawn time. The UI creates, views, and edits these profiles:

- **Create VM modal** — a dropdown (from `GET /config/profiles`) picks which
  profile a new VM uses (`default` → the daemon's default config).
- **Settings screen** — lists profiles, **creates** new ones (name + provider/model
  + optional **per-VM vCPU/memory**, v0.5.1) and edits provider/model.

Endpoints (auth-protected, `cmd/goose-daemon/config_api.go`):
- `GET /config/providers` → known providers + which have a keychain API key (v0.5.1)
- `GET /config/profiles` → `[{name, provider, model, vcpu_count, mem_size_mib}]`
- `POST /config/profiles` body `{name, provider, model, vcpu_count?, mem_size_mib?}` — create a profile (v0.5.1)
- `GET /config/profiles/{name}` → `{name, provider, model, vcpu_count, mem_size_mib}`
- `PUT /config/profiles/{name}` body `{provider, model, vcpu_count?, mem_size_mib?}` — rewrites
  GOOSE_PROVIDER/GOOSE_MODEL (+ optional `EPHEMERA_VCPU_COUNT`/`EPHEMERA_MEM_SIZE_MIB`) in place
  (comments + `extensions:` block preserved; values validated against newline injection).
- `DELETE /config/profiles/{name}` — remove a user-defined profile (v0.5.1)

**Constraints:** API keys (`goose-secrets.yaml`) are **never** exposed or edited
through the UI — they stay server-side. Config is injected at spawn, so edits
apply to the **next** Create VM, not to already-running VMs. To make that
visible, `VMInfo` records the provider/model baked into each VM at spawn time
(`provider`/`model` fields), shown in the VM list and detail — so a running VM
that predates a profile edit clearly shows its older model.

## Developing

```bash
cd web
npm ci          # or: npm install
npm run dev     # Vite dev server (proxy API calls to a running daemon as needed)
```

## Rebuilding the embedded bundle (REQUIRED after any UI change)

```bash
cd web
npm ci
npm run build   # writes the bundle into ../cmd/goose-daemon/uidist/
```

Then rebuild the daemon: `go build -o ephemera-daemon ./cmd/goose-daemon/`.

### Why the build output is committed

CI (build-and-test) and the e2e harness are **node-free**, and `go:embed`
requires the files to exist at compile time. So `cmd/goose-daemon/uidist/` is
**committed to git** — `go build` then works with only the Go toolchain. Treat
`uidist/` diffs as generated artifacts and keep "build UI" commits separate from
logic commits.

### This does NOT trigger a golden-image rebuild

The golden image is rebuilt only when `scripts/build_image.sh`,
`scripts/gtwall`, `scripts/gtcall`, `artifacts/goose-agent`, or
`artifacts/micro-init` change (see `internal/storage/provisioner.go`). The UI
bundle lives under `cmd/goose-daemon/uidist/` — outside `artifacts/` — so
rebuilding it changes only the daemon binary and never invalidates the image.
