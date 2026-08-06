package main

import (
	"net/http"
	"testing"
)

// assertStreamSafeTimeouts encodes the timeout contract every ephemera HTTP
// server must satisfy.
//
// ReadHeaderTimeout + IdleTimeout are mandatory: without them a peer can hold a
// connection (and its goroutine + fd) open forever *before* authentication runs,
// so no token is needed to exhaust the daemon.
//
// ReadTimeout and WriteTimeout must stay unset. These servers legitimately hold
// connections open far longer than any fixed budget: SSE Town Wall streams and
// NDJSON task streams write for the lifetime of a task, and snapshot
// import/export plus workspace PUT read multi-gigabyte bodies. A global
// Read/WriteTimeout would sever those working features, which is why the
// header/idle pair is the correct control here.
func assertStreamSafeTimeouts(t *testing.T, name string, srv *http.Server) {
	t.Helper()
	if srv == nil {
		t.Fatalf("%s: server is nil", name)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("%s: ReadHeaderTimeout = %v, want > 0 (slowloris is reachable pre-auth)", name, srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("%s: IdleTimeout = %v, want > 0 (idle keep-alives pin goroutines and fds)", name, srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("%s: WriteTimeout = %v, want 0 — it would sever SSE/NDJSON streams and snapshot export", name, srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("%s: ReadTimeout = %v, want 0 — it would sever snapshot import and workspace PUT uploads", name, srv.ReadTimeout)
	}
}

// TestControlPlaneServerSetsStreamSafeTimeouts covers the operator control plane,
// which serves the SSE Town Wall stream and streaming task responses.
func TestControlPlaneServerSetsStreamSafeTimeouts(t *testing.T) {
	cp := newTestControlPlaneWithHandler(t)
	assertStreamSafeTimeouts(t, "control plane", cp.srv)
}

// TestMCPGatewayServerSetsStreamSafeTimeouts covers the runtime MCP gateway, the
// listener guest VMs connect to directly.
func TestMCPGatewayServerSetsStreamSafeTimeouts(t *testing.T) {
	cp := newTestCP(t)
	writeMCPFixtures(t, cp)
	t.Setenv("EPHEMERA_MCP_ENABLED", "1")

	cp.initMCPGateway()

	assertStreamSafeTimeouts(t, "mcp gateway", cp.mcpSrv)
}
