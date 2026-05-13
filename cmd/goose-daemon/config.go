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
	agentPort = envInt("EPHEMERA_AGENT_PORT", defaultAgentPort)

	// apiAddr is the address the control plane API binds to.
	// Default 127.0.0.1:3000 makes the API reachable only on localhost,
	// requiring a reverse proxy for external access.
	// Set EPHEMERA_API_ADDR=0.0.0.0:3000 to bind on all interfaces.
	apiAddr = resolveAPIAddr()

	// publicURL is the externally-reachable base URL of the control plane
	// (no trailing slash). When set, agent_url in VM responses points to the
	// control plane proxy path ("{publicURL}/vms/{vm_id}") instead of the
	// VM's private IP. Example: "https://api.example.com"
	// Set via EPHEMERA_PUBLIC_URL env var.
	publicURL = strings.TrimRight(os.Getenv("EPHEMERA_PUBLIC_URL"), "/")

	// apiClients is the set of authorized callers loaded once at startup.
	// Populated from EPHEMERA_API_TOKENS (multi-client) or EPHEMERA_API_TOKEN
	// (single-client fallback). Empty = authentication disabled.
	apiClients = loadAPIClients()
)

// loadAPIClients parses caller tokens from environment variables.
//
// Multi-client (preferred):
//
//	EPHEMERA_API_TOKENS=alice:token1,bob:token2
//
// Single-client (backward-compatible fallback):
//
//	EPHEMERA_API_TOKEN=token
func loadAPIClients() []APIClient {
	if raw := os.Getenv("EPHEMERA_API_TOKENS"); raw != "" {
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
	// Fall back to the legacy single-token variable.
	if t := os.Getenv("EPHEMERA_API_TOKEN"); t != "" {
		return []APIClient{{Name: "default", Token: t}}
	}
	return nil
}

// resolveAPIAddr builds the listen address.
// EPHEMERA_API_ADDR (full host:port) takes precedence over EPHEMERA_API_PORT (port only).
func resolveAPIAddr() string {
	if v := os.Getenv("EPHEMERA_API_ADDR"); v != "" {
		return v
	}
	return fmt.Sprintf("127.0.0.1:%d", envInt("EPHEMERA_API_PORT", defaultAPIPort))
}

func envInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}

// AgentProfile bundles per-role VM sizing and the on-disk profile directory.
// ProfileDir is resolved relative to {workDir}/configs/profiles/{ProfileDir}.
// An empty ProfileDir signals "use the daemon's default goose config".
type AgentProfile struct {
	Name       string
	VcpuCount  int64
	MemSizeMib int64
	ProfileDir string
}

// agentProfiles maps a profile name to its canonical sizing and profile directory.
// The empty key "" is the backward-compatible default returned for unset profiles.
// Unknown names fall back to default sizing with ProfileDir set to the name
// itself, preserving prior behavior where any directory under configs/profiles
// could be selected by name.
var agentProfiles = map[string]AgentProfile{
	"":             {Name: "default", VcpuCount: 2, MemSizeMib: 2048, ProfileDir: ""},
	"researcher":   {Name: "researcher", VcpuCount: 1, MemSizeMib: 512, ProfileDir: "researcher"},
	"reviewer":     {Name: "reviewer", VcpuCount: 1, MemSizeMib: 512, ProfileDir: "reviewer"},
	"worker":       {Name: "worker", VcpuCount: 2, MemSizeMib: 2048, ProfileDir: "worker"},
	"orchestrator": {Name: "orchestrator", VcpuCount: 2, MemSizeMib: 2048, ProfileDir: "orchestrator"},
	"builder":      {Name: "builder", VcpuCount: 4, MemSizeMib: 4096, ProfileDir: "worker"},
}

// LookupProfile returns the canonical AgentProfile for a known name, or a
// default-sized profile whose ProfileDir mirrors the requested name when the
// name is unknown. The latter preserves the legacy "any directory works" behavior
// so callers that supply ad-hoc profile directories keep functioning.
func LookupProfile(name string) AgentProfile {
	if p, ok := agentProfiles[name]; ok {
		return p
	}
	return AgentProfile{Name: name, VcpuCount: 2, MemSizeMib: 2048, ProfileDir: name}
}
