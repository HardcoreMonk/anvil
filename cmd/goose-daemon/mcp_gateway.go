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

// 학습 주석 개요: 이 파일은 goose-daemon 프로세스가 internal/mcpgateway 를
// 실제로 배선하는 곳이다 — gateway 는 daemon 의 API mux 와 별도로 독립 리스너
// (mcpSrv, bridge IP mcpVMHostIP 에 바인딩)를 연다. EPHEMERA_MCP_* 환경변수가
// 이 파일의 진입점이며, ANVIL_MCP_* IronClaw adapter 환경변수와는 이름만
// 비슷할 뿐 완전히 다른 표면(cmd/anvil-mcp, docs/architecture/mcp-architecture.md
// 참고)이다. VM 에는 mcpEndpoint(letter-starting alias URL)만 주입되고
// (provisioner.go 의 MCPGatewayURL), audit(appendMCPAudit)은 고정 key set
// metadata 만 {workDir}/audit/mcp.jsonl 에 남긴다 — tool argument/result 는
// 절대 기록하지 않는다(mcp_audit_privacy_test.go 가드).

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
// 학습 주석: 기본값(mcpVMHostIP, 10.0.1.1)이 곧 gateway 를 외부 노출로부터
// 지키는 1차 방어선이고, identity.go 의 source-IP 403 판정이 defense-in-depth
// 로 겹친다(handoff 문서의 "KVM gate 스크립트" 절 참고).
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
// 학습 주석: 이 함수가 gateway.Options 를 실제로 조립하는 지점이다 —
// Resolver=NewIPCallerResolver(cp.lookupVMByIP), Policy=바인딩 교집합 store,
// Observe=cp.observeMCPCall(audit). WithStdioUser/WithStdioDir 로 stdio 강화
// (nobody, /var/lib/ephemera/mcp-stdio)도 여기서 registry 에 전달된다.
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
	cp.mcpSrv = &http.Server{Addr: fmt.Sprintf("%s:%d", mcpBindIP(), port), Handler: mux}
	slog.Warn("mcp gateway configured", "endpoint", cp.mcpEndpoint, "bind", cp.mcpSrv.Addr, "servers", len(servers))
}

// startMCPGateway starts the gateway listener (no-op when disabled). A bind
// failure is logged but must not take down the daemon — the control-plane API
// stays up so operators can diagnose it.
// 학습 주석: gateway 는 daemon 의 메인 API mux 와 다른 http.Server(cp.mcpSrv,
// 별도 bridge IP 리스너)이므로, 이 goroutine 이 죽어도 /config/* 등 daemon API
// 는 영향받지 않는다.
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
// 학습 주석: 이 함수의 반환값이 provisioner.go 의 MCPGatewayURL 로 흘러가
// VM 의 /root/.ephemera-mcp 에 그대로 쓰인다 — 여기서 만들어지는 것은 오직 URL
// 문자열뿐이고 credential 은 절대 이 경로에 섞이지 않는다.
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
// 학습 주석: internal/mcpgateway.VMLookup 의 실제 구현 — 이 함수가 "세션이 아닌
// source IP 로 신원을 판정한다"는 anvil 경계를 daemon 쪽에서 완성한다.
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
// 학습 주석: gateway.Options.Observe 로 주입되는 hook — mcpgateway 패키지가
// audit 저장 형식(JSONL, 파일 경로)을 몰라도 되게 host(daemon) 쪽에 위임한다.
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
// 학습 주석: 여기 기록되는 key 는 ts/vm/profile/server/kind/tool/outcome/ms
// 뿐이다 — rec.Err 원문이나 tool argument/result 는 이 함수에도, AuditRecord
// 자체에도 담기지 않는다(gateway.go 의 AuditRecord 정의 참고, sentinel 가드는
// mcp_audit_privacy_test.go).
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
