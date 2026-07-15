# QUIC N-데이터그램 재조립 증명 + 문서 교정 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** anvil egress QUIC SNI 필터가 3개 이상 Initial 데이터그램에 걸친 ClientHello를 이미 재조립·허용함을 회귀 테스트로 증명하고, 이를 "미지원/fail-closed deny"로 잘못 서술한 문서·주석을 바로잡는다.

**Architecture:** 런타임 로직은 바꾸지 않는다. `InitialReassembler.Feed`는 이미 데이터그램 수 하드 제한 없이 CRYPTO를 offset 누적하며(상한은 `maxQuicClientHelloBytes = 8192`뿐), `decideQUIC`의 `!done` 분기가 미완결 데이터그램을 drop하되 flow를 캐시 유지해 N개까지 누적한다. Task 1은 N-데이터그램 test 빌더 + 회귀 테스트로 이 동작을 증명하고 틀린 코드 주석을 수정한다. Task 2는 스테일 문서 8곳을 교정한다.

**Tech Stack:** Go 1.24+, `internal/network/quic`(자체 QUIC Initial 복호), `internal/network/sni`(ParseHandshakeSNI), 표준 `testing`.

## Global Constraints

- **런타임 로직 변경 금지** — production 코드(`Feed`/`decideQUIC`/`extractCryptoChunks` 등)의 동작을 바꾸지 않는다. 유일한 production-파일 변경은 `frames.go`의 **주석**뿐이다.
- **8192B 캡 유지** — `maxQuicClientHelloBytes = 8192`를 바꾸지 않는다(캡 상향은 YAGNI, 스코프 밖).
- **e2e 없음** — KVM e2e는 스코프 밖(follow-up 등재).
- **관측 사실 보존** — "PQ ClientHello가 Initial 데이터그램 2개에 걸친다"는 참인 관측이므로 유지한다. 오직 *한계/잔여* 클레임("3+ 미지원", "2 데이터그램까지만 재조립")만 교정한다.
- **교정 문구(일관)**: "재조립은 데이터그램 수에 하드 제한이 없다 — 실질 상한은 per-flow 8192B 바이트 캡(≈7 데이터그램); 이를 초과하는 ClientHello만 fail-closed deny(주류 클라이언트 PQ ~1.5KB는 해당 없음)."
- **커밋 트레일러 없음** — anvil 저장소 커밋에는 Co-Authored-By 등 트레일러를 붙이지 않는다.
- **브랜치**: `feature/quic-ndatagram-verify` (이미 생성, spec 커밋 `af2c0b5`).

## File Structure

| 파일 | 책임 | Task |
|------|------|------|
| `internal/network/quic/testsupport.go` | N-데이터그램 test 빌더 추가 + 기존 2-arg 빌더를 위임 | 1 |
| `internal/network/quic/frames_test.go` | 3/4-데이터그램 재조립 회귀 테스트 추가 | 1 |
| `internal/network/quic/frames.go` | `errIncompleteInDatagram` 주석의 거짓 서술 수정(주석만) | 1 |
| `docs/adr/0002-egress-sni-transparent-filter.md` | 잔여위험 서술 교정(canonical) | 2 |
| `CONTEXT.md` | fail-closed 목록 정정 + 재조립 문단 보강 + 후속목록 정정 | 2 |
| `docs/PUBLIC_RELEASE_BOUNDARY.md` | 표면 표 꼬리 교정 | 2 |
| `docs/operations/runbook.md` | 운영 지연 특성 절 교정 | 2 |
| `docs/operations/security-policy.md` | 메커니즘/잔여위험 2곳 교정 | 2 |
| `docs/operations/2026-07-14-quic-sni-handoff.md` | 서술 + 표 + follow-up 정정 | 2 |
| `docs/superpowers/specs/2026-07-14-quic-sni-filter-design.md` | 후속 후보 1줄 정정 주(historical 보존) | 2 |

---

## Task 1: N-데이터그램 test 빌더 + 회귀 테스트 + 주석 수정

**Files:**
- Modify: `internal/network/quic/testsupport.go` (신규 `BuildInitialDatagramsForTestN`, 기존 `BuildInitialDatagramsForTest` 위임)
- Modify: `internal/network/quic/frames.go` (`errIncompleteInDatagram` 주석, 현재 L12-17)
- Test: `internal/network/quic/frames_test.go` (신규 `TestInitialReassembler_NDatagrams`)

**Interfaces:**
- Consumes (기존 helper, 변경 금지): `buildInitialDatagram(dcid []byte, version uint32, cryptoOffset uint64, cryptoData []byte, pn uint64) []byte`, `buildClientHelloHandshake(serverName string) []byte`, `mustHex(string) []byte`, `InitialReassembler.Feed(datagram []byte) (name string, done bool, err error)`.
- Produces: `BuildInitialDatagramsForTestN(dcid []byte, version uint32, handshake []byte, cuts ...int) [][]byte` — `len(cuts)+1`개 Initial 데이터그램(offset 순, pn 0..N-1, 동일 DCID). 기존 `BuildInitialDatagramsForTest(dcid, version, handshake, splitAt)` 시그니처는 불변.

**배경 사실(구현자 참고):** `ParseHandshakeSNI`는 handshake body가 선언된 길이(`hsLen`)만큼 present하기 전에는 `sni.ErrIncomplete`를 반환한다(`parser.go`: `if len(body) < hsLen { return "", ErrIncomplete }`). 따라서 SNI가 ClientHello 앞쪽에 있어도, 전체 데이터그램이 다 모여 contiguous 길이가 handshake 전체가 되기 전에는 `Feed`가 `("", false, nil)`을 반환한다. 이 사실이 아래 테스트의 "마지막 Feed 전까지 not-done" 단언을 보장한다.

- [ ] **Step 1: Write the failing test**

`internal/network/quic/frames_test.go`의 `TestInitialReassembler_InconsistentOverlapAcrossDatagrams` 바로 앞(즉 `// --- Task 6b: InitialReassembler multi-datagram coverage. ---` 블록 내, `TestInitialReassembler_TwoDatagrams` 근처)에 다음을 추가한다:

```go
// TestInitialReassembler_NDatagrams: a ClientHello split across THREE or FOUR
// Initial datagrams reassembles regardless of arrival order. The reassembler has no
// hard datagram-count limit — it accumulates CRYPTO across as many datagrams as the
// ClientHello spans, bounded only by the per-flow byte cap — so a large ClientHello
// spanning more than two datagrams is handled, not denied. Each non-final Feed
// returns ("", false, nil) (still accumulating; ParseHandshakeSNI needs the whole
// handshake body length before it yields an SNI), and only the datagram that fills
// the last gap completes with the SNI.
func TestInitialReassembler_NDatagrams(t *testing.T) {
	const sni = "api.anthropic.com"
	dcid := mustHex("8394c8f03e515708")
	ch := buildClientHelloHandshake(sni)
	n := len(ch)
	if n < 8 {
		t.Fatalf("clienthello too small to split into 3/4: %d bytes", n)
	}
	three := BuildInitialDatagramsForTestN(dcid, 0x00000001, ch, n/3, 2*n/3)
	four := BuildInitialDatagramsForTestN(dcid, 0x00000001, ch, n/4, n/2, 3*n/4)

	cases := []struct {
		name  string
		dgs   [][]byte
		order []int // indices into dgs, giving arrival order (last one completes)
	}{
		{"3-datagram in-order", three, []int{0, 1, 2}},
		{"3-datagram out-of-order", three, []int{0, 2, 1}},
		{"4-datagram in-order", four, []int{0, 1, 2, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.dgs) != len(tc.order) {
				t.Fatalf("built %d datagrams, order lists %d", len(tc.dgs), len(tc.order))
			}
			r := &InitialReassembler{}
			for i, idx := range tc.order {
				name, done, err := r.Feed(tc.dgs[idx])
				if err != nil {
					t.Fatalf("Feed #%d (datagram %d): unexpected err %v", i, idx, err)
				}
				if last := i == len(tc.order)-1; !last {
					if done || name != "" {
						t.Fatalf("Feed #%d (datagram %d) = %q, %v; want \"\", false (accumulating)", i, idx, name, done)
					}
					continue
				}
				if !done || name != sni {
					t.Fatalf("final Feed #%d (datagram %d) = %q, %v; want %q, true", i, idx, name, done, sni)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails (compile error)**

Run: `go test ./internal/network/quic/ -run TestInitialReassembler_NDatagrams`
Expected: FAIL — `undefined: BuildInitialDatagramsForTestN`.

- [ ] **Step 3: Add the N-datagram builder and delegate the existing 2-arg builder**

`internal/network/quic/testsupport.go`에서 기존 `BuildInitialDatagramsForTest`(현재 L36-43)를 다음으로 **교체**한다:

```go
// BuildInitialDatagramsForTest builds the QUIC Initial datagrams carrying handshake,
// splitting it into two CRYPTO frames — offsets [0,splitAt) and [splitAt,len) — so a
// caller can exercise cross-datagram reassembly. Both datagrams share dcid (so they
// share the same client Initial keys). If splitAt is not strictly inside the
// handshake (splitAt <= 0 or splitAt >= len), the handshake fits in one CRYPTO frame
// and a single datagram is returned. It delegates to BuildInitialDatagramsForTestN.
func BuildInitialDatagramsForTest(dcid []byte, version uint32, handshake []byte, splitAt int) [][]byte {
	if splitAt <= 0 || splitAt >= len(handshake) {
		return BuildInitialDatagramsForTestN(dcid, version, handshake)
	}
	return BuildInitialDatagramsForTestN(dcid, version, handshake, splitAt)
}

// BuildInitialDatagramsForTestN builds len(cuts)+1 QUIC Initial datagrams that
// together carry handshake, cut at the ascending byte offsets in cuts. Datagram i
// carries CRYPTO bytes [start_i, end_i) at that stream offset with packet number i,
// all under the same dcid (so they share the client Initial keys). Each cut must be
// strictly ascending and strictly inside (0, len(handshake)); a cut out of that range
// or out of order panics (a test helper should fail loudly). With no cuts it returns a
// single datagram carrying the whole handshake at offset 0.
func BuildInitialDatagramsForTestN(dcid []byte, version uint32, handshake []byte, cuts ...int) [][]byte {
	bounds := []int{0}
	for _, c := range cuts {
		if c <= bounds[len(bounds)-1] || c >= len(handshake) {
			panic("quic: BuildInitialDatagramsForTestN cut out of range or not strictly ascending")
		}
		bounds = append(bounds, c)
	}
	bounds = append(bounds, len(handshake))

	dgs := make([][]byte, 0, len(bounds)-1)
	for i := 0; i < len(bounds)-1; i++ {
		start, end := bounds[i], bounds[i+1]
		dgs = append(dgs, buildInitialDatagram(dcid, version, uint64(start), handshake[start:end], uint64(i)))
	}
	return dgs
}
```

- [ ] **Step 4: Run the new test to verify it passes**

Run: `go test ./internal/network/quic/ -run TestInitialReassembler_NDatagrams -v`
Expected: PASS — subtests `3-datagram in-order`, `3-datagram out-of-order`, `4-datagram in-order` all PASS.

- [ ] **Step 5: Fix the false comment in frames.go**

`internal/network/quic/frames.go`의 `errIncompleteInDatagram` 주석(현재 L12-16)을 교체한다. 기존:

```go
// errIncompleteInDatagram means the ClientHello was not fully carried within this
// single Initial datagram (a CRYPTO offset gap, a truncated frame, or no CRYPTO
// data at all). anvil inspects only the first Initial datagram, so a ClientHello
// that spans several datagrams is treated as terminal — the verdict loop fails
// closed (deny) rather than buffering across packets.
```

교체 후:

```go
// errIncompleteInDatagram means the ClientHello was not fully carried within a
// single Initial datagram (a CRYPTO offset gap, a truncated frame, or no CRYPTO
// data at all). It is returned by the single-datagram convenience ParseInitialSNI
// (and by reassembleCryptoFrames, which reassembles within one datagram): those
// callers treat "not complete within this datagram" as terminal and fail closed. The
// multi-datagram InitialReassembler.Feed does NOT — it accumulates CRYPTO across as
// many datagrams as the ClientHello spans (see InitialReassembler), so a ClientHello
// spanning several datagrams reassembles rather than being denied, bounded only by
// the per-flow byte cap.
```

- [ ] **Step 6: Run the full quic package under the race detector**

Run: `go test ./internal/network/quic/... -race`
Expected: PASS (ok) — 신규 N-데이터그램 테스트 + 기존 전체(2-datagram, single, oversize, overlap, RFC v1/v2 golden, fuzz seed corpus) green. 주석 변경은 동작에 영향 없음.

- [ ] **Step 7: Verify the dispatch path still compiles/tests (no runtime change)**

Run: `go build ./... && go vet ./internal/network/quic/... ./cmd/goose-daemon/... && gofmt -l internal/network/quic/testsupport.go internal/network/quic/frames.go internal/network/quic/frames_test.go`
Expected: build OK, vet clean, `gofmt -l` prints **nothing** (all three files formatted).

- [ ] **Step 8: Commit**

```bash
git add internal/network/quic/testsupport.go internal/network/quic/frames_test.go internal/network/quic/frames.go
git commit -m "test(quic): prove N-datagram reassembly (3/4 datagrams) + fix stale single-datagram comment"
```

---

## Task 2: 스테일 문서 8곳 교정

**Files:**
- Modify: `docs/adr/0002-egress-sni-transparent-filter.md` (2곳)
- Modify: `CONTEXT.md` (3곳)
- Modify: `docs/PUBLIC_RELEASE_BOUNDARY.md` (1곳)
- Modify: `docs/operations/runbook.md` (1곳)
- Modify: `docs/operations/security-policy.md` (2곳)
- Modify: `docs/operations/2026-07-14-quic-sni-handoff.md` (3곳)
- Modify: `docs/superpowers/specs/2026-07-14-quic-sni-filter-design.md` (1곳)

**Interfaces:** 없음(문서만). Task 1의 코드 사실("재조립은 데이터그램 수 하드 제한 없음, 상한 8192B")을 문서에 반영.

각 편집은 아래의 정확한 old → new 문자열 치환이다. 모든 old 문자열은 현재 파일에 유일하게 존재한다(줄바꿈·들여쓰기 포함, 파일 그대로).

- [ ] **Step 1: `docs/adr/0002-egress-sni-transparent-filter.md` — 잔여 서술(멀티-데이터그램 절)**

old:
```
per-flow 바이트 상한(8192B) +
  flow-count LRU(4096)로 reassembler 상태를 무한 성장 없이 bound한다. **3개
  이상 데이터그램에 걸치는 ClientHello는 v1 미지원**(후속 후보, 아래
  잔여위험 표).
```
new:
```
per-flow 바이트 상한(8192B) +
  flow-count LRU(4096)로 reassembler 상태를 무한 성장 없이 bound한다. **재조립은
  데이터그램 수에 하드 제한이 없다** — 실질 상한은 per-flow 8192B 바이트 캡
  (≈7 데이터그램)이며, 이를 초과하는 ClientHello만 fail-closed deny한다(주류
  클라이언트 PQ ~1.5KB는 해당 없음; 아래 잔여위험 표).
```

- [ ] **Step 2: `docs/adr/0002-egress-sni-transparent-filter.md` — 잔여위험 표 셀**

old: `**3개 이상 Initial 데이터그램에 걸치는 매우 큰 ClientHello는 v1 미지원**(후속 후보 — 2 데이터그램까지만 재조립).`
new: `**재조립은 데이터그램 수에 하드 제한이 없다** — 실질 상한은 per-flow 8192B 바이트 캡(≈7 데이터그램)이며, 이를 초과하는 ClientHello만 fail-closed deny(주류 클라 PQ ~1.5KB 미해당).`

- [ ] **Step 3: `CONTEXT.md` — 재조립 문단 보강**

old: `per-flow 바이트 상한(8192B)+flow-count LRU(4096)로 상태를
  bound한다. **deny = silent DROP**(UDP엔 RST 없음) — QUIC 타임아웃 후`
new: `per-flow 바이트 상한(8192B)+flow-count LRU(4096)로 상태를
  bound한다. 재조립은 데이터그램 수에 하드 제한이 없다(3개 이상 데이터그램에
  걸치는 ClientHello도 지원); 실질 상한은 8192B 캡뿐이다. **deny = silent
  DROP**(UDP엔 RST 없음) — QUIC 타임아웃 후`

- [ ] **Step 4: `CONTEXT.md` — fail-closed 목록에서 거짓 항목 제거**

old:
```
복호 실패, no-SNI, per-flow 바이트 상한 초과, **3개 이상 데이터그램에
  걸치는 매우 큰 ClientHello(v1 미지원)** → 전부 DROP(TCP slice와 동일
```
new:
```
복호 실패, no-SNI, per-flow 바이트 상한(8192B) 초과 → 전부 DROP(TCP slice와 동일
```

- [ ] **Step 5: `CONTEXT.md` — 후속 목록 정정**

old:
```
  ~~QUIC/UDP:443 SNI 파싱~~ — **DONE(2026-07-14)**, 위 항목 참조. QUIC
  확장이 새로 남긴 후속: 3개 이상 Initial 데이터그램에 걸치는 매우 큰
  ClientHello 지원(현재 fail-closed deny), TCP/UDP proto별
  `ephemera_egress_sni_verdict_total` metric label 분리(현재 공유), 새 QUIC
  버전(v1/v2 외) salt/label 추가.
```
new:
```
  ~~QUIC/UDP:443 SNI 파싱~~ — **DONE(2026-07-14)**, 위 항목 참조. QUIC
  확장이 새로 남긴 후속: TCP/UDP proto별
  `ephemera_egress_sni_verdict_total` metric label 분리(현재 공유), 새 QUIC
  버전(v1/v2 외) salt/label 추가, 3-데이터그램 kernel 경로 KVM e2e 실증.
  (3+ 데이터그램 ClientHello 지원은 2026-07-15 증명·문서교정으로 종결 —
  이미 지원되며 >8192B만 잔여, 주류 클라 미해당.)
```

- [ ] **Step 6: `docs/PUBLIC_RELEASE_BOUNDARY.md` — 표 셀 꼬리**

old: `3개 이상 데이터그램에 걸치는 ClientHello는 v1 미지원(fail-closed deny)`
new: `재조립은 데이터그램 수에 하드 제한이 없다(3개 이상 데이터그램 ClientHello 지원) — 실질 상한은 per-flow 8192B 캡이며 이를 초과하는 ClientHello만 fail-closed deny(주류 클라 미해당)`

- [ ] **Step 7: `docs/operations/runbook.md` — 지연 특성 절**

old:
```
QUIC 손실복구 retransmit 1왕복만큼 handshake가 지연된다(정상 동작, 오탐
아님). 3개 이상 데이터그램에 걸치는 매우 큰 ClientHello는 v1 미지원이라
계속 fail-closed deny로 관측된다(허용 도메인이어도 handshake가 끝내
완결되지 않음 — 후속 후보, ADR-0002 잔여위험 표 참조). 허용 도메인의
```
new:
```
QUIC 손실복구 retransmit 1왕복만큼 handshake가 지연된다(정상 동작, 오탐
아님). 재조립은 데이터그램 수에 하드 제한이 없어 3개 이상에 걸치는
ClientHello도 허용된다(실질 상한은 per-flow 8192B 캡, ≈7 데이터그램) —
8192B를 초과하는 ClientHello만 fail-closed deny로 관측된다(주류 클라
PQ ~1.5KB 미해당, ADR-0002 잔여위험 표 참조). 허용 도메인의
```

- [ ] **Step 8: `docs/operations/security-policy.md` — 메커니즘 절**

old:
```
필터를 타 `allow_sni`면 허용된다(자연 degrade). 3개 이상 데이터그램에
걸치는 매우 큰 ClientHello는 v1 미지원(후속 후보). 상세는
```
new:
```
필터를 타 `allow_sni`면 허용된다(자연 degrade). 재조립은 데이터그램 수에
하드 제한이 없다 — 실질 상한은 per-flow 8192B 캡(≈7 데이터그램)이며 이를
초과하는 ClientHello만 fail-closed deny(주류 클라 미해당). 상세는
```

- [ ] **Step 9: `docs/operations/security-policy.md` — 잔여위험 목록(QUIC 항목)**

old:
```
  절). SNI는 TCP와 동일하게 guest-asserted다. 3개 이상 Initial 데이터그램에
  걸치는 매우 큰 ClientHello는 v1 미지원 — fail-closed deny(안전측).
```
new:
```
  절). SNI는 TCP와 동일하게 guest-asserted다. 재조립은 데이터그램 수에 하드
  제한이 없다 — 실질 상한은 per-flow 8192B 캡(≈7 데이터그램)이며, 이를 초과하는
  ClientHello만 fail-closed deny(안전측, 주류 클라 미해당).
```

- [ ] **Step 10: `docs/operations/2026-07-14-quic-sni-handoff.md` — 서술**

old:
```
LRU(`quicFlowLRU`)로 bound한다. **3개 이상 데이터그램에 걸치는 매우 큰
  ClientHello는 v1 미지원**(fail-closed deny, 후속 후보).
```
new:
```
LRU(`quicFlowLRU`)로 bound한다. 재조립은 데이터그램 수에 하드 제한이 없다
  (3개 이상 데이터그램 ClientHello 지원); 실질 상한은 per-flow 8192B 캡이며 이를
  초과하는 ClientHello만 fail-closed deny(주류 클라 미해당 — 2026-07-15 증명·문서교정).
```

- [ ] **Step 11: `docs/operations/2026-07-14-quic-sni-handoff.md` — 표 행**

old: `| 3개 이상 데이터그램에 걸치는 ClientHello | **v1 미지원** — fail-closed deny(후속 후보) |`
new: `| >8192B ClientHello (per-flow 바이트 캡 초과) | fail-closed deny(주류 클라 미해당; 3+ 데이터그램 재조립 자체는 지원) |`

- [ ] **Step 12: `docs/operations/2026-07-14-quic-sni-handoff.md` — Follow-Up #1**

old:
```
1. **3개 이상 Initial 데이터그램에 걸치는 ClientHello 지원** — 현재 v1은
   2-데이터그램 재조립만 지원한다. 매우 큰 확장(예: 다중 인증서 체인 힌트,
   대형 ALPN 목록)을 쓰는 클라이언트는 계속 fail-closed deny로 관측될 수
   있다. 실사용 빈도를 관찰한 뒤 3+ 지원 여부를 재검토한다.
```
new:
```
1. **3+ 데이터그램 ClientHello — 이미 지원(2026-07-15 증명)** — 재조립은
   데이터그램 수에 하드 제한이 없다(N-데이터그램 회귀 테스트로 증명). 잔여는
   per-flow 8192B 캡을 초과하는 ClientHello뿐이며 주류 클라(PQ ~1.5KB)는
   해당 없다 — 캡 상향은 YAGNI. 3-데이터그램 kernel connmark 경로 KVM e2e
   실증은 별도 follow-up(로직은 2-데이터그램 e2e로 이미 실증).
```

- [ ] **Step 13: `docs/superpowers/specs/2026-07-14-quic-sni-filter-design.md` — 후속 후보 정정 주(historical 보존)**

old: `- **후속 후보**: proto별 metric label, 새 QUIC 버전 salt/label 추가, 매우 큰 ClientHello(3+ 데이터그램) 한계 재검토.`
new: `- **후속 후보**: proto별 metric label, 새 QUIC 버전 salt/label 추가. (정정 2026-07-15: "3+ 데이터그램 한계"는 오기재였다 — 재조립은 데이터그램 수 무제한이고 실질 상한은 8192B 캡뿐이다. 상세 `docs/superpowers/specs/2026-07-15-quic-ndatagram-verify-design.md`.)`

- [ ] **Step 14: Verify no stale claim remains (except the historical corrected note)**

Run:
```bash
grep -rn "v1 미지원\|2 데이터그램까지만\|3개 이상 데이터그램에 걸치는 ClientHello 지원\|3개 이상 Initial 데이터그램에 걸치는 ClientHello 지원" docs/ CONTEXT.md
```
Expected: **아무 것도 매치되지 않는다**(빈 출력). (교정 주에 남는 "3+ 데이터그램 한계는 오기재" 같은 문구는 위 패턴에 걸리지 않는다.)

추가로 잔여 "3개 이상 데이터그램" 언급이 전부 *지원됨* 맥락인지 육안 확인:
```bash
grep -rn "3개 이상\|3+ 데이터그램\|8192" docs/adr/0002-egress-sni-transparent-filter.md CONTEXT.md docs/operations/security-policy.md docs/operations/runbook.md docs/operations/2026-07-14-quic-sni-handoff.md docs/PUBLIC_RELEASE_BOUNDARY.md
```
Expected: 각 매치가 "하드 제한 없음/지원/8192B 초과만 deny" 맥락(미지원·deny 서술 아님).

- [ ] **Step 15: Commit**

```bash
git add docs/adr/0002-egress-sni-transparent-filter.md CONTEXT.md docs/PUBLIC_RELEASE_BOUNDARY.md docs/operations/runbook.md docs/operations/security-policy.md docs/operations/2026-07-14-quic-sni-handoff.md docs/superpowers/specs/2026-07-14-quic-sni-filter-design.md
git commit -m "docs(quic): correct stale '3+ datagram unsupported' claim (N-datagram already supported, cap is 8192B)"
```

---

## 최종 검증 (모든 태스크 후)

- [ ] `go test ./internal/network/quic/... -race` — green(신규 N-데이터그램 포함).
- [ ] `go test ./cmd/goose-daemon/... -race` — green(디스패치 무회귀).
- [ ] `go build ./... && go vet ./...` — clean.
- [ ] `gofmt -l internal/network/quic/` — 빈 출력.
- [ ] Step 14의 grep 두 개 — stale 클레임 0.
- [ ] 최종 whole-branch 리뷰: 런타임 로직 무변경(주석·테스트·문서만), 교정 문구 일관, 관측 사실 보존, 8192B 캡 유지 확인. full KVM 게이트는 런타임 로직 무변경이라 불요(리뷰에서 재확인).
