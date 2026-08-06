package mcpgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sessionCount reports how many live entries the in-memory session store holds.
// It reaches into the concrete store because the SessionStore interface
// deliberately exposes no size — the bound is an implementation invariant, and
// this is the only place that needs to observe it.
func sessionCount(t *testing.T, s SessionStore) int {
	t.Helper()
	m, ok := s.(*memSessionStore)
	if !ok {
		t.Fatalf("session store is %T, want *memSessionStore", s)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// wantSessionCap is the entry ceiling the in-memory session store must enforce.
// It is spelled out here rather than read from the production constant so the
// guard fails if the cap is ever raised without a matching review of this test.
const wantSessionCap = 1024

// TestMemSessionStore_IsBounded is the M6 guard for "the gateway session map
// cannot grow without bound." Every initialize mints a fresh random session id,
// so an unbounded map turns a request loop from a VM the resolver already trusts
// into a memory exhaustion of the daemon that owns every VM's lifecycle. The
// sibling tokenBucketLimiter already sweeps its map for exactly this reason;
// this pins the same discipline on the session store.
func TestMemSessionStore_IsBounded(t *testing.T) {
	s := NewMemSessionStore()
	const creates = wantSessionCap * 4
	for i := 0; i < creates; i++ {
		if id := s.Create(Caller{VMID: "vm1", Profile: "leader"}); id == "" {
			t.Fatalf("Create returned an empty session id at iteration %d", i)
		}
	}
	if n := sessionCount(t, s); n > wantSessionCap {
		t.Fatalf("session map holds %d entries after %d Create calls; it must stay within the %d-entry cap", n, creates, wantSessionCap)
	}
}

// TestMemSessionStore_EvictionKeepsRecentSessions pins that the bound evicts the
// OLDEST entries, not arbitrary ones: the most recently created session — the one
// a live client is actually holding — must survive a flood of newer creates only
// insofar as it is not itself the oldest. Concretely, the last id created is
// always still present.
func TestMemSessionStore_EvictionKeepsRecentSessions(t *testing.T) {
	s := NewMemSessionStore()
	var last string
	for i := 0; i < wantSessionCap*2; i++ {
		last = s.Create(Caller{VMID: "vm1", Profile: "leader"})
	}
	if _, ok := s.Get(last); !ok {
		t.Fatal("the most recently created session must survive eviction (eviction must drop the oldest, not the newest)")
	}
}

// TestMemSessionStore_SweepsExpiredSessions pins the TTL half of the bound: a
// slow trickle of initializes never reaches the hard cap, so without a sweep the
// map would still creep upward forever. This mirrors tokenBucketLimiter.sweep —
// entries older than the TTL are dropped, at most once a minute, from Create.
func TestMemSessionStore_SweepsExpiredSessions(t *testing.T) {
	s := NewMemSessionStore().(*memSessionStore)
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }

	stale := s.Create(Caller{VMID: "vm1", Profile: "leader"})
	if _, ok := s.Get(stale); !ok {
		t.Fatal("a just-created session must be retrievable")
	}

	// Past the TTL (and past the once-a-minute sweep interval), the next Create
	// sweeps the stale entry out.
	clock = clock.Add(sessionTTL + time.Minute)
	fresh := s.Create(Caller{VMID: "vm2", Profile: "leader"})

	if _, ok := s.Get(stale); ok {
		t.Fatalf("a session older than the %s TTL must be swept out", sessionTTL)
	}
	if _, ok := s.Get(fresh); !ok {
		t.Fatal("the session created by the sweeping call must survive")
	}
	if n := sessionCount(t, s); n != 1 {
		t.Fatalf("session map holds %d entries after the sweep, want 1", n)
	}
}

// TestGateway_InitializeLoopCannotExhaustMemory drives the real threat through
// the HTTP surface: a guest that loops {"method":"initialize"} from an address
// the resolver accepts. The session map must stay bounded, and — because the
// gateway re-resolves the caller from the source IP on every request and never
// reads a session back — the gateway must keep working normally afterwards, with
// a fresh initialize issuing a usable session id.
func TestGateway_InitializeLoopCannotExhaustMemory(t *testing.T) {
	m := newMockMCP(t, false)
	reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, Profiles: []string{"leader"}}, nil)
	store := NewMemSessionStore()
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
		Sessions: store,
	})

	const floods = wantSessionCap * 3
	for i := 0; i < floods; i++ {
		if r := doRPC(t, g, "initialize", initializeParams{ProtocolVersion: "2025-03-26"}); r.Error != nil {
			t.Fatalf("initialize %d returned an error: %v", i, r.Error)
		}
	}
	if n := sessionCount(t, store); n > wantSessionCap {
		t.Fatalf("after %d initialize requests the session map holds %d entries; it must stay within the %d-entry cap", floods, n, wantSessionCap)
	}

	// Recovery: re-initialize still issues a session id on the header, and the
	// resulting session is live in the store.
	body, _ := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26"}`),
	})
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("re-initialize after eviction: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	sid := rr.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("re-initialize after eviction must still issue an Mcp-Session-Id")
	}
	if _, ok := store.Get(sid); !ok {
		t.Fatal("the session issued by a post-eviction initialize must be live in the store")
	}

	// And the gateway still serves real work for that caller.
	list := doRPC(t, g, "tools/list", map[string]any{})
	var lr toolsListResult
	if err := json.Unmarshal(list.Result, &lr); err != nil {
		t.Fatalf("tools/list after eviction: decode: %v", err)
	}
	if len(lr.Tools) != 2 {
		t.Fatalf("tools/list after eviction returned %d tools, want 2 (gateway must be unaffected by session eviction)", len(lr.Tools))
	}
}
