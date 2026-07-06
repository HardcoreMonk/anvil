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

// initializeResult is the MCP initialize response. The gateway advertises the
// tools, resources, and prompts capabilities it aggregates from backends (v0.6.2).
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

// Resource is one MCP resource as advertised by a backend (and, with a
// namespaced URI, by the gateway). A resource is identified by its URI.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// resourcesListResult is the MCP resources/list response.
type resourcesListResult struct {
	Resources []Resource `json:"resources"`
}

// resourcesReadParams is the MCP resources/read request: a (namespaced) URI. The
// gateway namespaces resource URIs the same way it namespaces tool names, so the
// first separator splits the backend namespace from the backend's own URI.
type resourcesReadParams struct {
	URI string `json:"uri"`
}

// Prompt is one MCP prompt as advertised by a backend (and, namespaced, by the
// gateway). A prompt is identified by its name.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument is one argument a prompt template accepts.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// promptsListResult is the MCP prompts/list response.
type promptsListResult struct {
	Prompts []Prompt `json:"prompts"`
}

// promptsGetParams is the MCP prompts/get request: a (namespaced) prompt name and
// its optional arguments object.
type promptsGetParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// gatewayCapabilities is the capability set the gateway advertises to goose.
func gatewayCapabilities() map[string]any {
	return map[string]any{
		// listChanged=false: the gateway's catalog is static for a session.
		"tools":     map[string]any{"listChanged": false},
		"resources": map[string]any{"listChanged": false},
		"prompts":   map[string]any{"listChanged": false},
	}
}
