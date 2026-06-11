package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ephemera/internal/mcpgateway"
)

func TestHandleConfigMCP_Disabled(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleConfigMCP(rr, httptest.NewRequest(http.MethodGet, "/config/mcp", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false when gateway off", got["enabled"])
	}
	if got["server_count"] != float64(0) {
		t.Fatalf("server_count = %v, want 0", got["server_count"])
	}
}

func TestHandleConfigMCPServers_DisabledEmpty(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleConfigMCPServers(rr, httptest.NewRequest(http.MethodGet, "/config/mcp/servers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var list []any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty server list when gateway off, got %d", len(list))
	}
}

func TestHandleConfigMCP_MethodGuards(t *testing.T) {
	cp := newTestCP(t)
	for _, h := range []http.HandlerFunc{cp.handleConfigMCP, cp.handleConfigMCPServers} {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodPost, "/config/mcp", nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST status = %d, want 405", rr.Code)
		}
	}
}

func TestHandleConfigMCPServers_TransportFields(t *testing.T) {
	cp := newTestCP(t)
	// /bin/false passes the LookPath preflight but dies at first use, giving a
	// deterministic up=false health probe without any network.
	reg, err := mcpgateway.NewRegistry([]mcpgateway.ServerConfig{
		{ID: "remote", URL: "http://127.0.0.1:1/mcp"},
		{ID: "local", Transport: "stdio", Command: "/bin/false"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cp.mcpRegistry = reg
	rr := httptest.NewRecorder()
	cp.handleConfigMCPServers(rr, httptest.NewRequest(http.MethodGet, "/config/mcp/servers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d servers, want 2", len(list))
	}
	byID := map[string]map[string]any{}
	for _, s := range list {
		byID[s["id"].(string)] = s
	}
	remote, local := byID["remote"], byID["local"]
	if remote["transport"] != "http" || remote["command"] != "" {
		t.Fatalf("remote row wrong (transport must normalize to http): %+v", remote)
	}
	if local["transport"] != "stdio" || local["command"] != "/bin/false" || local["url"] != "" {
		t.Fatalf("local row wrong: %+v", local)
	}
	if local["up"] != false || local["error"] == "" {
		t.Fatalf("dead stdio server must probe down with an error: %+v", local)
	}
	if _, leaked := local["args"]; leaked {
		t.Fatal("args must never be exposed via the API")
	}
}

func TestMCPStdioUser(t *testing.T) {
	t.Setenv("EPHEMERA_MCP_STDIO_USER", "")
	if got := mcpStdioUser(); got != "nobody" {
		t.Fatalf("default stdio user = %q, want nobody", got)
	}
	t.Setenv("EPHEMERA_MCP_STDIO_USER", "mcprunner")
	if got := mcpStdioUser(); got != "mcprunner" {
		t.Fatalf("stdio user override = %q, want mcprunner", got)
	}
}

func TestLookupVMByIP(t *testing.T) {
	cp := newTestCP(t)
	cp.vms["vm-a"] = &runningVM{VMInfo: VMInfo{VMID: "vm-a", GuestIP: "10.0.1.5", Profile: "leader"}}

	if id, profile, ok := cp.lookupVMByIP("10.0.1.5"); !ok || id != "vm-a" || profile != "leader" {
		t.Fatalf("lookup = %q/%q/%v, want vm-a/leader/true", id, profile, ok)
	}
	if _, _, ok := cp.lookupVMByIP("10.0.1.99"); ok {
		t.Fatal("unknown IP should not resolve")
	}
}
