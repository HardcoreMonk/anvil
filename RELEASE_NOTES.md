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
