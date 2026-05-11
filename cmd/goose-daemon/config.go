package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultAPIPort   = 3000
	defaultAgentPort = 8080

	envEphemeraAgentPort = "EPHEMERA_AGENT_PORT"
	envAnvilAgentPort    = "ANVIL_AGENT_PORT"

	envEphemeraAPIAddr = "EPHEMERA_API_ADDR"
	envAnvilAPIAddr    = "ANVIL_API_ADDR"
	envEphemeraAPIPort = "EPHEMERA_API_PORT"
	envAnvilAPIPort    = "ANVIL_API_PORT"

	envEphemeraPublicURL = "EPHEMERA_PUBLIC_URL"
	envAnvilPublicURL    = "ANVIL_PUBLIC_URL"

	envEphemeraAPITokens = "EPHEMERA_API_TOKENS"
	envAnvilAPITokens    = "ANVIL_API_TOKENS"
	envEphemeraAPIToken  = "EPHEMERA_API_TOKEN"
	envAnvilAPIToken     = "ANVIL_API_TOKEN"
)

// APIClient represents a named caller with its own Bearer token.
// Using separate tokens per client allows individual revocation and audit logging.
type APIClient struct {
	Name  string
	Token string
}

var (
	// agentPort is the port goose-agent listens on inside each VM.
	// Must match GOOSE_AGENT_PORT if overridden on the VM side.
	agentPort = resolveAgentPort()

	// apiAddr is the address the control plane API binds to.
	// Default 127.0.0.1:3000 makes the API reachable only on localhost,
	// requiring a reverse proxy for external access.
	// Set EPHEMERA_API_ADDR=0.0.0.0:3000 or ANVIL_API_ADDR=0.0.0.0:3000
	// to bind on all interfaces.
	apiAddr = resolveAPIAddr()

	// publicURL is the externally-reachable base URL of the control plane
	// (no trailing slash). When set, agent_url in VM responses points to the
	// control plane proxy path ("{publicURL}/vms/{vm_id}") instead of the
	// VM's private IP. Example: "https://api.example.com"
	// Set via EPHEMERA_PUBLIC_URL or ANVIL_PUBLIC_URL env var.
	publicURL = resolvePublicURL()

	// apiClients is the set of authorized callers loaded once at startup.
	// Populated from EPHEMERA_API_TOKENS/ANVIL_API_TOKENS (multi-client)
	// or EPHEMERA_API_TOKEN/ANVIL_API_TOKEN (single-client fallback).
	// Empty = authentication disabled.
	apiClients = loadAPIClients()
)

// loadAPIClients parses caller tokens from environment variables.
//
// Multi-client (preferred):
//
//	EPHEMERA_API_TOKENS=alice:token1,bob:token2
//	ANVIL_API_TOKENS=alice:token1,bob:token2
//
// Single-client fallback:
//
//	EPHEMERA_API_TOKEN=token
//	ANVIL_API_TOKEN=token
//
// Precedence:
//
//	EPHEMERA_API_TOKENS -> ANVIL_API_TOKENS -> EPHEMERA_API_TOKEN -> ANVIL_API_TOKEN
func loadAPIClients() []APIClient {
	if raw := envWithAlias(envEphemeraAPITokens, envAnvilAPITokens); raw != "" {
		var clients []APIClient
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			idx := strings.Index(entry, ":")
			if idx <= 0 {
				continue
			}
			clients = append(clients, APIClient{
				Name:  entry[:idx],
				Token: entry[idx+1:],
			})
		}
		return clients
	}
	if t := envWithAlias(envEphemeraAPIToken, envAnvilAPIToken); t != "" {
		return []APIClient{{Name: "default", Token: t}}
	}
	return nil
}

func resolveAgentPort() int {
	return envIntWithAlias(envEphemeraAgentPort, envAnvilAgentPort, defaultAgentPort)
}

func resolvePublicURL() string {
	return strings.TrimRight(envWithAlias(envEphemeraPublicURL, envAnvilPublicURL), "/")
}

// resolveAPIAddr builds the listen address.
// EPHEMERA_API_ADDR/ANVIL_API_ADDR (full host:port) takes precedence over
// EPHEMERA_API_PORT/ANVIL_API_PORT (port only). EPHEMERA_* canonical values
// take precedence over ANVIL_* aliases.
func resolveAPIAddr() string {
	if v := envWithAlias(envEphemeraAPIAddr, envAnvilAPIAddr); v != "" {
		return v
	}
	return fmt.Sprintf("127.0.0.1:%d", envIntWithAlias(envEphemeraAPIPort, envAnvilAPIPort, defaultAPIPort))
}

func envWithAlias(canonicalKey, aliasKey string) string {
	if v := os.Getenv(canonicalKey); v != "" {
		return v
	}
	if aliasKey == "" {
		return ""
	}
	return os.Getenv(aliasKey)
}

func envInt(key string, defaultVal int) int {
	return envIntWithAlias(key, "", defaultVal)
}

func envIntWithAlias(canonicalKey, aliasKey string, defaultVal int) int {
	if v := envWithAlias(canonicalKey, aliasKey); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}
