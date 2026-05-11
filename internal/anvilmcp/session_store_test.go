package anvilmcp

import "testing"

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
