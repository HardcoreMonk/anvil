# anvil

[![CI](https://github.com/HardcoreMonk/anvil/actions/workflows/ci.yml/badge.svg)](https://github.com/HardcoreMonk/anvil/actions/workflows/ci.yml)
[![Latest Tag](https://img.shields.io/github/v/tag/HardcoreMonk/anvil?sort=semver&label=tag)](https://github.com/HardcoreMonk/anvil/tags)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Firecracker](https://img.shields.io/badge/Firecracker-v1.15.1-FF4500?logo=amazonaws&logoColor=white)](https://github.com/firecracker-microvm/firecracker)

**IronClaw의 tool call을 격리 MicroVM 실행으로 변환하는 AI agent execution layer**

`anvil`은 IronClaw의 판단과 tool call을 Firecracker MicroVM 안의 실제 agent
실행으로 변환하는 격리 execution layer다.

IronClaw는 상위 orchestration, planner, MCP client 역할을 맡는다. anvil은 그
요청을 VM 생성, 작업 실행, health 확인, graceful stop/delete, snapshot/restore
같은 실행 lifecycle로 바꾼다.

구조적으로 anvil은 두 경계를 연결한다. 첫 번째는 IronClaw가 호출하는
`anvil_*` MCP tool surface이고, 두 번째는 ephemera가 제공하는 KVM 기반
Firecracker MicroVM runtime boundary다.

즉 anvil은 IronClaw가 직접 host runtime 세부사항을 알지 않아도, 격리된 agent
workspace를 생성하고 제어할 수 있게 만드는 MCP adapter이자 실행 계약이다.

IronClaw와 ephemera를 서비스 대 서비스로 1:1 직접 연결하지 않는 이유는 두
시스템의 책임과 추상화 수준이 다르기 때문이다.

IronClaw는 "어떤 agent 작업을 수행할 것인가"를 결정하는 orchestration/MCP client
계층이다. ephemera는 "어떤 VM을 만들고 어떤 host resource를 정리할 것인가"를
다루는 low-level runtime control plane이다.

직접 연결하면 IronClaw가 VM ID, guest private URL, daemon token, agent token,
snapshot file lifecycle, cleanup 실패 처리 같은 runtime 세부사항을 알아야 한다.

anvil은 이 결합을 막는다. IronClaw에는 안전한 `anvil_*` tool 계약만 노출하고,
내부에서 ephemera API 호출, session alias, token redaction, workspace 정책,
restore/cleanup 의미를 변환한다.

anvil의 상위 통합 대상은 IronClaw 전용이다. OpenClaw 연동은 anvil의 지원 범위가
아니며, OpenClaw용 compatibility layer나 운영 계약은 제공하지 않는다.

이 저장소의 현재 URL은 `https://github.com/HardcoreMonk/anvil/`이다.
이 저장소는 `https://github.com/steve-seungeui/ephemera`의 fork로 유지한다.
ephemera는 계속 버전업되는 runtime engine upstream이고, anvil은 그 runtime을
IronClaw 실행 계층으로 통합하는 downstream product fork다. 따라서 Go 모듈 경로,
daemon 이름, HTTP API, 일부 환경 변수에는 `ephemera` 또는 `goose` 이름이 남아
있다. README에서는 `anvil`을 IronClaw 통합 프로젝트로, `ephemera`를 분리된 기반
runtime으로 구분한다.

버전별 ephemera 소스 snapshot은 Git tag로 공개된다. 현재 `main`의 anvil runtime
baseline은 upstream ephemera `v0.3.6`까지 병합한 상태다. 이 병합은 MicroVM
lifecycle, flock resilience, token rotation, observability 같은 runtime substrate를
끌어올린 것이며, anvil의 제품 정체성을 ephemera로 바꾸는 작업이 아니다.
2026-07-02 기준 upstream `main`과 최신 upstream tag는 `v0.7.0`이지만, anvil
`main`의 runtime baseline은 계속 `v0.3.6`이다. `v0.4.0`-`v0.4.5` runtime 안정화는
다음 sync 후보이고, `v0.5.0`-`v0.7.0`은 별도 adoption review가 필요한 backlog다.
upstream `v0.7.0`의 kernel SHA 검증, `waitForAgent` per-probe timeout,
`EPHEMERA_HOME` hardening은 선별 backport됐지만 baseline sync 완료를 의미하지
않는다.

IronClaw 통합 프로젝트 anvil의 최신 공개 tag는 `anvil-v0.3.2`이다. 이 release는
ephemera `v0.3.2`-`v0.3.6` runtime baseline 위에 scheduler metrics, manual
cross-host snapshot replication, scheduler-aware single-host flock placement를
기록한다. 첫 공개 tag는 `anvil-v0.1.0`이다.

<p align="center">
  <img src="docs/assets/ironclaw-e2e.gif" alt="IronClaw anvil E2E terminal replay" width="900">
</p>

<p align="center">
  <sub>IronClaw 본체에서 anvil MCP tool을 호출한 E2E terminal replay</sub>
</p>

---

## 프로젝트 경계

- **IronClaw**: 상위 orchestration, MCP client, 작업 의사결정을 담당한다.
  현재 구현은 anvil 밖의 외부 통합 계층이다.

- **anvil**: IronClaw가 사용할 MCP tool surface와 실행 lifecycle 계약을 제공한다.
  구현 위치는 `cmd/anvil-mcp`, `internal/anvilmcp`다.

- **anvil runtime scheduler**: 여러 ephemera daemon host 후보 중 요청을 실행할
  host를 고르고 placement/snapshot locality/quota 상태를 보존한다. 구현 위치는
  `cmd/anvil-scheduler`, `internal/anvilmcp`다.

- **ephemera**: Firecracker MicroVM 생성, agent proxy, snapshot/restore,
  host resource 정리를 담당한다. 구현 위치는 `cmd/goose-daemon`, `internal/vm`,
  `internal/storage`, `internal/network`다.

- **guest runtime**: VM 내부 task 실행, health, graceful stop을 담당한다.
  구현 위치는 `cmd/goose-agent`, `cmd/micro-init`이다.

anvil은 ephemera를 이름만 바꾼 프로젝트가 아니다. anvil은 IronClaw와 ephemera를
연결하는 통합 실행 layer이고, ephemera는 독립적인 runtime 구현과 API 계약을
가진다. 따라서 runtime API와 환경 변수는 호환성을 위해 ephemera/goose 이름을
유지하고, IronClaw가 직접 사용하는 표면은 `anvil_*` MCP tool로 노출한다.

### Runtime Baseline

`sync/ephemera-v0.3.6`은 upstream ephemera `v0.3.6`을 runtime baseline으로 사용한다.

| 구분 | 현재 기준 | anvil에서의 의미 |
|---|---|---|
| ephemera runtime baseline | `v0.3.6` | Firecracker VM lifecycle, cold-restart, flock recovery, token rotation, `/metrics`, `/stats`, `slog`, in-VM `gtcall`, webdev demo 기반 |
| upstream latest observed | `v0.7.0` (2026-07-02 확인) | 아직 anvil baseline으로 병합하지 않음. `v0.4.0`-`v0.4.5`는 planned sync, `v0.5.0`-`v0.7.0`은 별도 검토 backlog |
| anvil product surface | `anvil_*` MCP tool, scheduler, tenant/egress, workload runner | IronClaw가 직접 사용하는 공개 실행 계약 |
| namespace policy | `EPHEMERA_*`, `goose-*`, `ephemera_*` 유지 | upstream runtime 호환성. anvil 이름으로 일괄 rename하지 않는다. |

ephemera `v0.3.2`-`v0.3.6`은 anvil 안에서 runtime baseline으로 채택/적응된 변경이다.
anvil release note에서는 이 내용을 "upstream runtime baseline"으로 분리해 기록하고,
MCP/scheduler/workload/tenant/egress 같은 anvil 고유 기능과 섞어 제품명처럼 쓰지
않는다.
선별 backport된 upstream `v0.7.0` hardening은 현재 baseline 위의 보안/운영 보강으로
취급하며, `v0.4.x`-`v0.7.x` 전체 sync 완료로 표기하지 않는다.

## Fork와 upstream 관리

`HardcoreMonk/anvil`은 `steve-seungeui/ephemera`의 fork network를 유지한다. 이
관계는 의도된 운영 방식이다. ephemera가 runtime engine으로 계속 버전업되기 때문에
anvil은 upstream runtime 변경을 merge로 받아들이고, IronClaw 통합 계층을 그 위에
적응시킨다.

권장 remote 설정:

```bash
git remote -v
git remote add upstream https://github.com/steve-seungeui/ephemera.git
git remote set-url upstream https://github.com/steve-seungeui/ephemera.git
git remote set-url --push upstream DISABLED
```

upstream sync는 별도 branch에서 수행한다.

```bash
git fetch upstream main
git ls-remote --tags upstream
git fetch upstream tag v0.4.5
git checkout -b sync/ephemera-v0.4-runtime-core origin/main
git merge --no-ff v0.4.5
```

기존 `v*` tag를 덮어쓰는 `git fetch --tags --force`는 사용하지 않는다. ephemera
runtime release tag는 `v*`, anvil product release tag는 `anvil-v*` prefix를
사용한다. anvil 작업 branch는 rebase/history rewrite 없이 PR merge로 관리한다.

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

## anvil 핵심 기능

- **IronClaw MCP adapter**:
  `cmd/anvil-mcp`가 IronClaw에 `anvil_*` MCP tool을 제공한다.

- **VM lifecycle tool**:
  `anvil_spawn_vm`, `anvil_run_task`, `anvil_get_vm_health`,
  `anvil_stop_vm`, `anvil_delete_vm`을 제공한다.

- **Snapshot lifecycle tool**:
  `anvil_create_snapshot`, `anvil_list_snapshots`, `anvil_restore_snapshot`,
  `anvil_delete_snapshot`, `anvil_replicate_snapshot`을 제공한다.

- **Session alias**:
  adapter process 내부에서 `session_name -> vm_id` alias를 유지해
  IronClaw workflow를 단순화한다.

- **Token redaction**:
  `agent_token`은 `POST /vms` 응답에만 노출하며 daemon restore 응답과 MCP output에는
  노출하지 않는다.

- **Restore cleanup 계약**:
  restore 성공 후 alias bind가 실패하면 restored VM을 자동 삭제하지 않고
  error에 VM ID를 포함한다.

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
                                -> Town Wall 전체 history 조회

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
  Town Wall log를 복구한다. 현재 upstream `v0.3.6` baseline에서는 spawn-path member
  VM도 `vms/<vm_id>/state.json` 기반으로 cold-restart된다. memory state와 in-flight
  task는 보존되지 않는다.

- **Town Wall sequence**:
  Town Wall message는 per-flock monotonic `seq`를 포함해 subscriber가 gap을
  감지하고 history로 복구할 수 있다.

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
| **VM health watchdog** (v0.3.1) | Polls every flock-member `/health` every 5 s; 3 consecutive failures → agent `status=dead` + auto Town Wall notice. See [Resilience](#resilience). |
| **Flock metadata persistence** (v0.3.1) | `flocks/<id>/metadata.json` written atomically on spawn; daemon startup re-registers every flock and reopens its Town Wall log. |
| **Monotonic Town Wall seq** (v0.3.1) | Every `Message` carries `seq` (uint64, 1-based per flock); subscribers can detect dropped messages and recover from `/wall/history`. |
| **Fatal-on-bind daemon startup** (v0.3.1) | Daemon `log.Fatalf` if the API listener fails to bind (e.g. port already in use), so a stale process never silently masks a fresh one. |
| **Live VM cold-restart** (v0.3.2) | `vms/<vm_id>/state.json` written on every spawn; daemon startup cleans orphan Firecracker processes, re-reserves the original TAP/IP/MAC, and boots each VM from its existing rootfs clone. Same `vm_id`, same agent token, same `agent_url` across the restart. Memory state is not preserved. See [Resilience](#resilience). |
| **Watchdog dead-status persistence** (v0.3.3) | When the watchdog marks an agent `dead`, the new status is written to `flocks/<id>/metadata.json` (via `Flock.Persist`, serialized by a per-flock `writeMu`). Daemon restart and cold-restart both preserve the marking, so a once-dead agent stays dead until explicitly restarted. |
| **Per-agent restart** (v0.3.3) | `POST /flocks/{id}/agents/{agent_id}/restart` tears down one flock member's VM and respawns it with the same `agent_id`, role, and `agent_token` (callers' cached tokens keep working). The new VM gets a fresh `vm_id` / `guest_ip`; the agent's status resets to `ready`. |
| **Auto-injected control-plane token** (v0.3.3) | When `EPHEMERA_API_TOKENS` is set, the host writes `apiClients[0].Token` into each flock VM at `/root/.ephemera-cp-token` (mode 0600); the in-VM `/townwall/post` forwarder reads it automatically. No more manual `EPHEMERA_CONTROL_PLANE_TOKEN` env inside every VM. |
| **CP token hot rotation** (v0.3.4) | `EPHEMERA_API_TOKENS_FILE=/path/to/tokens` enables true hot rotation: edit the file, send SIGHUP, and the daemon both swaps `cp.clients` and fans the new token out to every running VM over vsock (`SET_CP_TOKEN` command, atomic file rewrite inside the guest). No per-VM restart needed for the in-VM forwarder to pick up the new bearer. |
| **Env-tunable watchdog** (v0.3.4) | `EPHEMERA_WATCHDOG_INTERVAL_SEC` / `_TIMEOUT_SEC` / `_THRESHOLD` override the 5 s / 1 s / 3-fail defaults at startup. `EPHEMERA_WATCHDOG_AUTO_HEAL=true` opts in to self-healing — a `dead` agent that resumes responding is auto-marked `ready` (default off preserves sticky-dead). |
| **Observability trio** (v0.3.5) | Prometheus `/metrics` endpoint (zero-dep exposition format, counters + gauges + histograms), per-VM `GET /vms/{vm_id}/stats` snapshot (cpu/mem/net/uptime/agent_busy), and a `log/slog` migration with `EPHEMERA_LOG_FORMAT=json` + `EPHEMERA_LOG_LEVEL=...` controls. See [Observability](#observability-v035). |
| **Autonomous multi-agent demo** (v0.3.6) | `webdev_demo.sh` stands up an orchestrator + worker + reviewer flock that designs, generates, reviews, and publishes a complete React + Vite site to the Town Wall with zero host authorship. See [Multi-Agent Webdev Demo](#multi-agent-webdev-demo-v036). |
| **In-VM agent-to-agent dispatch** (v0.3.6) | `gtcall <agent_id> "<prompt>"` sends a task to a peer through the control-plane proxy, which injects the peer's token. Both `gtcall` and `gtwall` build their request bodies with `jq --arg`, so arbitrary multi-line prompts and file bodies (newlines, quotes, backticks) post safely. |
| **Clean agent task output** (v0.3.6) | goose-agent runs Goose with `--output-format json` and returns the extracted assistant text, so `/tasks` output is no longer interleaved with the startup banner or truncated to an in-VM temp file when fenced code exceeds 50 lines. |

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
    api.go            HTTP API: VM + snapshot CRUD, auth middleware,
                      two-mux split for unauthenticated /metrics (v0.3.5),
                      spawnVMInternal (shared by /vms and /flocks paths;
                      AgentToken / ControlPlaneToken plumb-through),
                      counter/histogram wiring for spawn/destroy/snapshot/
                      flock/SIGHUP/CP-token paths (v0.3.5),
                      controlPlaneTokenForVM (apiClients[0] → in-VM bearer)
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
  goose-agent/        In-VM HTTP agent (baked into golden image)
    main.go           /tasks, /health, /stop, /townwall/post  (Bearer token auth);
                      prepends role system prompt to /tasks bodies;
                      runs `goose run --output-format json` and extracts the
                      assistant text via extractGooseJSONText (banner-skip) (v0.3.6)
  micro-init/         PID 1 for each MicroVM (baked into golden image)
    main.go           Mounts virtual filesystems, manages goose-agent,
                      calls poweroff(2) on exit

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

e2e_test.sh           End-to-end integration test (62 numbered steps incl. resilience + v0.3.3 / v0.3.4 / v0.3.5 sub-steps; requires /dev/kvm + root)
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

## 문서 지도

- [CONTEXT.md](CONTEXT.md):
  anvil/ephemera/IronClaw 경계, 진실 기준 문서 순서, 고정 계약.

- [AGENTS.md](AGENTS.md):
  Codex 작업 규약, 검증 명령, 불변 조건.

- [docs/PUBLIC_RELEASE_BOUNDARY.md](docs/PUBLIC_RELEASE_BOUNDARY.md):
  anvil 공개 포함/조건부 포함/제외 표면, upstream ephemera 변경 채택 분류.

- [docs/ADR_INDEX.md](docs/ADR_INDEX.md):
  anvil 장기 설계 결정의 현재 적용 상태와 ADR 작성 기준.

- [RELEASE_NOTES.md](RELEASE_NOTES.md):
  anvil product release note와 upstream ephemera runtime release note를 분리해
  기록한다. 현재 `v0.3.2`-`v0.3.6`은 `main`에 병합된 anvil runtime baseline이다.

- [docs/architecture/runtime-architecture.md](docs/architecture/runtime-architecture.md):
  ephemera daemon, MicroVM, storage, network, guest runtime 구조.

- [docs/architecture/service-logic.md](docs/architecture/service-logic.md):
  ephemera control-plane API, VM lifecycle, snapshot/restore, guest agent 흐름.

- [docs/architecture/mcp-architecture.md](docs/architecture/mcp-architecture.md):
  IronClaw MCP adapter 구조와 tool 계약.

- [docs/architecture/multi-tenant-roadmap.md](docs/architecture/multi-tenant-roadmap.md):
  tenant quota, scheduler, egress policy, audit storage, multi-host runtime의
  책임 경계와 단계적 확장 기준.

- [docs/operations/security-policy.md](docs/operations/security-policy.md):
  공개 노출, 제어 평면 token, guest agent token, snapshot metadata 반출 보안 정책.

- [docs/operations/runbook.md](docs/operations/runbook.md):
  daemon 빌드/시작, health 확인, VM 정리, snapshot GC dry-run/apply 운영 명령.

- [docs/operations/disaster-recovery.md](docs/operations/disaster-recovery.md):
  daemon crash, stale TAP/IP, restore 실패, GC 실패, diff base 누락 대응 playbook.

- [docs/operations/observability.md](docs/operations/observability.md):
  daemon log, `/health`, `/metrics`, `/metrics/vms`, snapshot GC audit, runtime audit
  API, optional trace export, 향후 지표 후보.

- [docs/analysis/README.md](docs/analysis/README.md):
  ephemera 0.1.0/0.2.0 분석 문서와 upstream 0.3.x 변경 검토 문서 index.

- [ephemera v0.3.2/v0.3.3 upstream 변경 검토](docs/analysis/08-v0.3.2-v0.3.3-upstream-change-review.md):
  upstream `v0.3.2` live VM cold-restart와 `v0.3.3` operational polish 변경의
  태그/commit/diff 근거, anvil 채택 검토 포인트. 현재는 병합 전 분석 근거로
  보존한다.

- [anvil redesign handoff](docs/operations/2026-05-11-anvil-redesign-handoff.md):
  재설계 release/operate handoff 근거.

- [anvil release checklist](docs/operations/release-checklist.md):
  ephemera runtime 릴리즈와 anvil integration 릴리즈를 구분하는 게시 전
  확인 절차, `anvil-v0.2.0` 게시 기록, historical GitHub Release 본문.

- [upstream sync policy](docs/operations/upstream-sync-policy.md):
  `steve-seungeui/ephemera` fork 유지, upstream merge, tag 충돌 방지, sync PR 운영
  기준.

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
**What it tests (62 numbered steps incl. sub-steps):**

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
| 54b | **In-VM forwarding** — direct `POST $agent_url/townwall/post` (the chain that `gtwall` uses) round-trips through goose-agent → control plane; unauthenticated probe rejected with 401 |
| 55 | `GET /flocks/{id}/wall/history` returns ≥ 3 entries (orchestrator init + step 54 + step 54b) and the 54b body (escaped quote + backslash) matches verbatim |
| 56 | `GET /flocks` lists the new flock |
| 57 | **Flock teardown** — `DELETE /flocks/{id}` returns 200; all 5 VMs and the flock registry entry are gone |
| 57a | Create a separate resilience flock (3 agents) |
| 57b | **SSE seq monotonicity** — successive `POST /flocks/{id}/post` responses carry strictly increasing `seq` |
| 57c | **Flock persistence** — `flocks/<id>/metadata.json` exists with correct `flock_id` and `schema_version: 1` |
| 57d | **Recovery setup** — verify `vms/<vm_id>/state.json` for each agent; kill daemon (and Firecrackers); restart with `EPHEMERA_API_ADDR=0.0.0.0:3000`; flock metadata reloaded; Town Wall history preserved with seq continuity |
| 57e | **Cold-restart VM IDs preserved** — every pre-restart `vm_id` reappears in `GET /vms` with same identity |
| 57f | **Recovered VM `/health` responds** — proxy `GET /vms/{id}/health` returns 200 for each cold-restarted member |
| 57g | `DELETE` on a recovered flock removes its `metadata.json` |
| 57h | Daemon log shows the `watchdog started` slog line for each daemon invocation (lowercase since the v0.3.5 slog migration) |
| 57i | **Watchdog persists dead status to disk** (v0.3.3) — kill an in-VM agent, watchdog marks `dead`, `flocks/{id}/metadata.json` on disk reflects `dead` before the next probe (Persist hook fired). Daemon restart is intentionally not part of this step because cold-restart of a healthy guest legitimately re-flips to `ready`. |
| 57j | **Per-agent restart preserves identity + token** (v0.3.3) — `POST /flocks/{id}/agents/{agent_id}/restart` swaps `vm_id`, keeps role/token; new VM's `/townwall/post` accepts the OLD token |
| 58 | Daemon graceful shutdown |
| 58b | **Auth-on CP token auto-injection** (v0.3.3) — restart daemon with `EPHEMERA_API_TOKENS` set; flock VM's `/townwall/post` forward to CP returns 200 without any in-VM env setup |
| 59 | **Real-LLM round-trip** (v0.3.3) — when `GOOGLE_API_KEY` / `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` is in env, spawn researcher, send `/tasks`, verify `ROUNDTRIP_OK` reaches Town Wall via `gtwall`. Skipped (ok) when no key. |
| 58c | **CP token hot rotation via SIGHUP** (v0.3.4) — restart daemon with `EPHEMERA_API_TOKENS_FILE`; spawn flock under v1; edit file to v2 + SIGHUP; verify post-rotation `/townwall/post` still 200 (in-VM `/root/.ephemera-cp-token` rewritten via vsock), v1 operator bearer now 401, and the daemon log carries `msg="sighup: cp token propagated" ok=N total=M` (slog form since v0.3.5). |
| 61 | **`/metrics` endpoint format** (v0.3.5) — `GET /metrics` returns 200 unauthenticated, `Content-Type: text/plain; version=0.0.4`, body contains `# HELP`/`# TYPE` lines plus `ephemera_vm_count` gauge and `ephemera_sighup_reload_total` counter samples. |
| 62 | **Per-VM `/stats` endpoint + `?stats=true`** (v0.3.5) — spawn a VM, `GET /vms/{vm_id}/stats` returns a JSON snapshot with `uptime_seconds ≥ 0`, `mem_total_mib > 0`, numeric `cpu_percent`; `GET /vms?stats=true` inlines the same `stats` block on every VM list entry. |
| 60 | Rotation daemon shutdown |

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
| `EPHEMERA_API_TOKENS_FILE` | *(unset)* | Path to a file containing `name:token` entries (comma- or newline-separated). When set, **takes precedence over `EPHEMERA_API_TOKENS`** and is re-read on every `loadAPIClients()` call — which is what enables SIGHUP-driven hot rotation since env values are fixed at exec (v0.3.4). |
| `EPHEMERA_API_TOKENS` | *(unset)* | Per-client Bearer tokens: `alice:token1,bob:token2`. The first token (`apiClients[0]`) is also auto-injected into every flock VM at `/root/.ephemera-cp-token` so the in-VM `/townwall/post` forwarder can call back to the control plane without manual setup (v0.3.3). v0.3.4 SIGHUP fan-out propagates rotations to running VMs — see `_TOKENS_FILE` for true hot rotation. |
| `EPHEMERA_API_TOKEN` | *(unset)* | Single Bearer token (backward-compatible fallback). |
| `EPHEMERA_AGENT_PORT` | `8080` | Port goose-agent listens on inside each VM. |
| `EPHEMERA_PUBLIC_URL` | *(unset)* | Externally-reachable base URL of the control plane (no trailing slash). When set, `agent_url` in VM responses uses the proxy path `{EPHEMERA_PUBLIC_URL}/vms/{vm_id}` instead of the VM's private IP. Example: `https://api.example.com`. |
| `EPHEMERA_HOME` | current working directory | Work directory used to resolve `artifacts/`, `configs/`, `snapshots/`, and other daemon-local paths. Useful when launching from systemd or another supervisor. |
| `EPHEMERA_DISK_MODE` | *(unset)* | Set to `cow` to provision spawn disks as a dm-snapshot view of the golden image (~0 MiB initial usage) instead of a 700 MiB full copy. Default behavior is preserved when unset. |
| `EPHEMERA_WATCHDOG_INTERVAL_SEC` | `5` | Watchdog poll cadence (v0.3.4). |
| `EPHEMERA_WATCHDOG_TIMEOUT_SEC` | `1` | Watchdog per-probe HTTP timeout (v0.3.4). Clamped: `interval` is bumped up to `timeout` if smaller. |
| `EPHEMERA_WATCHDOG_THRESHOLD` | `3` | Consecutive probe failures before marking an agent `dead` (v0.3.4). |
| `EPHEMERA_WATCHDOG_AUTO_HEAL` | `false` | When `true` (`1`/`yes`/`on` also accepted), a `dead` agent that resumes responding is auto-marked `ready` and a recovery notice posted to the Town Wall (v0.3.4). Default off preserves sticky-dead. |
| `EPHEMERA_METRICS_REQUIRE_AUTH` | `false` | When `true`, `GET /metrics` requires a valid Bearer token like every other endpoint (v0.3.5). Default off matches the standard Prometheus scrape pattern; flip on when the metrics endpoint is exposed beyond a trusted network. |
| `EPHEMERA_LOG_FORMAT` | `text` | `text` (default) emits `key=value` lines from `log/slog`'s TextHandler; `json` switches to JSONHandler for log-aggregation pipelines (v0.3.5). |
| `EPHEMERA_LOG_LEVEL` | `warn` | Minimum slog level: `debug`, `info`, `warn`, or `error` (v0.3.5). Default `warn` preserves the previous `log.Printf` tone — every lifecycle event in the daemon is emitted at warn-or-higher so operators see it without configuration. |

`EPHEMERA_API_ADDR` takes precedence over `EPHEMERA_API_PORT`. Most variables are read at startup; use SIGHUP to reload tokens. With `EPHEMERA_API_TOKENS_FILE` SIGHUP also propagates the new `apiClients[0].Token` to running VMs via vsock (v0.3.4).

---

## IronClaw MCP 어댑터

`cmd/anvil-mcp`는 ephemera daemon API를 stdio MCP server로 노출한다.

```bash
go build -o anvil-mcp ./cmd/anvil-mcp
```

환경 변수 설정:

```bash
export ANVIL_DAEMON_URL=http://127.0.0.1:3000
export ANVIL_API_TOKEN="<daemon-bearer-token>"
export ANVIL_MCP_DEFAULT_TIMEOUT=300
# 선택 사항: multi-tenant foundation과 runtime audit
export ANVIL_MCP_TENANT_ID=tenant.alpha
export ANVIL_MCP_AUDIT_LOG=/var/lib/anvil-mcp/runtime-audit.jsonl
```

여기서 `ANVIL_API_TOKEN`은 `cmd/anvil-mcp` 프로세스가 daemon으로 보내는 outbound
Bearer token이다. goose-daemon 환경 변수에서는 같은 이름이
`EPHEMERA_API_TOKEN`의 fallback alias로, daemon이 client 요청에서 받아들이는
control-plane token을 뜻한다.

또는 설정 파일을 사용할 수 있다.

```bash
cp configs/anvil-mcp.yaml.example configs/anvil-mcp.yaml
export ANVIL_MCP_CONFIG=configs/anvil-mcp.yaml
```

MCP tool:

- `anvil_spawn_vm`:
  ephemera VM을 만들고 optional `session_name` alias를 연결한다.

- `anvil_run_task`:
  `vm_id` 또는 `session_name`으로 VM에 prompt를 실행한다.

- `anvil_copy_in`:
  `vm_id` 또는 `session_name`으로 VM `/workspace`에 단일 file을 쓴다.

- `anvil_copy_out`:
  `vm_id` 또는 `session_name`으로 VM `/workspace`의 단일 file을 읽는다.

- `anvil_get_vm_health`:
  VM agent health를 확인한다.

- `anvil_stop_vm`:
  guest agent에 graceful stop을 요청한다.

- `anvil_delete_vm`:
  host VM 리소스를 삭제하고 session alias를 해제한다.

- `anvil_create_snapshot`:
  `vm_id` 또는 `session_name`으로 VM snapshot을 생성한다.

- `anvil_list_snapshots`:
  daemon이 알고 있는 snapshot 목록을 조회한다.

- `anvil_restore_snapshot`:
  `snapshot_id`에서 새 VM을 restore하고 optional `session_name` alias를 연결한다.

- `anvil_delete_snapshot`:
  `snapshot_id`로 snapshot을 삭제한다.

- `anvil_replicate_snapshot`:
  `snapshot_id`, `source_host`, `target_host`, `include_dependencies`로 host 간
  snapshot bundle을 복제한다. diff snapshot에서 `include_dependencies=true`이면
  base full snapshot을 먼저 target host로 복제한 뒤 diff를 복제한다.

- `anvil_spawn_flock`:
  `task`, `roles`, optional `tenant_id`, optional `egress_policy`로 Goosetown
  flock을 생성한다. blank `task`, empty role, `/` 또는 `\`가 포함된 role은
  daemon VM spawn 전에 거부된다.

- `anvil_create_routed_flock_members`:
  `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`와 persistent scheduler state가
  설정된 경우에만 활성화되는 experimental tool이다. scheduler `POST /schedule/flock`
  plan으로 role별 host를 고른 뒤 host daemon `POST /vms`로 member VM만 생성한다.

- `anvil_list_flocks` / `anvil_get_flock` / `anvil_delete_flock`:
  live flock 목록, 단일 flock metadata와 agent 상태 조회, flock 소속 VM 삭제를 처리한다.
  일반 Goosetown flock은 daemon `GET/DELETE /flocks` 의미를 따른다. members-only
  routed flock은 `scheduler_state_path` registry의 visible record를 list/get에 합치고,
  delete는 registry의 member placement를 따라 host별 daemon `DELETE /vms`로 라우팅한다.

- `anvil_post_townwall` / `anvil_get_townwall_history`:
  flock Town Wall에 message를 append하고 stdio-compatible history를 조회한다.

Goosetown MCP tool은 additive extension이며 기존 VM/snapshot tool 계약을
대체하지 않는다. VM `session_name` alias는 flock alias로 재사용하지 않고, flock
작업은 명시적인 `flock_id`를 사용한다.

daemon direct `POST /flocks` 응답도 `agent_token`/`agent_tokens`를 노출하지 않는다.
upstream ephemera `v0.3.1`의 `agent_tokens` 응답 추가는 anvil 보안 불변 조건에 맞춰
downstream에서 채택하지 않는다.

MCP adapter는 얇은 runtime bridge다. 현재 workspace copy는 VM 내부
`/workspace` 기준 단일 file copy-in/copy-out만 지원한다. 기본 encoding은
`text`이고, binary payload는 `encoding: "base64"`로 전달한다. 단일 파일 크기는
4 MiB로 제한하며, copy-in은 기본적으로 기존 파일을 덮어쓰지 않는다.
`overwrite: true`를 명시해야 교체한다. directory sync, snapshot alias,
HTTP MCP transport는 제공하지 않는다. Restore 응답은 daemon direct response와
MCP output 모두 `agent_token`을 노출하지 않는다.
Restore 후 `session_name` bind가 실패하면 adapter는 restored VM을 자동 삭제하지
않고 error에 restored VM ID를 포함한다.

Multi-tenant foundation은 MCP adapter boundary에서 optional `tenant_id`와
`ANVIL_MCP_TENANT_ID` 기본값을 검증한 뒤 `POST /vms`,
`POST /vms/{id}/snapshot`, `POST /snapshots/{id}/restore` daemon body로 전달한다.
`egress_policy`는 `deny_all`, `profile`, `allow_all` 중 하나이며 VM/snapshot
metadata에 보존된다. `ANVIL_MCP_AUDIT_LOG`를 설정하면 성공/실패 tool call에 대해
tenant ID, VM ID, session alias, tool name, daemon operation, result code,
timestamp, sanitized error만 JSONL로 append한다. 이 audit record에는 snapshot
metadata, daemon raw body, `agent_token`을 저장하지 않는다.
`ANVIL_MCP_AUDIT_LOG`를 켠 상태에서는 tool input `tenant_id` 또는
`ANVIL_MCP_TENANT_ID`가 필요하다.

현재 control-plane foundation은 host inventory polling, `cmd/anvil-scheduler`,
scheduler-backed `RuntimeRouter`, JSON quota store, persistent placement/snapshot
locality store, daemon `/tenants`, `/audit/runtime`, `/health`, `/metrics`,
`/metrics/vms`를 제공한다. router는 snapshot locality preferred host, retry/failover,
placement reconciliation helper를 제공한다. `anvil_replicate_snapshot`은
RuntimeRouter가 source daemon의 `POST /snapshots/{id}/export` stream을 target
daemon의 `POST /snapshots/import`로 전달하고, 성공한 target host만 scheduler
`SnapshotLocations`에 기록한다. operator-facing response와 audit record는
`agent_token`, authorization header, daemon raw body, raw `metadata.json` body를
포함하지 않는다.

MCP production config에서 기존 VM/snapshot tool은 `ANVIL_DAEMON_URL` direct daemon
동작을 유지한다. snapshot replication과 scheduler-aware flock placement만 router를
사용하며, router는 `scheduler_state_path` 또는 `ANVIL_MCP_SCHEDULER_STATE`,
`scheduler_hosts_file` 또는 `ANVIL_MCP_SCHEDULER_HOSTS_FILE`이 설정된 경우
활성화된다. `ANVIL_MCP_SCHEDULER_STATE` 또는
`ANVIL_MCP_SCHEDULER_HOSTS_FILE`로 router config가 제공되면 `anvil_spawn_flock`은
기존 scheduler-aware single-host placement를 계속 사용한다. roles 수만큼 active VM
capacity/quota를 확인한 뒤 하나의 healthy host를 선택하고, daemon
`POST /flocks`는 그 host에서 기존 single-host 의미로 실행한다.
`ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only`와 persistent
`scheduler_state_path`가 함께 설정되면 experimental
`anvil_create_routed_flock_members`도 사용할 수 있다. 이 tool은
`POST /schedule/flock` plan으로 role별 host를 정하고, 각 host daemon의 `POST /vms`를
호출해 role VM을 생성한 뒤 member placement를 `scheduler_state_path`의 routed flock
registry에 기록한다. 반환 `mode`는 `cross_host_members_only`,
`town_wall_enabled=false`이며 `townwall_url`과 `post_url`은 없다. 이 첫 slice는
Town Wall, cross-host `gtcall`, guest flock context injection을 지원하지 않는다.
`scheduler_quota_store_path` 또는 `ANVIL_MCP_SCHEDULER_QUOTA_STORE`는 scheduler quota
store를 함께 지정할 때 사용한다. host daemon client 인증에는 `ANVIL_API_TOKEN`을
사용한다.

MCP router 관련 설정:

| 설정 | 의미 |
|---|---|
| `scheduler_state_path` / `ANVIL_MCP_SCHEDULER_STATE` | router placement, snapshot locality, routed flock registry를 저장하는 persistent JSON path |
| `scheduler_hosts_file` / `ANVIL_MCP_SCHEDULER_HOSTS_FILE` | router가 사용할 runtime host inventory JSON path |
| `scheduler_quota_store_path` / `ANVIL_MCP_SCHEDULER_QUOTA_STORE` | optional tenant quota JSON path |
| `cross_host_flock_create_mode` / `ANVIL_MCP_CROSS_HOST_FLOCK_CREATE=members_only` | members-only routed flock create opt-in. persistent `scheduler_state_path`가 필요하며 기본 `anvil_spawn_flock`을 대체하지 않는다 |

예시:

```bash
anvil_replicate_snapshot \
  snapshot_id=snap-1 \
  source_host=host-a \
  target_host=host-b \
  include_dependencies=true
```

Scheduler service를 별도 process로 실행할 때는 다음 환경 변수를 사용한다.

```bash
go build -o anvil-scheduler ./cmd/anvil-scheduler

ANVIL_SCHEDULER_ADDR=127.0.0.1:3010 \
ANVIL_SCHEDULER_STATE=/var/lib/anvil/scheduler.json \
ANVIL_SCHEDULER_QUOTA_STORE=/var/lib/anvil/tenants.json \
ANVIL_SCHEDULER_POLL_INTERVAL=10s \
ANVIL_SCHEDULER_RECONCILE_INTERVAL=30s \
ANVIL_SCHEDULER_FAILURE_THRESHOLD=3 \
./anvil-scheduler
```

control loop과 host bootstrap 관련 scheduler 환경 변수:

| Env var | 목적 |
|---|---|
| `ANVIL_SCHEDULER_HOSTS_FILE` | config-managed runtime host bootstrap JSON 파일. 설정한 파일은 존재해야 한다 |
| `ANVIL_SCHEDULER_POLL_INTERVAL` | host observation poll 주기 |
| `ANVIL_SCHEDULER_RECONCILE_INTERVAL` | placement reconciliation 주기 |
| `ANVIL_SCHEDULER_HOST_TIMEOUT` | daemon `/health`, `/vms` 요청별 timeout |
| `ANVIL_SCHEDULER_FAILURE_THRESHOLD` | host를 `unhealthy`로 전이하기 전 연속 실패 횟수 |
| `ANVIL_SCHEDULER_API_TOKEN` | ephemera daemon에 전달할 bearer token |
| `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE` | `true`면 scheduler state 저장 장애 중 신규 scheduling을 503으로 차단 |

현재 daemon `/health`는 scheduler capacity 필드를 제공하지 않으므로 hosts file에서
`available_vms`, `available_snapshot_bytes`, `egress_policies`를 함께 지정한다.

`--verify`와 standalone smoke harness는 host에 `curl`, `python3`가 있어야
실행된다. systemd 설치 host에서는 dry-run, start, smoke verify를 같은 installer로
수행한다.

```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --verify
sudo bash scripts/install-anvil-scheduler-systemd.sh --start --verify
```

이미 실행 중인 scheduler만 확인할 때는 standalone smoke harness를 사용한다.

```bash
bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010
```

smoke harness는 기본 host id를 `anvil-scheduler-smoke-*`로 생성하고,
`GET /hosts`로 같은 host id의 기존 inventory record가 없는지 먼저 확인한다.
충돌이 있으면 `PUT /hosts`를 실행하기 전에 실패하므로 운영 host record를 덮어쓰지
않는다. 등록한 fake host는 `DELETE /hosts/{name}`로 제거하며 cleanup 실패는 smoke
성공으로 취급하지 않는다. smoke harness가 등록하는 fake host는 `smoke_only: true`이며,
`PreferredHosts`에 해당 host id를 명시한 smoke 요청에서만 선택된다. smoke harness는
`PreferredHosts`가 없는 추가 `/schedule/spawn`도 실행해 smoke-only host가 일반
fallback placement 후보로 선택되지 않는지 확인한다. `POST /schedule/flock`도
dry-run으로 호출해 planner response가 `agents`, `requested`,
`host_status_summary` 계약을 유지하는지 확인한다.

`POST /schedule/flock`을 수동 확인할 때는 다음처럼 dry-run 요청을 보낸다. 이 요청은
VM을 생성하지 않는다.

```bash
curl -sS -X POST http://127.0.0.1:3010/schedule/flock \
  -H 'Content-Type: application/json' \
  --data '{"tenant_id":"tenant-1","egress_policy":"profile","roles":["worker","reviewer"]}'
```

Scheduler service API는 operator가 host inventory와 placement 상태를 관리하는
얇은 control-plane surface다.

| Endpoint | 목적 |
|---|---|
| `GET /health` | scheduler process 상태 확인 |
| `GET/PUT /hosts` | runtime host inventory 조회/등록 |
| `DELETE /hosts/{name}` | smoke/운영 정리용 runtime host inventory 제거. 없는 host 삭제는 idempotent success로 처리 |
| `GET /placements` | host, VM placement, snapshot location state 조회 |
| `GET /control-loop/status` | scheduler control loop 실행 상태, host observation, degraded/unhealthy 판단 조회 |
| `POST /reconcile` | 현재 placement state 반환. router reconciliation은 daemon `GET /vms` 기반 helper가 수행 |
| `POST /schedule/spawn` | spawn 요청의 host decision 반환 |
| `POST /schedule/flock` | flock roles를 host별 agent placement plan으로 dry-run한다. VM은 생성하지 않는다. |
| `POST /schedule/restore?snapshot_id=...` | snapshot locality를 반영한 restore host decision 반환 |

Scheduler service는 operator JSON endpoint와 별도로 scheduler 전용 Prometheus text
`GET /metrics`를 제공한다. 이 endpoint는 daemon `/metrics`와 다른 surface이며
`anvil_scheduler_*` namespace로 control loop running flag, persistence degraded
flag, host status count, suspect placement count, last poll/reconcile timestamp를
반환한다. scheduler에는 자체 인증 계층이 없으므로 기존 scheduler 운영 경계처럼
loopback/private network 또는 reverse proxy policy 뒤에서만 노출한다.

`deny_all` egress policy는 host `iptables` reject rule로 강제한다. `profile` policy는
`configs/profiles/{profile}/egress.json`,
`EPHEMERA_EGRESS_PROFILE_DIR`, `ANVIL_EGRESS_PROFILE_DIR` 아래의 profile별
`egress.json`이 있을 때 allow CIDR/host/DNS rule을 적용하고, policy 파일이 없으면
기존 profile 호환성을 위해 no-op이다. 예시:

```json
{
  "allow_cidrs": ["203.0.113.10/32"],
  "allow_hosts": ["api.anthropic.com"],
  "dns_servers": ["1.1.1.1"]
}
```

Optional trace export는 `ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT` 또는
`OTEL_EXPORTER_OTLP_ENDPOINT`를 설정하면 lifecycle span을 `{endpoint}/v1/traces`로
보낸다. trace attribute는 token/secret 계열 값을 제거한 뒤 전송한다.

정확한 입력/출력 계약은 `docs/architecture/mcp-architecture.md`를 참조한다.

문서 기준 MCP smoke test는 실제 daemon과 `anvil-mcp` stdio server를 함께
사용한다. 일반 CI에서는 KVM/root가 필요한 daemon 실행을 요구하지 않고
`go test ./...`, `go build ./cmd/anvil-mcp`, `go build ./cmd/anvil-scheduler` 같은
CI-safe 검증만 수행한다.
MCP smoke는 Firecracker를 실행할 수 있는 host에서 별도로 수행한다.

먼저 root 권한으로 daemon을 실행한다.

```bash
sudo ANVIL_API_ADDR=127.0.0.1:3000 ./anvil-daemon
```

다른 터미널에서 smoke wrapper를 실행한다. wrapper는
`go build -o /tmp/anvil-mcp ./cmd/anvil-mcp`로 adapter를 빌드한 뒤 smoke client가
해당 binary를 stdio MCP server로 실행하게 한다.

```bash
scripts/anvil-mcp-e2e.sh lifecycle
scripts/anvil-mcp-e2e.sh semantic
scripts/anvil-mcp-e2e.sh flock
```

`lifecycle`은 기본 모드이며 내부적으로
`go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -expect-output ""`를
실행한다. `semantic`은
`go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -expect-output "anvil-smoke-ok"`를
실행한다.

두 모드 모두 `anvil_spawn_vm`, `anvil_copy_in`, `anvil_copy_out`,
`anvil_run_task`, `anvil_get_vm_health`, `anvil_stop_vm`,
`anvil_delete_vm` 순서로 tool call을 수행한다. `lifecycle`은 workspace copy
round-trip과 VM cleanup 경로를 확인하되 `anvil_run_task` 응답 body의 의미적
marker는 검사하지 않는다. `semantic`은 같은 flow에 더해 `anvil-smoke-ok`
포함 여부를 확인한다.

`flock` 모드는 daemon이 이미 실행 중인 상태에서 MCP를 통해
`anvil_spawn_flock`, `anvil_list_flocks`, `anvil_post_townwall`,
`anvil_get_townwall_history`, `anvil_delete_flock` 경로를 확인한다. Town Wall
SSE stream은 MCP smoke 대상이 아니며, history 조회로 stdio-compatible inspection을
수행한다.

daemon은 smoke 실행 전에 이미 떠 있어야 하며 `ANVIL_DAEMON_URL`과 필요한 경우
`ANVIL_API_TOKEN`으로 adapter가 daemon에 도달할 수 있어야 한다. daemon 실행에는
`/dev/kvm`, root 권한, Firecracker 실행 가능 host가 필요하다. `semantic`은 유효한
LLM credential과 provider 응답까지 요구한다. `lifecycle`은 의미적 marker 검사만
끄므로, 선택한 daemon/profile의 `anvil_run_task` 경로가 2xx로 완료될 수 있어야
한다.

---

## API 참조

token이 설정되어 있으면 모든 control-plane endpoint는
`Authorization: Bearer <token>`을 요구한다.

### VM 생성

```text
POST /vms
Content-Type: application/json

{ "profile": "anthropic", "tenant_id": "tenant.alpha", "egress_policy": "profile" }
```

`profile`을 생략하면 기본 `configs/goose.yaml`과
`configs/goose-secrets.yaml`을 사용한다. `tenant_id`와 `egress_policy`는 optional
계약이다. `tenant_id`는 ASCII letter/digit으로 시작해야 하며 letter, digit, `.`,
`_`, `-`만 허용한다. `egress_policy`는 `deny_all`, `profile`, `allow_all` 중 하나다.

```bash
curl -X POST http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"profile": "anthropic", "tenant_id": "tenant.alpha", "egress_policy": "profile"}'
```

```json
{
  "vm_id": "vm-1778227813435",
  "guest_ip": "10.0.1.10",
  "agent_url": "http://10.0.1.10:8080",
  "profile": "anthropic",
  "tenant_id": "tenant.alpha",
  "egress_policy": "profile",
  "agent_token": "3f9a2c..."
}
```

보안 불변 조건은 `POST /vms` 외 응답에서 `agent_token`을 노출하지 않는 것이다.
daemon의 `POST /snapshots/{id}/restore`, MCP output, audit record는
`agent_token`을 노출하지 않는다.

### VM 목록

```bash
curl http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN"
```

### VM 삭제

```bash
curl -X DELETE http://localhost:3000/vms/vm-1778227813435 \
  -H "Authorization: Bearer $TOKEN"
```

### Snapshot 생성

```text
POST /vms/{vm_id}/snapshot
Content-Type: application/json

{
  "stop_after": false,
  "type": ""
}
```

`type`이 비어 있으면 자동 선택한다.

| 조건 | 결과 |
|---|---|
| 해당 VM의 기존 Full snapshot 없음 | `full` |
| 해당 VM의 기존 Full snapshot 있음 | `diff` |

```bash
curl -X POST http://localhost:3000/vms/vm-1778227813435/snapshot \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json"
```

### Snapshot 목록

```bash
curl http://localhost:3000/snapshots \
  -H "Authorization: Bearer $TOKEN"
```

### Snapshot bundle export/import

```text
POST /snapshots/{id}/export       -> snapshot bundle export
POST /snapshots/import            -> snapshot bundle import
```

`POST /snapshots/{id}/export`는 `application/vnd.anvil.snapshot-bundle` content type의
streamable bundle을 반환한다. `POST /snapshots/import`는 target host에서 bundle을
staging, validation, atomic publish 순서로 반입한다. cross-host 복제는 MCP
`anvil_replicate_snapshot`을 통해 source export stream을 target import로 전달한다.
diff snapshot 복제 시 `include_dependencies=true`를 사용하면 base full snapshot을
먼저 복제하고 diff를 복제한다.

### Snapshot 복원

```bash
curl -X POST http://localhost:3000/snapshots/snap-1778229000000/restore \
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
  "mem_total_mib": 2048,
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
| `ephemera_flock_spawn_total` / `_destroy_total` | counter | — | success path of `createFlock` / `deleteFlock` |
| `ephemera_watchdog_dead_total` / `_heal_total` | counter | — | dyingThreshold and autoHeal transitions |
| `ephemera_sighup_reload_total` | counter | — | after `ReloadClients` completes |
| `ephemera_cp_token_propagated_total` | counter | `outcome` | per-VM vsock fan-out result |
| `ephemera_vm_count` / `_flock_count` / `_snapshot_count` / `_api_clients_count` | gauge | — | re-read on each scrape (GaugeFunc) |
| `ephemera_vm_spawn_duration_seconds` | histogram | — | wall-clock spawn time |
| `ephemera_snapshot_restore_duration_seconds` | histogram | — | wall-clock restore time |
| `ephemera_watchdog_probe_duration_seconds` | histogram | — | per-probe `/health` duration |

---

### Agent Proxy (via Control Plane)

The control plane proxies the three agent endpoints, making them accessible to external clients without direct access to the private VM subnet. Authentication uses the **control plane Bearer token** — the agent token is injected internally.

```
POST /vms/{vm_id}/tasks    → proxied to goose-agent /tasks
GET  /vms/{vm_id}/health   → proxied to goose-agent /health  (no auth required)
POST /vms/{vm_id}/stop     → proxied to goose-agent /stop
```

`POST /vms/{vm_id}/tasks`의 response shape는 `{ "output", "error" }`를 유지한다.
`v0.3.6` baseline부터 `output`은 Goose `--output-format json` envelope에서 추출한
assistant text다. agent는 Goose startup banner 앞부분을 건너뛰고 assistant text
block을 이어 붙이며, envelope parsing에 실패하면 raw stdout으로 fallback한다.

> The agent's `/townwall/post` is **not** proxied. It is an in-VM convenience used by the bundled `gtwall` CLI, which already has the flock context. External callers should `POST /flocks/{id}/post` directly — they already know the flock ID and can pick the `agent_id` themselves.

When `EPHEMERA_PUBLIC_URL` is configured, `agent_url` in VM responses points directly to the proxy base (`{EPHEMERA_PUBLIC_URL}/vms/{vm_id}`), so clients can use it as-is:

```bash
export EPHEMERA_PUBLIC_URL=https://api.example.com
# agent_url in POST /vms response will be: https://api.example.com/vms/vm-...

curl -X POST "$AGENT_URL/tasks" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tenant_id": "tenant.alpha", "egress_policy": "profile"}'
```

source VM이 아직 실행 중이면 restore는 거부된다. restore된 VM은 새 VM ID와
새 IP를 받지만 snapshot의 agent token은 daemon 내부 proxy용으로만 유지한다.
restore request의 `tenant_id` 또는 `egress_policy`가 snapshot metadata와 충돌하면
daemon은 restore를 거부한다. restore success response에는 `agent_token`이 없다.

snapshot `metadata.json`은 restore 인증 계약을 보존하기 위해 `agent_token`을
담고 있다. metadata를 반출하거나 백업 산출물이 신뢰된 host 경계 밖으로 나가기
전에는 scrubber로 token을 제거한다.
단, `POST /snapshots/{id}/export` replication bundle 안의 `metadata.json`은 raw local
metadata file이 아니라 `agent_token`을 제거한 portable copy다. Firecracker restore
state가 요구하는 `disk_path`, `vsock_path`는 safe workspace/tmp path인지 import 때
검증한 뒤 보존하고, target daemon은 `mem_file_path`, `state_file_path`,
`disk_copy_path`를 target snapshot directory로 다시 기록한다. 복제된 snapshot을
restore할 때는 target daemon이 새 `agent_token`을 생성해 vsock control channel로
guest agent에 주입하므로 source host의 token을 bundle에 싣지 않는다.

```bash
go run ./scripts/snapshot-metadata-scrub.go -input snapshots/snap-1778229000000/metadata.json > metadata.scrubbed.json
```

restore 실패는 JSON error body를 반환한다.

```json
{
  "error": "snapshot not found",
  "code": "snapshot_not_found",
  "source_snapshot_id": "snap-1778229000000"
}
```

`code`는 안정적인 machine-readable 값이다.

| code | 의미 |
|---|---|
| `snapshot_not_found` | 요청한 snapshot metadata가 없다 |
| `source_vm_running` | source VM이 아직 실행 중이라 restore할 수 없다 |
| `network_unavailable` | restore용 TAP/IP allocation에 실패했다 |
| `diff_base_missing` | diff snapshot의 base full snapshot이 없다 |
| `memory_merge_failed` | diff memory merge에 실패했다 |
| `firecracker_restore_failed` | disk setup 또는 Firecracker restore에 실패했다 |
| `guest_reconfigure_failed` | restore 후 guest IP 재설정에 실패했다 |
| `agent_not_ready` | restore된 VM의 `goose-agent` health 대기가 실패했다 |

현재 snapshot lifecycle은 보수적으로 직렬화되어 하나의 create/restore/delete/GC
lifecycle operation만 동시에 실행된다.

### Snapshot 삭제

```bash
curl -X DELETE http://localhost:3000/snapshots/snap-1778229000000 \
  -H "Authorization: Bearer $TOKEN"
```

diff snapshot이 참조 중인 full snapshot은 삭제할 수 없다.

### Snapshot GC dry-run/apply

`POST /snapshots/gc`는 snapshot retention plan을 계산한다. 기본값은 dry-run이며
파일을 삭제하지 않는다. `older_than_seconds`와 `keep_last_per_vm`에 더해
`max_total_bytes`를 지정할 수 있다. `max_total_bytes` 기본값 `0`은 비활성화이며,
양수이면 모든 snapshot directory의 apparent file size를 합산한 뒤 projected
remaining total이 한도 이하가 될 때까지 보호되지 않은 snapshot을 오래된 순서로
추가 후보에 넣는다.

```bash
curl -X POST http://localhost:3000/snapshots/gc \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $EPHEMERA_API_TOKEN" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"max_total_bytes":10737418240}'
```

실제 삭제는 `apply: true`를 명시해야 수행된다.

```bash
curl -X POST http://localhost:3000/snapshots/gc \
  -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $EPHEMERA_API_TOKEN" \
  -d '{"older_than_seconds":604800,"keep_last_per_vm":1,"max_total_bytes":10737418240,"apply":true}'
```

diff snapshot이 참조 중인 full snapshot은 항상 보호된다. full과 diff가 모두 오래된
경우 첫 GC apply에서는 diff만 삭제되고, 다음 GC 호출에서 full이 삭제 후보가 된다.
`candidates`, `protected`, `deleted` entry는 계산 가능한 경우 `size_bytes`를 포함한다.
`max_total_bytes` 때문에 추가된 후보의 `reason`은 `max_total_bytes`다. `apply: true`
호출은 삭제 시도 후 `snapshots/gc-audit.jsonl`에 JSONL audit record를 1줄 append한다.
audit record에는 timestamp, applied, policy, candidates/deleted/errors count만 들어가며
snapshot metadata나 `agent_token`은 기록하지 않는다. dry-run은 audit record를 쓰지
않는다.

### Agent proxy 사용

```bash
curl http://localhost:3000/vms/$VM_ID/health \
  -H "Authorization: Bearer $TOKEN"

curl -X POST http://localhost:3000/vms/$VM_ID/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"hello from inside the VM"}'

curl -X POST http://localhost:3000/vms/$VM_ID/stop \
  -H "Authorization: Bearer $TOKEN"
```

외부 client는 control-plane token만 사용한다. daemon이 guest agent token을
내부적으로 주입한다.

#### Restart a single agent (v0.3.3)

```bash
curl -X POST http://localhost:3000/flocks/$FLOCK_ID/agents/reviewer-1/restart \
  -H "Authorization: Bearer $TOKEN"
# {"vm_id":"vm-...","guest_ip":"10.0.1.7","agent_url":"http://10.0.1.7:8080","profile":"reviewer"}
```

Tears down the named agent's VM and respawns it with the same `agent_id`, role, and `agent_token` (callers that cached the token keep working). The new VM has a fresh `vm_id` / `guest_ip` / `agent_url`; the agent status resets to `ready`. On spawn failure the agent is left `Status=dead` and persisted, so callers see the truth without needing to poll.

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

---

## 보안 모델

- **client -> control plane**:
  `EPHEMERA_API_TOKENS`/`EPHEMERA_API_TOKEN` 또는
  `ANVIL_API_TOKENS`/`ANVIL_API_TOKEN` Bearer token을 사용한다.

- **control plane -> guest agent**:
  VM별 Bearer token을 사용한다.

- **guest task isolation**:
  Firecracker MicroVM + KVM boundary로 격리한다.

- **guest network**:
  host-only `10.0.1.0/24` network와 `goose-br0` bridge를 사용한다.

- **외부 공개**:
  TLS 종료 reverse proxy 뒤에서 운영하고 운영 환경에서는 `EPHEMERA_API_TOKENS`를
  설정한다. 자세한 정책은
  [docs/operations/security-policy.md](docs/operations/security-policy.md)를 참조한다.

- **secret**:
  gitignore된 로컬 config에서 guest disk로 주입한다.

실제 API key는 문서, issue, commit, 채팅에 남기지 않는다.

---

## 알려진 제약

- snapshot create/restore/delete/GC lifecycle operation은 한 번에 하나만 실행된다.
- source VM이 실행 중인 동안 해당 VM의 snapshot restore는 거부된다.
- diff snapshot은 memory만 diff다. rootfs는 snapshot마다 full copy다.
- diff restore는 임시 merged memory file을 만들 disk space가 필요하다.
- daemon restart 후 spawn-path VM은 `vms/<vm_id>/state.json` 기반으로 cold-restart된다.
  같은 VM ID, IP, TAP, MAC, agent token, agent URL을 유지하지만 memory state와
  in-flight task는 보존하지 않는다.
- COW-mode VM과 snapshot-restored VM은 daemon restart 후 자동 복구 대상이 아니다.
- watchdog이 표시한 `dead` status는 `flocks/<flock_id>/metadata.json`에
  persist된다. per-agent restart 또는 watchdog auto-heal opt-in이 상태를 다시
  `ready`로 바꾸는 명시 경로다.
- control-plane auth가 켜진 flock VM은 host가 `/root/.ephemera-cp-token`을
  자동 주입한다. v0.3.4 이상에서는 `EPHEMERA_API_TOKENS_FILE` + `SIGHUP`으로
  running VM에도 token rotation을 전파한다.
- control-plane token 환경 변수를 설정하지 않으면 API 인증이 비활성화된다.
- MCP v1은 snapshot/restore tool을 제공하지만 snapshot alias와 session alias
  영속화는 별개다. snapshot alias는 제공하지 않고, VM `session_name` alias는
  `session_store_path` 또는 `ANVIL_MCP_SESSION_STORE`가 설정된 경우에만 local JSON
  file로 영속화한다.

## Control plane API authentication

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

`ReloadClients` re-reads the file, swaps the in-memory client list under `clientsMu`, **and (v0.3.4) fans the new `apiClients[0].Token` out to every running flock VM over vsock** (`SET_CP_TOKEN` command, atomic rewrite of `/root/.ephemera-cp-token`). The in-VM `/townwall/post` forwarder picks up the rotated bearer on the next request without any VM restart. See [CP token rotation via vsock](#cp-token-rotation-via-vsock-v034).

| Scenario | Action |
|----------|--------|
| Adding a new client | Edit `EPHEMERA_API_TOKENS_FILE` → SIGHUP |
| Rotating `apiClients[0]` (the CP token VMs use) | Edit file → SIGHUP; in-VM `/root/.ephemera-cp-token` is updated automatically (v0.3.4+) |
| Emergency revocation | Edit file → SIGHUP — **no VM interruption** |
| Legacy `EPHEMERA_API_TOKENS` env (no file) | Still works for the `cp.clients` swap, but does not see env-value changes without daemon restart. Use `_TOKENS_FILE` for live rotation. |

---

### goose-agent authentication

Each VM's agent is protected by a unique 32-byte random Bearer token generated at spawn time and written to `/root/.ephemera-agent-token` (mode `0600`) inside the VM disk. The token is returned only in the `POST /vms` response; snapshot restore responses do not expose it.

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
3. **Cold boot** — Firecracker is restarted against the same rootfs clone; `goose-agent` is waited for on `/health` up to 60 s.
4. **Flock association** — if the VM belonged to a flock, the agent's status is flipped back to `"ready"`. If recovery fails, the agent is marked `"dead"` and a `<orchestrator>` notice is posted to the Town Wall.

The daemon-side shutdown path is designed to feed cold-restart:

- **Graceful shutdown (SIGTERM/SIGINT)** — `ControlPlane.DestroyAll` stops every Firecracker process via `StopVMM`, releases TAP/IP/vsock/socket, and **preserves each VM's rootfs ext4 and `state.json`**. The next daemon start cold-restarts them.
- **Explicit `DELETE /vms/{id}`** — routes through `destroyVM`, which does a full cleanup (deletes `state.json`, removes the rootfs ext4, releases all resources). The VM is gone and is not cold-restarted.
- **SIGKILL / crash** — defers don't run. `state.json` + rootfs survive on disk; on the next start, cold-restart picks them up exactly as for graceful shutdown.
- **COW-restored and snapshot-restored VMs** — torn down fully during `DestroyAll` (dm-snapshot devices / bind mounts would leak kernel resources otherwise); their `state.json` is removed so the next start does not attempt to recover them.

What this preserves:

| Preserved | Lost |
|-----------|------|
| `vm_id`, `guest_ip`, `tap_device`, `mac_addr` | In-flight `/tasks` work (memory is not snapshotted) |
| `agent_token`, `agent_url` | Goose conversation context (in-VM, in-memory) |
| Disk contents (the rootfs clone is reused, not recreated) | `runningVM.dmSnapshot` info (COW-mode VMs are not auto-recovered) |
| Flock membership, Town Wall history | (none) |
| Watchdog `status=dead` markings (v0.3.3 — persisted to `metadata.json`) | |

Callers that need at-most-once semantics across daemon restarts should idempotency-key their `/tasks` calls or poll for completion before retrying.

**Out of scope for v0.3.2**:
- VMs spawned with `EPHEMERA_DISK_MODE=cow` skip recovery (logged on startup); they require dm-snapshot orphan cleanup that is deferred to a later release.
- Snapshot-restored VMs (`POST /snapshots/{id}/restore`) are not auto-recovered — restore from the snapshot again after the daemon comes back.

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

- `ControlPlane.controlPlaneTokenForVM()` returns `apiClients[0].Token` under `clientsMu` (so SIGHUP-driven `ReloadClients` stays safe). Empty when auth is disabled.
- `spawnVMForFlock` (and `restartAgent`) pass the token through `spawnVMOptions.ControlPlaneToken` → `VMPrepareOptions.ControlPlaneToken` → `injectVMFiles`, which writes it to `/root/.ephemera-cp-token` at mode 0600. Standalone `POST /vms` does NOT inject it because non-flock VMs do not use `/townwall/post`.
- `goose-agent`'s `loadCPToken` prefers the file and falls back to the legacy `EPHEMERA_CONTROL_PLANE_TOKEN` env var for older golden images.

This removes the per-VM operator burden documented in earlier releases. v0.3.4 adds true hot rotation on top — see [CP token rotation via vsock](#cp-token-rotation-via-vsock-v034) below.

### CP token rotation via vsock (v0.3.4)

When you want to rotate the control-plane bearer without restarting either the daemon or any VMs:

1. Run the daemon with `EPHEMERA_API_TOKENS_FILE=/etc/ephemera/tokens` (one `name:token` entry per line — comma-separated also works). The file source takes precedence over `EPHEMERA_API_TOKENS` env when set; both legacy env paths remain as fallback.
2. Edit the file (operator action).
3. `pkill -HUP ephemera-daemon`. `ReloadClients` re-reads the file (env values are fixed at exec, the file is not), hot-swaps `cp.clients` under `clientsMu`, and fans the new `apiClients[0].Token` out to every running flock VM over the existing vsock channel.

In-VM side, `goose-agent`'s vsock listener now dispatches both `CHANGE_IP` (used since v0.2.0 for snapshot-restore IP plumbing) and the new `SET_CP_TOKEN <token>` command, which atomically rewrites `/root/.ephemera-cp-token` (tmp + rename, mode 0600). The `/townwall/post` handler re-reads the file on every request, so the next forwarder call sees the new bearer.

The fan-out is **best-effort**: each VM gets ~4 s (20 attempts × 200 ms, matching the existing `ReconfigureGuestIP` budget) and any per-VM failure is logged but never propagated. The SIGHUP path therefore completes in bounded time regardless of unresponsive VMs. A final log line summarizes results:

```
SIGHUP: token reload complete — 1 client(s): alice
SIGHUP: CP token propagated to 3/3 VM(s)
```

**SDK signal forwarding** — `firecracker-go-sdk` v1.0.0 defaults to forwarding `SIGINT/SIGQUIT/SIGTERM/SIGHUP/SIGABRT` from the daemon to every Firecracker child (see `internal/vm/machine.go`'s `setupSignals` reference). The daemon explicitly narrows `firecracker.Config.ForwardSignals` to `SIGQUIT` and `SIGABRT` only. `SIGHUP` is owned by the token-reload + vsock fan-out flow described here; forwarding it would kill every running Firecracker and the fan-out would immediately get `connection refused`. `SIGINT` and `SIGTERM` are also daemon-owned: `Ctrl-C` / `systemctl stop` enter the daemon's graceful teardown and auto-snapshot path, which then stops each child explicitly. `SIGQUIT` and `SIGABRT` stay forwarded because the daemon does not trap those abnormal exits, so forwarding them reduces orphaned Firecracker children.

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

> **Recovery scope (v0.3.2)**: flock metadata is restored here; the live VMs are brought back via the cold-restart path described above. After daemon restart, recovered flocks are fully interactive (`/tasks`, `/stop`, `/post`, `/wall`, `DELETE` all work), with the caveat that in-VM memory state is lost — agents resume from a fresh boot, not from where they left off.

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
| **Single-host VM runtime** | VM 실행 자체는 host-local daemon이 소유한다. Cross-host snapshot replication은 MCP router와 scheduler state를 통해 수동 운영 workflow로 지원한다. |
| **Same-snapshot concurrent restores not supported** | The guest IP is reconfigured via vsock after restore, so different-snapshot concurrent restores each get a fresh IP. However, two VMs from the *same* snapshot would still collide on the Firecracker vsock UDS path (which is fixed in `state.bin`), so same-snapshot concurrent restores are not supported. |
| **Cross-machine restore** | `anvil_replicate_snapshot`으로 target host에 snapshot bundle을 import한 뒤 `POST /snapshots/{id}/restore`를 호출한다. diff snapshot은 target에 base full snapshot이 필요하며 `include_dependencies=true`가 base를 먼저 복제한다. |
| **Cold-restart loses in-VM memory** (v0.3.2) | Live VM auto-restart re-boots each VM from its rootfs clone; the guest kernel and `goose-agent` start fresh. Any `/tasks` request in flight at the moment of daemon shutdown is dropped. Callers should idempotency-key tasks or re-poll for completion across a restart. |
| **COW-mode VMs are not auto-recovered** (v0.3.2) | VMs spawned with `EPHEMERA_DISK_MODE=cow` are skipped during cold-restart (logged on startup). dm-snapshot orphan cleanup is deferred. Workaround: re-spawn the agent if you depend on it. |
| **Snapshot-restored VMs are not auto-recovered** (v0.3.2) | Only spawn-path VMs are cold-restarted. After a daemon restart, call `POST /snapshots/{id}/restore` again to bring back a snapshot-derived VM. |
| **CP token rotation needs v0.3.4 VMs and `_TOKENS_FILE`** (updated v0.3.4) | v0.3.4 hot-propagates the new `apiClients[0].Token` to running VMs via vsock on SIGHUP. Two prerequisites: (a) the daemon must source tokens from `EPHEMERA_API_TOKENS_FILE` rather than env (env values are fixed at exec time and cannot change on SIGHUP); (b) the VMs must run a v0.3.4+ `goose-agent` (older ones lack the `SET_CP_TOKEN` vsock handler). When either is missing the v0.3.3 fallback (`POST /flocks/{id}/agents/{agent_id}/restart`) still works. |
| **Metrics retention is external** (v0.3.5) | `/metrics` exposes raw counters and gauges only — the daemon does not aggregate, store, or rotate history. Operators are expected to wire an external Prometheus (or any text-exposition-compatible) scraper. |

---

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for development setup, test gates, configuration / secrets handling, PR expectations, and the areas of code that need extra care (KVM, networking, snapshots, golden image bake, in-VM auth).

---

## License

MIT — see [LICENSE](LICENSE).
