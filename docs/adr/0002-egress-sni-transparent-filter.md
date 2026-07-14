# ADR-0002: Egress :443 transparent SNI 필터 — substring 매치에서 파싱된 SNI로 전환

> **상태:** accepted
> **날짜:** 2026-07-13
> **대상:** anvil downstream repository (`cmd/goose-daemon`, `internal/network`)

---

## 맥락

`profile` egress policy는 guest IP 기준 `iptables FORWARD` 규칙으로 (1) 기본
REJECT, (2) DNS 서버/CIDR/host-string 예외 ACCEPT를 삽입해왔다
(`egress_policy.go`). 이 중 도메인 단위 통제를 노리는 `allow_hosts`는 필드
파싱이 아니라 **패킷 payload substring 매치**였다(`iptables -m string --string
<host> --algo bm`). `docs/operations/security-policy.md`는 이미 이 한계를
"`allow_hosts` rule은 packet string match 기반의 coarse host allowlist이며,
L7 proxy 또는 SNI gateway를 대체하지 않는다"고 명시해왔다.

substring 매치의 구체적 한계:

- TLS ClientHello가 여러 TCP 세그먼트로 쪼개지면 매치가 실패한다.
- SNI가 아닌 위치(HTTP body, cert chain, 임의 payload)에 같은 문자열이
  나타나도 우연히 ACCEPT한다.
- CDN 뒤 도메인은 CIDR로 고정할 수 없어 IP 통제와 도메인 의도가 어긋난다.

결과적으로 "이 tenant는 `api.anthropic.com`에만 나갈 수 있다"는 도메인
계약을 정직하게 강제할 계층이 없었다. 최소 3개 릴리즈(v0.2.x~v0.3.1)에서
"egress L7/SNI hardening"이 후속 후보로 이월됐으나(`RELEASE_NOTES.md`
v0.3.1/v0.3.0 절, `CONTEXT.md` 후속 후보) 착수된 적이 없었다. 설계 근거 원문은
[`docs/superpowers/specs/2026-07-13-egress-sni-filter-design.md`](../superpowers/specs/2026-07-13-egress-sni-filter-design.md),
구현 계획은
[`docs/superpowers/plans/2026-07-13-egress-sni-filter.md`](../superpowers/plans/2026-07-13-egress-sni-filter.md)에
보존한다.

---

## 위협 모델

anvil guest는 **신뢰된 golden-image 워크로드**(goose agent)를 돌리는 VM이지,
루트를 쥔 적대적 사용자가 임의 바이너리로 host를 공격하는 환경이 아니다.
이 필터의 목표는:

- **In-scope**: 오작동/과잉 egress 방지, tenant가 의도한 provider 도메인으로만
  나가도록 강제, 차단된 egress의 감사 가능성.
- **Out-of-scope(잔여 위험으로 계약)**: guest 안에서 루트를 쥔 적대자가 SNI
  spoofing/domain fronting/ECH로 필터를 우회하는 것을 **완전히** 막는 것.
  이는 TLS 종단(MITM) 없이는 불가능하며, 아래 §잔여 위험 계약에 명시한다.

이 위협 모델을 ADR에 못 박는 이유는, SNI 필터를 "완전한 L7 방화벽"으로
과대 포장하면 잔여 위험이 은폐되기 때문이다.

---

## 결정

**Host-local transparent SNI 필터**를 in-process NFQUEUE verdict loop로
도입한다. guest는 무변경(여전히 L3 라우팅만, L7 가시성 없음)이다.

### 메커니즘

새 :443 TCP 흐름의 ClientHello가 실린 패킷 →
`iptables(-nft) -I FORWARD -s <guestIP> -p tcp --dport 443 -m connmark
! --mark 0x534e49 -j NFQUEUE --queue-num 88`(env `ANVIL_SNI_QUEUE_NUM`로
override 가능) → goose-daemon **in-process** verdict 루프
(`github.com/florianl/go-nfqueue/v2` — 이 slice의 유일한 신규 direct
의존, MIT/pure-Go)가 재조립된 ClientHello를 파싱해 SNI를 `allow_sni`
매처와 대조한다.

- **허용** → `SetVerdictWithConnMark`로 흐름의 conntrack 엔트리에 승인
  mark(`0x534e49`, ASCII "SNI")를 찍고 ACCEPT. 이후 같은 흐름의 패킷은
  `-m connmark --mark 0x534e49 -j ACCEPT` fast-path 규칙이 커널에서
  바로 처리한다(NFQUEUE 슬로우패스는 ClientHello 세그먼트에만 탄다).
- **거부** → `NfDrop` + best-effort TCP RST 주입(guest가 타임아웃 대신
  즉시 연결 종료를 관측하도록).

dispatch/fast-path 규칙은 기존 `egressCommand`/`commandEgressEnforcer`
rollback 배관(`-I`↔`-D` 대칭 cleanup)을 그대로 재사용한다 — 신규 규칙 엔진은
없다.

### 스키마 확장

`egressProfile`에 신규 `allow_sni []string` 필드를 추가한다(기존
`allow_hosts`의 의미 재해석이 아니다):

```go
type egressProfile struct {
    AllowCIDRs []string `json:"allow_cidrs"`
    AllowHosts []string `json:"allow_hosts"` // legacy: -m string substring (deprecated)
    AllowSNI   []string `json:"allow_sni"`   // parsed ClientHello SNI, default-deny
    DNSServers []string `json:"dns_servers"`
}
```

- **하위호환**: `allow_cidrs`/`allow_hosts`/`dns_servers`만 있는 기존
  profile은 무변경 동작. `allow_sni`는 opt-in(비어 있으면 :443용 NFQUEUE
  dispatch 규칙 자체를 생성하지 않는다).
- **wildcard**: `*.example.com` = 왼쪽 한 개 이상 라벨 매치(leading label만,
  임의 위치 glob은 비지원). exact match가 기본.
- **검증 재사용**: `validateEgressHost`(ASCII 영숫자/`.`/`-`)를 SNI 항목에도
  재사용한다 — 알려진 부작용으로 에러 메시지가 `allow_sni` 항목에도
  `allow_hosts`라는 문자열을 쓴다(cosmetic, 코드 리뷰 triage 항목으로 등재).
- `allow_hosts`는 **legacy/deprecated**(substring, coarse)로 표기하고 유지한다
  (OQ8, 아래).
- profile directory 계약은 변경하지 않는다:
  `configs/profiles/{profile}/egress.json`, `EPHEMERA_EGRESS_PROFILE_DIR`,
  `ANVIL_EGRESS_PROFILE_DIR`.

### fail-closed + preflight (OQ1) — `--queue-bypass` 배제

verdict 루프/NFQUEUE 리스너 부재 시 두 방향이 있다: fail-open(:443 허용 +
로그만 남김)과 fail-closed(:443 차단). **fail-closed를 채택**한다 — 조용한
미강제보다 명시적 차단이 이 필터의 정직성 계약에 부합한다.

이를 위해 두 가지를 함께 확정한다:

1. **`--queue-bypass` 미사용**. 이 iptables 플래그는 큐 리스너 부재 시
   패킷을 통과시키는 fail-**open** 동작이라 위 결정과 정면으로 모순된다 —
   dispatch 규칙에서 명시적으로 배제한다. `nfqueue.Config`도
   `NfQaCfgFlagFailOpen`을 설정하지 않는다(`sni_verdict.go` `Start`) —
   리스너가 죽거나 없으면 커널이 큐에 들어간 패킷을 DROP한다.
2. **preflight capability 체크**. `commandEgressEnforcer.ApplyWithProfile`이
   `allow_sni`가 있는 profile에 대해 verdict 루프가 `Ready()`가 아니면 규칙을
   *하나도 설치하지 않고* spawn 자체를 거부한다(`"egress profile %q requires
   SNI verdict loop but host lacks NFQUEUE capability (fail-closed)"`). 규칙만
   깔리고 검사기가 없는 "절반만 배선된" 상태(사실상 fail-open)를 원천
   차단한다.

**OQ2 — host capability 기준**: `allow_sni` profile을 지원하지 않는 host에
스케줄되면 조용한 under-enforce가 된다. v1은 별도 스케줄러 capability 축을
신설하지 않고, "`profile` egress policy를 지원하는 모든 host는 NFQUEUE
verdict 루프를 갖춰야 한다"는 **baseline 요구**로 문서화한다(운영 확인 절차는
`docs/operations/runbook.md`).

### OQ3~OQ8 — 나머지 미결 확정

- **OQ3 (verdict 위치)**: 별도 systemd unit이 아니라 goose-daemon
  **in-process** goroutine. 신규 unit/패키지 의존을 만들지 않고, 이미
  daemon이 `Manager`로 네트워크를 세팅하는 "단일 바이너리 + in-process
  Manager" 패턴을 유지한다.
- **OQ4 (dispatch 표현)**: native `nft` 재작성이 아니라 **iptables(-nft)
  exec 재사용**. dispatch/fast-path 규칙을 기존 `egressCommand` rollback
  기계에 그대로 태워 진짜 신규는 verdict 루프 하나로 좁힌다. egress 규칙
  엔진 전체의 native `nft` 이관은 별개 리팩터로 비목표.
- **OQ5 (wildcard 깊이)**: 단일 `*.` 접두(한 개 이상 라벨) + exact만 지원.
  임의 glob은 비지원(YAGNI).
- **OQ6 (deny 응답)**: 조용한 DROP이 아니라 **best-effort TCP RST** 회신 —
  guest가 타임아웃 대신 즉시 실패를 관측한다(디버깅/UX). RST 주입 실패 시
  조용한 DROP으로 degrade한다(never fail-open).
- **OQ7 (ECH fallback)**: ECH 엔드포인트에 outer-SNI allowlist를 허용하지
  않는다 — **CIDR fallback만** 명시 opt-in으로 허용한다. outer SNI는 신뢰가
  약하다는 판단.
- **OQ8 (`allow_hosts` 폐기 시점)**: 즉시 제거하지 않는다. **deprecated
  표기 후 유지**하고 신규 profile은 `allow_sni`로 유도한다. 고정 런타임
  계약 표면(`docs/operations/runbook.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`)이라
  제거는 별도 결정이 필요하다.

### Additive 계약 — CIDR가 SNI보다 상위

SNI 층은 CIDR allowlist를 **대체하지 않는다**. `planProfileEgressCommands`는
매 `-I`(head insert) 삽입 호출마다 규칙을 체인 맨 위로 밀어올리므로, 최종
`FORWARD` 체인 평가 순서(위→아래, 먼저 매치한 규칙이 승리)는 명령 생성
순서의 역순이다 — 결과적으로 **CIDR allow 규칙이 SNI fast-path/NFQUEUE
dispatch 규칙보다 위에 앉는다**. 즉 :443 목적지가 CIDR allowlist에 있으면
SNI 검사에 도달하지도 않고 ACCEPT된다(명시 IP 신뢰가 도메인 검사를
우회한다). CDN 뒤 도메인은 SNI로, 고정 IP 백엔드나 비-TLS 엔드포인트는
CIDR로, DNS는 `dns_servers`로 — 세 층은 병렬 additive 계약이다.
`allow_sni`가 비어 있으면 :443용 NFQUEUE dispatch 규칙 자체가 생성되지
않아(`NoSNIWhenEmpty`) 기존 CIDR-only profile은 완전히 무변경으로 남는다.

---

## 잔여 위험 계약

| 위협 | 이 설계의 처리 | 잔여 위험 / 계약 |
|---|---|---|
| **ECH/ESNI**(SNI 암호화) | ClientHello에 cleartext SNI가 없거나 decoy outer SNI만 있으면, verdict는 **인식 가능한 allowlisted SNI가 없으므로 DROP**(fail-closed, default-deny와 일관) | anvil은 ECH를 무력화하지 않는다. ECH 필요 엔드포인트는 CIDR fallback으로만 명시 opt-in 허용한다(OQ7) — outer-SNI allowlist는 지원하지 않는다 |
| **non-TLS**(plain HTTP:80, 임의 TCP) | SNI가 없으므로 NFQUEUE dispatch 대상이 아니다 → base REJECT로 떨어진다(CIDR/포트 allow가 없으면 차단) | HTTP Host 헤더 검사는 L7 proxy 영역(비목표). :80은 CIDR로만 허용/차단된다 |
| **QUIC/UDP:443** | QUIC Initial SNI 파싱은 v1 비목표. UDP:443은 allow 규칙이 없으면 base REJECT | **UDP:443 기본 차단**을 계약. QUIC SNI 파싱은 후속(Follow-Up) |
| **SNI spoofing**(가짜 SNI로 다른 IP 접속) | verdict는 SNI 문자열만 본다 — guest가 `allow_sni` 값을 제시하며 실제로는 SNI를 무시하는 임의 서버로 갈 수 있다. `dns_servers` 강제 + CIDR 핀으로 부분 완화 | SNI는 **guest-asserted**다. CIDR 핀 없이는 allowed SNI 값으로 임의 IP에 터널링 가능. 신뢰 워크로드 위협 모델상 수용, 계약으로 명시 |
| **domain fronting**(SNI ≠ 내부 Host) | verdict는 SNI만 본다. TLS 내부 Host는 암호문이라 관측 불가 → 같은 CDN의 fronted 도메인에 도달 가능 | TLS 종단 없이는 탐지 불가(비목표). 잔여 위험 계약 |
| **pre-decision 부분 ClientHello 전달**(신규 — 구현 세부에서 확인) | 멀티세그먼트 ClientHello에서, 아직 완결되지 않은 세그먼트는 SNI 판정이 나기 **전에** unmarked ACCEPT로 통과한다(`sniVerdictLoop.Start`의 `!done` 분기) — 다음 세그먼트가 같은 큐로 재진입해 판정을 이어간다. 완결/malformed 시점에만 최종 verdict(ACCEPT+mark 또는 DROP)가 나며, 그때만 metric/audit이 기록된다. 재조립 테이블(`sniReassemblerMaxFlows=4096`, per-flow `sni.maxClientHelloBytes=16384`)이 가득 차면 LRU eviction이 발생하고, evict된 흐름의 다음 세그먼트는 **새 reassembler 인스턴스**로 시작해 레코드 경계를 못 잡고 fail-closed DROP한다(never fail-open) | 이 전달은 **승인 누수가 아니다** — conntrack mark(fast-path 자격)는 오직 완결된 ClientHello의 positive SNI 매치에서만 찍힌다. 다만 16 KiB는 흐름당 하드 캡이 아니다: eviction 후 새 인스턴스가 시작되므로, 공격자가 세그먼트를 의도적으로 지연시키며 계속 "미완결" 상태를 유지하면 판정 전 unmarked 세그먼트가 반복 통과할 수 있다(각 세그먼트가 개별 NFQUEUE 판정을 거쳐야 하므로 TCP 자체의 전송 hiccup으로만 자연 rate-limit된다). 완전 봉쇄에는 판정 전 전송을 보류하는 hold-then-decide 재설계가 필요하다 — v1은 채택하지 않는다(YAGNI, 아래 결과·비용 참조) |
| **복구 중 transient egress 창** — **RESOLVED (2026-07-14)** | (과거) 복구가 egress를 부팅 후에 재적용해, 호스트 리부트 직후 복구 VM의 부팅~agent-wait 구간에 일시 fail-open 창이 있었다. **egress-before-boot 리팩터로 제거**: 복구가 이제 세 경로(warm/cold/snapshot) 모두 egress를 **부팅 전**에 적용하고(spawn과 동일), 적용 실패 시 부팅하지 않으므로(don't-boot) 규칙 없이 패킷을 낼 창이 없다. 실행 중 fenced VM이 생기지 않아 emergency fence 메커니즘도 제거됨 | **닫힘.** 더는 잔여 위험 아님 — 복구 실패 시 부팅 자체를 거부(fail-closed)하고, 모든 give-up 경로가 `dropRecoveryState`에서 적용된 egress 규칙을 회수한다 |

**핵심 계약 한 줄**: SNI 필터는 신뢰 워크로드의 의도된 :443 egress를
강제·감사한다. 적대적 in-guest 루트에 대한 완전 봉쇄가 아니다.

---

## 결과

긍정적 결과:

- "이 tenant는 `api.anthropic.com`에만 나갈 수 있다" 같은 도메인 계약을
  실제 파싱된 SNI로 강제·감사할 수 있다(substring 오탐/fragmentation
  취약점이 없어진다).
- 승인된 흐름은 conntrack mark + 커널 fast-path로 처리되어 steady-state
  throughput은 NFQUEUE 슬로우패스를 타지 않는다.
- 신규 코드 의존은 `github.com/florianl/go-nfqueue/v2` 하나뿐이고, dispatch는
  기존 iptables-exec/rollback 배관을 재사용해 침습 범위가 좁다.
- fail-closed + preflight 조합이 "규칙은 깔렸는데 검사기가 없어 조용히
  통과"하는 상태를 구조적으로 배제한다.

비용/잔여 위험:

- verdict 루프가 host별 신규 런타임 컴포넌트다 — NFQUEUE/netlink 가용성이
  host baseline 요구가 된다(OQ2, 스케줄러 capability 축은 아직 없음).
- 잔여 위험 계약 표의 6개 항목(특히 SNI spoofing/domain fronting/pre-decision
  부분전달)은 적대적 in-guest 루트를 완전히 막지 못한다 — 신뢰 워크로드
  전제가 깨지면 이 설계의 보장도 약해진다.
- `allow_hosts`(legacy substring)를 당장 제거하지 않아 두 계층(coarse
  substring + precise SNI)이 당분간 공존한다.

---

## 검증 기준

- 유닛: `internal/network/sni`(파서 정상/부재/malformed/멀티세그먼트/ECH
  outer, wildcard 매처, fuzz), `cmd/goose-daemon`(`decide` 라우팅,
  preflight 거부, apply/cleanup 대칭, recovery 재적용, audit/metric
  side-effect) — 전부 root 없이 PASS.
- KVM e2e(`sudo -n bash scripts/anvil-egress-sni-e2e.sh`, exit 0):
  Phase 1 허용 도메인(`api.anthropic.com:443`) 도달 + `-sni-fastpath`/
  `-sni-nfqueue`/`connmark 0x534e49` 규칙 확인, Phase 2 비허용 도메인
  (`example.org`) 차단(RST 즉시 실패), Phase 3 감사 레코드에
  `egress_sni_denied sni=example.org` 기록 + redaction 확인, Phase 4 승인
  흐름의 fast-path 동작(대량 패킷에도 metric 증분이 1).
- `git diff main -- go.mod`가 신규 direct 의존 `github.com/florianl/go-nfqueue`
  하나만 보여야 한다.
- 기존 egress 회귀(`go test ./cmd/goose-daemon/ -run Egress`)와 전체 KVM
  게이트(`sudo bash e2e_test.sh`)가 그대로 PASS해야 한다(egress apply 경로를
  만졌으므로 필수).

상세 근거·경계 사례·테스트 매핑은
[design spec](../superpowers/specs/2026-07-13-egress-sni-filter-design.md),
[implementation plan](../superpowers/plans/2026-07-13-egress-sni-filter.md),
[handoff](../operations/2026-07-13-egress-sni-handoff.md)에 있다.

---

## 관련 문서

- [`docs/operations/security-policy.md`](../operations/security-policy.md) — Egress
  policy 절이 이 ADR의 계약을 운영 관점에서 요약한다.
- [`docs/architecture/multi-tenant-roadmap.md`](../architecture/multi-tenant-roadmap.md) —
  비목표 절이 SNI in-scope 전환을 반영한다.
- [`docs/PUBLIC_RELEASE_BOUNDARY.md`](../PUBLIC_RELEASE_BOUNDARY.md) — egress 표면에
  `allow_sni`/`ANVIL_SNI_QUEUE_NUM`/connmark 상수를 추가한다.
- [`docs/operations/runbook.md`](../operations/runbook.md) — profile 작성법, preflight
  거부 관측, fail-closed 진단, deny 감사/metric 확인 절차.
