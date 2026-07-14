package quic

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
)

// QUIC long-header first-byte bits (RFC 9000 §17.2).
const (
	longHeaderBit  = 0x80 // Header Form: 1 == long header.
	fixedBit       = 0x40 // Fixed Bit: must be 1 for these versions.
	longTypeMask   = 0x30 // Long Packet Type occupies bits 5-4.
	longTypeShift  = 4
	pnLenMask      = 0x03 // Low 2 bits of the (unprotected) first byte encode pn length-1.
	hpLongMask     = 0x0f // Header protection masks the low 4 bits of a long-header first byte.
	maxConnIDLen   = 20   // RFC 9000 §17.2: connection IDs are at most 20 bytes in v1/v2.
	hpSampleLen    = 16   // AES-based header protection samples 16 bytes.
	hpSampleOffset = 4    // Sample starts 4 bytes past the packet-number offset.
	aeadTagLen     = 16   // AES-128-GCM authentication tag length.
	versionV1      = 0x00000001
	versionV2      = 0x6b3343cf
	v1InitialType  = 0x0 // RFC 9000 §17.2: Initial = 0b00.
	v2InitialType  = 0x1 // RFC 9369 §3.2: Initial = 0b01.
)

// readVarint decodes a QUIC variable-length integer (RFC 9000 §16). The upper
// two bits of the first byte select the encoded length (1/2/4/8 bytes). It
// returns ok=false when b is empty or too short, so callers never panic.
func readVarint(b []byte) (val uint64, n int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	prefix := b[0] >> 6
	length := 1 << prefix
	if len(b) < length {
		return 0, 0, false
	}
	val = uint64(b[0] & 0x3f)
	for i := 1; i < length; i++ {
		val = val<<8 | uint64(b[i])
	}
	return val, length, true
}

// initialTypeForVersion returns the Long Packet Type value that denotes an
// Initial packet for the given version, plus whether the version is supported.
func initialTypeForVersion(version uint32) (initialType byte, supported bool) {
	switch version {
	case versionV1:
		return v1InitialType, true
	case versionV2:
		return v2InitialType, true
	default:
		return 0, false
	}
}

// decryptInitial parses the first QUIC Initial packet in datagram, removes
// header protection, and AEAD-decrypts it, returning the QUIC frame payload
// (RFC 9001 §5.2–5.4, RFC 9000 §17.2). A UDP datagram may coalesce several
// QUIC packets; only the first packet is examined and its Length field bounds
// it. Any malformed header, unsupported version, non-Initial packet, or AEAD
// failure returns an error (fail-closed); the function never panics and never
// mutates datagram.
func decryptInitial(datagram []byte) ([]byte, error) {
	// --- Long-header parsing (RFC 9000 §17.2) ---
	// first byte (1) + version (4) is the minimum before the DCID length byte.
	if len(datagram) < 5 {
		return nil, fmt.Errorf("quic: datagram too short for long header (%d bytes)", len(datagram))
	}
	first := datagram[0]
	if first&longHeaderBit == 0 {
		return nil, fmt.Errorf("quic: not a long-header packet")
	}
	if first&fixedBit == 0 {
		return nil, fmt.Errorf("quic: fixed bit not set")
	}
	version := binary.BigEndian.Uint32(datagram[1:5])
	initialType, supported := initialTypeForVersion(version)
	if !supported {
		return nil, fmt.Errorf("quic: unsupported version 0x%08x", version)
	}
	// The Long Packet Type bits (0x30) are not covered by header protection,
	// so they can be checked on the protected first byte.
	if (first&longTypeMask)>>longTypeShift != initialType {
		return nil, fmt.Errorf("quic: not an Initial packet (type bits 0x%02x)", first&longTypeMask)
	}

	off := 5
	// Destination Connection ID.
	if off >= len(datagram) {
		return nil, fmt.Errorf("quic: truncated before DCID length")
	}
	dcidLen := int(datagram[off])
	off++
	if dcidLen > maxConnIDLen {
		return nil, fmt.Errorf("quic: DCID length %d exceeds %d", dcidLen, maxConnIDLen)
	}
	if off+dcidLen > len(datagram) {
		return nil, fmt.Errorf("quic: truncated DCID")
	}
	dcid := datagram[off : off+dcidLen]
	off += dcidLen

	// Source Connection ID.
	if off >= len(datagram) {
		return nil, fmt.Errorf("quic: truncated before SCID length")
	}
	scidLen := int(datagram[off])
	off++
	if scidLen > maxConnIDLen {
		return nil, fmt.Errorf("quic: SCID length %d exceeds %d", scidLen, maxConnIDLen)
	}
	if off+scidLen > len(datagram) {
		return nil, fmt.Errorf("quic: truncated SCID")
	}
	off += scidLen

	// Token (Initial packets carry a variable-length token).
	tokenLen, n, ok := readVarint(datagram[off:])
	if !ok {
		return nil, fmt.Errorf("quic: malformed token length")
	}
	off += n
	if uint64(off)+tokenLen > uint64(len(datagram)) {
		return nil, fmt.Errorf("quic: truncated token")
	}
	off += int(tokenLen)

	// Length: number of bytes in packet number + protected payload + tag.
	length, n, ok := readVarint(datagram[off:])
	if !ok {
		return nil, fmt.Errorf("quic: malformed length field")
	}
	off += n
	pnOffset := off
	if uint64(pnOffset)+length > uint64(len(datagram)) {
		return nil, fmt.Errorf("quic: length %d exceeds datagram", length)
	}

	// --- Derive client Initial keys (Task 2) ---
	key, iv, hp, err := deriveClientInitialKeys(dcid, version)
	if err != nil {
		return nil, err
	}

	// --- Remove header protection (RFC 9001 §5.4.1) ---
	// Sample 16 bytes of ciphertext starting 4 bytes past the packet-number
	// offset (assuming the maximum 4-byte packet number).
	if pnOffset+hpSampleOffset+hpSampleLen > len(datagram) {
		return nil, fmt.Errorf("quic: datagram too short for header-protection sample")
	}
	sample := datagram[pnOffset+hpSampleOffset : pnOffset+hpSampleOffset+hpSampleLen]
	hpBlock, err := aes.NewCipher(hp)
	if err != nil {
		return nil, fmt.Errorf("quic: hp cipher: %w", err)
	}
	mask := make([]byte, hpSampleLen)
	hpBlock.Encrypt(mask, sample) // AES-ECB single-block encryption.

	// Unmask the low 4 bits of the first byte to recover the packet-number length.
	unprotectedFirst := first ^ (mask[0] & hpLongMask)
	pnLen := int(unprotectedFirst&pnLenMask) + 1
	// The Length field must cover the packet number plus the AEAD tag.
	if uint64(pnLen+aeadTagLen) > length {
		return nil, fmt.Errorf("quic: length %d too small for pn(%d)+tag", length, pnLen)
	}

	// Unmask the packet number and reconstruct its integer value.
	pnBytes := make([]byte, pnLen)
	var pn uint64
	for i := 0; i < pnLen; i++ {
		pnBytes[i] = datagram[pnOffset+i] ^ mask[1+i]
		pn = pn<<8 | uint64(pnBytes[i])
	}

	// --- AEAD nonce = iv XOR left-padded packet number (RFC 9001 §5.3) ---
	nonce := make([]byte, len(iv))
	copy(nonce, iv)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}

	// --- Associated data = the unprotected header (RFC 9001 §5.3) ---
	// Copy so we do not mutate the caller's datagram.
	aad := make([]byte, pnOffset+pnLen)
	copy(aad, datagram[:pnOffset+pnLen])
	aad[0] = unprotectedFirst
	copy(aad[pnOffset:], pnBytes)

	// --- AES-128-GCM Open (RFC 9001 §5.3) ---
	// Ciphertext is the protected payload including the 16-byte tag, bounded by
	// the Length field (first packet of a possibly coalesced datagram).
	ciphertext := datagram[pnOffset+pnLen : pnOffset+int(length)]
	aeadBlock, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("quic: aead cipher: %w", err)
	}
	aead, err := cipher.NewGCM(aeadBlock)
	if err != nil {
		return nil, fmt.Errorf("quic: gcm: %w", err)
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("quic: AEAD open failed: %w", err)
	}
	return plaintext, nil
}
