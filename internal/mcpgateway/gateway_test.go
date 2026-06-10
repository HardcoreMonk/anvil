package mcpgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockMCP is a minimal in-process MCP server for tests. It speaks the subset the
// gateway uses (initialize / notifications/initialized / tools/list / tools/call)
// and records the Authorization header it last saw, for credential assertions.
type mockMCP struct {
	srv      *httptest.Server
	lastAuth string
	useSSE   bool // respond to tools/list with text/event-stream
}

func newMockMCP(t *testing.T, useSSE bool) *mockMCP {
	t.Helper()
	m := &mockMCP{useSSE: useSSE}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.lastAuth = r.Header.Get("Authorization")
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "mock-session-1")
			writeJSONResp(w, req.ID, map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "mock", "version": "1"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			result := map[string]any{"tools": []map[string]any{
				{"name": "echo", "description": "echoes input", "inputSchema": map[string]any{"type": "object"}},
				{"name": "add", "description": "adds numbers"},
			}}
			if m.useSSE {
				writeSSEResp(w, req.ID, result)
			} else {
				writeJSONResp(w, req.ID, result)
			}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			writeJSONResp(w, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "called " + p.Name + " with " + string(p.Arguments)}},
				"isError": false,
			})
		default:
			writeJSONResp(w, req.ID, map[string]any{})
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func writeJSONResp(w http.ResponseWriter, id json.RawMessage, result any) {
	raw, _ := json.Marshal(result)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: raw})
}

func writeSSEResp(w http.ResponseWriter, id json.RawMessage, result any) {
	raw, _ := json.Marshal(result)
	msg, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: raw})
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("event: message\ndata: " + string(msg) + "\n\n"))
}

// stubResolver returns a fixed Caller, ignoring the request.
type stubResolver struct{ c Caller }

func (s stubResolver) Resolve(*http.Request) (Caller, error) { return s.c, nil }

// errResolver always fails (an unknown caller).
type errResolver struct{}

func (errResolver) Resolve(*http.Request) (Caller, error) { return Caller{}, errUnknown }

var errUnknown = &rpcError{Message: "unknown"}

func newTestRegistry(t *testing.T, cfg ServerConfig, secrets map[string]string) *Registry {
	t.Helper()
	reg, err := NewRegistry([]ServerConfig{cfg}, secrets, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func doRPC(t *testing.T, g *Gateway, method string, params any) rpcResponse {
	t.Helper()
	pb, _ := json.Marshal(params)
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method, Params: pb})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	g.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d: %s", method, rr.Code, rr.Body.String())
	}
	var resp rpcResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: decode response: %v (%s)", method, err, rr.Body.String())
	}
	return resp
}

func TestHTTPBackend_ListAndCall(t *testing.T) {
	m := newMockMCP(t, false)
	b := NewHTTPBackend("mock", "mock", m.srv.URL, nil, NewMapCredentialProvider(map[string]string{"mock": "testtok"}))

	tools, err := b.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	// Credential injection: the backend must have sent the bearer token.
	if m.lastAuth != "Bearer testtok" {
		t.Fatalf("auth header = %q, want Bearer testtok", m.lastAuth)
	}

	res, err := b.CallTool(context.Background(), "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(string(res), "called echo with") {
		t.Fatalf("unexpected call result: %s", res)
	}
}

func TestHTTPBackend_SSE(t *testing.T) {
	m := newMockMCP(t, true) // tools/list responds via SSE
	b := NewHTTPBackend("mock", "mock", m.srv.URL, nil, nil)
	tools, err := b.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools over SSE: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("SSE: got %d tools, want 2", len(tools))
	}
}

func TestParseSSEResponse(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[]}}\n\n"
	resp, err := parseSSEResponse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseSSEResponse: %v", err)
	}
	if len(resp.Result) == 0 {
		t.Fatalf("expected a result, got %+v", resp)
	}
}

func TestSplitNamespaced(t *testing.T) {
	ns, tool, ok := splitNamespaced("github__create_issue")
	if !ok || ns != "github" || tool != "create_issue" {
		t.Fatalf("split = %q/%q/%v", ns, tool, ok)
	}
	// Tool names may themselves contain the separator; only the first splits.
	ns, tool, ok = splitNamespaced("fs__read__file")
	if !ok || ns != "fs" || tool != "read__file" {
		t.Fatalf("split = %q/%q/%v", ns, tool, ok)
	}
	if _, _, ok := splitNamespaced("noseparator"); ok {
		t.Fatal("expected ok=false for un-namespaced name")
	}
}

func TestStaticPolicyStore(t *testing.T) {
	servers := []ServerConfig{
		{ID: "open"}, // no profiles → all
		{ID: "leaderonly", Profiles: []string{"leader"}},
	}
	ps := NewStaticPolicyStore(servers)
	if !ps.For("worker").Allows("open") || ps.For("worker").Allows("leaderonly") {
		t.Fatal("worker policy wrong")
	}
	if !ps.For("leader").Allows("leaderonly") {
		t.Fatal("leader should see leaderonly")
	}
}

func TestGateway_EndToEnd(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, Credential: "k", Profiles: []string{"leader"}}, map[string]string{"k": "secret"})

	var audited []AuditRecord
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
		Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
	})

	// initialize
	init := doRPC(t, g, "initialize", initializeParams{ProtocolVersion: "2025-03-26"})
	if init.Error != nil {
		t.Fatalf("initialize error: %v", init.Error)
	}

	// tools/list → namespaced
	list := doRPC(t, g, "tools/list", map[string]any{})
	var lr toolsListResult
	_ = json.Unmarshal(list.Result, &lr)
	if len(lr.Tools) != 2 || lr.Tools[0].Name != "mock__add" {
		t.Fatalf("tools/list = %+v (want namespaced mock__*)", lr.Tools)
	}

	// tools/call → routed + audited
	call := doRPC(t, g, "tools/call", toolsCallParams{Name: "mock__echo", Arguments: json.RawMessage(`{"x":1}`)})
	if call.Error != nil {
		t.Fatalf("tools/call error: %v", call.Error)
	}
	if !strings.Contains(string(call.Result), "called echo") {
		t.Fatalf("unexpected call result: %s", call.Result)
	}
	if len(audited) != 1 || !audited[0].OK || audited[0].Server != "mock" || audited[0].Tool != "echo" {
		t.Fatalf("audit record wrong: %+v", audited)
	}
	// Backend received the injected credential, not the VM.
	if m.lastAuth != "Bearer secret" {
		t.Fatalf("backend auth = %q, want Bearer secret", m.lastAuth)
	}
}

func TestGateway_PolicyFiltersCatalog(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", URL: m.srv.URL, Profiles: []string{"leader"}}, nil)
	// Caller's profile "worker" is NOT in the server's allow-list.
	g := New(Options{Resolver: stubResolver{Caller{VMID: "vm2", Profile: "worker"}}, Registry: reg})

	list := doRPC(t, g, "tools/list", map[string]any{})
	var lr toolsListResult
	_ = json.Unmarshal(list.Result, &lr)
	if len(lr.Tools) != 0 {
		t.Fatalf("worker should see no tools, got %+v", lr.Tools)
	}
	// A direct call must be rejected by policy.
	call := doRPC(t, g, "tools/call", toolsCallParams{Name: "mock__echo"})
	if call.Error == nil {
		t.Fatal("expected policy rejection for worker")
	}
}

func TestGateway_UnknownCaller(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", URL: m.srv.URL}, nil)
	g := New(Options{Resolver: errResolver{}, Registry: reg})

	rr := httptest.NewRecorder()
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/list"})
	g.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unknown caller: HTTP %d, want 403", rr.Code)
	}
}

func TestGateway_UnknownNamespace(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", URL: m.srv.URL}, nil)
	g := New(Options{Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}}, Registry: reg})
	call := doRPC(t, g, "tools/call", toolsCallParams{Name: "ghost__do"})
	if call.Error == nil {
		t.Fatal("expected error for unknown namespace")
	}
}

func TestRegistry_Validation(t *testing.T) {
	// Duplicate namespace is rejected.
	_, err := NewRegistry([]ServerConfig{
		{ID: "a", Namespace: "x", URL: "http://a"},
		{ID: "b", Namespace: "x", URL: "http://b"},
	}, nil, nil)
	if err == nil {
		t.Fatal("expected duplicate-namespace error")
	}
	// Namespace containing the separator is rejected.
	if _, err := NewRegistry([]ServerConfig{{ID: "a", Namespace: "a__b", URL: "http://a"}}, nil, nil); err == nil {
		t.Fatal("expected separator-in-namespace error")
	}
	// Missing credential referenced by a server is rejected.
	if _, err := NewRegistry([]ServerConfig{{ID: "a", URL: "http://a", Credential: "nope"}}, nil, nil); err == nil {
		t.Fatal("expected missing-credential error")
	}
	// Non-http URL is rejected.
	if _, err := NewRegistry([]ServerConfig{{ID: "a", URL: "ftp://a"}}, nil, nil); err == nil {
		t.Fatal("expected non-http url error")
	}
}
