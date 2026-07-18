package sni

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// buildClientHello assembles a minimal but wire-valid TLS 1.2/1.3 record carrying
// a ClientHello. If sni != "" a server_name extension is included. If ech is true
// a (dummy) encrypted_client_hello extension is appended alongside the cleartext SNI.
func buildClientHello(sni string, ech bool) []byte {
	ext := &bytes.Buffer{}
	if sni != "" {
		name := []byte(sni)
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
	}
	if ech {
		binary.Write(ext, binary.BigEndian, uint16(0xfe0d)) // encrypted_client_hello
		binary.Write(ext, binary.BigEndian, uint16(4))
		ext.Write([]byte{0x00, 0x01, 0x02, 0x03})
	}

	body := &bytes.Buffer{}
	body.Write([]byte{0x03, 0x03})             // client_version TLS1.2
	body.Write(make([]byte, 32))               // random
	body.WriteByte(0x00)                       // session_id len 0
	body.Write([]byte{0x00, 0x02, 0x13, 0x01}) // cipher_suites: len2 + TLS_AES_128_GCM_SHA256
	body.Write([]byte{0x01, 0x00})             // compression: len1 + null
	binary.Write(body, binary.BigEndian, uint16(ext.Len()))
	body.Write(ext.Bytes())

	hs := &bytes.Buffer{}
	hs.WriteByte(0x01) // handshake type client_hello
	l := body.Len()
	hs.Write([]byte{byte(l >> 16), byte(l >> 8), byte(l)}) // uint24 length
	hs.Write(body.Bytes())

	rec := &bytes.Buffer{}
	rec.WriteByte(0x16) // content_type handshake
	rec.Write([]byte{0x03, 0x01})
	binary.Write(rec, binary.BigEndian, uint16(hs.Len()))
	rec.Write(hs.Bytes())
	return rec.Bytes()
}

// stripRecordHeader strips the 5-byte TLS record header, leaving the raw
// handshake message (buildClientHello returns a full TLS record).
func stripRecordHeader(b []byte) []byte { return b[5:] }

func TestParseHandshakeSNI_BareClientHello(t *testing.T) {
	// buildClientHello is the existing test helper — it returns a full TLS
	// record, so strip the 5-byte record header to get the bare handshake
	// message ParseHandshakeSNI expects.
	hs := buildClientHello("api.anthropic.com", false)
	got, _, err := ParseHandshakeSNI(stripRecordHeader(hs))
	if err != nil || got != "api.anthropic.com" {
		t.Fatalf("ParseHandshakeSNI = %q, %v", got, err)
	}
}

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

// buildClientHelloECHThenTruncatedExt builds a wire-valid ClientHello whose
// extensions block is [encrypted_client_hello (0xfe0d, well-formed), <non-server_name
// ext whose declared length overruns the block>] with NO server_name. The
// extensions_len field exactly covers the present bytes (so record/handshake framing
// is valid and the SNI walk runs), but the second extension's internal length points
// past the block end — so the SNI walk itself reaches its "truncated extension" error
// branch (not a pre-extension length guard, and not an early return at server_name,
// since there is none). The ECH extension precedes the truncation, so scanForECH
// observes it independently of the walk's failure. Returns a full TLS record.
func buildClientHelloECHThenTruncatedExt() []byte {
	ext := &bytes.Buffer{}
	// Well-formed ECH extension first.
	binary.Write(ext, binary.BigEndian, uint16(0xfe0d)) // encrypted_client_hello
	binary.Write(ext, binary.BigEndian, uint16(4))
	ext.Write([]byte{0x00, 0x01, 0x02, 0x03})
	// Truncated non-server_name extension: declared length 255, zero body present.
	binary.Write(ext, binary.BigEndian, uint16(0x1234)) // arbitrary non-ECH, non-server_name type
	binary.Write(ext, binary.BigEndian, uint16(255))    // overruns the block (no body follows)

	body := &bytes.Buffer{}
	body.Write([]byte{0x03, 0x03})                          // client_version TLS 1.2
	body.Write(make([]byte, 32))                            // random
	body.WriteByte(0x00)                                    // session_id len 0
	body.Write([]byte{0x00, 0x02, 0x13, 0x01})              // cipher_suites
	body.Write([]byte{0x01, 0x00})                          // compression
	binary.Write(body, binary.BigEndian, uint16(ext.Len())) // extensions_len covers present bytes only
	body.Write(ext.Bytes())

	hs := &bytes.Buffer{}
	hs.WriteByte(0x01) // handshake type client_hello
	l := body.Len()
	hs.Write([]byte{byte(l >> 16), byte(l >> 8), byte(l)}) // uint24 length
	hs.Write(body.Bytes())

	rec := &bytes.Buffer{}
	rec.WriteByte(0x16) // content_type handshake
	rec.Write([]byte{0x03, 0x01})
	binary.Write(rec, binary.BigEndian, uint16(hs.Len()))
	rec.Write(hs.Bytes())
	return rec.Bytes()
}

func TestParseHandshakeSNI_TruncatedExtInWalkWithECH(t *testing.T) {
	// The SNI walk itself hits its "truncated extension" error branch (an extension
	// whose declared length overruns the block, encountered before any server_name),
	// with an ECH extension present earlier in the block. The error must be the same
	// terminal (fail-closed) error the pre-ECH parser returned — NOT ErrIncomplete and
	// NOT ErrNoSNI — and must not leak an SNI. And because scanForECH is a separate
	// whole-block pass, echPresent must be true even though the walk errors (its value
	// is discarded by callers on any error path, but this pins that the added ech slot
	// is threaded through this error branch and reflects the block, not the walk).
	rec := buildClientHelloECHThenTruncatedExt()
	name, ech, err := ParseClientHelloSNI(rec)
	if err == nil {
		t.Fatalf("expected terminal error from truncated extension, got name=%q nil err", name)
	}
	if errors.Is(err, ErrIncomplete) {
		t.Fatalf("truncated extension must be terminal, not ErrIncomplete: %v", err)
	}
	if errors.Is(err, ErrNoSNI) {
		t.Fatalf("truncated extension must be its own error, not ErrNoSNI: %v", err)
	}
	if name != "" {
		t.Fatalf("no SNI must be returned on truncated extension: got %q", name)
	}
	if !strings.Contains(err.Error(), "truncated extension") {
		t.Fatalf("want 'truncated extension' error (SNI-walk branch), got: %v", err)
	}
	if !ech {
		t.Fatalf("scanForECH must observe the 0xfe0d ext even when the SNI walk errors; got ech=false")
	}
}

func TestParseClientHelloSNI(t *testing.T) {
	sni, _, err := ParseClientHelloSNI(buildClientHello("api.anthropic.com", false))
	if err != nil || sni != "api.anthropic.com" {
		t.Fatalf("normal: sni=%q err=%v", sni, err)
	}
	sni, _, err = ParseClientHelloSNI(buildClientHello("API.Example.COM", false))
	if err != nil || sni != "api.example.com" {
		t.Fatalf("case-fold: sni=%q err=%v", sni, err)
	}
}

func TestParseClientHelloNoSNI(t *testing.T) {
	if _, _, err := ParseClientHelloSNI(buildClientHello("", false)); !errors.Is(err, ErrNoSNI) {
		t.Fatalf("no-SNI err = %v, want ErrNoSNI", err)
	}
}

func TestParseClientHelloECHOuterExtracted(t *testing.T) {
	// ECH decoy: cleartext outer SNI still present; parser returns it so the
	// matcher can deny it. (anvil does not defeat ECH — unparseable SNI = deny.)
	sni, _, err := ParseClientHelloSNI(buildClientHello("cloudflare-ech.com", true))
	if err != nil || sni != "cloudflare-ech.com" {
		t.Fatalf("ech outer: sni=%q err=%v", sni, err)
	}
}

func TestParseClientHelloMalformed(t *testing.T) {
	full := buildClientHello("api.anthropic.com", false)
	// Not a handshake record.
	bad := append([]byte(nil), full...)
	bad[0] = 0x17 // application_data
	if _, _, err := ParseClientHelloSNI(bad); err == nil || errors.Is(err, ErrIncomplete) {
		t.Fatalf("non-handshake err = %v, want hard error", err)
	}
	// Truncated mid-record -> incomplete.
	if _, _, err := ParseClientHelloSNI(full[:12]); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("truncated err = %v, want ErrIncomplete", err)
	}
}

func TestReassemblerMultiSegment(t *testing.T) {
	full := buildClientHello("split.example.com", false)
	r := &Reassembler{}
	// Feed byte-by-byte in three chunks; only the final Feed completes.
	chunks := [][]byte{full[:5], full[5 : len(full)-3], full[len(full)-3:]}
	var got string
	for i, c := range chunks {
		sni, done, _, err := r.Feed(c)
		if err != nil {
			t.Fatalf("chunk %d Feed err = %v", i, err)
		}
		if done {
			got = sni
		}
	}
	if got != "split.example.com" {
		t.Fatalf("reassembled sni = %q", got)
	}
}

// --- Task 1 review-derived coverage (trailing dot + charset) ---

// TestParseClientHelloTrailingDot: RFC 6066 forbids a trailing dot on the SNI,
// but some clients append the DNS root label. The parser canonicalizes by
// stripping a single trailing dot so "host." matches the same allow-list entry
// as "host". (Strip, not reject — see report; no under-block risk because the
// matcher default-denies.)
func TestParseClientHelloTrailingDot(t *testing.T) {
	sni, _, err := ParseClientHelloSNI(buildClientHello("api.anthropic.com.", false))
	if err != nil || sni != "api.anthropic.com" {
		t.Fatalf("trailing-dot: sni=%q err=%v, want api.anthropic.com", sni, err)
	}
}

// TestParseClientHelloRejectsNonASCII: control characters and high bytes never
// appear in a compliant (A-label / punycode) SNI. Treat them as malformed and
// fail closed with a terminal hard error (not ErrNoSNI, not ErrIncomplete) so
// nothing exotic reaches the matcher or the logs.
func TestParseClientHelloRejectsNonASCII(t *testing.T) {
	for _, bad := range []string{"api\x00.com", "api\x1f.com", "ex\xffample.com", "\x7f.com"} {
		_, _, err := ParseClientHelloSNI(buildClientHello(bad, false))
		if err == nil || errors.Is(err, ErrNoSNI) || errors.Is(err, ErrIncomplete) {
			t.Fatalf("non-ascii %q: err=%v, want terminal hard error", bad, err)
		}
	}
}

// TestParseClientHelloEmptyHostName: a server_name extension whose host_name is
// zero-length, only a root dot, or ends in an empty label ("host..") is
// malformed -> terminal hard error. (The "host.." case is a fuzz regression:
// stripping a single trailing dot must not silently leave a dangling dot.)
func TestParseClientHelloEmptyHostName(t *testing.T) {
	for _, bad := range []string{".", "api.anthropic.com..", "host.."} {
		_, _, err := ParseClientHelloSNI(buildClientHello(bad, false))
		if err == nil || errors.Is(err, ErrIncomplete) {
			t.Fatalf("malformed host_name %q: err=%v, want terminal hard error", bad, err)
		}
	}
}

// TestReassemblerRejectsOversized: the reassembly buffer is bounded so a peer
// cannot make anvil buffer forever by never completing the ClientHello.
func TestReassemblerRejectsOversized(t *testing.T) {
	r := &Reassembler{}
	_, done, _, err := r.Feed(make([]byte, maxClientHelloBytes+1))
	if err == nil || done {
		t.Fatalf("oversized Feed: done=%v err=%v, want error", done, err)
	}
}

// TestReassemblerHardErrorPropagates: a hard parse error (non-handshake record)
// surfaces through Feed as a terminal error, not a silent "need more".
func TestReassemblerHardErrorPropagates(t *testing.T) {
	full := buildClientHello("api.anthropic.com", false)
	full[0] = 0x17 // application_data, not handshake
	r := &Reassembler{}
	_, done, _, err := r.Feed(full)
	if err == nil || done {
		t.Fatalf("hard error via Feed: done=%v err=%v, want terminal error", done, err)
	}
}

// FuzzParseClientHelloSNI asserts the parser never panics on arbitrary bytes and
// that a nil-error result always upholds the output invariant (non-empty,
// lowercased, no trailing dot, printable ASCII only).
func FuzzParseClientHelloSNI(f *testing.F) {
	f.Add(buildClientHello("api.anthropic.com", false))
	f.Add(buildClientHello("", false))
	f.Add(buildClientHello("cloudflare-ech.com", true))
	f.Add(buildClientHello("api.anthropic.com.", false))
	f.Add(buildClientHello("api.anthropic.com", false)[:12])
	f.Add([]byte{0x16, 0x03, 0x01, 0xff, 0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		sni, _, err := ParseClientHelloSNI(data) // must never panic
		if err != nil {
			return
		}
		if sni == "" {
			t.Fatalf("nil error but empty sni for %x", data)
		}
		if sni != strings.ToLower(sni) {
			t.Fatalf("sni %q not lowercased", sni)
		}
		if strings.HasSuffix(sni, ".") {
			t.Fatalf("sni %q has trailing dot", sni)
		}
		for i := 0; i < len(sni); i++ {
			if c := sni[i]; c < 0x20 || c >= 0x7f {
				t.Fatalf("sni %q has non-printable byte 0x%02x", sni, c)
			}
		}
	})
}
