package anvilmcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionStoreBindResolve(t *testing.T) {
	store := NewSessionStore()

	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	vmID, ok := store.Resolve("work")
	if !ok {
		t.Fatal("Resolve() ok = false, want true")
	}
	if vmID != "vm-1" {
		t.Fatalf("Resolve() vmID = %q, want %q", vmID, "vm-1")
	}
}

func TestSessionStoreRejectsDuplicate(t *testing.T) {
	store := NewSessionStore()

	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := store.Bind("work", "vm-2"); err == nil {
		t.Fatal("Bind() duplicate error = nil, want error")
	}
}

func TestSessionStoreExists(t *testing.T) {
	store := NewSessionStore()

	if store.Exists("work") {
		t.Fatal("Exists() = true, want false")
	}
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if !store.Exists("work") {
		t.Fatal("Exists() = false, want true")
	}
}

func TestSessionStoreResolveVMIDPriority(t *testing.T) {
	store := NewSessionStore()

	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	vmID, err := store.ResolveIdentity("vm-explicit", "work")
	if err != nil {
		t.Fatalf("ResolveIdentity() error = %v", err)
	}
	if vmID != "vm-explicit" {
		t.Fatalf("ResolveIdentity() vmID = %q, want %q", vmID, "vm-explicit")
	}
}

func TestSessionStoreUnknownSession(t *testing.T) {
	store := NewSessionStore()

	if _, err := store.ResolveIdentity("", "missing"); err == nil {
		t.Fatal("ResolveIdentity() error = nil, want error")
	}
}

func TestSessionStoreRequiresIdentity(t *testing.T) {
	store := NewSessionStore()

	if _, err := store.ResolveIdentity("", ""); err == nil {
		t.Fatal("ResolveIdentity() error = nil, want error")
	}
}

func TestSessionStoreRemoveVM(t *testing.T) {
	store := NewSessionStore()

	for sessionName, vmID := range map[string]string{
		"a": "vm-1",
		"b": "vm-2",
		"c": "vm-1",
	} {
		if err := store.Bind(sessionName, vmID); err != nil {
			t.Fatalf("Bind(%q, %q) error = %v", sessionName, vmID, err)
		}
	}

	store.RemoveVM("vm-1")

	if store.Exists("a") {
		t.Fatal("Exists(a) = true, want false")
	}
	if !store.Exists("b") {
		t.Fatal("Exists(b) = false, want true")
	}
	if store.Exists("c") {
		t.Fatal("Exists(c) = true, want false")
	}
}

func TestLoadSessionStoreMissingFileReturnsEmptyStore(t *testing.T) {
	store, err := LoadSessionStore(filepath.Join(t.TempDir(), "missing", "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionStore() error = %v", err)
	}
	if store.Exists("work") {
		t.Fatal("loaded store unexpectedly contains session work")
	}
}

func TestSessionStoreSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anvil-mcp", "sessions.json")
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := store.Bind("review", "vm-2"); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var raw struct {
		Sessions map[string]string `json:"sessions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("saved JSON did not decode: %v", err)
	}
	if raw.Sessions["work"] != "vm-1" || raw.Sessions["review"] != "vm-2" {
		t.Fatalf("saved sessions = %#v, want work/review mappings", raw.Sessions)
	}

	loaded, err := LoadSessionStore(path)
	if err != nil {
		t.Fatalf("LoadSessionStore() error = %v", err)
	}
	for sessionName, wantVMID := range map[string]string{"work": "vm-1", "review": "vm-2"} {
		gotVMID, ok := loaded.Resolve(sessionName)
		if !ok || gotVMID != wantVMID {
			t.Fatalf("Resolve(%q) = %q, %v; want %q, true", sessionName, gotVMID, ok, wantVMID)
		}
	}
}

func TestLoadSessionStoreRejectsCorruptJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte(`{"sessions":`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := LoadSessionStore(path)
	if err == nil {
		t.Fatal("LoadSessionStore() error = nil, want error")
	}
}

func TestSessionStoreSaveUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "sessions.json")
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(parent) error = %v", err)
	}
	if got := parentInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent permissions = %o, want 700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(file) error = %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file permissions = %o, want 600", got)
	}
}

func TestSessionStoreSaveFailureCleansUpTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	if err := store.Save(path); err == nil {
		t.Fatal("Save() error = nil, want error for directory target")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sessions-") {
			t.Fatalf("temporary session store file was not removed: %s", entry.Name())
		}
	}
}
