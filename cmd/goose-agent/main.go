package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	agentTokenPath        = "/root/.ephemera-agent-token"
	workspaceRoot         = "/workspace"
	flockMetaPath         = "/root/.ephemera-flock"
	systemPromptPath      = "/root/.goose-system-prompt"
	maxWorkspaceFileBytes = 4 << 20
	townWallPostTimeout   = 10 * time.Second
	// defaultControlPlaneAddr is the gateway IP the host uses inside the VM's
	// /24 network. Overridable via EPHEMERA_CONTROL_PLANE for testing.
	defaultControlPlaneAddr = "http://10.0.1.1:3000"
)

// loadAgentToken reads the per-VM Bearer token written by the control plane at VM provision time.
// Returns an empty string (auth disabled) if the file does not exist — backward compatible with
// golden images that predate this feature.
func loadAgentToken() string {
	b, err := os.ReadFile(agentTokenPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Warning: could not read agent token file: %v", err)
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
	token := loadAgentToken()
	if token == "" {
		log.Println("Warning: no agent token found — authentication disabled")
	} else {
		log.Println("goose-agent token auth enabled")
	}

	startVsockListener()

	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", agentAuthMiddleware(token, handleTask))
	mux.HandleFunc("/workspace", agentAuthMiddleware(token, workspaceHandler(workspaceRoot)))
	mux.HandleFunc("/stop", agentAuthMiddleware(token, handleStop))
	mux.HandleFunc("/townwall/post", agentAuthMiddleware(token, handleTownWallPost))
	mux.HandleFunc("/health", handleHealth) // always unauthenticated

	addr := agentListenAddr()
	srv = &http.Server{Addr: addr, Handler: mux}
	log.Printf("goose-agent ready on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

// startVsockListener opens an AF_VSOCK socket on vsockReconfigPort and accepts
// CHANGE_IP commands from the host control plane. Used after snapshot restore to
// reconfigure eth0 without rebooting, decoupling the guest IP from the snapshot state.
func startVsockListener() {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		log.Printf("Warning: vsock unavailable — post-restore IP reconfiguration disabled: %v", err)
		return
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: vsockReconfigPort}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		log.Printf("Warning: vsock bind: %v", err)
		return
	}
	if err := unix.Listen(fd, 4); err != nil {
		unix.Close(fd)
		log.Printf("Warning: vsock listen: %v", err)
		return
	}
	log.Printf("vsock reconfig listener ready on port %d", vsockReconfigPort)
	go func() {
		for {
			connFd, _, err := unix.Accept(fd)
			if err != nil {
				log.Printf("vsock accept: %v", err)
				continue
			}
			go handleVsockConn(connFd)
		}
	}()
}

// handleVsockConn processes a single vsock connection from the host.
// Protocol: "CHANGE_IP <cidr_ip> <gateway>\n" → "OK\n" or "ERROR: ...\n"
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
	if len(parts) != 3 || parts[0] != "CHANGE_IP" {
		fmt.Fprintf(f, "ERROR: expected CHANGE_IP <cidr_ip> <gateway>\n")
		return
	}
	cidrIP, gateway := parts[1], parts[2]

	if err := applyIPConfig(cidrIP, gateway); err != nil {
		fmt.Fprintf(f, "ERROR: %v\n", err)
		log.Printf("vsock CHANGE_IP failed: %v", err)
		return
	}
	fmt.Fprintf(f, "OK\n")
	log.Printf("IP reconfigured: eth0 → %s via %s", cidrIP, gateway)
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
				log.Printf("workspace response copy failed: %v", err)
			}

		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "GET or PUT required")
		}
	}
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

	cmd := exec.CommandContext(r.Context(), "/usr/local/bin/goose", "run", "-i", "-")
	cmd.Stdin = strings.NewReader(finalPrompt)
	out, err := cmd.CombinedOutput()

	res := TaskResult{Output: string(out)}
	if err != nil {
		res.Error = err.Error()
		w.WriteHeader(http.StatusInternalServerError)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
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
	if cpTok := os.Getenv("EPHEMERA_CONTROL_PLANE_TOKEN"); cpTok != "" {
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
