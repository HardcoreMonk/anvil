# anvil 공개 릴리즈 경계

> **대상:** anvil downstream repository
> **현행화 기준:** 2026-07-08
> **목적:** anvil이 공개적으로 책임지는 기능 표면과, upstream ephemera에서 가져오더라도 anvil 정책상 수정하거나 제외해야 하는 표면을 구분한다.

---

## 1. 읽는 규칙

이 문서는 anvil의 공개 릴리즈 경계를 판단하는 기준이다. 문서가 충돌하면
`CONTEXT.md`가 우선하고, 이 문서는 README보다 먼저 공개 표면을 판단한다.

- `CONTEXT.md`: 프로젝트 경계, 변경 불가 계약, 진실 기준
- `docs/PUBLIC_RELEASE_BOUNDARY.md`: 공개 릴리즈 포함/제외 표면
- `docs/ADR_INDEX.md`: 장기 설계 결정의 현재 적용 상태
- `README.md`: 사용자 진입점과 현재 사용법
- `RELEASE_NOTES.md`: 릴리즈별 변경 기록

`ephemera` upstream 릴리즈 문서는 기반 runtime 분석으로 유지한다. ephemera
릴리즈 분석 문서의 제목이나 태그 이름을 anvil로 바꾸지 않는다.

---

## 2. 공개 포함 표면

현재 anvil 공개 표면은 다음 범위를 책임진다.

| 영역 | 공개 표면 | 구현/문서 위치 |
|---|---|---|
| MCP adapter | IronClaw가 호출하는 `anvil_*` stdio MCP tool | `cmd/anvil-mcp`, `internal/anvilmcp`, `docs/architecture/mcp-architecture.md` |
| VM lifecycle | VM 생성, task 실행(`POST /vms/{id}/tasks?stream=1` optional NDJSON streaming, 기본은 buffered `{"output","error"}` 계약), health, stop, delete | ephemera daemon API + anvil MCP wrapper |
| Snapshot lifecycle | full/diff snapshot 생성, 목록, restore, 삭제 | `internal/storage`, `anvil_create_snapshot` 계열 MCP tool |
| Runtime boundary | Firecracker MicroVM, TAP/IP, rootfs, guest agent proxy | `cmd/goose-daemon`, `internal/vm`, `internal/network`, `internal/storage` |
| Egress SNI filter | `profile` egress policy의 `configs/profiles/{profile}/egress.json`(`EPHEMERA_EGRESS_PROFILE_DIR`/`ANVIL_EGRESS_PROFILE_DIR` 고정 계약) 스키마에 신규 `allow_sni []string` 필드(exact + leading-label `*.` wildcard, `allow_cidrs`/`dns_servers`와 병렬 additive, 하위호환)를 추가한다. :443 새 흐름의 ClientHello가 `iptables -j NFQUEUE --queue-num 88`(env `ANVIL_SNI_QUEUE_NUM` override)로 goose-daemon in-process verdict 루프(`github.com/florianl/go-nfqueue/v2` — 유일한 신규 direct 의존)에 dispatch되어 SNI를 파싱·매칭한다. 허용 흐름은 conntrack mark `0x534e49`를 찍고 커널 fast-path ACCEPT, 비허용/파싱불가는 fail-closed DROP(+best-effort RST) — `--queue-bypass`(fail-open)는 명시 배제한다. **NFQUEUE baseline 요구**: `profile` egress policy를 지원하는 모든 host는 verdict 루프 바인딩이 가능해야 하며, 불가능한 host는 `allow_sni` profile의 VM spawn을 preflight로 거부한다(규칙만 깔리고 검사기가 없는 상태를 원천 차단). CIDR allow는 SNI dispatch보다 head-insert 순서상 위에 있어 :443 CIDR 매치가 SNI 검사보다 우선한다. `allow_hosts`(packet substring match)는 **legacy/deprecated**로 표기되며 유지된다(제거 시점은 별도 결정). 위협 모델·잔여 위험(ECH/spoofing/fronting/pre-decision 부분전달)은 ADR-0002 계약. **UDP:443(QUIC/HTTP3) 확장(2026-07-14)**: 같은 queue 88·같은 connmark를 UDP:443에도 적용한다 — `allow_sni` profile은 TCP `-sni-nfqueue`/`-sni-fastpath`와 대칭으로 `iptables -p udp --dport 443 ... --comment <prefix>-sni-udp-nfqueue`/`-sni-udp-fastpath` 규칙도 head-insert한다. verdict 루프가 QUIC Initial을 자체 구현 crypto(`internal/network/quic`, HKDF+AES-128-GCM+header protection, 신규 direct 의존 `golang.org/x/crypto` 하나)로 복호해 CRYPTO 프레임에서 TLS ClientHello를 얻어 같은 `allow_sni` 매처와 대조한다. QUICv1(`0x00000001`)+QUICv2(`0x6b3343cf`) 지원, 미지원 버전은 fail-closed deny. post-quantum(X25519MLKEM768) ClientHello가 Initial 데이터그램 2개에 걸치는 경우를 flow별 bounded-LRU reassembler로 재조립하며, 미완결 데이터그램은 drop(fail-closed)하되 CRYPTO는 누적한다(connmark가 conntrack에 first-accepted 패킷으로 confirm되게 하기 위함 — 미완결 passthrough-accept는 mark 0 confirm으로 fast-path를 깨뜨린다). UDP엔 RST가 없어 deny는 silent DROP이며, QUIC 타임아웃 후 브라우저가 TCP/HTTP2로 fallback하면 TCP:443 SNI 필터가 이어받는다. 3개 이상 데이터그램에 걸치는 ClientHello는 v1 미지원(fail-closed deny) | `cmd/goose-daemon/egress_policy.go`, `cmd/goose-daemon/sni_verdict.go`, `internal/network/sni`, `internal/network/quic`, [`docs/adr/0002-egress-sni-transparent-filter.md`](adr/0002-egress-sni-transparent-filter.md) |
| Runtime observability | `/metrics`, `/metrics/vms`, `/vms/{vm_id}/stats`, `GET /watchdog/status`(read-only count/ID/config), structured daemon logs | upstream ephemera runtime namespace + anvil 운영 문서 |
| Operator Web console | daemon이 `/ui/`로 serve하는 embedded Svelte SPA(EN/KO). `/ui/`(정적 bundle + login)만 auth 밖, VM list/create/detail/stats/settings/delete·multi-turn session data API는 bearer 뒤. runtime/operator surface이며 IronClaw MCP surface가 아니다 | `cmd/goose-daemon/uidist/`, `cmd/goose-daemon/config_api.go`, `docs/architecture/service-logic.md` |
| Profile config surface | `/config/profiles`(provider/model), `/config/providers`(key 존재 여부만), `/config/presets`(sizing preset), `/config/clients`(이름+만료만), profile `system.md` 편집(`64 KiB` cap). `goose-secrets.yaml` 값은 read/write·노출하지 않음 | `cmd/goose-daemon/config_api.go`(profile CRUD·`system.md`·`/ui/`), `providers.go`, `presets.go`, `clients.go` |
| Runtime MCP Gateway | daemon이 VM 내부 agent에 backend MCP server를 policy·rate-limit·audit로 중개(`EPHEMERA_MCP_*`, `internal/mcpgateway`, `configs/mcp/*`, Web UI MCP console). caller profile은 source IP↔VM registry로 판정(unknown → `403`), backend credential은 host-side only(VM엔 gateway URL만), `audit/mcp.jsonl` metadata-only, `/config/mcp*`는 bearer 뒤. **runtime/operator surface이며 IronClaw MCP surface가 아니다 — `cmd/anvil-mcp` IronClaw adapter를 대체하지 않는다** | `internal/mcpgateway`, `cmd/goose-daemon`, `configs/mcp/`, `docs/architecture/mcp-architecture.md` |
| Operator installer | end-user installer(`install.sh`/`uninstall.sh`/`INSTALL.md`/`ephemera.service.in`)와 release build(`scripts/build_release.sh`, 다운로드 kernel/firecracker를 `main.go` pin과 `sha256sum -c` 검증). runtime/operator installer surface. systemd service는 canonical `ephemera` 이름을 유지한다(rule-permitted, anvil alias wrapper 없음). 외부 노출은 reverse proxy/TLS 또는 private network 뒤에서만 | `install.sh`, `uninstall.sh`, `ephemera.service.in`, `scripts/build_release.sh`, `INSTALL.md` |
| Conversation transcript | daemon proxy `GET /vms/{id}/sessions/{name}/transcript`(bearer)로 대화 transcript 복원. agent export는 read-only(`goose session export`, model call 없음), 응답 schema `{turns:[{role,text}]}`는 auth-free여서 Web UI가 daemon token 없이 렌더한다. payload는 provider key/CP token/`agent_token` sentinel-free(guard) | `cmd/goose-daemon/api.go`, `cmd/goose-agent/main.go` |
| Workload automation | script-only `POST /vms/{vm_id}/workloads/run` 계약 | `cmd/goose-agent`, `cmd/goose-daemon`, `scripts/vm-workload-e2e.sh` |
| Goosetown in-VM helpers | `gtwall`, `gtcall`, `webdev_demo.sh` operator demo | upstream ephemera runtime namespace + anvil 보안 경계 문서 |
| Cross-host shared Town Wall | routed flock의 여러 host member가 하나의 공유 Town Wall에 post/observe. home-host hub(`roles[0]` 배치 호스트가 canonical `TownWall` 소유) + member daemon relay(`/flocks/{id}/post`·`/wall`·`/wall/history`·SSE를 home으로 forward/proxy) 토폴로지. guest는 여전히 로컬 daemon(`10.0.1.1`)에만 post(bridge-only 불변). runtime/operator surface이며 IronClaw 표면으로 새 `anvil_*` MCP tool을 추가하지 않는다(`TestIronClawSchemasExcludeCrossHostWallTools` guard). **전제:** member↔home daemon은 control-plane 포트로 상호 도달 가능한 신뢰(private) 네트워크 위에 있어야 한다. 외부 노출은 기존 reverse-proxy/TLS 정책 뒤에서만. cross-host broadcast fan-out은 이 표면 범위 밖(비목표) — cross-host `gtcall`은 별도 표면(아래 행)으로 편입. daemon-to-daemon hop은 dial-실패 한정 동기 bounded retry(총 3시도, 1s/2s)로 순단을 흡수한다(전달 semantics 불변). **home SPOF는 재선출 failover로 해소한다**(wall 과거 기록 손실을 수용하는 계약 — 아래 "Home 재선출 failover" 행) | `internal/anvilmcp/routed_flock.go`, `cmd/goose-daemon/orchestrator_api.go`, `internal/orchestrator/townwall.go`, `docs/superpowers/specs/2026-07-06-cross-host-shared-townwall-design.md` |
| Cross-host gtcall | routed flock의 임의 member가 다른 임의 member를 daemon `POST /flocks/{id}/call`(`{agent_id, prompt}`)로 호출. member→home→target 2-hop(home이 canonical roster로 agent→host,vm_id를 해석), guest는 여전히 로컬 daemon에만 접촉(bridge-only 불변). **토큰 모델(A안)**: `relay_token`은 guest 능력 토큰으로 그 flock의 wall sub-path(`post\|wall\|wall/history`) **와 `call` 진입**을 모두 admit한다(guest가 로컬 daemon에서 gtwall/gtcall 모두 이 토큰으로 인증 — 단일 host flock의 기존 guest CP token→gtcall 개방과 동형). 별도 `call_token`은 daemon-to-daemon call hop 전용(member→home, home→target)으로 **오직 `call` 경로만** admit하고 **wall 경로는 거부**하며(`call_token`→wall 방향 배타 유지·테스트 고정) control-plane bearer로 승격하지 않는다. 두 토큰 모두 `PlacementStore`에 영속되고 `State()` 등 모든 직렬화 표면에서 redact된다. hop 요청은 `X-Ephemera-Call-Hop`으로 표식해 어느 daemon도 재forward하지 않는다(kind와 무관하게 로컬 해석 전용). member→home 구간은 unmarked(home은 해석자이지 종단이 아니며 자신의 2번째 hop을 위해 표식 없이 받아야 한다), home→target 종단 hop만 marked다(2026-07-08 최종 리뷰 C1 수정 — 이전에는 member→home 구간에도 무조건 표식을 붙여 home의 2번째 hop이 성립하지 않았다). `X-Ephemera-Task-Depth`를 전 hop에 전파해 `EPHEMERA_MAX_TASK_DEPTH`가 cross-host에서도 성립한다. 대상 VM의 `agent_token`은 target host daemon 로컬에서만 주입(wire는 `{agent_id, prompt}` + depth/hop 헤더뿐). 기존 2-step `GET /flocks/{id}` + `POST /vms/{vm_id}/tasks` 계약은 하위호환으로 유지. runtime/operator surface이며 새 `anvil_*` MCP tool을 추가하지 않는다. home SPOF는 wall과 동일하게 재선출 failover로 해소한다(아래 행). cross-host broadcast fan-out은 계속 비목표. daemon-to-daemon hop은 dial-실패 한정 동기 bounded retry(총 3시도, 1s/2s)로 순단을 흡수한다(전달 semantics 불변) | `cmd/goose-daemon/orchestrator_api.go`(callFlockAgent/forwardFlockCall/dispatchFlockCall), `cmd/goose-daemon/api.go`(authMiddleware relay-token/call-token admission), `internal/anvilmcp/routed_flock.go`, `internal/anvilmcp/placement_store.go`, `docs/superpowers/specs/2026-07-07-cross-host-gtcall-design.md` |
| Home 재선출 failover | routed flock의 home host(hub) SPOF를 hub 복제(mesh) 없이 재선출로 해소한다. adapter reconcile 루프가 flock 단위로 연속 `homeFailureThreshold`회(상수, 기본 3) dial-계열 home 실패를 관측하면, `record.Agents` 순서상 첫 생존 host(구 home 제외)로 결정적으로 재선출한다. 전환은 `HomeHost` 영속(원자적 전환점) → 새 home hub 승격 등록 → 구 home 포함 전 member relay 재등록 → 구 home best-effort `DELETE` 순서다. **wall 과거 기록 손실을 명시 계약으로 수용**한다(새 home은 빈 log에서 seq 재시작, 구 home 디스크의 기록은 병합되지 않음). relay/call token은 flock 단위로 불변 재사용되어 guest는 무중단·무개입이다. **자동 fail-back 없음**(구 home 부활 시 수동 개입 없이는 relay로만 heal). 전환 창(최대 ~`threshold`×`ANVIL_MCP_RECONCILE_INTERVAL`, 기본 설정 기준 ~3분) 동안 wall/call은 기존과 동일하게 502 + bounded retry로 관측된다. **kind 전환 전제**: daemon `POST /flocks/{id}/distributed`(relay→hub 승격)와 `POST /flocks/{id}/relay`(hub→relay 강등) 두 endpoint는 CP bearer 전용이며 local flock kind는 양쪽 모두 `409`로 보호된다(guest/relay/call token은 이 승격·강등 endpoint에 admit되지 않음). runtime/operator surface이며 새 `anvil_*` MCP tool을 추가하지 않는다. KVM e2e `scripts/anvil-cross-host-failover-e2e.sh`로 검증됐다. 실 2-daemon 수동 검증(§6b)은 2026-07-11 수행 완료 — 전 세부 단계 PASS(전환 창 실측 ~27s @`reconcile 10s`; 기록: `docs/operations/2026-07-11-6b-failover-verification-run.md`) | `internal/anvilmcp/home_failover.go`, `internal/anvilmcp/runtime_router.go`, `cmd/goose-daemon/orchestrator_api.go`, `docs/superpowers/specs/2026-07-08-home-failover-design.md` |
| Cross-host snapshot replication | 스냅샷을 다른 host로 복제해 host 단일 장애점을 줄인다. **수동 경로**(baseline, 2026-06-02): `anvil_replicate_snapshot` MCP tool이 명령형 escape hatch로 source→target export/import 스트림을 한 번 연결한다(현행 유지, 상태는 자동 경로와 `SnapshotLocations`를 공유). **자동 경로**(이 slice): adapter reconcile 루프가 스냅샷마다 desired replica factor(**상수 N=2**, 원본+복제 1)로 **best-effort eventual 수렴**한다 — 스냅샷 생성이 복제본 완료를 기다리지 않으며 **무손실 보장이 아니다**. dial 실패는 연속 3회(상수 cap) 뒤 giving-up, 대상 복귀 시 재시도 재개. D3 coarse-fs 거부/tenant 불일치는 terminal(비재시도, 프로세스 수명 한정 — 재시작 시 re-arm). 복제본 GC/전파는 비목표(스냅샷 삭제가 다른 replica에 전파되지 않음). **새 metric 표면**: scheduler `/metrics`에 `anvil_scheduler_snapshot_replication_*`(attempts_total, latency_seconds, queue_depth, giving_up, last_success/last_failure timestamp)를 노출한다 — label은 저-cardinality `outcome`/`reason`/`phase`뿐이며 host 주소·토큰·snapshot id는 어떤 label/log에도 없다(공개 안전). anvil 경계는 metric 노출 + runbook 권장 alert 식 문서화까지이며 in-adapter alerting/notification은 없다(실 alerting은 zone 대시보드 몫). runtime/operator surface이며 새 `anvil_*` MCP tool을 추가하지 않는다. 유닛(`-race`) + KVM e2e `scripts/anvil-snapshot-replication-e2e.sh`(대상 down→giving-up→복귀→자동 복제→metrics 전이, 2연속 green)로 검증됐다 | `internal/anvilmcp/snapshot_replication.go`, `internal/anvilmcp/runtime_router.go`, `internal/anvilmcp/placement_store.go`, `internal/anvilmcp/scheduler_metrics.go`, `docs/superpowers/specs/2026-07-11-snapshot-replication-automation-design.md` |
| Token policy | daemon token과 guest `agent_token` 분리, MCP output token redaction | `CONTEXT.md`, `README.md`, MCP adapter |
| IronClaw integration | IronClaw 전용 실행 layer 계약 | `CONTEXT.md`, `README.md` |
| 문서 계약 | 한국어 운영 문서, 실제 API/env/file 이름 보존 | `AGENTS.md`, `CONTEXT.md` |

공개 표면에 들어간 기능은 README, RELEASE_NOTES, 관련 architecture 문서 중
영향을 받는 문서와 함께 갱신해야 한다.

---

## 3. 조건부 포함 표면

다음 표면은 runtime에는 존재할 수 있지만, anvil product 공개 표면으로 승격할 때
별도 검토가 필요하다.

| 영역 | 조건 |
|---|---|
| upstream ephemera 새 API | token 노출, lifecycle 의미, cleanup 계약이 anvil 정책과 충돌하지 않아야 한다. |
| Goosetown/flock 기능 | IronClaw MCP tool surface로 승격할 때 `agent_token` 노출 없이 Town Wall/API 경유 방식으로 노출해야 한다. |
| multi-host/scheduler/quota | runtime 안정성, 보안 경계, 운영 문서, full KVM E2E 기준이 먼저 정리되어야 한다. |
| audit/metrics/job store | public API 계약과 retention/보안 정책이 함께 정의되어야 한다. |
| replay/player 산출물 | 검증 근거와 운영 URL을 문서화한 경우에만 공개 문서 표면으로 취급한다. |
| upstream runtime namespace | `EPHEMERA_*`, `goose-*`, `ephemera_*` 이름은 runtime 호환성으로 유지한다. anvil product 표면으로 rename하려면 별도 ADR이 필요하다. |
| upstream ephemera low-level API | anvil MCP tool로 노출하려면 token redaction, tenant/egress, cleanup, audit 의미를 먼저 정의한다. |

조건부 표면을 공개 표면으로 올릴 때는 ADR을 추가하거나 기존 ADR 적용 상태를
갱신한다.

---

## 4. 공개 제외 표면

다음은 현재 anvil 공개 릴리즈 범위가 아니다.

- OpenClaw compatibility layer, shared gateway, shared runtime contract
- IronClaw가 ephemera low-level HTTP API를 직접 다루는 결합 구조
- `POST /vms` 응답 외부의 `agent_token` 노출
- upstream ephemera의 flock `agent_tokens` 응답을 anvil public API로 그대로 노출
- flock broadcast(`POST /flocks/{id}/broadcast`, `ephemera-ctl flock broadcast`)를
  `anvil_*` MCP tool로 노출하는 것. broadcast는 daemon-only runtime operator 표면으로만
  두며, MCP tool 노출은 2026-07-11 기각 확정이다(로컬 host scope 전용이라 routed flock
  원격 멤버 미도달·audit 1:1 불변식·adapter rate limit 부재가 근거;
  `TestToolRegistrationsExcludeBroadcast`,
  `TestCurrentIronClawSchemasExcludeBroadcastTool`로 고정).
- 실제 구현 계약 없이 `EPHEMERA_*`, `goose-*` API/env/path를 anvil 이름으로
  일괄 rename하는 변경
- purecvisor의 libvirt/QEMU, LXC, ZFS zvol clone, multi-node HA, live migration,
  cluster evacuation, OVN multi-node automation
- 공개 검증 없이 runtime 산출물, local secret, profile별 secret을 릴리즈 표면에
  포함하는 변경

제외 표면은 코드에 존재하거나 upstream에 존재해도 anvil 공개 기능으로 설명하지
않는다.

---

## 5. upstream ephemera 변경 채택 분류

upstream ephemera 변경을 병합할 때는 다음 상태 중 하나로 분류한다.

| 상태 | 의미 | 예시 |
|---|---|---|
| `adopted` | anvil 정책과 충돌하지 않아 그대로 채택 | watchdog, metadata persistence, stale artifact rebuild |
| `adapted` | runtime 가치는 채택하되 public/API 보안 표면은 수정 | restore 응답 token redaction, env alias 병행 |
| `excluded` | anvil 공개 경계와 충돌해 제외 | flock `agent_tokens` public response |
| `deferred` | 가치가 있으나 현재 release scope 밖 | multi-host scheduler, persistent audit store, quota |
| `historical` | 과거 분석 근거로만 유지 | ephemera 0.1.0/0.2.0 분석 문서 |

`adapted`, `excluded`, `deferred` 판단은 README나 RELEASE_NOTES만으로 처리하지
말고 ADR 또는 ADR_INDEX에 남긴다.

현재 anvil main runtime baseline의 upstream runtime 채택 상태:

| upstream tag | 주요 변경 | 현재 anvil 분류 |
|---|---|---|
| `v0.3.2` | live VM cold-restart, `vms/<vm_id>/state.json`, same-identity recovery | `adapted` — runtime recovery는 채택, token persistence는 보안/문서 redaction 정책으로 관리 |
| `v0.3.3` | watchdog dead persistence, per-agent restart, in-VM CP token auto-injection, real-LLM e2e | `adapted` — daemon 기능은 채택, MCP/public output token 노출은 금지 |
| `v0.3.4` | `EPHEMERA_API_TOKENS_FILE`, SIGHUP CP-token vsock fan-out, watchdog tunables/auto-heal, Firecracker SIGHUP forwarding hot-fix | `adapted` — hot rotation은 채택, file permission/VM version caveat를 운영 문서에 명시 |
| `v0.3.5` | Prometheus `/metrics`, `/vms/{vm_id}/stats`, `log/slog`, observability demo | `adapted` — runtime observability는 채택, `ephemera_*` metric namespace와 `/metrics` auth 정책을 compatibility로 설명 |
| `v0.3.6` | autonomous webdev demo, in-VM `gtcall`, multi-line-safe `gtwall`, Goose JSON output parsing | `adapted` — demo/helper는 runtime operator 표면으로 채택, peer `agent_token`은 계속 control-plane proxy가 주입하며 직접 노출하지 않음 |
| `v0.4.0` | memory auto-snapshot, diff/COW rootfs, spawn-path cold-restart | `adapted` — storage/recovery 채택, `EPHEMERA_AUTOSNAPSHOT=true`는 opt-in·disk-expensive로 두고 public support로 승격하지 않음 |
| `v0.4.1` | client identity, `GET /audit`, per-token TTL, `ephemera-ctl` | `adapted` — daemon access audit/CLI 채택, `ephemera-ctl`은 runtime operator CLI(IronClaw MCP 대체 아님) |
| `v0.4.2` | COW spawn probe/fallback, COW+Diff snapshot | `adapted`, default plain 확정(**D4 종결, upstream-tracked**) — `EPHEMERA_DISK_MODE=cow`는 명시적 opt-in, default plain 무기한 유지. burn-in에서 D4(diff-restore guest GPF)가 부하 하 재발, 4라운드 조사로 anvil-측 소진·근본은 anvil 밖 KVM/fc resume-race로 확정 → 2026-07-13 종결(fc v1.16.1이 실패율 100%→~15–25%로 최대 완화). flip 재개는 upstream 해소 시에만(상세 `docs/ADR_INDEX.md` v0.4.2 행 + `operations/2026-07-11-cow-burnin-run.md` round 1~4 + `operations/2026-07-13-d4-firecracker-upstream-report.md`) |
| `v0.4.3` | dynamic flock membership, pause/resume, `max_agents`, Town Wall filter/rotation | `adapted` — single-host flock lifecycle 채택, routed cross-host flock에는 미적용 |
| `v0.4.4` | streaming `/tasks`, depth guard, `/watchdog/status`, flock broadcast, slog | `adapted`, broadcast MCP exposure 기각 확정 — streaming은 buffered 기본 계약 유지, `GET /watchdog/status`는 read-only 공개 표면, flock broadcast는 daemon-only(MCP tool 노출 2026-07-11 기각 확정) |
| `v0.4.5` | snapshot-restore auto-recovery | `adapted` — restore state persist + token redaction 유지. live·persisted restored VM이 참조하는 source snapshot `DELETE`는 `409`로 보호(upstream e2e 46c의 `200` orphan과 의도적 divergent) |
| `v0.5.0` | operator Web UI `/ui/`, `/config/profiles`, multi-turn session, graceful delete | `adapted` — Web UI/`/config/*`는 runtime/operator 표면으로 채택(IronClaw MCP surface 아님). `/ui/`(정적 bundle + login)만 auth 밖, data API는 bearer 뒤(guard). `/config/profiles`는 `goose-secrets.yaml` 비노출(sentinel). `cmd/anvil-mcp` 불변, `VMInfo` provider/model additive |
| `v0.5.1`-`v0.5.5` | `/config/providers`·`/config/clients`, `system.md` 편집, profile guard, sizing preset + per-VM `VcpuCount`/`MemSizeMib`, `SystemAuthor`, restore wait 60s | `adapted` — provider/client surface는 secret 비노출(sentinel), default sizing `1` vCPU/`1024` MiB 채택(이전 2/2048). keep-alive divergence(`64ec57c`)는 ADR_INDEX/upstream-sync-policy 참조 |
| `v0.6.0` | runtime MCP Gateway(`internal/mcpgateway`, `/config/mcp*`, `configs/mcp/*`, Web UI MCP console) | `adapted` — **runtime/operator 표면, IronClaw MCP surface 아님, `cmd/anvil-mcp` 대체 아님.** source IP로 caller profile 판정(unknown `403`), backend credential host-side only(VM엔 URL만 주입), `audit/mcp.jsonl` metadata-only, profile policy는 `servers.yaml`을 좁히기만 함. IronClaw schema/adapter가 gateway tool 제외(guard) |
| `v0.6.1`, `v0.6.2`, `v0.6.4` | anti-spoof, per-(VM,server) rate limit, resources/prompts policy·rate 공유, stdio backends | `adapted` — `EPHEMERA_NET_ANTISPOOF` 기본 on, `EPHEMERA_MCP_RATE` 기본 `0`=unlimited, `GET /config/mcp/servers`는 transport/command + `has_credential`만(leak guard), stdio child env 재구성 + `credential_env` + `nobody`/scratch + process-group reap. upstream에 `v0.6.3` 없음 |
| `v0.7.0` | end-user installer, release workflow, conversation transcript restore, upstream hardening reconcile | `adapted` — installer는 runtime/operator surface이고 systemd는 canonical `ephemera`(alias wrapper 없음). transcript는 daemon proxy(bearer), agent export read-only(model call 없음), payload sentinel-free(guard). 사전 backport 3종이 reconcile에서 single definition으로 남고(anvil stricter) 기존 anvil adaptation rollback 없음 |

2026-07-06 기준 upstream `main`과 최신 upstream tag는 `v0.7.0`이다. `v0.4.0`-`v0.7.0`은
anvil main runtime baseline으로 병합·적응되어 full KVM gate로 검증됐으며(위 표), upstream
parity scope 코드 편입이 완료됐다. anvil runtime/operator baseline supports upstream
ephemera v0.7.0 with anvil adaptations for token redaction, tenant/egress, scheduler,
audit, and IronClaw MCP surface separation. 전 태그별 채택/적응/deferred/excluded 분류는
[`docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`](analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md)의
parity matrix에 있다. 현재 관찰 범위(`v0.7.0`까지)에는 조건부/제외로 남은 upstream
tag가 없다. `v0.7.0` 이후 새 upstream tag가 관찰되면 별도 adoption review 뒤 공개
경계를 분류한다.

`v0.3.2`/`v0.3.3` 병합 전 검토 근거는
`docs/analysis/08-v0.3.2-v0.3.3-upstream-change-review.md`에 historical analysis로
보존한다.

## 6. 이름/브랜드 경계

anvil은 ephemera를 이름만 바꾼 프로젝트가 아니다. 공개 문서에서는 다음 규칙을
유지한다.

- `anvil`: IronClaw MCP 실행 계층, scheduler, tenant/egress governance, workload
  automation, product release 경계.
- `ephemera`: Firecracker MicroVM runtime baseline, daemon HTTP API, guest agent,
  upstream runtime release tag.
- `EPHEMERA_*`, `goose-*`, `ephemera_*`는 runtime compatibility namespace다.
  호환성 때문에 보존하며 anvil product identity의 약화로 해석하지 않는다.
- anvil release note는 "upstream runtime baseline"과 "anvil product changes"를
  분리해 작성한다.

---

## 7. 릴리즈 전 경계 확인

공개 릴리즈 또는 push 전에는 변경 성격에 맞게 다음을 확인한다.

- 공개 기능이 늘었으면 이 문서의 포함/조건부/제외 표면을 갱신한다.
- 장기 설계 결정이면 `docs/ADR_INDEX.md`와 `docs/adr/*.md`를 갱신한다.
- upstream ephemera 변경을 병합했으면 채택 상태를 명시한다.
- `agent_token` 노출 경로가 `POST /vms` 응답 외부로 늘지 않았는지 확인한다.
- runtime lifecycle 변경이면 full KVM E2E 또는 그에 준하는 실환경 검증 근거를
  남긴다.
- 로컬 secret, runtime artifact, snapshot, profile secret은 커밋하지 않는다.
