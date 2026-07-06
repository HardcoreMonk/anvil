// Package mcpgateway implements a single-host MCP (Model Context Protocol)
// gateway: one MCP server endpoint that goose (the in-VM MCP client) connects to,
// which aggregates many backend MCP servers behind a single, policy-filtered,
// namespaced tool catalog. Backend credentials stay on the host and are never
// exposed to the ephemeral VMs.
//
// The pieces are wired through small interfaces (CallerResolver, Registry,
// PolicyStore, CredentialProvider, SessionStore) so a future multi-host build can
// re-implement them — distributed registry, shared sessions, token identity —
// without changing the protocol core or the VM-side contract.
package mcpgateway

import "encoding/json"

// jsonRPCVersion is the only JSON-RPC version MCP uses.
const jsonRPCVersion = "2.0"

// JSON-RPC 2.0 error codes used by the gateway (subset of the spec plus MCP's
// reserved range). InvalidParams/MethodNotFound/InternalError are standard.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcRequest is an inbound JSON-RPC message. A request carries an id; a
// notification omits it (id stays nil), and gets no response.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the message is a notification (no id → no reply).
func (r rpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// rpcResponse is an outbound JSON-RPC response. Exactly one of Result/Error is set.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// newResult builds a success response for the given id and marshaled result.
func newResult(id json.RawMessage, result any) rpcResponse {
	raw, err := json.Marshal(result)
	if err != nil {
		return newError(id, codeInternalError, "failed to encode result")
	}
	return rpcResponse{JSONRPC: jsonRPCVersion, ID: id, Result: raw}
}

// newError builds an error response for the given id.
func newError(id json.RawMessage, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: jsonRPCVersion, ID: id, Error: &rpcError{Code: code, Message: msg}}
}
