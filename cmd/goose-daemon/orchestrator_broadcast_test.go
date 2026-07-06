package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"ephemera/internal/orchestrator"
)

// TestBroadcastFlock_UnknownFlockAnd400ShortCircuits covers the guard paths of
// POST /flocks/{id}/broadcast that never contact an agent (KVM-free): unknown
// flock → 404, malformed body → 400, empty prompt → 400. Mirrors the
// proxy_depth_test.go convention (direct handler call, httptest recorder).
func TestBroadcastFlock_UnknownFlockAnd400ShortCircuits(t *testing.T) {
	cp := newMetricsTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: orchestrator.AgentStatusReady})

	rr := httptest.NewRecorder()
	cp.broadcastFlock(rr, httptest.NewRequest(http.MethodPost, "/flocks/nope/broadcast", strings.NewReader(`{"body":"hi"}`)), "nope")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown flock = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	cp.broadcastFlock(rr, httptest.NewRequest(http.MethodPost, "/flocks/flock-1/broadcast", strings.NewReader(`{`)), "flock-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", rr.Code)
	}

	rr = httptest.NewRecorder()
	cp.broadcastFlock(rr, httptest.NewRequest(http.MethodPost, "/flocks/flock-1/broadcast", strings.NewReader(`{"body":""}`)), "flock-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt = %d, want 400", rr.Code)
	}
}

// TestBroadcastFlock_FanOutShapeAndNoToken drives a successful scatter-gather
// against httptest agents and asserts (1) the sent/skipped/failed tally, (2)
// each per-agent result is shaped only {status,output,error}, and (3) the per-VM
// agent bearer token never appears in the response. #4 daemon-side coverage
// (previously only KVM e2e 57h/59e exercised this path).
func TestBroadcastFlock_FanOutShapeAndNoToken(t *testing.T) {
	const okToken = "tok-ok-DO-NOT-LEAK"
	const busyToken = "tok-busy-DO-NOT-LEAK"

	// The agent server routes by the injected per-VM bearer token: the busy VM's
	// token yields 503 (reported "busy"), the other yields 200 with output.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+busyToken {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"ran the task"}`))
	}))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	cp := newMetricsTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "worker-1", Role: "worker", VMID: "vm-ok", Status: orchestrator.AgentStatusReady})
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "worker-2", Role: "worker", VMID: "vm-busy", Status: orchestrator.AgentStatusReady})
	cp.vms["vm-ok"] = &runningVM{VMInfo: VMInfo{VMID: "vm-ok", GuestIP: host}, agentToken: okToken}
	cp.vms["vm-busy"] = &runningVM{VMInfo: VMInfo{VMID: "vm-busy", GuestIP: host}, agentToken: busyToken}

	rr := httptest.NewRecorder()
	cp.broadcastFlock(rr, httptest.NewRequest(http.MethodPost, "/flocks/flock-1/broadcast", strings.NewReader(`{"body":"do it"}`)), "flock-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("broadcast = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// The per-VM agent token must never appear in the fan-out response.
	for _, s := range []string{okToken, busyToken, "agent_token"} {
		if strings.Contains(body, s) {
			t.Errorf("broadcast response leaked %q: %s", s, body)
		}
	}

	var resp struct {
		Agents  int                                   `json:"agents"`
		Sent    int                                   `json:"sent"`
		Skipped int                                   `json:"skipped"`
		Failed  int                                   `json:"failed"`
		Results map[string]map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode broadcast response: %v", err)
	}
	if resp.Agents != 2 || resp.Sent != 1 || resp.Skipped != 1 || resp.Failed != 0 {
		t.Fatalf("tally agents=%d sent=%d skipped=%d failed=%d, want 2/1/1/0", resp.Agents, resp.Sent, resp.Skipped, resp.Failed)
	}

	// Each per-agent result carries only status/output/error (no token/url/etc.).
	allowed := map[string]bool{"status": true, "output": true, "error": true}
	for agentID, res := range resp.Results {
		if _, ok := res["status"]; !ok {
			t.Errorf("agent %s result missing status: %v", agentID, res)
		}
		for k := range res {
			if !allowed[k] {
				t.Errorf("agent %s result has unexpected key %q (want only status/output/error)", agentID, k)
			}
		}
	}
	if got := string(resp.Results["worker-1"]["status"]); got != `"ok"` {
		t.Errorf("worker-1 status = %s, want \"ok\"", got)
	}
	if got := string(resp.Results["worker-2"]["status"]); got != `"busy"` {
		t.Errorf("worker-2 status = %s, want \"busy\"", got)
	}
}
