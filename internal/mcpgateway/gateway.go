package mcpgateway

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
	Kind       string // "tool" | "resource" | "prompt" | "list"
	Tool       string // identifier: tool name, resource URI, prompt name, or list method
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

// fanOut runs one backend leg of a */list method: it draws from the caller's
// per-(VM, server) budget, times the backend call, and reports the leg to the
// Observe hook as a metadata-only "list" record — the audit trail for catalog
// enumeration, which reaches every allowed backend on host-side credentials the
// VM never sees. It reports whether the leg's result may be used; a throttled or
// failing leg is skipped, the same degradation an unreachable backend produces.
func (g *Gateway) fanOut(caller Caller, server, method string, call func() error) bool {
	rec := AuditRecord{VMID: caller.VMID, Profile: caller.Profile, Server: server, Kind: "list", Tool: method}
	if !g.limiter.Allow(caller.VMID, server) {
		rec.Err = "rate limited"
		g.observe(rec)
		slog.Warn("mcp gateway: backend list rate limited", "method", method, "server", server, "vm", caller.VMID)
		return false
	}
	start := nowMs()
	err := call()
	rec.DurationMs = nowMs() - start
	if err != nil {
		rec.Err = err.Error()
		g.observe(rec)
		slog.Warn("mcp gateway: backend list failed", "method", method, "server", server, "err", err)
		return false
	}
	rec.OK = true
	g.observe(rec)
	return true
}

// handleToolsList returns the aggregated, namespaced catalog of every tool from
// the backends the caller's profile may use. A backend that errors is skipped
// (logged) so one bad server does not blank the whole catalog.
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
		var bt []Tool
		if !g.fanOut(caller, s.ID, "tools/list", func() (err error) {
			bt, err = b.ListTools(r.Context())
			return err
		}) {
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
		var br []Resource
		if !g.fanOut(caller, s.ID, "resources/list", func() (err error) {
			br, err = b.ListResources(r.Context())
			return err
		}) {
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
		var bp []Prompt
		if !g.fanOut(caller, s.ID, "prompts/list", func() (err error) {
			bp, err = b.ListPrompts(r.Context())
			return err
		}) {
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
func writeRPC(w http.ResponseWriter, _ string, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
