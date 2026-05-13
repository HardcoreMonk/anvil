package anvilmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func LoadSessionStore(path string) (*SessionStore, error) {
	path = strings.TrimSpace(path)
	store := NewSessionStore()
	if path == "" {
		return store, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("read session store %q: %w", path, err)
	}

	var payload struct {
		Sessions map[string]string `json:"sessions"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parse session store %q: %w", path, err)
	}
	for sessionName, vmID := range payload.Sessions {
		if err := store.Bind(sessionName, vmID); err != nil {
			return nil, fmt.Errorf("invalid session store %q entry %q: %w", path, sessionName, err)
		}
	}
	return store, nil
}

func (s *SessionStore) Save(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}

	payload := struct {
		Sessions map[string]string `json:"sessions"`
	}{
		Sessions: s.snapshot(),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session store: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create session store dir %q: %w", dir, err)
		}
	}
	tempFile, err := os.CreateTemp(dir, ".sessions-")
	if err != nil {
		return fmt.Errorf("create temp session store in %q: %w", dir, err)
	}
	tempPath := tempFile.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod temp session store %q: %w", tempPath, err)
	}
	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temp session store %q: %w", tempPath, err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync temp session store %q: %w", tempPath, err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp session store %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp session store %q to %q: %w", tempPath, path, err)
	}
	removeTemp = false
	fsyncDir(dir)
	return nil
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

func (s *SessionStore) RemoveVM(vmID string) bool {
	return len(s.removeVMAliases(vmID)) > 0
}

func (s *SessionStore) removeVMAliases(vmID string) map[string]string {
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := make(map[string]string)
	for sessionName, mappedVMID := range s.sessions {
		if mappedVMID == vmID {
			delete(s.sessions, sessionName)
			removed[sessionName] = mappedVMID
		}
	}
	return removed
}

func (s *SessionStore) restoreAliases(aliases map[string]string) {
	if len(aliases) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for sessionName, vmID := range aliases {
		s.sessions[sessionName] = vmID
	}
}

func (s *SessionStore) snapshot() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make(map[string]string, len(s.sessions))
	for sessionName, vmID := range s.sessions {
		sessions[sessionName] = vmID
	}
	return sessions
}

func fsyncDir(dir string) {
	if dir == "" {
		dir = "."
	}
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Sync()
}
