# anvil 기여 가이드

이 저장소는 `HardcoreMonk/anvil`이며 `steve-seungeui/ephemera` fork network를
유지한다. ephemera는 Firecracker MicroVM runtime engine이고, anvil은 그 runtime을
IronClaw MCP 실행 계층으로 통합하는 downstream product fork다.

기여 전에 [README.md](README.md), [CONTEXT.md](CONTEXT.md),
[AGENTS.md](AGENTS.md)를 먼저 읽는다. 문서가 충돌하면 `CONTEXT.md`를 우선한다.

## 로컬 개발 환경

필수 조건:

- Linux host. KVM E2E는 `/dev/kvm`이 필요하다.
- root 또는 `sudo` 권한.
- `curl`, `debootstrap`, `e2fsprogs`, `util-linux`, `jq`, `dmsetup`.
- Go toolchain. 현재 MCP SDK와 프로젝트 검증은 Go 1.25 이상을 기준으로 한다.

기본 빌드:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

KVM E2E:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
```

`e2e_test.sh`는 실제 Firecracker VM을 부팅한다. `configs/goose-secrets.yaml` 또는
profile별 secret file에는 로컬 LLM API key가 필요할 수 있으며, 이 파일들은 절대
커밋하지 않는다.

## 빌드 산출물

daemon은 첫 실행 시 다음 runtime artifact를 준비한다.

- `artifacts/golden-image.ext4`
- `artifacts/vmlinux.bin`
- `artifacts/firecracker`
- `artifacts/goose-agent`
- `artifacts/micro-init`

`goose-agent`와 `micro-init`은 VM 내부에서 실행되는 Linux amd64 binary다. 수동으로
빌드할 때 host 기본값으로 빌드하지 않는다. daemon의 bootstrap helper가 필요한
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 설정을 적용한다.

ephemera `v0.3.1` 기준으로 daemon은 `goose-agent`, `micro-init`, golden image의
stale input을 감지해 필요한 경우 자동 재빌드한다. 그래도 runtime artifact는
gitignored 상태로 유지한다.

## 설정과 secret

커밋 가능한 예시:

- `configs/goose.yaml.example`
- `configs/goose-secrets.yaml.example`
- `configs/profiles/*/goose.yaml.example`
- `configs/profiles/*/goose-secrets.yaml.example`
- `configs/profiles/*/system.md`

커밋 금지:

- `configs/goose.yaml`
- `configs/goose-secrets.yaml`
- `configs/profiles/*/goose.yaml`
- `configs/profiles/*/goose-secrets.yaml`
- 실제 provider API key, Bearer token, 고객 데이터

채팅, issue, 문서, commit message에도 실제 key를 남기지 않는다.

## 보안 불변 조건

- `POST /vms` 응답 외에는 `agent_token`을 노출하지 않는다.
- daemon restore 응답, flock 응답, MCP output, runtime audit, metrics, trace,
  snapshot GC audit에는 `agent_token`이 없어야 한다.
- upstream ephemera `v0.3.1`의 `POST /flocks` `agent_tokens` 응답 추가는 anvil에서
  채택하지 않는다.
- upstream ephemera `v0.3.2`/`v0.3.3`을 sync할 때는 `vms/<vm_id>/state.json`의
  `agent_token` 보존과 `/root/.ephemera-cp-token` 주입이 문서, log, replay fixture,
  MCP output, audit/metrics/trace로 노출되지 않는지 별도 확인한다.
- Town Wall message body는 `flocks/<flock_id>/TOWN_WALL.log`와 history 응답에
  남으므로 secret 전달 채널로 쓰지 않는다.

관련 변경을 하면 최소한 다음 검색을 수행한다.

```bash
rg -n "agent_token|agent_tokens|Authorization|Bearer|secret" .
```

검색 결과는 정책상 허용된 위치인지 직접 확인한다.

## 테스트 기준

일반 변경:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
bash -n e2e_test.sh
bash -n scripts/anvil-mcp-e2e.sh
git diff --check
```

다음 경로를 건드리면 KVM E2E를 실행한다.

- `cmd/goose-daemon/`
- `cmd/goose-agent/`
- `cmd/micro-init/`
- `internal/network/`
- `internal/storage/`
- `internal/vm/`
- `internal/orchestrator/`
- `scripts/build_image.sh`
- `scripts/gtwall`

MCP adapter만 변경한 경우에도 daemon-backed smoke가 필요한 변경이면 다음을
확인한다.

```bash
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
```

## 주의가 필요한 영역

**Cold-restart state (v0.3.2)** — `vms/<vm_id>/state.json` is the source of truth for cold-restart. When you add fields to `runningVM`, decide whether they need to survive a daemon restart; if yes, persist them via `storage.VMState` and reconstruct them in `RecoverVMs` (`cmd/goose-daemon/recovery.go`). Don't rely on Firecracker `*Machine` invariants in recovery — the SDK does not re-attach to a running process, so recovery boots a fresh Firecracker with the same socket/TAP/IP/MAC. `EPHEMERA_DISK_MODE=cow` VMs reconstruct their dm-snapshot on recovery (re-layer the preserved exception store over the golden image, see `RecoverVMs` in `cmd/goose-daemon/recovery.go`); if you change the disk mode default, also update the recovery COW-reconstruction logic and the "Known Limitations" section of `docs/guides/security-and-resilience.md`.

**Graceful shutdown vs. `destroyVM` (v0.3.2)** — these are two distinct teardown paths and they intentionally behave differently. `destroyVM` is the explicit-DELETE path: it removes `state.json`, the rootfs ext4, and all transient files (full cleanup). `DestroyAll` is the deferred-on-SIGTERM path in `main`: it stops Firecracker and releases network resources but **preserves `state.json` + rootfs** so the next daemon start cold-restarts the VM. If you add cleanup code, decide which of these two paths it belongs to. A common mistake (caught in the v0.3.2 cycle) is to put cleanup inside `destroyVM` and call it from `DestroyAll` for the "shared" case — that drops the persisted state and silently breaks cold-restart.

**Flock metadata writes (v0.3.3)** — Always go through `Flock.Persist(workDir)` rather than calling `orchestrator.SaveFlockMetadata` directly. The helper holds a per-flock `writeMu` around the tmp+rename, which is the only thing keeping concurrent writers (createFlock, watchdog `onFailure`, recovery `markFlockAgentDead`, per-agent restart) from tearing each other's writes. The raw `SaveFlockMetadata` is kept as the persistence primitive used by `Flock.Persist` itself and by tests that have no live Flock; calling it from a daemon path silently dodges the serialization invariant.

**Per-agent restart token semantics (v0.3.3)** — `restartAgent` deliberately reuses the existing `agent_token` so caller-side caches keep working across a restart. The mechanism is `spawnVMOptions.AgentToken`: empty triggers fresh generation (the standalone spawn path), non-empty reuses verbatim (the restart path). Changing this contract is a breaking change — every caller that previously cached the token would start hitting 401s after restarts.

**In-VM control-plane token (v0.3.3, hot-rotated v0.3.4)** — The host injects `apiClients[0].Token` into each flock VM at `/root/.ephemera-cp-token` (mode 0600) via `spawnVMOptions.ControlPlaneToken`. Standalone `POST /vms` deliberately does NOT inject (non-flock VMs have no `/townwall/post` use case), so a confusing empty file does not litter every disk. v0.3.4 propagates rotations on SIGHUP via the `SET_CP_TOKEN` vsock command — only meaningful when the daemon reads tokens from `EPHEMERA_API_TOKENS_FILE` (env values cannot change without re-exec).

**CP token vsock fan-out (v0.3.4)** — `ReloadClients` snapshots `cp.vms` under `cp.mu.RLock()` (NOT `clientsMu`), then fans `vm.SetGuestCPToken` calls out as parallel goroutines. Per-VM failures MUST be logged, not propagated — a stuck VM cannot block the SIGHUP path. The 1 s per-VM budget × `len(vms)` parallelism caps total wall-clock. If you add new mutable VM-level state that needs to survive rotation, mirror this pattern: snapshot under `cp.mu.RLock()`, dispatch in parallel, swallow per-VM errors.

**Watchdog tunables + auto-heal (v0.3.4)** — `Watchdog.Configure` is the only public entry to override `interval` / `httpTimeout` / `dyingThreshold` / `autoHeal`; the underlying fields stay unexported so external callers cannot bypass the `interval >= httpTimeout` clamp. In-package tests still set fields directly — that's fine and intentional. `EPHEMERA_WATCHDOG_AUTO_HEAL` MUST default to off; `TestWatchdog_MarksDeadAfterThreshold` is the sticky-dead contract and changes that flip a once-dead agent back to ready without the opt-in flag are a regression by definition.

**Firecracker SDK signal forwarding (v0.3.4 hot-fix)** — `firecracker-go-sdk` v1.0.0's `firecracker.Config.ForwardSignals` defaults to `[SIGINT, SIGQUIT, SIGTERM, SIGHUP, SIGABRT]` and installs a goroutine (`setupSignals` in the SDK) that forwards each received signal to the Firecracker child via `cmd.Process.Signal(sig)`. The daemon uses `SIGHUP` for token hot reload, so the default would kill every running Firecracker the moment a reload signal arrives. `internal/vm/machine.go` defines a package-level `forwardSignals` slice that **deliberately omits `SIGHUP`** and passes it to both `StartMachine` and `RestoreMachine`'s `firecracker.Config`. If you add new SDK call sites (a third spawn path, a re-attach path, etc.), set `ForwardSignals: forwardSignals` there too — leaving it nil silently re-enables the bug. If we ever stop using `SIGHUP` for hot reload, the explicit list can shrink, but never grow back to include `SIGHUP` while reload semantics are tied to it.

**Metrics counter discipline (v0.3.5)** — `internal/metrics/` is a hand-written exposition formatter; the registry validates metric and label names against `[a-zA-Z_:][a-zA-Z0-9_:]*` at registration time and panics on a duplicate or malformed name. When adding a counter, keep label sets closed and small (`outcome=ok|fail`, or `ok|denied|expired` for `ephemera_auth_total` — never free text); when adding a `type` label, document the closed set in the metrics catalogue (`docs/guides/api-reference.md` → Metrics). The `orchestrator/` package intentionally does NOT import `internal/metrics/`; the daemon wires watchdog observations via the `OnDead` / `OnHeal` / `OnProbeDuration` callbacks set on `Watchdog`. Keeping that direction one-way prevents a package cycle when watchdog tests run with no daemon-level registry.

**slog message convention (v0.3.5)** — `cmd/goose-daemon/`, `internal/storage/`, `internal/orchestrator/`, and `internal/network/` use `log/slog` (not the standard `log` package). Messages are short, lowercase, present-tense phrases — `"vm spawn start"`, `"recovery: disk missing"`, `"snapshot: copying disk"`. Structured fields are snake_case (`vm_id`, `flock_id`, `agent_id`, `snapshot_id`, `err`). Don't smuggle dynamic data into the message; use a field. Lifecycle events (recovery, spawn, destroy, watchdog transitions) are emitted at `Warn` so the default `EPHEMERA_LOG_LEVEL=warn` still surfaces them. `cmd/goose-agent/` retains its original `log.Printf` output this cycle — touching the agent triggers a golden-image rebuild, which is deferred to v0.4.3.

**Per-VM stats PID resolution (v0.3.5)** — `firecracker-go-sdk` does not expose the Firecracker child's PID. `cmd/goose-daemon/stats_collector.go` resolves it by tracing `runningVM.socketPath` through `/proc/net/unix` (path → inode) and then `/proc/<pid>/fd` (inode → process), with a `comm == "firecracker"` prefilter to skip kernel threads. The resolved PID is cached on `runningVM.fcPID` (atomic). On every stats request, the cached PID is re-validated by reading `/proc/<pid>/comm`; a process death or PID reuse for a non-firecracker binary invalidates the cache and forces a fresh lookup. Do not skip this check — Linux PID reuse is rare but real, and a stale cache could read another process's `/proc/<pid>/stat` and report its CPU to the wrong VM.

KVM/Firecracker:
VM cold spawn, snapshot create, restore는 서로 다른 경로다. 하나를 고치면서 다른
경로의 cleanup을 깨뜨리기 쉽다. 실패 경로에서도 TAP/IP, dm-snapshot, loop device,
bind mount, sparse COW file을 정리해야 한다.

**In-VM helper JSON construction (v0.3.6)** — `scripts/gtwall` and `scripts/gtcall` build their request bodies with `jq -n --arg` (e.g. `jq -n --arg b "$1" '{body: $b}'`), never hand-rolled `sed` escaping. A `sed`-only escape (the pre-v0.3.6 gtwall, which escaped quotes and backslashes but not newlines) leaves raw newlines inside the JSON string, which the in-VM goose-agent rejects with HTTP 400 — and curl `-f` surfaces that as the misleading "exited with code 22". The bug was latent for the entire v0.3.x line because gtwall had only ever posted single-line status messages; the first multi-line body (a whole source file framed in `<<<FILE:>>>` sentinels) exposed it. Any new in-VM helper that posts JSON must use `jq --arg`. Both scripts live in the golden image (`build_image.sh` step 5b) and are in `EnsureGoldenImage`'s input list, so editing either triggers a rebuild.

**goose-agent task output parsing (v0.3.6)** — `cmd/goose-agent/main.go` runs `goose run --output-format json` and parses the envelope in `extractGooseJSONText`: it slices from the first `{` to the last `}` (Goose prints a startup banner to stdout even in JSON mode), unmarshals `messages[].content[]`, and concatenates every assistant `text` block — falling back to raw stdout if the envelope will not parse. This is the only working escape from goose-cli's `streaming_buffer.rs` 50-line code-block truncation (neither `--debug` nor `GOOSE_SHOW_FULL_OUTPUT` disables it). Reuse the banner-skip-slice idiom for any future feature that shells out to `goose run`. Touching `goose-agent` triggers a golden-image rebuild — batch it with other in-VM changes.

**Access audit log never records secrets (v0.4.1)** — `cmd/goose-daemon/audit.go`'s `auditRecord` carries only `{ts, client, method, path, status, duration_ms, remote_addr, bytes}`. It MUST never include the `Authorization` header, any token, or request/response bodies, and `path` is `r.URL.Path` only (the query string is excluded so a secret accidentally placed in a query param is not persisted). Any new field is a security-review item; `TestAuditRecord_NoSecretLeak` guards the marshaled output against `bearer`/`token`/`authorization` substrings.

**Audit-outer / auth-inner middleware ordering (v0.4.1)** — the mux wires `cp.auditMiddleware(authMiddleware(...))`: audit is the OUTER wrapper so it captures unauthorized (401) responses and the final status/bytes. The matched client name flows from the inner auth to the outer audit via a request-scoped `clientHolder` — a `r.WithContext` mutation inside auth is invisible to the outer wrapper, so the shared pointer bridges them. Do not reorder so audit sits inside auth (it would stop recording 401s). The `statusRecorder` wrapper MUST keep forwarding `http.Flusher`, or the SSE Town Wall stream (`streamTownWall`, which asserts `w.(http.Flusher)`) breaks.

**First-non-expired CP token + post-loop expiry check (v0.4.1)** — `controlPlaneTokenForVM` and `propagateCPTokenToVMs` select the first NON-EXPIRED client (via `firstActiveClient`), not blindly `clients[0]`, so an expired primary token does not break the in-VM `/townwall/post` forwarder; when every token has expired they propagate `""` (unauthenticated) and log a warning. Mirror this if you add a new consumer of the "primary" token. Token expiry is checked in `authMiddleware` AFTER the constant-time compare loop (never inside it) so it adds no timing signal, and an expired match returns the same 401 body as an unknown token.

**Autonomous orchestration prompts (v0.3.6)** — `webdev_demo.sh`'s orchestrator prompt (`configs/profiles/orchestrator/system.webdev.md`) must forcefully enforce loop completion: Gemini agentic loops in `goose run` stop after a few *successful* tool calls unless the prompt forbids mid-task summaries and makes the irreversible step (publish via `gtwall`) mandatory right after generation. Errors keep the loop alive (the model retries); successes invite a premature stop. Use `gemini-2.5-flash` (not `-lite`) for any role that must drive a multi-step tool-calling loop — `-lite` plans and then quits. This is a paid-tier-only choice (free tier shares a 20 RPM cap across all models).

Snapshot:
실행 중인 원본 VM의 snapshot은 restore하지 않는다. diff snapshot이 참조 중인 full
snapshot은 삭제하지 않는다.

Goosetown:
flock metadata persistence는 daemon restart 뒤 read-mostly registry와 Town Wall
history를 복구한다. 이전 daemon process와 함께 종료된 Firecracker VM은 자동
재시작하지 않는다. watchdog의 `dead` 상태는 live probe 결과다.

MCP:
`cmd/anvil-mcp`와 `internal/anvilmcp`는 얇은 stdio adapter다. VM lifecycle 의미는
ephemera daemon API가 소유한다. MCP tool이 host-local cleanup 의미를 재해석하지
않는다.

## PR 기준

- 한 PR에는 하나의 논리 변경만 담는다.
- 사용자/운영자 문서는 한국어로 작성한다. API 경로, env var, command, file path,
  code identifier는 원문을 유지한다.
- 로컬 secret과 runtime artifact를 포함하지 않는다.
- upstream ephemera sync는 merge commit으로 수행하고 rebase/history rewrite를 하지
  않는다.
- release note와 release checklist는 release prep 작업에서 함께 정리한다.

보안 이슈는 공개 issue 대신 GitHub Security Advisory 등 비공개 경로로 보고한다.
