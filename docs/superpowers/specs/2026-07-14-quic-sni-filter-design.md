# QUIC/UDP:443 SNI 필터 설계 (v1)

> **상태:** approved (brainstorming)
> **날짜:** 2026-07-14
> **대상:** anvil downstream repository (`internal/network/quic` 신규, `internal/network/sni`, `cmd/goose-daemon`)
> **선행:** [ADR-0002 egress SNI 필터](../../adr/0002-egress-sni-transparent-filter.md) (TCP:443 SNI slice)

**Goal:** `allow_sni` egress 정책을 UDP:443(QUIC/HTTP3)에도 적용한다 — 현재 UDP:443은 전면 default-deny다.

**Architecture:** QUIC Initial 패킷의 암호화된 CRYPTO 프레임을 복호(공개 DCID 파생 키)해 TLS ClientHello를 추출하고, 기존 SNI 매처/verdict 루프/dispatch 배관을 UDP로 확장 재사용한다. 신규 crypto는 자체 구현(stdlib + `x/crypto/hkdf`), 신규 direct 의존 0.

**Tech Stack:** Go, `crypto/aes`·`crypto/cipher`(AES-128-GCM, AES-ECB), `golang.org/x/crypto/hkdf`(이미 간접 의존), 기존 `go-nfqueue/v2` verdict 루프.

## Global Constraints

- **신규 direct 의존 0.** QUIC crypto는 stdlib + `x/crypto/hkdf`로 자체 구현. QUIC 라이브러리(quic-go 등) 도입 금지.
- **fail-closed 일관.** 복호 불가·미지원 버전·non-Initial·no-SNI·데이터그램 분할 등 모든 미확정 상태는 DROP(TCP slice와 동일 계약).
- **guest 무변경.** guest는 여전히 L3 라우팅만, L7 가시성 없음.
- **additive 계약 유지.** UDP:443 SNI는 CIDR/DNS를 대체하지 않는다. CIDR allow가 SNI dispatch보다 상위(TCP와 동일).
- **DOM 무관**(백엔드 전용).

---

## 맥락

TCP:443 SNI 필터(ADR-0002)는 cleartext TLS ClientHello를 NFQUEUE로 파싱해 `allow_sni`를 강제한다. 그러나 **UDP:443(QUIC/HTTP3)은 dispatch 대상이 아니라 base REJECT로 default-deny**된다 — allow_sni 도메인이 QUIC로는 도달 불가하다. HTTP/3 채택이 늘며 이는 실질 기능 간극이다.

QUIC Initial 패킷의 ClientHello는 **암호화**돼 있다. 단 그 키는 패킷의 **공개 Destination Connection ID(DCID)**에서 well-known salt로 파생되므로(RFC 9001 §5.2) 누구나 복호할 수 있다 — 기밀성이 아니라 ossification 방지용 obfuscation이다. 따라서 SNI를 얻는 유일한 경로는 Initial을 복호하는 것이고, 우회는 없다(대안은 QUIC 미지원 유지 또는 CIDR-only 허용뿐).

---

## 설계 결정 (brainstorming 확정)

1. **복호 자체 구현** (stdlib + `x/crypto/hkdf`). 최소-의존 기조 유지, 패킷 파싱만 하고 QUIC 상태머신 불필요라 상대적 소규모(~300줄). crypto 정확성은 RFC golden 벡터 유닛으로 담보.
2. **QUICv1 + QUICv2 지원.** version별 salt/label 분기, 복호 로직 공통. 미지원/알 수 없는 버전 → fail-closed deny.
3. **멀티-데이터그램 CRYPTO 재조립** (2026-07-14 개정 — 최초 "유닛-데이터그램" 결정을 뒤집음). **개정 사유**: Go 1.24+/Chrome/Firefox가 이제 **기본으로** post-quantum 하이브리드 키교환(X25519MLKEM768)을 쓰는데, 그 ClientHello(~1516B)는 QUIC Initial 1개(payload ~1162B)를 넘어 **2 Initial 데이터그램에 걸친다**. 최초의 "대다수 ClientHello는 첫 데이터그램에 들어감" 전제는 PQ-default 시대에 틀렸고, 유닛-데이터그램 한정은 현대 QUIC 대다수의 **허용** 트래픽을 fail-closed로 잘못 DENY한다(KVM e2e에서 실측 확인). 따라서 flow(srcIP:sport)별로 여러 Initial 데이터그램의 CRYPTO 프레임을 offset순 누적 재조립한다. 각 Initial 데이터그램은 같은 DCID→같은 키로 독립 복호(pn만 상이)되며, CRYPTO 청크를 offset 병합. ClientHello 완결 시 판정. **미완결 데이터그램은 drop(fail-closed)하되 reassembler는 CRYPTO 누적을 유지한다** — 완결 데이터그램이 flow의 first-accepted 패킷이 되어 그 connmark가 UDP conntrack 엔트리에 깨끗이 confirm되도록(미완결을 passthrough-accept하면 그 패킷이 mark 0으로 엔트리를 confirm해 완결 데이터그램의 connmark가 race에서 져 fast-path가 안 붙는다 — KVM e2e 실측). 클라는 dropped 데이터그램을 QUIC 손실복구로 retransmit하며, 그때 flow는 이미 allow+mark라 fast-path를 탄다. 부분 ClientHello가 서버에 도달하지 않아 strictly fail-closed. connectionless flow 상태는 **bounded LRU 캐시**(무한 성장 금지)로 유지.
4. **dispatch/fast-path**: 기존 queue 88 공유 + 프로토콜 분기. UDP conntrack flow에 connmark fast-path.
5. **deny 응답 = silent DROP.** UDP엔 RST가 없다. QUIC는 타임아웃 후 (브라우저 기준) TCP/HTTP2로 fallback → 그 흐름은 TCP:443 SNI 필터를 타 allow_sni면 허용된다. 자연스러운 degrade, 최소 코드.

---

## 컴포넌트 설계

### 1. `internal/network/quic` (신규 패키지)

**책임:** QUIC Initial 데이터그램 1개 → SNI 문자열 or fail-closed 신호. 순수 함수(root 불필요, 유닛 검증 가능).

```go
package quic

// ParseInitialSNI extracts the TLS SNI from a single QUIC Initial datagram
// (v1/v2). Returns the lowercased server_name, or an error. Every error is
// terminal (fail-closed): unsupported version, not an Initial, decrypt failure,
// CRYPTO reassembly incomplete within this datagram, or no SNI.
func ParseInitialSNI(datagram []byte) (string, error)
```

내부 단계 (`initial.go`, `crypto_frames.go`):
1. **long-header 파싱**: first byte(long-header bit 0x80 + packet-type=Initial), version(4B), DCID-len+DCID, SCID-len+SCID, token-len(varint)+token, length(varint), packet-number(protected). Initial이 아니거나 형식 오류 → error.
2. **키 파생** (RFC 9001 §5.2, RFC 9369 §3.3):
   - `initial_secret = HKDF-Extract(salt = version_salt, IKM = DCID)`
   - `client_secret = HKDF-Expand-Label(initial_secret, "client in", "", 32)`
   - `key = HKDF-Expand-Label(client_secret, <keyLabel>, "", 16)`, `iv = (<ivLabel>, 12)`, `hp = (<hpLabel>, 16)`
   - **version 상수 (구현 시 RFC 원문 대조 필수)**:
     - v1 (`0x00000001`): salt `0x38762cf7f55934b34d179ae6a4c80cadccbb7f0a`; labels `quic key`/`quic iv`/`quic hp`.
     - v2 (`0x6b3343cf`): salt `0x0dede3def700a6db819381be6e269dcbf9bd2ed9`; labels `quicv2 key`/`quicv2 iv`/`quicv2 hp`. (`client in`은 공통.)
   - HKDF-Expand-Label은 TLS 1.3 형식(RFC 8446 §7.1): label에 `tls13 ` 접두.
3. **header protection 제거** (RFC 9001 §5.4): 암호문에서 16B sample(PN offset+4)을 hp 키로 AES-ECB 암호화 → mask. first byte 하위비트·packet-number를 unmask.
4. **AEAD 복호** (AES-128-GCM): nonce = iv XOR (좌측 0-패딩한 packet number). AAD = 헤더(unprotect된 PN까지). 실패 → error.
5. **CRYPTO 프레임 재조립** (`crypto_frames.go`): 복호된 QUIC payload에서 CRYPTO 프레임(type `0x06`: offset varint, length varint, data) 수집, offset순 정렬·연결. PADDING(`0x00`)/PING(`0x01`)/ACK 등 다른 프레임은 skip. 재조립 바이트가 handshake 메시지(`0x01` ClientHello)로 완결되지 않으면(offset gap or 잘림) → `ErrIncompleteInDatagram`(terminal, deny).
6. **ClientHello 파싱**: 재조립 바이트를 `sni.ParseHandshakeSNI`(아래 리팩터)로 넘겨 SNI 추출.

coalesced 패킷: 하나의 UDP 데이터그램에 여러 QUIC 패킷이 이어붙을 수 있다(Initial + 0-RTT/Handshake). Initial 패킷들만 대상으로 CRYPTO를 모으고, 뒤따르는 non-Initial은 무시.

### 2. `internal/network/sni` 리팩터 (재사용)

현 `ParseClientHelloSNI(b)`는 TLS **record**(`0x16`)를 기대하고 record→handshake→SNI를 파싱한다. handshake-메시지 파싱부(`hs[0]==0x01` 이후: length/version/random/session_id/cipher_suites/compression/extensions→server_name)를 **`ParseHandshakeSNI(hs []byte) (string, error)`**로 추출한다.

- `ParseClientHelloSNI`(TLS 경로): record 헤더 검증 후 handshake 바이트를 `ParseHandshakeSNI`에 위임 — 동작 불변.
- `quic.ParseInitialSNI`(QUIC 경로): CRYPTO 재조립 바이트(= handshake 메시지)를 `ParseHandshakeSNI`에 직접 위임(record 없음).
- 기존 `parser_test.go`가 TLS 경로 회귀 가드. `ParseHandshakeSNI`에 bare-handshake 유닛 추가.
- import 방향: `quic` → `sni`(단방향, 사이클 없음). `sni`는 `quic`에 의존하지 않는다.

### 3. verdict 루프 UDP 확장 (`cmd/goose-daemon/sni_verdict.go`)

- **`parseIPv4UDP(pkt) (ipv4UDP, error)`** 추가: proto `IPPROTO_UDP` 검증, srcIP/sport/dport/payload 추출.
- Start 훅이 IP proto byte(`pkt[9]`)로 분기:
  - TCP → 기존 TLS 경로(`sni.Reassembler`/`ParseClientHelloSNI`).
  - UDP & dport 443 → QUIC 경로: `quic.ParseInitialSNI(udp.payload)`.
- **QUIC decide**: flow(srcIP:sport)별 `quic.InitialReassembler`를 bounded-LRU 캐시에서 얻어 데이터그램을 Feed. 미등록 srcIP → drop(`unregistered_source`); ClientHello **완결** & SNI ∈ matcher → `sniAcceptMark`(connmark, flow evict); ∉ → drop(`egress_sni_denied`, flow evict); ClientHello **미완결**(더 필요) → **drop**(`egress_sni_incomplete`, flow 유지 — reassembler는 누적, 패킷만 drop; connmark-race 회피, 클라 retransmit); 복호 실패·미지원·오버사이즈·overlap 모순 → drop(`egress_sni_unparsed`, flow evict). **UDP는 RST 없음 → drop이 silent DROP**(injectRST는 TCP 전용). decideQUIC은 sniPassthrough를 내지 않는다(TCP 경로와 상이).
- fast-path: `SetVerdictWithConnMark`가 UDP conntrack flow에 mark → 후속 QUIC 패킷은 UDP connmark ACCEPT 규칙이 커널 처리. TCP와 동일 connmark(`0x534e49`).
- metric/audit: `recordVerdict` 재사용. deny → `egress_sni_denied` audit + metric. (proto 구분은 v1 비목표 — 동일 outcome. 필요 시 후속.)
- **멀티-데이터그램 재조립**: `quic.InitialReassembler`가 flow별로 여러 Initial 데이터그램의 CRYPTO를 offset 누적. 완결 전 데이터그램은 **drop(fail-closed)하되 CRYPTO는 누적**(connmark-race 회피 + 부분 ClientHello 미전달; 클라 retransmit이 완결 후 fast-path를 탄다). TCP의 passthrough와 다른 이유는 QUIC Initial이 UDP conntrack flow의 first 패킷이기 때문(TCP는 ClientHello가 SYN 이후라 이미 confirm된 엔트리에 mark). bounded LRU flow cap(`sniReassemblerMaxFlows` 재사용) + per-flow 바이트 상한(오버사이즈 → drop). 판정/실패 시 flow evict, 미완결 drop 시 flow 유지.

### 4. dispatch (`cmd/goose-daemon/egress_policy.go`)

`planProfileEgressCommands`가 `len(AllowSNI)>0`일 때 TCP:443 규칙과 **대칭으로 UDP:443** 규칙을 추가한다:
```
-I FORWARD -s <guestIP> -p udp --dport 443 -m connmark ! --mark 0x534e49 -j NFQUEUE --queue-num <q> ... --comment <prefix>-sni-udp-nfqueue
-I FORWARD -s <guestIP> -p udp --dport 443 -m connmark   --mark 0x534e49 -j ACCEPT              ... --comment <prefix>-sni-udp-fastpath
```
- 같은 queue(`sniQueueNum()`), 같은 connmark. TCP 규칙 블록 옆(같은 순서 계약: fastpath가 nfqueue 위, 둘 다 CIDR 아래).
- `allow_sni` 비면 UDP:443 규칙도 미생성 → 기존 UDP:443 default-deny 하위호환.
- rollback: 기존 `-I`↔`-D` 대칭 배관 그대로. `flushEgressByComment`의 `anvil-egress-<vmID>-` prefix가 `-sni-udp-*`도 회수.

---

## Data Flow

```
guest UDP:443 QUIC Initial
  → iptables -p udp --dport 443 -m connmark ! --mark → NFQUEUE 88
  → sni_verdict Start hook: parseIPv4UDP → proto=UDP,dport=443
  → quic.ParseInitialSNI(payload):
       long-header 파싱 → version별 키 파생 → HP 제거 → AES-GCM 복호
       → CRYPTO 재조립 → sni.ParseHandshakeSNI → SNI
  → decide: SNI ∈ matcher?
       yes → SetVerdictWithConnMark(ACCEPT+0x534e49); 후속 패킷 UDP connmark fastpath
       no/실패 → NfDrop(silent); recordVerdict(egress_sni_denied) audit+metric
```

---

## Fail-closed / 잔여 위험 계약 (ADR-0002 표에 QUIC 행 추가 예정)

| 상황 | 처리 |
|---|---|
| 미지원/알 수 없는 QUIC 버전 | 키 파생 불가 → **DROP** |
| non-Initial(Handshake/1-RTT/Retry/VN) 첫 패킷 | ClientHello 없음 → **DROP** |
| header protection/AEAD 복호 실패 | **DROP** |
| CRYPTO가 여러 Initial 데이터그램에 걸침(PQ ClientHello 등) | flow별 재조립으로 **누적**; 완결 전 데이터그램은 drop(fail-closed, CRYPTO 누적 유지, 클라 retransmit). per-flow 바이트 상한 초과 → **DROP** |
| ClientHello에 SNI 없음 | **DROP** |
| SNI ∈ allow_sni | ACCEPT + connmark |
| SNI ∉ allow_sni | **DROP** + audit |

- **SNI는 guest-asserted**(TCP와 동일 잔여위험) — CIDR 핀 없이는 allowed SNI로 임의 IP 터널 가능. 신뢰 워크로드 모델상 수용.
- **0-RTT**: Initial이 차단되면 handshake 미성립이라 0-RTT 성립 불가. migration은 handshake 후(이미 mark)라 무관.
- **QUIC 버전 변화**: 새 버전 등장 시 salt/label 추가 전까지 fail-closed deny(안전측).

---

## Testing 전략

**유닛 (root 불필요):**
- `internal/network/quic`: **RFC 9001 Appendix A(v1) + RFC 9369 Appendix A(v2)의 golden Initial 패킷 바이트**로 복호→ClientHello→SNI 검증(crypto 정확성 담보). + malformed(short/bad-version/non-Initial)·no-SNI·데이터그램-분할·CRYPTO offset gap → 각각 terminal error. fuzz(panic-free 경계).
- `internal/network/sni`: `ParseHandshakeSNI` bare-handshake 유닛 + 기존 `ParseClientHelloSNI` 회귀(리팩터 동작 불변).
- `cmd/goose-daemon`: UDP decide 라우팅(미등록/allowed/denied/미지원-버전 fail-closed) 유닛; `planProfileEgressCommands`가 UDP:443 dispatch/fastpath 대칭 생성 + empty시 미생성 유닛.

**KVM e2e** (`scripts/anvil-egress-sni-e2e.sh` 확장 또는 신규):
- guest에서 HTTP/3 가능 클라이언트(`curl --http3` 또는 Go QUIC 클라)로 **허용 도메인(HTTP/3 지원, 예 `cloudflare.com`)** :443 UDP 도달 성공 + `iptables -S`에 `-sni-udp-*` 규칙·connmark 확인.
- **비허용 도메인** QUIC → 차단(타임아웃/미도달) 확인.
- audit에 `egress_sni_denied` + redaction. (verdict 루프 netlink/UDP 커널 경로는 root+KVM 필요라 이 e2e가 유일 실검.)

---

## Scope / 비목표 (YAGNI)

- **비목표**: 0-RTT 파싱, QUIC 상태머신/handshake 추적, connection migration 추적, QUICv1/v2 외 버전, DNS-over-QUIC(:853) 등 non-:443. 전부 fail-closed deny로 안전 처리. (멀티-데이터그램 재조립은 2026-07-14 개정으로 **in-scope** — PQ-default 대응.)
- **후속 후보**: proto별 metric label, 새 QUIC 버전 salt/label 추가, 매우 큰 ClientHello(3+ 데이터그램) 한계 재검토.

---

## 미결/구현 시 확인

1. **QUIC 상수 정확성**: v1/v2 salt·label·version number를 RFC 9001/9369 원문과 golden 벡터로 반드시 대조(위 값은 근거이나 구현 시 검증).
2. **e2e QUIC 클라이언트**: golden image에 HTTP/3 curl 또는 Go QUIC 클라 가용성 확인. 없으면 e2e에서 QUIC 클라 준비 절차 추가.
3. **UDP conntrack connmark**: UDP flow에 `SetVerdictWithConnMark`가 실제로 conntrack 엔트리에 mark를 유지하고 후속 패킷이 fastpath ACCEPT됨을 KVM e2e에서 실증(TCP와 동일 원리이나 UDP 타임아웃 특성 확인).
