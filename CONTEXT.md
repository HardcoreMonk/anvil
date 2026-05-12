# anvil 컨텍스트

## 목적

`anvil`은 IronClaw와 ephemera를 결합하는 새로운 프로젝트다. IronClaw는
상위 orchestration과 MCP client 역할을 맡고, ephemera는 Firecracker MicroVM
기반 격리 실행 runtime을 제공한다. anvil은 이 둘을 연결해 AI agent 작업을
격리 VM에서 생성, 실행, 중지, snapshot, restore할 수 있게 만드는 통합
프로젝트다.

anvil의 상위 통합 대상은 IronClaw 전용이다. OpenClaw 연동은 지원 범위가 아니며,
OpenClaw compatibility layer, shared gateway, shared runtime contract를 anvil
요구사항으로 취급하지 않는다.

현재 GitHub 저장소는 `https://github.com/HardcoreMonk/ephemera/`이다.
ephemera는 이미 `0.1.0`, `0.2.0`이 릴리즈된 기반 runtime이며, 이 저장소의
Go 모듈 경로와 기존 API/환경 변수에는 `ephemera` 또는 `goose` 이름이 남아
있다. anvil 통합 릴리즈는 ephemera runtime tag와 충돌하지 않도록
`anvil-v0.1.0`처럼 별도 prefix를 사용한다. 문서에서는 anvil과 ephemera를 같은
이름으로 취급하지 않는다.

## 진실 기준 문서 순서

1. `CONTEXT.md`: anvil/ephemera/IronClaw 경계, 변경 불가 계약
2. `README.md`: anvil 결합 프로젝트 개요와 현재 구현 사용법
3. `RELEASE_NOTES.md`: ephemera 릴리즈와 anvil 통합 작업 변화
4. `docs/architecture/*.md`: ephemera runtime, service logic, anvil MCP 설계
5. `docs/analysis/*.md`: ephemera 0.1.0/0.2.0 분석과 보조 설명
6. 업로드된 과거 문서와 초안: 참고 자료

## 도메인 용어집

| 용어 | 의미 | 담당 영역 |
|---|---|---|
| anvil | IronClaw와 ephemera를 결합하는 새 프로젝트 이름 | project-wide |
| IronClaw | MCP client/orchestration 계층. anvil VM 실행 기능을 사용하는 상위 시스템 | 외부/상위 통합 |
| OpenClaw | anvil의 통합 대상이 아님. anvil 문서와 구현은 OpenClaw 운영 계약을 제공하지 않음 | 제외 범위 |
| ephemera | Firecracker MicroVM 기반 격리 실행 runtime. `0.1.0`, `0.2.0` 릴리즈 기준 구현 | `cmd/goose-daemon`, `internal/*` |
| ephemera control plane | VM 생성, 삭제, snapshot, restore, proxy를 담당하는 호스트 daemon | `cmd/goose-daemon` |
| MicroVM | Firecracker + KVM으로 실행되는 ephemera 격리 실행 환경 | `internal/vm` |
| goose-agent | VM 안에서 prompt 실행, health, stop API를 제공하는 HTTP agent | `cmd/goose-agent` |
| micro-init | VM의 PID 1. 가상 파일시스템 mount, agent 실행, clean poweroff 담당 | `cmd/micro-init` |
| Full snapshot | guest RAM 전체와 rootfs 사본, Firecracker state를 저장한 기준 snapshot | `internal/storage` |
| Diff snapshot | 기준 Full snapshot 이후 dirty memory page만 sparse file로 저장한 snapshot | `internal/storage` |
| COW restore | snapshot rootfs를 read-only base로 두고 per-VM sparse exception store에 쓰기를 기록하는 restore 방식 | `internal/storage` |
| IronClaw MCP adapter | IronClaw가 ephemera daemon API를 anvil tool로 호출하게 해 주는 stdio bridge | `cmd/anvil-mcp` |

## 경계 규칙

- `docs/analysis/`는 ephemera 0.1.0/0.2.0 분석 근거 자료다. 제목과 설명은
  ephemera 릴리즈 분석임을 명확히 해야 한다.
- `README.md`는 anvil 결합 프로젝트의 현재 진입점이다. ephemera runtime
  사용법은 anvil의 기반 runtime 설명으로 포함한다.
- 코드 식별자, API 경로, 환경 변수, 파일 경로는 실제 구현과 호환성이
  더 중요하므로 임의로 한국어화하지 않는다.
- `ephemera`라는 이름이 남아 있는 API/환경 변수는 기반 runtime 계약으로
  취급한다. 이것을 anvil 제품명으로 덮어쓰지 않는다.
- 공개 운영 URL은 reverse proxy/TLS 계층에서 결정한다. 현재 로컬 검증
  환경에서는 사용자가 지정한 `192.168.3.73` 주소를 기준으로 한다.

## 고정된 런타임 계약

이번 문서 재작성과 프로젝트 재설계는 다음 계약을 임의로 변경하지 않는다.

- daemon 기본 bind 주소/포트: `127.0.0.1:3000`
- VM private network: `10.0.1.0/24`, bridge `goose-br0`
- guest agent port: `8080`
- control-plane token canonical 환경 변수: `EPHEMERA_API_TOKENS`,
  `EPHEMERA_API_TOKEN`
- control-plane token alias 환경 변수: `ANVIL_API_TOKENS`,
  `ANVIL_API_TOKEN`
- public agent URL canonical 환경 변수: `EPHEMERA_PUBLIC_URL`
- public agent URL alias 환경 변수: `ANVIL_PUBLIC_URL`
- daemon bind canonical 환경 변수: `EPHEMERA_API_ADDR`,
  `EPHEMERA_API_PORT`
- daemon bind alias 환경 변수: `ANVIL_API_ADDR`,
  `ANVIL_API_PORT`
- guest agent port canonical 환경 변수: `EPHEMERA_AGENT_PORT`
- guest agent port alias 환경 변수: `ANVIL_AGENT_PORT`
- MCP adapter daemon URL 환경 변수: `ANVIL_DAEMON_URL`
- MCP adapter token 환경 변수: `ANVIL_API_TOKEN`

`ANVIL_API_TOKEN`은 프로세스별 의미가 다르다. goose-daemon에서는
`EPHEMERA_API_TOKEN`의 control-plane token alias이고, `cmd/anvil-mcp`에서는
daemon으로 보내는 outbound Bearer token이다.

## 후속 후보

- 공개 tag/release 정리: Git tag와 GitHub Release page 상태를 함께 관리
- MCP v2에서 snapshot/restore tool, workspace copy, persistent session 지원
- multi-host runtime, scheduler, quota, audit storage 추가
