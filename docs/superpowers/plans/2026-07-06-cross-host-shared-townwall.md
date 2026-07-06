# Cross-host Shared Town Wall Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let flock members spawned across multiple hosts (routed flock) post to and observe one shared Town Wall, via a home-host hub with daemon-to-daemon relay, without changing the guest's bridge-only isolation.

**Architecture:** One host is the flock's `home` and owns the canonical `TownWall` (a `hub` flock in its `FlockManager`). Member hosts register a `relay` flock that forwards `/flocks/{id}/post` to the home daemon and proxies `/wall`/`/wall/history` reads, authenticated by a per-flock `relay_token`. Guests are unchanged: they post to their local daemon at `10.0.1.1:3000`, and the local daemon does the relay.

**Tech Stack:** Go (module `ephemera`), `net/http`, `httptest` for unit tests, anvil `internal/anvilmcp` control-plane + `cmd/goose-daemon` runtime. Design spec: `docs/superpowers/specs/2026-07-06-cross-host-shared-townwall-design.md`.

## Global Constraints

- Scope is **shared Town Wall only** — no cross-host gtcall, no cross-host broadcast fan-out.
- **Guests stay bridge-only**: no guest reaches a daemon other than its local `10.0.1.1:3000`. Relay is daemon-to-daemon.
- **relay_token** is per-flock, daemon-to-daemon only; relayed payload is `{agent_id, body}` ONLY — never a per-VM `agent_token` or a peer CP token. `relay_token` is persisted in `PlacementStore` but **redacted from every serialized output, MCP output, audit record, and log line** (same discipline as `agent_token`).
- The Town Wall stays **runtime/operator surface**: no new `anvil_*` MCP tool is added; the existing IronClaw schema-exclusion guards must still pass.
- New daemon endpoints (`/flocks/{id}/distributed`, `/flocks/{id}/relay`) go on the daemon's **internalMux behind `authMiddleware`** (control-plane bearer), never on the auth-exempt external routes.
- `home` host selection is deterministic: **the host where `roles[0]` (first requested role) is placed**.
- No existing anvil test may be removed or weakened. TDD (RED→GREEN) for every behavior change. No git trailers on commits (branch convention).
- Preserve the existing `local` flock path (single-host flocks) byte-for-byte in behavior.

---

## File Structure

- `internal/anvilmcp/daemon_client.go` — extend `SpawnVMRequest`; add `RegisterDistributedFlock`/`RegisterRelayFlock` client methods + request types.
- `internal/anvilmcp/routed_flock.go` — home selection, relay_token generation, hub+relay registration, spawn-with-identity, flip `TownWallEnabled`, rollback deregistration, post/history delegation.
- `internal/anvilmcp/placement_store.go` — `RoutedFlockRecord.HomeHost` + `relayToken` (unexported/redacted).
- `internal/anvilmcp/runtime_router.go` — reconcile re-registration of hub/relay flocks.
- `internal/orchestrator/flock.go` — `Flock.Kind` + relay fields; `FlockManager` register helpers for hub/relay.
- `cmd/goose-daemon/api.go` — accept `flock_id`/`agent_id`/`control_plane_token` on `POST /vms`.
- `cmd/goose-daemon/orchestrator_api.go` — `/flocks/{id}/distributed`, `/flocks/{id}/relay` routes; relay branch in `postToTownWall`/`townWallHistory`/`streamTownWall`; a `relayClient` helper.
- `scripts/anvil-cross-host-wall-e2e.sh` — KVM e2e (real member daemon + stub home).
- Tests: `internal/anvilmcp/routed_flock_test.go`, `internal/anvilmcp/daemon_client_test.go`, `cmd/goose-daemon/orchestrator_api_test.go`, `cmd/goose-daemon/townwall_relay_test.go` (new), `internal/orchestrator/flock_test.go`.

---

## Task 1: Extend the spawn contract to carry flock identity

**Files:**
- Modify: `internal/anvilmcp/daemon_client.go` (`SpawnVMRequest`)
- Modify: `cmd/goose-daemon/api.go` (`POST /vms` request struct + spawn options wiring)
- Test: `cmd/goose-daemon/api_test.go`

**Interfaces:**
- Produces: `SpawnVMRequest{ Profile, TenantID, EgressPolicy string; FlockID, AgentID, ControlPlaneToken string }` — later tasks set the three new fields on routed spawn.

- [ ] **Step 1: Write the failing test** — assert `POST /vms` with `flock_id`/`agent_id`/`control_plane_token` reaches the provisioner as flock-injection options.

In `cmd/goose-daemon/api_test.go` add (adapt to the file's existing `newTestCP` helper and injection-capture seam — search for an existing test that captures `VMPrepareOptions`, e.g. a test that sets `cp.provisioner` or a spawn seam; assert on the captured `opts`):

```go
func TestSpawnVM_AcceptsFlockIdentity(t *testing.T) {
	cp := newTestCP(t)
	var captured storage.VMPrepareOptions
	cp.prepareVMFiles = func(mnt string, opts storage.VMPrepareOptions) error {
		captured = opts
		return nil
	}
	body := `{"profile":"researcher","flock_id":"routed-flock-1","agent_id":"researcher-1","control_plane_token":"cp-tok-xyz"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(body))
	cp.spawnVM(rr, req)

	if captured.FlockID != "routed-flock-1" {
		t.Fatalf("FlockID = %q, want routed-flock-1", captured.FlockID)
	}
	if captured.AgentID != "researcher-1" {
		t.Fatalf("AgentID = %q, want researcher-1", captured.AgentID)
	}
	if captured.ControlPlaneToken != "cp-tok-xyz" {
		t.Fatalf("ControlPlaneToken not threaded to provisioner")
	}
}
```

If `cp.prepareVMFiles` is not an existing seam, use the seam the file already has for injecting the provisioner (grep `api_test.go` for how existing spawn tests stub file injection); the assertion target is that the daemon threads the three fields into the same `VMPrepareOptions` the single-host `spawnVMForFlock` path already populates.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/goose-daemon -run TestSpawnVM_AcceptsFlockIdentity -count=1`
Expected: FAIL — the `POST /vms` request struct has no `flock_id`/`agent_id`/`control_plane_token` fields, so `captured` is empty.

- [ ] **Step 3: Add fields to the client struct**

In `internal/anvilmcp/daemon_client.go`, extend `SpawnVMRequest`:

```go
type SpawnVMRequest struct {
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
	// Cross-host flock identity (routed flock members). When set, the daemon
	// injects .ephemera-flock and .ephemera-cp-token so gtwall works in-VM.
	FlockID           string `json:"flock_id,omitempty"`
	AgentID           string `json:"agent_id,omitempty"`
	ControlPlaneToken string `json:"control_plane_token,omitempty"`
}
```

- [ ] **Step 4: Thread the fields through the daemon `POST /vms` handler**

In `cmd/goose-daemon/api.go`, find the `POST /vms` request decode struct (grep for the struct decoded in `spawnVM`; it has `profile`/`tenant_id`/`egress_policy`). Add the three fields and pass them into the same `spawnVMOptions`/`VMPrepareOptions` that `spawnVMForFlock` uses (grep `orchestrator_api.go:558-561,790-793` for the exact option field names `FlockID`, `AgentID`, `ControlPlaneToken`). Set them from the request:

```go
// inside the POST /vms decode struct
FlockID           string `json:"flock_id,omitempty"`
AgentID           string `json:"agent_id,omitempty"`
ControlPlaneToken string `json:"control_plane_token,omitempty"`
```

```go
// where spawnVMOptions{...} is built in spawnVM:
opts.FlockID = req.FlockID
opts.AgentID = req.AgentID
opts.ControlPlaneToken = req.ControlPlaneToken
```

Reuse the exact option field names already consumed by `provisioner.go:288-314`. Do not add new provisioner logic — the injection already exists gated on these fields.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./cmd/goose-daemon -run TestSpawnVM_AcceptsFlockIdentity -count=1`
Expected: PASS.

- [ ] **Step 6: Run the daemon package to check nothing regressed**

Run: `go test ./cmd/goose-daemon -count=1`
Expected: `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/anvilmcp/daemon_client.go cmd/goose-daemon/api.go cmd/goose-daemon/api_test.go
git commit -m "feat(runtime): accept flock identity on POST /vms for routed members"
```

---

## Task 2: Flock kind model (local / hub / relay) in the daemon

**Files:**
- Modify: `internal/orchestrator/flock.go` (`Flock` struct + `FlockManager` register helpers)
- Test: `internal/orchestrator/flock_test.go`

**Interfaces:**
- Produces:
  - `Flock.Kind string` with constants `FlockKindLocal = ""`, `FlockKindHub = "hub"`, `FlockKindRelay = "relay"`.
  - `Flock.HomeAddr string`, `Flock.RelayToken string` (set only for relay flocks).
  - `func (fm *FlockManager) RegisterHub(flockID string, wall *TownWall, roster []RosterMember, relayToken string) *Flock`
  - `func (fm *FlockManager) RegisterRelay(flockID, homeAddr, relayToken string) *Flock`
  - `type RosterMember struct { AgentID, Host string }`

- [ ] **Step 1: Write the failing test**

In `internal/orchestrator/flock_test.go`:

```go
func TestFlockManager_RegisterHubAndRelay(t *testing.T) {
	fm := NewFlockManager()
	wall, err := NewTownWall("routed-1", filepath.Join(t.TempDir(), "TOWN_WALL.log"))
	if err != nil {
		t.Fatal(err)
	}
	hub := fm.RegisterHub("routed-1", wall, []RosterMember{{AgentID: "researcher-1", Host: "hostB"}}, "relay-tok")
	if hub.Kind != FlockKindHub {
		t.Fatalf("hub.Kind = %q, want %q", hub.Kind, FlockKindHub)
	}
	if got, ok := fm.Get("routed-1"); !ok || got.TownWall == nil {
		t.Fatalf("hub flock not registered with a wall")
	}

	fm2 := NewFlockManager()
	relay := fm2.RegisterRelay("routed-1", "http://hostA:3000", "relay-tok")
	if relay.Kind != FlockKindRelay || relay.HomeAddr != "http://hostA:3000" || relay.RelayToken != "relay-tok" {
		t.Fatalf("relay flock fields wrong: %+v", relay)
	}
	if relay.TownWall != nil {
		t.Fatalf("relay flock must not own a wall")
	}
}
```

(If the manager constructor is named differently, grep `flock.go` for how `FlockManager` is created and how `fm.flocks[...]` is populated; reuse that.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/orchestrator -run TestFlockManager_RegisterHubAndRelay -count=1`
Expected: FAIL — `Kind`, `RegisterHub`, `RegisterRelay`, `RosterMember` undefined.

- [ ] **Step 3: Add the kind fields to `Flock`**

In `internal/orchestrator/flock.go`, add to the `Flock` struct (after `TownWall`):

```go
	// Kind distinguishes single-host flocks ("" / local) from cross-host
	// routed-flock roles: "hub" owns the canonical wall on the home host;
	// "relay" forwards posts to the home host. Local flocks are unchanged.
	Kind string `json:"-"`
	// HomeAddr and RelayToken are set only on relay flocks: the home daemon
	// base URL and the per-flock daemon-to-daemon relay token.
	HomeAddr   string `json:"-"`
	RelayToken string `json:"-"`
	// Roster lists remote members for a hub flock (informational; the hub owns
	// no local VMs for those agents).
	Roster []RosterMember `json:"-"`
```

Add near the top of the file:

```go
const (
	FlockKindLocal = ""
	FlockKindHub   = "hub"
	FlockKindRelay = "relay"
)

// RosterMember records a remote member of a cross-host (hub) flock.
type RosterMember struct {
	AgentID string `json:"agent_id"`
	Host    string `json:"host"`
}
```

- [ ] **Step 4: Add the register helpers**

Add to `internal/orchestrator/flock.go` (match the locking/field style used by the existing register path around `fm.flocks[f.ID] = f`):

```go
// RegisterHub registers a hub flock that owns the canonical Town Wall on the
// home host. It has no local member VMs; roster is the remote membership.
func (fm *FlockManager) RegisterHub(flockID string, wall *TownWall, roster []RosterMember, relayToken string) *Flock {
	f := &Flock{
		ID:         flockID,
		Kind:       FlockKindHub,
		TownWall:   wall,
		Roster:     roster,
		RelayToken: relayToken,
		Agents:     map[string]*AgentInfo{},
		CreatedAt:  time.Now().UTC(),
	}
	fm.mu.Lock()
	fm.flocks[flockID] = f
	fm.mu.Unlock()
	return f
}

// RegisterRelay registers a relay flock on a member host. It owns no wall;
// posts and reads are forwarded to homeAddr with relayToken.
func (fm *FlockManager) RegisterRelay(flockID, homeAddr, relayToken string) *Flock {
	f := &Flock{
		ID:         flockID,
		Kind:       FlockKindRelay,
		HomeAddr:   homeAddr,
		RelayToken: relayToken,
		Agents:     map[string]*AgentInfo{},
		CreatedAt:  time.Now().UTC(),
	}
	fm.mu.Lock()
	fm.flocks[flockID] = f
	fm.mu.Unlock()
	return f
}
```

(Use the exact mutex field name the manager uses — grep `flock.go` for `fm.mu` or the lock guarding `fm.flocks`.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/orchestrator -run TestFlockManager_RegisterHubAndRelay -count=1`
Expected: PASS.

- [ ] **Step 6: Run the package**

Run: `go test ./internal/orchestrator -count=1`
Expected: `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/orchestrator/flock.go internal/orchestrator/flock_test.go
git commit -m "feat(runtime): add hub/relay flock kinds for cross-host town wall"
```

---

## Task 3: Home-daemon hub registration + member-daemon relay registration endpoints

**Files:**
- Modify: `cmd/goose-daemon/orchestrator_api.go` (routes + two handlers)
- Test: `cmd/goose-daemon/orchestrator_api_test.go`

**Interfaces:**
- Produces:
  - `POST /flocks/{id}/distributed` body `{"roster":[{"agent_id","host"}],"relay_token":"..."}` → registers a hub flock (creates a `TownWall` under `flocks/{id}/TOWN_WALL.log`).
  - `POST /flocks/{id}/relay` body `{"home_addr":"http://host:3000","relay_token":"..."}` → registers a relay flock.
  - Request types `distributedFlockRequest`, `relayFlockRequest` in orchestrator_api.go.

- [ ] **Step 1: Write the failing test**

In `cmd/goose-daemon/orchestrator_api_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/goose-daemon -run TestRegisterDistributedAndRelayFlock -count=1`
Expected: FAIL — routes `distributed`/`relay` return 404.

- [ ] **Step 3: Add the routes**

In `cmd/goose-daemon/orchestrator_api.go` `handleFlockItem`, add two cases in the `switch` (alongside `wall`, `post`, etc.):

```go
	case sub == "distributed" && r.Method == http.MethodPost:
		cp.registerDistributedFlock(w, r, flockID)
	case sub == "relay" && r.Method == http.MethodPost:
		cp.registerRelayFlock(w, r, flockID)
```

- [ ] **Step 4: Add the handlers**

Add to `cmd/goose-daemon/orchestrator_api.go`:

```go
type distributedFlockRequest struct {
	Roster     []orchestrator.RosterMember `json:"roster"`
	RelayToken string                      `json:"relay_token"`
}

type relayFlockRequest struct {
	HomeAddr   string `json:"home_addr"`
	RelayToken string `json:"relay_token"`
}

// registerDistributedFlock (home host) creates the canonical wall for a
// cross-host routed flock. Idempotent: re-registration reuses the existing wall.
func (cp *ControlPlane) registerDistributedFlock(w http.ResponseWriter, r *http.Request, flockID string) {
	var req distributedFlockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if req.RelayToken == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("relay_token required"))
		return
	}
	if existing, ok := cp.flockMgr.Get(flockID); ok && existing.Kind == orchestrator.FlockKindHub {
		w.WriteHeader(http.StatusCreated)
		return
	}
	wallPath := filepath.Join(cp.workDir, "flocks", flockID, "TOWN_WALL.log")
	wall, err := orchestrator.NewTownWall(flockID, wallPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	cp.flockMgr.RegisterHub(flockID, wall, req.Roster, req.RelayToken)
	// Admit the relay hop through authMiddleware: register relay_token as an
	// accepted bearer on this (home) daemon, scoped for later deregistration.
	// authMiddleware is the transport gate; the hub post handler additionally
	// checks bearer == flock.RelayToken so a valid-but-wrong-flock token is
	// rejected (Task 4). If the daemon runs auth-disabled, only the hub check
	// applies. Reuse the daemon's accepted-token set (grep api.go for how
	// apiClients / accepted tokens are stored) and add/remove the relay_token.
	cp.addAcceptedRelayToken(flockID, req.RelayToken)
	w.WriteHeader(http.StatusCreated)
}

// registerRelayFlock (member host) registers a forward-to-home stub. Idempotent.
func (cp *ControlPlane) registerRelayFlock(w http.ResponseWriter, r *http.Request, flockID string) {
	var req relayFlockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if req.HomeAddr == "" || req.RelayToken == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("home_addr and relay_token required"))
		return
	}
	cp.flockMgr.RegisterRelay(flockID, req.HomeAddr, req.RelayToken)
	w.WriteHeader(http.StatusCreated)
}
```

(Confirm `cp.workDir` is the field name used at `orchestrator_api.go:181` for the wall path; reuse it verbatim.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./cmd/goose-daemon -run TestRegisterDistributedAndRelayFlock -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/goose-daemon/orchestrator_api.go cmd/goose-daemon/orchestrator_api_test.go
git commit -m "feat(runtime): hub/relay flock registration endpoints"
```

---

## Task 4: Relay branch in post/history/stream handlers

**Files:**
- Modify: `cmd/goose-daemon/orchestrator_api.go` (`postToTownWall`, `townWallHistory`, `streamTownWall`, new `relayClient`)
- Test: `cmd/goose-daemon/townwall_relay_test.go` (new)

**Interfaces:**
- Consumes: `Flock.Kind`, `Flock.HomeAddr`, `Flock.RelayToken` (Task 2); the two registration endpoints (Task 3).
- Produces: relay behavior — a `relay` flock's `POST /flocks/{id}/post` forwards `{agent_id, body}` to `{HomeAddr}/flocks/{id}/post` with header `Authorization: Bearer {RelayToken}`; `/wall/history` proxies GET to home.

- [ ] **Step 1: Write the failing test** — a relay flock forwards the post to a stub home; the stub receives `{agent_id, body}` and the relay token, and NO per-VM token.

Create `cmd/goose-daemon/townwall_relay_test.go`:

```go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostToTownWall_RelayForwardsToHome(t *testing.T) {
	var gotAuth, gotBody string
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"seq":1,"agent_id":"researcher-1","body":"hi"}`))
	}))
	defer home.Close()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/post", strings.NewReader(`{"agent_id":"researcher-1","body":"hi"}`))
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("relay post status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer rt-1" {
		t.Fatalf("home saw auth %q, want Bearer rt-1", gotAuth)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("home body not json: %v", err)
	}
	if payload["agent_id"] != "researcher-1" || payload["body"] != "hi" {
		t.Fatalf("relayed body = %s, want only agent_id+body", gotBody)
	}
	if _, leaked := payload["agent_token"]; leaked {
		t.Fatalf("relay leaked agent_token")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/goose-daemon -run TestPostToTownWall_RelayForwardsToHome -count=1`
Expected: FAIL — `postToTownWall` calls `f.TownWall.Post` and NPEs / 500s because a relay flock has a nil `TownWall`.

- [ ] **Step 3: Add the relay client helper**

Add to `cmd/goose-daemon/orchestrator_api.go`:

```go
// relayTownWallPost forwards a member's post to the home daemon that owns the
// canonical wall. Only {agent_id, body} crosses the wire; the per-flock relay
// token authenticates the daemon-to-daemon hop. No per-VM credential is sent.
func relayTownWallPost(homeAddr, relayToken, flockID, agentID, body string) (int, []byte, error) {
	payload, _ := json.Marshal(map[string]string{"agent_id": agentID, "body": body})
	url := strings.TrimRight(homeAddr, "/") + "/flocks/" + flockID + "/post"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayToken)
	resp, err := newAgentHTTPClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, nil
}
```

(Reuse `newAgentHTTPClient()` — the `DisableKeepAlives` client from `api.go:389` — for the same stale-connection safety as agent proxying. Add `bytes`/`io` imports if missing.)

- [ ] **Step 4: Add the relay branch to `postToTownWall`**

In `postToTownWall`, right after the `cp.flockMgr.Get` lookup succeeds, before the `f.TownWall.Post` call, add:

```go
	if f.Kind == orchestrator.FlockKindRelay {
		var req TownWallPostRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if req.AgentID == "" || req.Body == "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("agent_id and body required"))
			return
		}
		status, respBody, err := relayTownWallPost(f.HomeAddr, f.RelayToken, flockID, req.AgentID, req.Body)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, fmt.Errorf("town wall relay to home failed: %w", err))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
		return
	}
```

For the `hub` path (a relayed post arriving at the home daemon), before calling `f.TownWall.Post`, validate the flock-scoped token so a valid-transport-but-wrong-flock caller is rejected:

```go
	if f.Kind == orchestrator.FlockKindHub {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if f.RelayToken != "" && bearer != f.RelayToken {
			writeJSONError(w, http.StatusForbidden, fmt.Errorf("relay token does not match flock"))
			return
		}
	}
```

Leave the existing `local` path (which calls `f.TownWall.Post`) unchanged below this branch. (`hub` still falls through to `f.TownWall.Post` after the token check — same local-wall write.)

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./cmd/goose-daemon -run TestPostToTownWall_RelayForwardsToHome -count=1`
Expected: PASS.

- [ ] **Step 6: Add the history proxy branch + test**

Add a test `TestTownWallHistory_RelayProxiesToHome` (mirror Step 1: stub home returns a JSON array on `GET /flocks/routed-1/wall/history`; assert the relay returns the same array and sends `Bearer rt-1`). Then in `townWallHistory`, after the `Get`, add:

```go
	if f.Kind == orchestrator.FlockKindRelay {
		url := strings.TrimRight(f.HomeAddr, "/") + "/flocks/" + flockID + "/wall/history"
		if raw := r.URL.RawQuery; raw != "" {
			url += "?" + raw
		}
		hreq, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err)
			return
		}
		hreq.Header.Set("Authorization", "Bearer "+f.RelayToken)
		resp, err := newAgentHTTPClient().Do(hreq)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err)
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
		return
	}
```

(SSE `streamTownWall` relay proxy: add a `relay` branch that dials `{HomeAddr}/flocks/{id}/wall` and copies the stream with `Flusher` per-chunk — mirror the existing flush loop in `streamTownWall`. Post + history are the load-bearing paths for this scope; write the SSE branch the same way and add a focused test that the relay dials home with the token. If SSE proxying proves large, note it and keep post+history as the gate-critical paths.)

- [ ] **Step 7: Run the daemon package**

Run: `go test ./cmd/goose-daemon -count=1`
Expected: `ok`.

- [ ] **Step 8: Commit**

```bash
git add cmd/goose-daemon/orchestrator_api.go cmd/goose-daemon/townwall_relay_test.go
git commit -m "feat(runtime): relay town wall post/history to home daemon"
```

---

## Task 5: anvil orchestration — home selection, registration, spawn-with-identity

**Files:**
- Modify: `internal/anvilmcp/daemon_client.go` (add `RegisterDistributedFlock`/`RegisterRelayFlock` methods + request types; `Daemon` interface)
- Modify: `internal/anvilmcp/placement_store.go` (`RoutedFlockRecord.HomeHost` + redacted `relayToken`)
- Modify: `internal/anvilmcp/routed_flock.go` (`CreateRoutedFlockMembers`)
- Test: `internal/anvilmcp/routed_flock_test.go`

**Interfaces:**
- Consumes: Task 1 `SpawnVMRequest` fields; Task 3 endpoints.
- Produces: after `CreateRoutedFlockMembers`, `RoutedFlockRecord.HomeHost` is `roles[0]`'s host, `TownWallEnabled == true`, the home daemon has a hub flock and each member daemon a relay flock, and each member VM was spawned with `FlockID/AgentID/ControlPlaneToken`.

- [ ] **Step 1: Write the failing test** — use the existing routed_flock_test scaffolding (fake daemons per host). Assert home selection + registration calls + spawn identity.

In `internal/anvilmcp/routed_flock_test.go` (adapt to the file's existing fake-daemon test harness — grep for how `r.daemons` is stubbed and how `CreateRoutedFlockMembers` is currently tested):

```go
func TestCreateRoutedFlockMembers_WiresSharedTownWall(t *testing.T) {
	// two fake host daemons recording spawn + registration calls
	home := newFakeDaemon("hostA")
	member := newFakeDaemon("hostB")
	r := newTestRouterWithDaemons(t, map[string]Daemon{"hostA": home, "hostB": member},
		/* scheduler that places roles[0]->hostA, roles[1]->hostB */)

	out, err := r.CreateRoutedFlockMembers(context.Background(), RoutedFlockCreateInput{
		Task: "smoke", Roles: []string{"coordinator", "researcher"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.TownWallEnabled {
		t.Fatalf("TownWallEnabled = false, want true")
	}
	rec, _ := r.placementStore.GetRoutedFlock(out.FlockID)
	if rec.HomeHost != "hostA" {
		t.Fatalf("HomeHost = %q, want hostA (roles[0] placement)", rec.HomeHost)
	}
	if home.distributedCalls != 1 {
		t.Fatalf("home did not get a distributed (hub) registration")
	}
	if member.relayCalls != 1 {
		t.Fatalf("member did not get a relay registration")
	}
	// spawn identity injected
	if member.lastSpawn.FlockID != out.FlockID || member.lastSpawn.AgentID == "" || member.lastSpawn.ControlPlaneToken == "" {
		t.Fatalf("member spawn missing flock identity: %+v", member.lastSpawn)
	}
}
```

(Extend the existing fake daemon type with `distributedCalls`, `relayCalls int`, `lastSpawn SpawnVMRequest`, and no-op `RegisterDistributedFlock`/`RegisterRelayFlock`. If the file has no fake daemon yet, model it on the `Daemon` interface in `daemon_client.go`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/anvilmcp -run TestCreateRoutedFlockMembers_WiresSharedTownWall -count=1`
Expected: FAIL — no home selection, `TownWallEnabled` hardcoded false, no registration calls, spawn omits identity.

- [ ] **Step 3: Add client methods + interface + request types**

In `internal/anvilmcp/daemon_client.go` add to the `Daemon` interface and the HTTP client impl:

```go
type DistributedFlockRequest struct {
	Roster     []RosterMember `json:"roster"`
	RelayToken string         `json:"relay_token"`
}
type RelayFlockRequest struct {
	HomeAddr   string `json:"home_addr"`
	RelayToken string `json:"relay_token"`
}
type RosterMember struct {
	AgentID string `json:"agent_id"`
	Host    string `json:"host"`
}
```

Interface additions:

```go
	RegisterDistributedFlock(ctx context.Context, flockID string, req DistributedFlockRequest) error
	RegisterRelayFlock(ctx context.Context, flockID string, req RelayFlockRequest) error
```

Implement each as a `POST` to `/flocks/{id}/distributed` and `/flocks/{id}/relay` with the daemon's existing auth (reuse the client's existing request helper that sets the bearer for other daemon calls in this file). Expect 201.

- [ ] **Step 4: Add `HomeHost` + redacted `relayToken` to the record**

In `internal/anvilmcp/placement_store.go`:

```go
	HomeHost   string `json:"home_host,omitempty"`
	// relayToken is persisted for reconcile re-registration but MUST NOT be
	// serialized into MCP output/audit; it is unexported and written via a
	// dedicated persistence path that scrubs it from any external view.
```

Persist the token in the store's on-disk JSON under a field the record's public marshaling omits (add a separate serialization struct, or a `json:"-"` field plus a sidecar). Ensure `RoutedFlockRecord`'s outward JSON (used by MCP output) carries `home_host` but never the token. If the store already round-trips one struct, add an internal `relayToken` field with `json:"relay_token_internal,omitempty"` written only to the store file and stripped by the MCP-facing projection — Task 6 asserts the redaction.

- [ ] **Step 5: Rewrite the create body**

In `internal/anvilmcp/routed_flock.go` `CreateRoutedFlockMembers`, after `ScheduleFlock` returns `plan`:

```go
	homeHost := strings.TrimSpace(plan.Agents[0].Host.Name) // roles[0] placement
	relayToken, err := newRelayToken() // crypto/rand hex, 32 bytes
	if err != nil {
		return nil, err // wrap as a create failure per existing rollback style
	}
	roster := make([]RosterMember, 0, len(plan.Agents))
	for i, a := range plan.Agents {
		roster = append(roster, RosterMember{AgentID: fmt.Sprintf("%s-%d", a.Role, i+1), Host: strings.TrimSpace(a.Host.Name)})
	}
	homeDaemon := r.daemons[homeHost]
	if err := homeDaemon.RegisterDistributedFlock(ctx, record.FlockID, DistributedFlockRequest{Roster: roster, RelayToken: relayToken}); err != nil {
		return nil, rollbackRoutedFlockCreate(ctx, r, record, /* metric */)
	}
	record.HomeHost = homeHost
	homeAddr := r.daemonAddr(homeHost) // base URL of the home daemon
```

Then, inside the per-agent spawn loop, before `daemon.SpawnVM`, register the relay flock on that member host (skip if the member host IS the home host — the home already owns the hub) and pass identity to spawn:

```go
		agentID := fmt.Sprintf("%s-%d", planned.Role, idx+1)
		if hostName != homeHost {
			if err := daemon.RegisterRelayFlock(ctx, record.FlockID, RelayFlockRequest{HomeAddr: homeAddr, RelayToken: relayToken}); err != nil {
				return nil, rollbackRoutedFlockCreate(ctx, r, record, /* metric */)
			}
		}
		resp, err := daemon.SpawnVM(ctx, SpawnVMRequest{
			Profile:           planned.Role,
			TenantID:          plan.TenantID,
			EgressPolicy:      string(plan.EgressPolicy),
			FlockID:           record.FlockID,
			AgentID:           agentID,
			ControlPlaneToken: relayToken, // guest CP token == the flock relay token (accepted by home)
		})
```

Finally set `TownWallEnabled` on the output to `true` (replace the hardcoded `false` at `routed_flock.go:230`). Persist `record.HomeHost` and the relay token via the store (Step 4 path). Add `newRelayToken()` (crypto/rand → hex) and `r.daemonAddr(host)` (base URL from the router's host→addr map; grep `runtime_router.go` for how daemon base addresses are known — reuse it).

Note: using the relay token as the in-guest CP token means the home daemon accepts both the guest-originated forward and the relay hop under one per-flock secret. Keep it internal/redacted.

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/anvilmcp -run TestCreateRoutedFlockMembers_WiresSharedTownWall -count=1`
Expected: PASS.

- [ ] **Step 7: Run the package**

Run: `go test ./internal/anvilmcp -count=1`
Expected: `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/anvilmcp/daemon_client.go internal/anvilmcp/placement_store.go internal/anvilmcp/routed_flock.go internal/anvilmcp/routed_flock_test.go
git commit -m "feat(runtime): wire shared town wall on routed flock create"
```

---

## Task 6: Redaction + IronClaw schema-exclusion guards

**Files:**
- Modify: `internal/anvilmcp/routed_flock.go` / `replicating_daemon.go` (remove Town Wall hard-error for routed flocks; delegate to home)
- Test: `internal/anvilmcp/routed_flock_test.go`, `internal/anvilmcp/ironclaw_schema_test.go`

**Interfaces:**
- Consumes: Task 5.
- Produces: `PostRoutedTownWall`/`RoutedTownWallHistory` delegate to the home daemon (or are served through the member relay); relay token never appears in any MCP-facing output.

- [ ] **Step 1: Write the failing redaction test**

```go
func TestRoutedFlockRecord_RedactsRelayToken(t *testing.T) {
	r := newTestRouterWithDaemons(t, /* home+member */)
	out, err := r.CreateRoutedFlockMembers(context.Background(), RoutedFlockCreateInput{Task: "t", Roles: []string{"coordinator", "researcher"}})
	if err != nil {
		t.Fatal(err)
	}
	// The MCP-facing projection of the record must not contain the relay token.
	rec, _ := r.placementStore.GetRoutedFlock(out.FlockID)
	blob, _ := json.Marshal(rec) // the value returned to MCP/tools
	if strings.Contains(string(blob), "relay_token") || tokenLeaks(blob) {
		t.Fatalf("relay token leaked into MCP-facing record: %s", blob)
	}
}
```

Where `tokenLeaks` checks the actual generated token value is absent (capture it via a test seam on `newRelayToken`, or assert the field is empty in the marshaled projection).

- [ ] **Step 2: Run test to verify it fails (or passes trivially)**

Run: `go test ./internal/anvilmcp -run TestRoutedFlockRecord_RedactsRelayToken -count=1`
Expected: FAIL if the token is in the marshaled record; if Step 4-of-Task-5 already omits it, strengthen the test to capture the real token value and assert absence.

- [ ] **Step 3: Ensure the MCP-facing projection omits the token**

Confirm the record type returned to MCP tools (`internal/anvilmcp/tools.go` routed-flock output) carries `home_host` but marshals no relay token. If the store persists the token in the same struct, split into `RoutedFlockRecord` (MCP-facing, no token) and an internal persistence struct that holds it. Add the token to any audit-scrub list alongside `agent_token`.

- [ ] **Step 4: Remove the Town Wall hard-error for routed flocks**

In `internal/anvilmcp/routed_flock.go` (`PostRoutedTownWall`, `RoutedTownWallHistory`) and `replicating_daemon.go` (the mirrored guards), replace the "not supported" errors with delegation: post/history for a routed flock now succeed because members relay to the home wall. Keep them returning the home wall's response (or simply document that in-guest `gtwall` is the path and these MCP helpers proxy to the home daemon's `/wall/history`).

- [ ] **Step 5: Write/extend the IronClaw schema-exclusion guard**

In `internal/anvilmcp/ironclaw_schema_test.go`, extend the existing `TestCurrentIronClawSchemasExclude...` guard to assert NO new `anvil_*` tool for cross-host town wall was added (the tool set must be unchanged except the pre-existing `anvil_create_routed_flock_members`; there is no `anvil_post_wall`/`anvil_relay`/`anvil_distributed` tool):

```go
func TestIronClawSchemasExcludeCrossHostWallTools(t *testing.T) {
	for _, name := range currentToolNames(t) {
		if strings.Contains(name, "relay") || strings.Contains(name, "distributed") || name == "anvil_post_wall" {
			t.Fatalf("cross-host wall must not add an anvil_* tool, found %q", name)
		}
	}
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/anvilmcp -run 'TestRoutedFlockRecord_RedactsRelayToken|TestIronClawSchemasExcludeCrossHostWallTools' -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/anvilmcp/routed_flock.go internal/anvilmcp/replicating_daemon.go internal/anvilmcp/tools.go internal/anvilmcp/routed_flock_test.go internal/anvilmcp/ironclaw_schema_test.go
git commit -m "feat(runtime): enable routed flock town wall; guard token redaction and schema"
```

---

## Task 7: Reconcile re-registration after daemon restart

**Files:**
- Modify: `internal/anvilmcp/runtime_router.go` (`ReconcilePlacements`)
- Test: `internal/anvilmcp/runtime_router_test.go`

**Interfaces:**
- Consumes: Task 5 (`HomeHost`, relay token, registration methods).
- Produces: on reconcile, hub flock re-registered on the home daemon and relay flocks re-registered on member daemons from `PlacementStore`.

- [ ] **Step 1: Write the failing test** — reconcile after a "restarted" (registration-cleared) fake daemon re-issues distributed + relay registrations.

```go
func TestReconcile_ReregistersSharedTownWall(t *testing.T) {
	home := newFakeDaemon("hostA")
	member := newFakeDaemon("hostB")
	r := newTestRouterWithDaemons(t, map[string]Daemon{"hostA": home, "hostB": member}, /* scheduler */)
	out, _ := r.CreateRoutedFlockMembers(context.Background(), RoutedFlockCreateInput{Task: "t", Roles: []string{"coordinator", "researcher"}})
	home.distributedCalls, member.relayCalls = 0, 0 // simulate restart: registrations lost

	if err := r.ReconcilePlacements(context.Background()); err != nil {
		t.Fatal(err)
	}
	if home.distributedCalls != 1 || member.relayCalls != 1 {
		t.Fatalf("reconcile did not re-register (home=%d member=%d)", home.distributedCalls, member.relayCalls)
	}
	_ = out
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/anvilmcp -run TestReconcile_ReregistersSharedTownWall -count=1`
Expected: FAIL — reconcile does not touch hub/relay registrations.

- [ ] **Step 3: Extend `ReconcilePlacements`**

In `internal/anvilmcp/runtime_router.go`, after the existing VM-placement reconciliation, iterate persisted routed flocks with a non-empty `HomeHost` and re-issue `RegisterDistributedFlock` to the home daemon and `RegisterRelayFlock` to each member host (using the persisted relay token). Registration endpoints are idempotent (Task 3), so re-issuing when already present is safe. Skip flocks whose status is `deleted`/`deleting`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/anvilmcp -run TestReconcile_ReregistersSharedTownWall -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/anvilmcp/runtime_router.go internal/anvilmcp/runtime_router_test.go
git commit -m "feat(runtime): reconcile re-registers cross-host town wall"
```

---

## Task 8: Rollback deregistration on partial create failure

**Files:**
- Modify: `internal/anvilmcp/routed_flock.go` (`rollbackRoutedFlockCreate`)
- Test: `internal/anvilmcp/routed_flock_test.go`

**Interfaces:**
- Consumes: Task 5.
- Produces: on create failure after hub/relay registration, the rollback deregisters them (best-effort) so a retried create starts clean.

- [ ] **Step 1: Write the failing test** — a member spawn failure triggers rollback that calls deregister on the home + already-registered members.

```go
func TestRollback_DeregistersSharedTownWall(t *testing.T) {
	home := newFakeDaemon("hostA")
	member := newFakeDaemon("hostB")
	member.spawnErr = errors.New("boom") // force failure after registration
	r := newTestRouterWithDaemons(t, map[string]Daemon{"hostA": home, "hostB": member}, /* scheduler */)

	_, err := r.CreateRoutedFlockMembers(context.Background(), RoutedFlockCreateInput{Task: "t", Roles: []string{"coordinator", "researcher"}})
	if err == nil {
		t.Fatal("expected create failure")
	}
	if home.deregisterCalls == 0 {
		t.Fatalf("rollback did not deregister the hub flock")
	}
}
```

(Add a `Deregister`/`DeleteFlock` client call — reuse the existing daemon `DELETE /flocks/{id}` if present, else add a minimal deregistration call. Extend the fake daemon with `deregisterCalls` and `spawnErr`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/anvilmcp -run TestRollback_DeregistersSharedTownWall -count=1`
Expected: FAIL — rollback doesn't deregister.

- [ ] **Step 3: Extend `rollbackRoutedFlockCreate`**

Add best-effort deregistration: `DELETE /flocks/{id}` on the home daemon and each member daemon that was registered, before/along with the existing VM cleanup. The home daemon's flock delete path must also **remove the accepted relay_token** it added in Task 3 (`cp.removeAcceptedRelayToken(flockID)`), so a cleaned-up flock's token is no longer honored. Failures during rollback keep the existing `failed_cleanup_pending` status.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/anvilmcp -run TestRollback_DeregistersSharedTownWall -count=1`
Expected: PASS.

- [ ] **Step 5: Run the full anvilmcp package**

Run: `go test ./internal/anvilmcp -count=1`
Expected: `ok`.

- [ ] **Step 6: Commit**

```bash
git add internal/anvilmcp/routed_flock.go internal/anvilmcp/routed_flock_test.go
git commit -m "feat(runtime): rollback deregisters cross-host town wall on failure"
```

---

## Task 9: KVM e2e — real member daemon relays to a stub home

**Files:**
- Create: `scripts/anvil-cross-host-wall-e2e.sh`
- Modify: `docs/operations/release-checklist.md` (add the script to the KVM gate list)
- Test: the script itself (KVM-gated)

**Interfaces:**
- Consumes: Tasks 1-6.
- Produces: an executable, non-LLM KVM check that a real flock member VM's `gtwall` post traverses guest→in-VM agent→member daemon→relay→stub home.

- [ ] **Step 1: Write the e2e script**

`scripts/anvil-cross-host-wall-e2e.sh` (model structure on `scripts/vm-workload-e2e.sh`): start one real `anvil-daemon` as the MEMBER host (`EPHEMERA_API_ADDR=127.0.0.1:3000`); start a tiny stub HOME HTTP server (a few lines of `python3 -m http.server`-style handler, or a bundled Go stub under `scripts/`) on `127.0.0.1:3100` that records `POST /flocks/{id}/post`. Then:
1. `POST /flocks/wall-e2e/relay` to the member daemon with `home_addr=http://127.0.0.1:3100`, `relay_token=rt-e2e`.
2. `POST /vms` with `flock_id=wall-e2e`, `agent_id=researcher-1`, `control_plane_token=rt-e2e` → a real member VM with flock identity injected.
3. In-VM, run `gtwall "ROUNDTRIP_OK"` (via the workloads/run or task path — a plain post, no LLM).
4. Assert the stub home received `{"agent_id":"researcher-1","body":"ROUNDTRIP_OK"}` with `Authorization: Bearer rt-e2e` and NO `agent_token`.
Print `✓` per assertion; exit non-zero on any failure. Record that full two-daemon cross-host integration (bridge/IP collisions on one host) is a **manual multi-host** check, out of single-host CI scope.

- [ ] **Step 2: Syntax-check**

Run: `bash -n scripts/anvil-cross-host-wall-e2e.sh`
Expected: exit 0.

- [ ] **Step 3: Run on a KVM host**

Run: `go build -o anvil-daemon ./cmd/goose-daemon/ && sudo bash scripts/anvil-cross-host-wall-e2e.sh`
Expected: all `✓`, exit 0. (Provider-key-free — the post carries no model call.)

- [ ] **Step 4: Add to the release-checklist gate list**

In `docs/operations/release-checklist.md`, add `scripts/anvil-cross-host-wall-e2e.sh` to the KVM gate script enumeration.

- [ ] **Step 5: Commit**

```bash
git add scripts/anvil-cross-host-wall-e2e.sh docs/operations/release-checklist.md
git commit -m "test(runtime): cross-host town wall relay e2e (real member + stub home)"
```

---

## Task 10: Docs — roadmap, boundary, handoff

**Files:**
- Modify: `docs/architecture/multi-tenant-roadmap.md`, `docs/PUBLIC_RELEASE_BOUNDARY.md`, `docs/ADR_INDEX.md`
- Create: `docs/operations/2026-07-06-cross-host-town-wall-handoff.md`

- [ ] **Step 1: Update the roadmap + boundary**

In `multi-tenant-roadmap.md`, move "cross-host `gtcall`, coordinator Town Wall, guest flock context injection" — for the shared-wall slice — from "비구현 범위" to implemented (shared wall only; gtcall/broadcast still out). In `PUBLIC_RELEASE_BOUNDARY.md`, record the shared Town Wall as runtime/operator surface (not an `anvil_*` tool), and the cross-host trust-network prerequisite.

- [ ] **Step 2: ADR + handoff**

Add an `ADR_INDEX.md` row for the cross-host wall (home-hub, relay_token redaction, trust-network prerequisite, SPOF-then-mesh evolution). Write the handoff with what shipped, gate results, and follow-ups (SSE relay hardening if deferred, bounded relay retry/buffer, mesh evolution, cross-host gtcall as the next capability).

- [ ] **Step 3: Verify + commit**

Run: `git diff --check`
Expected: clean.

```bash
git add docs/architecture/multi-tenant-roadmap.md docs/PUBLIC_RELEASE_BOUNDARY.md docs/ADR_INDEX.md docs/operations/2026-07-06-cross-host-town-wall-handoff.md
git commit -m "docs: document cross-host shared town wall"
```

---

## Final verification gate (after all tasks)

```bash
go test ./cmd/... ./internal/... -count=1
go build ./cmd/goose-daemon ./cmd/anvil-mcp ./cmd/anvil-scheduler ./cmd/ephemera-ctl
git diff --check
bash -n scripts/anvil-cross-host-wall-e2e.sh
# KVM host:
go build -o anvil-daemon ./cmd/goose-daemon/
sudo bash e2e_test.sh
sudo bash scripts/anvil-cross-host-wall-e2e.sh
scripts/anvil-mcp-e2e.sh flock
```

Expected: unit suite green; builds succeed; existing flock e2e still green (single-host path unchanged); cross-host wall e2e green; IronClaw schema-exclusion + token-redaction guards pass.
