package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"ephemera/internal/orchestrator"
	"ephemera/internal/storage"
)

// operatorBearer is the daemon's own control-plane bearer in these tests. Every
// assertion that a guest was NOT handed it compares against this exact value.
const operatorBearer = "operator-bearer-secret"

// cpTokenRecorder captures the ControlPlaneToken each spawn injected into its
// guest, keyed by VM id. It wraps the prepareVM seam spawnHarness installs, which
// is the last point the token is observable before it would be written to
// /root/.ephemera-cp-token inside the guest rootfs.
type cpTokenRecorder struct {
	mu   sync.Mutex
	byVM map[string]string
}

func (r *cpTokenRecorder) get(vmID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byVM[vmID]
}

func (r *cpTokenRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.byVM))
	for _, v := range r.byVM {
		out = append(out, v)
	}
	return out
}

// recordSpawnedCPTokens must be called AFTER spawnHarness, which installs the
// prepareVM stub this decorates.
func recordSpawnedCPTokens(cp *ControlPlane) *cpTokenRecorder {
	rec := &cpTokenRecorder{byVM: map[string]string{}}
	inner := cp.prepareVM
	cp.prepareVM = func(vmID string, opts storage.VMPrepareOptions) error {
		rec.mu.Lock()
		rec.byVM[vmID] = opts.ControlPlaneToken
		rec.mu.Unlock()
		if inner != nil {
			return inner(vmID, opts)
		}
		return nil
	}
	return rec
}

// authProbe drives the REAL authMiddleware chain and reports the status code, so
// a test isolates the admission decision from any handler behaviour downstream.
func authProbe(cp *ControlPlane) func(method, path, bearer string) int {
	handler := authMiddleware(cp.getClients, cp.relayTokenFor, cp.callTokenFor, nil,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	return func(method, path, bearer string) int {
		req := httptest.NewRequest(method, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
}

// createFlockViaHTTP drives the real POST /flocks handler and returns the new
// flock id. Requires spawnHarness to have been installed on cp.
func createFlockViaHTTP(t *testing.T, cp *ControlPlane, body string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	cp.createFlock(rr, httptest.NewRequest(http.MethodPost, "/flocks", strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /flocks = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	var out FlockCreateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode flock create response: %v", err)
	}
	return out.FlockID
}

// TestFlockCapabilityToken_AdmitsOnlyOwnGuestPaths is the positive half of the
// capability contract: a local flock's guest token opens exactly that flock's
// guest-scoped sub-paths (wall post / read / history and the call entry point)
// and nothing belonging to any other flock.
func TestFlockCapabilityToken_AdmitsOnlyOwnGuestPaths(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: operatorBearer}} // auth ENABLED

	tokA := cp.ensureLocalFlockGuestToken("flock-a")
	tokB := cp.ensureLocalFlockGuestToken("flock-b")
	if tokA == "" || tokB == "" {
		t.Fatalf("capability tokens not issued: a=%q b=%q", tokA, tokB)
	}
	if tokA == tokB {
		t.Fatal("two flocks were issued the same capability token")
	}
	if tokA == operatorBearer || tokB == operatorBearer {
		t.Fatal("capability token IS the operator bearer; the whole point is that it is not")
	}
	if len(tokA) != 64 {
		t.Fatalf("capability token length = %d, want 64 hex chars (32 random bytes)", len(tokA))
	}

	do := authProbe(cp)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/flocks/flock-a/post"},
		{http.MethodGet, "/flocks/flock-a/wall"},
		{http.MethodGet, "/flocks/flock-a/wall/history"},
		{http.MethodPost, "/flocks/flock-a/call"},
	} {
		if code := do(tc.method, tc.path, tokA); code != http.StatusOK {
			t.Errorf("%s %s with own capability token = %d, want admitted (200)", tc.method, tc.path, code)
		}
	}

	// Cross-flock isolation: flock-a's token must not open flock-b's guest paths.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/flocks/flock-b/post"},
		{http.MethodGet, "/flocks/flock-b/wall"},
		{http.MethodGet, "/flocks/flock-b/wall/history"},
		{http.MethodPost, "/flocks/flock-b/call"},
	} {
		if code := do(tc.method, tc.path, tokA); code != http.StatusUnauthorized {
			t.Errorf("%s %s with ANOTHER flock's capability token = %d, want 401", tc.method, tc.path, code)
		}
	}
}

// TestFlockCapabilityToken_RejectedOnControlPlaneRoutes is the core regression
// and the reason this change exists: the credential a flock guest holds must not
// reach any control-plane route. Each rejected route is also probed with the
// operator bearer, so a route that silently stopped existing cannot masquerade as
// a passing rejection.
func TestFlockCapabilityToken_RejectedOnControlPlaneRoutes(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: operatorBearer}}
	tok := cp.ensureLocalFlockGuestToken("flock-a")
	if tok == "" {
		t.Fatal("capability token not issued")
	}
	do := authProbe(cp)

	forbidden := []struct{ method, path string }{
		{http.MethodPost, "/vms"},
		{http.MethodGet, "/vms"},
		{http.MethodDelete, "/vms/vm-x"},
		{http.MethodPost, "/config/clients"},
		{http.MethodGet, "/config/profiles"},
		{http.MethodGet, "/tenants"},
		{http.MethodPost, "/tenants"},
		{http.MethodGet, "/snapshots"},
		{http.MethodDelete, "/flocks/flock-a"},
		{http.MethodPost, "/flocks/flock-a/agents"},
	}
	for _, tc := range forbidden {
		if code := do(tc.method, tc.path, tok); code != http.StatusUnauthorized {
			t.Errorf("%s %s with a flock capability token = %d, want 401", tc.method, tc.path, code)
		}
		if code := do(tc.method, tc.path, operatorBearer); code != http.StatusOK {
			t.Errorf("%s %s with the operator bearer = %d, want admitted — the rejection above proves nothing if this route is unreachable", tc.method, tc.path, code)
		}
	}
}

// TestOperatorBearerStillAdmittedAfterCapabilityTokens is the mid-upgrade
// guarantee: VMs spawned before this change hold the operator bearer, so that
// credential must keep being admitted on the guest paths they use. cp.clients
// admission is untouched by the capability-token store.
func TestOperatorBearerStillAdmittedAfterCapabilityTokens(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: operatorBearer}}
	cp.ensureLocalFlockGuestToken("flock-a")
	do := authProbe(cp)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/flocks/flock-a/post"},
		{http.MethodPost, "/flocks/flock-a/call"},
		{http.MethodPost, "/vms"},
	} {
		if code := do(tc.method, tc.path, operatorBearer); code != http.StatusOK {
			t.Errorf("%s %s with the operator bearer = %d, want admitted (pre-upgrade guests must keep working)", tc.method, tc.path, code)
		}
	}
	if code := do(http.MethodPost, "/flocks/flock-a/post", "not-a-real-token"); code != http.StatusUnauthorized {
		t.Errorf("garbage bearer = %d, want 401", code)
	}
}

// TestCreateFlock_IssuesPersistsAndInjectsCapabilityToken walks the whole create
// path: mint, register for admission, persist at 0600 in the flock's own
// directory, inject into the member guest — and never write it to metadata.json.
func TestCreateFlock_IssuesPersistsAndInjectsCapabilityToken(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: operatorBearer}}
	spawnHarness(t, cp)
	rec := recordSpawnedCPTokens(cp)

	flockID := createFlockViaHTTP(t, cp, `{"task":"review the diff","roles":["researcher"]}`)

	tok := cp.relayTokenFor(flockID)
	if tok == "" {
		t.Fatal("createFlock did not register a capability token for admission")
	}
	if tok == operatorBearer {
		t.Fatal("createFlock registered the operator bearer as the flock's guest token")
	}

	// Persisted, in its own file, at an explicit 0600.
	onDisk, err := orchestrator.LoadFlockGuestToken(cp.workDir, flockID)
	if err != nil {
		t.Fatalf("LoadFlockGuestToken: %v", err)
	}
	if onDisk != tok {
		t.Fatalf("persisted token %q != registered token %q", onDisk, tok)
	}
	fi, err := os.Stat(orchestrator.FlockGuestTokenPath(cp.workDir, flockID))
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("token file mode = %04o, want 0600", perm)
	}

	// Injected into the guest instead of the operator bearer.
	injected := rec.all()
	if len(injected) != 1 {
		t.Fatalf("recorded %d spawns, want 1: %v", len(injected), injected)
	}
	if injected[0] != tok {
		t.Fatalf("guest was injected %q, want the flock capability token %q", injected[0], tok)
	}
	if injected[0] == operatorBearer {
		t.Fatal("guest was injected the operator bearer")
	}

	// metadata.json still holds no secret.
	raw, err := os.ReadFile(cp.workDir + "/flocks/" + flockID + "/metadata.json")
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	if strings.Contains(string(raw), tok) {
		t.Fatalf("metadata.json leaked the capability token:\n%s", raw)
	}
}

// TestFlockVMSpawn_NotCPTokenManaged pins the flag that makes the whole change
// stick. A capability token is not the daemon's operator bearer, so the daemon
// must not record it as one — otherwise the next SIGHUP rotation overwrites the
// capability token with the operator bearer and silently undoes this change.
func TestFlockVMSpawn_NotCPTokenManaged(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: operatorBearer}}
	spawnHarness(t, cp)

	cp.ensureLocalFlockGuestToken("flock-1")
	info, _, err := cp.spawnVMForFlock("flock-1", "researcher-1", "researcher", "", "")
	if err != nil {
		t.Fatalf("spawnVMForFlock: %v", err)
	}

	cp.mu.RLock()
	managed := cp.vms[info.VMID].cpTokenManaged
	cp.mu.RUnlock()
	if managed {
		t.Fatal("flock VM cpTokenManaged = true; a per-flock capability token is not the daemon's bearer to rotate")
	}
	st, err := storage.LoadVMState(cp.workDir, info.VMID)
	if err != nil {
		t.Fatalf("LoadVMState: %v", err)
	}
	if st.CPTokenManaged {
		t.Fatal("persisted VMState.CPTokenManaged = true for a capability-token flock VM")
	}
}

// TestSIGHUP_DoesNotOverwriteFlockCapabilityToken is the end-to-end form of the
// same guarantee, driven through the real fan-out: a flock member spawned under
// the capability-token model must receive no SET_CP_TOKEN when the operator
// bearer rotates.
func TestSIGHUP_DoesNotOverwriteFlockCapabilityToken(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: operatorBearer}}
	spawnHarness(t, cp)
	dir := vsockTempDir(t)
	guest := startFakeGuestVsock(t, dir, "flockmember")

	tok := cp.ensureLocalFlockGuestToken("flock-1")
	info, _, err := cp.spawnVMForFlock("flock-1", "researcher-1", "researcher", "", "")
	if err != nil {
		t.Fatalf("spawnVMForFlock: %v", err)
	}
	// Point the registered VM at the fake guest listener; spawnVMInternal derives
	// its real vsock path from the VM id, which no listener is bound to here.
	cp.mu.Lock()
	cp.vms[info.VMID].vsockPath = guest.path
	cp.mu.Unlock()

	cp.propagateCPTokenToVMs([]APIClient{{Name: "ops", Token: "ROTATED-operator-bearer"}})

	if got := guest.commands(); len(got) != 0 {
		t.Fatalf("flock member received %v on SIGHUP; its capability token must not be overwritten with the operator bearer", got)
	}
	if cp.relayTokenFor("flock-1") != tok {
		t.Fatalf("capability token changed across the rotation: %q -> %q", tok, cp.relayTokenFor("flock-1"))
	}
}

// TestSIGHUP_StillRotatesPreExistingManagedVM is the non-disruptive-upgrade
// guard. A VM spawned before this change holds the operator bearer and carries
// CPTokenManaged=true in its state.json; after the upgrade it must go on
// receiving rotations, or an operator token rotation would silently strand it.
// The propagation set then drains on its own as those VMs are replaced.
func TestSIGHUP_StillRotatesPreExistingManagedVM(t *testing.T) {
	cp := newTestCP(t)
	dir := vsockTempDir(t)
	legacy := startFakeGuestVsock(t, dir, "legacy")

	// Exactly what recovery reads back for a pre-upgrade local flock member.
	cp.registerRecoveredVM(storage.VMState{
		VMID:           "vm-legacy",
		GuestIP:        "10.0.1.9",
		AgentURL:       "http://10.0.1.9:8080",
		VsockPath:      legacy.path,
		CPTokenManaged: true,
	}, nil, nil)

	cp.propagateCPTokenToVMs([]APIClient{{Name: "ops", Token: "ROTATED-operator-bearer"}})

	got := legacy.commands()
	if len(got) != 1 || got[0] != "SET_CP_TOKEN ROTATED-operator-bearer" {
		t.Fatalf("pre-upgrade managed VM commands = %v, want exactly [SET_CP_TOKEN ROTATED-operator-bearer]", got)
	}
}

// TestDeleteFlock_RevokesCapabilityToken proves revocation is complete: in-memory
// admission is dropped AND the on-disk token is removed, so a restart cannot
// rehydrate a token for a flock that no longer exists.
func TestDeleteFlock_RevokesCapabilityToken(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: operatorBearer}}
	spawnHarness(t, cp)

	flockID := createFlockViaHTTP(t, cp, `{"task":"review the diff","roles":["researcher"]}`)
	tok := cp.relayTokenFor(flockID)
	if tok == "" {
		t.Fatal("no capability token issued")
	}

	// Drop the member VMs from the registry before deleting. spawnHarness fakes
	// startMachine with a nil *firecracker.Machine, which destroyVM cannot stop;
	// an absent VM instead takes deleteFlock's existing "vm already absent" branch
	// (the path a flock recovered from a previous daemon process takes anyway), so
	// the teardown under test is the token revocation, not Firecracker.
	cp.mu.Lock()
	cp.vms = map[string]*runningVM{}
	cp.mu.Unlock()

	rr := httptest.NewRecorder()
	cp.deleteFlock(rr, flockID)
	if rr.Code != http.StatusOK {
		t.Fatalf("deleteFlock = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}

	if got := cp.relayTokenFor(flockID); got != "" {
		t.Fatalf("relayTokenFor after delete = %q, want empty (admission not revoked)", got)
	}
	if _, err := os.Stat(orchestrator.FlockGuestTokenPath(cp.workDir, flockID)); !os.IsNotExist(err) {
		t.Fatalf("token file survived flock deletion (stat err = %v)", err)
	}
	if code := authProbe(cp)(http.MethodPost, "/flocks/"+flockID+"/post", tok); code != http.StatusUnauthorized {
		t.Fatalf("deleted flock's token still admits its wall post = %d, want 401", code)
	}
}

// TestFlockCapabilityToken_RehydratedAcrossDaemonRestart closes the restart hole.
// cp.relayTokens is in-memory only and starts empty every process; a local flock
// has no external reconcile driver to refill it (routed flocks do). Without an
// explicit rehydration step the next member spawn after a restart would inject an
// EMPTY token and the guest's wall/call calls would 401 — a regression against
// the operator-bearer behaviour this change replaces, because that source was
// re-read from configuration on every start and survived restart for free.
func TestFlockCapabilityToken_RehydratedAcrossDaemonRestart(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: operatorBearer}}
	spawnHarness(t, cp)
	flockID := createFlockViaHTTP(t, cp, `{"task":"review the diff","roles":["researcher"]}`)
	tok := cp.relayTokenFor(flockID)
	if tok == "" {
		t.Fatal("no capability token issued")
	}

	// --- daemon restart --- (restartCP mirrors the api.go boot sequence)
	cp2 := restartCP(t, cp)

	if got := cp2.relayTokenFor(flockID); got != tok {
		t.Fatalf("relayTokenFor after restart = %q, want the persisted capability token %q", got, tok)
	}
	if code := authProbe(cp2)(http.MethodPost, "/flocks/"+flockID+"/post", tok); code != http.StatusOK {
		t.Fatalf("capability token admission not restored after restart: %d, want 200", code)
	}

	// A member joined after the restart must be injected the REAL token, not "".
	spawnHarness(t, cp2)
	rec := recordSpawnedCPTokens(cp2)
	rr := httptest.NewRecorder()
	cp2.addFlockAgent(rr, httptest.NewRequest(http.MethodPost, "/flocks/"+flockID+"/agents",
		strings.NewReader(`{"role":"researcher"}`)), flockID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("addFlockAgent after restart = %d, want 201 (%s)", rr.Code, rr.Body.String())
	}
	injected := rec.all()
	if len(injected) != 1 {
		t.Fatalf("recorded %d spawns, want 1: %v", len(injected), injected)
	}
	if injected[0] == "" {
		t.Fatal("member joined after restart was injected an EMPTY control-plane token; rehydration did not run")
	}
	if injected[0] != tok {
		t.Fatalf("member joined after restart was injected %q, want the flock capability token %q", injected[0], tok)
	}
}

// TestFlockCapabilityToken_AuthDisabledInjectsNothing preserves today's
// development-mode behaviour exactly. With no API clients configured,
// authMiddleware admits every request unconditionally, so a capability token
// would authenticate nothing; the operator-bearer source this replaces likewise
// returned "" in that mode and the provisioner wrote no cp-token file.
func TestFlockCapabilityToken_AuthDisabledInjectsNothing(t *testing.T) {
	cp := newTestCP(t) // no clients -> auth DISABLED
	spawnHarness(t, cp)
	rec := recordSpawnedCPTokens(cp)

	if tok := cp.ensureLocalFlockGuestToken("flock-1"); tok != "" {
		t.Fatalf("capability token issued with auth disabled: %q", tok)
	}
	if got := cp.controlPlaneTokenForVM(); got != "" {
		t.Fatalf("baseline controlPlaneTokenForVM() with auth disabled = %q, want empty", got)
	}
	info, _, err := cp.spawnVMForFlock("flock-1", "researcher-1", "researcher", "", "")
	if err != nil {
		t.Fatalf("spawnVMForFlock: %v", err)
	}
	if got := rec.get(info.VMID); got != "" {
		t.Fatalf("guest injected %q with auth disabled, want empty (no cp-token file is written)", got)
	}
	if _, err := os.Stat(orchestrator.FlockGuestTokenPath(cp.workDir, "flock-1")); !os.IsNotExist(err) {
		t.Fatalf("token file written with auth disabled (stat err = %v)", err)
	}
}
