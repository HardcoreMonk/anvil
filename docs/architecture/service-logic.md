# ephemera 서비스 로직

## 상태

- 기준 버전: `v0.2.0`
- 범위: ephemera daemon HTTP 동작, VM lifecycle, agent proxy, snapshot lifecycle,
  guest agent 동작
- 제외 범위: IronClaw MCP client 동작. 해당 내용은
  [mcp-architecture.md](mcp-architecture.md)를 참조한다.

이 문서는 각 service operation이 수행하는 일과 반드시 지켜야 할 invariant를
설명한다. 파일 수준 런타임 구조는
[runtime-architecture.md](runtime-architecture.md)에 정리한다.

## 서비스 경계

control plane daemon은 하나의 HTTP service를 노출한다.

| API group | 소유자 | 목적 |
|---|---|---|
| `/vms` | `cmd/goose-daemon/api.go` | VM 생성, 목록, 삭제 |
| `/vms/{vm_id}/tasks` | `cmd/goose-daemon/api.go` | guest agent로 task 실행 proxy |
| `/vms/{vm_id}/workspace` | `cmd/goose-daemon/api.go` | guest `/workspace` 단일 파일 read/write proxy |
| `/vms/{vm_id}/health` | `cmd/goose-daemon/api.go` | guest health proxy |
| `/vms/{vm_id}/stop` | `cmd/goose-daemon/api.go` | guest agent에 stop 요청 |
| `/vms/{vm_id}/snapshot` | `cmd/goose-daemon/api.go` | full 또는 diff VM snapshot 생성 |
| `/snapshots` | `cmd/goose-daemon/api.go` | 저장된 snapshot 목록 |
| `/snapshots/gc` | `cmd/goose-daemon/api.go` | snapshot retention GC dry-run/apply |
| `/snapshots/{id}/restore` | `cmd/goose-daemon/api.go` | snapshot에서 VM restore |
| `/snapshots/{id}` | `cmd/goose-daemon/api.go` | snapshot 삭제 |

VM 내부의 `goose-agent`는 다음 endpoint를 제공한다.

| Endpoint | Auth | 목적 |
|---|---|---|
| `POST /tasks` | VM별 Bearer token | Goose prompt 실행 |
| `PUT /workspace?path=...` | VM별 Bearer token | `/workspace` 아래 단일 파일 쓰기 |
| `GET /workspace?path=...` | VM별 Bearer token | `/workspace` 아래 단일 파일 읽기 |
| `GET /health` | 없음 | `idle` 또는 `busy` 반환 |
| `POST /stop` | VM별 Bearer token | agent HTTP server graceful stop |

외부 caller는 private guest IP에 직접 접근하기보다 control plane proxy endpoint를
사용해야 한다.

## Runtime 설정 alias

daemon은 기존 `EPHEMERA_*` 환경 변수를 canonical 계약으로 유지하면서 다음
`ANVIL_*` alias를 fallback으로 인식한다.

| Canonical | Alias |
|---|---|
| `EPHEMERA_API_ADDR` | `ANVIL_API_ADDR` |
| `EPHEMERA_API_PORT` | `ANVIL_API_PORT` |
| `EPHEMERA_API_TOKENS` | `ANVIL_API_TOKENS` |
| `EPHEMERA_API_TOKEN` | `ANVIL_API_TOKEN` |
| `EPHEMERA_AGENT_PORT` | `ANVIL_AGENT_PORT` |
| `EPHEMERA_PUBLIC_URL` | `ANVIL_PUBLIC_URL` |

각 설정은 canonical 값이 비어 있을 때만 alias 값을 사용한다. 이 규칙은 기존
ephemera 배포의 동작을 보존하면서 anvil 운영 문서에서 `ANVIL_*` 이름을 사용할 수
있게 한다.

## 제어 평면 인증

모든 control-plane route는 `authMiddleware`로 감싼다.

```text
incoming request
  -> cp.getClients()로 현재 API client list 읽기
  -> client가 설정되어 있지 않으면 요청 허용
  -> Authorization header를 모든 등록 token과 비교
  -> 인증 실패 시 401 JSON body 반환
  -> matched client name을 log에 남기고 route handler 호출
```

token 비교는 constant-time comparison을 사용하고 첫 후보에서 멈추지 않는다.
partial token match가 timing으로 새지 않게 하기 위한 선택이다.

`SIGHUP`은 `ControlPlane.ReloadClients`를 호출한다. daemon 재시작이나 실행 중
VM 중단 없이 `EPHEMERA_API_TOKENS`/`ANVIL_API_TOKENS` 또는
`EPHEMERA_API_TOKEN`/`ANVIL_API_TOKEN`을 메모리에 다시 로드한다.

환경 변수 precedence:

```text
EPHEMERA_API_TOKENS
  -> ANVIL_API_TOKENS
  -> EPHEMERA_API_TOKEN
  -> ANVIL_API_TOKEN
  -> 인증 비활성화
```

`EPHEMERA_*`는 ephemera runtime의 canonical 설정이고 `ANVIL_*`는 anvil 운영자를
위한 alias다. canonical 값이 있으면 alias 값보다 우선한다.

## VM 생성 로직

Route: `POST /vms`

입력:

```json
{
  "profile": "optional-profile-name"
}
```

흐름:

```text
spawnVM()
  -> optional JSON body decode
  -> profile name trim
  -> config/secrets path 해석
       empty profile -> configs/goose.yaml + configs/goose-secrets.yaml
       named profile -> configs/profiles/<name>/{goose.yaml,goose-secrets.yaml}
       slash/backslash가 있는 profile name 거부
  -> 32-byte random agent token 생성
  -> TAP, guest IP, MAC 할당
  -> golden image를 /tmp/goose-workspaces/<vm_id>.ext4로 clone
  -> disk를 한 번 mount해 config, secrets, token, timezone 주입
  -> Firecracker API socket과 vsock UDS path 생성
  -> vm.StartMachine()으로 Firecracker 시작
  -> cp.vms에 VM 등록
  -> 최대 60초 동안 http://<guest_ip>:8080/health poll
  -> vm_id, guest_ip, agent_url, profile, agent_token 반환
```

실패 cleanup:

| 실패 지점 | Cleanup |
|---|---|
| Network allocation 실패 | `500` 반환 |
| Disk clone 실패 | TAP/IP 반환 |
| Disk preparation 실패 | cloned disk 삭제, TAP/IP 반환 |
| Firecracker start 실패 | cloned disk 삭제, TAP/IP 반환 |
| Agent readiness 실패 | `cp.destroyVM`으로 VM 제거 |

응답의 `agent_token`은 민감 정보다. control plane은 proxy 호출을 위해 token을
메모리에 보관한다.

## VM 목록 로직

Route: `GET /vms`

```text
listVMs()
  -> cp.vms를 lock 아래에서 읽기
  -> []VMInfo 반환
```

목록 응답에는 `agent_token`을 포함하지 않는다.

## VM 삭제 로직

Route: `DELETE /vms/{vm_id}`

```text
stopVM()
  -> vm_id 존재 확인
  -> cp.destroyVM(vm_id)
  -> {"status":"stopped","vm_id":"..."} 반환
```

실제 teardown은 `destroyVM`이 수행한다.

```text
destroyVM()
  -> cp.vms lock 아래에서 VM 제거
  -> StopVMM()
       Firecracker가 SIGTERM 전송
       micro-init이 signal 수신
       micro-init이 goose-agent 종료 요청
       micro-init이 poweroff(2) 호출
  -> Firecracker socket/log/vsock file 삭제
  -> COW-restored VM이면 TeardownDMSnapshot()
  -> legacy bind restore이면 TeardownBindMount()
  -> 일반 VM이면 cloned ext4 disk 삭제
  -> TAP/IP를 network.Manager로 반환
```

## Agent proxy 로직

Routes:

- `POST /vms/{vm_id}/tasks`
- `GET /vms/{vm_id}/health`
- `POST /vms/{vm_id}/stop`

```text
proxyAgentEndpoint()
  -> vm_id로 실행 중인 VM 찾기
  -> private target URL http://<guest_ip>:8080/<agent_path> 구성
  -> incoming context와 body로 새 request 생성
  -> Content-Type 보존
  -> /health가 아니면 "Authorization: Bearer <agent_token>" 주입
  -> cp.agentHTTPClient로 request 전송
  -> response header, status code, body를 caller에게 복사
```

proxy는 외부 caller에게 하나의 인증 모델만 노출한다. caller는 control plane에만
인증하고, daemon이 필요한 guest agent token을 내부적으로 주입한다.

## Snapshot 유형 선택

`resolveSnapshotType(req.Type, vmID)`는 다음 규칙을 적용한다.

| 요청 type | 결과 |
|---|---|
| `"full"` | full snapshot 생성 |
| `"diff"`이고 기존 full base 있음 | latest full snapshot을 참조하는 diff snapshot 생성 |
| `"diff"`이지만 full base 없음 | error 반환 |
| 비어 있거나 unknown이고 full base 없음 | full snapshot 생성 |
| 비어 있거나 unknown이고 full base 있음 | latest full snapshot을 참조하는 diff snapshot 생성 |

latest full snapshot은 같은 `source_vm_id`를 가진 snapshot 중 `CreatedAt` 기준으로
선택한다.

## Snapshot 생성 로직

Route: `POST /vms/{vm_id}/snapshot`

입력:

```json
{
  "stop_after": false,
  "type": "full | diff | optional"
}
```

흐름:

```text
createSnapshot()
  -> optional body parse
  -> 실행 중인 VM 찾기
  -> full/diff type과 base snapshot ID 결정
  -> snapshots/<snapshot_id>/ 생성
  -> VM pause
  -> CreateSnapshot(memory.bin, state.bin)
       diff snapshot은 Firecracker SnapshotType="Diff" 전달
  -> pause 상태에서 /tmp/goose-workspaces/<vm_id>.ext4를 rootfs.ext4로 copy
  -> stop_after=false이면 VM resume
  -> stop_after=true이면 source VM destroy
  -> metadata.json 작성
  -> cp.snapshots에 metadata 추가
  -> public SnapshotInfo 반환
```

중요 invariant:

- disk copy는 VM이 pause된 상태에서 수행한다.
- diff snapshot도 rootfs는 full copy한다. memory만 sparse/diff다.
- `metadata.json`은 tenant ID, egress policy, original TAP name, MAC, vsock path,
  agent token, disk path, memory path, state path, base snapshot ID를 보존한다.
- snapshot API response에는 `agent_token`을 노출하지 않는다.

### Snapshot token 수명 주기

snapshot metadata는 guest agent token을 저장한다. restore된 VM이 기존
`goose-agent` 인증 계약을 그대로 유지해야 하므로, snapshot에서 복원한 VM은
metadata에 있던 원래 token을 계속 사용한다.

공개 API 응답은 이 값을 노출하지 않는 것이 정책이며, 허용된 노출 지점은
`POST /vms` 응답뿐이다. `POST /snapshots/{id}/restore`, snapshot 생성, snapshot
목록, snapshot GC, MCP output, audit output은 실제 `agent_token`을 포함하지 않는다.

snapshot metadata를 반출하거나 백업 workflow에서 신뢰된 host 경계 밖으로
보낼 때는 먼저 `scripts/snapshot-metadata-scrub.go`로 `agent_token`을 제거한다.

```bash
go run ./scripts/snapshot-metadata-scrub.go -input snapshots/snap-.../metadata.json > metadata.scrubbed.json
```

token 회전은 생성 시점에만 명확하다. 새 VM은 새 guest agent token을 받지만,
기존 snapshot restore는 snapshot metadata의 원래 token을 유지한다. 이미 만들어진
snapshot의 token을 회전하려면 향후 guest-side rekey와 metadata rewrite 설계가
필요하며, 이 문서의 구현 범위에는 포함되지 않는다.

## Snapshot restore 로직

Route: `POST /snapshots/{id}/restore`

```text
restoreSnapshot()
  -> cp.snapshots에서 snapshot metadata load
  -> request tenant_id/egress_policy가 metadata와 충돌하면 reject
  -> source VM이 아직 실행 중이면 reject
  -> 새 VM ID 할당
  -> stale Firecracker socket 제거
  -> snapshot metadata의 original vsock UDS path 제거
  -> AllocateForRestore(original TAP, original MAC)
       original TAP name + 사용 가능한 guest IP 반환
  -> cp.restoreMu lock
  -> SetupDMSnapshot(rootfs.ext4, <new_vm_id>.cow, original disk path) 시도
       read-only loop for base rootfs 생성
       sparse exception store 생성
       dm-snapshot device 생성
       original disk path 위로 bind mount
  -> dm-snapshot 실패 시 network release 후 restoreLegacyBindMount() fallback
  -> snapshot이 diff이면 base snapshot metadata load
  -> MergeMemoryDiff(base.memory.bin, diff.memory.bin, tmp/<new_vm_id>-merged.bin)
  -> RestoreMachine(memory file, state.bin)
  -> Firecracker가 disk path를 연 뒤 cp.restoreMu unlock
  -> 임시 merged memory file 삭제
  -> ReconfigureGuestIP(original vsock path, new IP, gateway)
  -> restored VM을 cp.vms에 등록
  -> 최대 30초 동안 guest agent health 대기
  -> source_snapshot_id를 포함한 VMRestoreResult 반환
```

restore된 VM은 guest agent 연속성을 위해 snapshot metadata의 original agent token을
내부적으로 유지한다. 외부 client는 guest agent token이 아니라 control-plane
token과 daemon proxy를 사용해야 한다. restore success response에는
`source_snapshot_id`, restored VM info, tenant ID, egress policy만 포함하고
`agent_token`은 포함하지 않는다.

restore 실패는 `Content-Type: application/json`인 `RestoreErrorResponse`를
반환한다.

```json
{
  "error": "snapshot not found",
  "code": "snapshot_not_found",
  "source_snapshot_id": "snap-..."
}
```

`source_snapshot_id`는 restore 요청의 snapshot ID이며, stable `code`는 다음
값 중 하나다.

| code | 상태 |
|---|---|
| `snapshot_not_found` | 요청한 snapshot metadata 없음 |
| `source_vm_running` | source VM이 아직 실행 중 |
| `network_unavailable` | restore용 network allocation 실패 |
| `diff_base_missing` | diff snapshot의 base metadata 없음 |
| `memory_merge_failed` | diff memory merge 실패 |
| `firecracker_restore_failed` | disk setup 또는 Firecracker restore 실패 |
| `guest_reconfigure_failed` | guest IP reconfiguration 실패 |
| `agent_not_ready` | restored VM agent readiness 실패 |

실패 cleanup:

| 실패 지점 | Cleanup |
|---|---|
| Network allocation 실패 | `network_unavailable`, `409` 반환 |
| dm-snapshot setup 실패 | network release 후 bind-mount fallback 시도 |
| dm-snapshot과 bind-mount fallback 모두 실패 | network release, `firecracker_restore_failed` 반환 |
| Diff base 없음 | COW teardown, network release, `diff_base_missing`, `409` 반환 |
| Diff merge 실패 | COW teardown, network release, `memory_merge_failed` 반환 |
| Firecracker restore 실패 | COW teardown, network release, `firecracker_restore_failed` 반환 |
| Guest IP reconfiguration 실패 | Stop VMM, COW teardown, network release, `guest_reconfigure_failed` 반환 |
| Agent readiness 실패 | restored VM destroy, `agent_not_ready` 반환 |

## 기존 bind-mount restore fallback

`SetupDMSnapshot`이 실패하면 daemon은 `restoreLegacyBindMount`로 복원한다.

```text
  -> snapshot rootfs.ext4를 /tmp/goose-workspaces/<new_vm_id>.ext4로 copy
  -> state.bin의 original disk path 위로 해당 file bind mount
  -> 필요 시 diff memory merge
  -> RestoreMachine()
  -> ReconfigureGuestIP()
  -> later teardown을 위해 bindMountTarget이 있는 VM 등록
```

이 fallback은 COW restore보다 느리고 disk를 더 많이 사용하지만,
dm-snapshot을 사용할 수 없는 host에서도 restore 기능을 유지한다.

## Snapshot 삭제 로직

Route: `DELETE /snapshots/{id}`

```text
deleteSnapshot()
  -> cp.snapshots에서 base_snapshot_id == requested ID인 diff 검색
  -> 있으면 409 반환
  -> cp.snapshots에서 snapshot metadata 제거
  -> snapshots/<id>/를 disk에서 삭제
  -> {"status":"deleted","snapshot_id":"..."} 반환
```

이 규칙은 diff snapshot이 아직 필요로 하는 full snapshot을 삭제하지 못하게
막는다.

## Snapshot GC 로직

Route: `POST /snapshots/gc`

Request 예시:

```json
{
  "older_than_seconds": 604800,
  "keep_last_per_vm": 1,
  "max_total_bytes": 10737418240,
  "apply": false
}
```

Flow:

```text
handleSnapshotGC()
  -> optional JSON body parse
  -> negative older_than_seconds / keep_last_per_vm / max_total_bytes 거부
  -> cp.snapshots metadata를 복사해 CreatedAt 기준 정렬
  -> 각 storage.SnapshotDir(workDir, snapshotID)를 walk해 file Info().Size() 합산
  -> diff snapshot의 base_snapshot_id reverse reference map 생성
  -> referenced full snapshot 보호
  -> source_vm_id별 최신 keep_last_per_vm개 보호
  -> age 조건을 통과하고 보호되지 않은 snapshot을 candidates로 분류
  -> max_total_bytes > 0이면 전체 size에서 기존 candidates size를 뺀 projected
     remaining total을 계산
  -> projected remaining total이 max_total_bytes보다 크면 보호되지 않고 아직 후보가
     아닌 snapshot을 오래된 순서로 추가 후보 처리(reason=max_total_bytes)
  -> apply=false이면 plan만 반환
  -> apply=true이면 candidates를 storage.DeleteSnapshot으로 삭제하고 cp.snapshots에서 제거
  -> apply=true이면 snapshots/gc-audit.jsonl에 JSONL audit record append
```

Size 계산은 planning 보조 정보다. walk 중 파일이 사라지거나 stat할 수 없는 경우 해당
파일은 무시하고 planner는 실패하지 않는다. `candidates`, `protected`, `deleted` entry는
계산 가능한 경우 `size_bytes`를 포함한다. 이미 age 후보인 snapshot은 size pressure가
있어도 기존 `older_than` reason을 유지한다.

Audit record는 timestamp, applied, policy, candidates_count, deleted_count,
errors_count만 포함한다. snapshot metadata 전체를 기록하지 않으므로 `agent_token`이나
profile별 secret 값이 audit file에 들어가지 않는다. audit append 실패는 HTTP 200
response 안의 `errors`에 `snapshot_id: ""`, `error: "write GC audit: ..."` 형태로
추가하며 삭제 결과는 유지한다.

불변 조건:

- `apply` 기본값은 `false`다.
- 응답에 `agent_token`을 포함하지 않는다.
- diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
- `max_total_bytes`를 만족하지 못하더라도 diff snapshot이 참조 중인 full snapshot은
  보호 상태로 남긴다.
- 같은 GC 호출에서 diff를 삭제한 뒤 해당 full을 연쇄 삭제하지 않는다.
- 하나의 create/restore/delete/GC lifecycle operation만 동시에 실행된다. snapshot
  graph locking이 설계되기 전까지 restore/delete race와 진행 중인 restore 파일
  읽기 중 diff base 삭제를 피하기 위한 보수적 직렬화다.

## Guest agent 로직

`goose-agent`는 각 VM 내부에서 실행된다.

Startup:

```text
main()
  -> /root/.ephemera-agent-token 읽기
  -> vsock CHANGE_IP listener 시작
  -> /tasks, /workspace, /stop, /health 등록
  -> 기본 :8080 listen
```

Task 실행:

```text
POST /tasks
  -> POST method 요구
  -> {"prompt":"..."} decode
  -> 빈 prompt 거부
  -> busy이면 503 반환
  -> busy=true
  -> prompt를 stdin으로 넘겨 /usr/local/bin/goose run -i - 실행
  -> {"output":"..."} 또는 {"output":"...","error":"..."} 반환
  -> busy=false
```

Workspace file copy:

```text
PUT /workspace?path=<relative-path>[&overwrite=true]
  -> VM /workspace 기준 relative path 검증
  -> body가 4 MiB를 초과하면 413 JSON error
  -> overwrite=false 기본값이면 기존 file 존재 시 409 JSON error
  -> parent directory 생성
  -> raw bytes 저장
  -> {"path":"...","bytes":N} 반환

GET /workspace?path=<relative-path>
  -> VM /workspace 기준 relative path 검증
  -> file 없음이면 404 JSON error
  -> file이 4 MiB를 초과하면 413 JSON error
  -> raw bytes 반환
```

Health:

```text
GET /health
  -> {"status":"idle"} 또는 {"status":"busy"} 반환
```

Stop:

```text
POST /stop
  -> {"status":"stopping"} 반환
  -> 200 ms 뒤 HTTP server graceful shutdown
```

Vsock IP reconfiguration:

```text
CHANGE_IP <cidr_ip> <gateway>
  -> ip addr flush dev eth0
  -> ip addr add <cidr_ip> dev eth0
  -> ip link set eth0 up
  -> ip route replace default via <gateway>
  -> OK 또는 ERROR 반환
```

## Guest init 로직

`micro-init`은 VM 내부 PID 1이다.

```text
micro-init
  -> /proc, /sys, /dev, /dev/pts mount
  -> HOME, USER, PATH 설정
  -> /usr/local/bin/goose-agent 시작
  -> goose-agent exit 또는 SIGTERM/SIGINT 대기
  -> signal 수신 시 goose-agent에 SIGTERM 전송
  -> sync
  -> poweroff(2)
```

이 흐름은 PID 1이 단순 종료되면서 발생할 수 있는 guest kernel panic을 피한다.

## 오류 모델

- Control-plane auth 실패: `401`, body `{"error":"unauthorized"}`
- VM 없음: 일반적으로 `404`
- Snapshot base dependency conflict: `409`
- Snapshot restore 실패: `{"error":"...","code":"...","source_snapshot_id":"..."}`
  JSON body와 함께 `snapshot_not_found`, `source_vm_running`,
  `network_unavailable`, `diff_base_missing`, `memory_merge_failed`,
  `firecracker_restore_failed`, `guest_reconfigure_failed`, `agent_not_ready`
  중 하나를 반환한다.
- Invalid profile 또는 invalid snapshot type request: `400`
- Workspace copy error: `{"error":"..."}` JSON body와 함께 `400`, `404`,
  `409`, `413`, `500` 중 하나
- Host/runtime setup 실패: 일반적으로 `500`
- Agent proxy connection 실패: `502`

일부 legacy path는 아직 plain text body를 반환한다. MCP v1은 daemon status와
body를 보존하며, 모든 response를 새 domain model로 정규화하지 않는다.

## 운영 불변 조건

- 실행 중인 VM disk를 `destroyVM` 밖에서 삭제하거나 변경하지 않는다.
- list/snapshot/restore response에 guest `agent_token`을 노출하지 않는다.
- source VM이 실행 중인 snapshot은 restore하지 않는다.
- diff가 참조하는 full snapshot은 삭제하지 않는다.
- VM 생성 또는 restore 실패 시 항상 TAP/IP를 반환한다.
- VM 삭제 시 dm-snapshot, loop device, bind mount, sparse COW file을 정리한다.
- MCP layer에서 `anvil_stop_vm`과 `anvil_delete_vm` 의미를 구분한다.
  stop은 guest agent 중지 요청이고, delete는 host VM resource 삭제다.

## 소스 참조

- `cmd/goose-daemon/api.go`
- `cmd/goose-daemon/config.go`
- `cmd/goose-agent/main.go`
- `cmd/micro-init/main.go`
- `internal/storage/snapshot.go`
- `internal/storage/provisioner.go`
- `internal/network/manager.go`
- `internal/vm/machine.go`
