package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ephemera/internal/orchestrator"
	"ephemera/internal/storage"
)

// restartCP simulates a daemon process restart: a fresh ControlPlane rooted at
// the SAME workDir as cp, whose new FlockManager has already run LoadFromDisk —
// mirroring the api.go boot sequence (LoadFromDisk before RecoverVMs). In-memory
// admission (relayTokens/callTokens) starts empty, exactly as a real process
// restart drops it. Auth is enabled (a non-empty clients list) so relay-token
// admission through authMiddleware is meaningful.
func restartCP(t *testing.T, cp *ControlPlane) *ControlPlane {
	t.Helper()
	cp2 := &ControlPlane{
		clients:          []APIClient{{Name: "op", Token: "op-tok"}},
		vms:              make(map[string]*runningVM),
		relayTokens:      map[string]string{},
		callTokens:       map[string]string{},
		snapshots:        make(map[string]storage.SnapshotMetadata),
		workDir:          cp.workDir,
		gooseConfigPath:  cp.gooseConfigPath,
		gooseSecretsPath: cp.gooseSecretsPath,
		flockMgr:         orchestrator.NewFlockManager(cp.workDir),
		agentHTTPClient:  &http.Client{Timeout: time.Second},
	}
	cp2.metrics = newDaemonMetrics(cp2)
	if _, _, err := cp2.flockMgr.LoadFromDisk(); err != nil {
		t.Fatalf("restart LoadFromDisk: %v", err)
	}
	// The boot sequence re-registers recovered LOCAL flocks' persisted guest
	// capability tokens right after LoadFromDisk; a restart simulation that
	// skipped it would not reproduce what the daemon actually does.
	cp2.rehydrateFlockGuestTokens()
	return cp2
}

// TestRegisterDistributedFlock_SurvivesDaemonRestart reproduces D1 on the home
// host: a hub flock plus a home-placed member VM. Before the fix RegisterHub did
// not persist the hub kind, so after restart LoadFromDisk found no hub metadata
// and RecoverVMs (reconcileRecoveredFlockAgent) recreated the flock as a LOCAL
// flock — making the reconcile re-POST /distributed hit the non-hub guard (409)
// and permanently 401ing the relay hop. With the fix the hub kind survives, the
// re-POST heals (201), the restored relay-token admission lets a relay hop post
// to the canonical wall (200), and the Town Wall history is preserved.
func TestRegisterDistributedFlock_SurvivesDaemonRestart(t *testing.T) {
	cp := newTestCP(t)
	body := `{"roster":[{"agent_id":"researcher-1","host":"home","vm_id":"vm-home-1"}],"relay_token":"rt-1","call_token":"ct-1"}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("initial distributed register = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	hub, ok := cp.flockMgr.Get("routed-1")
	if !ok {
		t.Fatal("hub flock not registered")
	}
	if _, err := hub.TownWall.Post("researcher-1", "pre-restart-message"); err != nil {
		t.Fatalf("seed wall: %v", err)
	}

	// --- daemon restart ---
	cp2 := restartCP(t, cp)
	// RecoverVMs reattaches the home-placed member VM. This is exactly where the
	// pre-fix downgrade struck: with no persisted hub metadata the flock is absent
	// after LoadFromDisk, so reconcile recreates it as a local flock.
	cp2.reconcileRecoveredFlockAgent(storage.VMState{
		FlockID:   "routed-1",
		AgentID:   "researcher-1",
		VMID:      "vm-home-1",
		CreatedAt: time.Now().UTC(),
	}, "http://10.0.0.1:8080")

	// Reconcile re-POST /distributed must heal (201), NOT 409.
	rr2 := httptest.NewRecorder()
	cp2.handleFlockItem(rr2, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("post-restart distributed re-POST = %d, want 201 (409 = D1 downgrade) (%s)", rr2.Code, rr2.Body.String())
	}
	f2, ok := cp2.flockMgr.Get("routed-1")
	if !ok || f2.Kind != orchestrator.FlockKindHub {
		t.Fatalf("recovered flock is not a hub: %+v", f2)
	}

	// Admission restored: a relay hop with the relay token posts to the wall (200).
	handler := authMiddleware(cp2.getClients, cp2.relayTokenFor, cp2.callTokenFor, nil,
		http.HandlerFunc(cp2.handleFlockItem))
	wr := httptest.NewRecorder()
	preq := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/post",
		strings.NewReader(`{"agent_id":"researcher-1","body":"post-restart-message"}`))
	preq.Header.Set("Authorization", "Bearer rt-1")
	handler.ServeHTTP(wr, preq)
	if wr.Code != http.StatusOK {
		t.Fatalf("relay-token wall post after restart = %d, want 200 (admission not restored) (%s)", wr.Code, wr.Body.String())
	}

	// Town Wall history preserved across the restart (append mode).
	hist, err := f2.TownWall.History()
	if err != nil {
		t.Fatalf("read wall history: %v", err)
	}
	var seenPre, seenPost bool
	for _, m := range hist {
		switch m.Body {
		case "pre-restart-message":
			seenPre = true
		case "post-restart-message":
			seenPost = true
		}
	}
	if !seenPre || !seenPost {
		t.Fatalf("history missing messages: pre=%v post=%v (%d msgs)", seenPre, seenPost, len(hist))
	}
}

// TestRegisterRelayFlock_SurvivesDaemonRestart reproduces D1 on a member host: a
// relay flock plus a local member VM. Before the fix RegisterRelay did not
// persist the relay kind/home_addr, so after restart LoadFromDisk found nothing
// and RecoverVMs recreated the flock as LOCAL — making the reconcile re-POST
// /relay hit the non-relay guard (409). With the fix the relay kind + home_addr
// survive LoadFromDisk, so reconcile keeps the flock a relay and the re-POST
// heals (201).
func TestRegisterRelayFlock_SurvivesDaemonRestart(t *testing.T) {
	cp := newTestCP(t)
	body := `{"home_addr":"http://hostA:3000","relay_token":"rt-1","call_token":"ct-1"}`
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("initial relay register = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	// --- daemon restart ---
	cp2 := restartCP(t, cp)
	// The relay identity must be restored from disk even before the reconcile
	// re-POST — this is the LoadFromDisk restoration the fix adds.
	restored, ok := cp2.flockMgr.Get("routed-1")
	if !ok || restored.Kind != orchestrator.FlockKindRelay || restored.HomeAddr != "http://hostA:3000" {
		t.Fatalf("relay identity not restored from disk: %+v (ok=%v)", restored, ok)
	}
	// RecoverVMs reattaches the local member VM (the pre-fix downgrade point).
	cp2.reconcileRecoveredFlockAgent(storage.VMState{
		FlockID:   "routed-1",
		AgentID:   "worker-1",
		VMID:      "vm-local-1",
		CreatedAt: time.Now().UTC(),
	}, "http://10.0.0.2:8080")

	// Reconcile re-POST /relay must heal (201), NOT 409, and keep the relay kind.
	rr2 := httptest.NewRecorder()
	cp2.handleFlockItem(rr2, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/relay", strings.NewReader(body)))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("post-restart relay re-POST = %d, want 201 (409 = D1 downgrade) (%s)", rr2.Code, rr2.Body.String())
	}
	f2, ok := cp2.flockMgr.Get("routed-1")
	if !ok || f2.Kind != orchestrator.FlockKindRelay || f2.HomeAddr != "http://hostA:3000" {
		t.Fatalf("recovered flock is not a relay: %+v", f2)
	}
}

// TestRegisterDistributedFlock_StillRefusesGenuineLocalFlock proves the fix did
// not weaken the guard: a genuine local flock recovered across a restart (kind
// still "") must keep refusing POST /distributed with 409, so a stray
// distributed registration can never hijack a local flock's wall.
func TestRegisterDistributedFlock_StillRefusesGenuineLocalFlock(t *testing.T) {
	cp := newTestCP(t)
	if _, err := cp.flockMgr.Create("local-1", "local-task",
		filepath.Join(cp.workDir, "flocks", "local-1", "TOWN_WALL.log")); err != nil {
		t.Fatalf("seed local flock: %v", err)
	}
	f, ok := cp.flockMgr.Get("local-1")
	if !ok {
		t.Fatal("local flock not registered")
	}
	if err := f.Persist(cp.workDir); err != nil {
		t.Fatalf("persist local flock: %v", err)
	}

	cp2 := restartCP(t, cp)
	recovered, ok := cp2.flockMgr.Get("local-1")
	if !ok || recovered.Kind != orchestrator.FlockKindLocal {
		t.Fatalf("local flock not recovered as local: %+v (ok=%v)", recovered, ok)
	}

	rr := httptest.NewRecorder()
	body := `{"roster":[{"agent_id":"researcher-1","host":"hostB"}],"relay_token":"rt-1"}`
	cp2.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/local-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("distributed over recovered local flock = %d, want 409 (%s)", rr.Code, rr.Body.String())
	}
	if cp2.relayTokenFor("local-1") != "" {
		t.Fatalf("rejected distributed register admitted a relay token")
	}
}

// TestRegisterDistributedFlock_RefillsHubTokensAfterRestart covers D1b: after a
// home daemon restart, LoadFromDisk recovers the hub flock WITHOUT its tokens
// (admission secrets are never persisted), so f.RelayToken/f.CallToken come back
// empty. The idempotent reconcile re-POST /distributed refills the INBOUND
// admission maps (setRelayToken/setCallToken) and roster, but must ALSO re-seat
// the hub Flock struct's own tokens — those drive the OUTBOUND hops. Without it
// the hub's 2nd-hop /call to a member sends an empty bearer and the member
// daemon 401s. Asserts the struct fields, the end-to-end outbound bearer, and
// re-confirms tokens never touch disk.
func TestRegisterDistributedFlock_RefillsHubTokensAfterRestart(t *testing.T) {
	const relayTok = "RT-SENT-d1b-aa11"
	const callTok = "CT-SENT-d1b-bb22"

	// Stub member daemon: the target of the hub's outbound 2nd hop. It captures
	// the bearer so we can prove the hub forwarded a non-empty call token.
	var gotAuth string
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer member.Close()

	body := `{"roster":[{"agent_id":"target-1","host":"host-b","vm_id":"vm-9","addr":"` + member.URL +
		`"}],"relay_token":"` + relayTok + `","call_token":"` + callTok + `"}`
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleFlockItem(rr, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("initial distributed register = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}

	// --- daemon restart --- LoadFromDisk recovers the hub; tokens are gone.
	cp2 := restartCP(t, cp)
	if pre, ok := cp2.flockMgr.Get("routed-1"); !ok || pre.RelayToken != "" || pre.CallToken != "" {
		t.Fatalf("precondition: recovered hub should have empty tokens, got relay=%q call=%q (ok=%v)",
			pre.RelayToken, pre.CallToken, ok)
	}

	// Reconcile re-POST /distributed (idempotent heal).
	rr2 := httptest.NewRecorder()
	cp2.handleFlockItem(rr2, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/distributed", strings.NewReader(body)))
	if rr2.Code != http.StatusCreated {
		t.Fatalf("post-restart distributed re-POST = %d, want 201 (%s)", rr2.Code, rr2.Body.String())
	}

	// (a) The hub Flock struct's own tokens are re-seated.
	f2, ok := cp2.flockMgr.Get("routed-1")
	if !ok {
		t.Fatal("hub flock missing after re-POST")
	}
	if f2.RelayToken != relayTok {
		t.Fatalf("hub f.RelayToken = %q, want %q (not refilled on idempotent re-POST)", f2.RelayToken, relayTok)
	}
	if f2.CallToken != callTok {
		t.Fatalf("hub f.CallToken = %q, want %q (not refilled → member 401 = D1b)", f2.CallToken, callTok)
	}

	// (b) Outbound proof: the hub's 2nd-hop /call to the member carries callTok.
	callRR := httptest.NewRecorder()
	cp2.handleFlockItem(callRR, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/call",
		strings.NewReader(`{"agent_id":"target-1","prompt":"ping"}`)))
	if callRR.Code != http.StatusOK {
		t.Fatalf("hub 2nd-hop call = %d, want 200 (%s)", callRR.Code, callRR.Body.String())
	}
	if gotAuth != "Bearer "+callTok {
		t.Fatalf("member received auth %q, want %q (empty bearer = D1b)", gotAuth, "Bearer "+callTok)
	}

	// (c) Tokens still never persisted.
	raw, err := os.ReadFile(filepath.Join(cp2.workDir, "flocks", "routed-1", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if strings.Contains(string(raw), relayTok) || strings.Contains(string(raw), callTok) {
		t.Fatalf("metadata.json leaked a token:\n%s", raw)
	}
}
