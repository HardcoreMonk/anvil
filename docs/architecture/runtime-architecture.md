# ephemera 런타임 아키텍처

## 상태

- 기준 버전: upstream ephemera `v0.7.0` + anvil runtime control-plane updates
- anvil 관점: ephemera runtime은 IronClaw 결합 프로젝트의 기반 실행 계층
- upstream: `https://github.com/steve-seungeui/ephemera`. anvil fork network를
  유지하며 ephemera runtime version을 merge로 반영한다.
- upstream `v0.3.2`-`v0.7.0`는 anvil main runtime baseline으로 반영되어 있다. `v0.3.2`는
  cold-restart, `v0.3.3`은 watchdog/restart/CP-token polish, `v0.3.4`는 token
  hot rotation과 watchdog tunable, `v0.3.5`는 metrics/stats/slog 관측성,
  `v0.3.6`은 in-VM `gtcall`, multi-line-safe `gtwall`, Goose JSON output parsing,
  autonomous webdev demo를 제공한다. `v0.4.0`-`v0.4.3`은 memory auto-snapshot,
  diff/COW rootfs, auth/audit, COW spawn, dynamic flock lifecycle, pause/resume,
  `max_agents`, Town Wall filter/rotation을 제공한다. `v0.4.4`는 streaming
  `/tasks?stream=1`(buffered 기본 계약 유지), nested task depth guard
  (`EPHEMERA_MAX_TASK_DEPTH`/`508`), read-only `GET /watchdog/status`, daemon-only
  flock broadcast를 제공하고, `v0.4.5`는 snapshot-restore auto-recovery
  (`recoverRestoredVM`/`reRestoreMachine`)를 제공한다. `v0.5.0`-`v0.5.5`는 operator
  Web UI(`/ui/`, embedded `cmd/goose-daemon/uidist/`), `/config/*` profile/provider/
  client/system-prompt surface, per-VM sizing(`EPHEMERA_VCPU_COUNT`/
  `EPHEMERA_MEM_SIZE_MIB`, default `1` vCPU / `1024` MiB), `SystemAuthor` migration을
  제공한다. `v0.6.0`-`v0.6.4`는 runtime MCP Gateway(`internal/mcpgateway`,
  `EPHEMERA_MCP_*`, `configs/mcp/*`, daemon `/config/mcp*` handler, Web UI MCP console,
  anti-spoof·rate-limit·stdio backend)를 제공한다. Web UI, `/config/*`, runtime MCP
  Gateway는 모두 runtime/operator surface이고 IronClaw MCP surface가 아니며, runtime
  MCP Gateway(`EPHEMERA_MCP_*`)는 `cmd/anvil-mcp` IronClaw adapter(`ANVIL_MCP_*`)를
  대체하지 않는다. gateway 런타임 동작은 [service-logic.md](service-logic.md)에 정리한다.
  `v0.7.0`은 end-user installer(`install.sh`/`uninstall.sh`/`ephemera.service.in`,
  release build `scripts/build_release.sh`)와 conversation transcript restore(daemon
  proxy `GET /vms/{id}/sessions/{name}/transcript`)를 제공한다. installer/transcript도
  runtime/operator surface이며, systemd service는 canonical `ephemera` 이름을 유지한다
  (anvil alias wrapper 없음). 사전 hardening backport(kernel SHA atomic 검증,
  `resolveWorkDir`/`EPHEMERA_HOME`, `waitForAgent` per-probe)는 v0.7.0 reconcile에서
  single definition으로 남았다. 이로써 upstream parity scope(`v0.4.0`-`v0.7.0`) 코드
  편입이 완료됐다.
- 저장소/모듈 이름: `ephemera`
- 런타임 소유 파일:
  - `cmd/goose-daemon/`
  - `cmd/goose-agent/`
  - `cmd/micro-init/`
  - `internal/storage/`
  - `internal/network/`
  - `internal/vm/`

이 문서는 ephemera runtime 구조와 anvil runtime scheduler service의 host-side
상태를 설명한다. 요청별 서비스 흐름은 [service-logic.md](service-logic.md),
IronClaw MCP 연동은 [mcp-architecture.md](mcp-architecture.md)에 정리한다.

## 시스템 관점

```text
외부 client
  |
  | HTTPS, 외부 공개 시 TLS proxy 사용
  v
Reverse proxy
  |
  | HTTP + control-plane Bearer token
  v
ephemera control plane daemon :3000
  |
  | Firecracker SDK, KVM, TAP, rootfs, snapshot files
  v
Firecracker MicroVM
  |
  | PID 1
  v
micro-init
  |
  | 시작 및 감시
  v
goose-agent :8080
  |
  | 실행
  v
goose CLI task
```

control plane은 host 쪽의 단일 런타임 조정자다. VM lifecycle, network
allocation, disk preparation, snapshot metadata, snapshot restore, guest
agent proxy를 모두 소유한다.

daemon은 기본적으로 process current working directory를 runtime work directory로
사용한다. `EPHEMERA_HOME`이 설정되면 이 값을 기준으로 `artifacts/`, `configs/`,
`snapshots/` 경로를 해석하므로 systemd 같은 supervisor에서 시작 directory에
의존하지 않는다.

## 구성 요소 책임

| 구성 요소 | 파일 | 책임 |
|---|---|---|
| Control plane daemon | `cmd/goose-daemon/main.go`, `cmd/goose-daemon/api.go`, `cmd/goose-daemon/config.go`, `cmd/goose-daemon/metrics_handler.go`, `cmd/goose-daemon/stats_handler.go`, `cmd/goose-daemon/orchestrator_api.go` | host artifact bootstrap, HTTP API 시작, client 인증, 실행 중인 VM 관리와 cold-restart, agent proxy, snapshot 생성/복원/삭제, metrics/stats, Goosetown flock/Town Wall API, flock watchdog 시작/중지 |
| Runtime scheduler service | `cmd/anvil-scheduler/main.go`, `internal/anvilmcp/scheduler_service.go` | host inventory, quota store, placement/snapshot locality state를 기준으로 runtime host schedule decision 반환 |
| Goosetown orchestrator | `internal/orchestrator/` | flock registry, agent 상태 snapshot, Town Wall append-only log와 SSE subscriber 관리, flock metadata persistence, health watchdog |
| Storage provisioner | `internal/storage/provisioner.go` | golden image build/검증, Firecracker/kernel artifact SHA256 검증, stale input 감지, VM별 disk clone, Goose config/secrets 주입, VM별 agent token 작성, timezone data 주입 |
| Snapshot storage | `internal/storage/snapshot.go` | snapshot metadata 저장, rootfs copy, COW restore device 생성/해제, diff memory snapshot merge |
| VM wrapper | `internal/vm/machine.go` | Firecracker config 구성, cold VM 시작, snapshot state restore, vsock 기반 guest IP 재설정 |
| Network manager | `internal/network/manager.go` | `goose-br0` 생성, `10.0.1.0/24` 관리, guest IP/TAP 할당과 재사용, NAT 설정 |
| Guest init | `cmd/micro-init/main.go` | guest PID 1, virtual filesystem mount, `goose-agent` 시작, 종료/신호 수신 시 clean poweroff |
| Guest agent | `cmd/goose-agent/main.go` | guest 내부 `/tasks`, `/workloads/run`, `/health`, `/stop` 제공, mutating endpoint token 인증, Goose task와 script-only workload 실행, vsock CP-token update 처리 |
| Image builder | `scripts/build_image.sh` | Debian Trixie 기반 golden rootfs에 Goose, `goose-agent`, `micro-init` 설치 |

## 런타임 상태

Host memory 상태:

| 상태 | 소유자 | 의미 |
|---|---|---|
| `ControlPlane.vms` | `cmd/goose-daemon/api.go` | `vm_id` 기준 실행 중인 VM registry |
| `ControlPlane.snapshots` | `cmd/goose-daemon/api.go` | `snapshot_id` 기준 로드된 snapshot metadata |
| `ControlPlane.clients` | `cmd/goose-daemon/api.go` | 현재 control-plane API client 목록. `SIGHUP`으로 reload |
| `ControlPlane.flockMgr` | `cmd/goose-daemon/orchestrator_api.go`, `internal/orchestrator` | host-local live flock registry와 Town Wall handle |
| `ControlPlane.tenantStore` | `cmd/goose-daemon/api.go` | `tenants/tenants.json` 기반 tenant quota/usage store |
| `ControlPlane.metrics` | `cmd/goose-daemon/metrics_handler.go` | `ephemera_*` Prometheus registry, legacy `anvil_*` lifecycle compatibility lines |
| `PlacementStore` | `internal/anvilmcp/placement_store.go` | scheduler host, VM placement, snapshot location state |
| `QuotaStore` | `internal/anvilmcp/quota_store.go` | scheduler와 daemon tenant API에서 사용하는 quota/usage JSON state |
| `network.Manager.ipInUse` | `internal/network/manager.go` | 할당된 private guest IP |
| `network.Manager.freeTapIDs` | `internal/network/manager.go` | 재사용 가능한 TAP ID |

Host disk 상태:

| 경로 | 의미 |
|---|---|
| `artifacts/golden-image.ext4` | 새 VM disk의 base rootfs |
| `artifacts/vmlinux.bin` | Firecracker 호환 Linux kernel |
| `artifacts/firecracker` | 다운로드 시 SHA256 검증된 Firecracker binary |
| `artifacts/goose-agent` | VM 내부 HTTP agent binary |
| `artifacts/goose-agent.sha256` | `cmd/goose-agent`, `go.mod`, `go.sum` 기준 source hash stamp |
| `artifacts/micro-init` | VM 내부 PID 1 binary |
| `tenants/tenants.json` | daemon-local tenant quota/usage state, mode `0600` |
| `audit/runtime-audit.jsonl` | daemon runtime audit 조회/보관 API가 읽는 JSONL record |
| `flocks/<flock_id>/TOWN_WALL.log` | Goosetown flock별 append-only Town Wall log |
| `flocks/<flock_id>/metadata.json` | daemon restart 뒤 flock registry 복구와 watchdog status persistence를 위한 flock metadata |
| `vms/<vm_id>/state.json` | spawn-path VM cold-restart state. network identity, disk path, profile, flock link, agent token 포함 |
| `ANVIL_SCHEDULER_STATE` path | scheduler service placement/snapshot locality JSON state |
| `ANVIL_SCHEDULER_QUOTA_STORE` path | scheduler service tenant quota/usage JSON state |
| `/tmp/goose-workspaces/<vm_id>.ext4` | cold-spawn VM의 writable rootfs clone |
| `/tmp/goose-workspaces/<vm_id>.cow` | COW-restored VM의 sparse exception store |
| `snapshots/<snapshot_id>/memory.bin` | Full 또는 sparse diff guest memory snapshot |
| `snapshots/<snapshot_id>/state.bin` | Firecracker device/machine state |
| `snapshots/<snapshot_id>/rootfs.ext4` | snapshot rootfs copy |
| `snapshots/<snapshot_id>/metadata.json` | restore metadata. token, MAC, TAP, type, base snapshot ID 포함 |

Guest disk 상태:

| 경로 | 의미 |
|---|---|
| `/root/.config/goose/config.yaml` | 주입된 Goose config |
| `/root/.config/goose/secrets.yaml` | 주입된 Goose secrets |
| `/root/.ephemera-agent-token` | VM별 guest agent Bearer token, mode `0600` |
| `/root/.ephemera-cp-token` | in-VM Town Wall forwarder가 control plane으로 callback할 때 쓰는 bearer. flock member는 그 flock의 **guest 능력 토큰**을 받는다([ADR-0003](../adr/0003-per-flock-guest-capability-tokens.md)) — 운영자 bearer가 아니며 SIGHUP fan-out 대상도 아니다. v0.3.4 SIGHUP fan-out은 daemon이 자기 운영자 bearer를 주입했던 VM(= ADR-0003 이전 spawn)만 갱신한다 |
| `/root/.ephemera-flock` | flock member VM의 flock ID, agent ID, role, Town Wall endpoint context |
| `/root/.goose-system-prompt` | role profile의 system prompt |
| `/usr/local/bin/goose-agent` | guest task server |
| `/usr/local/bin/goose-agent.sha256` | golden image에 설치된 guest agent source hash stamp |
| `/usr/local/bin/gtwall` | VM 내부 Town Wall post helper |
| `/usr/local/sbin/micro-init` | guest PID 1 |

Guest agent API/workload surface:

| 경로 | 의미 |
|---|---|
| `/workloads/run` | VM 내부 `/workspace/workloads/*.sh` script-only runner. LLM provider를 거치지 않고 deterministic workload smoke와 benchmark를 실행한다. |

## 시작 흐름

```text
cmd/goose-daemon/main.go
  -> EPHEMERA_LOG_FORMAT / EPHEMERA_LOG_LEVEL 기준 slog handler 설정
  -> project-relative artifact/config path 해석
  -> EPHEMERA_API_TOKENS_FILE 또는 token env에서 API client list load
  -> snapshots/ 없으면 생성
  -> artifacts/micro-init compile 또는 재사용
  -> artifacts/goose-agent source hash 확인 후 compile 또는 재사용
  -> storage.Provisioner 생성
       -> /tmp/goose-workspaces 보장
       -> artifacts/golden-image.ext4 보장
       -> 없으면 scripts/build_image.sh 실행
  -> golden image 내부 goose-agent hash stamp 확인
       -> host artifact stamp와 다르면 image 내부 /usr/local/bin/goose-agent 교체
  -> Firecracker kernel 다운로드 또는 재사용
  -> Firecracker binary 다운로드 또는 재사용, SHA256 검증
  -> network.Manager 생성
       -> goose-br0 생성/활성화
       -> ip_forward 활성화
       -> iptables MASQUERADE rule 추가
  -> ControlPlane 생성
       -> snapshots/*/metadata.json에서 snapshot metadata load
       -> flocks/*/metadata.json에서 flock metadata와 Town Wall load
       -> vms/*/state.json에서 spawn-path VM cold-restart
       -> tenants/tenants.json load
       -> optional trace exporter load
       -> Prometheus metric registry와 stats endpoint wiring
       -> watchdog tunable 적용
       -> HTTP route 등록
  -> API serve
  -> SIGINT/SIGTERM shutdown 처리
  -> SIGHUP token reload 처리
```

Scheduler service 시작 흐름:

```text
cmd/anvil-scheduler/main.go
  -> ANVIL_SCHEDULER_ADDR 읽기, 기본값 127.0.0.1:3010
  -> ANVIL_SCHEDULER_STATE에서 PlacementStore load
  -> ANVIL_SCHEDULER_QUOTA_STORE에서 QuotaStore load
  -> /health, /hosts, /hosts/{name}, /placements, /reconcile, /schedule/spawn, /schedule/restore 등록
  -> HTTP serve
```

daemon은 self-bootstrapping을 의도한다. 첫 실행에서 image, kernel,
Firecracker binary, guest binary를 준비하고, 이후 실행에서는 누락된 artifact만
다시 준비한다.

## VM 형태

Cold-spawn VM은 `vm.StartMachine`으로 시작한다.

| 설정 | 값 |
|---|---|
| vCPU | `1` (default; profile/per-VM `EPHEMERA_VCPU_COUNT` override. `v0.5.3`부터 upstream default 1, 이전 2) |
| Memory | `1024 MiB` (default; profile/per-VM `EPHEMERA_MEM_SIZE_MIB` override. `v0.5.3`부터 upstream default 1024, 이전 2048) |
| Root drive | VM별 ext4 clone |
| Network | `goose-br0`에 연결된 TAP interface 1개 |
| IP assignment | DHCP 없이 kernel `ip=` boot argument |
| Init | `init=/usr/local/sbin/micro-init` |
| Dirty page tracking | diff snapshot 지원을 위해 활성화 |
| Vsock | restore 후 guest IP 재설정을 위해 활성화 |

Restored VM은 Firecracker memory file과 `state.bin`을 사용해
`vm.RestoreMachine`으로 시작한다. kernel boot path를 다시 타지 않고 snapshot
state에서 resume한 뒤, vsock reconfiguration channel로 새 IP를 받는다.

Goosetown flock VM도 같은 cold-spawn lifecycle을 사용한다. 차이는 daemon이 역할별
profile sizing과 `system.md`를 적용하고, VM disk에 flock context와 Town Wall helper
context를 추가 주입한다는 점이다.

## 네트워크 모델

anvil은 host bridge `goose-br0`를 만들고 gateway `10.0.1.1/24`를 사용한다.
guest IP는 `10.0.1.2`부터 `10.0.1.254`까지 할당한다.

Cold spawn:

```text
network.Manager.Allocate()
  -> 사용 가능한 guest IP 선택
  -> 새 TAP ID 또는 재사용 TAP ID 선택
  -> tap<N> 생성
  -> tap<N>을 goose-br0에 연결
  -> deterministic MAC AA:FC:00:00:xx:yy 생성
```

Snapshot restore:

```text
network.Manager.AllocateForRestore(original_tap, original_mac)
  -> 사용 가능한 guest IP 할당
  -> original TAP name 재생성
  -> original MAC 설정
  -> TAP을 goose-br0에 연결
  -> guest IP는 이후 vsock으로 변경
```

restore path가 original TAP name과 MAC을 요구하는 이유는 Firecracker snapshot
state가 device identity를 포함하기 때문이다. guest IP는 restore 후
`CHANGE_IP` command로 snapshot state와 분리한다.

## 스토리지와 Snapshot 모델

새 VM disk는 `artifacts/golden-image.ext4`를
`/tmp/goose-workspaces/<vm_id>.ext4`로 full copy해서 만든다. control plane은
disk를 한 번 mount해 Goose config, secrets, timezone, VM별 agent token을
주입한다.

Snapshot 생성 결과:

```text
snapshots/<snapshot_id>/
  memory.bin
  state.bin
  rootfs.ext4
  metadata.json
```

Snapshot directory는 `POST /snapshots/gc`로 수동 정리할 수 있다. GC는 먼저
`cp.snapshots` metadata graph를 기준으로 삭제 후보와 보호 사유를 계산한다.
`apply: false`가 기본값이므로 dry-run은 host disk를 변경하지 않는다.
`apply: true`일 때만 보호되지 않은 후보 snapshot directory를 삭제한다. age와
`keep_last_per_vm` policy 외에 `max_total_bytes`를 지정하면 모든 snapshot directory의
apparent file size를 합산하고, 기존 후보 삭제 후에도 projected remaining total이 한도를
넘는 경우 보호되지 않은 snapshot을 오래된 순서로 추가 후보 처리한다. 이때 추가 후보의
reason은 `max_total_bytes`이며, response entry에는 계산 가능한 경우 `size_bytes`가
포함된다. diff snapshot이 참조 중인 full snapshot은 base dependency가 사라질 때까지
보호되므로 size 한도를 넘더라도 같은 GC run에서 삭제하지 않는다. 적용된 manual GC는
`snapshots/gc-audit.jsonl`에 timestamp, applied, policy, candidates/deleted/errors count만
담은 JSONL audit record를 남기며, dry-run은 audit file을 변경하지 않는다.

Snapshot type 선택:

| 요청 | 결과 |
|---|---|
| 해당 VM의 기존 full snapshot 없음 | `full` |
| 기존 full snapshot 있음 | `diff` |
| 명시적 `type: "full"` | full 강제 |
| 명시적 `type: "diff"`이지만 base full 없음 | HTTP 400 |

Diff snapshot은 sparse dirty memory page만 저장하고, `base_snapshot_id`로 full
base snapshot을 참조한다. rootfs는 full/diff 모두 현재는 전체 copy한다.

Cross-host snapshot import는 target host에서 bundle을 곧바로 최종 경로에 풀지 않는다.
daemon은 `snapshots/.import-*` staging directory에 bundle payload를 먼저 기록하고,
`metadata.json`, `memory.bin`, `state.bin`, `rootfs.ext4`와 diff
`base_snapshot_id` dependency를 validation한다. validation이 끝나면 bundle 안의 host
local path를 target daemon의 work directory 기준으로 rebase하고 최종
`snapshots/<snapshot_id>/` directory로 atomic rename한다. publish 전 실패는 staging
directory만 정리하며, 이미 존재하는 최종 snapshot directory를 부분 갱신하지 않는다.
export bundle의 `metadata.json`은 source host의 raw metadata file이 아니라
`agent_token`을 제거한 portable metadata다. Firecracker snapshot state는 original
`disk_path`와 `vsock_path`를 요구하므로 target import는 두 path가 safe workspace/tmp
pattern인지 검증한 뒤 보존한다. `mem_file_path`, `state_file_path`, `disk_copy_path`는
target snapshot directory로 다시 쓴 뒤 최종 metadata checksum을 manifest에 기록한다.
복제된 snapshot restore는 target daemon이 새 `agent_token`을 생성하고 validated
`vsock_path`로 guest agent에 주입해 source host token을 재사용하지 않는다.
operator-facing import/replication 결과는 raw `metadata.json`, `agent_token`,
authorization header를 포함하지 않는다.

Restore는 Linux device-mapper snapshot COW를 우선 사용한다.

```text
snapshot rootfs.ext4, read-only base
  + sparse /tmp/goose-workspaces/<new_vm_id>.cow exception store
  -> /dev/mapper/cow-*
  -> state.bin에 기록된 original disk path 위로 bind mount
```

dm-snapshot setup이 실패하면 daemon은 legacy bind-mount restore path로
fallback한다. 이 경우 snapshot rootfs를 per-restore ext4 file로 copy한다.

## 보안 경계

| 경계 | 메커니즘 |
|---|---|
| Host client -> control plane | control-plane Bearer token. token env가 없으면 비활성화 |
| Control plane -> guest agent | guest disk에 주입된 VM별 Bearer token |
| Guest task isolation | Firecracker MicroVM + KVM boundary |
| Guest private network | `goose-br0` 뒤 host-only `10.0.1.0/24` |
| 외부 공개 | localhost 밖에서는 TLS 종료 reverse proxy 뒤에서 운영 |
| Secrets | gitignore된 로컬 config file에서 guest disk로 주입 |
| Metrics scrape | `/metrics`는 기본 unauthenticated. 외부 노출 시 `EPHEMERA_METRICS_REQUIRE_AUTH=true` 또는 network isolation 필요 |

`GET /health`는 readiness poll을 위해 guest agent에서 인증 없이 유지한다.
mutating guest endpoint는 token file이 없을 때를 제외하면 VM별 agent token을
요구한다.

## 동시성 모델

- `ControlPlane.mu`: 실행 중인 VM registry 보호
- `ControlPlane.snapshotsMu`: in-memory snapshot metadata 보호
- `ControlPlane.clientsMu`: API client token reload 보호
- `ControlPlane.restoreMu`: restore 중 disk setup과 Firecracker open 구간 직렬화
- `FlockManager.mu`: host-local flock registry 보호
- `Flock.mu`: flock agent map과 JSON marshal 보호
- `TownWall`: append-only log write와 subscriber delivery 보호
- `network.Manager.mu`: IP/TAP allocation 보호
- `goose-agent`: VM당 한 번에 하나의 task만 수행. 두 번째 concurrent task는
  `503 agent busy` 반환

## 현재 제약

- 같은 snapshot의 concurrent restore는 지원하지 않는다. snapshot state가
  original vsock UDS path를 포함하기 때문이다.
- 서로 다른 snapshot의 concurrent restore는 지원한다.
- snapshot restore 전에 source VM을 삭제해야 한다.
- diff snapshot이 참조 중인 full snapshot은 삭제할 수 없다.
- live 또는 persisted restored VM state가 참조 중인 source snapshot은 삭제할 수
  없다. `DELETE /snapshots/{id}`는 참조 중이면 `409`를 반환하므로 restored VM을
  먼저 삭제한 뒤 source snapshot을 삭제한다. (upstream e2e 46c의 `200` orphan
  동작과 다른 의도적 anvil divergence.)
- diff restore에는 임시 merged memory file을 만들 disk space가 필요하다.
- control-plane auth는 API token file/env가 없으면 비활성화된다.
- `/metrics`는 Prometheus scrape 관례에 따라 기본적으로 control-plane auth 밖에 있다.
  외부 노출 시 `EPHEMERA_METRICS_REQUIRE_AUTH=true`로 보호한다.
- MCP v1은 runtime control plane의 일부가 아니라 client adapter다.
- flock registry는 daemon process memory가 주 상태지만, `metadata.json`이 있는 flock은
  daemon 재시작 뒤 registry와 Town Wall history가 복구된다. spawn-path member VM은
  `vms/<vm_id>/state.json` 기반 cold-restart 대상이며, memory state와 in-flight task는
  보존되지 않는다.
- spawn-path VM(plain·COW disk mode 모두)은 `vms/<vm_id>/state.json` 기반
  cold-restart 대상이다. snapshot-restored VM은 `v0.4.5`부터 `source_snapshot_id`를
  담은 `state.json`으로 daemon restart 시 source snapshot에서 re-restore된다
  (snapshot-time memory·disk로 복귀하며 post-restore write는 보존되지 않는다).
  bind-mount-fallback restore와 source snapshot이 삭제된 restored VM은 recovery
  대상이 아니며 drop되어 `failed[]`/dead flock agent로 surface된다.
- `POST /flocks` 응답은 anvil 보안 불변 조건에 따라 `agent_token`/`agent_tokens`를
  노출하지 않는다.

## 소스 참조

- `cmd/goose-daemon/main.go`
- `cmd/goose-daemon/api.go`
- `cmd/goose-daemon/config.go`
- `cmd/goose-daemon/metrics_handler.go`
- `cmd/goose-daemon/stats_handler.go`
- `cmd/goose-daemon/stats_collector.go`
- `cmd/goose-daemon/recovery.go`
- `cmd/goose-daemon/orchestrator_api.go`
- `cmd/goose-daemon/egress_policy.go`
- `cmd/goose-daemon/sni_verdict.go`
- `cmd/goose-daemon/otel.go`
- `cmd/anvil-scheduler/main.go`
- `cmd/goose-agent/main.go`
- `cmd/micro-init/main.go`
- `internal/orchestrator/flock.go`
- `internal/orchestrator/persistence.go`
- `internal/orchestrator/townwall.go`
- `internal/orchestrator/watchdog.go`
- `internal/anvilmcp/placement_store.go`
- `internal/anvilmcp/quota_store.go`
- `internal/anvilmcp/scheduler_service.go`
- `internal/storage/provisioner.go`
- `internal/storage/snapshot.go`
- `internal/network/manager.go`
- `internal/network/sni/`
- `internal/network/quic/`
- `internal/vm/machine.go`
- `README.md`
- `CONTEXT.md`
