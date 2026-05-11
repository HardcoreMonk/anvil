# anvil 0.1.0 소스 줄 단위 분석 보고서

분석 기준:
- 대상 저장소: `https://github.com/HardcoreMonk/ephemera/`
- 분석 커밋: `157753fb5234679ca7cbebb6658e431c6a748ef6`
- 로컬 브랜치: `main`
- 원격 HEAD 확인: 위 커밋과 동일
- 작성일: 2026-05-09

이 문서는 애플리케이션 동작에 영향을 주는 소스, 설정, CI, 스크립트를 줄 번호 기준으로 해설한다. `README.md`, `RELEASE_NOTES.md`, `LICENSE`는 제품 설명/라이선스 문서이므로 별도 코드 로직 분석 대상에서 제외했다. `go.sum`은 Go가 자동 관리하는 의존성 체크섬 잠금 파일이라 각 줄이 "모듈 버전 + 체크섬"이라는 동일한 의미를 갖는다. 따라서 애플리케이션 로직 해설은 `go.mod`에서 하고, `go.sum`은 무결성 잠금 파일로 다룬다.

## 전체 구조

anvil은 Firecracker MicroVM 위에서 Goose AI agent를 격리 실행하는 Go 기반 백엔드 시스템이다. 이 0.1.0 분석 당시 저장소/모듈에는 `ephemera` 이름이 남아 있었다. 웹 프론트엔드 코드는 없다. 외부 사용자는 HTTP API 또는 `simple_test_scenario.sh` 같은 CLI 스크립트를 통해 제어 평면에 접근한다.

구성:
- `cmd/goose-daemon`: 호스트에서 실행되는 control plane. VM 생성, 목록 조회, 삭제를 담당한다.
- `cmd/goose-agent`: 각 MicroVM 내부에서 실행되는 HTTP agent. Goose 작업 실행, 상태 확인, 종료를 담당한다.
- `internal/storage`: golden image 생성, VM 디스크 복제, 설정/시크릿 주입, Firecracker/kernel/goose-agent artifact 준비를 담당한다.
- `internal/network`: Linux bridge, TAP device, IP pool, NAT 설정을 담당한다.
- `internal/vm`: Firecracker SDK로 VM을 실제 부팅한다.
- `configs`: Goose 설정/시크릿 예제.
- `scripts`: Debian 기반 golden image를 만드는 셸 스크립트.
- `.github/workflows`: Claude Code 관련 GitHub Actions 자동화.

요청 흐름:
1. 사용자가 `POST /vms`를 호출한다.
2. control plane이 IP/TAP을 할당한다.
3. golden image를 VM별 ext4 디스크로 복제한다.
4. Goose config/secrets/timezone을 VM 디스크에 주입한다.
5. Firecracker를 실행해 MicroVM을 부팅한다.
6. VM 내부 `goose-agent`가 `/health`에 응답할 때까지 기다린다.
7. 사용자는 반환된 `agent_url`로 `/tasks`를 호출해 Goose 작업을 실행한다.
8. `DELETE /vms/{id}`가 VM, 소켓, FIFO, 디스크, TAP, IP를 정리한다.

## `go.mod`

| 줄 | 분석 |
|---:|---|
| 1 | Go 모듈 이름을 `ephemera`로 선언한다. 내부 import 경로 `ephemera/internal/...`의 기준이다. |
| 3 | Go 언어 버전 기준을 1.18로 둔다. 최신 문법보다 Go 1.18 호환성을 우선한다. |
| 5 | 의존성 블록 시작이다. |
| 6-11 | Firecracker SDK가 끌고 오는 URL 정규화, validation, CNI 관련 간접 의존성이다. 직접 호출 코드는 거의 없다. |
| 12 | `firecracker-go-sdk v1.0.0`이 핵심 런타임 의존성이다. `internal/vm/machine.go`에서 VM 생성/시작에 사용한다. |
| 13-22 | Firecracker SDK의 OpenAPI 클라이언트 모델/검증 계층이 요구하는 간접 의존성이다. |
| 23-32 | 에러 래핑, UUID, mapstructure, tracing 등 Firecracker SDK 생태계에서 사용하는 보조 라이브러리다. |
| 33 | `logrus`는 Firecracker SDK logger를 낮은 verbosity로 제어하기 위해 사용한다. |
| 34-35 | netlink/netns 계열 의존성이다. 현재 코드는 직접 `ip` 명령을 실행하지만 SDK/CNI 의존성으로 포함된다. |
| 36-40 | MongoDB driver, x/net, x/sys, yaml 등 간접 의존성이다. 현재 애플리케이션 코드가 직접 DB나 YAML 파서를 쓰지는 않는다. |
| 41 | 의존성 블록 종료다. |

## `cmd/goose-daemon/main.go`

| 줄 | 분석 |
|---:|---|
| 1 | `main` 패키지로, 실행 바이너리 진입점임을 뜻한다. |
| 3-13 | 표준 라이브러리와 내부 `network`, `storage` 패키지를 가져온다. HTTP 서버 오류 비교, OS 신호 처리, 경로 조합이 주요 목적이다. |
| 15 | `goose-daemon` 프로세스 시작 함수다. |
| 16 | control plane 시작 로그를 출력한다. |
| 17-19 | API token이 없으면 경고한다. 이 경우 control plane API가 인증 없이 열린다. |
| 21-24 | 현재 작업 디렉터리를 얻는다. artifact와 config 경로를 cwd 기준으로 만들기 때문에 실행 위치가 중요하다. 실패하면 프로세스를 종료한다. |
| 26-32 | golden image, image build script, kernel, Firecracker, goose-agent, Goose config/secrets 경로를 고정한다. 모두 cwd 기준이다. |
| 34-38 | 커널/Firecracker 다운로드 URL과 Firecracker tarball SHA256을 상수로 정의한다. 커널은 checksum 검증이 없고 Firecracker만 검증된다. |
| 40-44 | VM 이미지에 넣을 `goose-agent` Linux amd64 바이너리를 먼저 보장한다. 없으면 `storage.EnsureGooseAgent`가 빌드한다. |
| 46-51 | storage provisioner를 만든다. 이 과정에서 workspace directory 생성과 golden image 존재 확인/생성이 일어난다. |
| 53-56 | Firecracker 호환 Linux kernel artifact를 보장한다. 없으면 다운로드한다. |
| 58-61 | Firecracker VMM binary artifact를 보장한다. 없으면 다운로드, SHA256 검증, 압축 해제를 수행한다. |
| 63-65 | network manager를 초기화한다. `10.0.1.0/24` 내부망과 gateway `10.0.1.1`을 사용한다. |
| 67-72 | control plane 객체를 만든다. storage, network, artifact 경로, config 경로, workDir를 주입한다. |
| 73 | daemon 종료 시 모든 VM을 파괴하도록 예약한다. |
| 75-79 | HTTP API 서버를 goroutine으로 실행한다. 서버 오류는 로그에 남긴다. |
| 80 | main 종료 시 HTTP 서버도 graceful shutdown한다. |
| 82-85 | SIGINT/SIGTERM을 기다린다. systemd/터미널 종료 신호를 받으면 종료 흐름으로 넘어간다. |
| 86-87 | 종료 로그를 남기고 defer들이 실행되도록 `main`을 빠져나간다. |

핵심 판단: `main.go`는 조립 계층이다. 실제 비즈니스 로직은 `storage`, `network`, `vm`, `api.go`로 분리되어 있다.

## `cmd/goose-daemon/config.go`

| 줄 | 분석 |
|---:|---|
| 1 | daemon 실행 바이너리의 `main` 패키지 일부다. |
| 3-8 | 환경변수 읽기, 포트 숫자 변환, 문자열 파싱을 위한 표준 라이브러리를 가져온다. |
| 10-13 | control plane 기본 포트 3000, VM 내부 agent 기본 포트 8080을 정의한다. |
| 15-20 | `APIClient` 구조체다. client별 이름과 Bearer token을 묶어 감사 로그와 개별 폐기를 가능하게 한다. |
| 22-37 | 패키지 전역 설정값이다. 프로세스 시작 시 한 번만 계산된다. |
| 23-25 | VM 내부 `goose-agent` 포트를 `EPHEMERA_AGENT_PORT`에서 읽고, 없으면 8080을 쓴다. |
| 27-31 | control plane listen address를 결정한다. 기본값은 localhost 전용이다. |
| 33-36 | API client/token 목록을 환경변수에서 읽는다. 비어 있으면 인증이 꺼진다. |
| 39-48 | `loadAPIClients`의 입력 형식을 주석으로 문서화한다. 다중 client 토큰이 우선이다. |
| 49-63 | `EPHEMERA_API_TOKENS`를 `alice:token1,bob:token2` 형식으로 파싱한다. 콜론이 없거나 이름이 비어 있는 항목은 건너뛴다. |
| 64-67 | 레거시 단일 토큰 `EPHEMERA_API_TOKEN`을 `default` client로 변환한다. |
| 68-69 | 토큰 환경변수가 없으면 `nil`을 반환한다. 이 값은 인증 비활성화로 해석된다. |
| 71-78 | `EPHEMERA_API_ADDR`가 있으면 그대로 쓰고, 없으면 `127.0.0.1:{port}`를 만든다. |
| 80-87 | 환경변수를 양의 정수로 파싱하는 공통 helper다. 실패하면 기본값을 반환한다. |

핵심 판단: 설정은 hot reload되지 않는다. 토큰 변경은 daemon 재시작이 필요하고, 재시작 시 `DestroyAll` 때문에 VM이 모두 종료된다.

## `cmd/goose-daemon/api.go`

| 줄 | 분석 |
|---:|---|
| 1 | daemon API 구현도 `main` 패키지에 속한다. |
| 3-20 | HTTP, JSON, 동시성, 시간, Firecracker SDK, 내부 패키지를 가져온다. |
| 22-32 | 인증 middleware의 보안 의도를 설명한다. client별 token, audit log, constant-time 비교를 강조한다. |
| 33-36 | token 목록이 비어 있으면 요청을 그대로 통과시킨다. 운영에서는 위험하므로 reverse proxy 외부 노출 전 반드시 token을 설정해야 한다. |
| 38-49 | 각 client token을 `"Bearer "+token` 바이트 배열로 미리 만든다. 요청마다 문자열 조합을 피한다. |
| 51-52 | 실제 HTTP handler wrapper를 반환하고 요청의 Authorization header를 읽는다. |
| 54-61 | 모든 등록 token과 constant-time 비교를 수행한다. 일치 client가 있어도 루프를 끝까지 돈다. |
| 63-68 | 일치 token이 없으면 JSON 형태의 401 응답을 보낸다. |
| 69-71 | 인증된 client 이름, method, path를 로그로 남기고 다음 handler로 넘긴다. |
| 74-80 | VM 생성 응답 DTO다. VM ID, guest IP, agent URL을 JSON으로 반환한다. |
| 82-87 | 실행 중인 VM의 내부 상태다. 외부 응답 정보에 Firecracker machine, TAP device, socket path를 덧붙인다. |
| 89-106 | `ControlPlane` 구조체다. VM map, storage/network 의존성, artifact 경로, HTTP 서버를 가진다. |
| 108-123 | control plane 생성자다. 의존성을 주입하고 VM map, stop channel을 초기화한다. |
| 125-129 | HTTP route를 등록한다. `/vms`와 `/vms/`만 있다. 인증 middleware가 전체 mux를 감싼다. |
| 132-146 | 서버 시작 함수다. 인증 상태와 API 사용법을 로그로 보여주고 `ListenAndServe`를 호출한다. |
| 149-153 | HTTP 서버 graceful shutdown이다. 최대 5초 동안 기존 요청 정리를 기다린다. |
| 155 | stop channel getter다. 현재 코드에서는 외부에서 사용되지 않는다. |
| 157-168 | `/vms` handler다. POST는 VM 생성, GET은 VM 목록, 그 외는 405를 반환한다. |
| 170-182 | `/vms/{vm_id}` handler다. DELETE만 허용하고, path에서 VM ID를 잘라 `stopVM`으로 보낸다. |
| 184-185 | VM 생성 로직 시작이다. millisecond timestamp 기반 VM ID를 만든다. 동시에 같은 millisecond 요청이 들어오면 충돌 가능성이 있다. |
| 187-191 | network manager에서 TAP device, guest IP, MAC 주소를 할당받는다. 실패하면 500을 반환한다. |
| 193-198 | golden image를 VM 전용 디스크로 복제한다. 실패 시 network 할당을 되돌린다. |
| 200-205 | Goose config/secrets/timezone을 VM 디스크에 주입한다. 실패 시 디스크와 network 자원을 모두 정리한다. |
| 207-208 | Firecracker API socket path를 `/tmp`에 만들고 오래된 socket을 제거한다. |
| 210-220 | VM 부팅에 필요한 `vm.VMConfig`를 채워 `vm.StartMachine`을 호출한다. gateway는 코드에 `10.0.1.1`로 고정되어 있다. |
| 221-226 | VM 시작 실패 시 디스크와 network를 정리하고 500을 반환한다. socket 제거는 이 실패 경로에는 없다. |
| 228-232 | 외부 caller에게 반환할 VM 정보를 만든다. |
| 234-236 | VM map에 실행 상태를 저장한다. mutex로 동시 접근을 보호한다. |
| 238-243 | VM 내부 `goose-agent` `/health`가 OK가 될 때까지 최대 60초 기다린다. 실패하면 `destroyVM`으로 모든 자원을 정리한다. |
| 244-249 | VM 준비 완료 로그를 남기고 201 JSON 응답을 반환한다. |
| 251-260 | VM 목록 조회다. 읽기 lock으로 map을 순회하고 `VMInfo` 배열을 JSON으로 반환한다. |
| 263-275 | VM 삭제 API handler다. 존재 여부를 확인하고 `destroyVM`을 실행한 뒤 stopped JSON을 반환한다. |
| 277-283 | `destroyVM` 시작부다. 쓰기 lock으로 VM map에서 항목을 삭제한다. |
| 285-287 | 이미 없는 VM이면 조용히 반환한다. |
| 288-295 | Firecracker VM 종료, socket/FIFO 삭제, 디스크 삭제, TAP/IP 반환을 수행한다. |
| 296-297 | VM destroyed 로그를 남긴다. |
| 299-310 | daemon 종료 시 모든 VM ID를 복사한 뒤 하나씩 파괴한다. lock을 잡은 상태로 오래 걸리는 삭제를 하지 않기 위해 ID만 복사한다. |
| 312-327 | agent readiness polling이다. 500ms마다 `http://guestIP:agentPort/health`를 호출하고 200 OK가 오면 성공한다. |

핵심 판단: control plane은 task traffic을 proxy하지 않는다. VM lifecycle만 관리하고, Goose 작업은 caller가 VM private IP의 `goose-agent`로 직접 보낸다.

## `cmd/goose-agent/main.go`

| 줄 | 분석 |
|---:|---|
| 1 | VM 내부에서 실행되는 별도 바이너리의 `main` 패키지다. |
| 3-15 | HTTP 서버, JSON, 외부 명령 실행, mutex, graceful shutdown에 필요한 표준 라이브러리를 가져온다. |
| 17-19 | `/tasks` 요청 body 구조다. `prompt` 문자열만 받는다. |
| 21-24 | `/tasks` 응답 구조다. Goose 표준 출력/표준 에러 결합 결과와 optional error를 담는다. |
| 26-30 | agent 전역 상태다. `busy`로 동시 작업을 막고 `srv`로 shutdown을 수행한다. |
| 32-40 | `GOOSE_AGENT_PORT`를 읽어 listen address를 만든다. 유효하지 않으면 8080을 사용한다. |
| 42-47 | HTTP mux에 `/tasks`, `/stop`, `/health` endpoint를 등록한다. |
| 48-53 | 서버를 시작한다. `http.ErrServerClosed`는 정상 shutdown으로 간주한다. |
| 56-60 | `/tasks`는 POST만 허용한다. |
| 62-66 | JSON body를 decode하고 prompt가 비어 있으면 400을 반환한다. |
| 68-75 | mutex로 busy 상태를 확인/설정한다. 이미 작업 중이면 503을 반환한다. |
| 76-80 | handler가 끝날 때 busy를 false로 되돌린다. |
| 82-84 | `/usr/local/bin/goose run -i -`를 실행하고 prompt를 stdin으로 넣는다. 요청 context가 취소되면 command도 취소된다. |
| 86-90 | stdout/stderr 결합 출력과 command error를 응답 구조로 만든다. 에러가 있으면 HTTP 500을 먼저 쓴다. |
| 91-92 | JSON 응답을 반환한다. 에러 상황에서도 output은 같이 내려간다. |
| 95-99 | `/stop`은 POST만 허용한다. |
| 100-107 | 즉시 `{status:"stopping"}`을 반환한 뒤 goroutine에서 200ms 후 HTTP 서버를 shutdown한다. |
| 110-119 | `/health`는 busy 여부를 읽고 `idle` 또는 `busy` JSON을 반환한다. method 제한은 없다. |

핵심 판단: VM 내부 agent 자체에는 인증이 없다. 설계상 private subnet에서 host/client만 접근한다는 전제다.

## `internal/storage/provisioner.go`

| 줄 | 분석 |
|---:|---|
| 1 | storage 내부 패키지다. 외부 command 패키지에서 import한다. |
| 3-16 | tar/gzip, sha256, 파일 IO, HTTP 다운로드, 외부 command 실행, 경로/문자열 처리를 가져온다. |
| 18-23 | `Provisioner` 구조체다. golden image 위치, per-VM workspace, build script 위치를 보관한다. |
| 25-44 | 생성자다. workspace directory를 만들고 golden image 존재를 보장한다. |
| 27-30 | workspace directory 생성 실패 시 wrapping된 error를 반환한다. |
| 32-36 | 입력 경로들을 구조체에 저장한다. |
| 38-41 | golden image가 없으면 build script를 실행해 생성한다. |
| 43-44 | 초기화 성공 시 provisioner를 반환한다. |
| 46-52 | golden image 파일이 이미 있으면 build를 건너뛴다. |
| 54-56 | `os.Stat` 실패가 "없음"이 아닌 경우 권한/IO 오류로 보고 반환한다. |
| 58-60 | 첫 build가 오래 걸릴 수 있음을 로그로 알린다. |
| 61-70 | `bash scripts/build_image.sh`를 실행한다. stdout/stderr를 daemon에 그대로 연결해 진행 상황을 보이게 한다. |
| 72-75 | script가 성공했는데 예상 image가 없으면 오류로 처리한다. |
| 77-79 | golden image 준비 완료를 로그로 남긴다. |
| 81-108 | `CloneDisk`는 golden image를 VM별 ext4 파일로 복제한다. |
| 83-84 | destination path를 `/tmp/goose-workspaces/{vmID}.ext4`로 만든다. |
| 86-90 | golden image를 읽기용으로 연다. |
| 92-96 | 목적지 파일을 새로 만든다. |
| 98-104 | 파일 내용을 복사하고 disk sync까지 수행한다. |
| 106-108 | VM 전용 디스크 경로를 반환한다. |
| 110-143 | VM ext4 disk를 loop mount하고 callback 실행 후 unmount하는 공통 helper다. |
| 113-115 | disk path와 mount directory를 VM ID 기준으로 만든다. |
| 116-119 | 이전 실패로 남은 mount를 lazy unmount하고 mount dir를 제거한다. |
| 121-123 | 새 mount dir를 만든다. |
| 125-135 | callback 이후 mount가 되었으면 lazy unmount하고 mount dir를 제거하는 defer다. |
| 137-140 | `mount -o loop`로 ext4 disk를 mount한다. 실패하면 command output을 error에 포함한다. |
| 142-143 | caller가 넘긴 파일 주입 작업을 mount point에서 실행한다. |
| 145-180 | `PrepareVM`은 VM disk 안에 Goose 설정, secret, optional task, timezone을 한 번의 mount cycle로 주입한다. |
| 150-154 | `/root/.config/goose` directory를 VM disk 안에 만든다. |
| 156-163 | host의 `goose.yaml`, `goose-secrets.yaml`을 VM disk에 복사한다. |
| 165-171 | task 문자열이 있으면 `/root/task.txt`를 쓴다. 현재 daemon은 persistent agent mode라 빈 문자열을 넘긴다. |
| 173-175 | host timezone을 VM에 반영한다. 실패해도 warning만 남기고 VM 준비는 계속한다. |
| 177-180 | 주입 완료 로그를 남기고 callback을 종료한다. |
| 182-219 | `injectHostTimezone`은 VM의 `/etc/localtime`, `/etc/timezone`을 host timezone과 맞춘다. |
| 188-195 | `/etc/timezone`을 우선 읽고, 없으면 `/etc/localtime` symlink에서 IANA timezone 이름을 추출한다. |
| 197-201 | VM image 안에 해당 zoneinfo 파일이 있는지 확인한다. 없으면 tzdata 누락 가능성을 알린다. |
| 203-209 | VM disk의 `/etc/localtime`을 host timezone symlink로 교체한다. |
| 211-215 | VM disk의 `/etc/timezone` 파일도 plain text로 쓴다. |
| 217-219 | timezone 설정 완료 로그를 남긴다. |
| 221-237 | `copyFile` helper다. src를 열고 dst를 생성한 뒤 `io.Copy`로 복사한다. |
| 239-267 | `EnsureGooseAgent`는 VM 이미지에 넣을 agent 바이너리를 빌드한다. |
| 243-246 | 이미 artifact가 있으면 재빌드하지 않는다. |
| 248-252 | artifact directory를 만든다. |
| 254-259 | `go build -o artifacts/goose-agent ./cmd/goose-agent/`를 Linux amd64 정적 링크 조건으로 실행한다. |
| 260-263 | 빌드 실패 시 partial binary를 삭제하고 error를 반환한다. |
| 265-267 | 빌드 성공 로그를 남긴다. |
| 269-310 | `EnsureKernel`은 kernel binary를 없을 때 다운로드한다. |
| 271-274 | 이미 kernel artifact가 있으면 skip한다. |
| 276-280 | 다운로드 대상 directory를 만든다. |
| 282-290 | HTTP GET을 수행하고 200 OK가 아니면 오류로 처리한다. |
| 292-306 | 파일을 생성해 response body를 쓰고 close까지 확인한다. 실패하면 partial file을 삭제한다. |
| 308-310 | kernel 다운로드 완료 로그를 남긴다. |
| 312-363 | `EnsureFirecracker`는 Firecracker tarball 다운로드, SHA256 검증, binary 추출을 담당한다. |
| 315-318 | 이미 binary가 있으면 skip한다. |
| 320-324 | artifact directory를 만든다. |
| 326-332 | 임시 tarball 파일을 만들고 함수 종료 시 삭제한다. |
| 334-344 | HTTP GET과 status code 검증을 수행한다. |
| 346-351 | 다운로드 내용을 temp file에 쓰면서 동시에 SHA256을 계산한다. |
| 353-355 | 계산된 SHA256과 기대값을 비교한다. mismatch면 설치하지 않는다. |
| 357-362 | tarball에서 Firecracker binary를 추출하고 성공 로그를 남긴다. |
| 365-406 | `extractFirecrackerBin`은 `.tgz` 내부에서 `firecracker-*` 정규 파일을 찾아 destination에 쓴다. |
| 368-378 | gzip tarball을 연다. |
| 380-388 | tar entry를 순회한다. EOF면 종료한다. |
| 389-393 | basename이 `firecracker-`로 시작하는 regular file만 추출 대상으로 삼는다. |
| 394-403 | destination 파일을 0755 권한으로 만들고 tar entry 내용을 복사한다. 실패하면 partial binary를 제거한다. |
| 405-406 | archive 안에서 binary를 찾지 못하면 error를 반환한다. |
| 408-423 | `CleanupDisk`는 VM별 ext4 disk를 삭제한다. 이미 없으면 성공으로 간주한다. |

핵심 판단: 이 패키지는 host root 권한과 Linux filesystem command에 강하게 의존한다. unit test를 만들려면 command runner/file system 추상화가 필요하다.

## `internal/network/manager.go`

| 줄 | 분석 |
|---:|---|
| 1 | network 내부 패키지다. |
| 3-10 | formatting, logging, `/proc` 쓰기, 외부 command, sort, mutex를 가져온다. |
| 12-21 | `Manager` 구조체다. IP pool, gateway/subnet, TAP ID pool, bridge name을 보관한다. |
| 23-30 | `NewManager`는 `10.0.1.2`부터 `10.0.1.254`까지 IP 목록을 만들고 정렬한다. 문자열 정렬이라 `10.0.1.10`이 `10.0.1.2`보다 먼저 올 수 있다. |
| 32-35 | IP 사용 여부 map을 모두 false로 초기화한다. |
| 37-45 | manager field를 채운다. bridge 이름은 `goose-br0`로 고정된다. |
| 47-49 | bridge/NAT 설정을 시도한다. 실패해도 warning만 남기고 manager는 반환한다. |
| 51-52 | 초기화된 manager를 반환한다. |
| 54-59 | Linux bridge를 생성하고 gateway IP를 부여하고 link를 up 상태로 만든다. 기존 bridge가 있으면 add/addr command 실패를 무시한다. |
| 61-64 | `/proc/sys/net/ipv4/ip_forward`에 `1`을 써서 host forwarding을 켠다. 실패해도 warning만 남긴다. |
| 66-74 | iptables NAT MASQUERADE rule이 없으면 추가한다. command 실패 처리는 적극적으로 하지 않는다. |
| 76-77 | bridge setup 함수 종료다. |
| 79-81 | IP/TAP/MAC 할당 함수다. mutex로 동시 생성 요청을 직렬화한다. |
| 83-94 | 아직 사용하지 않는 첫 IP를 찾고 사용 중으로 표시한다. 없으면 error를 반환한다. |
| 96-104 | free-list에 TAP ID가 있으면 재사용하고, 없으면 nextTapID를 증가시킨다. |
| 106-107 | TAP device 이름과 deterministic MAC 주소를 만든다. |
| 109-114 | TAP device 생성에 실패하면 IP와 TAP ID 할당을 되돌리고 error를 반환한다. |
| 116-117 | TAP device, guest IP, MAC 주소를 반환한다. |
| 119-121 | release 함수도 mutex로 보호한다. |
| 123-125 | TAP device 삭제를 시도한다. 실패해도 warning만 남긴다. |
| 127-130 | IP pool에서 guest IP를 사용 가능으로 되돌린다. |
| 132-136 | `tapN`에서 숫자를 파싱해 free-list에 넣는다. |
| 137-138 | release 종료다. |
| 140-142 | stale TAP device가 있으면 먼저 삭제한다. |
| 143-145 | `ip tuntap add`로 TAP device를 만든다. |
| 147-150 | TAP device를 bridge에 붙인다. 실패 시 생성한 TAP을 삭제한다. |
| 152-155 | TAP device를 up 상태로 만든다. 실패 시 TAP을 삭제한다. |
| 157-158 | TAP 생성 성공이다. |
| 160-162 | `ip link delete`로 TAP device를 삭제한다. |

핵심 판단: Linux network namespace를 직접 다루지는 않고 host bridge + TAP + NAT 모델이다. 운영 환경에서는 iptables/nftables 충돌, root 권한, bridge 기존 상태를 확인해야 한다.

## `internal/vm/machine.go`

| 줄 | 분석 |
|---:|---|
| 1 | VM 실행 내부 패키지다. |
| 3-11 | context, formatting, os, Firecracker SDK, 모델 타입, logrus를 가져온다. |
| 13-24 | `VMConfig`는 VM 하나를 시작하는 데 필요한 모든 입력값이다. socket, binary, kernel, rootfs, TAP, MAC, IP, gateway를 가진다. |
| 26-27 | `StartMachine`은 Firecracker machine을 만들고 바로 boot한다. |
| 28-30 | rootfs drive 설정에 사용할 local 변수다. root device이며 writable이다. |
| 32-39 | Firecracker drive 모델을 만든다. VM별 cloned rootfs를 `/dev/vda` root device로 연결한다. |
| 41-48 | Firecracker network interface를 만든다. host TAP device와 MAC 주소를 지정한다. |
| 50-54 | Linux kernel `ip=` boot parameter를 만들어 guest IP/gateway/netmask를 DHCP 없이 주입한다. |
| 55-63 | kernel args를 만든다. `init=/usr/local/sbin/micro-init`로 VM 내부 첫 프로세스를 지정한다. `panic=1 reboot=k`는 PID 1 종료 시 Firecracker가 빠르게 종료되도록 하는 설계다. |
| 65-70 | Firecracker log FIFO path를 만들고 stale file을 제거한다. |
| 72-84 | Firecracker SDK config를 구성한다. vCPU 2개, 메모리 2048MiB, Warning log level이다. |
| 86-91 | SDK logger를 Warning level로 낮춰 부팅 로그 소음을 줄인다. |
| 93-98 | Firecracker process command를 만든다. 지정 binary, socket path, stdout/stderr 연결을 사용한다. |
| 100-103 | SDK option으로 logger와 process runner를 주입한다. |
| 105-109 | Firecracker machine 객체를 만든다. 아직 VM process는 시작 전이다. |
| 111-114 | Firecracker process를 시작하고 VM을 부팅한다. |
| 116 | 성공 로그를 Info level로 남기지만 logger level이 Warn이라 일반 상황에서는 출력되지 않는다. |
| 118-120 | control plane이 lifecycle을 관리할 수 있도록 machine pointer를 반환한다. |

핵심 판단: VM spec이 코드에 하드코딩되어 있다. vCPU/memory/kernel args를 설정화하려면 `VMConfig`와 daemon config 확장이 필요하다.

## `scripts/build_image.sh`

| 줄 | 분석 |
|---:|---|
| 1-2 | bash script이며 오류, unset 변수, pipe 실패를 즉시 중단한다. |
| 4-8 | Debian Bookworm minbase를 선택한 이유와 host dependency를 주석으로 설명한다. |
| 9-14 | output image, mount directory, Goose download URL, temp 파일 변수를 선언한다. |
| 16-25 | cleanup trap이다. mount된 pseudo filesystem/rootfs를 해제하고 temp 파일/directory를 제거한다. |
| 27-37 | host dependency 검사 함수다. curl, debootstrap, ext4 도구가 없으면 설치 명령을 안내하고 종료한다. |
| 39-40 | dependency를 검사하고 artifact directory를 만든다. |
| 42-43 | Goose release tarball을 host에서 다운로드한다. checksum 검증은 없다. |
| 45-50 | 1GB ext4 image를 만들고 format한 뒤 mount한다. 주석의 "512M initial"은 실제 `fallocate -l 1G`와 맞지 않는다. |
| 52-60 | debootstrap으로 Debian Bookworm minbase를 설치한다. libgomp1, ca-certificates, tzdata를 같이 포함한다. |
| 62-71 | Goose tarball을 풀어 `goose` binary를 찾아 VM image의 `/usr/local/bin/goose`에 설치한다. |
| 73-79 | daemon이 미리 빌드한 `artifacts/goose-agent`를 VM image에 설치한다. 없으면 실패한다. |
| 81-88 | `micro-init`의 역할을 주석으로 설명한다. systemd 없이 goose-agent가 PID 1이 된다. |
| 89-119 | heredoc으로 VM 내부 `/usr/local/sbin/micro-init` script를 작성한다. |
| 90-96 | VM 내부 init script 시작부다. proc/sys/dev/devpts를 mount한다. |
| 97-105 | HOME, USER, PATH를 설정하고 timezone은 host가 주입한 `/etc/localtime`을 사용한다고 설명한다. |
| 107-118 | `goose-agent`가 있으면 실행하고, 없으면 one-shot task 또는 interactive goose로 fallback한다. |
| 120 | micro-init에 실행 권한을 준다. |
| 122-123 | hostname과 hosts 파일을 설정한다. |
| 125-131 | VM 내부 DNS resolver를 8.8.8.8/1.1.1.1로 고정한다. |
| 133-140 | apt cache, docs, manpages, locale data를 제거해 image 크기를 줄인다. |
| 142-145 | rootfs를 unmount하고 cleanup trap을 해제하며 Goose tarball을 제거한다. |
| 147-148 | filesystem check 후 ext4 image를 최소 크기로 shrink한다. |
| 150-151 | 최종 image 크기를 출력한다. |

핵심 판단: 이 스크립트는 실제 제품 품질에 큰 영향을 주는 "이미지 빌더"다. Goose binary checksum 검증, image build 재현성, cleanup 실패 처리 강화가 중요하다.

## `simple_test_scenario.sh`

| 줄 | 분석 |
|---:|---|
| 1-4 | bash E2E 테스트 스크립트다. sudo로 실행해야 하며 엄격 모드를 켠다. |
| 6-9 | control plane API 주소, 테스트 prompt, 로그 파일, PASS 상태 변수를 정의한다. 인증 token은 사용하지 않는다. |
| 11-16 | 출력 helper와 HTTP status 검증 helper를 정의한다. |
| 18-24 | daemon을 백그라운드로 시작하고 PID/log 위치를 기록한다. |
| 26-31 | `/vms` API가 응답할 때까지 최대 30초 대기한다. |
| 33-38 | trap cleanup이다. daemon process를 kill하고 wait한다. |
| 40-49 | 첫 번째 VM을 생성하고 응답에서 `vm_id`, `agent_url`, `guest_ip`를 jq로 추출한다. |
| 51-56 | VM1 agent `/tasks`로 prompt를 보내고 output 일부를 확인한다. |
| 58-62 | VM1 agent `/stop`을 호출하고 5초 대기한다. |
| 64-68 | control plane `DELETE /vms/{id}`로 VM1 자원을 정리한다. |
| 70-78 | VM2를 생성한다. |
| 80-86 | VM3를 생성한다. |
| 88-91 | control plane VM 목록 길이가 2인지 확인하고 ID/IP를 출력한다. |
| 93-101 | VM2와 VM3에 작업을 병렬 요청하고 두 curl process를 기다린다. |
| 103-106 | 두 VM의 output을 확인한다. |
| 108-112 | VM2/VM3 agent `/stop`을 호출한다. |
| 114-117 | VM2/VM3를 control plane에서 삭제한다. |
| 119-121 | VM 목록이 비었는지 확인한다. |
| 123-127 | daemon을 종료하고 trap을 해제한다. |
| 129-140 | PASS 여부에 따라 성공/실패 메시지를 출력하고 실패 시 exit 1을 반환한다. |

핵심 판단: 실제 KVM/Firecracker 환경이 필요해 CI에서 쉽게 돌리기 어렵다. 인증 켜진 운영 구성에서는 Authorization header를 추가해야 한다.

## `configs/goose.yaml.example`

| 줄 | 분석 |
|---:|---|
| 1-8 | 이 파일이 VM에 주입되는 비밀이 아닌 Goose 설정 템플릿이며 실제 `configs/goose.yaml`은 commit하지 말라고 안내한다. |
| 10 | 기본 provider를 `google`로 둔다. |
| 11 | 기본 model을 `gemini-2.5-flash`로 둔다. |
| 12 | telemetry를 끈다. |
| 14-16 | VM에는 keyring daemon이 없으므로 Goose keyring을 비활성화한다. |
| 18-25 | Goose built-in developer extension을 활성화하고 timeout 300초를 설정한다. |

## `configs/goose-secrets.yaml.example`

| 줄 | 분석 |
|---:|---|
| 1-8 | secrets 파일 용도와 commit 금지 원칙을 설명한다. |
| 10-11 | Google AI Studio API key placeholder다. |
| 13-15 | Anthropic/OpenAI key 예시를 주석으로 제공한다. |

## `.github/workflows/claude.yml`

| 줄 | 분석 |
|---:|---|
| 1 | GitHub Actions workflow 이름이다. |
| 3-11 | issue comment, PR review comment, issue open/assign, PR review 제출 이벤트에서 실행 후보가 된다. |
| 13-20 | job 조건이다. 본문/제목/review/comment에 `@claude`가 포함될 때만 실행한다. |
| 20 | runner는 `ubuntu-latest`다. |
| 21-27 | repository/PR/issue/action 읽기와 OIDC token 쓰기 권한을 부여한다. |
| 28-31 | repository를 shallow checkout한다. |
| 33-37 | Anthropic Claude Code Action v1을 실행하고 secret token을 전달한다. |
| 39-42 | Claude가 PR CI 결과를 읽을 수 있도록 추가 권한을 선언한다. |
| 43-49 | custom prompt/args 예시를 주석으로 남긴다. |

## `.github/workflows/claude-code-review.yml`

| 줄 | 분석 |
|---:|---|
| 1 | workflow 이름은 Claude Code Review다. |
| 3-11 | PR opened/synchronize/ready_for_review/reopened에서 실행된다. path filter 예시는 주석 처리되어 있다. |
| 13-21 | `claude-review` job 정의와 runner 설정이다. |
| 22-27 | contents, PR, issue 읽기와 OIDC token 쓰기 권한을 부여한다. |
| 28-32 | repository를 shallow checkout한다. |
| 34-41 | Claude Code Action에 code-review plugin을 붙여 현재 PR 리뷰 prompt를 실행한다. |
| 42-43 | action 사용 문서 링크를 주석으로 남긴다. |

## `.gitignore`

| 줄 | 분석 |
|---:|---|
| 1-3 | 파일 제목 주석이다. |
| 5-13 | Go build output과 native library artifact를 무시한다. |
| 15-22 | Firecracker artifact, ext4/image/bin/tgz, vmlinux 계열 대용량 파일을 무시한다. |
| 24-27 | socket, log, `/tmp/*` 경로를 무시한다. |
| 29-31 | 실제 Goose config/secrets 파일을 무시한다. API key 유출 방지 목적이다. |
| 33-34 | Claude Code context 파일을 무시한다. |
| 36-39 | IDE/OS 생성 파일을 무시한다. |

## `go.sum`

`go.sum`은 1083줄의 checksum lock 파일이다. 각 줄은 다음 둘 중 하나다.

- `module version h1:checksum`: 해당 모듈 zip/source의 checksum
- `module version/go.mod h1:checksum`: 해당 모듈의 `go.mod` checksum

이 파일은 개발자가 직접 비즈니스 로직으로 읽는 대상이 아니다. `go mod download`, `go test`, `go build`가 dependency 무결성을 확인할 때 사용한다. 줄 단위로 보면 모든 줄의 역할은 "특정 module version이 예상한 checksum과 일치하는지 검증"이다.

## 설계상 중요한 관찰

- 프론트엔드는 없다. HTTP API와 shell script가 외부 인터페이스다.
- 백엔드는 host daemon과 guest agent 두 프로세스로 나뉜다.
- core logic은 `internal/storage`, `internal/network`, `internal/vm` 세 패키지에 있다.
- VM 생성 API는 동기식이다. `/vms` POST는 agent health check까지 끝난 뒤 반환한다.
- task 실행은 control plane을 거치지 않는다. caller가 guest private IP로 직접 요청한다.
- host root 권한, `/dev/kvm`, `ip`, `iptables`, `mount`, `debootstrap`이 사실상 런타임 전제다.
- 설정/시크릿은 golden image에 bake하지 않고 VM 생성 시 주입한다.
- Firecracker binary는 SHA256 검증이 있지만 kernel과 Goose tarball은 checksum 검증이 없다.
- test coverage는 shell E2E 중심이며 Go unit test 파일은 없다.
