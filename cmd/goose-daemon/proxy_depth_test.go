package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestProxyAgentEndpoint_DepthGuard verifies the nested-invocation depth guard
// (v0.4.4): a /tasks hop at or over EPHEMERA_MAX_TASK_DEPTH is refused with 508
// Loop Detected before the agent is contacted.
func TestProxyAgentEndpoint_DepthGuard(t *testing.T) {
	cp := &ControlPlane{vms: make(map[string]*runningVM), agentHTTPClient: &http.Client{}}
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{GuestIP: "127.0.0.1"}}

	old := maxTaskDepth
	maxTaskDepth = 2
	defer func() { maxTaskDepth = old }()

	r := httptest.NewRequest(http.MethodPost, "/vms/vm-1/tasks", strings.NewReader(`{"prompt":"x"}`))
	r.Header.Set("X-Ephemera-Task-Depth", "2") // == cap
	w := httptest.NewRecorder()
	cp.proxyAgentEndpoint(w, r, "vm-1", "/tasks")
	if w.Code != http.StatusLoopDetected {
		t.Fatalf("expected 508 at depth==max, got %d", w.Code)
	}
}

// TestProxyAgentEndpoint_ForwardsQuery verifies the proxy passes the request's
// query string through to the agent so ?stream=1 (v0.4.4) selects goose-agent's
// streaming path. Regression guard: the proxy previously dropped RawQuery.
func TestProxyAgentEndpoint_ForwardsQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	cp := &ControlPlane{vms: make(map[string]*runningVM), agentHTTPClient: &http.Client{}}
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{GuestIP: host}}
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	r := httptest.NewRequest(http.MethodPost, "/vms/vm-1/tasks?stream=1", strings.NewReader(`{"prompt":"x"}`))
	w := httptest.NewRecorder()
	cp.proxyAgentEndpoint(w, r, "vm-1", "/tasks")
	if gotQuery != "stream=1" {
		t.Errorf("expected agent to receive query %q, got %q", "stream=1", gotQuery)
	}
}

// TestProxyAgentEndpoint_DepthForwarded verifies a /tasks hop below the cap is
// forwarded to the agent with the depth incremented by one.
func TestProxyAgentEndpoint_DepthForwarded(t *testing.T) {
	var gotDepth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDepth = r.Header.Get("X-Ephemera-Task-Depth")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"output":"ok"}`))
	}))
	defer srv.Close()

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	cp := &ControlPlane{vms: make(map[string]*runningVM), agentHTTPClient: &http.Client{}}
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{GuestIP: host}}

	oldPort, oldMax := agentPort, maxTaskDepth
	agentPort, maxTaskDepth = port, 5
	defer func() { agentPort, maxTaskDepth = oldPort, oldMax }()

	r := httptest.NewRequest(http.MethodPost, "/vms/vm-1/tasks", strings.NewReader(`{"prompt":"x"}`))
	r.Header.Set("X-Ephemera-Task-Depth", "2")
	w := httptest.NewRecorder()
	cp.proxyAgentEndpoint(w, r, "vm-1", "/tasks")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 below cap, got %d", w.Code)
	}
	if gotDepth != "3" {
		t.Errorf("expected forwarded X-Ephemera-Task-Depth=3, got %q", gotDepth)
	}
}
