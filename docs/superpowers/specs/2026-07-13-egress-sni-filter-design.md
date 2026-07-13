# Egress L7 도메인 강제 — Transparent SNI 필터 설계

- 작성일: 2026-07-13
- 상태: **설계 확정 (2026-07-13 사용자 설계 리뷰 승인)** — 골격(in-process
  NFQUEUE + userspace TLS 파서·conntrack fast-path / default-deny additive SNI 층 /
  신규 `allow_sni` 필드 / 명시적 잔여-위험 계약)과 미결 8건 전부 **권고안대로 확정**:
  OQ1 fail-closed+preflight, OQ2 baseline 요구, OQ3 in-process, OQ4 iptables-exec
  재사용, OQ5 `*.` 단일 wildcard+exact, OQ6 RST 응답, OQ7 ECH는 CIDR fallback만,
  OQ8 allow_hosts deprecated 유지. 아래 각 Q절의 권고가 곧 확정안이다.
  (원래 초안 표기 — 기록 보존: 옵션과 권고를 담되
  각 미결 질문의 최종 결정은 리뷰에서. 구현 착수는 별도 승인 후.)
- 브랜치: `feature/egress-sni-filter`
- 사용자 확정 접근: **transparent SNI 필터 (host nftables, guest 무변경)**.
  L7 forward proxy(가이드 방식)도 eBPF/tc 방식도 아님.
- 관련 코드:
  - `internal/network/manager.go`(`setupBridge` bridge/NAT/baseline FORWARD, `manager.go:72-110`)
  - `cmd/goose-daemon/egress_policy.go`(`egressProfile` 스키마·`planProfileEgressCommands`)
  - `cmd/goose-daemon/api.go`(`commandEgressEnforcer.ApplyWithProfile`, `api.go:1959-2006`)
  - `internal/anvilmcp/tenant_policy.go`(`EgressPolicy` enum·`SelectRuntimeHost`)
  - `internal/network/antispoof.go`(L2 anti-spoof, 인접 계층)
- 증거 브리프: `/tmp/.../scratchpad/egress-l7-evidence.md`(controller 판단용 1차 자료)

## 문제

현재 egress 강제는 L3/L4에서 멈춘다. `profile` 정책은 guest IP source 기준
`iptables FORWARD` 규칙으로 (1) 기본 REJECT, (2) DNS 서버/CIDR/host-string
예외 ACCEPT를 삽입한다(`egress_policy.go:104-133`). 이 중 도메인 단위 통제를
노리는 `allow_hosts`는 **필드 파싱이 아니라 패킷 payload substring 매치**다
(`iptables -m string --string <host> --algo bm`, `egress_policy.go:127-132`).

이 방식의 한계는 이미 운영 문서에 명시돼 있다:
- `docs/operations/security-policy.md:121-122` — "`allow_hosts` rule은 packet
  string match 기반의 coarse host allowlist이며, L7 proxy 또는 SNI gateway를
  대체하지 않는다."
- substring 매치는 (a) TLS ClientHello가 여러 TCP 세그먼트로 쪼개지면
  실패하고, (b) SNI가 아닌 위치(예: HTTP body, cert chain, 임의 payload)에
  같은 문자열이 나타나도 우연히 ACCEPT하며, (c) CDN 뒤 도메인은 CIDR로 고정
  불가라 IP 통제와 도메인 의도가 어긋난다(`AllowCIDRs` net.ParseCIDR 고정,
  `egress_policy.go:58-61`).

결과: "이 tenant는 `api.anthropic.com`에만 나갈 수 있다"는 도메인 계약을
정직하게 강제할 계층이 없다. 최소 3개 릴리즈(v0.2.x~현재)에서 "egress L7/SNI
hardening"이 후속 후보로 이월됐으나(`RELEASE_NOTES.md:747`, `:860`;
`CONTEXT.md:478`) 착수된 적 없다.

## 결정 (사용자 확정)

**Host-local transparent SNI 필터.** guest는 무변경(여전히 L3 라우팅만, L7
가시성 없음)이고, host의 netfilter 경로가 TLS ClientHello의 SNI 필드를
읽어 `profile`의 도메인 allowlist를 강제한다. guest→NAT 경로는 그대로
`goose-br0` bridge → `FORWARD` 체인 → `MASQUERADE`(`manager.go:87-107`)를
지난다 — 필터는 이 `FORWARD` 슬로우패스에서 완결된다.

### 위협 모델 (명시 — 이 설계의 정직성 기준)

anvil guest는 **신뢰된 golden-image 워크로드**(goose agent)를 돌리는 VM이지,
루트를 쥔 적대적 사용자가 임의 바이너리로 host를 공격하는 환경이 아니다.
따라서 이 필터의 목표는:

- **In-scope**: 오작동/과잉 egress 방지, tenant가 의도한 provider 도메인으로만
  나가도록 강제, 차단된 egress의 감사 가능성.
- **Out-of-scope(잔여 위험으로 계약)**: guest 안에서 루트를 쥔 적대자가 SNI
  spoofing/domain fronting/ECH로 필터를 우회하는 것을 **완전히** 막는 것.
  이는 TLS 종단(MITM) 없이는 불가능하며 아래 §"실패·우회 계약"에 명시.

이 위협 모델을 스펙에 못 박는 이유: SNI 필터를 "완전한 L7 방화벽"으로
과대 포장하면 잔여 위험이 은폐된다. 정직한 계약은 "신뢰 워크로드의 의도된
egress를 강제하고 이탈을 감사한다"이다.

---

## Q1. SNI 추출 메커니즘 (핵심 난제)

TLS ClientHello의 SNI(`server_name` extension)를 host에서 매칭하는 세 방식.

| 축 | (a) `-m string` SNI 오프셋 anchor | (b) nftables raw payload/`@th` 파싱 | (c) userspace SNI 파서 (NFQUEUE) |
|---|---|---|---|
| **정확성** | 낮음. `--from/--to`로 검색 창을 좁혀도 SNI 오프셋은 cipher suite 목록·session id·compression·extension 순서에 따라 **가변**이라 고정 anchor 불가. 여전히 우연 매치·다른 필드 오탐 | 중간~불가. TLS record→handshake→ClientHello→extensions→server_name까지 전부 **가변 길이 필드**인데 nft는 루프/파서가 없어 `@th,off,len`으로 변동 오프셋을 따라갈 수 없다. ClientHello가 여러 TCP 세그먼트에 걸치면 완전 실패 | 높음. 실제 TLS 파서가 record/handshake 헤더를 읽고 SNI 필드를 정확히 추출. 세그먼트 재조립·wildcard suffix 매치·ECH outer/inner 구분 가능 |
| **복잡도** | 최저. 기존 `-m string` 규칙에 `--from/--to`만 추가. `planProfileEgressCommands`(`egress_policy.go:127-132`) 소폭 수정 | 높음. 손으로 쓴 취약한 nft 표현식. 유지보수 지옥, 테스트 곤란 | 중간. 신규 userspace verdict 루프(TLS 파서 ~200줄) + NFQUEUE dispatch 규칙. 나머지 배관은 재사용 |
| **실패 모드** | fragmentation·오탐 그대로 상속 → 현재 문서가 disavow한 semantics를 유지하게 됨 | 세그먼트 분할 시 조용히 통과 or 조용히 차단(둘 다 위험). 규칙 자체 버그 리스크 큼 | verdict 프로그램 crash/부재 시 fail-open/closed 정책 필요(→ OQ1). NFQUEUE steady-state 성능 비용(→ 아래 완화) |
| **anvil 코드 접점** | `egress_policy.go`만 | 신규 nft 규칙 체계(iptables-exec 추상화와 이질적) | dispatch 규칙은 기존 `egressCommand`(`egress_policy.go:24-27`) 재사용 + 신규 in-process verdict 루프 |

### 권고: (c) userspace SNI 파서 — 단, goose-daemon **in-process** NFQUEUE

**근거**: (b) 순수 nft는 가변 오프셋/멀티세그먼트 SNI를 견고하게 파싱할 수
없고(TLS는 nft가 다룰 수 없는 가변 길이 중첩 구조), (a)는 문서가 이미
부정한 substring 오탐/fragmentation semantics를 그대로 남긴다. SNI 필드를
**정확히** 읽고 **fail-closed** 가능한 유일한 방식은 실제 TLS 파서를
태우는 userspace verdict다.

**"신규 데몬/성능" 비용 완화** (evidence가 지적한 (c)의 약점):

1. **In-process**: 별도 systemd unit·패키지 의존을 만들지 않고, goose-daemon
   프로세스 안 goroutine으로 NFQUEUE consumer를 띄운다. 이미 daemon이
   `Manager`로 네트워크를 세팅하므로(`manager.go`) verdict 루프도 같은
   프로세스에 두는 것이 anvil의 "단일 바이너리 + in-process Manager" 패턴에
   맞다. Go netlink NFQUEUE 라이브러리(예: `go-nfqueue`)로 구현. → OQ3.
2. **슬로우패스 최소화**: NFQUEUE로 보내는 것은 **새 :443 흐름의 ClientHello가
   실린 첫 세그먼트뿐**이다. verdict가 ACCEPT면 conntrack mark를 찍고, 이후
   같은 흐름의 패킷은 `--ctstate ESTABLISHED` fast-path ACCEPT로 커널에서
   처리 — steady-state throughput은 커널 경로 그대로.
3. **dispatch는 기존 iptables(-nft) exec 재사용**: NFQUEUE target(`-j NFQUEUE
   --queue-num N`)은 배포 서버의 iptables-nft 백엔드에서 정상 동작하므로,
   dispatch 규칙을 기존 `egressCommand`/`commandEgressEnforcer` rollback
   기계(`api.go:1988-2043`)에 그대로 태운다. 진짜 신규는 verdict 루프 하나뿐.
   (egress 규칙 엔진 전체를 native `nft`로 재작성하는 것은 별개 리팩터 — 이
   스펙 범위 밖. → OQ4.)

> "host nftables"라는 사용자 표현은 **백엔드**(iptables-nft/nf_tables)를
> 뜻하며, dispatch 규칙을 native `nft` 문법으로 쓸지 iptables-exec로 쓸지는
> 하위 결정이다. 권고는 배관 재사용을 위해 iptables-exec 유지.

---

## Q2. Enforcement 모델

**권고: default-deny + SNI는 L3/L4 위에 얹는 추가층(대체 아님).**

현 `profile` 모델은 이미 default-deny다 — guest에 대해 base REJECT를 깔고
DNS/CIDR/host 예외를 `-I`로 그 앞에 삽입한다(`egress_policy.go:104-133`,
삽입 순서상 예외가 REJECT보다 먼저 평가됨). SNI 층은 이 구조에
**:443 TLS에 한정된 도메인-정밀 allow 경로**를 하나 더 추가한다:

- 새 :443 TCP 흐름의 ClientHello → NFQUEUE dispatch → verdict.
  SNI ∈ `allow_sni` → ACCEPT(+conntrack mark), 아니면 DROP.
- **:443이 아닌/비-TLS 트래픽은 SNI 층을 통과하지 않고** 기존 CIDR/DNS/포트
  allow + base REJECT가 그대로 통제한다.

즉 계층 관계는 **추가(additive)**다. SNI는 CIDR allowlist를 대체하지 않는다 —
CDN 뒤 도메인은 SNI로, 고정 IP 백엔드나 비-TLS 엔드포인트는 CIDR로, DNS는
`dns_servers`로. `allow_sni`는 CIDR로 표현할 수 없던 "이 도메인만" 의도를
:443에서 정밀하게 메운다. `EgressPolicy` enum(`tenant_policy.go:44-48`)은
무변경 — `profile` 값 하나가 이제 "CIDR + DNS + SNI"를 의미하도록 재정의만
하면 스케줄러 계약(`SelectRuntimeHost`, `tenant_policy.go:155-209`)은 그대로다.

---

## Q3. 실패·우회 모드 (명시 계약)

각 위협을 이 설계가 **어떻게 처리하고 무엇이 잔여 위험인지** 계약으로 못 박는다.

| 위협 | 이 설계의 처리 | 잔여 위험 / 계약 |
|---|---|---|
| **ECH/ESNI** (SNI 암호화) | ClientHello에 cleartext SNI가 없거나 decoy outer SNI만 있음. verdict는 **인식 가능한 allowlisted SNI가 없으면 DROP**(fail-closed, default-deny 일관) | anvil golden image는 ECH를 기본 활성화하지 않으므로 fail-closed 수용 가능. ECH 필요 엔드포인트는 outer(public-name) SNI를 allowlist하거나 CIDR fallback으로 명시 허용(→ OQ7). **anvil은 ECH를 무력화하지 않는다** — 파싱 불가 SNI는 deny로 계약 |
| **non-TLS** (plain HTTP:80, 임의 TCP) | SNI 없음 → NFQUEUE dispatch 대상 아님 → base REJECT로 떨어짐(CIDR/포트 allow 없으면 차단) | HTTP Host 헤더 검사는 L7 proxy 영역(비목표). :80은 CIDR로만 허용/차단됨을 계약 |
| **QUIC/UDP:443** | QUIC Initial에도 SNI가 있으나 포맷이 다름(v1 비목표). UDP:443은 allow 규칙이 없으면 base REJECT로 차단 | **UDP:443 기본 차단**을 계약(안전측). QUIC SNI 파싱은 후속 |
| **SNI spoofing** (가짜 SNI로 다른 IP 접속) | verdict는 SNI 문자열 기준 — guest가 `allow_sni` 값을 제시하며 SNI 무시하는 임의 서버로 갈 수 있음. `dns_servers`(신뢰 DNS 강제) + CIDR 핀으로 부분 완화 | SNI는 **guest-asserted**다. CIDR 핀 없이는 allowed SNI 값으로 임의 IP 터널링 가능. 위협 모델(신뢰 워크로드)상 수용, 계약으로 명시 |
| **domain fronting** (SNI ≠ 내부 Host) | verdict는 SNI만 봄. TLS 내부 Host는 암호문이라 안 보임 → 같은 CDN의 fronted 도메인 도달 가능 | TLS 종단 없이는 탐지 불가(비목표). 잔여 위험 계약 |

**핵심 계약 한 줄**: SNI 필터는 **신뢰 워크로드의 의도된 :443 egress를
강제하고 이탈을 감사**한다. 적대적 in-guest 루트에 대한 완전 봉쇄가 아니다.

---

## Q4. `egressProfile` 스키마 확장

**권고: 신규 `allow_sni []string` 필드 추가 (기존 `allow_hosts` 재해석 아님).**

```go
type egressProfile struct {
    AllowCIDRs []string `json:"allow_cidrs"`
    AllowHosts []string `json:"allow_hosts"` // legacy: -m string substring (deprecated)
    AllowSNI   []string `json:"allow_sni"`   // NEW: parsed ClientHello SNI, default-deny
    DNSServers []string `json:"dns_servers"`
}
```

**근거**:
- `allow_hosts`의 substring semantics는 이미 `egress_policy_test.go:16-21`이
  `-m string ... --algo bm` 명령으로 고정하고 있고, `EPHEMERA_/ANVIL_EGRESS_
  PROFILE_DIR`은 `docs/PUBLIC_RELEASE_BOUNDARY.md:214-215`에 "고정 런타임 계약"
  으로 등재된 표면이다. 같은 필드의 의미를 "substring"→"파싱된 SNI"로 조용히
  바꾸면 **기존 profile의 강제 semantics가 무언 변경**되고 테스트가 깨진다.
- 신규 필드는 **하위호환**: `allow_cidrs/allow_hosts/dns_servers`만 있는 기존
  profile은 무변경 동작. `allow_sni`는 opt-in.
- 검증은 `validateEgressHost`(`egress_policy.go:76-91`, ASCII 영숫자/`.`/`-`)
  재사용 가능(도메인명 문자셋 동일).
- **wildcard**: verdict가 실제 파서라 정밀 suffix 매치가 싸다. `*.example.com`
  = 왼쪽 한 개 이상 라벨 매치를 지원 권고(CDN 서브도메인 흔한 요구). exact가
  기본. 임의 glob은 비지원(YAGNI). → wildcard 깊이는 OQ5.
- `allow_hosts`(substring)는 문서에서 **legacy/deprecated**로 표기하고 신규
  profile은 `allow_sni`로 유도. 제거 시점은 OQ8.

---

## Q5. Anti-spoof / DNS와의 상호작용

- **ebtables L2 anti-spoof** (`antispoof.go:57-70`, source MAC/IP pin): SNI
  필터와 직교하지만 **load-bearing 의존**이다. verdict와 per-VM FORWARD 규칙은
  `-s guestIP`로 VM을 식별하는데, source-IP 스푸핑이 가능하면 다른 VM의
  allowlist로 오귀속될 수 있다. anti-spoof가 source IP를 핀하므로 `-s guestIP`
  선택이 올바른 VM에 대응함이 보장된다. → SNI 필터는 anti-spoof를 전제한다고
  계약(anti-spoof degrade 시 SNI 강제도 약화됨을 문서화).
- **DNS allowlist** (`dns_servers` → 신뢰 DNS 강제, `egress_policy.go:108-120`):
  SNI-spoofing 부분 완화의 defense-in-depth. 단 목적지 IP를 DNS 응답에 핀하지는
  않으므로(그건 별도 기능) 완전 완화는 아님. **인접 정합성 주의**: golden image
  resolv.conf가 `8.8.8.8`/`1.1.1.1` baked-in(`scripts/build_image.sh:119-124`)
  이므로 `dns_servers`가 이 둘을 포함하지 않으면 DNS 자체가 깨진다 — profile
  작성 시점에 해소해야 할 기존 이슈(SNI와 별개지만 결합 설계에서 재확인).
- **best-effort degrade 패턴**: `ebtablesAvailable()`(`manager.go:60-63`)처럼
  NFQUEUE/netlink 가용성을 체크해 예측 가능하게 degrade해야 한다. degrade
  방향(fail-open vs fail-closed)은 OQ1.

---

## Q6. 관측·감사

- **차단 로그**: verdict가 DROP하면 `{timestamp, vmID, tenant, sni, reason:
  egress_sni_denied}` 구조화 레코드. 기존 `RuntimeAuditRecord`
  (`tenant_policy.go:70-79`) 패턴 재사용. follow-up 문서가 이미 후보로 올린
  "profile별 deny reason"(`docs/operations/2026-05-29-...:177-202`)에 정합.
- **Redaction**: SNI 도메인은 **secret이 아님** — CIDR/host처럼 profile에 이미
  평문으로 들어가는 목적지 힌트이고 secret 저장 규약(`security-policy.md:120-123`)
  대상이 아니다. **로깅 가능**. 단 tenant 상관은 기존 redaction 규율 유지 —
  토큰/authorization은 절대 방출 금지(`tools.go:878-891`,
  `api.go:2593` `record.Error = "[redacted]"` 선례).
- **Metric**: profile별 allowed/denied :443 흐름 카운터. tenant×domain
  cross-product은 카디널리티/상관 누출 우려로 지양 — profile 단위 집계.

---

## 메커니즘 (규칙 배치·lifecycle)

per-VM 적용은 기존 `commandEgressEnforcer.ApplyWithProfile`
(`api.go:1959-2006`) 경로에 SNI 규칙을 **추가**한다 (spawn 시점
`api.go:1361-1370`, restore `api.go:3490`):

1. base REJECT + DNS/CIDR 예외: 현행 그대로(`egress_policy.go:104-126`).
2. **신규 dispatch**: `-I FORWARD -s guestIP -p tcp --dport 443 --ctstate NEW
   -j NFQUEUE --queue-num N --queue-bypass` (ClientHello 세그먼트를 verdict로).
3. **신규 fast-path**: `-I FORWARD -s guestIP -p tcp --dport 443 -m conntrack
   --ctstate ESTABLISHED -m mark --mark <approved> -j ACCEPT` (verdict가 mark 찍은
   흐름만 커널 fast-path). (정확한 mark/규칙 순서는 plan에서 확정.)
4. verdict 루프(in-process): NFQUEUE에서 ClientHello 파싱 → SNI ∈ `allow_sni`
   (wildcard 포함)면 conntrack mark + ACCEPT verdict, 아니면 DROP(+선택적 RST,
   → OQ6) + 감사 레코드.
- **lifecycle/rollback 재사용**: dispatch/fast-path 규칙은 `egressCommand`
  (`egress_policy.go:24-27`)로 표현해 `-I`↔`-D` 대칭 cleanup
  (`api.go:2028-2043`)과 vmID 키 rule map(`api.go:1999-2004`)에 그대로 태운다.
  verdict 루프의 per-VM allow_sni 매핑은 apply 시 등록, cleanup 시 제거.
- **삽입 순서**: 기존 규칙처럼 `-I`(head insert)라 daemon 기동 시
  `setupBridge`의 baseline ACCEPT(`manager.go:96-107`)보다 앞선다(evidence §1.3).

---

## 경계 사례

- **allow_sni 비었고 CIDR만 있는 profile**: :443은 dispatch되지만 allow_sni가
  비면 전량 DROP → 실수로 모든 TLS를 막을 수 있음. 계약: allow_sni 비어 있으면
  :443 default-deny(명시 문서화). CIDR-only egress를 원하면 dispatch 규칙을
  달지 않는 선택지 필요(→ profile에 SNI 미사용 시 dispatch skip).
- **ClientHello 없는 :443 흐름**(재개 세션 0-RTT, 비정상 TCP): SNI 추출 실패 →
  fail-closed DROP.
- **verdict 루프 crash/부재**: OQ1 결정에 따름. fail-closed면 모든 :443 차단
  위험, fail-open이면 조용한 미강제 — 둘 다 정직하게 계약 필요.
- **restore 경로**(`api.go:3490`): NFQUEUE 규칙 + verdict 매핑이 spawn과 동일
  하게 재설치돼야 함(idempotent).
- **다중 VM 동일 host**: queue-num을 VM별로 나눌지 단일 큐에서 `-s guestIP`로
  구분할지 — 단일 큐 + conntrack/소스 IP 매핑 권고(큐 고갈 방지).

---

## 테스트 (구현 시, TDD)

- **유닛**:
  - `allow_sni` 파싱/검증(`validateEgressHost` 재사용, ASCII/wildcard) 회귀.
  - `planProfileEgressCommands`가 allow_sni 존재 시 NFQUEUE dispatch +
    fast-path 명령을 정확히 생성(문자열 비교, 기존 `egress_policy_test.go`
    스타일). allow_sni 비면 dispatch 미생성.
  - verdict TLS 파서: 정상 ClientHello에서 SNI 추출, wildcard 매치, 세그먼트
    분할 재조립, SNI 부재→deny, decoy/ECH outer→deny.
  - enforcer apply/cleanup 대칭(`api_test.go:254-280` 패턴): SNI 규칙
    `-I`↔`-D` 역순 삭제.
- **KVM e2e**: allow_sni=`api.anthropic.com` profile로 spawn한 guest가 (1)
  허용 도메인 :443 성공, (2) 비허용 도메인 :443 실패, (3) 감사 레코드에 deny
  SNI 기록 — exit-code 판정(anvil e2e 규율).
- **성능 스모크**: 승인 흐름의 steady-state가 fast-path(커널)로 처리돼
  NFQUEUE 슬로우패스에 ClientHello만 도는지 확인(대량 전송 시 큐 미적체).

## 문서 반영 (구현 시)

- `docs/operations/security-policy.md:107-123`의 gap 서술을 SNI 필터 계약으로
  갱신(위협 모델·잔여 위험·ECH/fronting 한계 명시).
- `docs/architecture/multi-tenant-roadmap.md:294-300` 비목표 절 조정(SNI는
  이제 in-scope, 단 full HTTP CONNECT/proxy는 여전히 비목표).
- `egress.json` 스키마 문서에 `allow_sni`(+wildcard) 추가, `allow_hosts`
  deprecated 표기. `docs/PUBLIC_RELEASE_BOUNDARY.md` egress 표면 갱신.
- 전용 ADR 신설(현재 egress/network 전용 ADR 부재 — `docs/adr/`에 0001 1건뿐):
  substring→SNI 전환, 위협 모델, fail-open/closed 결정 기록.
- `CONTEXT.md:478`·`RELEASE_NOTES.md`·release-checklist의 반복 이월 항목 해소.

## 비목표 (명시)

- **전 트래픽 TLS 종단/MITM 복호화** — 신뢰 워크로드 위협 모델상 불필요하고,
  guest에 신뢰 CA 주입은 golden image·계약 변경이 과함(YAGNI).
- **ECH 완전 대응** — 파싱 불가 SNI는 fail-closed deny로 계약. outer SNI/CIDR
  fallback 이상은 안 함.
- **QUIC/UDP L7 SNI** — v1은 UDP:443 default-deny. QUIC Initial 파싱은 후속.
- **per-request HTTP Host 헤더 검사** — L7 forward proxy 영역(사용자가 이미
  proxy 방식을 배제). :80은 CIDR 통제만.
- **SNI↔cert↔IP 교차검증** — SNI-spoofing/fronting 완전 봉쇄는 MITM 필요, 위
  비목표에 종속.
- **egress 규칙 엔진의 native `nft` 전면 재작성** — dispatch는 기존
  iptables(-nft) exec 재사용. 엔진 마이그레이션은 별개 리팩터.
- **SNI 지원을 스케줄러 host capability 축으로 신설** — v1은 `profile` 지원
  host의 baseline 요구로 간주(→ OQ2).

---

## 결정 기록 (2026-07-13 설계 리뷰 — 전 항목 권고 확정)

초안의 미결 8건은 모두 권고안대로 확정됐다(위 상태 절 요약). 아래는 각 결정의 원 근거 보존.

### (구) 미결 질문 — 확정 근거

1. **OQ1 — degrade 방향**: NFQUEUE/verdict 부재 시 fail-open(:443 허용+로그) vs
   fail-closed(:443 차단). fail-closed가 보안 정직성엔 맞으나 커널 기능 부재로
   전 egress를 brick할 위험. **권고: fail-closed + 명시적 preflight 체크(기능
   없으면 spawn 거부)** — 조용한 미강제보다 낫다.
2. **OQ2 — host capability**: SNI 미지원 host에 `allow_sni` profile이 스케줄되면
   조용한 under-enforce. `RuntimeHost`에 capability 플래그 신설 vs 모든
   `profile` host의 baseline 요구. **권고: baseline 요구(v1) + 문서화.**
3. **OQ3 — verdict 위치**: goose-daemon in-process goroutine vs 별도 감독
   바이너리(systemd unit). **권고: in-process**(단일 바이너리 패턴, 신규 unit
   의존 없음). 격리/재시작 독립성이 필요하면 재고.
4. **OQ4 — dispatch 표현**: iptables(-nft) exec 재사용 vs native `nft` 규칙.
   **권고: iptables-exec 재사용**(rollback 배관 그대로).
5. **OQ5 — wildcard 깊이**: `*.example.com` 단일 접두 wildcard만 vs 미지원 vs
   더 깊은 패턴. **권고: 단일 `*.` 접두(한 개 이상 라벨) + exact. 임의 glob 없음.**
6. **OQ6 — deny 응답**: 조용한 DROP vs TCP RST 회신(빠른 실패로 guest 타임아웃
   단축). **권고: RST**(디버깅·UX). 단 fingerprinting 노출은 무시 가능 수준.
7. **OQ7 — ECH fallback 정책**: ECH 엔드포인트에 outer-SNI allowlist 허용할지,
   CIDR fallback만 허용할지, 아예 불허할지. **권고: CIDR fallback만**(명시
   opt-in) — outer SNI 신뢰는 약함.
8. **OQ8 — `allow_hosts` 폐기**: substring `allow_hosts`를 언제 제거할지(즉시
   deprecated만 vs 다음 major에서 제거 vs 유지). **권고: deprecated 표기 후
   유지, 신규는 allow_sni 유도.** 고정 런타임 계약 표면이라 제거는 별도 결정.
