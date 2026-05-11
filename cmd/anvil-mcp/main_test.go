package main

import "testing"

func TestToolRegistrationsIncludeSnapshotTools(t *testing.T) {
	registrations := toolRegistrations()
	names := make(map[string]bool, len(registrations))
	for _, registration := range registrations {
		if registration.name == "" {
			t.Fatal("tool registration has empty name")
		}
		if registration.description == "" {
			t.Fatalf("tool %q has empty description", registration.name)
		}
		if registration.register == nil {
			t.Fatalf("tool %q has nil register function", registration.name)
		}
		names[registration.name] = true
	}

	want := []string{
		"anvil_spawn_vm",
		"anvil_run_task",
		"anvil_get_vm_health",
		"anvil_stop_vm",
		"anvil_delete_vm",
		"anvil_create_snapshot",
		"anvil_list_snapshots",
		"anvil_restore_snapshot",
		"anvil_delete_snapshot",
	}
	for _, name := range want {
		if !names[name] {
			t.Fatalf("missing tool registration %q; names = %v", name, names)
		}
	}
	if len(registrations) != len(want) {
		t.Fatalf("registration count = %d, want %d", len(registrations), len(want))
	}
}
