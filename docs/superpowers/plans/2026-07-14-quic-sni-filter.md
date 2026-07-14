# QUIC/UDP:443 SNI 필터 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `allow_sni` egress 정책을 UDP:443(QUIC/HTTP3)에도 적용한다 — QUIC Initial의 암호화된 ClientHello를 복호·파싱해 SNI를 강제한다.

**Architecture:** 신규 `internal/network/quic` 패키지가 QUIC Initial을 자체 복호(공개 DCID 파생 키, HKDF+AES-128-GCM+header protection)해 CRYPTO 프레임에서 TLS ClientHello를 얻고, `internal/network/sni`의 추출된 handshake 파서를 재사용한다. verdict 루프·dispatch 배관을 UDP로 확장한다.

**Tech Stack:** Go, `crypto/aes`·`crypto/cipher`(AES-128-GCM, AES-ECB), `golang.org/x/crypto/hkdf`, 기존 `go-nfqueue/v2` verdict 루프.

**설계 근거:** [docs/superpowers/specs/2026-07-14-quic-sni-filter-design.md](../specs/2026-07-14-quic-sni-filter-design.md)

## Global Constraints

- **신규 direct 의존은 `golang.org/x/crypto` 하나만** (이미 indirect — direct 승격). QUIC 라이브러리 도입 금지.
- **fail-closed 일관**: 복호 불가·미지원 버전·non-Initial·no-SNI·데이터그램 분할은 전부 terminal error(호출측 DROP).
- **crypto 정확성은 RFC golden 벡터로 담보**: 자체 crypto는 RFC 9001 Appendix A.1(v1)·RFC 9369 Appendix A(v2) 벡터 유닛 통과가 필수. 상수(salt/label/version)는 RFC 원문 대조.
- **패닉 금지**: 모든 가변길이/varint 파싱은 경계 검증(fuzz-safe).
- **커밋 trailer 금지**(anvil 관례). 브랜치 `feature/quic-sni-filter`.
- QUIC 상수: v1 version=`0x00000001` salt=`38762cf7f55934b34d179ae6a4c80cadccbb7f0a` labels=`quic key`/`quic iv`/`quic hp`; v2 version=`0x6b3343cf` salt=`0dede3def700a6db819381be6e269dcbf9bd2ed9` labels=`quicv2 key`/`quicv2 iv`/`quicv2 hp`; 공통 `client in`. AEAD=AES-128-GCM(key16/iv12/tag16), HP=AES-128-ECB.

---

## File Structure

- `internal/network/sni/parser.go` (MODIFY): `ParseHandshakeSNI(hs []byte)` 추출.
- `internal/network/sni/parser_test.go` (MODIFY): bare-handshake 유닛.
- `internal/network/quic/hkdf.go` (CREATE): HKDF-Expand-Label + `deriveClientInitialKeys(dcid []byte, version uint32)`.
- `internal/network/quic/initial.go` (CREATE): long-header 파싱 + HP 제거 + AEAD 복호 → `decryptInitial(datagram) (payload []byte, err error)`.
- `internal/network/quic/frames.go` (CREATE): CRYPTO 재조립 + `ParseInitialSNI(datagram []byte) (string, error)`.
- `internal/network/quic/*_test.go` (CREATE): RFC 벡터 + 라운드트립 + 경계.
- `cmd/goose-daemon/sni_verdict.go` (MODIFY): `parseIPv4UDP` + 훅 proto 분기 + QUIC decide.
- `cmd/goose-daemon/sni_verdict_test.go` (MODIFY): UDP decide 라우팅.
- `cmd/goose-daemon/egress_policy.go` (MODIFY): UDP:443 dispatch/fastpath.
- `cmd/goose-daemon/egress_policy_test.go` (MODIFY): UDP dispatch 유닛.
- `scripts/anvil-quic-sni-e2e.sh` (CREATE): HTTP/3 KVM e2e.
- `go.mod`/`go.sum` (MODIFY): x/crypto direct 승격.
- 문서 (Task 8): ADR-0002 QUIC 행, handoff, runbook, PUBLIC_RELEASE_BOUNDARY, CONTEXT.

---

### Task 1: `sni.ParseHandshakeSNI` 추출 (behavior-preserving 리팩터)

**Files:**
- Modify: `internal/network/sni/parser.go` (`ParseClientHelloSNI` L20-, 함수 분리)
- Test: `internal/network/sni/parser_test.go`

**Interfaces:**
- Produces: `func ParseHandshakeSNI(hs []byte) (string, error)` — TLS handshake 메시지(`0x01` ClientHello 헤더로 시작)에서 lowercased SNI 추출. `ErrIncomplete`(더 필요)·`ErrNoSNI`·기타 terminal. `quic` 패키지와 `ParseClientHelloSNI`가 공유.
- `ParseClientHelloSNI(b []byte)`는 TLS record(`0x16`) 검증·strip 후 `ParseHandshakeSNI(hs)` 호출 — 동작 불변.

- [ ] **Step 1: 실패 테스트 추가** (`parser_test.go`)
```go
func TestParseHandshakeSNI_BareClientHello(t *testing.T) {
	// buildClientHello는 기존 test helper — TLS record가 아니라 handshake 메시지(0x01..)를 만든다.
	hs := buildClientHello("api.anthropic.com") // 기존 helper가 record를 만들면, record 헤더 5바이트를 벗겨 hs로 전달
	got, err := ParseHandshakeSNI(stripRecordHeader(hs))
	if err != nil || got != "api.anthropic.com" {
		t.Fatalf("ParseHandshakeSNI = %q, %v", got, err)
	}
}
```
(기존 `buildClientHello`가 TLS record를 반환하면 `stripRecordHeader(b []byte) []byte { return b[5:] }` 로컬 helper 추가. record를 안 만들면 그대로 전달.)

- [ ] **Step 2: 실패 확인**
```bash
go test ./internal/network/sni/ -run BareClientHello 2>&1 | head
```
Expected: 컴파일 실패(`ParseHandshakeSNI` 부재).

- [ ] **Step 3: 추출 구현** (`parser.go`)
`ParseClientHelloSNI`의 handshake-파싱부(현 L33 `// Handshake header` ~ 함수 끝의 SNI 추출 로직 전체)를 새 함수로 옮긴다:
```go
// ParseHandshakeSNI parses a TLS handshake message (starting at the ClientHello
// handshake header, msg_type 0x01) and returns the lowercased server_name.
// QUIC carries the handshake directly in CRYPTO frames (no TLS record), so the
// QUIC path calls this after reassembly; ParseClientHelloSNI calls it after
// stripping the TLS record layer. ErrIncomplete = need more bytes; ErrNoSNI and
// other errors are terminal.
func ParseHandshakeSNI(hs []byte) (string, error) {
	if len(hs) < 4 {
		return "", ErrIncomplete
	}
	if hs[0] != 0x01 {
		return "", fmt.Errorf("not a client_hello (msg_type 0x%02x)", hs[0])
	}
	hsLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
	body := hs[4:]
	if len(body) < hsLen {
		return "", ErrIncomplete
	}
	body = body[:hsLen]
	// ... (현 ParseClientHelloSNI의 client_version/random/session_id/.../server_name 추출 로직을 그대로 이동)
}
```
`ParseClientHelloSNI`는 record 처리 후 위임:
```go
func ParseClientHelloSNI(b []byte) (string, error) {
	if len(b) < 5 { return "", ErrIncomplete }
	if b[0] != 0x16 { return "", fmt.Errorf("not a handshake record (type 0x%02x)", b[0]) }
	recLen := int(binary.BigEndian.Uint16(b[3:5]))
	if len(b) < 5+recLen { return "", ErrIncomplete }
	return ParseHandshakeSNI(b[5 : 5+recLen])
}
```

- [ ] **Step 4: 통과 확인**
```bash
go test -race ./internal/network/sni/ 2>&1 | tail -5
```
Expected: PASS(신규 + 기존 `parser_test.go` 전부 — 리팩터 동작 불변 회귀).

- [ ] **Step 5: Commit**
```bash
git add internal/network/sni/parser.go internal/network/sni/parser_test.go
git commit -m "refactor(sni): extract ParseHandshakeSNI for TLS-record-less reuse (QUIC)"
```

---

### Task 2: QUIC Initial 키 파생 (HKDF)

**Files:**
- Create: `internal/network/quic/hkdf.go`, `internal/network/quic/hkdf_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces: `func deriveClientInitialKeys(dcid []byte, version uint32) (key, iv, hp []byte, err error)` — key(16)/iv(12)/hp(16). 미지원 version → error. `initial.go`가 소비.
- Consumes: `golang.org/x/crypto/hkdf`.

- [ ] **Step 1: 의존 추가**
```bash
go get golang.org/x/crypto/hkdf 2>&1 | tail; go mod tidy 2>&1 | tail
git diff go.mod | grep -E "^\+" | grep x/crypto
```
Expected: `golang.org/x/crypto`가 direct require로 승격(이미 indirect). 신규 다른 direct 의존 없음(있으면 BLOCKED 보고).

- [ ] **Step 2: 실패 테스트** (`hkdf_test.go`) — **RFC 9001 §5.2 golden**
```go
package quic

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// RFC 9001 Appendix A.1: DCID 0x8394c8f03e515708 로 파생한 client Initial 키.
func TestDeriveClientInitialKeys_RFC9001_V1(t *testing.T) {
	dcid := mustHex("8394c8f03e515708")
	key, iv, hp, err := deriveClientInitialKeys(dcid, 0x00000001)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	// RFC 9001 A.1 공표값:
	wantKey := mustHex("1f369613dd76d5467730efcbe3b1a22d")
	wantIV := mustHex("fa044b2f42a3fd3b46fb255c")
	wantHP := mustHex("9f50449e04a0e810283a1e9933adedd2")
	if !bytes.Equal(key, wantKey) || !bytes.Equal(iv, wantIV) || !bytes.Equal(hp, wantHP) {
		t.Fatalf("keys mismatch\n key=%x\n iv=%x\n hp=%x", key, iv, hp)
	}
}

func TestDeriveClientInitialKeys_UnsupportedVersion(t *testing.T) {
	if _, _, _, err := deriveClientInitialKeys(mustHex("8394c8f03e515708"), 0xdeadbeef); err == nil {
		t.Fatal("unsupported version must error (fail-closed)")
	}
}
```
(구현 시 RFC 9001 A.1의 wantKey/IV/HP 정확 hex를 원문과 재확인. v2는 Task 4에서 RFC 9369 벡터로 커버.)

- [ ] **Step 3: 실패 확인**
```bash
go test ./internal/network/quic/ -run DeriveClientInitial 2>&1 | head
```
Expected: 컴파일 실패.

- [ ] **Step 4: 구현** (`hkdf.go`)
```go
package quic

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

var (
	saltV1 = []byte{0x38,0x76,0x2c,0xf7,0xf5,0x59,0x34,0xb3,0x4d,0x17,0x9a,0xe6,0xa4,0xc8,0x0c,0xad,0xcc,0xbb,0x7f,0x0a}
	saltV2 = []byte{0x0d,0xed,0xe3,0xde,0xf7,0x00,0xa6,0xdb,0x81,0x93,0x81,0xbe,0x6e,0x26,0x9d,0xcb,0xf9,0xbd,0x2e,0xd9}
)

// versionParams는 version별 salt와 key/iv/hp label을 준다.
func versionParams(version uint32) (salt []byte, keyLabel, ivLabel, hpLabel string, ok bool) {
	switch version {
	case 0x00000001:
		return saltV1, "quic key", "quic iv", "quic hp", true
	case 0x6b3343cf:
		return saltV2, "quicv2 key", "quicv2 iv", "quicv2 hp", true
	default:
		return nil, "", "", "", false
	}
}

// hkdfExpandLabel = TLS 1.3 HKDF-Expand-Label (RFC 8446 §7.1): label에 "tls13 " 접두.
func hkdfExpandLabel(secret []byte, label string, length int) []byte {
	full := "tls13 " + label
	info := make([]byte, 0, 2+1+len(full)+1)
	info = binary.BigEndian.AppendUint16(info, uint16(length))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0) // zero-length context
	out := make([]byte, length)
	r := hkdf.Expand(sha256.New, secret, info)
	io.ReadFull(r, out)
	return out
}

func deriveClientInitialKeys(dcid []byte, version uint32) (key, iv, hp []byte, err error) {
	salt, keyLabel, ivLabel, hpLabel, ok := versionParams(version)
	if !ok {
		return nil, nil, nil, fmt.Errorf("unsupported quic version 0x%08x", version)
	}
	initialSecret := hkdf.Extract(sha256.New, dcid, salt)
	clientSecret := hkdfExpandLabel(initialSecret, "client in", 32)
	key = hkdfExpandLabel(clientSecret, keyLabel, 16)
	iv = hkdfExpandLabel(clientSecret, ivLabel, 12)
	hp = hkdfExpandLabel(clientSecret, hpLabel, 16)
	return key, iv, hp, nil
}
```

- [ ] **Step 5: 통과 확인**
```bash
go test -race ./internal/network/quic/ -run 'DeriveClientInitial' -v 2>&1 | tail -8
```
Expected: PASS(RFC 벡터 일치). 불일치면 상수/label/Expand-Label 형식 RFC 재대조.

- [ ] **Step 6: Commit**
```bash
git add internal/network/quic/hkdf.go internal/network/quic/hkdf_test.go go.mod go.sum
git commit -m "feat(quic): Initial key derivation (HKDF, QUICv1/v2 salt+labels, RFC 9001 vector)"
```

---

### Task 3: QUIC Initial 복호 (long-header + HP 제거 + AEAD)

**Files:**
- Create: `internal/network/quic/initial.go`, `internal/network/quic/initial_test.go`

**Interfaces:**
- Produces: `func decryptInitial(datagram []byte) (payload []byte, err error)` — 첫 QUIC Initial 패킷을 복호해 QUIC frame payload 반환. non-Initial/미지원버전/복호실패 → error. `frames.go`가 소비. (coalesced: 첫 Initial만.)
- Consumes: `deriveClientInitialKeys`(Task 2).

- [ ] **Step 1: 실패 테스트** (`initial_test.go`) — **RFC 9001 A.1 golden**
```go
func TestDecryptInitial_RFC9001_V1(t *testing.T) {
	// RFC 9001 Appendix A.1의 "client Initial packet" 전체 hex(원문에서 그대로 복사).
	packet := mustHex("c000000001088394c8f03e5157080000449e" /* ...RFC 9001 A.1 full packet... */)
	payload, err := decryptInitial(packet)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	// RFC 9001 A.1의 복호된 payload는 CRYPTO 프레임(0x06)으로 시작 + PADDING.
	if len(payload) == 0 || payload[0] != 0x06 {
		t.Fatalf("decrypted payload should start with CRYPTO frame, got %x...", payload[:min(8, len(payload))])
	}
}

func TestDecryptInitial_NotLongHeader(t *testing.T) {
	if _, err := decryptInitial([]byte{0x40, 0x00}); err == nil { // short-header
		t.Fatal("non-Initial must error")
	}
}
```
**구현 주의**: RFC 9001 A.1 전체 packet hex(약 1200B)는 RFC 원문에서 정확히 복사할 것(기억 재현 금지). 복호 plaintext도 RFC A.1에 공표 — 필요 시 앞부분을 비교.

- [ ] **Step 2: 실패 확인** → 컴파일 실패.

- [ ] **Step 3: 구현** (`initial.go`)
long-header 파싱(first byte 0x80 long + type=Initial=0b00 for v1 / v2는 type 인코딩 다름 — RFC 9369 §3.2 대조), version, DCID/SCID, token(varint len), length(varint), pn offset 계산 → HP 제거(암호문 sample[pn_off+4 : +20] AES-ECB로 mask, first byte 하위 + PN unmask) → nonce=iv XOR PN → AES-128-GCM Open(AAD=header). varint 디코더 헬퍼 포함. 모든 경계 검증(패닉 금지).
```go
package quic

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
)

func readVarint(b []byte) (val uint64, n int, ok bool) {
	if len(b) == 0 { return 0, 0, false }
	prefix := b[0] >> 6
	length := 1 << prefix
	if len(b) < length { return 0, 0, false }
	val = uint64(b[0] & 0x3f)
	for i := 1; i < length; i++ { val = val<<8 | uint64(b[i]) }
	return val, length, true
}

func decryptInitial(datagram []byte) ([]byte, error) {
	// (long-header bit + Initial type 검증, version/DCID/SCID/token/length/pn-offset,
	//  HP 제거(AES-ECB sample), nonce, AES-128-GCM Open — RFC 9001 §5.3~5.4.
	//  구현은 위 test가 RFC A.1 벡터로 검증. 경계마다 ok/err.)
	// 핵심 스텁 시그니처만 표기; 실제 로직은 RFC + golden test로 채운다.
	...
}
```
(이 태스크의 correctness gate는 RFC A.1 golden test다 — test-first로 RFC를 따라 구현.)

- [ ] **Step 4: 통과 확인**
```bash
go test -race ./internal/network/quic/ -run 'DecryptInitial' -v 2>&1 | tail -8
```
Expected: PASS(RFC A.1 복호 일치).

- [ ] **Step 5: Commit**
```bash
git add internal/network/quic/initial.go internal/network/quic/initial_test.go
git commit -m "feat(quic): decrypt Initial packet (long-header, header protection, AES-GCM; RFC 9001 A.1)"
```

---

### Task 4: CRYPTO 재조립 + `ParseInitialSNI` (end-to-end)

**Files:**
- Create: `internal/network/quic/frames.go`, `internal/network/quic/frames_test.go`

**Interfaces:**
- Produces: `func ParseInitialSNI(datagram []byte) (string, error)` — QUIC Initial 데이터그램 → lowercased SNI. 모든 실패는 terminal(fail-closed). verdict 루프(Task 5)가 소비.
- Consumes: `decryptInitial`(Task 3), `sni.ParseHandshakeSNI`(Task 1).

- [ ] **Step 0: test-support 헬퍼** (`quic/testsupport.go`, 일반 .go 파일 — cmd/goose-daemon 테스트가 cross-package 재사용)
```go
package quic

// BuildInitialForTest constructs a valid QUIC Initial datagram carrying the given
// TLS handshake bytes (ClientHello), encrypted for the given version — the inverse
// of ParseInitialSNI. For tests only (this package's + the verdict loop's).
func BuildInitialForTest(dcid []byte, version uint32, handshake []byte) []byte { ... }
// buildClientHelloHandshake / ...NoSNI: sni.buildClientHello 형식(record 없는 0x01 handshake)의 최소 인코더.
```
(버전이 미지원이면 BuildInitialForTest는 v1 키로 만들고 헤더의 version 필드만 인자 값으로 세팅 — decrypt가 미지원 version에서 실패하는지 테스트 가능.)

- [ ] **Step 1: 실패 테스트** (`frames_test.go`) — 라운드트립 + v2 + fail-closed
```go
func TestParseInitialSNI_RoundTrip_V1(t *testing.T) {
	ch := buildClientHelloHandshake("cdn.example.com") // record 없는 handshake 메시지
	dg := BuildInitialForTest(mustHex("8394c8f03e515708"), 0x00000001, ch)
	got, err := ParseInitialSNI(dg)
	if err != nil || got != "cdn.example.com" {
		t.Fatalf("ParseInitialSNI = %q, %v", got, err)
	}
}
func TestParseInitialSNI_RoundTrip_V2(t *testing.T) {
	ch := buildClientHelloHandshake("api.anthropic.com")
	dg := BuildInitialForTest(mustHex("8394c8f03e515708"), 0x6b3343cf, ch)
	if got, err := ParseInitialSNI(dg); err != nil || got != "api.anthropic.com" {
		t.Fatalf("v2 ParseInitialSNI = %q, %v", got, err)
	}
}
func TestParseInitialSNI_FailClosed(t *testing.T) {
	for name, dg := range map[string][]byte{
		"empty":           {},
		"unsupported-ver": BuildInitialForTest(mustHex("8394c8f03e515708"), 0xdeadbeef, buildClientHelloHandshake("x.com")),
		"no-sni":          BuildInitialForTest(mustHex("8394c8f03e515708"), 0x00000001, buildClientHelloHandshakeNoSNI()),
	} {
		if _, err := ParseInitialSNI(dg); err == nil {
			t.Fatalf("%s: expected fail-closed error", name)
		}
	}
}
```
(RFC A.1 golden은 Task 3가 복호를 검증하므로, 여기선 SNI 추출 파이프라인을 라운드트립으로 검증.)

- [ ] **Step 2: 실패 확인** → 컴파일 실패.

- [ ] **Step 3: 구현** (`frames.go`)
```go
package quic

import (
	"errors"
	"fmt"

	"ephemera/internal/network/sni"
)

var errIncompleteInDatagram = errors.New("quic: clienthello not complete within datagram")

// ParseInitialSNI decrypts one QUIC Initial datagram and extracts the SNI.
func ParseInitialSNI(datagram []byte) (string, error) {
	payload, err := decryptInitial(datagram)
	if err != nil {
		return "", err
	}
	hs, err := reassembleCryptoFrames(payload)
	if err != nil {
		return "", err
	}
	name, err := sni.ParseHandshakeSNI(hs)
	if err != nil {
		// ErrIncomplete(데이터그램 내 미완결)도 terminal deny로 취급.
		return "", fmt.Errorf("quic clienthello: %w", err)
	}
	return name, nil
}

// reassembleCryptoFrames는 복호된 QUIC payload의 CRYPTO 프레임(0x06)을 offset순으로
// 이어붙인다. PADDING(0x00)/PING(0x01)/ACK(0x02,0x03) 등은 skip. offset gap이나
// 잘림이면 errIncompleteInDatagram. (varint offset/length, 경계 검증.)
func reassembleCryptoFrames(payload []byte) ([]byte, error) { ... }
```

- [ ] **Step 4: 통과 확인**
```bash
go test -race ./internal/network/quic/ -v 2>&1 | tail -12
```
Expected: PASS(라운드트립 v1/v2 + fail-closed + RFC A.1 복호 + hkdf). fuzz 추가 권장(`FuzzParseInitialSNI`, panic-free).

- [ ] **Step 5: Commit**
```bash
git add internal/network/quic/frames.go internal/network/quic/frames_test.go internal/network/quic/testsupport.go
git commit -m "feat(quic): CRYPTO frame reassembly + ParseInitialSNI (round-trip v1/v2, fail-closed)"
```

---

### Task 5: verdict 루프 UDP 확장

**Files:**
- Modify: `cmd/goose-daemon/sni_verdict.go` (`parseIPv4UDP`, Start 훅 proto 분기, QUIC decide)
- Modify: `cmd/goose-daemon/sni_verdict_test.go`

**Interfaces:**
- Consumes: `quic.ParseInitialSNI`(Task 4), 기존 `sniVerdictLoop`/`resolveEntry`/`classifyParsedSNI`/`applyVerdict`/`sniRegistryEntry`.
- Produces: UDP:443 QUIC 패킷에 대해 decide → sniAcceptMark(connmark) or sniDrop(silent, RST 없음).

- [ ] **Step 1: 실패 테스트** (`sni_verdict_test.go`)
```go
func TestSNIDecideQUICRouting(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com", "*.example.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})

	// decideQUIC(srcIP, udpPayload) — QUIC Initial payload를 quic.ParseInitialSNI로 판정.
	allow := buildQUICInitialForTest("10.0.1.10", "api.anthropic.com") // test helper: quic 패키지 라운드트립 재사용
	if d := l.decideQUIC("10.0.1.10", allow); d.Action != sniAcceptMark {
		t.Fatalf("allowed QUIC SNI -> %v (%s)", d.Action, d.Reason)
	}
	deny := buildQUICInitialForTest("10.0.1.10", "evil.test")
	if d := l.decideQUIC("10.0.1.10", deny); d.Action != sniDrop || d.Reason != "egress_sni_denied" {
		t.Fatalf("denied QUIC -> %v/%s", d.Action, d.Reason)
	}
	if d := l.decideQUIC("10.0.1.99", allow); d.Action != sniDrop { // 미등록
		t.Fatal("unregistered QUIC must fail closed")
	}
	if d := l.decideQUIC("10.0.1.10", []byte{0x40, 0x00}); d.Action != sniDrop { // non-Initial
		t.Fatal("unparseable QUIC must fail closed")
	}
}
```
(`buildQUICInitialForTest`는 `quic` 패키지의 exported 테스트 헬퍼가 없으면, sni_verdict_test.go 안에서 quic 공개 API로 만들 수 없으므로 — quic 패키지에 `BuildInitialForTest(dcid, version, handshake)` 얇은 exported helper를 추가하거나, 이 테스트는 quic 라운드트립 벡터를 상수로 넣는다. 결정: quic 패키지에 test 파일이 아닌 곳에 exported helper를 두지 말고, sni_verdict_test.go가 `quic.ParseInitialSNI`가 받는 유효 데이터그램을 만드는 최소 헬퍼를 quic의 exported 저수준(derive... 은 unexported)로는 불가 → **quic 패키지에 `//go:build` 없는 test-support 파일 `quic/testsupport.go`에 `func BuildInitialForTest(...)`를 exported로 제공**하고 verdict 테스트가 재사용. 이 결정은 Task 4에서 `buildTestInitial`을 `BuildInitialForTest`로 exported 배치.)

- [ ] **Step 2: 실패 확인** → 컴파일 실패(`decideQUIC` 부재).

- [ ] **Step 3: 구현** (`sni_verdict.go`)
- `parseIPv4UDP(pkt) (ipv4UDP, error)`: `pkt[9]==IPPROTO_UDP` 검증, IHL 후 UDP 헤더(sport/dport/len) + payload. 경계 검증.
- `decideQUIC(srcIP string, payload []byte) sniDecision`:
```go
func (l *sniVerdictLoop) decideQUIC(srcIP string, payload []byte) sniDecision {
	entry, ok := l.resolveEntry(srcIP)
	if !ok {
		return sniDecision{Action: sniDrop, Reason: "unregistered_source"}
	}
	name, err := quic.ParseInitialSNI(payload)
	if err != nil {
		return sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"} // fail-closed
	}
	return l.classifyParsedSNI(entry, name) // 기존 TCP와 공유: ∈→acceptMark, ∉→denied
}
```
- Start 훅: `parseIPv4TCP` 대신 proto 분기. `t.dport`(UDP는 443) 확인 후 `decideQUIC`. **applyVerdict의 sniDrop이 UDP에선 injectRST 호출하지 않도록** — proto를 applyVerdict에 전달하거나, UDP 경로는 별도 `applyVerdictNoRST` 사용(injectRST는 TCP `ipv4TCP`의 seq/ack 필요, UDP엔 없음). 최소: decideQUIC 결과를 `setAccept`/`SetVerdictWithConnMark`/`setDrop(nf,id, ipv4TCP{})`(빈 t → injectRST가 no-op이 되도록 injectRST가 t.ackSeq==0 등에서 skip하거나, UDP용 setDropNoRST 추가). **결정**: `setDropNoRST(nf, id)` 추가(단순 NfDrop), UDP 경로가 사용. recordVerdict는 공유.

- [ ] **Step 4: 통과 확인**
```bash
go test -race ./cmd/goose-daemon/ -run 'SNIDecide|QUIC|Egress' -v 2>&1 | tail -20
go build ./cmd/goose-daemon/
```
Expected: PASS + build. (netlink/UDP 커널 경로는 Task 7 e2e.)

- [ ] **Step 5: Commit**
```bash
git add cmd/goose-daemon/sni_verdict.go cmd/goose-daemon/sni_verdict_test.go
git commit -m "feat(egress): QUIC UDP:443 verdict path (parseIPv4UDP, decideQUIC, silent drop)"
```

---

### Task 6: dispatch UDP:443 대칭 규칙

**Files:**
- Modify: `cmd/goose-daemon/egress_policy.go` (`planProfileEgressCommands` SNI 블록)
- Modify: `cmd/goose-daemon/egress_policy_test.go`

**Interfaces:**
- Produces: `allow_sni>0`일 때 UDP:443 dispatch+fastpath `egressCommand` 2개 추가(TCP 블록과 대칭, 같은 queue/connmark). empty면 미생성.

- [ ] **Step 1: 실패 테스트**
```go
func TestPlanProfileEgressCommandsEmitsUDPSNIDispatch(t *testing.T) {
	commands, _ := planProfileEgressCommands("vm-1", "10.0.1.10", egressProfile{AllowSNI: []string{"api.anthropic.com"}})
	joined := joinCommands(commands)
	for _, want := range []string{
		"iptables -I FORWARD -s 10.0.1.10 -p udp --dport 443 -m connmark --mark 0x534e49 -j ACCEPT -m comment --comment anvil-egress-vm-1-sni-udp-fastpath",
		"iptables -I FORWARD -s 10.0.1.10 -p udp --dport 443 -m connmark ! --mark 0x534e49 -j NFQUEUE --queue-num 88 -m comment --comment anvil-egress-vm-1-sni-udp-nfqueue",
	} {
		if !strings.Contains(joined, want) { t.Fatalf("missing %q\n%s", want, joined) }
	}
}
func TestPlanProfileEgressCommandsNoUDPSNIWhenEmpty(t *testing.T) {
	commands, _ := planProfileEgressCommands("vm-1", "10.0.1.10", egressProfile{AllowCIDRs: []string{"203.0.113.10/32"}})
	if strings.Contains(joinCommands(commands), "udp --dport 443") {
		t.Fatal("empty allow_sni must not emit UDP:443 SNI rules")
	}
}
```

- [ ] **Step 2: 실패 확인** → FAIL(missing udp fastpath).

- [ ] **Step 3: 구현** — SNI 블록(현 TCP nfqueue/fastpath append 자리)에 UDP 2개 추가. TCP 블록 바로 뒤, 같은 순서 계약(slice에 `[udp-nfqueue, udp-fastpath]`도 dispatch-먼저). `-p udp` 외 동일.
```go
	if len(profile.AllowSNI) > 0 {
		q := strconv.Itoa(sniQueueNum())
		commands = append(commands,
			// TCP (기존)
			egressCommand{Name: "iptables", Args: []string{"-I","FORWARD","-s",guestIP,"-p","tcp","--dport","443","-m","connmark","!","--mark",sniApprovedConnmark,"-j","NFQUEUE","--queue-num",q,"-m","comment","--comment",prefix+"-sni-nfqueue"}},
			egressCommand{Name: "iptables", Args: []string{"-I","FORWARD","-s",guestIP,"-p","tcp","--dport","443","-m","connmark","--mark",sniApprovedConnmark,"-j","ACCEPT","-m","comment","--comment",prefix+"-sni-fastpath"}},
			// UDP (신규, QUIC)
			egressCommand{Name: "iptables", Args: []string{"-I","FORWARD","-s",guestIP,"-p","udp","--dport","443","-m","connmark","!","--mark",sniApprovedConnmark,"-j","NFQUEUE","--queue-num",q,"-m","comment","--comment",prefix+"-sni-udp-nfqueue"}},
			egressCommand{Name: "iptables", Args: []string{"-I","FORWARD","-s",guestIP,"-p","udp","--dport","443","-m","connmark","--mark",sniApprovedConnmark,"-j","ACCEPT","-m","comment","--comment",prefix+"-sni-udp-fastpath"}},
		)
	}
```

- [ ] **Step 4: 통과 확인**
```bash
go test -race ./cmd/goose-daemon/ -run 'Egress|SNI|UDP' -v 2>&1 | tail -20
```
Expected: PASS(신규 UDP 2 + 기존 TCP/rollback/recovery 회귀 없음 — `flushEgressByComment`의 `anvil-egress-<vmID>-` prefix가 `-sni-udp-*`도 커버).

- [ ] **Step 5: Commit**
```bash
git add cmd/goose-daemon/egress_policy.go cmd/goose-daemon/egress_policy_test.go
git commit -m "feat(egress): emit UDP:443 NFQUEUE dispatch + fast-path for allow_sni (QUIC)"
```

---

### Task 7: KVM e2e (HTTP/3)

**Files:**
- Create: `scripts/anvil-quic-sni-e2e.sh`

**Interfaces:**
- Consumes: Task 1-6(빌드된 daemon), 기존 e2e 하니스 패턴(`scripts/anvil-egress-sni-e2e.sh`).
- Produces: 단독 KVM e2e(root+KVM). 판정=exit code + 마지막 "passed" 라인.

- [ ] **Step 1~2: 골격 + Phase 0** — `anvil-egress-sni-e2e.sh`를 복제·개조. allow_sni profile로 VM spawn. `iptables -S FORWARD`에 `-sni-udp-nfqueue`/`-sni-udp-fastpath` 규칙 존재 확인. **guest HTTP/3 클라 가용성 확인**: golden image에 `curl --http3`(HTTP/3 빌드) 또는 Go QUIC 클라. 없으면 e2e 준비 절차에 설치/빌드 추가(스펙 미결#2).
- [ ] **Step 3: Phase 1 — 허용 QUIC 도달.** guest에서 HTTP/3 지원 허용 도메인(예 `cloudflare.com`, `ANVIL_QUIC_E2E_ALLOW_DOMAIN` override)에 `curl --http3 -sS --max-time 15 https://<allow>/` → 성공(rc 0). `iptables -S`/conntrack에 connmark 0x534e49 확인(fast-path 실효).
- [ ] **Step 4: Phase 2 — 비허용 QUIC 차단.** 비허용 도메인 `curl --http3` → 실패(타임아웃/미도달, non-zero). (silent DROP이라 RST 아님 — 타임아웃 관측.) 판정: 연결 미성립.
- [ ] **Step 5: Phase 3 — 감사.** audit에 `egress_sni_denied sni=<deny>` + redaction spot-check.
- [ ] **Step 6: 실행 검증**
```bash
sudo -n bash scripts/anvil-quic-sni-e2e.sh; echo "exit=$?"
```
Expected: `exit=0` + `All QUIC SNI e2e steps passed ✓`. (golden staleness 우회: 소스가 golden보다 새로우면 재빌드되나 #62 fix로 정상 — 필요 시 `sudo touch artifacts/golden-image.ext4`.)
- [ ] **Step 7: Commit**
```bash
git add scripts/anvil-quic-sni-e2e.sh
git commit -m "test(e2e): QUIC SNI KVM e2e (allow HTTP/3 reaches, deny blocked, audit)"
```

---

### Task 8: 문서

**Files:**
- Modify: `docs/adr/0002-egress-sni-transparent-filter.md`(QUIC 잔여위험 행 + 메커니즘 절 UDP:443 추가), `docs/operations/security-policy.md`, `docs/operations/runbook.md`(QUIC 운영·HTTP/3 e2e), `docs/PUBLIC_RELEASE_BOUNDARY.md`(UDP:443 dispatch + `-sni-udp-*`), `CONTEXT.md`, `RELEASE_NOTES.md`.
- (신규 ADR 불필요 — ADR-0002 확장.)

- [ ] **Step 1**: ADR-0002 — 메커니즘 절에 "UDP:443 QUIC Initial 복호→SNI"와 잔여위험 표에 QUIC 행(미지원버전/non-Initial/복호실패/데이터그램-분할=fail-closed) 추가. QUICv1+v2 범위·자체 crypto·유닛-데이터그램 재조립 계약 명시.
- [ ] **Step 2**: security-policy/runbook/PUBLIC_RELEASE_BOUNDARY에 UDP:443 SNI(QUIC) 표면 반영. runbook에 HTTP/3 e2e 절차 + silent-DROP-후-TCP-fallback 동작 설명.
- [ ] **Step 3**: CONTEXT/RELEASE_NOTES 신규 기능 엔트리. handoff Follow-Up #1(QUIC) → DONE.
- [ ] **Step 4: Commit**
```bash
git add docs/ CONTEXT.md RELEASE_NOTES.md
git commit -m "docs(quic): ADR-0002 QUIC extension + security-policy/boundary/runbook, RELEASE_NOTES"
```

---

## 최종 검증 (전체 슬라이스)
- [ ] `go build ./... && go vet ./... && gofmt -l . | grep -v '^web/'` — clean
- [ ] `go test -race ./internal/network/... ./cmd/...` — PASS(특히 `internal/network/quic`, `sni`, `cmd/goose-daemon`)
- [ ] `git diff main -- go.mod` — 신규 direct 의존이 `golang.org/x/crypto`뿐(그 외면 보고)
- [ ] `sudo -n bash scripts/anvil-quic-sni-e2e.sh` — exit 0 + passed
- [ ] 기존 회귀: `go test ./cmd/goose-daemon/ -run 'Egress|SNI|Recover'` + 전체 KVM 게이트 `sudo bash e2e_test.sh` — exit 판정(verdict/dispatch 경로 확장했으므로 필수)
- [ ] secret-scan: `bash scripts/secret-scan.sh` — 신규 유출 없음
- [ ] PR 생성(`feature/quic-sni-filter` → main). **자체 머지 금지.**

## Self-Review 기록
- **Spec 커버리지**: quic 패키지(Task 2-4=키파생/복호/재조립+SNI, RFC 벡터) / sni 재사용(Task 1) / verdict UDP(Task 5) / dispatch(Task 6) / e2e(Task 7) / 문서·잔여위험(Task 8). 스펙 전 절 대응.
- **유닛 vs e2e 분담**: 순수 crypto·파서·decide·dispatch생성은 root 없이 유닛(RFC 골든+라운드트립). netlink/UDP 커널 mark·conntrack fast-path·실 QUIC 왕복은 root+KVM → Task 7 e2e가 유일 실검. Task 3/5 헤더에 이 경계 명시.
- **Type consistency**: `ParseHandshakeSNI`(T1)↔`ParseInitialSNI`(T4)↔`decideQUIC`/`parseIPv4UDP`(T5)↔UDP dispatch comment `-sni-udp-*`(T6) 시그니처·이름 일치. `BuildInitialForTest`(T4 exported)를 T5 테스트가 재사용. connmark `0x534e49`·queue 88 TCP와 공유.
- **알려진 리스크**: (a) RFC 상수/Expand-Label/HP offset 정확성 — golden test가 gate. (b) v2 long-header packet-type 인코딩 v1과 상이(RFC 9369 §3.2) — 복호 전 version별 type 판정. (c) golden image HTTP/3 클라 가용성(스펙 미결#2) — Task 7에서 확정. (d) UDP conntrack connmark 지속성 — Task 7 실증.

---

## [2026-07-14 개정 — 멀티-데이터그램 재조립] Task 6b, 6c

**개정 사유**: KVM e2e(Task 7)가 실측 확인 — Go1.24+/Chrome/Firefox default post-quantum(X25519MLKEM768) ClientHello(~1516B)가 2 Initial 데이터그램에 걸쳐, 유닛-데이터그램 v1이 현대 QUIC **허용** 트래픽을 fail-closed로 잘못 DENY. 멀티-데이터그램 재조립을 in-scope로 확장. Tasks 1-6은 그대로(단일-데이터그램 fast path 유지), 6b/6c가 누적 재조립을 얹는다. Task 7 e2e는 PQ 재검, Task 8 docs는 멀티-데이터그램 반영.

### Task 6b: `quic.InitialReassembler` (멀티-데이터그램 CRYPTO 누적)

**Files:** Create/Modify: `internal/network/quic/frames.go`(reassembler + ParseInitialSNI 위임), `internal/network/quic/testsupport.go`(2-datagram split 헬퍼), `internal/network/quic/frames_test.go`.

**Interfaces:**
- Produces: `type InitialReassembler struct{...}` + `func (r *InitialReassembler) Feed(datagram []byte) (sni string, done bool, err error)`. done=true & sni set → ClientHello 완결·SNI 추출; done=false & err=nil → 더 필요(passthrough); err!=nil → terminal(deny). per-flow 바이트 상한 `maxQuicClientHelloBytes`(예 8192) 초과 → err. `ParseInitialSNI(datagram)`는 `r:=&InitialReassembler{}; sni,done,err:=r.Feed(datagram); done이면 sni, 아니면 errIncompleteInDatagram` 로 **단일-데이터그램 위임**(기존 시그니처·동작 유지, 기존 테스트 회귀 가드).
- Consumes: `decryptInitial`(Task 3), `sni.ParseHandshakeSNI`(Task 1).

- [ ] **Step 1: 실패 테스트** — 2-datagram 재조립
  - testsupport에 `BuildInitialDatagramsForTest(dcid, version, handshake []byte, splitAt int) [][]byte` 추가: handshake를 CRYPTO offset [0,splitAt)·[splitAt,len)로 나눠 2개 Initial 데이터그램 생성(각각 유효 복호 가능, pn=0/pn=1). splitAt이 len 이상이면 1개.
  - `TestInitialReassembler_TwoDatagrams`: 큰 handshake(SNI 포함, splitAt=중간)로 2 데이터그램 생성 → r.Feed(d1)=("",false,nil)[미완결], r.Feed(d2)=(sni,true,nil)[완결]. SNI 일치.
  - `TestInitialReassembler_SingleDatagramStillWorks`: 작은 ClientHello 1 데이터그램 → Feed 즉시 (sni,true,nil). 그리고 `ParseInitialSNI`(단일) 기존 라운드트립/RFC 골든 회귀.
  - `TestInitialReassembler_OversizeTerminal`: 바이트 상한 초과 CRYPTO → err(deny).
  - `TestInitialReassembler_InconsistentOverlapAcrossDatagrams`: 두 데이터그램의 겹치는 offset이 다른 바이트 → err(fail-closed, Task 4 overlap 규율 flow 전체로 확장).

- [ ] **Step 2~4**: 실패확인 → 구현(frames.go에 InitialReassembler: 내부에 offset-indexed 누적 버퍼 + 바이트 상한 + inconsistent-overlap 검사(Task 4 로직 재사용); 각 Feed는 decryptInitial→CRYPTO 청크 추출→누적→sni.ParseHandshakeSNI 시도(ErrIncomplete면 done=false, 완결이면 done=true, 기타 err면 terminal)) → 통과확인 `go test -race ./internal/network/quic/ -v`(신규+기존 전부·fuzz).
- [ ] **Step 5: Commit** `feat(quic): multi-datagram Initial reassembler (PQ-default ClientHello spanning 2 datagrams)`

### Task 6c: verdict 루프 QUIC flow-cache 배선

**Files:** Modify: `cmd/goose-daemon/sni_verdict.go`, `cmd/goose-daemon/sni_verdict_test.go`.

**Interfaces:**
- Consumes: `quic.InitialReassembler`/`Feed`(6b). 기존 TCP `reassemblerFor`/flow-cache/`evictFlow`/`sniReassemblerMaxFlows` 패턴.
- Produces: `decideQUIC`가 flow(srcIP:sport)별 `*quic.InitialReassembler`를 bounded-LRU 캐시로 유지, 데이터그램 Feed → 완결시 classifyParsedSNI(+flow evict), 미완결시 **sniPassthrough**(unmarked ACCEPT, 다음 재큐잉), err시 sniDrop(+evict).

- [ ] **Step 1: 실패 테스트** (`sni_verdict_test.go`)
  - `TestSNIDecideQUICMultiDatagram`: 등록된 srcIP, allow_sni 매처. `quic.BuildInitialDatagramsForTest`로 허용 SNI를 2 데이터그램으로 → l.decideQUIC(srcIP, d1).Action==sniPassthrough(미완결) → l.decideQUIC(srcIP, d2).Action==sniAcceptMark(완결·허용). deny SNI 2-datagram → 2번째에 sniDrop egress_sni_denied.
  - 기존 `TestSNIDecideQUICRouting`(단일-데이터그램 allowed/denied/미등록/non-Initial) 회귀 유지 — 단일 데이터그램은 첫 Feed에 즉시 완결.
- [ ] **Step 2~4**: 구현 — `reassemblerForQUIC(flowKey string) *quic.InitialReassembler`(bounded LRU, TCP `reassemblerFor` 미러); decideQUIC이 flowKey=srcIP:sport로 얻어 Feed; done→classifyParsedSNI+evictQUICFlow; !done&nil err→sniDecision{sniPassthrough}; err→sniDrop egress_sni_unparsed + evict. **applyVerdictUDP에 sniPassthrough 케이스 재추가**(setAccept unmarked — 6c에서 QUIC이 다시 passthrough를 낼 수 있음; Task 5에서 유닛-데이터그램이라 제거했던 것을 복원). handleUDPQUIC hook이 flow 상태를 안전히(단일 goroutine 디스패치 전제 — TCP reassemblerFor와 동일) 접근. → 통과확인 `go test -race ./cmd/goose-daemon/ -run 'SNIDecide|QUIC|Egress'`.
- [ ] **Step 5: Commit** `feat(egress): QUIC per-flow reassembly cache (multi-datagram Initial passthrough)`
