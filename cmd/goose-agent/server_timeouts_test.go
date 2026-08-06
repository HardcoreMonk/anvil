package main

import (
	"net/http"
	"testing"
)

// TestNewAgentServerSetsStreamSafeTimeouts pins the in-guest agent listener's
// timeout contract.
//
// ReadHeaderTimeout + IdleTimeout are mandatory: both apply before the handler
// (and therefore before agentAuthMiddleware's bearer check) runs, so without them
// a peer inside the VM network can pin goroutines and fds with no valid token.
//
// ReadTimeout and WriteTimeout must stay unset: /tasks streams NDJSON for the
// whole life of a task and /workspace accepts large uploads, both of which a
// global request/response budget would sever.
func TestNewAgentServerSetsStreamSafeTimeouts(t *testing.T) {
	srv := newAgentServer(":8080", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0 (slowloris is reachable pre-auth)", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want > 0 (idle keep-alives pin goroutines and fds)", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — it would sever the NDJSON task stream", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 — it would sever workspace uploads", srv.ReadTimeout)
	}
}

// TestNewAgentServerKeepsAddrAndHandler proves the extraction is a pure refactor:
// the server still binds the resolved address and serves the mux it was given.
func TestNewAgentServerKeepsAddrAndHandler(t *testing.T) {
	mux := http.NewServeMux()

	srv := newAgentServer(":9999", mux)

	if srv.Addr != ":9999" {
		t.Errorf("Addr = %q, want :9999", srv.Addr)
	}
	if srv.Handler != http.Handler(mux) {
		t.Errorf("Handler = %v, want the handler passed in", srv.Handler)
	}
}
