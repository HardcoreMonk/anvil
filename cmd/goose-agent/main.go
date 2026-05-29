package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// vsockReconfigPort is the well-known port for host→guest IP reconfiguration commands.
const vsockReconfigPort = 1234

type TaskRequest struct {
	Prompt string `json:"prompt"`
}

type TaskResult struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

var (
	mu   sync.Mutex
	busy bool
	srv  *http.Server
)

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
	agentTokenPath   = "/root/.ephemera-agent-token"
	flockMetaPath    = "/root/.ephemera-flock"
	systemPromptPath = "/root/.goose-system-prompt"
	cpTokenPath      = "/root/.ephemera-cp-token"
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
// (matches apiClients[0]); falls back to EPHEMERA_CONTROL_PLANE_TOKEN for
// older golden images that predate v0.3.3. Returns "" when neither is set
// (auth disabled mode).
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
	return func(w http.ResponseWriter, r *http.Request) {
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

func main() {
	initSlog()
	token := loadAgentToken()
	if token == "" {
		slog.Warn("no agent token found, authentication disabled")
	} else {
		slog.Warn("goose-agent token auth enabled")
	}

	startVsockListener()

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", agentAuthMiddleware(token, handleTask))
	mux.HandleFunc("/stop", agentAuthMiddleware(token, handleStop))
	mux.HandleFunc("/townwall/post", agentAuthMiddleware(token, handleTownWallPost))
	mux.HandleFunc("/health", handleHealth) // always unauthenticated

	addr := agentListenAddr()
	srv = &http.Server{Addr: addr, Handler: mux}
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

	default:
		fmt.Fprintf(f, "ERROR: unknown command %q\n", parts[0])
	}
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

	// --output-format json bypasses goose-cli's streaming markdown buffer, whose
	// truncate_code_blocks() unconditionally caps fenced code at 50 lines and
	// spills the overflow into /tmp/goose-*.txt — a path the host caller cannot
	// reach across the HTTP boundary. Neither --debug nor GOOSE_SHOW_FULL_OUTPUT
	// disables that cap; the JSON path is the only code-level escape because
	// session/mod.rs gates the markdown buffer flush behind !is_json_mode.
	cmd := exec.CommandContext(r.Context(), "/usr/local/bin/goose", "run", "--output-format", "json", "-i", "-")
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

	// Opt-in streaming: ?stream=1 streams NDJSON progress + a final result frame.
	// The default path keeps the buffered {"output","error"} contract verbatim.
	if r.URL.Query().Get("stream") == "1" {
		runTaskStreaming(w, cmd)
		return
	}
	runTaskBuffered(w, cmd)
}

// runTaskBuffered runs goose to completion and returns the whole TaskResult as a
// single JSON object — the original (pre-v0.4.4) behavior, unchanged.
func runTaskBuffered(w http.ResponseWriter, cmd *exec.Cmd) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()

	res := TaskResult{Output: extractGooseJSONText(stdout)}
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
func runTaskStreaming(w http.ResponseWriter, cmd *exec.Cmd) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flushing available — fall back to the buffered contract.
		runTaskBuffered(w, cmd)
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		runTaskBuffered(w, cmd)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		runTaskBuffered(w, cmd)
		return
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
		return
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
}

// extractGooseJSONText parses the envelope produced by `goose run --output-format json`
// and returns the concatenated text of every assistant content block. Returns ""
// when stdout is not the expected shape so callers can fall back to raw bytes.
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
	var sb strings.Builder
	for _, m := range env.Messages {
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

// TownWallPostBody is the JSON body accepted by /townwall/post inside the VM.
type TownWallPostBody struct {
	Body string `json:"body"`
}

// handleTownWallPost forwards the agent's message to the control plane's
// /flocks/{flock_id}/post endpoint. The VM is identified by its flock metadata
// file written at provision time; the agent token authenticates the local
// /townwall/post call, while the forward to the control plane is currently
// performed without bearer auth (v1: control plane runs unauthenticated in
// flock e2e mode; a CP token can be injected in v2).
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
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(payload))
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
