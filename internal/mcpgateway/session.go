package mcpgateway

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
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

// memSessionStore is a mutex-guarded in-memory SessionStore.
type memSessionStore struct {
	mu       sync.Mutex
	sessions map[string]Caller
}

// NewMemSessionStore returns an empty in-memory SessionStore.
func NewMemSessionStore() SessionStore {
	return &memSessionStore{sessions: map[string]Caller{}}
}

func (s *memSessionStore) Create(c Caller) string {
	id := newSessionID()
	s.mu.Lock()
	s.sessions[id] = c
	s.mu.Unlock()
	return id
}

func (s *memSessionStore) Get(id string) (Caller, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.sessions[id]
	return c, ok
}

func (s *memSessionStore) Delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
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
