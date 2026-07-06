package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ephemera/internal/mcpgateway"
)

// TestHandleConfigMCPServers_NeverLeaksArgsOrCredential is an anvil guard for the
// v0.6.4 hardening rule: "GET /config/mcp/servers exposes transport and command
// but never args or credentials." Upstream's TestHandleConfigMCPServers_Transport
// Fields proves args are absent, but only for a stdio server with no credential
// and no args. This drives a stdio server that carries BOTH a resolved credential
// token and argument values, then asserts the raw JSON response leaks neither the
// token nor the args — while still surfacing transport, command, and the
// has_credential flag the operator console needs.
func TestHandleConfigMCPServers_NeverLeaksArgsOrCredential(t *testing.T) {
	const (
		credSentinel = "SUPER_SECRET_TOKEN_VALUE"
		argSentinel  = "SECRET_ARG_VALUE"
	)
	cp := newTestCP(t)
	reg, err := mcpgateway.NewRegistry([]mcpgateway.ServerConfig{{
		ID:            "local",
		Transport:     "stdio",
		Command:       "/bin/false",
		Args:          []string{"--flag", argSentinel},
		Credential:    "tok",
		CredentialEnv: "LOCAL_TOKEN",
	}}, map[string]string{"tok": credSentinel}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cp.mcpRegistry = reg

	rr := httptest.NewRecorder()
	cp.handleConfigMCPServers(rr, httptest.NewRequest(http.MethodGet, "/config/mcp/servers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// The two things that must never appear anywhere in the response.
	if strings.Contains(body, credSentinel) {
		t.Fatalf("credential token leaked in /config/mcp/servers response: %s", body)
	}
	if strings.Contains(body, argSentinel) {
		t.Fatalf("stdio args leaked in /config/mcp/servers response: %s", body)
	}
	// The operator-facing metadata that must be present.
	if !strings.Contains(body, `"transport":"stdio"`) || !strings.Contains(body, `"command":"/bin/false"`) {
		t.Fatalf("transport/command missing from response: %s", body)
	}
	if !strings.Contains(body, `"has_credential":true`) {
		t.Fatalf("has_credential flag missing/false for a credentialed server: %s", body)
	}
}
