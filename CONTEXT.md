# anvil 컨텍스트

## 목적

`anvil`은 Firecracker MicroVM을 이용해 AI agent 작업을 하드웨어 가상화
경계 안에서 실행하고, 작업이 끝나면 VM 상태를 폐기하거나 snapshot으로
보존하는 단일 호스트 제어 평면이다.

GitHub 저장소는 `https://github.com/HardcoreMonk/ephemera/`이고, Go 모듈
경로와 일부 기존 API/환경 변수에는 `ephemera` 또는 `goose` 이름이 남아
있다. 제품명과 문서상의 공식 명칭은 `anvil`로 통일한다.

## 진실 기준 문서 순서

1. `CONTEXT.md`: 제품명, 경계 규칙, 변경 불가 계약
2. `README.md`: 현재 사용법, 공개 API, 운영 절차
3. `RELEASE_NOTES.md`: 버전별 기능 변화
4. `docs/architecture/*.md`: 런타임, 서비스 로직, MCP 설계
5. `docs/analysis/*.md`: 버전 분석과 보조 설명
6. 업로드된 과거 문서와 초안: 참고 자료

## 도메인 용어집

| 용어 | 의미 | 담당 영역 |
|---|---|---|
| anvil control plane | VM 생성, 삭제, snapshot, restore, proxy를 담당하는 호스트 daemon | `cmd/goose-daemon` |
| MicroVM | Firecracker + KVM으로 실행되는 격리 실행 환경 | `internal/vm` |
| goose-agent | VM 안에서 prompt 실행, health, stop API를 제공하는 HTTP agent | `cmd/goose-agent` |
| micro-init | VM의 PID 1. 가상 파일시스템 mount, agent 실행, clean poweroff 담당 | `cmd/micro-init` |
| Full snapshot | guest RAM 전체와 rootfs 사본, Firecracker state를 저장한 기준 snapshot | `internal/storage` |
| Diff snapshot | 기준 Full snapshot 이후 dirty memory page만 sparse file로 저장한 snapshot | `internal/storage` |
| COW restore | snapshot rootfs를 read-only base로 두고 per-VM sparse exception store에 쓰기를 기록하는 restore 방식 | `internal/storage` |
| IronClaw MCP adapter | IronClaw 같은 MCP client가 anvil daemon API를 호출하게 해 주는 stdio bridge | `cmd/anvil-mcp` |

## 경계 규칙

- `docs/analysis/`는 근거 자료이며, 현재 설계의 최종 계약은
  `docs/architecture/`와 `README.md`가 담당한다.
- 코드 식별자, API 경로, 환경 변수, 파일 경로는 실제 구현과 호환성이
  더 중요하므로 임의로 한국어화하지 않는다.
- `ephemera`라는 이름이 남아 있는 API/환경 변수는 당장 깨지지 않는
  호환 계약으로 취급한다.
- 공개 운영 URL은 reverse proxy/TLS 계층에서 결정한다. 현재 로컬 검증
  환경에서는 사용자가 지정한 `192.168.3.73` 주소를 기준으로 한다.

## 고정된 런타임 계약

이번 문서 재작성과 프로젝트 재설계는 다음 계약을 임의로 변경하지 않는다.

- daemon 기본 bind 주소/포트: `127.0.0.1:3000`
- VM private network: `10.0.1.0/24`, bridge `goose-br0`
- guest agent port: `8080`
- control-plane token 환경 변수: `EPHEMERA_API_TOKENS`,
  `EPHEMERA_API_TOKEN`
- public agent URL 환경 변수: `EPHEMERA_PUBLIC_URL`
- MCP adapter daemon URL 환경 변수: `ANVIL_DAEMON_URL`
- MCP adapter token 환경 변수: `ANVIL_API_TOKEN`

## 후속 후보

- 공개 tag/release 정리: Git tag와 GitHub Release page 상태를 함께 관리
- `EPHEMERA_*` 환경 변수의 `ANVIL_*` alias 추가
- MCP v2에서 snapshot/restore tool, workspace copy, persistent session 지원
- multi-host runtime, scheduler, quota, audit storage 추가
