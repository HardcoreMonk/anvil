# 2026-07-13 egress SNI transparent 필터 handoff

egress `profile` policy의 도메인 통제를 packet-string substring 매치에서 실제
파싱된 TLS ClientHello SNI로 전환하는 slice의 구현·검증 기록. branch
`feature/egress-sni-filter`. 설계 근거는
[design spec](../superpowers/specs/2026-07-13-egress-sni-filter-design.md)과
[implementation plan](../superpowers/plans/2026-07-13-egress-sni-filter.md),
결정 원문은 [ADR-0002](../adr/0002-egress-sni-transparent-filter.md)에 있다 —
이 handoff는 그 셋을 압축 요약하고 검증 증거·Follow-Up만 추가한다.

## 무엇이 main에 있나 (PR #59로 병합됨; 후속 정리 #60~#64는 아래 병합 이력·Follow-Up 참조)

- **신규 스키마 필드**: `egressProfile.AllowSNI []string`(`allow_sni` json
  key). `allow_cidrs`/`allow_hosts`/`dns_servers`와 병렬 additive, 기존
  profile 무변경 하위호환. `*.example.com` leading-label wildcard(한 개 이상
  라벨) + exact match. 도메인 charset 검사(ASCII 영숫자/`.`/`-`)를
  `allow_hosts`와 공유한다 — PR #61에서 `validateDomainCharset(field, host)`로
  분리해 에러 메시지가 이제 `allow_sni` 항목을 정확히 명명한다.
- **dispatch**: `allow_sni`가 비어 있지 않은 profile만 :443 새 흐름에
  `iptables -I FORWARD -s <guestIP> -p tcp --dport 443 -m connmark
  ! --mark 0x534e49 -j NFQUEUE --queue-num 88`(env `ANVIL_SNI_QUEUE_NUM`로
  override) + `-m connmark --mark 0x534e49 -j ACCEPT` fast-path 규칙을
  head-insert한다. 두 규칙 모두 기존 `egressCommand`/rollback 배관을 그대로
  탄다. **additive 순서**: 매 `-I` 삽입이 체인 맨 위로 밀어올리므로, 최종
  평가 순서(top-down, 역순)는 CIDR allow가 SNI dispatch보다 위 — `:443`
  목적지가 CIDR에 있으면 SNI 검사 없이 ACCEPT된다.
- **verdict 루프**(`cmd/goose-daemon/sni_verdict.go`): goose-daemon
  **in-process** goroutine, `github.com/florianl/go-nfqueue/v2`(이 slice의
  유일한 신규 direct 의존, MIT/pure-Go)로 NFQUEUE 큐 88을 bind한다. 재조립된
  ClientHello를 `internal/network/sni.ParseClientHelloSNI`로 파싱해
  `sni.Matcher`와 대조: 허용 → `SetVerdictWithConnMark`(mark `0x534e49`) +
  ACCEPT, 거부/파싱불가 → `NfDrop` + best-effort TCP RST 주입(guest 빠른
  실패). `NfQaCfgFlagFailOpen`을 설정하지 않고 `--queue-bypass`도 쓰지 않아
  리스너 부재/사망 시 커널이 큐에 든 패킷을 DROP한다(fail-closed).
- **preflight**(`commandEgressEnforcer.ApplyWithProfile`): `allow_sni`
  profile인데 verdict 루프가 `Ready()`가 아니면 iptables 규칙을 하나도
  적용하지 않고 spawn을 거부한다(`"egress profile %q requires SNI verdict
  loop but host lacks NFQUEUE capability (fail-closed)"`) — "규칙만 있고
  검사기가 없는" 절반 배선 상태를 원천 차단.
- **재조립**: `sni.Reassembler`가 흐름별(`guestIP:sport`)로 최대
  `sni.maxClientHelloBytes`(16 KiB)까지 버퍼링. 루프는 흐름 최대
  `sniReassemblerMaxFlows`(4096)개까지 LRU로 유지하고, 가득 차면 evict한다
  — evict된 흐름의 다음 세그먼트는 새 reassembler로 레코드 경계를 다시
  찾아야 하므로 malformed로 fail-closed DROP된다(never fail-open). 완결
  전 세그먼트는 **판정 없이 unmarked ACCEPT**로 통과한다 — 이 계약의 세부는
  아래 "알려진 한계" 참조.
- **감사/metric**: deny는 tenant가 있는 VM에 한해 `RuntimeAuditRecord`
  (`ToolName: "egress_sni_filter"`, `DaemonOperation: "egress_sni_denied"`,
  `SNI: <domain>`)로 기록되고, tenant 없는 VM은 redaction-safe slog로
  degrade한다. `ephemera_egress_sni_verdict_total{outcome="allowed"|"denied"|"dropped"}`
  카운터는 판정 흐름(allowed/denied)과 pre-classify infra drop(no-payload/
  IPv4 파싱실패/미등록 source — PR #61에서 `outcome="dropped"`로 추가)을 모두
  반영한다. 판정 전 unmarked passthrough는 verdict가 아니라 미포함.
- **복구 무결성**: VM 복구(warm/cold restart, snapshot restore)가 per-VM
  egress 전체(iptables 규칙 재설치 + SNI 레지스트리 재등록)를 **부팅 전**에
  재적용한다 — 호스트 리부트 후 fail-open 창과, 데몬 재시작 후 SNI 레지스트리만
  비어 규칙은 있는데 verdict가 안 걸리는 갭을 둘 다 봉쇄한다. 적용 실패 시
  VM을 부팅하지 않고 복구 실패로 처리하며(don't-boot, fail-closed), 모든
  give-up 경로가 적용된 egress 규칙을 `dropRecoveryState`에서 회수한다. (초기
  구현의 부팅-후-적용 + 비상 fence 방식은 egress-before-boot 리팩터로 대체됨.)
- **`go.mod`**: 신규 direct 의존 `github.com/florianl/go-nfqueue/v2 v2.1.0`
  하나뿐(`git diff main -- go.mod`로 확인됨). indirect로 `mdlayher/netlink`,
  `mdlayher/socket`가 전이 도입되고 `golang.org/x/net`/`x/sync`/`x/text`가
  버전업된다.

## 위협 모델 / 잔여 위험 (요약 — 전문은 ADR-0002)

anvil guest는 신뢰된 golden-image 워크로드다. **핵심 계약 한 줄**: SNI
필터는 신뢰 워크로드의 의도된 :443 egress를 강제·감사한다. 적대적
in-guest 루트에 대한 완전 봉쇄가 아니다.

| 잔여 위험 | 계약 |
|---|---|
| ECH/ESNI | 인식 가능한 SNI가 없으면 fail-closed deny. ECH 엔드포인트는 CIDR fallback opt-in만 |
| non-TLS(:80 등) | SNI 층 미통과, base REJECT+CIDR만 |
| QUIC/UDP:443 | v1 비목표, default-deny |
| SNI spoofing | SNI는 guest-asserted, CIDR 핀 없이는 임의 IP 터널 가능 |
| domain fronting | TLS 종단 없이 미탐지 |
| pre-decision 부분 ClientHello 전달 | 미완결 세그먼트는 판정 전 unmarked 통과(승인 mark는 아님 — 승인 누수 아님). 16 KiB/flow는 하드 캡이 아니라 eviction 후 재시작되므로 TCP hiccup 속도로만 rate-limit. 완전 봉쇄엔 hold-then-decide 재설계 필요(v1 미채택) |

## 검증 증거

**유닛** (root 불필요, `-race`):
- `internal/network/sni`: 파서 정상/부재/malformed/멀티세그먼트 재조립/ECH
  outer 추출/wildcard 매처/오버사이즈 거부 회귀(`parser_test.go`,
  `matcher_test.go`).
- `cmd/goose-daemon`: `allow_sni` 파싱/검증(`egress_policy_test.go`
  `TestLoadEgressProfileParsesAllowSNI`,
  `TestValidateEgressSNIAcceptsWildcardAndExact`), dispatch 명령 생성
  (`TestPlanProfileEgressCommandsEmitsSNIDispatch`,
  `TestPlanProfileEgressCommandsNoSNIWhenEmpty`,
  `TestPlanProfileEgressCommandsCIDRAboveSNI`), `decide` 라우팅/fail-closed
  (`sni_verdict_test.go` `TestSNIDecideRouting`,
  `TestSNIDeregisterFailsClosed`), audit/metric side-effect
  (`TestSNIRecordVerdictAuditsDenyWithTenant`,
  `TestSNIRecordVerdictNoAuditWithoutTenant`,
  `TestSNIRecordVerdictMetricAlwaysIncrements`,
  `metrics_handler_test.go` `TestMetrics_HandlerExposesSNIVerdictTotal`),
  enforcer apply/cleanup 대칭(`api_test.go`
  `TestApplyWithProfileRefusesSNIWhenLoopNotReady`,
  `TestApplyWithProfileRegistersAndDeregistersSNI`,
  `TestCommandEgressEnforcerProfileCleanupRemovesSNIRulesInReverse`),
  recovery 재적용(`recovery_test.go`
  `TestReapplyRecoveredEgressReRegistersSNI`,
  `TestReapplyRecoveredEgressIdempotentFlush`,
  `TestReapplyRecoveredEgressFailClosedOnApplyError`).
- 이 verdict 글루(netlink 바인딩, `SetVerdictWithConnMark` 커널 mark, RST
  원시소켓 주입, dispatch 규칙 실효, steady-state fast-path)는 root +
  netfilter가 필요해 유닛으로 실검 불가 — 아래 KVM e2e가 유일한 실검 경로다.

**KVM e2e** (`sudo -n bash scripts/anvil-egress-sni-e2e.sh`, exit 0, 독립
재실행으로 확인됨):
- Phase 0: daemon + verdict loop가 NFQUEUE를 bind하고, `allow_sni` profile
  VM spawn이 `Ready()` 전제(fail-closed preflight)를 실제로 통과한다.
- Phase 1: `api.anthropic.com:443` 도달 — TLS handshake 성공(curl rc=0,
  HTTP 404 응답 자체가 성공 증거) + `iptables -S FORWARD`에 해당 VM의
  `-sni-fastpath`/`-sni-nfqueue`/`connmark 0x534e49` 규칙 존재 확인.
- Phase 2: `example.org:443` 차단 — curl rc=35, DUR=0s(양쪽 모두 TCP
  handshake는 완료되므로 IP/포트 차단이 아니라 SNI 기반 차단임이 증명됨,
  RST 즉시 실패로 관측).
- Phase 3: runtime audit에 `egress_sni_denied sni=example.org` 기록 +
  redaction spot-check(bearer/API-key 미유출) 확인.
- Phase 4: 대량 패킷을 왕복하는 전체 allow 흐름에서 metric delta가 1만
  증가 — conntrack mark fast-path가 슬로우패스를 우회함을 증명(steady-state
  성능 스모크).

## 병합 이력 (post-merge)

- **#59** — 슬라이스 본체 병합(2026-07-13). 최종 검증(유닛 `-race`, `go.mod`
  신규 direct 의존 1개, 전체 KVM 게이트 334✓, egress KVM e2e, secret-scan) 통과.
- **#60** — flaky `TestDistributedTokens_ConcurrentRefill` 안정화(무관 pre-existing 스케줄링 race).
- **#61** — 코드품질 Minor 배치(아래 Follow-Up #6 대부분·#7 해소).
- **#62** — golden-image 재빌드 견고화(Follow-Up #8 해소).
- **#63** — 복구 egress-before-boot 리팩터(Follow-Up #9 해소, emergency fence 제거).
- **#64** — transient-recovery-창 문서 정합(ADR-0002 해당 행 RESOLVED).

## Follow-Up Tasks

1. **QUIC/UDP:443 SNI 파싱** — v1 비목표로 보류(QUIC Initial 포맷이 TLS
   ClientHello와 달라 별도 파서 필요). UDP:443은 현재 default-deny.
2. **`allow_hosts`(legacy substring) 제거 시점 재검토**(OQ8) — 고정 런타임
   계약 표면이라 이번 slice에서는 deprecated 표기만 하고 유지했다. 신규
   profile이 `allow_sni`로 충분히 이행된 뒤 제거 여부를 별도 결정한다.
3. **multi-queue per-VM NFQUEUE 재검토** — 현재는 단일 queue 88 + verdict
   루프 내부 src-IP 레지스트리 라우팅. VM 수가 늘어나 큐 경합/고갈이
   문제가 되면 VM별 queue-num 분리를 검토한다.
4. **ECH inner 대응 불가 재확인**(설계 한계) — anvil은 ECH를 무력화하지
   않으며 outer(cleartext) SNI만 관측 가능하다. ECH 채택이 늘면 CIDR
   fallback opt-in 외의 대응이 필요한지 재확인한다.
5. **pre-decision 부분 ClientHello 전달의 hold-then-decide 재설계** —
   수용된 잔여 위험(승인 누수는 아님)이지만, 완전 봉쇄가 필요해지면 판정
   전 세그먼트를 보류하는 재설계를 검토한다(v1은 YAGNI로 미채택).
6. ~~**코드 품질 Minor**~~ — **DONE/OBSOLETE (PR #61·#63)**. #61에서 수정:
   fastpath↔nfqueue slice-순서 테스트, Task 5b bare-comment flush arm 테스트,
   `decide`/`Start` pre-parse dedup(`resolveEntry`), `reassemblerFor` lock
   주석, `validateEgressHost` 메시지(`validateDomainCharset`). #63의
   egress-before-boot로 obsolete: egress fence 이중실패 teardown·fenced VM
   recovered 카운트 — 부팅 전 실패로 대체돼 fenced running VM 자체가 없음.
7. ~~**metric completeness 재검토**~~ — **DONE (PR #61)**. no-payload/IPv4
   파싱실패/미등록 source drop이 이제 `outcome="dropped"`로 카운트된다.
8. ~~**golden-image staleness robustness**~~ — **DONE (PR #62)**. golden
   재빌드를 temp+atomic rename + 올바른 cwd/env로 견고화 — 중단된 빌드가 부분
   이미지를 real path에 남기지 않고, EPHEMERA_HOME≠launch-cwd 배포에서도 산출
   경로 발산 없음.
9. ~~**복구 경로 egress-before-boot 검토**(transient 창 제거)~~ — **DONE
   (2026-07-14, egress-before-boot 리팩터)**. 복구가 세 경로 모두 egress를
   부팅 전에 적용하고 실패 시 부팅하지 않으므로 transient fail-open 창이
   제거됐다. emergency fence 메커니즘도 함께 삭제(부팅 전 실패로 대체). ADR-0002
   잔여위험 표에서 해당 행은 RESOLVED로 갱신됨.
10. **zone `~/projects/claude-zone/docs/FOLLOWUP.md` 갱신** — zone repo는
   이 anvil branch 밖이므로 이 handoff에는 트리거만 기록한다. "egress
   L7/SNI hardening"류 이월 항목이 있으면 구현 완료로 갱신 필요(anvil
   `CONTEXT.md`/`RELEASE_NOTES.md`는 이 slice에서 이미 갱신했다).
11. **release 단계 zone 인벤토리 동기화** — PR 머지 후 release 단계에서
    `ops/units.yaml`/`ops/projects.yaml`/`wiki/entities/`(필요 시) 갱신.
    이 slice는 신규 systemd unit/서비스를 추가하지 않는다(goose-daemon
    in-process 확장뿐) — 인벤토리 변경 필요 여부는 release 단계에서
    판단한다.

## 관련 문서

- [ADR-0002](../adr/0002-egress-sni-transparent-filter.md) — 결정 원문,
  잔여 위험 계약 표 전문.
- [design spec](../superpowers/specs/2026-07-13-egress-sni-filter-design.md),
  [implementation plan](../superpowers/plans/2026-07-13-egress-sni-filter.md).
- [security-policy.md `allow_sni` 절](security-policy.md),
  [runbook.md `Egress SNI 필터 운영` 절](runbook.md),
  [PUBLIC_RELEASE_BOUNDARY.md `Egress SNI filter` 행](../PUBLIC_RELEASE_BOUNDARY.md),
  [multi-tenant-roadmap.md 비목표 절](../architecture/multi-tenant-roadmap.md).
