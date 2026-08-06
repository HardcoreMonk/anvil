package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ephemera/internal/mcpgateway"
)

const (
	defaultMCPPort = 3001
	// mcpStdioScratchBase is where stdio backend children get their per-server
	// cwd + HOME (v0.6.4). Deliberately NOT under cp.workDir: the children run
	// de-privileged and must be able to traverse into their dir, but the work
	// dir commonly lives under a 0700/0750 home directory the stdio user cannot
	// enter. /var/lib is root-owned and world-traversable, and survives reboots
	// (the dirs double as the servers' cache).
	mcpStdioScratchBase = "/var/lib/ephemera/mcp-stdio"
	// mcpVMHostIP is the bridge gateway IP every VM reaches the host at.
	mcpVMHostIP = "10.0.1.1"
	// mcpVMHostName is a letter-starting alias for the gateway used in the URL
	// injected into VMs. goose derives the streamable-HTTP extension name — and
	// thus the LLM-facing tool-name prefix (e.g. <name>__deepwiki__ask_question) —
	// by sanitizing the URL. An IP host yields a name starting with a digit, which
	// providers like Gemini reject ("function name must start with a letter or
	// underscore"). A letter-starting hostname avoids that; it resolves to
	// mcpVMHostIP via an /etc/hosts entry injected at spawn (the guest's nsswitch
	// is files→dns, so the entry is consulted first).
	mcpVMHostName = "ephemera-gw"
)

// mcpHostsEntry is the /etc/hosts line injected into a VM that uses the gateway,
// mapping the letter-starting mcpVMHostName to the bridge gateway IP.
func mcpHostsEntry() string { return mcpVMHostIP + " " + mcpVMHostName }

// mcpEnabled reports whether the MCP gateway is switched on via EPHEMERA_MCP_ENABLED.
func mcpEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EPHEMERA_MCP_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// mcpPort returns the gateway port (EPHEMERA_MCP_PORT, default 3001).
func mcpPort() int {
	if v := os.Getenv("EPHEMERA_MCP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMCPPort
}

// mcpBindIP returns the listener bind IP (EPHEMERA_MCP_BIND_IP, default the bridge
// gateway IP so the gateway is reachable only from VMs and the host, never
// externally).
func mcpBindIP() string {
	if v := strings.TrimSpace(os.Getenv("EPHEMERA_MCP_BIND_IP")); v != "" {
		return v
	}
	return mcpVMHostIP
}

// mcpRate returns the per-(VM, server) tool-call budget in calls/minute
// (EPHEMERA_MCP_RATE). 0 (default) = unlimited.
func mcpRate() int {
	if v := os.Getenv("EPHEMERA_MCP_RATE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// mcpBurst returns the token-bucket burst for the rate limiter
// (EPHEMERA_MCP_BURST). 0/unset → the limiter defaults it to the rate.
func mcpBurst() int {
	if v := os.Getenv("EPHEMERA_MCP_BURST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// mcpStdioUser returns the unprivileged user stdio backend subprocesses run as
// (EPHEMERA_MCP_STDIO_USER, default "nobody"). Only consulted when the daemon
// runs as root; otherwise children inherit the daemon's own user.
func mcpStdioUser() string {
	if v := strings.TrimSpace(os.Getenv("EPHEMERA_MCP_STDIO_USER")); v != "" {
		return v
	}
	return "nobody"
}

// initMCPGateway loads configs/mcp/{servers,secrets}.yaml and builds the gateway
// and its listener. Disabled or misconfigured → the gateway stays nil and VMs get
// no MCP extension (behavior unchanged). Failures are logged, never fatal.
func (cp *ControlPlane) initMCPGateway() {
	if !mcpEnabled() {
		return
	}
	dir := filepath.Join(cp.workDir, "configs", "mcp")
	servers, err := mcpgateway.LoadServers(filepath.Join(dir, "servers.yaml"))
	if err != nil {
		slog.Error("mcp gateway: load servers.yaml failed; gateway disabled", "err", err)
		return
	}
	secrets, err := mcpgateway.LoadSecrets(filepath.Join(dir, "secrets.yaml"))
	if err != nil {
		slog.Error("mcp gateway: load secrets.yaml failed; gateway disabled", "err", err)
		return
	}
	reg, err := mcpgateway.NewRegistry(servers, secrets, &http.Client{Timeout: 60 * time.Second},
		mcpgateway.WithStdioUser(mcpStdioUser()),
		mcpgateway.WithStdioDir(mcpStdioScratchBase))
	if err != nil {
		slog.Error("mcp gateway: invalid server config; gateway disabled", "err", err)
		return
	}
	// Policy intersects servers.yaml `profiles:` with each profile's explicit
	// EPHEMERA_MCP_SERVERS binding (read per request, so edits apply immediately).
	cp.mcpPolicy = mcpgateway.NewStaticPolicyStoreWithBinding(reg.ServerConfigs(), cp.mcpBindingForProfile)
	opts := mcpgateway.Options{
		Resolver: mcpgateway.NewIPCallerResolver(cp.lookupVMByIP),
		Registry: reg,
		Policy:   cp.mcpPolicy,
		Observe:  cp.observeMCPCall,
	}
	if r := mcpRate(); r > 0 {
		opts.Limiter = mcpgateway.NewTokenBucketLimiter(r, mcpBurst())
		slog.Warn("mcp gateway rate limit enabled", "calls_per_min", r, "burst", mcpBurst())
	}
	gw := mcpgateway.New(opts)
	port := mcpPort()
	cp.mcpRegistry = reg
	cp.mcpGateway = gw
	cp.mcpEndpoint = fmt.Sprintf("http://%s:%d/mcp", mcpVMHostName, port)

	mux := http.NewServeMux()
	mux.Handle("/mcp", gw)
	mux.Handle("/mcp/", gw)
	cp.mcpSrv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", mcpBindIP(), port),
		Handler:           mux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
	slog.Warn("mcp gateway configured", "endpoint", cp.mcpEndpoint, "bind", cp.mcpSrv.Addr, "servers", len(servers))
}

// startMCPGateway starts the gateway listener (no-op when disabled). A bind
// failure is logged but must not take down the daemon — the control-plane API
// stays up so operators can diagnose it.
func (cp *ControlPlane) startMCPGateway() {
	if cp.mcpSrv == nil {
		return
	}
	go func() {
		if err := cp.mcpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("mcp gateway listener stopped", "err", err)
		}
	}()
}

// stopMCPGateway gracefully shuts the gateway down (no-op when disabled):
// first the listener drains so no VM call is in flight, then the registry
// reaps any stdio backend subprocesses (v0.6.4).
func (cp *ControlPlane) stopMCPGateway() {
	if cp.mcpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cp.mcpSrv.Shutdown(ctx)
		cancel()
	}
	if cp.mcpRegistry != nil {
		cp.mcpRegistry.Close()
	}
}

// mcpURLForProfile returns the VM-facing gateway URL to inject for a profile, or
// "" when the gateway is off or the profile is permitted no backend server. This
// keeps a VM whose role needs no external tools from connecting to the gateway at
// all (no extra protocol overhead, no empty catalog).
func (cp *ControlPlane) mcpURLForProfile(profile string) string {
	if cp.mcpGateway == nil || cp.mcpRegistry == nil || cp.mcpPolicy == nil || cp.mcpEndpoint == "" {
		return ""
	}
	policy := cp.mcpPolicy.For(profile)
	for _, s := range cp.mcpRegistry.ServerConfigs() {
		if policy.Allows(s.ID) {
			return cp.mcpEndpoint
		}
	}
	return ""
}

// lookupVMByIP resolves a VM guest IP to its id and profile, for the gateway's
// source-IP caller identity. Read-locks cp.mu for a consistent snapshot.
func (cp *ControlPlane) lookupVMByIP(ip string) (string, string, bool) {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	for _, v := range cp.vms {
		if v.GuestIP == ip {
			return v.VMID, v.Profile, true
		}
	}
	return "", "", false
}

// observeMCPCall records one gateway tool call: a metric, an append to
// {workDir}/audit/mcp.jsonl, and a structured slog line. Arguments and results
// are never recorded (metadata only), matching the access-log privacy invariant.
func (cp *ControlPlane) observeMCPCall(rec mcpgateway.AuditRecord) {
	outcome := "ok"
	if !rec.OK {
		switch rec.Err {
		case "forbidden":
			outcome = "forbidden"
		case "rate limited":
			outcome = "rate_limited"
		default:
			outcome = "fail"
		}
	}
	if cp.metrics != nil {
		cp.metrics.mcpToolCalls.WithLabelValues(rec.Server, outcome).Inc()
	}
	cp.appendMCPAudit(rec, outcome)
	slog.Info("mcp tool call", "vm", rec.VMID, "profile", rec.Profile, "server", rec.Server, "tool", rec.Tool, "outcome", outcome, "ms", rec.DurationMs)
}

// appendMCPAudit appends one tool-call record to {workDir}/audit/mcp.jsonl.
// Best-effort (a failed open is silently skipped); O_APPEND keeps line-sized
// writes from concurrent calls from interleaving.
func (cp *ControlPlane) appendMCPAudit(rec mcpgateway.AuditRecord, outcome string) {
	dir := filepath.Join(cp.workDir, "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "mcp.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]any{
		"ts":      time.Now().UTC().Format(time.RFC3339),
		"vm":      rec.VMID,
		"profile": rec.Profile,
		"server":  rec.Server,
		"kind":    rec.Kind,
		"tool":    rec.Tool,
		"outcome": outcome,
		"ms":      rec.DurationMs,
	})
	_, _ = f.Write(append(line, '\n'))
}
