# anvil 런타임 사용 가이드

> anvil/ephemera runtime의 실행 모델, 시작하기, 테스트, 설정, Operator CLI, Web UI 상세입니다.
> 프로젝트 개요·경계·빠른 시작은 [README](../../README.md)로 돌아가세요. API 전체 참조는
> [api-reference.md](api-reference.md), IronClaw MCP 어댑터는 [mcp-adapter.md](mcp-adapter.md),
> 보안/복원력은 [security-and-resilience.md](security-and-resilience.md)를 참고하세요.

## Runtime baseline (parity 상세)

아래는 README의 Runtime Baseline 요약 표를 뒷받침하는 anvil↔upstream ephemera parity 분류
서술입니다. 태그별 채택/적응/deferred/excluded 전체 분류는
[`../../CONTEXT.md`](../../CONTEXT.md)와 [`../PUBLIC_RELEASE_BOUNDARY.md`](../PUBLIC_RELEASE_BOUNDARY.md),
[`../analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`](../analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md)에 있습니다.

ephemera `v0.3.2`-`v0.7.0`는 anvil 안에서 runtime baseline으로 채택/적응된 변경이다.
`v0.4.4` flock broadcast는 daemon-only이고 `anvil_*` MCP tool로 노출하지 않으며,
`v0.4.5` snapshot-restore auto-recovery에서 anvil은 live·persisted restored VM이
참조하는 source snapshot의 `DELETE`를 `409`로 계속 막는다(upstream e2e 46c의 `200`
orphan과 다른 의도적 divergence). `v0.5.0` operator Web UI(`/ui/`)와 `/config/*`는
runtime/operator surface로만 채택하고 IronClaw `anvil_*` MCP surface로 노출하지 않으며,
`cmd/anvil-mcp`는 v0.5 sync에서 변경되지 않았다. `v0.5.x`가 드러낸 upstream pooled
agent-proxy 결함은 `64ec57c`가 request마다 fresh dial(`DisableKeepAlives`)로 고쳤다
(upstream connection pooling과의 divergence). `v0.6.0` runtime MCP Gateway
(`EPHEMERA_MCP_*`, `internal/mcpgateway`)도 runtime/operator surface로만 채택하며
IronClaw `anvil_*` MCP surface(`ANVIL_MCP_*` adapter)를 대체하지 않는다 — caller
profile은 source IP로 판정하고, backend credential은 host-side에만 두며(VM엔 gateway
URL만), audit은 metadata-only이고 profile policy는 서버 목록을 넓힐 수 없다.
`v0.7.0` end-user installer(`install.sh`/`uninstall.sh`/`ephemera.service.in`)와
conversation transcript restore도 runtime/operator surface로 채택하며, systemd service는
canonical `ephemera` 이름을 유지한다(anvil alias wrapper 없음). transcript는 daemon
proxy(bearer)로 노출하고 payload는 provider key/CP token/`agent_token` sentinel-free다.
anvil release note에서는 이 내용을 "upstream runtime baseline"으로 분리해 기록하고,
MCP/scheduler/workload/tenant/egress 같은 anvil 고유 기능과 섞어 제품명처럼 쓰지
않는다.
`v0.7.0`의 hardening(kernel SHA atomic 검증, `resolveWorkDir`/`EPHEMERA_HOME`,
`waitForAgent` per-probe)은 sync 전 독립 backport로 먼저 반영돼 있었고 v0.7.0 병합 시
single definition으로 reconcile됐다. 이로써 upstream parity scope(`v0.4.0`-`v0.7.0`)의
코드 편입이 완료됐다.

---

## anvil 실행 모델

```text
IronClaw
  - planner / orchestrator
  - MCP client
      |
      | stdio MCP tool call
      v
anvil MCP adapter
  - anvil_spawn_vm
  - anvil_run_task
  - anvil_create_snapshot
  - anvil_restore_snapshot
      |
      | optional scheduler-backed host selection
      v
anvil runtime scheduler
  - host inventory
  - quota and placement state
  - snapshot locality
      |
      | HTTP + optional Bearer token
      v
ephemera runtime boundary
  - control plane :3000
  - Firecracker MicroVM
  - goose-agent task runtime
```

IronClaw 관점에서 anvil은 다음 계약을 제공한다.

- VM 생성과 local `session_name` alias binding
- VM 내부 prompt/task 실행
- VM health, graceful stop, delete lifecycle
- full/diff snapshot 생성, 목록, restore, 삭제
- daemon token과 guest agent token을 분리하는 proxy 보안 경계
- restore 후 alias bind race를 명시적으로 노출하는 cleanup 계약

---

## ephemera runtime 분리

ephemera는 anvil이 사용하는 기반 실행 엔진이다. VM 생성, Firecracker machine
관리, TAP/IP 할당, rootfs 준비, snapshot file 관리, guest agent proxy, multi-agent
flock과 Town Wall log는 ephemera control plane이 소유한다. anvil MCP adapter는
이 의미를 재해석하지 않고 얇게 호출한다.

ephemera runtime의 현재 HTTP API 구조:

```text
외부 client 또는 anvil MCP adapter
      |
      | HTTPS, TLS 종료는 reverse proxy가 담당
      v
Reverse proxy :443
      |
      | HTTP + control-plane Bearer token
      v
ephemera control plane :3000
  GET    /health               -> daemon 상태
  GET    /metrics              -> Prometheus text metrics
  GET    /metrics/vms          -> 실행 중인 VM별 JSON metrics
  GET    /tenants              -> tenant quota/usage 목록
  GET    /tenants/{tenant_id}  -> tenant quota/usage 조회
  PUT    /tenants/{tenant_id}  -> tenant quota 설정
  GET    /audit/runtime        -> runtime audit 조회
  POST   /audit/runtime/prune  -> runtime audit 보관 정책 적용
  POST   /vms                  -> VM 생성
  GET    /vms                  -> 실행 중인 VM 목록
  DELETE /vms/{vm_id}          -> VM 종료 및 리소스 정리
  POST   /vms/{vm_id}/snapshot -> VM snapshot 생성
  GET    /snapshots            -> snapshot 목록
  POST   /snapshots/gc         -> snapshot GC dry-run/apply
  POST   /snapshots/{id}/export
                                -> snapshot bundle export
  POST   /snapshots/import     -> snapshot bundle import
  POST   /snapshots/{id}/restore
                                -> snapshot에서 VM 복원
  DELETE /snapshots/{id}       -> snapshot 삭제
  POST   /flocks               -> 역할별 VM flock 생성
  GET    /flocks               -> flock 목록
  GET    /flocks/{id}          -> flock과 agent 상태 조회
  DELETE /flocks/{id}          -> flock 소속 VM 병렬 삭제
  POST   /flocks/{id}/post     -> Town Wall message append
  GET    /flocks/{id}/wall     -> Town Wall SSE stream
  GET    /flocks/{id}/wall/history
                                -> active Town Wall history 조회
  POST   /flocks/{id}/distributed
                                -> home host에 hub flock(공유 Town Wall) 등록
  POST   /flocks/{id}/relay    -> member host에 relay flock 등록(post/wall proxy)
  POST   /flocks/{id}/call     -> 지정 agent에 프롬프트 호출(2-hop: local/hub 직접,
                                relay는 home으로 forward)

      |
      | Firecracker SDK, KVM, TAP, rootfs, snapshot files
      v
Firecracker MicroVM, ephemera runtime
  - Debian Trixie minbase rootfs
  - micro-init, PID 1
  - goose-agent :8080
  - goose CLI task 실행

외부 client
      |
      | control plane proxy
      v
POST /vms/{vm_id}/tasks  -> goose-agent :8080/tasks
GET  /vms/{vm_id}/workspace?path=...
PUT  /vms/{vm_id}/workspace?path=...
                         -> goose-agent :8080/workspace
GET  /vms/{vm_id}/health -> goose-agent :8080/health
POST /vms/{vm_id}/stop   -> goose-agent :8080/stop
```

`EPHEMERA_PUBLIC_URL`이 설정되어 있으면 ephemera `POST /vms` 응답의 `agent_url`은
control plane proxy 경로를 가리킨다. 설정되지 않은 경우 host에서 접근 가능한
VM private IP가 반환된다.

### VM 생성 흐름

```text
CloneDisk() 또는 CloneDiskCOW()
  -> 기본값은 golden image를 VM별 ext4 disk로 copy
  -> EPHEMERA_DISK_MODE=cow이면 dm-snapshot 기반 sparse COW disk 사용

PrepareVM()
  -> goose.yaml, goose-secrets.yaml, agent_token, /etc/localtime 주입
  -> flock member이면 /root/.ephemera-flock, /root/.goose-system-prompt 주입
  -> 단일 mount/unmount cycle 사용

StartMachine()
  -> Firecracker kernel + disk + TAP NIC 시작
  -> DHCP 없이 kernel ip= boot parameter로 네트워크 설정

waitForAgent()
  -> http://10.0.1.x:8080/health readiness poll
  -> cold boot 기준 약 60초
```

### Snapshot/Restore 흐름

```text
POST /vms/{id}/snapshot
  -> snapshot type 자동 선택
     - 해당 VM의 기존 Full 없음: Full
     - 기존 Full 있음: Diff
  -> PauseVM()
  -> CreateSnapshot(memory.bin, state.bin)
  -> rootfs.ext4 copy
  -> ResumeVM() 또는 stop_after=true이면 source VM 삭제

POST /snapshots/{id}/restore
  -> Diff이면 base memory + diff memory merge
  -> SetupDMSnapshot()으로 COW rootfs 구성
  -> original TAP name/MAC 재생성, 새 IP 할당
  -> Firecracker RestoreMachine()
  -> vsock CHANGE_IP로 guest IP 재설정
  -> /health readiness poll
```

Firecracker snapshot state에는 TAP device name과 disk path가 들어 있다.
ephemera는 restore 시 해당 device identity를 재생성하고, IP는 vsock channel을
통해 새 값으로 재설정한다.

### 종료 흐름

```text
DELETE /vms/{id}
  -> StopVMM()
  -> micro-init이 SIGTERM 수신
  -> goose-agent 종료 요청
  -> sync + poweroff(2)
  -> COW restore VM이면 dm-snapshot/loop/bind mount/.cow 정리
  -> 일반 VM이면 cloned ext4 disk 삭제
  -> TAP/IP 반환
```

---

## ephemera runtime 기능

- **자체 bootstrap**:
  ephemera 첫 실행 시 golden image, kernel, Firecracker binary를 준비하고 검증한다.

- **최소 guest OS**:
  Debian Trixie minbase와 Go 기반 `micro-init`으로 구성한다.

- **안전한 guest 종료**:
  `micro-init`이 signal을 받아 `poweroff(2)`를 호출해 kernel panic을 피한다.

- **VM별 LLM profile**:
  VM 생성 시 `configs/profiles/{name}/`의 provider/model/secret을 선택할 수 있다.

- **런타임 설정 주입**:
  `goose.yaml`, `goose-secrets.yaml`을 provision time에 주입한다.

- **VM별 agent 인증**:
  VM마다 별도 Bearer token을 생성하고 guest disk에 `0600`으로 저장한다.

- **Full/Diff snapshot**:
  첫 snapshot은 Full, 이후 snapshot은 dirty memory page 기반 Diff로 자동 선택된다.

- **COW rootfs restore**:
  restore VM은 snapshot rootfs를 read-only base로 공유하고 sparse COW file에
  쓰기를 기록한다.

- **Restore 후 IP 재설정**:
  VM은 새 IP를 할당받고 vsock으로 guest network stack을 갱신한다.

- **IP/TAP 재사용**:
  lifecycle 종료 후 `10.0.1.2-254` IP와 TAP ID를 pool에 반환한다.

- **Outbound NAT**:
  `goose-br0`와 iptables MASQUERADE로 guest의 LLM API outbound를 지원한다.

- **Control-plane 인증**:
  named Bearer token, timing-safe compare, audit log, `SIGHUP` hot reload를 지원한다.

- **Multi-agent flock**:
  `POST /flocks`가 역할별 VM 여러 개를 생성하고 하나의 flock ID 아래에서
  관리한다. blank `task`, empty role, path separator가 포함된 role은 VM spawn
  전에 거부하고, 생성 중 일부 VM이 실패하면 이미 생성된 VM과 flock registry를
  정리한다.

- **Town Wall**:
  flock별 append-only coordination log를 제공한다. control plane API,
  SSE stream, VM 내부 `gtwall` CLI로 같은 log에 message를 남길 수 있다.

- **Flock health watchdog**:
  daemon이 flock member VM health를 주기적으로 확인한다. 연속 실패 임계값을
  넘으면 agent status를 `dead`로 표시하고 Town Wall에 notice를 남긴다.

- **Flock metadata persistence**:
  `flocks/<flock_id>/metadata.json`을 기록하고 daemon restart 뒤 flock registry와
  Town Wall log를 복구한다. 현재 baseline에서 spawn-path member VM은
  `vms/<vm_id>/state.json` 기반으로 cold-restart되고, snapshot-restored member VM은
  `v0.4.5` 이후 source snapshot에서 re-restore로 auto-recovery된다. 두 경우 모두
  memory state와 in-flight task는 보존되지 않는다.

- **Town Wall sequence**:
  Town Wall message는 per-flock monotonic `seq`를 포함해 subscriber가 gap을
  감지하고 active history로 복구할 수 있다. size rotation 이후 API history는
  rotated backup을 스캔하지 않는다.

- **역할별 resource profile**:
  `researcher`, `reviewer`, `worker`, `orchestrator`, `builder` 역할은
  `LookupProfile`을 통해 vCPU/memory와 profile directory를 결정한다.

- **역할 system prompt 주입**:
  `configs/profiles/{role}/system.md`를 VM 내부 `/root/.goose-system-prompt`로
  주입하고, `goose-agent`가 task prompt 앞에 system instruction으로 붙인다.

- **선택적 COW spawn disk**:
  `EPHEMERA_DISK_MODE=cow`이면 새 VM 생성도 dm-snapshot 기반 sparse COW disk를
  사용한다. unset 상태에서는 기존 full clone 동작을 유지한다.

- **Runtime artifact stale detection**:
  `goose-agent`, `micro-init`, golden image는 source/build input이 갱신되면
  필요한 경우 자동으로 재빌드된다.

Upstream ephemera feature matrix:

| Feature | Detail |
|---------|--------|
| **Self-bootstrapping** | Golden image, kernel, Firecracker downloaded + SHA256-verified on first run; goose-agent / micro-init / golden image are also rebuilt automatically when their sources are newer than the cached artifact (mtime-based staleness check), so editing in-VM Go code or `build_image.sh` does not need a manual `rm artifacts/...` |
| **Minimal guest OS** | Debian Trixie minbase — no SSH, no init daemon; `micro-init` (Go binary, PID 1) mounts virtual filesystems and manages goose-agent lifecycle |
| **Graceful guest shutdown** | `micro-init` traps SIGTERM and calls `poweroff(2)` — no kernel panic on VM exit |
| **Per-VM LLM profiles** | Each VM spawn can specify a named profile (`configs/profiles/{name}/`) with its own provider, model, and API key |
| **Per-profile vCPU/memory** | Known roles (`researcher`, `worker`, `reviewer`, `orchestrator`, `builder`) map to canonical sizing (e.g. 1 vCPU / 512 MiB for researcher, 4 vCPU / 4096 MiB for builder); unknown profiles fall back to the legacy 2 vCPU / 2048 MiB default |
| **Multi-agent flocks** | `POST /flocks` spawns a group of role-specialized VMs in one call; `DELETE /flocks/{id}` tears them all down in parallel |
| **Town Wall log** | Per-flock append-only log with SSE streaming (`/flocks/{id}/wall`) for coordination; `gtwall "..."` CLI inside each VM posts to it, and `gtcall <agent_id> "..."` (v0.3.6) dispatches a prompt to a peer agent — both hide curl/token/JSON-quoting behind a one-line interface |
| **Role system prompts** | Each role profile can ship a `system.md` that is injected into the VM and prepended to every `/tasks` prompt |
| **Optional COW spawn rootfs** | `EPHEMERA_DISK_MODE=cow` provisions new VMs with a dm-snapshot view of the golden image instead of a 700 MiB full copy (~0 MiB initial disk). The daemon probes dm-snapshot support when COW is explicitly selected and auto-falls back to a full clone if unavailable. Auto-recovered across a daemon restart since v0.4.0. Default remains plain/full clone in anvil. |
| **Runtime config injection** | `goose.yaml` and `goose-secrets.yaml` injected at provision time — no image rebuild required to change provider/model |
| **Per-VM agent authentication** | Control plane generates a 32-byte random Bearer token per VM; token is written to the VM disk and returned once in `POST /vms` response |
| **MicroVM snapshots (Full + Diff)** | Freeze VM memory state to disk; restore in ~5 s. First snapshot → Full (2 GB); subsequent snapshots of the same VM → Diff (sparse, dirty pages only). Diff is automatically selected; Full is always the reference base. Original agent token preserved across restores. |
| **COW rootfs on restore** | Restored VMs use a Linux dm-snapshot COW device backed by the snapshot's `rootfs.ext4` (read-only base, shared). Per-VM guest writes accumulate in a sparse exception store (~0 initial disk usage). Eliminates the ~700 MB full copy previously required per restore. |
| **Post-restore IP reconfiguration** | Restored VMs receive a fresh IP from the pool via vsock — the guest's network stack is updated in-place without reboot, decoupling the restore IP from the snapshot state. |
| **Restored VM auto-recovery** (v0.4.5) | dm-snapshot restored VMs now persist a `state.json` (with `source_snapshot_id`) and are **auto-recovered** across a daemon restart — re-restored from their source snapshot (back to snapshot-time memory+disk; post-restore writes are not preserved, same as a manual re-restore). Spawn-path VMs were already cold-restarted since v0.4.0. Caveats: bind-mount-fallback restores (dm-snapshot tooling absent) and restored VMs whose source snapshot was deleted are not recovered (the latter is surfaced as dropped, not silently kept). |
| **IP and TAP recycling** | IPs (10.0.1.2–254) and TAP IDs are returned to a pool and reused across VM lifecycle |
| **NAT for outbound internet** | Host bridge `goose-br0` with iptables MASQUERADE enables VM-to-internet for LLM API calls |
| **Per-client API auth** | Named Bearer tokens per client (`alice:tok1,bob:tok2`); timing-safe comparison; optional per-token TTL (`name:token:expires`, v0.4.1); the matched client identity is threaded into request context for the audit log |
| **SIGHUP token hot reload** | API token list can be updated without restarting the daemon or interrupting running VMs |
| **VM health watchdog** (v0.3.1) | Polls every flock-member `/health` every 5 s; 3 consecutive failures → agent `status=dead` + auto Town Wall notice. See [Resilience](security-and-resilience.md#resilience). |
| **Flock metadata persistence** (v0.3.1) | `flocks/<id>/metadata.json` written atomically on spawn; daemon startup re-registers every flock and reopens its Town Wall log. |
| **Monotonic Town Wall seq** (v0.3.1) | Every `Message` carries `seq` (uint64, 1-based per flock); subscribers can detect dropped messages and recover active-log entries from `/wall/history`. |
| **Fatal-on-bind daemon startup** (v0.3.1) | Daemon `log.Fatalf` if the API listener fails to bind (e.g. port already in use), so a stale process never silently masks a fresh one. |
| **Live VM cold-restart** (v0.3.2) | `vms/<vm_id>/state.json` written on every spawn; daemon startup cleans orphan Firecracker processes, re-reserves the original TAP/IP/MAC, and boots each VM from its existing rootfs clone. Same `vm_id`, same agent token, same `agent_url` across the restart. Memory state is not preserved. See [Resilience](security-and-resilience.md#resilience). |
| **Watchdog dead-status persistence** (v0.3.3) | When the watchdog marks an agent `dead`, the new status is written to `flocks/<id>/metadata.json` (via `Flock.Persist`, serialized by a per-flock `writeMu`). Daemon restart and cold-restart both preserve the marking, so a once-dead agent stays dead until explicitly restarted. |
| **Per-agent restart** (v0.3.3) | `POST /flocks/{id}/agents/{agent_id}/restart` tears down one flock member's VM and respawns it with the same `agent_id`, role, and `agent_token` (callers' cached tokens keep working). The new VM gets a fresh `vm_id` / `guest_ip`; the agent's status resets to `ready`. |
| **Dynamic flock membership** (v0.4.3) | `POST /flocks/{id}/agents` adds an agent (per-role `role-N` id; anvil omits guest token fields from the response; 20-agent cap); `DELETE /flocks/{id}/agents/{agent_id}` removes one (empty flock allowed); `PATCH …/agents/{agent_id}` changes role by recreating the VM under the new sizing/prompt (`agent_id` + token preserved internally). CLI: `ephemera-ctl flock add-agent`/`rm-agent`/`set-role`. |
| **Flock pause/resume + max_agents** (v0.4.3) | `POST /flocks/{id}/pause` · `/resume` pause/resume **all** member VMs via Firecracker (runtime-only — not persisted; the watchdog skips dead-marking paused agents). `POST /flocks` accepts `max_agents` for a per-flock cap (default 20), enforced on create and add. CLI: `ephemera-ctl flock pause`/`resume`, `create --max-agents`. |
| **Town Wall query + rotation** (v0.4.3) | `GET /flocks/{id}/wall/history` filters: `?agent_id=` / `since=` / `until=` (RFC3339) / `contains=`. The log rotates by size (`EPHEMERA_TOWNWALL_MAX_MIB` default 10 MiB, `_KEEP` default 3); history reflects the active file (rotated backups kept on disk). |
| **Flock broadcast** (v0.4.4) | `POST /flocks/{id}/broadcast` `{"body":"…"}` scatters one prompt to **every** member agent's `/tasks` in parallel and gathers each result (`sent`/`skipped`/`failed` tally + per-agent `results`); busy agents are reported `busy` (skipped). The broadcast is also recorded on the Town Wall. CLI: `ephemera-ctl flock broadcast <flock_id> <message>`. |
| **Watchdog status** (v0.4.4) | `GET /watchdog/status` returns the health watchdog's tunables (`interval_sec`/`timeout_sec`/`dying_threshold`/`auto_heal`) and live per-VM state (`vm_fail_counts`, `vm_dead_marked`). Read-only; behind the same auth as the other internal routes. |
| **Streaming `/tasks`** (v0.4.4) | `POST /vms/{id}/tasks?stream=1` streams newline-delimited JSON — `{"type":"progress",…}` frames (goose stderr activity + 15s heartbeat) then one `{"type":"result","output":…,"error":…}` frame. The proxy flushes per chunk. The default (no `stream=1`) path is unchanged. Streaming commits `200` up front, so goose errors arrive in `result.error`, not the status code. |
| **Nested-task depth guard** (v0.4.4) | Agent→agent `gtcall` is loop-guarded: the proxy reads `X-Ephemera-Task-Depth` on each `/tasks` hop, refuses at/over `EPHEMERA_MAX_TASK_DEPTH` (default 5) with `508 Loop Detected`, and forwards `depth+1`. `goose-agent` passes the depth to the goose subprocess (`EPHEMERA_TASK_DEPTH`) and `gtcall` re-sends it, so depth accumulates across the call tree. |
| **Auto-injected control-plane token** (v0.3.3) | When `EPHEMERA_API_TOKENS` is set, the host writes the first non-expired client's token (`apiClients[0]` until v0.4.1's per-token TTL) into each flock VM at `/root/.ephemera-cp-token` (mode 0600); the in-VM `/townwall/post` forwarder reads it automatically. No more manual `EPHEMERA_CONTROL_PLANE_TOKEN` env inside every VM. |
| **CP token hot rotation** (v0.3.4) | `EPHEMERA_API_TOKENS_FILE=/path/to/tokens` enables true hot rotation: edit the file, send SIGHUP, and the daemon both swaps `cp.clients` and fans the new token out to every running VM over vsock (`SET_CP_TOKEN` command, atomic file rewrite inside the guest). No per-VM restart needed for the in-VM forwarder to pick up the new bearer. |
| **Env-tunable watchdog** (v0.3.4) | `EPHEMERA_WATCHDOG_INTERVAL_SEC` / `_TIMEOUT_SEC` / `_THRESHOLD` override the 5 s / 1 s / 3-fail defaults at startup. `EPHEMERA_WATCHDOG_AUTO_HEAL=true` opts in to self-healing — a `dead` agent that resumes responding is auto-marked `ready` (default off preserves sticky-dead). |
| **Observability trio** (v0.3.5) | Prometheus `/metrics` endpoint (zero-dep exposition format, counters + gauges + histograms), per-VM `GET /vms/{vm_id}/stats` snapshot (cpu/mem/net/uptime/agent_busy), and a `log/slog` migration with `EPHEMERA_LOG_FORMAT=json` + `EPHEMERA_LOG_LEVEL=...` controls. See [Observability](security-and-resilience.md#observability-v035). |
| **Autonomous multi-agent demo** (v0.3.6) | `webdev_demo.sh` stands up an orchestrator + worker + reviewer flock that designs, generates, reviews, and publishes a complete React + Vite site to the Town Wall with zero host authorship. See [Multi-Agent Webdev Demo](demos.md#multi-agent-webdev-demo-v036). |
| **In-VM agent-to-agent dispatch** (v0.3.6) | `gtcall <agent_id> "<prompt>"` sends a task to a peer through the control-plane proxy, which injects the peer's token. Both `gtcall` and `gtwall` build their request bodies with `jq --arg`, so arbitrary multi-line prompts and file bodies (newlines, quotes, backticks) post safely. |
| **Clean agent task output** (v0.3.6) | goose-agent runs Goose with `--output-format json` and returns the extracted assistant text, so `/tasks` output is no longer interleaved with the startup banner or truncated to an in-VM temp file when fenced code exceeds 50 lines. |
| **Access audit log** (v0.4.1) | Every API request is appended as one JSON line to `{workDir}/audit/access.jsonl` (`ts, client, method, path, status, duration_ms, remote_addr, bytes` — never tokens or bodies), size-rotated (`EPHEMERA_AUDIT_MAX_MIB`/`_KEEP`), queryable via authenticated `GET /audit`. On by default; `EPHEMERA_AUDIT_DISABLE=true` to disable. See [Access audit log](security-and-resilience.md#access-audit-log-v041). |
| **Per-token TTL & rotation** (v0.4.1) | Token entries accept an optional expiry — `name:token:expires` (RFC3339 or Unix seconds); a matched-but-expired token is rejected `401` (`ephemera_auth_total{outcome="expired"}`). The in-VM control-plane token is the first **non-expired** client. Two-field `name:token` never expires (backward compatible). |
| **Operator CLI `ephemera-ctl`** (v0.4.1) | Dependency-free stdlib CLI wrapping the REST API (`vm`/`flock`/`snapshot`/`audit`/`metrics` verbs; human tables or `--json`). Reads `EPHEMERA_CTL_URL` + `--token`/`EPHEMERA_CTL_TOKEN`/`EPHEMERA_API_TOKEN`. See [Operator CLI](#operator-cli-ephemera-ctl-v041). |
| **Web console** (v0.5.0) | A browser console the daemon serves at `/ui/` (single binary via `go:embed`, same origin as the API — no CORS): token login (auto-skipped when auth is disabled), VM list with live stats + model, Create VM (profile dropdown), VM detail with live stats and a **multi-turn conversation** panel (cancelable streaming), per-profile model/provider **Settings**, and VM delete. Svelte + Vite SPA; the build is committed (`cmd/goose-daemon/uidist/`) so `go build` needs no Node. See [Web UI](#web-ui-v050). |
| **English / Korean UI** (v0.5.0) | The Web console ships full EN/KO localization (`svelte-i18n`); the initial language follows the browser (`ko*` → Korean, else English) and a nav toggle switches + persists the choice. UI vocabulary is generic IT (display only): *Platform Agent* (in-VM goose agent), *Agent Group* (flock), *Activity Feed* (Town Wall), *Create/Delete* (spawn/destroy). |
| **Profile/model editing** (v0.5.0) | `GET /config/profiles` lists each profile's provider/model; `PUT /config/profiles/{name}` rewrites `GOOSE_PROVIDER`/`GOOSE_MODEL` in place (comments + `extensions:` preserved; API keys are never read or written here). The Settings screen drives these; an edit applies to the **next** VM (config is injected at spawn), and each VM records the provider/model it was spawned with (`VMInfo.model`). |
| **Multi-turn conversation** (v0.5.0) | `POST /vms/{id}/tasks` accepts an optional `session`; with it, `goose-agent` runs goose as `-n <session> [--resume]`, so consecutive turns continue one goose chat session (context preserved). Omitting `session` keeps the original stateless one-shot behavior (`ephemera-ctl`, `gtcall`). |
| **Graceful VM delete** (v0.5.0) | `DELETE /vms/{id}` first asks the in-VM agent to shut down cleanly (best-effort `POST /stop`, 2 s) before force-stopping Firecracker, then frees TAP/IP/disk and deregisters. The old "stop agent" action — which actually halted the whole guest while leaving the VM registered — was removed; Delete is the single teardown. |
| **Profile config API** (v0.5.1) | `GET /config/providers` reports per-provider API-key **availability** only (never the key value); the Web UI adds snapshot/restore and per-profile sizing screens. Config data APIs sit behind bearer auth when configured. |
| **Orchestration UI + Activity Feed** (v0.5.2) | The Web console gains a flock (Agent Group) orchestration view, an Activity Feed (Town Wall) panel, and an operator-only single-agent Send-task action. |
| **Sizing presets + per-VM sizing** (v0.5.3) | Profiles carry a sizing **preset**; `POST /vms` honors per-VM `EPHEMERA_VCPU_COUNT` / `EPHEMERA_MEM_SIZE_MIB` (struct `VcpuCount`/`MemSizeMib`), and snapshot metadata records the VM's sizing (legacy snapshots fall back to 2 vCPU / 2048 MiB). The upstream **default drops to 1 vCPU / 1024 MiB** (was 2/2048). *anvil adopts the 1/1024 default (KVM-verified, full e2e 3× 316✓).* Flock members still size from `LookupProfile` defaults and do **not** yet honor per-profile sizing overrides — known follow-up. |
| **Profile guards** (v0.5.3) | Deleting a profile a running VM was spawned from is refused `409`; the `default` profile is reserved; path traversal in a profile name is rejected. |
| **System prompt editor** (v0.5.4) | `GET/PUT/DELETE /config/profiles/{name}` edit a profile's `system.md` only (64 KiB cap); the Web UI adds a system-prompt editor and an operator-only feed. |
| **System & Monitoring console** (v0.5.5) | The Web console adds System/Monitoring endpoints plus an `API_TOKEN` warning; `GET /config/clients` lists control-plane client **names + expiry** only (never the token). Town Wall author migrated to `SystemAuthor`; snapshot-restore agent-wait raised 30 s → 60 s. |
| **Per-profile builtin extensions** (v0.6.0) | Each profile selects which goose builtin extensions its agents load (`EPHEMERA_BUILTINS` in the profile `goose.yaml`; registry at `GET /config/builtins`; `GET/PUT /config/profiles/{name}/builtins`; Settings checkbox group + Extensions editor). Replaces the old hardcoded `--with-builtin developer`; absent → `developer` fallback (existing profiles unchanged). Ships in the same rebake as the MCP extension. |
| **MCP Gateway** (v0.6.0, opt-in, runtime/operator surface) | `EPHEMERA_MCP_ENABLED=1` starts a host-resident MCP server on the bridge IP (`10.0.1.1:3001`) that the in-VM goose clients connect to, aggregating backend MCP servers (`configs/mcp/servers.yaml`) behind one namespaced, per-profile-filtered tool catalog. Backend credentials (`configs/mcp/secrets.yaml`) stay host-side; VMs get only an injected endpoint URL, added via `--with-streamable-http-extension`. Caller identity is by source IP → profile. `GET /config/mcp` + `GET /config/mcp/servers` (live health) back the **System › MCP Gateway** tab; calls are metered (`ephemera_mcp_tool_calls_total`) and audited to `audit/mcp.jsonl` (metadata only). Built on interfaces so the multi-host build re-implements them without touching the protocol core. This gateway is the runtime tool surface **for in-VM agents**; it is separate from and does **not** replace `cmd/anvil-mcp`, which remains anvil's only IronClaw-facing MCP adapter. |
| **MCP Gateway anti-spoof + rate limit** (v0.6.1) | `EPHEMERA_NET_ANTISPOOF` (default on) adds best-effort ebtables MAC/IP anti-spoof so a guest cannot forge another VM's source IP — the gateway's identity signal. Per-(VM, backend server) token-bucket rate limiting via `EPHEMERA_MCP_RATE` (default `0` = unlimited) / `EPHEMERA_MCP_BURST`. |
| **MCP catalog + granular policy** (v0.6.2) | The gateway aggregates backend **resources** and **prompts** alongside tools; a profile's policy narrows access per-server and per-tool and can only **narrow**, never widen `servers.yaml`. Resources/prompts share the same policy filter and rate-limit bucket as tools (anvil guard). Audit records gain a `kind` field. |
| **MCP stdio backends** (v0.6.4) | Backends may be spawned as local subprocesses (`transport: stdio`). The child env is rebuilt from scratch (`PATH`/`HOME`/`LANG` + the server's `spec.Env`) so daemon `EPHEMERA_*` vars never leak in (canary test); the credential is passed only through `credential_env` (never argv). When the daemon runs as root the child drops to `EPHEMERA_MCP_STDIO_USER` (default `nobody`) with a `/var/lib/ephemera/mcp-stdio` scratch cwd/HOME, and shutdown reaps the child's process group (pgid-recycling-safe). `GET /config/mcp/servers` exposes transport/command + `has_credential` only (leak guard). *(Upstream has no v0.6.3.)* |
| **End-user installer** (v0.7.0, runtime/operator surface) | `install.sh` / `uninstall.sh` / `INSTALL.md` install the daemon as a systemd service (`ephemera.service.in` → canonical `ephemera` unit, `EPHEMERA_HOME=@DEST@`), and `scripts/build_release.sh` builds SLIM/FULL release tarballs. This is a **runtime/operator** installer for the ephemera daemon — not an anvil/IronClaw product wrapper; the service keeps the canonical `ephemera` name (no anvil alias). `build_release.sh` re-verifies the downloaded kernel/firecracker with `sha256sum -c` against the pins parsed from `main.go`, closing the FULL-tarball supply-chain gap (runtime `EnsureKernel` `os.Stat`-skips existing files). `uninstall.sh` cleans ephemera-scoped `/tmp` scratch (root-gated, prefix-anchored). Expose the daemon only behind a reverse proxy/TLS or a private network. |
| **Conversation transcript restore** (v0.7.0) | `GET /vms/{id}/sessions/{name}/transcript` (bearer, via the daemon proxy) returns a prior conversation as `{turns:[{role,text}]}`. The agent serves a cached transcript and, on a cache miss, fills it with a **read-only** `goose session export -n {name} --format json` — no model call. The response schema is auth-free so the Web UI can render it without a daemon token. Four safety guards lock the invariants: the endpoint `401`s without a bearer; the payload is sentinel-free of the provider key / CP token / `agent_token`; a cache hit serves without spawning the agent; and the export argv is exactly `session export -n {name} --format json` (run-token rejected). |

---

## 프로젝트 구조

```text
cmd/
  goose-daemon/       ephemera control plane daemon
    main.go           startup, artifact bootstrap, ControlPlane init
    api.go            VM/snapshot API, auth middleware, proxy
    config.go         환경 변수 기반 설정
    orchestrator_api.go
                      flock/Town Wall control-plane API
  anvil-mcp/          anvil/IronClaw용 stdio MCP adapter entrypoint
  anvil-scheduler/    runtime host/quota/placement scheduler service
  e2e-replay-server/  browser 기반 E2E terminal replay player
  goose-agent/        VM 내부 HTTP agent
  micro-init/         VM 내부 PID 1

internal/
  anvilmcp/           MCP config, daemon client, session alias, scheduler/router,
                      quota, placement, runtime audit helper
  orchestrator/       flock registry, agent 상태, Town Wall append-only log
  vm/machine.go       Firecracker SDK wrapper
  network/manager.go  IP pool, TAP lifecycle, bridge, NAT
  storage/
    provisioner.go    golden image bootstrap, disk clone, config/token injection
    snapshot.go       snapshot metadata, COW restore, diff memory merge

configs/
  anvil-mcp.yaml.example
  goose.yaml.example
  goose-secrets.yaml.example
  profiles/<profile-name>/
    goose.yaml.example
    goose-secrets.yaml.example
    system.md
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
    config_api.go     GET /config/profiles, GET/PUT /config/profiles/{name} —
                      read/update a profile's GOOSE_PROVIDER/GOOSE_MODEL on disk (v0.5.0)
    uidist/           Committed Web UI build (go:embed input; rebuilt from web/, v0.5.0)
  goose-agent/        In-VM HTTP agent (baked into golden image)
    main.go           /tasks (optional `session` → goose -n/--resume for multi-turn, v0.5.0),
                      /health, /stop, /townwall/post  (Bearer token auth);
                      prepends role system prompt to /tasks bodies;
                      runs `goose run --output-format json` and extracts the
                      assistant text via extractGooseJSONText (banner-skip) (v0.3.6)
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
                      (VcpuCount / MemSizeMib are per-call; zero falls back to 2 / 2048)
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
    orphan.go         KillStaleFirecracker + RemoveStaleVMArtifacts (cold-restart cleanup)
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
  profiles/                      Per-VM LLM profiles (optional)
    <profile-name>/
      goose.yaml                 (gitignored; copied from .example)
      goose-secrets.yaml         (gitignored; copied from .example)
      system.md                  Role system prompt prepended to /tasks (optional)
      system.webdev.md           webdev_demo.sh override prompt, swapped over system.md at demo time (v0.3.6)
      goose.webdev.yaml          webdev_demo.sh override config (Gemini model per role), swapped over goose.yaml (v0.3.6)
    researcher/  worker/  reviewer/  orchestrator/    ← built-in role profiles
  webdev-demo/                   Host-side vite-template overlaid onto worker output by webdev_demo.sh (v0.3.6)
    vite-template/               package.json, vite.config.js, index.html, src/* placeholders
  observability/                 Provisioning bundle for observability_demo.sh (v0.3.5)
    prometheus.yml               Prometheus scrape config (localhost:3000, 5s)
    grafana-datasource.yml       Prometheus datasource provisioning
    grafana-dashboards.yml       Grafana dashboards-provider provisioning
    dashboards/
      ephemera-overview.json     Pre-built Grafana 10.x dashboard (8 panels)

docs/
  PUBLIC_RELEASE_BOUNDARY.md
                       anvil 공개 포함/조건부 포함/제외 표면
  ADR_INDEX.md         ADR 현재 적용 상태와 upstream ephemera 채택 상태
  adr/                 공개 경계, token/auth, runtime lifecycle 장기 결정
  architecture/        ephemera 런타임, 서비스 로직, anvil MCP 아키텍처
  analysis/            ephemera 버전 비교와 소스 분석
  lifecycle/runs/      계산된 lifecycle 상태 snapshot
  operations/          보안 정책, runbook, DR, 관측성, release/operate 기록
  replays/             browser replay player용 sanitized E2E recording
  superpowers/         승인된 spec, review, plan 기록

snapshots/             snapshot 저장 디렉터리, gitignore
artifacts/             runtime artifact 디렉터리, gitignore
e2e_test.sh            58단계 통합 테스트
scripts/build_image.sh golden image build script
scripts/anvil-mcp-e2e.sh daemon 기반 MCP smoke wrapper
scripts/gtwall         VM 내부 Town Wall post helper
snapshots/            Stored snapshot directories (auto-created, gitignored)
  <snapshot-id>/
    memory.bin        Guest RAM dump — 2 GB (Full) or sparse/small (Diff)
    state.bin         Firecracker hardware state
    rootfs.ext4       Disk copy (always full, ~700 MB)
    metadata.json     Restore params (IP, TAP, MAC, token, type, base_snapshot_id)

e2e_test.sh           End-to-end integration test (89 numbered steps incl. resilience + v0.3.x–v0.7.0 sub-steps; requires /dev/kvm + root)
observability_demo.sh One-shot live demo: daemon + Prometheus + Grafana, auto workload, browser-driven exploration until Ctrl-C (v0.3.5)
webdev_demo.sh        One-shot live demo: orchestrator+worker+reviewer flock builds a React+Vite site, harvested from the Town Wall and served via vite preview until Ctrl-C (v0.3.6; manual gate, needs a Gemini key + /dev/kvm)

scripts/
  build_image.sh      Builds golden image (Debian Trixie + curl + jq + Goose + goose-agent + micro-init + gtwall + gtcall)
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

## 사전 요구사항

| 항목 | 내용 |
|---|---|
| Host OS | Ubuntu 22.04 또는 24.04 권장 |
| CPU | `/dev/kvm` 접근 가능 |
| Go | 1.25 이상 |
| Package | `curl`, `debootstrap`, `e2fsprogs`, `util-linux` |
| 권한 | 실행 시 `sudo` 필요. KVM, bridge, TAP, iptables를 설정한다. |

Upstream ephemera baseline requirements:

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

Firecracker, Linux kernel, golden image는 첫 실행 시 자동으로 다운로드하거나
빌드한다.

---

## 시작하기

### 1. 복제와 빌드

```bash
git clone https://github.com/HardcoreMonk/anvil.git
cd anvil
go build -o anvil-daemon ./cmd/goose-daemon/
go build -o anvil-mcp ./cmd/anvil-mcp
go build -o anvil-scheduler ./cmd/anvil-scheduler
go build -o ephemera-ctl ./cmd/ephemera-ctl/   # upstream/runtime operator CLI (v0.4.1)
```

`cmd/anvil-mcp`는 공식 MCP Go SDK를 사용하므로 Go 1.25 이상이 필요하다.

### 2. 기본 LLM 설정

```bash
cp configs/goose.yaml.example configs/goose.yaml
cp configs/goose-secrets.yaml.example configs/goose-secrets.yaml
```

`configs/goose.yaml` 예시:

```yaml
GOOSE_PROVIDER: google
GOOSE_MODEL: gemini-2.5-flash
GOOSE_TELEMETRY_ENABLED: false
GOOSE_DISABLE_KEYRING: true
```

`configs/goose-secrets.yaml` 예시:

```yaml
GOOGLE_API_KEY: "your-key-here"
```

`configs/goose-secrets.yaml`은 실제 API key를 담는 로컬 파일이며 절대
커밋하지 않는다. 지원 provider는 `google`, `anthropic`, `openai`,
`ollama` 및 Goose가 지원하는 provider를 따른다.

### 3. 실행

```bash
sudo ./anvil-daemon
```

첫 실행에서는 `micro-init`, `goose-agent`, golden image, Firecracker kernel,
Firecracker binary를 준비한다. 이후 실행에서는 기존 artifact를 재사용한다.

---

## 테스트

### 단위 테스트

```bash
go test ./...
```

GitHub Actions에서도 push/PR마다 실행된다. API token parsing, profile path
resolution, agent auth middleware, token generation, Town Wall seq, flock
metadata persistence, watchdog dead-marking 등을 검증한다.
Covers: API token parsing, LLM profile path resolution, agent auth middleware, token generation, Town Wall append/history/seq monotonicity, flock metadata persistence round-trip and disk recovery, watchdog dead-marking under failure thresholds (incl. v0.3.4 Configure + auto-heal tunables), artifact staleness check, per-VM state.json round-trip/sort/idempotent-delete/empty-workdir, **Prometheus registry counters / counter-vecs / gauges / histograms** (race-safe, exposition format spec compliance, label escaping) (v0.3.5), **`/metrics` handler** (content-type, GET-only, default-unauth, counter/gauge reflection) (v0.3.5), **`/vms/{vm_id}/stats` handler** (`/proc/<pid>/stat`+`/status` parsing, TAP statistics, agent-busy probe with timeout, `?stats=true` inline branch) (v0.3.5), **slog handler selection** (TextHandler vs JSONHandler, `EPHEMERA_LOG_LEVEL` gating) (v0.3.5).

### 종단 간 테스트

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
go build -o anvil-scheduler ./cmd/anvil-scheduler
sudo bash e2e_test.sh
```

`e2e_test.sh`는 실제 Firecracker MicroVM을 부팅하는 58단계 통합 테스트다.
호스트에 `/dev/kvm`, root 권한, 로컬 LLM API key가 필요하다. 환경과 API
rate limit에 따라 보통 15-30분 이상 걸릴 수 있다.

검증 범위:

| 단계 | 시나리오 |
|---|---|
| 1-5 | daemon startup, 단일 VM create/task/stop/delete |
| 6-9 | VM 두 개의 병렬 task 실행 |
| 11-17 | Full snapshot create/list/restore/delete |
| 19-24 | 서로 다른 snapshot의 concurrent restore |
| 26-29 | Diff snapshot 자동 선택과 sparse size 검증 |
| 30-34 | Diff restore와 full/diff dependency protection |
| 36-43 | COW rootfs restore와 kernel resource cleanup |
| 45-47 | control-plane agent proxy endpoint |
| 48-49 | `EPHEMERA_PUBLIC_URL` 기반 proxy `agent_url` |
| 51-57 | Goosetown flock 생성, `/vms` 반영, Town Wall post/history/list/delete, token redaction |
| 57a-57f | Town Wall seq, flock metadata persistence, daemon restart recovery, watchdog log |
| 58 | daemon graceful shutdown |
**What it tests (steps 1–89 incl. lettered sub-steps):**

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
| 56 | `GET /flocks/{id}/wall/history` returns ≥ 3 entries (orchestrator init + step 55 + step 55a) and the 55a body (escaped quote + backslash) matches verbatim |
| 56a | **Town Wall query filters** (v0.4.3) — `?agent_id=` returns only that agent's entries; `?contains=` returns only matching bodies |
| 57 | `GET /flocks` lists the new flock |
| 57a–c | **Dynamic agent membership** (v0.4.3) — `POST /flocks/{id}/agents` adds `worker-2` (count→6, `/health` 200); `PATCH …/agents/worker-2` `{role:reviewer}` recreates the VM (vm_id swap, role updated); `DELETE …/agents/worker-2` (count→5, VM torn down) |
| 57d–f | **Pause/resume + max_agents** (v0.4.3) — `POST /flocks/{id}/pause` (members → runtime-only `paused`; watchdog leaves them alone past its threshold), `/resume` (pre-pause status restored, `/health` 200 for running members); `POST /flocks {roles:3, max_agents:2}` → 400 |
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
| 59e | **Broadcast fan-out** (v0.4.4, LLM-gated) — `POST /flocks/{id}/broadcast` to the live researcher flock returns 200 with `agents==1`/`sent==1`, `results["researcher-1"].status=="ok"`, and the broadcast notice lands on the Town Wall |
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

**Example output (passing, flock steps 51–60):**

### E2E replay player

`cmd/e2e-replay-server`는 full KVM E2E와 IronClaw E2E terminal recording을
브라우저에서 line-by-line replay하는 작은 web player다. Recording은 서버에서
ANSI 제어 문자와 token/API key를 제거한 뒤 `/api/recording`으로 제공한다.

기본 playlist는 다음 두 항목이다.

| Replay | Source | 설명 |
|---|---|---|
| `full-kvm-e2e` | `docs/replays/full-kvm-e2e.txt` | `anvil-v0.2.0` full KVM 58단계 replay |
| `ironclaw-e2e` | `/tmp/anvil-real-e2e-recording.typescript` | 로컬에 recording이 있을 때만 사용 가능한 IronClaw MCP replay |

```bash
go run ./cmd/e2e-replay-server
```

기본 주소는 `http://192.168.3.73:8787`이다. 다른 recording을 지정하려면:

```bash
go run ./cmd/e2e-replay-server \
  -addr 127.0.0.1:8788 \
  -full-kvm-recording docs/replays/full-kvm-e2e.txt \
  -recording /tmp/anvil-real-e2e-recording.typescript
```

API:

```bash
curl http://192.168.3.73:8787/api/playlist
curl 'http://192.168.3.73:8787/api/recording?id=full-kvm-e2e'
━━━ 54b. Post via agent /townwall/post (in-VM forwarding path) ━━━
  ✓ Got agent_token for researcher-1 (64 chars)
  ✓ Resolved private IP for researcher-1: 10.0.1.3
  ✓ POST http://10.0.1.3:8080/townwall/post (HTTP 200)
  ✓ POST /townwall/post without bearer (must be rejected) (HTTP 401)

━━━ 55. Retrieve Town Wall history ━━━
  ✓ Town Wall has 3 entries
  ✓ In-VM /townwall/post entry round-tripped (agent_id+body match) ✓

━━━ 56. Verify GET /flocks lists the new flock ━━━
  ✓ GET /flocks returns 1 entry(ies)

━━━ 57. Delete flock and verify all member VMs are torn down ━━━
  ✓ DELETE /flocks/flock-1778665945495324840 (HTTP 200)
  ✓ All flock VMs torn down
  ✓ Flock unregistered from manager

━━━ 57a. Create flock for resilience scenarios ━━━
  ✓ POST /flocks (resilience) (HTTP 201)
  ✓ Resilience flock: flock-1778666301234567890

━━━ 57b. Town Wall messages carry monotonic seq ━━━
  ✓ First post has seq=2 ✓
  ✓ Seq monotonic: 2 → 3 ✓

━━━ 57c. Flock metadata.json persisted to disk ━━━
  ✓ metadata.json exists at /home/.../flocks/flock-.../metadata.json ✓
  ✓ metadata.json has correct flock_id ✓
  ✓ schema_version == 1 ✓

━━━ 57d. Daemon restart recovers flock from disk ━━━
  ✓ VM state.json persisted for all flock members ✓
  ✓ Daemon back up after restart
  ✓ Flock flock-... recovered after daemon restart ✓
  ✓ Town Wall history preserved: 3 entries ✓
  ✓ Recovered history seq 3 ≥ pre-restart seq 3 ✓

━━━ 57e. Cold-restart preserves VM IDs ━━━
  ✓ VM IDs unchanged across daemon restart ✓
  ✓ VM vm-... is live in /vms after cold-restart ✓
  ✓ VM vm-... is live in /vms after cold-restart ✓
  ✓ VM vm-... is live in /vms after cold-restart ✓

━━━ 57f. Recovered VM /health responds ━━━
  ✓ VM vm-... /health → 200 ✓
  ✓ VM vm-... /health → 200 ✓
  ✓ VM vm-... /health → 200 ✓

━━━ 57g. DELETE recovered flock removes metadata.json ━━━
  ✓ DELETE recovered flock (HTTP 200)
  ✓ metadata.json removed after DELETE ✓

━━━ 57h. Watchdog start log line present ━━━
  ✓ Watchdog start log line present in 3 daemon run(s) ✓

━━━ 57i. Watchdog persists dead status to metadata.json ━━━
  ✓ POST /flocks (watchdog persist) (HTTP 201)
  ✓ Watchdog marked worker-1 dead in ≤30s ✓
  ✓ metadata.json on disk shows status=dead (Persist hook fired) ✓

━━━ 57j. Per-agent restart preserves identity and reuses agent_token ━━━
  ✓ POST /flocks (restart test) (HTTP 201)
  ✓ POST .../agents/reviewer-1/restart (HTTP 200)
  ✓ VM ID swapped: vm-1779176432527292612 → vm-1779176434494332773 ✓
  ✓ Restarted agent status reset to ready ✓
  ✓ Role preserved across restart ✓
  ✓ New VM /health → 200 ✓
  ✓ Old agent_token still valid on the new VM (token preserved) ✓

━━━ 58. Shut down daemon ━━━
  ✓ Daemon stopped

━━━ 58b. Auth-on daemon spawned for v0.3.3 CP-token scenarios ━━━
  ✓ Auth-on daemon ready

━━━ 58b.i. In-VM /townwall/post auto-authenticates under auth-on CP ━━━
  ✓ POST /flocks (auth-on) (HTTP 201)
  ✓ In-VM /townwall/post → CP succeeded with auto-injected CP token ✓
  ✓ Town Wall received auth-on forward ✓

━━━ 59. Real-LLM /tasks smoke test ━━━
  ✓ Skipped — set GOOGLE_API_KEY, ANTHROPIC_API_KEY, or OPENAI_API_KEY to run

━━━ 58c. Kill auth-on daemon to prep for rotation test ━━━
  ✓ Auth-on daemon stopped

━━━ 58c.i. Spawn TOKENS_FILE-backed daemon (token=v1) ━━━
  ✓ File-source daemon ready

━━━ 58c.ii. Spawn flock and verify v1 in-VM CP forward ━━━
  ✓ POST /flocks (rotation) (HTTP 201)
  ✓ Pre-rotation /townwall/post via v1 CP token: 200 ✓

━━━ 58c.iii. Edit tokens file + SIGHUP daemon ━━━
  vsock UDS state before SIGHUP:
    srwxr-xr-x 1 root root 0 May 20 15:51 /tmp/firecracker-vsock-vm-1779259880788616345.sock
  ✓ vsock fan-out: 1/1 VMs OK

━━━ 58c.iv. Post-rotation /townwall/post must still succeed (v2 reached VM) ━━━
  ✓ Post-rotation /townwall/post via v2 CP token: 200 ✓

━━━ 58c.v. v1 operator bearer must now be rejected ━━━
  ✓ v1 operator bearer correctly rejected (401) ✓

━━━ 58c.vi. Town Wall received both pre- and post-rotation posts ━━━
  ✓ Town Wall recorded both posts ✓

━━━ 58c.vii. Cleanup rotation test ━━━
  ✓ Rotation flock deleted, tokens file removed

━━━ 60. Shut down rotation daemon ━━━
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

## 설정

모든 daemon 설정은 시작 시 환경 변수에서 읽는다.

- `EPHEMERA_API_ADDR` / `ANVIL_API_ADDR`
  - 기본값: `127.0.0.1:3000`
  - control plane bind 주소다.
  - reverse proxy 뒤에서는 `0.0.0.0:3000`으로 설정할 수 있다.
  - VM 내부 `gtwall`/`/townwall/post` forward path를 쓰려면 bridge gateway
    `10.0.1.1:3000`에서도 control plane에 닿아야 하므로
    `0.0.0.0:3000` bind가 필요하다.

- `EPHEMERA_API_PORT` / `ANVIL_API_PORT`
  - 기본값: `3000`
  - API addr가 없을 때 사용하는 port다.

- `EPHEMERA_API_TOKENS` / `ANVIL_API_TOKENS`
  - 기본값: unset
  - named Bearer token 목록이다.
  - 예: `alice:token1,bob:token2`

- `EPHEMERA_API_TOKEN` / `ANVIL_API_TOKEN`
  - 기본값: unset
  - 단일 Bearer token fallback이다.

- `EPHEMERA_AGENT_PORT` / `ANVIL_AGENT_PORT`
  - 기본값: `8080`
  - VM 내부 `goose-agent` listen port다.

- `EPHEMERA_PUBLIC_URL` / `ANVIL_PUBLIC_URL`
  - 기본값: unset
  - 외부에서 접근 가능한 control plane base URL이다.
  - 설정 시 `agent_url`이 proxy path가 된다.

- `EPHEMERA_HOME`
  - 기본값: process current working directory
  - daemon이 `artifacts/`, `configs/`, `snapshots/` 같은 runtime path를 해석할
    기준 directory다.

- `EPHEMERA_DISK_MODE`
  - 기본값: unset
  - `cow`로 설정하면 새 VM 생성 시 golden image full copy 대신 dm-snapshot 기반
    sparse COW disk를 사용한다.

- `EPHEMERA_EGRESS_PROFILE_DIR` / `ANVIL_EGRESS_PROFILE_DIR`
  - 기본값: `configs/profiles`
  - `profile` egress policy가 `egress.json`을 찾는 profile directory다.
  - canonical `EPHEMERA_EGRESS_PROFILE_DIR`가 alias보다 우선한다.

- `ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_ENDPOINT`
  - 기본값: unset
  - 설정 시 daemon lifecycle span을 `{endpoint}/v1/traces`로 전송한다.
  - `ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT`가 우선한다.

`EPHEMERA_*`는 ephemera runtime의 canonical 변수이고 `ANVIL_*`는 anvil 운영자를
위한 alias다. 각 변수 쌍에서는 `EPHEMERA_*` 값이 `ANVIL_*` 값보다 우선한다.
bind 주소 쌍(`EPHEMERA_API_ADDR`/`ANVIL_API_ADDR`)은 port 쌍보다 우선한다.
인증 token precedence는 `EPHEMERA_API_TOKENS` -> `ANVIL_API_TOKENS` ->
`EPHEMERA_API_TOKEN` -> `ANVIL_API_TOKEN` -> 인증 비활성화 순서다. token은
`SIGHUP`으로 daemon 재시작 없이 reload할 수 있다.

flock VM 내부에서 `EPHEMERA_CONTROL_PLANE`은 Town Wall forward 대상 control plane
URL을 바꾸는 test override다. 기본값은 `http://10.0.1.1:3000`이다.
`EPHEMERA_CONTROL_PLANE_TOKEN`이 설정되어 있으면 VM 내부 `/townwall/post`가 host
control plane으로 전달할 때 Bearer token으로 첨부한다.
| Variable | Default | Description |
|----------|---------|-------------|
| `EPHEMERA_API_ADDR` | `127.0.0.1:3000` | Control plane bind address. Set to `0.0.0.0:3000` when behind a reverse proxy, or when using flocks: the in-VM `gtwall` / `/townwall/post` forwarder targets `http://10.0.1.1:3000` (the bridge gateway), which is unreachable with the loopback-only default. |
| `EPHEMERA_API_PORT` | `3000` | Port only (used when `EPHEMERA_API_ADDR` is not set). |
| `EPHEMERA_API_TOKENS_FILE` | *(unset)* | Path to a file containing `name:token[:expires]` entries (comma- or newline-separated). When set, **takes precedence over `EPHEMERA_API_TOKENS`** and is re-read on every `loadAPIClients()` call — which is what enables SIGHUP-driven hot rotation since env values are fixed at exec (v0.3.4). The optional `:expires` (RFC3339 or Unix seconds) enforces a per-token TTL (v0.4.1). |
| `EPHEMERA_API_TOKENS` | *(unset)* | Per-client Bearer tokens: `alice:token1,bob:token2` (each entry may carry an optional `:expires`, see `_TOKENS_FILE` and the Token TTL docs — v0.4.1). The first **non-expired** token is also auto-injected into every flock VM at `/root/.ephemera-cp-token` so the in-VM `/townwall/post` forwarder can call back to the control plane without manual setup (v0.3.3; first-non-expired since v0.4.1). v0.3.4 SIGHUP fan-out propagates rotations to running VMs — see `_TOKENS_FILE` for true hot rotation. |
| `EPHEMERA_API_TOKEN` | *(unset)* | Single Bearer token (backward-compatible fallback). |
| `EPHEMERA_AGENT_PORT` | `8080` | Port goose-agent listens on inside each VM. |
| `EPHEMERA_MCP_ENABLED` | *(unset)* | Enable the MCP gateway (`1`/`true`/`yes`/`on`) (v0.6.0). Off = VMs get no MCP extension; behavior unchanged. Requires `configs/mcp/servers.yaml`. |
| `EPHEMERA_MCP_PORT` | `3001` | Port the MCP gateway listens on. The endpoint injected into each VM is `http://ephemera-gw:<port>/mcp` — a letter-starting alias for the bridge IP (mapped via an injected `/etc/hosts` entry) so the tool-name prefix goose derives from the URL stays valid for providers like Gemini (v0.6.0). |
| `EPHEMERA_MCP_BIND_IP` | `10.0.1.1` | Bind IP for the gateway listener — the bridge gateway IP, reachable only from VMs and the host, never externally (v0.6.0). |
| `EPHEMERA_MAX_TASK_DEPTH` | `5` | Max nested agent→agent `/tasks` hops (v0.4.4). The proxy reads `X-Ephemera-Task-Depth` per hop, rejects at/over this cap with `508 Loop Detected`, and forwards `depth+1`. A large value effectively disables the guard. |
| `EPHEMERA_PUBLIC_URL` | *(unset)* | Externally-reachable base URL of the control plane (no trailing slash). When set, `agent_url` in VM responses uses the proxy path `{EPHEMERA_PUBLIC_URL}/vms/{vm_id}` instead of the VM's private IP. Example: `https://api.example.com`. |
| `EPHEMERA_HOME` | current working directory | Work directory used to resolve `artifacts/`, `configs/`, `snapshots/`, and other daemon-local paths. Useful when launching from systemd or another supervisor. |
| `EPHEMERA_DISK_MODE` | *(unset)* | Spawn disk strategy. Unset, `plain`, or `full` uses the existing full byte-for-byte rootfs clone and does not probe COW support. Set to `cow` to provision spawn disks as a dm-snapshot view of the golden image (~0 MiB initial usage); if `losetup`/`dmsetup`/`dm_snapshot` support is unavailable, the daemon logs a warning and falls back to plain at startup. |
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
- **VM detail** — live stats + the spawned provider/model; a **conversation** panel that streams each turn (`POST /vms/{id}/tasks?stream=1`) and keeps context across turns (multi-turn, below), with Cancel + elapsed time; and a Delete action behind an in-app confirm (graceful teardown).
- **Settings** — lists every profile and edits its provider/model (`PUT /config/profiles/{name}`); changes apply to the **next** Create VM, not running VMs.

### Localization (EN / KO)

All UI strings live in `web/src/locales/{en,ko}.json` and render via `svelte-i18n`. The initial language follows the browser (`ko*` → Korean, otherwise English); a nav toggle (`EN | 한국어`) switches it and persists the choice in `localStorage`. Server-originated error text (the daemon's `{"error":…}`) is shown verbatim, not translated. The UI uses generic IT vocabulary — *Platform Agent*, *Agent Group*, *Activity Feed*, *Create/Delete* — as display labels only; API routes/fields/env vars keep their original identifiers (see `web/README.md`).

### Multi-turn conversation

The conversation panel sends an optional `session` on `POST /vms/{id}/tasks`. When present, `goose-agent` runs `goose run --output-format json -n <session> [--resume] -i -` — the first turn creates the named session, later turns `--resume` it — so the agent keeps conversation context across turns (stored in the VM's goose session db). Omitting `session` preserves the original stateless one-shot behavior used by `ephemera-ctl` and `gtcall`.


---

## VM별 LLM profile 설정

기본 설정:

```text
configs/goose.yaml
configs/goose-secrets.yaml
```

named profile:

```text
configs/profiles/anthropic/goose.yaml
configs/profiles/anthropic/goose-secrets.yaml
```

생성 요청:

```bash
curl -X POST http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"profile":"anthropic"}'
```

profile 이름에는 `/` 또는 `\`를 사용할 수 없다.

### Editing a profile's model (Web UI / API, v0.5.0)

A profile's provider/model can be read and changed at runtime without restarting the daemon — the Web UI **Settings** screen drives these endpoints:

```bash
# List all profiles with their current provider/model
curl http://localhost:3000/config/profiles -H "Authorization: Bearer $TOKEN"
# → [{"name":"default","provider":"google","model":"gemini-2.5-flash"}, {"name":"worker", …}]

# Update one profile (rewrites GOOSE_PROVIDER/GOOSE_MODEL in place — comments +
# extensions preserved; API keys in goose-secrets.yaml are never touched here)
curl -X PUT http://localhost:3000/config/profiles/worker \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"provider":"anthropic","model":"claude-sonnet-4-6"}'
```

`name` is `default` for `configs/goose.yaml`, otherwise a `configs/profiles/{name}/` directory. Because config is injected at spawn, an edit applies to the **next** VM created from that profile; already-running VMs keep the model they were spawned with (`GET /vms` reports each VM's `provider`/`model`).

