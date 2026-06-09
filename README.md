# Ephemera

[![CI](https://github.com/steve-seungeui/ephemera/actions/workflows/ci.yml/badge.svg)](https://github.com/steve-seungeui/ephemera/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/steve-seungeui/ephemera)](https://github.com/steve-seungeui/ephemera/releases)
[![Go](https://img.shields.io/badge/Go-1.18+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Firecracker](https://img.shields.io/badge/Firecracker-v1.15.1-FF4500?logo=amazonaws&logoColor=white)](https://github.com/firecracker-microvm/firecracker)

**Enterprise Control Plane for Ephemeral AI Agents via Firecracker MicroVMs**

Ephemera orchestrates isolated, KVM-backed MicroVM environments for agentic AI workloads. Each VM runs [Goose](https://github.com/aaif-goose/goose) as an autonomous agent inside a minimal Debian guest, fully contained within hardware VM boundaries and completely wiped on termination.

Beyond single-VM execution, Ephemera supports **multi-agent flocks** ("Goosetown"): one `POST /flocks` call spawns a group of role-labeled VMs (orchestrator, researcher, worker, reviewer, …), all sharing an append-only **Town Wall** log for coordination. Each role name doubles as a profile name: if a matching `configs/profiles/{role}/` exists it supplies that agent's provider/model and system prompt, otherwise the agent spawns with the default config.

---

## Architecture

```
External Client
      │  HTTPS (TLS-terminated by reverse proxy)
      ▼
Reverse Proxy  :443                   ← Caddy / Nginx (TLS termination)
      │  HTTP (Bearer token, encrypted inside TLS tunnel)
      ▼
Ephemera Control Plane  :3000         ← VM + snapshot + flock management
  POST   /vms                         → spawn VM → returns {vm_id, agent_url, agent_token}
  GET    /vms                         → list running VMs
  DELETE /vms/{vm_id}                 → stop & destroy VM
  POST   /vms/{vm_id}/snapshot        → freeze VM state to disk
  GET    /snapshots                   → list stored snapshots
  POST   /snapshots/{id}/restore      → resume VM from snapshot (~5 s vs ~60 s cold boot)
  DELETE /snapshots/{id}              → delete snapshot files
  POST   /flocks                      → spawn multi-agent flock (one VM per role)
  GET    /flocks                      → list flocks
  GET    /flocks/{id}                 → describe flock + agents
  DELETE /flocks/{id}                 → tear down all member VMs in parallel
  POST   /flocks/{id}/post            → append message to Town Wall
  POST   /flocks/{id}/broadcast       → fan one prompt out to every member agent (v0.4.4)
  GET    /flocks/{id}/wall            → SSE stream of Town Wall messages
  GET    /flocks/{id}/wall/history    → Town Wall log (filters: ?agent_id/since/until/contains, v0.4.3)
  GET    /watchdog/status             → watchdog tunables + per-VM health state (v0.4.4)
  GET    /config/providers            → known LLM providers + which have an API key (v0.5.1)
  GET    /config/clients              → configured API clients (name + expiry; never tokens) (v0.5.5)
  GET    /config/monitoring           → Grafana base URL for the embedded Monitoring tab (v0.5.5)
  GET    /config/profiles             → list each profile's provider/model (v0.5.0)
  POST   /config/profiles             → create a user-defined profile (+ optional vCPU/memory) (v0.5.1)
  PUT    /config/profiles/{name}      → update a profile's provider/model (v0.5.0)
  DELETE /config/profiles/{name}      → delete a user-defined profile (v0.5.1)
  GET    /config/profiles/{name}/system    → read a profile's system.md prompt (v0.5.4)
  PUT    /config/profiles/{name}/system    → write a profile's system.md (≤64 KiB) (v0.5.4)
  DELETE /config/profiles/{name}/system    → clear a profile's system.md (v0.5.4)
  GET    /ui/                         → embedded browser Web console (Svelte SPA, v0.5.0)

      │  provision
      ▼
MicroVM (Firecracker + KVM)           ← isolated KVM hardware boundary
  ├── Debian Bookworm minbase (rootfs)
  ├── micro-init (PID 1)  →  goose-agent :8080
  └── goose (AI agent, runs per task)

External Client
      │  HTTP (via control plane proxy — no direct VM access needed)
      ▼
Control Plane  :3000  /vms/{vm_id}    ← proxies to VM's private agent
  POST  /vms/{vm_id}/tasks            → proxy → goose-agent :8080/tasks  (?stream=1 NDJSON; ?session = multi-turn, v0.5.0)
  GET   /vms/{vm_id}/health           → proxy → goose-agent :8080/health
  POST  /vms/{vm_id}/stop             → proxy → goose-agent :8080/stop

goose-agent  http://10.0.1.x:8080    ← private subnet 10.0.1.0/24 (host-only)
  POST  /tasks    {"prompt":"..."}    → run a Goose task, return result
  GET   /health                       → idle | busy
  POST  /stop                         → graceful shutdown
```

> `agent_url` in VM responses points to the control plane proxy when `EPHEMERA_PUBLIC_URL` is set, or to the VM's private IP otherwise. Direct access to the private IP still works from the host.

### VM Provisioning Flow

```
CloneDiskCOW()   → dm-snapshot view of golden image → ~0 MiB initial (default)
                   (or CloneDisk full copy with EPHEMERA_DISK_MODE=plain → per-VM ext4 disk)
PrepareVM()      → inject goose.yaml, goose-secrets.yaml, agent_token,
                   /etc/localtime, and (flock members only) /root/.ephemera-flock
                   + /root/.goose-system-prompt   (single mount/unmount cycle)
StartMachine()   → Firecracker: kernel + disk + TAP NIC + per-profile vCPU/memory
                   network via kernel ip= boot parameter (no DHCP)
waitForAgent()   → poll http://10.0.1.x:8080/health until ready (~60 s cold boot)
```

### Snapshot/Restore Flow

```
POST /vms/{id}/snapshot
  → auto-detect type:
      no prior Full for this VM → Full  (memory.bin = 2 GB, non-sparse)
      prior Full exists         → Diff  (memory.bin = sparse, dirty pages only)
  → PauseVM()         (freeze guest CPU execution)
  → CreateSnapshot()  (write memory.bin + state.bin; Diff uses SnapshotType="Diff")
  → Full: CopyDisk          → snapshots/{id}/rootfs.ext4 (reflink/sparse full copy)
    Diff: WriteRootfsDiff   → snapshots/{id}/rootfs.diff (changed 4 KiB blocks only)
  → ResumeVM()        (unfreeze guest, or destroy if stop_after=true)

POST /snapshots/{id}/restore
  → if Diff: MergeMemoryDiff(base.memory.bin + diff.memory.bin → tmp/merged.bin)
             MergeRootfsDiff(base.rootfs.ext4 + diff.rootfs.diff → tmp/merged.ext4)
  → SetupDMSnapshot() (COW restore: losetup × 2 + dmsetup snapshot → bind-mount;
                        initial extra disk usage ≈ 0, writes-on-demand to sparse .cow file)
  → AllocateForRestore() (recreate original TAP name + MAC; allocate any free IP)
  → RestoreMachine()  (Firecracker loads snapshot; vsock device rebuilt from state.bin)
  → ReconfigureGuestIP() (vsock: CHANGE_IP new_ip/24 → ip addr + ip route in guest)
  → waitForAgent()    (poll /health at new IP, ~5 s)
  → cleanup:          merged.bin deleted after VM starts;
                      .cow exception store deleted on VM delete
```

> Firecracker v1.x stores the TAP device name and disk path inside `state.bin`. Restoration recreates the TAP with the original name and places the disk at the original path. The guest IP is reconfigured via vsock after restore.

### Teardown Flow

```
DELETE /vms/{id}
  → StopVMM()          (SIGTERM to Firecracker → micro-init catches SIGTERM,
                         calls sync + poweroff(2); guest shuts down gracefully)
  → For COW-restored VMs:
    TeardownDMSnapshot() (umount -l bind-mount → dmsetup remove → losetup -d × 2
                           → rm sparse .cow exception store)
  → For fresh VMs:
    Remove disk        (delete cloned ext4 via stored diskPath)
  → Release()          (delete TAP device, return IP to pool)
```

---

## Key Features

| Feature | Detail |
|---------|--------|
| **Self-bootstrapping** | Golden image, kernel, Firecracker downloaded + SHA256-verified on first run; goose-agent / micro-init / golden image are also rebuilt automatically when their sources are newer than the cached artifact (mtime-based staleness check), so editing in-VM Go code or `build_image.sh` does not need a manual `rm artifacts/...` |
| **Minimal guest OS** | Debian Bookworm minbase — no SSH, no init daemon; `micro-init` (Go binary, PID 1) mounts virtual filesystems and manages goose-agent lifecycle |
| **Graceful guest shutdown** | `micro-init` traps SIGTERM and calls `poweroff(2)` — no kernel panic on VM exit |
| **Per-VM LLM profiles** | A mandatory `default` profile (`configs/goose.yaml`) plus any number of user-defined profiles (`configs/profiles/{name}/goose.yaml`), each selecting a provider + model. API keys live in one global keychain (`configs/goose-secrets.yaml`), never per profile. User-defined profiles can set their own per-VM vCPU/memory (v0.5.1); the default and any unsized profile spawn at 1 vCPU / 1024 MiB; the Settings UI offers Light/Standard/Advanced sizing presets (v0.5.3) |
| **Provider-restricted config** | The Settings UI only offers providers whose API key is present in the global keychain (`GET /config/providers`); the built-in registry covers Google, Anthropic, OpenAI, and Groq |
| **Multi-agent flocks** | `POST /flocks` spawns a group of role-specialized VMs in one call; `DELETE /flocks/{id}` tears them all down in parallel |
| **Town Wall log** | Per-flock append-only log with SSE streaming (`/flocks/{id}/wall`) for coordination; `gtwall "..."` CLI inside each VM posts to it, and `gtcall <agent_id> "..."` (v0.3.6) dispatches a prompt to a peer agent — both hide curl/token/JSON-quoting behind a one-line interface |
| **Role system prompts** | Each role profile can ship a `system.md` that is injected into the VM and prepended to every `/tasks` prompt. Editable from the Settings UI (`GET/PUT/DELETE /config/profiles/{name}/system`, v0.5.4) — a UI-created profile no longer boots with an empty prompt |
| **COW spawn rootfs (default)** | New VMs get a dm-snapshot view of the golden image instead of a 700 MiB full copy — ~43% faster spawn, ~0 MiB initial disk. Default since v0.4.2; opt out with `EPHEMERA_DISK_MODE=plain`. The daemon probes dm-snapshot support at startup and auto-falls back to a full clone if unavailable. Auto-recovered across a daemon restart since v0.4.0. |
| **Runtime config injection** | `goose.yaml` and `goose-secrets.yaml` injected at provision time — no image rebuild required to change provider/model |
| **Per-VM agent authentication** | Control plane generates a 32-byte random Bearer token per VM; token is written to the VM disk and returned once in `POST /vms` response |
| **MicroVM snapshots (Full + Diff)** | Freeze VM memory state to disk; restore in ~5 s. First snapshot → Full; subsequent snapshots of the same VM → Diff, storing only changed memory pages **and** changed rootfs blocks (v0.4.0), merged onto the base on restore. Diff is automatically selected; Full is always the reference base. Original agent token preserved across restores. |
| **COW rootfs on restore** | Restored VMs use a Linux dm-snapshot COW device backed by the snapshot's `rootfs.ext4` (read-only base, shared). Per-VM guest writes accumulate in a sparse exception store (~0 initial disk usage). Eliminates the ~700 MB full copy previously required per restore. |
| **Post-restore IP reconfiguration** | Restored VMs receive a fresh IP from the pool via vsock — the guest's network stack is updated in-place without reboot, decoupling the restore IP from the snapshot state. |
| **Restored VM auto-recovery** (v0.4.5) | dm-snapshot restored VMs now persist a `state.json` (with `source_snapshot_id`) and are **auto-recovered** across a daemon restart — re-restored from their source snapshot (back to snapshot-time memory+disk; post-restore writes are not preserved, same as a manual re-restore). Spawn-path VMs were already cold-restarted since v0.4.0. Caveats: bind-mount-fallback restores (dm-snapshot tooling absent) and restored VMs whose source snapshot was deleted are not recovered (the latter is surfaced as dropped, not silently kept). |
| **IP and TAP recycling** | IPs (10.0.1.2–254) and TAP IDs are returned to a pool and reused across VM lifecycle |
| **NAT for outbound internet** | Host bridge `goose-br0` with iptables MASQUERADE enables VM-to-internet for LLM API calls |
| **Per-client API auth** | Named Bearer tokens per client (`alice:tok1,bob:tok2`); timing-safe comparison; optional per-token TTL (`name:token:expires`, v0.4.1); the matched client identity is threaded into request context for the audit log |
| **SIGHUP token hot reload** | API token list can be updated without restarting the daemon or interrupting running VMs |
| **VM health watchdog** (v0.3.1) | Polls every flock-member `/health` every 5 s; 3 consecutive failures → agent `status=dead` + auto Town Wall notice. See [Resilience](#resilience). |
| **Flock metadata persistence** (v0.3.1) | `flocks/<id>/metadata.json` written atomically on spawn; daemon startup re-registers every flock and reopens its Town Wall log. |
| **Monotonic Town Wall seq** (v0.3.1) | Every `Message` carries `seq` (uint64, 1-based per flock); subscribers can detect dropped messages and recover from `/wall/history`. |
| **Fatal-on-bind daemon startup** (v0.3.1) | Daemon `log.Fatalf` if the API listener fails to bind (e.g. port already in use), so a stale process never silently masks a fresh one. |
| **Live VM cold-restart** (v0.3.2) | `vms/<vm_id>/state.json` written on every spawn; daemon startup cleans orphan Firecracker processes, re-reserves the original TAP/IP/MAC, and boots each VM from its existing rootfs clone. Same `vm_id`, same agent token, same `agent_url` across the restart. Memory state is not preserved unless `EPHEMERA_AUTOSNAPSHOT` warm restore is enabled (v0.4.0). See [Resilience](#resilience). |
| **Memory auto-snapshot** (v0.4.0) | Opt-in `EPHEMERA_AUTOSNAPSHOT=true` snapshots each recoverable VM's memory on **graceful** shutdown and **warm-restores** it on the next start (same `vm_id`/IP/token), so a daemon bounce preserves in-flight agent state instead of cold-booting. Best-effort and one-shot, with a cold-boot fallback; a SIGKILL/crash still cold-boots. See [Resilience](#resilience). |
| **Watchdog dead-status persistence** (v0.3.3) | When the watchdog marks an agent `dead`, the new status is written to `flocks/<id>/metadata.json` (via `Flock.Persist`, serialized by a per-flock `writeMu`). Daemon restart and cold-restart both preserve the marking, so a once-dead agent stays dead until explicitly restarted. |
| **Per-agent restart** (v0.3.3) | `POST /flocks/{id}/agents/{agent_id}/restart` tears down one flock member's VM and respawns it with the same `agent_id`, role, and `agent_token` (callers' cached tokens keep working). The new VM gets a fresh `vm_id` / `guest_ip`; the agent's status resets to `ready`. |
| **Dynamic flock membership** (v0.4.3) | `POST /flocks/{id}/agents` adds an agent (per-role `role-N` id, returns `agent_token`, 20-agent cap); `DELETE /flocks/{id}/agents/{agent_id}` removes one (empty flock allowed); `PATCH …/agents/{agent_id}` changes role by recreating the VM under the new sizing/prompt (`agent_id` + token preserved). CLI: `ephemera-ctl flock add-agent`/`rm-agent`/`set-role`. |
| **Flock pause/resume + max_agents** (v0.4.3) | `POST /flocks/{id}/pause` · `/resume` pause/resume **all** member VMs via Firecracker (runtime-only — not persisted; the watchdog skips dead-marking paused agents). `POST /flocks` accepts `max_agents` for a per-flock cap (default 20), enforced on create and add. CLI: `ephemera-ctl flock pause`/`resume`, `create --max-agents`. |
| **Town Wall query + rotation** (v0.4.3) | `GET /flocks/{id}/wall/history` filters: `?agent_id=` / `since=` / `until=` (RFC3339) / `contains=`. The log rotates by size (`EPHEMERA_TOWNWALL_MAX_MIB` default 10 MiB, `_KEEP` default 3); history reflects the active file (rotated backups kept on disk). |
| **Flock broadcast** (v0.4.4) | `POST /flocks/{id}/broadcast` `{"body":"…"}` scatters one prompt to **every** member agent's `/tasks` in parallel and gathers each result (`sent`/`skipped`/`failed` tally + per-agent `results`); busy agents are reported `busy` (skipped). The broadcast is also recorded on the Town Wall. CLI: `ephemera-ctl flock broadcast <flock_id> <message>`. |
| **Watchdog status** (v0.4.4) | `GET /watchdog/status` returns the health watchdog's tunables (`interval_sec`/`timeout_sec`/`dying_threshold`/`auto_heal`) and live per-VM state (`vm_fail_counts`, `vm_dead_marked`). Read-only; behind the same auth as the other internal routes. |
| **Streaming `/tasks`** (v0.4.4) | `POST /vms/{id}/tasks?stream=1` streams newline-delimited JSON — `{"type":"progress",…}` frames (goose stderr activity + 15s heartbeat) then one `{"type":"result","output":…,"error":…}` frame. The proxy flushes per chunk. The default (no `stream=1`) path is unchanged. Streaming commits `200` up front, so goose errors arrive in `result.error`, not the status code. |
| **Nested-task depth guard** (v0.4.4) | Agent→agent `gtcall` is loop-guarded: the proxy reads `X-Ephemera-Task-Depth` on each `/tasks` hop, refuses at/over `EPHEMERA_MAX_TASK_DEPTH` (default 5) with `508 Loop Detected`, and forwards `depth+1`. `goose-agent` passes the depth to the goose subprocess (`EPHEMERA_TASK_DEPTH`) and `gtcall` re-sends it, so depth accumulates across the call tree. |
| **Auto-injected control-plane token** (v0.3.3) | When `EPHEMERA_API_TOKENS` is set, the host writes the first non-expired client's token (`apiClients[0]` until v0.4.1's per-token TTL) into each flock VM at `/root/.ephemera-cp-token` (mode 0600); the in-VM `/townwall/post` forwarder reads it automatically. No more manual `EPHEMERA_CONTROL_PLANE_TOKEN` env inside every VM. |
| **CP token hot rotation** (v0.3.4) | `EPHEMERA_API_TOKENS_FILE=/path/to/tokens` enables true hot rotation: edit the file, send SIGHUP, and the daemon both swaps `cp.clients` and fans the new token out to every running VM over vsock (`SET_CP_TOKEN` command, atomic file rewrite inside the guest). No per-VM restart needed for the in-VM forwarder to pick up the new bearer. |
| **Env-tunable watchdog** (v0.3.4) | `EPHEMERA_WATCHDOG_INTERVAL_SEC` / `_TIMEOUT_SEC` / `_THRESHOLD` override the 5 s / 1 s / 3-fail defaults at startup. `EPHEMERA_WATCHDOG_AUTO_HEAL=true` opts in to self-healing — a `dead` agent that resumes responding is auto-marked `ready` (default off preserves sticky-dead). |
| **Observability trio** (v0.3.5) | Prometheus `/metrics` endpoint (zero-dep exposition format, counters + gauges + histograms), per-VM `GET /vms/{vm_id}/stats` snapshot (cpu/mem/net/uptime/agent_busy), and a `log/slog` migration with `EPHEMERA_LOG_FORMAT=json` + `EPHEMERA_LOG_LEVEL=...` controls. See [Observability](#observability-v035). |
| **Autonomous multi-agent demo** (v0.3.6) | `webdev_demo.sh` stands up an orchestrator + worker + reviewer flock that designs, generates, reviews, and publishes a complete React + Vite site to the Town Wall with zero host authorship. See [Multi-Agent Webdev Demo](#multi-agent-webdev-demo-v036). |
| **In-VM agent-to-agent dispatch** (v0.3.6) | `gtcall <agent_id> "<prompt>"` sends a task to a peer through the control-plane proxy, which injects the peer's token. Both `gtcall` and `gtwall` build their request bodies with `jq --arg`, so arbitrary multi-line prompts and file bodies (newlines, quotes, backticks) post safely. |
| **Clean agent task output** (v0.3.6) | goose-agent runs Goose with `--output-format json` and returns the extracted assistant text, so `/tasks` output is no longer interleaved with the startup banner or truncated to an in-VM temp file when fenced code exceeds 50 lines. |
| **Access audit log** (v0.4.1) | Every API request is appended as one JSON line to `{workDir}/audit/access.jsonl` (`ts, client, method, path, status, duration_ms, remote_addr, bytes` — never tokens or bodies), size-rotated (`EPHEMERA_AUDIT_MAX_MIB`/`_KEEP`), queryable via authenticated `GET /audit`. On by default; `EPHEMERA_AUDIT_DISABLE=true` to disable. See [Access audit log](#access-audit-log-v041). |
| **Per-token TTL & rotation** (v0.4.1) | Token entries accept an optional expiry — `name:token:expires` (RFC3339 or Unix seconds); a matched-but-expired token is rejected `401` (`ephemera_auth_total{outcome="expired"}`). The in-VM control-plane token is the first **non-expired** client. Two-field `name:token` never expires (backward compatible). |
| **Operator CLI `ephemera-ctl`** (v0.4.1) | Dependency-free stdlib CLI wrapping the REST API (`vm`/`flock`/`snapshot`/`audit`/`metrics` verbs; human tables or `--json`). Reads `EPHEMERA_CTL_URL` + `--token`/`EPHEMERA_CTL_TOKEN`/`EPHEMERA_API_TOKEN`. See [Operator CLI](#operator-cli-ephemera-ctl-v041). |
| **Web console** (v0.5.0–0.5.5) | A browser console the daemon serves at `/ui/` (single binary via `go:embed`, same origin as the API — no CORS): token login (auto-skipped when auth is disabled), VM list with live stats + model, Create VM (profile dropdown), VM detail with live stats and a **multi-turn conversation** panel (cancelable streaming), per-profile model/provider **Settings**, and VM delete (v0.5.0); snapshot/restore screens + profile creation with per-VM sizing (v0.5.1); and the **Orchestration** console — Agent Group (flock) CRUD, per-agent actions, pause/resume, broadcast, and a live **Activity Feed** (Town Wall over SSE) (v0.5.2); a per-profile **system prompt** editor, an **operator**-authored feed, and a per-agent **Send task** action (v0.5.4); a read-only **System** console — Audit log / Watchdog / Configured clients (tokens never shown) / embedded Grafana **Monitoring** (v0.5.5). Svelte + Vite SPA; the build is committed (`cmd/goose-daemon/uidist/`) so `go build` needs no Node. See [Web UI](#web-ui-v050). |
| **English / Korean UI** (v0.5.0) | The Web console ships full EN/KO localization (`svelte-i18n`); the initial language follows the browser (`ko*` → Korean, else English) and a nav toggle switches + persists the choice. UI vocabulary is generic IT (display only): *Platform Agent* (in-VM goose agent), *Agent Group* (flock), *Activity Feed* (Town Wall), *Create/Delete* (spawn/destroy). |
| **Profile/model editing** (v0.5.0) | `GET /config/profiles` lists each profile's provider/model; `PUT /config/profiles/{name}` rewrites `GOOSE_PROVIDER`/`GOOSE_MODEL` in place (comments + `extensions:` preserved; API keys are never read or written here). The Settings screen drives these; an edit applies to the **next** VM (config is injected at spawn), and each VM records the provider/model it was spawned with (`VMInfo.model`). |
| **Multi-turn conversation** (v0.5.0) | `POST /vms/{id}/tasks` accepts an optional `session`; with it, `goose-agent` runs goose as `-n <session> [--resume]`, so consecutive turns continue one goose chat session (context preserved). Omitting `session` keeps the original stateless one-shot behavior (`ephemera-ctl`, `gtcall`). |
| **Graceful VM delete** (v0.5.0) | `DELETE /vms/{id}` first asks the in-VM agent to shut down cleanly (best-effort `POST /stop`, 2 s) before force-stopping Firecracker, then frees TAP/IP/disk and deregisters. The old "stop agent" action — which actually halted the whole guest while leaving the VM registered — was removed; Delete is the single teardown. |

---

## Project Layout

```
cmd/
  goose-daemon/       Control plane daemon (main binary)
    main.go           Startup, artifact bootstrap, ControlPlane init,
                      initSlog (TextHandler/JSONHandler + level gating, v0.3.5)
    api.go            HTTP API: VM + snapshot CRUD, auth middleware
                      (timing-safe; per-token TTL + client-identity context, v0.4.1),
                      two-mux split for unauthenticated /metrics (v0.3.5),
                      spawnVMInternal (shared by /vms and /flocks paths;
                      AgentToken / ControlPlaneToken plumb-through),
                      counter/histogram wiring for spawn/destroy/snapshot/
                      flock/SIGHUP/CP-token paths (v0.3.5),
                      controlPlaneTokenForVM (first non-expired client → in-VM bearer, v0.4.1)
    config.go         Env-var configuration + AgentProfile / LookupProfile
                      (role → vCPU, memory, profile directory mapping);
                      EPHEMERA_METRICS_REQUIRE_AUTH (v0.3.5)
    orchestrator_api.go  /flocks endpoints, SSE Town Wall streaming,
                      restartAgent (per-agent restart endpoint),
                      flock_spawn / flock_destroy counter increments (v0.3.5)
    recovery.go       RecoverVMs (cold-restart) + flock cross-link;
                      markFlockAgentDead persists dead status;
                      restores runningVM.spawnedAt from VMState.CreatedAt (v0.3.5)
    metrics_handler.go   daemonMetrics bundle + handleMetrics (v0.3.5)
    stats_handler.go     /vms/{vm_id}/stats + ?stats=true branch (v0.3.5)
    stats_collector.go   Firecracker PID resolution via /proc/net/unix →
                      /proc/<pid>/fd inode trace, /proc/<pid>/stat CPU sampling,
                      VmRSS, TAP statistics, agent /health probe (v0.3.5)
    context.go        client-identity context keys + request-scoped holder (v0.4.1)
    audit.go          access audit log: rotating jsonl writer, statusRecorder
                      (Flusher-preserving), GET /audit, auditMiddleware (v0.4.1)
    ui.go             Serves the embedded Web UI at /ui/ (go:embed uidist) + SPA
                      fallback + "/" → /ui/ redirect, outside the auth chain (v0.5.0)
    config_api.go     GET /config/providers (registry + keychain availability);
                      GET/POST /config/profiles, GET/PUT/DELETE /config/profiles/{name} —
                      list/create/update/delete a profile's GOOSE_PROVIDER/GOOSE_MODEL
                      (+ optional per-VM vCPU/memory) on disk (v0.5.1);
                      GET/PUT/DELETE /config/profiles/{name}/system — a profile's
                      system.md prompt (≤64 KiB, suffix-routed) (v0.5.4);
                      GET /config/clients — configured API clients (name + expiry,
                      never tokens); GET /config/monitoring — Grafana URL for the
                      embedded Monitoring tab (v0.5.5)
    uidist/           Committed Web UI build (go:embed input; rebuilt from web/, v0.5.0)
  goose-agent/        In-VM HTTP agent (baked into golden image)
    main.go           /tasks (optional `session` → goose -n/--resume for multi-turn, v0.5.0),
                      /health, /stop, /townwall/post  (Bearer token auth);
                      prepends role system prompt to /tasks bodies;
                      runs `goose run --output-format json --no-profile --with-builtin
                      developer` (+ capped GOOSE_MAX_TOKENS, `/nothink` for qwen) and
                      extracts only the latest turn's text via extractGooseJSONText (v0.5.1)
  micro-init/         PID 1 for each MicroVM (baked into golden image)
    main.go           Mounts virtual filesystems, manages goose-agent,
                      calls poweroff(2) on exit
  ephemera-ctl/       Operator CLI — dependency-free stdlib HTTP wrapper (v0.4.1)
    main.go           noun/verb dispatch, EPHEMERA_CTL_URL/_TOKEN, --json
    client.go         HTTP client (Bearer, non-2xx → error) + wire-type mirrors
    commands.go       vm/flock/snapshot/audit/metrics verbs, tabwriter output

web/                  Web UI source — Svelte 4 + Vite 5 SPA (v0.5.0)
  src/
    App.svelte        Shell: bootstrap/auth, nav, EN/KO language toggle, view router
    components/       Login, VMList, SpawnModal (Create VM), VMDetail,
                      TaskPanel (multi-turn conversation), Settings, Toasts
    lib/              api.js (bearer + 401→login), store.js, stream.js (NDJSON),
                      i18n.js (svelte-i18n: EN/KO, browser-detect, persist)
    locales/          en.json, ko.json (all UI strings)
  README.md           UI terminology glossary + i18n / rebuild docs
  package.json        svelte-i18n dep; `npm run build` → ../cmd/goose-daemon/uidist/

internal/
  vm/machine.go       Firecracker SDK wrapper — StartMachine, RestoreMachine
                      (VcpuCount / MemSizeMib are per-call; zero falls back to 1 / 1024)
  network/manager.go  IP pool, TAP device lifecycle, AllocateForRestore,
                      ReclaimAllocation (cold-restart reuse), bridge, NAT
  storage/
    provisioner.go    Golden image bootstrap, disk clone, config/token/flock injection
                      (incl. /root/.ephemera-cp-token via injectVMFiles),
                      CloneDiskCOW (dm-snapshot-backed spawn), artifact download + SHA256
    snapshot.go       Snapshot metadata (read/write), disk copy helpers,
                      SetupDMSnapshot/TeardownDMSnapshot (COW restore via dm-snapshot),
                      MergeMemoryDiff (SEEK_DATA/SEEK_HOLE sparse merge)
    vm_state.go       Per-VM state.json — Save/Load/Delete/List (cold-restart input)
    orphan.go         KillStaleFirecracker + RemoveStaleVMArtifacts + COW device reclaim (cold-restart cleanup)
  orchestrator/
    townwall.go       Per-flock append-only log + subscriber fan-out
    flock.go          Flock + FlockManager (lock-safe JSON via MarshalJSON);
                      Persist (writeMu-serialized metadata write),
                      UpdateAgentVM (per-agent restart swap)
    persistence.go    FlockMetadata Save/Load/Delete/List (raw API;
                      always go through Flock.Persist for live flocks)
    watchdog.go       Per-VM health probing + dead marking;
                      onFailure persists status, ForgetVM clears restart caches;
                      OnDead/OnHeal/OnProbeDuration metric callbacks (v0.3.5)
    handoff.go        Structured JSON handoff between agents
  metrics/            Self-implemented Prometheus exposition formatter (v0.3.5)
    registry.go       Registry + Counter/CounterVec/Gauge/GaugeFunc/Histogram
                      types (atomic, race-safe; zero external dependency)
    exposition.go     Text format 0.0.4 writer — HELP/TYPE/value lines,
                      label-value escaping, histogram bucket + _count + _sum

configs/
  goose.yaml.example             Default provider/model template
  goose-secrets.yaml.example     API key template
  profiles/                      User-defined LLM profiles (created via the Settings UI; empty by default)
    <profile-name>/
      goose.yaml                 (gitignored) provider + model for this profile; API keys come from the global keychain above
      system.md                  Role system prompt prepended to /tasks (optional)
  webdev-demo/                   Host-side vite-template overlaid onto worker output by webdev_demo.sh (v0.3.6)
    vite-template/               package.json, vite.config.js, index.html, src/* placeholders
  observability/                 Provisioning bundle for observability_demo.sh (v0.3.5)
    prometheus.yml               Prometheus scrape config (localhost:3000, 5s)
    grafana-datasource.yml       Prometheus datasource provisioning
    grafana-dashboards.yml       Grafana dashboards-provider provisioning
    dashboards/
      ephemera-overview.json     Pre-built Grafana 10.x dashboard (8 panels)

.github/
  workflows/ci.yml    go build + go vet + go test on push/PR (ubuntu-22.04)

snapshots/            Stored snapshot directories (auto-created, gitignored)
  <snapshot-id>/
    memory.bin        Guest RAM dump — 2 GB (Full) or sparse/small (Diff)
    state.bin         Firecracker hardware state
    rootfs.ext4       Full rootfs copy — Full snapshots only (reflink/sparse, ~570 MB actual)
    rootfs.diff       Sparse rootfs delta vs base — Diff snapshots only (changed 4 KiB blocks)
    metadata.json     Restore params (IP, TAP, MAC, token, type, base_snapshot_id, rootfs_diff_path)

e2e_test.sh           End-to-end integration test (80+ numbered steps incl. resilience + v0.3.x–v0.4.5 sub-steps; requires /dev/kvm + root)
observability_demo.sh One-shot live demo: daemon + Prometheus + Grafana, auto workload, browser-driven exploration until Ctrl-C (v0.3.5)
webdev_demo.sh        One-shot live demo: orchestrator+worker+reviewer flock builds a React+Vite site, harvested from the Town Wall and served via vite preview until Ctrl-C (v0.3.6; manual gate, needs a Gemini key + /dev/kvm)

scripts/
  build_image.sh      Builds golden image (Debian Bookworm + curl + jq + Goose + goose-agent + micro-init + gtwall + gtcall)
  gtwall              In-VM CLI: post a message to the flock's Town Wall (jq-built JSON body; installed into the golden image)
  gtcall              In-VM CLI: dispatch a prompt to a peer agent via the control-plane proxy (v0.3.6; installed into the golden image)

flocks/               Per-flock workspace (auto-created at first flock spawn, gitignored)
  <flock-id>/
    TOWN_WALL.log     Append-only log of agent messages
    metadata.json     Flock recovery state (atomic tmp+rename; schema_version: 1)

vms/                  Per-VM cold-restart state (auto-created on first spawn, gitignored)
  <vm-id>/
    state.json        Network identity, agent token, disk path, profile, flock link

artifacts/            Auto-populated at runtime (gitignored)
  golden-image.ext4   Golden VM disk image
  vmlinux.bin         Firecracker-compatible Linux 6.1 kernel
  firecracker         Firecracker VMM binary (SHA256-verified)
  goose-agent         In-VM HTTP agent binary (compiled from source)
  micro-init          PID 1 init binary (compiled from source)
  prometheus-X.Y.Z.linux-amd64/   Prometheus binary (downloaded by observability_demo.sh, SHA256-pinned, v0.3.5)
  grafana-vX.Y.Z/                 Grafana OSS binary (downloaded by observability_demo.sh, SHA256-pinned, v0.3.5)
```

---

## Prerequisites

| Requirement | Detail |
|-------------|--------|
| **Host OS** | Ubuntu 22.04 or 24.04 (bare metal, or VM with nested virtualization) |
| **CPU** | `/dev/kvm` accessible |
| **Go** | 1.21+ (bumped in v0.3.5 for stdlib `log/slog`) |
| **Packages** | `curl`, `debootstrap`, `e2fsprogs`, `util-linux`, `jq` (e2e + demo), `dmsetup` (snapshot/COW tests) |
| **Privileges** | `sudo` at runtime (KVM + network interface management) |

```bash
sudo apt-get install -y curl debootstrap e2fsprogs util-linux jq dmsetup
```

Firecracker, the Linux kernel, and the golden image are **downloaded and built automatically** on first run.

---

## Getting Started

### 1. Clone and build

```bash
git clone https://github.com/steve-seungeui/ephemera.git
cd ephemera
go build -o ephemera-daemon ./cmd/goose-daemon/
go build -o ephemera-ctl ./cmd/ephemera-ctl/   # operator CLI (v0.4.1)
```

### 2. Configure the default LLM

```bash
cp configs/goose.yaml.example    configs/goose.yaml
cp configs/goose-secrets.yaml.example configs/goose-secrets.yaml
```

Edit `configs/goose.yaml`:

```yaml
GOOSE_PROVIDER: google
GOOSE_MODEL: gemini-2.5-flash
GOOSE_TELEMETRY_ENABLED: false
GOOSE_DISABLE_KEYRING: true   # required — MicroVM has no keyring daemon
```

Edit `configs/goose-secrets.yaml` (**never commit this file**):

```yaml
GOOGLE_API_KEY: "your-key-here"
```

Supported providers: `google` · `anthropic` · `openai` · `ollama` · [others supported by Goose](https://goose-docs.ai/docs/getting-started/providers/).

### 3. Run

```bash
sudo ./ephemera-daemon
```

On first run, Ephemera will:
1. Compile `micro-init` and `goose-agent` binaries
2. Build the golden image via `debootstrap` (~5–8 minutes)
3. Download the Firecracker kernel and binary

Subsequent starts skip these steps if artifacts already exist.

---

## Testing

Ephemera has two levels of testing.

### Unit tests (CI)

Run automatically on every push and pull request via GitHub Actions. No special hardware required.

```bash
go test ./...           # standard
go test -race ./...     # mandatory before merging concurrency-sensitive changes
```

Covers: API token parsing, LLM profile path resolution, agent auth middleware, token generation, Town Wall append/history/seq monotonicity, flock metadata persistence round-trip and disk recovery, watchdog dead-marking under failure thresholds (incl. v0.3.4 Configure + auto-heal tunables), artifact staleness check, per-VM state.json round-trip/sort/idempotent-delete/empty-workdir, **Prometheus registry counters / counter-vecs / gauges / histograms** (race-safe, exposition format spec compliance, label escaping) (v0.3.5), **`/metrics` handler** (content-type, GET-only, default-unauth, counter/gauge reflection) (v0.3.5), **`/vms/{vm_id}/stats` handler** (`/proc/<pid>/stat`+`/status` parsing, TAP statistics, agent-busy probe with timeout, `?stats=true` inline branch) (v0.3.5), **slog handler selection** (TextHandler vs JSONHandler, `EPHEMERA_LOG_LEVEL` gating) (v0.3.5).

### End-to-end test (`e2e_test.sh`)

A full integration test that boots a real daemon, spawns actual Firecracker MicroVMs, and exercises every API endpoint. Requires a host with `/dev/kvm` and root privileges.

```bash
# Build first
go build -o ephemera-daemon ./cmd/goose-daemon/

# Run (takes ~15–30 minutes depending on API rate limits)
sudo bash e2e_test.sh
```

**What it tests (80+ numbered steps incl. sub-steps):**

| Steps | Scenario |
|-------|----------|
| 1–5 | Daemon startup, single VM lifecycle (create → task → stop → delete) |
| 3a | **Streaming `/tasks`** (v0.4.4) — `POST /vms/{id}/tasks?stream=1` through the proxy returns `Content-Type: application/x-ndjson`; every frame is valid JSON and the stream ends with a `result` frame (proxy per-chunk flush + agent NDJSON) |
| 3b | **Task depth guard** (v0.4.4) — an over-cap `/tasks` hop (`X-Ephemera-Task-Depth: 99`) is refused with `508 Loop Detected` before goose is contacted |
| 6–9 | Two VMs in parallel — concurrent task execution |
| 10–16 | Full snapshot lifecycle: create with `stop_after`, list, restore, verify agent token and new IP, delete |
| 17–22 | **Concurrent restore** — two different snapshots restored simultaneously; verifies both VMs run at the same time with independent IPs and disks |
| 23–25 | **Diff snapshot creation** — auto-detection: first snapshot → `full`, second → `diff` with correct `base_snapshot_id` |
| 26 | **Diff size verification** — `stat -c%b` confirms Diff `memory.bin` allocates fewer disk blocks than Full (sparse file) |
| 26b | **Diff rootfs is a sparse delta** (v0.4.0) — Diff snapshot stores `rootfs.diff` (no full `rootfs.ext4`); `stat -c%b` confirms far fewer blocks than the Full snapshot's rootfs |
| 27–29 | Diff snapshot restore — merged memory applied, agent responds, token preserved |
| 30 | **Dependency protection** — deleting the Full base while Diff references it returns `409 Conflict` |
| 31 | Ordered cleanup: delete Diff → delete Full (now unblocked) |
| 32–33 | **COW rootfs** — create VM, take snapshot |
| 34–36 | Restore via dm-snapshot: verify `/dev/mapper/cow-*` device active; exception store initially ≈ 0 MB actual disk usage |
| 37 | Restored agent `/health` responds |
| 38 | Delete restored VM: verify dm device, loop devices, and `.cow` file all cleaned up |
| 39 | Delete snapshot and verify empty |
| 40–46 | **COW spawn cold-restart** (v0.4.0) — relaunch daemon with `EPHEMERA_DISK_MODE=cow`; spawn 2 COW VMs; graceful restart preserves each `.cow` store and re-creates the dm device (same `vm_id`, `/health` 200); then a SIGKILL crash with one `state.json` removed proves `RemoveOrphanCOWDevices` reclaims the orphan while the survivor is cold-restarted; restores plain disk mode |
| 46a–c | **Restored VM auto-recovery** (v0.4.5) — restore a snapshot (its `state.json` records `source_snapshot_id`); a graceful daemon bounce re-restores the VM from its snapshot (back in `/vms`, `/health` 200, log `re-restored from snapshot`); then deleting the source snapshot + bouncing drops the now-unrecoverable VM |
| 47–49 | **Agent proxy** — `GET /vms/{id}/health`, `POST /vms/{id}/stop` via control plane proxy; no direct VM IP access |
| 50–51 | **`EPHEMERA_PUBLIC_URL`** — restart daemon with var set; verify `agent_url` becomes proxy path; use `agent_url` for health + stop |
| 52 | Prep role profile yaml files from `.example` placeholders |
| 53 | **Flock spawn** — `POST /flocks` with 5 roles (orchestrator/researcher×2/worker/reviewer) returns 201, `agents.length == 5`, valid `townwall_url` |
| 54 | `GET /vms` shows all 5 flock members |
| 55 | `POST /flocks/{id}/post` accepts a message and persists it |
| 55a | **In-VM forwarding** — direct `POST $agent_url/townwall/post` (the chain that `gtwall` uses) round-trips through goose-agent → control plane; unauthenticated probe rejected with 401 |
| 56 | `GET /flocks/{id}/wall/history` returns ≥ 3 entries (control-plane init + step 55 + step 55a) and the 55a body (escaped quote + backslash) matches verbatim |
| 56a | **Town Wall query filters** (v0.4.3) — `?agent_id=` returns only that agent's entries; `?contains=` returns only matching bodies |
| 57 | `GET /flocks` lists the new flock |
| 57a–c | **Dynamic agent membership** (v0.4.3) — `POST /flocks/{id}/agents` adds `worker-2` (count→6, `/health` 200); `PATCH …/agents/worker-2` `{role:reviewer}` recreates the VM (vm_id swap, role updated); `DELETE …/agents/worker-2` (count→5, VM torn down) |
| 57d–f | **Pause/resume + max_agents** (v0.4.3) — `POST /flocks/{id}/pause` (members → `paused`; watchdog leaves them alone past its threshold), `/resume` (→ `ready`, `/health` 200); `POST /flocks {roles:3, max_agents:2}` → 400 |
| 57g | **Watchdog status** (v0.4.4) — `GET /watchdog/status` returns 200 with sane config fields (`interval_sec`/`dying_threshold` ≥ 1, `auto_heal` boolean), well-typed state (`vm_fail_counts` object, `vm_dead_marked` array), and an empty dead list on the healthy flock |
| 57h | **Broadcast contract** (v0.4.4) — `POST /flocks/{unknown}/broadcast` → 404; `POST /flocks/{id}/broadcast {body:""}` → 400 (short-circuit paths that do not invoke goose) |
| 58 | **Flock teardown** — `DELETE /flocks/{id}` returns 200; all 5 VMs and the flock registry entry are gone |
| 59 | Create a separate resilience flock (3 agents) |
| 60 | **SSE seq monotonicity** — successive `POST /flocks/{id}/post` responses carry strictly increasing `seq` |
| 61 | **Flock persistence** — `flocks/<id>/metadata.json` exists with correct `flock_id` and `schema_version: 1` |
| 62 | **Recovery setup** — verify `vms/<vm_id>/state.json` for each agent; kill daemon (and Firecrackers); restart with `EPHEMERA_API_ADDR=0.0.0.0:3000`; flock metadata reloaded; Town Wall history preserved with seq continuity |
| 63 | **Cold-restart VM IDs preserved** — every pre-restart `vm_id` reappears in `GET /vms` with same identity |
| 64 | **Recovered VM `/health` responds** — proxy `GET /vms/{id}/health` returns 200 for each cold-restarted member |
| 65 | `DELETE` on a recovered flock removes its `metadata.json` |
| 66 | Daemon log shows the `watchdog started` slog line for each daemon invocation (lowercase since the v0.3.5 slog migration) |
| 67 | **Watchdog persists dead status to disk** (v0.3.3) — kill an in-VM agent, watchdog marks `dead`, `flocks/{id}/metadata.json` on disk reflects `dead` before the next probe (Persist hook fired). Daemon restart is intentionally not part of this step because cold-restart of a healthy guest legitimately re-flips to `ready`. |
| 68 | **Per-agent restart preserves identity + token** (v0.3.3) — `POST /flocks/{id}/agents/{agent_id}/restart` swaps `vm_id`, keeps role/token; new VM's `/townwall/post` accepts the OLD token |
| 69 | Daemon graceful shutdown |
| 70 | **Auth-on CP token auto-injection** (v0.3.3) — restart daemon with `EPHEMERA_API_TOKENS` set; flock VM's `/townwall/post` forward to CP returns 200 without any in-VM env setup |
| 71 | **Real-LLM round-trip** (v0.3.3) — when `GOOGLE_API_KEY` / `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` is in env, spawn researcher, send `/tasks`, verify `ROUNDTRIP_OK` reaches Town Wall via `gtwall`. Skipped (ok) when no key. |
| 71e | **Broadcast fan-out** (v0.4.4, LLM-gated) — `POST /flocks/{id}/broadcast` to the live researcher flock returns 200 with `agents==1`/`sent==1`, `results["researcher-1"].status=="ok"`, and the broadcast notice lands on the Town Wall |
| 72 | **CP token hot rotation via SIGHUP** (v0.3.4) — restart daemon with `EPHEMERA_API_TOKENS_FILE`; spawn flock under v1; edit file to v2 + SIGHUP; verify post-rotation `/townwall/post` still 200 (in-VM `/root/.ephemera-cp-token` rewritten via vsock), v1 operator bearer now 401, and the daemon log carries `msg="sighup: cp token propagated" ok=N total=M` (slog form since v0.3.5). |
| 73 | **`/metrics` endpoint format** (v0.3.5) — `GET /metrics` returns 200 unauthenticated, `Content-Type: text/plain; version=0.0.4`, body contains `# HELP`/`# TYPE` lines plus `ephemera_vm_count` gauge and `ephemera_sighup_reload_total` counter samples. |
| 74 | **Per-VM `/stats` endpoint + `?stats=true`** (v0.3.5) — spawn a VM, `GET /vms/{vm_id}/stats` returns a JSON snapshot with `uptime_seconds ≥ 0`, `mem_total_mib > 0`, numeric `cpu_percent`; `GET /vms?stats=true` inlines the same `stats` block on every VM list entry. |
| 75 | Rotation daemon shutdown |
| 76 | **Memory auto-snapshot warm restore** (v0.4.0) — spawn a VM under `EPHEMERA_AUTOSNAPSHOT=true`; graceful SIGTERM bounce writes `vms/{id}/auto/{memory,state}.bin`; the next start warm-restores it (same `vm_id`, `/health` 200, daemon logs `vm warm-restored` rather than `vm back up`) and deletes the one-shot snapshot. Also gates the `forwardSignals` SIGTERM fix — a forwarded SIGTERM would kill Firecracker mid-snapshot. |
| 77 | **Recovery disk-missing clean drop** (v0.4.0) — spawn a 1-agent flock, SIGKILL the daemon (host TAP lingers), delete the worker's rootfs, restart: `state.json` dropped, VM absent from `/vms`, stale TAP released, flock agent marked `dead` in `metadata.json`, and surfaced via the `vms not cold-restarted` log (not a silent drop). |
| 78 | **Audit log records an authenticated request** (v0.4.1) — auth-on daemon; a valid `GET /vms` (200) appears in `GET /audit` with `client=ops`, and the audit body contains no token/Authorization material. |
| 79 | **Audit captures a 401** (v0.4.1) — a bogus-bearer `GET /vms` is recorded in `/audit` with `status=401`, `client=-`. |
| 80 | **Per-token TTL** (v0.4.1) — `EPHEMERA_API_TOKENS_FILE` with a short-TTL `name:token:expires` token: accepted before expiry (200), rejected after (401) while the never-expiring primary still works; daemon logs `token expired` and `/metrics` shows `ephemera_auth_total{outcome="expired"}`. |
| 81 | **SSE stream survives the audit wrapper** (v0.4.1) — `GET /flocks/{id}/wall` still streams (200) through the audit `statusRecorder`, proving `http.Flusher` is preserved. |
| 82 | **`ephemera-ctl` drives the daemon** (v0.4.1) — `ephemera-ctl vm spawn` / `ls` / `rm` against the live daemon (spawned VM appears then disappears); a bogus `--token` exits non-zero. |
| 83 | **`ephemera-ctl audit`** (v0.4.1) — `ephemera-ctl audit --method GET` returns the access-log entries for the calls just made. |

**Example output (passing, tail of the run — steps 52–83):**

```
━━━ 52. Prep role profile yaml files ━━━
  ✓ Profile yaml files ready

━━━ 53. Create flock with 5 agents ━━━
  ✓ POST /flocks (HTTP 201)
  ✓ Spawned 5 agents in flock flock-1778665945495324840
  ✓ townwall_url: http://localhost:3000/flocks/flock-1778665945495324840/wall

━━━ 54. Verify /vms shows the 5 flock members ━━━
  ✓ Found 5 VM(s) running

━━━ 55. Post a message to the Town Wall ━━━
  ✓ POST /flocks/flock-1778665945495324840/post (HTTP 200)
  ✓ Town Wall accepted the post

━━━ 55a. Post via agent /townwall/post (in-VM forwarding path) ━━━
  ✓ Got agent_token for researcher-1 (64 chars)
  ✓ Resolved private IP for researcher-1: 10.0.1.3
  ✓ POST http://10.0.1.3:8080/townwall/post (HTTP 200)
  ✓ POST /townwall/post without bearer (must be rejected) (HTTP 401)

━━━ 56. Retrieve Town Wall history ━━━
  ✓ Town Wall has 3 entries
  ✓ In-VM /townwall/post entry round-tripped (agent_id+body match) ✓

━━━ 57. Verify GET /flocks lists the new flock ━━━
  ✓ GET /flocks returns 1 entry(ies)

━━━ 58. Delete flock and verify all member VMs are torn down ━━━
  ✓ DELETE /flocks/flock-1778665945495324840 (HTTP 200)
  ✓ All flock VMs torn down
  ✓ Flock unregistered from manager

━━━ 59. Create flock for resilience scenarios ━━━
  ✓ POST /flocks (resilience) (HTTP 201)
  ✓ Resilience flock: flock-1778666301234567890

━━━ 60. Town Wall messages carry monotonic seq ━━━
  ✓ First post has seq=2 ✓
  ✓ Seq monotonic: 2 → 3 ✓

━━━ 61. Flock metadata.json persisted to disk ━━━
  ✓ metadata.json exists at /home/.../flocks/flock-.../metadata.json ✓
  ✓ metadata.json has correct flock_id ✓
  ✓ schema_version == 1 ✓

━━━ 62. Daemon restart recovers flock from disk ━━━
  ✓ VM state.json persisted for all flock members ✓
  ✓ Daemon back up after restart
  ✓ Flock flock-... recovered after daemon restart ✓
  ✓ Town Wall history preserved: 3 entries ✓
  ✓ Recovered history seq 3 ≥ pre-restart seq 3 ✓

━━━ 63. Cold-restart preserves VM IDs ━━━
  ✓ VM IDs unchanged across daemon restart ✓
  ✓ VM vm-... is live in /vms after cold-restart ✓
  ✓ VM vm-... is live in /vms after cold-restart ✓
  ✓ VM vm-... is live in /vms after cold-restart ✓

━━━ 64. Recovered VM /health responds ━━━
  ✓ VM vm-... /health → 200 ✓
  ✓ VM vm-... /health → 200 ✓
  ✓ VM vm-... /health → 200 ✓

━━━ 65. DELETE recovered flock removes metadata.json ━━━
  ✓ DELETE recovered flock (HTTP 200)
  ✓ metadata.json removed after DELETE ✓

━━━ 66. Watchdog start log line present ━━━
  ✓ Watchdog start log line present in 3 daemon run(s) ✓

━━━ 67. Watchdog persists dead status to metadata.json ━━━
  ✓ POST /flocks (watchdog persist) (HTTP 201)
  ✓ Watchdog marked worker-1 dead in ≤30s ✓
  ✓ metadata.json on disk shows status=dead (Persist hook fired) ✓

━━━ 68. Per-agent restart preserves identity and reuses agent_token ━━━
  ✓ POST /flocks (restart test) (HTTP 201)
  ✓ POST .../agents/reviewer-1/restart (HTTP 200)
  ✓ VM ID swapped: vm-1779176432527292612 → vm-1779176434494332773 ✓
  ✓ Restarted agent status reset to ready ✓
  ✓ Role preserved across restart ✓
  ✓ New VM /health → 200 ✓
  ✓ Old agent_token still valid on the new VM (token preserved) ✓

━━━ 69. Shut down daemon ━━━
  ✓ Daemon stopped

━━━ 70. Auth-on daemon spawned for v0.3.3 CP-token scenarios ━━━
  ✓ Auth-on daemon ready

━━━ 70a. In-VM /townwall/post auto-authenticates under auth-on CP ━━━
  ✓ POST /flocks (auth-on) (HTTP 201)
  ✓ In-VM /townwall/post → CP succeeded with auto-injected CP token ✓
  ✓ Town Wall received auth-on forward ✓

━━━ 71. Real-LLM /tasks smoke test ━━━
  ✓ Skipped — set GOOGLE_API_KEY, ANTHROPIC_API_KEY, or OPENAI_API_KEY to run

━━━ 72. Kill auth-on daemon to prep for rotation test ━━━
  ✓ Auth-on daemon stopped

━━━ 72a. Spawn TOKENS_FILE-backed daemon (token=v1) ━━━
  ✓ File-source daemon ready

━━━ 72b. Spawn flock and verify v1 in-VM CP forward ━━━
  ✓ POST /flocks (rotation) (HTTP 201)
  ✓ Pre-rotation /townwall/post via v1 CP token: 200 ✓

━━━ 72c. Edit tokens file + SIGHUP daemon ━━━
  vsock UDS state before SIGHUP:
    srwxr-xr-x 1 root root 0 May 20 15:51 /tmp/firecracker-vsock-vm-1779259880788616345.sock
  ✓ vsock fan-out: 1/1 VMs OK

━━━ 72d. Post-rotation /townwall/post must still succeed (v2 reached VM) ━━━
  ✓ Post-rotation /townwall/post via v2 CP token: 200 ✓

━━━ 72e. v1 operator bearer must now be rejected ━━━
  ✓ v1 operator bearer correctly rejected (401) ✓

━━━ 72f. Town Wall received both pre- and post-rotation posts ━━━
  ✓ Town Wall recorded both posts ✓

━━━ 72g. Cleanup rotation test ━━━
  ✓ Rotation flock deleted, tokens file removed

━━━ 73. /metrics endpoint exposes Prometheus format ━━━
  ✓ GET /metrics returned 200 (unauthenticated by default) ✓
  ✓ HELP line present
  ✓ gauge TYPE present
  ✓ vm_count value present
  ✓ sighup_reload counter present

━━━ 74. /vms/{vm_id}/stats returns per-VM snapshot ━━━
  ✓ stats test VM spawned: vm-...
  ✓ stats schema verified (uptime, mem_total, cpu_percent)
  ✓ ?stats=true inlines per-VM stats ✓
  ✓ stats test VM cleaned up

━━━ 75. Shut down rotation daemon ━━━
  ✓ Rotation daemon stopped

━━━ 76. EPHEMERA_AUTOSNAPSHOT: warm restore preserves VM memory across a graceful daemon bounce ━━━
  ✓ Daemon up with EPHEMERA_AUTOSNAPSHOT=true
  ✓ POST /vms (autosnapshot) (HTTP 201)
  ✓ Agent healthy before bounce ✓
  ✓ auto-snapshot written: vms/<id>/auto/{memory,state}.bin ✓
  ✓ VM <id> live after warm restore ✓
  ✓ warm-restored agent /health → 200 ✓
  ✓ daemon took warm-restore path (memory preserved, not cold boot) ✓
  ✓ auto-snapshot consumed (one-shot delete) ✓
  ✓ metric ephemera_auto_restore_total{ok} present ✓
  ✓ Auto-snapshot test VM cleaned up

━━━ 77. Recovery with a missing disk artifact drops state cleanly (TAP released, agent dead, surfaced) ━━━
  ✓ Plain-mode daemon up for disk-missing recovery test
  ✓ POST /flocks (disk-missing) (HTTP 201)
  ✓ worker state.json persisted ✓
  ✓ host TAP present before crash ✓
  ✓ Crashed daemon; deleted worker rootfs ✓
  ✓ state.json dropped on recovery ✓
  ✓ dropped VM absent from /vms ✓
  ✓ stale TAP released by recovery ✓
  ✓ flock agent worker-1 marked dead in metadata.json ✓
  ✓ daemon logged disk-missing drop ✓
  ✓ drop surfaced in failed[] (vms not cold-restarted) ✓
  ✓ Disk-missing flock cleaned up

━━━ 78. Audit log records an authenticated request (client + status, no secrets) ━━━
  ✓ Auth-on daemon up (client: ops)
  ✓ GET /vms with valid bearer (HTTP 200)
  ✓ GET /vms with bogus bearer (HTTP 401)
  ✓ audit recorded GET /vms 200 by client=ops ✓
  ✓ audit log contains no token/Authorization material ✓

━━━ 79. Audit captures a 401 as client=- ━━━
  ✓ audit recorded the 401 with client=- ✓

━━━ 80. Per-token TTL: an expired token is rejected; never-expiring primary keeps working ━━━
  ✓ Auth-on daemon up with a short-TTL token
  ✓ short-TTL token accepted before expiry (HTTP 200)
  ✓ short-TTL token rejected after expiry (HTTP 401)
  ✓ never-expiring primary token still accepted (HTTP 200)
  ✓ daemon logged token expiry ✓
  ✓ metric ephemera_auth_total{outcome=expired} present ✓

━━━ 81. SSE /flocks/{id}/wall streams through the audit statusRecorder (Flusher preserved) ━━━
  ✓ SSE guard flock: flock-...
  ✓ GET /flocks/{id}/wall streamed (200; Flusher preserved through audit wrapper) ✓

━━━ 82. ephemera-ctl spawn/ls/rm against the live daemon ━━━
  ✓ ctl vm spawn → vm-... ✓
  ✓ ctl vm ls shows the spawned VM ✓
  ✓ ctl vm rm removed it from ls ✓
  ✓ ctl bogus token → non-zero exit ✓

━━━ 83. ephemera-ctl audit reads the access log ━━━
  ✓ ctl audit shows recent GET /vms ✓

══════════════════════════════════
  All test steps passed ✓
══════════════════════════════════
```

---

## Configuration

All settings are read from environment variables at startup.

| Variable | Default | Description |
|----------|---------|-------------|
| `EPHEMERA_API_ADDR` | `127.0.0.1:3000` | Control plane bind address. Set to `0.0.0.0:3000` when behind a reverse proxy, or when using flocks: the in-VM `gtwall` / `/townwall/post` forwarder targets `http://10.0.1.1:3000` (the bridge gateway), which is unreachable with the loopback-only default. |
| `EPHEMERA_API_PORT` | `3000` | Port only (used when `EPHEMERA_API_ADDR` is not set). |
| `EPHEMERA_API_TOKENS_FILE` | *(unset)* | Path to a file containing `name:token[:expires]` entries (comma- or newline-separated). When set, **takes precedence over `EPHEMERA_API_TOKENS`** and is re-read on every `loadAPIClients()` call — which is what enables SIGHUP-driven hot rotation since env values are fixed at exec (v0.3.4). The optional `:expires` (RFC3339 or Unix seconds) enforces a per-token TTL (v0.4.1). |
| `EPHEMERA_API_TOKENS` | *(unset)* | Per-client Bearer tokens: `alice:token1,bob:token2` (each entry may carry an optional `:expires`, see `_TOKENS_FILE` and the Token TTL docs — v0.4.1). The first **non-expired** token is also auto-injected into every flock VM at `/root/.ephemera-cp-token` so the in-VM `/townwall/post` forwarder can call back to the control plane without manual setup (v0.3.3; first-non-expired since v0.4.1). v0.3.4 SIGHUP fan-out propagates rotations to running VMs — see `_TOKENS_FILE` for true hot rotation. |
| `EPHEMERA_API_TOKEN` | *(unset)* | Single Bearer token (backward-compatible fallback). |
| `EPHEMERA_AGENT_PORT` | `8080` | Port goose-agent listens on inside each VM. |
| `EPHEMERA_MAX_TASK_DEPTH` | `5` | Max nested agent→agent `/tasks` hops (v0.4.4). The proxy reads `X-Ephemera-Task-Depth` per hop, rejects at/over this cap with `508 Loop Detected`, and forwards `depth+1`. A large value effectively disables the guard. |
| `EPHEMERA_PUBLIC_URL` | *(unset)* | Externally-reachable base URL of the control plane (no trailing slash). When set, `agent_url` in VM responses uses the proxy path `{EPHEMERA_PUBLIC_URL}/vms/{vm_id}` instead of the VM's private IP. Example: `https://api.example.com`. |
| `EPHEMERA_GRAFANA_URL` | *(unset)* | Base URL of a Grafana instance to embed in the Web UI's **System → Monitoring** tab (v0.5.5). `observability_demo.sh` sets it to `http://localhost:3001`; when unset, the tab shows a "not configured" notice. Grafana must permit embedding (`allow_embedding=true` + an anonymous Viewer — the demo wires both). |
| `EPHEMERA_DISK_MODE` | `cow` | Spawn disk strategy. **COW (a dm-snapshot view of the golden image, ~0 MiB initial usage) is the default since v0.4.2.** Set to `plain` (or `full`) to force a 700 MiB full copy. When COW is selected but the host lacks dm-snapshot support (`losetup`/`dmsetup`/`dm_snapshot`), the daemon logs a warning and falls back to plain at startup. Auto-recovered across a daemon restart since v0.4.0. |
| `EPHEMERA_DISK_MIN_FREE_MIB` | `1024` | Free-space floor (MiB) enforced before a VM clone or snapshot writes to disk (v0.4.0). A `statfs` pre-flight estimates the footprint (clone / full snapshot ≈ rootfs + memory; diff snapshot ≈ memory only) and returns `507 Insufficient Storage` when the result would drop free space below this margin, rather than failing mid-write. |
| `EPHEMERA_AUTOSNAPSHOT` | `false` | When `true` (`1`/`yes`/`on` also accepted), the daemon snapshots each recoverable VM's memory+state into `vms/<id>/auto/` on **graceful** shutdown, and the next start **warm-restores** it (in-flight agent work survives a daemon bounce) instead of cold-booting (v0.4.0). One-shot per shutdown; on any restore failure it falls back to cold boot. Requires a graceful shutdown — a SIGKILL/crash cold-boots as before. A 5-agent flock snapshot is ~10 GB, so default off. |
| `EPHEMERA_WATCHDOG_INTERVAL_SEC` | `5` | Watchdog poll cadence (v0.3.4). |
| `EPHEMERA_WATCHDOG_TIMEOUT_SEC` | `1` | Watchdog per-probe HTTP timeout (v0.3.4). Clamped: `interval` is bumped up to `timeout` if smaller. |
| `EPHEMERA_WATCHDOG_THRESHOLD` | `3` | Consecutive probe failures before marking an agent `dead` (v0.3.4). |
| `EPHEMERA_WATCHDOG_AUTO_HEAL` | `false` | When `true` (`1`/`yes`/`on` also accepted), a `dead` agent that resumes responding is auto-marked `ready` and a recovery notice posted to the Town Wall (v0.3.4). Default off preserves sticky-dead. |
| `EPHEMERA_METRICS_REQUIRE_AUTH` | `false` | When `true`, `GET /metrics` requires a valid Bearer token like every other endpoint (v0.3.5). Default off matches the standard Prometheus scrape pattern; flip on when the metrics endpoint is exposed beyond a trusted network. |
| `EPHEMERA_LOG_FORMAT` | `text` | `text` (default) emits `key=value` lines from `log/slog`'s TextHandler; `json` switches to JSONHandler for log-aggregation pipelines (v0.3.5). The in-VM `goose-agent` honors the same variable since v0.4.4 (when injected into the VM environment). |
| `EPHEMERA_LOG_LEVEL` | `warn` | Minimum slog level: `debug`, `info`, `warn`, or `error` (v0.3.5). Default `warn` preserves the previous `log.Printf` tone — every lifecycle event in the daemon is emitted at warn-or-higher so operators see it without configuration. `goose-agent` adopted `log/slog` with the same default in v0.4.4. |
| `EPHEMERA_AUDIT_DISABLE` | `false` | Set to `true` to turn off the access audit log (v0.4.1). When enabled (the default), every API request is appended as one JSON line to `{workDir}/audit/access.jsonl` (method, path, client name, status, latency — never tokens or bodies) and is queryable via `GET /audit`. |
| `EPHEMERA_AUDIT_MAX_MIB` | `100` | Active audit file size (MiB) that triggers rotation to `access.jsonl.1` (v0.4.1). |
| `EPHEMERA_AUDIT_KEEP` | `5` | Number of rotated audit files to retain; older ones are deleted (v0.4.1). Disk ceiling ≈ `MAX_MIB × (KEEP + 1)`. |
| `EPHEMERA_TOWNWALL_MAX_MIB` / `_KEEP` | `10` / `3` | Town Wall log size-based rotation (v0.4.3): once the active `TOWN_WALL.log` passes `MAX_MIB` it shifts to `.1`…`.KEEP` and a fresh file continues. `GET /flocks/{id}/wall/history` reflects the active file. |
| `EPHEMERA_CTL_URL` | `http://127.0.0.1:3000` | Base URL the `ephemera-ctl` operator CLI dials (v0.4.1). Not derived from `EPHEMERA_API_ADDR` — that is a bind address and `0.0.0.0` is not dialable. |
| `EPHEMERA_CTL_TOKEN` | *(unset)* | Bearer token for `ephemera-ctl`; falls back to `EPHEMERA_API_TOKEN`. A `--token` flag overrides both (v0.4.1). |

`EPHEMERA_API_ADDR` takes precedence over `EPHEMERA_API_PORT`. Most variables are read at startup; use SIGHUP to reload tokens. With `EPHEMERA_API_TOKENS_FILE` SIGHUP also propagates the first non-expired client's token to running VMs via vsock (v0.3.4; first-non-expired since v0.4.1).

---

## Operator CLI (`ephemera-ctl`) (v0.4.1)

`ephemera-ctl` is a dependency-free (stdlib) HTTP wrapper over the control-plane
API for day-to-day operations. Build it with `go build -o ephemera-ctl ./cmd/ephemera-ctl/`.
It reads `EPHEMERA_CTL_URL` (default `http://127.0.0.1:3000`) and a bearer token
from `--token` / `EPHEMERA_CTL_TOKEN` / `EPHEMERA_API_TOKEN`. Add `--json` to any
command for raw JSON (default output is a human-readable table).

```bash
export EPHEMERA_CTL_TOKEN=$OPS_TOKEN          # if the daemon has auth enabled

ephemera-ctl vm spawn [--profile NAME]        # → vm_id, guest_ip, agent_url, agent_token
ephemera-ctl vm ls [--stats]                  # vm rm|health|stop|stats <id>; vm task <id> "<prompt>"
ephemera-ctl vm snapshot <id> [--stop-after] [--type full|diff]

ephemera-ctl flock create --task "build X" --roles orchestrator,worker,reviewer
ephemera-ctl flock ls | get <id> | rm <id>
ephemera-ctl flock post <id> --agent worker-1 --body "msg"
ephemera-ctl flock wall <id> [--history]      # stream Town Wall SSE, or print history
ephemera-ctl flock restart <id> <agent_id>
ephemera-ctl flock add-agent <id> <role>      # v0.4.3: add / remove / role-change
ephemera-ctl flock rm-agent <id> <agent_id>
ephemera-ctl flock set-role <id> <agent_id> <role>
ephemera-ctl flock pause <id> | resume <id>   # v0.4.3: pause/resume all members

ephemera-ctl snapshot ls | restore <id> | rm <id>
ephemera-ctl audit [--limit N] [--client C] [--status S] [--method M]
ephemera-ctl metrics                          # raw Prometheus exposition
```

Non-2xx responses print the server's JSON error to stderr and exit non-zero, so
the CLI composes in scripts. Global flags (`--json`, `--token`) may appear
anywhere; command-specific flags precede positional arguments.

---

## Web UI (v0.5.0)

A browser console served by the daemon itself — one stop from system management to agent usage. It is served at **`/ui/`** on the same address as the API (`EPHEMERA_API_ADDR`, default `127.0.0.1:3000`), so no extra process or port is needed:

```
http://localhost:3000/ui/      ← "/" also redirects here
```

The UI is a Svelte + Vite single-page app embedded into the daemon binary via `go:embed` (`cmd/goose-daemon/ui.go`). Its build output (`cmd/goose-daemon/uidist/`) is **committed**, so `go build` needs no Node toolchain; rebuild it only after editing `web/`:

```bash
cd web && npm install && npm run build   # writes ../cmd/goose-daemon/uidist/
```

`/ui/` is mounted **outside** the auth/audit chain — the login page and JS bundle must load before the user has a token, and the bundle carries no secrets — while every data API call the app makes still flows through Bearer auth.

### Screens

- **Login** — takes an API Bearer token (`sessionStorage`, or `localStorage` with "remember"). If the server has no clients configured (auth disabled), login is auto-skipped.
- **VM list** — `GET /vms?stats=true` (polled): id, IP, profile, **model**, CPU/memory/uptime. *Create VM* opens a modal with a **profile dropdown** (`GET /config/profiles`) and shows the one-time `agent_token`.
- **VM detail** — live stats + the spawned provider/model; a **conversation** panel that streams each turn (`POST /vms/{id}/tasks?stream=1`) and keeps context across turns (multi-turn, below), with Cancel + elapsed time; a **Snapshots** section to capture Full/Diff snapshots (optionally stop-after, v0.5.1); and a Delete action behind an in-app confirm (graceful teardown).
- **Settings** — lists every profile, edits its provider/model (`PUT /config/profiles/{name}`), edits each profile's **system prompt** in a modal (`GET/PUT/DELETE /config/profiles/{name}/system`, v0.5.4), and **creates** profiles with a name + provider/model + **per-VM vCPU/memory** (`POST /config/profiles`, v0.5.1); changes apply to the **next** Create VM, not running VMs.
- **Snapshots** — lists stored snapshots (`GET /snapshots`: type, base, created), **restores** one into a new VM (`POST /snapshots/{id}/restore`), and deletes — each behind a confirm modal (v0.5.1).
- **Orchestration** — lists Agent Groups (`GET /flocks`) and creates them (a task + one role per VM; the one-time `agent_token` per agent is shown once). **Group detail** shows the agent table with per-agent **send task** (a one-shot prompt to a single agent, the targeted counterpart to broadcast, v0.5.4) / **restart** / **remove** / **change role**, group **pause** / **resume**, **add agent**, **broadcast**, and **delete**, plus a live **Activity Feed** that streams the Town Wall over SSE (`GET /flocks/{id}/wall`, read via `fetch` + `streamSSE` since `EventSource` can't send the bearer) with an **operator**-authored post composer (v0.5.2; the control plane's own lifecycle notices are authored `control-plane`, v0.5.4).
- **System** — a read-only operations console (single nav tab, four sub-tabs): **Audit log** (`GET /audit` with method/client/status filters), **Watchdog** (`GET /watchdog/status` — tunables + per-VM fail counts + dead-marked list), **Configured clients** (`GET /config/clients` — each client's name + token expiry; **tokens are never shown**), and **Monitoring** (an embedded Grafana dashboard iframe via `GET /config/monitoring`; shown when `EPHEMERA_GRAFANA_URL` is set, e.g. by `observability_demo.sh`) (v0.5.5).

### Localization (EN / KO)

All UI strings live in `web/src/locales/{en,ko}.json` and render via `svelte-i18n`. The initial language follows the browser (`ko*` → Korean, otherwise English); a nav toggle (`EN | 한국어`) switches it and persists the choice in `localStorage`. Server-originated error text (the daemon's `{"error":…}`) is shown verbatim, not translated. The UI uses generic IT vocabulary — *Platform Agent*, *Agent Group*, *Activity Feed*, *Create/Delete* — as display labels only; API routes/fields/env vars keep their original identifiers (see `web/README.md`).

### Multi-turn conversation

The conversation panel sends an optional `session` on `POST /vms/{id}/tasks`. When present, `goose-agent` runs `goose run --output-format json --no-profile --with-builtin developer -n <session> [--resume] -i -` — the first turn creates the named session, later turns `--resume` it — so the agent keeps conversation context across turns (stored in the VM's goose session db). Omitting `session` preserves the original stateless one-shot behavior used by `ephemera-ctl` and `gtcall`.

> **v0.5.1 agent hardening.** goose-agent now (a) loads **only** the `developer` builtin extension (`--no-profile --with-builtin developer`) and caps `GOOSE_MAX_TOKENS`, so a single request fits tight provider token-per-minute budgets — e.g. Groq's free tier, where the full default toolset otherwise overflows and the 429/413 is misreported as a context overflow; (b) returns **only the latest turn's reply**, slicing goose's whole-transcript `--resume` output to the last user message (fixes the multi-turn "accumulating output" bug); and (c) prepends **`/nothink`** for qwen reasoning models, since goose replays their `reasoning_content` on resume and Groq rejects it with a 400. The qwen workaround is partial — very long multi-turn sessions can still flake; a non-reasoning model is the robust choice there.

---

## API Reference

### Control Plane API (`localhost:3000`)

All endpoints require `Authorization: Bearer <token>` when tokens are configured.

---

#### Spawn a VM

```
POST /vms
Content-Type: application/json

{ "profile": "anthropic" }   ← optional; omit to use default config
```

```bash
curl -X POST http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"profile": "anthropic"}'
```

```json
{
  "vm_id":       "vm-1778227813435",
  "guest_ip":    "10.0.1.10",
  "agent_url":   "http://10.0.1.10:8080",
  "profile":     "anthropic",
  "agent_token": "3f9a2c..."
}
```

Blocks until `goose-agent` is ready (~60 s cold boot). `agent_token` is returned **only here** — store it, as it cannot be retrieved again.

#### List VMs

```bash
curl http://localhost:3000/vms -H "Authorization: Bearer $TOKEN"
```

#### Delete a VM

```bash
curl -X DELETE http://localhost:3000/vms/vm-1778227813435 \
  -H "Authorization: Bearer $TOKEN"
```

---

#### Create a snapshot

Freeze the running VM's memory state to disk.

```
POST /vms/{vm_id}/snapshot
Content-Type: application/json

{
  "stop_after": false,   ← optional; true = destroy VM after snapshot (migration mode)
  "type": ""             ← optional; "full" | "diff" | "" (auto, default)
}
```

**Snapshot type auto-detection** (`type` omitted or `""`):

| Condition | Result |
|-----------|--------|
| No prior Full snapshot for this VM | `full` — captures all 2 GB of guest RAM |
| Prior Full snapshot exists | `diff` — captures only dirty pages since the last Full (sparse file, typically much smaller) |

```bash
# First snapshot → Full (auto)
curl -X POST http://localhost:3000/vms/vm-1778227813435/snapshot \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json"

# Second snapshot → Diff (auto, references the Full above)
curl -X POST http://localhost:3000/vms/vm-1778227813435/snapshot \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json"
```

```json
{
  "snapshot_id":      "snap-1778227847573",
  "source_vm_id":     "vm-1778227813435",
  "profile":          "anthropic",
  "snapshot_type":    "diff",
  "base_snapshot_id": "snap-1778227840000",
  "created_at":       "2026-05-08T08:10:50Z"
}
```

Snapshot files are written to `snapshots/<snapshot_id>/`. For a Diff snapshot the `memory.bin` is a sparse file — only dirty pages consume actual disk blocks.

#### List snapshots

```bash
curl http://localhost:3000/snapshots -H "Authorization: Bearer $TOKEN"
```

#### Restore a VM from snapshot

```bash
curl -X POST http://localhost:3000/snapshots/snap-1778227847573/restore \
  -H "Authorization: Bearer $TOKEN"
```

```json
{
  "vm_id":              "vm-1778227851562",
  "guest_ip":           "10.0.1.10",
  "agent_url":          "http://10.0.1.10:8080",
  "profile":            "anthropic",
  "agent_token":        "3f9a2c...",
  "source_snapshot_id": "snap-1778227847573"
}
```

Restoration takes ~5 s (vs ~60 s cold boot). The `agent_token` is identical to the original VM's token — existing clients continue to work without reconfiguration.

**Restore constraints:**
- The original guest IP must be available (not in use by another VM)
- Same-snapshot concurrent restores are not supported — the vsock UDS path is fixed in `state.bin` and would collide. Different-snapshot concurrent restores work correctly.

#### Delete a snapshot

```bash
curl -X DELETE http://localhost:3000/snapshots/snap-1778227847573 \
  -H "Authorization: Bearer $TOKEN"
```

> **Dependency rule**: A Full snapshot that is the base for one or more Diff snapshots cannot be deleted (returns `409 Conflict`). Delete all referencing Diff snapshots first.

---

### Per-VM Stats (v0.3.5)

```
GET /vms/{vm_id}/stats
```

Returns a point-in-time snapshot of host-observable VM stats. Authentication uses the **control plane Bearer token**.

```json
{
  "vm_id": "vm-1778227000000",
  "cpu_percent": 12.4,
  "mem_used_mib": 187,
  "mem_total_mib": 1024,
  "uptime_seconds": 312,
  "network_rx_bytes": 12849,
  "network_tx_bytes": 5328,
  "agent_busy": false
}
```

| Field | Source |
|-------|--------|
| `cpu_percent` | 100 ms sample of `/proc/<firecracker_pid>/stat` (utime+stime). 100 = one full host core. |
| `mem_used_mib` | `VmRSS:` line of `/proc/<firecracker_pid>/status`. |
| `mem_total_mib` | VM spawn sizing (mirrors `VMState.MemSizeMib`). |
| `uptime_seconds` | `time.Since(spawned_at)` — `VMState.CreatedAt` for recovered VMs. |
| `network_rx_bytes`, `network_tx_bytes` | `/sys/class/net/<tap>/statistics/{tx,rx}_bytes`, swapped to VM perspective. |
| `agent_busy` | 1 s `GET /health` against the in-VM agent; `true` when `status == "busy"`. |

Per-VM stats failures (firecracker PID not resolvable, `/proc` race, agent unreachable) degrade fields to zero and emit a slog `Warn`; the endpoint still returns 200 so dashboards see partial data instead of intermittent errors.

For bulk dashboards, `GET /vms?stats=true` returns the standard `[]VMInfo` list with an embedded `stats` field on each element.

> The endpoint emits a snapshot. Streaming (`text/event-stream`) is on the v0.4.3 roadmap.

---

### Metrics (v0.3.5)

```
GET /metrics
```

Returns the control plane's Prometheus exposition payload (text format version 0.0.4). Unauthenticated by default; set `EPHEMERA_METRICS_REQUIRE_AUTH=true` to require a Bearer token like the other endpoints.

Exposed series (additive — never breaks the wire format on minor bumps):

| Family | Type | Labels | Notes |
|--------|------|--------|-------|
| `ephemera_vm_spawn_total` | counter | `outcome=ok\|fail` | every `spawnVMInternal` exit |
| `ephemera_vm_destroy_total` | counter | `outcome=ok` | `destroyVM` after teardown |
| `ephemera_snapshot_create_total` | counter | `type=full\|diff` | success path of `createSnapshot` |
| `ephemera_snapshot_restore_total` | counter | `outcome` | dm-snapshot and bind-mount fallback both contribute |
| `ephemera_auto_snapshot_total` | counter | `outcome=ok\|fail` | graceful-shutdown memory auto-snapshot (`EPHEMERA_AUTOSNAPSHOT`, v0.4.0) |
| `ephemera_auto_restore_total` | counter | `outcome=ok\|fail` | recovery warm-restore attempt (v0.4.0) |
| `ephemera_auth_total` | counter | `outcome=ok\|denied\|expired` | per-request API auth decision (v0.4.1) |
| `ephemera_flock_spawn_total` / `_destroy_total` | counter | — | success path of `createFlock` / `deleteFlock` |
| `ephemera_watchdog_dead_total` / `_heal_total` | counter | — | dyingThreshold and autoHeal transitions |
| `ephemera_sighup_reload_total` | counter | — | after `ReloadClients` completes |
| `ephemera_cp_token_propagated_total` | counter | `outcome` | per-VM vsock fan-out result |
| `ephemera_vm_count` / `_flock_count` / `_snapshot_count` / `_api_clients_count` | gauge | — | re-read on each scrape (GaugeFunc) |
| `ephemera_vm_spawn_duration_seconds` | histogram | — | wall-clock spawn time |
| `ephemera_snapshot_restore_duration_seconds` | histogram | — | wall-clock restore time |
| `ephemera_watchdog_probe_duration_seconds` | histogram | — | per-probe `/health` duration |

---

### Access Audit Log (v0.4.1)

```
GET /audit?limit=100&client=alice&status=200&method=GET
Authorization: Bearer <token>
```

Returns the most recent access-log records (newest first) as a JSON array. All
query params are optional: `limit` (default 100, max 1000), `client`, `status`,
`method`.

```json
[
  { "ts": "2026-05-27T06:11:05Z", "client": "alice", "method": "GET",
    "path": "/vms", "status": 200, "duration_ms": 3, "remote_addr": "127.0.0.1:54xxx", "bytes": 412 }
]
```

Records are appended to `{workDir}/audit/access.jsonl` (size-rotated; see the
`EPHEMERA_AUDIT_*` env vars) and **never contain tokens, the `Authorization`
header, request/response bodies, or the query string**. Unauthenticated requests
are logged with `client` = `"-"`; `/metrics` is excluded. This endpoint is
authenticated (and is itself audited). See also [Access audit log](#access-audit-log-v041) under Security.

---

### Agent Proxy (via Control Plane)

The control plane proxies the three agent endpoints, making them accessible to external clients without direct access to the private VM subnet. Authentication uses the **control plane Bearer token** — the agent token is injected internally.

```
POST /vms/{vm_id}/tasks    → proxied to goose-agent /tasks
GET  /vms/{vm_id}/health   → proxied to goose-agent /health  (no auth required)
POST /vms/{vm_id}/stop     → proxied to goose-agent /stop
```

> The agent's `/townwall/post` is **not** proxied. It is an in-VM convenience used by the bundled `gtwall` CLI, which already has the flock context. External callers should `POST /flocks/{id}/post` directly — they already know the flock ID and can pick the `agent_id` themselves.

When `EPHEMERA_PUBLIC_URL` is configured, `agent_url` in VM responses points directly to the proxy base (`{EPHEMERA_PUBLIC_URL}/vms/{vm_id}`), so clients can use it as-is:

```bash
export EPHEMERA_PUBLIC_URL=https://api.example.com
# agent_url in POST /vms response will be: https://api.example.com/vms/vm-...

curl -X POST "$AGENT_URL/tasks" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Check system environment."}'
```

Without `EPHEMERA_PUBLIC_URL`, `agent_url` still contains the private IP, but the proxy paths (`/vms/{vm_id}/tasks` etc.) are always available on the control plane regardless.

---

### Flock API (Multi-Agent Orchestration)

A **flock** is one `POST /flocks` call that spawns one VM per requested role and registers them under a shared flock ID, all sharing a Town Wall log. Each role string is used directly as a profile name.

> Every agent spawns at default sizing (1 vCPU / 1024 MiB). A role uses `configs/profiles/{role}/goose.yaml` (provider/model) and `system.md` (prompt) when that directory exists; otherwise it falls back to the default config. API keys always come from the global `configs/goose-secrets.yaml`.

#### Spawn a flock

```
POST /flocks
Content-Type: application/json

{
  "task": "Add dark mode toggle to login page",
  "roles": ["orchestrator","researcher","researcher","worker","reviewer"]
}
```

`profiles[]` is an optional array **parallel to `roles[]`** (v0.5.3): `profiles[i]` is the config profile (sizing / model / system prompt) for `roles[i]`. An empty or omitted entry falls back to the role name as the profile (back-compat), so existing callers are unchanged. This lets a logical role label (e.g. `frontend`) differ from the profile it runs (e.g. `worker`); the chosen profile is preserved across restart / change-role / add-agent.

```bash
curl -X POST http://localhost:3000/flocks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "task":"Add dark mode toggle to login page",
        "roles":["orchestrator","researcher","worker"],
        "profiles":["fast-orchestrator","cheap-researcher","worker"]
      }'
```

```json
{
  "flock_id":     "flock-1778665945495324840",
  "task":         "Add dark mode toggle to login page",
  "agents": [
    { "agent_id":"orchestrator-1","role":"orchestrator","vm_id":"vm-...","agent_url":"http://10.0.1.2:8080","status":"ready" },
    { "agent_id":"researcher-1",  "role":"researcher",  "vm_id":"vm-...","agent_url":"http://10.0.1.3:8080","status":"ready" },
    { "agent_id":"researcher-2",  "role":"researcher",  "vm_id":"vm-...","agent_url":"http://10.0.1.4:8080","status":"ready" },
    { "agent_id":"worker-1",      "role":"worker",      "vm_id":"vm-...","agent_url":"http://10.0.1.5:8080","status":"ready" },
    { "agent_id":"reviewer-1",    "role":"reviewer",    "vm_id":"vm-...","agent_url":"http://10.0.1.6:8080","status":"ready" }
  ],
  "agent_tokens": {
    "orchestrator-1": "3f9a2c...",
    "researcher-1":   "8b1e74...",
    "researcher-2":   "c0d51a...",
    "worker-1":       "2147ef...",
    "reviewer-1":     "9a36b8..."
  },
  "townwall_url": "http://localhost:3000/flocks/flock-1778665945495324840/wall",
  "post_url":     "http://localhost:3000/flocks/flock-1778665945495324840/post"
}
```

`agent_tokens` is returned **only here** — store it. Each token authenticates direct calls to that agent's `agent_url` endpoints (e.g. `agent_url/townwall/post`, `agent_url/tasks`) and is identical to the value injected at `/root/.ephemera-agent-token` inside the VM.

If any VM fails to spawn, every VM spawned so far is torn down and the flock is removed before the error response — partial flocks are never left running. The max flock size is **20** to bound IP-pool / TAP exhaustion.

#### Post to the Town Wall

```bash
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/post \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"researcher-1","body":"Found existing dark mode CSS variables"}'
```

Inside a flock VM, the same effect can be achieved via the bundled `gtwall` CLI (typically invoked by the goose-agent's `/tasks` execution context — there is no SSH into the guest):

```bash
gtwall "Claiming src/styles/theme.css"
```

`gtwall` reads `/root/.ephemera-flock` for the flock context, reads `/root/.ephemera-agent-token` for auth, then calls the in-VM `goose-agent /townwall/post`, which forwards to the control plane's `/flocks/{id}/post`. For this forward to succeed, the control plane must be reachable on the bridge gateway IP `10.0.1.1` — start the daemon with `EPHEMERA_API_ADDR=0.0.0.0:3000` (see [Configuration](#configuration)).

#### Stream the Town Wall (SSE)

```bash
curl -N http://localhost:3000/flocks/$FLOCK_ID/wall \
  -H "Authorization: Bearer $TOKEN"
# data: {"timestamp":"2026-05-13T...","agent_id":"orchestrator","body":"Flock spawned with 5 agents..."}
# data: {"timestamp":"2026-05-13T...","agent_id":"researcher-1","body":"Found existing dark mode CSS variables"}
# ...
```

The stream begins with the full history, then keeps the connection open and emits each subsequent `POST /post` as it happens.

#### Town Wall history (one-shot)

```bash
curl http://localhost:3000/flocks/$FLOCK_ID/wall/history \
  -H "Authorization: Bearer $TOKEN"
# Filters (v0.4.3, combinable): ?agent_id=worker-1 · ?since= / ?until= (RFC3339) · ?contains=text
curl "http://localhost:3000/flocks/$FLOCK_ID/wall/history?agent_id=worker-1&contains=build" \
  -H "Authorization: Bearer $TOKEN"
```

```json
[
  { "timestamp":"...","agent_id":"orchestrator","body":"Flock spawned with 5 agents: [...]" },
  { "timestamp":"...","agent_id":"researcher-1","body":"Found existing dark mode CSS variables" }
]
```

#### List flocks

```bash
curl http://localhost:3000/flocks -H "Authorization: Bearer $TOKEN"
```

#### Describe a flock

```bash
curl http://localhost:3000/flocks/$FLOCK_ID -H "Authorization: Bearer $TOKEN"
```

#### Add, remove, or change an agent (v0.4.3)

```bash
# Add an agent — role-N id auto-assigned, agent_token returned once (20-agent cap)
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/agents \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"role":"worker"}'

# Change an agent's role — VM recreated under the new role (agent_id + token kept)
curl -X PATCH http://localhost:3000/flocks/$FLOCK_ID/agents/worker-2 \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"role":"reviewer"}'

# Remove an agent — VM torn down; removing the last agent leaves an empty flock
curl -X DELETE http://localhost:3000/flocks/$FLOCK_ID/agents/worker-2 \
  -H "Authorization: Bearer $TOKEN"
```

#### Pause or resume a flock (v0.4.3)

```bash
# Pause all members (runtime-only — a daemon restart brings them back running)
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/pause -H "Authorization: Bearer $TOKEN"
# Resume all members
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/resume -H "Authorization: Bearer $TOKEN"
```

> A per-flock agent cap is set at creation: `POST /flocks {"…", "max_agents": N}` (default 20), enforced on create and on add.

#### Tear down a flock

```bash
curl -X DELETE http://localhost:3000/flocks/$FLOCK_ID \
  -H "Authorization: Bearer $TOKEN"
# {"status":"deleted","flock_id":"flock-..."}
```

Destroys every member VM in parallel and removes the flock from the registry. The Town Wall log on disk (`flocks/<id>/TOWN_WALL.log`) is left in place as an audit artifact.

#### Restart a single agent (v0.3.3)

```bash
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/agents/reviewer-1/restart \
  -H "Authorization: Bearer $TOKEN"
# {"vm_id":"vm-...","guest_ip":"10.0.1.7","agent_url":"http://10.0.1.7:8080","profile":"reviewer"}
```

Tears down the named agent's VM and respawns it with the same `agent_id`, role, and `agent_token` (callers that cached the token keep working). The new VM has a fresh `vm_id` / `guest_ip` / `agent_url`; the agent status resets to `ready`. On spawn failure the agent is left `Status=dead` and persisted, so callers see the truth without needing to poll.

---

### goose-agent API (`http://<guest_ip>:8080`)

Direct access to the VM's private IP — reachable from the host only. `POST /tasks` and `POST /stop` require the `agent_token` returned by `POST /vms` or `POST /snapshots/{id}/restore`. `GET /health` is always unauthenticated.

#### Run a task

```bash
curl -X POST http://10.0.1.10:8080/tasks \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -d '{"prompt": "Check my current system environment."}'
```

```json
{ "output": "...", "error": "" }
```

Returns when the task completes. `output` is the assistant text extracted from Goose's `--output-format json` envelope (v0.3.6): the agent slices from the first `{` to skip Goose's startup banner, concatenates every assistant `text` block, and falls back to raw stdout if the envelope cannot be parsed. This route bypasses goose-cli's streaming-buffer truncation, which otherwise caps fenced code at 50 lines and spills the overflow into an in-VM `/tmp/goose-*.txt` the host caller cannot reach. Only one task runs at a time per VM; concurrent requests receive `503 agent busy`. If `/root/.goose-system-prompt` is present (injected by `PrepareVM` for flock members), it is prepended to the user prompt as `[SYSTEM INSTRUCTIONS]\n...\n\n[USER TASK]\n...` before being piped to Goose.

**Streaming (v0.4.4):** add `?stream=1` to receive the task as newline-delimited JSON over chunked transfer — zero or more `{"type":"progress","text":"…"}` frames (relayed from Goose's stderr activity, with a 15 s heartbeat) followed by exactly one `{"type":"result","output":"…","error":"…"}` frame mirroring the buffered shape. The control-plane proxy flushes per chunk. Because a `200` is committed before Goose runs, a Goose failure surfaces in `result.error` rather than an HTTP `500` — streaming clients must inspect the result frame, not the status code. Omitting `stream=1` keeps the buffered behavior above unchanged.

**Nested-task depth guard (v0.4.4):** when an agent dispatches to a peer via `gtcall`, the control plane stamps `X-Ephemera-Task-Depth` on each `/tasks` hop and refuses at/over `EPHEMERA_MAX_TASK_DEPTH` (default 5) with `508 Loop Detected`, preventing runaway nested-invocation loops.

#### Post to Town Wall (flock members only)

```bash
curl -X POST http://10.0.1.10:8080/townwall/post \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -d '{"body":"Claiming src/styles/theme.css"}'
```

Reads `FLOCK_ID` and `AGENT_ID` from `/root/.ephemera-flock` (written by `PrepareVM` for flock members) and forwards the message to the host control plane's `POST /flocks/{id}/post`. Returns `400` when the VM is not a flock member. The bundled `gtwall` shell wrapper calls this endpoint and builds the JSON body with `jq --arg`, so multi-line bodies (e.g. a whole source file framed in `<<<FILE:>>>` sentinels) post correctly (v0.3.6). For agent-to-agent task dispatch, the bundled `gtcall <agent_id> "<prompt>"` wrapper resolves the peer's `vm_id` from `GET /flocks/{id}` and posts to the control plane's `POST /vms/{vm_id}/tasks` proxy, which injects the peer's agent token — so a calling agent never needs peer credentials (v0.3.6).

#### Health check

```bash
curl http://10.0.1.10:8080/health
# {"status":"idle"}  or  {"status":"busy"}
```

No authentication required — used internally by the control plane's `waitForAgent` poller.

#### Stop the agent

```bash
curl -X POST http://10.0.1.10:8080/stop \
  -H "Authorization: Bearer $AGENT_TOKEN"
```

Shuts down `goose-agent`. `micro-init` (PID 1) then calls `sync` + `poweroff(2)`, triggering a clean Firecracker exit. Call `DELETE /vms/{id}` afterwards to release host resources.

---

## Per-VM LLM Profiles

Profiles allow each VM to use a different LLM provider or model without modifying the default config. API keys stay on the server — clients only pass a profile name.

### Built-in role profiles

A handful of role names are pre-mapped to canonical sizing and a profile directory under `configs/profiles/`. Each ships an `.example` config and a `system.md` system prompt; copy the examples to real `*.yaml` files and fill in the API keys to enable them.

| Role | vCPU | Memory (MiB) | Profile dir | Intent |
|------|------|--------------|-------------|--------|
| `researcher` | 1 | 512 | `researcher/` | Read-only exploration, fast/cheap model recommended |
| `reviewer` | 1 | 512 | `reviewer/` | Adversarial diff review |
| `worker` | 2 | 2048 | `worker/` | Implementation — code-writing model recommended |
| `orchestrator` | 2 | 2048 | `orchestrator/` | Delegation + synthesis (never executes work itself) |
| `builder` | 4 | 4096 | `worker/` | Heavyweight worker (reuses the worker profile) |

Unknown names also work — a Web-UI-created profile uses its own vCPU/memory when set (v0.5.1), otherwise the default `1 vCPU / 1024 MiB` (the Standard preset); either way it looks up `configs/profiles/{name}/`.

### Setup

Each profile directory holds three files:

```
configs/
  profiles/
    anthropic/
      goose.yaml           ← GOOSE_PROVIDER: anthropic, GOOSE_MODEL: claude-sonnet-4-6
      goose-secrets.yaml   ← ANTHROPIC_API_KEY: sk-ant-...
    researcher/
      goose.yaml.example   ← committed; copy to goose.yaml
      goose-secrets.yaml.example
      system.md            ← role system prompt (always committed)
```

Real `goose.yaml` and `goose-secrets.yaml` files inside every `configs/profiles/*/` are gitignored.

### Usage

```bash
# Spawn VM with the 'anthropic' profile (uses default sizing)
curl -X POST http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"profile": "anthropic"}'

# Spawn VM with a built-in role (sized at 1 vCPU / 512 MiB)
curl -X POST http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"profile": "researcher"}'
```

Omitting `profile` (or sending an empty body) uses `configs/goose.yaml` and `configs/goose-secrets.yaml` at the default 1 vCPU / 1024 MiB sizing.

If the profile directory has a `system.md`, its contents are written into the VM as `/root/.goose-system-prompt` and the in-VM `goose-agent` prepends it to every `/tasks` prompt — so the role stays in-character even when the orchestrator dispatches plain user prompts.

### Editing a profile's model (Web UI / API, v0.5.0)

A profile can be **created**, and its provider/model read and changed, at runtime without restarting the daemon — the Web UI **Settings** screen drives these endpoints:

```bash
# List all profiles with their current provider/model
curl http://localhost:3000/config/profiles -H "Authorization: Bearer $TOKEN"
# → [{"name":"default","provider":"google","model":"gemini-2.5-flash"}, {"name":"worker", …}]

# List the named sizing presets (v0.5.3) the Settings editor offers as quick-select chips.
curl http://localhost:3000/config/presets -H "Authorization: Bearer $TOKEN"
# → [{"id":"light","label":"Light","vcpu_count":1,"mem_size_mib":512},
#    {"id":"standard","label":"Standard","vcpu_count":1,"mem_size_mib":1024},
#    {"id":"advanced","label":"Advanced","vcpu_count":2,"mem_size_mib":2048}]

# Create a profile (provider/model + optional per-VM vCPU/memory, v0.5.1).
# Omit vcpu_count/mem_size_mib (or pass 0) to use the default 1 vCPU / 1024 MiB.
curl -X POST http://localhost:3000/config/profiles \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"fast-worker","provider":"groq","model":"openai/gpt-oss-120b","vcpu_count":2,"mem_size_mib":2048}'

# Update one profile (rewrites GOOSE_PROVIDER/GOOSE_MODEL in place — comments +
# extensions preserved; API keys in goose-secrets.yaml are never touched here;
# vcpu_count/mem_size_mib update sizing too — 0 or omitted keeps the current value)
curl -X PUT http://localhost:3000/config/profiles/worker \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"provider":"anthropic","model":"claude-sonnet-4-6"}'

# Delete a profile. Refused with 409 Conflict (v0.5.3) if any running VM was spawned
# from it — remove or re-profile those agents first so they don't silently fall back
# to the default config on their next restart / change-role.
curl -X DELETE http://localhost:3000/config/profiles/fast-worker \
  -H "Authorization: Bearer $TOKEN"
```

`name` is `default` for `configs/goose.yaml`, otherwise a `configs/profiles/{name}/` directory. Because config is injected at spawn, an edit applies to the **next** VM created from that profile; already-running VMs keep the model they were spawned with (`GET /vms` reports each VM's `provider`/`model`).

---

## Diff Snapshots (Multi-Checkpoint)

Diff snapshots capture only the memory pages dirtied since the last Full snapshot, reducing storage cost for repeated checkpointing of a long-running VM.

### Storage comparison

| Scenario | Full × N | With Diff |
|----------|----------|-----------|
| 3 checkpoints | 3 × 2.7 GB = **8.1 GB** | 2.7 GB + 2 × ~0.9 GB = **4.5 GB** |
| 5 checkpoints | 5 × 2.7 GB = **13.5 GB** | 2.7 GB + 4 × ~0.9 GB = **6.3 GB** |

*Diff size depends on actual memory activity; typical Goose workloads dirty 10–20% of RAM.*

### How it works

```
VM starts (TrackDirtyPages=true in MachineConfiguration)

POST /vms/{id}/snapshot          ← first call
  snapshot_type: "full"          ← full memory.bin + full rootfs.ext4

... VM runs tasks, dirties pages ...

POST /vms/{id}/snapshot          ← second call (auto-detects prior Full)
  snapshot_type: "diff"          ← sparse memory.bin (dirty pages) + sparse rootfs.diff (changed blocks)
  base_snapshot_id: snap-xxx     ← references the Full above

POST /snapshots/{diff-id}/restore
  → MergeMemoryDiff(full.memory.bin + diff.memory.bin → tmp/merged.bin)
  → MergeRootfsDiff(full.rootfs.ext4 + diff.rootfs.diff → tmp/merged.ext4)  ← dm-snapshot origin
  → RestoreMachine(merged.bin, diff.state.bin)
  → os.Remove(merged.bin)        ← memory temp cleaned up after VM starts
                                    (merged.ext4 unlinked after losetup; freed at VM destroy)
```

> **Disk space during restore**: a diff restore writes a temporary ~2 GB `merged.bin` (memory — removed once Firecracker opens it) and a ~570 MB `merged.ext4` (rootfs — unlinked right after `losetup`, its blocks back the dm-snapshot origin until VM destroy) under `{workDir}/tmp`. Ensure the host has a few GB free before restoring a diff snapshot.

### Dependency rule

A Full snapshot referenced by one or more Diff snapshots is **protected from deletion**:

```bash
# Will fail with 409 Conflict while diff exists
curl -X DELETE http://localhost:3000/snapshots/$FULL_SNAP_ID

# Correct order: delete Diff first
curl -X DELETE http://localhost:3000/snapshots/$DIFF_SNAP_ID
curl -X DELETE http://localhost:3000/snapshots/$FULL_SNAP_ID  # now succeeds
```

### Explicit type override

```bash
# Force a full snapshot even if a prior Full exists
curl -X POST http://localhost:3000/vms/$VMID/snapshot \
  -H "Content-Type: application/json" \
  -d '{"type": "full"}'

# Force a diff snapshot (returns 400 if no Full exists)
curl -X POST http://localhost:3000/vms/$VMID/snapshot \
  -H "Content-Type: application/json" \
  -d '{"type": "diff"}'
```

---

## COW Rootfs Restore

When restoring a VM from a snapshot, Ephemera uses Linux **device mapper snapshot** (dm-snapshot) to create a block-level copy-on-write view of the snapshot's `rootfs.ext4`. This eliminates the ~700 MB full disk copy that was previously required per restore.

### How it works

```
snapshots/<id>/rootfs.ext4   (read-only base, shared across all restores of this snapshot)
        │
  losetup -r --find → /dev/loopX      (read-only loop device for base)
  truncate -s 8G   → vm-{id}.cow      (sparse exception store, ~0 bytes initially)
  losetup --find   → /dev/loopY      (read-write loop device for exception store)
        │
  dmsetup create cow-vm-{id}.cow
    --table "0 <sectors> snapshot /dev/loopX /dev/loopY P 8"
        │
  /dev/mapper/cow-vm-{id}.cow         (COW block device)
        │
  mount --bind /dev/mapper/cow-{id}   (over original disk path from state.bin)
  /tmp/goose-workspaces/vm-{orig}.ext4
        │
  Firecracker opens the path → reads base, writes go to .cow
```

- **Base**: `rootfs.ext4` in the snapshot directory (read-only, never modified)
- **Exception store** (`vm-{id}.cow`): 8 GB sparse file; actual disk blocks allocated only on VM write
- **Initial extra disk usage**: ~0 MB (16 × 512-byte blocks for dm-snapshot metadata)

### Disk usage comparison

| Restores | Before (full copy per restore) | After (COW) |
|----------|-------------------------------|-------------|
| 1 restore | +700 MB | +~0 MB |
| 5 concurrent restores | +5 × 700 MB = **3.5 GB** | +5 × ~0 MB = **~0 MB** |
| After 1 GB of VM writes | +700 MB | +~1 GB |

### Cleanup

When a COW-restored VM is deleted:

```
TeardownDMSnapshot()
  → umount -l <original disk path>   (lazy unmount — safe if Firecracker still holds fd)
  → dmsetup remove cow-vm-{id}.cow   (retries up to 5× for Firecracker fd release)
  → losetup -d /dev/loopY            (detach COW loop device)
  → losetup -d /dev/loopX            (detach base loop device)
  → rm vm-{id}.cow                   (delete sparse exception store)
```

### Fallback

If dm-snapshot setup fails (e.g., `dmsetup` unavailable), the control plane automatically falls back to the original bind-mount approach (full 700 MB disk copy per restore) and logs the reason.

---

## Security

### Control plane API authentication

#### Per-client tokens (recommended)

```bash
ALICE_TOKEN=$(openssl rand -hex 32)
BOB_TOKEN=$(openssl rand -hex 32)

export EPHEMERA_API_TOKENS="alice:$ALICE_TOKEN,bob:$BOB_TOKEN"
sudo -E ./ephemera-daemon
```

Startup log:
```
Control plane API on 127.0.0.1:3000  (auth: Bearer token (2 client(s): alice, bob))
```

Each request is logged with the authenticated client name:
```
[alice] POST /vms
[bob]   GET  /vms
```

#### Single-token fallback

```bash
export EPHEMERA_API_TOKEN=$(openssl rand -hex 32)
sudo -E ./ephemera-daemon
```

Treated as a single client named `default`.

If neither variable is set, a startup warning is logged and the API is unauthenticated — **never expose the control plane without a token**.

#### Token hot reload (SIGHUP)

API tokens can be updated without restarting the daemon or interrupting running VMs. The recommended path since v0.3.4 is a file source — env vars are captured at exec, so a SIGHUP can only observe a value change when the daemon reads from disk:

```bash
# One-time setup: point the daemon at a tokens file.
echo "alice:$ALICE_TOKEN,bob:$BOB_TOKEN" > /etc/ephemera/tokens
chmod 0600 /etc/ephemera/tokens
EPHEMERA_API_TOKENS_FILE=/etc/ephemera/tokens \
    ./ephemera-daemon &

# Later: rotate by editing the file and signalling.
echo "alice:$NEW_ALICE,carol:$CAROL_TOKEN" > /etc/ephemera/tokens
kill -HUP $(pgrep ephemera-daemon)
```

`ReloadClients` re-reads the file, swaps the in-memory client list under `clientsMu`, **and (v0.3.4) fans the first non-expired client's token out to every running flock VM over vsock** (`SET_CP_TOKEN` command, atomic rewrite of `/root/.ephemera-cp-token`). The in-VM `/townwall/post` forwarder picks up the rotated bearer on the next request without any VM restart. See [CP token rotation via vsock](#cp-token-rotation-via-vsock-v034).

| Scenario | Action |
|----------|--------|
| Adding a new client | Edit `EPHEMERA_API_TOKENS_FILE` → SIGHUP |
| Rotating the primary CP token (the one VMs use) | Edit file → SIGHUP; in-VM `/root/.ephemera-cp-token` is updated automatically (v0.3.4+) |
| Emergency revocation | Edit file → SIGHUP — **no VM interruption** |
| Legacy `EPHEMERA_API_TOKENS` env (no file) | Still works for the `cp.clients` swap, but does not see env-value changes without daemon restart. Use `_TOKENS_FILE` for live rotation. |

#### Token TTL & rotation (v0.4.1)

A client entry may carry an optional expiry as a third colon-separated field —
`name:token:expires` — where `expires` is **RFC3339** (e.g. `2026-06-01T00:00:00Z`)
or **Unix seconds**. A two-field `name:token` never expires (backward compatible).

```bash
# A short-lived CI token plus a never-expiring operator token.
printf 'ops:%s\nci:%s:2026-06-01T00:00:00Z\n' "$OPS_TOKEN" "$CI_TOKEN" > /etc/ephemera/tokens
```

- Expiry is enforced **per request**: a matched-but-expired token returns `401`
  (identical body to an unknown token; only the server-side log + the
  `ephemera_auth_total{outcome="expired"}` metric distinguish it). No background
  reaper — checking at request time is sufficient.
- Tokens may themselves contain `:`; the expiry is recognized only when the
  trailing colon-separated field parses as a timestamp, so an existing
  colon-bearing token keeps working.
- **Primary (CP) token selection:** the token injected into flock VMs is the
  **first non-expired** client (not blindly the first), so letting a primary
  token expire does not break in-VM `/townwall/post`. If every token has expired,
  an empty token is propagated (the forwarder then calls unauthenticated) and a
  warning is logged. Keep at least one never-expiring client for VM callbacks.

### Access audit log (v0.4.1)

Every API request is appended as one JSON line to `{workDir}/audit/access.jsonl`
(on by default; set `EPHEMERA_AUDIT_DISABLE=true` to turn off). Each record is
`{ts, client, method, path, status, duration_ms, remote_addr, bytes}` — it
**never contains tokens, the `Authorization` header, request/response bodies, or
the query string**. Unauthenticated requests record `client="-"`. `/metrics` is
not audited (to avoid flooding the log with scrapes).

The file is size-rotated (`EPHEMERA_AUDIT_MAX_MIB`, default 100) keeping
`EPHEMERA_AUDIT_KEEP` (default 5) generations. Query recent entries:

```bash
curl -H "Authorization: Bearer $TOKEN" \
    "http://127.0.0.1:3000/audit?limit=100&client=alice&status=200&method=GET"
# → JSON array, newest first
```

`GET /audit` is itself authenticated (and audited). Filters `client`, `status`,
`method` are optional; `limit` defaults to 100, capped at 1000.

---

### goose-agent authentication

Each VM's agent is protected by a unique 32-byte random Bearer token generated at spawn time and written to `/root/.ephemera-agent-token` (mode `0600`) inside the VM disk. The token is returned once in the `POST /vms` response (and again in `POST /snapshots/{id}/restore`).

- `POST /tasks` and `POST /stop` require `Authorization: Bearer <agent_token>`
- `GET /health` is always open (used by the control plane's internal health poller)
- The token is tied to the VM's disk and persists across snapshot/restore cycles

---

### TLS and network exposure

By default the control plane binds to `127.0.0.1:3000` (localhost only). Place a TLS-terminating reverse proxy in front for external access.

#### Step 1 — allow external binding

```bash
export EPHEMERA_API_ADDR=0.0.0.0:3000
sudo -E ./ephemera-daemon
```

#### Step 2 — configure a reverse proxy

**Caddy** (automatic HTTPS via Let's Encrypt — recommended):

`/etc/caddy/Caddyfile`:
```
api.example.com {
    reverse_proxy localhost:3000
}
```

```bash
sudo apt-get install -y caddy
sudo systemctl restart caddy
```

**Nginx** (manual certificate):

`/etc/nginx/sites-available/ephemera`:
```nginx
server {
    listen 443 ssl;
    server_name api.example.com;

    ssl_certificate     /etc/ssl/certs/ephemera.crt;
    ssl_certificate_key /etc/ssl/private/ephemera.key;
    ssl_protocols       TLSv1.2 TLSv1.3;

    location / {
        proxy_pass         http://127.0.0.1:3000;
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_read_timeout 300s;   # POST /vms/*/snapshot can take several minutes
    }
}

server {
    listen 80;
    server_name api.example.com;
    return 301 https://$host$request_uri;
}
```

```bash
sudo ln -s /etc/nginx/sites-available/ephemera /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl restart nginx
```

#### Step 3 — call via HTTPS

```bash
curl -X POST https://api.example.com/vms \
  -H "Authorization: Bearer $ALICE_TOKEN" \
  -H "Content-Type: application/json"
```

### VM isolation

- Each VM runs in a separate KVM hardware boundary.
- Each VM gets a **cloned** rootfs — no shared filesystem state between VMs.
- Goose config and API keys are injected at provision time and exist only inside the ephemeral VM disk.
- On teardown: `micro-init` calls `poweroff(2)`, TAP device is deleted, disk is wiped, IP is returned to pool.

---

## Resilience

v0.3.1 hardened Goosetown for long-running flock workloads (health watchdog, flock metadata persistence, monotonic `seq`). v0.3.2 extends this with **live VM cold-restart**: any VM that was running when the daemon stops is automatically brought back up on the next daemon start, with the same IP, TAP, MAC, agent token, and `vm_id`. All features preserve v0.3.0 wire compatibility (`seq` is a new JSON field; watchdog is server-side only; persistence touches only `flocks/<id>/metadata.json` and `vms/<vm_id>/state.json`).

### Live VM cold-restart (v0.3.2)

Every successful spawn writes `vms/<vm_id>/state.json` (atomic tmp + rename) capturing the network identity, disk path, agent token, profile, and flock association. On daemon startup, after flock metadata is rescanned, each persisted VM is automatically brought back up:

1. **Orphan cleanup** — any leftover Firecracker process bound to the persisted API socket is sent SIGTERM, then SIGKILL after a 1.5 s grace. Stale socket / log FIFO / vsock UDS files are removed. (After a graceful shutdown this is a no-op because the previous daemon already stopped them; after a SIGKILL / crash it does the actual cleanup.)
2. **Network re-reservation** — the original TAP device is recreated with the same name and MAC, and the original IP is re-marked as in-use in the pool.
3. **Cold boot** — Firecracker is restarted against the same rootfs: a plain VM reuses its full rootfs clone, while a COW VM (`EPHEMERA_DISK_MODE=cow`) reconstructs its dm-snapshot by re-layering the preserved exception store over the golden image (v0.4.0). `goose-agent` is waited for on `/health` up to 60 s.
4. **Flock association** — if the VM belonged to a flock, the agent's status is flipped back to `"ready"`. If recovery fails, the agent is marked `"dead"` and a `<orchestrator>` notice is posted to the Town Wall.

The daemon-side shutdown path is designed to feed cold-restart:

- **Graceful shutdown (SIGTERM/SIGINT)** — `ControlPlane.DestroyAll` stops every Firecracker process via `StopVMM`, releases TAP/IP/vsock/socket, and **preserves each VM's rootfs ext4 and `state.json`**. The next daemon start cold-restarts them.
- **Explicit `DELETE /vms/{id}`** — routes through `destroyVM`, which does a full cleanup (deletes `state.json`, removes the rootfs ext4, releases all resources). The VM is gone and is not cold-restarted.
- **SIGKILL / crash** — defers don't run. `state.json` + rootfs survive on disk; on the next start, cold-restart picks them up exactly as for graceful shutdown.
- **COW spawn VMs** (`EPHEMERA_DISK_MODE=cow`) — `DestroyAll` releases the dm-snapshot kernel objects but **preserves the sparse exception store + `state.json`** (`TeardownDMSnapshotKeepStore`); the next start re-layers the store over the golden image and cold-restarts them (v0.4.0).
- **Snapshot-restored VMs** (`POST /snapshots/{id}/restore`, dm-snapshot path) — `DestroyAll` drops the dm device + transient exception store but **keeps `state.json`** (which carries `source_snapshot_id`). The next start **re-restores** the VM from that source snapshot via `RecoverVMs` → `recoverRestoredVM` (back to snapshot-time memory+disk; the store is recreated fresh), v0.4.5. The legacy **bind-mount fallback** path (dm-snapshot tooling unavailable) still persists no `state.json` and is not auto-recovered.

**Memory auto-snapshot (v0.4.0, opt-in).** With `EPHEMERA_AUTOSNAPSHOT=true`, `DestroyAll` additionally snapshots each recoverable VM's live memory+state into `vms/<id>/auto/{memory.bin,state.bin}` *before* stopping it (graceful shutdown only — a SIGKILL cannot run it). On the next start, `RecoverVMs` **warm-restores** from that snapshot via `vm.RestoreMachine` (memory preserved, same `vm_id`/IP/TAP/MAC/token), so in-flight `/tasks` work survives a daemon bounce. The snapshot is **one-shot** (deleted after the attempt, so a later bounce never rolls the VM back to a stale image) and **best-effort**: any failure — snapshot write, restore, or agent handshake — logs and falls back to the cold boot above. This is why `forwardSignals` omits `SIGTERM`/`SIGINT` (v0.4.0): the daemon owns graceful teardown, and a forwarded SIGTERM would kill Firecracker mid-snapshot.

What this preserves:

| Preserved | Lost |
|-----------|------|
| `vm_id`, `guest_ip`, `tap_device`, `mac_addr` | In-flight `/tasks` work — unless `EPHEMERA_AUTOSNAPSHOT` warm restore is enabled (v0.4.0) |
| `agent_token`, `agent_url` | Goose conversation context (in-VM memory) — likewise preserved by `EPHEMERA_AUTOSNAPSHOT` warm restore |
| Disk contents — plain rootfs clone reused; COW exception store re-layered over the golden image (v0.4.0) | dm/loop kernel objects (not persisted — recovery recreates them) |
| Flock membership, Town Wall history | (none) |
| Watchdog `status=dead` markings (v0.3.3 — persisted to `metadata.json`) | |

Callers that need at-most-once semantics across daemon restarts should idempotency-key their `/tasks` calls or poll for completion before retrying.

**Snapshot-restored VM recovery (v0.4.5):** dm-snapshot restored VMs **are** auto-recovered — they persist a `state.json` with `source_snapshot_id`, and `RecoverVMs` re-restores them from that snapshot on the next start (the VM returns to its snapshot-time memory+disk; writes since the restore are not preserved, same as a manual re-restore). Still **out of scope**: the legacy bind-mount-fallback restore path (no `state.json`), and a restored VM whose **source snapshot was deleted** while it ran — recovery cannot re-restore it, so it is dropped and surfaced (not silently kept).

> COW *spawn* VMs (`EPHEMERA_DISK_MODE=cow`) **are** auto-recovered as of v0.4.0: `DestroyAll` preserves the exception store and `RecoverVMs` re-layers it over the golden image. Orphan dm-snapshot devices left by a crashed run (no surviving `state.json`) are reclaimed on the next start via `RemoveOrphanCOWDevices`.

### Watchdog dead-status persistence (v0.3.3)

The watchdog's `dead` marking is now durable. `Watchdog.onFailure` calls `Flock.Persist(workDir)` immediately after flipping `AgentInfo.Status` to `"dead"`, so the change lands in `flocks/<id>/metadata.json` before the next probe cycle. `Flock.Persist` holds a per-flock `writeMu` around `ToMetadata` + `SaveFlockMetadata`'s `tmp + rename`, so concurrent writers (`createFlock`, `watchdog.onFailure`, `recovery.markFlockAgentDead`, the new per-agent restart) never tear each other's writes.

Recovery's status transitions are persisted on the same path: successful cold-restart flips status back to `ready` and persists, while a VM that cannot be cold-restarted is marked `dead` (with an `<orchestrator>` Town Wall notice) and persisted. The net effect is that the on-disk metadata always reflects the freshest known liveness, and a daemon restart can never silently revive an agent that was already dead.

> **Operational note**: a recovered `dead` agent stays dead — even if the watchdog later sees the VM respond, the dead marking is intentionally not auto-cleared. Use `POST /flocks/{id}/agents/{agent_id}/restart` (below) to replace the VM and reset the status to `ready`.

### Per-agent restart (v0.3.3)

`POST /flocks/{flock_id}/agents/{agent_id}/restart` is the surgical alternative to recreating a whole flock when one member dies. The handler:

1. Looks up the agent in the flock (404 if either is missing).
2. Captures the existing `agent_token` from `cp.vms` before teardown.
3. Tears down the dead VM via `destroyVM` and calls `Watchdog.ForgetVM(oldVMID)` to drop cached failure state.
4. Calls `spawnVMInternal` with the same role/profile/flockID/agentID **plus the captured `AgentToken`**; an empty token would trigger fresh generation, but here we reuse so callers' cached tokens keep working.
5. Calls `Flock.UpdateAgentVM(agentID, newVMID, newAgentURL)`, which swaps the VM identity in place and resets `Status` to `ready`.
6. Persists the updated metadata.

On spawn failure the agent is left in `Status=dead` (and persisted) so external callers see the truth — they can retry restart or `DELETE` the flock entirely.

```bash
curl -X POST "$API/flocks/$FLOCK_ID/agents/reviewer-1/restart"
# {"vm_id":"vm-1715...","guest_ip":"10.0.1.7","agent_url":"http://10.0.1.7:8080","profile":"reviewer"}
```

### Auto-injected control-plane token (v0.3.3)

When the control plane runs with `EPHEMERA_API_TOKENS` set, the in-VM `/townwall/post` forwarder needs a Bearer to authenticate against `/flocks/{id}/post`. v0.3.3 plumbs that token automatically:

- `ControlPlane.controlPlaneTokenForVM()` returns the first **non-expired** client's token under `clientsMu` (so SIGHUP-driven `ReloadClients` stays safe; `apiClients[0]` until v0.4.1 added per-token TTL). Empty when auth is disabled or every token has expired.
- `spawnVMForFlock` (and `restartAgent`) pass the token through `spawnVMOptions.ControlPlaneToken` → `VMPrepareOptions.ControlPlaneToken` → `injectVMFiles`, which writes it to `/root/.ephemera-cp-token` at mode 0600. Standalone `POST /vms` does NOT inject it because non-flock VMs do not use `/townwall/post`.
- `goose-agent`'s `loadCPToken` prefers the file and falls back to the legacy `EPHEMERA_CONTROL_PLANE_TOKEN` env var for older golden images.

This removes the per-VM operator burden documented in earlier releases. v0.3.4 adds true hot rotation on top — see [CP token rotation via vsock](#cp-token-rotation-via-vsock-v034) below.

### CP token rotation via vsock (v0.3.4)

When you want to rotate the control-plane bearer without restarting either the daemon or any VMs:

1. Run the daemon with `EPHEMERA_API_TOKENS_FILE=/etc/ephemera/tokens` (one `name:token[:expires]` entry per line — comma-separated also works; the optional `:expires` is the v0.4.1 per-token TTL). The file source takes precedence over `EPHEMERA_API_TOKENS` env when set; both legacy env paths remain as fallback.
2. Edit the file (operator action).
3. `pkill -HUP ephemera-daemon`. `ReloadClients` re-reads the file (env values are fixed at exec, the file is not), hot-swaps `cp.clients` under `clientsMu`, and fans the first **non-expired** client's token (v0.4.1; was `apiClients[0]`) out to every running flock VM over the existing vsock channel.

In-VM side, `goose-agent`'s vsock listener now dispatches both `CHANGE_IP` (used since v0.2.0 for snapshot-restore IP plumbing) and the new `SET_CP_TOKEN <token>` command, which atomically rewrites `/root/.ephemera-cp-token` (tmp + rename, mode 0600). The `/townwall/post` handler re-reads the file on every request, so the next forwarder call sees the new bearer.

The fan-out is **best-effort**: each VM gets ~4 s (20 attempts × 200 ms, matching the existing `ReconfigureGuestIP` budget) and any per-VM failure is logged but never propagated. The SIGHUP path therefore completes in bounded time regardless of unresponsive VMs. A final log line summarizes results:

```
SIGHUP: token reload complete — 1 client(s): alice
SIGHUP: CP token propagated to 3/3 VM(s)
```

**SDK signal forwarding** — `firecracker-go-sdk` v1.0.0 defaults to forwarding `SIGINT/SIGQUIT/SIGTERM/SIGHUP/SIGABRT` from the daemon to every Firecracker child (see `internal/vm/machine.go`'s `setupSignals` reference). Because the daemon itself uses `SIGHUP` for the rotation flow described here, we explicitly set `firecracker.Config.ForwardSignals` to a list that **excludes SIGHUP** — otherwise the daemon's own reload signal would kill every running Firecracker and the vsock fan-out would immediately get `connection refused`. The shutdown signals stay forwarded so `Ctrl-C` / `systemctl stop` still propagate cleanly.

**Caveat**: only VMs spawned by a v0.3.4 (or newer) daemon implement the `SET_CP_TOKEN` handler. VMs whose `goose-agent` was baked from an older golden image will log a per-VM "unknown command" failure during fan-out; for those, the v0.3.3 fallback (`POST /flocks/{id}/agents/{agent_id}/restart`) is still the rotation path.

### Health watchdog

A background goroutine polls every flock-member VM's `/health` endpoint every 5 seconds (1 s HTTP timeout). After 3 consecutive failures the agent's status transitions to `"dead"` and a notice is auto-posted to the Town Wall:

```
[2026-05-15T14:33:12Z] <orchestrator> worker-1 unresponsive after 3 health probes - marked dead
```

Subscribers on the SSE stream see this in real time. The dead agent is **not** auto-revived even if it transiently recovers — operators decide when to reset by deleting the flock or the individual VM. Standalone (non-flock) VMs are not watched.

**Env-tunable since v0.3.4.** All three thresholds are overridable at startup:

| Variable | Default | Purpose |
|----------|---------|---------|
| `EPHEMERA_WATCHDOG_INTERVAL_SEC` | `5` | Poll cadence |
| `EPHEMERA_WATCHDOG_TIMEOUT_SEC` | `1` | Per-probe HTTP timeout (clamped: `interval ≥ timeout`) |
| `EPHEMERA_WATCHDOG_THRESHOLD` | `3` | Consecutive fails before marking dead |
| `EPHEMERA_WATCHDOG_AUTO_HEAL` | `false` | When `true`, a dead agent that resumes responding is auto-marked `ready` and a recovery notice (`"<id> recovered - auto-healed to ready"`) is posted to the Town Wall. Default off preserves the sticky-dead contract. |

Tunables apply once at daemon startup and land via `Watchdog.Configure` before `Start`. The startup log line confirms the resolved values:

```
Watchdog started (interval=5s, timeout=1s, threshold=3, auto_heal=false)
```

### Flock state persistence

`POST /flocks` writes `flocks/<flock-id>/metadata.json` atomically (tmp + rename) before returning the response. On daemon startup the file is rescanned and every flock is re-registered in memory. The Town Wall log is reopened in append mode so full message history is preserved across restarts; `seq` numbering continues monotonically.

> **Recovery scope (v0.3.2; memory mitigated v0.4.0)**: flock metadata is restored here; the live VMs are brought back via the cold-restart path described above. After daemon restart, recovered flocks are fully interactive (`/tasks`, `/stop`, `/post`, `/wall`, `DELETE` all work). By default in-VM memory state is lost — agents resume from a fresh boot, not from where they left off — unless `EPHEMERA_AUTOSNAPSHOT` warm restore is enabled and the shutdown was graceful (v0.4.0).

### Monotonic message sequence numbers

Each Town Wall `Message` carries a `seq` field starting at 1 per flock. A subscriber that reconnects after a network blip can compare its last received `seq` against the newest message it sees and detect any gap; missing entries can be fetched from `/flocks/{id}/wall/history` and filtered by `seq`.

```bash
LAST_SEQ=42
curl -N "$API/flocks/$FLOCK_ID/wall" | while read -r line; do
    case "$line" in
        data:*)
            msg="${line#data: }"
            seq=$(echo "$msg" | jq -r .seq)
            if [ "$seq" -gt "$((LAST_SEQ + 1))" ]; then
                # gap — recover from history
                curl -s "$API/flocks/$FLOCK_ID/wall/history" | \
                    jq --argjson last "$LAST_SEQ" --argjson seen "$seq" \
                        '.[] | select(.seq > $last and .seq < $seen)'
            fi
            LAST_SEQ=$seq
            ;;
    esac
done
```

`seq` is reassigned 1..N from the on-disk log each time `History` is read, so it is stable across daemon restarts (the file format itself does not store seq — it is the canonical assignment from line order).

---

## Observability (v0.3.5)

### Prometheus metrics

The control plane exposes a counter / gauge / histogram catalogue at `GET /metrics`. Defaults follow the standard scrape model (unauthenticated, text format 0.0.4); see the [Metrics endpoint](#metrics-v035) under API Reference for the full catalogue and `EPHEMERA_METRICS_REQUIRE_AUTH` to gate it behind Bearer auth. The exposition formatter is self-implemented (`internal/metrics/`) — the project keeps its zero-runtime-dependency policy.

**Grafana embedding (v0.5.5):** `observability_demo.sh` launches Grafana with `allow_embedding=true` + an anonymous Viewer and passes `EPHEMERA_GRAFANA_URL` to the daemon, so the Web UI's **System → Monitoring** tab embeds the `ephemera-overview` dashboard (8 panels) in an iframe (`kiosk` mode). Without `EPHEMERA_GRAFANA_URL` the tab shows a "not configured" notice.

### Structured logging (`log/slog`)

Every daemon-side log call (control plane, recovery, watchdog, network, storage) was migrated from `log.Printf` to `log/slog`. Two env knobs control output:

- `EPHEMERA_LOG_FORMAT=text` (default) — `key=value` lines from slog's TextHandler.
- `EPHEMERA_LOG_FORMAT=json` — slog's JSONHandler, suitable for log-aggregation pipelines.
- `EPHEMERA_LOG_LEVEL=debug|info|warn|error` (default `warn`) — minimum level emitted.

Context fields are attached as structured pairs (`vm_id`, `flock_id`, `agent_id`, `err`, …) rather than embedded in the message string. The in-VM `goose-agent` keeps its existing `log.Printf` output unchanged this cycle to avoid touching the golden-image bake budget; revisit in v0.4.3.

### Per-VM stats endpoint

`GET /vms/{vm_id}/stats` returns a JSON snapshot of cpu/mem/network/uptime/agent_busy (see [Per-VM Stats](#per-vm-stats-v035) under API Reference). The endpoint is a point-in-time snapshot — repeated polling is the intended scrape pattern; streaming is on the v0.4.3 roadmap.

### Try the demo (`observability_demo.sh`)

`sudo bash observability_demo.sh` spins up the daemon, downloads + launches Prometheus and Grafana (cached under `artifacts/`), then runs an automatic workload that exercises every metric family (VM spawn/destroy, snapshot create, flock spawn, SIGHUP reload). After ~2 minutes a banner prints the URLs:

| Service | URL | Notes |
|---------|-----|-------|
| Daemon API + `/metrics` | http://localhost:3000 | Bearer `demo-token-v035` for API calls; `/metrics` is unauthenticated |
| Prometheus | http://localhost:9090 | 5-second scrape interval (demo-only) |
| Grafana | http://localhost:3001 | `admin` / `admin`, dashboard "Ephemera Overview" pre-provisioned |

The daemon, Prometheus, and Grafana remain running until you press `Ctrl-C`; the trap then shuts down all three and removes the per-run TSDB / data dir under `/tmp/observability-demo-*`. Targets Prometheus 2.51.x and Grafana 10.4.x (versions + SHA256 are pinned in the script).

---

## Multi-Agent Webdev Demo (v0.3.6)

`webdev_demo.sh` is a one-shot operator demo that exercises the full flock stack: it stands up an **orchestrator + worker + reviewer** flock and has them collaboratively design, build, and publish a small React + Vite portfolio site — entirely from inside the VMs, with the host acting only as a passive harvester.

### What it does

1. Preflight (memory headroom, `/dev/kvm`, vite-template present), then swaps each role's `*.webdev.{md,yaml}` overrides over its `system.md` / `goose.yaml` and starts the daemon.
2. `POST /flocks` spawns the three agents.
3. A background SSE subscriber (`GET /flocks/{id}/wall`) harvests `<<<FILE: path>>> … <<<END>>>` sentinels off the Town Wall, writes each file under a working `site/` tree, and exits on `<<<DONE>>>`.
4. One `POST /vms/{orchestrator}/tasks` kicks off the orchestrator, which drives the whole job in a single Goose session: for each of `src/App.jsx`, `src/main.jsx`, `src/index.css`, `index.html` it runs `gtcall worker-1 '…'` to generate the file, `gtwall` to publish it to the Town Wall, then a best-effort `gtcall reviewer-1 '…'` review note — and finally posts `<<<DONE>>>`.
5. The host overlays the harvested files onto the vite-template, runs `npm install` + `vite build`, and serves the result with `vite preview` on `:5173` until `Ctrl-C`.

### Run it

```bash
sudo WEBDEV_MIN_MEM_MIB=5000 bash webdev_demo.sh
```

Requirements: a Google Gemini API key in `configs/goose-secrets.yaml`, `/dev/kvm` + root, and enough free RAM for three 2 GiB VMs (`WEBDEV_MIN_MEM_MIB` sets the preflight floor; Firecracker allocates guest RAM lazily and host swap cushions the peak). Open `http://localhost:5173` to see the generated site; `GET /flocks/{id}/wall/history` shows the four `<<<FILE:>>>` posts authored by `orchestrator-1`.

### Notes

- **Manual gate, not CI.** Like `observability_demo.sh`, this demo needs an LLM key and `/dev/kvm`, neither of which exists on GitHub Actions runners, so it is an operator-run gate rather than an automated test.
- **Model choice.** The orchestrator runs `gemini-2.5-flash` — it must drive a ~13-step tool-calling loop without stalling, which `gemini-2.5-flash-lite` could not do reliably (it tended to plan and then stop). Worker and reviewer stay on `gemini-2.5-flash-lite` for single-shot generation/review. On the free tier all models share a 20 RPM cap that multi-turn orchestration exhausts in seconds, so the demo assumes a **paid-tier** key.
- **No host authorship.** Every published file is authored by an in-VM agent via `gtwall`; the host only harvests and builds. If the orchestrator fails to publish a file, the host keeps that file's vite-template placeholder so `vite build` still succeeds.

---

## Known Limitations

| Limitation | Detail |
|------------|--------|
| **Single-host** | All VMs run on one physical host. Multi-host clustering is not supported. |
| **Same-snapshot concurrent restores not supported** | The guest IP is reconfigured via vsock after restore, so different-snapshot concurrent restores each get a fresh IP. However, two VMs from the *same* snapshot would still collide on the Firecracker vsock UDS path (which is fixed in `state.bin`), so same-snapshot concurrent restores are not supported. |
| **Cross-machine restore** | Supported manually: copy the `snapshots/<id>/` directory to the target host at the same absolute path, then call `POST /snapshots/{id}/restore`. Automated transfer is not built in. |
| **Cold-restart loses in-VM memory by default** (v0.3.2; mitigated v0.4.0) | Live VM auto-restart re-boots each VM from its rootfs clone — the guest kernel and `goose-agent` start fresh, and any `/tasks` request in flight at the moment of daemon shutdown is dropped. Set `EPHEMERA_AUTOSNAPSHOT=true` to warm-restore in-VM memory across a *graceful* shutdown (v0.4.0). A SIGKILL/crash still cold-boots, so callers should still idempotency-key tasks or re-poll for completion across an ungraceful restart. |
| **CP token hot-rotation requires `_TOKENS_FILE`** (v0.3.4) | On SIGHUP the daemon hot-propagates the new control-plane token (the first non-expired client since v0.4.1; `apiClients[0]` before) to running VMs via the `SET_CP_TOKEN` vsock command. This requires sourcing tokens from `EPHEMERA_API_TOKENS_FILE` — env-supplied tokens are fixed at exec and cannot change on SIGHUP. (The in-VM `SET_CP_TOKEN` handler ships in every current golden image, which auto-rebakes on any `goose-agent` change.) When tokens come from the env, the `POST /flocks/{id}/agents/{agent_id}/restart` fallback re-injects the current token. |
| **Metrics retention is external** (v0.3.5) | `/metrics` exposes raw counters and gauges only — the daemon does not aggregate, store, or rotate history. Operators are expected to wire an external Prometheus (or any text-exposition-compatible) scraper. |
| **Web UI conversation is in-memory** (v0.5.0) | The conversation panel holds its transcript in the browser tab; a page reload starts a fresh `session`, so prior turns are no longer shown (the underlying goose session persists in the VM but is not re-loaded into the UI). Snapshot-restored / cold-recovered VMs may also show an empty model, since `provider`/`model` is recorded only at spawn time. |

---

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for development setup, test gates, configuration / secrets handling, PR expectations, and the areas of code that need extra care (KVM, networking, snapshots, golden image bake, in-VM auth).

---

## License

MIT — see [LICENSE](LICENSE).
