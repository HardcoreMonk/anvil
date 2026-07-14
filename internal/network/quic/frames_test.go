package quic

import (
	"bytes"
	"errors"
	"testing"

	"ephemera/internal/network/sni"
)

// --- Round-trip: BuildInitialForTest is the inverse of ParseInitialSNI. ---

func TestParseInitialSNI_RoundTrip_V1(t *testing.T) {
	ch := buildClientHelloHandshake("cdn.example.com")
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

// TestParseInitialSNI_RoundTrip_Uppercase confirms the SNI is lowercased end to end
// (the matcher is case-sensitive; the parser must canonicalize).
func TestParseInitialSNI_RoundTrip_Uppercase(t *testing.T) {
	ch := buildClientHelloHandshake("API.Anthropic.COM")
	dg := BuildInitialForTest(mustHex("8394c8f03e515708"), 0x00000001, ch)
	if got, err := ParseInitialSNI(dg); err != nil || got != "api.anthropic.com" {
		t.Fatalf("ParseInitialSNI = %q, %v, want api.anthropic.com", got, err)
	}
}

// --- Fail-closed: every failure mode returns a terminal error. ---

func TestParseInitialSNI_FailClosed(t *testing.T) {
	dcid := mustHex("8394c8f03e515708")
	for name, dg := range map[string][]byte{
		"empty":           {},
		"unsupported-ver": BuildInitialForTest(dcid, 0xdeadbeef, buildClientHelloHandshake("x.com")),
		"no-sni":          BuildInitialForTest(dcid, 0x00000001, buildClientHelloHandshakeNoSNI()),
	} {
		if _, err := ParseInitialSNI(dg); err == nil {
			t.Fatalf("%s: expected fail-closed error", name)
		}
	}
}

// --- CARRYOVER (Task 3 review): RFC 9369 Appendix A.2 v2 golden vector. ---
//
// The v1 RFC 9001 A.2 vector is exercised in initial_test.go; a self-consistent
// v1/v2 round trip cannot catch a wrong v2 constant because BuildInitialForTest and
// decryptInitial would share the same mistake. This asserts decryptInitial against
// an externally published v2 packet: the QUICv2 salt/labels and v2 Initial packet
// type (0b01) must all be correct or the AEAD open fails / the plaintext mismatches.
//
// Copied verbatim from RFC 9369 §A.2 (https://www.rfc-editor.org/rfc/rfc9369.txt).
// DCID = 8394c8f03e515708, version 0x6b3343cf, packet number 2. The unprotected
// payload is a single CRYPTO frame (offset 0, length 0x00f1 = 241) carrying a
// ClientHello for "example.com", then PADDING to a 1162-byte payload.
const rfc9369A2ClientInitialHex = "" +
	"d76b3343cf088394c8f03e5157080000449ea0c95e82ffe67b6abcdb4298b485" +
	"dd04de806071bf03dceebfa162e75d6c96058bdbfb127cdfcbf903388e99ad04" +
	"9f9a3dd4425ae4d0992cfff18ecf0fdb5a842d09747052f17ac2053d21f57c5d" +
	"250f2c4f0e0202b70785b7946e992e58a59ac52dea6774d4f03b55545243cf1a" +
	"12834e3f249a78d395e0d18f4d766004f1a2674802a747eaa901c3f10cda5500" +
	"cb9122faa9f1df66c392079a1b40f0de1c6054196a11cbea40afb6ef5253cd68" +
	"18f6625efce3b6def6ba7e4b37a40f7732e093daa7d52190935b8da58976ff33" +
	"12ae50b187c1433c0f028edcc4c2838b6a9bfc226ca4b4530e7a4ccee1bfa2a3" +
	"d396ae5a3fb512384b2fdd851f784a65e03f2c4fbe11a53c7777c023462239dd" +
	"6f7521a3f6c7d5dd3ec9b3f233773d4b46d23cc375eb198c63301c21801f6520" +
	"bcfb7966fc49b393f0061d974a2706df8c4a9449f11d7f3d2dcbb90c6b877045" +
	"636e7c0c0fe4eb0f697545460c806910d2c355f1d253bc9d2452aaa549e27a1f" +
	"ac7cf4ed77f322e8fa894b6a83810a34b361901751a6f5eb65a0326e07de7c12" +
	"16ccce2d0193f958bb3850a833f7ae432b65bc5a53975c155aa4bcb4f7b2c4e5" +
	"4df16efaf6ddea94e2c50b4cd1dfe06017e0e9d02900cffe1935e0491d77ffb4" +
	"fdf85290fdd893d577b1131a610ef6a5c32b2ee0293617a37cbb08b847741c3b" +
	"8017c25ca9052ca1079d8b78aebd47876d330a30f6a8c6d61dd1ab5589329de7" +
	"14d19d61370f8149748c72f132f0fc99f34d766c6938597040d8f9e2bb522ff9" +
	"9c63a344d6a2ae8aa8e51b7b90a4a806105fcbca31506c446151adfeceb51b91" +
	"abfe43960977c87471cf9ad4074d30e10d6a7f03c63bd5d4317f68ff325ba3bd" +
	"80bf4dc8b52a0ba031758022eb025cdd770b44d6d6cf0670f4e990b22347a7db" +
	"848265e3e5eb72dfe8299ad7481a408322cac55786e52f633b2fb6b614eaed18" +
	"d703dd84045a274ae8bfa73379661388d6991fe39b0d93debb41700b41f90a15" +
	"c4d526250235ddcd6776fc77bc97e7a417ebcb31600d01e57f32162a8560cacc" +
	"7e27a096d37a1a86952ec71bd89a3e9a30a2a26162984d7740f81193e8238e61" +
	"f6b5b984d4d3dfa033c1bb7e4f0037febf406d91c0dccf32acf423cfa1e70710" +
	"10d3f270121b493ce85054ef58bada42310138fe081adb04e2bd901f2f13458b" +
	"3d6758158197107c14ebb193230cd1157380aa79cae1374a7c1e5bbcb80ee23e" +
	"06ebfde206bfb0fcbc0edc4ebec309661bdd908d532eb0c6adc38b7ca7331dce" +
	"8dfce39ab71e7c32d318d136b6100671a1ae6a6600e3899f31f0eed19e3417d1" +
	"34b90c9058f8632c798d4490da4987307cba922d61c39805d072b589bd52fdf1" +
	"e86215c2d54e6670e07383a27bbffb5addf47d66aa85a0c6f9f32e59d85a44dd" +
	"5d3b22dc2be80919b490437ae4f36a0ae55edf1d0b5cb4e9a3ecabee93dfc6e3" +
	"8d209d0fa6536d27a5d6fbb17641cde27525d61093f1b28072d111b2b4ae5f89" +
	"d5974ee12e5cf7d5da4d6a31123041f33e61407e76cffcdcfd7e19ba58cf4b53" +
	"6f4c4938ae79324dc402894b44faf8afbab35282ab659d13c93f70412e85cb19" +
	"9a37ddec600545473cfb5a05e08d0b209973b2172b4d21fb69745a262ccde96b" +
	"a18b2faa745b6fe189cf772a9f84cbfc"

// RFC 9369 A.2: the leading CRYPTO frame of the 1162-byte decrypted payload
// (245 bytes: type 0x06, offset 0, length 0x00f1, then the TLS ClientHello). This
// is byte-identical to the RFC 9001 A.2 v1 plaintext (same ClientHello, different
// keys); the remaining 917 bytes are PADDING (0x00).
const rfc9369A2PlaintextPrefixHex = "" +
	"060040f1010000ed0303ebf8fa56f12939b9584a3896472ec40bb863cfd3e868" +
	"04fe3a47f06a2b69484c00000413011302010000c000000010000e00000b6578" +
	"616d706c652e636f6dff01000100000a00080006001d00170018001000070005" +
	"04616c706e000500050100000000003300260024001d00209370b2c9caa47fba" +
	"baf4559fedba753de171fa71f50f1ce15d43e994ec74d748002b000302030400" +
	"0d0010000e0403050306030203080408050806002d00020101001c0002400100" +
	"3900320408ffffffffffffffff05048000ffff07048000ffff08011001048000" +
	"75300901100f088394c8f03e51570806048000ffff"

func TestDecryptInitial_RFC9369_V2(t *testing.T) {
	payload, err := decryptInitial(mustHex(rfc9369A2ClientInitialHex))
	if err != nil {
		t.Fatalf("v2 decrypt: %v", err)
	}
	if len(payload) != 1162 {
		t.Fatalf("v2 payload length = %d, want 1162", len(payload))
	}
	prefix := mustHex(rfc9369A2PlaintextPrefixHex)
	if !bytes.Equal(payload[:len(prefix)], prefix) {
		t.Fatalf("v2 payload prefix mismatch:\n got %x\nwant %x", payload[:len(prefix)], prefix)
	}
	for i, b := range payload[len(prefix):] {
		if b != 0 {
			t.Fatalf("v2 expected PADDING (0x00) at offset %d, got 0x%02x", len(prefix)+i, b)
		}
	}
}

// TestParseInitialSNI_RFC9369_V2 drives the full pipeline (decrypt -> reassemble ->
// SNI) on the RFC 9369 v2 golden packet and asserts the published SNI.
func TestParseInitialSNI_RFC9369_V2(t *testing.T) {
	got, err := ParseInitialSNI(mustHex(rfc9369A2ClientInitialHex))
	if err != nil || got != "example.com" {
		t.Fatalf("v2 golden ParseInitialSNI = %q, %v, want example.com", got, err)
	}
}

// --- reassembleCryptoFrames unit coverage. ---

// cryptoFrame builds a CRYPTO frame (type 0x06 + offset + length + data).
func cryptoFrame(offset uint64, data []byte) []byte {
	f := []byte{0x06}
	f = appendVarint(f, offset)
	f = appendVarint(f, uint64(len(data)))
	return append(f, data...)
}

// TestReassemble_SkipsAndReorders confirms PADDING/PING/ACK frames are skipped and
// out-of-order CRYPTO frames are joined in offset order.
func TestReassemble_SkipsAndReorders(t *testing.T) {
	var payload []byte
	payload = append(payload, 0x01)                            // PING
	payload = append(payload, cryptoFrame(3, []byte("lo"))...) // second half first
	payload = append(payload, 0x00, 0x00)                      // PADDING
	// ACK (type 0x02): largest=0, delay=0, range_count=0, first_range=0.
	payload = append(payload, 0x02, 0x00, 0x00, 0x00, 0x00)
	payload = append(payload, cryptoFrame(0, []byte("hel"))...) // first half
	payload = append(payload, 0x00)                             // PADDING

	out, err := reassembleCryptoFrames(payload)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if string(out) != "hello" {
		t.Fatalf("reassembled %q, want %q", out, "hello")
	}
}

// TestReassemble_OffsetGapTerminal: a hole between CRYPTO offsets means the
// ClientHello is not complete within this datagram -> terminal errIncomplete.
func TestReassemble_OffsetGapTerminal(t *testing.T) {
	var payload []byte
	payload = append(payload, cryptoFrame(0, []byte("hel"))...)
	payload = append(payload, cryptoFrame(5, []byte("lo"))...) // gap at bytes [3,5)
	if _, err := reassembleCryptoFrames(payload); !errors.Is(err, errIncompleteInDatagram) {
		t.Fatalf("offset gap err = %v, want errIncompleteInDatagram", err)
	}
}

// TestReassemble_TruncatedCryptoTerminal: a CRYPTO length that runs past the payload
// is terminal, never a panic.
func TestReassemble_TruncatedCryptoTerminal(t *testing.T) {
	// type 0x06, offset 0, length 10, but only 2 data bytes present.
	payload := []byte{0x06, 0x00, 0x0a, 0xaa, 0xbb}
	if _, err := reassembleCryptoFrames(payload); !errors.Is(err, errIncompleteInDatagram) {
		t.Fatalf("truncated CRYPTO err = %v, want errIncompleteInDatagram", err)
	}
}

// TestReassemble_NoCryptoTerminal: a payload of only PADDING carries no ClientHello.
func TestReassemble_NoCryptoTerminal(t *testing.T) {
	if _, err := reassembleCryptoFrames([]byte{0x00, 0x00, 0x00}); !errors.Is(err, errIncompleteInDatagram) {
		t.Fatalf("no-CRYPTO err = %v, want errIncompleteInDatagram", err)
	}
}

// TestReassemble_UnknownFrameTerminal: an unmodeled frame type cannot be
// length-skipped, so we fail closed rather than guess.
func TestReassemble_UnknownFrameTerminal(t *testing.T) {
	// 0x08 is a STREAM frame, not valid in an Initial packet.
	payload := append(cryptoFrame(0, []byte("hi")), 0x08, 0x00)
	if _, err := reassembleCryptoFrames(payload); err == nil {
		t.Fatal("unknown frame type must be terminal")
	}
}

// TestReassemble_DuplicateCryptoTolerated: an overlapping/duplicate CRYPTO frame is
// deduplicated, not treated as a gap.
func TestReassemble_DuplicateCryptoTolerated(t *testing.T) {
	var payload []byte
	payload = append(payload, cryptoFrame(0, []byte("hello"))...)
	payload = append(payload, cryptoFrame(0, []byte("hel"))...) // duplicate prefix
	out, err := reassembleCryptoFrames(payload)
	if err != nil || string(out) != "hello" {
		t.Fatalf("reassembled %q, %v, want hello", out, err)
	}
}

// TestReassemble_InconsistentOverlapTerminal: two CRYPTO frames that cover the same
// bytes with different contents are the QUIC analog of TCP-overlap evasion. They must
// deny (fail-closed), and — because sort.Slice is not stable — the verdict must not
// depend on frame ordering.
func TestReassemble_InconsistentOverlapTerminal(t *testing.T) {
	// Same offset, different bytes. sort.Slice may keep either frame first, so a
	// correct reassembler denies regardless of the order they appear in the payload.
	same := [2][]byte{cryptoFrame(0, []byte("hello")), cryptoFrame(0, []byte("world"))}
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		var p []byte
		p = append(p, same[order[0]]...)
		p = append(p, same[order[1]]...)
		if _, err := reassembleCryptoFrames(p); !errors.Is(err, errInconsistentOverlap) {
			t.Fatalf("same-offset conflict order %v: err = %v, want errInconsistentOverlap", order, err)
		}
	}
	// Partial overlap: [0,5)="abcde" and [3,7)="XYfg" disagree on bytes [3,5).
	a, b := cryptoFrame(0, []byte("abcde")), cryptoFrame(3, []byte("XYfg"))
	for _, order := range [][2][]byte{{a, b}, {b, a}} {
		var p []byte
		p = append(p, order[0]...)
		p = append(p, order[1]...)
		if _, err := reassembleCryptoFrames(p); !errors.Is(err, errInconsistentOverlap) {
			t.Fatalf("partial-overlap conflict: err = %v, want errInconsistentOverlap", err)
		}
	}
}

// TestParseInitialSNI_IncompleteHandshakeTerminal: a CRYPTO frame carrying a
// truncated ClientHello yields sni.ErrIncomplete, which must be terminal here
// (anvil does not reassemble across datagrams).
func TestParseInitialSNI_IncompleteHandshakeTerminal(t *testing.T) {
	full := buildClientHelloHandshake("cdn.example.com")
	dcid := mustHex("8394c8f03e515708")
	// Ship only the first half of the handshake in the CRYPTO frame.
	dg := BuildInitialForTest(dcid, 0x00000001, full[:len(full)/2])
	_, err := ParseInitialSNI(dg)
	if err == nil {
		t.Fatal("truncated ClientHello must be terminal")
	}
	if !errors.Is(err, sni.ErrIncomplete) {
		t.Fatalf("err = %v, want wrapped sni.ErrIncomplete", err)
	}
}

// FuzzParseInitialSNI asserts the whole pipeline never panics on arbitrary bytes and
// that a nil-error result yields a plausible SNI (non-empty, lowercased).
func FuzzParseInitialSNI(f *testing.F) {
	dcid := mustHex("8394c8f03e515708")
	f.Add(BuildInitialForTest(dcid, 0x00000001, buildClientHelloHandshake("api.anthropic.com")))
	f.Add(BuildInitialForTest(dcid, 0x6b3343cf, buildClientHelloHandshake("cdn.example.com")))
	f.Add(BuildInitialForTest(dcid, 0x00000001, buildClientHelloHandshakeNoSNI()))
	f.Add(mustHex(rfc9369A2ClientInitialHex))
	f.Add([]byte{})
	f.Add([]byte{0xc0, 0x00, 0x00, 0x00, 0x01})
	f.Fuzz(func(t *testing.T, data []byte) {
		sniName, err := ParseInitialSNI(data) // must never panic
		if err != nil {
			return
		}
		if sniName == "" {
			t.Fatalf("nil error but empty SNI for %x", data)
		}
	})
}
