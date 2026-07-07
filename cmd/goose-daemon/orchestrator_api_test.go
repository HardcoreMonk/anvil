package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ephemera/internal/orchestrator"
)

// TestReserveAgent covers the atomic per-role "<role>-N" id allocation used by
// addFlockAgent (v0.4.3): next = max(existing N for that role) + 1, a role with
// no existing agents starts at 1, and max_agents is enforced before spawn.
func TestReserveAgent(t *testing.T) {
	f := &orchestrator.Flock{Agents: map[string]*orchestrator.AgentInfo{
		"worker-1":   {AgentID: "worker-1", Role: "worker"},
		"worker-3":   {AgentID: "worker-3", Role: "worker"},
		"reviewer-1": {AgentID: "reviewer-1", Role: "reviewer"},
	}}
	cases := map[string]string{
		"worker":   "worker-4", // max(1,3)+1
		"reviewer": "reviewer-2",
		"builder":  "builder-1", // none yet
	}
	for role, want := range cases {
		got, err := f.ReserveAgent(role, 10)
		if err != nil {
			t.Fatalf("ReserveAgent(%q): %v", role, err)
		}
		if got != want {
			t.Errorf("ReserveAgent(%q) = %q, want %q", role, got, want)
		}
	}
	if _, err := f.ReserveAgent("worker", len(f.Snapshot())); err == nil {
		t.Fatal("ReserveAgent over cap returned nil error")
	}
}

// TestFlockMax covers the per-flock cap fallback (v0.4.3): 0/unset → default,
// otherwise the flock's own MaxAgents.
func TestFlockMax(t *testing.T) {
	f := &orchestrator.Flock{}
	if got := flockMax(f); got != defaultMaxAgentsPerFlock {
		t.Errorf("flockMax(unset) = %d, want %d", got, defaultMaxAgentsPerFlock)
	}
	f.MaxAgents = 5
	if got := flockMax(f); got != 5 {
		t.Errorf("flockMax(5) = %d, want 5", got)
	}
}

func TestFlockAddAgentResponseOmitsAgentTokenFields(t *testing.T) {
	resp := FlockAddAgentResponse{
		AgentID:  "worker-2",
		Role:     "worker",
		VMID:     "vm-added",
		AgentURL: "http://127.0.0.1:8080",
	}
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusCreated, resp)

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response JSON did not decode: %v", err)
	}
	for _, key := range []string{"agent_id", "role", "vm_id", "agent_url"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("response missing %q: %s", key, rr.Body.String())
		}
	}
	for _, key := range []string{"agent_token", "agent_tokens"} {
		if _, ok := body[key]; ok {
			t.Fatalf("response exposed %q: %s", key, rr.Body.String())
		}
	}
}

// TestFilterTownWall covers the v0.4.3 wall/history query filters.
func TestFilterTownWall(t *testing.T) {
	msgs := []orchestrator.Message{
		{AgentID: "worker-1", Timestamp: "2026-05-28T10:00:00Z", Body: "hello world"},
		{AgentID: "worker-2", Timestamp: "2026-05-28T11:00:00Z", Body: "goodbye"},
		{AgentID: "worker-1", Timestamp: "2026-05-28T12:00:00Z", Body: "world again"},
	}
	if got, err := filterTownWall(msgs, "", "", "", ""); err != nil || len(got) != 3 {
		t.Errorf("no filter: got %d, want 3", len(got))
	}
	if got, err := filterTownWall(msgs, "worker-1", "", "", ""); err != nil || len(got) != 2 {
		t.Errorf("agent_id: got %d, want 2", len(got))
	}
	if got, err := filterTownWall(msgs, "", "", "", "world"); err != nil || len(got) != 2 {
		t.Errorf("contains: got %d, want 2", len(got))
	}
	if got, err := filterTownWall(msgs, "", "2026-05-28T10:30:00Z", "2026-05-28T11:30:00Z", ""); err != nil || len(got) != 1 {
		t.Errorf("since/until: got %d, want 1", len(got))
	}
	if got, err := filterTownWall(msgs, "worker-1", "", "", "again"); err != nil || len(got) != 1 {
		t.Errorf("agent_id+contains: got %d, want 1", len(got))
	}
	if got, err := filterTownWall(msgs, "", "2026-05-28T06:30:00-04:00", "2026-05-28T07:30:00-04:00", ""); err != nil || len(got) != 1 {
		t.Errorf("since/until with offset: got %d, err=%v, want 1", len(got), err)
	}
	if _, err := filterTownWall(msgs, "", "not-a-time", "", ""); err == nil {
		t.Fatal("invalid since returned nil error")
	}
}

// TestRelayToken_AdmitsOnlyWallPaths proves a per-flock relay token is admitted
// by authMiddleware ONLY for that flock's Town Wall sub-paths and is NEVER a
// general control-plane bearer. The four 401 assertions are the load-bearing
// proof the privilege escalation is closed: registering relay token "rt-1" for
// flock "routed-1" must NOT let "Bearer rt-1" reach /vms, DELETE /vms/{id},
// /config/clients, or another flock's wall — only routed-1's own wall post.
func TestRelayToken_AdmitsOnlyWallPaths(t *testing.T) {
	cp := &ControlPlane{
		clients:     []APIClient{{Name: "op", Token: "op-tok"}}, // auth ENABLED
		relayTokens: map[string]string{},
	}
	cp.setRelayToken("routed-1", "rt-1")
	// Drive the REAL authMiddleware chain (relayTokenFor wired), matching the
	// production apiChain wrap; a 200 downstream isolates the middleware decision.
	handler := authMiddleware(cp.getClients, cp.relayTokenFor, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func(method, path, authz string) int {
		req := httptest.NewRequest(method, path, nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// ALLOWED: the flock's own wall post with its relay token.
	if code := do(http.MethodPost, "/flocks/routed-1/post", "Bearer rt-1"); code == http.StatusUnauthorized {
		t.Errorf("own wall post with relay token = 401, want admitted")
	}

	// REJECTED (401): relay token on admin routes and other flocks.
	rejected := []struct{ method, path string }{
		{http.MethodPost, "/vms"},
		{http.MethodDelete, "/vms/vm-x"},
		{http.MethodPost, "/config/clients"},
		{http.MethodPost, "/flocks/other-flock/post"},
	}
	for _, tc := range rejected {
		if code := do(tc.method, tc.path, "Bearer rt-1"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with relay token = %d, want 401", tc.method, tc.path, code)
		}
	}
}

// TestRegisterDistributedAndRelayFlock covers the two daemon-to-daemon
// endpoints the anvil control plane calls when creating a cross-host routed
// flock (v0.7.x): POST /flocks/{id}/distributed on the home daemon registers
// the hub that owns the canonical Town Wall, and POST /flocks/{id}/relay on
// each member daemon registers a relay stub pointing back at the home daemon.
func TestRegisterDistributedAndRelayFlock(t *testing.T) {
	cp := newTestCP(t)

	// distributed (hub) on the "home" daemon
	rr := httptest.NewRecorder()
	body := `{"roster":[{"agent_id":"researcher-1","host":"hostB"}],"relay_token":"rt-1"}`
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("distributed status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	f, ok := cp.flockMgr.Get("routed-1")
	if !ok || f.Kind != orchestrator.FlockKindHub || f.TownWall == nil {
		t.Fatalf("hub flock not registered with wall")
	}

	// relay on a "member" daemon
	cp2 := newTestCP(t)
	rr2 := httptest.NewRecorder()
	body2 := `{"home_addr":"http://hostA:3000","relay_token":"rt-1"}`
	cp2.handleFlockItem(rr2, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(body2)))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("relay status = %d, want 201", rr2.Code)
	}
	rf, ok := cp2.flockMgr.Get("routed-1")
	if !ok || rf.Kind != orchestrator.FlockKindRelay || rf.HomeAddr != "http://hostA:3000" {
		t.Fatalf("relay flock not registered")
	}
}

// TestRegisterDistributedFlock_RejectsDuplicateNonHubID proves a POST /distributed
// for a flock id already registered under a non-hub kind (here a local flock) is
// rejected with 409 Conflict instead of silently overwriting the existing flock
// via RegisterHub's unconditional fm.flocks[id]=f assignment. The pre-existing
// local flock (and its wall) must survive untouched.
func TestRegisterDistributedFlock_RejectsDuplicateNonHubID(t *testing.T) {
	cp := newTestCP(t)
	local, err := cp.flockMgr.Create("routed-1", "local-task", filepath.Join(cp.workDir, "flocks", "routed-1", "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("seed local flock: %v", err)
	}

	rr := httptest.NewRecorder()
	body := `{"roster":[{"agent_id":"researcher-1","host":"hostB"}],"relay_token":"rt-1"}`
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("distributed over local flock = %d, want 409 (%s)", rr.Code, rr.Body.String())
	}
	f, ok := cp.flockMgr.Get("routed-1")
	if !ok || f != local || f.Kind != orchestrator.FlockKindLocal || f.Task != "local-task" {
		t.Fatalf("local flock mutated by rejected distributed register: %+v", f)
	}
	if cp.relayTokenFor("routed-1") != "" {
		t.Fatalf("rejected distributed register admitted relay token %q", cp.relayTokenFor("routed-1"))
	}
}

// TestRegisterRelayFlock_RejectsDuplicateNonRelayID proves a POST /relay for a
// flock id already registered under a non-relay kind (here a local flock) is
// rejected with 409 Conflict rather than overwriting it, while a relay->relay
// re-register still succeeds and refreshes HomeAddr/token (reconcile heal).
func TestRegisterRelayFlock_RejectsDuplicateNonRelayID(t *testing.T) {
	cp := newTestCP(t)
	local, err := cp.flockMgr.Create("routed-1", "local-task", filepath.Join(cp.workDir, "flocks", "routed-1", "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("seed local flock: %v", err)
	}

	rr := httptest.NewRecorder()
	body := `{"home_addr":"http://hostA:3000","relay_token":"rt-1"}`
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(body)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("relay over local flock = %d, want 409 (%s)", rr.Code, rr.Body.String())
	}
	f, ok := cp.flockMgr.Get("routed-1")
	if !ok || f != local || f.Kind != orchestrator.FlockKindLocal {
		t.Fatalf("local flock mutated by rejected relay register: %+v", f)
	}

	// relay -> relay re-registration is still allowed: reconcile heal refreshes the
	// HomeAddr/token on the existing relay stub.
	cp2 := newTestCP(t)
	rr2 := httptest.NewRecorder()
	cp2.handleFlockItem(rr2, httptest.NewRequest(http.MethodPost, "/flocks/routed-2/relay", strings.NewReader(`{"home_addr":"http://hostA:3000","relay_token":"rt-1"}`)))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("initial relay register = %d, want 201 (%s)", rr2.Code, rr2.Body.String())
	}
	rr3 := httptest.NewRecorder()
	cp2.handleFlockItem(rr3, httptest.NewRequest(http.MethodPost, "/flocks/routed-2/relay", strings.NewReader(`{"home_addr":"http://hostA:3001","relay_token":"rt-2"}`)))
	if rr3.Code != http.StatusCreated {
		t.Fatalf("relay->relay re-register = %d, want 201 (reconcile heal) (%s)", rr3.Code, rr3.Body.String())
	}
	rf, ok := cp2.flockMgr.Get("routed-2")
	if !ok || rf.HomeAddr != "http://hostA:3001" || rf.RelayToken != "rt-2" {
		t.Fatalf("relay re-register did not refresh HomeAddr/token: %+v", rf)
	}
}

// TestDeleteFlock_RevokesRelayToken covers Task 8 rollback deregistration on the
// daemon side: deleting a hub flock (DELETE /flocks/{id}) must also strip its
// scoped relay-token admission, so a stale relay token can no longer authenticate
// a wall hop after the routed flock is gone.
func TestDeleteFlock_RevokesRelayToken(t *testing.T) {
	cp := newTestCP(t)

	body := `{"roster":[{"agent_id":"researcher-1","host":"hostB"}],"relay_token":"rt-1"}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("distributed status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	if cp.relayTokenFor("routed-1") != "rt-1" {
		t.Fatalf("relay token not admitted on hub registration")
	}

	rrDel := httptest.NewRecorder()
	cp.handleFlockItem(rrDel, httptest.NewRequest(http.MethodDelete, "/flocks/routed-1", nil))
	if rrDel.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (%s)", rrDel.Code, rrDel.Body.String())
	}
	if got := cp.relayTokenFor("routed-1"); got != "" {
		t.Fatalf("relay token after flock delete = %q, want \"\" (token revoked)", got)
	}
	if _, ok := cp.flockMgr.Get("routed-1"); ok {
		t.Fatalf("hub flock still present after delete")
	}
}

// TestRegisterDistributedFlock_ReAdmitsRelayTokenOnReRegister covers the
// reconcile heal path (Task 7): a re-POST /distributed for an already-registered
// hub flock must restore the scoped relay-token admission even when it was
// cleared (e.g. by a daemon process restart, which drops the in-memory
// cp.relayTokens map) while the hub flock itself survived in flockMgr. Without
// this, reconcile would re-issue the hub POST but leave the relay hop
// unauthenticated.
func TestRegisterDistributedFlock_ReAdmitsRelayTokenOnReRegister(t *testing.T) {
	cp := newTestCP(t)

	body := `{"roster":[{"agent_id":"researcher-1","host":"hostB"}],"relay_token":"rt-1"}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("initial distributed status = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	if cp.relayTokenFor("routed-1") != "rt-1" {
		t.Fatalf("relay token not admitted on initial registration")
	}

	// Simulate the relay-token admission being lost while the hub flock survives.
	cp.removeRelayToken("routed-1")
	if cp.relayTokenFor("routed-1") != "" {
		t.Fatalf("relay token not cleared for test setup")
	}
	if _, ok := cp.flockMgr.Get("routed-1"); !ok {
		t.Fatalf("hub flock should still exist after clearing only the relay token")
	}

	// Reconcile re-POSTs /distributed for the existing hub flock: admission restored.
	rr2 := httptest.NewRecorder()
	cp.handleFlockItem(rr2, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("re-register distributed status = %d, want 201 (%s)", rr2.Code, rr2.Body.String())
	}
	if got := cp.relayTokenFor("routed-1"); got != "rt-1" {
		t.Fatalf("relay token after re-register = %q, want rt-1 (admission not restored)", got)
	}
}
