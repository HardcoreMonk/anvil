package main

import (
	"fmt"
	"net/http"

	"ephemera/internal/mcpgateway"
)

// 학습 주석 개요: /config/mcp, /config/mcp/servers 는 daemon 의 기존 API mux
// (authMiddleware 뒤)에 배선되는 operator/System UI 데이터 API다 — VM 이 직접
// 호출하는 경로가 아니다(VM 쪽 경로는 mcp_gateway.go 의 별도 gateway 리스너,
// cp.mcpEndpoint). auth 가 설정돼 있으면 bearer 없이는 401 이어야 한다는 것이
// anvil boundary guard(v0.6.0)이며 cmd/goose-daemon/mcp_boundary_anvil_test.go
// 의 TestConfigMCPRoutesRequireAuthWhenConfigured 로 고정돼 있다. 두 handler
// 모두 mcpgateway.Registry.Servers()/HealthAll 을 그대로 직렬화만 하고
// credential 원본을 다루지 않는다.

// handleConfigMCP serves GET /config/mcp — the gateway's status for the System UI:
// whether it is enabled, the VM-facing endpoint, and how many backends are
// configured. Read-only; the gateway is configured via env + configs/mcp/*.yaml.
// 학습 주석: cp.mcpEndpoint 를 그대로 노출하지만 이 값은 URL(letter-starting
// alias, mcpVMHostName)일 뿐 credential 이 아니다 — VM 에 주입되는 것과 같은 값.
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
// included (only has_credential), mirroring /config/providers' secret-free view;
// stdio servers expose their command but never args (which may carry sensitive
// values).
// 학습 주석: servers/health 두 조회 결과를 id 로 join 해서 응답을 만들 뿐, 어느
// 필드도 mcpgateway.ServerConfig.Credential/Args 를 참조하지 않는다 — 구조적으로
// 비노출인 이유가 out 맵의 key 목록 자체에 그 필드들이 없다는 점이다
// (mcp_servers_listing_anvil_test.go 가드).
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
			"transport":      s.Transport,
			"url":            s.URL,
			"command":        s.Command,
			"profiles":       s.Profiles,
			"has_credential": s.HasCredential,
			"up":             h.Up,
			"error":          h.Error,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
