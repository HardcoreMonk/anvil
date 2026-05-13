# Ephemera

[![CI](https://github.com/steve-seungeui/ephemera/actions/workflows/ci.yml/badge.svg)](https://github.com/steve-seungeui/ephemera/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/steve-seungeui/ephemera)](https://github.com/steve-seungeui/ephemera/releases)
[![Go](https://img.shields.io/badge/Go-1.18+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Firecracker](https://img.shields.io/badge/Firecracker-v1.15.1-FF4500?logo=amazonaws&logoColor=white)](https://github.com/firecracker-microvm/firecracker)

**Enterprise Control Plane for Ephemeral AI Agents via Firecracker MicroVMs**

Ephemera orchestrates isolated, KVM-backed MicroVM environments for agentic AI workloads. Each VM runs [Goose](https://github.com/aaif-goose/goose) as an autonomous agent inside a minimal Debian guest, fully contained within hardware VM boundaries and completely wiped on termination.

Beyond single-VM execution, Ephemera supports **multi-agent flocks** ("Goosetown"): one `POST /flocks` call spawns a group of role-specialized VMs (orchestrator, researcher, worker, reviewer, …), each with its own vCPU/memory profile and system prompt, all sharing an append-only **Town Wall** log for coordination.

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
  GET    /flocks/{id}/wall            → SSE stream of Town Wall messages
  GET    /flocks/{id}/wall/history    → full Town Wall log

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
  POST  /vms/{vm_id}/tasks            → proxy → goose-agent :8080/tasks
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
CloneDisk()      → copy golden image → per-VM ext4 disk
                   (or CloneDiskCOW with EPHEMERA_DISK_MODE=cow → dm-snapshot, ~0 MiB)
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
  → CopyDisk()        (copy rootfs to snapshots/{id}/rootfs.ext4)
  → ResumeVM()        (unfreeze guest, or destroy if stop_after=true)

POST /snapshots/{id}/restore
  → if Diff: MergeMemoryDiff(base.memory.bin, diff.memory.bin → tmp/merged.bin)
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
| **Self-bootstrapping** | Golden image, kernel, Firecracker downloaded + SHA256-verified on first run |
| **Minimal guest OS** | Debian Bookworm minbase — no SSH, no init daemon; `micro-init` (Go binary, PID 1) mounts virtual filesystems and manages goose-agent lifecycle |
| **Graceful guest shutdown** | `micro-init` traps SIGTERM and calls `poweroff(2)` — no kernel panic on VM exit |
| **Per-VM LLM profiles** | Each VM spawn can specify a named profile (`configs/profiles/{name}/`) with its own provider, model, and API key |
| **Per-profile vCPU/memory** | Known roles (`researcher`, `worker`, `reviewer`, `orchestrator`, `builder`) map to canonical sizing (e.g. 1 vCPU / 512 MiB for researcher, 4 vCPU / 4096 MiB for builder); unknown profiles fall back to the legacy 2 vCPU / 2048 MiB default |
| **Multi-agent flocks** | `POST /flocks` spawns a group of role-specialized VMs in one call; `DELETE /flocks/{id}` tears them all down in parallel |
| **Town Wall log** | Per-flock append-only log with SSE streaming (`/flocks/{id}/wall`) for coordination; `gtwall "..."` CLI inside each VM posts via the in-VM agent |
| **Role system prompts** | Each role profile can ship a `system.md` that is injected into the VM and prepended to every `/tasks` prompt |
| **Optional COW spawn rootfs** | `EPHEMERA_DISK_MODE=cow` provisions new VMs with a dm-snapshot view of the golden image instead of a 700 MiB full copy (default off; safe rollback) |
| **Runtime config injection** | `goose.yaml` and `goose-secrets.yaml` injected at provision time — no image rebuild required to change provider/model |
| **Per-VM agent authentication** | Control plane generates a 32-byte random Bearer token per VM; token is written to the VM disk and returned once in `POST /vms` response |
| **MicroVM snapshots (Full + Diff)** | Freeze VM memory state to disk; restore in ~5 s. First snapshot → Full (2 GB); subsequent snapshots of the same VM → Diff (sparse, dirty pages only). Diff is automatically selected; Full is always the reference base. Original agent token preserved across restores. |
| **COW rootfs on restore** | Restored VMs use a Linux dm-snapshot COW device backed by the snapshot's `rootfs.ext4` (read-only base, shared). Per-VM guest writes accumulate in a sparse exception store (~0 initial disk usage). Eliminates the ~700 MB full copy previously required per restore. |
| **Post-restore IP reconfiguration** | Restored VMs receive a fresh IP from the pool via vsock — the guest's network stack is updated in-place without reboot, decoupling the restore IP from the snapshot state. |
| **IP and TAP recycling** | IPs (10.0.1.2–254) and TAP IDs are returned to a pool and reused across VM lifecycle |
| **NAT for outbound internet** | Host bridge `goose-br0` with iptables MASQUERADE enables VM-to-internet for LLM API calls |
| **Per-client API auth** | Named Bearer tokens per client (`alice:tok1,bob:tok2`); timing-safe comparison; per-request audit log |
| **SIGHUP token hot reload** | API token list can be updated without restarting the daemon or interrupting running VMs |

---

## Project Layout

```
cmd/
  goose-daemon/       Control plane daemon (main binary)
    main.go           Startup, artifact bootstrap, ControlPlane init
    api.go            HTTP API: VM + snapshot CRUD, auth middleware,
                      spawnVMInternal (shared by /vms and /flocks paths)
    config.go         Env-var configuration + AgentProfile / LookupProfile
                      (role → vCPU, memory, profile directory mapping)
    orchestrator_api.go  /flocks endpoints, SSE Town Wall streaming
  goose-agent/        In-VM HTTP agent (baked into golden image)
    main.go           /tasks, /health, /stop, /townwall/post  (Bearer token auth);
                      prepends role system prompt to /tasks bodies
  micro-init/         PID 1 for each MicroVM (baked into golden image)
    main.go           Mounts virtual filesystems, manages goose-agent,
                      calls poweroff(2) on exit

internal/
  vm/machine.go       Firecracker SDK wrapper — StartMachine, RestoreMachine
                      (VcpuCount / MemSizeMib are per-call; zero falls back to 2 / 2048)
  network/manager.go  IP pool, TAP device lifecycle, AllocateForRestore, bridge, NAT
  storage/
    provisioner.go    Golden image bootstrap, disk clone, config/token/flock injection,
                      CloneDiskCOW (dm-snapshot-backed spawn), artifact download + SHA256
    snapshot.go       Snapshot metadata (read/write), disk copy helpers,
                      SetupDMSnapshot/TeardownDMSnapshot (COW restore via dm-snapshot),
                      MergeMemoryDiff (SEEK_DATA/SEEK_HOLE sparse merge)
  orchestrator/
    townwall.go       Per-flock append-only log + subscriber fan-out
    flock.go          Flock + FlockManager (lock-safe JSON via MarshalJSON)
    handoff.go        Structured JSON handoff between agents

configs/
  goose.yaml.example             Default provider/model template
  goose-secrets.yaml.example     API key template
  profiles/                      Per-VM LLM profiles (optional)
    <profile-name>/
      goose.yaml                 (gitignored; copied from .example)
      goose-secrets.yaml         (gitignored; copied from .example)
      system.md                  Role system prompt prepended to /tasks (optional)
    researcher/  worker/  reviewer/  orchestrator/    ← built-in role profiles

.github/
  workflows/ci.yml    go build + go vet + go test on push/PR (ubuntu-22.04)

snapshots/            Stored snapshot directories (auto-created, gitignored)
  <snapshot-id>/
    memory.bin        Guest RAM dump — 2 GB (Full) or sparse/small (Diff)
    state.bin         Firecracker hardware state
    rootfs.ext4       Disk copy (always full, ~700 MB)
    metadata.json     Restore params (IP, TAP, MAC, token, type, base_snapshot_id)

e2e_test.sh           End-to-end integration test (58 steps; requires /dev/kvm + root)

scripts/
  build_image.sh      Builds golden image (Debian Bookworm + Goose + goose-agent + micro-init + gtwall)
  gtwall              In-VM CLI for posting to the flock's Town Wall (installed into the golden image)

flocks/               Per-flock workspace (auto-created at first flock spawn, gitignored)
  <flock-id>/
    TOWN_WALL.log     Append-only log of agent messages

artifacts/            Auto-populated at runtime (gitignored)
  golden-image.ext4   Golden VM disk image
  vmlinux.bin         Firecracker-compatible Linux 6.1 kernel
  firecracker         Firecracker VMM binary (SHA256-verified)
  goose-agent         In-VM HTTP agent binary (compiled from source)
  micro-init          PID 1 init binary (compiled from source)
```

---

## Prerequisites

| Requirement | Detail |
|-------------|--------|
| **Host OS** | Ubuntu 22.04 or 24.04 (bare metal, or VM with nested virtualization) |
| **CPU** | `/dev/kvm` accessible |
| **Go** | 1.18+ |
| **Packages** | `curl`, `debootstrap`, `e2fsprogs`, `util-linux` |
| **Privileges** | `sudo` at runtime (KVM + network interface management) |

```bash
sudo apt-get install -y curl debootstrap e2fsprogs util-linux
```

Firecracker, the Linux kernel, and the golden image are **downloaded and built automatically** on first run.

---

## Getting Started

### 1. Clone and build

```bash
git clone https://github.com/steve-seungeui/ephemera.git
cd ephemera
go build -o ephemera-daemon ./cmd/goose-daemon/
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
go test ./...
```

Covers: API token parsing, LLM profile path resolution, agent auth middleware, token generation.

### End-to-end test (`e2e_test.sh`)

A full integration test that boots a real daemon, spawns actual Firecracker MicroVMs, and exercises every API endpoint. Requires a host with `/dev/kvm` and root privileges.

```bash
# Build first
go build -o ephemera-daemon ./cmd/goose-daemon/

# Run (takes ~15–30 minutes depending on API rate limits)
sudo bash e2e_test.sh
```

**What it tests (58 steps):**

| Steps | Scenario |
|-------|----------|
| 1–5 | Daemon startup, single VM lifecycle (create → task → stop → delete) |
| 6–9 | Two VMs in parallel — concurrent task execution |
| 11–17 | Full snapshot lifecycle: create with `stop_after`, list, restore, verify agent token and new IP, delete |
| 19–24 | **Concurrent restore** — two different snapshots restored simultaneously; verifies both VMs run at the same time with independent IPs and disks |
| 26–28 | **Diff snapshot creation** — auto-detection: first snapshot → `full`, second → `diff` with correct `base_snapshot_id` |
| 29 | **Diff size verification** — `stat -c%b` confirms Diff `memory.bin` allocates fewer disk blocks than Full (sparse file) |
| 30–32 | Diff snapshot restore — merged memory applied, agent responds, token preserved |
| 33 | **Dependency protection** — deleting the Full base while Diff references it returns `409 Conflict` |
| 34 | Ordered cleanup: delete Diff → delete Full (now unblocked) |
| 36–37 | **COW rootfs** — create VM, take snapshot |
| 38–40 | Restore via dm-snapshot: verify `/dev/mapper/cow-*` device active; exception store initially ≈ 0 MB actual disk usage |
| 41 | Restored agent `/health` responds |
| 42 | Delete restored VM: verify dm device, loop devices, and `.cow` file all cleaned up |
| 43 | Delete snapshot and verify empty |
| 45–47 | **Agent proxy** — `GET /vms/{id}/health`, `POST /vms/{id}/stop` via control plane proxy; no direct VM IP access |
| 48–49 | **`EPHEMERA_PUBLIC_URL`** — restart daemon with var set; verify `agent_url` becomes proxy path; use `agent_url` for health + stop |
| 51 | Prep role profile yaml files from `.example` placeholders |
| 52 | **Flock spawn** — `POST /flocks` with 5 roles (orchestrator/researcher×2/worker/reviewer) returns 201, `agents.length == 5`, valid `townwall_url` |
| 53 | `GET /vms` shows all 5 flock members |
| 54 | `POST /flocks/{id}/post` accepts a message and persists it |
| 55 | `GET /flocks/{id}/wall/history` returns ≥ 2 entries (orchestrator init message + step 54 post) |
| 56 | `GET /flocks` lists the new flock |
| 57 | **Flock teardown** — `DELETE /flocks/{id}` returns 200; all 5 VMs and the flock registry entry are gone |
| 58 | Daemon graceful shutdown |

**Example output (passing, flock steps 51–58):**

```
━━━ 51. Prep role profile yaml files ━━━
  ✓ Profile yaml files ready

━━━ 52. Create flock with 5 agents ━━━
  ✓ POST /flocks (HTTP 201)
  ✓ Spawned 5 agents in flock flock-1778665945495324840
  ✓ townwall_url: http://localhost:3000/flocks/flock-1778665945495324840/wall

━━━ 53. Verify /vms shows the 5 flock members ━━━
  ✓ Found 5 VM(s) running

━━━ 54. Post a message to the Town Wall ━━━
  ✓ POST /flocks/flock-1778665945495324840/post (HTTP 200)
  ✓ Town Wall accepted the post

━━━ 55. Retrieve Town Wall history ━━━
  ✓ Town Wall has 2 entries

━━━ 56. Verify GET /flocks lists the new flock ━━━
  ✓ GET /flocks returns 1 entry(ies)

━━━ 57. Delete flock and verify all member VMs are torn down ━━━
  ✓ DELETE /flocks/flock-1778665945495324840 (HTTP 200)
  ✓ All flock VMs torn down
  ✓ Flock unregistered from manager

━━━ 58. Shut down daemon ━━━
  ✓ Daemon stopped

══════════════════════════════════
  All test steps passed ✓
══════════════════════════════════
```

---

## Configuration

All settings are read from environment variables at startup.

| Variable | Default | Description |
|----------|---------|-------------|
| `EPHEMERA_API_ADDR` | `127.0.0.1:3000` | Control plane bind address. Set to `0.0.0.0:3000` when behind a reverse proxy. |
| `EPHEMERA_API_PORT` | `3000` | Port only (used when `EPHEMERA_API_ADDR` is not set). |
| `EPHEMERA_API_TOKENS` | *(unset)* | Per-client Bearer tokens: `alice:token1,bob:token2`. Preferred. |
| `EPHEMERA_API_TOKEN` | *(unset)* | Single Bearer token (backward-compatible fallback). |
| `EPHEMERA_AGENT_PORT` | `8080` | Port goose-agent listens on inside each VM. |
| `EPHEMERA_PUBLIC_URL` | *(unset)* | Externally-reachable base URL of the control plane (no trailing slash). When set, `agent_url` in VM responses uses the proxy path `{EPHEMERA_PUBLIC_URL}/vms/{vm_id}` instead of the VM's private IP. Example: `https://api.example.com`. |
| `EPHEMERA_DISK_MODE` | *(unset)* | Set to `cow` to provision spawn disks as a dm-snapshot view of the golden image (~0 MiB initial usage) instead of a 700 MiB full copy. Default behavior is preserved when unset. |

`EPHEMERA_API_ADDR` takes precedence over `EPHEMERA_API_PORT`. All variables are read at startup; use SIGHUP to reload tokens without restarting.

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

### Agent Proxy (via Control Plane)

The control plane proxies the three agent endpoints, making them accessible to external clients without direct access to the private VM subnet. Authentication uses the **control plane Bearer token** — the agent token is injected internally.

```
POST /vms/{vm_id}/tasks    → proxied to goose-agent /tasks
GET  /vms/{vm_id}/health   → proxied to goose-agent /health  (no auth required)
POST /vms/{vm_id}/stop     → proxied to goose-agent /stop
```

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

A **flock** is one `POST /flocks` call that spawns one VM per requested role and registers them under a shared flock ID. Each role string is mapped through `LookupProfile` to (vCPU, memory, profile directory, system prompt), so a single request can produce a heterogeneous group of agents that all share a Town Wall log.

> Role names map through built-in profiles. Unknown names spawn at default sizing (2 vCPU / 2048 MiB) and look for `configs/profiles/{name}/` for goose config files.

#### Spawn a flock

```
POST /flocks
Content-Type: application/json

{
  "task": "Add dark mode toggle to login page",
  "roles": ["orchestrator","researcher","researcher","worker","reviewer"]
}
```

```bash
curl -X POST http://localhost:3000/flocks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "task":"Add dark mode toggle to login page",
        "roles":["orchestrator","researcher","researcher","worker","reviewer"]
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
  "townwall_url": "http://localhost:3000/flocks/flock-1778665945495324840/wall",
  "post_url":     "http://localhost:3000/flocks/flock-1778665945495324840/post"
}
```

If any VM fails to spawn, every VM spawned so far is torn down and the flock is removed before the error response — partial flocks are never left running. The max flock size is **20** to bound IP-pool / TAP exhaustion.

#### Post to the Town Wall

```bash
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/post \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"researcher-1","body":"Found existing dark mode CSS variables"}'
```

Inside a flock VM, the same effect can be achieved via the bundled `gtwall` CLI:

```bash
# inside the VM (via SSH-equivalent flow or the agent /tasks shell)
gtwall "Claiming src/styles/theme.css"
```

`gtwall` reads `/root/.ephemera-flock` for the flock context, calls the in-VM `goose-agent /townwall/post`, which forwards to the control plane.

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

#### Tear down a flock

```bash
curl -X DELETE http://localhost:3000/flocks/$FLOCK_ID \
  -H "Authorization: Bearer $TOKEN"
# {"status":"deleted","flock_id":"flock-..."}
```

Destroys every member VM in parallel and removes the flock from the registry. The Town Wall log on disk (`flocks/<id>/TOWN_WALL.log`) is left in place as an audit artifact.

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

Returns when the task completes. Only one task runs at a time per VM; concurrent requests receive `503 agent busy`. If `/root/.goose-system-prompt` is present (injected by `PrepareVM` for flock members), it is prepended to the user prompt as `[SYSTEM INSTRUCTIONS]\n...\n\n[USER TASK]\n...` before being piped to Goose.

#### Post to Town Wall (flock members only)

```bash
curl -X POST http://10.0.1.10:8080/townwall/post \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $AGENT_TOKEN" \
  -d '{"body":"Claiming src/styles/theme.css"}'
```

Reads `FLOCK_ID` and `AGENT_ID` from `/root/.ephemera-flock` (written by `PrepareVM` for flock members) and forwards the message to the host control plane's `POST /flocks/{id}/post`. Returns `400` when the VM is not a flock member. The bundled `gtwall` shell wrapper calls this endpoint.

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

Unknown names also work — they spawn at the default `2 vCPU / 2048 MiB` and look up `configs/profiles/{name}/`.

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

Omitting `profile` (or sending an empty body) uses `configs/goose.yaml` and `configs/goose-secrets.yaml` at the legacy 2 vCPU / 2048 MiB sizing.

If the profile directory has a `system.md`, its contents are written into the VM as `/root/.goose-system-prompt` and the in-VM `goose-agent` prepends it to every `/tasks` prompt — so the role stays in-character even when the orchestrator dispatches plain user prompts.

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
  snapshot_type: "full"          ← 2 GB memory.bin

... VM runs tasks, dirties pages ...

POST /vms/{id}/snapshot          ← second call (auto-detects prior Full)
  snapshot_type: "diff"          ← sparse memory.bin, only dirty pages
  base_snapshot_id: snap-xxx     ← references the Full above

POST /snapshots/{diff-id}/restore
  → MergeMemoryDiff(full.memory.bin + diff.memory.bin → tmp/merged.bin)
  → RestoreMachine(merged.bin, diff.state.bin)
  → os.Remove(merged.bin)        ← temp file cleaned up after VM starts
```

> **Disk space during restore**: the merge step writes a temporary 2 GB `merged.bin` alongside the existing base and diff files. Ensure the host has at least 2 GB of free space in the Ephemera working directory before restoring a diff snapshot. The file is removed as soon as Firecracker has opened it.

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

API tokens can be updated without restarting the daemon or interrupting running VMs:

```bash
# Update the environment variable and send SIGHUP
export EPHEMERA_API_TOKENS="alice:$NEW_ALICE,carol:$CAROL_TOKEN"
kill -HUP $(pgrep ephemera-daemon)
```

The daemon re-reads `EPHEMERA_API_TOKENS` / `EPHEMERA_API_TOKEN` and swaps the in-memory client list. All running VMs continue unaffected.

| Scenario | Action |
|----------|--------|
| Adding a new client | Update env var → SIGHUP |
| Rotating a token | Update env var → SIGHUP |
| Emergency revocation | Update env var → SIGHUP — **no VM interruption** |

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

## Known Limitations

| Limitation | Detail |
|------------|--------|
| **Single-host** | All VMs run on one physical host. Multi-host clustering is not supported. |
| **Same-snapshot concurrent restores not supported** | The guest IP is reconfigured via vsock after restore, so different-snapshot concurrent restores each get a fresh IP. However, two VMs from the *same* snapshot would still collide on the Firecracker vsock UDS path (which is fixed in `state.bin`), so same-snapshot concurrent restores are not supported. |
| **Cross-machine restore** | Supported manually: copy the `snapshots/<id>/` directory to the target host at the same absolute path, then call `POST /snapshots/{id}/restore`. Automated transfer is not built in. |

---

## License

MIT — see [LICENSE](LICENSE).
