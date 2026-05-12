# anvil

[![CI](https://github.com/HardcoreMonk/ephemera/actions/workflows/ci.yml/badge.svg)](https://github.com/HardcoreMonk/ephemera/actions/workflows/ci.yml)
[![Latest Tag](https://img.shields.io/github/v/tag/HardcoreMonk/ephemera?sort=semver&label=tag)](https://github.com/HardcoreMonk/ephemera/tags)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Firecracker](https://img.shields.io/badge/Firecracker-v1.15.1-FF4500?logo=amazonaws&logoColor=white)](https://github.com/firecracker-microvm/firecracker)

**IronClaw를 위한 격리 AI agent 실행 layer**

`anvil`은 IronClaw가 AI agent 작업을 안전하게 생성, 실행, 중지,
snapshot/restore할 수 있도록 만드는 통합 실행 프로젝트다. IronClaw는 상위
orchestration과 MCP client 역할을 맡고, anvil은 IronClaw의 tool call을 실제
격리 실행 환경으로 연결한다. 기반 실행 엔진은 ephemera이며, ephemera는 KVM 기반
Firecracker MicroVM runtime을 제공한다.

anvil의 상위 통합 대상은 IronClaw 전용이다. OpenClaw 연동은 anvil의 지원 범위가
아니며, OpenClaw용 compatibility layer나 운영 계약은 제공하지 않는다.

이 저장소의 현재 URL은 `https://github.com/HardcoreMonk/ephemera/`이다.
ephemera는 이미 `0.1.0`, `0.2.0`이 릴리즈된 기반 runtime이므로 Go 모듈 경로,
daemon 이름, HTTP API, 일부 환경 변수에는 `ephemera` 또는 `goose` 이름이 남아
있다. README에서는 `anvil`을 IronClaw 통합 프로젝트로, `ephemera`를 분리된 기반
runtime으로 구분한다.

버전별 ephemera 소스 snapshot은 Git tag로 공개된다. 현재 공개 소스 tag 기준은
`v0.2.0`이며, GitHub Release page는 아직 게시하지 않았다.

---

## 프로젝트 경계

| 영역 | 책임 | 현재 구현 |
|---|---|---|
| IronClaw | 상위 orchestration, MCP client, 작업 의사결정 | 외부 통합 계층 |
| anvil | IronClaw가 사용할 MCP tool surface와 실행 lifecycle 계약 | `cmd/anvil-mcp`, `internal/anvilmcp` |
| ephemera | Firecracker MicroVM 생성, agent proxy, snapshot/restore, host resource 정리 | `cmd/goose-daemon`, `internal/vm`, `internal/storage`, `internal/network` |
| guest runtime | VM 내부 task 실행, health, graceful stop | `cmd/goose-agent`, `cmd/micro-init` |

anvil은 ephemera를 이름만 바꾼 프로젝트가 아니다. anvil은 IronClaw와 ephemera를
연결하는 통합 실행 layer이고, ephemera는 독립적인 runtime 구현과 API 계약을
가진다. 따라서 runtime API와 환경 변수는 호환성을 위해 ephemera/goose 이름을
유지하고, IronClaw가 직접 사용하는 표면은 `anvil_*` MCP tool로 노출한다.

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

| 기능 | 설명 |
|---|---|
| IronClaw MCP adapter | `cmd/anvil-mcp`가 IronClaw에 `anvil_*` MCP tool을 제공한다. |
| VM lifecycle tool | `anvil_spawn_vm`, `anvil_run_task`, `anvil_get_vm_health`, `anvil_stop_vm`, `anvil_delete_vm`을 제공한다. |
| Snapshot lifecycle tool | `anvil_create_snapshot`, `anvil_list_snapshots`, `anvil_restore_snapshot`, `anvil_delete_snapshot`을 제공한다. |
| Session alias | adapter process 내부에서 `session_name -> vm_id` alias를 유지해 IronClaw workflow를 단순화한다. |
| Token redaction | daemon restore 응답의 `agent_token`은 decode할 수 있지만 MCP output에는 노출하지 않는다. |
| Restore cleanup 계약 | restore 성공 후 alias bind가 실패하면 restored VM을 자동 삭제하지 않고 error에 VM ID를 포함한다. |

---

## ephemera runtime 분리

ephemera는 anvil이 사용하는 기반 실행 엔진이다. VM 생성, Firecracker machine
관리, TAP/IP 할당, rootfs 준비, snapshot file 관리, guest agent proxy는
ephemera control plane이 소유한다. anvil MCP adapter는 이 의미를 재해석하지 않고
얇게 호출한다.

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
  POST   /vms                  -> VM 생성
  GET    /vms                  -> 실행 중인 VM 목록
  DELETE /vms/{vm_id}          -> VM 종료 및 리소스 정리
  POST   /vms/{vm_id}/snapshot -> VM snapshot 생성
  GET    /snapshots            -> snapshot 목록
  POST   /snapshots/{id}/restore
                                -> snapshot에서 VM 복원
  DELETE /snapshots/{id}       -> snapshot 삭제

      |
      | Firecracker SDK, KVM, TAP, rootfs, snapshot files
      v
Firecracker MicroVM, ephemera runtime
  - Debian Bookworm minbase rootfs
  - micro-init, PID 1
  - goose-agent :8080
  - goose CLI task 실행

외부 client
      |
      | control plane proxy
      v
POST /vms/{vm_id}/tasks  -> goose-agent :8080/tasks
GET  /vms/{vm_id}/health -> goose-agent :8080/health
POST /vms/{vm_id}/stop   -> goose-agent :8080/stop
```

`EPHEMERA_PUBLIC_URL`이 설정되어 있으면 ephemera `POST /vms` 응답의 `agent_url`은
control plane proxy 경로를 가리킨다. 설정되지 않은 경우 host에서 접근 가능한
VM private IP가 반환된다.

### VM 생성 흐름

```text
CloneDisk()
  -> golden image를 VM별 ext4 disk로 copy

PrepareVM()
  -> goose.yaml, goose-secrets.yaml, agent_token, /etc/localtime 주입
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

| 기능 | 설명 |
|---|---|
| 자체 bootstrap | ephemera 첫 실행 시 golden image, kernel, Firecracker binary를 준비하고 검증한다. |
| 최소 guest OS | Debian Bookworm minbase와 Go 기반 `micro-init`으로 구성한다. |
| 안전한 guest 종료 | `micro-init`이 signal을 받아 `poweroff(2)`를 호출해 kernel panic을 피한다. |
| VM별 LLM profile | VM 생성 시 `configs/profiles/{name}/`의 provider/model/secret을 선택할 수 있다. |
| 런타임 설정 주입 | `goose.yaml`, `goose-secrets.yaml`을 provision time에 주입한다. |
| VM별 agent 인증 | VM마다 별도 Bearer token을 생성하고 guest disk에 `0600`으로 저장한다. |
| Full/Diff snapshot | 첫 snapshot은 Full, 이후 snapshot은 dirty memory page 기반 Diff로 자동 선택된다. |
| COW rootfs restore | restore VM은 snapshot rootfs를 read-only base로 공유하고 sparse COW file에 쓰기를 기록한다. |
| Restore 후 IP 재설정 | VM은 새 IP를 할당받고 vsock으로 guest network stack을 갱신한다. |
| IP/TAP 재사용 | lifecycle 종료 후 `10.0.1.2-254` IP와 TAP ID를 pool에 반환한다. |
| Outbound NAT | `goose-br0`와 iptables MASQUERADE로 guest의 LLM API outbound를 지원한다. |
| Control-plane 인증 | named Bearer token, timing-safe compare, audit log, `SIGHUP` hot reload를 지원한다. |

---

## 프로젝트 구조

```text
cmd/
  goose-daemon/       ephemera control plane daemon
    main.go           startup, artifact bootstrap, ControlPlane init
    api.go            VM/snapshot API, auth middleware, proxy
    config.go         환경 변수 기반 설정
  anvil-mcp/          anvil/IronClaw용 stdio MCP adapter entrypoint
  goose-agent/        VM 내부 HTTP agent
  micro-init/         VM 내부 PID 1

internal/
  anvilmcp/           MCP config, daemon client, session alias, tool handler
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

docs/
  architecture/        ephemera 런타임, 서비스 로직, anvil MCP 아키텍처
  analysis/            ephemera 버전 비교와 소스 분석
  lifecycle/runs/      계산된 lifecycle 상태 snapshot
  operations/          release/operate handoff 기록
  superpowers/         승인된 spec, review, plan 기록

snapshots/             snapshot 저장 디렉터리, gitignore
artifacts/             runtime artifact 디렉터리, gitignore
e2e_test.sh            50단계 통합 테스트
scripts/build_image.sh golden image build script
```

## 문서 지도

| 문서 | 역할 |
|---|---|
| [CONTEXT.md](CONTEXT.md) | anvil/ephemera/IronClaw 경계, 진실 기준 문서 순서, 고정 계약 |
| [AGENTS.md](AGENTS.md) | Codex 작업 규약, 검증 명령, 불변 조건 |
| [RELEASE_NOTES.md](RELEASE_NOTES.md) | ephemera `v0.1.0`, `v0.2.0`, anvil 통합 미릴리즈 변경 사항 |
| [docs/architecture/runtime-architecture.md](docs/architecture/runtime-architecture.md) | ephemera daemon, MicroVM, storage, network, guest runtime 구조 |
| [docs/architecture/service-logic.md](docs/architecture/service-logic.md) | ephemera control-plane API, VM lifecycle, snapshot/restore, guest agent 흐름 |
| [docs/architecture/mcp-architecture.md](docs/architecture/mcp-architecture.md) | IronClaw MCP adapter 구조와 tool 계약 |
| [docs/analysis/README.md](docs/analysis/README.md) | ephemera 0.1.0/0.2.0 분석 문서 index |
| [docs/operations/2026-05-11-anvil-redesign-handoff.md](docs/operations/2026-05-11-anvil-redesign-handoff.md) | 재설계 release/operate handoff 근거 |

---

## 사전 요구사항

| 항목 | 내용 |
|---|---|
| Host OS | Ubuntu 22.04 또는 24.04 권장 |
| CPU | `/dev/kvm` 접근 가능 |
| Go | 1.25 이상 |
| Package | `curl`, `debootstrap`, `e2fsprogs`, `util-linux` |
| 권한 | 실행 시 `sudo` 필요. KVM, bridge, TAP, iptables를 설정한다. |

```bash
sudo apt-get install -y curl debootstrap e2fsprogs util-linux
```

Firecracker, Linux kernel, golden image는 첫 실행 시 자동으로 다운로드하거나
빌드한다.

---

## 시작하기

### 1. 복제와 빌드

```bash
git clone https://github.com/HardcoreMonk/ephemera.git
cd ephemera
go build -o anvil-daemon ./cmd/goose-daemon/
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
resolution, agent auth middleware, token generation 등을 검증한다.

### 종단 간 테스트

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
```

`e2e_test.sh`는 실제 Firecracker MicroVM을 부팅하는 50단계 통합 테스트다.
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
| 50 | daemon graceful shutdown |

---

## 설정

모든 daemon 설정은 시작 시 환경 변수에서 읽는다.

| Canonical 변수 | Alias 변수 | 기본값 | 설명 |
|---|---|---|---|
| `EPHEMERA_API_ADDR` | `ANVIL_API_ADDR` | `127.0.0.1:3000` | control plane bind 주소. reverse proxy 뒤에서는 `0.0.0.0:3000`으로 설정할 수 있다. |
| `EPHEMERA_API_PORT` | `ANVIL_API_PORT` | `3000` | API addr가 없을 때 사용하는 port. |
| `EPHEMERA_API_TOKENS` | `ANVIL_API_TOKENS` | unset | named Bearer token 목록. 예: `alice:token1,bob:token2` |
| `EPHEMERA_API_TOKEN` | `ANVIL_API_TOKEN` | unset | 단일 Bearer token fallback. |
| `EPHEMERA_AGENT_PORT` | `ANVIL_AGENT_PORT` | `8080` | VM 내부 `goose-agent` listen port. |
| `EPHEMERA_PUBLIC_URL` | `ANVIL_PUBLIC_URL` | unset | 외부에서 접근 가능한 control plane base URL. 설정 시 `agent_url`이 proxy path가 된다. |

`EPHEMERA_*`는 ephemera runtime의 canonical 변수이고 `ANVIL_*`는 anvil 운영자를
위한 alias다. 각 변수 쌍에서는 `EPHEMERA_*` 값이 `ANVIL_*` 값보다 우선한다.
bind 주소 쌍(`EPHEMERA_API_ADDR`/`ANVIL_API_ADDR`)은 port 쌍보다 우선한다.
인증 token precedence는 `EPHEMERA_API_TOKENS` -> `ANVIL_API_TOKENS` ->
`EPHEMERA_API_TOKEN` -> `ANVIL_API_TOKEN` -> 인증 비활성화 순서다. token은
`SIGHUP`으로 daemon 재시작 없이 reload할 수 있다.

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

| Tool | 역할 |
|---|---|
| `anvil_spawn_vm` | ephemera VM을 만들고 optional `session_name` alias를 연결한다. |
| `anvil_run_task` | `vm_id` 또는 `session_name`으로 VM에 prompt를 실행한다. |
| `anvil_copy_in` | `vm_id` 또는 `session_name`으로 VM `/workspace`에 단일 text file을 쓴다. |
| `anvil_copy_out` | `vm_id` 또는 `session_name`으로 VM `/workspace`의 단일 text file을 읽는다. |
| `anvil_get_vm_health` | VM agent health를 확인한다. |
| `anvil_stop_vm` | guest agent에 graceful stop을 요청한다. |
| `anvil_delete_vm` | host VM 리소스를 삭제하고 session alias를 해제한다. |
| `anvil_create_snapshot` | `vm_id` 또는 `session_name`으로 VM snapshot을 생성한다. |
| `anvil_list_snapshots` | daemon이 알고 있는 snapshot 목록을 조회한다. |
| `anvil_restore_snapshot` | `snapshot_id`에서 새 VM을 restore하고 optional `session_name` alias를 연결한다. |
| `anvil_delete_snapshot` | `snapshot_id`로 snapshot을 삭제한다. |

MCP adapter는 얇은 runtime bridge다. 현재 workspace copy는 VM 내부
`/workspace` 기준 단일 text file copy-in/copy-out만 지원한다. directory sync,
snapshot alias, session alias 영속화, HTTP MCP transport는 제공하지 않는다.
Restore 응답은 daemon의 `agent_token`을 decode할 수 있지만 MCP output에는
노출하지 않는다. Restore 후 `session_name` bind가 실패하면 adapter는 restored
VM을 자동 삭제하지 않고 error에 restored VM ID를 포함한다.

정확한 입력/출력 계약은 `docs/architecture/mcp-architecture.md`를 참조한다.

문서 기준 MCP smoke test는 실제 daemon과 `anvil-mcp` stdio server를 함께
사용한다. 먼저 root 권한으로 daemon을 실행한다.

```bash
sudo ANVIL_API_ADDR=127.0.0.1:3000 ./anvil-daemon
```

다른 터미널에서 smoke client를 실행한다.

```bash
go run scripts/anvil-mcp-smoke.go -session smoke
```

이 검사는 `anvil_spawn_vm`, `anvil_copy_in`, `anvil_copy_out`,
`anvil_run_task`, `anvil_get_vm_health`, `anvil_stop_vm`,
`anvil_delete_vm` 순서로 tool call을 수행한다. 기본값은 workspace copy
round-trip과 `anvil_run_task` 응답의 `anvil-smoke-ok` 포함 여부를 확인한다.
provider credential이 아직 유효하지 않은 환경에서 MCP lifecycle과 workspace
copy만 확인하려면 `-expect-output ""`를 사용한다.

---

## API 참조

token이 설정되어 있으면 모든 control-plane endpoint는
`Authorization: Bearer <token>`을 요구한다.

### VM 생성

```text
POST /vms
Content-Type: application/json

{ "profile": "anthropic" }
```

`profile`을 생략하면 기본 `configs/goose.yaml`과
`configs/goose-secrets.yaml`을 사용한다.

```bash
curl -X POST http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"profile": "anthropic"}'
```

```json
{
  "vm_id": "vm-1778227813435",
  "guest_ip": "10.0.1.10",
  "agent_url": "http://10.0.1.10:8080",
  "profile": "anthropic",
  "agent_token": "3f9a2c..."
}
```

daemon API에서는 `POST /vms`와 `POST /snapshots/{id}/restore` 응답에
`agent_token`이 포함될 수 있다. MCP output은 restore 응답의 `agent_token`을
노출하지 않는다.

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

### Snapshot 복원

```bash
curl -X POST http://localhost:3000/snapshots/snap-1778229000000/restore \
  -H "Authorization: Bearer $TOKEN"
```

source VM이 아직 실행 중이면 restore는 거부된다. restore된 VM은 새 VM ID와
새 IP를 받지만 snapshot의 agent token은 유지한다.

### Snapshot 삭제

```bash
curl -X DELETE http://localhost:3000/snapshots/snap-1778229000000 \
  -H "Authorization: Bearer $TOKEN"
```

diff snapshot이 참조 중인 full snapshot은 삭제할 수 없다.

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

| 경계 | 메커니즘 |
|---|---|
| client -> control plane | `EPHEMERA_API_TOKENS`/`EPHEMERA_API_TOKEN` 또는 `ANVIL_API_TOKENS`/`ANVIL_API_TOKEN` Bearer token |
| control plane -> guest agent | VM별 Bearer token |
| guest task isolation | Firecracker MicroVM + KVM boundary |
| guest network | host-only `10.0.1.0/24`, bridge `goose-br0` |
| 외부 공개 | TLS 종료 reverse proxy 뒤에서 운영 |
| secret | gitignore된 로컬 config에서 guest disk로 주입 |

실제 API key는 문서, issue, commit, 채팅에 남기지 않는다.

---

## 알려진 제약

- 같은 snapshot을 동시에 두 번 restore하는 흐름은 지원하지 않는다.
- 서로 다른 snapshot의 concurrent restore는 지원한다.
- source VM이 실행 중인 동안 해당 VM의 snapshot restore는 거부된다.
- diff snapshot은 memory만 diff다. rootfs는 snapshot마다 full copy다.
- diff restore는 임시 merged memory file을 만들 disk space가 필요하다.
- control-plane token 환경 변수를 설정하지 않으면 API 인증이 비활성화된다.
- MCP v1은 snapshot/restore tool을 제공하지만 snapshot alias와 session alias 영속화는 제공하지 않는다.

---

## 라이선스

MIT License. 자세한 내용은 [LICENSE](LICENSE)를 참조한다.
