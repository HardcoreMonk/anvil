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

**KVM / Firecracker (`internal/vm/`)** — Cold boot, snapshot creation, and snapshot restore are three distinct paths. A change that fixes one can break another; e2e steps 11–49 cover all three (full snapshot, diff snapshot, COW restore). Always preserve unconditional cleanup on error: every allocated TAP / disk / dm-snapshot must roll back if a later step fails.

**Networking (`internal/network/`)** — IP/TAP allocation is concurrent and the bridge `goose-br0` is shared across all VMs. Tests must exercise concurrent `Allocate`/`Release` cycles. The bridge survives daemon restarts intentionally — don't add code that tears it down on shutdown.

**Snapshots (`internal/storage/snapshot.go`)** — The guest IP is baked into the snapshot's memory state via the kernel `ip=` boot parameter, so same-snapshot concurrent restore is unsupported (two restores would both claim the same IP). Diff snapshots reference a Full base; the Full cannot be deleted while any Diff references it (returns 409). Don't change deletion ordering without updating the dependency check.

**Generated artifacts (`artifacts/`)** — `golden-image.ext4`, `goose-agent`, `micro-init`, `firecracker`, `vmlinux.bin`. All are auto-rebuilt or auto-downloaded by the daemon. Do not commit them. The mtime-based staleness check covers source edits but **not** flag changes — if you change `CGO_ENABLED` / `GOOS` / `GOARCH` for an in-VM binary, delete the cached artifact manually.

**Golden image bake (`scripts/build_image.sh`)** — Editing this triggers a ~5-minute rebuild on the next daemon start (cascades through to the golden image staleness check). Batch image-related changes; don't iterate on small tweaks.

**API authentication** — Both the control plane and the in-VM goose-agent use Bearer tokens. When adding an endpoint, decide explicitly whether it needs auth and use the existing middleware (`authMiddleware` for the control plane, `agentAuthMiddleware` for the agent). The agent's `/health` is intentionally unauthenticated — the host's health-poller relies on this. The agent's `/townwall/post` is intentionally not proxied through the control plane (external callers should use `/flocks/{id}/post` instead).

**`EPHEMERA_API_ADDR` for flocks** — The default `127.0.0.1:3000` is loopback-only. Inside a flock VM, `gtwall` and the agent's `/townwall/post` forwarder target `http://10.0.1.1:3000` (the bridge gateway). For flocks to work end-to-end, start the daemon with `EPHEMERA_API_ADDR=0.0.0.0:3000` (or any address that includes the bridge IP).

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
