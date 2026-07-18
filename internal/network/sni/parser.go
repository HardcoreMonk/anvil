package sni

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNoSNI      = errors.New("clienthello has no server_name")
	ErrIncomplete = errors.New("clienthello incomplete, need more bytes")
)

const maxClientHelloBytes = 16384 // TLS record max; bound the reassembly buffer

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

func parseServerName(data []byte) (string, error) {
	// server_name_list: list_len(2) then entries of name_type(1) name_len(2) name.
	if len(data) < 2 {
		return "", ErrNoSNI
	}
	listLen := int(binary.BigEndian.Uint16(data[0:2]))
	list := data[2:]
	if len(list) < listLen {
		return "", fmt.Errorf("truncated server_name_list")
	}
	list = list[:listLen]
	for len(list) >= 3 {
		nameType := list[0]
		nameLen := int(binary.BigEndian.Uint16(list[1:3]))
		if len(list) < 3+nameLen {
			return "", fmt.Errorf("truncated server_name entry")
		}
		if nameType == 0x00 { // host_name
			return normalizeServerName(list[3 : 3+nameLen])
		}
		list = list[3+nameLen:]
	}
	return "", ErrNoSNI
}

// normalizeServerName canonicalizes the raw host_name bytes into the form the
// allow-list matcher expects, or returns a terminal (fail-closed) error.
//
//   - Charset: a compliant SNI is an ASCII A-label (IDN hosts arrive as
//     punycode "xn--..."). Control bytes (< 0x20, 0x7f) and high bytes (>= 0x80)
//     never appear in a legitimate SNI, so we reject them as malformed rather
//     than forward exotic bytes into the matcher or the logs (defense in depth;
//     also closes the "matcher does not validate charset" gap from Task 1 review).
//   - Trailing dot: RFC 6066 forbids a trailing dot, but some clients append the
//     DNS root label. We STRIP a SINGLE trailing dot (canonicalize) instead of
//     rejecting so "host." matches the same allow-list entry as "host". This is
//     safe against under-blocking: the matcher default-denies, so stripping can
//     only fold a name to its canonical form, never conjure a new allowed host.
//     A residual trailing dot after that (an empty label, e.g. "host..") is
//     malformed and rejected — we tolerate exactly the one legitimate deviation.
func normalizeServerName(raw []byte) (string, error) {
	for _, c := range raw {
		if c < 0x20 || c >= 0x7f {
			return "", fmt.Errorf("server_name has non-printable/non-ASCII byte 0x%02x", c)
		}
	}
	name := strings.ToLower(strings.TrimSuffix(string(raw), "."))
	if name == "" {
		return "", fmt.Errorf("empty server_name")
	}
	if strings.HasSuffix(name, ".") {
		return "", fmt.Errorf("server_name has empty trailing label")
	}
	return name, nil
}

// Reassembler accumulates TCP payload segments until a full ClientHello parses.
type Reassembler struct{ buf []byte }

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
