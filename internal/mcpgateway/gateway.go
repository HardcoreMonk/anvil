package mcpgateway

// 학습 주석 개요 (anvil v0.6.x 학습 브랜치, 기준 커밋 04e2a12)
//
// internal/mcpgateway 는 v0.6.0 에서 도입된 runtime MCP Gateway 의 핵심 패키지다.
// EPHEMERA_MCP_* 로 설정되는 이 표면은 cmd/anvil-mcp 의 IronClaw MCP adapter
// (ANVIL_MCP_*, north-bound: IronClaw -> anvil VM lifecycle 호출)와 이름만 비슷할 뿐
// 방향이 반대인 south-bound 표면이다: VM 내부 agent -> goose-daemon -> 이 gateway
// -> 실제 backend MCP server. gateway 는 cmd/anvil-mcp 를 대체하지 않으며, gateway
// tool 은 IronClaw anvil_* tool 목록/schema 에 노출되지 않는다(가드 테스트로 고정,
// ANNOTATIONS.md 참고).
//
// 이 파일(gateway.go)은 v0.6.0 core 요청 라우팅(ServeHTTP, tools/*) 위에 v0.6.2
// resources/prompts 표면(handleResourcesList/Read, handlePromptsList/Get)이
// additive 로 얹힌 결과다. 모든 표면이 공유하는 요청 흐름은 다음과 같다:
//
//	caller 신원 해석(identity.go, source IP -> VM registry) ->
//	policy 교집합 확인(policy.go, servers.yaml 을 좁히기만 함) ->
//	rate limit(ratelimit.go, VM x server 키) ->
//	backend 호출(backend.go http 또는 backend_stdio.go stdio) ->
//	audit 기록(observe hook, cmd/goose-daemon/mcp_gateway.go 의 appendMCPAudit)
//
// caller profile 은 request 의 source IP 를 VM registry 와 대조해 서버 쪽에서만
// 판정한다 — 세션이나 요청 바디로 신원을 자칭할 수 없다(identity.go 참고).

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// nowMs is the current Unix time in milliseconds, for call-duration measurement.
func nowMs() int64 { return time.Now().UnixMilli() }

// AuditRecord is one gateway call observation (a tool call, resource read, or
// prompt fetch) handed to the daemon's Observe hook for audit logging and
// metrics. Arguments and results are deliberately excluded (only metadata),
// matching Ephemera's access-log privacy invariant.
type AuditRecord struct {
	VMID       string
	Profile    string
	Server     string
	Kind       string // "tool" | "resource" | "prompt"
	Tool       string // identifier: tool name, resource URI, or prompt name
	OK         bool
	DurationMs int64
	Err        string
}

// Options configures a Gateway. Resolver and Registry are required; Policy
// defaults to "allow every configured server", Sessions to an in-memory store,
// Limiter to "allow every call", and Observe to a no-op.
type Options struct {
	Resolver CallerResolver
	Registry *Registry
	Policy   PolicyStore
	Sessions SessionStore
	Limiter  RateLimiter
	Observe  func(AuditRecord)
}

// Gateway is the MCP server endpoint goose connects to. It aggregates the
// configured backend MCP servers behind one policy-filtered, namespaced catalog.
type Gateway struct {
	resolver CallerResolver
	registry *Registry
	policy   PolicyStore
	sessions SessionStore
	limiter  RateLimiter
	observe  func(AuditRecord)
}

// New builds a Gateway from Options, filling in defaults.
// 학습 주석: Options 의 nil 필드에 기본값(전역 허용 Policy, in-memory Sessions,
// 무제한 Limiter, no-op Observe)을 채워 넣어 Gateway 를 완성한다.
func New(opts Options) *Gateway {
	g := &Gateway{
		resolver: opts.Resolver,
		registry: opts.Registry,
		policy:   opts.Policy,
		sessions: opts.Sessions,
		limiter:  opts.Limiter,
		observe:  opts.Observe,
	}
	if g.policy == nil {
		g.policy = NewStaticPolicyStore(opts.Registry.ServerConfigs())
	}
	if g.sessions == nil {
		g.sessions = NewMemSessionStore()
	}
	if g.limiter == nil {
		g.limiter = noopLimiter{}
	}
	if g.observe == nil {
		g.observe = func(AuditRecord) {}
	}
	return g
}

// ServeHTTP implements the MCP Streamable HTTP server. goose POSTs a JSON-RPC
// request; the gateway resolves the caller by source IP, dispatches, and replies
// with a single application/json JSON-RPC response.
// 학습 주석: 요청 흐름의 진입점. DELETE(세션 종료)와 POST 만 허용하고,
// resolver.Resolve 로 caller 신원을 먼저 해석한다 — 이 단계에서 실패하면(등록 안 된
// source IP) 뒤의 policy/rate/backend 단계로 전혀 진행하지 않고 403 이다.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		// Session teardown: drop the session if we know it. Always 200.
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			g.sessions.Delete(sid)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		// The gateway does not open server-initiated SSE streams (GET); reject.
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	caller, err := g.resolver.Resolve(r)
	if err != nil {
		slog.Warn("mcp gateway: unresolved caller", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, "", newError(nil, codeParseError, "invalid JSON"))
		return
	}

	// Notifications get no response body (202 Accepted).
	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		g.handleInitialize(w, caller, req)
	case "ping":
		writeRPC(w, "", newResult(req.ID, map[string]any{}))
	case "tools/list":
		g.handleToolsList(w, r, caller, req)
	case "tools/call":
		g.handleToolsCall(w, r, caller, req)
	case "resources/list":
		g.handleResourcesList(w, r, caller, req)
	case "resources/read":
		g.handleResourcesRead(w, r, caller, req)
	case "prompts/list":
		g.handlePromptsList(w, r, caller, req)
	case "prompts/get":
		g.handlePromptsGet(w, r, caller, req)
	default:
		writeRPC(w, "", newError(req.ID, codeMethodNotFound, "method not found: "+req.Method))
	}
}

// handleInitialize negotiates the session: it issues an Mcp-Session-Id, echoes
// the client's requested protocol version (or the gateway default), and
// advertises the tools, resources, and prompts capabilities.
// 학습 주석: sessions.Create(caller) 는 이미 해석된 Caller(VMID, Profile)를 세션에
// 묶는다 — 즉 세션은 신원의 근거가 아니라 신원 해석 결과를 담는 그릇일 뿐이다.
func (g *Gateway) handleInitialize(w http.ResponseWriter, caller Caller, req rpcRequest) {
	var p initializeParams
	_ = json.Unmarshal(req.Params, &p)
	version := p.ProtocolVersion
	if version == "" {
		version = defaultProtocolVersion
	}
	sid := g.sessions.Create(caller)
	result := initializeResult{
		ProtocolVersion: version,
		Capabilities:    gatewayCapabilities(),
		ServerInfo:      serverInfo{Name: gatewayServerName, Version: gatewayServerVersion},
	}
	if sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	writeRPC(w, sid, newResult(req.ID, result))
	slog.Debug("mcp gateway: initialized", "vm", caller.VMID, "profile", caller.Profile)
}

// handleToolsList returns the aggregated, namespaced catalog of every tool from
// the backends the caller's profile may use. A backend that errors is skipped
// (logged) so one bad server does not blank the whole catalog.
// 학습 주석: policy.Allows(server)로 서버 단위 1차 필터링 후, policy.AllowsTool 로
// tool 단위 2차 필터링을 한다. 이 이중 필터가 policy.go 의 교집합-축소 모델이다.
func (g *Gateway) handleToolsList(w http.ResponseWriter, r *http.Request, caller Caller, req rpcRequest) {
	policy := g.policy.For(caller.Profile)
	var tools []Tool
	for _, s := range g.registry.ServerConfigs() {
		if !policy.Allows(s.ID) {
			continue
		}
		b, ok := g.registry.Backend(s.ID)
		if !ok {
			continue
		}
		bt, err := b.ListTools(r.Context())
		if err != nil {
			slog.Warn("mcp gateway: backend tools/list failed", "server", s.ID, "err", err)
			continue
		}
		for _, t := range bt {
			if !policy.AllowsTool(s.ID, t.Name) {
				continue
			}
			t.Name = namespacedName(b.Namespace(), t.Name)
			tools = append(tools, t)
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	writeRPC(w, "", newResult(req.ID, toolsListResult{Tools: tools}))
}

// handleToolsCall routes a namespaced tool call to its backend after a policy
// check, injects the backend's credentials (inside the backend), and relays the
// result. Every call is reported to the Observe hook.
// 학습 주석: 요청 흐름 전체(identity는 caller 인자로 이미 확보 -> AllowsTool ->
// limiter.Allow -> b.CallTool -> observe)를 한 함수에서 순서대로 보여주는 대표
// handler다. credential 은 backend 내부(HTTPBackend/StdioBackend)에서만 주입되고
// 이 함수는 credential 값을 전혀 보지 않는다.
func (g *Gateway) handleToolsCall(w http.ResponseWriter, r *http.Request, caller Caller, req rpcRequest) {
	var p toolsCallParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "invalid tools/call params"))
		return
	}
	ns, tool, ok := splitNamespaced(p.Name)
	if !ok {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "tool name is not namespaced: "+p.Name))
		return
	}
	b, ok := g.registry.BackendByNamespace(ns)
	if !ok {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "unknown tool namespace: "+ns))
		return
	}
	// Policy: the caller's profile must be allowed to use this backend and the
	// specific tool (AllowsTool also re-checks the backend is permitted at all).
	if !g.policy.For(caller.Profile).AllowsTool(b.ID(), tool) {
		g.observe(AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "tool", Tool: tool, OK: false, Err: "forbidden"})
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "tool not permitted for this profile: "+p.Name))
		return
	}
	// Rate limit: a transient, retryable per-(VM, server) budget.
	if !g.limiter.Allow(caller.VMID, b.ID()) {
		g.observe(AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "tool", Tool: tool, OK: false, Err: "rate limited"})
		writeRPC(w, "", newError(req.ID, codeInternalError, "rate limit exceeded for this server; retry shortly"))
		return
	}

	start := nowMs()
	result, err := b.CallTool(r.Context(), tool, p.Arguments)
	rec := AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "tool", Tool: tool, DurationMs: nowMs() - start}
	if err != nil {
		rec.OK, rec.Err = false, err.Error()
		g.observe(rec)
		slog.Warn("mcp gateway: tool call failed", "server", b.ID(), "tool", tool, "err", err)
		writeRPC(w, "", newError(req.ID, codeInternalError, "tool call failed: "+err.Error()))
		return
	}
	rec.OK = true
	g.observe(rec)
	writeRPC(w, "", rpcResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result})
}

// handleResourcesList returns the aggregated, namespaced catalog of every
// resource from the backends the caller's profile may use. A backend that errors
// is skipped (logged) so one bad server does not blank the whole catalog.
// 학습 주석: v0.6.2 에서 tools/list 옆에 추가된 표면. 같은 policy.Allows 필터를
// 재사용하되, tool 처럼 개별 이름 단위 allow/deny(AllowsTool)는 적용하지 않는다.
func (g *Gateway) handleResourcesList(w http.ResponseWriter, r *http.Request, caller Caller, req rpcRequest) {
	policy := g.policy.For(caller.Profile)
	var resources []Resource
	for _, s := range g.registry.ServerConfigs() {
		if !policy.Allows(s.ID) {
			continue
		}
		b, ok := g.registry.Backend(s.ID)
		if !ok {
			continue
		}
		br, err := b.ListResources(r.Context())
		if err != nil {
			slog.Warn("mcp gateway: backend resources/list failed", "server", s.ID, "err", err)
			continue
		}
		// Namespace the URI the same way tool names are namespaced, so resources/read
		// can route back to the owning backend.
		for _, res := range br {
			res.URI = namespacedName(b.Namespace(), res.URI)
			resources = append(resources, res)
		}
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].URI < resources[j].URI })
	writeRPC(w, "", newResult(req.ID, resourcesListResult{Resources: resources}))
}

// handleResourcesRead routes a namespaced resource URI to its backend after a
// policy and rate-limit check, and relays the result. Every read is reported to
// the Observe hook.
// 학습 주석: v0.6.2 하드닝 대상 — resources/read 도 tools/call 과 동일한
// limiter.Allow(caller.VMID, b.ID())를 거친다(가드: resources_prompts_ratelimit_
// anvil_test.go). rate bucket 을 tool 과 공유해 우회 경로가 없다.
func (g *Gateway) handleResourcesRead(w http.ResponseWriter, r *http.Request, caller Caller, req rpcRequest) {
	var p resourcesReadParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "invalid resources/read params"))
		return
	}
	ns, uri, ok := splitNamespaced(p.URI)
	if !ok {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "resource uri is not namespaced: "+p.URI))
		return
	}
	b, ok := g.registry.BackendByNamespace(ns)
	if !ok {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "unknown resource namespace: "+ns))
		return
	}
	if !g.policy.For(caller.Profile).Allows(b.ID()) {
		g.observe(AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "resource", Tool: uri, OK: false, Err: "forbidden"})
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "resource not permitted for this profile: "+p.URI))
		return
	}
	if !g.limiter.Allow(caller.VMID, b.ID()) {
		g.observe(AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "resource", Tool: uri, OK: false, Err: "rate limited"})
		writeRPC(w, "", newError(req.ID, codeInternalError, "rate limit exceeded for this server; retry shortly"))
		return
	}

	start := nowMs()
	result, err := b.ReadResource(r.Context(), uri)
	rec := AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "resource", Tool: uri, DurationMs: nowMs() - start}
	if err != nil {
		rec.OK, rec.Err = false, err.Error()
		g.observe(rec)
		slog.Warn("mcp gateway: resource read failed", "server", b.ID(), "uri", uri, "err", err)
		writeRPC(w, "", newError(req.ID, codeInternalError, "resource read failed: "+err.Error()))
		return
	}
	rec.OK = true
	g.observe(rec)
	writeRPC(w, "", rpcResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result})
}

// handlePromptsList returns the aggregated, namespaced catalog of every prompt
// from the backends the caller's profile may use. A backend that errors is
// skipped (logged) so one bad server does not blank the whole catalog.
// 학습 주석: resources/list 와 같은 구조의 v0.6.2 추가 표면. namespace 규칙도
// 동일하게 namespacedName(b.Namespace(), pr.Name) 으로 tool/resource 와 통일된다.
func (g *Gateway) handlePromptsList(w http.ResponseWriter, r *http.Request, caller Caller, req rpcRequest) {
	policy := g.policy.For(caller.Profile)
	var prompts []Prompt
	for _, s := range g.registry.ServerConfigs() {
		if !policy.Allows(s.ID) {
			continue
		}
		b, ok := g.registry.Backend(s.ID)
		if !ok {
			continue
		}
		bp, err := b.ListPrompts(r.Context())
		if err != nil {
			slog.Warn("mcp gateway: backend prompts/list failed", "server", s.ID, "err", err)
			continue
		}
		for _, pr := range bp {
			pr.Name = namespacedName(b.Namespace(), pr.Name)
			prompts = append(prompts, pr)
		}
	}
	sort.Slice(prompts, func(i, j int) bool { return prompts[i].Name < prompts[j].Name })
	writeRPC(w, "", newResult(req.ID, promptsListResult{Prompts: prompts}))
}

// handlePromptsGet routes a namespaced prompt name to its backend after a policy
// and rate-limit check, and relays the result. Every fetch is reported to the
// Observe hook.
// 학습 주석: handleToolsCall/handleResourcesRead 와 동형 — policy.Allows ->
// limiter.Allow -> backend 호출 -> observe. prompts 도 tool 과 같은 rate bucket 을
// 공유해 별도 우회 경로가 되지 않는다.
func (g *Gateway) handlePromptsGet(w http.ResponseWriter, r *http.Request, caller Caller, req rpcRequest) {
	var p promptsGetParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "invalid prompts/get params"))
		return
	}
	ns, name, ok := splitNamespaced(p.Name)
	if !ok {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "prompt name is not namespaced: "+p.Name))
		return
	}
	b, ok := g.registry.BackendByNamespace(ns)
	if !ok {
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "unknown prompt namespace: "+ns))
		return
	}
	if !g.policy.For(caller.Profile).Allows(b.ID()) {
		g.observe(AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "prompt", Tool: name, OK: false, Err: "forbidden"})
		writeRPC(w, "", newError(req.ID, codeInvalidParams, "prompt not permitted for this profile: "+p.Name))
		return
	}
	if !g.limiter.Allow(caller.VMID, b.ID()) {
		g.observe(AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "prompt", Tool: name, OK: false, Err: "rate limited"})
		writeRPC(w, "", newError(req.ID, codeInternalError, "rate limit exceeded for this server; retry shortly"))
		return
	}

	start := nowMs()
	result, err := b.GetPrompt(r.Context(), name, p.Arguments)
	rec := AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: b.ID(), Kind: "prompt", Tool: name, DurationMs: nowMs() - start}
	if err != nil {
		rec.OK, rec.Err = false, err.Error()
		g.observe(rec)
		slog.Warn("mcp gateway: prompt get failed", "server", b.ID(), "prompt", name, "err", err)
		writeRPC(w, "", newError(req.ID, codeInternalError, "prompt get failed: "+err.Error()))
		return
	}
	rec.OK = true
	g.observe(rec)
	writeRPC(w, "", rpcResponse{JSONRPC: jsonRPCVersion, ID: req.ID, Result: result})
}

// writeRPC writes a single JSON-RPC response as application/json. sid, when set
// on initialize, has already been put on the header; it is accepted here only to
// keep call sites uniform.
// 학습 주석: 모든 handle* 가 공유하는 단일 JSON-RPC 응답 writer. audit/rate/policy
// 로직이 여기 섞이지 않는 얇은 직렬화 계층이라는 점이 이 파일의 계층 분리 원칙이다.
func writeRPC(w http.ResponseWriter, _ string, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
