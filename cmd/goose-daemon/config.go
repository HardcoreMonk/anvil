package main

import (
	"fmt"
	"log/slog"
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

	// Watchdog tunables (v0.3.4). Defaults match the original hard-coded values
	// so existing deployments observe no change unless the env var is set.
	watchdogIntervalSec = envInt("EPHEMERA_WATCHDOG_INTERVAL_SEC", 5)
	watchdogTimeoutSec  = envInt("EPHEMERA_WATCHDOG_TIMEOUT_SEC", 1)
	watchdogThreshold   = envInt("EPHEMERA_WATCHDOG_THRESHOLD", 3)
	watchdogAutoHeal    = envBool("EPHEMERA_WATCHDOG_AUTO_HEAL", false)

	// metricsRequireAuth gates the /metrics endpoint behind the same Bearer
	// authentication as the rest of the API. Default false matches the
	// standard Prometheus scrape model (network-level isolation expected).
	metricsRequireAuth = envBool("EPHEMERA_METRICS_REQUIRE_AUTH", false)

	// diskMinFreeMiB is the free-space floor (MiB) that must remain available
	// after a disk-consuming operation (VM clone, snapshot copy). The daemon
	// refuses such an operation with 507 Insufficient Storage when it would dip
	// below this margin (v0.4.0). Default 1024 MiB.
	diskMinFreeMiB = envInt("EPHEMERA_DISK_MIN_FREE_MIB", 1024)

	// enableAutoSnapshot opts into memory auto-snapshot (v0.4.0): on graceful
	// shutdown the daemon snapshots each recoverable VM's memory+state into
	// vms/<id>/auto/ so RecoverVMs can warm-restore (memory preserved) instead
	// of cold-booting. One-shot per shutdown; falls back to cold boot on any
	// restore failure or a SIGKILL (no graceful teardown runs). Default off
	// (a 5-agent flock snapshot is ~10 GB).
	enableAutoSnapshot = envBool("EPHEMERA_AUTOSNAPSHOT", false)

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
	// Populated from EPHEMERA_API_TOKENS_FILE (preferred), EPHEMERA_API_TOKENS
	// (multi-client env), or EPHEMERA_API_TOKEN (single-client fallback).
	// Empty = authentication disabled.
	apiClients = loadAPIClients()
)

// loadAPIClients parses caller tokens. Precedence: file > multi-env > single-env > nil.
//
// File source (v0.3.4, preferred for rotation):
//
//	EPHEMERA_API_TOKENS_FILE=/etc/ephemera/tokens
//
// Multi-client env:
//
//	EPHEMERA_API_TOKENS=alice:token1,bob:token2
//
// Single-client fallback:
//
//	EPHEMERA_API_TOKEN=token
//
// The file source is re-read on every call (including SIGHUP via ReloadClients),
// which is what enables hot rotation. Env vars are captured at exec and cannot
// change without a daemon restart.
func loadAPIClients() []APIClient {
	if path := os.Getenv("EPHEMERA_API_TOKENS_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			return parseAPIClients(string(raw))
		}
		slog.Warn("api tokens file unreadable, falling back to env", "path", path, "err", err)
	}
	if raw := os.Getenv("EPHEMERA_API_TOKENS"); raw != "" {
		return parseAPIClients(raw)
	}
	if t := os.Getenv("EPHEMERA_API_TOKEN"); t != "" {
		return []APIClient{{Name: "default", Token: t}}
	}
	return nil
}

// parseAPIClients parses a raw token list. Entries are name:token pairs
// separated by commas or newlines (operators write one-per-line in files;
// env stays CSV). Whitespace is trimmed; entries without a ':' or with an
// empty name are skipped silently.
func parseAPIClients(raw string) []APIClient {
	var clients []APIClient
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n'
	}) {
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

// envBool returns true when the env var is set to a recognized truthy
// value, false for a recognized falsy value, and defaultVal otherwise.
// Recognized: "1"/"true"/"yes"/"on" and "0"/"false"/"no"/"off",
// case-insensitive. Unknown values fall back to defaultVal silently.
func envBool(key string, defaultVal bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return defaultVal
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
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
