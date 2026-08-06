# ADR 적용 상태 인덱스

> **대상:** anvil downstream repository
> **현행화 기준:** 2026-08-06
> **목적:** anvil에서 장기 유지할 설계 결정과 upstream ephemera 변경 채택 상태를 추적한다.

---

## 1. 읽는 규칙

`docs/adr/`의 ADR은 설계 결정 원문이다. 이 인덱스는 각 ADR이 현재 anvil에
어떻게 적용되는지를 요약한다.

문서가 충돌하면 다음 순서를 따른다.

1. `CONTEXT.md`
2. `docs/PUBLIC_RELEASE_BOUNDARY.md`
3. `docs/ADR_INDEX.md`
4. 개별 ADR 원문
5. `README.md`
6. `RELEASE_NOTES.md`

개별 ADR 원문과 공개 경계가 충돌하면 공개 경계를 우선한다. ADR을 바꾸지 않고
역사 기록으로 보존해야 할 때는 이 인덱스에서 상태를 `superseded` 또는
`historical`로 바꾼다.

---

## 2. 상태 값

| 상태 | 의미 |
|---|---|
| `accepted` | 현재 anvil에 적용되는 결정 |
| `adapted` | 원칙은 유지하지만 anvil 정책에 맞게 수정 적용 |
| `superseded` | 더 최신 ADR 또는 공개 경계 문서가 대체 |
| `historical` | 과거 판단 근거로만 보존 |
| `rejected` | 검토했지만 현재 anvil 경계와 맞지 않아 채택하지 않음 |

---

## 3. 현재 적용 상태

| ADR | 상태 | 적용 요약 |
|---|---|---|
| [ADR-0001](adr/0001-anvil-public-boundary-and-upstream-adoption.md) | accepted | `PUBLIC_RELEASE_BOUNDARY.md`와 ADR_INDEX를 도입하고, upstream ephemera 변경을 `adopted/adapted/excluded/deferred/historical`로 분류한다. |
| [ADR-0002](adr/0002-egress-sni-transparent-filter.md) | accepted | `profile` egress의 도메인 통제를 substring `-m string`(coarse, fragmentation/오탐 취약)에서 실제 파싱된 TLS ClientHello SNI로 전환한다. 신규 `allow_sni []string` 필드(기존 `allow_hosts` 재해석 아님, 하위호환 additive)를 :443 새 흐름에 한해 강제한다 — `iptables -j NFQUEUE --queue-num 88`(env `ANVIL_SNI_QUEUE_NUM`)로 dispatch, goose-daemon in-process verdict 루프(`github.com/florianl/go-nfqueue/v2`, 이 slice의 유일한 신규 direct 의존)가 SNI를 파싱해 `allow_sni` 매처와 대조한다. 허용 SNI는 conntrack mark(`0x534e49`)를 찍고 이후 패킷은 커널 fast-path ACCEPT, 비허용/파싱불가는 fail-closed DROP(+best-effort RST). `--queue-bypass`(fail-open)는 명시 배제하고, verdict 루프가 `Ready()`가 아니면 `allow_sni` profile의 spawn 자체를 preflight 거부한다(규칙만 깔리고 검사기가 없는 상태를 원천 차단). **위협 모델**: 신뢰 golden-image 워크로드의 의도된 egress 강제·감사가 in-scope, 적대적 in-guest 루트의 spoof/fronting/ECH 완전봉쇄는 out-of-scope(잔여 위험으로 계약 — ECH는 outer(공개) SNI만 관측 — outer가 allowlisted면 flow 허용+암호화 inner 은닉(guest-asserted SNI와 동일 신뢰등급 잔여), outer가 없거나 비허용이면 fail-closed deny, QUIC/UDP:443도 자체 복호(HKDF+AES-128-GCM+header protection) SNI 필터로 동일 `allow_sni` 매처를 적용하며 미지원 버전/복호실패는 fail-closed DROP(non-TLS UDP는 여전히 SNI 층 밖 — CIDR/base REJECT만), SNI는 guest-asserted라 CIDR 핀 없이는 스푸핑 가능, domain fronting은 TLS 종단 없이 미탐지, 멀티세그먼트 ClientHello의 미완결 세그먼트는 판정 전 unmarked로 통과하되 승인 mark는 완결된 positive match에서만 찍힘). **additive 계약**: CIDR allow 규칙이 head-insert 순서상 SNI dispatch보다 위에 앉아 :443 CIDR 매치가 SNI 검사보다 우선한다(명시 IP 신뢰가 도메인 검사를 우회). `allow_hosts`(substring)는 deprecation cycle 확정(OQ8, 2026-07-18): release N(지금)은 profile 로드 시 런타임 deprecation 경고를 남기고(`loadEgressProfile`), release N+1(다음 tagged anvil 릴리즈)에서 필드·apply·validate·cleanup·test를 제거하고 `egressProfile` unmarshal에 `DisallowUnknownFields`로 잔존 `allow_hosts` profile을 loud fail-closed 거부한다(마이그레이션: 도메인 → `allow_sni`, IP → `allow_cidrs`). **ECH-on-allowed-flow는 이제 관측 가능(2026-07-18)**: 파서가 ECH 확장(`0xfe0d`)을 best-effort 탐지해 allowlisted outer SNI로 허용된 flow가 ECH를 담으면 `ephemera_egress_sni_ech_observed_total{proto}` metric + content-free `slog.Info`를 방출한다(관측 전용, verdict/deny 경로 불변). 유닛(`internal/network/sni` 파서/매처, `cmd/goose-daemon` decide/preflight/rollback/recovery/audit/metric) + KVM e2e `scripts/anvil-egress-sni-e2e.sh`(허용 도달·비허용 차단(RST)·감사 레코드·fast-path metric delta, 독립 재실행 exit 0 확인)로 검증됐다. 상세: [ADR-0002 원문](adr/0002-egress-sni-transparent-filter.md), [design spec](superpowers/specs/2026-07-13-egress-sni-filter-design.md), [plan](superpowers/plans/2026-07-13-egress-sni-filter.md), [handoff](operations/2026-07-13-egress-sni-handoff.md). |
| [ADR-0003](adr/0003-per-flock-guest-capability-tokens.md) | accepted | flock member VM에 주입하는 자격증명을 **per-flock guest 능력 토큰으로 통일**한다. local flock도 routed flock과 같은 admission store(`cp.relayTokens`)를 쓰므로, guest 토큰은 그 flock의 wall sub-path(`post\|wall\|wall/history`)와 `call` 진입만 admit하고 어떤 control-plane 경로(`/vms`, `/config/*`, `/tenants`, `/snapshots`)도 열지 않는다 — `authMiddleware` 로직은 무변경이며 바뀐 것은 *무엇을 주입하는가*뿐이다. 토큰은 flock 디렉토리의 별도 파일 `guest-token`에 **호출 지점 명시 0600**(umask 의존 아님) + atomic tmp+rename으로 영속된다(`FlockMetadata`의 무조건 no-secret 불변식을 조건부로 만들지 않으려고 분리). `cp.relayTokens`가 in-memory 전용이고 `LoadFromDisk`가 `ControlPlane`을 참조하지 못하므로 **daemon 시작 경로에 재수화 단계**를 함께 낸다(없으면 재시작 후 첫 member spawn이 빈 토큰을 주입 — 구 모델 대비 회귀). member spawn 3개 지점(`spawnVMForFlock`·`restartAgent`·`changeFlockAgentRole`)이 `ControlPlaneTokenManaged=false`를 세워 SIGHUP 회전이 능력 토큰을 운영자 bearer로 덮어쓰지 못하게 한다. **무중단 업그레이드**: 이전에 spawn된 VM은 `CPTokenManaged=true`가 디스크에 남아 계속 회전을 받고 운영자 bearer도 계속 admit되므로, 회전 대상은 VM 교체와 함께 자연히 말라붙는다(마이그레이션·버전 게이트 없음). **명시된 거래**: 만료되는 넓은 자격증명 → **만료되지 않는** 좁은 것. flock에 TTL·reaper·GC가 없어 토큰 수명 = flock 수명이며 폐기 수단은 flock 삭제뿐이다. blast radius 축소(제어평면 전체 → 한 flock의 wall/call 서브패스)가 이 거래를 정당화한다. **기각 대안**: per-member 토큰 — 재수화 요구를 소멸시키는 것은 사실이나 admission이 동등 비교에서 집합 소속 검사로 바뀌고 `removeFlockAgent`에 per-member 폐기 경로가 필요해 폐기 표면이 member 수만큼 늘어난다. 상세: [ADR-0003 원문](adr/0003-per-flock-guest-capability-tokens.md). |
| [Cross-host shared Town Wall](superpowers/specs/2026-07-06-cross-host-shared-townwall-design.md) | accepted | home-host hub 토폴로지: `roles[0]` 배치 호스트가 canonical `TownWall`(hub flock)을 소유하고, 나머지 멤버 host daemon은 relay flock으로 `/flocks/{id}/post`·`/wall`·`/wall/history`·SSE를 home으로 forward/proxy한다. daemon-to-daemon hop은 flock-scoped `relay_token`으로 인증하며, `authMiddleware`는 이 token을 해당 flock의 wall sub-path(`/flocks/{id}/(post\|wall\|wall/history)`)에 admit한다(도입 당시 wall 전용 — cross-host gtcall 편입부터 guest 능력 토큰으로 `call` 진입도 admit, 아래 행) — 일반 control-plane bearer로 승격하지 않는다. `relay_token`은 `PlacementStore`에 영속되지만 모든 MCP output/audit/HTTP view에서 redact한다(`State()`가 nil 처리). **전제**: member↔home daemon은 control-plane 포트로 상호 도달 가능한 신뢰(private) 네트워크 위에 있어야 한다 — 외부 노출은 기존 reverse-proxy/TLS 정책 뒤에서만. **진화 경로**: 도입 당시 home 단일 장애점(SPOF)을 1차 수용했으나, 재선출 failover로 해소됐다(아래 "Home 재선출 failover" row — §6b 실 2-daemon 수동 검증까지 2026-07-11 PASS로 완료). daemon-to-daemon hop은 dial-실패 한정 동기 bounded retry(총 3시도, 1s/2s)로 순단을 흡수한다(전달 semantics 불변). cross-host broadcast fan-out은 비목표(cross-host `gtcall`은 별도 결정으로 편입, 아래 행). `TestIronClawSchemasExcludeCrossHostWallTools`가 새 `anvil_*` MCP tool 미노출을 고정한다. 상세: [design spec](superpowers/specs/2026-07-06-cross-host-shared-townwall-design.md), [handoff](operations/2026-07-06-cross-host-town-wall-handoff.md). 별도 `docs/adr/*.md` 원문은 없음 — 이 row는 design spec을 결정 원문으로 삼는다. |
| [Cross-host gtcall](superpowers/specs/2026-07-07-cross-host-gtcall-design.md) | accepted | routed flock의 임의 member가 다른 임의 member를 daemon `POST /flocks/{id}/call`(`{agent_id, prompt}`)로 호출한다. member→home→target 2-hop(home이 canonical roster로 agent→host,vm_id 해석), guest는 bridge-only 유지. **토큰 모델(A안, 2026-07-07 재확정)**: `relay_token`이 guest 능력 토큰으로 재해석돼 그 flock의 wall sub-path **와 `call` 진입**을 모두 admit한다(단일 host flock의 기존 guest CP token→gtcall 개방과 동형). 별도 `call_token`은 daemon-to-daemon call hop 전용(member→home, home→target)으로 **오직 `call` 경로만** admit하고 **wall 경로는 거부**한다(배타는 유지·테스트 고정, control-plane bearer 승격 금지). `call_token`은 `relay_token`과 나란히 `PlacementStore`에 영속되고 `State()` 등 모든 직렬화 표면에서 redact된다. hop 요청은 `X-Ephemera-Call-Hop`으로 loop guard(kind와 무관하게 로컬 해석 전용 — 2026-07-08 설계 보정)하되, member→home 구간은 unmarked(home은 해석자이지 종단이 아니며 자신의 2번째 hop을 위해 표식 없이 받아야 한다), home→target 종단 hop만 marked다(2026-07-08 최종 리뷰 C1 수정 — 이전에는 member→home 구간에도 무조건 표식을 붙여 home의 2번째 hop이 성립하지 않았다). `X-Ephemera-Task-Depth`를 전 hop 전파해 `EPHEMERA_MAX_TASK_DEPTH`가 cross-host에서도 성립한다. 이 slice가 wall slice의 잠재 결함(`registerRelayFlock`이 admit 등록을 하지 않아 auth-on member daemon에서 routed guest의 gtwall이 401되던 문제)을 동반 수정한다. 기존 2-step `GET /flocks/{id}` + `POST /vms/{vm_id}/tasks` 계약은 하위호환 유지, 신규 `anvil_*` MCP tool 없음. KVM e2e `scripts/anvil-cross-host-gtcall-e2e.sh`(real member VM + stub home, **auth-on** member daemon)로 검증됐다. home SPOF는 wall과 동일하게 재선출 failover로 해소(아래 row — §6b 실 2-daemon 수동 검증 2026-07-11 PASS), cross-host broadcast fan-out은 계속 비목표. daemon-to-daemon hop은 dial-실패 한정 동기 bounded retry(총 3시도, 1s/2s)로 순단을 흡수한다(전달 semantics 불변). 상세: [design spec](superpowers/specs/2026-07-07-cross-host-gtcall-design.md), [handoff](operations/2026-07-08-cross-host-gtcall-handoff.md). 별도 `docs/adr/*.md` 원문은 없음 — 이 row는 design spec을 결정 원문으로 삼는다. |
| [Home 재선출 failover](superpowers/specs/2026-07-08-home-failover-design.md) | accepted | routed flock의 home host(hub) SPOF를 hub 복제(mesh)가 아니라 **재선출 failover**로 해소한다 — 복제 일관성 문제(seq 단조성, 이중 쓰기, 복제 지연, 분산 합의)를 구조적으로 제거하는 대신 wall 과거 기록 손실을 명시 계약으로 수용한다. **감지**: adapter reconcile 루프가 flock 단위로 **연속 `homeFailureThreshold`회(상수, 기본 3)** dial-계열 home 실패를 관측하면 발화(성공 시 카운터 리셋, 임계는 설정화하지 않음 — YAGNI). **선출(결정적)**: 직전 reconcile에서 생존 관측된 host 중 `record.Agents` 순서상 첫 host, 구 home 제외 — 같은 입력은 같은 결론. 후보가 0이면 no-op(카운터는 포화 유지, 다음 주기 재평가). **전환**: `HomeHost` 영속이 원자적 전환점(이후 모든 재등록의 기준) → 새 home에 `RegisterDistributedFlock`(VMID/Addr roster + 토큰) → 구 home 포함 전 member에 `RegisterRelayFlock`(새 HomeAddr) → 구 home에 best-effort `DELETE`(도달 불가면 skip). 부분 실패는 다음 reconcile 주기가 idempotent 수렴. **kind 전환 결정(spec 보정)**: spec 원안은 기존 hub/relay 등록 배관 재사용을 가정했으나, D1 fix(PR #30)가 도입한 kind 충돌 `409` 가드 때문에 daemon 쪽에 kind 전환이 필수였다 — `POST /flocks/{id}/distributed`가 relay 점유 id 위에서 hub로 승격(`201`), `POST /flocks/{id}/relay`가 hub 점유 id 위에서 relay로 강등(`201`)한다. 두 endpoint 모두 **CP bearer 전용**(relay/call token은 admit 대상 아님), **local flock kind는 양쪽 모두 `409` 불변 보호**. 승격된 hub의 Agents map은 빈 map으로 시작한다(`deleteFlock`의 VM-safety 불변식 — Agents가 있으면 삭제 거부되므로 승격 hub도 비어 있어야 정합). 강등 시 구 `TOWN_WALL.log`는 디스크에 잔존(정리하지 않음). **wall 손실 명시 계약**: 새 home은 빈 log에서 seq를 재시작한다 — 이전 기록은 구 home 디스크에 남지만 새 wall로 병합되지 않는다(agent 관점에서 과거 메시지가 사라진 것으로 보임). **토큰 불변**: relay/call token은 flock 단위로 그대로 재사용 — guest 주입 토큰(`.ephemera-cp-token`)이 바뀌지 않으므로 guest는 무중단·무개입. **자동 fail-back 없음**: 구 home이 부활해도 새 home을 유지한다(flap 방지) — 부활한 구 home은 다음 reconcile에서 relay로 강등되어 heal된다. 전환 창은 최대 ~`threshold`(3)×`ANVIL_MCP_RECONCILE_INTERVAL`(기본 60s)+전환 시간(기본 설정 기준 ~3분) — 그 동안 wall/call은 기존대로 502 + bounded retry. 유닛 8종(`TestFailover_*`, `internal/anvilmcp/home_failover_test.go`)이 발화/no-op/카운터리셋/non-dial 제외/후보 0/부분 실패 수렴/구 home 부활/로그 redaction을 모두 고정한다. KVM e2e `scripts/anvil-cross-host-failover-e2e.sh`(stub A→stub B 재선출 + real daemon relay→hub 승격, wall 손실 계약 관측, redaction 검증)로 검증됐다. 실 2-daemon 수동 검증([수동 검증 절차 §6b](operations/2026-07-08-cross-host-manual-verification.md))은 2026-07-11 수행 완료 — 전 세부 단계 PASS(전환 창 실측 ~27s @`reconcile 10s`; 상세: [run](operations/2026-07-11-6b-failover-verification-run.md), [handoff](operations/2026-07-11-home-failover-handoff.md)). 상세: [design spec](superpowers/specs/2026-07-08-home-failover-design.md), [handoff](operations/2026-07-11-home-failover-handoff.md). 별도 `docs/adr/*.md` 원문은 없음 — 이 row는 design spec을 결정 원문으로 삼는다. |
| [Snapshot replication 자동화](superpowers/specs/2026-07-11-snapshot-replication-automation-design.md) | accepted | cross-host snapshot replication을 수동·동기(baseline, 2026-06-02)에서 **선언적 reconcile sweep**로 확장한다. adapter(`RuntimeRouter`)가 home failover와 대칭인 "desired state를 reconcile이 heal" 패턴을 그대로 재사용한다 — 큐 소유자를 별도 job queue/worker(비동기 relay buffer와 동일 사유로 기각, CONTEXT.md의 "비동기 relay buffer 기각" 항목(2026-07-11))가 아니라 adapter 자신으로 둔다. **desired replica factor = 상수 N=2**(원본+복제 1). **재시도 정책**: reconcile-idempotent(import는 이미 `already_present`로 idempotent) + in-memory (snapshot,target) dial 실패 카운터, **cap 3(상수)** 도달 시 giving-up 표시하고 대상 host 복귀 관측 시 카운터 리셋(failover의 "복귀 시 재평가" 규율과 동일). **terminal 분류**: D3 coarse-fs 거부·tenant/검증 실패는 non-dial 실패로 즉시 재시도 대상에서 제외되지만, 이 exclude는 in-memory(adapter `cmd/anvil-mcp` 프로세스 수명 한정)라 adapter 재시작이 re-arm한다 — 영속 차단 목록이 아니다. **replica factor·retry cap·dial-only 게이팅·terminal 분류 넷 다 설정화하지 않은 상수다**(`homeFailureThreshold` 방침, 위 "Home 재선출 failover" row와 동일 YAGNI 근거 — 실사용 요구 발생 시에만 설정 노출). **metric**: `anvil_scheduler_snapshot_replication_*`(attempts_total{outcome,reason}, latency_seconds{phase="total"만 — export/import sub-timing은 `ReplicateSnapshot` 재사용 원칙상 관측 불가}, queue_depth, giving_up, last_success/last_failure timestamp)가 flock placement metric과 동일하게 **`PlacementStore`에 영속**된다(파일 성장 유계인 카운터형). **alert 경계**: anvil은 metric 노출 + runbook 권장 alert 식(queue_depth 지속>0, giving_up>0, last_success staleness) 문서화까지만 책임지고, 실 alerting/notification은 두지 않는다(zone 대시보드가 scrape·발화). **비목표**: 복제 토폴로지 자동 재배치/복제본 GC(스냅샷 삭제 시 복제본 전파 삭제 없음 — `SnapshotLocations`는 add-only), unbounded async job queue/worker pool(기각 선례), 정책 설정화, in-adapter alerting, cross-tenant 복제. 유닛(fake daemon, `-race`) + KVM e2e `scripts/anvil-snapshot-replication-e2e.sh`(대상 down→giving-up→복귀→자동 복제→metrics 전이, 2연속 green) + 실 2-daemon(비-stub) 수동 검증(2026-07-11 PASS — host-b가 실 daemon으로 import bundle 수신·재서빙)으로 검증됐다. 상세: [design spec](superpowers/specs/2026-07-11-snapshot-replication-automation-design.md), [handoff](operations/2026-07-11-snapshot-replication-automation-handoff.md), [multihost run](operations/2026-07-11-replication-multihost-verification-run.md). 별도 `docs/adr/*.md` 원문은 없음 — 이 row는 design spec을 결정 원문으로 삼는다. |

---

## 4. upstream runtime baseline 채택 상태

다음 upstream tag는 anvil `main`에 병합되어 runtime baseline으로 사용된다. 분류는
`PUBLIC_RELEASE_BOUNDARY.md`의 공개 표면과 보안 경계를 기준으로 한다.

| upstream tag | 상태 | 적용 전 검토 요약 |
|---|---|---|
| `v0.3.2` | adapted | live VM cold-restart와 `vms/<vm_id>/state.json`은 채택한다. `agent_token` persistence는 host-local recovery state로 취급하고 MCP output, audit, metrics, replay fixture에는 노출하지 않는다. |
| `v0.3.3` | adapted | watchdog dead persistence, per-agent restart, in-VM CP token auto-injection은 채택한다. `/root/.ephemera-cp-token`은 secret file로 취급하고 standalone VM에는 주입하지 않는 경계를 유지한다. |
| `v0.3.4` | adapted | `EPHEMERA_API_TOKENS_FILE`, SIGHUP CP-token vsock fan-out, watchdog tunables/auto-heal, Firecracker SIGHUP forwarding hot-fix는 채택한다. true hot rotation은 token file과 v0.3.4+ guest agent가 있을 때만 보장한다. |
| `v0.3.5` | adapted | `/metrics`, `/vms/{vm_id}/stats`, `log/slog`, observability demo는 채택한다. `ephemera_*` metric namespace와 `EPHEMERA_*` env는 runtime compatibility namespace로 유지한다. |
| `v0.3.6` | adapted | autonomous webdev demo, in-VM `gtcall`, multi-line-safe `gtwall`, Goose JSON output parsing은 채택한다. `gtcall`은 peer `agent_token`을 VM 내부에 노출하지 않고 control-plane proxy token injection 경계를 유지한다. |
| `v0.4.0` | adapted, auto-snapshot env-only 확정 | storage/recovery core(memory auto-snapshot, diff/COW rootfs, spawn-path cold-restart)는 채택한다. `EPHEMERA_AUTOSNAPSHOT=true` auto-snapshot은 opt-in·disk-expensive로 두고 public support로 승격하지 않는다 — env-only 확정(2026-07-11: config API/UI/MCP 공개 표면 미노출, opt-in 운영법은 runbook). |
| `v0.4.1` | adapted | client identity, daemon access audit(`GET /audit`), per-token TTL/rotation은 채택한다. `ephemera-ctl`은 runtime operator CLI로 유지하고 IronClaw MCP tool을 대체하거나 anvil MCP public surface로 승격하지 않는다. |
| `v0.4.2` | adapted, default plain 확정(D4 upstream-tracked, COW opt-in) | COW probe/fallback과 COW+Diff snapshot은 채택한다. `EPHEMERA_DISK_MODE=cow`는 anvil에서 명시적 opt-in이다. default 전환은 **종결**(2026-07-13, default plain 무기한 유지) — burn-in run 1이 diff-restore `500`으로 FAIL→D4 회부→1차 fix(PR #46: `overlaySparseDiff` `out.Sync()`)는 필요했으나 불충분(flip 재검증에서 **D4 재발**, 그 green은 n=1 우연). 재-RCA: host-b도 byte-identical 데몬으로 동일 GPF 재현=일반 결함, anvil-측 레버·fc CHANGELOG(#5705 v1.15.0 이미 포함, 조건 미충족) 전부 음성. fc v1.16.1 업그레이드가 실패율 100%→~15–25%(v1.16.0 vsock RX-race fix #5882 주 기여)로 극감시켰으나 잔여 존속, pre-resume quiescence 지연도 어느 고정값도 양 host n≥2 미달 → 근본은 anvil 밖 KVM/Firecracker resume-race, anvil-측 소진으로 종결. flip 재개는 upstream 해소 시에만. fc/KVM 상류 제보문 [`operations/2026-07-13-d4-firecracker-upstream-report.md`](operations/2026-07-13-d4-firecracker-upstream-report.md), run 기록 [`operations/2026-07-11-cow-burnin-run.md`](operations/2026-07-11-cow-burnin-run.md) round 1~4. |
| `v0.4.3` | adapted | dynamic flock membership, pause/resume, per-flock `max_agents`, Town Wall filter/rotation single-host lifecycle은 채택한다. routed members-only cross-host flock에는 그대로 적용하지 않는다. |
| `v0.4.4` | adapted, broadcast MCP exposure 기각 확정 | streaming `/tasks`(buffered 기본 계약 유지), nested depth guard(`EPHEMERA_MAX_TASK_DEPTH`, `508`), `GET /watchdog/status`, goose-agent slog는 채택한다. flock broadcast는 daemon API/CLI(`ephemera-ctl`)로만 두고 `anvil_*` MCP tool 노출은 기각 확정이다(2026-07-11: 로컬 host scope 전용이라 routed flock 원격 멤버 미도달·audit 1:1 불변식·adapter rate limit 부재가 근거, guard로 계속 고정). |
| `v0.4.5` | adapted | snapshot-restore auto-recovery(`recoverRestoredVM`/`reRestoreMachine`)를 채택하고 restore state에 `tenant_id`/`egress_policy`를 persist하되 응답 token redaction은 유지한다. anvil은 live·persisted restored VM이 참조하는 source snapshot의 `DELETE`를 `409`로 막아 upstream e2e 46c의 `200` orphan 동작과 의도적으로 divergent하다(먼저 VM을 삭제한 뒤 snapshot 삭제). |
| `v0.5.0` | adapted | operator Web UI(Svelte SPA, EN/KO, `cmd/goose-daemon/uidist/` embedded), `/config/profiles`, multi-turn goose session, graceful VM delete를 채택한다. Web UI는 runtime/operator surface이고 IronClaw MCP surface가 아니다 — `/ui/`(정적 bundle + login)만 auth 밖, 모든 data API는 bearer 뒤(guard `config_api_anvil_test.go`). `/config/profiles`는 `goose-secrets.yaml`을 절대 read/write하지 않는다(sentinel test). `cmd/anvil-mcp`는 그대로이고 `VMInfo`는 additive `provider`/`model` 필드만 추가한다. |
| `v0.5.1`-`v0.5.5` | adapted | `/config/providers`(key 존재 여부만), `/config/clients`(이름+만료만)를 sentinel test로 secret 비노출 확인. `system.md`-only prompt 편집(`64 KiB` cap), profile delete in-use → `409`, default profile 예약, traversal 거부. sizing preset + per-VM `VcpuCount`/`MemSizeMib`(snapshot metadata에 기록, legacy → 2/2048 fallback). Town Wall `SystemAuthor` author migration. restore agent-wait 30s→60s. |
| `v0.6.0` | adapted | runtime MCP Gateway(`internal/mcpgateway`, daemon `/config/mcp`·`/config/mcp/servers`·`/config/mcp/builtins` handler, `configs/mcp/*.example`, Web UI MCP console). **runtime/operator surface이고 IronClaw MCP surface가 아니다 — `cmd/anvil-mcp`를 대체하지 않는다.** 경계는 구조적으로 강제: caller profile은 source IP↔VM registry로 server-side 판정(unknown → `403`), backend credential은 host-side(`configs/mcp/secrets.yaml`, gitignored)에만 있고 VM에는 gateway URL만 주입(`VMPrepareOptions`에 credential 필드 없음), `audit/mcp.jsonl`은 metadata-only(고정 key set, `Err`도 omit), profile policy는 `servers.yaml`을 좁히기만 하고 넓힐 수 없다. anvil boundary guard 4종: IronClaw schema/adapter가 gateway tool 제외, audit metadata-only sentinel, `/config/mcp*` bearer 없으면 `401`, VM은 URL(credential 아님)만 받고 policy는 widen 불가. |
| `v0.6.1`, `v0.6.2`, `v0.6.4` | adapted | (upstream에 `v0.6.3` 없음) `EPHEMERA_NET_ANTISPOOF` 기본 on(ebtables best-effort); per-(VM,server) token-bucket rate limit(`EPHEMERA_MCP_RATE` 기본 `0`=unlimited, `EPHEMERA_MCP_BURST`); resources/prompts가 tools와 policy·rate bucket 공유(anvil guard); audit `kind` field; `GET /config/mcp/servers`는 transport/command + `has_credential`만 노출(leak guard + sentinel); stdio backend child env를 `[PATH,HOME,LANG]`+`spec.Env`로 재구성(`EPHEMERA_*` canary test), credential은 `credential_env`로만(argv 아님), root면 `nobody`로 실행 + `/var/lib/ephemera/mcp-stdio` scratch, shutdown이 stdio process group reap(pgid recycling-safe). |
| `v0.7.0` | adapted | end-user installer(`install.sh`/`uninstall.sh`/`INSTALL.md`/`ephemera.service.in`), release workflow(`scripts/build_release.sh`), conversation transcript restore를 채택한다. installer는 runtime/operator surface이고 systemd service는 canonical `ephemera` 이름을 유지한다(rule-permitted, anvil alias wrapper 없음). transcript restore는 daemon proxy `GET /vms/{id}/sessions/{name}/transcript`(bearer)로 노출하며, agent export는 read-only `goose session export`(model call 없음)이고 응답 schema `{turns:[{role,text}]}`는 auth-free여서 Web UI가 daemon token 없이 렌더한다. 4개 transcript-safety guard: bearer 없으면 `401`, payload는 provider key/CP token/`agent_token` sentinel-free, cache-hit는 agent spawn 없이 serve, export argv는 `session export -n {name} --format json`이며 run-token 거부. release build integrity: `build_release.sh`가 kernel/firecracker를 `main.go` pin과 `sha256sum -c`로 검증. 사전 backport 3종(kernel SHA atomic temp+rename, `resolveWorkDir`/`EPHEMERA_HOME`, `waitForAgent` per-probe)이 v0.7.0 reconcile에서 single definition으로 남았고(anvil이 stricter, net Go diff doc-comment-only), 기존 anvil adaptation은 rollback 없음. |

runtime MCP Gateway 경계(요약): `EPHEMERA_MCP_*`(gateway, runtime/operator)와
`ANVIL_MCP_*`(`cmd/anvil-mcp` IronClaw adapter 설정)는 서로 다른 namespace·surface다.
gateway는 adapter를 대체하지 않으며, IronClaw tool 목록에 gateway tool을 추가하지
않는다(guard `TestToolRegistrationsExcludeGatewayTools`,
`TestCurrentIronClawSchemasExcludeGatewayNamespacedTools`).

`v0.4.0`-`v0.7.0`은 anvil main runtime baseline으로 병합·적응되었고 full KVM gate로
검증됐다(`v0.4.x`는 Task 8, `v0.5.x`는 Task 4, `v0.6.x`는 Task 6, `v0.7.0`은 Task 7
문서 반영 기준). 이로써 upstream parity scope(`v0.4.0`-`v0.7.0`) 코드 편입이 완료됐다.
anvil runtime/operator baseline supports upstream ephemera v0.7.0 with anvil adaptations
for token redaction, tenant/egress, scheduler, audit, and IronClaw MCP surface separation.
전 태그별 adopted/adapted/deferred/excluded parity matrix는
[`docs/analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md`](analysis/11-v0.5.0-v0.7.0-core-service-parity-review.md)에
있다. 상세 병합 근거는
[`docs/analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md`](analysis/10-v0.4.0-v0.4.5-runtime-stabilization-adoption.md),
Phase 2 handoff([`docs/operations/2026-07-05-ephemera-v0.5-operator-sync-handoff.md`](operations/2026-07-05-ephemera-v0.5-operator-sync-handoff.md)),
Phase 3 handoff([`docs/operations/2026-07-05-ephemera-v0.6-mcp-gateway-sync-handoff.md`](operations/2026-07-05-ephemera-v0.6-mcp-gateway-sync-handoff.md)),
Phase 4 handoff([`docs/operations/2026-07-06-ephemera-v0.7-parity-sync-handoff.md`](operations/2026-07-06-ephemera-v0.7-parity-sync-handoff.md))에
보존한다.

sizing 결정: `v0.5.3`부터 anvil은 upstream default VM sizing `1` vCPU / `1024` MiB를
채택한다(이전 2/2048, KVM 근거로 승인, full e2e 3× `316✓`). snapshot metadata가 per-VM
sizing을 기록하고 legacy snapshot은 2/2048로 fallback한다. flock member spawn
(createFlock·add-agent)이 per-profile `EPHEMERA_VCPU_COUNT`/`EPHEMERA_MEM_SIZE_MIB`
override를 무시하고 `LookupProfile` default로만 sizing하던 upstream-inherited gap은
`POST /vms` 경로와 동일한 `readProfileConfig` override 블록을 미러링해 닫혔다
(2026-07-11, guard `TestSpawnVMForFlock_HonorsPerProfileSizing`).

keep-alive divergence(`adapted`): `v0.5.x` `gracefulAgentStop`이 v0.2.0부터 잠재하던
upstream shared pooled agent proxy client 결함을 드러냈다(guest IP 재활용 시 stale
keep-alive connection 재사용 → restored VM `/tasks` hang/`502`). `64ec57c`가 request마다
fresh dial(`DisableKeepAlives`)하고 connection-reuse guard test를 추가한다. upstream
connection pooling과의 의도적 divergence다. 2026-07-06 결정으로 anvil-side에서 유지하고
upstream 제안(기여)은 하지 않는다. 같은 divergence를
[`docs/operations/upstream-sync-policy.md`](operations/upstream-sync-policy.md)에도
기록한다.

---

## 5. 다음 upstream sync 후보 예비 분류

`v0.4.0`-`v0.7.0`은 모두 Section 4의 baseline 채택 상태로 이동했다. 2026-07-06 기준
upstream `main`과 최신 upstream tag는 `v0.7.0`이며, anvil은 이 관찰 범위 전체를 병합·
적응했다 — upstream parity scope 코드 편입이 완료돼 현재 pending sync 후보는 없다.

`v0.7.0` 이후 upstream 태그가 새로 관찰되면 이 인덱스에 pre-review 분류로 다시 추가하고,
sync 전 backlog triage와 별도 analysis 문서/sync branch 검증 뒤 채택 상태를 확정한다.
현재 남은 작업은 tag 채택이 아니다. release-gate 코드 항목 4종(audit-writer sentinel,
stdio stderr scrub, `credential_env` reserved names, production-mux auth sentinel)은
2026-07-06 follow-up batch(`4a802f5`, `0376afa`, `613a01b`, `cd2e70b`, `de5a7aa`,
`0625df5`)로 닫혔고, 마지막 open gate(valid provider key `semantic` run, e2e step 59)도
`18c7559`에서 OpenAI `gpt-4o`로 닫혔다(full e2e `343✓/0✗`) — release-gate open 항목
없음. 2026-07-11 결정으로 `v0.4.4` broadcast MCP 노출은 기각 확정, auto-snapshot
public support는 env-only 확정, flock member per-profile sizing 존중은 완료됐다. `v0.4.2`
default COW 전환은 **종결**됐다(2026-07-13 — default plain 무기한 확정, COW opt-in; D4는
upstream KVM/fc resume-race 추적 known limitation, anvil-측 소진, fc v1.16.1 최대 완화;
재개는 upstream 해소 시에만 — 위 v0.4.2 행). 남은 비목표는 runtime MCP Gateway의 IronClaw
표면 승격 금지다.

---

## 6. 새 ADR 작성 기준

다음 조건 중 하나에 해당하면 새 ADR을 추가한다.

- anvil 공개 표면 또는 제외 표면이 바뀐다.
- upstream ephemera 변경을 anvil 정책에 맞게 수정 채택하거나 제외한다.
- token, auth, MCP output, guest access 같은 보안 경계가 바뀐다.
- VM lifecycle, snapshot/restore, cleanup, watchdog, persistence 의미가 바뀐다.
- IronClaw와 anvil 사이의 tool 계약이 바뀐다.
- 기존 ADR을 대체하거나 폐기해야 한다.

새 ADR에는 반드시 상태, 날짜, 결정, 결과, 검증 기준을 적는다.
