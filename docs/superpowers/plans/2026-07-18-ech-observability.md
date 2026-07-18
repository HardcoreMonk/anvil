# ECH observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the egress SNI filter *allows* a flow whose ClientHello carries an ECH extension (`encrypted_client_hello`, 0xfe0d), emit a metric + log line so operators can see ECH usage on allowed flows — without ever changing the allow/deny verdict.

**Architecture:** ECH observation is threaded end-to-end as one additive boolean. The TLS/QUIC parser detects the extension best-effort (SNI semantics byte-for-byte unchanged); both reassemblers propagate an `echObserved` flag; the daemon carries it on `sniDecision.ECHObserved`; and `recordVerdict` emits a new counter + `slog.Info` only on `allowed && ECHObserved`. No deny path is added.

**Tech Stack:** Go 1.23, in-tree `internal/network/sni` (TLS ClientHello parse), `internal/network/quic` (QUIC Initial decrypt/reassembly), `cmd/goose-daemon` (NFQUEUE verdict loop + `internal/metrics` Prometheus renderer).

## Global Constraints

- **SNI parsing semantics are invariant.** The SNI returned by `ParseHandshakeSNI` / `ParseClientHelloSNI` and every fail-closed error condition (`ErrIncomplete`, `ErrNoSNI`, truncation errors) MUST stay byte-for-byte identical to the pre-ECH parser. ECH detection is purely additive and best-effort.
- **Verdict is invariant.** No allow/deny/drop decision changes anywhere. ECH observation only adds a metric + log side-effect on already-`allowed` flows. `blanket ECH-deny is explicitly out of scope` (net-negative under the trusted-workload threat model — see spec).
- **ECH extension type: `0xfe0d`** (`encrypted_client_hello`, RFC 9001 / draft-ietf-tls-esni). Use the literal `0xfe0d` with a `// encrypted_client_hello` comment, matching the existing `0x0000 // server_name` style in `parser.go`.
- **New metric: `ephemera_egress_sni_ech_observed_total{proto="tcp|udp"}`** — a CounterVec with a single label `proto`. Only `tcp`/`udp` values ever occur (ECH is observed only on a completed, allowed classify; never `unknown`).
- **Content-free telemetry.** The metric carries only `proto`. The `slog.Info` carries only `proto` + the allowed outer SNI (the same class of data already logged on the deny path) + a fixed message. Never VMID/tenant/token.
- **Both commits keep `go build ./...` green.** Task 1 threads the new return values through *every* caller in the same task, so the whole repo compiles and `go test ./... -race` passes after each task. There is no intermediate red-build window.
- **Verification per task:** `go test ./... -race`, `go build ./...`, `go vet ./...`, `gofmt -l` (must print nothing). Frequent commits.

---

### Task 1: Thread ECH observation through parser → reassemblers → sniDecision (no emit, behavior unchanged)

Detect the ECH extension in the parser and propagate an `echObserved` boolean up through both reassemblers into `sniDecision.ECHObserved`. Nothing consumes the flag yet (Task 2 emits it), so this task is a pure, behavior-preserving plumbing change: every existing test must still pass with only signature updates, plus new assertions proving detection + propagation.

**Files:**
- Modify: `internal/network/sni/parser.go` — `ParseHandshakeSNI`, `ParseClientHelloSNI`, `Reassembler.Feed`; add `scanForECH`.
- Modify: `internal/network/quic/frames.go` — `InitialReassembler.Feed`, `ParseInitialSNI` (discard).
- Modify: `cmd/goose-daemon/sni_verdict.go` — `sniDecision` struct, `decide`, `decideQUIC`, TCP `Start` hook body.
- Test: `internal/network/sni/parser_test.go` — update call sites + add ECH assertions.
- Test: `internal/network/quic/frames_test.go` — update `r.Feed(...)` call sites + add ECH propagation test.
- Test: `cmd/goose-daemon/sni_verdict_test.go` — update the one direct `sni.ParseClientHelloSNI` call site (L74).

**Interfaces:**
- Produces (consumed by Task 2):
  - `sni.ParseHandshakeSNI(hs []byte) (sni string, echPresent bool, err error)`
  - `sni.ParseClientHelloSNI(b []byte) (sni string, echPresent bool, err error)`
  - `(*sni.Reassembler).Feed(segment []byte) (name string, done bool, echObserved bool, err error)`
  - `(*quic.InitialReassembler).Feed(datagram []byte) (name string, done bool, echObserved bool, err error)`
  - `sniDecision` gains field `ECHObserved bool` (set on allowed *and* denied classify results, since it is computed from the parsed ClientHello before the matcher runs; only Task 2's `allowed` gate makes it observable).
- Unchanged: `quic.ParseInitialSNI(datagram []byte) (string, error)` (discards `echObserved` internally).

- [ ] **Step 1: Write the failing parser tests**

Add to `internal/network/sni/parser_test.go` (the `buildClientHello(sni string, ech bool)` helper already appends a 0xfe0d extension *after* the server_name extension when `ech` is true — the harder ordering, since the SNI walk early-returns at server_name and never reaches it):

```go
func TestParseHandshakeSNI_ECHDetected(t *testing.T) {
	// ECH extension sits AFTER server_name; the authoritative SNI walk early-returns
	// at server_name, so detecting ECH here proves scanForECH walks the whole block.
	hs := buildClientHello("cloudflare-ech.com", true)
	name, ech, err := ParseHandshakeSNI(stripRecordHeader(hs))
	if err != nil || name != "cloudflare-ech.com" {
		t.Fatalf("ParseHandshakeSNI = %q, %v; want cloudflare-ech.com", name, err)
	}
	if !ech {
		t.Fatalf("echPresent = false, want true (0xfe0d extension present)")
	}
}

func TestParseHandshakeSNI_NoECH(t *testing.T) {
	hs := buildClientHello("api.anthropic.com", false)
	name, ech, err := ParseHandshakeSNI(stripRecordHeader(hs))
	if err != nil || name != "api.anthropic.com" {
		t.Fatalf("ParseHandshakeSNI = %q, %v", name, err)
	}
	if ech {
		t.Fatalf("echPresent = true, want false (no 0xfe0d extension)")
	}
}

func TestParseClientHelloSNI_ECHFlag(t *testing.T) {
	name, ech, err := ParseClientHelloSNI(buildClientHello("cloudflare-ech.com", true))
	if err != nil || name != "cloudflare-ech.com" || !ech {
		t.Fatalf("ech clienthello: name=%q ech=%v err=%v; want cloudflare-ech.com,true,nil", name, ech, err)
	}
	name, ech, err = ParseClientHelloSNI(buildClientHello("cloudflare-ech.com", false))
	if err != nil || name != "cloudflare-ech.com" || ech {
		t.Fatalf("non-ech clienthello: name=%q ech=%v err=%v; want cloudflare-ech.com,false,nil", name, ech, err)
	}
}

// buildClientHelloTruncatedTrailingExt builds a wire-valid ClientHello whose
// extensions are [server_name(sni), <trailing NON-ECH ext whose declared length
// overruns the block>]. extensions_len exactly covers the bytes actually present
// (so record/handshake framing is valid and the SNI walk runs), but the final
// extension's internal length points past the end. The authoritative SNI walk
// returns at server_name before reaching it, so SNI must still parse; scanForECH
// must stop best-effort without error. Returns a full TLS record.
func buildClientHelloTruncatedTrailingExt(sni string) []byte {
	name := []byte(sni)
	sn := &bytes.Buffer{}
	sn.WriteByte(0x00) // name_type host_name
	binary.Write(sn, binary.BigEndian, uint16(len(name)))
	sn.Write(name)
	list := &bytes.Buffer{}
	binary.Write(list, binary.BigEndian, uint16(sn.Len()))
	list.Write(sn.Bytes())

	ext := &bytes.Buffer{}
	binary.Write(ext, binary.BigEndian, uint16(0x0000)) // server_name
	binary.Write(ext, binary.BigEndian, uint16(list.Len()))
	ext.Write(list.Bytes())
	// Trailing extension: non-ECH type, declared length 255, zero body present.
	binary.Write(ext, binary.BigEndian, uint16(0x1234)) // arbitrary non-ECH type
	binary.Write(ext, binary.BigEndian, uint16(255))    // overruns the block (no body follows)

	body := &bytes.Buffer{}
	body.Write([]byte{0x03, 0x03})
	body.Write(make([]byte, 32))
	body.WriteByte(0x00)
	body.Write([]byte{0x00, 0x02, 0x13, 0x01})
	body.Write([]byte{0x01, 0x00})
	binary.Write(body, binary.BigEndian, uint16(ext.Len())) // extensions_len covers present bytes only
	body.Write(ext.Bytes())

	hs := &bytes.Buffer{}
	hs.WriteByte(0x01)
	l := body.Len()
	hs.Write([]byte{byte(l >> 16), byte(l >> 8), byte(l)})
	hs.Write(body.Bytes())

	rec := &bytes.Buffer{}
	rec.WriteByte(0x16)
	rec.Write([]byte{0x03, 0x01})
	binary.Write(rec, binary.BigEndian, uint16(hs.Len()))
	rec.Write(hs.Bytes())
	return rec.Bytes()
}

func TestParseHandshakeSNI_TruncatedTrailingExtInvariant(t *testing.T) {
	// A truncated trailing extension after server_name must NOT change SNI
	// semantics: the SNI walk returns at server_name (before the truncation), so
	// SNI still parses with no error, and best-effort ECH scan stops without error.
	rec := buildClientHelloTruncatedTrailingExt("api.anthropic.com")
	name, ech, err := ParseClientHelloSNI(rec)
	if err != nil || name != "api.anthropic.com" {
		t.Fatalf("SNI must be unchanged by truncated trailing ext: name=%q err=%v", name, err)
	}
	if ech {
		t.Fatalf("ech = true, want false (trailing ext is non-ECH and truncated)")
	}
}
```

Then update every existing call site in `parser_test.go` to the new arity (these currently take 2 returns; they must become 3). Exact edits:

- L67: `got, err := ParseHandshakeSNI(stripRecordHeader(hs))` → `got, _, err := ParseHandshakeSNI(stripRecordHeader(hs))`
- L74: `sni, err := ParseClientHelloSNI(buildClientHello("api.anthropic.com", false))` → `sni, _, err := ...`
- L78: `sni, err = ParseClientHelloSNI(buildClientHello("API.Example.COM", false))` → `sni, _, err = ...`
- L85: `if _, err := ParseClientHelloSNI(buildClientHello("", false)); ...` → `if _, _, err := ParseClientHelloSNI(buildClientHello("", false)); ...`
- L93: `sni, err := ParseClientHelloSNI(buildClientHello("cloudflare-ech.com", true))` → `sni, _, err := ...` (this `TestParseClientHelloECHOuterExtracted` still asserts the outer SNI; the new `TestParseClientHelloSNI_ECHFlag` covers the flag)
- L104: `if _, err := ParseClientHelloSNI(bad); ...` → `if _, _, err := ParseClientHelloSNI(bad); ...`
- L108: `if _, err := ParseClientHelloSNI(full[:12]); ...` → `if _, _, err := ParseClientHelloSNI(full[:12]); ...`
- L120: `sni, done, err := r.Feed(c)` → `sni, done, _, err := r.Feed(c)`
- L141: `sni, err := ParseClientHelloSNI(buildClientHello("api.anthropic.com.", false))` → `sni, _, err := ...`
- L153: `_, err := ParseClientHelloSNI(buildClientHello(bad, false))` → `_, _, err := ...`
- L166: `_, err := ParseClientHelloSNI(buildClientHello(bad, false))` → `_, _, err := ...`
- L177: `_, done, err := r.Feed(make([]byte, maxClientHelloBytes+1))` → `_, done, _, err := ...`
- L189: `_, done, err := r.Feed(full)` → `_, done, _, err := ...`
- L207 (inside `FuzzParseClientHelloSNI`): `sni, err := ParseClientHelloSNI(data)` → `sni, _, err := ParseClientHelloSNI(data)`

- [ ] **Step 2: Run the parser tests to verify they fail**

Run: `go test ./internal/network/sni/ 2>&1 | head -30`
Expected: compile failure — `not enough return values` / `assignment mismatch` at the call sites, and the three new tests unresolved. (RED: the implementation still returns 2 values.)

- [ ] **Step 3: Implement ECH detection in `parser.go`**

Add `scanForECH` (place it just after `ParseHandshakeSNI`, before `parseServerName`):

```go
// scanForECH reports whether the extensions block contains an
// encrypted_client_hello extension (ECH, type 0xfe0d). It is deliberately
// best-effort and SEPARATE from the authoritative SNI walk in ParseHandshakeSNI:
// it never returns an error and never influences the parsed SNI or any
// fail-closed error condition. A truncated trailing extension simply stops the
// scan (reporting whatever it found so far) instead of being flagged malformed —
// ECH observation must never turn a currently-parseable ClientHello into a parse
// error. Unlike the SNI walk it scans the WHOLE block (no early return at
// server_name), so it detects ECH whether it appears before or after server_name.
func scanForECH(ext []byte) bool {
	for len(ext) >= 4 {
		etype := binary.BigEndian.Uint16(ext[0:2])
		elen := int(binary.BigEndian.Uint16(ext[2:4]))
		if etype == 0xfe0d { // encrypted_client_hello
			return true
		}
		if len(ext) < 4+elen {
			return false // truncated trailing extension: best-effort stop, no error
		}
		ext = ext[4+elen:]
	}
	return false
}
```

Replace `ParseHandshakeSNI` (current L41–97) with the 3-return version. Every pre-extension early return gets `false` for `echPresent`; the extension block computes `ech := scanForECH(ext)` once and threads it into the loop's returns:

```go
// ParseHandshakeSNI parses a TLS handshake message (starting at the ClientHello
// handshake header, msg_type 0x01) and returns the lowercased server_name and
// whether the ClientHello carried an ECH extension (best-effort; see scanForECH).
// QUIC carries the handshake directly in CRYPTO frames (no TLS record), so the
// QUIC path calls this after reassembly; ParseClientHelloSNI calls it after
// stripping the TLS record layer. ErrIncomplete = need more bytes; ErrNoSNI and
// other errors are terminal. echPresent is only meaningful when err == nil.
func ParseHandshakeSNI(hs []byte) (string, bool, error) {
	// Handshake header: msg_type(1) length(3)
	if len(hs) < 4 {
		return "", false, ErrIncomplete
	}
	if hs[0] != 0x01 {
		return "", false, fmt.Errorf("not a client_hello (msg_type 0x%02x)", hs[0])
	}
	hsLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
	body := hs[4:]
	if len(body) < hsLen {
		return "", false, ErrIncomplete
	}
	body = body[:hsLen]

	// client_version(2) random(32) then variable fields.
	p := 2 + 32
	if len(body) < p+1 {
		return "", false, fmt.Errorf("clienthello truncated before session_id")
	}
	sidLen := int(body[p])
	p += 1 + sidLen
	if len(body) < p+2 {
		return "", false, fmt.Errorf("clienthello truncated before cipher_suites")
	}
	csLen := int(binary.BigEndian.Uint16(body[p:]))
	p += 2 + csLen
	if len(body) < p+1 {
		return "", false, fmt.Errorf("clienthello truncated before compression")
	}
	compLen := int(body[p])
	p += 1 + compLen
	if len(body) < p+2 {
		// No extensions at all -> no SNI (and no ECH).
		return "", false, ErrNoSNI
	}
	extTotal := int(binary.BigEndian.Uint16(body[p:]))
	p += 2
	if len(body) < p+extTotal {
		return "", false, fmt.Errorf("clienthello truncated extensions")
	}
	ext := body[p : p+extTotal]

	// Best-effort ECH observation over the whole extensions block, computed
	// independently of the authoritative SNI walk below so the returned SNI and
	// every fail-closed error condition stay identical to the pre-ECH parser.
	ech := scanForECH(ext)

	for len(ext) >= 4 {
		etype := binary.BigEndian.Uint16(ext[0:2])
		elen := int(binary.BigEndian.Uint16(ext[2:4]))
		if len(ext) < 4+elen {
			return "", ech, fmt.Errorf("truncated extension 0x%04x", etype)
		}
		data := ext[4 : 4+elen]
		if etype == 0x0000 { // server_name
			name, err := parseServerName(data)
			return name, ech, err
		}
		ext = ext[4+elen:]
	}
	return "", ech, ErrNoSNI
}
```

Replace `ParseClientHelloSNI` (current L20–33) with the 3-return version:

```go
// ParseClientHelloSNI parses reassembled TLS record bytes and returns the
// lowercased server_name plus whether an ECH extension was present (best-effort;
// echPresent is only meaningful when err == nil). ErrIncomplete means the caller
// should feed more bytes; any other error (including ErrNoSNI) is terminal and
// the caller must fail closed.
func ParseClientHelloSNI(b []byte) (string, bool, error) {
	// TLS record header: type(1) version(2) length(2)
	if len(b) < 5 {
		return "", false, ErrIncomplete
	}
	if b[0] != 0x16 {
		return "", false, fmt.Errorf("not a handshake record (type 0x%02x)", b[0])
	}
	recLen := int(binary.BigEndian.Uint16(b[3:5]))
	if len(b) < 5+recLen {
		return "", false, ErrIncomplete
	}
	return ParseHandshakeSNI(b[5 : 5+recLen])
}
```

Replace `Reassembler.Feed` (current L162–175) with the 4-return version:

```go
// Feed appends segment and re-attempts a parse. It returns (sni, true,
// echObserved, nil) once a full ClientHello is parsed, ("", false, false, nil)
// when more bytes are needed, and ("", false, false, err) on any terminal error
// (malformed, no-SNI, or the buffer bound being exceeded) — which the caller must
// treat as fail-closed. echObserved reports whether the completed ClientHello
// carried an ECH extension; it is false unless done is true.
func (r *Reassembler) Feed(segment []byte) (string, bool, bool, error) {
	if len(r.buf)+len(segment) > maxClientHelloBytes {
		return "", false, false, fmt.Errorf("clienthello exceeds %d bytes", maxClientHelloBytes)
	}
	r.buf = append(r.buf, segment...)
	sni, ech, err := ParseClientHelloSNI(r.buf)
	if errors.Is(err, ErrIncomplete) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return sni, true, ech, nil
}
```

- [ ] **Step 4: Run the sni package tests to verify GREEN**

Run: `go test ./internal/network/sni/ -race`
Expected: PASS (all existing tests + the 3 new ECH tests).

- [ ] **Step 5: Update the QUIC reassembler + its tests**

In `internal/network/quic/frames.go`, replace `InitialReassembler.Feed` (current L71–119). The body is unchanged except every return grows a `false`/`ech` slot, and the final parse threads `ech`. Update the doc comment's three-way return list:

```go
// Feed decrypts one QUIC Initial datagram, merges its CRYPTO frames into the flow's
// offset-indexed buffer, and attempts to parse the SNI from the contiguous prefix.
// Its four-way return is:
//   - (name, true, echObserved, nil): the ClientHello is complete and carried SNI
//     "name"; echObserved reports whether it also carried an ECH extension.
//   - ("", false, false, nil):        more bytes are needed; feed the next Initial datagram.
//   - ("", false, false, err):        terminal failure (fail-closed) — the datagram was
//     undecryptable/unsupported/non-Initial, a CRYPTO frame was malformed, a
//     cross-datagram overlap conflicted, the ClientHello carried no SNI, or the
//     accumulation exceeded maxQuicClientHelloBytes.
//
// It never panics: every slice access is bounds-checked and the buffer is capped.
func (r *InitialReassembler) Feed(datagram []byte) (name string, done bool, echObserved bool, err error) {
	payload, err := decryptInitial(datagram)
	if err != nil {
		return "", false, false, err
	}
	chunks, err := extractCryptoChunks(payload)
	if err != nil {
		return "", false, false, err
	}
	for _, c := range chunks {
		end := c.offset + uint64(len(c.data))
		if end > maxQuicClientHelloBytes {
			// Reject before growing the buffer, so a bogus high offset cannot force
			// a large allocation. end >= c.offset, so both fit in int below.
			return "", false, false, errOversizeClientHello
		}
		if uint64(len(r.buf)) < end {
			grow := int(end) - len(r.buf)
			r.buf = append(r.buf, make([]byte, grow)...)
			r.present = append(r.present, make([]bool, grow)...)
		}
		base := int(c.offset)
		for k := 0; k < len(c.data); k++ {
			idx := base + k
			if r.present[idx] {
				if r.buf[idx] != c.data[k] {
					return "", false, false, errInconsistentOverlap
				}
				continue // idempotent overlap; byte already verified consistent.
			}
			r.buf[idx] = c.data[k]
			r.present[idx] = true
		}
	}
	// Advance the contiguous-from-0 prefix over any bytes newly filled this Feed.
	for r.contiguous < len(r.present) && r.present[r.contiguous] {
		r.contiguous++
	}
	name, ech, perr := sni.ParseHandshakeSNI(r.buf[:r.contiguous])
	if perr != nil {
		if errors.Is(perr, sni.ErrIncomplete) {
			// The ClientHello is not complete yet; wait for the next datagram.
			return "", false, false, nil
		}
		// ErrNoSNI and any other parse error are terminal (fail-closed).
		return "", false, false, fmt.Errorf("quic clienthello: %w", perr)
	}
	return name, true, ech, nil
}
```

In the same file, update `ParseInitialSNI` (current L128–140) to discard `echObserved`:

```go
func ParseInitialSNI(datagram []byte) (string, error) {
	r := &InitialReassembler{}
	name, done, _, err := r.Feed(datagram)
	if err != nil {
		return "", err
	}
	if !done {
		// Not complete within this single datagram — anvil's single-datagram callers
		// treat "need more bytes" as a deny, not a request for more input.
		return "", errIncompleteInDatagram
	}
	return name, nil
}
```

Then update the direct `r.Feed(...)` call sites in `internal/network/quic/frames_test.go` (`ParseInitialSNI(...)` callers are unchanged):

- L288: `if name, done, err := r.Feed(dgs[0]); err != nil || done || name != "" {` → `if name, done, _, err := r.Feed(dgs[0]); ...`
- L291: `if name, done, err := r.Feed(dgs[1]); err != nil || !done || name != "api.anthropic.com" {` → `if name, done, _, err := r.Feed(dgs[1]); ...`
- L305: handled in Step 6 — this `cdn.example.com` call is strengthened into a negative ECH assertion (capture `ech`, require `false`), NOT discarded here. Skip it in this mechanical pass.
- L317: `if name, done, err := r2.Feed(dgs[0]); err != nil || !done || name != "cdn.example.com" {` → `if name, done, _, err := r2.Feed(dgs[0]); ...`
- L329: `if _, _, err := r.Feed(dg); !errors.Is(err, errOversizeClientHello) {` → `if _, _, _, err := r.Feed(dg); ...`
- L369: `name, done, err := r.Feed(tc.dgs[idx])` → `name, done, _, err := r.Feed(tc.dgs[idx])`
- L403: `if name, done, err := r.Feed(d1); err != nil || done || name != "" {` → `if name, done, _, err := r.Feed(d1); ...`
- L406: `if _, _, err := r.Feed(d2); !errors.Is(err, errInconsistentOverlap) {` → `if _, _, _, err := r.Feed(d2); ...`

- [ ] **Step 6: Add the QUIC ECH-propagation test**

First add an ECH-carrying handshake builder to `internal/network/quic/testsupport.go`, next to the existing `buildClientHelloHandshakeNoSNI` (it mirrors `buildClientHelloHandshake` at `testsupport.go:168`, appending a 0xfe0d extension after server_name — same layout as `sni.buildClientHello`'s `ech=true` case):

```go
// buildClientHelloHandshakeECH builds a wire-valid record-less ClientHello with a
// server_name (SNI) extension followed by an encrypted_client_hello (ECH, 0xfe0d)
// extension. Mirrors buildClientHelloHandshake; the ECH extension lets tests assert
// InitialReassembler.Feed propagates echObserved through decrypt+reassembly.
func buildClientHelloHandshakeECH(serverName string) []byte {
	ext := &bytes.Buffer{}
	name := []byte(serverName)
	sn := &bytes.Buffer{}
	sn.WriteByte(0x00) // name_type = host_name
	binary.Write(sn, binary.BigEndian, uint16(len(name)))
	sn.Write(name)
	list := &bytes.Buffer{}
	binary.Write(list, binary.BigEndian, uint16(sn.Len()))
	list.Write(sn.Bytes())
	binary.Write(ext, binary.BigEndian, uint16(0x0000)) // ext_type server_name
	binary.Write(ext, binary.BigEndian, uint16(list.Len()))
	ext.Write(list.Bytes())
	// ECH extension after server_name (mirrors sni.buildClientHello's ech=true layout).
	binary.Write(ext, binary.BigEndian, uint16(0xfe0d)) // encrypted_client_hello
	binary.Write(ext, binary.BigEndian, uint16(4))
	ext.Write([]byte{0x00, 0x01, 0x02, 0x03})

	body := &bytes.Buffer{}
	body.Write([]byte{0x03, 0x03})             // client_version TLS 1.2
	body.Write(make([]byte, 32))               // random
	body.WriteByte(0x00)                       // session_id len 0
	body.Write([]byte{0x00, 0x02, 0x13, 0x01}) // cipher_suites
	body.Write([]byte{0x01, 0x00})             // compression
	binary.Write(body, binary.BigEndian, uint16(ext.Len()))
	body.Write(ext.Bytes())

	hs := &bytes.Buffer{}
	hs.WriteByte(0x01) // handshake type client_hello
	l := body.Len()
	hs.Write([]byte{byte(l >> 16), byte(l >> 8), byte(l)}) // uint24 length
	hs.Write(body.Bytes())
	return hs.Bytes()
}
```

Then add the propagation test to `internal/network/quic/frames_test.go` (uses the same `BuildInitialForTest(mustHex("8394c8f03e515708"), 0x00000001, ch)` idiom as `TestParseInitialSNI_RoundTrip_V1`):

```go
func TestInitialReassembler_ECHObserved(t *testing.T) {
	// A single-datagram Initial whose ClientHello carries an ECH extension: Feed
	// must complete, return the outer SNI, AND report echObserved == true.
	// echObserved rides the same ParseHandshakeSNI return as name, so single-datagram
	// coverage proves propagation; the multi-datagram name/done path is tested above.
	dg := BuildInitialForTest(mustHex("8394c8f03e515708"), 0x00000001, buildClientHelloHandshakeECH("cloudflare-ech.com"))
	r := &InitialReassembler{}
	name, done, ech, err := r.Feed(dg)
	if err != nil || !done || name != "cloudflare-ech.com" {
		t.Fatalf("Feed = %q done=%v err=%v; want cloudflare-ech.com,true,nil", name, done, err)
	}
	if !ech {
		t.Fatalf("echObserved = false, want true")
	}
}
```

Also strengthen the existing non-ECH round-trip at `frames_test.go:305` (inside `TestInitialReassembler...`, the `r.Feed(dg)` for `cdn.example.com`) into a negative ECH assertion — capture the new `echObserved` return and require it `false`:

```go
	name, done, ech, err := r.Feed(dg)
	if err != nil || !done || name != "cdn.example.com" || ech {
		t.Fatalf("Feed = %q done=%v ech=%v err=%v; want cdn.example.com,true,false,nil", name, done, ech, err)
	}
```

- [ ] **Step 7: Run the quic package tests to verify GREEN**

Run: `go test ./internal/network/quic/ -race`
Expected: PASS.

- [ ] **Step 8: Update the daemon plumbing (`sni_verdict.go`) + its one test call site**

Add the field to `sniDecision` (current L73–77):

```go
type sniDecision struct {
	Action      sniAction
	SNI         string
	Reason      string
	ECHObserved bool // outer ClientHello carried an ECH extension (0xfe0d). Set on classified decisions; consumed only on allowed flows (Task 2). Never affects Action.
}
```

Update `decide` (current L197–210) — thread the parser's `echPresent` onto the classified decision:

```go
func (l *sniVerdictLoop) decide(srcIP string, payload []byte) sniDecision {
	entry, ok := l.resolveEntry(srcIP)
	if !ok {
		return sniDecision{Action: sniDrop, Reason: "unregistered_source"}
	}
	if len(payload) == 0 {
		return sniDecision{Action: sniPassthrough} // handshake packet, let it through unmarked
	}
	name, ech, err := sni.ParseClientHelloSNI(payload)
	if err != nil {
		return sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"} // fail-closed
	}
	d := l.classifyParsedSNI(entry, name)
	d.ECHObserved = ech
	return d
}
```

Update `decideQUIC`'s Feed call + success branch (current L261–278):

```go
	name, done, ech, ferr := r.Feed(payload)
	switch {
	case ferr != nil:
		// Terminal parse error (malformed / no-SNI / inconsistent overlap /
		// oversized) -> fail closed.
		l.evictQUICFlow(flowKey)
		return sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"}
	case !done:
		// Incomplete ClientHello: fail closed (drop, no RST for UDP) rather than
		// forward a partial ClientHello to the server. The flow stays cached so
		// the next Initial datagram accumulates onto the same reassembler; the
		// client retransmits this dropped datagram once the flow is allowed and
		// the connmark fast path is in place. See the doc comment above.
		return sniDecision{Action: sniDrop, Reason: "egress_sni_incomplete"}
	default:
		l.evictQUICFlow(flowKey)
		d := l.classifyParsedSNI(entry, name) // shared with TCP: in matcher -> acceptMark, else denied
		d.ECHObserved = ech
		return d
	}
```

Update the TCP `Start` hook's Feed call + `default` branch (current L535–548):

```go
		name, done, ech, ferr := r.Feed(t.payload)
		switch {
		case ferr != nil:
			// Terminal parse error (malformed / no-SNI / oversized) -> fail closed.
			l.evictFlow(flowKey)
			l.applyVerdict(nf, id, sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"}, t, entry)
		case !done:
			// Incomplete ClientHello: forward this segment unmarked so the next
			// segment re-queues here. Bounded by the reassembler's 16 KiB buffer
			// and the LRU flow cap; it can never yield an approved connmark.
			l.setAccept(nf, id)
		default:
			l.evictFlow(flowKey)
			d := l.classifyParsedSNI(entry, name)
			d.ECHObserved = ech
			l.applyVerdict(nf, id, d, t, entry)
		}
		return 0
```

Update the one direct `sni.ParseClientHelloSNI` call in `cmd/goose-daemon/sni_verdict_test.go` (L74): `if got, err := sni.ParseClientHelloSNI(b); err != nil || got != name {` → `if got, _, err := sni.ParseClientHelloSNI(b); err != nil || got != name {`

- [ ] **Step 9: Run the whole suite to verify GREEN + clean build**

Run: `go test ./... -race && go build ./... && go vet ./... && gofmt -l internal/network/sni internal/network/quic cmd/goose-daemon`
Expected: all tests PASS, build/vet clean, `gofmt -l` prints nothing.

- [ ] **Step 10: Commit**

```bash
git add internal/network/sni/parser.go internal/network/sni/parser_test.go \
        internal/network/quic/frames.go internal/network/quic/frames_test.go \
        cmd/goose-daemon/sni_verdict.go cmd/goose-daemon/sni_verdict_test.go
git commit -m "feat(egress): detect + propagate ECH observation through SNI/QUIC parse (no emit)"
```

---

### Task 2: Emit the ECH-observed metric + log on allowed flows

Consume `sniDecision.ECHObserved`: register the new counter, and have `recordVerdict` emit it (plus a content-free `slog.Info`) only when the verdict is `allowed` *and* ECH was observed. Verdict behavior stays identical — this task adds only side-effects on the already-`allowed` branch.

**Files:**
- Modify: `cmd/goose-daemon/metrics_handler.go` — add `sniECHObserved *metrics.CounterVec` field, register it, add `IncSNIECHObserved`.
- Modify: `cmd/goose-daemon/sni_verdict.go` — `recordVerdict` emits on `sniAcceptMark && ECHObserved`.
- Test: `cmd/goose-daemon/sni_verdict_test.go` — recordVerdict emit / no-emit cases.
- Test: `cmd/goose-daemon/metrics_handler_test.go` — `/metrics` exposes the new counter.

**Interfaces:**
- Consumes (from Task 1): `sniDecision.ECHObserved bool`.
- Produces: `(*daemonMetrics).IncSNIECHObserved(proto string)`; metric `ephemera_egress_sni_ech_observed_total{proto}`.

- [ ] **Step 1: Write the failing recordVerdict emit tests**

Add to `cmd/goose-daemon/sni_verdict_test.go` (uses the existing `newMetricsTestCP(t)` + `newSNIVerdictLoop(88, auditPath, cp.metrics)` helpers and the `/metrics` scrape pattern already used by `TestSNIRecordVerdictMetricAlwaysIncrements`):

```go
func TestSNIRecordVerdictEmitsECHOnAllowed(t *testing.T) {
	cp := newMetricsTestCP(t)
	l := newSNIVerdictLoop(88, "", cp.metrics)

	// allowed + ECH observed -> ech counter fires (and the normal allowed verdict still fires).
	l.recordVerdict(sniRegistryEntry{VMID: "vm-a", TenantID: "ta"},
		sniDecision{Action: sniAcceptMark, SNI: "cloudflare-ech.com", ECHObserved: true}, protoTCP)

	rec := httptest.NewRecorder()
	cp.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	if !strings.Contains(out, `ephemera_egress_sni_ech_observed_total{proto="tcp"} 1`) {
		t.Fatalf("expected ech counter=1 on allowed+ech, got:\n%s", out)
	}
	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{proto="tcp",outcome="allowed"} 1`) {
		t.Fatalf("verdict must still record allowed=1 (unchanged), got:\n%s", out)
	}
}

func TestSNIRecordVerdictNoECHWhenNotObserved(t *testing.T) {
	cp := newMetricsTestCP(t)
	l := newSNIVerdictLoop(88, "", cp.metrics)

	// allowed but no ECH -> ech counter absent.
	l.recordVerdict(sniRegistryEntry{VMID: "vm-b"},
		sniDecision{Action: sniAcceptMark, SNI: "api.anthropic.com", ECHObserved: false}, protoUDP)

	rec := httptest.NewRecorder()
	cp.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(body), "ephemera_egress_sni_ech_observed_total") {
		t.Fatalf("ech counter must be absent when ECH not observed, got:\n%s", string(body))
	}
}

func TestSNIRecordVerdictNoECHOnDenied(t *testing.T) {
	cp := newMetricsTestCP(t)
	l := newSNIVerdictLoop(88, "", cp.metrics)

	// ECHObserved on a DENIED verdict must NOT emit the ech counter (observation is
	// for allowed flows only — a denied flow is already blocked and audited).
	l.recordVerdict(sniRegistryEntry{VMID: "vm-c", TenantID: "tc"},
		sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test", ECHObserved: true}, protoTCP)

	rec := httptest.NewRecorder()
	cp.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(body), "ephemera_egress_sni_ech_observed_total") {
		t.Fatalf("ech counter must not fire on denied verdict, got:\n%s", string(body))
	}
}

func TestSNIRecordVerdictECHNilMetricsSafe(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil) // metrics intentionally nil
	// Must not panic on a nil *daemonMetrics receiver.
	l.recordVerdict(sniRegistryEntry{VMID: "vm-d", TenantID: "td"},
		sniDecision{Action: sniAcceptMark, SNI: "ok.test", ECHObserved: true}, protoTCP)
}
```

- [ ] **Step 2: Run to verify the tests fail**

Run: `go test ./cmd/goose-daemon/ -run 'TestSNIRecordVerdict(EmitsECHOnAllowed|NoECHWhenNotObserved|NoECHOnDenied|ECHNilMetricsSafe)' 2>&1 | head -30`
Expected: compile failure — `cp.metrics` has no field/method for the ECH counter, and the `ephemera_egress_sni_ech_observed_total` line is never emitted. (RED.)

- [ ] **Step 3: Register the counter + add `IncSNIECHObserved` in `metrics_handler.go`**

Add the field to the `daemonMetrics` struct, right after `sniVerdictTotal` (current L40):

```go
	sniVerdictTotal   *metrics.CounterVec // proto=tcp|udp|unknown, outcome=allowed|denied|dropped (egress SNI filter)
	sniECHObserved    *metrics.CounterVec // proto=tcp|udp — allowed flows whose ClientHello carried an ECH extension (0xfe0d)
```

Register it in `newDaemonMetrics`, right after the `sniVerdictTotal: r.NewCounterVec(...)` block (current L117–121):

```go
		sniECHObserved: r.NewCounterVec(
			"ephemera_egress_sni_ech_observed_total",
			"Total :443 flows ALLOWED by the SNI filter whose ClientHello also carried an ECH extension (encrypted_client_hello, 0xfe0d), by proto (tcp|udp). Observation only — the flow is allowed; this never denies. Surfaces ECH usage / potential outer-SNI tunneling on allowed flows.",
			"proto",
		),
```

Add the emitter near `IncSNIVerdict` (after current L245):

```go
// IncSNIECHObserved records one ALLOWED :443 flow whose ClientHello carried an
// ECH extension, by proto (tcp|udp). It is a pure observation counter — the flow
// was allowed by the SNI matcher; this never influences the verdict. Content-free
// (proto only, never SNI/VMID/tenant), so like IncSNIVerdict it carries no
// redaction risk and is never gated on the audit write. Nil-safe.
func (m *daemonMetrics) IncSNIECHObserved(proto string) {
	if m == nil {
		return
	}
	if m.sniECHObserved != nil {
		m.sniECHObserved.WithLabelValues(proto).Inc()
	}
}
```

- [ ] **Step 4: Emit from `recordVerdict` in `sni_verdict.go`**

In `recordVerdict` (current L689–707), extend the `sniAcceptMark` case (current L691–692) to emit the ECH signal:

```go
	case sniAcceptMark:
		l.metrics.IncSNIVerdict(proto, "allowed")
		if d.ECHObserved {
			// Observation only: the flow is allowed. Surface ECH-on-allowed-flow so
			// operators can see potential outer-SNI tunneling (ADR-0002 ECH residual).
			// Content-free beyond proto + the already-allowed outer SNI; never denies.
			l.metrics.IncSNIECHObserved(proto)
			slog.Info("egress sni: ech observed on allowed flow", "proto", proto, "sni", d.SNI)
		}
```

- [ ] **Step 5: Run the daemon suite to verify GREEN**

Run: `go test ./cmd/goose-daemon/ -race`
Expected: PASS (the 4 new tests + all existing).

- [ ] **Step 6: Add the `/metrics` exposition test**

Add to `cmd/goose-daemon/metrics_handler_test.go` (mirrors `TestMetrics_HandlerExposesSNIVerdictTotal`):

```go
func TestMetrics_HandlerExposesSNIECHObserved(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.metrics.IncSNIECHObserved(protoTCP)
	cp.metrics.IncSNIECHObserved(protoTCP)
	cp.metrics.IncSNIECHObserved(protoUDP)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	wantLines := []string{
		`ephemera_egress_sni_ech_observed_total{proto="tcp"} 2`,
		`ephemera_egress_sni_ech_observed_total{proto="udp"} 1`,
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("missing line %q in:\n%s", w, out)
		}
	}
}

func TestMetrics_IncSNIECHObservedNilMetricsSafe(t *testing.T) {
	var m *daemonMetrics
	m.IncSNIECHObserved(protoTCP) // must not panic on nil receiver
}
```

- [ ] **Step 7: Run the whole suite + clean build**

Run: `go test ./... -race && go build ./... && go vet ./... && gofmt -l internal/network/sni internal/network/quic cmd/goose-daemon`
Expected: all PASS, build/vet clean, `gofmt -l` prints nothing.

- [ ] **Step 8: Commit**

```bash
git add cmd/goose-daemon/metrics_handler.go cmd/goose-daemon/metrics_handler_test.go \
        cmd/goose-daemon/sni_verdict.go cmd/goose-daemon/sni_verdict_test.go
git commit -m "feat(egress): emit ephemera_egress_sni_ech_observed_total on allowed flows with ECH"
```

---

## Post-implementation (controller, after both tasks pass review)

These are documentation-sync steps, not code tasks — the controller does them before the final whole-branch review / merge (they mirror how prior egress items closed):

- `docs/adr/0002-egress-sni-transparent-filter.md`: update the ECH residual-risk row to note that ECH-on-allowed-flow is now *observable* via `ephemera_egress_sni_ech_observed_total` + `slog.Info` (still not denied; blanket-deny remains rejected as net-negative).
- `CONTEXT.md`: close the "ECH-deny 하드닝 / ECH 관측성" backlog line, noting the resolution was observability (metric + log), not deny.
- If a metrics catalog / runbook lists egress counters, add `ephemera_egress_sni_ech_observed_total`.

## Notes for the executor

- **No KVM e2e gate required.** This is a parse-time observation with no change to the kernel verdict path (allow/deny/drop packets are byte-identical). The `sni`/`quic` unit tests prove detection+propagation; the `cmd/goose-daemon` unit tests prove emit + `/metrics` exposure + verdict-invariance. The QUIC e2e gate (`scripts/anvil-quic-sni-e2e.sh`) still runs unchanged in the release gate and must stay green, but no new e2e assertion is needed for observability.
- **Model routing (SDD):** Task 1 is a multi-file signature cascade with real parser logic → standard model. Task 2 is a small, well-specified emit → cheap model. Final whole-branch review → most capable model.
