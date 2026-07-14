package quic

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"ephemera/internal/network/sni"
)

// errIncompleteInDatagram means the ClientHello was not fully carried within this
// single Initial datagram (a CRYPTO offset gap, a truncated frame, or no CRYPTO
// data at all). anvil inspects only the first Initial datagram, so a ClientHello
// that spans several datagrams is treated as terminal — the verdict loop fails
// closed (deny) rather than buffering across packets.
var errIncompleteInDatagram = errors.New("quic: clienthello not complete within datagram")

// errInconsistentOverlap means two CRYPTO frames cover an overlapping byte range
// with conflicting contents. Letting frame ordering decide which bytes win is the
// QUIC analog of TCP-overlap evasion — a malicious guest could arrange overlapping
// CRYPTO chunks so anvil reads a different SNI than the destination server does — so
// reassembly fails closed here. Identical (idempotent) overlaps are still tolerated.
var errInconsistentOverlap = errors.New("quic: inconsistent overlapping CRYPTO frames")

// ParseInitialSNI decrypts one QUIC Initial datagram, reassembles its CRYPTO
// frames into the TLS ClientHello, and returns the lowercased SNI. Every failure
// is terminal (fail-closed): a malformed/unsupported/undecryptable packet, a
// ClientHello not complete within the datagram, or a ClientHello with no SNI all
// return an error. It never panics.
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
		// Every sni error is terminal here, including sni.ErrIncomplete: anvil does
		// not reassemble a ClientHello across datagrams, so "need more bytes" is a
		// deny, not a request for more input.
		return "", fmt.Errorf("quic clienthello: %w", err)
	}
	return name, nil
}

// reassembleCryptoFrames walks the frames of a decrypted QUIC Initial payload,
// concatenates the CRYPTO frames (type 0x06) in offset order, and returns the
// reassembled handshake bytes. PADDING (0x00), PING (0x01), and ACK (0x02/0x03)
// frames are skipped. Any other frame type — or a truncated/gapped CRYPTO stream —
// is terminal: an unknown frame type cannot be length-skipped safely, so we fail
// closed rather than guess. Overlapping CRYPTO frames are tolerated only when their
// overlapping bytes are identical; a conflicting overlap is terminal
// (errInconsistentOverlap) so frame ordering can never change the reassembled bytes
// (fail-closed against SNI-evasion). All slice access is bounds-checked; it never panics.
func reassembleCryptoFrames(payload []byte) ([]byte, error) {
	type cryptoChunk struct {
		offset uint64
		data   []byte
	}
	var chunks []cryptoChunk

	i := 0
	for i < len(payload) {
		switch frameType := payload[i]; {
		case frameType == 0x00, frameType == 0x01: // PADDING, PING: single byte.
			i++
		case frameType == 0x02, frameType == 0x03: // ACK (with ECN counts when 0x03).
			n, err := skipAckFrame(payload[i:])
			if err != nil {
				return nil, err
			}
			i += n
		case frameType == 0x06: // CRYPTO: type(1) offset(varint) length(varint) data.
			j := i + 1
			offset, m, ok := readVarint(payload[j:])
			if !ok {
				return nil, errIncompleteInDatagram
			}
			j += m
			length, m, ok := readVarint(payload[j:])
			if !ok {
				return nil, errIncompleteInDatagram
			}
			j += m
			end := uint64(j) + length
			if end > uint64(len(payload)) {
				return nil, errIncompleteInDatagram // truncated CRYPTO data.
			}
			chunks = append(chunks, cryptoChunk{offset: offset, data: payload[j:end]})
			i = int(end)
		default:
			// A frame type we do not model cannot be skipped without knowing its
			// length; fail closed rather than risk misreading the frame stream.
			return nil, fmt.Errorf("quic: unexpected frame type 0x%02x in Initial", frameType)
		}
	}

	if len(chunks) == 0 {
		return nil, errIncompleteInDatagram
	}
	sort.Slice(chunks, func(a, b int) bool { return chunks[a].offset < chunks[b].offset })

	var out []byte
	var next uint64
	for _, c := range chunks {
		end := c.offset + uint64(len(c.data))
		if c.offset > next {
			return nil, errIncompleteInDatagram // gap: bytes [next, offset) are missing.
		}
		// This chunk overlaps the already-reassembled region [0,next) on
		// [c.offset, min(end,next)). The overlapping bytes must match byte-for-byte;
		// a conflicting overlap is an SNI-evasion attempt, so fail closed. Slice
		// bounds are safe: c.offset <= next == len(out), and the overlap length is
		// at most len(c.data).
		overlapEnd := end
		if next < overlapEnd {
			overlapEnd = next
		}
		if overlapEnd > c.offset && !bytes.Equal(out[c.offset:overlapEnd], c.data[:overlapEnd-c.offset]) {
			return nil, errInconsistentOverlap
		}
		if end <= next {
			continue // fully overlapping duplicate; already verified consistent.
		}
		out = append(out, c.data[next-c.offset:]...) // append only the new tail.
		next = end
	}
	return out, nil
}

// skipAckFrame returns the byte length of the ACK frame at the start of b (b[0] is
// 0x02 or 0x03), so the caller can advance past it. It parses only the varint
// structure (RFC 9000 §19.3) and never reads the acknowledged ranges' semantics.
// Any truncation is terminal (errIncompleteInDatagram); the loop is bounded because
// every field consumes at least one byte, so a bogus range count cannot spin.
func skipAckFrame(b []byte) (int, error) {
	i := 1 // past the frame type byte.
	next := func() bool {
		_, n, ok := readVarint(b[i:])
		if !ok {
			return false
		}
		i += n
		return true
	}
	// Largest Acknowledged, ACK Delay, ACK Range Count, First ACK Range.
	if !next() || !next() {
		return 0, errIncompleteInDatagram
	}
	rangeCount, n, ok := readVarint(b[i:])
	if !ok {
		return 0, errIncompleteInDatagram
	}
	i += n
	if !next() { // First ACK Range.
		return 0, errIncompleteInDatagram
	}
	for k := uint64(0); k < rangeCount; k++ {
		if !next() || !next() { // Gap, ACK Range Length.
			return 0, errIncompleteInDatagram
		}
	}
	if b[0] == 0x03 { // ECN counts: ECT0, ECT1, ECN-CE.
		if !next() || !next() || !next() {
			return 0, errIncompleteInDatagram
		}
	}
	return i, nil
}
