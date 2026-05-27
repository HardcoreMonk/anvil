package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestAuditLogger(t *testing.T, maxBytes int64, keep int) *auditLogger {
	t.Helper()
	dir := t.TempDir()
	a := &auditLogger{
		dir:      dir,
		path:     filepath.Join(dir, "access.jsonl"),
		maxBytes: maxBytes,
		keep:     keep,
		enabled:  true,
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open audit file: %v", err)
	}
	a.file = f
	t.Cleanup(func() { a.Close() })
	return a
}

func TestStatusRecorder(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec}
	if _, err := sr.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if sr.status != http.StatusOK {
		t.Errorf("default status = %d, want 200", sr.status)
	}
	if sr.bytes != 5 {
		t.Errorf("bytes = %d, want 5", sr.bytes)
	}

	rec2 := httptest.NewRecorder()
	sr2 := &statusRecorder{ResponseWriter: rec2}
	sr2.WriteHeader(http.StatusNotFound)
	sr2.Write([]byte("x"))
	if sr2.status != http.StatusNotFound {
		t.Errorf("explicit status = %d, want 404", sr2.status)
	}
}

// flushRecorder is an http.ResponseWriter that also implements http.Flusher.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

// nonFlusher implements http.ResponseWriter but NOT http.Flusher.
type nonFlusher struct{}

func (nonFlusher) Header() http.Header         { return http.Header{} }
func (nonFlusher) Write(b []byte) (int, error) { return len(b), nil }
func (nonFlusher) WriteHeader(int)             {}

func TestStatusRecorder_FlushForwarding(t *testing.T) {
	fr := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	(&statusRecorder{ResponseWriter: fr}).Flush()
	if !fr.flushed {
		t.Error("Flush must forward to a Flusher underlying writer (SSE depends on this)")
	}
	// Must not panic when the underlying writer is not a Flusher.
	(&statusRecorder{ResponseWriter: nonFlusher{}}).Flush()
}

func TestAuditLogger_RotateAndKeep(t *testing.T) {
	a := newTestAuditLogger(t, 200, 2) // tiny cap forces rotation; keep 2
	for i := 0; i < 50; i++ {
		a.Write(auditRecord{TS: "t", Client: "c", Method: "GET", Path: "/vms", Status: 200})
	}
	if _, err := os.Stat(a.path + ".1"); err != nil {
		t.Errorf("access.jsonl.1 should exist after rotation: %v", err)
	}
	if _, err := os.Stat(a.path + ".3"); !os.IsNotExist(err) {
		t.Errorf("access.jsonl.3 must not exist (keep=2), got err=%v", err)
	}
}

func TestAuditLogger_TailFilters(t *testing.T) {
	a := newTestAuditLogger(t, 1<<30, 3) // large cap → no rotation
	a.Write(auditRecord{Client: "alice", Method: "GET", Path: "/vms", Status: 200})
	a.Write(auditRecord{Client: "bob", Method: "POST", Path: "/vms", Status: 201})
	a.Write(auditRecord{Client: "alice", Method: "DELETE", Path: "/vms/x", Status: 200})

	all, _ := a.tail(10, auditFilter{})
	if len(all) != 3 || all[0].Method != "DELETE" {
		t.Fatalf("tail newest-first: got %d entries, first=%+v", len(all), all)
	}
	if ac, _ := a.tail(10, auditFilter{client: "alice"}); len(ac) != 2 {
		t.Errorf("client filter: got %d, want 2", len(ac))
	}
	if s, _ := a.tail(10, auditFilter{status: 201}); len(s) != 1 || s[0].Client != "bob" {
		t.Errorf("status filter: got %+v", s)
	}
	if lim, _ := a.tail(1, auditFilter{}); len(lim) != 1 || lim[0].Method != "DELETE" {
		t.Errorf("limit=1 newest: got %+v", lim)
	}
}

func TestAuditRecord_NoSecretLeak(t *testing.T) {
	// The record type carries no token/header/body field. Guard against a future
	// field addition silently leaking auth material (HARD invariant).
	b, _ := json.Marshal(auditRecord{
		TS: "t", Client: "alice", Method: "GET", Path: "/vms", Status: 200, RemoteAddr: "10.0.0.1:5",
	})
	low := strings.ToLower(string(b))
	if strings.Contains(low, "bearer") || strings.Contains(low, "authorization") || strings.Contains(low, "token") {
		t.Errorf("audit record must not contain auth material: %s", b)
	}
}
