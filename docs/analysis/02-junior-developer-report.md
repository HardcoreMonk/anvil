# Ephemera 주니어 개발자 실무 투입 보고서

분석 기준 커밋: `157753fb5234679ca7cbebb6658e431c6a748ef6`

## 한 문장 요약

Ephemera는 호스트에서 Firecracker MicroVM을 만들고, 각 VM 안에서 Goose AI agent를 실행하게 해, 작업이 끝나면 VM 디스크/네트워크를 지워 격리된 AI 작업 환경을 제공하는 Go 백엔드 프로젝트다.

## 아키텍처 분해

### 프론트엔드

프론트엔드 애플리케이션은 없다. React/Vue/Next.js 같은 UI 코드도 없다. 사용자는 HTTP API, `curl`, 또는 `simple_test_scenario.sh` 같은 shell script로 시스템을 다룬다.

### 백엔드

백엔드는 두 바이너리로 나뉜다.

- `goose-daemon`: 호스트에서 실행된다. VM lifecycle API를 제공한다.
- `goose-agent`: VM 내부에서 실행된다. Goose 작업 실행 API를 제공한다.

`goose-daemon` API:
- `POST /vms`: VM 생성
- `GET /vms`: 실행 중인 VM 목록
- `DELETE /vms/{vm_id}`: VM 삭제 및 자원 정리

`goose-agent` API:
- `POST /tasks`: prompt를 Goose에 전달하고 결과 반환
- `GET /health`: `idle` 또는 `busy`
- `POST /stop`: agent 서버 종료

### 코어 로직

- `internal/storage/provisioner.go`: 이미지/디스크/설정 주입 담당
- `internal/network/manager.go`: IP/TAP/bridge/NAT 담당
- `internal/vm/machine.go`: Firecracker VM boot 담당
- `cmd/goose-daemon/api.go`: 위 세 코어 모듈을 조합해 API 요청을 처리

## 실행 흐름

### daemon 시작

1. `cmd/goose-daemon/main.go`가 실행된다.
2. 현재 작업 디렉터리 기준으로 artifact/config 경로를 잡는다.
3. `artifacts/goose-agent`가 없으면 `cmd/goose-agent`를 Linux amd64 바이너리로 빌드한다.
4. golden image가 없으면 `scripts/build_image.sh`로 Debian Bookworm rootfs를 만든다.
5. kernel과 Firecracker binary를 준비한다.
6. Linux bridge `goose-br0`와 NAT를 준비한다.
7. `127.0.0.1:3000` 기본 주소로 control plane HTTP API를 시작한다.

### VM 생성

1. client가 `POST /vms`를 호출한다.
2. `api.go`의 `spawnVM`이 `vm-{timestamp}` ID를 만든다.
3. `network.Manager.Allocate()`가 guest IP, TAP device, MAC 주소를 할당한다.
4. `storage.Provisioner.CloneDisk()`가 golden image를 VM 전용 ext4 disk로 복사한다.
5. `storage.Provisioner.PrepareVM()`이 Goose 설정과 secrets를 VM disk에 복사한다.
6. `vm.StartMachine()`이 Firecracker SDK로 VM을 boot한다.
7. `waitForAgent()`가 `http://{guestIP}:8080/health`를 polling한다.
8. 준비되면 `{vm_id, guest_ip, agent_url}` JSON을 반환한다.

### Task 실행

1. client가 `agent_url`의 `/tasks`로 prompt를 보낸다.
2. `goose-agent`가 busy flag를 확인한다.
3. busy가 아니면 `/usr/local/bin/goose run -i -`를 실행한다.
4. prompt를 stdin으로 넣는다.
5. Goose output과 command error를 JSON으로 반환한다.

### VM 삭제

1. client가 `DELETE /vms/{vm_id}`를 호출한다.
2. control plane이 VM map에서 VM을 제거한다.
3. Firecracker VM을 `StopVMM()`으로 종료한다.
4. socket, log FIFO, VM disk를 삭제한다.
5. TAP device를 삭제하고 IP를 pool에 반환한다.

## 파일별 책임

`cmd/goose-daemon/main.go`
- 애플리케이션 조립 코드다.
- artifact 경로와 상수를 정의한다.
- storage/network/control plane을 초기화한다.
- OS signal을 기다리고 종료 시 모든 VM을 정리한다.

`cmd/goose-daemon/config.go`
- 환경변수를 읽는다.
- API address, agent port, API token 목록을 결정한다.
- token은 시작 시 한 번만 읽는다.

`cmd/goose-daemon/api.go`
- 인증 middleware를 제공한다.
- VM lifecycle HTTP API를 구현한다.
- 실행 중 VM 상태 map을 mutex로 보호한다.
- VM 생성 실패 단계마다 이미 할당한 자원을 되돌린다.

`cmd/goose-agent/main.go`
- VM 내부 HTTP server다.
- Goose CLI를 subprocess로 실행한다.
- 한 VM에서 동시에 하나의 task만 실행되도록 busy flag를 둔다.

`internal/storage/provisioner.go`
- golden image 존재를 보장한다.
- VM별 disk clone을 만든다.
- VM disk를 mount해서 config/secrets/timezone을 주입한다.
- kernel/Firecracker/goose-agent artifact를 준비한다.

`internal/network/manager.go`
- `10.0.1.0/24` IP pool을 관리한다.
- bridge `goose-br0`를 만들고 NAT를 설정한다.
- TAP device를 만들고 삭제한다.

`internal/vm/machine.go`
- Firecracker SDK 설정을 만든다.
- rootfs, kernel args, network interface, CPU/memory를 설정한다.
- VM process를 시작한다.

`scripts/build_image.sh`
- Debian Bookworm minbase rootfs를 만든다.
- Goose binary와 goose-agent를 image 안에 넣는다.
- `micro-init`을 작성한다.
- image를 shrink한다.

## 설정과 환경변수

| 변수 | 기본값 | 의미 |
|---|---|---|
| `EPHEMERA_API_ADDR` | `127.0.0.1:3000` | control plane listen address |
| `EPHEMERA_API_PORT` | `3000` | `EPHEMERA_API_ADDR`가 없을 때 쓰는 port |
| `EPHEMERA_API_TOKENS` | 없음 | `alice:token1,bob:token2` 형식의 client별 token |
| `EPHEMERA_API_TOKEN` | 없음 | 단일 token fallback |
| `EPHEMERA_AGENT_PORT` | `8080` | VM 내부 goose-agent port |
| `GOOSE_AGENT_PORT` | `8080` | goose-agent 바이너리가 직접 읽는 port |

주의할 점:
- API token이 없으면 control plane 인증이 꺼진다.
- `EPHEMERA_API_ADDR=0.0.0.0:3000`으로 열 때는 TLS reverse proxy와 token이 사실상 필수다.
- `configs/goose.yaml`, `configs/goose-secrets.yaml`은 `.gitignore`에 들어가며 commit하면 안 된다.

## 운영 전제

이 프로젝트는 일반 웹 서버보다 OS 의존성이 크다.

- Linux host가 필요하다.
- `/dev/kvm` 접근이 필요하다.
- `sudo` 또는 root 권한이 필요하다.
- `ip`, `iptables`, `mount`, `umount`, `debootstrap`, `mkfs.ext4`, `resize2fs`가 필요하다.
- Firecracker가 실행될 수 있는 CPU/커널 환경이 필요하다.

Mac/Windows에서 바로 실행하는 프로젝트가 아니다. VM 또는 bare-metal Linux 환경에서 검증해야 한다.

## 동시성 이해 포인트

- control plane의 `vms` map은 `sync.RWMutex`로 보호된다.
- network manager의 IP/TAP pool은 `sync.Mutex`로 보호된다.
- goose-agent는 전역 `busy` flag와 mutex로 동시 task 실행을 막는다.
- VM 생성 요청 자체는 HTTP server 특성상 동시에 들어올 수 있다.
- VM ID가 millisecond timestamp 기반이라 극단적으로 같은 millisecond에 두 생성 요청이 오면 충돌할 수 있다.

## 에러 처리 패턴

`spawnVM`은 단계별 rollback을 한다.

- network 할당 실패: 바로 500
- disk clone 실패: network release
- VM prepare 실패: disk cleanup + network release
- VM start 실패: disk cleanup + network release
- agent readiness 실패: `destroyVM`으로 VM/disk/network 정리

이런 패턴은 자원 관리 코드에서 매우 중요하다. 주니어 개발자는 "새 자원을 할당한 뒤 다음 단계가 실패하면 반드시 되돌리는가?"를 계속 확인해야 한다.

## 보안 포인트

좋은 점:
- control plane token 비교에 `crypto/subtle.ConstantTimeCompare`를 사용한다.
- client별 token 이름을 로그에 남긴다.
- 기본 bind address가 localhost다.
- 실제 Goose secrets 파일은 gitignore에 포함되어 있다.
- Firecracker binary는 SHA256을 검증한다.

주의점:
- token이 없으면 API가 완전히 unauthenticated다.
- goose-agent에는 인증이 없다. private subnet 격리에 의존한다.
- kernel download와 Goose tarball download는 checksum 검증이 없다.
- VM 내부 agent URL은 private IP지만 host에서 접근 가능하다. host compromise 시 agent도 접근 가능하다.
- token 변경은 daemon restart가 필요하고 running VM을 모두 종료한다.

## 테스트 상태

현재 Go unit test 파일은 없다. 있는 테스트는 `simple_test_scenario.sh` E2E 시나리오다.

E2E가 검증하는 것:
- daemon 시작
- VM 생성
- agent task 실행
- agent stop
- VM 삭제
- VM 2개 병렬 생성/작업
- resource cleanup 후 목록이 비는지 확인

부족한 테스트:
- `config.go` 환경변수 파싱 unit test
- `authMiddleware` 인증 성공/실패/WWW-Authenticate header test
- `goose-agent` busy 상태 test
- `network.Manager` IP/TAP 할당 순서와 release test
- `storage`는 외부 command 의존성이 커서 command runner abstraction 후 test 필요

## 주니어 개발자 온보딩 순서

1. `README.md`의 architecture 그림을 읽는다.
2. `cmd/goose-daemon/main.go`를 읽어 시작 순서를 이해한다.
3. `cmd/goose-daemon/api.go`의 `spawnVM`, `destroyVM`을 따라간다.
4. `internal/storage/provisioner.go`에서 disk clone과 config injection을 이해한다.
5. `internal/network/manager.go`에서 TAP/IP/bridge/NAT를 이해한다.
6. `internal/vm/machine.go`에서 Firecracker config를 이해한다.
7. `cmd/goose-agent/main.go`에서 VM 내부 API를 이해한다.
8. `scripts/build_image.sh`에서 golden image가 어떻게 만들어지는지 확인한다.
9. 마지막으로 `simple_test_scenario.sh`를 보며 end-to-end 흐름을 확인한다.

## 자주 만날 장애와 확인법

`/dev/kvm` 권한 문제:
- 증상: Firecracker VM start 실패
- 확인: `ls -l /dev/kvm`
- 대응: sudo 실행, KVM group 권한, nested virtualization 확인

bridge/TAP 생성 실패:
- 증상: `failed to create TAP device`
- 확인: `ip link show goose-br0`, `ip link show tap1`
- 대응: root 권한, 기존 device 충돌, iproute2 설치 확인

VM에서 외부 API 호출 실패:
- 증상: Goose provider API 연결 실패
- 확인: host ip forwarding, iptables MASQUERADE, VM `/etc/resolv.conf`
- 대응: NAT rule, DNS, outbound firewall 확인

agent readiness timeout:
- 증상: `goose-agent not ready after 60s`
- 확인: Firecracker stdout/stderr, rootfs에 `goose-agent` 존재 여부, micro-init 실행 권한
- 대응: `scripts/build_image.sh`, `artifacts/goose-agent`, kernel args 확인

Goose 인증 실패:
- 증상: `/tasks` output에 provider key error
- 확인: `configs/goose-secrets.yaml`
- 대응: API key와 provider/model 이름 확인

## 현업 투입 체크리스트

- Go HTTP handler의 request/response 흐름을 설명할 수 있다.
- `defer`가 cleanup에 어떻게 쓰이는지 설명할 수 있다.
- mutex가 필요한 공유 상태를 구분할 수 있다.
- Linux bridge, TAP, NAT의 역할을 그림으로 설명할 수 있다.
- Firecracker VM과 일반 Docker container의 격리 차이를 설명할 수 있다.
- 실패 시 rollback해야 하는 자원 목록을 말할 수 있다.
- secrets 파일을 commit하지 않는 이유를 설명할 수 있다.
- 운영에서 인증 없는 API bind가 왜 위험한지 설명할 수 있다.
- E2E 시나리오가 실제로 무엇을 검증하는지 설명할 수 있다.

## 개선 제안

우선순위 높은 개선:
- VM ID를 timestamp 대신 UUID/ULID로 변경한다.
- kernel과 Goose tarball checksum 검증을 추가한다.
- `authMiddleware`, config parsing, agent busy 상태 unit test를 추가한다.
- token 설정이 없을 때 production mode에서는 daemon 시작을 막는 옵션을 추가한다.
- agent API 인증 또는 host-local firewall rule을 강화한다.

중기 개선:
- VM CPU/memory/subnet/gateway를 환경변수 또는 config로 뺀다.
- storage/network command 실행을 interface로 추상화해 unit test 가능하게 만든다.
- token hot reload를 추가한다.
- VM 생성을 async job으로 바꿔 long HTTP request를 줄인다.
- OpenAPI 문서 또는 client SDK를 제공한다.

## 결론

이 프로젝트는 코드 양은 크지 않지만 OS, 네트워크, VM, 보안, 프로세스 관리가 섞여 있어 난도가 높은 백엔드 시스템이다. 주니어 개발자는 단순히 Go 문법만 보면 안 되고, "요청 하나가 어떤 host 자원을 만들고 어떤 순서로 되돌리는지"를 중심으로 이해해야 한다.
