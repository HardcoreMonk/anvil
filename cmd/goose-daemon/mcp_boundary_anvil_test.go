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

// These guards lock in the Phase 3 (v0.6.0) MCP-gateway boundary decisions that
// upstream's own tests do not cover:
//
//   - The new gateway data routes (/config/mcp, /config/mcp/servers,
//     /config/builtins) are DATA APIs and must sit BEHIND bearer auth when auth
//     is configured — only /ui/ (static bundle + login) lives outside auth.
//   - "VM receives only gateway URL/config, never backend credentials": the URL
//     injected into a VM is the bridge endpoint alias and never carries a
//     backend token, and the secret-free server view exposes only has_credential.
//   - "Profile policy cannot widen access beyond servers.yaml": a profile not in
//     a server's allow-list gets no gateway URL at all.

const mcpSecretSentinel = "SENTINEL-MCP-BACKEND-TOKEN-DO-NOT-LEAK"

// writeMCPFixtures seeds configs/mcp/{servers,secrets}.yaml under the CP work dir
// with one credentialed backend restricted to the "leader" profile. The secret is
// a sentinel that must never reach a VM or a data route.
func writeMCPFixtures(t *testing.T, cp *ControlPlane) {
	t.Helper()
	dir := filepath.Join(cp.workDir, "configs", "mcp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir configs/mcp: %v", err)
	}
	servers := "servers:\n" +
		"  - id: backend\n" +
		"    namespace: backend\n" +
		"    transport: http\n" +
		"    url: http://127.0.0.1:9/mcp\n" +
		"    credential: backend_token\n" +
		"    profiles: [leader]\n"
	if err := os.WriteFile(filepath.Join(dir, "servers.yaml"), []byte(servers), 0o644); err != nil {
		t.Fatalf("write servers.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.yaml"), []byte("backend_token: "+mcpSecretSentinel+"\n"), 0o644); err != nil {
		t.Fatalf("write secrets.yaml: %v", err)
	}
}

// buildMCPConfigChain wires the new gateway data routes exactly as
// NewControlPlane does (internal mux behind authMiddleware) so the test exercises
// production routing, not a stub.
func buildMCPConfigChain(cp *ControlPlane, getClients func() []APIClient) http.Handler {
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/config/builtins", cp.handleConfigBuiltins)
	internalMux.HandleFunc("/config/mcp", cp.handleConfigMCP)
	internalMux.HandleFunc("/config/mcp/servers", cp.handleConfigMCPServers)
	return authMiddleware(getClients, nil, nil, internalMux)
}

func TestConfigMCPRoutesRequireAuthWhenConfigured(t *testing.T) {
	cp := newTestCP(t)
	chain := buildMCPConfigChain(cp, authEnabled) // auth IS configured

	for _, path := range []string{"/config/mcp", "/config/mcp/servers", "/config/builtins"} {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without token = %d, want 401 (gateway data API must be behind auth)", path, rr.Code)
		}
	}

	// A valid bearer must reach the handler (proving the 401 is auth, not routing).
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/config/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret") // matches authEnabled()'s token
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config/mcp with valid token = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestMCPInjectionCarriesURLNotCredential proves that the material a VM receives
// (the gateway URL from mcpURLForProfile) is the bridge endpoint alias and never
// the backend credential, and that profile policy cannot widen access beyond
// servers.yaml's allow-list.
func TestMCPInjectionCarriesURLNotCredential(t *testing.T) {
	cp := newTestCP(t)
	writeMCPFixtures(t, cp)
	t.Setenv("EPHEMERA_MCP_ENABLED", "1")

	cp.initMCPGateway()
	if cp.mcpGateway == nil || cp.mcpRegistry == nil {
		t.Fatal("gateway did not initialize from fixtures")
	}

	// The endpoint injected into VMs must be the bridge alias, never the secret.
	if !strings.HasPrefix(cp.mcpEndpoint, "http://ephemera-gw:") || !strings.HasSuffix(cp.mcpEndpoint, "/mcp") {
		t.Fatalf("mcpEndpoint = %q, want http://ephemera-gw:<port>/mcp alias", cp.mcpEndpoint)
	}
	if strings.Contains(cp.mcpEndpoint, mcpSecretSentinel) {
		t.Fatalf("mcpEndpoint leaked the backend credential: %q", cp.mcpEndpoint)
	}

	// A permitted profile gets the endpoint URL (the ONLY injected material)...
	leaderURL := cp.mcpURLForProfile("leader")
	if leaderURL != cp.mcpEndpoint {
		t.Fatalf("mcpURLForProfile(leader) = %q, want the gateway endpoint", leaderURL)
	}
	if strings.Contains(leaderURL, mcpSecretSentinel) {
		t.Fatalf("injected VM URL leaked the backend credential: %q", leaderURL)
	}

	// ...and a profile outside the server's allow-list gets nothing (policy cannot
	// widen beyond servers.yaml).
	if got := cp.mcpURLForProfile("worker"); got != "" {
		t.Fatalf("mcpURLForProfile(worker) = %q, want empty (profile not permitted any backend)", got)
	}

	// The secret-free server view (what /config/mcp/servers serializes) must expose
	// only has_credential, never the token value.
	blob, err := json.Marshal(cp.mcpRegistry.Servers())
	if err != nil {
		t.Fatalf("marshal server view: %v", err)
	}
	if strings.Contains(string(blob), mcpSecretSentinel) {
		t.Fatalf("server view leaked the backend credential: %s", blob)
	}
	if !strings.Contains(string(blob), `"has_credential":true`) {
		t.Fatalf("server view missing has_credential flag: %s", blob)
	}
}
