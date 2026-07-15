package quic

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// 이 파일은 QUIC "Initial 패킷"을 복호하기 위한 키를 유도한다.
//
// 왜 유도가 필요한가: QUIC 연결의 맨 처음(핸드셰이크 이전)에는 양측이 아직
// 협상한 비밀 키가 없다. 그런데도 첫 Initial 패킷(안에 TLS ClientHello가 들어
// 있다)은 암호화되어 있다. 그래서 RFC 9001 §5.2는 "누구나 알 수 있는 공개
// 재료"만으로 결정론적으로 키를 만든다:
//   - 첫 패킷 헤더에 평문으로 실려 오는 Destination Connection ID(DCID)
//   - QUIC 버전마다 스펙에 박혀 있는 고정 salt 상수
// 즉 Initial 암호화는 "기밀성"이 목적이 아니라 무결성/off-path 공격 완화가
// 목적이다. DCID와 버전만 보이면 경로 위의 누구든(=anvil의 egress 필터도)
// 같은 키를 유도해 ClientHello의 SNI를 관측할 수 있다. 이 파일이 바로 그
// "누구든"의 키 유도 절반을 담당한다.

// saltV1 / saltV2 — QUIC 버전별 Initial salt. 협상되는 값이 아니라 RFC에
// 그대로 박혀 있는 상수다(모든 구현이 동일 바이트를 쓴다). HKDF-Extract의
// salt로 들어가 initial_secret을 만든다.
//   - saltV1: QUICv1, RFC 9001 §5.2.
//   - saltV2: QUICv2, RFC 9369 §3.3.1.
var (
	saltV1 = []byte{0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3, 0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad, 0xcc, 0xbb, 0x7f, 0x0a}
	saltV2 = []byte{0x0d, 0xed, 0xe3, 0xde, 0xf7, 0x00, 0xa6, 0xdb, 0x81, 0x93, 0x81, 0xbe, 0x6e, 0x26, 0x9d, 0xcb, 0xf9, 0xbd, 0x2e, 0xd9}
)

// versionParams는 QUIC 버전 하나에 대해 그 버전의 Initial 키 유도에 필요한
// 4가지(salt + key/iv/hp label)를 돌려준다. label 문자열이 버전마다 다른 이유:
// 같은 DCID라도 버전이 다르면 서로 다른 키가 나오도록 스펙이 label을 분리했다
// (도메인 분리). ok=false면 우리가 모르는(지원하지 않는) 버전이라는 뜻이고,
// 호출자는 이 경우 fail-closed(복호 포기 → deny)로 처리한다.
//   - 0x00000001: QUICv1  (labels "quic key/iv/hp",   RFC 9001 §5.1)
//   - 0x6b3343cf: QUICv2  (labels "quicv2 key/iv/hp", RFC 9369 §3.3.3)
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

// hkdfExpandLabel은 TLS 1.3의 HKDF-Expand-Label(RFC 8446 §7.1)을 구현한다.
// QUIC의 키 유도는 전부 이 함수를 거친다(RFC 9001 §5.1).
//
// 평범한 HKDF-Expand와 다른 점은 "info"(문맥 바이트)를 아무렇게나 주지 않고
// 아래의 정해진 구조로 인코딩한다는 것이다. 이렇게 label을 구조화해 넣으면
// 같은 secret에서 파생되는 여러 키(key/iv/hp/…)가 서로 절대 겹치지 않는다.
//
//	struct {
//	    uint16 length          // 뽑아낼 키 길이(바이트)
//	    opaque label<7..255>   // 1바이트 길이 접두 + "tls13 "+label 문자열
//	    opaque context<0..255> // 1바이트 길이 접두 + 문맥(여기선 빈 문자열)
//	}
//
// TLS 1.3 원본은 label 앞에 "tls13 "를 붙이는데, QUIC도 이 접두를 그대로
// 재사용한다(RFC 9001). 그래서 예: 넘어온 label이 "quic key"면 실제로는
// "tls13 quic key"가 인코딩된다.
func hkdfExpandLabel(secret []byte, label string, length int) []byte {
	full := "tls13 " + label // TLS 1.3 접두를 붙인 최종 label 문자열.

	// info = length(2바이트 BE) ‖ label_len(1) ‖ full ‖ context_len(1=0)
	info := make([]byte, 0, 2+1+len(full)+1)
	info = binary.BigEndian.AppendUint16(info, uint16(length)) // 뽑을 길이(2바이트).
	info = append(info, byte(len(full)))                       // label 길이(1바이트).
	info = append(info, full...)                               // "tls13 "+label 본문.
	info = append(info, 0)                                     // 문맥 길이 0(빈 context).

	// HKDF-Expand로 원하는 length만큼 키 스트림을 뽑는다. 해시는 SHA-256
	// (Initial 키 유도는 항상 SHA-256 고정, RFC 9001 §5.2). hkdf.Expand는
	// io.Reader를 주므로 정확히 length바이트를 읽어 채운다. length는 항상
	// SHA-256 출력(32B) 이하라 ReadFull이 부족분을 낼 일이 없다(에러 무시 안전).
	out := make([]byte, length)
	r := hkdf.Expand(sha256.New, secret, info)
	io.ReadFull(r, out)
	return out
}

// deriveClientInitialKeys는 DCID와 QUIC 버전으로부터 "클라이언트가 보낸
// Initial 패킷"을 복호하는 데 필요한 세 키를 유도한다(RFC 9001 §5.2).
// anvil은 guest(클라이언트)가 서버로 보내는 ClientHello만 들여다보므로
// 클라이언트 쪽 키만 필요하다(서버 방향 키는 유도하지 않는다).
//
// 키 스케줄(위→아래로 유도):
//
//	initial_secret = HKDF-Extract(salt=버전salt, IKM=DCID)
//	client_secret  = Expand-Label(initial_secret, "client in", 32)   // 클라 방향 비밀
//	key = Expand-Label(client_secret, "<v> key", 16)  // AES-128-GCM 키(16B)
//	iv  = Expand-Label(client_secret, "<v> iv",  12)  // GCM nonce의 베이스(12B)
//	hp  = Expand-Label(client_secret, "<v> hp",  16)  // 헤더 보호용 AES 키(16B)
//
// 길이가 16/12/16인 이유: Initial은 AES-128-GCM 고정이라 키는 128비트(16B),
// GCM nonce는 12B, 헤더 보호도 AES-128이라 16B다. 반환된 key/iv/hp는
// decryptInitial이 각각 AEAD 복호·nonce 계산·헤더 보호 해제에 사용한다.
// 지원하지 않는 버전이면 에러를 돌려 호출자가 fail-closed 하도록 한다.
func deriveClientInitialKeys(dcid []byte, version uint32) (key, iv, hp []byte, err error) {
	salt, keyLabel, ivLabel, hpLabel, ok := versionParams(version)
	if !ok {
		return nil, nil, nil, fmt.Errorf("unsupported quic version 0x%08x", version)
	}
	// 1) 공개 재료(salt+DCID)를 HKDF-Extract로 섞어 initial_secret 생성.
	initialSecret := hkdf.Extract(sha256.New, dcid, salt)
	// 2) "client in"으로 클라이언트 방향 비밀만 뽑는다("server in"은 안 씀).
	clientSecret := hkdfExpandLabel(initialSecret, "client in", 32)
	// 3) 그 비밀에서 실제 사용 키 3종을 각자의 label로 파생.
	key = hkdfExpandLabel(clientSecret, keyLabel, 16)
	iv = hkdfExpandLabel(clientSecret, ivLabel, 12)
	hp = hkdfExpandLabel(clientSecret, hpLabel, 16)
	return key, iv, hp, nil
}
