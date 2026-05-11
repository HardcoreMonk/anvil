package anvilmcp

import (
	"fmt"
	"strings"
	"sync"
)

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]string
}

func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions: make(map[string]string),
	}
}

func (s *SessionStore) Bind(sessionName, vmID string) error {
	sessionName = strings.TrimSpace(sessionName)
	vmID = strings.TrimSpace(vmID)
	if sessionName == "" {
		return fmt.Errorf("session name must be non-empty")
	}
	if vmID == "" {
		return fmt.Errorf("vm ID must be non-empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[sessionName]; ok {
		return fmt.Errorf("session %q already exists", sessionName)
	}
	s.sessions[sessionName] = vmID
	return nil
}

func (s *SessionStore) Resolve(sessionName string) (string, bool) {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return "", false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	vmID, ok := s.sessions[sessionName]
	return vmID, ok
}

func (s *SessionStore) Exists(sessionName string) bool {
	_, ok := s.Resolve(sessionName)
	return ok
}

func (s *SessionStore) ResolveIdentity(vmID, sessionName string) (string, error) {
	vmID = strings.TrimSpace(vmID)
	sessionName = strings.TrimSpace(sessionName)
	if vmID != "" {
		return vmID, nil
	}
	if sessionName == "" {
		return "", fmt.Errorf("vm ID or session name is required")
	}

	resolvedVMID, ok := s.Resolve(sessionName)
	if !ok {
		return "", fmt.Errorf("unknown session %q", sessionName)
	}
	return resolvedVMID, nil
}

func (s *SessionStore) RemoveVM(vmID string) {
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for sessionName, mappedVMID := range s.sessions {
		if mappedVMID == vmID {
			delete(s.sessions, sessionName)
		}
	}
}
