# anvil v0.1.0 — IronClaw 통합 E2E 완료

`anvil`은 IronClaw와 ephemera를 결합하는 새 프로젝트다. 이 저장소의 공개
릴리즈 `v0.1.0`, `v0.2.0`은 ephemera runtime 릴리즈이며, anvil 통합
릴리즈는 `anvil-v0.1.0` 태그로 분리한다.

## 추가됨

- ephemera daemon `POST /snapshots/gc`: 수동 snapshot retention/GC API.
  - 기본 dry-run mode로 삭제 후보와 보호 사유를 반환한다.
  - `apply: true`일 때만 후보 snapshot directory를 삭제한다.
  - diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
- `cmd/anvil-mcp`: IronClaw 연동용 Go stdio MCP 서버.
- `internal/anvilmcp`: 설정 로더, daemon HTTP client, session alias 저장소,
  MCP tool handler.
- `configs/anvil-mcp.yaml.example`: 파일 기반 MCP adapter 설정 예시.
- MCP tool:
  - `anvil_copy_in`
  - `anvil_copy_out`
  - `anvil_create_snapshot`
  - `anvil_delete_snapshot`
  - `anvil_delete_vm`
  - `anvil_get_vm_health`
  - `anvil_list_snapshots`
  - `anvil_restore_snapshot`
  - `anvil_run_task`
  - `anvil_spawn_vm`
  - `anvil_stop_vm`
- `scripts/anvil-mcp-smoke.go`: daemon 없이 MCP tool surface를 검증하는 smoke
  client.
- `docs/architecture/`: 런타임 아키텍처, 서비스 로직, MCP 아키텍처 문서.
- `docs/operations/2026-05-12-ironclaw-integration-check.md`: IronClaw 설치,
  MCP 연결, 실제 IronClaw agent E2E 검증 기록.

## 변경됨

- 공식 MCP Go SDK 지원을 위해 최소 Go 버전은 1.25 이상이다.
- 로컬 빌드 산출물 `anvil-daemon`이 git에 들어가지 않도록 ignore 규칙을
  정리했다.
- `ANVIL_API_*`, `ANVIL_PUBLIC_URL`, `ANVIL_DAEMON_*` 환경 변수 alias를
  지원해 IronClaw/anvil 문맥에서 daemon 설정을 읽을 수 있게 했다.
- workspace copy-in/out은 파일 크기 제한, overwrite 정책, binary/base64 거부,
  표준화된 오류 본문을 적용한다.
- `artifacts/goose-agent`는 source hash/version stamp 기반으로 재빌드 여부를
  판단한다.

## 검증됨

- `go test ./...`
- `go build -o /tmp/anvil-daemon ./cmd/goose-daemon`
- `go build -o /tmp/anvil-mcp ./cmd/anvil-mcp`
- `ironclaw mcp test anvil --no-onboard --cli-only`
- 실제 IronClaw agent 기준 anvil MCP tool call E2E

## 알려진 운영 주의사항

- IronClaw 기본 전체 tool inventory와 Gemini tool schema 조합에서는 non-anvil
  tool schema 때문에 agent 실행 전 schema 오류가 발생할 수 있다. anvil 전용 tool
  permission profile을 적용하면 anvil MCP tool call은 정상 검증된다.

# ephemera v0.2.0 — 단일 호스트 기능 완성

ephemera `v0.2.0`은 v0.1.0의 기본 VM 생성/작업 실행 모델에 snapshot, restore,
인증, proxy, profile, COW rootfs, diff snapshot을 추가한 릴리즈다.

## 새 기능

### 안전한 게스트 종료

- 새 guest PID 1인 `micro-init` 추가.
- VM 종료 시 Firecracker가 보내는 `SIGTERM`을 `micro-init`이 받아
  `goose-agent`를 종료하고 `sync` 후 `poweroff(2)`를 호출한다.
- PID 1이 그냥 종료될 때 발생할 수 있는 guest kernel panic을 제거했다.

### VM별 agent 인증

- 각 VM 생성 시 32-byte random Bearer token을 생성한다.
- token은 VM disk의 `/root/.ephemera-agent-token`에 `0600` 권한으로
  주입된다.
- `POST /tasks`와 `POST /stop`은 VM별 token을 요구한다.
- `GET /health`는 readiness probe를 위해 인증 없이 유지한다.
- `POST /vms` 응답에만 `agent_token`을 포함한다. list, snapshot 응답에는
  노출하지 않는다.

### 제어 평면 API 인증

- daemon API에 per-client Bearer token 인증을 추가했다.
- 선호 설정: `EPHEMERA_API_TOKENS=alice:tok1,bob:tok2`
- 기존 단일 token 호환 설정: `EPHEMERA_API_TOKEN=tok`
- 비교는 timing-safe 방식으로 수행한다.
- 요청 로그에 인증된 client 이름을 남긴다.
- `SIGHUP`으로 token list를 hot reload할 수 있다.

### Agent proxy endpoint 추가

- 새 control-plane proxy endpoint:
  - `POST /vms/{vm_id}/tasks`
  - `GET /vms/{vm_id}/health`
  - `POST /vms/{vm_id}/stop`
- 외부 client는 VM private IP로 직접 접근하지 않아도 된다.
- daemon이 VM별 `agent_token`을 내부적으로 주입한다.

### 공개 `agent_url`

- 새 환경 변수: `EPHEMERA_PUBLIC_URL`
- 설정하면 `POST /vms` 응답의 `agent_url`이
  `{EPHEMERA_PUBLIC_URL}/vms/{vm_id}` 형태의 proxy path가 된다.
- reverse proxy/TLS 배포에서 VM private IP를 외부에 노출하지 않는다.

### VM별 LLM profile 지원

- `POST /vms`가 optional `profile` field를 받는다.
- 기본 설정:
  - `configs/goose.yaml`
  - `configs/goose-secrets.yaml`
- named profile 설정:
  - `configs/profiles/{profile}/goose.yaml`
  - `configs/profiles/{profile}/goose-secrets.yaml`
- profile 이름에는 path separator를 허용하지 않는다.
- 설정과 secret은 image rebuild 없이 provision time에 주입된다.

### Full snapshot 수명주기

- 새 endpoint:
  - `POST /vms/{vm_id}/snapshot`
  - `GET /snapshots`
  - `POST /snapshots/{id}/restore`
  - `DELETE /snapshots/{id}`
- snapshot은 다음 파일을 저장한다.
  - `memory.bin`
  - `state.bin`
  - `rootfs.ext4`
  - `metadata.json`
- `stop_after` option으로 snapshot 생성 뒤 source VM을 삭제할 수 있다.
- restore 후 새 VM ID와 새 IP를 할당한다.
- snapshot metadata는 original agent token, MAC, TAP, disk path, vsock path를
  보존한다.

### Diff memory snapshot 지원

- dirty page tracking을 사용해 memory diff snapshot을 지원한다.
- 첫 snapshot은 자동으로 `full`이다.
- 같은 VM의 이후 snapshot은 자동으로 `diff`이며 latest full snapshot을
  `base_snapshot_id`로 참조한다.
- 명시적 `type: "full"` 또는 `type: "diff"` 요청도 지원한다.
- diff restore는 base memory와 diff memory를 sparse-aware 방식으로 merge한
  뒤 Firecracker restore에 전달한다.
- diff가 참조 중인 full snapshot 삭제는 `409 Conflict`로 차단한다.

### COW rootfs restore 지원

- restore된 VM은 기본적으로 Linux `dm-snapshot` COW device를 사용한다.
- snapshot `rootfs.ext4`는 read-only base로 공유한다.
- VM별 쓰기는 `/tmp/goose-workspaces/<vm_id>.cow` sparse exception store에
  기록한다.
- restore마다 약 700 MB rootfs copy를 만들던 방식을 제거했다.
- VM 삭제 시 dm device, loop device, bind mount, `.cow` file을 정리한다.
- dm-snapshot setup이 실패하면 기존 bind-mount restore fallback으로
  동작한다.

### Restore 후 IP 재설정

- Firecracker snapshot state에는 TAP device identity와 disk path가 들어 있다.
- restore 시 original TAP name/MAC을 재생성한다.
- guest IP는 vsock `CHANGE_IP` command로 새 IP로 재설정한다.
- 같은 host에서 snapshot state와 runtime IP allocation을 분리한다.

### 통합 테스트 확장

- `e2e_test.sh`를 50단계 통합 테스트로 확장했다.
- 검증 범위:
  - daemon startup
  - VM lifecycle
  - 병렬 VM 작업
  - full snapshot create/list/restore/delete
  - concurrent restore
  - diff snapshot 자동 선택
  - diff sparse size 검증
  - diff restore
  - full/diff dependency protection
  - COW restore resource cleanup
  - agent proxy endpoints
  - `EPHEMERA_PUBLIC_URL` proxy URL behavior
  - graceful daemon shutdown

## 변경됨

- guest boot flow가 `init=/usr/local/sbin/micro-init`을 사용한다.
- provisioner는 VM별 token과 timezone data를 한 번의 mount/unmount cycle에서
  주입한다.
- Firecracker restore path는 vsock device와 original disk path 복원을
  명시적으로 처리한다.
- README를 현재 architecture와 운영 절차에 맞춰 갱신했다.

## 수정됨

- VM 종료 시 PID 1 exit kernel panic 문제를 수정했다.
- restore 후 IP 충돌과 stale private IP 의존 문제를 수정했다.
- VM 생성/restore 실패 경로의 TAP/IP cleanup을 강화했다.
- COW restore 삭제 시 kernel resource 누수를 방지했다.

## 알려진 제약

- 같은 snapshot을 동시에 두 번 restore하는 흐름은 지원하지 않는다.
  snapshot state의 original vsock UDS path 제약 때문이다.
- 서로 다른 snapshot의 concurrent restore는 지원한다.
- diff snapshot은 memory만 diff다. rootfs는 snapshot마다 full copy다.
- diff restore는 임시 merged memory file을 만들 수 있는 disk space가 필요하다.
- control-plane auth 환경 변수를 설정하지 않으면 API 인증이 비활성화된다.
- GitHub tag는 공개되어 있지만 GitHub Release page는 아직 게시하지 않았다.

# ephemera v0.1.0 — 초기 구현

ephemera `v0.1.0`은 초기 proof-of-concept 릴리즈다. 단일 host에서
Firecracker MicroVM을 만들고, 그 안에서 Goose task를 실행하는 기본
경로를 제공했다.

## 포함된 기능

- Go 기반 control-plane daemon: `cmd/goose-daemon`
- Firecracker MicroVM 생성
- Debian Bookworm minbase golden image build
- first-run bootstrap:
  - golden rootfs build
  - Firecracker binary download
  - Linux kernel download
  - guest agent build
- host bridge `goose-br0`
- private network `10.0.1.0/24`
- outbound NAT
- TAP/IP allocation and recycling
- VM별 writable rootfs clone
- Goose config와 secret injection
- guest-side `goose-agent` HTTP server
- API:
  - `POST /vms`
  - `GET /vms`
  - `DELETE /vms/{vm_id}`
  - guest direct `POST /tasks`
  - guest direct `GET /health`
- 초기 e2e smoke test

## v0.1.0 제약

- API 인증이 없었다.
- VM별 agent token이 없었다.
- 외부 client가 guest private IP에 직접 접근해야 했다.
- snapshot/restore가 없었다.
- diff memory snapshot이 없었다.
- COW rootfs restore가 없었다.
- VM별 LLM profile이 없었다.
- graceful guest shutdown이 없어서 종료 시 kernel panic이 발생할 수 있었다.
- public reverse proxy URL model이 없었다.
- MCP/IronClaw adapter가 없었다.

## 사전 요구사항

- Linux host with `/dev/kvm`
- root 또는 sudo 권한
- `curl`
- `debootstrap`
- `e2fsprogs`
- `util-linux`
- Go 1.25 이상
