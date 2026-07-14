package quic

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// RFC 9001 Appendix A.1: DCID 0x8394c8f03e515708 로 파생한 client Initial 키.
func TestDeriveClientInitialKeys_RFC9001_V1(t *testing.T) {
	dcid := mustHex("8394c8f03e515708")
	key, iv, hp, err := deriveClientInitialKeys(dcid, 0x00000001)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	// RFC 9001 A.1 공표값:
	wantKey := mustHex("1f369613dd76d5467730efcbe3b1a22d")
	wantIV := mustHex("fa044b2f42a3fd3b46fb255c")
	wantHP := mustHex("9f50449e04a0e810283a1e9933adedd2")
	if !bytes.Equal(key, wantKey) || !bytes.Equal(iv, wantIV) || !bytes.Equal(hp, wantHP) {
		t.Fatalf("keys mismatch\n key=%x\n iv=%x\n hp=%x", key, iv, hp)
	}
}

func TestDeriveClientInitialKeys_UnsupportedVersion(t *testing.T) {
	if _, _, _, err := deriveClientInitialKeys(mustHex("8394c8f03e515708"), 0xdeadbeef); err == nil {
		t.Fatal("unsupported version must error (fail-closed)")
	}
}
