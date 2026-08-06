package mcpgateway

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// SessionStore tracks MCP sessions (the gateway↔goose conversation). MCP's
// Streamable HTTP transport keys a session by the Mcp-Session-Id header issued at
// initialize. The single-host store is an in-memory map; a multi-host fork can
// back it with a shared store so sessions survive across gateway nodes.
type SessionStore interface {
	Create(c Caller) string
	Get(id string) (Caller, bool)
	Delete(id string)
}

const (
	// maxSessions caps the in-memory session map. A session id carries no
	// authority — the gateway re-resolves the caller from the source IP on every
	// request and never reads a session back for authorization — so an evicted
	// session costs a client nothing beyond a re-initialize. A VM holds one live
	// session at a time and a host runs tens of VMs, so this leaves better than
	// 10x headroom for reconnect churn while capping the map at well under a
	// megabyte.
	maxSessions = 1024

	// sessionTTL bounds how long an entry is kept. Nothing reads a session back,
	// so this is a memory bound rather than a client-visible session timeout.
	sessionTTL = time.Hour
)

// session is one stored session: the caller it was issued to, plus the creation
// time the TTL sweep and the oldest-first eviction key off.
type session struct {
	caller  Caller
	created time.Time
}

// memSessionStore is a mutex-guarded in-memory SessionStore. Every initialize
// mints a fresh random id, so an unbounded map would let a request loop from an
// already-resolvable VM exhaust the daemon's memory. Entries are therefore both
// swept by TTL and hard-capped at maxSessions, mirroring how tokenBucketLimiter
// keeps its per-(VM, server) map from growing without bound.
type memSessionStore struct {
	mu         sync.Mutex
	sessions   map[string]session
	maxLen     int
	evictAfter time.Duration
	lastSweep  time.Time
	now        func() time.Time // injectable clock for tests; defaults to time.Now
}

// NewMemSessionStore returns an empty in-memory SessionStore.
func NewMemSessionStore() SessionStore {
	return &memSessionStore{
		sessions:   map[string]session{},
		maxLen:     maxSessions,
		evictAfter: sessionTTL,
		now:        time.Now,
	}
}

func (s *memSessionStore) Create(c Caller) string {
	id := newSessionID()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.sweep(now)
	// The sweep runs at most once a minute and every request mints a fresh key,
	// so a Create loop can outrun the TTL. The hard cap holds the map at maxLen
	// regardless of arrival rate; dropping the oldest keeps live clients last in
	// line for eviction.
	for len(s.sessions) >= s.maxLen && s.evictOldest() {
	}
	s.sessions[id] = session{caller: c, created: now}
	return id
}

func (s *memSessionStore) Get(id string) (Caller, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess.caller, ok
}

func (s *memSessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// sweep drops sessions older than evictAfter. It runs at most once per minute and
// is called under s.mu from Create, mirroring tokenBucketLimiter.sweep. Nothing
// reads a session back, so evicting one is free: the client simply re-initializes.
func (s *memSessionStore) sweep(now time.Time) {
	if now.Sub(s.lastSweep) < time.Minute {
		return
	}
	s.lastSweep = now
	for id, sess := range s.sessions {
		if now.Sub(sess.created) > s.evictAfter {
			delete(s.sessions, id)
		}
	}
}

// evictOldest drops the single oldest entry and reports whether it dropped one
// (false when the map is already empty, which also terminates Create's loop).
// The scan is O(len(sessions)) but only runs once the cap is reached — under a
// Create flood, where a bounded scan is negligible beside the request itself.
func (s *memSessionStore) evictOldest() bool {
	var (
		oldestID string
		oldest   time.Time
		found    bool
	)
	for id, sess := range s.sessions {
		if !found || sess.created.Before(oldest) {
			oldestID, oldest, found = id, sess.created, true
		}
	}
	if !found {
		return false
	}
	delete(s.sessions, oldestID)
	return true
}

// newSessionID returns a random 128-bit hex session id. rand.Read never fails on
// the platforms ephemera targets; on the off chance it does, the empty string is
// handled by the caller (a missing session id simply forces re-resolution).
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
