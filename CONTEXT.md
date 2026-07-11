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
prefix를 사용한다. **`anvil-v0.7.0`부터 anvil 공개 릴리즈 버전은 upstream ephemera
버전과 동일하게 정렬한다.** 현재 최신(Latest) 릴리즈는 `anvil-v0.7.0`이고 tag
target은 `2f367dd`(태그 시점 main; parity + release-gate hardening + post-release
backlog + open-gate 마감; full KVM e2e 343✓, step 59 실 LLM 포함)이며 설치 아티팩트
(SLIM/FULL tarball + sha256)를 제공한다. 이후 main은 cross-host shared Town
Wall/gtcall/home 재선출 failover, adapter reconcile loop, bounded relay retry,
cross-host snapshot replication 자동화에 더해 routed flock 스택 결함 D1~D4 종결,
web major(vite8/svelte5), scheduler 실배포+installer 검증(PR #19~#46) 등 untagged
작업을 더 포함한다(요지는 아래 "후속 완료 상태", 상세는 dated handoff).

이전 anvil 통합 번호 계보(`anvil-v0.1.0`→`v0.4.0`)와 upstream 시리즈별 마일스톤은
**개발 내역으로 보존한다**(전부 non-latest):
- `anvil-v0.4.0`(`de82481`) — upstream parity(`v0.4.0`-`v0.7.0`)를 처음 통합한 anvil
  통합 릴리즈. 내용상 `anvil-v0.7.0`의 직전 상태.
- `anvil-ephemera-v0.7.0`(`7b3f009`) — v0.7.0 parity 경계 마일스톤(게이트 334✓).
- `anvil-ephemera-v0.6.4`(`04e2a12`) — v0.6.4 (MCP Gateway) 마일스톤(게이트 334✓, source-only).
- `anvil-v0.5.5-snapshot`(`7f207a0` — keep-alive → `64ec57c`), `anvil-v0.4.5-snapshot`
  (`8daf6f3` — restore 500 → `4c1c803`, 크래시 EBUSY → `38fbedc`): 해당 시점 결함으로
  게이트 미통과인 학습 스냅샷 pre-release. 운영 배포용이 아니다.
모든 개발 내역 tag는 학습 브랜치 `annotate/v0.4.5`~`v0.7.0`와 짝을 이룬다. anvil main runtime
baseline은 upstream ephemera `v0.7.0` 병합·적응분을 포함하며, `anvil-v0.3.2`
이후의 scheduler control loop, scheduler `/metrics`, manual cross-host snapshot
replication, scheduler-aware flock placement(placement planner 기반 routed flock
member 생성 포함), cross-host shared Town Wall(2026-07-07, home-host hub +
daemon-to-daemon relay) 위에 `v0.4.0`-`v0.7.0`
runtime·operator 변경을 더한다. 즉 anvil main runtime baseline은 upstream ephemera
`v0.7.0` adapted runtime·operator support를 포함하며, anvil을 수정 없는 ephemera
`v0.7.0`와 동일시하지 않는다. `v0.4.0`-`v0.7.0`은 full KVM gate로 검증한
adopted/adapted baseline이며, 이로써 upstream parity scope(`v0.4.0`-`v0.7.0`)의 코드
편입이 완료됐다. anvil runtime/operator baseline supports upstream ephemera v0.7.0 with
anvil adaptations for token redaction, tenant/egress, scheduler, audit, and IronClaw MCP
surface separation. 전 태그별 채택/적응/deferred/excluded 분류는
[`docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`](docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md)의
parity matrix에 있다. `v0.5.0` operator Web UI(`/ui/`, `/config/*`), `v0.6.0` runtime MCP
Gateway(`EPHEMERA_MCP_*`, `internal/mcpgateway`), `v0.7.0` end-user installer
(`install.sh`/`uninstall.sh`/`ephemera.service.in`)와 transcript restore는
runtime/operator surface로만 채택해 IronClaw `anvil_*` MCP surface로 노출하지 않으며
(runtime MCP Gateway는 `cmd/anvil-mcp` IronClaw adapter를 대체하지 않는다), systemd
service는 canonical `ephemera` 이름을 유지한다(anvil alias wrapper 없음). 남은
deferred/비목표는 `v0.4.2` default COW 전환(KVM burn-in 후 flip)과 runtime MCP
Gateway의 IronClaw 표면 승격 금지(비목표 유지)다. `v0.4.4` flock broadcast의 MCP tool
노출은 기각 확정(2026-07-11, daemon-only 유지), auto-snapshot public support는 env-only
확정(2026-07-11, 공개 표면 미노출), flock member spawn의 per-profile sizing 존중은
완료(2026-07-11)로 세 항목 모두 deferred에서 종결됐다. release-gate 코드 항목 4종
(audit-writer sentinel, stdio stderr scrub, `credential_env` reserved names,
production-mux auth assert)은 2026-07-06 follow-up batch로 닫혔고, 마지막 open gate
(valid provider key `semantic` run, e2e step 59)도 `18c7559`에서 OpenAI `gpt-4o`로
닫혔다(full e2e `343✓/0✗`) — 남은 release-gate open 항목은 없다. 2026-07-06 기준
upstream `main`과
최신 upstream tag는 `v0.7.0`까지 진행되어 있다. `v0.7.0`의 kernel SHA 검증,
`waitForAgent` per-probe timeout, `EPHEMERA_HOME` work directory 지정은 sync 전 독립
hardening backport로 먼저 반영돼 있었고, v0.7.0 병합 시 upstream 버전과 reconcile해 anvil
backport(atomic temp+rename 무조건 검증 포함, upstream보다 stricter)가 single definition으로
남았다(net Go diff는 doc-comment-only). 문서에서는 anvil과 ephemera를 같은 이름으로
취급하지 않는다.

## 진실 기준 문서 순서

1. `CONTEXT.md`: anvil/ephemera/IronClaw 경계, 변경 불가 계약
2. `docs/PUBLIC_RELEASE_BOUNDARY.md`: anvil 공개 포함/조건부 포함/제외 표면
3. `docs/ADR_INDEX.md`: 장기 설계 결정과 upstream ephemera 채택 상태
4. `README.md`: anvil 결합 프로젝트 개요·경계·빠른 시작 진입점 (상세 사용법은 `docs/guides/`)
5. `RELEASE_NOTES.md`: ephemera 릴리즈와 anvil 통합 작업 변화
6. `docs/architecture/*.md`: ephemera runtime, service logic, anvil MCP 설계
7. `docs/analysis/*.md`: ephemera 0.1.0/0.2.0 분석과 보조 설명
8. 업로드된 과거 문서와 초안: 참고 자료

## 도메인 용어집

| 용어 | 의미 | 담당 영역 |
|---|---|---|
| anvil | IronClaw와 ephemera를 결합하는 새 프로젝트 이름 | project-wide |
| IronClaw | MCP client/orchestration 계층. anvil VM 실행 기능을 사용하는 상위 시스템 | 외부/상위 통합 |
| OpenClaw | anvil의 통합 대상이 아님. anvil 문서와 구현은 OpenClaw 운영 계약을 제공하지 않음 | 제외 범위 |
| ephemera | Firecracker MicroVM 기반 격리 실행 runtime. anvil main runtime baseline은 upstream ephemera `v0.7.0` adapted runtime·operator support를 포함하고, 2026-07-02 기준 upstream latest observed는 `v0.7.0`이다. `v0.4.0`-`v0.7.0`은 adopted/adapted baseline으로 upstream parity scope 코드 편입이 완료됐다. | `cmd/goose-daemon`, `internal/*` |
| ephemera control plane | VM 생성, 삭제, snapshot, restore, proxy를 담당하는 호스트 daemon | `cmd/goose-daemon` |
| MicroVM | Firecracker + KVM으로 실행되는 ephemera 격리 실행 환경 | `internal/vm` |
| goose-agent | VM 안에서 prompt 실행, health, stop API를 제공하는 HTTP agent | `cmd/goose-agent` |
| micro-init | VM의 PID 1. 가상 파일시스템 mount, agent 실행, clean poweroff 담당 | `cmd/micro-init` |
| Full snapshot | guest RAM 전체와 rootfs 사본, Firecracker state를 저장한 기준 snapshot | `internal/storage` |
| Diff snapshot | 기준 Full snapshot 이후 dirty memory page만 sparse file로 저장한 snapshot | `internal/storage` |
| COW restore | snapshot rootfs를 read-only base로 두고 per-VM sparse exception store에 쓰기를 기록하는 restore 방식 | `internal/storage` |
| IronClaw MCP adapter (`ANVIL_MCP_*`) | IronClaw가 ephemera daemon API를 anvil tool로 호출하게 해 주는 stdio bridge. 설정은 `ANVIL_MCP_*` 환경 변수를 사용한다. runtime MCP Gateway와 별개 개념이다. | `cmd/anvil-mcp` |
| runtime MCP Gateway (`EPHEMERA_MCP_*`) | upstream `v0.6.0` runtime MCP Gateway. VM 내부 agent에 backend MCP server를 policy·rate-limit·audit로 중개하는 daemon-side runtime/operator surface. `EPHEMERA_MCP_*` 환경 변수를 사용하며 IronClaw adapter(`cmd/anvil-mcp`)를 대체하지 않는다. | `internal/mcpgateway`, `cmd/goose-daemon` |
| anvil scheduler service | host inventory, quota, placement, snapshot locality를 바탕으로 runtime host 선택을 반환하는 얇은 HTTP service | `cmd/anvil-scheduler`, `internal/anvilmcp` |
| routed flock | scheduler placement planner로 여러 host daemon에 member VM을 배치하는 cross-host flock. control plane(`internal/anvilmcp`)이 registry(`PlacementStore`)를 소유한다 | `internal/anvilmcp` |
| Town Wall hub/relay | flock 공유 게시판의 cross-host 형태. routed flock의 home host(= `roles[0]` 배치 host) daemon이 canonical wall을 hub flock으로 소유하고, 나머지 member host daemon은 relay flock으로 post를 forward, wall/history/SSE를 proxy한다. 단일 host flock은 계속 local kind다 | `internal/orchestrator`, `cmd/goose-daemon` |
| relay_token | routed flock의 **guest 능력 토큰**(per-flock secret, 2026-07-07 A안 재해석). 해당 flock의 wall sub-path(`post\|wall\|wall/history`) **와 `call` 진입**을 모두 admit한다 — guest가 로컬 daemon에서 gtwall/gtcall 모두 이 토큰으로 인증한다(단일 host flock의 guest CP token→gtcall 개방과 동형). control-plane bearer로 승격하지 않으며 모든 직렬화 표면에서 redaction된다 | `cmd/goose-daemon`, `internal/anvilmcp` |
| call_token | routed flock의 daemon-to-daemon **call hop 전용** per-flock secret(member→home, home→target). `relay_token`과 나란한 규율이지만 admit 범위는 더 좁다 — **오직 `/flocks/{id}/call` 경로만** admit하고 wall sub-path는 거부한다(control-plane bearer 승격 금지). `PlacementStore`에 영속되고 모든 직렬화 표면에서 redaction된다 | `cmd/goose-daemon`, `internal/anvilmcp` |
| 공개 릴리즈 경계 | anvil이 공개적으로 책임지는 기능 표면과 제외 표면 | `docs/PUBLIC_RELEASE_BOUNDARY.md` |
| ADR | 공개 경계, token/auth, MCP tool 계약, runtime lifecycle 같은 장기 결정을 남기는 기록 | `docs/adr/*.md` |

## 경계 규칙

- `docs/analysis/`는 ephemera 0.1.0/0.2.0 분석 근거 자료다. 제목과 설명은
  ephemera 릴리즈 분석임을 명확히 해야 한다.
- `README.md`는 anvil 결합 프로젝트의 현재 진입점이며 개요·경계·빠른 시작·문서
  지도를 요약·링크한다. ephemera runtime 사용법 상세(실행 모델, 설정, API, 보안,
  MCP 어댑터 등)는 anvil의 기반 runtime 설명으로 `docs/guides/`에 분리해 둔다.
- 코드 식별자, API 경로, 환경 변수, 파일 경로는 실제 구현과 호환성이
  더 중요하므로 임의로 한국어화하지 않는다.
- `ephemera`라는 이름이 남아 있는 API/환경 변수는 기반 runtime 계약으로
  취급한다. 이것을 anvil 제품명으로 덮어쓰지 않는다.
- 공개 운영 URL은 reverse proxy/TLS 계층에서 결정한다. 현재 로컬 검증
  환경에서는 사용자가 지정한 `192.168.3.73` 주소를 기준으로 한다.
- 공개 기능을 추가하거나 upstream ephemera 변경을 병합할 때는
  `docs/PUBLIC_RELEASE_BOUNDARY.md`의 포함/조건부 포함/제외 표면을 확인한다.
- upstream ephemera 변경이 anvil 정책과 충돌하면 그대로 채택하지 않고
  `adopted`, `adapted`, `excluded`, `deferred`, `historical` 중 하나로 분류한다.
- token/auth, MCP output, VM lifecycle, snapshot/restore, cleanup 의미를 바꾸는
  결정은 `docs/ADR_INDEX.md`와 `docs/adr/*.md`에 남긴다.

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
- daemon work directory canonical 환경 변수: `EPHEMERA_HOME`
- nested task depth guard canonical 환경 변수: `EPHEMERA_MAX_TASK_DEPTH`
  (upstream `v0.4.4` 신설, ANVIL alias 없음, 기본값 `5`, 한계 도달 시 `508`)
- per-VM sizing canonical 환경 변수: `EPHEMERA_VCPU_COUNT`, `EPHEMERA_MEM_SIZE_MIB`
  (profile `goose.yaml`에서 읽음, unset이면 default `1` vCPU / `1024` MiB,
  ANVIL alias 없음. `POST /vms`와 flock member spawn(createFlock·add-agent) 모두 존중 — 2026-07-11)
- runtime MCP Gateway canonical 환경 변수: `EPHEMERA_MCP_ENABLED`,
  `EPHEMERA_MCP_SERVERS`, `EPHEMERA_MCP_PORT`, `EPHEMERA_MCP_BIND_IP`,
  `EPHEMERA_MCP_RATE`, `EPHEMERA_MCP_BURST`, `EPHEMERA_MCP_STDIO_USER`
  (upstream `v0.6.x` 신설, ANVIL alias 없음. `EPHEMERA_MCP_RATE` 기본 `0`=unlimited,
  `EPHEMERA_MCP_STDIO_USER` 기본 `nobody`, `EPHEMERA_MCP_BIND_IP` unset이면 안전한
  bridge IP bind. adapter 설정 `ANVIL_MCP_*`와 별개 namespace다)
- guest anti-spoof canonical 환경 변수: `EPHEMERA_NET_ANTISPOOF`
  (upstream `v0.6.1` 신설, 기본 on, ebtables best-effort, ANVIL alias 없음)
- MCP adapter daemon URL 환경 변수: `ANVIL_DAEMON_URL`
- MCP adapter token 환경 변수: `ANVIL_API_TOKEN`
- MCP adapter tenant 기본값 환경 변수: `ANVIL_MCP_TENANT_ID`
- MCP adapter runtime audit JSONL 환경 변수: `ANVIL_MCP_AUDIT_LOG`
- MCP adapter reconcile 주기 환경 변수: `ANVIL_MCP_RECONCILE_INTERVAL`
  (`time.ParseDuration` 형식, 기본 `60s`, `0`=off. `members_only` cross-host
  모드에서만 루프가 돌며 daemon 재시작 후 hub/relay wall 등록을 자동 복구)
- scheduler service 환경 변수: `ANVIL_SCHEDULER_ADDR`,
  `ANVIL_SCHEDULER_STATE`, `ANVIL_SCHEDULER_QUOTA_STORE`
- scheduler resident host-inventory polling 환경 변수: `ANVIL_SCHEDULER_HOSTS_FILE`
  (control loop가 poll할 host 인벤토리 JSON; unset이면 persistent state의 host만
  poll), `ANVIL_SCHEDULER_POLL_INTERVAL`(기본 `10s`), `ANVIL_SCHEDULER_RECONCILE_INTERVAL`
  (기본 `30s`), `ANVIL_SCHEDULER_HOST_TIMEOUT`(기본 `3s`), `ANVIL_SCHEDULER_FAILURE_THRESHOLD`
  (기본 `3`, `>=1`), `ANVIL_SCHEDULER_API_TOKEN`(daemon `/health`,`/vms` poll에 실을 bearer,
  unset이면 미인증 poll), `ANVIL_SCHEDULER_REQUIRE_PERSISTENCE`(`true`면 state 저장 저하 시
  scheduling `503`). 모두 `cmd/anvil-scheduler`가 이미 읽으며 ANVIL alias 없음. 설치
  스크립트는 operator가 설정한 값만 systemd env 파일에 기록한다(`ANVIL_SCHEDULER_HOSTS_SRC`는
  hosts JSON을 `ANVIL_SCHEDULER_HOSTS_FILE`로 설치하는 설치 스크립트 전용 knob)
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
- `anvil-v0.2.0` 공개 tag와 GitHub Release page는 2026-05-15 17:53:21 KST에
  게시된 상태다. Release URL은
  `https://github.com/HardcoreMonk/anvil/releases/tag/anvil-v0.2.0`이고, tag
  target은 `5b8298fab17b455a9e4e4325618d2743d9486a6c`다.
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
- upstream `v0.3.1`-`v0.7.0` runtime/operator 변경은 전부 anvil main runtime
  baseline으로 병합·적응됐고 full KVM gate로 검증됐다(upstream parity scope 코드
  편입 완료). 태그별 adopted/adapted/deferred/excluded 분류와 보안 경계 원문은
  [`docs/ADR_INDEX.md`](docs/ADR_INDEX.md) §4(+ §3의 sizing/keep-alive 절),
  [`docs/analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md`](docs/analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md),
  [`docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`](docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md)에
  있고 이 목록은 그 요지만 둔다: `v0.3.x` Goosetown/watchdog/cold-restart/CP token
  injection/gtcall·gtwall(단 `POST /flocks` `agent_tokens` 응답 노출은 anvil 보안
  불변으로 미채택), `v0.4.x` storage/COW/nested depth guard/snapshot-restore
  auto-recovery(v0.4.5 restore가 참조하는 source snapshot의 `DELETE`를 `409`로 보호 —
  upstream e2e 46c `200` orphan과 의도적 divergent), `v0.5.x` operator Web UI/`/config/*`
  (`goose-secrets.yaml` 미노출 sentinel)·default sizing 1vCPU/1024MiB, `v0.6.x` runtime
  MCP Gateway(caller profile server-side 판정·credential host-side·audit metadata-only·
  policy widen 불가로 경계 구조적 강제), `v0.7.0` installer/transcript restore + release
  build integrity(kernel/fc `sha256sum -c`). 전부 runtime/operator surface로만 채택 —
  IronClaw `anvil_*` MCP surface 미노출, `cmd/anvil-mcp` adapter 불대체, systemd는
  canonical `ephemera` 이름 유지.
- keep-alive divergence(upstream 기여 후보): `v0.5.x` `gracefulAgentStop`이 v0.2.0부터
  잠재하던 upstream shared pooled agent proxy client 결함(guest IP 재활용 시 stale
  connection → restored VM `/tasks` hang/`502`)을 `64ec57c`가 request마다 fresh
  dial(`DisableKeepAlives`)로 고쳤다(connection-reuse guard test). upstream connection
  pooling과 의도적 divergence. 상세 [`docs/ADR_INDEX.md`](docs/ADR_INDEX.md) §4 keep-alive 절.
- cross-host shared Town Wall(2026-07-07, PR #19)이 main에 편입됐다. routed flock
  member가 home-host hub + daemon-to-daemon relay로 하나의 공유 Town Wall을
  사용한다. `POST /vms`가 flock identity(`flock_id`/`agent_id`/
  `control_plane_token`)를 수용해 routed member VM에서 `gtwall`이 작동하고, daemon
  `POST /flocks/{id}/distributed`·`POST /flocks/{id}/relay`가 hub/relay 등록을
  제공한다(다른 kind가 점유한 id는 `409`). per-flock `relay_token`은 해당 flock의
  wall sub-path를 admit하며(도입 당시 wall 전용 — cross-host gtcall부터 guest
  능력 토큰으로 `call` 진입도 admit, 아래 항목; 전 표면 redaction, auth metric
  `relay` outcome +
  `relay:<flockID>` identity 기록), rollback/delete는 spawn 실패 member를 포함해
  hub/relay 등록을 해제하고 reconcile이 daemon 재시작 후 재등록한다. guest는
  bridge-only를 유지하고 신규 `anvil_*` MCP tool은 없다(runtime/operator 표면).
  KVM e2e `scripts/anvil-cross-host-wall-e2e.sh`(real member VM + stub home)로
  검증됐다. home host는 도입 당시 SPOF였으나 재선출 failover(아래 항목)로 해소됐다.
  cross-host `gtcall`/broadcast fan-out과 relay retry는 비범위였다(각각 별도
  slice로 편입, broadcast fan-out은 계속 비목표).
- `ReconcilePlacements`의 주기적 control loop 배선이 2026-07-07 reconcile-loop
  slice(`b32cd72`, `6c1ca87`)로 완료됐다. `reconcile_interval`/
  `ANVIL_MCP_RECONCILE_INTERVAL`(기본 `60s`, `0`=off)과 `StartReconcileLoop`가
  `members_only` 모드에서 daemon 시작 시 주기적으로 `ReconcilePlacements`를 호출해
  hub/relay wall 등록과 relay-token admission을 자동 복구한다.
- cross-host gtcall(2026-07-08)이 main에 편입됐다. routed flock의 임의 member가
  다른 임의 member를 daemon `POST /flocks/{id}/call`(`{agent_id, prompt}`)로
  호출한다 — member→home→target 2-hop(home이 canonical roster로
  agent→host,vm_id를 해석). hub 등록 roster가 `{AgentID, Host, VMID, Addr}`로
  확장되고, home 재등록/reconcile이 spawn 완료 후 VMID 포함 roster로 갱신한다.
  relay 등록도 host-local `agents: [{agent_id, vm_id}]`를 포함해, hopped call이
  kind와 무관하게 **로컬 해석 전용**으로 성립한다(2026-07-08 설계 보정 — target
  member daemon은 relay kind로 등록돼 있으므로 hopped call을 무조건 404로
  끝내면 실토폴로지에서 2번째 hop이 성립하지 않던 문제를 고쳤다). **토큰 모델
  (A안)**: `relay_token`은 guest 능력 토큰으로 그 flock의 wall과 `call` 진입을
  모두 admit하고, 별도 `call_token`은 daemon-to-daemon call hop 전용으로 `call`
  경로만 admit하며 wall 경로는 거부한다(`call_token`→wall 방향 배타를 테스트로
  고정). 이 slice가
  wall slice의 잠재 결함(`registerRelayFlock`이 admit 등록을 하지 않아 auth-on
  member daemon에서 routed guest의 gtwall이 401되던 문제)도 동반 수정한다.
  `X-Ephemera-Call-Hop`으로 loop guard, `X-Ephemera-Task-Depth` 전파로
  cross-host에서도 `EPHEMERA_MAX_TASK_DEPTH`가 성립한다. 기존 2-step
  `GET /flocks/{id}` + `POST /vms/{vm_id}/tasks` 계약은 하위호환 유지, 신규
  `anvil_*` MCP tool 없음. KVM e2e `scripts/anvil-cross-host-gtcall-e2e.sh`
  (real member VM + stub home, **auth-on** member daemon)로 18/18 checks ×2회
  검증됐다.
- bounded relay retry(PR #23, `e94028b`, `6317a58`)가 main에 편입됐다.
  daemon-to-daemon relay hop 3곳(wall post relay, call hop 양방향, wall
  history relay)이 dial-실패로 한정된 transport 에러에만 동기 bounded
  retry(최대 2회, 총 3시도, backoff 1s→2s)를 적용해 짧은 네트워크 순단을
  자동 흡수한다. reset/EOF·HTTP 응답·ctx 취소는 재시도 대상에서 제외해
  전달 semantics는 불변이다. SSE relay 재접속은 비범위.
- wall relay 에러 redaction 정합(PR #24, `be73461`)이 main에 편입됐다. wall
  relay 4개 에러 지점(post, history, stream 요청 빌드, stream relay)이 전부
  call 경로와 동일하게 flock id만 노출하는 opaque 502 에러로 바뀌어, home
  daemon 주소가 더 이상 어떤 relay hop에서도 노출되지 않는다.
- home 재선출 failover가 main에 편입돼 routed flock home host(hub) SPOF를
  해소했다. **kind 전환**: daemon `POST /flocks/{id}/distributed`가 relay
  점유 id 위에서 hub로 승격(`201`), `POST /flocks/{id}/relay`가 hub 점유 id
  위에서 relay로 강등(`201`)한다 — 두 endpoint 모두 CP bearer 전용(relay/call
  token은 admit 대상 아님), local flock은 양쪽 모두 `409` 불변 보호. 이
  kind 전환은 spec 원안(기존 배관 재사용 가정)의 보정이다 — D1 fix(PR #30)의
  kind 충돌 `409` 가드 때문에 daemon 쪽 승격/강등이 필수가 됐다. **감지·선출·
  전환**: adapter reconcile 루프가 flock 단위로 연속 `homeFailureThreshold`회
  (상수, 기본 3) dial-계열 home 실패를 관측하면, `record.Agents` 순서상 첫
  생존 host(구 home 제외)로 결정적 재선출한다(후보 0이면 no-op). 전환은
  `HomeHost` 영속(원자적 전환점) → 새 home hub 승격 등록 → 구 home 포함 전
  member relay 재등록 → 구 home best-effort `DELETE` 순서이며, 어느 단계가
  실패해도 다음 reconcile 주기가 idempotent 수렴한다. **wall 손실 명시
  계약**: 새 home은 빈 log에서 seq를 재시작하고, 구 home 디스크의 이전 기록은
  병합되지 않는다(agent 관점에서 과거 메시지가 사라진 것으로 보임). relay/call
  token은 flock 단위 불변 재사용이라 guest는 무중단·무개입이며, 자동
  fail-back은 없다(구 home 부활 시 다음 reconcile이 relay로 강등해 heal).
  유닛 8종(`TestFailover_*`, `internal/anvilmcp/home_failover_test.go`) +
  KVM e2e `scripts/anvil-cross-host-failover-e2e.sh`(stub A→stub B 재선출 +
  real daemon relay→hub 승격, wall 손실 계약 관측, redaction 검증, 3회
  연속 green)로 검증됐다. 실 2-daemon 수동 검증(§6b)은 2026-07-11 수행 완료
  — 전 세부 단계 PASS(전환 창 실측 ~27s @`reconcile 10s`, hub→relay 강등·wall
  손실 계약·양방향 wall/gtcall 재확인·redaction·정리+revoke; 신규 결함 없음).
  상세:
  [`docs/superpowers/specs/2026-07-08-home-failover-design.md`](docs/superpowers/specs/2026-07-08-home-failover-design.md),
  [`docs/operations/2026-07-11-home-failover-handoff.md`](docs/operations/2026-07-11-home-failover-handoff.md),
  [`docs/operations/2026-07-11-6b-failover-verification-run.md`](docs/operations/2026-07-11-6b-failover-verification-run.md).
- `scripts/anvil-mcp-e2e.sh flock`, 전체 KVM `sudo bash e2e_test.sh`, script-only
  workload runner E2E, cross-host wall relay E2E
  (`scripts/anvil-cross-host-wall-e2e.sh`), cross-host gtcall relay E2E
  (`scripts/anvil-cross-host-gtcall-e2e.sh`), cross-host home failover E2E
  (`scripts/anvil-cross-host-failover-e2e.sh`)가 Goosetown MCP surface, daemon
  flock lifecycle, deterministic workload, cross-host relay/call/failover 검증
  경로에 포함된다.
- cross-host snapshot replication 자동화가 구현됐다. adapter(`RuntimeRouter`)
  reconcile 루프가 매 주기 desired replica factor(**상수 N=2**, 원본+복제 1)
  미달 스냅샷을 discover(probe-reachable daemon `ListSnapshots` add-only union)
  → drift 계산 → `SelectRuntimeHost`로 대상 선정(tenant/egress carry) →
  `ReplicateSnapshot` 1회 재사용 시도로 heal한다. dial 실패는
  (snapshot,target) in-memory 카운터로 세어 **연속 3회(상수 cap)** 도달 시
  giving-up으로 표시하고, 대상 host 복귀 관측 시 카운터를 리셋한다(무한
  재시도 금지, home failover와 동일한 재평가 규율). D3 coarse-fs 거부와
  tenant/검증 실패는 **terminal**로 분류해 그 대상에 대한 재시도를
  제외하지만, 이 exclude는 adapter(`cmd/anvil-mcp`) 프로세스 수명 한정이라
  **adapter 재시작이 re-arm**한다(카운터 청소는 positive-evidence 규칙만
  사용 — 실제 삭제가
  관측될 때만 GC, 복제본 GC/전파 자체는 비목표). metric family
  `anvil_scheduler_snapshot_replication_*`(`attempts_total{outcome,reason}`,
  `latency_seconds{phase="total"만}`, `queue_depth`, `giving_up`,
  `last_success`/`last_failure_timestamp_seconds`)가 `PlacementStore`에
  영속되고 scheduler `/metrics`에 노출된다. anvil 경계는 metric 노출 +
  runbook 권장 alert 식까지이며 실 alerting은 zone 대시보드 몫이다. 유닛
  (`-race`) + KVM e2e `scripts/anvil-snapshot-replication-e2e.sh`(대상
  down→dial 실패→복귀→자동 복제→metrics 전이 관측, 2연속 green)로
  검증됐다. 실 2-daemon(비-stub) 수동 검증(PR #45)도 2026-07-11 수행 완료 —
  host-b가 실 daemon으로 import bundle을 수신·저장·재서빙하는 조건에서 자동
  복제·dial-cap giving-up·복귀 reset·수렴·metric 전이·redaction 전부 PASS
  (신규 결함 없음). 상세:
  [`docs/superpowers/specs/2026-07-11-snapshot-replication-automation-design.md`](docs/superpowers/specs/2026-07-11-snapshot-replication-automation-design.md),
  [`docs/operations/2026-07-11-snapshot-replication-automation-handoff.md`](docs/operations/2026-07-11-snapshot-replication-automation-handoff.md),
  [`docs/operations/2026-07-11-replication-multihost-verification-run.md`](docs/operations/2026-07-11-replication-multihost-verification-run.md).
- routed flock 스택 결함 D1~D4가 전부 종결됐다(2026-07-10~11). D1: daemon 재시작
  시 routed flock 분산 상태 유실 + 재등록 비멱등 `409`(PR #30·#31, hub/relay 토큰
  refill 포함). D2: routed flock delete가 이미 소멸한 VM의 `DELETE` `404`를
  cleanup 실패로 오보고하던 것을 "이미 소멸"로 분류(진짜 실패는 계속
  `cleanup_failed`, 경계 테스트 고정, PR #37). D3: ZFS 등 coarse-hole(recordsize
  >4K) 파일시스템에서 sparse diff snapshot의 hole이 record 단위로만 보고돼 guest
  메모리를 오염시키던 것을 `ProbeHoleGranularity`(fine=`HoleGranularityFine` 4096B)
  기반 창설측 diff→full 강등 + 판독측 overlay 거부(`refusing overlay ... (see D3)`)
  로 방어(PR #36). D4: cow-스폰 VM의 diff-restore가 ZFS+전체 게이트 부하에서만
  guest kernel panic(GPF)하던 fsync 부재 durability race를 `overlaySparseDiff`가
  loop attach 전 `out.Sync()`로 커밋하도록 수정(PR #46, 재-burn-in host-a full cow
  gate `334✓/0✗` green). D1~D3 상세:
  [`docs/operations/2026-07-10-cross-host-verification-run-handoff.md`](docs/operations/2026-07-10-cross-host-verification-run-handoff.md),
  D4 상세:
  [`docs/operations/2026-07-11-cow-burnin-run.md`](docs/operations/2026-07-11-cow-burnin-run.md).
- `v0.4.2` default COW 전환 KVM burn-in 1차가 수행됐다(PR #41 트랙 B). run 1은
  host-a full cow gate에서 FAIL(step 31 diff-restore `500`) → D4 회부 → D4 fix
  (PR #46) 후 재-burn-in `334✓/0✗` green. 2026-07-11 "burn-in 후 전환" 결정의
  조건("D4 해소·재-burn-in 통과")이 충족됐고, 실제 default flip은 별도 승인
  slice로 남는다(아래 "남은 후속 후보").
- web frontend가 vite 8 + svelte 5 major로 업그레이드됐다(legacy-compat 마이그레이션,
  PR #39). `web/package.json`은 `svelte ^5.56`/`vite ^8.1`/`@sveltejs/vite-plugin-svelte
  ^7.2`, embedded `cmd/goose-daemon/uidist/` 번들도 재생성됐다. svelte 5 runes 전환은
  선택적 후속(미착수).
- anvil-scheduler 실운영 systemd 배포 + host inventory polling 상주화가 완료됐다
  (PR #40 트랙 A). host-a에 `anvil-scheduler.service`가 상주(active+enabled, control
  loop running, host-b healthy 관측·toggle·restart 지속성·redaction 검증). 설치
  스크립트(`scripts/install-anvil-scheduler-systemd.sh`)가 operator 지정 polling
  knob(`HOSTS_FILE`/`POLL_INTERVAL`/`RECONCILE_INTERVAL`/`HOST_TIMEOUT`/
  `FAILURE_THRESHOLD`/`API_TOKEN`/`REQUIRE_PERSISTENCE`)만 env 파일에 기록하고
  API token은 dry-run에서 `<redacted>`. installer 실 systemd host 검증(FULL variant,
  PR #40 트랙 B) 중 release 설치본 daemon 기동 결함을 발견했다 —
  `storage.EnsureGooseAgent`가 `cmd/goose-agent` 소스 트리를 WalkDir 하드 요구해
  소스 없는 `/opt/ephemera` 설치본에서 `walk goose-agent sources ... fatal`. fix
  (PR #42)로 소스 부재 시 shipped `artifacts/goose-agent`+`.sha256` stamp를
  current로 수용(재빌드 없음, 둘 중 하나라도 없으면 진단 에러)하고 `build_release.sh`
  가 `print-goose-agent-source-hash`로 stamp를 동봉하도록 고쳐, host-b FULL
  재검증에서 daemon 기동 성공 + VM spawn(`{"status":"idle"}`)/rm smoke +
  `uninstall.sh --purge` 원복(격리 유지)까지 통과. runtime MCP Gateway 운영 정책은
  `runbook.md` 문서화 + live dry-run(credential 0회 노출)까지 검증됐다(PR #40 트랙 C,
  실 backend operator 배포 검증은 잔여). 상세:
  [`docs/operations/2026-07-11-scheduler-ops-deploy-handoff.md`](docs/operations/2026-07-11-scheduler-ops-deploy-handoff.md).
- ~~비동기 relay buffer~~는 **기각 확정(2026-07-11)**이다. failover가 home 불능
  창을 유계화(~3×reconcile interval; §6b 실측 ~27s@10s)해 명분이 소멸했고, buffer는
  wall 손실 계약과 충돌하는 부분-부활 semantics·전달 보장 약화·seq/중복 복잡성을
  재도입한다. 필요 대두 시 대안은 guest-side `gtwall` 지수 backoff 재시도(미등재).
  상세: [`docs/operations/2026-07-11-6b-failover-verification-run.md`](docs/operations/2026-07-11-6b-failover-verification-run.md).

남은 후속 후보:

- `v0.4.2` default COW default flip — burn-in 조건이 충족됐다(D4 해소·재-burn-in
  host-a full cow gate `334✓/0✗` green, 위 완료 항목). flip 가능 상태이나 **실제
  전환은 별도 승인 slice로 대기**. (auto-snapshot env-only 확정·`v0.4.4` broadcast
  MCP 노출 기각 확정·flock member per-profile sizing 존중 완료는 2026-07-11 종결 —
  후보 아님.)
- upstream 기여 후보 2건: proxy agent client keep-alive 비활성화(`64ec57c`, guest IP
  재활용 시 stale pooled connection), diff-restore fsync(`overlaySparseDiff`
  `out.Sync()`, D4). 둘 다 upstream ephemera에 동형 경로가 있으면 같은 잠재 결함이라
  upstream 확인 후 기여 검토.
- runtime MCP Gateway backend 실 operator 배포 검증. 운영 정책(backend→profile
  바인딩, rate-limit/burst, `secrets.yaml` 규율)은 `runbook.md` 문서화 + live
  dry-run까지 완료(PR #40 트랙 C) — 실 backend server를 붙인 operator 배포 검증만 잔여.
- egress allow host rule의 L7 proxy/SNI 기반 강화
- snapshot storage quota dashboard
- web svelte 5 runes 전환(선택) — PR #39는 legacy-compat 유지, runes 마이그레이션 미착수
- fc upstream/OpenZFS 참고 보고 검토(D3의 fc diff "sparseness=의미" 상호작용)
- e2e 포트 공유 특성 — cross-host wall/gtcall/failover e2e 스크립트가 기본 포트를
  공유해 동시 실행 불가(각 handoff Follow-Up 기록, 소소한 잔여)
