# ANNOTATIONS — anvil v0.6.x 학습 주석 브랜치

## 브랜치 목적

이 브랜치(`annotate/v0.6.4`)는 **학습용 참고 전용(reference-only)** 브랜치다. anvil 이
upstream ephemera `v0.6.0`–`v0.6.4`(upstream 에는 `v0.6.3` 없음)를 병합·적응한 시점의
코드에, 이해를 돕는 한국어 주석(`//` 줄 주석)만 추가했다. 코드 로직은 단 한 줄도
바꾸지 않았고, 이 브랜치는 **절대 merge 되지 않는다** — anvil main 라인과는 독립된
스터디용 스냅샷이다.

주석의 초점은 v0.6.x 시리즈가 도입한 **runtime MCP Gateway**(`internal/mcpgateway`,
`EPHEMERA_MCP_*`)다: 요청이 신원 해석 → policy 교집합 → rate limit → backend 호출 →
audit 기록 순으로 흐르는 구조, 호출자 신원이 세션이 아니라 host 가 판정하는 source IP
로만 정해지는 구조, credential 이 VM에 절대 전달되지 않는 구조를 코드 단위로
설명한다.

## 기준 커밋

`04e2a12` — `fix(runtime): adapt ephemera v0.6.4 stdio servers-listing leak guard`

이 커밋은 `sync/ephemera-v0.7-core-service-parity` 라인에서 `v0.6.0`–`v0.6.4` 병합·적응
Phase 3 이 끝난 시점이다(참고: `/data/projects/codex-zone/anvil-ephemera-parity`의
`docs/operations/2026-07-05-ephemera-v0.6-mcp-gateway-sync-handoff.md`). `v0.7.0`
(installer/transcript/hardening)은 이 시점에서 미병합 backlog로 남아 있다.

## 파일 목록 및 요약

| 파일 | 요약 |
|---|---|
| `internal/mcpgateway/gateway.go` | Gateway 본체. `ServeHTTP` 가 JSON-RPC 요청을 받아 identity→policy→rate→backend→audit 순으로 처리하는 `tools/*`(v0.6.0)와 `resources/*`·`prompts/*`(v0.6.2) handler 를 담는다. |
| `internal/mcpgateway/identity.go` | caller 신원 해석. source IP를 VM registry와 대조해 server-side 로만 `Caller{VMID, Profile}`을 판정 — 세션/헤더로 신원을 자칭할 수 없다. |
| `internal/mcpgateway/policy.go` | profile → `Policy` 매핑. `staticPolicyStore.For`는 servers.yaml 의 `profiles:` 허용 집합을 EPHEMERA_MCP_SERVERS 바인딩과 **교집합으로만 축소**한다(절대 확장 불가). |
| `internal/mcpgateway/registry.go` | `configs/mcp/{servers,secrets}.yaml` 로드 및 backend(HTTPBackend/StdioBackend) 조립. `ServerConfig`(내부, credential 문자열 보유 가능)와 `ServerInfo`(외부 API/UI 노출용, secret-free)의 타입 분리가 핵심. |
| `internal/mcpgateway/ratelimit.go` | v0.6.1 token-bucket rate limiter. 키는 반드시 `(VMID, server)` 2차원 — VM 단위나 server 단위로 뭉치면 우회/기아 문제가 생긴다. |
| `internal/mcpgateway/secrets.go` | HTTP backend 전용 `CredentialProvider`. `Authorization: Bearer` 헤더 주입이 credential 이 네트워크로 나가는 유일한 지점. |
| `internal/mcpgateway/backend.go` | v0.6.0 core 의 유일한 backend, `HTTPBackend`(Streamable HTTP/SSE). lazy initialize, 세션 재사용, credential 은 요청 직전 헤더 주입. |
| `internal/mcpgateway/backend_stdio.go` | v0.6.4 `StdioBackend`(로컬 subprocess). `minimalChildEnv`(child env 백지 구성), `credential_env`(child env 단일 경로), `applyRlimits`(rlimits), root 실행 시 `nobody` 강하 + scratch dir, `Setpgid`+그룹 SIGKILL(pgid 재활용 안전 reap)을 구현. |
| `cmd/goose-daemon/mcp_gateway.go` | goose-daemon 이 gateway 를 실제로 배선하는 곳. 별도 리스너(bridge IP), `EPHEMERA_MCP_*` env 진입점, `observeMCPCall`/`appendMCPAudit`(고정 key set, metadata-only audit). |
| `cmd/goose-daemon/mcp_api.go` | `/config/mcp`, `/config/mcp/servers` — daemon 기존 API mux(auth 뒤)에 배선되는 operator 조회 API. servers 목록은 구조적으로 credential/args 비노출. |
| `internal/network/antispoof.go` | v0.6.1 `EPHEMERA_NET_ANTISPOOF`(기본 on). ebtables 로 TAP 포트를 daemon 이 할당한 MAC+IP 에 고정 — mcpgateway 의 source-IP 신원 판정을 네트워크 계층에서 보강하는 defense-in-depth. ebtables 부재 시 best-effort(조용히 미적용, daemon 은 안 죽음). |
| `internal/storage/provisioner.go`(MCP 주입부) | `VMPrepareOptions.MCPGatewayURL`/`MCPGatewayHostsEntry` 필드와 `injectVMFiles` 의 해당 블록. VM 에는 gateway URL과 `/etc/hosts` alias 엔트리만 주입되고, 이 struct 에는 backend credential 필드 자체가 없다. |

## 시리즈 개요 (태그별 기여)

| 태그 | 기여 |
|---|---|
| `v0.6.0` | runtime MCP Gateway 골격 — `internal/mcpgateway`(identity/policy/registry/backend/gateway), daemon `/config/mcp*` handler, `configs/mcp/*.example`, Web UI MCP console. |
| `v0.6.1` | `EPHEMERA_NET_ANTISPOOF` 기본 on(ebtables best-effort), per-(VM,server) token-bucket rate limit(`EPHEMERA_MCP_RATE`/`BURST`). |
| `v0.6.2` | resources/prompts 표면 추가(aggregate + per-tool/per-profile policy), resources/prompts 가 tools 와 policy·rate bucket 공유, audit `kind` 필드. |
| `v0.6.4` | stdio backend(subprocess), child env 재구성(`minimalChildEnv`), `credential_env`, root 면 `nobody`+scratch dir, process-group reap, `GET /config/mcp/servers` 는 `has_credential` 만 노출. |

upstream 에는 `v0.6.3` 태그가 없다(`v0.6.2` 다음 바로 `v0.6.4`).

## 두 MCP 표면의 분리 — 반드시 구분할 것

이름이 비슷하지만 방향과 목적이 완전히 다른 두 표면이 anvil 코드베이스에 공존한다.

| 구분 | IronClaw MCP adapter | runtime MCP Gateway (이 브랜치의 주석 대상) |
|---|---|---|
| 목적 | IronClaw가 anvil VM lifecycle을 `anvil_*` tool로 호출 | VM 내부 agent가 host가 중개하는 backend MCP server를 사용 |
| 방향 | north-bound (IronClaw → anvil) | south-bound (guest agent → daemon → backend) |
| 구현 | `cmd/anvil-mcp`, `internal/anvilmcp` | `internal/mcpgateway`, `cmd/goose-daemon` |
| 설정 | `ANVIL_MCP_*` | `EPHEMERA_MCP_*` |
| surface | IronClaw MCP surface(제품 통합 대상) | runtime/operator surface |

runtime MCP Gateway는 `cmd/anvil-mcp` IronClaw adapter를 **대체하지 않는다**. gateway
tool은 IronClaw `anvil_*` tool 목록·schema에 절대 추가되지 않으며, 이는 아래 가드
테스트로 고정돼 있다. 참고 문서: 이 워크트리의 `docs/architecture/mcp-architecture.md`
(IronClaw adapter 상세)와 `anvil-ephemera-parity` 브랜치의
`docs/operations/2026-07-05-ephemera-v0.6-mcp-gateway-sync-handoff.md`(runtime gateway
sync 상세).

## anvil 경계 가드 테스트 8종

v0.6.0 도입 시점의 4종 경계 가드 + 이후 v0.6.1/v0.6.2/v0.6.4 하드닝을 고정하는 4종
가드로 총 8종이다. 전부 upstream 이 커버하지 않는, anvil 이 추가한 회귀 방지 테스트다.

**v0.6.0 경계 가드 (4종)**

1. **IronClaw tool 목록에서 gateway tool 제외** — `internal/anvilmcp/ironclaw_schema_test.go`
   의 `TestCurrentIronClawSchemasExcludeGatewayNamespacedTools`, `cmd/anvil-mcp/main_test.go`
   의 `TestToolRegistrationsExcludeGatewayTools`. gateway 의 namespaced tool 이 IronClaw
   `anvil_*` 목록/schema 에 절대 섞이지 않음을 고정.
2. **audit는 metadata-only** — `internal/mcpgateway/mcp_audit_privacy_test.go` 의
   `TestAuditRecordExcludesArgsAndResults`. sentinel 인자/결과가 `AuditRecord`
   직렬화 전체에 나타나지 않음을 검증.
3. **`/config/mcp*` 는 bearer 없으면 401** — `cmd/goose-daemon/mcp_boundary_anvil_test.go`
   의 `TestConfigMCPRoutesRequireAuthWhenConfigured`. production 배선(`authMiddleware`)
   그대로 검증.
4. **VM 은 URL만, policy 는 widen 불가** — 같은 파일의
   `TestMCPInjectionCarriesURLNotCredential`. 주입되는 유일한 값이 gateway URL alias
   뿐이고 secret sentinel 이 어디에도 없음을, 그리고 서버 allow-list 밖 profile 은
   빈 URL 을 받음을 검증.

**v0.6.1/v0.6.2/v0.6.4 하드닝 가드 (4종)**

5. **rate limit 키는 (VM, server) 2차원** — `internal/mcpgateway/ratelimit_anvil_test.go`
   의 `TestTokenBucketLimiter_KeyIsPerVMAndServer`(v0.6.1). 한 VM 의 소진이 다른 VM 의
   budget 을 침범하지 않음을 고정.
6. **anti-spoof 기본 on, opt-out만 가능** — `internal/network/antispoof_anvil_test.go`
   의 `TestAntiSpoofEnabledFromEnv_DefaultOnAndOptOut`(v0.6.1). falsey 값 외에는(오타
   포함) 항상 enabled 로 남는 fail-closed 기본값을 고정.
7. **resources/prompts 가 tools 와 rate bucket 공유** —
   `internal/mcpgateway/resources_prompts_ratelimit_anvil_test.go` 의
   `TestGateway_ResourcesAndPromptsShareRateLimit`(v0.6.2). resources/read,
   prompts/get 도 동일 limiter 를 소비해 우회 경로가 없음을 검증.
8. **`/config/mcp/servers` 는 args/credential 비노출** —
   `cmd/goose-daemon/mcp_servers_listing_anvil_test.go` 의
   `TestHandleConfigMCPServers_NeverLeaksArgsOrCredential`(v0.6.4). credential 과
   민감한 args 를 모두 가진 stdio 서버로 구동해도 원문 JSON 응답에 둘 다 없음을,
   `has_credential`/`command`/`transport` 는 정상 노출됨을 검증.
