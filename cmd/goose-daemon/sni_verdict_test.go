package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ephemera/internal/anvilmcp"
	"ephemera/internal/network/quic"
	"ephemera/internal/network/sni"

	"golang.org/x/sys/unix"
)

// encodeClientHelloSNI is a SNI-only reduction of Task 2's parser_test.go
// buildClientHello. It emits one wire-valid TLS record carrying a ClientHello
// whose single extension is a server_name for name. Kept local to this _test.go
// so the verdict tests do not depend on exporting the sni package's test helper
// (low coupling: the byte layout is copied, and mustHello self-checks it against
// the real exported parser as an oracle).
func encodeClientHelloSNI(name string) []byte {
	host := []byte(name)

	sn := &bytes.Buffer{}
	sn.WriteByte(0x00) // name_type = host_name
	binary.Write(sn, binary.BigEndian, uint16(len(host)))
	sn.Write(host)

	list := &bytes.Buffer{}
	binary.Write(list, binary.BigEndian, uint16(sn.Len()))
	list.Write(sn.Bytes())

	ext := &bytes.Buffer{}
	binary.Write(ext, binary.BigEndian, uint16(0x0000)) // ext_type server_name
	binary.Write(ext, binary.BigEndian, uint16(list.Len()))
	ext.Write(list.Bytes())

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

func mustHello(t *testing.T, name string) []byte {
	t.Helper()
	// Reuse the parser's own acceptance as an oracle: build via a tiny inline
	// encoder mirroring Task 2's buildClientHello (kept local to avoid exporting
	// test helpers). Implementation copies the byte layout from parser_test.go.
	b := encodeClientHelloSNI(name) // small local encoder in this _test.go file
	if got, _, err := sni.ParseClientHelloSNI(b); err != nil || got != name {
		t.Fatalf("oracle: built hello for %q parsed as %q err=%v", name, got, err)
	}
	return b
}

// encodeQUICClientHelloHandshake is encodeClientHelloSNI's QUIC-CRYPTO-frame
// shaped sibling: the identical ClientHello wire layout but WITHOUT the 5-byte
// TLS record header, because QUIC CRYPTO frames carry the handshake message
// directly (RFC 9001 §4) rather than a TLS record. Mirrors the quic package's
// private buildClientHelloHandshake (see internal/network/quic/testsupport.go);
// kept local here for the same low-coupling reason encodeClientHelloSNI is
// local (avoids exporting a quic-package test helper across packages).
func encodeQUICClientHelloHandshake(name string) []byte {
	host := []byte(name)

	sn := &bytes.Buffer{}
	sn.WriteByte(0x00) // name_type = host_name
	binary.Write(sn, binary.BigEndian, uint16(len(host)))
	sn.Write(host)

	list := &bytes.Buffer{}
	binary.Write(list, binary.BigEndian, uint16(sn.Len()))
	list.Write(sn.Bytes())

	ext := &bytes.Buffer{}
	binary.Write(ext, binary.BigEndian, uint16(0x0000)) // ext_type server_name
	binary.Write(ext, binary.BigEndian, uint16(list.Len()))
	ext.Write(list.Bytes())

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
	return hs.Bytes()
}

// buildQUICInitialForTest builds a QUIC Initial UDP payload — decideQUIC's
// input — carrying a ClientHello with SNI=name, via quic.BuildInitialForTest's
// exported round trip (Task 4). srcIPHint is not part of the QUIC wire format;
// it exists only to document which registry entry the caller means to exercise
// the datagram against, matching the brief's literal call shape.
func buildQUICInitialForTest(srcIPHint, name string) []byte {
	dcid := []byte{0x83, 0x94, 0xc8, 0xf0, 0x3e, 0x51, 0x57, 0x08}
	return quic.BuildInitialForTest(dcid, 0x00000001, encodeQUICClientHelloHandshake(name))
}

// TestSNIDeregisterFailsClosed pins that a deregistered source fails closed.
// Originally written against a single-packet decide() helper that had no
// production caller; that helper has since been deleted and every property it
// covered (allow/deny classification, empty payload, unregistered-source
// fail-closed, the malformed vector) now lives on decideTCP — the function the
// data path actually calls — via TestSNIDecideTCPRouting below.
// This is the one property that test never exercised on decideTCP, so it moves
// here rather than being dropped.
func TestSNIDeregisterFailsClosed(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})
	l.Deregister("10.0.1.10")
	// decideTCP is the production TCP classifier; the sport is arbitrary here
	// since the unregistered-source check runs before any per-flow reassembler
	// is touched.
	if d := l.decideTCP("10.0.1.10", 9001, mustHello(t, "api.anthropic.com")); d.Action != sniDrop {
		t.Fatal("deregistered source must fail closed")
	}
}

// buildUDPPacket assembles a minimal IPv4/UDP packet (20-byte IP header, no
// options) for parseIPv4UDP's boundary tests below. udpLen is written verbatim
// into the UDP header's own Length field, letting tests exercise a mismatch
// between it and the packet's actual size (parseIPv4UDP must bound the payload
// by that field, not just by remaining packet bytes — see its doc comment).
func buildUDPPacket(proto byte, udpLen uint16, payload []byte) []byte {
	pkt := make([]byte, 20+8+len(payload))
	pkt[0] = 0x45 // version 4, IHL 5 (20 bytes, no options)
	pkt[9] = proto
	copy(pkt[12:16], net.IPv4(10, 0, 1, 10).To4())
	copy(pkt[16:20], net.IPv4(93, 184, 216, 34).To4())
	binary.BigEndian.PutUint16(pkt[20:22], 55555) // sport
	binary.BigEndian.PutUint16(pkt[22:24], 443)   // dport
	binary.BigEndian.PutUint16(pkt[24:26], udpLen)
	copy(pkt[28:], payload)
	return pkt
}

// TestParseIPv4UDPBoundaries is parseIPv4TCP's boundary-validation coverage
// extended to parseIPv4UDP — the one piece of the UDP glue that is pure logic
// and so, unlike the netlink/NFQUEUE wiring in Start (root + netfilter, Task 7
// e2e only), is fully unit-testable here.
func TestQUICParseIPv4UDPBoundaries(t *testing.T) {
	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	valid := buildUDPPacket(unix.IPPROTO_UDP, uint16(8+len(payload)), payload)

	u, err := parseIPv4UDP(valid)
	if err != nil {
		t.Fatalf("valid udp packet: unexpected error %v", err)
	}
	if u.sport != 55555 || u.dport != 443 || !bytes.Equal(u.payload, payload) {
		t.Fatalf("unexpected parse: sport=%d dport=%d payload=%x", u.sport, u.dport, u.payload)
	}
	if u.srcIP.String() != "10.0.1.10" || u.dstIP.String() != "93.184.216.34" {
		t.Fatalf("unexpected addrs: src=%s dst=%s", u.srcIP, u.dstIP)
	}

	withVersion := func(pkt []byte, version byte) []byte {
		out := append([]byte(nil), pkt...)
		out[0] = version<<4 | (out[0] & 0x0f)
		return out
	}

	cases := map[string][]byte{
		"short ip packet":     valid[:19],
		"not ipv4":            withVersion(valid, 6),
		"not udp proto":       buildUDPPacket(unix.IPPROTO_TCP, uint16(8+len(payload)), payload),
		"short udp header":    valid[:20+7],
		"udp length under 8":  buildUDPPacket(unix.IPPROTO_UDP, 4, payload),
		"udp length overruns": buildUDPPacket(unix.IPPROTO_UDP, 0xffff, payload),
	}
	for name, pkt := range cases {
		if _, err := parseIPv4UDP(pkt); err == nil {
			t.Fatalf("%s: expected error, got none", name)
		}
	}
}

// TestSNIDecideQUICRouting is decideQUIC's single-datagram routing-contract
// test, the UDP/QUIC counterpart of TestSNIDecideRouting above. A single
// datagram carrying a complete ClientHello reaches the reassembler's "done"
// branch on the first Feed, so every case here still resolves immediately
// (allow/deny/unregistered/unparseable) — Task 6c's multi-datagram
// incomplete/passthrough branch is covered separately by
// TestSNIDecideQUICMultiDatagram below.
func TestSNIDecideQUICRouting(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com", "*.example.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})
	const sport = uint16(55555)

	// decideQUIC(srcIP, sport, udpPayload) — flowKey=srcIP:sport feeds the flow's
	// *quic.InitialReassembler (Task 6c); a single complete datagram completes
	// on the first Feed and evicts the flow immediately.
	allow := buildQUICInitialForTest("10.0.1.10", "api.anthropic.com") // test helper: quic 패키지 라운드트립 재사용
	if d := l.decideQUIC("10.0.1.10", sport, allow); d.Action != sniAcceptMark {
		t.Fatalf("allowed QUIC SNI -> %v (%s)", d.Action, d.Reason)
	}
	deny := buildQUICInitialForTest("10.0.1.10", "evil.test")
	if d := l.decideQUIC("10.0.1.10", sport, deny); d.Action != sniDrop || d.Reason != "egress_sni_denied" {
		t.Fatalf("denied QUIC -> %v/%s", d.Action, d.Reason)
	}
	if d := l.decideQUIC("10.0.1.99", sport, allow); d.Action != sniDrop { // 미등록
		t.Fatal("unregistered QUIC must fail closed")
	}
	if d := l.decideQUIC("10.0.1.10", sport, []byte{0x40, 0x00}); d.Action != sniDrop { // non-Initial
		t.Fatal("unparseable QUIC must fail closed")
	}
}

// TestSNIDecideQUICMultiDatagram is Task 6c's core coverage (revised Task 7b): a
// post-quantum-shaped ClientHello split across two Initial datagrams accumulates
// in the flow's *quic.InitialReassembler across two decideQUIC calls sharing the
// same flowKey=srcIP:sport (same sport on both datagrams, mirroring a real QUIC
// flow). The first Feed is incomplete and is DROPPED fail-closed (reason
// egress_sni_incomplete — the datagram is not forwarded, but the reassembler
// keeps its buffered CRYPTO so the second datagram still completes the
// ClientHello); the second Feed completes and classifies.
func TestSNIDecideQUICMultiDatagram(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})

	dcid := []byte{0x83, 0x94, 0xc8, 0xf0, 0x3e, 0x51, 0x57, 0x08}
	const sport = uint16(55555)

	// Allowed SNI split across 2 datagrams.
	allowHandshake := encodeQUICClientHelloHandshake("api.anthropic.com")
	allowDgs := quic.BuildInitialDatagramsForTest(dcid, 0x00000001, allowHandshake, len(allowHandshake)/2)
	if len(allowDgs) != 2 {
		t.Fatalf("expected allow handshake split into 2 datagrams, got %d", len(allowDgs))
	}
	if d := l.decideQUIC("10.0.1.10", sport, allowDgs[0]); d.Action != sniDrop || d.Reason != "egress_sni_incomplete" {
		t.Fatalf("allow datagram 1 -> %v (%s), want drop/egress_sni_incomplete (fail-closed buffering)", d.Action, d.Reason)
	}
	if d := l.decideQUIC("10.0.1.10", sport, allowDgs[1]); d.Action != sniAcceptMark {
		t.Fatalf("allow datagram 2 -> %v (%s), want accept_mark (complete+allowed)", d.Action, d.Reason)
	}

	// Denied SNI split across 2 datagrams, same flowKey (same sport) reused
	// after the prior flow's completion evicted it.
	denyHandshake := encodeQUICClientHelloHandshake("evil.test")
	denyDgs := quic.BuildInitialDatagramsForTest(dcid, 0x00000001, denyHandshake, len(denyHandshake)/2)
	if len(denyDgs) != 2 {
		t.Fatalf("expected deny handshake split into 2 datagrams, got %d", len(denyDgs))
	}
	if d := l.decideQUIC("10.0.1.10", sport, denyDgs[0]); d.Action != sniDrop || d.Reason != "egress_sni_incomplete" {
		t.Fatalf("deny datagram 1 -> %v (%s), want drop/egress_sni_incomplete (fail-closed buffering)", d.Action, d.Reason)
	}
	if d := l.decideQUIC("10.0.1.10", sport, denyDgs[1]); d.Action != sniDrop || d.Reason != "egress_sni_denied" {
		t.Fatalf("deny datagram 2 -> %v/%s, want drop/egress_sni_denied", d.Action, d.Reason)
	}
}

// --- H3: TCP segment classification (decideTCP), the Start hook's own core ---

// splitHelloIntoRecords re-frames the single TLS record encodeClientHelloSNI
// produces into n consecutive handshake records (content_type 0x16), each with
// its own 5-byte header. RFC 8446 §5.1 explicitly permits a handshake message to
// span records, so this is wire-legal ClientHello traffic that a guest can emit
// at will. Records are returned separately so a test can send each in its own
// TCP segment, the shape that made the ClientHello unclassifiable.
func splitHelloIntoRecords(rec []byte, n int) [][]byte {
	hs := rec[5:] // strip the single record header encodeClientHelloSNI wrote
	chunk := (len(hs) + n - 1) / n
	var recs [][]byte
	for off := 0; off < len(hs); off += chunk {
		end := off + chunk
		if end > len(hs) {
			end = len(hs)
		}
		out := &bytes.Buffer{}
		out.WriteByte(0x16)
		out.Write([]byte{0x03, 0x01})
		binary.Write(out, binary.BigEndian, uint16(end-off))
		out.Write(hs[off:end])
		recs = append(recs, out.Bytes())
	}
	return recs
}

// TestSNIDecideTCPRouting is the routing contract of the function the Start
// hook's TCP branch actually calls. It replaces the coverage that used to sit on
// a single-packet decide() helper with no production caller, whose green never
// touched the real data path — which is how H3 stayed hidden.
func TestSNIDecideTCPRouting(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com", "*.example.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Profile: "p", Matcher: m})

	// Each case uses a distinct sport so it gets its own flow reassembler.
	if d := l.decideTCP("10.0.1.10", 1001, mustHello(t, "api.anthropic.com")); d.Action != sniAcceptMark {
		t.Fatalf("allowed exact -> %v (%s)", d.Action, d.Reason)
	}
	if d := l.decideTCP("10.0.1.10", 1002, mustHello(t, "cdn.example.com")); d.Action != sniAcceptMark {
		t.Fatalf("allowed wildcard -> %v (%s)", d.Action, d.Reason)
	}
	d := l.decideTCP("10.0.1.10", 1003, mustHello(t, "evil.test"))
	if d.Action != sniDrop || d.Reason != "egress_sni_denied" || d.SNI != "evil.test" {
		t.Fatalf("denied -> action=%v reason=%q sni=%q", d.Action, d.Reason, d.SNI)
	}
	// Bare ACK / handshake segment: no TLS bytes yet, forwarded unmarked and
	// deliberately unreasoned (so recordVerdict does not count every ACK).
	if d := l.decideTCP("10.0.1.10", 1004, []byte{}); d.Action != sniPassthrough || d.Reason != "" {
		t.Fatalf("empty payload -> %v/%q, want passthrough with no reason", d.Action, d.Reason)
	}
	if d := l.decideTCP("10.0.1.99", 1005, mustHello(t, "api.anthropic.com")); d.Action != sniDrop {
		t.Fatalf("unregistered -> %v, want fail-closed drop", d.Action)
	}
	// Terminal parse error (not a handshake record) -> fail-closed drop.
	badRec := append([]byte(nil), mustHello(t, "api.anthropic.com")...)
	badRec[0] = 0x17 // application_data
	if d := l.decideTCP("10.0.1.10", 1006, badRec); d.Action != sniDrop || d.Reason != "egress_sni_unparsed" {
		t.Fatalf("non-handshake record -> %v/%q, want drop/egress_sni_unparsed", d.Action, d.Reason)
	}
	// {0x16,0x03,0x01,0xff,0xff,0x01}: a handshake record declaring a 64 KiB
	// body with only 1 byte buffered. This is the retired decide() test's
	// "malformed" vector (TestSNIDecideRouting, now removed) — decide()'s
	// single-shot parser had no way to say "need more bytes", so it fail-closed
	// on every parse error including ErrIncomplete and pinned this vector to a
	// drop, a policy the production TCP path never had. decideTCP's reassembler
	// correctly recognizes a declared length exceeding the buffered bytes as
	// merely INCOMPLETE, not malformed, and forwards the segment unmarked so the
	// next one re-queues onto the same flow (egress_sni_incomplete) — this does
	// not fail open: this branch can never yield an approved connmark (see
	// decideTCP's doc comment), and a genuinely unparsable record still drops,
	// as the content_type-0x17 case above proves.
	if d := l.decideTCP("10.0.1.10", 1007, []byte{0x16, 0x03, 0x01, 0xff, 0xff, 0x01}); d.Action != sniPassthrough || d.Reason != "egress_sni_incomplete" {
		t.Fatalf("declared-length-exceeds-buffer vector -> %v/%q, want passthrough/egress_sni_incomplete (not a drop; see comment above)", d.Action, d.Reason)
	}
}

// TestSNIDecideTCPMultiRecordClientHello is the H3 regression at the production
// layer: a ClientHello split across two TLS records, one per TCP segment, must
// reach the SAME verdict as the one-record form. Before the record-layer fix the
// parse stayed pinned to record #1, so every segment took the !done branch and
// was ACCEPTed unmarked — NF_ACCEPT bypasses the FORWARD chain's default REJECT,
// so a denied host was reachable with no verdict ever being taken.
func TestSNIDecideTCPMultiRecordClientHello(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})

	for _, tc := range []struct {
		name       string
		sni        string
		wantAction sniAction
		wantReason string
		sport      uint16
	}{
		{"denied host over 2 records", "evil.test", sniDrop, "egress_sni_denied", 2001},
		{"allowed host over 2 records", "api.anthropic.com", sniAcceptMark, "egress_sni_allowed", 2002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recs := splitHelloIntoRecords(mustHello(t, tc.sni), 2)
			if len(recs) != 2 {
				t.Fatalf("test setup: want 2 records, got %d", len(recs))
			}
			// Segment 1 carries only record #1: genuinely incomplete, forwarded
			// unmarked but now COUNTED (reason egress_sni_incomplete).
			if d := l.decideTCP("10.0.1.10", tc.sport, recs[0]); d.Action != sniPassthrough || d.Reason != "egress_sni_incomplete" {
				t.Fatalf("segment 1 -> %v/%q, want passthrough/egress_sni_incomplete", d.Action, d.Reason)
			}
			// Segment 2 completes the ClientHello across the record boundary.
			d := l.decideTCP("10.0.1.10", tc.sport, recs[1])
			if d.Action != tc.wantAction || d.Reason != tc.wantReason || d.SNI != tc.sni {
				t.Fatalf("segment 2 -> action=%v reason=%q sni=%q; want %v/%q/%q",
					d.Action, d.Reason, d.SNI, tc.wantAction, tc.wantReason, tc.sni)
			}
		})
	}
}

// TestSNIDecideTCPMultiRecordSingleSegment covers both records arriving in ONE
// TCP segment — the record split alone (no TCP fragmentation) was equally fatal,
// since the pin to record #1 is at the record layer, not the segment layer.
func TestSNIDecideTCPMultiRecordSingleSegment(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})

	both := bytes.Join(splitHelloIntoRecords(mustHello(t, "evil.test"), 2), nil)
	d := l.decideTCP("10.0.1.10", 3001, both)
	if d.Action != sniDrop || d.Reason != "egress_sni_denied" || d.SNI != "evil.test" {
		t.Fatalf("2 records in 1 segment -> action=%v reason=%q sni=%q; want drop/egress_sni_denied/evil.test",
			d.Action, d.Reason, d.SNI)
	}
}

// TestSNIDecideTCPStalledFlowIsBounded pins that the passthrough branch cannot be
// ridden indefinitely: a guest that dribbles handshake records which never
// complete a ClientHello is forwarded unmarked only until the reassembler's byte
// cap trips, after which the flow is terminally denied. This is the property
// ADR-0002 assumed when it dismissed pre-decision partial forwarding as harmless.
func TestSNIDecideTCPStalledFlowIsBounded(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})

	empty := []byte{0x16, 0x03, 0x01, 0x00, 0x00} // handshake record, zero-length payload
	var denied bool
	for i := 0; i < 1<<14; i++ {
		d := l.decideTCP("10.0.1.10", 4001, empty)
		if d.Action == sniAcceptMark {
			t.Fatalf("stalled flow must never be approved (iteration %d)", i)
		}
		if d.Action == sniDrop {
			if d.Reason != "egress_sni_unparsed" {
				t.Fatalf("stalled flow terminal drop reason = %q, want egress_sni_unparsed", d.Reason)
			}
			denied = true
			break
		}
	}
	if !denied {
		t.Fatal("stalled flow was forwarded forever; the reassembly bound never tripped")
	}
}

// --- Task 6: recordVerdict / auditDeny (audit + metric emit) ---
//
// applyVerdict itself needs a live *nfqueue.Nfqueue (root + netfilter, see
// Start's doc comment) so it is exercised only by the Task 7 KVM e2e.
// recordVerdict/auditDeny factor the Task 6 audit/metric side effects out of
// that nf-dependent path, so they are unit-testable here in isolation.

func TestSNIRecordVerdictAuditsDenyWithTenant(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, nil)
	entry := sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Profile: "p"}

	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test"}, protoTCP)

	recs, err := anvilmcp.ReadRuntimeAudit(auditPath)
	if err != nil || len(recs) != 1 {
		t.Fatalf("read err=%v n=%d", err, len(recs))
	}
	rec := recs[0]
	if rec.SNI != "evil.test" || rec.TenantID != "t1" || rec.VMID != "vm-1" ||
		rec.DaemonOperation != "egress_sni_denied" || rec.ResultCode != "denied" ||
		rec.ToolName != "egress_sni_filter" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	// redaction: no field beyond {timestamp, tenant, vmid, tool, op, result,
	// sni} should ever be populated — no tokens/authorization/args/profile.
	if rec.SessionAlias != "" || rec.Error != "" {
		t.Fatalf("unexpected extra fields leaked into audit record: %+v", rec)
	}
}

func TestSNIRecordVerdictNoAuditWithoutTenant(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, nil)
	entry := sniRegistryEntry{VMID: "vm-2", Profile: "p"} // no TenantID

	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test"}, protoTCP)

	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Fatalf("expected no audit file written without tenant (must degrade to slog), stat err=%v", err)
	}
}

func TestSNIRecordVerdictDegradesOnEmptyAuditPath(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil) // auditPath intentionally empty (pre-Task-6 daemon wiring)
	entry := sniRegistryEntry{VMID: "vm-3", TenantID: "t3"}

	// AppendRuntimeAudit rejects an empty path; must degrade to slog, not panic.
	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test"}, protoTCP)
}

func TestSNIRecordVerdictUnparsedNoAudit(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, nil)
	entry := sniRegistryEntry{VMID: "vm-5", TenantID: "t5"}

	// No SNI was parsed, so there is nothing worth auditing — only the metric fires.
	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"}, protoTCP)

	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Fatalf("expected no audit record for unparsed (no SNI) deny, stat err=%v", err)
	}
}

func TestSNIRecordVerdictNilMetricsSafe(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil) // metrics intentionally nil
	entry := sniRegistryEntry{VMID: "vm-4", TenantID: "t4"}

	// Must not panic on a nil *daemonMetrics receiver.
	l.recordVerdict(entry, sniDecision{Action: sniAcceptMark, SNI: "ok.test"}, protoTCP)
	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"}, protoTCP)
}

func TestSNIRecordVerdictMetricAlwaysIncrements(t *testing.T) {
	cp := newMetricsTestCP(t)
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, cp.metrics)

	// One deny with a tenant (audit succeeds) and one without (degrades to
	// slog) — the metric must increment identically in both cases.
	l.recordVerdict(sniRegistryEntry{VMID: "vm-a", TenantID: "ta"}, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test"}, protoTCP)
	l.recordVerdict(sniRegistryEntry{VMID: "vm-b"}, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil2.test"}, protoTCP)
	l.recordVerdict(sniRegistryEntry{VMID: "vm-c", TenantID: "tc"}, sniDecision{Action: sniAcceptMark, SNI: "ok.test"}, protoTCP)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{proto="tcp",outcome="denied"} 2`) {
		t.Fatalf("expected denied=2 regardless of tenant/audit outcome, got:\n%s", out)
	}
	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{proto="tcp",outcome="allowed"} 1`) {
		t.Fatalf("expected allowed=1, got:\n%s", out)
	}
}

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
	// The counter is always registered, so its # HELP/# TYPE header lines are
	// present regardless of observations (see internal/metrics/exposition.go
	// writeCounterVec) — assert on the absence of a data sample line instead of
	// the bare metric name.
	if strings.Contains(string(body), `ephemera_egress_sni_ech_observed_total{`) {
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
	// See TestSNIRecordVerdictNoECHWhenNotObserved: HELP/TYPE lines are always
	// present for a registered counter, so assert on the data sample line.
	if strings.Contains(string(body), `ephemera_egress_sni_ech_observed_total{`) {
		t.Fatalf("ech counter must not fire on denied verdict, got:\n%s", string(body))
	}
}

// TestSNIRecordVerdictCountsIncompletePassthrough closes the H3 observability
// gap: the TCP hook's "ClientHello not complete yet" branch FORWARDS the segment
// unmarked, so those bytes leave the host without any policy decision — the one
// verdict path that lets traffic out unclassified. Before this it emitted no
// metric and no audit at all, which is why a ClientHello that could never
// complete forwarded ~16 KiB with a zero reading on every counter.
//
// The bare-ACK passthrough (no reason) stays deliberately uncounted: it fires on
// every empty-payload segment of every flow and carries no classification signal.
func TestSNIRecordVerdictCountsIncompletePassthrough(t *testing.T) {
	cp := newMetricsTestCP(t)
	l := newSNIVerdictLoop(88, "", cp.metrics)
	entry := sniRegistryEntry{VMID: "vm-i", TenantID: "ti"}

	l.recordVerdict(entry, sniDecision{Action: sniPassthrough, Reason: "egress_sni_incomplete"}, protoTCP)
	l.recordVerdict(entry, sniDecision{Action: sniPassthrough, Reason: "egress_sni_incomplete"}, protoTCP)
	l.recordVerdict(entry, sniDecision{Action: sniPassthrough}, protoTCP) // bare ACK: uncounted

	rec := httptest.NewRecorder()
	cp.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := rec.Body.String()

	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{proto="tcp",outcome="incomplete"} 2`) {
		t.Fatalf("expected incomplete=2 (bare-ACK passthrough uncounted), got:\n%s", out)
	}
	// A forwarded-but-unclassified segment is neither an allow nor a policy deny.
	if strings.Contains(out, `ephemera_egress_sni_verdict_total{proto="tcp",outcome="allowed"}`) ||
		strings.Contains(out, `ephemera_egress_sni_verdict_total{proto="tcp",outcome="denied"}`) {
		t.Fatalf("incomplete passthrough must not be counted as allowed/denied, got:\n%s", out)
	}
}

// TestSNIRecordVerdictIncompletePassthroughNoAudit pins that the new passthrough
// counter did not also open an audit path: no SNI has been parsed yet, so there
// is no domain worth recording (same rule as the egress_sni_unparsed deny).
func TestSNIRecordVerdictIncompletePassthroughNoAudit(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, nil) // nil metrics: also pins nil-safety

	l.recordVerdict(sniRegistryEntry{VMID: "vm-i", TenantID: "ti"},
		sniDecision{Action: sniPassthrough, Reason: "egress_sni_incomplete"}, protoTCP)

	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Fatalf("expected no audit record for an unclassified passthrough, stat err=%v", err)
	}
}

func TestSNIRecordVerdictECHNilMetricsSafe(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil) // metrics intentionally nil
	// Must not panic on a nil *daemonMetrics receiver.
	l.recordVerdict(sniRegistryEntry{VMID: "vm-d", TenantID: "td"},
		sniDecision{Action: sniAcceptMark, SNI: "ok.test", ECHObserved: true}, protoTCP)
}
