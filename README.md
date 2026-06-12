# Ephemera

[![CI](https://github.com/steve-seungeui/ephemera/actions/workflows/ci.yml/badge.svg)](https://github.com/steve-seungeui/ephemera/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/steve-seungeui/ephemera)](https://github.com/steve-seungeui/ephemera/releases)
[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Firecracker](https://img.shields.io/badge/Firecracker-v1.15.1-FF4500?logo=amazonaws&logoColor=white)](https://github.com/firecracker-microvm/firecracker)

**Control plane for ephemeral AI agents on Firecracker MicroVMs.**

Ephemera spins up isolated, KVM-backed MicroVMs in milliseconds to run agentic AI
workloads ([Goose](https://github.com/aaif-goose/goose)) inside a minimal Debian
guest, then tears them down completely — disk, network, and all. A single host
runs many VMs behind one HTTP control plane that handles spawn, snapshot/restore,
multi-agent orchestration, and an optional MCP tool gateway.

> **Scope.** This is a **single-host** control plane. Multi-host clustering is
> intentionally out of scope and reserved for a downstream fork built on the MCP
> gateway's swap-in interfaces. Per-release detail lives in
> [`RELEASE_NOTES.md`](RELEASE_NOTES.md).

---

## How it works

```
External client
      │  HTTPS  (TLS terminated by a reverse proxy you run)
      ▼
Control plane  :3000                  ← VM / snapshot / flock / config API + Web UI
      │  provision
      ▼
MicroVM (Firecracker + KVM)           ← one isolated KVM boundary per VM
  ├── Debian Bookworm minbase rootfs
  ├── micro-init (PID 1)  →  goose-agent :8080
  └── goose (the AI agent, run per task)

      ▲  HTTP, host-only subnet 10.0.1.0/24
      │
goose-agent  http://10.0.1.x:8080     ← reachable from the host (and via the control-plane proxy)
  POST /tasks · GET /health · POST /stop · POST /townwall/post
```

The control plane never exposes the VM subnet externally. Clients talk only to
`:3000`; the proxy routes `/vms/{id}/tasks|health|stop` to the VM's private
agent and injects the per-VM token, so callers only ever hold a control-plane
Bearer token.

**Optional MCP gateway** (off unless `EPHEMERA_MCP_ENABLED=1`): a host-resident
MCP server bound to the bridge IP (`10.0.1.1:3001`) that in-VM goose clients
connect to. It aggregates backend MCP servers (`configs/mcp/servers.yaml`) —
remote HTTP servers or local de-privileged stdio subprocesses — into one
namespaced, per-profile-filtered catalog of tools/resources/prompts. Backend
credentials stay host-side and are never injected into a VM; the caller is
identified by source IP → VM → profile.

### VM lifecycle

```
spawn     CloneDiskCOW   dm-snapshot view of the golden image (~0 MiB; default)
          PrepareVM      inject goose.yaml + secrets + agent token, timezone,
                         and (flock / MCP) extra files in one mount cycle
          StartMachine   Firecracker: kernel + disk + TAP NIC + sized vCPU/memory;
                         networking via the kernel ip= boot parameter (no DHCP)
          waitForAgent   poll :8080/health until ready (~60 s cold boot)

snapshot  PauseVM → CreateSnapshot (memory.bin + state.bin)
          Full: copy rootfs.ext4      Diff: write rootfs.diff (changed blocks)
          ResumeVM (or destroy if stop_after=true)

restore   merge diff onto base (memory + rootfs) when restoring a Diff
          SetupDMSnapshot (COW) → RestoreMachine → ReconfigureGuestIP (vsock)
          waitForAgent (~5 s)

teardown  StopVMM (SIGTERM → micro-init sync + poweroff(2))
          release dm-snapshot / disk, delete TAP, return IP to the pool
```

Firecracker embeds the TAP name and disk path in `state.bin`, so restore
recreates the TAP with its original name and places the disk at its original
path; the guest IP is reconfigured over vsock afterward.

---

## Install (from a release)

Run Ephemera from a prebuilt release — no Go toolchain or source checkout. Each
release ships two tarballs:

- **FULL** (`ephemera-<ver>-linux-amd64-full.tar.gz`) — bundles the VM image; the
  first VM is ready immediately.
- **SLIM** (`ephemera-<ver>-linux-amd64-slim.tar.gz`) — small download; the image
  is built on first boot (needs `debootstrap` + outbound internet).

```bash
sudo apt-get install -y iproute2 dmsetup iptables   # runtime deps (both variants)
tar xzf ephemera-<ver>-linux-amd64-full.tar.gz
cd ephemera-<ver>
sudo ./install.sh
```

The interactive `install.sh` runs preflight checks (amd64, `/dev/kvm`, runtime
tools), installs into **`/opt/ephemera`**, prompts for your LLM provider + API key
(writing `goose.yaml` / `goose-secrets.yaml`), optionally mints an API token, and
registers + starts a `systemd` service. When it finishes, the Web console is at
**http://localhost:3000/ui/** and `ephemera-ctl` is on your `PATH`. Manage it with
`systemctl {status|restart|stop} ephemera`, watch logs via `journalctl -u ephemera -f`,
and remove it with `sudo /opt/ephemera/uninstall.sh`. The SLIM variant additionally
needs `curl debootstrap util-linux e2fsprogs` for its first-boot image build.

→ Full guide (requirements, variant choice, post-install, upgrade, uninstall,
troubleshooting): **[`INSTALL.md`](INSTALL.md)**.

## Quick start (build from source)

The steps below build Ephemera from source — the developer flow. End users should
use **[Install (from a release)](#install-from-a-release)** above.

### Prerequisites

| Requirement | Detail |
|---|---|
| Host OS | Ubuntu 22.04 or 24.04 (bare metal, or a VM with nested virtualization) |
| CPU | `/dev/kvm` accessible |
| Go | 1.21+ |
| Packages | `curl`, `debootstrap`, `e2fsprogs`, `util-linux`, `jq`, `dmsetup` |
| Privileges | root at runtime (KVM + network interface management) |

```bash
sudo apt-get install -y curl debootstrap e2fsprogs util-linux jq dmsetup
```

Firecracker, the Linux kernel, and the golden image are downloaded and built
automatically on first run (the kernel and Firecracker binary are SHA256-verified).

### Build

```bash
git clone https://github.com/steve-seungeui/ephemera.git
cd ephemera
go build -o ephemera-daemon ./cmd/goose-daemon/
go build -o ephemera-ctl   ./cmd/ephemera-ctl/   # optional operator CLI
```

### Configure the default LLM

```bash
cp configs/goose.yaml.example         configs/goose.yaml
cp configs/goose-secrets.yaml.example configs/goose-secrets.yaml
```

`configs/goose.yaml` selects the provider/model the default profile uses:

```yaml
GOOSE_PROVIDER: google
GOOSE_MODEL: gemini-2.5-flash
GOOSE_TELEMETRY_ENABLED: false
GOOSE_DISABLE_KEYRING: true   # required — a MicroVM has no keyring daemon
```

`configs/goose-secrets.yaml` holds the API keys (**never commit it**):

```yaml
GOOGLE_API_KEY: "your-key-here"
```

The built-in provider registry covers **google**, **anthropic**, **openai**, and
**groq**; the Settings UI offers a provider only when its key is present in this
keychain. (You can still point `goose.yaml` at any provider Goose itself
supports — those just aren't surfaced in the registry/UI.)

### Run

```bash
sudo ./ephemera-daemon
```

First run compiles the in-VM binaries (`micro-init`, `goose-agent`), builds the
golden image via `debootstrap` (~5–8 min), and downloads the kernel + Firecracker.
Later starts skip anything already present. Then open the Web console at
**http://localhost:3000/ui/**, or drive the API directly:

```bash
curl -X POST http://localhost:3000/vms \
  -H "Content-Type: application/json" -d '{"profile":"anthropic"}'
# → {"vm_id":"vm-…","guest_ip":"10.0.1.10","agent_url":"http://10.0.1.10:8080","agent_token":"…"}

curl -X POST http://localhost:3000/vms/vm-…/tasks \
  -H "Content-Type: application/json" -d '{"prompt":"What is my kernel version?"}'
```

---

## Profiles

A **profile** bundles the LLM configuration a VM boots with. The mandatory
`default` profile is `configs/goose.yaml`; any number of named profiles live in
`configs/profiles/{name}/`:

```
configs/profiles/researcher/
  goose.yaml    GOOSE_PROVIDER / GOOSE_MODEL, EPHEMERA_BUILTINS,
                and optional EPHEMERA_VCPU_COUNT / EPHEMERA_MEM_SIZE_MIB
  system.md     a system prompt prepended to every /tasks prompt (optional)
```

API keys are **never** stored per profile — every VM reads the one global
keychain (`configs/goose-secrets.yaml`) at spawn time. Real `goose.yaml` /
`goose-secrets.yaml` files under `configs/profiles/*/` are gitignored.

You create and edit profiles from the **Settings** UI (or the
`/config/profiles` API) at runtime — no daemon restart, no image rebuild, since
config is injected at spawn. An edit applies to the **next** VM created from that
profile; already-running VMs keep what they were spawned with (`GET /vms` reports
each VM's `provider`/`model`). Deleting a profile that a running VM was spawned
from is refused with `409 Conflict`.

### Sizing

A VM defaults to **1 vCPU / 1024 MiB** (the "Standard" preset). How a profile
changes that depends on the spawn path:

- **`POST /vms` (standalone):** a profile's `goose.yaml` may set
  `EPHEMERA_VCPU_COUNT` / `EPHEMERA_MEM_SIZE_MIB`, and those override the
  default. The Settings UI writes them (with Light / Standard / Advanced quick
  presets: 1/512, 1/1024, 2/2048) and validates the range (vCPU ≤ 8, memory
  256–16384 MiB).
- **Flock members (`POST /flocks`):** every agent spawns at the default
  1 vCPU / 1024 MiB. Per-profile sizing is **not** applied on the flock path —
  the profile still supplies provider/model/builtins/system prompt, just not
  vCPU/memory.

### Builtin extensions

Each profile selects which goose builtin extensions its agents load, via
`EPHEMERA_BUILTINS` in the profile `goose.yaml` (Settings → Extensions, or the
`/config/profiles/{name}/builtins` API). Absent → `developer` (the historical
default). When the MCP gateway is on and the profile is allowed a backend, the
gateway is added as an extra streamable-HTTP extension.

---

## Flocks (multi-agent orchestration)

A **flock** ("Goosetown") is one `POST /flocks` call that spawns a group of VMs
sharing an append-only **Town Wall** log for coordination.

**Role vs. profile.** Each agent has two independent labels:

- a **role** — a logical label (`orchestrator`, `researcher`, `worker`, …) that
  names the agent within the flock and forms its `agent_id` (`role-N`);
- a **profile** — the config source (provider / model / system prompt / builtins).

The request carries `roles[]` and an optional **parallel** `profiles[]`. For
agent *i*, `profiles[i]` chooses the profile; if it's empty or omitted, the
**role name is used as the profile name** (so `configs/profiles/{role}/` applies
when it exists, else the default config). This lets a logical role label
(`frontend`) differ from the profile it actually runs (`worker`). The chosen
profile is preserved across restart / role-change / add-agent.

```bash
curl -X POST http://localhost:3000/flocks \
  -H "Content-Type: application/json" \
  -d '{
        "task": "Add dark mode toggle to login page",
        "roles":    ["orchestrator","researcher","worker"],
        "profiles": ["fast-orchestrator","cheap-researcher",""]
      }'
# researcher uses the "cheap-researcher" profile; worker (empty) falls back to
# the "worker" profile; both run at 1 vCPU / 1024 MiB.
```

The response returns the agents plus a one-time `agent_tokens` map (store it —
it isn't retrievable later). If any VM fails to spawn, every VM spawned so far is
torn down and the flock is removed — partial flocks are never left running. A
per-flock cap (`max_agents`, default 20) is enforced on create and add to bound
IP/TAP exhaustion.

**Coordination primitives:**

- **Town Wall** — `POST /flocks/{id}/post` appends a message;
  `GET /flocks/{id}/wall` streams it over SSE (history first, then live);
  `GET /flocks/{id}/wall/history` returns the log with optional
  `agent_id` / `since` / `until` / `contains` filters. Inside a VM, the bundled
  `gtwall "msg"` posts to it and `gtcall <agent_id> "prompt"` dispatches a task to
  a peer (the proxy injects the peer's token — no peer credentials needed).
- **Membership** — `POST /flocks/{id}/agents` adds one, `DELETE …/agents/{id}`
  removes one, `PATCH …/agents/{id}` recreates a member under a new role.
- **Lifecycle** — `POST /flocks/{id}/pause` · `/resume` pause/resume all members
  (runtime-only; a daemon restart brings them back running);
  `POST /flocks/{id}/broadcast` fans one prompt to every member in parallel and
  gathers each result; `POST …/agents/{id}/restart` recreates a single member,
  keeping its `agent_id`, role, and token.
- **Health watchdog** — polls each member's `/health` (default 5 s / 1 s timeout /
  3 fails) and marks an unresponsive agent `dead` with a Town Wall notice. The
  marking is persisted and sticky by default (`EPHEMERA_WATCHDOG_AUTO_HEAL=true`
  opts into self-healing). Standalone VMs are not watched.

---

## API reference

All control-plane endpoints require `Authorization: Bearer <token>` when tokens
are configured. The agent endpoints (`:8080`) use the per-VM `agent_token`;
`GET /health` is always unauthenticated.

### Control plane (`:3000`)

| Method & path | Purpose |
|---|---|
| `POST /vms` | Spawn a VM (optional `{"profile":"…"}`). Returns `vm_id`, `guest_ip`, `agent_url`, and the one-time `agent_token`. Blocks ~60 s for cold boot. |
| `GET /vms` | List running VMs (`?stats=true` inlines per-VM cpu/mem/net/uptime). |
| `DELETE /vms/{id}` | Graceful teardown (best-effort agent `/stop`, then force-stop + free TAP/IP/disk). |
| `GET /vms/{id}/stats` | Point-in-time host-observed VM stats. |
| `POST /vms/{id}/snapshot` | Snapshot to disk (`{"type":"full\|diff\|"" (auto)", "stop_after":false}`). |
| `POST /vms/{id}/tasks` | **Proxy** → agent `/tasks` (`?stream=1` for NDJSON; `?session=` for multi-turn). |
| `GET /vms/{id}/health` · `POST /vms/{id}/stop` · `GET /vms/{id}/sessions` · `…/sessions/{name}/transcript` | **Proxy** → agent. |
| `GET /snapshots` · `POST /snapshots/{id}/restore` · `DELETE /snapshots/{id}` | Snapshot list / restore / delete. |
| `POST/GET /flocks`, `GET/DELETE /flocks/{id}` | Flock create / list / describe / tear down. |
| `POST /flocks/{id}/post` · `GET …/wall` · `GET …/wall/history` | Town Wall post / SSE stream / history. |
| `POST /flocks/{id}/broadcast` · `/pause` · `/resume` | Fan-out a prompt; pause/resume all members. |
| `POST/DELETE/PATCH /flocks/{id}/agents[/{id}]` · `…/{id}/restart` | Add / remove / change-role / restart an agent. |
| `GET /watchdog/status` | Watchdog tunables + per-VM fail counts and dead list. |
| `GET /config/providers` · `/clients` · `/monitoring` · `/presets` · `/builtins` | Registry / API clients (names + expiry, never tokens) / Grafana URL / sizing presets / builtins. |
| `GET/POST /config/profiles`, `GET/PUT/DELETE /config/profiles/{name}` | List / create / update / delete a profile (provider/model + optional sizing). |
| `GET/PUT/DELETE /config/profiles/{name}/system` | A profile's `system.md` (≤ 64 KiB). |
| `GET/PUT /config/profiles/{name}/builtins` · `/mcp` | A profile's builtin set / MCP server binding. |
| `GET /config/mcp` · `/config/mcp/servers` | MCP gateway status / configured backends + live health (never credentials). |
| `GET /audit` | Recent access-log entries (`?limit/client/status/method`). |
| `GET /metrics` | Prometheus exposition (unauthenticated by default). |
| `GET /ui/` | Embedded Web console. |

### goose-agent (`http://10.0.1.x:8080`, host-only)

| Method & path | Purpose |
|---|---|
| `POST /tasks` | Run a Goose task. `{"prompt":"…","session":"…"}`; `?stream=1` for NDJSON. Returns `{"output","error"}`. One task at a time per VM (`503` when busy). |
| `GET /health` | `idle` \| `busy` (unauthenticated). |
| `POST /stop` | Graceful shutdown (PID 1 then powers the guest off). |
| `POST /townwall/post` | Flock members only — forwards `{"body":"…"}` to the control plane's `/flocks/{id}/post` using the flock context baked into the VM. |
| `GET /sessions` · `GET /sessions/{name}/transcript` | List chat sessions; fetch one session's full transcript so the UI can repaint a resumed conversation (cache from the last task turn, or a read-only `goose session export` fallback). |

`/tasks` output is the assistant text extracted from Goose's
`--output-format json` envelope, which avoids the goose-cli streaming buffer that
otherwise truncates fenced code past 50 lines into an in-VM temp file. With a
`session`, the agent runs `goose run … -n <session> [--resume]` and returns only
the latest turn's reply (so a multi-turn conversation doesn't re-emit the whole
transcript). A `system.md` profile prompt, when present, is prepended as
`[SYSTEM INSTRUCTIONS]\n…\n\n[USER TASK]\n…`.

When a conversation is resumed, the Web UI repaints earlier turns from
`GET /sessions/{name}/transcript`: the agent returns the transcript it cached on
the last task turn (a VM memory snapshot preserves it), falling back to a
read-only `goose session export` dump — no model call — after a cold restart.

When `EPHEMERA_PUBLIC_URL` is set, `agent_url` in VM responses uses the proxy
path `{EPHEMERA_PUBLIC_URL}/vms/{id}` instead of the private IP, so external
clients can call it as-is; the proxy paths are always available regardless.

### Snapshots: Full and Diff

The first snapshot of a VM is **Full** (complete `memory.bin` + `rootfs.ext4`);
later snapshots auto-detect to **Diff** (sparse `memory.bin` of dirty pages +
`rootfs.diff` of changed 4 KiB blocks, merged onto the base on restore). Force a
type with `{"type":"full"|"diff"}`. A Full snapshot that is the base of any Diff
is protected from deletion (`409 Conflict`) — delete the Diffs first. A restore
takes ~5 s vs. ~60 s cold boot and reuses the original `agent_token`.

Restore uses a Linux **dm-snapshot** COW device over the snapshot's read-only
`rootfs.ext4`: guest writes accumulate in a sparse `.cow` exception store
(~0 MiB initially), eliminating the ~700 MB per-restore full copy. If dm-snapshot
tooling is unavailable, the daemon falls back to a bind-mount full copy. A
different-snapshot concurrent restore works; two restores of the **same**
snapshot collide on the vsock UDS path baked into `state.bin` and are not
supported.

---

## Operator CLI

`ephemera-ctl` is a dependency-free stdlib wrapper over the API. It reads
`EPHEMERA_CTL_URL` (default `http://127.0.0.1:3000`) and a token from `--token` /
`EPHEMERA_CTL_TOKEN` / `EPHEMERA_API_TOKEN`; add `--json` for raw output.

```bash
ephemera-ctl vm spawn [--profile NAME]              # → vm_id, guest_ip, agent_url, agent_token
ephemera-ctl vm ls [--stats]                        # vm rm|health|stop|stats <id>; vm task <id> "<prompt>"
ephemera-ctl vm snapshot <id> [--stop-after] [--type full|diff]

ephemera-ctl flock create --task "build X" --roles orchestrator,worker,reviewer
ephemera-ctl flock ls|get <id>|rm <id>
ephemera-ctl flock post <id> --agent worker-1 --body "msg"
ephemera-ctl flock wall <id> [--history]            # stream SSE, or print history
ephemera-ctl flock add-agent <id> <role> | rm-agent <id> <agent_id> | set-role <id> <agent_id> <role>
ephemera-ctl flock restart <id> <agent_id> | pause <id> | resume <id> | broadcast <id> <message>

ephemera-ctl snapshot ls|restore <id>|rm <id>
ephemera-ctl audit [--limit N] [--client C] [--status S] [--method M]
ephemera-ctl metrics
```

Non-2xx responses print the server's JSON error to stderr and exit non-zero, so
the CLI composes in scripts.

---

## Web UI

The daemon serves a Svelte SPA at **`/ui/`** on the same address as the API
(no extra process or port; `/` redirects there). The build is embedded via
`go:embed` and committed (`cmd/goose-daemon/uidist/`), so `go build` needs no
Node toolchain — rebuild only after editing `web/`:

```bash
cd web && npm install && npm run build   # writes ../cmd/goose-daemon/uidist/
```

`/ui/` is mounted outside the auth chain (the login page must load before the
user has a token; the bundle carries no secrets), while every data API call still
flows through Bearer auth. Screens: **Login** (auto-skipped when auth is off),
**VM list** (live stats + model, Create VM with a profile dropdown), **VM detail**
(live stats, a cancelable multi-turn **conversation** panel that repaints prior
turns when reopened, snapshot capture, delete), **Settings** (per-profile provider/model + system prompt + sizing,
profile create), **Snapshots** (restore/delete), **Orchestration** (flock CRUD,
per-agent actions, pause/resume/broadcast, live Town Wall feed), and a read-only
**System** console (Audit / Watchdog / Configured clients / embedded Grafana).

The UI ships EN/KO localization (`svelte-i18n`; follows the browser, toggle
persists) and uses generic display vocabulary — *Platform Agent* (the in-VM
goose agent), *Agent Group* (flock), *Activity Feed* (Town Wall) — while API
routes/fields keep their original identifiers.

---

## Configuration

All settings are environment variables, read once at startup unless noted.

| Variable | Default | Description |
|---|---|---|
| `EPHEMERA_API_ADDR` | `127.0.0.1:3000` | Control-plane bind address. Use `0.0.0.0:3000` behind a reverse proxy **or** when using flocks — the in-VM Town Wall forwarder targets the bridge gateway `10.0.1.1:3000`, unreachable on the loopback default. |
| `EPHEMERA_API_PORT` | `3000` | Port only (used when `EPHEMERA_API_ADDR` is unset). |
| `EPHEMERA_API_TOKENS_FILE` | _(unset)_ | Path to a `name:token[:expires]` file (comma/newline-separated). Takes precedence over `EPHEMERA_API_TOKENS` and is re-read on every load, which is what enables SIGHUP hot rotation. |
| `EPHEMERA_API_TOKENS` | _(unset)_ | Per-client Bearer tokens: `alice:tok1,bob:tok2` (each may carry `:expires`). The first non-expired token is auto-injected into flock VMs for the Town Wall forwarder. |
| `EPHEMERA_API_TOKEN` | _(unset)_ | Single-token fallback (client name `default`). Unset = auth disabled (local/dev only). |
| `EPHEMERA_AGENT_PORT` | `8080` | Port goose-agent listens on inside each VM. |
| `EPHEMERA_PUBLIC_URL` | _(unset)_ | Externally reachable base URL (no trailing slash). When set, `agent_url` uses the proxy path instead of the private IP. |
| `EPHEMERA_DISK_MODE` | `cow` | Spawn disk strategy. COW (a dm-snapshot view of the golden image, ~0 MiB initial) is default; `plain`/`full` forces a 700 MiB copy. Auto-falls back to plain when dm-snapshot tooling is absent. |
| `EPHEMERA_DISK_MIN_FREE_MIB` | `1024` | Free-space floor enforced before a clone or snapshot writes; a `statfs` pre-flight returns `507 Insufficient Storage` rather than failing mid-write. |
| `EPHEMERA_AUTOSNAPSHOT` | `false` | When on, snapshots each recoverable VM's memory on **graceful** shutdown and **warm-restores** it next start (in-flight agent work survives a daemon bounce). One-shot, best-effort, cold-boot fallback. A 5-agent flock snapshot is ~10 GB. |
| `EPHEMERA_MAX_TASK_DEPTH` | `5` | Max nested agent→agent `/tasks` hops; the proxy refuses at/over the cap with `508 Loop Detected`. |
| `EPHEMERA_WATCHDOG_INTERVAL_SEC` / `_TIMEOUT_SEC` / `_THRESHOLD` | `5` / `1` / `3` | Watchdog poll cadence / per-probe timeout (interval clamped ≥ timeout) / consecutive fails before `dead`. |
| `EPHEMERA_WATCHDOG_AUTO_HEAL` | `false` | When on, a recovered `dead` agent auto-returns to `ready` with a Town Wall notice (default off = sticky-dead). |
| `EPHEMERA_METRICS_REQUIRE_AUTH` | `false` | When on, `GET /metrics` requires a Bearer token (default off matches the Prometheus scrape model). |
| `EPHEMERA_LOG_FORMAT` / `EPHEMERA_LOG_LEVEL` | `text` / `warn` | slog handler (`text`/`json`) and minimum level (`debug`/`info`/`warn`/`error`). |
| `EPHEMERA_AUDIT_DISABLE` | `false` | Turn off the access audit log (on by default). |
| `EPHEMERA_AUDIT_MAX_MIB` / `_KEEP` | `100` / `5` | Audit-log rotation size / generations retained. |
| `EPHEMERA_TOWNWALL_MAX_MIB` / `_KEEP` | `10` / `3` | Town Wall log rotation size / generations. |
| `EPHEMERA_GRAFANA_URL` | _(unset)_ | Grafana base URL embedded in the System → Monitoring tab (Grafana must allow embedding). |
| `EPHEMERA_CTL_URL` / `EPHEMERA_CTL_TOKEN` | `http://127.0.0.1:3000` / _(unset)_ | `ephemera-ctl` target and token (token falls back to `EPHEMERA_API_TOKEN`). |
| `EPHEMERA_MCP_ENABLED` | _(unset)_ | Enable the MCP gateway (`1`/`true`/`yes`/`on`). Off = VMs get no MCP extension. Requires `configs/mcp/servers.yaml`. |
| `EPHEMERA_MCP_PORT` | `3001` | Gateway listen port. The injected endpoint is `http://ephemera-gw:<port>/mcp` — a letter-starting `/etc/hosts` alias for the bridge IP, so the tool-name prefix goose derives stays valid for providers like Gemini. |
| `EPHEMERA_MCP_BIND_IP` | `10.0.1.1` | Gateway bind IP (the bridge gateway — reachable only from VMs and the host). |
| `EPHEMERA_MCP_RATE` / `_BURST` | `0` / `0` | Per-(VM, backend) tool-call budget in calls/minute (`0` = unlimited) and token-bucket burst (`0` = the rate). Throttled calls return a JSON-RPC error metered `outcome=rate_limited`. |
| `EPHEMERA_MCP_STDIO_USER` | `nobody` | Unprivileged user `transport: stdio` backend subprocesses run as (only when the daemon is root; a non-root daemon spawns children as itself with a warning). An unresolvable user disables the gateway at startup. |
| `EPHEMERA_NET_ANTISPOOF` | `1` (on) | Per-TAP ebtables anti-spoof pinning each VM to its source MAC+IP, hardening the gateway's source-IP caller identity. Opt out with `0`/`false`/`no`/`off`; if `ebtables` is absent, logs a warning and continues disabled (never fatal). |

`EPHEMERA_API_ADDR` takes precedence over `EPHEMERA_API_PORT`. Send SIGHUP to
reload tokens; with `EPHEMERA_API_TOKENS_FILE` SIGHUP also fans the rotated token
out to running VMs over vsock.

---

## Security

**API authentication.** Configure per-client Bearer tokens
(`EPHEMERA_API_TOKENS="alice:$TOK1,bob:$TOK2"`, or a file for hot rotation);
comparison is timing-safe and the matched client name is threaded into the audit
log. A token entry may carry an optional `:expires` (RFC3339 or Unix seconds); a
matched-but-expired token returns `401` (metered `outcome=expired`). The token
injected into flock VMs for Town Wall callbacks is always the **first non-expired**
client, so letting a primary token expire doesn't break in-VM forwarding — keep
at least one never-expiring client. With no tokens set, the API is unauthenticated
(local/dev only — **never expose the control plane without a token**).

**Token rotation (SIGHUP).** Point the daemon at `EPHEMERA_API_TOKENS_FILE`, edit
the file, and `kill -HUP`. `ReloadClients` re-reads the file, hot-swaps the
client list, and fans the new control-plane token out to every running flock VM
over vsock (`SET_CP_TOKEN`, atomic rewrite of `/root/.ephemera-cp-token`) — no VM
restart. Env-supplied tokens are fixed at exec, so live rotation needs the file
source.

**Per-VM agent token.** Each VM gets a unique 32-byte random Bearer token, written
to `/root/.ephemera-agent-token` (mode 0600) and returned once at spawn/restore.
`/tasks` and `/stop` require it; `/health` is open for the internal poller. The
token is tied to the VM disk and survives snapshot/restore.

**Network isolation & MCP caller identity.** VMs share one host-only bridge
(`goose-br0`, `10.0.1.0/24`) with NAT for outbound LLM calls; the subnet is never
externally routable. The MCP gateway identifies a caller by source IP → VM →
profile, so per-TAP ebtables anti-spoof (`EPHEMERA_NET_ANTISPOOF`, default on)
pins each VM to its assigned source MAC+IP — a compromised VM can't forge
another's IP to borrow its tool permissions. The control-plane API is
Bearer-authed (not IP-trusted), so it's unaffected by spoofing regardless; the
gateway is the one source-IP-trusting surface this hardens. An optional
per-(VM, server) rate limit caps a runaway agent.

**MCP credential boundary.** Backend credentials (`configs/mcp/secrets.yaml`)
stay host-side: HTTP backends get them injected per request; stdio backends get
one child env var. They are never written into a VM. stdio subprocesses run
de-privileged (setuid to `EPHEMERA_MCP_STDIO_USER`), in their own process group,
with rlimits (`RLIMIT_NOFILE`, `RLIMIT_CORE=0`, and `RLIMIT_NPROC` when
de-privileged) and a minimal env, and are reaped on daemon shutdown.

**TLS.** The control plane speaks plain HTTP — terminate TLS at a reverse proxy
(Caddy or Nginx) and set `EPHEMERA_API_ADDR=0.0.0.0:3000`. A 300 s+
`proxy_read_timeout` is recommended since `POST /vms/{id}/snapshot` can take
minutes.

---

## Resilience

**Live VM cold-restart.** Every spawn writes `vms/<vm_id>/state.json` (atomic).
On startup the daemon clears orphan Firecracker processes and stale sockets,
re-reserves each VM's original TAP/IP/MAC, and cold-boots it from its existing
rootfs (a plain clone, or a COW VM's exception store re-layered over the golden
image). The same `vm_id`, agent token, and `agent_url` are preserved.

- **Graceful shutdown** (SIGTERM/SIGINT) stops every Firecracker via `StopVMM`
  and preserves each recoverable VM's rootfs + `state.json` for the next start.
- **`DELETE /vms/{id}`** does a full cleanup — the VM is gone, not restarted.
- **SIGKILL/crash** — deferred cleanup doesn't run, but `state.json` + rootfs
  survive and the next start cold-restarts them.

**Memory warm-restore** (`EPHEMERA_AUTOSNAPSHOT=true`, opt-in): on graceful
shutdown the daemon snapshots each recoverable VM's live memory, and the next
start warm-restores it (same identity), so in-flight `/tasks` work survives a
bounce. One-shot and best-effort, with a cold-boot fallback; a SIGKILL still cold
boots. (This is why the Firecracker signal-forwarding list excludes SIGTERM/SIGINT:
the daemon owns graceful teardown, and a forwarded SIGTERM would kill Firecracker
mid-snapshot.)

**What survives a restart:** `vm_id` / IP / TAP / MAC, agent token + URL, disk
contents, flock membership, Town Wall history, and persisted watchdog `dead`
markings. In-VM memory (and goose conversation context) is lost unless warm-restore
is enabled and the shutdown was graceful — so callers should idempotency-key
`/tasks` or re-poll across an ungraceful restart.

**Snapshot-restored VMs** persist a `state.json` with `source_snapshot_id` and are
re-restored from their source snapshot on the next start (back to snapshot-time
memory+disk). Two cases are intentionally **not** recovered: the legacy
bind-mount-fallback restore path (no `state.json`), and a restored VM whose source
snapshot was deleted while it ran (dropped and surfaced, not silently kept).

---

## Observability

`GET /metrics` exposes a Prometheus catalogue (text format 0.0.4, unauthenticated
by default) via a self-implemented, zero-dependency formatter — counters
(`ephemera_vm_spawn_total`, `_snapshot_create_total`, `_auth_total`,
`_flock_spawn_total`, `_mcp_tool_calls_total`, …), gauges
(`ephemera_vm_count`, `_flock_count`, …), and histograms
(`ephemera_vm_spawn_duration_seconds`, `_snapshot_restore_duration_seconds`,
`_watchdog_probe_duration_seconds`). `GET /vms/{id}/stats` returns a per-VM
cpu/mem/net/uptime/agent_busy snapshot (poll it for time series). Daemon logs use
`log/slog` (`EPHEMERA_LOG_FORMAT`/`_LEVEL`). Every API request is appended to
`{workDir}/audit/access.jsonl` (`ts, client, method, path, status, duration_ms,
remote_addr, bytes` — never tokens, headers, bodies, or query strings),
size-rotated and queryable via `GET /audit`.

`observability_demo.sh` stands up the daemon + Prometheus + Grafana with a
pre-provisioned dashboard and an automatic workload (`sudo bash
observability_demo.sh`; runs until Ctrl-C).

---

## Testing

**Unit tests (CI).** Run on every push/PR via GitHub Actions; no special hardware.

```bash
go test ./...           # standard
go test -race ./...     # before merging concurrency-sensitive changes
```

They cover token parsing, profile path resolution, agent auth, Town Wall
append/history/seq, flock persistence + disk recovery, watchdog dead-marking,
per-VM `state.json` round-trips, the Prometheus registry + exposition format, the
`/metrics` and `/stats` handlers, and slog handler selection.

**End-to-end (`e2e_test.sh`).** A full integration test that boots a real daemon,
spawns actual Firecracker MicroVMs, and exercises every endpoint — including
two-VM parallelism, Full/Diff snapshot + concurrent restore, COW spawn + cold
restart, restored-VM auto-recovery, the full flock lifecycle + dynamic membership
+ pause/resume/broadcast, watchdog dead-marking, token TTL + SIGHUP rotation,
audit + metrics + stats, and the MCP gateway (HTTP and stdio backends, including
subprocess reaping). Requires `/dev/kvm` and root:

```bash
go build -o ephemera-daemon ./cmd/goose-daemon/   # e2e runs the prebuilt binary
sudo bash e2e_test.sh                             # ~15–30 min depending on API rate limits
```

**Multi-Agent Webdev demo (`webdev_demo.sh`).** A manual, LLM-gated demo: an
orchestrator + worker + reviewer flock collaboratively builds and publishes a
React + Vite site entirely from inside the VMs, harvested off the Town Wall and
served via `vite preview`. Needs a Gemini key and `/dev/kvm`; see the script for
model-choice and rate-limit notes.

---

## Project layout

```
cmd/
  goose-daemon/     Control-plane daemon: HTTP API (api.go), config + profiles
                    (config.go, config_api.go), flock orchestration
                    (orchestrator_api.go), recovery, metrics/stats/audit handlers,
                    MCP gateway wiring (mcp_*.go), embedded Web UI (ui.go, uidist/)
  goose-agent/      In-VM HTTP agent (baked into the golden image): /tasks, /health,
                    /stop, /townwall/post; runs goose and extracts the latest turn
  micro-init/       PID 1 for each MicroVM: mounts virtual FS, manages goose-agent,
                    poweroff(2) on exit
  ephemera-ctl/     Dependency-free operator CLI

web/                Svelte + Vite SPA (source; build output committed to uidist/)

internal/
  vm/               Firecracker SDK wrapper (StartMachine / RestoreMachine, vsock)
  network/          IP/TAP pool, bridge + NAT, per-TAP ebtables anti-spoof
  storage/          Golden-image bootstrap, disk clone, COW dm-snapshot,
                    snapshot/restore + diff merge, per-VM state.json, orphan reclaim
  orchestrator/     Flock + FlockManager, Town Wall, health watchdog
  metrics/          Self-implemented Prometheus registry + exposition formatter
  mcpgateway/       Host-resident MCP gateway: HTTP + stdio backends, per-profile
                    policy, source-IP caller identity, rate limit, session store

configs/            goose.yaml(.example), goose-secrets.yaml(.example),
                    profiles/<name>/, mcp/{servers,secrets}.yaml(.example)
scripts/            build_image.sh, build_release.sh, in-VM gtwall / gtcall CLIs
install.sh · uninstall.sh · ephemera.service.in · INSTALL.md   end-user release installer
e2e_test.sh · observability_demo.sh · webdev_demo.sh
```

Runtime state directories (`artifacts/`, `snapshots/`, `flocks/`, `vms/`) are
auto-created and gitignored.

---

## Known limitations

| Limitation | Detail |
|---|---|
| **Single-host** | All VMs run on one host; multi-host clustering is out of scope. |
| **Same-snapshot concurrent restore** | Two VMs from the *same* snapshot collide on the vsock UDS path fixed in `state.bin`. Different-snapshot concurrent restores work. |
| **Cross-machine restore** | Manual only: copy `snapshots/<id>/` to the target host at the same absolute path, then restore. No built-in transfer. |
| **Cold-restart loses in-VM memory by default** | Auto-restart re-boots from the rootfs clone; in-flight `/tasks` work is dropped unless `EPHEMERA_AUTOSNAPSHOT` warm-restore is on and the shutdown was graceful. |
| **CP token hot-rotation needs `_TOKENS_FILE`** | Env tokens are fixed at exec; live SIGHUP rotation requires the file source. (The `…/restart` fallback re-injects the current token for env-sourced tokens.) |
| **Metrics retention is external** | `/metrics` exposes raw counters/gauges only — wire an external Prometheus to store history. |

---

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for development setup, test gates,
secrets handling, and the areas that need extra care (KVM, networking, snapshots,
golden-image bake, in-VM auth).

## License

MIT — see [LICENSE](LICENSE).
