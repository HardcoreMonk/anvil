package quic

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"ephemera/internal/network/sni"
)

// errIncompleteInDatagram means the ClientHello was not fully carried within a
// single Initial datagram (a CRYPTO offset gap, a truncated frame, or no CRYPTO
// data at all). It is returned by the single-datagram convenience ParseInitialSNI
// (and by reassembleCryptoFrames, which reassembles within one datagram): those
// callers treat "not complete within this datagram" as terminal and fail closed. The
// multi-datagram InitialReassembler.Feed does NOT — it accumulates CRYPTO across as
// many datagrams as the ClientHello spans (see InitialReassembler), so a ClientHello
// spanning several datagrams reassembles rather than being denied, bounded only by
// the per-flow byte cap.
var errIncompleteInDatagram = errors.New("quic: clienthello not complete within datagram")

// errInconsistentOverlap means two CRYPTO frames cover an overlapping byte range
// with conflicting contents. Letting frame ordering decide which bytes win is the
// QUIC analog of TCP-overlap evasion — a malicious guest could arrange overlapping
// CRYPTO chunks so anvil reads a different SNI than the destination server does — so
// reassembly fails closed here. Identical (idempotent) overlaps are still tolerated.
var errInconsistentOverlap = errors.New("quic: inconsistent overlapping CRYPTO frames")

// maxQuicClientHelloBytes bounds a single flow's reassembly buffer. A modern
// post-quantum ClientHello (X25519MLKEM768) is ~1.5 KB and spans two Initial
// datagrams; this cap leaves generous headroom while a hostile guest cannot grow
// the per-flow buffer without bound (memory-exhaustion / offset-inflation DoS).
// Once the accumulation would exceed it, reassembly is terminal (fail-closed).
const maxQuicClientHelloBytes = 8192

// errOversizeClientHello means a flow's accumulated CRYPTO stream would exceed
// maxQuicClientHelloBytes. It is terminal: an oversized ClientHello is denied
// rather than buffered without limit.
var errOversizeClientHello = errors.New("quic: reassembled ClientHello exceeds per-flow byte cap")

// InitialReassembler accumulates the TLS ClientHello carried by the CRYPTO frames
// of one QUIC flow's Initial packets, across as many datagrams as the ClientHello
// spans. A modern post-quantum ClientHello does not fit in one Initial datagram, so
// the single-datagram parser fails closed on legitimate traffic; this reassembler
// buffers CRYPTO chunks by flow offset until the ClientHello is complete.
//
// The buffer is offset-indexed: buf[i] is flow byte i, and present[i] records
// whether byte i has been received. Overlapping bytes from different datagrams must
// agree byte-for-byte — a conflicting overlap is the cross-datagram analog of
// TCP-overlap evasion and is terminal (errInconsistentOverlap), so datagram arrival
// order can never change the reassembled bytes (fail-closed against SNI-evasion).
// A single InitialReassembler is not safe for concurrent use.
type InitialReassembler struct {
	buf        []byte // buf[i] is flow byte i once present[i] is true.
	present    []bool // present[i] reports whether flow byte i has been received.
	contiguous int    // length of the contiguous, all-present prefix from offset 0.
}

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

// ParseInitialSNI decrypts one QUIC Initial datagram, reassembles its CRYPTO
// frames into the TLS ClientHello, and returns the lowercased SNI. It is the
// single-datagram convenience over InitialReassembler: it feeds exactly one
// datagram, so a ClientHello that is not complete within it is terminal
// (errIncompleteInDatagram). Every failure is terminal (fail-closed): a
// malformed/unsupported/undecryptable packet, a ClientHello not complete within the
// datagram, or a ClientHello with no SNI all return an error. It never panics.
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

// cryptoChunk is one CRYPTO frame's payload keyed by its stream offset. data
// aliases the decrypted payload it was parsed from; callers that retain it past the
// payload's lifetime must copy the bytes.
type cryptoChunk struct {
	offset uint64
	data   []byte
}

// extractCryptoChunks walks the frames of a decrypted QUIC Initial payload and
// returns the CRYPTO frames (type 0x06) as offset-keyed chunks, in wire order.
// PADDING (0x00), PING (0x01), and ACK (0x02/0x03) frames are skipped. Any other
// frame type — or a truncated CRYPTO frame — is terminal: an unknown frame type
// cannot be length-skipped safely, so we fail closed rather than guess. It does not
// order, merge, or gap-check the chunks; callers (reassembleCryptoFrames within one
// datagram, InitialReassembler.Feed across datagrams) impose their own accumulation
// policy. All slice access is bounds-checked; it never panics.
func extractCryptoChunks(payload []byte) ([]cryptoChunk, error) {
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
	return chunks, nil
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
	chunks, err := extractCryptoChunks(payload)
	if err != nil {
		return nil, err
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
