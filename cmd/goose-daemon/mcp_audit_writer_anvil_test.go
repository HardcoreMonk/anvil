package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ephemera/internal/mcpgateway"
)

// auditKeySet is the exact set of keys appendMCPAudit is allowed to write to
// audit/mcp.jsonl. The AuditRecord struct is separately locked by a reflect
// guard, but the WRITER's map is hand-built — this set locks the writer so that
// adding e.g. `"err": rec.Err` (which can carry backend-controlled content:
// stderr, argument/result echoes) fails the test.
var auditKeySet = []string{"ts", "vm", "profile", "server", "tool", "kind", "outcome", "ms"}

// readAuditLines returns the JSONL lines written to {workDir}/audit/mcp.jsonl.
func readAuditLines(t *testing.T, cp *ControlPlane) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cp.workDir, "audit", "mcp.jsonl"))
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// assertAuditLine parses one JSONL audit line, asserts its key set is exactly
// auditKeySet, and that none of the forbidden sentinels appear anywhere in it.
func assertAuditLine(t *testing.T, line string, forbidden ...string) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatalf("audit line is not JSON: %v (%s)", err, line)
	}
	want := map[string]bool{}
	for _, k := range auditKeySet {
		want[k] = true
		if _, ok := obj[k]; !ok {
			t.Errorf("audit line missing required key %q: %s", k, line)
		}
	}
	for k := range obj {
		if !want[k] {
			t.Errorf("audit line has unexpected key %q (writer key set must be exactly %v): %s", k, auditKeySet, line)
		}
	}
	for _, s := range forbidden {
		if strings.Contains(line, s) {
			t.Errorf("audit line leaked sentinel %q: %s", s, line)
		}
	}
}

// TestAppendMCPAuditWriterOmitsErrSentinel drives the real observe→write path
// with an AuditRecord whose Err field carries a sentinel, and locks the written
// JSONL key set to metadata only (no err field). Regression guard for #25.
func TestAppendMCPAuditWriterOmitsErrSentinel(t *testing.T) {
	const errSentinel = "ERR-SENTINEL-DO-NOT-WRITE"
	cp := newTestCP(t)

	cp.observeMCPCall(mcpgateway.AuditRecord{
		VMID:       "vm-1",
		Profile:    "leader",
		Server:     "deepwiki",
		Kind:       "tool",
		Tool:       "ask_question",
		OK:         false,
		DurationMs: 42,
		Err:        errSentinel,
	})

	lines := readAuditLines(t, cp)
	if len(lines) != 1 {
		t.Fatalf("expected 1 audit line, got %d: %v", len(lines), lines)
	}
	assertAuditLine(t, lines[0], errSentinel)
}

// TestMCPAuditWriterOmitsBackendSentinelsEndToEnd exercises the whole gateway
// path against a real (httptest) mock backend: the tool call carries a sentinel
// in its arguments and the backend replies with an error message carrying a
// second sentinel. The gateway builds the AuditRecord (Err = backend message)
// and hands it to cp.observeMCPCall — the written line must contain neither
// sentinel and exactly the metadata key set.
func TestMCPAuditWriterOmitsBackendSentinelsEndToEnd(t *testing.T) {
	const argSentinel = "ARG-SENTINEL-DO-NOT-LEAK"
	const errSentinel = "ERRMSG-SENTINEL-DO-NOT-LEAK"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2025-03-26",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "mock", "version": "1"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
		case "tools/call":
			// Reply with a JSON-RPC error whose message carries a sentinel; the
			// gateway folds resp.Error.Message into rec.Err.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32000, "message": "backend blew up: " + errSentinel},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer srv.Close()

	cp := newTestCP(t)
	reg, err := mcpgateway.NewRegistry([]mcpgateway.ServerConfig{{ID: "mock", URL: srv.URL}}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	gw := mcpgateway.New(mcpgateway.Options{
		Registry: reg,
		Observe:  cp.observeMCPCall,
		Resolver: mcpgateway.NewIPCallerResolver(cp.lookupVMByIP),
	})
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{VMID: "vm-1", Profile: "leader", GuestIP: "10.9.9.9"}}

	callBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"mock__danger","arguments":{"secret_arg":"` + argSentinel + `"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(callBody))
	req.RemoteAddr = "10.9.9.9:5555"
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("gateway tools/call HTTP status = %d, want 200 (JSON-RPC error is in the body)", rr.Code)
	}

	lines := readAuditLines(t, cp)
	if len(lines) != 1 {
		t.Fatalf("expected 1 audit line, got %d: %v", len(lines), lines)
	}
	assertAuditLine(t, lines[0], argSentinel, errSentinel)
}
