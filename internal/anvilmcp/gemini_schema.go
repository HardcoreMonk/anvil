package anvilmcp

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NormalizeSchemaForGemini rewrites a generated JSON Schema so it satisfies the
// Gemini function-declaration subset, which requires every schema node to carry a
// single `type` value.
//
// jsonschema-go marks nilable Go kinds (slices, maps, pointers) as a null union —
// e.g. a `[]string` field becomes {"type":["null","array"],"items":{"type":"string"}}.
// Gemini rejects that union: it cannot map ["null","array"] to Type.ARRAY, so the
// request fails with
//
//	properties[<field>].items: field predicate failed: $type == Type.ARRAY
//
// Collapsing ["null", X] -> X preserves the tool's meaning (a required list stays a
// list of the same element type; an optional field is simply omitted rather than sent
// as JSON null) and remains valid JSON Schema for non-Gemini MCP clients — a
// single-type array with typed items is standard, so the change is additive.
func NormalizeSchemaForGemini(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if len(s.Types) > 0 {
		nonNull := make([]string, 0, len(s.Types))
		for _, t := range s.Types {
			if t != "null" {
				nonNull = append(nonNull, t)
			}
		}
		// Only collapse an unambiguous nullable type (["null", X]) to its single
		// non-null type X. A genuine multi-type union (rare, and not produced by any
		// anvil input struct) is left untouched so the guard still surfaces it.
		if len(nonNull) == 1 {
			s.Type = nonNull[0]
			s.Types = nil
		}
	}
	NormalizeSchemaForGemini(s.Items)
	NormalizeSchemaForGemini(s.AdditionalProperties)
	NormalizeSchemaForGemini(s.AdditionalItems)
	for _, sub := range s.PrefixItems {
		NormalizeSchemaForGemini(sub)
	}
	for _, sub := range s.ItemsArray {
		NormalizeSchemaForGemini(sub)
	}
	for _, sub := range s.Properties {
		NormalizeSchemaForGemini(sub)
	}
	for _, sub := range s.PatternProperties {
		NormalizeSchemaForGemini(sub)
	}
	for _, sub := range s.AllOf {
		NormalizeSchemaForGemini(sub)
	}
	for _, sub := range s.AnyOf {
		NormalizeSchemaForGemini(sub)
	}
	for _, sub := range s.OneOf {
		NormalizeSchemaForGemini(sub)
	}
}

// GeminiSafeInputSchema builds the MCP input schema for In and normalizes it so it
// is accepted by Gemini function-declaration validation.
func GeminiSafeInputSchema[In any]() (*jsonschema.Schema, error) {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		return nil, err
	}
	NormalizeSchemaForGemini(s)
	return s, nil
}

// AddToolGeminiSafe registers a tool whose input schema is normalized for Gemini, so
// the same MCP surface works for IronClaw's Gemini backend and for other clients.
func AddToolGeminiSafe[In, Out any](server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out]) {
	if tool.InputSchema == nil {
		schema, err := GeminiSafeInputSchema[In]()
		if err != nil {
			panic(fmt.Sprintf("AddToolGeminiSafe: tool %q: %v", tool.Name, err))
		}
		tool.InputSchema = schema
	}
	mcp.AddTool(server, tool, handler)
}

// ValidateGeminiWireSchema returns an error if any node in the schema uses a
// multi-valued (union) type, or is an array without a typed items schema — the
// constructs the Gemini function-declaration validator rejects.
func ValidateGeminiWireSchema(toolName string, s *jsonschema.Schema) error {
	return validateGeminiWireSchema(toolName, "input", s)
}

func validateGeminiWireSchema(toolName, path string, s *jsonschema.Schema) error {
	if s == nil {
		return nil
	}
	if len(s.Types) > 0 {
		return fmt.Errorf(
			"tool %s: %s has a multi-valued type %v; Gemini function declarations require a single type per node "+
				"(a []T slice emits [\"null\",\"array\"], which Gemini rejects with "+
				"`properties[..].items: field predicate failed: $type == Type.ARRAY`)",
			toolName, path, s.Types)
	}
	if s.Type == "array" && (s.Items == nil || (s.Items.Type == "" && len(s.Items.Types) == 0)) {
		return fmt.Errorf("tool %s: %s is an array but its items have no type", toolName, path)
	}
	if err := validateGeminiWireSchema(toolName, path+".items", s.Items); err != nil {
		return err
	}
	if err := validateGeminiWireSchema(toolName, path+".additionalProperties", s.AdditionalProperties); err != nil {
		return err
	}
	for name, p := range s.Properties {
		if err := validateGeminiWireSchema(toolName, path+".properties."+name, p); err != nil {
			return err
		}
	}
	for i, sub := range s.PrefixItems {
		if err := validateGeminiWireSchema(toolName, fmt.Sprintf("%s.prefixItems[%d]", path, i), sub); err != nil {
			return err
		}
	}
	return nil
}
