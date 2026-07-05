package mcpgateway

import "encoding/json"

// defaultProtocolVersion is the MCP protocol version the gateway advertises to a
// backend when initializing, and the fallback it returns to goose when the client
// does not request a specific version. The gateway otherwise echoes the client's
// requested version to maximize compatibility.
const defaultProtocolVersion = "2025-03-26"

// gatewayServerName/Version identify this gateway in the MCP initialize handshake.
const (
	gatewayServerName    = "ephemera-mcp-gateway"
	gatewayServerVersion = "0.6.0"
)

// namespaceSep joins a backend namespace and a tool name in the aggregated
// catalog (e.g. "github__create_issue"). Namespaces are validated to exclude it,
// so the first occurrence reliably separates namespace from tool name.
const namespaceSep = "__"

// namespacedName prefixes a backend tool with its namespace.
func namespacedName(ns, tool string) string { return ns + namespaceSep + tool }

// splitNamespaced splits a namespaced tool name on its first separator. ok is
// false when the name carries no namespace prefix.
func splitNamespaced(name string) (ns, tool string, ok bool) {
	i := indexSep(name)
	if i < 0 {
		return "", "", false
	}
	return name[:i], name[i+len(namespaceSep):], true
}

// indexSep returns the index of the first namespaceSep in s, or -1.
func indexSep(s string) int {
	for i := 0; i+len(namespaceSep) <= len(s); i++ {
		if s[i:i+len(namespaceSep)] == namespaceSep {
			return i
		}
	}
	return -1
}

// Tool is one MCP tool as advertised by a backend (and, namespaced, by the
// gateway). InputSchema is passed through verbatim as a JSON Schema object.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// initializeParams is the subset of the MCP initialize request the gateway reads.
type initializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	ClientInfo      json.RawMessage `json:"clientInfo,omitempty"`
}

// initializeResult is the MCP initialize response. The gateway advertises only
// the tools capability (it aggregates tools; resources/prompts are out of scope
// for the single-host MVP).
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      serverInfo     `json:"serverInfo"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// toolsListResult is the MCP tools/list response.
type toolsListResult struct {
	Tools []Tool `json:"tools"`
}

// toolsCallParams is the MCP tools/call request: a (namespaced) tool name and
// its arguments object.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// gatewayCapabilities is the capability set the gateway advertises to goose.
func gatewayCapabilities() map[string]any {
	return map[string]any{
		// listChanged=false: the gateway's catalog is static for a session.
		"tools": map[string]any{"listChanged": false},
	}
}
