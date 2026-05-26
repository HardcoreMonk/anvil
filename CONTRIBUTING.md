# Contributing to Ephemera

Thanks for considering a contribution. Ephemera spins up Firecracker MicroVMs as ephemeral runtimes for AI agents, and the moving parts (KVM, host networking, snapshot lifecycle, in-VM binaries, golden image bake) make it easy to fall into a few traps the first time. This guide focuses on what is *specific* to this project; for an overview of features, configuration, and the API surface, read [`README.md`](README.md) first.

## Local development setup

Host requirements:

- Ubuntu 22.04 or 24.04 (bare metal, or a VM with nested virtualization enabled)
- `/dev/kvm` accessible to your user (the daemon itself runs as root)
- Host packages: `curl`, `debootstrap`, `e2fsprogs`, `util-linux`, `jq` (e2e), `dmsetup` (snapshot tests)
- Go (any version supporting the module's `go` directive in `go.mod`)

First-time setup:

```bash
git clone <your fork>
cd ephemera
go build -o ephemera-daemon ./cmd/goose-daemon/
sudo ./ephemera-daemon          # bootstraps artifacts/ + golden image (~5 min)
```

The daemon self-bootstraps the golden image, kernel, Firecracker binary, and the two in-VM binaries (`goose-agent`, `micro-init`) on first start. Subsequent starts are fast — these artifacts are cached under `artifacts/` and only rebuilt when sources are newer (mtime check). You should never need to `rm artifacts/*` by hand after editing Go code; the next daemon start will detect staleness and rebuild.

Two binary classes live in this repo and they are built differently:

| Class | Binaries | Build flags |
|------|---------|-------------|
| Host-side | `ephemera-daemon` | default (`go build`) |
| In-VM | `goose-agent`, `micro-init` | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` |

The daemon's `Ensure{GooseAgent,MicroInit}` helpers set the right flags automatically. Do **not** `go build -o artifacts/goose-agent ./cmd/goose-agent/` with default flags on a non-Linux host — the resulting binary will not run inside the guest, but the daemon's mtime check will then accept it and you will silently ship a broken image.

## Configuration files & secrets

Files committed to the repo:

- `configs/profiles/<role>/goose.yaml.example`
- `configs/profiles/<role>/goose-secrets.yaml.example`

Files that must **never** be committed:

- `configs/profiles/<role>/goose.yaml`
- `configs/profiles/<role>/goose-secrets.yaml`
- `configs/goose.yaml`, `configs/goose-secrets.yaml`
- Anything containing real LLM API keys

`.gitignore` already covers these patterns. To run flocks against real LLMs, copy each `*.example` to its real-name counterpart and fill in keys locally — the e2e test does this automatically with placeholder keys (sufficient for the spawn path, which never calls the LLM).

## Tests

Two layers, run them at different gates:

| Layer | Command | When to run | Cost |
|-------|---------|-------------|------|
| Unit | `go test ./...` | Every change, before every PR | Seconds |
| End-to-end | `sudo bash e2e_test.sh` | Any change touching VM lifecycle, networking, storage, snapshots, or flock orchestration | 15–30 min, root + `/dev/kvm` |

Always run unit tests + `go vet ./... && go build ./...` before opening a PR.

Run the full e2e when your diff touches:

- `internal/vm/`, `internal/network/`, `internal/storage/`
- `internal/orchestrator/` or `cmd/goose-daemon/orchestrator_api.go`
- `cmd/goose-agent/`, `cmd/micro-init/`
- `scripts/build_image.sh`, `scripts/gtwall`
- The control-plane API surface in `cmd/goose-daemon/api.go`

The first e2e run rebuilds the golden image (~5 min) if any in-VM source has changed; subsequent runs reuse the cached image. The script's pre-flight kills any stale `ephemera-daemon` process and cleans `/tmp/goose-workspaces/`, `snapshots/snap-*`, and `flocks/flock-*` — you don't need to clean up between runs.

## Areas that need extra care

These are the parts that have surprised contributors before. Read the existing code carefully and consult the relevant test before changing.

**KVM / Firecracker (`internal/vm/`)** — Cold boot, snapshot creation, and snapshot restore are three distinct paths. A change that fixes one can break another; e2e steps 10–51 cover all three (full snapshot, diff snapshot, COW restore). Always preserve unconditional cleanup on error: every allocated TAP / disk / dm-snapshot must roll back if a later step fails.

**Networking (`internal/network/`)** — IP/TAP allocation is concurrent and the bridge `goose-br0` is shared across all VMs. Tests must exercise concurrent `Allocate`/`Release` cycles. The bridge survives daemon restarts intentionally — don't add code that tears it down on shutdown.

**Snapshots (`internal/storage/snapshot.go`)** — The guest IP is baked into the snapshot's memory state via the kernel `ip=` boot parameter, so same-snapshot concurrent restore is unsupported (two restores would both claim the same IP). Diff snapshots reference a Full base; the Full cannot be deleted while any Diff references it (returns 409). Don't change deletion ordering without updating the dependency check.

**Rootfs diff snapshots (v0.4.0, `internal/storage/snapshot.go` + `restoreSnapshot`)** — A diff snapshot's `rootfs.diff` is a sparse 4 KiB-block delta vs its base's `rootfs.ext4`; `WriteRootfsDiff` computes it by **byte-comparing** current vs base, NOT by walking source holes — the source can be a bind-mounted dm-snapshot block device (COW / restored source VM) that reports no holes. On restore, `MergeRootfsDiff` rebuilds a full ext4 that becomes the dm-snapshot read-only origin; it is `os.Remove`'d immediately after `SetupDMSnapshot` because `losetup` pins the inode for the VM's life (freed by `losetup -d` in teardown). **Never place the merged rootfs on `/dev/shm`** (unlike the merged memory file) — the loop-device pin would hold ~570 MB of tmpfs RAM per restored VM for its whole life. Restore must key the rootfs-merge branch on `meta.RootfsDiffPath != ""`, not `SnapshotType == "diff"`, or legacy diff snapshots (sparse memory + a full `rootfs.ext4`) stop restoring.

**Generated artifacts (`artifacts/`)** — `golden-image.ext4`, `goose-agent`, `micro-init`, `firecracker`, `vmlinux.bin`. All are auto-rebuilt or auto-downloaded by the daemon. Do not commit them. The mtime-based staleness check covers source edits but **not** flag changes — if you change `CGO_ENABLED` / `GOOS` / `GOARCH` for an in-VM binary, delete the cached artifact manually.

**Golden image bake (`scripts/build_image.sh`)** — Editing this triggers a ~5-minute rebuild on the next daemon start (cascades through to the golden image staleness check). Batch image-related changes; don't iterate on small tweaks.

**API authentication** — Both the control plane and the in-VM goose-agent use Bearer tokens. When adding an endpoint, decide explicitly whether it needs auth and use the existing middleware (`authMiddleware` for the control plane, `agentAuthMiddleware` for the agent). The agent's `/health` is intentionally unauthenticated — the host's health-poller relies on this. The agent's `/townwall/post` is intentionally not proxied through the control plane (external callers should use `/flocks/{id}/post` instead).

**`EPHEMERA_API_ADDR` for flocks** — The default `127.0.0.1:3000` is loopback-only. Inside a flock VM, `gtwall` and the agent's `/townwall/post` forwarder target `http://10.0.1.1:3000` (the bridge gateway). For flocks to work end-to-end, start the daemon with `EPHEMERA_API_ADDR=0.0.0.0:3000` (or any address that includes the bridge IP).

**Cold-restart state (v0.3.2; COW recovery v0.4.0)** — `vms/<vm_id>/state.json` is the source of truth for cold-restart. When you add fields to `runningVM`, decide whether they need to survive a daemon restart; if yes, persist them via `storage.VMState` and reconstruct them in `RecoverVMs` (`cmd/goose-daemon/recovery.go`). Don't rely on Firecracker `*Machine` invariants in recovery — the SDK does not re-attach to a running process, so recovery boots a fresh Firecracker with the same socket/TAP/IP/MAC. `EPHEMERA_DISK_MODE=cow` spawn VMs are recovered by re-layering the preserved exception store (`<workspace>/<vm_id>.cow`) over the golden image: `DestroyAll` keeps the store (`TeardownDMSnapshotKeepStore`, no `DeleteVMState`) and `RecoverVMs` calls `ReclaimCOWDeviceKeepStore` (clears a crashed run's stale dm device, store-preserving) then `SetupDMSnapshot`. **This assumes the golden image is unchanged since spawn** — a rebuild (the mtime staleness check) invalidates the block-level COW layering, so never recover a COW VM across a golden-image bake. Snapshot-restored VMs persist no `state.json` and are intentionally not recovered (their base is a snapshot copy, not the golden image). The shutdown-path preserve/teardown decision keys on `storage.VMStateExists`, so only state-backed (spawn) VMs are preserved.

**Graceful shutdown vs. `destroyVM` (v0.3.2)** — these are two distinct teardown paths and they intentionally behave differently. `destroyVM` is the explicit-DELETE path: it removes `state.json`, the rootfs ext4, and all transient files (full cleanup). `DestroyAll` is the deferred-on-SIGTERM path in `main`: it stops Firecracker and releases network resources but **preserves `state.json` + rootfs** so the next daemon start cold-restarts the VM. If you add cleanup code, decide which of these two paths it belongs to. A common mistake (caught in the v0.3.2 cycle) is to put cleanup inside `destroyVM` and call it from `DestroyAll` for the "shared" case — that drops the persisted state and silently breaks cold-restart.

**Flock metadata writes (v0.3.3)** — Always go through `Flock.Persist(workDir)` rather than calling `orchestrator.SaveFlockMetadata` directly. The helper holds a per-flock `writeMu` around the tmp+rename, which is the only thing keeping concurrent writers (createFlock, watchdog `onFailure`, recovery `markFlockAgentDead`, per-agent restart) from tearing each other's writes. The raw `SaveFlockMetadata` is kept as the persistence primitive used by `Flock.Persist` itself and by tests that have no live Flock; calling it from a daemon path silently dodges the serialization invariant.

**Per-agent restart token semantics (v0.3.3)** — `restartAgent` deliberately reuses the existing `agent_token` so caller-side caches keep working across a restart. The mechanism is `spawnVMOptions.AgentToken`: empty triggers fresh generation (the standalone spawn path), non-empty reuses verbatim (the restart path). Changing this contract is a breaking change — every caller that previously cached the token would start hitting 401s after restarts.

**In-VM control-plane token (v0.3.3, hot-rotated v0.3.4)** — The host injects `apiClients[0].Token` into each flock VM at `/root/.ephemera-cp-token` (mode 0600) via `spawnVMOptions.ControlPlaneToken`. Standalone `POST /vms` deliberately does NOT inject (non-flock VMs have no `/townwall/post` use case), so a confusing empty file does not litter every disk. v0.3.4 propagates rotations on SIGHUP via the `SET_CP_TOKEN` vsock command — only meaningful when the daemon reads tokens from `EPHEMERA_API_TOKENS_FILE` (env values cannot change without re-exec).

**CP token vsock fan-out (v0.3.4)** — `ReloadClients` snapshots `cp.vms` under `cp.mu.RLock()` (NOT `clientsMu`), then fans `vm.SetGuestCPToken` calls out as parallel goroutines. Per-VM failures MUST be logged, not propagated — a stuck VM cannot block the SIGHUP path. The 1 s per-VM budget × `len(vms)` parallelism caps total wall-clock. If you add new mutable VM-level state that needs to survive rotation, mirror this pattern: snapshot under `cp.mu.RLock()`, dispatch in parallel, swallow per-VM errors.

**Watchdog tunables + auto-heal (v0.3.4)** — `Watchdog.Configure` is the only public entry to override `interval` / `httpTimeout` / `dyingThreshold` / `autoHeal`; the underlying fields stay unexported so external callers cannot bypass the `interval >= httpTimeout` clamp. In-package tests still set fields directly — that's fine and intentional. `EPHEMERA_WATCHDOG_AUTO_HEAL` MUST default to off; `TestWatchdog_MarksDeadAfterThreshold` is the sticky-dead contract and changes that flip a once-dead agent back to ready without the opt-in flag are a regression by definition.

**Firecracker SDK signal forwarding (v0.3.4 hot-fix)** — `firecracker-go-sdk` v1.0.0's `firecracker.Config.ForwardSignals` defaults to `[SIGINT, SIGQUIT, SIGTERM, SIGHUP, SIGABRT]` and installs a goroutine (`setupSignals` in the SDK) that forwards each received signal to the Firecracker child via `cmd.Process.Signal(sig)`. The daemon uses `SIGHUP` for token hot reload, so the default would kill every running Firecracker the moment a reload signal arrives. `internal/vm/machine.go` defines a package-level `forwardSignals` slice that **deliberately omits `SIGHUP`** and passes it to both `StartMachine` and `RestoreMachine`'s `firecracker.Config`. If you add new SDK call sites (a third spawn path, a re-attach path, etc.), set `ForwardSignals: forwardSignals` there too — leaving it nil silently re-enables the bug. If we ever stop using `SIGHUP` for hot reload, the explicit list can shrink, but never grow back to include `SIGHUP` while reload semantics are tied to it.

**Metrics counter discipline (v0.3.5)** — `internal/metrics/` is a hand-written exposition formatter; the registry validates metric and label names against `[a-zA-Z_:][a-zA-Z0-9_:]*` at registration time and panics on a duplicate or malformed name. When adding a counter, keep the `outcome` label set closed (`ok` / `fail` only — no free text); when adding a `type` label, document the closed set in the README catalogue. The `orchestrator/` package intentionally does NOT import `internal/metrics/`; the daemon wires watchdog observations via the `OnDead` / `OnHeal` / `OnProbeDuration` callbacks set on `Watchdog`. Keeping that direction one-way prevents a package cycle when watchdog tests run with no daemon-level registry.

**slog message convention (v0.3.5)** — `cmd/goose-daemon/`, `internal/storage/`, `internal/orchestrator/`, and `internal/network/` use `log/slog` (not the standard `log` package). Messages are short, lowercase, present-tense phrases — `"vm spawn start"`, `"recovery: disk missing"`, `"snapshot: copying disk"`. Structured fields are snake_case (`vm_id`, `flock_id`, `agent_id`, `snapshot_id`, `err`). Don't smuggle dynamic data into the message; use a field. Lifecycle events (recovery, spawn, destroy, watchdog transitions) are emitted at `Warn` so the default `EPHEMERA_LOG_LEVEL=warn` still surfaces them. `cmd/goose-agent/` retains its original `log.Printf` output this cycle — touching the agent triggers a golden-image rebuild, which is deferred to v0.4.3.

**Per-VM stats PID resolution (v0.3.5)** — `firecracker-go-sdk` does not expose the Firecracker child's PID. `cmd/goose-daemon/stats_collector.go` resolves it by tracing `runningVM.socketPath` through `/proc/net/unix` (path → inode) and then `/proc/<pid>/fd` (inode → process), with a `comm == "firecracker"` prefilter to skip kernel threads. The resolved PID is cached on `runningVM.fcPID` (atomic). On every stats request, the cached PID is re-validated by reading `/proc/<pid>/comm`; a process death or PID reuse for a non-firecracker binary invalidates the cache and forces a fresh lookup. Do not skip this check — Linux PID reuse is rare but real, and a stale cache could read another process's `/proc/<pid>/stat` and report its CPU to the wrong VM.

**In-VM helper JSON construction (v0.3.6)** — `scripts/gtwall` and `scripts/gtcall` build their request bodies with `jq -n --arg` (e.g. `jq -n --arg b "$1" '{body: $b}'`), never hand-rolled `sed` escaping. A `sed`-only escape (the pre-v0.3.6 gtwall, which escaped quotes and backslashes but not newlines) leaves raw newlines inside the JSON string, which the in-VM goose-agent rejects with HTTP 400 — and curl `-f` surfaces that as the misleading "exited with code 22". The bug was latent for the entire v0.3.x line because gtwall had only ever posted single-line status messages; the first multi-line body (a whole source file framed in `<<<FILE:>>>` sentinels) exposed it. Any new in-VM helper that posts JSON must use `jq --arg`. Both scripts live in the golden image (`build_image.sh` step 5b) and are in `EnsureGoldenImage`'s input list, so editing either triggers a rebuild.

**goose-agent task output parsing (v0.3.6)** — `cmd/goose-agent/main.go` runs `goose run --output-format json` and parses the envelope in `extractGooseJSONText`: it slices from the first `{` to the last `}` (Goose prints a startup banner to stdout even in JSON mode), unmarshals `messages[].content[]`, and concatenates every assistant `text` block — falling back to raw stdout if the envelope will not parse. This is the only working escape from goose-cli's `streaming_buffer.rs` 50-line code-block truncation (neither `--debug` nor `GOOSE_SHOW_FULL_OUTPUT` disables it). Reuse the banner-skip-slice idiom for any future feature that shells out to `goose run`. Touching `goose-agent` triggers a golden-image rebuild — batch it with other in-VM changes.

**Autonomous orchestration prompts (v0.3.6)** — `webdev_demo.sh`'s orchestrator prompt (`configs/profiles/orchestrator/system.webdev.md`) must forcefully enforce loop completion: Gemini agentic loops in `goose run` stop after a few *successful* tool calls unless the prompt forbids mid-task summaries and makes the irreversible step (publish via `gtwall`) mandatory right after generation. Errors keep the loop alive (the model retries); successes invite a premature stop. Use `gemini-2.5-flash` (not `-lite`) for any role that must drive a multi-step tool-calling loop — `-lite` plans and then quits. This is a paid-tier-only choice (free tier shares a 20 RPM cap across all models).

## PR expectations

- **One logical change per PR.** Mixed PRs are slow to review and risky to revert.
- **Small diffs.** ≤ 300 lines of net change is a good target. If a feature genuinely needs more, consider splitting it (refactor → feature → tests).
- **No half-finished implementations.** A PR that adds a struct field but doesn't wire it to anything is dead code.
- **Comments only when the *why* is non-obvious.** Don't restate what the code does. Don't reference the PR or task.
- **English** for all code comments, log messages, and user-facing strings.
- **Run before pushing**: `go build ./... && go vet ./... && go test ./...`. For diffs in the "extra care" areas above, also run the full e2e and paste the passing tail in the PR description.
- **Don't touch `RELEASE_NOTES.md`** in everyday PRs. It's updated as part of a dedicated release prep PR.

## Bug reports & feature requests

File a GitHub issue.

Bug reports help the most when they include:

- Host OS and version
- The relevant tail of the daemon log (the script logs to `/tmp/ephemera-test-*.log`)
- Reproduction steps — ideally a minimal script or curl sequence
- What you expected vs. what you saw

Feature requests should lead with the **use case** before the proposed solution. Many requests can be satisfied by an existing API or env var; describing the goal first lets us check that.

For security issues, please report privately via GitHub Security Advisories rather than opening a public issue.
