# anvil Service Logic

## Status

- Baseline: `v0.2.0`
- Scope: daemon HTTP behavior, VM lifecycle, agent proxying, snapshot lifecycle,
  guest agent behavior
- Out of scope: IronClaw MCP client behavior. See
  [mcp-architecture.md](mcp-architecture.md).

This document explains what each service operation does and which invariants it
must preserve. File-level architecture is documented in
[runtime-architecture.md](runtime-architecture.md).

## Service Boundary

The control plane daemon exposes one HTTP service:

| API group | Owner | Purpose |
|---|---|---|
| `/vms` | `cmd/goose-daemon/api.go` | Create/list/delete VMs |
| `/vms/{vm_id}/tasks` | `cmd/goose-daemon/api.go` | Proxy task execution to the guest agent |
| `/vms/{vm_id}/health` | `cmd/goose-daemon/api.go` | Proxy guest health |
| `/vms/{vm_id}/stop` | `cmd/goose-daemon/api.go` | Ask the guest agent to stop |
| `/vms/{vm_id}/snapshot` | `cmd/goose-daemon/api.go` | Create full or diff VM snapshots |
| `/snapshots` | `cmd/goose-daemon/api.go` | List stored snapshots |
| `/snapshots/{id}/restore` | `cmd/goose-daemon/api.go` | Restore a VM from snapshot |
| `/snapshots/{id}` | `cmd/goose-daemon/api.go` | Delete a snapshot |

Inside the VM, `goose-agent` exposes:

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /tasks` | Per-VM Bearer token | Run a Goose prompt |
| `GET /health` | None | Return `idle` or `busy` |
| `POST /stop` | Per-VM Bearer token | Gracefully stop the agent HTTP server |

External callers should use the control plane proxy endpoints rather than calling
the private guest IP directly.

## Control-Plane Authentication

All control-plane routes are wrapped by `authMiddleware`.

```text
incoming request
  -> load current API client list through cp.getClients()
  -> if no clients are configured, allow request
  -> compare Authorization header with every registered token
  -> reject unauthorized requests with 401 and JSON body
  -> log matched client name and pass request to route handler
```

Token comparison uses constant-time comparison and does not stop after the first
candidate. This avoids leaking partial token matches through timing.

`SIGHUP` triggers `ControlPlane.ReloadClients`, which reloads
`EPHEMERA_API_TOKENS` or `EPHEMERA_API_TOKEN` into memory without restarting the
daemon or interrupting running VMs.

## VM Spawn Logic

Route: `POST /vms`

Input:

```json
{
  "profile": "optional-profile-name"
}
```

Flow:

```text
spawnVM()
  -> decode optional JSON body
  -> trim profile name
  -> resolve config/secrets paths
       empty profile -> configs/goose.yaml + configs/goose-secrets.yaml
       named profile -> configs/profiles/<name>/{goose.yaml,goose-secrets.yaml}
       reject profile names with slash or backslash
  -> generate 32-byte random agent token
  -> allocate TAP, guest IP, and MAC
  -> clone golden image to /tmp/goose-workspaces/<vm_id>.ext4
  -> mount the disk once and inject config, secrets, token, timezone
  -> create Firecracker API socket and vsock UDS path
  -> start Firecracker through vm.StartMachine()
  -> register VM in cp.vms
  -> poll http://<guest_ip>:8080/health for up to 60 seconds
  -> return VMSpawnResult with vm_id, guest_ip, agent_url, profile, agent_token
```

Failure cleanup:

| Failure point | Cleanup |
|---|---|
| Network allocation fails | Return `500` |
| Disk clone fails | Release TAP/IP |
| Disk preparation fails | Remove cloned disk, release TAP/IP |
| Firecracker start fails | Remove cloned disk, release TAP/IP |
| Agent readiness fails | Destroy the VM through `cp.destroyVM` |

The returned `agent_token` is sensitive. It is also held in memory by the control
plane so the proxy can call guest endpoints on the caller's behalf.

## VM List Logic

Route: `GET /vms`

Flow:

```text
listVMs()
  -> read cp.vms under lock
  -> return []VMInfo
```

The list response does not include `agent_token`.

## VM Delete Logic

Route: `DELETE /vms/{vm_id}`

Flow:

```text
stopVM()
  -> verify vm_id exists
  -> cp.destroyVM(vm_id)
  -> return {"status":"stopped","vm_id":"..."}
```

`destroyVM` performs the actual teardown:

```text
destroyVM()
  -> remove VM from cp.vms under lock
  -> StopVMM()
       Firecracker sends SIGTERM
       micro-init catches it
       micro-init asks goose-agent to exit
       micro-init calls poweroff(2)
  -> remove Firecracker socket/log/vsock files
  -> if COW-restored VM: TeardownDMSnapshot()
  -> else if legacy bind restore: TeardownBindMount()
  -> else remove cloned ext4 disk
  -> release TAP and IP back to network.Manager
```

## Agent Proxy Logic

Routes:

- `POST /vms/{vm_id}/tasks`
- `GET /vms/{vm_id}/health`
- `POST /vms/{vm_id}/stop`

Flow:

```text
proxyAgentEndpoint()
  -> find running VM by vm_id
  -> build private target URL http://<guest_ip>:8080/<agent_path>
  -> create request with incoming context and body
  -> preserve Content-Type
  -> inject "Authorization: Bearer <agent_token>" except for /health
  -> send request through cp.agentHTTPClient
  -> copy response headers, status code, and body back to caller
```

The proxy keeps external callers on one auth model: they authenticate to the
control plane only. The daemon injects the private agent token when needed.

## Snapshot Type Selection

`resolveSnapshotType(req.Type, vmID)` applies these rules:

| Request type | Result |
|---|---|
| `"full"` | Create a full snapshot |
| `"diff"` with an existing full base | Create a diff snapshot referencing the latest full snapshot |
| `"diff"` without a full base | Return an error |
| Empty or unknown value with no full base | Create a full snapshot |
| Empty or unknown value with a full base | Create a diff snapshot referencing the latest full snapshot |

The latest full snapshot is selected by `CreatedAt` among snapshots with the same
`source_vm_id`.

## Snapshot Create Logic

Route: `POST /vms/{vm_id}/snapshot`

Input:

```json
{
  "stop_after": false,
  "type": "full | diff | optional"
}
```

Flow:

```text
createSnapshot()
  -> parse optional body
  -> find running VM
  -> resolve full/diff type and base snapshot ID
  -> create snapshots/<snapshot_id>/
  -> pause VM
  -> CreateSnapshot(memory.bin, state.bin)
       diff snapshots pass Firecracker SnapshotType="Diff"
  -> copy /tmp/goose-workspaces/<vm_id>.ext4 to rootfs.ext4 while paused
  -> if stop_after=false: resume VM
  -> if stop_after=true: destroy source VM
  -> write metadata.json
  -> add metadata to cp.snapshots
  -> return public SnapshotInfo
```

Important invariants:

- Disk copy happens while the VM is paused.
- Diff snapshots still copy the full rootfs. Only memory is sparse/diff.
- `metadata.json` preserves the original TAP name, MAC, vsock path, agent token,
  disk path, memory path, state path, and base snapshot ID.
- Snapshot API responses do not expose `agent_token`.

## Snapshot Restore Logic

Route: `POST /snapshots/{id}/restore`

Flow:

```text
restoreSnapshot()
  -> load snapshot metadata from cp.snapshots
  -> reject restore if source VM is still running
  -> allocate a new VM ID
  -> remove stale Firecracker socket
  -> remove original vsock UDS path from snapshot metadata
  -> AllocateForRestore(original TAP, original MAC)
       returns original TAP name + any free guest IP
  -> lock cp.restoreMu
  -> try SetupDMSnapshot(rootfs.ext4, <new_vm_id>.cow, original disk path)
       creates read-only loop for base rootfs
       creates sparse exception store
       creates dm-snapshot device
       bind-mounts it over original disk path
  -> if dm-snapshot fails:
       release and fall back to restoreLegacyBindMount()
  -> if snapshot is diff:
       load base snapshot metadata
       MergeMemoryDiff(base.memory.bin, diff.memory.bin, tmp/<new_vm_id>-merged.bin)
  -> RestoreMachine(memory file, state.bin)
  -> unlock cp.restoreMu after Firecracker has opened the disk path
  -> remove temporary merged memory file
  -> ReconfigureGuestIP(original vsock path, new IP, gateway)
  -> register restored VM in cp.vms
  -> wait for guest agent health for up to 30 seconds
  -> return VMRestoreResult with source_snapshot_id and agent_token
```

The restored VM keeps the original agent token from the snapshot metadata. This
preserves caller access across snapshot/restore cycles.

Failure cleanup:

| Failure point | Cleanup |
|---|---|
| Network allocation fails | Return `409` |
| dm-snapshot setup fails | Release network, then attempt bind-mount fallback |
| Diff base missing | Tear down COW, release network, return `409` |
| Diff merge fails | Tear down COW, release network |
| Firecracker restore fails | Tear down COW, release network |
| Guest IP reconfiguration fails | Stop VMM, tear down COW, release network |
| Agent readiness fails | Destroy restored VM |

## Legacy Bind-Mount Restore Fallback

If `SetupDMSnapshot` fails, the daemon restores through `restoreLegacyBindMount`.
This path:

```text
  -> copies snapshot rootfs.ext4 to /tmp/goose-workspaces/<new_vm_id>.ext4
  -> bind-mounts that file over the original disk path from state.bin
  -> merges diff memory when needed
  -> RestoreMachine()
  -> ReconfigureGuestIP()
  -> registers VM with bindMountTarget for later teardown
```

This fallback is slower and uses more disk than COW restore, but keeps restore
functional on hosts without working dm-snapshot support.

## Snapshot Delete Logic

Route: `DELETE /snapshots/{id}`

Flow:

```text
deleteSnapshot()
  -> scan cp.snapshots for diffs whose base_snapshot_id == requested ID
  -> if any exist, return 409
  -> remove snapshot metadata from cp.snapshots
  -> remove snapshots/<id>/ from disk
  -> return {"status":"deleted","snapshot_id":"..."}
```

This prevents deleting a full snapshot that a diff snapshot still needs.

## Guest Agent Logic

`goose-agent` runs inside each VM.

Startup:

```text
main()
  -> read /root/.ephemera-agent-token
  -> start vsock CHANGE_IP listener
  -> register /tasks, /stop, /health
  -> listen on :8080 by default
```

Task execution:

```text
POST /tasks
  -> require method POST
  -> decode {"prompt":"..."}
  -> reject empty prompt
  -> if busy, return 503
  -> set busy=true
  -> run /usr/local/bin/goose run -i - with prompt on stdin
  -> return {"output":"..."} or {"output":"...","error":"..."}
  -> set busy=false
```

Health:

```text
GET /health
  -> return {"status":"idle"} or {"status":"busy"}
```

Stop:

```text
POST /stop
  -> return {"status":"stopping"}
  -> after 200 ms, gracefully shut down the HTTP server
```

Vsock IP reconfiguration:

```text
CHANGE_IP <cidr_ip> <gateway>
  -> ip addr flush dev eth0
  -> ip addr add <cidr_ip> dev eth0
  -> ip link set eth0 up
  -> ip route replace default via <gateway>
  -> return OK or ERROR
```

## Guest Init Logic

`micro-init` is PID 1 inside the VM.

Flow:

```text
micro-init
  -> mount /proc, /sys, /dev, /dev/pts
  -> set HOME, USER, PATH
  -> start /usr/local/bin/goose-agent
  -> wait for goose-agent exit or SIGTERM/SIGINT
  -> on signal, send SIGTERM to goose-agent
  -> sync
  -> poweroff(2)
```

This avoids the kernel panic that can happen when PID 1 exits without powering
off the guest cleanly.

## Error Model

- Control-plane auth failure: `401` with `{"error":"unauthorized"}`.
- Missing VM: usually `404`.
- Snapshot base dependency conflict: `409`.
- Invalid profile or invalid snapshot type request: `400`.
- Host/runtime setup failure: usually `500`.
- Agent proxy connection failure: `502`.

Some legacy paths still return plain text bodies. MCP v1 preserves daemon status
and body rather than normalizing every response.

## Operational Invariants

- Do not delete or mutate a running VM's disk outside `destroyVM`.
- Do not expose guest `agent_token` in list/snapshot responses.
- Do not restore from a snapshot while the source VM is still running.
- Do not delete a full snapshot while any diff references it.
- Always release TAP/IP on failed VM creation or restore.
- Always tear down dm-snapshot, loop devices, bind mounts, and sparse COW files on
  VM deletion.
- Keep `anvil_stop_vm` and `anvil_delete_vm` semantics distinct at the MCP layer:
  stop asks the guest agent to stop, delete destroys host VM resources.

## Source References

- `cmd/goose-daemon/api.go`
- `cmd/goose-daemon/config.go`
- `cmd/goose-agent/main.go`
- `cmd/micro-init/main.go`
- `internal/storage/snapshot.go`
- `internal/storage/provisioner.go`
- `internal/network/manager.go`
- `internal/vm/machine.go`
