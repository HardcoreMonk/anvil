package main

// ===== 학습 노트 (anvil v0.5.x 학습용 주석, 참고 전용 브랜치) =====
// 이 파일은 daemon 전역 설정(포트, 토큰, watchdog 튜너블 등)과 AgentProfile/LookupProfile
// (per-role VM sizing) 을 함께 담는다. v0.5.x 관련 핵심은 맨 아래 AgentProfile /
// agentProfiles / LookupProfile 세 덩어리다.
//
// anvil 적응 포인트(sizing 기본값 전환): v0.5.3부터 anvil은 upstream 기본 VM sizing인
// 1 vCPU / 1024 MiB("Standard" 프리셋)를 채택했다(v0.5.3 이전엔 2 vCPU / 2048 MiB였다).
// KVM 3회 반복 e2e(316✓)가 1024 MiB에서 통과해 승인됐다. agentProfiles의 유일한 항목
// (""→default)과 LookupProfile의 unknown-name 폴백이 모두 1/1024를 반환하는 이유다.
//
// upstream-inherited sizing 갭(follow-up으로 남음): POST /vms(api.go의 spawnVM)는
// LookupProfile 기본값 위에 UI가 만든 profile의 goose.yaml에 박힌 EPHEMERA_VCPU_COUNT/
// EPHEMERA_MEM_SIZE_MIB(readProfileConfig 경유)를 얹어 override 하지만, flock 멤버
// spawn 경로는 이 override를 거치지 않고 LookupProfile 기본값만 사용한다 — 즉 flock으로
// 만든 에이전트는 아직 per-profile 커스텀 sizing을 받지 못한다(docs/operations의
// ephemera-v0.5-operator-sync-handoff.md "Known upstream-inherited gap" 참고).

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

// [학습] v0.5.5 System 콘솔(config_api.go의 handleConfigClients)이 리스트업하는
// "이름+만료"는 결국 이 함수가 만드는 APIClient 슬라이스에서 Token 필드만 잘라낸 것이다
// — 토큰 값 자체는 이 함수와 authMiddleware(api.go) 사이에서만 오가고 UI까지 나가지 않는다.
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

// [학습] v0.5.x sizing과는 별개 축이지만 같은 "anvil은 upstream 기본값을 그대로
// 따르지 않는다" 패턴을 보여준다: upstream은 COW(dm-snapshot) spawn을 기본으로 바꿨지만
// anvil은 KVM burn-in 검증 전까지 plain full-clone을 기본으로 유지하고, COW는
// EPHEMERA_DISK_MODE=cow로 명시적 opt-in해야만 켜진다.
// resolveDiskModeCOW decides the spawn disk strategy once at startup. anvil keeps
// the plain/full clone default; only EPHEMERA_DISK_MODE=cow opts into COW. When
// COW is requested but the host lacks dm-snapshot support, it logs a warning and
// falls back to plain so spawns still succeed. probe is injected so the decision
// logic is unit-testable without real device-mapper.
func resolveDiskModeCOW(probe func() error) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EPHEMERA_DISK_MODE"))) {
	case "", "plain", "full":
		return false
	case "cow":
		if err := probe(); err != nil {
			slog.Warn("COW disk mode unavailable; falling back to plain full-clone", "err", err)
			return false
		}
		return true
	default:
		slog.Warn("unknown EPHEMERA_DISK_MODE; falling back to plain full-clone", "mode", os.Getenv("EPHEMERA_DISK_MODE"))
		return false
	}
}

// [학습] Name은 로그/표시용, ProfileDir은 {workDir}/configs/profiles/{ProfileDir}로
// goose.yaml을 찾을 실제 디렉터리명이다. 빈 ProfileDir("")은 "디렉터리 없음, daemon
// 기본 goose.yaml 사용"을 뜻하며 defaultProfileName("default")과 짝을 이룬다.
// AgentProfile bundles per-role VM sizing and the on-disk profile directory.
// ProfileDir is resolved relative to {workDir}/configs/profiles/{ProfileDir}.
// An empty ProfileDir signals "use the daemon's default goose config".
type AgentProfile struct {
	Name       string
	VcpuCount  int64
	MemSizeMib int64
	ProfileDir string
}

// [학습] 이 map에는 "default" 하나만 하드코딩돼 있다 — v0.5.0 이전에는 flock role별로
// 여러 항목을 미리 등록해 뒀을 법한 구조지만, Settings UI로 프로필을 자유롭게 만들 수
// 있게 되면서(config_api.go의 createProfile) 정적 등록이 더 필요 없어졌다. 나머지
// 이름은 전부 LookupProfile의 unknown-name 분기가 처리한다.
// agentProfiles holds only the reserved default. Every other profile is
// user-defined: created through the Settings UI as a configs/profiles/{name}
// directory and resolved by name. LookupProfile returns default sizing
// (1 vCPU / 1024 MiB, the "Standard" preset) with ProfileDir set to the requested
// name, so any user profile or flock role works without a hardcoded entry here.
var agentProfiles = map[string]AgentProfile{
	"": {Name: "default", VcpuCount: 1, MemSizeMib: 1024, ProfileDir: ""},
}

// [학습] 이 함수 자체는 goose.yaml을 읽지 않는다 — 반환하는 VcpuCount/MemSizeMib는
// 항상 1/1024(기본값)이고, "실제 프로필별 sizing"은 호출자(api.go의 spawnVM)가
// LookupProfile 결과 위에 readProfileConfig(config_api.go)로 얻은 값을 덮어써서
// 얻는다. 이 두 단계 분리 때문에 "LookupProfile만 거치는 경로"(flock spawn)는 기본값
// 이상으로 커지지 못한다 — 파일 상단 학습 노트의 sizing 갭 설명 참고.
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
