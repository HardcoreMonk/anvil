package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"ephemera/internal/mcpgateway"
)

func TestParseProfileMCPServers(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		want  []string
		bound bool
	}{
		// A missing key means "no binding" — the PolicyStore uses servers.yaml as-is.
		{"missing key", "GOOSE_PROVIDER: groq\n", nil, false},
		{"single", "EPHEMERA_MCP_SERVERS: deepwiki\n", []string{"deepwiki"}, true},
		{"multiple", "EPHEMERA_MCP_SERVERS: deepwiki,github\n", []string{"deepwiki", "github"}, true},
		{"spaces trimmed", "EPHEMERA_MCP_SERVERS: deepwiki , github \n", []string{"deepwiki", "github"}, true},
		// An explicit empty value is bound to nothing (the profile uses no servers).
		{"explicit empty", "EPHEMERA_MCP_SERVERS:\n", []string{}, true},
		// Indented (nested) keys must be ignored.
		{"ignores nested", "extensions:\n  EPHEMERA_MCP_SERVERS: nope\n", nil, false},
	}
	for _, c := range cases {
		got, bound := parseProfileMCPServers([]byte(c.yaml))
		if bound != c.bound || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: = %#v,%v want %#v,%v", c.name, got, bound, c.want, c.bound)
		}
	}
}

func TestNormalizeMCPServers(t *testing.T) {
	got := normalizeMCPServers([]string{"github", "deepwiki", "github", " "})
	if !reflect.DeepEqual(got, []string{"deepwiki", "github"}) {
		t.Errorf("normalizeMCPServers = %#v, want [deepwiki github]", got)
	}
}

func TestHandleConfigProfileMCP_PutThenGet(t *testing.T) {
	cp := newTestCP(t)
	reg, err := mcpgateway.NewRegistry([]mcpgateway.ServerConfig{
		{ID: "deepwiki", URL: "https://mcp.deepwiki.com/mcp"},
		{ID: "github", URL: "https://api.github.com/mcp"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cp.mcpRegistry = reg
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	// PUT through the router so the /mcp suffix dispatch runs.
	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/mcp", strings.NewReader(`{"servers":["github","deepwiki"]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	out := mustRead(t, path)
	// Stored sorted + de-duplicated.
	if !strings.Contains(out, "EPHEMERA_MCP_SERVERS: deepwiki,github") {
		t.Fatalf("EPHEMERA_MCP_SERVERS not written sorted:\n%s", out)
	}
	// The original comments/keys/extensions block survive the line-based edit.
	for _, want := range []string{"# Goose config", "GOOSE_PROVIDER: google", "extensions:"} {
		if !strings.Contains(out, want) {
			t.Errorf("write dropped %q:\n%s", want, out)
		}
	}

	// GET reflects it.
	rr = httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodGet, "/config/profiles/worker/mcp", nil))
	var resp struct {
		Servers []string `json:"servers"`
		Bound   bool     `json:"bound"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Bound || !reflect.DeepEqual(resp.Servers, []string{"deepwiki", "github"}) {
		t.Fatalf("GET = %+v, want {[deepwiki github] true}", resp)
	}
}

func TestHandleConfigProfileMCP_RejectsUnknown(t *testing.T) {
	cp := newTestCP(t)
	reg, _ := mcpgateway.NewRegistry([]mcpgateway.ServerConfig{{ID: "deepwiki", URL: "https://x.example/mcp"}}, nil, nil)
	cp.mcpRegistry = reg
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/mcp", strings.NewReader(`{"servers":["deepwiki","ghost"]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(mustRead(t, path), "EPHEMERA_MCP_SERVERS") {
		t.Fatal("a rejected PUT must not write the key")
	}
}

func TestHandleConfigProfileMCP_GetUnboundIsNull(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodGet, "/config/profiles/worker/mcp", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Servers []string `json:"servers"`
		Bound   bool     `json:"bound"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Bound || resp.Servers != nil {
		t.Fatalf("unbound profile GET = %+v, want {null false}", resp)
	}
}
