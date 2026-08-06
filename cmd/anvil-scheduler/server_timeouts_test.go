package main

import (
	"net/http"
	"testing"
)

// TestNewSchedulerServerSetsStreamSafeTimeouts pins the scheduler listener's
// timeout contract.
//
// ReadHeaderTimeout + IdleTimeout are mandatory: both apply before the handler
// (and therefore before any token check) runs, so without them an unauthenticated
// peer can pin goroutines and fds indefinitely.
//
// ReadTimeout and WriteTimeout must stay unset, matching the other ephemera
// servers: a global budget on the whole request/response would sever long-lived
// bodies and streamed responses.
func TestNewSchedulerServerSetsStreamSafeTimeouts(t *testing.T) {
	srv := newSchedulerServer("127.0.0.1:0", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want > 0 (slowloris is reachable pre-auth)", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want > 0 (idle keep-alives pin goroutines and fds)", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 — a global write budget severs streamed responses", srv.WriteTimeout)
	}
	if srv.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 — a global read budget severs large uploads", srv.ReadTimeout)
	}
}

// TestNewSchedulerServerKeepsAddrAndHandler proves the extraction is a pure
// refactor: the server still binds the configured address and serves the handler
// it was given.
func TestNewSchedulerServerKeepsAddrAndHandler(t *testing.T) {
	mux := http.NewServeMux()

	srv := newSchedulerServer("127.0.0.1:3010", mux)

	if srv.Addr != "127.0.0.1:3010" {
		t.Errorf("Addr = %q, want 127.0.0.1:3010", srv.Addr)
	}
	if srv.Handler != http.Handler(mux) {
		t.Errorf("Handler = %v, want the handler passed in", srv.Handler)
	}
}
