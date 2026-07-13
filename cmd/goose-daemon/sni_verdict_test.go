package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ephemera/internal/anvilmcp"
	"ephemera/internal/network/sni"
)

// encodeClientHelloSNI is a SNI-only reduction of Task 2's parser_test.go
// buildClientHello. It emits one wire-valid TLS record carrying a ClientHello
// whose single extension is a server_name for name. Kept local to this _test.go
// so the verdict tests do not depend on exporting the sni package's test helper
// (low coupling: the byte layout is copied, and mustHello self-checks it against
// the real exported parser as an oracle).
func encodeClientHelloSNI(name string) []byte {
	host := []byte(name)

	sn := &bytes.Buffer{}
	sn.WriteByte(0x00) // name_type = host_name
	binary.Write(sn, binary.BigEndian, uint16(len(host)))
	sn.Write(host)

	list := &bytes.Buffer{}
	binary.Write(list, binary.BigEndian, uint16(sn.Len()))
	list.Write(sn.Bytes())

	ext := &bytes.Buffer{}
	binary.Write(ext, binary.BigEndian, uint16(0x0000)) // ext_type server_name
	binary.Write(ext, binary.BigEndian, uint16(list.Len()))
	ext.Write(list.Bytes())

	body := &bytes.Buffer{}
	body.Write([]byte{0x03, 0x03})             // client_version TLS1.2
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

	rec := &bytes.Buffer{}
	rec.WriteByte(0x16) // content_type handshake
	rec.Write([]byte{0x03, 0x01})
	binary.Write(rec, binary.BigEndian, uint16(hs.Len()))
	rec.Write(hs.Bytes())
	return rec.Bytes()
}

func mustHello(t *testing.T, name string) []byte {
	t.Helper()
	// Reuse the parser's own acceptance as an oracle: build via a tiny inline
	// encoder mirroring Task 2's buildClientHello (kept local to avoid exporting
	// test helpers). Implementation copies the byte layout from parser_test.go.
	b := encodeClientHelloSNI(name) // small local encoder in this _test.go file
	if got, err := sni.ParseClientHelloSNI(b); err != nil || got != name {
		t.Fatalf("oracle: built hello for %q parsed as %q err=%v", name, got, err)
	}
	return b
}

func TestSNIDecideRouting(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com", "*.example.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Profile: "p", Matcher: m})

	// allowed exact
	if d := l.decide("10.0.1.10", mustHello(t, "api.anthropic.com")); d.Action != sniAcceptMark {
		t.Fatalf("allowed exact -> %v (%s)", d.Action, d.Reason)
	}
	// allowed wildcard
	if d := l.decide("10.0.1.10", mustHello(t, "cdn.example.com")); d.Action != sniAcceptMark {
		t.Fatalf("allowed wildcard -> %v", d.Action)
	}
	// denied
	d := l.decide("10.0.1.10", mustHello(t, "evil.test"))
	if d.Action != sniDrop || d.Reason != "egress_sni_denied" || d.SNI != "evil.test" {
		t.Fatalf("denied -> action=%v reason=%q sni=%q", d.Action, d.Reason, d.SNI)
	}
	// handshake / no application payload -> passthrough
	if d := l.decide("10.0.1.10", []byte{}); d.Action != sniPassthrough {
		t.Fatalf("empty payload -> %v, want passthrough", d.Action)
	}
	// unregistered source -> fail-closed drop
	if d := l.decide("10.0.1.99", mustHello(t, "api.anthropic.com")); d.Action != sniDrop {
		t.Fatalf("unregistered -> %v, want drop", d.Action)
	}
	// malformed application payload -> fail-closed drop
	if d := l.decide("10.0.1.10", []byte{0x16, 0x03, 0x01, 0xff, 0xff, 0x01}); d.Action != sniDrop {
		t.Fatalf("malformed -> %v, want drop", d.Action)
	}
}

func TestSNIDeregisterFailsClosed(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil)
	m, _ := sni.NewMatcher([]string{"api.anthropic.com"})
	l.Register("10.0.1.10", sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Matcher: m})
	l.Deregister("10.0.1.10")
	if d := l.decide("10.0.1.10", mustHello(t, "api.anthropic.com")); d.Action != sniDrop {
		t.Fatal("deregistered source must fail closed")
	}
}

// --- Task 6: recordVerdict / auditDeny (audit + metric emit) ---
//
// applyVerdict itself needs a live *nfqueue.Nfqueue (root + netfilter, see
// Start's doc comment) so it is exercised only by the Task 7 KVM e2e.
// recordVerdict/auditDeny factor the Task 6 audit/metric side effects out of
// that nf-dependent path, so they are unit-testable here in isolation.

func TestSNIRecordVerdictAuditsDenyWithTenant(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, nil)
	entry := sniRegistryEntry{VMID: "vm-1", TenantID: "t1", Profile: "p"}

	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test"})

	recs, err := anvilmcp.ReadRuntimeAudit(auditPath)
	if err != nil || len(recs) != 1 {
		t.Fatalf("read err=%v n=%d", err, len(recs))
	}
	rec := recs[0]
	if rec.SNI != "evil.test" || rec.TenantID != "t1" || rec.VMID != "vm-1" ||
		rec.DaemonOperation != "egress_sni_denied" || rec.ResultCode != "denied" ||
		rec.ToolName != "egress_sni_filter" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	// redaction: no field beyond {timestamp, tenant, vmid, tool, op, result,
	// sni} should ever be populated — no tokens/authorization/args/profile.
	if rec.SessionAlias != "" || rec.Error != "" {
		t.Fatalf("unexpected extra fields leaked into audit record: %+v", rec)
	}
}

func TestSNIRecordVerdictNoAuditWithoutTenant(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, nil)
	entry := sniRegistryEntry{VMID: "vm-2", Profile: "p"} // no TenantID

	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test"})

	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Fatalf("expected no audit file written without tenant (must degrade to slog), stat err=%v", err)
	}
}

func TestSNIRecordVerdictDegradesOnEmptyAuditPath(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil) // auditPath intentionally empty (pre-Task-6 daemon wiring)
	entry := sniRegistryEntry{VMID: "vm-3", TenantID: "t3"}

	// AppendRuntimeAudit rejects an empty path; must degrade to slog, not panic.
	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test"})
}

func TestSNIRecordVerdictUnparsedNoAudit(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, nil)
	entry := sniRegistryEntry{VMID: "vm-5", TenantID: "t5"}

	// No SNI was parsed, so there is nothing worth auditing — only the metric fires.
	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"})

	if _, err := os.Stat(auditPath); !os.IsNotExist(err) {
		t.Fatalf("expected no audit record for unparsed (no SNI) deny, stat err=%v", err)
	}
}

func TestSNIRecordVerdictNilMetricsSafe(t *testing.T) {
	l := newSNIVerdictLoop(88, "", nil) // metrics intentionally nil
	entry := sniRegistryEntry{VMID: "vm-4", TenantID: "t4"}

	// Must not panic on a nil *daemonMetrics receiver.
	l.recordVerdict(entry, sniDecision{Action: sniAcceptMark, SNI: "ok.test"})
	l.recordVerdict(entry, sniDecision{Action: sniDrop, Reason: "egress_sni_unparsed"})
}

func TestSNIRecordVerdictMetricAlwaysIncrements(t *testing.T) {
	cp := newMetricsTestCP(t)
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	l := newSNIVerdictLoop(88, auditPath, cp.metrics)

	// One deny with a tenant (audit succeeds) and one without (degrades to
	// slog) — the metric must increment identically in both cases.
	l.recordVerdict(sniRegistryEntry{VMID: "vm-a", TenantID: "ta"}, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil.test"})
	l.recordVerdict(sniRegistryEntry{VMID: "vm-b"}, sniDecision{Action: sniDrop, Reason: "egress_sni_denied", SNI: "evil2.test"})
	l.recordVerdict(sniRegistryEntry{VMID: "vm-c", TenantID: "tc"}, sniDecision{Action: sniAcceptMark, SNI: "ok.test"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	cp.handleMetrics(rec, req)
	body, _ := io.ReadAll(rec.Body)
	out := string(body)

	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{outcome="denied"} 2`) {
		t.Fatalf("expected denied=2 regardless of tenant/audit outcome, got:\n%s", out)
	}
	if !strings.Contains(out, `ephemera_egress_sni_verdict_total{outcome="allowed"} 1`) {
		t.Fatalf("expected allowed=1, got:\n%s", out)
	}
}
