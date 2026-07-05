package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestNewAgentHTTPClient_NoConnectionReuse pins the fix for the snapshot-restore
// /tasks hang/502 (Phase 2 KVM gate). The control plane proxies every /vms/{id}/*
// request through a single long-lived agentHTTPClient. Guest IPs are recycled
// across VM destroy/create/restore, so a keep-alive connection pooled to a
// now-destroyed VM at IP X can be reused for a request to a *different* VM that
// later reuses IP X. Because the proxied POST body is not rewindable, net/http
// cannot retry the failed reused connection, so the stale connection surfaces as
// a 502 (peer RST) or an unbounded hang (black-holed packets). The client must
// therefore never reuse a pooled connection: each proxied request dials fresh,
// exactly like waitForAgent's throwaway health-probe client (which never fails).
//
// The assertion is behavioral: N sequential requests must open N distinct
// server-side connections. With keep-alives enabled (the pre-fix bug) they all
// share one connection and this fails.
func TestNewAgentHTTPClient_NoConnectionReuse(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]struct{}{}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			seen[c.RemoteAddr().String()] = struct{}{}
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()

	client := newAgentHTTPClient()
	const n = 3
	for i := 0; i < n; i++ {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	mu.Lock()
	distinct := len(seen)
	mu.Unlock()
	if distinct != n {
		t.Fatalf("agentHTTPClient reused pooled connections: %d distinct server connections for %d requests (want %d). "+
			"Pooled keep-alive connections get reused across recycled guest IPs, causing restored-VM /tasks 502/hang.", distinct, n, n)
	}
}
