package mcpgateway

import "net/http"

// CredentialProvider injects a backend's authentication into an outbound request
// just before the gateway forwards a call to it. Credentials live only on the
// host (configs/mcp/secrets.yaml) and are never sent to a VM. A multi-host fork
// can back this with a central vault behind the same interface.
type CredentialProvider interface {
	// Inject adds the backend's auth header(s) to h. A backend with no configured
	// credential is a no-op (the remote server is unauthenticated or public).
	Inject(serverID string, h http.Header)
}

// mapCredentialProvider injects "Authorization: Bearer <token>" for each server
// that has a token. Built from servers.yaml (server → credential key) plus
// secrets.yaml (credential key → token).
type mapCredentialProvider struct {
	// tokens maps server id → bearer token.
	tokens map[string]string
}

// NewMapCredentialProvider builds a provider from a server-id→token map.
func NewMapCredentialProvider(tokens map[string]string) CredentialProvider {
	return mapCredentialProvider{tokens: tokens}
}

func (m mapCredentialProvider) Inject(serverID string, h http.Header) {
	if tok := m.tokens[serverID]; tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
}
