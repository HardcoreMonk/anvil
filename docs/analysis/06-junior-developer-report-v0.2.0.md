# ephemera 0.2.0 주니어 개발자용 분석 보고서

## 한 줄 요약

0.2.0은 ephemera가 "VM을 띄우는 도구"에서 "VM을 띄우고, 명령을 실행하고, 상태를 저장하고, 다시 복원하는 단일 호스트 실행 플랫폼"으로 바뀐 릴리즈다.

## 먼저 알아야 할 배경

ephemera는 Firecracker MicroVM을 사용해 격리된 실행 환경을 만든다. 각 VM 안에는 `goose-agent`가 실행되고, host에서는 `goose-daemon`이 VM을 만들고 관리한다. anvil은 이 runtime을 IronClaw와 결합한다.

0.1.0에서는 주로 다음이 중요했다.

- VM을 생성할 수 있는가
- guest agent가 실행되는가
- task를 보낼 수 있는가
- VM을 삭제할 수 있는가

0.2.0에서는 여기에 다음이 추가되었다.

- VM별 인증 token
- daemon을 통한 agent proxy
- snapshot 생성
- snapshot에서 restore
- diff snapshot
- COW rootfs
- guest IP 재설정
- CI와 테스트

## 꼭 이해해야 하는 새 구성요소

### micro-init

`cmd/micro-init/main.go`는 guest VM 안에서 PID 1로 실행된다.

쉽게 말하면 VM 안의 가장 작은 init system이다.

하는 일:

- 필요한 filesystem mount
- 환경 변수 설정
- `goose-agent` 실행
- 종료 signal을 받으면 agent 종료
- 파일시스템 sync 후 VM poweroff

이 파일이 없거나 rootfs에 설치되지 않으면 VM boot가 제대로 끝나지 않을 수 있다.

### agent token

각 VM마다 agent token이 생긴다. 이 token은 daemon이 VM 생성 시 만든다.

저장 위치:

```text
/root/.ephemera-agent-token
```

권한:

```text
0600
```

이 token은 guest agent의 `/tasks`, `/stop` 요청을 보호한다. `/health`는 인증 없이 접근 가능하다.

### daemon proxy

이제 외부 클라이언트가 guest VM의 private IP에 직접 요청하지 않아도 된다.

대신 daemon에 요청한다.

```text
POST /vms/{vm_id}/tasks
```

daemon은 내부에서 VM별 agent token을 붙여 guest agent에 요청을 전달한다.

이 구조가 좋은 이유:

- 클라이언트가 guest IP를 몰라도 된다.
- 클라이언트가 agent token을 직접 관리하지 않아도 된다.
- 상위 시스템은 daemon API만 보면 된다.

### profile

VM 생성 요청에 `profile`을 줄 수 있다.

profile은 VM마다 다른 Goose 설정을 쓰기 위한 기능이다.

예상 경로:

```text
configs/profiles/{profile}/goose.yaml
configs/profiles/{profile}/goose-secrets.yaml
```

profile 이름에는 `/`, `\`, `..`을 쓸 수 없다. 이 검사는 path traversal 공격을 막기 위해 중요하다.

### snapshot

snapshot은 VM의 상태를 저장하는 기능이다.

0.2.0에서는 다음 API가 생겼다.

```text
GET /snapshots
POST /vms/{vm_id}/snapshot
POST /snapshots/{snapshot_id}/restore
DELETE /snapshots/{snapshot_id}
```

snapshot 종류:

- `full`: 전체 기준 snapshot
- `diff`: full 이후 바뀐 memory page 중심의 차이 snapshot
- `auto`: 첫 snapshot이면 full, 이후는 diff

### restore

restore는 snapshot에서 새 VM을 만드는 기능이다.

중요한 점은 "기존 VM을 되살리는 것"이 아니라 "snapshot을 기반으로 새 VM을 만든다"는 것이다.

restore 과정에서는 다음 문제가 생긴다.

- snapshot 안에 예전 IP가 남아 있을 수 있다.
- 기존 disk를 그대로 쓰면 여러 VM이 같은 disk를 동시에 쓸 수 있다.
- diff snapshot은 base full snapshot이 필요하다.

0.2.0은 이를 해결하기 위해 다음을 사용한다.

- vsock을 통한 guest IP 재설정
- device-mapper COW rootfs
- diff memory merge
- base snapshot 삭제 방지

## 중요한 파일별 설명

### `cmd/goose-daemon/api.go`

가장 많이 바뀐 파일이다.

담당:

- VM 생성 API
- VM 삭제 API
- task proxy API
- agent health proxy
- stop proxy
- snapshot API
- restore API
- agent token 생성
- profile 경로 선택

이 파일을 읽을 때는 한 번에 다 이해하려고 하지 말고 다음 순서로 보면 좋다.

1. VM 생성 request/response struct
2. `handleVMs`
3. `handleVM`
4. `proxyAgentEndpoint`
5. snapshot 관련 handler
6. restore 관련 handler

### `cmd/goose-agent/main.go`

guest VM 안에서 실행되는 agent다.

0.2.0에서 봐야 할 부분:

- token file 읽기
- auth middleware
- `/tasks`
- `/stop`
- `/health`
- vsock listener
- IP 변경 처리

### `cmd/micro-init/main.go`

guest VM의 PID 1이다.

이 파일은 짧지만 중요하다. VM boot와 shutdown의 시작점이기 때문이다.

### `internal/storage/snapshot.go`

snapshot 파일 관리와 COW rootfs 설정이 들어 있다.

초보자에게는 조금 어려운 파일이다. 먼저 struct와 함수 이름 중심으로 이해하면 된다.

먼저 볼 것:

- `SnapshotMetadata`
- `SaveMetadata`
- `LoadMetadata`
- `CopyDiskToSnapshot`
- `SetupDMSnapshot`
- `MergeMemoryDiff`

나중에 자세히 볼 것:

- loop device 처리
- `dmsetup`
- sparse file 처리
- bind mount fallback

### `internal/vm/machine.go`

Firecracker VM을 실제로 생성, snapshot, restore하는 코드다.

0.2.0에서 중요한 변화:

- kernel args에 `micro-init` 사용
- dirty page tracking 활성화
- snapshot 생성 함수 확장
- restore 함수 추가
- guest IP 재설정 함수 추가

### `internal/network/manager.go`

VM 네트워크 할당을 관리한다.

0.2.0에서는 restore를 위해 TAP device와 MAC address를 다루는 기능이 추가되었다.

### `internal/storage/provisioner.go`

VM rootfs를 준비한다.

0.2.0에서 추가로 하는 일:

- profile 설정 복사
- agent token 파일 작성
- micro-init artifact 확인

## 실행 흐름 이해하기

### VM 생성 흐름

```text
사용자 요청
  -> daemon /vms
  -> profile 검증
  -> agent token 생성
  -> rootfs 준비
  -> network 할당
  -> Firecracker 실행
  -> guest 안에서 micro-init 실행
  -> micro-init이 goose-agent 실행
  -> daemon이 agent health 확인
  -> VM 생성 응답 반환
```

### task 실행 흐름

```text
사용자 요청
  -> daemon /vms/{id}/tasks
  -> daemon API token 확인
  -> running VM 찾기
  -> VM agent token 붙이기
  -> guest agent /tasks 호출
  -> 결과 반환
```

### snapshot 생성 흐름

```text
사용자 요청
  -> daemon /vms/{id}/snapshot
  -> VM pause
  -> Firecracker snapshot 생성
  -> disk 또는 diff 정보 저장
  -> metadata 저장
  -> VM resume 또는 stop
  -> snapshot 정보 반환
```

### restore 흐름

```text
사용자 요청
  -> daemon /snapshots/{id}/restore
  -> metadata 읽기
  -> 새 VM ID 만들기
  -> 네트워크 준비
  -> COW disk 준비
  -> memory diff 병합
  -> Firecracker restore
  -> guest IP 재설정
  -> agent health 확인
  -> 새 VM 정보 반환
```

## 테스트에서 배울 수 있는 것

0.2.0에는 테스트가 많이 추가되었다. 기능을 이해하려면 테스트부터 읽는 것도 좋다.

추천 순서:

1. `cmd/goose-agent/main_test.go`
2. `cmd/goose-daemon/config_test.go`
3. `cmd/goose-daemon/api_test.go`
4. `internal/storage/provisioner_test.go`
5. `e2e_test.sh`

단위 테스트는 작은 규칙을 알려준다. e2e 테스트는 전체 사용 흐름을 알려준다.

## 자주 헷갈릴 부분

### agent token과 daemon API token은 다르다

daemon API token은 외부 client가 daemon에 접근할 때 쓴다.

agent token은 daemon이 guest agent에 접근할 때 쓴다.

### snapshot restore는 기존 VM을 다시 켜는 것이 아니다

snapshot에서 새 VM을 만든다. 그래서 새 VM ID, 새 network allocation, 새 runtime state가 생긴다.

### diff snapshot은 혼자 존재할 수 없다

diff snapshot은 base full snapshot이 필요하다. 그래서 base full snapshot을 먼저 지우면 안 된다.

### COW rootfs는 disk copy를 줄이기 위한 장치다

restore할 때 disk 전체를 복사하면 느리고 공간을 많이 쓴다. COW는 base disk를 공유하고 바뀐 부분만 별도로 기록하게 해준다.

### health는 인증이 없다

`/health`는 인증 없이 접근 가능하다. 운영 환경에서 이 endpoint 노출 범위는 별도로 고려해야 한다.

## 코드 리뷰 때 볼 체크리스트

- profile 이름 검증이 빠진 경로가 없는가
- agent token이 log에 찍히지 않는가
- snapshot metadata 권한이 유지되는가
- restore 실패 시 COW device, loop device, mount가 정리되는가
- VM delete 시 snapshot restore용 임시 리소스가 정리되는가
- malformed JSON 요청이 적절히 실패하는가
- 같은 snapshot에서 동시 restore 요청이 들어오면 어떻게 되는가
- API token reload 중 request가 들어와도 안전한가
- e2e 테스트가 root 권한과 host dependency를 명확히 요구하는가

## 0.2.0을 이해한 뒤 다음으로 볼 주제

- Firecracker snapshot API
- Linux device-mapper snapshot
- sparse file과 `SEEK_DATA`, `SEEK_HOLE`
- vsock 통신
- TAP device와 NAT
- Go HTTP middleware 패턴
- API secret rotation

## 결론

0.2.0은 ephemera의 구조를 크게 확장한 릴리즈다. 초보 개발자는 먼저 daemon, agent, micro-init의 역할을 구분하고, 그 다음 snapshot/restore 흐름을 따라가면 된다.

가장 중요한 관점은 다음이다.

- daemon은 control plane이다.
- agent는 guest 안의 executor다.
- micro-init은 guest lifecycle 관리자다.
- snapshot storage는 VM 상태 저장소다.
- restore는 storage, VM, network, agent가 모두 함께 움직이는 기능이다.
