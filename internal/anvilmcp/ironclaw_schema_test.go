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

func TestSpawnFlockRolesSchemaDescribesStringItems(t *testing.T) {
	var rolesField *IronClawToolInputField
	for _, schema := range CurrentIronClawToolInputSchemas() {
		if schema.ToolName != "anvil_spawn_flock" {
			continue
		}
		for idx := range schema.Fields {
			if schema.Fields[idx].Name == "roles" {
				rolesField = &schema.Fields[idx]
				break
			}
		}
	}

	if rolesField == nil {
		t.Fatal("anvil_spawn_flock roles field not found")
	}
	if rolesField.GeminiType != "ARRAY" {
		t.Fatalf("roles GeminiType = %q, want ARRAY", rolesField.GeminiType)
	}
	if rolesField.GeminiItemsType != "STRING" {
		t.Fatalf("roles GeminiItemsType = %q, want STRING", rolesField.GeminiItemsType)
	}
}
