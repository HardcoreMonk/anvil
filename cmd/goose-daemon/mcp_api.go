package main

import (
	"fmt"
	"net/http"

	"ephemera/internal/mcpgateway"
)

// handleConfigMCP serves GET /config/mcp — the gateway's status for the System UI:
// whether it is enabled, the VM-facing endpoint, and how many backends are
// configured. Read-only; the gateway is configured via env + configs/mcp/*.yaml.
func (cp *ControlPlane) handleConfigMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	count := 0
	if cp.mcpRegistry != nil {
		count = len(cp.mcpRegistry.Servers())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":      cp.mcpGateway != nil,
		"endpoint":     cp.mcpEndpoint,
		"server_count": count,
	})
}

// handleConfigMCPServers serves GET /config/mcp/servers — the configured backend
// MCP servers plus a live per-backend health probe. Credentials are never
// included (only has_credential), mirroring /config/providers' secret-free view.
func (cp *ControlPlane) handleConfigMCPServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	if cp.mcpRegistry == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	servers := cp.mcpRegistry.Servers()
	health := cp.mcpRegistry.HealthAll(r.Context())
	hmap := make(map[string]mcpgateway.Health, len(health))
	for _, h := range health {
		hmap[h.ID] = h
	}
	out := make([]map[string]any, 0, len(servers))
	for _, s := range servers {
		h := hmap[s.ID]
		out = append(out, map[string]any{
			"id":             s.ID,
			"namespace":      s.Namespace,
			"url":            s.URL,
			"profiles":       s.Profiles,
			"has_credential": s.HasCredential,
			"up":             h.Up,
			"error":          h.Error,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
