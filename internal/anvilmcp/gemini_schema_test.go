package anvilmcp

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func mustGeminiSchema[In any](t *testing.T) *jsonschema.Schema {
	t.Helper()
	s, err := GeminiSafeInputSchema[In]()
	if err != nil {
		t.Fatalf("GeminiSafeInputSchema: %v", err)
	}
	return s
}

// TestAnvilToolInputWireSchemasAreGeminiCompatible guards the ACTUAL MCP wire schema
// (jsonschema-go SDK reflection that mcp.AddTool emits) for every registered tool —
// not the idealized CurrentIronClawToolInputSchemas representation, which cannot model
// the defect below.
//
// Observed 2026-07-12 against gemini-2.5-flash driving IronClaw over the full 19-tool
// anvil surface, the shipped adapter was rejected with:
//
//	GenerateContentRequest.tools[0].function_declarations[2] & [16].parameters
//	  .properties[roles].items: field predicate failed: $type == Type.ARRAY
//
// declarations [2]/[16] are anvil_create_routed_flock_members / anvil_spawn_flock —
// the only tools with a `roles []string` field, which jsonschema-go renders as
// {"type":["null","array"],...}. Gemini cannot map that union to Type.ARRAY.
// GeminiSafeInputSchema must collapse the null-union to a single "array" type; this
// test asserts every tool's shipped input schema is free of union types.
func TestAnvilToolInputWireSchemasAreGeminiCompatible(t *testing.T) {
	checks := []struct {
		name   string
		schema *jsonschema.Schema
	}{
		{"anvil_spawn_vm", mustGeminiSchema[SpawnVMInput](t)},
		{"anvil_run_task", mustGeminiSchema[RunTaskInput](t)},
		{"anvil_copy_in", mustGeminiSchema[CopyInInput](t)},
		{"anvil_copy_out", mustGeminiSchema[CopyOutInput](t)},
		{"anvil_get_vm_health", mustGeminiSchema[VMIdentityInput](t)},
		{"anvil_stop_vm", mustGeminiSchema[VMIdentityInput](t)},
		{"anvil_delete_vm", mustGeminiSchema[VMIdentityInput](t)},
		{"anvil_create_snapshot", mustGeminiSchema[CreateSnapshotInput](t)},
		{"anvil_list_snapshots", mustGeminiSchema[struct{}](t)},
		{"anvil_restore_snapshot", mustGeminiSchema[RestoreSnapshotInput](t)},
		{"anvil_delete_snapshot", mustGeminiSchema[SnapshotIdentityInput](t)},
		{"anvil_replicate_snapshot", mustGeminiSchema[ReplicateSnapshotInput](t)},
		{"anvil_spawn_flock", mustGeminiSchema[SpawnFlockInput](t)},
		{"anvil_create_routed_flock_members", mustGeminiSchema[SpawnFlockInput](t)},
		{"anvil_list_flocks", mustGeminiSchema[ListFlocksInput](t)},
		{"anvil_get_flock", mustGeminiSchema[FlockIdentityInput](t)},
		{"anvil_delete_flock", mustGeminiSchema[FlockIdentityInput](t)},
		{"anvil_post_townwall", mustGeminiSchema[TownWallPostInput](t)},
		{"anvil_get_townwall_history", mustGeminiSchema[FlockIdentityInput](t)},
	}
	for _, c := range checks {
		if err := ValidateGeminiWireSchema(c.name, c.schema); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// TestNormalizeSchemaForGeminiCollapsesRolesNullUnion pins the exact defect: the
// roles []string field must ship as a single "array" type with typed string items.
func TestNormalizeSchemaForGeminiCollapsesRolesNullUnion(t *testing.T) {
	s := mustGeminiSchema[SpawnFlockInput](t)
	roles, ok := s.Properties["roles"]
	if !ok {
		t.Fatal("roles property missing from SpawnFlockInput schema")
	}
	if len(roles.Types) != 0 {
		t.Fatalf("roles must have a single type, got union %v", roles.Types)
	}
	if roles.Type != "array" {
		t.Fatalf("roles type = %q, want \"array\"", roles.Type)
	}
	if roles.Items == nil || roles.Items.Type != "string" {
		t.Fatalf("roles items type = %v, want \"string\"", roles.Items)
	}
}
