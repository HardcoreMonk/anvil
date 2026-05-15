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
- Live VM auto-restart is deferred to v0.4.0

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
- **Production with `EPHEMERA_API_TOKENS` set**: the in-VM `/townwall/post` forwarder still relies on `EPHEMERA_CONTROL_PLANE_TOKEN` being set inside the VM; auto-injection is on the v0.4.0 roadmap

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
