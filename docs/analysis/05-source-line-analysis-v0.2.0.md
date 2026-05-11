# ephemera 0.2.0 소스 분석

## 문서 목적

이 문서는 0.1.0 분석 문서인 `01-source-line-analysis.md`의 후속 문서다. 0.2.0에서 바뀐 소스 구조와 실행 흐름을 개발자가 이어서 이해할 수 있도록 모듈별로 정리한다.

## 전체 구조

0.2.0은 다음 축으로 코드가 확장되었다.

- guest 내부 초기화: `cmd/micro-init`
- guest agent 인증과 vsock 제어: `cmd/goose-agent`
- control-plane API 확장: `cmd/goose-daemon`
- snapshot storage: `internal/storage/snapshot.go`
- VM restore 지원: `internal/vm/machine.go`
- 네트워크 restore 지원: `internal/network/manager.go`
- rootfs provisioning 확장: `internal/storage/provisioner.go`
- 테스트와 CI

0.1.0에서 핵심 관심사가 "VM을 만들고 명령을 실행할 수 있는가"였다면, 0.2.0의 핵심 관심사는 "VM 실행 상태를 관리 가능한 리소스로 만들 수 있는가"다.

## `cmd/micro-init/main.go`

### 역할

`micro-init`은 guest VM의 PID 1이다. Firecracker guest 안에서 가장 먼저 실행되어 mount와 agent lifecycle을 책임진다.

### 주요 흐름

1. `/proc`, `/sys`, `/dev`, `/dev/pts`를 mount한다.
2. `HOME`, `USER`, `PATH` 같은 기본 환경 변수를 설정한다.
3. `/usr/local/bin/goose-agent`를 실행한다.
4. SIGTERM, SIGINT를 받으면 agent process에 signal을 전달한다.
5. agent가 종료되면 `sync`를 호출하고 guest poweroff를 시도한다.

### 의미

이 변경 전에는 host가 VM을 종료할 때 guest 내부 process 정리와 poweroff가 명확하지 않았다. 0.2.0에서는 stop/delete가 들어왔을 때 guest 내부에서 agent가 종료되고 파일시스템 flush가 발생하는 경로가 생겼다.

### 주의점

`micro-init`은 PID 1이기 때문에 signal 처리, zombie process 처리, mount 실패 처리의 품질이 전체 guest 안정성에 영향을 준다. 현재는 최소 init에 가깝고, 복잡한 process supervisor는 아니다.

## `cmd/goose-agent/main.go`

### agent token 로딩

0.2.0 agent는 `/root/.ephemera-agent-token` 파일에서 token을 읽는다. token 문자열은 trim 처리된다.

token이 설정되어 있으면 다음 endpoint는 인증이 필요하다.

- `POST /tasks`
- `POST /stop`

다음 endpoint는 인증 없이 유지된다.

- `GET /health`

### 인증 방식

agent auth middleware는 요청의 bearer token을 읽어 rootfs에 주입된 token과 비교한다. 이 token은 VM 생성 시 daemon이 무작위로 생성한다.

이 구조는 host control-plane API token과 guest agent token을 분리한다. 외부 사용자는 control-plane token으로 daemon에 접근하고, daemon은 내부적으로 VM별 agent token을 사용한다.

### vsock IP 변경 listener

0.2.0 agent는 vsock listener도 가진다. host는 restore 후 다음 명령을 보낼 수 있다.

```text
CHANGE_IP <cidr_ip> <gateway>
```

agent는 이 명령을 받아 `eth0` 주소를 다시 설정한다.

실행되는 네트워크 작업은 다음 성격이다.

- 기존 eth0 주소 flush
- 새 CIDR 주소 추가
- link up
- default route 교체

### 의미

snapshot restore에서 가장 까다로운 부분은 memory snapshot 안의 과거 네트워크 상태다. 0.2.0은 복원 직후 vsock을 통해 guest IP를 재설정해 이 문제를 완화한다.

## `cmd/goose-daemon/config.go`

### 추가된 설정

0.2.0에서는 `EPHEMERA_PUBLIC_URL`이 추가되었다. 이 값이 있으면 VM 생성 응답의 `agent_url`이 guest private IP가 아니라 daemon public URL 기반으로 만들어진다.

예시는 다음과 같다.

```text
EPHEMERA_PUBLIC_URL=https://anvil.example.com
agent_url=https://anvil.example.com/vms/{vm_id}
```

### API client 로딩

설정 파일 기반 API client 로딩은 그대로 유지되지만, 0.2.0에서는 daemon이 SIGHUP을 받으면 client 목록을 다시 읽을 수 있다.

## `cmd/goose-daemon/main.go`

### SIGHUP 처리

daemon은 SIGHUP을 받으면 `ControlPlane.ReloadClients()`를 호출한다. 기존 running VM은 유지된다.

이 기능은 다음 운영 상황에 중요하다.

- API key rotate
- client 추가
- client 제거
- daemon restart 없이 control-plane 접근 권한 변경

### rootfs 준비

0.2.0에서는 VM rootfs에 agent뿐 아니라 micro-init도 들어가야 한다. daemon 시작 시 또는 image 준비 단계에서 관련 artifact 존재 여부가 중요해졌다.

## `cmd/goose-daemon/api.go`

0.2.0의 핵심 변경이 가장 많이 들어간 파일이다.

### VM 생성

VM 생성 요청에는 `profile`이 추가되었다. daemon은 profile 이름을 검증하고 profile별 설정 파일을 rootfs에 주입한다.

VM 생성 시 daemon은 다음도 수행한다.

- per-VM agent token 생성
- rootfs에 token 파일 작성
- running VM 상태에 profile과 token 저장
- agent URL 생성

### profile 경로 검증

profile 이름에는 경로 구분자와 traversal 패턴이 허용되지 않는다.

차단 대상:

- `/`
- `\`
- `..`

이는 profile 이름이 host filesystem path로 직접 연결되기 때문에 필수 방어다.

### agent proxy

0.2.0 daemon은 다음 요청을 VM agent로 proxy한다.

- `POST /vms/{vm_id}/tasks`
- `GET /vms/{vm_id}/health`
- `POST /vms/{vm_id}/stop`

proxy 시 `/health`를 제외하고 per-VM agent token을 bearer token으로 붙인다.

### snapshot 생성

`POST /vms/{vm_id}/snapshot`은 running VM에서 snapshot을 생성한다.

요청 옵션:

- `stop_after`
- `type`

snapshot type:

- `full`
- `diff`
- `auto`

생성 흐름:

1. VM pause
2. Firecracker snapshot 생성
3. disk copy 또는 diff metadata 준비
4. metadata 저장
5. `stop_after`가 false면 VM resume
6. `stop_after`가 true면 source VM destroy

### snapshot restore

`POST /snapshots/{snapshot_id}/restore`는 snapshot에서 새 VM을 만든다.

주요 흐름:

1. snapshot metadata 로딩
2. source VM이 아직 running이면 복원 거부
3. 새 VM ID 생성
4. 네트워크 할당 복원 또는 재할당
5. COW rootfs 준비
6. diff snapshot이면 memory diff merge
7. Firecracker restore
8. vsock으로 guest IP 재설정
9. running VM registry에 등록
10. agent health 대기

### snapshot 삭제

`DELETE /snapshots/{snapshot_id}`는 snapshot metadata와 관련 파일을 삭제한다.

단, 삭제하려는 full snapshot을 base로 참조하는 diff snapshot이 있으면 삭제를 막는다. 이 정책은 diff snapshot의 무결성을 유지하기 위해 필요하다.

### 코드상 주의할 부분

snapshot 생성 요청 body decode error를 엄격하게 400으로 처리하지 않는 경로가 있다. API 품질 관점에서는 malformed JSON을 명확하게 거부하는 편이 낫다.

device-mapper COW 설정 실패 후 legacy bind mount fallback을 사용하는 경로는 lock 범위가 약해 보인다. 0.1 분석에서 bind mount와 restore의 원자성을 강조했다면, 0.2에서도 이 부분은 회귀 위험으로 관리해야 한다.

## `internal/storage/provisioner.go`

### VMPrepareOptions

0.2.0에서는 VM rootfs 준비 옵션이 구조체로 확장되었다.

포함되는 값:

- VM ID
- profile
- agent token

### agent token 파일

provisioner는 rootfs 안에 `/root/.ephemera-agent-token`을 작성한다. 권한은 `0600`이다.

이 파일은 guest agent가 실행 시 읽는 인증 secret이다.

### micro-init 확인

rootfs에는 `goose-agent`뿐 아니라 `micro-init`도 들어가야 한다. 따라서 provisioner에는 micro-init artifact 존재와 설치를 확인하는 경로가 추가되었다.

## `internal/storage/snapshot.go`

0.2.0에서 새로 추가된 snapshot storage 핵심 파일이다.

### SnapshotMetadata

metadata는 snapshot restore에 필요한 host/guest 상태를 저장한다.

중요 필드:

- snapshot ID
- source VM ID
- profile
- snapshot type
- base snapshot ID
- guest IP
- TAP device
- vsock path
- MAC address
- agent token
- disk path
- memory file path
- disk copy path
- 생성 시각

### metadata 저장

metadata는 JSON으로 저장되며 파일 권한은 `0600`이다. agent token이 포함되므로 이 권한은 중요하다.

### disk copy

full snapshot에는 disk copy가 포함된다. diff snapshot에서는 base snapshot과의 관계가 중요해진다.

### COW rootfs

`SetupDMSnapshot`은 device-mapper snapshot을 구성한다.

구성 요소:

- base disk loop device
- COW exception store
- COW loop device
- `/dev/mapper/cow-*`
- bind mount

이 경로는 rootfs를 매번 full copy하지 않고도 restore VM별 쓰기 레이어를 만들기 위한 것이다.

### legacy bind mount

device-mapper snapshot 구성이 실패하면 legacy bind mount 경로가 사용된다. 이는 compatibility fallback이지만 동시성 측면에서는 조심해서 다뤄야 한다.

### memory diff merge

`MergeMemoryDiff`는 sparse file의 data/hole 정보를 사용한다. diff memory 파일의 실제 데이터 구간만 base memory 위에 복사한다.

이 구현은 diff snapshot의 저장 효율을 유지하는 데 중요하다.

## `internal/vm/machine.go`

### kernel args 변경

kernel args는 `init=/usr/local/sbin/micro-init`을 사용하도록 바뀌었다. guest boot의 책임이 micro-init으로 이동했다.

### dirty page tracking

Firecracker machine config에서 dirty page tracking을 켠다. 이는 diff snapshot에 필요하다.

### CreateSnapshot

snapshot type에 따라 Firecracker snapshot API를 호출한다. full과 diff snapshot의 구분이 생겼다.

### RestoreMachine

0.2.0에서 Firecracker restore 경로가 추가되었다.

복원 시 주의할 점:

- 기존 socket path 정리
- restore용 Firecracker process 시작
- memory snapshot과 VM state snapshot 지정
- network interface 재연결
- vsock 처리

restore 경로의 주석 일부는 실제 처리와 약간 어긋나 보이는 부분이 있다. 특히 restore 직후 vsock listener를 다시 붙이는 방식은 일반 boot와 다르기 때문에, 주석과 실제 옵션 구성을 정리할 필요가 있다.

### ReconfigureGuestIP

restore 후 guest IP를 변경하기 위해 vsock 명령을 보낸다. 이는 snapshot 내부에 남아 있는 과거 IP 상태를 새 할당 정보와 맞추기 위한 후처리다.

## `internal/network/manager.go`

### AllocateForRestore

restore 시 기존 snapshot metadata의 TAP 이름과 MAC 주소를 고려해 네트워크를 할당하는 함수가 추가되었다.

### createTapDeviceWithMAC

특정 MAC address를 가진 TAP device를 만들 수 있게 되었다. snapshot restore에서 guest의 네트워크 정체성을 유지하거나 재구성하는 데 필요하다.

### 의미

0.1.0의 네트워크 manager는 주로 신규 VM 할당을 다뤘다. 0.2.0에서는 "과거 VM 상태를 새 host runtime에 다시 연결"하는 역할까지 맡는다.

## `scripts/build_image.sh`

0.2.0 rootfs에는 다음 artifact가 들어가야 한다.

- `goose-agent`
- `micro-init`

빌드 스크립트는 `artifacts/micro-init`을 `/usr/local/sbin/micro-init`에 설치한다. 따라서 release build나 local build 과정에서 micro-init binary가 누락되면 VM boot가 실패한다.

## `.github/workflows/ci.yml`

0.2.0부터 GitHub Actions CI가 추가되었다.

실행 항목:

- Go 버전 설정
- `go build ./...`
- `go vet ./...`
- `go test ./...`

이는 0.1.0 대비 중요한 품질 관리 개선이다. 다만 Firecracker, loop device, device-mapper가 필요한 e2e는 일반 CI에서 완전히 실행되기 어렵다.

## 단위 테스트

### `cmd/goose-daemon/api_test.go`

검증 항목:

- profile 경로 기본값
- valid profile
- missing profile
- path traversal 차단
- agent token 길이
- agent token hex 형식
- token uniqueness

### `cmd/goose-daemon/config_test.go`

검증 항목:

- API client config 로딩
- 환경 변수와 파일 설정 precedence
- malformed line skip
- API address resolve

### `cmd/goose-agent/main_test.go`

검증 항목:

- agent auth middleware
- health endpoint 인증 제외
- bearer token 처리
- token file trim

### `internal/storage/provisioner_test.go`

검증 항목:

- token file 작성
- token file 권한 `0600`
- token이 비어 있을 때 파일 미작성

일부 테스트는 root 권한이나 loop mount 기능이 필요할 수 있다.

## e2e 테스트

`e2e_test.sh`는 50단계 수준의 통합 시나리오를 제공한다.

검증 범위:

- daemon startup
- single VM lifecycle
- task execution
- parallel VM task
- full snapshot
- restore
- token preservation
- diff snapshot
- sparse COW allocation
- agent proxy
- public URL behavior
- daemon shutdown

0.1.0의 단순 시나리오보다 훨씬 실제 운영 흐름에 가깝다.

## 데이터 흐름 요약

### VM 생성

```text
client
  -> goose-daemon POST /vms
  -> profile 검증
  -> agent token 생성
  -> rootfs copy/provision
  -> token/profile/micro-init 주입
  -> network allocate
  -> Firecracker boot
  -> agent health wait
  -> VMSpawnResult 반환
```

### task 실행

```text
client
  -> goose-daemon POST /vms/{id}/tasks
  -> daemon auth
  -> running VM lookup
  -> per-VM agent token 주입
  -> guest agent /tasks proxy
  -> response relay
```

### snapshot 생성

```text
client
  -> goose-daemon POST /vms/{id}/snapshot
  -> VM pause
  -> Firecracker snapshot
  -> disk copy or diff metadata
  -> snapshot metadata save
  -> VM resume or destroy
  -> SnapshotInfo 반환
```

### snapshot restore

```text
client
  -> goose-daemon POST /snapshots/{id}/restore
  -> metadata load
  -> network allocation
  -> COW disk setup
  -> memory diff merge if needed
  -> Firecracker restore
  -> guest IP reconfigure via vsock
  -> agent health wait
  -> VMRestoreResult 반환
```

## 개발자 관점 결론

0.2.0은 ephemera core runtime을 크게 전진시켰다. 이제 daemon은 단순 VM launcher가 아니라 single-host execution control plane에 가깝다.

가장 중요한 구현 변화는 세 가지다.

- guest lifecycle을 `micro-init`으로 명확히 관리한다.
- daemon이 guest agent 접근을 proxy하고 VM별 token을 관리한다.
- snapshot/restore가 storage, network, VM, agent를 가로지르는 일급 기능이 되었다.

다음 개발에서 가장 조심해야 할 부분은 snapshot restore의 동시성, COW fallback 경로, token lifecycle, e2e 재현성이다.
