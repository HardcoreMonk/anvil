# 2026-07-14 QUIC/UDP:443 SNI 필터 확장 handoff

TCP:443 transparent SNI 필터(ADR-0002, 2026-07-13)를 UDP:443(QUIC/HTTP3)으로
확장하는 slice의 구현·검증 기록. branch `feature/quic-sni-filter`. 설계 근거는
[design spec](../superpowers/specs/2026-07-14-quic-sni-filter-design.md),
결정 원문은 [ADR-0002](../adr/0002-egress-sni-transparent-filter.md)의
"메커니즘 확장 — UDP:443 QUIC/HTTP3" 절과 잔여 위험 표 QUIC 행에 있다 — 이
handoff는 그 둘을 압축 요약하고 검증 증거·Follow-Up만 추가한다.

## 무엇이 main에 있나 (PR 대기, main 아님 — feature 브랜치)

- **신규 패키지 `internal/network/quic`**: QUIC Initial 데이터그램에서 SNI를
  추출하는 순수 함수 계층(root 불필요, 유닛 검증 가능). 공개 Destination
  Connection ID(DCID)에서 version별 salt로 파생한 키로 HKDF-Extract/
  Expand-Label(RFC 9001 §5.2, RFC 9369 §3.3, TLS 1.3 label 형식 RFC 8446
  §7.1) → AES-128-GCM key/iv/hp를 얻고, header protection을 AES-ECB sample
  mask로 제거(RFC 9001 §5.4)한 뒤 AEAD로 payload를 복호한다. **QUICv1
  (`0x00000001`, salt `38762cf7...`, label `quic key`/`quic iv`/`quic hp`)
  + QUICv2(`0x6b3343cf`, salt `0dede3de...`, label `quicv2 key`/`quicv2 iv`/
  `quicv2 hp`)** 지원, 미지원/알 수 없는 버전은 즉시 fail-closed error.
  복호된 payload에서 CRYPTO 프레임(type `0x06`)을 offset순 재조립해
  handshake 메시지(`0x01` ClientHello)로 완결되면 `sni.ParseHandshakeSNI`
  (아래 리팩터)로 SNI를 추출한다. coalesced 패킷(Initial + 0-RTT/Handshake)은
  선행 Initial만 대상으로 하고 뒤따르는 non-Initial은 무시한다.
- **`internal/network/sni` 리팩터(재사용, 동작 불변)**: 기존
  `ParseClientHelloSNI`의 handshake-메시지 파싱부(`hs[0]==0x01` 이후)를
  `ParseHandshakeSNI(hs []byte) (string, error)`로 추출했다.
  `ParseClientHelloSNI`(TLS 경로)는 record 헤더 검증 후 이 함수에 위임하고,
  `quic.ParseInitialSNI`(QUIC 경로)는 CRYPTO 재조립 바이트(record 없음)를
  직접 위임한다. import 방향은 `quic` → `sni` 단방향(사이클 없음). 기존
  `parser_test.go`가 TLS 경로 회귀를 그대로 가드한다.
- **멀티-데이터그램 재조립 (`quic.InitialReassembler`, 2026-07-14 설계
  개정)**: 최초 설계는 "ClientHello는 대부분 첫 Initial 데이터그램에 들어감"
  전제로 유닛-데이터그램만 지원했으나, KVM e2e에서 Go 1.24+/Chrome/Firefox의
  **기본** post-quantum(X25519MLKEM768) ClientHello(~1516B)가 QUIC Initial
  페이로드 1개(~1162B)를 넘어 **2개 데이터그램에 걸침**을 실측해, 유닛-
  데이터그램 한정이 현대 QUIC 대다수의 **허용** 트래픽을 fail-closed로 오탐
  DENY함을 확인했다. flow(`srcIP:sport`)별로 여러 Initial의 CRYPTO를 offset
  순 누적하도록 설계를 개정했다(각 데이터그램은 같은 DCID→같은 키로 독립
  복호, packet number만 상이). **미완결 데이터그램은 drop(fail-closed)하되
  reassembler는 CRYPTO 누적을 유지**한다 — 완결 데이터그램이 그 flow의
  first-accepted 패킷이 되어야 conntrack 엔트리에 connmark가 깨끗이
  confirm되기 때문이다(미완결을 passthrough-accept하면 그 패킷이 mark 0으로
  엔트리를 먼저 confirm해, 뒤이은 완결 데이터그램의 connmark 적용이 race에서
  져 UDP conntrack fast-path가 붙지 않는다 — KVM e2e 실측). 클라는 dropped
  데이터그램을 QUIC 손실복구로 retransmit하며, 그 재전송이 도달할 때는 이미
  flow가 allow+mark라 fast-path를 탄다. 부분 ClientHello는 서버(우리 verdict
  루프)에 완결 전달되지 않으므로 strictly fail-closed다. per-flow 바이트
  상한(`maxQuicClientHelloBytes = 8192`) 초과 → DROP, flow-count는 TCP
  reassembler와 같은 상수 `sniReassemblerMaxFlows`(4096)를 공유하는 별도
  LRU(`quicFlowLRU`)로 bound한다. **3개 이상 데이터그램에 걸치는 매우 큰
  ClientHello는 v1 미지원**(fail-closed deny, 후속 후보).
- **verdict 루프 UDP 확장(`cmd/goose-daemon/sni_verdict.go`)**:
  `parseIPv4UDP`가 IPv4 proto byte(`pkt[9]`)로 UDP를 식별해 srcIP/sport/
  dport/payload를 추출한다. Start 훅이 proto로 분기: TCP → 기존 TLS 경로,
  UDP & dport 443 → `decideQUIC`(`quic.InitialReassembler` Feed 결과에 따라
  라우팅). 미등록 srcIP → drop(`unregistered_source`); Feed 에러(복호
  실패/미지원버전/non-Initial/오버사이즈/overlap 모순) → drop
  (`egress_sni_unparsed`) + flow evict; Feed 미완결 → drop
  (`egress_sni_incomplete`) + **flow 유지**(reassembler는 CRYPTO 누적,
  패킷만 drop); ClientHello 완결 & SNI ∈ matcher → `sniAcceptMark`(connmark)
  + flow evict; ∉ matcher → drop(`egress_sni_denied`, SNI 기록) + flow
  evict. **UDP는 RST가 없어 모든 drop이 silent DROP**이다(TCP 경로의
  best-effort RST 주입은 UDP에 적용하지 않는다). `decideQUIC`은
  `sniPassthrough`를 내지 않는다(TCP 경로의 "미완결 세그먼트 unmarked
  passthrough"와 다른 계약 — 위 재조립 단락의 connmark-race 근거).
- **dispatch(`cmd/goose-daemon/egress_policy.go`)**: `planProfileEgressCommands`
  가 `len(AllowSNI)>0`일 때 TCP `-sni-nfqueue`/`-sni-fastpath`와 대칭으로
  UDP 규칙도 head-insert한다(`-p udp --dport 443`, 같은 queue `sniQueueNum()`,
  같은 connmark, comment `<prefix>-sni-udp-nfqueue`/`<prefix>-sni-udp-fastpath`).
  `allow_sni`가 비면 UDP:443 규칙도 미생성(기존 UDP:443 default-deny
  하위호환). rollback은 기존 `-I`↔`-D` 대칭 배관과 `flushEgressByComment`의
  `anvil-egress-<vmID>-` prefix 회수를 그대로 재사용한다.
- **`go.mod`**: 신규 direct 의존 `golang.org/x/crypto v0.54.0` 하나뿐
  (`git diff main -- go.mod`로 확인됨 — HKDF 유틸용, 이미 간접 의존이었으나
  이번에 direct로 승격). **참고**: 같은 diff에 `github.com/google/jsonschema-go`가
  indirect→direct로 재분류된 항목도 보이는데, 이는 QUIC 작업이 도입한 신규
  의존이 아니라 기존 의존의 재분류다(`go mod tidy` 부수 효과로 추정, 별도
  확인 권장 — 아래 Follow-Up).

## 위협 모델 / 잔여 위험 (요약 — 전문은 ADR-0002)

TCP:443 SNI 필터의 **핵심 계약 한 줄**을 그대로 계승한다: SNI 필터는 신뢰
워크로드의 의도된 :443 egress를 강제·감사한다. 적대적 in-guest 루트에 대한
완전 봉쇄가 아니다.

| 잔여 위험 | 계약 |
|---|---|
| 미지원/알 수 없는 QUIC 버전 | 키 파생 불가 → fail-closed DROP |
| non-Initial(Handshake/1-RTT/Retry/VN) 첫 패킷 | ClientHello 없음 → DROP |
| header protection/AEAD 복호 실패 | DROP |
| 멀티-데이터그램(PQ ClientHello 등) | flow별 재조립으로 CRYPTO 누적, 완결 전 데이터그램은 drop(fail-closed, connmark-race 회피), 클라 retransmit이 완결 후 fast-path를 탐 |
| 3개 이상 데이터그램에 걸치는 ClientHello | **v1 미지원** — fail-closed deny(후속 후보) |
| per-flow 바이트 상한(8192B) 초과 | DROP |
| SNI spoofing/domain fronting | TCP와 동일 — guest-asserted, 완전 봉쇄 비목표 |
| ECH/non-TLS | QUIC Initial에 인식 가능한 SNI가 없으면 DROP(TCP와 동일 계약) |

## 검증 증거

**유닛** (root 불필요, `-race`):
- `internal/network/quic`: RFC 9001 Appendix A(v1)/RFC 9369 Appendix A(v2)
  golden Initial 패킷 벡터로 키파생·복호·SNI 추출 정확성 검증
  (`TestDeriveClientInitialKeys_RFC9001_V1`, `TestDecryptInitial_RFC9001_V1`,
  `TestDecryptInitial_RFC9369_V2`, `TestParseInitialSNI_RFC9369_V2`,
  `TestParseInitialSNI_RoundTrip_V1`, `TestParseInitialSNI_RoundTrip_V2`).
  malformed/미지원버전/non-Initial/truncated → terminal error
  (`TestDeriveClientInitialKeys_UnsupportedVersion`,
  `TestDecryptInitial_NotLongHeader`, `TestDecryptInitial_UnsupportedVersion`,
  `TestDecryptInitial_NotInitialType`, `TestDecryptInitial_TruncatedBounds`,
  `TestParseInitialSNI_FailClosed`). CRYPTO 재조립 offset gap/truncated/
  overlap 모순 → terminal(`TestReassemble_OffsetGapTerminal`,
  `TestReassemble_TruncatedCryptoTerminal`,
  `TestReassemble_InconsistentOverlapTerminal`,
  `TestReassemble_NoCryptoTerminal`, `TestReassemble_UnknownFrameTerminal`,
  `TestReassemble_DuplicateCryptoTolerated`,
  `TestReassemble_SkipsAndReorders`). 멀티-데이터그램 재조립
  (`TestInitialReassembler_TwoDatagrams`,
  `TestInitialReassembler_SingleDatagramStillWorks`,
  `TestInitialReassembler_OversizeTerminal`,
  `TestInitialReassembler_InconsistentOverlapAcrossDatagrams`,
  `TestParseInitialSNI_IncompleteHandshakeTerminal`).
- `cmd/goose-daemon`: IPv4 UDP 파싱 경계(`TestQUICParseIPv4UDPBoundaries`),
  QUIC decide 라우팅(`TestSNIDecideQUICRouting`), 멀티-데이터그램 decide
  (`TestSNIDecideQUICMultiDatagram`), dispatch 대칭 생성
  (`TestPlanProfileEgressCommandsEmitsUDPSNIDispatch`,
  `TestPlanProfileEgressCommandsNoUDPSNIWhenEmpty`).
- 이 verdict 글루(netlink 바인딩, UDP conntrack mark, dispatch 규칙 실효,
  PQ multi-datagram 실 왕복)는 root+netfilter가 필요해 유닛으로 실검
  불가 — 아래 KVM e2e가 유일한 실검 경로다(TCP slice와 동일 경계).

**KVM e2e** (`sudo -n bash scripts/anvil-quic-sni-e2e.sh`, exit 0):
- HTTP/3 지원 클라이언트로 허용 도메인에 QUIC(UDP:443)로 도달 — post-quantum
  (X25519MLKEM768) default ClientHello의 2-데이터그램 재조립 경로를 실제로
  태운다(golden image 클라 기본 설정 그대로).
- `iptables -S FORWARD`에 해당 VM의 `-sni-udp-nfqueue`/`-sni-udp-fastpath`
  규칙과 connmark `0x534e49` 존재 확인.
- 비허용 도메인 QUIC → 차단(타임아웃/미도달, silent DROP이라 RST 없음).
- runtime audit에 `egress_sni_denied` 기록 확인(TCP와 동일 스키마 재사용).

## Next Action

1. "최종 검증(전체 슬라이스)" 수행: `go build ./... && go vet ./... &&
   gofmt -l . | grep -v '^web/'`, `go test -race ./internal/network/...
   ./cmd/...`(특히 `internal/network/quic`, `sni`, `cmd/goose-daemon`),
   `git diff main -- go.mod`(신규 direct 의존 `golang.org/x/crypto` 하나만 —
   `jsonschema-go` 재분류는 위 "무엇이 main에 있나" 참고 사항으로 별도
   확인), 기존 회귀(`go test ./cmd/goose-daemon/ -run 'Egress|SNI|Recover'`)
   + 전체 KVM 게이트(`sudo bash e2e_test.sh`), `bash scripts/secret-scan.sh`.
2. PR 생성(`feature/quic-sni-filter` → `main`). **자체 머지 금지** — 머지는
   사용자 승인으로만.

## Follow-Up Tasks

1. **3개 이상 Initial 데이터그램에 걸치는 ClientHello 지원** — 현재 v1은
   2-데이터그램 재조립만 지원한다. 매우 큰 확장(예: 다중 인증서 체인 힌트,
   대형 ALPN 목록)을 쓰는 클라이언트는 계속 fail-closed deny로 관측될 수
   있다. 실사용 빈도를 관찰한 뒤 3+ 지원 여부를 재검토한다.
2. **proto별(`tcp`/`udp`) `ephemera_egress_sni_verdict_total` metric label
   분리** — 현재 TCP/QUIC이 같은 outcome label(`allowed`/`denied`)을
   공유해 운영자가 QUIC 채택률/차단률을 TCP와 구분해 볼 수 없다.
3. **새 QUIC 버전(v1/v2 외) salt/label 추가** — 새 버전 등장 시 이 slice가
   fail-closed deny로 안전측 처리하지만, 채택이 늘면 별도 slice로 salt/
   label을 추가해야 한다.
4. **`github.com/google/jsonschema-go`의 indirect→direct 재분류 확인** —
   `git diff main -- go.mod`에 QUIC이 의도한 `golang.org/x/crypto` 외에
   이 재분류가 같이 보인다. QUIC 작업이 신규로 도입한 의존은 아니나(이미
   존재하던 간접 의존), `go mod tidy` 실행 시점/원인을 확인해 의도치 않은
   변경이 아닌지 별도 점검 권장.
5. **golden-image HTTP/3 클라이언트 가용성 재확인** — e2e가 사용하는 클라
   (`curl --http3` 또는 Go QUIC 클라)의 golden image 상 유지보수 상태를
   `scripts/build_image.sh` 변경 시 함께 확인한다(설계 스펙 미결#2에서
   이어짐).

## 관련 문서

- [ADR-0002](../adr/0002-egress-sni-transparent-filter.md) — "메커니즘 확장 —
  UDP:443 QUIC/HTTP3" 절, 잔여 위험 계약 표 QUIC 행.
- [design spec](../superpowers/specs/2026-07-14-quic-sni-filter-design.md).
- [security-policy.md `UDP:443(QUIC/HTTP3) 확장` 절](security-policy.md),
  [runbook.md `(e) QUIC/UDP:443(HTTP3) 운영` 절](runbook.md),
  [PUBLIC_RELEASE_BOUNDARY.md `Egress SNI filter` 행](../PUBLIC_RELEASE_BOUNDARY.md).
- [2026-07-13 TCP:443 SNI 필터 handoff](2026-07-13-egress-sni-handoff.md) —
  선행 slice(같은 ADR-0002의 TCP 절반).
