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
