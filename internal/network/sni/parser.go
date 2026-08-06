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

// recordTypeHandshake is the TLS record content_type for handshake records
// (RFC 8446 §5.1). It is the only content_type the ClientHello walk consumes.
const recordTypeHandshake = 0x16

// recordHeaderLen is the TLS record header: type(1) legacy_version(2) length(2).
const recordHeaderLen = 5

// coalesceHandshakeRecords flattens the TLS record layer at the front of b: it
// walks the leading run of COMPLETE handshake records (content_type 0x16) and
// concatenates their payloads into one handshake byte stream.
//
// This loop is the fix for the multi-record fail-open: RFC 8446 §5.1 explicitly
// permits a single handshake message to span several records, so reading only
// the first record's length and handing b[5:5+recLen] to the handshake parser
// pins the parse to record #1 forever. A ClientHello split across two records
// then never completes — every Feed returns ErrIncomplete, and the TCP hook's
// non-terminal branch forwards each segment unmarked, so unbounded unclassified
// bytes reach the server (fail-OPEN) until the byte cap finally trips.
//
// It stops at the first record that is not a complete handshake record and
// reports which case ended the run:
//
//   - blocked == true: the next record's content_type is NOT handshake, so no
//     further handshake bytes can ever follow the ones returned. If the returned
//     bytes do not already contain a whole ClientHello the caller MUST fail
//     closed — waiting for more would stall forever, which is the fail-open
//     shape this function exists to remove.
//   - blocked == false: the buffer merely ends mid-header or mid-record body;
//     more bytes may still complete the handshake, so the caller waits.
//
// The single-record case (the overwhelming majority) returns a subslice of b
// with no copy, exactly as the pre-fix code did.
func coalesceHandshakeRecords(b []byte) (hs []byte, blocked bool, blockedType byte) {
	var first []byte
	n := 0
	for off := 0; len(b)-off >= recordHeaderLen; {
		if b[off] != recordTypeHandshake {
			blocked, blockedType = true, b[off]
			break
		}
		recLen := int(binary.BigEndian.Uint16(b[off+3 : off+recordHeaderLen]))
		if recLen > len(b)-off-recordHeaderLen {
			break // record body has not fully arrived yet
		}
		payload := b[off+recordHeaderLen : off+recordHeaderLen+recLen]
		off += recordHeaderLen + recLen
		n++
		switch n {
		case 1:
			first = payload // single-record fast path: no copy
		case 2:
			hs = make([]byte, 0, len(first)+len(payload))
			hs = append(hs, first...)
			hs = append(hs, payload...)
		default:
			hs = append(hs, payload...)
		}
	}
	if n == 1 {
		return first, blocked, blockedType
	}
	return hs, blocked, blockedType
}

// ParseClientHelloSNI parses reassembled TLS record bytes and returns the
// lowercased server_name plus whether an ECH extension was present (best-effort;
// echPresent is only meaningful when err == nil). ErrIncomplete means the caller
// should feed more bytes; any other error (including ErrNoSNI) is terminal and
// the caller must fail closed.
//
// It owns the TLS record layer — flattening the leading run of handshake records
// (see coalesceHandshakeRecords) before handing one contiguous handshake stream
// to ParseHandshakeSNI. QUIC has no record layer and calls ParseHandshakeSNI
// directly, so that function's contract is untouched by the flattening.
func ParseClientHelloSNI(b []byte) (string, bool, error) {
	hs, blocked, blockedType := coalesceHandshakeRecords(b)
	name, ech, err := ParseHandshakeSNI(hs)
	if blocked && errors.Is(err, ErrIncomplete) {
		// The record run is cut short by a non-handshake record before the
		// ClientHello is complete: no more handshake bytes can arrive, so
		// "need more bytes" would never be satisfied. Fail closed instead.
		// (When the ClientHello IS already complete, err is nil and this branch
		// is skipped — trailing non-handshake records, e.g. the 0-RTT
		// change_cipher_spec of RFC 8446 Appendix D.4, are simply not consumed.)
		return "", false, fmt.Errorf("not a handshake record (type 0x%02x)", blockedType)
	}
	return name, ech, err
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
// It reassembles BOTH layers a ClientHello can be fragmented over: TCP segment
// boundaries (by buffering) and TLS record boundaries (via ParseClientHelloSNI's
// record-layer flattening, RFC 8446 §5.1). Neither fragmentation is a parse
// failure — a ClientHello split either way completes and gets classified, rather
// than staying "incomplete" forever while its segments are forwarded unmarked.
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
