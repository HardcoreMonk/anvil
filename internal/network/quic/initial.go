package quic

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
)

// QUIC long-header 첫 바이트의 비트 정의(RFC 9000 §17.2).
//
// Initial 패킷은 "long header"를 쓴다. 첫 바이트 하나에 여러 정보가 비트로
// 눌려 있는데, 그중 일부는 평문(header protection이 안 걸림)이고 일부는
// 보호되어 있다(복호 전엔 못 읽음). 아래 상수들은 그 비트 마스크·오프셋이다.
const (
	longHeaderBit  = 0x80 // Header Form 비트: 1이면 long header.
	fixedBit       = 0x40 // Fixed Bit: 이 버전들에선 반드시 1(아니면 QUIC 아님).
	longTypeMask   = 0x30 // Long Packet Type이 있는 비트(5~4). Initial/Handshake 등 구분.
	longTypeShift  = 4    // longTypeMask를 오른쪽으로 이만큼 밀면 타입 값이 된다.
	pnLenMask      = 0x03 // (보호 해제된) 첫 바이트 하위 2비트 = packet number 길이-1.
	hpLongMask     = 0x0f // long header에선 header protection이 첫 바이트 하위 4비트를 가린다.
	maxConnIDLen   = 20   // RFC 9000 §17.2: v1/v2에서 Connection ID는 최대 20바이트.
	hpSampleLen    = 16   // AES 기반 header protection은 16바이트 샘플을 쓴다.
	hpSampleOffset = 4    // 샘플은 packet-number 오프셋에서 4바이트 뒤부터 시작.
	aeadTagLen     = 16   // AES-128-GCM 인증 태그 길이(16바이트).
	versionV1      = 0x00000001
	versionV2      = 0x6b3343cf
	v1InitialType  = 0x0 // RFC 9000 §17.2: Initial = 0b00.
	v2InitialType  = 0x1 // RFC 9369 §3.2: Initial = 0b01(v2는 타입 인코딩이 다르다).
)

// readVarint는 QUIC 가변 길이 정수(varint, RFC 9000 §16)를 디코딩한다.
// QUIC은 길이·오프셋 등 많은 필드를 varint로 싣는데, 첫 바이트의 상위 2비트가
// 전체 길이를 정한다: 00→1바이트, 01→2, 10→4, 11→8바이트. 값 자체는 상위
// 2비트를 뺀 나머지 비트들을 big-endian으로 이어 붙인 것이다.
//
// 바이트가 모자라면(빈 슬라이스거나 length보다 짧으면) ok=false를 돌려주므로,
// 잘린(적대적) 패킷에도 절대 panic하지 않는다 — 호출자는 fail-closed 처리한다.
func readVarint(b []byte) (val uint64, n int, ok bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	prefix := b[0] >> 6   // 상위 2비트 = 길이 지수.
	length := 1 << prefix // 1<<{0,1,2,3} = {1,2,4,8}바이트.
	if len(b) < length {
		return 0, 0, false // 선언된 길이만큼 실제 바이트가 없음 → 잘린 패킷.
	}
	val = uint64(b[0] & 0x3f) // 첫 바이트에서 길이 비트(상위 2)를 제거.
	for i := 1; i < length; i++ {
		val = val<<8 | uint64(b[i]) // 나머지 바이트를 big-endian으로 이어붙임.
	}
	return val, length, true
}

// initialTypeForVersion은 주어진 QUIC 버전에서 "Initial 패킷"을 뜻하는 Long
// Packet Type 값과, 그 버전을 우리가 지원하는지를 돌려준다. v1과 v2는 Initial을
// 나타내는 타입 비트 값이 서로 다르다(v1=0b00, v2=0b01)는 점에 주의.
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

// decryptInitial은 datagram 안의 첫 QUIC Initial 패킷을 파싱해 header protection을
// 벗기고 AEAD로 복호한 뒤, 그 안의 QUIC 프레임 페이로드(복호된 평문)를 돌려준다
// (RFC 9001 §5.2–5.4, RFC 9000 §17.2). 이 평문을 상위 계층이 다시 CRYPTO
// 프레임으로 재조립하면 TLS ClientHello가 나오고, 거기서 SNI를 읽는다.
//
// 하나의 UDP datagram은 여러 QUIC 패킷을 이어 담을(coalesce) 수 있다. 여기서는
// 첫 패킷만 보고, 그 패킷의 Length 필드로 경계를 잡는다(뒤에 붙은 패킷은 무시).
//
// 전 구간 fail-closed: 헤더가 이상하거나·모르는 버전이거나·Initial이 아니거나·
// AEAD 검증이 실패하면 에러를 돌린다(복호 실패=deny). 모든 슬라이스 접근은
// 경계 검사를 하므로 잘린/적대적 datagram에도 panic하지 않으며, 입력 datagram을
// 절대 변형하지 않는다(읽기 전용).
func decryptInitial(datagram []byte) ([]byte, error) {
	// --- Long header 파싱 (RFC 9000 §17.2) ---
	// DCID 길이 바이트를 읽으려면 최소 첫 바이트(1) + 버전(4) = 5바이트가 있어야 한다.
	if len(datagram) < 5 {
		return nil, fmt.Errorf("quic: datagram too short for long header (%d bytes)", len(datagram))
	}
	first := datagram[0]
	if first&longHeaderBit == 0 {
		return nil, fmt.Errorf("quic: not a long-header packet") // short header = 핸드셰이크 이후 패킷 → Initial 아님.
	}
	if first&fixedBit == 0 {
		return nil, fmt.Errorf("quic: fixed bit not set") // Fixed Bit가 0이면 유효한 QUIC 패킷이 아님.
	}
	version := binary.BigEndian.Uint32(datagram[1:5])
	initialType, supported := initialTypeForVersion(version)
	if !supported {
		return nil, fmt.Errorf("quic: unsupported version 0x%08x", version)
	}
	// Long Packet Type 비트(0x30)는 header protection 대상이 아니라, 보호가
	// 걸린 첫 바이트에서도 그대로 읽을 수 있다. 이 값이 Initial이 아니면 거절.
	if (first&longTypeMask)>>longTypeShift != initialType {
		return nil, fmt.Errorf("quic: not an Initial packet (type bits 0x%02x)", first&longTypeMask)
	}

	// off는 "지금까지 소비한 바이트 수" = 다음에 읽을 위치. 헤더 필드를 순서대로
	// 건너뛰며 packet number가 시작되는 위치(pnOffset)까지 전진한다.
	off := 5
	// Destination Connection ID: 길이 1바이트 + 그만큼의 ID. 이 DCID가 곧
	// 키 유도(deriveClientInitialKeys)의 입력이다.
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

	// Source Connection ID: 길이 1바이트 + 그만큼의 ID. 키 유도에는 안 쓰지만
	// 그 뒤 필드로 넘어가려면 길이만큼 정확히 건너뛰어야 한다.
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

	// Token: Initial 패킷만 가지는 필드. varint 길이 + 그만큼의 토큰 바이트.
	// 값 자체는 안 쓰고 건너뛰기만 한다(0-RTT/재시도용).
	tokenLen, n, ok := readVarint(datagram[off:])
	if !ok {
		return nil, fmt.Errorf("quic: malformed token length")
	}
	off += n
	if uint64(off)+tokenLen > uint64(len(datagram)) {
		return nil, fmt.Errorf("quic: truncated token")
	}
	off += int(tokenLen)

	// Length(varint): 이 패킷에서 "packet number + 보호된 페이로드 + 태그"가
	// 차지하는 바이트 수. coalesce된 datagram에서 이 패킷의 끝 경계가 된다.
	// 이 varint를 읽고 난 지점이 바로 packet number의 시작(pnOffset).
	length, n, ok := readVarint(datagram[off:])
	if !ok {
		return nil, fmt.Errorf("quic: malformed length field")
	}
	off += n
	pnOffset := off
	if uint64(pnOffset)+length > uint64(len(datagram)) {
		return nil, fmt.Errorf("quic: length %d exceeds datagram", length)
	}

	// --- 클라이언트 Initial 키 유도 (hkdf.go) ---
	// 위에서 읽은 공개 DCID + 버전으로 key/iv/hp를 만든다. 여기부터가 실제 복호.
	key, iv, hp, err := deriveClientInitialKeys(dcid, version)
	if err != nil {
		return nil, err
	}

	// --- Header protection 해제 (RFC 9001 §5.4.1) ---
	// 첫 바이트의 하위 비트와 packet number는 "header protection"이라는 별도
	// 마스크로 가려져 있다. 마스크는 페이로드 암호문의 16바이트 "샘플"을 hp 키로
	// AES-ECB 1블록 암호화해서 얻는다. 샘플은 packet-number 오프셋에서 4바이트
	// 뒤부터 잡는다 — 이는 "packet number가 최대 길이(4바이트)"라고 가정하고 항상
	// 같은 위치를 쓰기로 한 스펙 규칙이다(실제 pn이 더 짧아도 이 오프셋 고정).
	if pnOffset+hpSampleOffset+hpSampleLen > len(datagram) {
		return nil, fmt.Errorf("quic: datagram too short for header-protection sample")
	}
	sample := datagram[pnOffset+hpSampleOffset : pnOffset+hpSampleOffset+hpSampleLen]
	hpBlock, err := aes.NewCipher(hp)
	if err != nil {
		return nil, fmt.Errorf("quic: hp cipher: %w", err)
	}
	mask := make([]byte, hpSampleLen)
	hpBlock.Encrypt(mask, sample) // AES-ECB 단일 블록 암호화 = header protection 마스크.

	// 마스크의 첫 바이트로 첫 바이트의 하위 4비트를 XOR해 벗긴다. 그러면 감춰져
	// 있던 packet number 길이(하위 2비트 + 1 = 1~4바이트)가 드러난다.
	unprotectedFirst := first ^ (mask[0] & hpLongMask)
	pnLen := int(unprotectedFirst&pnLenMask) + 1
	// Length 필드는 최소한 "packet number + AEAD 태그"를 담을 만큼은 되어야 한다.
	if uint64(pnLen+aeadTagLen) > length {
		return nil, fmt.Errorf("quic: length %d too small for pn(%d)+tag", length, pnLen)
	}

	// packet number 바이트들도 마스크(mask[1..])로 벗기고, 정수 값으로 복원한다.
	// 이 pn은 아래 AEAD nonce 계산에 쓰인다.
	pnBytes := make([]byte, pnLen)
	var pn uint64
	for i := 0; i < pnLen; i++ {
		pnBytes[i] = datagram[pnOffset+i] ^ mask[1+i]
		pn = pn<<8 | uint64(pnBytes[i])
	}

	// --- AEAD nonce = iv XOR (왼쪽으로 0-패딩한 packet number) (RFC 9001 §5.3) ---
	// nonce는 iv를 복사한 뒤, 오른쪽 끝(하위 바이트)부터 pn을 XOR해 만든다.
	// 즉 pn을 iv 길이에 맞춰 왼쪽 0-패딩한 값과 iv를 XOR하는 것과 같다.
	nonce := make([]byte, len(iv))
	copy(nonce, iv)
	for i := 0; i < 8; i++ {
		nonce[len(nonce)-1-i] ^= byte(pn >> (8 * i))
	}

	// --- Associated data(AAD) = 보호를 벗긴 상태의 헤더 (RFC 9001 §5.3) ---
	// GCM은 "복호"하는 페이로드 외에 "인증만" 하는 AAD를 함께 검증한다. QUIC의
	// AAD는 packet number 끝까지의 헤더인데, 단 header protection을 벗긴 뒤의
	// 값(unprotectedFirst, 벗긴 pnBytes)이어야 한다. 입력 datagram을 건드리지
	// 않으려고 사본에 담아 그 두 곳만 덮어쓴다.
	aad := make([]byte, pnOffset+pnLen)
	copy(aad, datagram[:pnOffset+pnLen])
	aad[0] = unprotectedFirst
	copy(aad[pnOffset:], pnBytes)

	// --- AES-128-GCM 복호(Open) (RFC 9001 §5.3) ---
	// 암호문 = "보호된 페이로드 + 16바이트 태그". 끝 경계는 Length 필드로 자른다
	// (coalesce된 datagram이면 뒤 패킷을 포함하지 않도록). Open은 태그를 검증해
	// 조금이라도 변조됐으면 에러를 낸다 → 그 자체가 fail-closed 게이트다.
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
