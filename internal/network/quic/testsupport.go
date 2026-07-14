package quic

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
)

// BuildInitialForTest constructs a valid QUIC Initial datagram carrying the given
// TLS handshake bytes (a record-less ClientHello) in a CRYPTO frame, encrypted for
// the given version — the inverse of ParseInitialSNI. It derives the client Initial
// keys, wraps the handshake in a CRYPTO frame (offset 0) plus PADDING to the
// RFC 9000 §14.1 minimum of 1200 bytes, AES-128-GCM Seals the payload, and applies
// header protection, exactly mirroring decryptInitial so a round trip recovers the
// handshake.
//
// If version is not a supported QUIC version (v1/v2), the packet is built with v1
// keys but the header's Version field carries the caller's value verbatim, so tests
// can exercise decryptInitial's unsupported-version rejection.
//
// For tests only (this package's tests and the verdict loop's). Because Go test
// files cannot be imported across packages, this lives in a regular .go file so
// cmd/goose-daemon's tests can reuse it.
func BuildInitialForTest(dcid []byte, version uint32, handshake []byte) []byte {
	keyVersion := version
	if _, supported := initialTypeForVersion(version); !supported {
		keyVersion = versionV1
	}
	key, iv, hp, err := deriveClientInitialKeys(dcid, keyVersion)
	if err != nil {
		// keyVersion is always supported here, so derivation cannot fail; a panic
		// in a test helper surfaces a programming error loudly.
		panic("quic: BuildInitialForTest key derivation: " + err.Error())
	}
	initialType, _ := initialTypeForVersion(keyVersion)

	// Payload: one CRYPTO frame (type 0x06, offset 0, length, data) then PADDING.
	payload := []byte{0x06}
	payload = appendVarint(payload, 0)
	payload = appendVarint(payload, uint64(len(handshake)))
	payload = append(payload, handshake...)
	if len(payload) < 1200 {
		payload = append(payload, make([]byte, 1200-len(payload))...)
	}

	// Fixed 1-byte packet number of 0 keeps the encoding trivial; the value is
	// irrelevant to the round trip as long as build and parse agree.
	const pnLen = 1
	const pn uint64 = 0

	// Unprotected long header up to and including the packet number.
	first := byte(longHeaderBit | fixedBit | (initialType << longTypeShift) | (pnLen - 1))
	hdr := []byte{first}
	hdr = binary.BigEndian.AppendUint32(hdr, version)
	hdr = append(hdr, byte(len(dcid)))
	hdr = append(hdr, dcid...)
	hdr = append(hdr, 0x00)    // Source Connection ID length 0.
	hdr = appendVarint(hdr, 0) // Token Length 0.
	length := uint64(pnLen + len(payload) + aeadTagLen)
	hdr = appendVarint(hdr, length) // packet number + payload + AEAD tag.
	pnOffset := len(hdr)
	hdr = append(hdr, 0x00) // packet number (pn == 0, pnLen == 1).

	// AAD is the unprotected header; nonce is iv XOR the left-padded packet number.
	aad := append([]byte(nil), hdr...)
	nonce := append([]byte(nil), iv...)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}

	aeadBlock, err := aes.NewCipher(key)
	if err != nil {
		panic("quic: BuildInitialForTest aead cipher: " + err.Error())
	}
	aead, err := cipher.NewGCM(aeadBlock)
	if err != nil {
		panic("quic: BuildInitialForTest gcm: " + err.Error())
	}
	ciphertext := aead.Seal(nil, nonce, payload, aad)

	datagram := append(hdr, ciphertext...)

	// Apply header protection using a 16-byte sample taken 4 bytes past the
	// packet-number offset (assuming a 4-byte pn, per RFC 9001 §5.4.2).
	sampleStart := pnOffset + hpSampleOffset
	hpBlock, err := aes.NewCipher(hp)
	if err != nil {
		panic("quic: BuildInitialForTest hp cipher: " + err.Error())
	}
	mask := make([]byte, hpSampleLen)
	hpBlock.Encrypt(mask, datagram[sampleStart:sampleStart+hpSampleLen])
	datagram[0] ^= mask[0] & hpLongMask
	for i := 0; i < pnLen; i++ {
		datagram[pnOffset+i] ^= mask[1+i]
	}
	return datagram
}

// appendVarint appends v as a QUIC variable-length integer (RFC 9000 §16) using
// the minimal encoding length.
func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return append(b, byte(0x40|(v>>8)), byte(v))
	case v < 1<<30:
		return append(b, byte(0x80|(v>>24)), byte(v>>16), byte(v>>8), byte(v))
	default:
		return append(b, byte(0xc0|(v>>56)), byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}

// buildClientHelloHandshake encodes a minimal, wire-valid TLS 1.3 ClientHello as a
// record-less handshake message (msg_type 0x01 ... — no 5-byte TLS record header),
// which is the form QUIC CRYPTO frames carry and sni.ParseHandshakeSNI consumes. If
// serverName != "" a server_name (SNI) extension is included. Mirrors the encoding
// of sni.buildClientHello minus the record layer.
func buildClientHelloHandshake(serverName string) []byte {
	ext := &bytes.Buffer{}
	if serverName != "" {
		name := []byte(serverName)
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

	body := &bytes.Buffer{}
	body.Write([]byte{0x03, 0x03})             // client_version TLS 1.2
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

// buildClientHelloHandshakeNoSNI builds a ClientHello handshake with no server_name
// extension, so ParseInitialSNI must fail closed (no SNI to extract).
func buildClientHelloHandshakeNoSNI() []byte { return buildClientHelloHandshake("") }
