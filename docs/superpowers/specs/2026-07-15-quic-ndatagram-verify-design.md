# QUIC N-데이터그램 재조립 증명 + 문서 교정 — 설계

**날짜:** 2026-07-15
**상태:** 승인됨 (사용자 승인 2026-07-15)
**관련:** [ADR-0002](../../adr/0002-egress-sni-transparent-filter.md), 선행 QUIC SNI 필터 (PR #66, `docs/superpowers/specs/2026-07-14-quic-sni-filter-design.md`)

## 목표

anvil egress QUIC SNI 필터가 **3개 이상 Initial 데이터그램에 걸친 ClientHello를 이미 재조립·허용**한다는 사실을 회귀 테스트로 증명하고, 이를 "미지원/fail-closed deny"라고 **잘못 서술한 문서·주석을 바로잡는다**. 런타임 동작은 바꾸지 않는다.

## 배경 — 전제 정정

백로그 항목 "3+ Initial 데이터그램 ClientHello 지원 (현재 fail-closed deny)"과 ADR-0002의 잔여위험 서술("2 데이터그램까지만 재조립, 3+ 미지원")은 **코드와 어긋난다**. 탐색 결과:

- `internal/network/quic/frames.go`의 `InitialReassembler.Feed`는 offset-indexed 버퍼로 **데이터그램 수 하드 제한 없이** CRYPTO를 누적한다. 유일한 상한은 `maxQuicClientHelloBytes = 8192`(≈7 데이터그램 분량).
- `cmd/goose-daemon/sni_verdict.go`의 `decideQUIC` `!done` 분기는 미완결 데이터그램을 drop하되 **flow를 캐시 유지**(evict 안 함)해 dg1·dg2·dg3…를 같은 reassembler에 계속 누적한다.
- **실증**(2026-07-15, 스크래치 white-box 테스트): 3-데이터그램 ClientHello를 순서까지 뒤섞어 `Feed`에 먹였더니 `done=true` + SNI 정확 추출 → PASS.

즉 "2까지만"은 **테스트 인프라의 한계**(`BuildInitialDatagramsForTest`가 정확히 2개만 생성, 3-dg 테스트 부재)를 저자가 *지원 경계*로 보수적으로 문서화한 결과다. 진짜 잔여는 8192B 바이트 캡뿐이며, 이를 넘는 ClientHello(>8KB)는 주류 클라이언트(PQ ~1.5KB)가 생성하지 않는다.

따라서 이 작업은 **기능 갭이 아니라 테스트+문서 갭**을 닫는다.

## 스코프

**포함:**
1. N-데이터그램 테스트 빌더
2. 3/4-데이터그램 재조립 회귀 유닛 테스트
3. 틀린 코드 주석 1곳 수정
4. 스테일 문서 7곳 교정

**제외 (YAGNI / 결정):**
- **8192B 캡 상향/제거** — 주류 클라 미해당(YAGNI), 캡 상향은 per-flow DoS 버퍼 bound만 늘림.
- **KVM e2e** — 3-데이터그램 kernel connmark 경로는 2-데이터그램 경로와 로직 동일(완결 데이터그램이 first-accepted, 미완결은 모두 drop). 차이는 "연속 2회 drop 후 완결"뿐이며 기존 QUIC e2e가 2-데이터그램 경로를 이미 실증. follow-up 후보로 등재.
- **런타임 로직 변경** — 없음. 이미 동작함.

## 변경 상세

### 1. 테스트 인프라 — `internal/network/quic/testsupport.go`

신규 함수:

```go
// BuildInitialDatagramsForTestN builds len(splits)+1 QUIC Initial datagrams that
// together carry `handshake`, cut at the given ascending offsets. Datagram i carries
// CRYPTO bytes [cut[i], cut[i+1]) at that stream offset with packet number i, all
// under the same DCID (so they share client Initial keys). splits must be strictly
// ascending and strictly inside (0, len(handshake)); otherwise it panics (test helper).
func BuildInitialDatagramsForTestN(dcid []byte, version uint32, handshake []byte, splits ...int) [][]byte
```

- 데이터그램 i는 `buildInitialDatagram(dcid, version, offset_i, handshake[cut_i:cut_{i+1}], pn=i)`로 구성(기존 내부 헬퍼 재사용).
- 기존 `BuildInitialDatagramsForTest(dcid, version, handshake, splitAt)`는 **N-버전에 위임**해 코드 경로 일원화(DRY): `splitAt`가 (0, len) 밖이면 종전대로 단일 데이터그램, 안이면 `BuildInitialDatagramsForTestN(..., splitAt)`. 시그니처 불변 → 기존 콜러(2-dg 테스트, cross-package Task 6c 테스트) 무영향.

### 2. 회귀 유닛 테스트 — `internal/network/quic/frames_test.go`

신규 `TestInitialReassembler_NDatagrams` (테이블 주도):

| case | 데이터그램 수 | 도착 순서 | 기대 |
|------|-------------|----------|------|
| 3-dg in-order | 3 | d0,d1,d2 | 마지막에 done + SNI |
| 3-dg out-of-order | 3 | d0,d2,d1 | 마지막에 done + SNI |
| 4-dg in-order | 4 | d0,d1,d2,d3 | 마지막에 done + SNI |

- 각 케이스: 마지막 데이터그램 전에는 `done=false, err=nil`(누적 중), 마지막에 `done=true, name=<SNI>`. `buildClientHelloHandshake("example.com")` 재사용, 정확 SNI 대조.
- 기존 `TestInitialReassembler_TwoDatagrams`, `_OversizeTerminal`, `_InconsistentOverlapAcrossDatagrams`, fuzz 전부 유지.

### 3. 코드 주석 수정 — `internal/network/quic/frames.go` (L12-17)

`errIncompleteInDatagram` 주석의 거짓 서술 제거. 현재:

> "anvil inspects only the first Initial datagram, so a ClientHello that spans several datagrams is treated as terminal — the verdict loop fails closed (deny) rather than buffering across packets."

이는 사실과 반대(verdict 루프는 실제로 buffer함). 수정: 이 sentinel은 **단일-데이터그램 편의 함수 `ParseInitialSNI`**(및 datagram-내 `reassembleCryptoFrames`)에 국한된 "이 데이터그램 안에서 미완결" 신호이며, 멀티-데이터그램 `InitialReassembler.Feed`는 여러 데이터그램에 걸쳐 누적한다는 점을 명시. `InitialReassembler` 타입 주석(L38-49)은 이미 정확하므로 그대로.

### 4. 문서 교정 (7곳)

**교정 문구(일관):**
> "N-데이터그램 재조립 지원(데이터그램 수 하드 제한 없음). 실질 상한 = per-flow 8192B 바이트 캡(≈7 데이터그램); 이를 초과하는 ClientHello만 fail-closed deny — 주류 클라이언트(PQ ~1.5KB)는 해당 없음."

**보존:** "PQ ClientHello가 2 Initial 데이터그램에 걸친다"는 *관측 사실*이므로 유지. 오직 *한계/잔여* 클레임("3+ 미지원", "2까지만 재조립")만 교정.

| 파일 | 위치 | 조치 |
|------|------|------|
| `docs/adr/0002-egress-sni-transparent-filter.md` | L116 잔여, L225 표 잔여("2 데이터그램까지만 재조립") | 교정 문구로 치환 |
| `CONTEXT.md` | L513-514 잔여 | 교정 문구로 치환 |
| `CONTEXT.md` | L557 후속목록 "3+ Initial 데이터그램…지원" | **항목 제거**(이미 지원). >8192B 한계는 ADR-0002 잔여위험 행에만 정확히 서술하고 CONTEXT 후속 목록엔 재등재하지 않는다(주류 클라 미해당, 무의미) |
| `docs/PUBLIC_RELEASE_BOUNDARY.md` | L35 꼬리 "3개 이상…v1 미지원" | 교정 문구로 치환 |
| `docs/operations/runbook.md` | L374-377 | 교정 문구로 치환 |
| `docs/operations/security-policy.md` | L167-168, L198-199 | 교정 문구로 치환 |
| `docs/operations/2026-07-14-quic-sni-handoff.md` | L51-52, L93(표), L154(follow-up) | 교정 문구 + follow-up 항목 정정 |
| `docs/superpowers/specs/2026-07-14-quic-sni-filter-design.md` | L159 follow-up | 1줄 정정 주 추가(historical 보존): "정정 2026-07-15 — N-dg는 이미 지원, 상한 8192B" |

## 검증

- `go test ./internal/network/quic/... -race` — 신규 N-데이터그램 테스트 + 기존 전체(2-dg, oversize, overlap, RFC v1/v2 golden, fuzz) green.
- `go test ./cmd/goose-daemon/... -race` — 디스패치 무변경 회귀 확인(decideQUIC 경로).
- `go build ./...`, `go vet ./...`, `gofmt -l`(신규/수정 파일 clean).
- **e2e 없음**(스코프 결정). **full KVM 게이트 불요** — 런타임 로직 무변경(주석·테스트·문서만)이며, 최종 whole-branch 리뷰에서 이 판단을 재확인.

## 잔여 위험 (교정 후)

- **>8192B ClientHello**: per-flow 바이트 캡 초과 시 fail-closed deny. 실재하는 한계이나 주류 클라이언트가 생성하지 않아 무의미. 정직하게 문서화(가공된 '3+ 데이터그램 미지원' 서술 제거). 캡 상향은 YAGNI로 미채택.
- **3-데이터그램 kernel connmark 경로 미-e2e**: 유닛으로 reassembler 로직 증명. 실-커널 경로는 2-dg e2e로 이미 실증됐고 3-dg는 로직 동일. follow-up 후보.

## 비목표

- 8192B 캡 변경. QUIC 버전 추가. proto별 metric label 분리(별도 백로그). ECH/hold-then-decide(별도 백로그).
