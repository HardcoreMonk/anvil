package main

import (
	"testing"

	"ephemera/internal/anvilmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolRegistrationsIncludeSnapshotTools(t *testing.T) {
	registrations := toolRegistrations()
	names := make(map[string]bool, len(registrations))
	want := map[string]string{
		"anvil_spawn_vm":         "Create an ephemera VM and optionally bind a local session_name alias.",
		"anvil_run_task":         "Run a prompt synchronously in an existing ephemera VM using vm_id or session_name.",
		"anvil_get_vm_health":    "Return health for an existing ephemera VM agent using vm_id or session_name.",
		"anvil_stop_vm":          "Ask the ephemera VM agent to stop gracefully without deleting VM resources.",
		"anvil_delete_vm":        "Delete an ephemera VM and release its session_name alias if present.",
		"anvil_create_snapshot":  "Create a full or diff snapshot for an ephemera VM using vm_id or session_name.",
		"anvil_list_snapshots":   "List snapshots known to the ephemera daemon.",
		"anvil_restore_snapshot": "Restore a new ephemera VM from a snapshot and optionally bind a session_name alias.",
		"anvil_delete_snapshot":  "Delete a snapshot by snapshot_id through the ephemera daemon.",
	}

	for _, registration := range registrations {
		if registration.name == "" {
			t.Fatal("tool registration has empty name")
		}
		description, ok := want[registration.name]
		if !ok {
			t.Fatalf("unexpected tool registration %q", registration.name)
		}
		if registration.description != description {
			t.Fatalf("tool %q description = %q, want %q", registration.name, registration.description, description)
		}
		if registration.register == nil {
			t.Fatalf("tool %q has nil register function", registration.name)
		}
		var _ func(*mcp.Server, *mcp.Tool, *anvilmcp.Tools) = registration.register
		names[registration.name] = true
	}

	for name := range want {
		if !names[name] {
			t.Fatalf("missing tool registration %q; names = %v", name, names)
		}
	}
	if len(registrations) != len(want) {
		t.Fatalf("registration count = %d, want %d", len(registrations), len(want))
	}
}
