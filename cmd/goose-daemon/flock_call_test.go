package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ephemera/internal/orchestrator"
)

// TestCallFlockAgent_LocalDispatchesToAgent covers the local-flock happy path:
// resolving agent_id via f.Agents, dispatching to the target VM's /tasks with
// the per-VM agent token injected, and returning the agent's response body.
// Depth: the incoming request carries no X-Ephemera-Task-Depth (absent → 0),
// so the agent must see "1" (0+1), matching proxyAgentEndpoint's accumulation.
func TestCallFlockAgent_LocalDispatchesToAgent(t *testing.T) {
	const agentToken = "at-1-DO-NOT-LEAK"
	var gotAuth, gotDepth string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDepth = r.Header.Get(taskDepthHeader)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"pong"}`))
	}))
	defer agent.Close()
	host, port := splitHostPort(t, agent.URL)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	cp := newTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "researcher-1", Role: "researcher", VMID: "vm-1", Status: orchestrator.AgentStatusReady})
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{VMID: "vm-1", GuestIP: host}, agentToken: agentToken}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/flock-1/call", strings.NewReader(`{"agent_id":"researcher-1","prompt":"ping"}`))
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("call status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "pong") {
		t.Fatalf("call body = %s, want it to contain \"pong\"", rr.Body.String())
	}
	if gotAuth != "Bearer "+agentToken {
		t.Fatalf("agent saw auth %q, want Bearer %s", gotAuth, agentToken)
	}
	if gotDepth != "1" {
		t.Fatalf("agent saw depth %q, want \"1\" (absent 0, +1)", gotDepth)
	}
}

// TestCallFlockAgent_UnknownAgent404 proves an agent_id absent from both the
// local roster and the flock's Agents map 404s, and that the error body never
// leaks a daemon address or bearer token — only agent/flock identifiers.
func TestCallFlockAgent_UnknownAgent404(t *testing.T) {
	const agentToken = "at-2-DO-NOT-LEAK"
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"should not be called"}`))
	}))
	defer agent.Close()
	host, port := splitHostPort(t, agent.URL)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	cp := newTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "researcher-1", Role: "researcher", VMID: "vm-1", Status: orchestrator.AgentStatusReady})
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{VMID: "vm-1", GuestIP: host}, agentToken: agentToken}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/flock-1/call", strings.NewReader(`{"agent_id":"ghost","prompt":"ping"}`))
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown agent status = %d, want 404 (%s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, leak := range []string{agentToken, host, agent.URL} {
		if strings.Contains(body, leak) {
			t.Fatalf("404 body leaked %q: %s", leak, body)
		}
	}
}

// TestCallFlockAgent_DepthLimit508 proves a local dispatch enforces the same
// nested-invocation depth cap proxyAgentEndpoint applies to /tasks: a request
// arriving at/over maxTaskDepth is refused 508 before the agent is contacted.
func TestCallFlockAgent_DepthLimit508(t *testing.T) {
	var calls int32
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"should not be called"}`))
	}))
	defer agent.Close()
	host, port := splitHostPort(t, agent.URL)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	oldMax := maxTaskDepth
	maxTaskDepth = 2
	defer func() { maxTaskDepth = oldMax }()

	cp := newTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "researcher-1", Role: "researcher", VMID: "vm-1", Status: orchestrator.AgentStatusReady})
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{VMID: "vm-1", GuestIP: host}}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/flock-1/call", strings.NewReader(`{"agent_id":"researcher-1","prompt":"ping"}`))
	req.Header.Set(taskDepthHeader, "2") // == cap
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusLoopDetected {
		t.Fatalf("depth at cap status = %d, want 508 (%s)", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("agent called %d times, want 0 (depth guard must short-circuit)", got)
	}
}

// TestCallFlockAgent_RelayForwardsToHome proves a relay flock forwards a call
// to its home daemon with: the per-flock call token (never the relay token),
// the hop marker set, the caller's depth propagated verbatim (not
// accumulated — accumulation happens only at the final local dispatch), and a
// body containing only {agent_id, prompt}.
func TestCallFlockAgent_RelayForwardsToHome(t *testing.T) {
	var gotPath, gotAuth, gotHop, gotDepth, gotBody string
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotHop = r.Header.Get(callHopHeader)
		gotDepth = r.Header.Get(taskDepthHeader)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"remote-pong"}`))
	}))
	defer home.Close()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "ct-1")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/call", strings.NewReader(`{"agent_id":"researcher-1","prompt":"ping"}`))
	req.Header.Set(taskDepthHeader, "2")
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("relay call status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "remote-pong") {
		t.Fatalf("relay call body = %s, want it to contain \"remote-pong\"", rr.Body.String())
	}
	if gotPath != "/flocks/routed-1/call" {
		t.Fatalf("home saw path %q, want /flocks/routed-1/call", gotPath)
	}
	if gotAuth != "Bearer ct-1" {
		t.Fatalf("home saw auth %q, want Bearer ct-1 (call token, not relay token)", gotAuth)
	}
	if gotHop != "1" {
		t.Fatalf("home saw hop header %q, want \"1\"", gotHop)
	}
	if gotDepth != "2" {
		t.Fatalf("home saw depth %q, want \"2\" (propagated verbatim, not accumulated)", gotDepth)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("home body not json: %v (%s)", err, gotBody)
	}
	if payload["agent_id"] != "researcher-1" || payload["prompt"] != "ping" {
		t.Fatalf("relayed body = %s, want only agent_id+prompt", gotBody)
	}
	if len(payload) != 2 {
		t.Fatalf("relayed body has extra fields: %s, want only agent_id+prompt", gotBody)
	}
}

// TestCallFlockAgent_RelayHonorsCallerContext mirrors
// TestPostToTownWall_RelayHonorsCallerContext in townwall_relay_test.go: once
// the caller's context is cancelled the relay hop must fail immediately with
// 502 rather than reaching a live (stub-200) home.
func TestCallFlockAgent_RelayHonorsCallerContext(t *testing.T) {
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"remote-pong"}`))
	}))
	defer home.Close()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "ct-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the relay hop

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/call", strings.NewReader(`{"agent_id":"researcher-1","prompt":"ping"}`)).WithContext(ctx)
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("relay call with cancelled caller context = %d, want 502 (relay did not honor r.Context())", rr.Code)
	}
}

// TestCallFlockAgent_HubSecondHopUsesRosterAddr proves a hub flock resolves an
// agent_id absent from any local VM to a roster member's Addr and forwards
// the call there (the second and final hop) with the call token and hop
// marker set, mirroring the home-routed 2-hop design.
func TestCallFlockAgent_HubSecondHopUsesRosterAddr(t *testing.T) {
	var gotPath, gotAuth, gotHop string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotHop = r.Header.Get(callHopHeader)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"hop2"}`))
	}))
	defer target.Close()

	cp := newTestCP(t)
	body := `{"roster":[{"agent_id":"remote-1","host":"host-b","vm_id":"vm-9","addr":"` + target.URL + `"}],"relay_token":"rt-1","call_token":"ct-1"}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/hub-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("distributed register status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/hub-1/call", strings.NewReader(`{"agent_id":"remote-1","prompt":"ping"}`))
	cp.handleFlockItem(rr2, req)

	if rr2.Code != http.StatusOK {
		t.Fatalf("hub 2nd-hop call status = %d, want 200 (%s)", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "hop2") {
		t.Fatalf("hub 2nd-hop call body = %s, want it to contain \"hop2\"", rr2.Body.String())
	}
	if gotPath != "/flocks/hub-1/call" {
		t.Fatalf("target saw path %q, want /flocks/hub-1/call", gotPath)
	}
	if gotAuth != "Bearer ct-1" {
		t.Fatalf("target saw auth %q, want Bearer ct-1", gotAuth)
	}
	if gotHop != "1" {
		t.Fatalf("target saw hop header %q, want \"1\"", gotHop)
	}
}

// TestCallFlockAgent_HopGuardNeverReforwards proves the loop guard: a request
// that already carries X-Ephemera-Call-Hop must never be re-forwarded, even
// by a hub with a matching roster Addr entry. Local resolution failing on a
// hopped request is an immediate 404, not a second forward.
func TestCallFlockAgent_HopGuardNeverReforwards(t *testing.T) {
	var calls int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"should not be called"}`))
	}))
	defer target.Close()

	cp := newTestCP(t)
	body := `{"roster":[{"agent_id":"remote-1","host":"host-b","vm_id":"vm-9","addr":"` + target.URL + `"}],"relay_token":"rt-1","call_token":"ct-1"}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/hub-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("distributed register status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/hub-1/call", strings.NewReader(`{"agent_id":"remote-1","prompt":"ping"}`))
	req.Header.Set(callHopHeader, "1")
	cp.handleFlockItem(rr2, req)

	if rr2.Code != http.StatusNotFound {
		t.Fatalf("hopped call to unresolved-locally agent = %d, want 404 (%s)", rr2.Code, rr2.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("target called %d times, want 0 (hop guard must never re-forward)", got)
	}
}

// TestCallFlockAgent_HubLocalTargetByVMRegistry proves a hub flock resolves a
// roster member whose VMID happens to live in THIS daemon's local VM registry
// by dispatching locally — even though the roster entry carries no Addr.
// Locality is decided purely by VM-registry presence, not by Addr.
func TestCallFlockAgent_HubLocalTargetByVMRegistry(t *testing.T) {
	const agentToken = "at-3-DO-NOT-LEAK"
	var gotAuth string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"local-hit"}`))
	}))
	defer agent.Close()
	host, port := splitHostPort(t, agent.URL)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	cp := newTestCP(t)
	cp.vms["vm-local"] = &runningVM{VMInfo: VMInfo{VMID: "vm-local", GuestIP: host}, agentToken: agentToken}

	body := `{"roster":[{"agent_id":"local-1","host":"host-a","vm_id":"vm-local"},{"agent_id":"remote-2","host":"host-c"}],"relay_token":"rt-1","call_token":"ct-1"}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/hub-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("distributed register status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/hub-1/call", strings.NewReader(`{"agent_id":"local-1","prompt":"ping"}`))
	cp.handleFlockItem(rr2, req)

	if rr2.Code != http.StatusOK {
		t.Fatalf("hub local-VM call status = %d, want 200 (%s)", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "local-hit") {
		t.Fatalf("hub local-VM call body = %s, want it to contain \"local-hit\"", rr2.Body.String())
	}
	if gotAuth != "Bearer "+agentToken {
		t.Fatalf("agent saw auth %q, want Bearer %s", gotAuth, agentToken)
	}
}
