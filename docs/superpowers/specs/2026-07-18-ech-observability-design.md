# ECH observability — 설계

**날짜:** 2026-07-18
**상태:** 승인됨 (사용자 승인 2026-07-18)
**관련:** [ADR-0002](../../adr/0002-egress-sni-transparent-filter.md) ECH 잔여위험 행, 2026-07-16 ECH 재확인(PR #73)

## 목표

egress SNI 필터가 허용(allowlisted outer SNI)한 flow가 **ECH(`encrypted_client_hello` 확장, 0xfe0d)를 담고 있으면 metric + 로그로 관측**한다 — flow는 **여전히 허용**(deny 안 함). ECH 잔여위험(allowlisted outer가 암호화 inner를 은닉)에 대한 운영자 가시성만 추가한다. **blanket ECH-deny는 채택하지 않는다**(신뢰-워크로드 모델상 net-negative: 정상 ECH 파손·모델 밖 위협·불완전 — brainstorming 탐색 결론).

## 배경

anvil은 ClientHello의 **outer(공개/cover) SNI만** 관측한다. allowlisted 도메인을 ECH 공개 이름으로 쓰면 flow가 허용되고 inner가 은닉된다(guest-asserted SNI 동일 신뢰등급 잔여). 정상 ECH 엔드포인트는 OQ7 CIDR 핀(SNI 필터 우회)으로 대응한다. 이 작업은 그 잔여를 **막지 않고 관측**만 한다 — false-positive DENY 없이 운영자가 ECH 사용/잠재 터널을 볼 수 있게.

## 스코프

**포함**: ECH 확장 탐지(파서, best-effort, SNI 의미 불변) + allowed flow에서 metric/log. TCP+QUIC 공통.
**제외(비목표)**: ECH deny/차단. 새 audit 레코드 타입(metric + slog로 충분, redaction 복잡도 회피). ECH inner 복호(불가). 3+ 데이터그램 등 무관.

## 변경 상세

### 1. 탐지 (`internal/network/sni/parser.go`)

`ParseHandshakeSNI`가 확장 순회 중 ECH 확장(`0xfe0d`) 존재를 **best-effort로 탐지**한다. **SNI 파싱 의미는 불변** — 반환되는 SNI(첫 server_name)와 모든 fail-closed 에러 조건(ErrIncomplete/ErrNoSNI/truncation)이 현재와 동일해야 한다. ECH 탐지는 additive: server_name을 찾아 반환하기 전/후로 ECH 확장 유무만 부수적으로 기록한다(잘린 후행 확장이 있어도 SNI 결과·에러를 바꾸지 않는 best-effort).

인터페이스:
- `ParseHandshakeSNI(hs []byte) (sni string, echPresent bool, err error)`
- `ParseClientHelloSNI(b []byte) (sni string, echPresent bool, err error)`

### 2. 전파

- `sni.Reassembler.Feed(segment) (name string, done bool, echObserved bool, err error)` — 완결 시 `echObserved` 함께 반환(미완결/에러 시 false).
- `quic.InitialReassembler.Feed(datagram) (name string, done bool, echObserved bool, err error)` — 동일.
- `quic.ParseInitialSNI`(단일-데이터그램 convenience, 주로 테스트)는 `(string, error)` 유지하고 echObserved는 내부 discard.
- daemon: TCP Start 훅·`decideQUIC`가 `echObserved`를 `sniDecision`의 신규 필드 `ECHObserved bool`에 실어 `recordVerdict`로 전달.

### 3. 방출 (`cmd/goose-daemon`, deny 없음)

- `daemonMetrics.IncSNIECHObserved(proto string)` 신설 → 신규 CounterVec `ephemera_egress_sni_ech_observed_total{proto="tcp|udp"}`.
- `recordVerdict`가 `d.Action == sniAcceptMark && d.ECHObserved`일 때 `IncSNIECHObserved(proto)` + `slog.Info`(content-free: proto + 허용된 outer SNI + "ech observed on allowed flow"). **flow는 여전히 allowed**(verdict 불변).

## 검증

- `internal/network/sni` 유닛: ECH 확장 있는/없는 ClientHello 파싱 — **SNI 결과 불변** + `echPresent` 정확. 잘린 후행 확장에도 SNI 의미 불변.
- `internal/network/quic` 유닛: reassembler Feed가 echObserved 전파(멀티-데이터그램 ECH ClientHello).
- `cmd/goose-daemon` 유닛: recordVerdict allowed+ech → metric 발화 + `/metrics` 노출; denied 또는 non-ech → 미발화. verdict(allow/deny) 불변 회귀.
- `go test ./... -race`, build/vet/gofmt.

## 비목표

- ECH deny/차단(설계상 net-negative). ECH inner 복호. blanket-deny opt-in 플래그(관측성이 채택된 방향).
