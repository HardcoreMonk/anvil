package mcpgateway

import (
	"strings"
	"testing"
)

// TestRegistry_RejectsReservedCredentialEnv locks the guard that a stdio
// backend's credential_env must not collide with a variable minimalChildEnv
// always sets from scratch (PATH, HOME, LANG). Injecting a credential under one
// of those would silently shadow the child's PATH/HOME/LANG (#30), so the
// registry rejects it as a clear config error at build time.
func TestRegistry_RejectsReservedCredentialEnv(t *testing.T) {
	secrets := map[string]string{"k": "tok"}
	for _, name := range []string{"PATH", "HOME", "LANG"} {
		cfg := ServerConfig{ID: "s", Transport: "stdio", Command: "/bin/true", Credential: "k", CredentialEnv: name}
		_, err := NewRegistry([]ServerConfig{cfg}, secrets, nil)
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("credential_env %q: error = %v, want a 'reserved' config error", name, err)
		}
	}

	// A non-reserved name that shares a prefix must still be accepted (guard is
	// an exact-match, not a prefix ban).
	ok := ServerConfig{ID: "s", Transport: "stdio", Command: "/bin/true", Credential: "k", CredentialEnv: "PATHFINDER"}
	if _, err := NewRegistry([]ServerConfig{ok}, secrets, nil); err != nil {
		t.Fatalf("non-reserved credential_env PATHFINDER rejected: %v", err)
	}
}
