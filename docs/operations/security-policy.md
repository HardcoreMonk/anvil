# anvil 보안 정책

## 공개 노출

`goose-daemon`은 TLS를 종료하는 reverse proxy 뒤에서만 공개 운영한다. daemon
자체는 기본적으로 HTTP control plane을 제공하므로, 인터넷 또는 팀 공용 network에
직접 bind하지 않는다.

운영 배포의 외부 경계는 다음 구조를 기준으로 한다.

```text
client
  -> HTTPS reverse proxy
  -> HTTP 127.0.0.1:3000 또는 private host network의 goose-daemon
```

reverse proxy는 TLS 인증서, 외부 access log, allowlist/rate limit 같은 공개 노출
정책을 담당한다. daemon은 VM lifecycle, snapshot lifecycle, guest agent proxy,
control-plane Bearer token 검증을 담당한다.

운영 환경은 `EPHEMERA_API_TOKENS`를 설정해야 한다. control-plane token이 없는 인증
비활성 로컬 전용 모드는 개발과 host-local smoke test 용도이며 공개 노출 용도가
아니다.

## 제어 평면 token 정책

control-plane token의 기준 설정은 `EPHEMERA_API_TOKENS`다. 호환 alias로
`ANVIL_API_TOKENS`, `EPHEMERA_API_TOKEN`, `ANVIL_API_TOKEN`을 인식한다. 실제 token
값은 문서, 채팅, 커밋 메시지, 테스트 fixture, release note에 남기지 않는다.

운영에서는 client 이름이 있는 다중 token 형식을 우선한다.

```bash
EPHEMERA_API_TOKENS="operator:$TOKEN,ci:$CI_TOKEN" ./anvil-daemon
```

단일 token alias는 기존 배포 호환을 위한 경로다. 새 운영 설정은
`EPHEMERA_API_TOKENS`를 사용한다.

## 게스트 agent token 정책

guest agent token은 VM마다 생성된다. daemon은 이 token을 guest disk에 주입하고,
control plane proxy가 guest agent 호출에 내부적으로 사용한다. 외부 client는 guest
agent token이 아니라 control-plane token으로 daemon에 인증한다. 보안 불변 조건상
`agent_token`은 `POST /vms` 응답 외에는 노출하지 않아야 한다.

다음 출력에는 정책상 `agent_token`이 나오면 안 된다.

- snapshot 생성, 목록, restore, delete 응답
- snapshot GC dry-run/apply 응답
- `POST /flocks`, `GET /flocks`, `GET /flocks/{flock_id}` 응답
- `snapshots/gc-audit.jsonl`
- MCP tool output
- 문서, audit log, test fixture

daemon의 restore 응답, flock 응답, MCP output은 `agent_token`을 노출하지 않는다.
운영자가 과거 로그나 오래된 test fixture를 공유해야 할 때는 legacy restore/flock
body에 token이 남아 있지 않은지 확인한다.

## 게스트 control-plane token 주입 (per-flock 능력 토큰)

flock member VM 안의 agent는 host daemon으로 되돌아오는 호출(`gtwall`의
`POST /flocks/{id}/post`, `gtcall`의 `POST /flocks/{id}/call`)을
`/root/.ephemera-cp-token`에 주입된 bearer로 인증한다.

이 자리에 주입되는 값은 **그 flock의 per-flock guest 능력 토큰**이다 — 운영자
control-plane bearer가 아니다. 두 flock kind가 같은 모델을 쓴다: routed flock은
adapter가 발급한 `relay_token`을, local flock은 daemon이 flock 생성 시 발급한
능력 토큰을 받는다. `authMiddleware`는 요청 경로에서 flock id를 뽑아 **그
flock의** 토큰과만 비교하므로, 이 토큰은 해당 flock의 wall
sub-path(`post|wall|wall/history`)와 `call` 진입만 admit하고 다른 flock의 같은
경로나 어떤 control-plane 경로(`/vms`, `/config/*`, `/tenants`, `/snapshots`)도
열지 않는다. 결정 근거는
[ADR-0003](../adr/0003-per-flock-guest-capability-tokens.md).

운영상 알아야 할 것:

- local flock의 능력 토큰은 flock 디렉토리 안 `guest-token` 파일에 **0600**으로
  영속되고, daemon 시작 시 `metadata.json` 복구 직후 admission에 되꽂힌다.
  `metadata.json`에는 어떤 토큰도 기록되지 않는다.
- **폐기 수단은 flock 삭제 하나뿐이다.** 능력 토큰에는 만료가 없고 개별 회전
  경로도 없다(routed flock의 `relay_token`과 동일 규율). flock을 지우면
  admission과 토큰 파일이 함께 제거된다.
- **SIGHUP 운영자 토큰 회전은 flock member의 능력 토큰을 건드리지 않는다.**
  회전 fan-out은 daemon이 자기 운영자 bearer를 주입했던 VM만 대상으로 하며,
  능력 토큰 도입 이전에 spawn된 VM만 거기 해당한다. 그런 구세대 VM은 계속
  회전을 받고, 교체되면서 대상 집합이 비워진다.
- 토큰 발급/영속에 실패하면 member는 **빈 토큰**으로 spawn된다(cp-token 파일이
  아예 기록되지 않는다). 운영자 bearer로 폴백하지 않는다. 이 경우 해당 member의
  `gtwall`/`gtcall`이 401을 받으므로 Error 로그를 확인하고
  `POST /flocks/{id}/agents/{agent_id}/restart`로 재주입한다.
- API auth가 비활성(`EPHEMERA_API_TOKENS` 미설정)이면 능력 토큰을 발급하지
  않는다 — admission 자체가 무의미한 개발 모드이며, 이는 종전 동작과 같다.

## 로컬 secret

`configs/goose-secrets.yaml`과 profile별 secrets 파일은 local secret이다. 예시는
커밋할 수 있지만 실제 secret 파일은 커밋하지 않는다.

커밋 금지 대상:

- `configs/goose-secrets.yaml`
- `configs/profiles/*/goose-secrets.yaml`
- 실제 LLM API key 또는 provider token이 들어간 임시 fixture
- 실제 token 값을 포함한 shell history, terminal transcript, release note, issue,
  PR description, commit message

운영 절차 문서에는 `$TOKEN`, `$CI_TOKEN`, `$SNAPSHOT_ID`, `$VM_ID` 같은 placeholder만
사용한다.

## Snapshot metadata 반출 정책

snapshot `metadata.json`에는 `agent_token`이 들어 있다. metadata 반출 또는 백업
산출물이 신뢰된 host 경계 밖으로 나가기 전에는 반드시 scrubber로 token을 제거한다.

```bash
go run ./scripts/snapshot-metadata-scrub.go -input snapshots/snap-.../metadata.json > metadata.scrubbed.json
```

신뢰된 host 경계 밖에는 off-host backup, support bundle, object storage, 외부 ticket,
채팅 업로드, release artifact가 포함된다. 원본 snapshot directory 전체를 외부로
복사해야 하는 운영 절차는 아직 승인된 표준 절차가 아니다. 필요한 경우 먼저
`metadata.json`을 scrub한 별도 산출물을 만들고, 원본 metadata가 포함되지 않았는지
검사한다.

snapshot GC audit은 metadata 전체나 `agent_token`을 기록하지 않는다.

## Town Wall message 정책

Goosetown Town Wall은 flock별 coordination log다. `POST /flocks/{flock_id}/post`,
`GET /flocks/{flock_id}/wall/history`, SSE stream, VM 내부 `gtwall` helper가 같은
append-only log를 사용한다.

Town Wall message body는 사용자/agent가 제공한 내용이며
`flocks/<flock_id>/TOWN_WALL.log`와 history 응답에 남는다. 따라서 provider API key,
Bearer token, `agent_token`, 내부 credential, 고객 PII를 게시하지 않는다.

MCP runtime audit은 `anvil_post_townwall` 호출 사실과 daemon operation만 기록하고
Town Wall body는 저장하지 않는다. 하지만 Town Wall 자체는 body를 보존하므로 audit
redaction을 secret 저장소로 오해하지 않는다.

## Egress policy

`egress_policy`는 `deny_all`, `profile`, `allow_all` 중 하나다. daemon은 선택된
policy를 VM/snapshot/restore metadata에 보존하고, host-local network rule 적용에
사용한다.

- `deny_all`: guest IP 기준 `iptables FORWARD` reject rule을 적용한다.
- `profile`: `configs/profiles/{profile}/egress.json`,
  `EPHEMERA_EGRESS_PROFILE_DIR`, `ANVIL_EGRESS_PROFILE_DIR` 아래의 profile별
  `egress.json`이 있으면 allow CIDR, `allow_sni` transparent SNI 필터(아래), DNS
  server allowlist와 default reject rule을 적용한다.
- `allow_all`: 기존 NAT outbound 동작을 유지한다.

`egress.json`은 secret 저장소가 아니다. provider API key, Bearer token, 내부
credential을 넣지 않는다. packet string match(`-m string --algo bm`) 기반이던
`allow_hosts` rule은 제거됐다. 잔존 key는 값이 empty/`null`이어도 profile load를
실패시키며 값은 error에 반복하지 않는다. domain은 `allow_sni`, 고정 IP/CIDR은
`allow_cidrs`로 옮기고 key를 삭제한다. policy 파일이 없는 `profile`은 기존 profile
호환성을 위해 no-op이다.

### `allow_sni` — transparent SNI 필터 (ADR-0002)

`allow_sni []string`은 실제 파싱된 TLS ClientHello의 `server_name`
extension을 :443 새 TCP 흐름 단위로 강제하며 `allow_cidrs`/`dns_servers`와 병렬로
동작한다. exact match가 기본이고 `*.example.com` 형태로 leading label
wildcard(한 개 이상 라벨)를 지원한다 — 임의 위치 glob은 비지원.

**메커니즘**: :443 새 흐름의 ClientHello 세그먼트가
`iptables -j NFQUEUE --queue-num 88`(env `ANVIL_SNI_QUEUE_NUM`로 override)로
goose-daemon의 **in-process** verdict 루프에 dispatch된다. 루프
(`github.com/florianl/go-nfqueue/v2`)가 SNI를 파싱해 `allow_sni` 매처와
대조한다 — 허용이면 흐름의 conntrack 엔트리에 승인 mark(`0x534e49`)를 찍고
이후 패킷은 커널 fast-path로 ACCEPT된다(NFQUEUE 슬로우패스는 최초
ClientHello 세그먼트에만 탄다). 비허용/파싱 불가는 **fail-closed
DROP**(+best-effort TCP RST로 guest 빠른 실패)이다.

**UDP:443(QUIC/HTTP3) 확장 (2026-07-14)**: 같은 queue 88·같은 connmark를
UDP:443에도 적용한다. 새 QUIC 흐름의 Initial 패킷이
`iptables -p udp --dport 443 -j NFQUEUE`로 같은 verdict 루프에 dispatch되면,
루프가 공개 Destination Connection ID에서 파생한 키(HKDF+AES-128-GCM+header
protection, 자체 구현 `internal/network/quic`, 신규 direct 의존
`golang.org/x/crypto` 하나)로 Initial을 복호해 CRYPTO 프레임에서 TLS
ClientHello를 얻고 같은 `allow_sni` 매처와 대조한다. QUICv1
(`0x00000001`)+QUICv2(`0x6b3343cf`)를 지원하며, 미지원 버전은 fail-closed
deny다. 현대 브라우저/Go 1.24+가 기본으로 쓰는 post-quantum
(X25519MLKEM768) ClientHello(~1516B)는 Initial 데이터그램 2개에 걸치므로,
flow(`srcIP:sport`)별 bounded-LRU reassembler가 CRYPTO를 여러 데이터그램에
걸쳐 offset 누적한다 — **완결되지 않은 데이터그램은 drop(fail-closed)하되
CRYPTO는 누적을 유지**한다(완결 데이터그램이 flow의 first-accepted
패킷이어야 connmark가 conntrack에 깨끗이 confirm된다; 미완결을
passthrough-accept하면 mark 0으로 엔트리가 먼저 confirm돼 완결 데이터그램의
connmark 적용이 race에서 진다). 클라는 dropped 데이터그램을 QUIC
손실복구로 retransmit하며, 재전송이 도달할 때는 이미 flow가 allow+mark라
fast-path를 탄다. per-flow 바이트 상한(8192B)+flow-count LRU(4096)로 상태를
bound한다. **UDP엔 RST가 없어 deny 응답은 silent DROP이다** — QUIC
타임아웃 후 브라우저가 TCP/HTTP2로 fallback하면 그 흐름은 TCP:443 SNI
필터를 타 `allow_sni`면 허용된다(자연 degrade). 재조립은 데이터그램 수에
하드 제한이 없다 — 실질 상한은 per-flow 8192B 캡(≈7 데이터그램)이며 이를
초과하는 ClientHello만 fail-closed deny(주류 클라 미해당). 상세는
[ADR-0002](../adr/0002-egress-sni-transparent-filter.md)의
"메커니즘 확장 — UDP:443 QUIC/HTTP3" 절 참조.

**fail-closed 계약**: `--queue-bypass`(fail-open 플래그)는 명시적으로 배제한다
— verdict 루프가 죽거나 리스너가 없으면 커널이 큐에 들어간 패킷을 그냥
DROP한다. 이에 더해 daemon은 **preflight**로, `allow_sni` profile인데
verdict 루프가 준비되지 않은 host면 iptables 규칙을 하나도 깔지 않고 VM
spawn 자체를 거부한다("규칙은 있는데 검사기가 없는" 조용한 fail-open 상태를
원천 차단). `profile` egress를 지원하는 모든 host는 NFQUEUE 사용 가능이
baseline 요구다(확인 절차는 `docs/operations/runbook.md`).

**additive 순서 계약**: SNI는 CIDR allowlist를 대체하지 않는다 — `:443`
목적지가 `allow_cidrs`에 있으면 CIDR allow 규칙이 SNI 검사보다 위에서
평가되어 SNI 판정 없이 ACCEPT된다(명시 IP 신뢰가 도메인 검사보다 우선).
CDN 뒤 도메인은 SNI로, 고정 IP 백엔드/비-TLS 엔드포인트는 CIDR로, DNS는
`dns_servers`로 통제한다.

**위협 모델과 잔여 위험**: anvil guest는 신뢰된 golden-image 워크로드이지,
루트를 쥔 적대적 사용자가 host를 공격하는 환경이 아니다. **핵심 계약 한 줄**:
SNI 필터는 신뢰 워크로드의 의도된 :443 egress를 강제·감사한다. 적대적 in-guest
루트에 대한 완전 봉쇄가 아니다. 알려진 잔여 위험:

- **ECH/ESNI**: anvil은 outer(공개/cover) SNI만 관측한다. outer가 없거나
  비허용 decoy면 allowlisted SNI가 없어 fail-closed DROP된다. **단 outer가
  allowlisted 도메인(ECH 공개 이름)이면 flow는 허용되고 암호화된 inner
  목적지는 은닉된다** — guest-asserted SNI와 동일 신뢰등급 잔여다. anvil은
  ECH를 무력화하지 않으며(inner는 서버 키 없이 복호 불가), 완화는 해당
  엔드포인트 CIDR fallback 핀뿐이다(outer-SNI allowlist는 신뢰 근거 아님).
  **2026-07-18부로 이 잔여 케이스는 최소한 관측 가능하다**: 허용된 flow의
  ClientHello가 ECH를 담으면 `ephemera_egress_sni_ech_observed_total{proto}`가
  증가한다(+content-free `slog.Info`) — 관측 전용이며 allow/deny verdict는
  그대로다.
- **non-TLS**(HTTP:80, 임의 TCP): SNI가 없어 NFQUEUE 대상이 아니다 — 기존
  base REJECT + CIDR만 통제한다.
- **QUIC/UDP:443**: 2026-07-14부터 구현됨(위 "UDP:443(QUIC/HTTP3) 확장"
  절). SNI는 TCP와 동일하게 guest-asserted다. 재조립은 데이터그램 수에 하드
  제한이 없다 — 실질 상한은 per-flow 8192B 캡(≈7 데이터그램)이며, 이를 초과하는
  ClientHello만 fail-closed deny(안전측, 주류 클라 미해당).
- **SNI spoofing**: SNI는 guest-asserted다. CIDR 핀 없이는 allowed SNI
  값을 제시하며 실제로는 다른 IP로 터널링할 수 있다. `dns_servers` 강제로
  부분 완화하지만 목적지 IP를 DNS 응답에 핀하지는 않는다.
- **domain fronting**: SNI ≠ 내부 Host는 TLS 종단 없이 탐지 불가하다.
- **pre-decision 부분 ClientHello 전달**: 멀티세그먼트 ClientHello에서
  아직 완결되지 않은 세그먼트는 판정 전에 unmarked ACCEPT로 통과하고, 다음
  세그먼트가 재조립을 이어간다(재조립 테이블이 가득 차 LRU eviction되면
  다음 세그먼트는 새 reassembler로 시작해 fail-closed DROP한다 — never
  fail-open). 승인 conntrack mark는 오직 완결된 ClientHello의 positive SNI
  매치에서만 찍히므로 이 전달 자체는 승인 누수가 아니다. 다만 흐름당 16 KiB
  재조립 버퍼는 하드 캡이 아니다 — eviction 후 새 인스턴스가 다시 시작되므로
  세그먼트를 의도적으로 지연시키는 적대자는 개별 세그먼트의 반복 통과를
  TCP 자체의 hiccup 속도로만 rate-limit받는다. 완전 봉쇄는 hold-then-decide
  재설계가 필요하며 v1은 채택하지 않는다.

anti-spoof(`EPHEMERA_NET_ANTISPOOF`, source MAC/IP pin)는 SNI 필터와 직교하지만
load-bearing 전제다 — verdict/규칙이 `-s guestIP`로 VM을 식별하므로 anti-spoof가
degrade되면 SNI 강제도 함께 약화된다. `dns_servers`는 golden image
`resolv.conf`가 baked-in한 `8.8.8.8`/`1.1.1.1`을 포함해야 DNS 자체가 깨지지
않는다.

전체 결정 기록·잔여 위험 표·경계 사례는
[`docs/adr/0002-egress-sni-transparent-filter.md`](../adr/0002-egress-sni-transparent-filter.md)를
참조한다.

## Audit, metrics, trace redaction

runtime audit API, snapshot GC audit, `/metrics`, `/metrics/vms`, optional trace
export는 daemon raw body, snapshot metadata 전체, secret, `agent_token`을 기록하지
않는다. trace exporter는 attribute key/value에서 token, secret, authorization 계열
값을 제거한 뒤 `{endpoint}/v1/traces`로 전송한다.

## 운영 점검 기준

- 공개 endpoint는 TLS reverse proxy 뒤에 있는가.
- 운영 daemon에 `EPHEMERA_API_TOKENS`가 설정되어 있는가.
- `POST /vms` 외 응답과 MCP output에 `agent_token`이 없는가.
- `deny_all` 또는 `profile` egress policy를 쓰는 profile의 `egress.json`이 의도한
  CIDR, host, DNS server만 허용하는가.
- runtime audit, metrics, trace export에 token/secret/metadata raw body가 없는가.
- Town Wall message에 token, provider secret, 고객 PII가 들어가지 않았는가.
- snapshot metadata를 host 밖으로 내보내기 전에 scrub했는가.
- `configs/goose-secrets.yaml`과 profile secrets가 git에 들어가지 않았는가.

## 제한 사항

이미 생성된 snapshot의 token 회전은 아직 구현되어 있지 않다.
