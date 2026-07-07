package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
// NO hop marker (2026-07-08 C1 fix — home is a resolver, not a terminus, and
// must remain free to take its own 2nd hop; setting the marker here made
// every home 2nd hop 404 unconditionally), the caller's depth propagated
// verbatim (not accumulated — accumulation happens only at the final local
// dispatch), and a body containing only {agent_id, prompt}.
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
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "ct-1", nil)

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
	if gotHop != "" {
		t.Fatalf("home saw hop header %q, want \"\" (unmarked — home must remain free to take its own 2nd hop)", gotHop)
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
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "ct-1", nil)

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

// TestCallFlockAgent_HoppedResolvesLocallyOnRelay proves the 2026-07-08 design
// correction (Task 3 review finding): the real cross-host topology has the
// TARGET member daemon see its own flock as relay-kind (it forwards its own
// guests' wall/call traffic to home), so a hopped call — home's 2nd hop —
// landing on this relay flock must resolve against the host-local agent list
// registered via POST /flocks/{id}/relay's "agents" field, not 404
// unconditionally. The home stub must see zero calls: a hopped request must
// never be forwarded again (loop guard holds even though this daemon knows a
// HomeAddr).
func TestCallFlockAgent_HoppedResolvesLocallyOnRelay(t *testing.T) {
	const agentToken = "at-4-DO-NOT-LEAK"
	var homeCalls int32
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&homeCalls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"should not be called"}`))
	}))
	defer home.Close()

	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"local-relay-hit"}`))
	}))
	defer agent.Close()
	host, port := splitHostPort(t, agent.URL)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	cp := newTestCP(t)
	cp.vms["vm-local"] = &runningVM{VMInfo: VMInfo{VMID: "vm-local", GuestIP: host}, agentToken: agentToken}

	relayBody := `{"home_addr":"` + home.URL + `","relay_token":"rt-1","call_token":"ct-1","agents":[{"agent_id":"local-1","vm_id":"vm-local"}]}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(relayBody)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("relay register status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/call", strings.NewReader(`{"agent_id":"local-1","prompt":"ping"}`))
	req.Header.Set(callHopHeader, "1")
	cp.handleFlockItem(rr2, req)

	if rr2.Code != http.StatusOK {
		t.Fatalf("hopped relay-local call status = %d, want 200 (%s)", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "local-relay-hit") {
		t.Fatalf("hopped relay-local call body = %s, want it to contain \"local-relay-hit\"", rr2.Body.String())
	}
	if got := atomic.LoadInt32(&homeCalls); got != 0 {
		t.Fatalf("home called %d times, want 0 (hopped request must never forward)", got)
	}
}

// TestCallFlockAgent_HoppedUnknownAgent404NoForward mirrors the previous test
// with an agent_id absent from the relay's host-local agents list: the local
// resolution failure is still an immediate 404 (identifiers-only error), and
// the home stub still sees zero calls — "hopped → local-only" does not
// degrade into "hopped → forward anyway on miss".
func TestCallFlockAgent_HoppedUnknownAgent404NoForward(t *testing.T) {
	var homeCalls int32
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&homeCalls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"should not be called"}`))
	}))
	defer home.Close()

	cp := newTestCP(t)
	relayBody := `{"home_addr":"` + home.URL + `","relay_token":"rt-1","call_token":"ct-1","agents":[{"agent_id":"local-1","vm_id":"vm-local"}]}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(relayBody)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("relay register status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/call", strings.NewReader(`{"agent_id":"ghost","prompt":"ping"}`))
	req.Header.Set(callHopHeader, "1")
	cp.handleFlockItem(rr2, req)

	if rr2.Code != http.StatusNotFound {
		t.Fatalf("hopped unknown-agent status = %d, want 404 (%s)", rr2.Code, rr2.Body.String())
	}
	if got := atomic.LoadInt32(&homeCalls); got != 0 {
		t.Fatalf("home called %d times, want 0 (hopped request must never forward)", got)
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

// TestCallFlockAgent_MemberToRemoteMemberFullChain is the synthesis
// regression test for the 2026-07-08 C1 fix (Task 3 final review): a member
// daemon (relay flock, non-hopped inbound call) forwards to a REAL hub
// ControlPlane — full HTTP server, real authMiddleware chain, call_token
// admission exercised over the wire, not bypassed — which then takes its own
// 2nd/final hop to a remote roster target. Before the fix, forwardFlockCall
// unconditionally set the hop marker on the relay->home leg, so the hub
// received an already-hopped request, resolved only locally, and 404'd
// instead of reaching the target: this test's target handler firing at all
// is the load-bearing proof the member->home leg is unmarked (an indirect
// check — the hub is a real ControlPlane, not a stub, so its internal hop
// decision isn't directly observable, but a marked inbound request can never
// produce a call to the target per the `hopped` branch in callFlockAgent).
func TestCallFlockAgent_MemberToRemoteMemberFullChain(t *testing.T) {
	const callToken = "ct-chain-1"
	var targetHop, targetAuth, targetBody string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHop = r.Header.Get(callHopHeader)
		targetAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		targetBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":"chain-ok"}`))
	}))
	defer target.Close()

	// Hub: a REAL ControlPlane with its production HTTP handler chain
	// (audit + authMiddleware + routes), listening on a real httptest server
	// so the relay's outbound forwardFlockCall is a genuine network hop, not a
	// direct method call — call_token admission at the hub is exercised via
	// authMiddleware exactly as it is in production.
	hubCP := newTestControlPlaneWithHandler(t)
	hub := httptest.NewServer(hubCP.srv.Handler)
	defer hub.Close()

	distBody := `{"roster":[{"agent_id":"target-1","host":"host-b","vm_id":"vm-9","addr":"` + target.URL + `"}],"relay_token":"rt-chain-1","call_token":"` + callToken + `"}`
	distReq, err := http.NewRequest(http.MethodPost, hub.URL+"/flocks/hub-chain/distributed", strings.NewReader(distBody))
	if err != nil {
		t.Fatalf("build distributed register request: %v", err)
	}
	distReq.Header.Set("Authorization", "Bearer secret-token") // hubCP's operator token (newTestControlPlaneWithHandler)
	distResp, err := http.DefaultClient.Do(distReq)
	if err != nil {
		t.Fatalf("register hub distributed: %v", err)
	}
	defer distResp.Body.Close()
	if distResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(distResp.Body)
		t.Fatalf("distributed register status = %d, want 201 (%s)", distResp.StatusCode, b)
	}

	// Member (relay): a separate daemon, registered as relay-kind pointing
	// HomeAddr at the hub's real server, sharing the SAME call_token the hub
	// just registered for "hub-chain" (in production the anvil control plane
	// issues the same call_token to both the member and the hub).
	relayCP := newTestCP(t)
	relayCP.flockMgr.RegisterRelay("hub-chain", hub.URL, "rt-chain-1", callToken, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/hub-chain/call", strings.NewReader(`{"agent_id":"target-1","prompt":"ping"}`))
	relayCP.handleFlockItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("member->hub->target chain status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "chain-ok") {
		t.Fatalf("chain response body = %s, want it to contain \"chain-ok\"", rr.Body.String())
	}
	// The target is the FINAL hop: it must see the hop marker set (hub's 2nd
	// hop is markHop=true) and the call token as bearer.
	if targetHop != "1" {
		t.Fatalf("target saw hop header %q, want \"1\" (hub's 2nd/final hop must mark it)", targetHop)
	}
	if targetAuth != "Bearer "+callToken {
		t.Fatalf("target saw auth %q, want Bearer %s", targetAuth, callToken)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(targetBody), &payload); err != nil {
		t.Fatalf("target body not json: %v (%s)", err, targetBody)
	}
	if payload["agent_id"] != "target-1" || payload["prompt"] != "ping" {
		t.Fatalf("target body = %s, want only agent_id+prompt", targetBody)
	}
	if len(payload) != 2 {
		t.Fatalf("target body has extra fields: %s, want only agent_id+prompt", targetBody)
	}
}

// TestCallFlockAgent_RelayRetryExhausted502 proves a relay call to a
// permanently-unreachable HomeAddr (closed port — dial fails on every
// attempt) exhausts all bounded retries and still returns 502, with the
// error body free of the daemon address and both tokens (redaction — an
// exhausted-retry error must not become a new leak surface), within one
// wall-clock test run (relayRetrySleep swapped for a no-sleep fake so the
// real 1s/2s backoff never elapses). A closed port gives client.Do no way to
// distinguish attempts from the outside, so the sleep-hook's own call count
// is the attempt-count witness: relayRetryAttempts-1 backoffs between
// relayRetryAttempts dial attempts.
func TestCallFlockAgent_RelayRetryExhausted502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserved listener (guarantee every attempt dial-fails): %v", err)
	}

	var sleepCalls int32
	oldSleep := relayRetrySleep
	relayRetrySleep = func(ctx context.Context, d time.Duration) error {
		atomic.AddInt32(&sleepCalls, 1)
		return noSleep(ctx, d)
	}
	defer func() { relayRetrySleep = oldSleep }()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", "http://"+addr, "rt-1", "ct-1", nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/call", strings.NewReader(`{"agent_id":"researcher-1","prompt":"ping"}`))

	start := time.Now()
	cp.handleFlockItem(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("relay call to permanently-closed home = %d, want 502 (%s)", rr.Code, rr.Body.String())
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("relay retry exhaustion took %v, want < 5s (relayRetrySleep must be the no-sleep fake)", elapsed)
	}
	if got := atomic.LoadInt32(&sleepCalls); got != int32(relayRetryAttempts-1) {
		t.Fatalf("relayRetrySleep called %d times, want %d (relayRetryAttempts-1 backoffs across relayRetryAttempts dial attempts)", got, relayRetryAttempts-1)
	}
	body := rr.Body.String()
	for _, leak := range []string{addr, "rt-1", "ct-1"} {
		if strings.Contains(body, leak) {
			t.Fatalf("502 body leaked %q: %s", leak, body)
		}
	}
}
