# anvil Runtime Architecture

## Status

- Baseline: `v0.2.0`
- Product name: `anvil`
- Repository/module legacy name: `ephemera`
- Runtime owner files: `cmd/goose-daemon/`, `cmd/goose-agent/`,
  `cmd/micro-init/`, `internal/storage/`, `internal/network/`, `internal/vm/`

This document describes the runtime architecture only. Service request flows are
documented in [service-logic.md](service-logic.md). IronClaw MCP integration is
documented in [mcp-architecture.md](mcp-architecture.md).

## System View

```text
External client
  |
  | HTTPS, when exposed through a TLS proxy
  v
Reverse proxy
  |
  | HTTP + control-plane Bearer token
  v
anvil control plane daemon :3000
  |
  | Firecracker SDK, KVM, TAP, rootfs, snapshot files
  v
Firecracker MicroVM
  |
  | PID 1
  v
micro-init
  |
  | starts and supervises
  v
goose-agent :8080
  |
  | runs
  v
goose CLI task
```

The control plane is the single host-side runtime coordinator. It owns VM
lifecycle, network allocation, disk preparation, snapshot metadata, snapshot
restore, and proxying into the VM-side agent.

## Component Responsibilities

| Component | Files | Responsibility |
|---|---|---|
| Control plane daemon | `cmd/goose-daemon/main.go`, `cmd/goose-daemon/api.go`, `cmd/goose-daemon/config.go` | Bootstraps host artifacts, starts the HTTP API, authenticates clients, manages running VMs, proxies agent calls, creates/restores/deletes snapshots |
| Storage provisioner | `internal/storage/provisioner.go` | Builds or verifies the golden image, clones per-VM disks, injects Goose config/secrets, writes the per-VM agent token, injects timezone data |
| Snapshot storage | `internal/storage/snapshot.go` | Persists snapshot metadata, copies rootfs, creates COW restore devices, tears down COW resources, merges diff memory snapshots |
| VM wrapper | `internal/vm/machine.go` | Builds Firecracker configs, starts cold VMs, restores VMs from snapshot state, reconfigures guest IP through vsock |
| Network manager | `internal/network/manager.go` | Creates `goose-br0`, manages `10.0.1.0/24`, allocates/recycles guest IPs and TAP devices, configures NAT |
| Guest init | `cmd/micro-init/main.go` | Runs as PID 1, mounts guest virtual filesystems, starts `goose-agent`, powers off the VM cleanly on exit or signal |
| Guest agent | `cmd/goose-agent/main.go` | Exposes `/tasks`, `/health`, `/stop` inside the guest, enforces per-VM token auth for mutating endpoints, runs Goose tasks |
| Image builder | `scripts/build_image.sh` | Builds the Debian Bookworm based golden rootfs with Goose, `goose-agent`, and `micro-init` |

## Runtime State

Host memory state:

| State | Owner | Meaning |
|---|---|---|
| `ControlPlane.vms` | `cmd/goose-daemon/api.go` | Running VM registry keyed by `vm_id` |
| `ControlPlane.snapshots` | `cmd/goose-daemon/api.go` | Loaded snapshot metadata keyed by `snapshot_id` |
| `ControlPlane.clients` | `cmd/goose-daemon/api.go` | Current control-plane API clients, reloaded on `SIGHUP` |
| `network.Manager.ipInUse` | `internal/network/manager.go` | Allocated private guest IPs |
| `network.Manager.freeTapIDs` | `internal/network/manager.go` | TAP IDs available for reuse |

Host disk state:

| Path | Meaning |
|---|---|
| `artifacts/golden-image.ext4` | Base rootfs cloned for each new VM |
| `artifacts/vmlinux.bin` | Firecracker-compatible Linux kernel |
| `artifacts/firecracker` | Firecracker binary, SHA256 verified on download |
| `artifacts/goose-agent` | VM-side HTTP agent binary |
| `artifacts/micro-init` | VM-side PID 1 binary |
| `/tmp/goose-workspaces/<vm_id>.ext4` | Per-VM writable rootfs clone for cold-spawned VMs |
| `/tmp/goose-workspaces/<vm_id>.cow` | Sparse exception store for COW-restored VMs |
| `snapshots/<snapshot_id>/memory.bin` | Full or sparse diff guest memory snapshot |
| `snapshots/<snapshot_id>/state.bin` | Firecracker device and machine state |
| `snapshots/<snapshot_id>/rootfs.ext4` | Snapshot rootfs copy |
| `snapshots/<snapshot_id>/metadata.json` | Restore metadata, including token, MAC, TAP, type, and base snapshot ID |

Guest disk state:

| Path | Meaning |
|---|---|
| `/root/.config/goose/config.yaml` | Injected Goose config |
| `/root/.config/goose/secrets.yaml` | Injected Goose secrets |
| `/root/.ephemera-agent-token` | Per-VM guest agent Bearer token, mode `0600` |
| `/usr/local/bin/goose-agent` | Guest task server |
| `/usr/local/sbin/micro-init` | Guest PID 1 |

## Startup Flow

```text
cmd/goose-daemon/main.go
  -> resolve project-relative artifact/config paths
  -> create snapshots/ if missing
  -> compile or reuse artifacts/micro-init
  -> compile or reuse artifacts/goose-agent
  -> create storage.Provisioner
       -> ensure /tmp/goose-workspaces exists
       -> ensure artifacts/golden-image.ext4 exists
       -> run scripts/build_image.sh if missing
  -> download or reuse Firecracker kernel
  -> download or reuse Firecracker binary with SHA256 verification
  -> create network.Manager
       -> create/raise goose-br0
       -> enable ip_forward
       -> add iptables MASQUERADE rule if missing
  -> create ControlPlane
       -> load persisted snapshots from snapshots/*/metadata.json
       -> register HTTP routes
  -> serve API
  -> handle SIGINT/SIGTERM shutdown
  -> handle SIGHUP token reload
```

The daemon is intentionally self-bootstrapping: a first run prepares the image,
kernel, Firecracker binary, and guest binaries. Later runs reuse existing
artifacts unless they are missing.

## VM Shape

Cold-spawned VMs are started by `vm.StartMachine` with:

| Setting | Value |
|---|---|
| vCPU | `2` |
| Memory | `2048 MiB` |
| Root drive | Per-VM ext4 clone |
| Network | One TAP interface attached to `goose-br0` |
| IP assignment | Kernel `ip=` boot argument, no DHCP |
| Init | `init=/usr/local/sbin/micro-init` |
| Dirty page tracking | Enabled for diff snapshot support |
| Vsock | Enabled for post-restore guest IP reconfiguration |

Restored VMs are started by `vm.RestoreMachine` with a Firecracker memory file
and `state.bin`. They do not run a kernel boot path. The guest resumes from the
snapshot state and is then assigned a fresh IP through the vsock reconfiguration
channel.

## Network Model

anvil creates a host bridge named `goose-br0` with gateway `10.0.1.1/24`.
Guest IPs are allocated from `10.0.1.2` through `10.0.1.254`.

Cold spawn:

```text
network.Manager.Allocate()
  -> choose first free guest IP
  -> choose a new or recycled TAP ID
  -> create tap<N>
  -> attach tap<N> to goose-br0
  -> generate deterministic MAC AA:FC:00:00:xx:yy
```

Snapshot restore:

```text
network.Manager.AllocateForRestore(original_tap, original_mac)
  -> allocate any free guest IP
  -> recreate original TAP name
  -> set original MAC
  -> attach TAP to goose-br0
  -> guest IP is changed later through vsock
```

The restore path needs the original TAP name and MAC because Firecracker snapshot
state embeds device identity. The guest IP is decoupled from the snapshot by the
post-restore `CHANGE_IP` command.

## Storage And Snapshot Model

New VM disks are full copies of `artifacts/golden-image.ext4` into
`/tmp/goose-workspaces/<vm_id>.ext4`. The control plane mounts the disk once to
inject Goose config, secrets, timezone, and the per-VM agent token.

Snapshot creation writes:

```text
snapshots/<snapshot_id>/
  memory.bin
  state.bin
  rootfs.ext4
  metadata.json
```

Snapshot type selection:

| Request | Result |
|---|---|
| No prior full snapshot for the VM | `full` |
| Prior full snapshot exists | `diff` |
| Explicit `type: "full"` | Force full |
| Explicit `type: "diff"` without base full | HTTP 400 |

Diff snapshots store sparse dirty memory pages and reference a full base snapshot
through `base_snapshot_id`. The rootfs is still copied fully for both full and
diff snapshots.

Restore prefers Linux device-mapper snapshot COW:

```text
snapshot rootfs.ext4, read-only base
  + sparse /tmp/goose-workspaces/<new_vm_id>.cow exception store
  -> /dev/mapper/cow-*
  -> bind-mounted over original disk path recorded in state.bin
```

If dm-snapshot setup fails, the daemon falls back to the legacy bind-mount path,
which copies the snapshot rootfs to a per-restore ext4 file.

## Security Boundaries

| Boundary | Mechanism |
|---|---|
| Host to client | Control-plane Bearer token, optional if no token env var is configured |
| Control plane to guest agent | Per-VM Bearer token injected into guest disk |
| Guest task isolation | Firecracker MicroVM with KVM boundary |
| Guest private network | Host-only `10.0.1.0/24` network behind `goose-br0` |
| External exposure | Expected to run behind a TLS-terminating proxy when exposed outside localhost |
| Secrets | Goose secrets injected into guest disk from ignored local config files |

`GET /health` on the guest agent is intentionally unauthenticated so the control
plane can poll readiness. Mutating guest endpoints require the per-VM agent token
unless the guest token file is missing.

## Concurrency Model

- `ControlPlane.mu` protects the running VM registry.
- `ControlPlane.snapshotsMu` protects in-memory snapshot metadata.
- `ControlPlane.clientsMu` protects API client token reloads.
- `ControlPlane.restoreMu` serializes disk setup plus Firecracker open during restore.
- `network.Manager.mu` protects IP and TAP allocation.
- `goose-agent` accepts one task at a time per VM. A second concurrent task gets
  `503 agent busy`.

## Current Constraints

- Same-snapshot concurrent restore is not supported because snapshot state embeds
  the original vsock UDS path. Different-snapshot concurrent restore is supported.
- A source VM must be deleted before restoring one of its snapshots.
- A full snapshot cannot be deleted while any diff snapshot references it.
- Diff restore temporarily needs enough disk space for a merged memory file.
- Control-plane auth is disabled if no API token environment variable is set.
- MCP v1 is not part of the runtime control plane. It is a client adapter.

## Source References

- `cmd/goose-daemon/main.go`
- `cmd/goose-daemon/api.go`
- `cmd/goose-daemon/config.go`
- `cmd/goose-agent/main.go`
- `cmd/micro-init/main.go`
- `internal/storage/provisioner.go`
- `internal/storage/snapshot.go`
- `internal/network/manager.go`
- `internal/vm/machine.go`
- `README.md`
- `CONTEXT.md`
