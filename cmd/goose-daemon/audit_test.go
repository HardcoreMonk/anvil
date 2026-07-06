package main

import (
	"bufio"
	"encoding/json"
	"errors"
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

func TestNewAuditLogger_CreatesPrivateAuditLog(t *testing.T) {
	t.Setenv("EPHEMERA_AUDIT_DISABLE", "false")
	a, err := newAuditLogger(t.TempDir())
	if err != nil {
		t.Fatalf("new audit logger: %v", err)
	}
	t.Cleanup(func() { a.Close() })

	dirInfo, err := os.Stat(a.dir)
	if err != nil {
		t.Fatalf("stat audit dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Fatalf("audit dir mode = %o, want 0700", got)
	}
	fileInfo, err := os.Stat(a.path)
	if err != nil {
		t.Fatalf("stat audit file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("audit file mode = %o, want 0600", got)
	}
}

func TestNewAuditLogger_TightensExistingLooseFile(t *testing.T) {
	t.Setenv("EPHEMERA_AUDIT_DISABLE", "false")
	workDir := t.TempDir()
	auditDir := filepath.Join(workDir, "audit")
	auditPath := filepath.Join(auditDir, "access.jsonl")
	if err := os.MkdirAll(auditDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditPath, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	a, err := newAuditLogger(workDir)
	if err != nil {
		t.Fatalf("new audit logger: %v", err)
	}
	t.Cleanup(func() { a.Close() })

	info, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("stat audit file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("existing audit file mode = %o, want 0600", got)
	}
}

func TestNewAuditLogger_RejectsSymlinkAuditFile(t *testing.T) {
	t.Setenv("EPHEMERA_AUDIT_DISABLE", "false")
	workDir := t.TempDir()
	auditDir := filepath.Join(workDir, "audit")
	if err := os.MkdirAll(auditDir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workDir, "target.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(auditDir, "access.jsonl")); err != nil {
		t.Fatal(err)
	}

	a, err := newAuditLogger(workDir)
	if a != nil {
		t.Cleanup(func() { a.Close() })
	}
	if err == nil {
		t.Fatal("new audit logger succeeded with symlink access.jsonl, want error")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want symlink rejection", err)
	}
}

func TestAuditLogger_RotateKeepsPrivateModes(t *testing.T) {
	t.Setenv("EPHEMERA_AUDIT_DISABLE", "false")
	a, err := newAuditLogger(t.TempDir())
	if err != nil {
		t.Fatalf("new audit logger: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	a.maxBytes = 1
	a.keep = 1

	a.Write(auditRecord{TS: "t", Client: "c", Method: "GET", Path: "/vms", Status: 200})

	for _, path := range []string{a.path, a.path + ".1"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("%s mode = %o, want 0600", filepath.Base(path), got)
		}
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

func TestAuditLogger_TailReturnsScannerError(t *testing.T) {
	a := newTestAuditLogger(t, 1<<30, 3)
	oversized := strings.Repeat("x", 1024*1024+1)
	if err := os.WriteFile(a.path, []byte(oversized), 0600); err != nil {
		t.Fatalf("write oversized line: %v", err)
	}

	if _, err := a.tail(10, auditFilter{}); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("tail err = %v, want %v", err, bufio.ErrTooLong)
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

func TestAuditMiddleware_AllowedFieldsOnlyAndNoSecretMaterial(t *testing.T) {
	a := newTestAuditLogger(t, 1<<30, 3)
	cp := &ControlPlane{audit: a}
	h := cp.auditMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent_token":"response-secret"}`))
	}))

	req := httptest.NewRequest(http.MethodPost, "/vms?agent_token=query-secret", strings.NewReader(`{"agent_token":"body-secret"}`))
	req.Header.Set("Authorization", "Bearer header-secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	records, err := a.tail(1, auditFilter{})
	if err != nil {
		t.Fatalf("tail audit: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	rec := records[0]
	if rec.Path != "/vms" {
		t.Fatalf("path = %q, want path without query string", rec.Path)
	}

	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("unmarshal record: %v", err)
	}
	allowed := map[string]bool{
		"ts": true, "client": true, "method": true, "path": true,
		"status": true, "duration_ms": true, "remote_addr": true, "bytes": true,
	}
	if len(fields) != len(allowed) {
		t.Fatalf("audit fields = %v, want only %v", fields, allowed)
	}
	for k := range fields {
		if !allowed[k] {
			t.Fatalf("unexpected audit field %q in %s", k, b)
		}
	}

	low := strings.ToLower(string(b))
	for _, forbidden := range []string{
		"authorization", "bearer", "agent_token",
		"header-secret", "query-secret", "body-secret", "response-secret",
	} {
		if strings.Contains(low, forbidden) {
			t.Fatalf("audit record leaked %q: %s", forbidden, b)
		}
	}
}
