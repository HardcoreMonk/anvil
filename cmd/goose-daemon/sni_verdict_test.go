package main

import (
	"bytes"
	"encoding/binary"
	"testing"

	"ephemera/internal/network/sni"
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
	if got, err := sni.ParseClientHelloSNI(b); err != nil || got != name {
		t.Fatalf("oracle: built hello for %q parsed as %q err=%v", name, got, err)
	}
	return b
}

func TestSNIDecideRouting(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com", "*.example.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Profile: "p", Matcher: m})

	// allowed exact
	if d := l.decide("10.0.1.10", mustHello(t, "api.anthropic.com")); d.Action != sniAcceptMark {
		t.Fatalf("allowed exact -> %v (%s)", d.Action, d.Reason)
	}
	// allowed wildcard
	if d := l.decide("10.0.1.10", mustHello(t, "cdn.example.com")); d.Action != sniAcceptMark {
		t.Fatalf("allowed wildcard -> %v", d.Action)
	}
	// denied
	d := l.decide("10.0.1.10", mustHello(t, "evil.test"))
	if d.Action != sniDrop || d.Reason != "egress_sni_denied" || d.SNI != "evil.test" {
		t.Fatalf("denied -> action=%v reason=%q sni=%q", d.Action, d.Reason, d.SNI)
	}
	// handshake / no application payload -> passthrough
	if d := l.decide("10.0.1.10", []byte{}); d.Action != sniPassthrough {
		t.Fatalf("empty payload -> %v, want passthrough", d.Action)
	}
	// unregistered source -> fail-closed drop
	if d := l.decide("10.0.1.99", mustHello(t, "api.anthropic.com")); d.Action != sniDrop {
		t.Fatalf("unregistered -> %v, want drop", d.Action)
	}
	// malformed application payload -> fail-closed drop
	if d := l.decide("10.0.1.10", []byte{0x16, 0x03, 0x01, 0xff, 0xff, 0x01}); d.Action != sniDrop {
		t.Fatalf("malformed -> %v, want drop", d.Action)
	}
}

func TestSNIDeregisterFailsClosed(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})
	l.Deregister("10.0.1.10")
	if d := l.decide("10.0.1.10", mustHello(t, "api.anthropic.com")); d.Action != sniDrop {
		t.Fatal("deregistered source must fail closed")
	}
}
