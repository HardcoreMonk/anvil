package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
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

	envEphemeraAPITokens  = "EPHEMERA_API_TOKENS"
	envAnvilAPITokens     = "ANVIL_API_TOKENS"
	envEphemeraAPIToken   = "EPHEMERA_API_TOKEN"
	envAnvilAPIToken      = "ANVIL_API_TOKEN"
	envEphemeraTokensFile = "EPHEMERA_API_TOKENS_FILE"
)

// APIClient represents a named caller with its own Bearer token.
// Using separate tokens per client allows individual revocation and audit logging.
type APIClient struct {
	Name    string
	Token   string
	Expires time.Time // zero value = never expires (v0.4.1)
}

var (
	// agentPort is the port goose-agent listens on inside each VM.
	// Must match GOOSE_AGENT_PORT if overridden on the VM side.
	agentPort = resolveAgentPort()

	// maxTaskDepth caps nested agent→agent /tasks invocations (v0.4.4). The proxy
	// reads X-Ephemera-Task-Depth on each /tasks hop, rejects at/over the cap with
	// 508 Loop Detected, and forwards depth+1. Default 5; a large value effectively
	// disables the guard. envInt rejects <=0, so the default always holds.
	maxTaskDepth = envInt("EPHEMERA_MAX_TASK_DEPTH", 5)

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
	// Populated from EPHEMERA_API_TOKENS_FILE (preferred), EPHEMERA_API_TOKENS
	// /ANVIL_API_TOKENS (multi-client env), or EPHEMERA_API_TOKEN
	// /ANVIL_API_TOKEN (single-client fallback).
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
//	ANVIL_API_TOKENS=alice:token1,bob:token2
//
// Single-client fallback:
//
//	EPHEMERA_API_TOKEN=token
//	ANVIL_API_TOKEN=token
//
// Precedence:
//
//	EPHEMERA_API_TOKENS_FILE -> EPHEMERA_API_TOKENS -> ANVIL_API_TOKENS
//	-> EPHEMERA_API_TOKEN -> ANVIL_API_TOKEN
//
// The file source is re-read on every call (including SIGHUP via ReloadClients),
// which is what enables hot rotation. Env vars are captured at exec and cannot
// change without a daemon restart.
func loadAPIClients() []APIClient {
	if path := os.Getenv(envEphemeraTokensFile); path != "" {
		raw, err := os.ReadFile(path)
		if err == nil {
			return parseAPIClients(string(raw))
		}
		slog.Warn("api tokens file unreadable, falling back to env", "path", path, "err", err)
	}
	if raw := envWithAlias(envEphemeraAPITokens, envAnvilAPITokens); raw != "" {
		return parseAPIClients(raw)
	}
	if t := envWithAlias(envEphemeraAPIToken, envAnvilAPIToken); t != "" {
		// A ':' here almost always means the operator meant EPHEMERA_API_TOKENS
		// (plural, name:token) — the singular form uses the whole value as the
		// token and fixes the client name to "default". Warn rather than silently
		// surprising them in the Clients view.
		if strings.Contains(t, ":") {
			slog.Warn("EPHEMERA_API_TOKEN contains ':'; its whole value is used as the token and the client name is \"default\" — use EPHEMERA_API_TOKENS (plural) as name:token to set a client name")
		}
		return []APIClient{{Name: "default", Token: t}}
	}
	return nil
}

// parseAPIClients parses a raw token list. Entries are name:token[:expires]
// records separated by commas or newlines (operators write one-per-line in
// files; env stays CSV). Whitespace is trimmed; entries without a ':' or with
// an empty name/token are skipped silently.
//
// The name is everything before the FIRST colon. Both the token and the expiry
// (RFC3339 contains ':') may contain colons, so the expiry is found by scanning
// colon boundaries in the remainder left-to-right and taking the FIRST whose
// suffix parses as a timestamp (RFC3339 or Unix seconds, see parseExpiry). If no
// such boundary exists, the whole remainder is the token. The remainder is never
// itself tested as an expiry, so a token that merely looks like a timestamp
// stays a token. Examples:
//
//	alice:tok                          -> name=alice token=tok      never expires
//	alice:tok:en                       -> name=alice token=tok:en   never expires
//	alice:tok:en:2026-06-01T00:00:00Z  -> name=alice token=tok:en   expires 2026-06-01
//	alice:2026-06-01T00:00:00Z         -> name=alice token=<that>   never expires (no 3rd field)
//
// A two-field "name:token" has a zero Expires and never expires (back-compat).
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
		name := entry[:idx]
		rest := entry[idx+1:] // token, or token:expires
		token := rest
		var expires time.Time
		for i := 0; i < len(rest); i++ {
			if rest[i] != ':' {
				continue
			}
			if exp, ok := parseExpiry(rest[i+1:]); ok {
				token = rest[:i]
				expires = exp
				break
			}
		}
		if token == "" {
			continue
		}
		clients = append(clients, APIClient{Name: name, Token: token, Expires: expires})
	}
	return clients
}

func resolveAgentPort() int {
	return envIntWithAlias(envEphemeraAgentPort, envAnvilAgentPort, defaultAgentPort)
}

func resolvePublicURL() string {
	return strings.TrimRight(envWithAlias(envEphemeraPublicURL, envAnvilPublicURL), "/")
}

// parseExpiry parses a token-expiry field as RFC3339 or as Unix seconds.
// Unix seconds must be a plausible epoch (>= 2001-09) so a short numeric token
// segment is not mistaken for an expiry. Returns ok=false when the field is not
// a recognized timestamp, so the caller treats it as part of the token.
func parseExpiry(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n >= 1_000_000_000 {
		return time.Unix(n, 0).UTC(), true
	}
	return time.Time{}, false
}

func apiClientExpired(c APIClient, now time.Time) bool {
	return !c.Expires.IsZero() && !now.Before(c.Expires)
}

// firstActiveClient returns the first client whose token has not expired (a zero
// Expires never expires). The in-VM control-plane forwarder token is selected
// this way so an expired primary token does not break in-VM callbacks (v0.4.1).
func firstActiveClient(clients []APIClient) (APIClient, bool) {
	now := time.Now()
	for _, c := range clients {
		if !apiClientExpired(c, now) {
			return c, true
		}
	}
	return APIClient{}, false
}

// countTokenExpiry returns how many clients are already expired and how many
// (still valid) expire within the next 24h. Used for startup/SIGHUP banners.
func countTokenExpiry(clients []APIClient) (expired, expiringSoon int) {
	now := time.Now()
	soon := now.Add(24 * time.Hour)
	for _, c := range clients {
		if c.Expires.IsZero() {
			continue
		}
		if apiClientExpired(c, now) {
			expired++
		} else if c.Expires.Before(soon) {
			expiringSoon++
		}
	}
	return expired, expiringSoon
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

// bridgeCallbackPort returns the API port that flock VMs can reach through the
// bridge gateway. Loopback binds are intentionally rejected: an iptables allow
// rule cannot make a 127.0.0.1-only listener reachable from 10.0.1.1.
func bridgeCallbackPort(addr string) (int, bool) {
	host, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return 0, false
	}
	if host == "" {
		return port, true
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() {
		return 0, false
	}
	if ip.IsUnspecified() || ip.Equal(net.ParseIP("10.0.1.1")) {
		return port, true
	}
	return 0, false
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

// resolveDiskModeCOW decides the spawn disk strategy once at startup. As of the
// 2026-07-12 default flip (feat/default-cow-flip) anvil matches upstream ephemera:
// an unset EPHEMERA_DISK_MODE (and an explicit "cow") probes dm-snapshot support
// and enables COW when available, falling back to a plain full clone otherwise so
// spawns still succeed. Explicit "plain"/"full" force the full byte-for-byte clone
// without probing — this is the documented rollback path. When the default falls
// back it logs at info level (expected on hosts without dm-snapshot); an explicit
// "cow" that can't be honored still warns. probe is injected so the decision logic
// is unit-testable without real device-mapper.
func resolveDiskModeCOW(probe func() error) bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("EPHEMERA_DISK_MODE")))
	switch mode {
	case "plain", "full":
		return false
	case "", "cow":
		if err := probe(); err != nil {
			if mode == "cow" {
				slog.Warn("COW disk mode unavailable; falling back to plain full-clone", "err", err)
			} else {
				slog.Info("COW disk mode unavailable; falling back to plain full-clone", "err", err)
			}
			return false
		}
		return true
	default:
		slog.Warn("unknown EPHEMERA_DISK_MODE; falling back to plain full-clone", "mode", os.Getenv("EPHEMERA_DISK_MODE"))
		return false
	}
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

// agentProfiles holds only the reserved default. Every other profile is
// user-defined: created through the Settings UI as a configs/profiles/{name}
// directory and resolved by name. LookupProfile returns default sizing
// (1 vCPU / 1024 MiB, the "Standard" preset) with ProfileDir set to the requested
// name, so any user profile or flock role works without a hardcoded entry here.
var agentProfiles = map[string]AgentProfile{
	"": {Name: "default", VcpuCount: 1, MemSizeMib: 1024, ProfileDir: ""},
}

// LookupProfile returns the canonical AgentProfile for a known name, or a
// default-sized profile whose ProfileDir mirrors the requested name when the
// name is unknown. The latter preserves the legacy "any directory works" behavior
// so callers that supply ad-hoc profile directories keep functioning.
func LookupProfile(name string) AgentProfile {
	if p, ok := agentProfiles[name]; ok {
		return p
	}
	return AgentProfile{Name: name, VcpuCount: 1, MemSizeMib: 1024, ProfileDir: name}
}
