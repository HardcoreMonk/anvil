# anvil — Codex 프로젝트 지침

`anvil`은 IronClaw와 ephemera를 결합하는 새 프로젝트다. 이 저장소는
`https://github.com/HardcoreMonk/ephemera/`이며, ephemera 0.1.0/0.2.0 기반
runtime과 anvil 통합 문서를 함께 담고 있다.

## 진실 기준 문서 순서

작업 중 문서가 서로 충돌하면 아래 순서로 판단한다.

1. `CONTEXT.md`
2. `README.md`
3. `RELEASE_NOTES.md`
4. `docs/architecture/*.md`
5. `docs/analysis/*.md`
6. 과거 업로드 문서와 초안

## 프로젝트 구조

- ephemera 호스트 제어 평면: `cmd/goose-daemon`, `internal/storage`,
  `internal/network`, `internal/vm`
- 게스트 구성 요소: `cmd/goose-agent`, `cmd/micro-init`
- IronClaw 연동 MCP 어댑터: `cmd/anvil-mcp`, `internal/anvilmcp`
- 런타임 스케줄러 서비스: `cmd/anvil-scheduler`, `internal/anvilmcp`
- 설정 예시: `configs/*.example`, `configs/profiles/*`
- 런타임 산출물: `artifacts/`, `snapshots/`, `/tmp/goose-workspaces/`

## 작업 흐름

- 문서는 한국어로 작성한다. 코드 식별자, API 경로, 환경 변수, 명령어,
  파일명처럼 번역하면 계약이 깨지는 값은 원문을 유지한다.
- 기존 동작을 바꾸기 전에는 `README.md`, `docs/architecture/`,
  `RELEASE_NOTES.md` 중 영향을 받는 문서를 함께 갱신한다.
- 로컬 비밀 파일(`configs/goose-secrets.yaml`, profile별 secrets)은 절대
  커밋하지 않는다.
- `anvil`은 IronClaw+ephemera 결합 프로젝트를 가리킨다.
- `ephemera`는 이미 릴리즈된 Firecracker runtime을 가리킨다. ephemera
  릴리즈 분석 문서의 제목을 anvil로 바꾸지 않는다.
- 실제 API/환경 변수/경로가 `EPHEMERA_*` 또는 `goose-*` 이름을 쓰는 경우에는
  코드의 현재 계약을 그대로 유지한다.

## 명령어

일반 검증:

```bash
go test ./...
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

전체 통합 검증:

```bash
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
```

통합 테스트는 `/dev/kvm`, root 권한, Firecracker 실행 가능 호스트,
LLM API 키가 들어 있는 로컬 `configs/goose-secrets.yaml`이 필요하다.

## 불변 조건

- `POST /vms` 응답 외에는 `agent_token`을 노출하지 않는다.
- VM 삭제 실패 경로에서도 TAP/IP, dm-snapshot, loop device, bind mount,
  sparse COW 파일을 정리해야 한다.
- 실행 중인 원본 VM의 snapshot은 restore하지 않는다.
- diff snapshot이 참조 중인 full snapshot은 삭제하지 않는다.
- MCP v1은 얇은 stdio 어댑터다. VM 수명주기 의미는 ephemera daemon API가
  소유한다.

## 보안

- 공개 노출은 TLS 종료 reverse proxy 뒤에서 수행한다.
- `EPHEMERA_API_TOKENS`가 설정된 운영 모드를 우선한다.
- 채팅, 문서, 커밋 메시지에 실제 API 키를 남기지 않는다.
- 런타임 산출물과 비밀 설정은 `.gitignore`에 남겨 둔다.
