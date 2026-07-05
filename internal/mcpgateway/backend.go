package mcpgateway

// 학습 주석 개요: 이 파일은 v0.6.0 core 의 유일한 backend 구현인 HTTPBackend
// (Streamable HTTP transport)를 담는다. v0.6.4 에서 추가된 StdioBackend 는
// backend_stdio.go 에 별도로 있다 — 둘 다 Backend interface 를 구현해
// gateway.go 는 transport 종류를 몰라도 된다(registry.go 가 transport 필드로
// 둘 중 하나를 골라 만든다). HTTPBackend 는 initialize 핸드셰이크를 lazy 하게
// 한 번만 하고 Mcp-Session-Id 를 재사용하며, credential 은 CredentialProvider
// (secrets.go)가 매 요청 직전에 헤더로 주입한다 — HTTPBackend 자신은 credential
// 문자열을 필드로 들고 있지 않는다.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Backend is one upstream MCP server the gateway aggregates. The single-host
// build ships HTTPBackend (Streamable HTTP / SSE); a multi-host fork can add a
// RemoteNodeBackend (a server living on another node) behind the same interface.
// 학습 주석: HTTPBackend(이 파일)와 StdioBackend(backend_stdio.go)가 이 하나의
// interface 를 구현한다 — gateway.go 의 handleToolsCall 등은 이 interface 로만
// backend 를 다루므로 transport 종류에 무관하게 동일한 요청 흐름을 유지한다.
type Backend interface {
	ID() string
	Namespace() string
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error)
	ListResources(ctx context.Context) ([]Resource, error)
	ReadResource(ctx context.Context, uri string) (json.RawMessage, error)
	ListPrompts(ctx context.Context) ([]Prompt, error)
	GetPrompt(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
	Health(ctx context.Context) error
}

// HTTPBackend is an MCP client over the Streamable HTTP transport. It performs
// the initialize handshake lazily on first use and reuses the negotiated session
// (Mcp-Session-Id) for subsequent tools/list and tools/call requests. Credentials
// are injected per request by the gateway's CredentialProvider just before send.
// 학습 주석: sessionID/protocol 은 backend 와 협상된 MCP 세션 상태다 — gateway 가
// goose 에게 발급하는 Mcp-Session-Id(session.go)와는 별개의, backend 쪽 세션이다.
type HTTPBackend struct {
	id        string
	namespace string
	url       string
	client    *http.Client
	creds     CredentialProvider

	mu          sync.Mutex
	initialized bool
	sessionID   string
	protocol    string
	idCounter   int64
}

// NewHTTPBackend builds an HTTPBackend. namespace prefixes the backend's tool
// names in the aggregated catalog; creds injects the backend's auth per request.
func NewHTTPBackend(id, namespace, url string, client *http.Client, creds CredentialProvider) *HTTPBackend {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if namespace == "" {
		namespace = id
	}
	return &HTTPBackend{id: id, namespace: namespace, url: url, client: client, creds: creds}
}

func (b *HTTPBackend) ID() string        { return b.id }
func (b *HTTPBackend) Namespace() string { return b.namespace }

func (b *HTTPBackend) nextID() json.RawMessage {
	n := atomic.AddInt64(&b.idCounter, 1)
	return json.RawMessage(fmt.Sprintf("%d", n))
}

// ensureInit performs the MCP initialize handshake once. Safe for concurrent
// callers; only the first does the work.
// 학습 주석: 모든 ListTools/CallTool/... 메서드가 자기 앞단에서 이 함수를 부른다 —
// backend 쪽 initialize 는 gateway 자신의 handleInitialize(gateway.go)와 완전히
// 독립적인, backend 별로 한 번씩 필요한 핸드셰이크다.
func (b *HTTPBackend) ensureInit(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.initialized {
		return nil
	}
	params := initializeParams{
		ProtocolVersion: defaultProtocolVersion,
		Capabilities:    json.RawMessage(`{}`),
		ClientInfo:      json.RawMessage(`{"name":"` + gatewayServerName + `","version":"` + gatewayServerVersion + `"}`),
	}
	resp, sid, err := b.roundTrip(ctx, "initialize", mustMarshal(params), "")
	if err != nil {
		return fmt.Errorf("initialize %s: %w", b.id, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize %s: %s", b.id, resp.Error.Message)
	}
	var ir initializeResult
	_ = json.Unmarshal(resp.Result, &ir)
	b.sessionID = sid
	b.protocol = ir.ProtocolVersion
	// Per spec, follow initialize with the initialized notification before any
	// other request. A backend that ignores it still works.
	_ = b.notify(ctx, "notifications/initialized", json.RawMessage(`{}`))
	b.initialized = true
	return nil
}

// reset drops the cached session so the next call re-initializes. Used when a
// backend reports its session is gone.
func (b *HTTPBackend) reset() {
	b.mu.Lock()
	b.initialized = false
	b.sessionID = ""
	b.mu.Unlock()
}

// ListTools returns the backend's advertised tools (un-namespaced).
func (b *HTTPBackend) ListTools(ctx context.Context) ([]Tool, error) {
	if err := b.ensureInit(ctx); err != nil {
		return nil, err
	}
	resp, _, err := b.roundTrip(ctx, "tools/list", json.RawMessage(`{}`), b.sessionID)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("tools/list %s: %s", b.id, resp.Error.Message)
	}
	var out toolsListResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("tools/list %s: bad result: %w", b.id, err)
	}
	return out.Tools, nil
}

// CallTool invokes one tool by its un-namespaced name and returns the raw MCP
// result object (content/isError) for the gateway to relay verbatim.
// 학습 주석: tool 은 여기서 이미 un-namespaced 다 — namespace 분리/복원은
// gateway.go 의 splitNamespaced/namespacedName 이 이 함수 호출 전후로 처리한다.
func (b *HTTPBackend) CallTool(ctx context.Context, tool string, args json.RawMessage) (json.RawMessage, error) {
	if err := b.ensureInit(ctx); err != nil {
		return nil, err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	params := mustMarshal(toolsCallParams{Name: tool, Arguments: args})
	resp, _, err := b.roundTrip(ctx, "tools/call", params, b.sessionID)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("tools/call %s/%s: %s", b.id, tool, resp.Error.Message)
	}
	return resp.Result, nil
}

// ListResources returns the backend's advertised resources (un-namespaced URIs).
func (b *HTTPBackend) ListResources(ctx context.Context) ([]Resource, error) {
	if err := b.ensureInit(ctx); err != nil {
		return nil, err
	}
	resp, _, err := b.roundTrip(ctx, "resources/list", json.RawMessage(`{}`), b.sessionID)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("resources/list %s: %s", b.id, resp.Error.Message)
	}
	var out resourcesListResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("resources/list %s: bad result: %w", b.id, err)
	}
	return out.Resources, nil
}

// ReadResource reads one resource by its un-namespaced URI and returns the raw
// MCP result object (contents) for the gateway to relay verbatim.
func (b *HTTPBackend) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) {
	if err := b.ensureInit(ctx); err != nil {
		return nil, err
	}
	params := mustMarshal(resourcesReadParams{URI: uri})
	resp, _, err := b.roundTrip(ctx, "resources/read", params, b.sessionID)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("resources/read %s/%s: %s", b.id, uri, resp.Error.Message)
	}
	return resp.Result, nil
}

// ListPrompts returns the backend's advertised prompts (un-namespaced names).
func (b *HTTPBackend) ListPrompts(ctx context.Context) ([]Prompt, error) {
	if err := b.ensureInit(ctx); err != nil {
		return nil, err
	}
	resp, _, err := b.roundTrip(ctx, "prompts/list", json.RawMessage(`{}`), b.sessionID)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("prompts/list %s: %s", b.id, resp.Error.Message)
	}
	var out promptsListResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("prompts/list %s: bad result: %w", b.id, err)
	}
	return out.Prompts, nil
}

// GetPrompt fetches one prompt by its un-namespaced name and returns the raw MCP
// result object (messages) for the gateway to relay verbatim.
func (b *HTTPBackend) GetPrompt(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if err := b.ensureInit(ctx); err != nil {
		return nil, err
	}
	params := mustMarshal(promptsGetParams{Name: name, Arguments: args})
	resp, _, err := b.roundTrip(ctx, "prompts/get", params, b.sessionID)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("prompts/get %s/%s: %s", b.id, name, resp.Error.Message)
	}
	return resp.Result, nil
}

// Health verifies the backend is reachable and speaks MCP by completing (or
// reusing) the initialize handshake.
func (b *HTTPBackend) Health(ctx context.Context) error {
	return b.ensureInit(ctx)
}

// roundTrip sends one JSON-RPC request and returns the response plus any
// Mcp-Session-Id the server issued. sessionID, when non-empty, is sent back as
// the Mcp-Session-Id header.
// 학습 주석: b.creds.Inject(b.id, httpReq.Header) 호출이 credential 이 실제로
// 네트워크에 나가는 유일한 지점이다 — 이 함수 밖 어디에도 token 문자열이
// 노출되지 않는다.
func (b *HTTPBackend) roundTrip(ctx context.Context, method string, params json.RawMessage, sessionID string) (rpcResponse, string, error) {
	reqMsg := rpcRequest{JSONRPC: jsonRPCVersion, ID: b.nextID(), Method: method, Params: params}
	body, _ := json.Marshal(reqMsg)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(body))
	if err != nil {
		return rpcResponse{}, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", sessionID)
	}
	if b.protocol != "" {
		httpReq.Header.Set("MCP-Protocol-Version", b.protocol)
	}
	if b.creds != nil {
		b.creds.Inject(b.id, httpReq.Header)
	}
	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		return rpcResponse{}, "", err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return rpcResponse{}, "", fmt.Errorf("%s returned HTTP %d", method, httpResp.StatusCode)
	}
	sid := httpResp.Header.Get("Mcp-Session-Id")
	resp, err := parseRPCResponse(httpResp.Header.Get("Content-Type"), httpResp.Body)
	return resp, sid, err
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (b *HTTPBackend) notify(ctx context.Context, method string, params json.RawMessage) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": jsonRPCVersion, "method": method, "params": params})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if b.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", b.sessionID)
	}
	if b.protocol != "" {
		httpReq.Header.Set("MCP-Protocol-Version", b.protocol)
	}
	if b.creds != nil {
		b.creds.Inject(b.id, httpReq.Header)
	}
	resp, err := b.client.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// parseRPCResponse decodes a JSON-RPC response from either an application/json
// body or a text/event-stream (SSE) body. For SSE it returns the first event
// whose data is a JSON-RPC response (carries "result" or "error").
// 학습 주석: MCP Streamable HTTP 는 응답이 application/json 또는 text/event-stream
// 둘 다일 수 있다 — 이 함수가 그 두 표현을 하나의 rpcResponse 로 통일한다.
func parseRPCResponse(contentType string, body io.Reader) (rpcResponse, error) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSEResponse(body)
	}
	var resp rpcResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return rpcResponse{}, fmt.Errorf("decode json-rpc: %w", err)
	}
	return resp, nil
}

// parseSSEResponse scans an SSE stream and returns the first `data:` payload that
// decodes to a JSON-RPC response. SSE data may span multiple `data:` lines per
// event (joined by newlines), separated by a blank line.
// 학습 주석: backend 가 SSE 로 응답하는 경우를 위한 최소 파서 — gateway 자신은
// 항상 application/json 단일 응답만 쓰고(writeRPC, gateway.go) SSE 를 goose 쪽
// 으로 열지 않는다. SSE 는 backend 와의 통신에서만 등장한다.
func parseSSEResponse(body io.Reader) (rpcResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data strings.Builder
	flush := func() (rpcResponse, bool) {
		if data.Len() == 0 {
			return rpcResponse{}, false
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(data.String()), &resp); err == nil && (len(resp.Result) > 0 || resp.Error != nil) {
			return resp, true
		}
		data.Reset()
		return rpcResponse{}, false
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" { // event boundary
			if resp, ok := flush(); ok {
				return resp, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if resp, ok := flush(); ok {
		return resp, nil
	}
	if err := scanner.Err(); err != nil {
		return rpcResponse{}, err
	}
	return rpcResponse{}, fmt.Errorf("no json-rpc response in event stream")
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
