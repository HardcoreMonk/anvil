package quic

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

var (
	saltV1 = []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a}
	saltV2 = []byte{0x0d, 0xed, 0xe3, 0xde, 0xf7, 0x00, 0xa6, 0xdb, 0x81, 0x93, 0x81, 0xbe, 0x6e, 0x26, 0x9d, 0xcb, 0xf9, 0xbd, 0x2e, 0xd9}
)

// versionParams는 version별 salt와 key/iv/hp label을 준다.
func versionParams(version uint32) (salt []byte, keyLabel, ivLabel, hpLabel string, ok bool) {
	switch version {
	case 0x00000001:
		return saltV1, "quic key", "quic iv", "quic hp", true
	case 0x6b3343cf:
		return saltV2, "quicv2 key", "quicv2 iv", "quicv2 hp", true
	default:
		return nil, "", "", "", false
	}
}

// hkdfExpandLabel = TLS 1.3 HKDF-Expand-Label (RFC 8446 §7.1): label에 "tls13 " 접두.
func hkdfExpandLabel(secret []byte, label string, length int) []byte {
	full := "tls13 " + label
	info := make([]byte, 0, 2+1+len(full)+1)
	info = binary.BigEndian.AppendUint16(info, uint16(length))
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, 0) // zero-length context
	out := make([]byte, length)
	r := hkdf.Expand(sha256.New, secret, info)
	io.ReadFull(r, out)
	return out
}

func deriveClientInitialKeys(dcid []byte, version uint32) (key, iv, hp []byte, err error) {
	salt, keyLabel, ivLabel, hpLabel, ok := versionParams(version)
	if !ok {
		return nil, nil, nil, fmt.Errorf("unsupported quic version 0x%08x", version)
	}
	initialSecret := hkdf.Extract(sha256.New, dcid, salt)
	clientSecret := hkdfExpandLabel(initialSecret, "client in", 32)
	key = hkdfExpandLabel(clientSecret, keyLabel, 16)
	iv = hkdfExpandLabel(clientSecret, ivLabel, 12)
	hp = hkdfExpandLabel(clientSecret, hpLabel, 16)
	return key, iv, hp, nil
}
