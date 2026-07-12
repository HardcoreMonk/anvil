package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"ephemera/internal/anvilmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRegisteredToolWireSchemasAreUnionFree enumerates the tools the anvil-mcp server
// actually advertises — built from the real toolRegistrations() the way main() wires
// them — and asserts that every advertised input schema is free of union ("type" as a
// JSON array) constructs, which Gemini's function-declaration validator rejects
// (see internal/anvilmcp: []T slices otherwise ship as {"type":["null","array"],...}).
//
// Unlike a hand-maintained (name, In-type) list, this walks the live registration set,
// so a tool added later — via AddToolGeminiSafe OR a plain mcp.AddTool that bypasses
// the normalizer — is automatically covered: it gets advertised and validated here.
func TestRegisteredToolWireSchemasAreUnionFree(t *testing.T) {
	tools := anvilmcp.NewTools(nil, anvilmcp.NewSessionStore(), time.Second)
	server := mcp.NewServer(&mcp.Implementation{Name: "anvil-mcp", Version: version}, nil)
	regs := toolRegistrations()
	for _, r := range regs {
		r.register(server, &mcp.Tool{Name: r.name, Description: r.description}, tools)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clientT, serverT := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "wire-schema-probe", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	advertised := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		advertised[tool.Name] = true
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("tool %s: marshal input schema: %v", tool.Name, err)
		}
		var node any
		if err := json.Unmarshal(raw, &node); err != nil {
			t.Fatalf("tool %s: unmarshal input schema: %v", tool.Name, err)
		}
		assertNoUnionType(t, tool.Name, "input", node)
	}

	// Count/name parity with the live registration list — catches a registration that
	// somehow fails to advertise (and, conversely, keeps this guard honest about the
	// set it is validating).
	if len(res.Tools) != len(regs) {
		t.Fatalf("advertised %d tools, but toolRegistrations() has %d", len(res.Tools), len(regs))
	}
	for _, r := range regs {
		if !advertised[r.name] {
			t.Errorf("registration %q is not advertised by the server", r.name)
		}
	}
}

// assertNoUnionType fails if any JSON Schema node advertises "type" as an array (a
// union such as ["null","array"]). Gemini requires a single type per node.
func assertNoUnionType(t *testing.T, tool, path string, node any) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		if typeVal, ok := v["type"]; ok {
			if arr, isArr := typeVal.([]any); isArr {
				t.Errorf("tool %s: %s advertises a union type %v; Gemini requires a single type "+
					"(rejected as `properties[..].items: field predicate failed: $type == Type.ARRAY`)",
					tool, path, arr)
			}
		}
		for k, sub := range v {
			assertNoUnionType(t, tool, path+"."+k, sub)
		}
	case []any:
		for i, sub := range v {
			assertNoUnionType(t, tool, fmt.Sprintf("%s[%d]", path, i), sub)
		}
	}
}
