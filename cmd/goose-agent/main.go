package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// vsockReconfigPort is the well-known port for host→guest IP reconfiguration commands.
const vsockReconfigPort = 1234

type TaskRequest struct {
	Prompt string `json:"prompt"`
	// Session, when set, names a goose chat session so consecutive tasks on this
	// VM continue one conversation (multi-turn). Empty preserves the original
	// stateless one-shot behavior.
	Session string `json:"session,omitempty"`
}

type TaskResult struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

type WorkloadRunRequest struct {
	Script         string `json:"script"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type WorkloadRunResult struct {
	Script          string `json:"script"`
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	DurationMs      int64  `json:"duration_ms"`
	TimedOut        bool   `json:"timed_out"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

var (
	mu           sync.Mutex
	busy         bool
	srv          *http.Server
	agentTokenMu sync.RWMutex
	agentToken   string
)

// sessionInfo is the agent's in-memory record of a goose chat session created on
// this VM. It lives only in memory, but VM memory snapshots capture it, so a
// snapshot-restored VM keeps its conversations and the Web UI can list/resume
// them (GET /sessions). The conversation itself lives in goose's own on-disk
// session file, which the snapshot's rootfs copy preserves in lockstep.
type sessionInfo struct {
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"created_at"`
	Title      string    `json:"title"`       // first user prompt, trimmed to a label
	Turns      int       `json:"turns"`       // completed task turns on this session
	LastOutput string    `json:"last_output"` // last cumulative goose output; lets the UI resume without re-dumping the transcript
	// transcript caches the full conversation captured on the last task turn
	// (goose --resume re-emits the whole transcript). Unexported so the /sessions
	// list stays lightweight; GET /sessions/{name}/transcript serves it, falling
	// back to `goose session export` on a cold-restart cache miss. A VM memory
	// snapshot captures it, so a snapshot-restored VM keeps the rendered history.
	transcript []TranscriptTurn
}

// sessions records goose chat sessions created on this VM so the second and later
// turns pass --resume and the Web UI can list/resume them. Guarded by sessionMu.
var (
	sessionMu sync.Mutex
	sessions  = map[string]*sessionInfo{}
)

// gooseArgs builds the goose run argv. With no session it preserves the original
// stateless invocation; with a session it names the session and (on the second+
// turn) resumes it so the conversation persists across tasks — multi-turn.
//
// --no-profile drops goose's default builtin extensions (8 of them, ~7.5k tokens
// of tool definitions on every request); the profile's selected builtins are
// added back via a single comma-separated --with-builtin so only the tools that
// role needs are loaded. Trimming the tool schema keeps each request small —
// otherwise the per-request token count can exceed a provider's per-minute budget
// (Groq free-tier TPM is 6000), which goose misreports as a context overflow and
// aborts with a spurious "failed to compact" error. An empty builtins list omits
// --with-builtin entirely (a tools-free, chat-only agent).
//
// When mcpURL is set (the host injected /root/.ephemera-mcp), the host MCP gateway
// is added as a streamable-HTTP extension so the agent's tools also include the
// gateway's aggregated, profile-filtered catalog of external MCP servers.
func gooseArgs(builtins []string, mcpURL, session string, resume bool) []string {
	args := []string{"run", "--output-format", "json", "--no-profile"}
	if len(builtins) > 0 {
		args = append(args, "--with-builtin", strings.Join(builtins, ","))
	}
	if mcpURL != "" {
		args = append(args, "--with-streamable-http-extension", mcpURL)
	}
	if session != "" {
		args = append(args, "-n", session)
		if resume {
			args = append(args, "--resume")
		}
	}
	return append(args, "-i", "-")
}

// loadMCPURL returns the host MCP gateway URL the daemon injected at
// /root/.ephemera-mcp, or "" when the gateway is off (file absent) or the profile
// has no permitted backends. Mirrors loadBuiltins' read-from-config-at-runtime
// approach so no rebake is needed to change which VMs use the gateway.
func loadMCPURL() string {
	data, err := os.ReadFile(mcpConfigPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// loadBuiltins reads EPHEMERA_BUILTINS (a comma-separated builtin list) from the
// VM's goose config and returns the builtin extensions to enable. The host writes
// this key per profile. A missing/unreadable key falls back to ["developer"], the
// historical hardcoded default, so profiles and snapshots created before per-
// profile builtin selection are unchanged; an explicit empty value yields no
// builtins. Mirrors the daemon's parseProfileBuiltins.
func loadBuiltins() []string {
	data, err := os.ReadFile(gooseConfigPath)
	if err != nil {
		return []string{"developer"}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "EPHEMERA_BUILTINS:") {
			continue
		}
		val := strings.Trim(strings.TrimSpace(line[len("EPHEMERA_BUILTINS:"):]), `"'`)
		out := []string{}
		for _, p := range strings.Split(val, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return []string{"developer"}
}

// noThinkForModel returns "/nothink\n" for qwen reasoning models, else "". qwen3
// emits thinking blocks that goose persists as reasoning_content and replays on
// --resume; Groq rejects reasoning_content in request messages with a 400
// ("property 'reasoning_content' is unsupported"), which breaks every multi-turn
// follow-up. The "/nothink" directive disables qwen thinking so none is produced.
// It is plain text other models ignore, so it is safe to prepend unconditionally
// for qwen and never for anything else.
func noThinkForModel(model string) string {
	if strings.Contains(strings.ToLower(model), "qwen") {
		return "/nothink\n"
	}
	return ""
}

// noThinkPrefix reads GOOSE_MODEL from the VM's goose config and returns the
// no-think directive for it, or "" when the config is unreadable or the model
// is not a reasoning model.
func noThinkPrefix() string {
	data, err := os.ReadFile(gooseConfigPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "GOOSE_MODEL:") {
			continue
		}
		model := strings.Trim(strings.TrimSpace(line[len("GOOSE_MODEL:"):]), `"'`)
		return noThinkForModel(model)
	}
	return ""
}

// sessionTitle derives a short, single-line label for a session from its first
// user prompt, for the Web UI's conversation picker. Rune-safe truncation.
func sessionTitle(prompt string) string {
	t := strings.TrimSpace(strings.ReplaceAll(prompt, "\n", " "))
	const max = 60
	if r := []rune(t); len(r) > max {
		return string(r[:max]) + "…"
	}
	return t
}

// validSessionName allows only filesystem/CLI-safe session identifiers (goose
// stores one file per session). The Web UI generates "<vm-id>-<timestamp>".
func validSessionName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func agentListenAddr() string {
	port := 8080
	if v := os.Getenv("GOOSE_AGENT_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}
	return fmt.Sprintf(":%d", port)
}

const (
	agentTokenPath         = "/root/.ephemera-agent-token"
	workspaceRoot          = "/workspace"
	flockMetaPath          = "/root/.ephemera-flock"
	systemPromptPath       = "/root/.goose-system-prompt"
	cpTokenPath            = "/root/.ephemera-cp-token"
	mcpConfigPath          = "/root/.ephemera-mcp"             // host MCP gateway URL; added as a goose streamable-HTTP extension
	gooseConfigPath        = "/root/.config/goose/config.yaml" // provider/model; read to detect reasoning models
	maxWorkspaceFileBytes  = 4 << 20
	townWallPostTimeout    = 10 * time.Second
	workloadDirName        = "workloads"
	workloadScriptSuffix   = ".sh"
	defaultWorkloadTimeout = 600 * time.Second
	maxWorkloadTimeout     = 1800 * time.Second
	maxWorkloadOutputBytes = 1 << 20
	// defaultControlPlaneAddr is the gateway IP the host uses inside the VM's
	// /24 network. Overridable via EPHEMERA_CONTROL_PLANE for testing.
	defaultControlPlaneAddr = "http://10.0.1.1:3000"
)

// initSlog configures the global slog default handler from EPHEMERA_LOG_FORMAT
// (text|json) and EPHEMERA_LOG_LEVEL (debug|info|warn|error), mirroring the
// host daemon (cmd/goose-daemon). Default level is Warn so the lifecycle lines
// this binary historically printed via log.Printf stay visible without config.
// The env vars only take effect when the control plane injects them into the
// VM environment; absent, the Warn default applies.
func initSlog() {
	level := slog.LevelWarn
	switch strings.ToLower(os.Getenv("EPHEMERA_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(os.Getenv("EPHEMERA_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

// fatal logs at Error and exits non-zero (mirrors the host daemon's fatal).
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

// loadAgentToken reads the per-VM Bearer token written by the control plane at VM provision time.
// Returns an empty string (auth disabled) if the file does not exist — backward compatible with
// golden images that predate this feature.
func loadAgentToken() string {
	b, err := os.ReadFile(agentTokenPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("could not read agent token file", "err", err)
		}
		return ""
	}
	return strings.TrimSpace(string(b))
}

func currentAgentToken() string {
	agentTokenMu.RLock()
	defer agentTokenMu.RUnlock()
	return agentToken
}

func setCurrentAgentToken(token string) {
	agentTokenMu.Lock()
	agentToken = strings.TrimSpace(token)
	agentTokenMu.Unlock()
}

// loadFlockMeta parses /root/.ephemera-flock if present. Returns ("", "") when
// the VM is running as a standalone agent (no flock context).
func loadFlockMeta() (flockID, agentID string) {
	b, err := os.ReadFile(flockMetaPath)
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "FLOCK_ID":
			flockID = val
		case "AGENT_ID":
			agentID = val
		}
	}
	return
}

// loadCPToken returns the bearer the in-VM /townwall/post forwarder uses
// when calling back into the control plane. Prefers the host-injected file
// (for a flock member, that flock's guest capability token); falls back to
// EPHEMERA_CONTROL_PLANE_TOKEN for older golden images that predate v0.3.3.
// Returns "" when neither is set (auth disabled mode). The guest never inspects
// the value — it forwards whatever the file holds.
func loadCPToken() string {
	if b, err := os.ReadFile(cpTokenPath); err == nil {
		return strings.TrimSpace(string(b))
	}
	return os.Getenv("EPHEMERA_CONTROL_PLANE_TOKEN")
}

// loadSystemPrompt returns the role's system prompt or "" when absent.
// Trailing whitespace is preserved as authors of system.md files generally
// terminate with a newline that does not affect prompting.
func loadSystemPrompt() string {
	b, err := os.ReadFile(systemPromptPath)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

// controlPlaneAddr returns the URL of the host control plane reachable from
// inside the VM. EPHEMERA_CONTROL_PLANE overrides the default for testing.
func controlPlaneAddr() string {
	if v := os.Getenv("EPHEMERA_CONTROL_PLANE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultControlPlaneAddr
}

// agentAuthMiddleware protects next with Bearer token auth.
// If token is empty, auth is disabled. /health is never wrapped with this middleware
// so the control plane's waitForAgent poller can reach it without a token.
func agentAuthMiddleware(token string, next http.HandlerFunc) http.HandlerFunc {
	return agentAuthMiddlewareWithTokenProvider(func() string { return token }, next)
}

func agentAuthMiddlewareWithTokenProvider(tokenProvider func() string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if tokenProvider != nil {
			token = tokenProvider()
		}
		if token == "" {
			next(w, r)
			return
		}
		auth := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(auth, []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="goose-agent"`)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// newAgentServer builds the in-guest agent's HTTP listener.
//
// ReadHeaderTimeout and IdleTimeout both apply before any handler — and thus
// before agentAuthMiddleware's bearer check — runs. Left at net/http's zero value
// they mean "no limit", so a peer on the VM network with no valid token could pin
// goroutines and fds indefinitely.
//
// ReadTimeout and WriteTimeout stay unset on purpose: /tasks streams NDJSON for
// the whole life of a task and /workspace accepts large uploads, both of which a
// global request/response budget would sever.
func newAgentServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func main() {
	initSlog()
	token := loadAgentToken()
	setCurrentAgentToken(token)
	if token == "" {
		slog.Warn("no agent token found, authentication disabled")
	} else {
		slog.Warn("goose-agent token auth enabled")
	}

	startVsockListener()

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", agentAuthMiddlewareWithTokenProvider(currentAgentToken, handleTask))
	mux.HandleFunc("/workspace", agentAuthMiddlewareWithTokenProvider(currentAgentToken, workspaceHandler(workspaceRoot)))
	mux.HandleFunc("/workloads/run", agentAuthMiddlewareWithTokenProvider(currentAgentToken, workloadRunHandler(workspaceRoot)))
	mux.HandleFunc("/stop", agentAuthMiddlewareWithTokenProvider(currentAgentToken, handleStop))
	mux.HandleFunc("/townwall/post", agentAuthMiddlewareWithTokenProvider(currentAgentToken, handleTownWallPost))
	mux.HandleFunc("/sessions", agentAuthMiddlewareWithTokenProvider(currentAgentToken, handleSessions))
	mux.HandleFunc("/sessions/", agentAuthMiddlewareWithTokenProvider(currentAgentToken, handleSessionItem))
	mux.HandleFunc("/health", handleHealth) // always unauthenticated

	addr := agentListenAddr()
	srv = newAgentServer(addr, mux)
	slog.Warn("goose-agent ready", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal("server error", "err", err)
	}
}

// startVsockListener opens an AF_VSOCK socket on vsockReconfigPort and accepts
// CHANGE_IP commands from the host control plane. Used after snapshot restore to
// reconfigure eth0 without rebooting, decoupling the guest IP from the snapshot state.
func startVsockListener() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		slog.Warn("vsock unavailable, post-restore IP reconfiguration disabled", "err", err)
		return
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: vsockReconfigPort}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		slog.Warn("vsock bind failed", "err", err)
		return
	}
	if err := unix.Listen(fd, 4); err != nil {
		unix.Close(fd)
		slog.Warn("vsock listen failed", "err", err)
		return
	}
	slog.Warn("vsock reconfig listener ready", "port", vsockReconfigPort)
	go func() {
		for {
			connFd, _, err := unix.Accept(fd)
			if err != nil {
				slog.Warn("vsock accept failed", "err", err)
				continue
			}
			go handleVsockConn(connFd)
		}
	}()
}

// handleVsockConn processes a single vsock connection from the host.
// Protocol: newline-delimited "<COMMAND> [args...]\n" → "OK\n" or "ERROR: ...\n".
// Supported commands:
//   - CHANGE_IP <cidr_ip> <gateway>     — reconfigure eth0 after snapshot restore
//   - SET_CP_TOKEN <token>              — atomically rewrite /root/.ephemera-cp-token (v0.3.4)
//   - SET_AGENT_TOKEN <token>           — update HTTP bearer after replicated snapshot restore
func handleVsockConn(fd int) {
	defer unix.Close(fd)
	f := os.NewFile(uintptr(fd), "vsock-conn")
	defer f.Close()

	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) == 0 {
		fmt.Fprintf(f, "ERROR: empty command\n")
		return
	}

	switch parts[0] {
	case "CHANGE_IP":
		if len(parts) != 3 {
			fmt.Fprintf(f, "ERROR: expected CHANGE_IP <cidr_ip> <gateway>\n")
			return
		}
		cidrIP, gateway := parts[1], parts[2]
		if err := applyIPConfig(cidrIP, gateway); err != nil {
			fmt.Fprintf(f, "ERROR: %v\n", err)
			slog.Warn("vsock CHANGE_IP failed", "err", err)
			return
		}
		fmt.Fprintf(f, "OK\n")
		slog.Warn("IP reconfigured", "ip", cidrIP, "gateway", gateway)

	case "SET_CP_TOKEN":
		if len(parts) != 2 {
			fmt.Fprintf(f, "ERROR: expected SET_CP_TOKEN <token>\n")
			return
		}
		if err := writeCPTokenAtomic(parts[1]); err != nil {
			fmt.Fprintf(f, "ERROR: %v\n", err)
			slog.Warn("vsock SET_CP_TOKEN failed", "err", err)
			return
		}
		fmt.Fprintf(f, "OK\n")
		slog.Warn("CP token updated via vsock")

	case "SET_AGENT_TOKEN":
		if len(parts) != 2 {
			fmt.Fprintf(f, "ERROR: expected SET_AGENT_TOKEN <token>\n")
			return
		}
		if err := writeAgentTokenAtomic(parts[1]); err != nil {
			fmt.Fprintf(f, "ERROR: %v\n", err)
			slog.Warn("vsock SET_AGENT_TOKEN failed", "err", err)
			return
		}
		setCurrentAgentToken(parts[1])
		fmt.Fprintf(f, "OK\n")
		slog.Warn("agent token updated via vsock")

	default:
		fmt.Fprintf(f, "ERROR: unknown command %q\n", parts[0])
	}
}

func writeAgentTokenAtomic(token string) error {
	tmp := agentTokenPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, agentTokenPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeCPTokenAtomic rewrites /root/.ephemera-cp-token with the supplied bearer.
// Uses tmp-and-rename so a reader (the per-request loadCPToken in handleTownWallPost)
// never observes a partial write. Mode is 0600 to match the original injectVMFiles write.
func writeCPTokenAtomic(token string) error {
	tmp := cpTokenPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, cpTokenPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// applyIPConfig reconfigures eth0 with a new IP/mask and default gateway.
// The goose-agent HTTP server binds to ":<port>" (all interfaces) so no rebind is needed.
// PATH is set explicitly because after snapshot restore the process environment may not
// propagate correctly to exec.Command's PATH lookup.
func applyIPConfig(cidrIP, gateway string) error {
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	for _, args := range [][]string{
		{"ip", "addr", "flush", "dev", "eth0"},
		{"ip", "addr", "add", cidrIP, "dev", "eth0"},
		{"ip", "link", "set", "eth0", "up"},
		{"ip", "route", "replace", "default", "via", gateway},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

type WorkspaceWriteResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type truncatingBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *truncatingBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = true
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *truncatingBuffer) String() string { return b.buf.String() }

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{Error: message})
}

func workspaceFilePath(root, relPath string) (string, error) {
	relPath = strings.TrimSpace(relPath)
	if relPath == "" || filepath.IsAbs(relPath) {
		return "", fmt.Errorf("path must be a non-empty relative path")
	}

	clean := filepath.Clean(relPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path must stay within workspace")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	fullPath, err := filepath.Abs(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	if fullPath != rootAbs && !strings.HasPrefix(fullPath, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("path must stay within workspace")
	}
	return fullPath, nil
}

func workloadScriptPath(root, script string) (fullPath, clean string, status int, err error) {
	script = strings.TrimSpace(script)
	if script == "" || filepath.IsAbs(script) {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must be a non-empty relative path")
	}

	clean = filepath.Clean(script)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, string(os.PathSeparator)+".."+string(os.PathSeparator)) {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must stay within workspace workloads")
	}
	if !strings.HasPrefix(clean, workloadDirName+string(os.PathSeparator)) && !strings.HasPrefix(clean, workloadDirName+"/") {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must be under workloads/")
	}
	if !strings.HasSuffix(clean, workloadScriptSuffix) {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must end with .sh")
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", http.StatusInternalServerError, fmt.Errorf("resolve workspace root: %w", err)
	}
	workloadRoot := filepath.Join(rootAbs, workloadDirName)
	fullPath, err = filepath.Abs(filepath.Join(rootAbs, clean))
	if err != nil {
		return "", "", http.StatusInternalServerError, fmt.Errorf("resolve workload script: %w", err)
	}
	if fullPath != workloadRoot && !strings.HasPrefix(fullPath, workloadRoot+string(os.PathSeparator)) {
		return "", "", http.StatusBadRequest, fmt.Errorf("script must stay within workloads/")
	}
	if status, err := validateWorkloadParentDirs(workloadRoot, filepath.Dir(fullPath)); err != nil {
		return "", "", status, err
	}

	info, err := os.Lstat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", http.StatusNotFound, fmt.Errorf("workload script not found")
		}
		return "", "", http.StatusInternalServerError, fmt.Errorf("stat workload script: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", http.StatusBadRequest, fmt.Errorf("workload script must be a regular file")
	}
	return fullPath, filepath.ToSlash(clean), http.StatusOK, nil
}

func validateWorkloadParentDirs(workloadRoot, scriptDir string) (int, error) {
	rel, err := filepath.Rel(workloadRoot, scriptDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return http.StatusBadRequest, fmt.Errorf("script must stay within workloads/")
	}

	current := workloadRoot
	if status, err := requireRealWorkloadDir(current); err != nil {
		return status, err
	}
	if rel == "." {
		return http.StatusOK, nil
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if status, err := requireRealWorkloadDir(current); err != nil {
			return status, err
		}
	}
	return http.StatusOK, nil
}

func requireRealWorkloadDir(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return http.StatusNotFound, fmt.Errorf("workload script not found")
		}
		return http.StatusInternalServerError, fmt.Errorf("stat workload directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return http.StatusBadRequest, fmt.Errorf("workload script parent must be a real directory")
	}
	return http.StatusOK, nil
}

func workloadTimeoutSeconds(seconds int) (time.Duration, error) {
	if seconds == 0 {
		return defaultWorkloadTimeout, nil
	}
	if seconds < 1 {
		return 0, fmt.Errorf("timeout_seconds must be >= 1")
	}
	timeout := time.Duration(seconds) * time.Second
	if timeout > maxWorkloadTimeout {
		return 0, fmt.Errorf("timeout_seconds must be <= %d", int(maxWorkloadTimeout/time.Second))
	}
	return timeout, nil
}

func workspaceOverwriteAllowed(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("overwrite")), "true")
}

func workspaceHandler(root string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fullPath, err := workspaceFilePath(root, r.URL.Query().Get("path"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWorkspaceFileBytes))
			if err != nil {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "workspace file exceeds size limit")
				return
			}
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("create parent directory: %v", err))
				return
			}

			flags := os.O_WRONLY | os.O_CREATE
			if workspaceOverwriteAllowed(r) {
				flags |= os.O_TRUNC
			} else {
				flags |= os.O_EXCL
			}
			out, err := os.OpenFile(fullPath, flags, 0644)
			if err != nil {
				if os.IsExist(err) {
					writeJSONError(w, http.StatusConflict, "workspace file already exists")
					return
				}
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("create workspace file: %v", err))
				return
			}
			n, copyErr := out.Write(data)
			closeErr := out.Close()
			if copyErr != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("write workspace file: %v", copyErr))
				return
			}
			if closeErr != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("close workspace file: %v", closeErr))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(WorkspaceWriteResult{
				Path:  filepath.ToSlash(strings.TrimPrefix(fullPath, filepath.Clean(root)+string(os.PathSeparator))),
				Bytes: int64(n),
			})

		case http.MethodGet:
			in, err := os.Open(fullPath)
			if err != nil {
				if os.IsNotExist(err) {
					writeJSONError(w, http.StatusNotFound, "workspace file not found")
					return
				}
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("open workspace file: %v", err))
				return
			}
			defer in.Close()
			info, err := in.Stat()
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("stat workspace file: %v", err))
				return
			}
			if info.Size() > maxWorkspaceFileBytes {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "workspace file exceeds size limit")
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			if _, err := io.Copy(w, in); err != nil {
				slog.Warn("workspace response copy failed", "err", err)
			}

		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "GET or PUT required")
		}
	}
}

// handleSessions lists the goose chat sessions created on this VM, newest first.
// The registry is in memory but VM snapshots capture it, so a snapshot-restored
// VM reports the conversations frozen in the snapshot and the Web UI can resume
// them. Authenticated like /tasks (titles are derived from user prompts).
func handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	sessionMu.Lock()
	list := make([]sessionInfo, 0, len(sessions))
	for _, s := range sessions {
		list = append(list, *s)
	}
	sessionMu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// handleSessionItem serves GET /sessions/{name}/transcript: the full conversation
// for a resumed chat. It returns the transcript cached on the last task turn, or
// falls back to `goose session export` (read-only, no model call) on a cold-restart
// cache miss. On any failure it returns an empty transcript with 200 so the UI
// degrades gracefully (the conversation still resumes; only prior turns are blank).
func handleSessionItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/sessions/")
	name, sub, _ := strings.Cut(rest, "/")
	if name == "" || sub != "transcript" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !validSessionName(name) {
		http.Error(w, "invalid session", http.StatusBadRequest)
		return
	}

	sessionMu.Lock()
	var turns []TranscriptTurn
	if s, ok := sessions[name]; ok {
		turns = s.transcript
	}
	sessionMu.Unlock()

	if len(turns) == 0 {
		// Cache miss (e.g. a cold restart cleared the in-memory session map): dump
		// the conversation from goose's on-disk session store.
		if exported, err := exportSessionTranscript(r.Context(), name); err != nil {
			slog.Warn("session transcript export failed", "session", name, "err", err)
		} else {
			turns = exported
		}
	}
	if turns == nil {
		turns = []TranscriptTurn{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]TranscriptTurn{"turns": turns})
}

func handleTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		http.Error(w, "prompt required", http.StatusBadRequest)
		return
	}
	if req.Session != "" && !validSessionName(req.Session) {
		http.Error(w, "invalid session", http.StatusBadRequest)
		return
	}

	mu.Lock()
	if busy {
		mu.Unlock()
		http.Error(w, "agent busy", http.StatusServiceUnavailable)
		return
	}
	busy = true
	mu.Unlock()
	defer func() {
		mu.Lock()
		busy = false
		mu.Unlock()
	}()

	finalPrompt := req.Prompt
	if sysPrompt := loadSystemPrompt(); sysPrompt != "" {
		// Prepend the role system prompt as plain text. Goose CLI has no
		// first-class system-prompt flag, so we delimit with headers a model
		// can recognize and ignore the prefix as instructions.
		finalPrompt = "[SYSTEM INSTRUCTIONS]\n" + sysPrompt + "\n\n[USER TASK]\n" + req.Prompt
	}
	// Disable qwen reasoning so thinking blocks don't break multi-turn --resume:
	// goose replays them as reasoning_content, which Groq rejects with a 400.
	finalPrompt = noThinkPrefix() + finalPrompt

	// --output-format json bypasses goose-cli's streaming markdown buffer, whose
	// truncate_code_blocks() unconditionally caps fenced code at 50 lines and
	// spills the overflow into /tmp/goose-*.txt — a path the host caller cannot
	// reach across the HTTP boundary. Neither --debug nor GOOSE_SHOW_FULL_OUTPUT
	// disables that cap; the JSON path is the only code-level escape because
	// session/mod.rs gates the markdown buffer flush behind !is_json_mode.
	// With a session name the conversation persists across turns (multi-turn):
	// the first turn creates the named session, later turns --resume it. No
	// session → the original stateless one-shot invocation.
	resume := false
	if req.Session != "" {
		sessionMu.Lock()
		if _, ok := sessions[req.Session]; ok {
			resume = true // created on a prior turn → continue it
		} else {
			sessions[req.Session] = &sessionInfo{
				Name:      req.Session,
				CreatedAt: time.Now().UTC(),
				Title:     sessionTitle(req.Prompt),
			}
		}
		sessionMu.Unlock()
	}
	cmd := exec.CommandContext(r.Context(), "/usr/local/bin/goose", gooseArgs(loadBuiltins(), loadMCPURL(), req.Session, resume)...)
	cmd.Stdin = strings.NewReader(finalPrompt)

	// Propagate the nested-invocation depth (v0.4.4) to the goose subprocess so a
	// gtcall the agent spawns re-sends the accumulated depth. The control plane
	// sets X-Ephemera-Task-Depth (>=1) on every proxied /tasks hop and enforces a
	// ceiling; absent means a direct top-level call (depth 0). Set cmd.Env
	// explicitly (it was nil → inherited) and pass the value through verbatim —
	// the control plane increments again on the next hop.
	depth := r.Header.Get("X-Ephemera-Task-Depth")
	if depth == "" {
		depth = "0"
	}
	cmd.Env = append(os.Environ(), "EPHEMERA_TASK_DEPTH="+depth)
	// Cap the per-response output reservation. goose otherwise reserves the
	// model's full output window (tens of thousands of tokens for some models);
	// added to the input that can exceed a provider's per-minute token budget
	// (Groq free-tier TPM is 6000), which goose misreports as a context overflow.
	// A modest default keeps the request in budget; override by exporting
	// GOOSE_MAX_TOKENS into the VM environment for higher-budget providers.
	if _, ok := os.LookupEnv("GOOSE_MAX_TOKENS"); !ok {
		cmd.Env = append(cmd.Env, "GOOSE_MAX_TOKENS=2048")
	}

	// Opt-in streaming: ?stream=1 streams NDJSON progress + a final result frame.
	// The default path keeps the buffered {"output","error"} contract verbatim.
	var res TaskResult
	var transcript []TranscriptTurn
	if r.URL.Query().Get("stream") == "1" {
		res, transcript = runTaskStreaming(w, cmd)
	} else {
		res, transcript = runTaskBuffered(w, cmd)
	}

	// Record the turn so GET /sessions can list it and the Web UI can resume after
	// a snapshot/restore. LastOutput seeds the UI's delta baseline so a resumed
	// conversation doesn't re-dump the whole transcript on the first follow-up;
	// transcript caches the full conversation for GET /sessions/{name}/transcript.
	if req.Session != "" {
		sessionMu.Lock()
		if s, ok := sessions[req.Session]; ok {
			s.Turns++
			if res.Output != "" {
				s.LastOutput = res.Output
			}
			if len(transcript) > 0 {
				s.transcript = transcript
			}
		}
		sessionMu.Unlock()
	}
}

// runTaskBuffered runs goose to completion and returns the whole TaskResult as a
// single JSON object — the original (pre-v0.4.4) behavior, unchanged.
func runTaskBuffered(w http.ResponseWriter, cmd *exec.Cmd) (TaskResult, []TranscriptTurn) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()

	res := TaskResult{Output: extractGooseJSONText(stdout)}
	transcript := extractGooseTranscript(stdout)
	if res.Output == "" && len(stdout) > 0 {
		// goose printed something that wasn't the expected JSON envelope (e.g.
		// crashed before producing output). Surface the raw bytes so the
		// caller can still inspect what happened.
		res.Output = string(stdout)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			res.Error = msg
		} else {
			res.Error = err.Error()
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
	return res, transcript
}

func workloadRunHandler(root string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "POST required")
			return
		}

		var req WorkloadRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		fullPath, cleanScript, status, err := workloadScriptPath(root, req.Script)
		if err != nil {
			writeJSONError(w, status, err.Error())
			return
		}
		timeout, err := workloadTimeoutSeconds(req.TimeoutSeconds)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		mu.Lock()
		if busy {
			mu.Unlock()
			writeJSONError(w, http.StatusServiceUnavailable, "agent busy")
			return
		}
		busy = true
		mu.Unlock()
		defer func() {
			mu.Lock()
			busy = false
			mu.Unlock()
		}()

		result := runWorkloadScript(r.Context(), root, cleanScript, fullPath, timeout)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	}
}

func runWorkloadScript(parent context.Context, root, cleanScript, fullPath string, timeout time.Duration) WorkloadRunResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	stdout := &truncatingBuffer{limit: maxWorkloadOutputBytes}
	stderr := &truncatingBuffer{limit: maxWorkloadOutputBytes}
	cmd := exec.Command("bash", fullPath)
	cmd.Dir = root
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = workloadEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	err := cmd.Start()
	ctxDone := false
	if err == nil {
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()

		select {
		case err = <-done:
		case <-ctx.Done():
			ctxDone = true
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			err = <-done
		}
	}
	result := WorkloadRunResult{
		Script:          cleanScript,
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		DurationMs:      time.Since(start).Milliseconds(),
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}
	if err == nil && !ctxDone {
		return result
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.ExitCode = -1
		result.TimedOut = true
		if result.Stderr == "" {
			result.Stderr = fmt.Sprintf("workload timed out after %ds", int(timeout/time.Second))
		}
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result
	}
	result.ExitCode = -1
	if result.Stderr == "" {
		result.Stderr = err.Error()
	}
	return result
}

func workloadEnvironment() []string {
	return []string{
		"DEBIAN_FRONTEND=noninteractive",
		"HOME=/root",
		"LANG=C.UTF-8",
		"LOGNAME=root",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"SHELL=/bin/bash",
		"USER=root",
	}
}

// streamFrame is one newline-delimited JSON frame emitted by runTaskStreaming.
// type="progress" carries an incremental stderr line (text); type="result" is
// the single final frame and mirrors TaskResult (output/error). A streaming
// client reconstructs the legacy object by reading the last frame.
type streamFrame struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// runTaskStreaming streams goose's progress as NDJSON over chunked transfer.
// goose --output-format json emits its envelope only at the end (stdout), so the
// incremental signal is goose's stderr (tool-call / thinking activity); each
// stderr line becomes a progress frame, and the buffered stdout is parsed into
// the final result frame. A 15s heartbeat keeps idle proxies from dropping the
// connection. Because the 200 status is committed before goose runs, a goose
// failure cannot be a 500 — the error rides in the result frame's error field,
// so streaming clients MUST inspect result.error rather than the status code.
func runTaskStreaming(w http.ResponseWriter, cmd *exec.Cmd) (TaskResult, []TranscriptTurn) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flushing available — fall back to the buffered contract.
		return runTaskBuffered(w, cmd)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return runTaskBuffered(w, cmd)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return runTaskBuffered(w, cmd)
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// w is not safe for concurrent writes; serialize the stderr scanner, the
	// heartbeat, and the final result behind one mutex.
	var writeMu sync.Mutex
	emit := func(fr streamFrame) {
		b, err := json.Marshal(fr)
		if err != nil {
			return
		}
		writeMu.Lock()
		w.Write(append(b, '\n'))
		flusher.Flush()
		writeMu.Unlock()
	}

	if err := cmd.Start(); err != nil {
		emit(streamFrame{Type: "result", Error: err.Error()})
		return TaskResult{Error: err.Error()}, nil
	}

	// Drain stderr line-by-line: relay each line as a progress frame and retain
	// it for the final error field (mirrors the buffered path's stderr capture).
	var stderrBuf bytes.Buffer
	var stderrMu sync.Mutex
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			stderrMu.Lock()
			stderrBuf.WriteString(line)
			stderrBuf.WriteByte('\n')
			stderrMu.Unlock()
			emit(streamFrame{Type: "progress", Text: line})
		}
	}()

	// Heartbeat so idle proxies/load balancers don't drop a long, quiet task.
	hbDone := make(chan struct{})
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-t.C:
				emit(streamFrame{Type: "progress", Text: ""})
			}
		}
	}()

	// Read stdout fully BEFORE Wait (StdoutPipe contract). stdout closes on
	// process exit, by which point stderr is also draining to EOF.
	stdout, _ := io.ReadAll(stdoutPipe)
	<-stderrDone
	close(hbDone)
	waitErr := cmd.Wait()

	res := TaskResult{Output: extractGooseJSONText(stdout)}
	transcript := extractGooseTranscript(stdout)
	if res.Output == "" && len(stdout) > 0 {
		res.Output = string(stdout)
	}
	if waitErr != nil {
		stderrMu.Lock()
		msg := strings.TrimSpace(stderrBuf.String())
		stderrMu.Unlock()
		if msg != "" {
			res.Error = msg
		} else {
			res.Error = waitErr.Error()
		}
	}
	emit(streamFrame{Type: "result", Output: res.Output, Error: res.Error})
	return res, transcript
}

// extractGooseJSONText parses the envelope produced by `goose run --output-format json`
// and returns the assistant text that follows the LAST user message — i.e. only the
// latest turn's reply. goose --resume re-emits the whole session transcript on every
// turn, so concatenating every assistant block would re-show all prior replies (the
// multi-turn "accumulating output" bug); slicing from the last user message fixes it
// at the source. Returns "" when stdout is not the expected shape so callers can fall
// back to raw bytes.
//
// Goose prints a startup banner to stdout BEFORE the JSON envelope even in
// --output-format json mode (e.g. "    __( O)> ● new session ... goose is ready"),
// so we slice from the first '{' to the last '}' before unmarshaling.
//
// Envelope shape (goose-cli session/mod.rs JsonOutput, camelCase via serde):
//
//	{ "messages": [ {"role":"assistant",
//	                  "content":[ {"type":"text","text":"..."}, ... ]} ],
//	  "metadata": {...} }
func extractGooseJSONText(stdout []byte) string {
	var env struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	start := bytes.IndexByte(stdout, '{')
	end := bytes.LastIndexByte(stdout, '}')
	if start < 0 || end < start {
		return ""
	}
	if err := json.Unmarshal(stdout[start:end+1], &env); err != nil {
		return ""
	}
	// Emit only the latest turn: find the last user message and concatenate the
	// assistant text after it. No user message (lastUser == -1) → whole transcript,
	// which is the single-shot / stateless case.
	lastUser := -1
	for i, m := range env.Messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	var sb strings.Builder
	for _, m := range env.Messages[lastUser+1:] {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.Content {
			if c.Type != "text" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// TranscriptTurn is one user or assistant turn of a session conversation, for the
// Web UI to repaint a resumed chat. Tool/thinking blocks are dropped; only text is kept.
type TranscriptTurn struct {
	Role string `json:"role"` // "user" | "assistant"
	Text string `json:"text"`
}

// extractGooseTranscript parses goose's `--output-format json` envelope (the same
// shape extractGooseJSONText reads) into the FULL ordered list of user/assistant
// turns. goose --resume re-emits the whole transcript on every turn, so the latest
// run's stdout already holds the complete conversation. User text is cleaned of the
// agent-injected prefixes (system-prompt frame, /nothink) so the UI shows the
// original prompt. Returns nil when stdout is not the expected envelope shape.
func extractGooseTranscript(stdout []byte) []TranscriptTurn {
	var env struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	start := bytes.IndexByte(stdout, '{')
	end := bytes.LastIndexByte(stdout, '}')
	if start < 0 || end < start {
		return nil
	}
	if err := json.Unmarshal(stdout[start:end+1], &env); err != nil {
		return nil
	}
	var turns []TranscriptTurn
	for _, m := range env.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		var sb strings.Builder
		for _, c := range m.Content {
			if c.Type != "text" || c.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(c.Text)
		}
		text := sb.String()
		if m.Role == "user" {
			text = cleanUserPrompt(text)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		turns = append(turns, TranscriptTurn{Role: m.Role, Text: text})
	}
	return turns
}

// cleanUserPrompt strips the prefixes goose-agent prepends to a user prompt
// (the "[SYSTEM INSTRUCTIONS]…[USER TASK]\n" frame from a role system.md, and the
// "/nothink\n" directive for qwen models), so a restored transcript shows what the
// user actually typed — mirroring handleTask's finalPrompt construction.
func cleanUserPrompt(text string) string {
	if i := strings.Index(text, "[USER TASK]\n"); i >= 0 {
		return text[i+len("[USER TASK]\n"):]
	}
	return strings.TrimPrefix(text, "/nothink\n")
}

// gooseExportBinary is the goose binary invoked by the transcript export fallback.
// It is a package var (not a literal) purely so a test can point it at a stub and
// prove the export path is read-only — the production value is the fixed in-VM path.
var gooseExportBinary = "/usr/local/bin/goose"

// exportSessionTranscript is the cache-miss fallback for GET /sessions/{name}/transcript
// (e.g. after a cold restart cleared the in-memory session map). `goose session export`
// is a read-only dump of the on-disk session that does NOT invoke the model. The JSON
// format is parsed with the same envelope reader; if goose wraps it differently the
// reader returns nil and the caller degrades to an empty transcript.
func exportSessionTranscript(ctx context.Context, name string) ([]TranscriptTurn, error) {
	cmd := exec.CommandContext(ctx, gooseExportBinary, "session", "export", "-n", name, "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("goose session export %q: %w", name, err)
	}
	turns := extractGooseTranscript(out)
	if turns == nil {
		return nil, fmt.Errorf("session export %q: unrecognized format", name)
	}
	return turns, nil
}

// TownWallPostBody is the JSON body accepted by /townwall/post inside the VM.
type TownWallPostBody struct {
	Body string `json:"body"`
}

// handleTownWallPost forwards the agent's message to the control plane's
// /flocks/{flock_id}/post endpoint. The VM is identified by its flock metadata
// file written at provision time. The agent token authenticates the local
// /townwall/post call; EPHEMERA_CONTROL_PLANE_TOKEN is forwarded to the control
// plane when daemon auth is enabled.
func handleTownWallPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
		return
	}
	flockID, agentID := loadFlockMeta()
	if flockID == "" {
		http.Error(w, `{"error":"this VM is not part of a flock"}`, http.StatusBadRequest)
		return
	}
	var body TownWallPostBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadRequest)
		return
	}
	if body.Body == "" {
		http.Error(w, `{"error":"body required"}`, http.StatusBadRequest)
		return
	}

	payload, _ := json.Marshal(map[string]string{"agent_id": agentID, "body": body.Body})
	target := controlPlaneAddr() + "/flocks/" + flockID + "/post"
	ctx, cancel := context.WithTimeout(r.Context(), townWallPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cpTok := loadCPToken(); cpTok != "" {
		req.Header.Set("Authorization", "Bearer "+cpTok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopping"})
	go func() {
		time.Sleep(200 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	isBusy := busy
	mu.Unlock()
	status := "idle"
	if isBusy {
		status = "busy"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}
