# anvil 컨텍스트

## 목적

`anvil`은 IronClaw의 tool call을 Firecracker MicroVM 안의 실제 agent 실행으로
변환하는 격리 execution layer다. IronClaw는 상위 orchestration, planner, MCP
client 역할을 맡고, ephemera는 Firecracker MicroVM 기반 격리 실행 runtime을
제공한다. anvil은 이 둘을 연결해 AI agent workspace를 격리 VM에서 생성, 실행,
중지, snapshot, restore할 수 있게 만드는 통합 프로젝트다.

구조적으로 anvil은 IronClaw가 호출하는 `anvil_*` MCP tool surface와 ephemera가
제공하는 runtime boundary 사이의 adapter다. 이 adapter는 IronClaw가 host runtime
세부사항을 직접 다루지 않고도 격리된 agent lifecycle을 제어할 수 있게 하는 실행
계약을 제공한다.

IronClaw와 ephemera는 1:1 service integration으로 직접 묶지 않는다. IronClaw는
orchestration/MCP client 계층이고, ephemera는 VM, token, guest network,
snapshot file, host cleanup을 다루는 runtime control plane이다. 직접 결합하면
IronClaw가 ephemera의 low-level HTTP API와 resource cleanup semantics에 종속된다.
anvil은 이 결합을 흡수해 IronClaw에는 `anvil_*` tool 계약만 제공하고, 내부에서
session alias, token redaction, workspace 정책, snapshot/restore 의미, 오류
정리를 ephemera API 호출로 변환한다.

anvil의 상위 통합 대상은 IronClaw 전용이다. OpenClaw 연동은 지원 범위가 아니며,
OpenClaw compatibility layer, shared gateway, shared runtime contract를 anvil
요구사항으로 취급하지 않는다.

현재 GitHub 저장소는 `https://github.com/HardcoreMonk/anvil/`이다.
이 저장소는 `https://github.com/steve-seungeui/ephemera`의 fork로 유지한다.
ephemera는 계속 버전업되는 runtime engine upstream이며, anvil은 그 runtime을
IronClaw 실행 계층으로 통합하는 downstream product fork다. 이 저장소의 Go 모듈
경로와 기존 API/환경 변수에는 `ephemera` 또는 `goose` 이름이 남아 있다. anvil
통합 릴리즈는 ephemera runtime tag와 충돌하지 않도록 `anvil-v0.1.0`처럼 별도
prefix를 사용한다. 문서에서는 anvil과 ephemera를 같은 이름으로 취급하지 않는다.

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
| anvil scheduler service | host inventory, quota, placement, snapshot locality를 바탕으로 runtime host 선택을 반환하는 얇은 HTTP service | `cmd/anvil-scheduler`, `internal/anvilmcp` |

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

## Fork와 upstream 정책

- fork network는 유지한다. `HardcoreMonk/anvil`을 standalone repository로 detach하지
  않는다.
- local `origin`은 `HardcoreMonk/anvil`, `upstream`은
  `steve-seungeui/ephemera`를 가리킨다.
- ephemera upstream 반영은 `sync/ephemera-*` 브랜치에서 merge commit으로 수행한다.
  upstream runtime 이력을 보존하기 위해 rebase나 history rewrite를 사용하지 않는다.
- runtime engine 계약이 upstream에서 바뀌면 `cmd/goose-daemon`, `internal/storage`,
  `internal/network`, `internal/vm`의 의미를 우선 존중하고, anvil MCP adapter와
  운영 문서를 그 계약에 맞춰 조정한다.
- upstream tag 확인은 `git ls-remote --tags upstream`을 사용한다. 이미 존재하는
  `v*` tag를 덮어쓰는 `git fetch --tags --force`는 사용하지 않는다.
- anvil release tag는 계속 `anvil-v*` prefix를 사용한다.

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
- MCP adapter tenant 기본값 환경 변수: `ANVIL_MCP_TENANT_ID`
- MCP adapter runtime audit JSONL 환경 변수: `ANVIL_MCP_AUDIT_LOG`
- scheduler service 환경 변수: `ANVIL_SCHEDULER_ADDR`,
  `ANVIL_SCHEDULER_STATE`, `ANVIL_SCHEDULER_QUOTA_STORE`
- profile egress policy directory 환경 변수: `EPHEMERA_EGRESS_PROFILE_DIR`,
  `ANVIL_EGRESS_PROFILE_DIR`
- optional trace export 환경 변수: `ANVIL_OTEL_EXPORTER_OTLP_ENDPOINT`,
  `OTEL_EXPORTER_OTLP_ENDPOINT`

`ANVIL_API_TOKEN`은 프로세스별 의미가 다르다. goose-daemon에서는
`EPHEMERA_API_TOKEN`의 control-plane token alias이고, `cmd/anvil-mcp`에서는
daemon으로 보내는 outbound Bearer token이다.

## 후속 후보

최근 후속 완료 상태:

- `anvil-v0.1.0` 공개 tag와 GitHub Release page는 게시된 상태다.
- MCP v2 workspace copy-in/out과 persistent session store는 구현된 상태다.
- snapshot GC는 `max_total_bytes`와 `snapshots/gc-audit.jsonl` audit record를
  지원한다.
- multi-tenant runtime foundation은 `internal/anvilmcp` 기준으로 tenant ID
  validation, quota decision, scheduler decision, egress policy, runtime audit
  JSONL append/read/retention helper를 제공한다.
- daemon API는 `tenant_id`와 `egress_policy`를 VM/snapshot/restore contract에
  보존하며, MCP adapter는 tenant/egress 값을 daemon 요청 본문으로 전달한다.
- `POST /snapshots/{id}/restore` 응답은 더 이상 `agent_token`을 노출하지 않는다.
- scheduler host inventory polling, runtime router, JSON quota store, daemon tenant
  API, `deny_all` host egress rule, runtime audit API, `/health`, `/metrics`가
  runtime control-plane foundation으로 구현된 상태다.
- `cmd/anvil-scheduler`, persistent `PlacementStore`, snapshot locality preference,
  router retry/failover, placement reconciliation helper가 scheduler service
  foundation으로 구현된 상태다.
- `profile` egress policy는 profile별 `egress.json` allowlist와 DNS server
  allowlist를 host `iptables` rule로 계획/적용할 수 있다. policy 파일이 없으면
  기존 profile 동작과 호환되도록 no-op이다.
- daemon은 `/metrics/vms`, lifecycle duration/queue depth metrics, optional
  OpenTelemetry-compatible trace export를 제공한다.
- anvil MCP tool input struct는 IronClaw/Gemini function declaration에서 빈 type이
  나오지 않도록 정적 schema compatibility 검증을 제공한다.
- Goosetown flock/Town Wall runtime API는 additive `anvil_*` MCP tool surface로
  노출된 상태이며, 기존 VM/snapshot tool 계약을 대체하지 않는다.
- daemon direct `POST /flocks`와 MCP `anvil_spawn_flock`은 blank `task`, empty role,
  path separator가 포함된 role을 VM spawn 전에 거부한다.
- `scripts/anvil-mcp-e2e.sh flock`과 전체 KVM `sudo bash e2e_test.sh` 58단계가
  Goosetown MCP surface와 daemon flock lifecycle 검증 경로에 포함된다.

남은 후속 후보:

- scheduler service의 실제 운영 배포와 host inventory polling daemonization
- snapshot locality의 cross-host snapshot replication
- scheduler-aware cross-host flock placement
- egress allow host rule의 L7 proxy/SNI 기반 강화
- snapshot storage quota dashboard
