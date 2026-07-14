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
// lowercased server_name. ErrIncomplete means the caller should feed more bytes;
// any other error (including ErrNoSNI) is terminal and the caller must fail closed.
func ParseClientHelloSNI(b []byte) (string, error) {
	// TLS record header: type(1) version(2) length(2)
	if len(b) < 5 {
		return "", ErrIncomplete
	}
	if b[0] != 0x16 {
		return "", fmt.Errorf("not a handshake record (type 0x%02x)", b[0])
	}
	recLen := int(binary.BigEndian.Uint16(b[3:5]))
	if len(b) < 5+recLen {
		return "", ErrIncomplete
	}
	return ParseHandshakeSNI(b[5 : 5+recLen])
}

// ParseHandshakeSNI parses a TLS handshake message (starting at the ClientHello
// handshake header, msg_type 0x01) and returns the lowercased server_name.
// QUIC carries the handshake directly in CRYPTO frames (no TLS record), so the
// QUIC path calls this after reassembly; ParseClientHelloSNI calls it after
// stripping the TLS record layer. ErrIncomplete = need more bytes; ErrNoSNI and
// other errors are terminal.
func ParseHandshakeSNI(hs []byte) (string, error) {
	// Handshake header: msg_type(1) length(3)
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

	// client_version(2) random(32) then variable fields.
	p := 2 + 32
	if len(body) < p+1 {
		return "", fmt.Errorf("clienthello truncated before session_id")
	}
	sidLen := int(body[p])
	p += 1 + sidLen
	if len(body) < p+2 {
		return "", fmt.Errorf("clienthello truncated before cipher_suites")
	}
	csLen := int(binary.BigEndian.Uint16(body[p:]))
	p += 2 + csLen
	if len(body) < p+1 {
		return "", fmt.Errorf("clienthello truncated before compression")
	}
	compLen := int(body[p])
	p += 1 + compLen
	if len(body) < p+2 {
		// No extensions at all -> no SNI.
		return "", ErrNoSNI
	}
	extTotal := int(binary.BigEndian.Uint16(body[p:]))
	p += 2
	if len(body) < p+extTotal {
		return "", fmt.Errorf("clienthello truncated extensions")
	}
	ext := body[p : p+extTotal]

	for len(ext) >= 4 {
		etype := binary.BigEndian.Uint16(ext[0:2])
		elen := int(binary.BigEndian.Uint16(ext[2:4]))
		if len(ext) < 4+elen {
			return "", fmt.Errorf("truncated extension 0x%04x", etype)
		}
		data := ext[4 : 4+elen]
		if etype == 0x0000 { // server_name
			return parseServerName(data)
		}
		ext = ext[4+elen:]
	}
	return "", ErrNoSNI
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

// Feed appends segment and re-attempts a parse. It returns (sni, true, nil) once
// a full ClientHello is parsed, ("", false, nil) when more bytes are needed, and
// ("", false, err) on any terminal error (malformed, no-SNI, or the buffer bound
// being exceeded) — which the caller must treat as fail-closed.
func (r *Reassembler) Feed(segment []byte) (string, bool, error) {
	if len(r.buf)+len(segment) > maxClientHelloBytes {
		return "", false, fmt.Errorf("clienthello exceeds %d bytes", maxClientHelloBytes)
	}
	r.buf = append(r.buf, segment...)
	sni, err := ParseClientHelloSNI(r.buf)
	if errors.Is(err, ErrIncomplete) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return sni, true, nil
}
