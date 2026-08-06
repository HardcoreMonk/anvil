package mcpgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		case "resources/list":
			writeJSONResp(w, req.ID, map[string]any{"resources": []map[string]any{
				{"uri": "file:///a.txt", "name": "A", "mimeType": "text/plain"},
				{"uri": "file:///b.txt", "name": "B"},
			}})
		case "resources/read":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			writeJSONResp(w, req.ID, map[string]any{"contents": []map[string]any{
				{"uri": p.URI, "mimeType": "text/plain", "text": "read " + p.URI},
			}})
		case "prompts/list":
			writeJSONResp(w, req.ID, map[string]any{"prompts": []map[string]any{
				{"name": "greet", "description": "a greeting"},
				{"name": "farewell"},
			}})
		case "prompts/get":
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &p)
			writeJSONResp(w, req.ID, map[string]any{"messages": []map[string]any{
				{"role": "user", "content": map[string]any{"type": "text", "text": "got " + p.Name}},
			}})
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

// TestHTTPBackend_InitializeError_ScrubsBackendDetail verifies #36 for the HTTP
// transport: an initialize response carrying a sentinel-bearing protocol error
// must yield a generic backend error (no sentinel) while the full detail reaches
// the host log for operators.
func TestHTTPBackend_InitializeError_ScrubsBackendDetail(t *testing.T) {
	logs := captureSlog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32001, Message: "SENTINEL_INIT_BOOM proprietary handshake refusal"}})
			return
		}
		writeJSONResp(w, req.ID, map[string]any{})
	}))
	t.Cleanup(srv.Close)

	b := NewHTTPBackend("mock", "mock", srv.URL, nil, nil)
	_, err := b.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected an error when initialize fails")
	}
	if strings.Contains(err.Error(), "SENTINEL_INIT_BOOM") {
		t.Fatalf("backend initialize error leaked backend detail: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "mock") {
		t.Fatalf("generic error should still name the backend, got: %q", err.Error())
	}
	waitForLog(t, logs, "SENTINEL_INIT_BOOM")
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
	calls := auditOfKind(audited, "tool")
	if len(calls) != 1 || !calls[0].OK || calls[0].Server != "mock" || calls[0].Tool != "echo" {
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

func TestGateway_RateLimited(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, Profiles: []string{"leader"}}, nil)

	// 1 call/min, burst 1, with a frozen clock so the second call gets no refill.
	lim := NewTokenBucketLimiter(1, 1)
	clock := time.Unix(1000, 0)
	lim.now = func() time.Time { return clock }

	var audited []AuditRecord
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
		Limiter:  lim,
		Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
	})

	// First call consumes the only token.
	first := doRPC(t, g, "tools/call", toolsCallParams{Name: "mock__echo", Arguments: json.RawMessage(`{}`)})
	if first.Error != nil {
		t.Fatalf("first call should succeed, got error: %v", first.Error)
	}
	// Second call (clock frozen → no refill) is rate-limited.
	second := doRPC(t, g, "tools/call", toolsCallParams{Name: "mock__echo", Arguments: json.RawMessage(`{}`)})
	if second.Error == nil || second.Error.Code != codeInternalError {
		t.Fatalf("second call should be rate-limited with codeInternalError, got: %+v", second.Error)
	}
	// The rate-limited call is audited as a failure with the sentinel error.
	last := audited[len(audited)-1]
	if last.OK || last.Err != "rate limited" || last.Server != "mock" {
		t.Fatalf("rate-limited audit record wrong: %+v", last)
	}
}

func TestTokenBucketLimiter_Refill(t *testing.T) {
	lim := NewTokenBucketLimiter(60, 2) // 60/min = 1/s, burst 2
	clock := time.Unix(0, 0)
	lim.now = func() time.Time { return clock }

	// The burst of 2 is admitted immediately; the 3rd is denied.
	if !lim.Allow("vm", "srv") || !lim.Allow("vm", "srv") {
		t.Fatal("first two calls within burst should be allowed")
	}
	if lim.Allow("vm", "srv") {
		t.Fatal("third call should be denied (bucket empty)")
	}

	// After 1 second, 60/min refills exactly 1 token.
	clock = clock.Add(time.Second)
	if !lim.Allow("vm", "srv") {
		t.Fatal("one token should have refilled after 1s")
	}
	if lim.Allow("vm", "srv") {
		t.Fatal("only one token refilled; second should be denied")
	}

	// A multi-second idle refills past burst but caps at burst (2).
	clock = clock.Add(5 * time.Second)
	if !lim.Allow("vm", "srv") || !lim.Allow("vm", "srv") {
		t.Fatal("burst of 2 should be available after refill")
	}
	if lim.Allow("vm", "srv") {
		t.Fatal("bucket should cap at burst=2, not accumulate")
	}

	// A different (vm, server) key has its own independent bucket.
	if !lim.Allow("vm2", "srv") {
		t.Fatal("distinct key should have a fresh bucket")
	}
}

func TestGateway_ResourcesAggregation(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, Profiles: []string{"leader"}}, nil)
	var audited []AuditRecord
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
		Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
	})

	// resources/list → namespaced URIs, sorted.
	list := doRPC(t, g, "resources/list", map[string]any{})
	var lr resourcesListResult
	_ = json.Unmarshal(list.Result, &lr)
	if len(lr.Resources) != 2 || lr.Resources[0].URI != "mock__file:///a.txt" {
		t.Fatalf("resources/list = %+v (want namespaced mock__*)", lr.Resources)
	}

	// resources/read → routed by namespace, relayed, audited as a resource.
	read := doRPC(t, g, "resources/read", resourcesReadParams{URI: "mock__file:///a.txt"})
	if read.Error != nil {
		t.Fatalf("resources/read error: %v", read.Error)
	}
	if !strings.Contains(string(read.Result), "read file:///a.txt") {
		t.Fatalf("unexpected read result: %s", read.Result)
	}
	reads := auditOfKind(audited, "resource")
	if len(reads) != 1 || !reads[0].OK || reads[0].Tool != "file:///a.txt" {
		t.Fatalf("resource audit record wrong: %+v", audited)
	}

	// A profile with no access sees nothing and is rejected on read.
	gw := New(Options{Resolver: stubResolver{Caller{VMID: "vm2", Profile: "worker"}}, Registry: reg})
	denied := doRPC(t, gw, "resources/read", resourcesReadParams{URI: "mock__file:///a.txt"})
	if denied.Error == nil {
		t.Fatal("expected policy rejection for worker")
	}
}

func TestGateway_PromptsAggregation(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, Profiles: []string{"leader"}}, nil)
	var audited []AuditRecord
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
		Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
	})

	// prompts/list → namespaced names, sorted (farewell < greet).
	list := doRPC(t, g, "prompts/list", map[string]any{})
	var lr promptsListResult
	_ = json.Unmarshal(list.Result, &lr)
	if len(lr.Prompts) != 2 || lr.Prompts[0].Name != "mock__farewell" {
		t.Fatalf("prompts/list = %+v (want namespaced, sorted)", lr.Prompts)
	}

	// prompts/get → routed, relayed, audited as a prompt.
	get := doRPC(t, g, "prompts/get", promptsGetParams{Name: "mock__greet"})
	if get.Error != nil {
		t.Fatalf("prompts/get error: %v", get.Error)
	}
	if !strings.Contains(string(get.Result), "got greet") {
		t.Fatalf("unexpected get result: %s", get.Result)
	}
	gets := auditOfKind(audited, "prompt")
	if len(gets) != 1 || !gets[0].OK || gets[0].Tool != "greet" {
		t.Fatalf("prompt audit record wrong: %+v", audited)
	}
}

func TestPolicy_AllowsTool(t *testing.T) {
	p := Policy{
		Servers: map[string]bool{"a": true, "b": true},
		Tools: map[string]ToolPolicy{
			"a": {Allow: []string{"x", "y"}}, // allow-list
			"b": {Deny: []string{"z"}},       // deny-list
		},
	}
	// allow-list: only x,y permitted on a.
	if !p.AllowsTool("a", "x") || p.AllowsTool("a", "z") {
		t.Fatal("allow-list wrong")
	}
	// deny-list: everything but z permitted on b.
	if !p.AllowsTool("b", "w") || p.AllowsTool("b", "z") {
		t.Fatal("deny-list wrong")
	}
	// a server the profile cannot use is always denied.
	if p.AllowsTool("c", "x") {
		t.Fatal("unlisted server should deny")
	}
	// an allowed server with no per-tool policy permits every tool.
	p2 := Policy{Servers: map[string]bool{"a": true}}
	if !p2.AllowsTool("a", "anything") {
		t.Fatal("no per-tool policy should allow all")
	}
}

func TestStaticPolicyStore_PerTool(t *testing.T) {
	servers := []ServerConfig{
		{ID: "gh", Profiles: []string{"leader"}, ToolsAllow: []string{"create_issue"}},
		{ID: "fs", ToolsDeny: []string{"delete"}},
	}
	pol := NewStaticPolicyStore(servers).For("leader")
	if !pol.AllowsTool("gh", "create_issue") || pol.AllowsTool("gh", "delete_repo") {
		t.Fatal("gh per-tool allow-list wrong")
	}
	if !pol.AllowsTool("fs", "read") || pol.AllowsTool("fs", "delete") {
		t.Fatal("fs per-tool deny-list wrong")
	}
}

func TestStaticPolicyStore_BindingIntersection(t *testing.T) {
	servers := []ServerConfig{{ID: "a"}, {ID: "b"}, {ID: "c"}} // all profiles
	binding := map[string][]string{"narrow": {"a", "b"}, "empty": {}}
	ps := NewStaticPolicyStoreWithBinding(servers, func(profile string) ([]string, bool) {
		v, ok := binding[profile]
		return v, ok
	})

	// Bound profile: servers.yaml (all) ∩ {a,b} → a,b only.
	pol := ps.For("narrow")
	if !pol.Allows("a") || !pol.Allows("b") || pol.Allows("c") {
		t.Fatalf("narrow binding wrong: %+v", pol.Servers)
	}
	// Explicit empty binding → no servers.
	if len(ps.For("empty").Servers) != 0 {
		t.Fatal("empty binding should allow nothing")
	}
	// Unbound profile → servers.yaml as-is (all three).
	if len(ps.For("other").Servers) != 3 {
		t.Fatal("unbound profile should see all servers")
	}

	// A binding must not widen beyond servers.yaml: profile "y" is bound to a+b,
	// but servers.yaml only grants "a" to profile "x".
	servers2 := []ServerConfig{{ID: "a", Profiles: []string{"x"}}, {ID: "b"}}
	ps2 := NewStaticPolicyStoreWithBinding(servers2, func(string) ([]string, bool) {
		return []string{"a", "b"}, true
	})
	poly := ps2.For("y")
	if poly.Allows("a") {
		t.Fatal("binding must not widen beyond servers.yaml")
	}
	if !poly.Allows("b") {
		t.Fatal("b (all-profiles + bound) should be allowed")
	}
}

func TestGateway_PerToolFilter(t *testing.T) {
	m := newMockMCP(t, false) // exposes echo, add
	reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, ToolsAllow: []string{"echo"}}, nil)
	g := New(Options{Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}}, Registry: reg})

	// tools/list is filtered to the allow-list: only echo, not add.
	list := doRPC(t, g, "tools/list", map[string]any{})
	var lr toolsListResult
	_ = json.Unmarshal(list.Result, &lr)
	if len(lr.Tools) != 1 || lr.Tools[0].Name != "mock__echo" {
		t.Fatalf("per-tool list = %+v (want only mock__echo)", lr.Tools)
	}
	// tools/call on the denied tool is rejected.
	if call := doRPC(t, g, "tools/call", toolsCallParams{Name: "mock__add"}); call.Error == nil {
		t.Fatal("expected add to be denied by tools_allow")
	}
	// tools/call on the allowed tool succeeds.
	if call := doRPC(t, g, "tools/call", toolsCallParams{Name: "mock__echo", Arguments: json.RawMessage(`{}`)}); call.Error != nil {
		t.Fatalf("echo should be allowed: %v", call.Error)
	}
}
