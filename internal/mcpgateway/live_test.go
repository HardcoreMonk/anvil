package mcpgateway

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestLive_DeepWiki exercises the HTTP MCP client against the public DeepWiki MCP
// server (https://mcp.deepwiki.com/mcp) — a real, no-auth Streamable HTTP backend.
// It validates the hand-rolled initialize/tools.list/tools.call path end-to-end
// against a real server. Skipped unless EPHEMERA_MCP_LIVE_TEST=1 because it needs
// network egress, so normal `go test ./...` stays hermetic.
func TestLive_DeepWiki(t *testing.T) {
	if os.Getenv("EPHEMERA_MCP_LIVE_TEST") != "1" {
		t.Skip("set EPHEMERA_MCP_LIVE_TEST=1 to run the live DeepWiki check")
	}
	b := NewHTTPBackend("deepwiki", "deepwiki", "https://mcp.deepwiki.com/mcp", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tools, err := b.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools against DeepWiki: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("expected DeepWiki to advertise at least one tool")
	}
	t.Logf("DeepWiki advertised %d tools:", len(tools))
	for _, tl := range tools {
		t.Logf("  - %s", tl.Name)
	}

	// A cheap, fast tool call: read_wiki_structure for a small well-known repo.
	args, _ := json.Marshal(map[string]any{"repoName": "modelcontextprotocol/servers"})
	res, err := b.CallTool(ctx, "read_wiki_structure", args)
	if err != nil {
		t.Fatalf("CallTool read_wiki_structure: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected a non-empty tool result")
	}
	t.Logf("read_wiki_structure result (first 240 bytes): %.240s", string(res))
}
