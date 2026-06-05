package anvilmcp

import "testing"

func TestIronClawSchemaValidationRejectsEmptyGeminiType(t *testing.T) {
	err := ValidateIronClawToolInputSchemas([]IronClawToolInputSchema{{
		ToolName: "broken_tool",
		Fields:   []IronClawToolInputField{{Name: "prompt", GeminiType: ""}},
	}})
	if err == nil {
		t.Fatal("ValidateIronClawToolInputSchemas error = nil, want empty type rejection")
	}
}

func TestIronClawSchemaValidationRejectsArrayWithoutItemsType(t *testing.T) {
	err := ValidateIronClawToolInputSchemas([]IronClawToolInputSchema{{
		ToolName: "broken_tool",
		Fields:   []IronClawToolInputField{{Name: "roles", GeminiType: "ARRAY"}},
	}})
	if err == nil {
		t.Fatal("ValidateIronClawToolInputSchemas error = nil, want empty array items type rejection")
	}
}

func TestCurrentAnvilToolInputsAreGeminiCompatible(t *testing.T) {
	if err := ValidateIronClawToolInputSchemas(CurrentIronClawToolInputSchemas()); err != nil {
		t.Fatalf("current anvil tool inputs are not Gemini compatible: %v", err)
	}
}

func TestCurrentIronClawSchemasIncludeGoosetownTools(t *testing.T) {
	schemas := CurrentIronClawToolInputSchemas()
	names := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		names[schema.ToolName] = true
	}

	for _, name := range []string{
		"anvil_spawn_flock",
		"anvil_create_routed_flock_members",
		"anvil_list_flocks",
		"anvil_get_flock",
		"anvil_delete_flock",
		"anvil_post_townwall",
		"anvil_get_townwall_history",
	} {
		if !names[name] {
			t.Fatalf("missing IronClaw tool input schema %q; names = %v", name, names)
		}
	}
}

func TestReplicateSnapshotSchemaRequiresHostAndSnapshotInputs(t *testing.T) {
	schemas := CurrentIronClawToolInputSchemas()
	var replicateSchema *IronClawToolInputSchema
	for idx := range schemas {
		if schemas[idx].ToolName == "anvil_replicate_snapshot" {
			replicateSchema = &schemas[idx]
			break
		}
	}

	if replicateSchema == nil {
		t.Fatal("anvil_replicate_snapshot schema not found")
	}

	fields := make(map[string]IronClawToolInputField, len(replicateSchema.Fields))
	for _, field := range replicateSchema.Fields {
		fields[field.Name] = field
	}

	for _, name := range []string{"snapshot_id", "source_host", "target_host"} {
		field, ok := fields[name]
		if !ok {
			t.Fatalf("field %q not found in anvil_replicate_snapshot schema: %+v", name, replicateSchema.Fields)
		}
		if !field.Required {
			t.Fatalf("field %q Required = false, want true", name)
		}
		if field.GeminiType != "STRING" {
			t.Fatalf("field %q GeminiType = %q, want STRING", name, field.GeminiType)
		}
	}

	field, ok := fields["include_dependencies"]
	if !ok {
		t.Fatal("field include_dependencies not found in anvil_replicate_snapshot schema")
	}
	if field.Required {
		t.Fatal("field include_dependencies Required = true, want false")
	}
	if field.GeminiType != "BOOLEAN" {
		t.Fatalf("field include_dependencies GeminiType = %q, want BOOLEAN", field.GeminiType)
	}
}

func TestSpawnFlockRolesSchemaDescribesStringItems(t *testing.T) {
	var spawnRolesField *IronClawToolInputField
	var routedRolesField *IronClawToolInputField
	for _, schema := range CurrentIronClawToolInputSchemas() {
		if schema.ToolName != "anvil_spawn_flock" && schema.ToolName != "anvil_create_routed_flock_members" {
			continue
		}
		for idx := range schema.Fields {
			if schema.Fields[idx].Name == "roles" {
				if schema.ToolName == "anvil_spawn_flock" {
					spawnRolesField = &schema.Fields[idx]
				} else {
					routedRolesField = &schema.Fields[idx]
				}
				break
			}
		}
	}

	if spawnRolesField == nil {
		t.Fatal("anvil_spawn_flock roles field not found")
	}
	if routedRolesField == nil {
		t.Fatal("anvil_create_routed_flock_members roles field not found")
	}
	for toolName, field := range map[string]*IronClawToolInputField{
		"anvil_spawn_flock":                 spawnRolesField,
		"anvil_create_routed_flock_members": routedRolesField,
	} {
		if field.GeminiType != "ARRAY" {
			t.Fatalf("%s roles GeminiType = %q, want ARRAY", toolName, field.GeminiType)
		}
		if field.GeminiItemsType != "STRING" {
			t.Fatalf("%s roles GeminiItemsType = %q, want STRING", toolName, field.GeminiItemsType)
		}
	}
}
