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
