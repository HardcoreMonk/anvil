package anvilmcp

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

type IronClawToolInputSchema struct {
	ToolName string                   `json:"tool_name"`
	Fields   []IronClawToolInputField `json:"fields"`
}

type IronClawToolInputField struct {
	Name       string `json:"name"`
	GeminiType string `json:"gemini_type"`
	Required   bool   `json:"required"`
}

func CurrentIronClawToolInputSchemas() []IronClawToolInputSchema {
	return []IronClawToolInputSchema{
		toolInputSchemaFromStruct("anvil_spawn_vm", SpawnVMInput{}),
		toolInputSchemaFromStruct("anvil_run_task", RunTaskInput{}),
		toolInputSchemaFromStruct("anvil_copy_in", CopyInInput{}),
		toolInputSchemaFromStruct("anvil_copy_out", CopyOutInput{}),
		toolInputSchemaFromStruct("anvil_get_vm_health", VMIdentityInput{}),
		toolInputSchemaFromStruct("anvil_stop_vm", VMIdentityInput{}),
		toolInputSchemaFromStruct("anvil_delete_vm", VMIdentityInput{}),
		toolInputSchemaFromStruct("anvil_create_snapshot", CreateSnapshotInput{}),
		{ToolName: "anvil_list_snapshots", Fields: nil},
		toolInputSchemaFromStruct("anvil_restore_snapshot", RestoreSnapshotInput{}),
		toolInputSchemaFromStruct("anvil_delete_snapshot", SnapshotIdentityInput{}),
	}
}

func ValidateIronClawToolInputSchemas(schemas []IronClawToolInputSchema) error {
	for _, schema := range schemas {
		if strings.TrimSpace(schema.ToolName) == "" {
			return fmt.Errorf("tool schema has empty tool name")
		}
		for _, field := range schema.Fields {
			if strings.TrimSpace(field.Name) == "" {
				return fmt.Errorf("tool %s has empty field name", schema.ToolName)
			}
			if strings.TrimSpace(field.GeminiType) == "" {
				return fmt.Errorf("tool %s field %s has empty Gemini type", schema.ToolName, field.Name)
			}
		}
	}
	return nil
}

func toolInputSchemaFromStruct(toolName string, value any) IronClawToolInputSchema {
	t := reflect.TypeOf(value)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var fields []IronClawToolInputField
	for idx := 0; idx < t.NumField(); idx++ {
		field := t.Field(idx)
		if !field.IsExported() {
			continue
		}
		jsonName, omitempty := jsonFieldName(field)
		if jsonName == "-" {
			continue
		}
		fields = append(fields, IronClawToolInputField{
			Name:       jsonName,
			GeminiType: geminiTypeFor(field.Type),
			Required:   !omitempty,
		})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return IronClawToolInputSchema{ToolName: toolName, Fields: fields}
}

func jsonFieldName(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name, false
	}
	parts := strings.Split(tag, ",")
	name := parts[0]
	if name == "" {
		name = field.Name
	}
	omitempty := false
	for _, part := range parts[1:] {
		if part == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty
}

func geminiTypeFor(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "STRING"
	case reflect.Bool:
		return "BOOLEAN"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "INTEGER"
	case reflect.Float32, reflect.Float64:
		return "NUMBER"
	case reflect.Slice, reflect.Array:
		return "ARRAY"
	case reflect.Struct, reflect.Map:
		return "OBJECT"
	default:
		return ""
	}
}
