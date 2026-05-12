package main

import (
	"bufio"
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

const agentTokenPath = "/root/.ephemera-agent-token"
const workspaceRoot = "/workspace"

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

func workspaceHandler(root string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fullPath, err := workspaceFilePath(root, r.URL.Query().Get("path"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodPut:
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				http.Error(w, fmt.Sprintf("create parent directory: %v", err), http.StatusInternalServerError)
				return
			}
			out, err := os.Create(fullPath)
			if err != nil {
				http.Error(w, fmt.Sprintf("create workspace file: %v", err), http.StatusInternalServerError)
				return
			}
			n, copyErr := io.Copy(out, r.Body)
			closeErr := out.Close()
			if copyErr != nil {
				http.Error(w, fmt.Sprintf("write workspace file: %v", copyErr), http.StatusInternalServerError)
				return
			}
			if closeErr != nil {
				http.Error(w, fmt.Sprintf("close workspace file: %v", closeErr), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(WorkspaceWriteResult{
				Path:  filepath.ToSlash(strings.TrimPrefix(fullPath, filepath.Clean(root)+string(os.PathSeparator))),
				Bytes: n,
			})

		case http.MethodGet:
			in, err := os.Open(fullPath)
			if err != nil {
				if os.IsNotExist(err) {
					http.Error(w, "workspace file not found", http.StatusNotFound)
					return
				}
				http.Error(w, fmt.Sprintf("open workspace file: %v", err), http.StatusInternalServerError)
				return
			}
			defer in.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			if _, err := io.Copy(w, in); err != nil {
				log.Printf("workspace response copy failed: %v", err)
			}

		default:
			http.Error(w, "GET or PUT required", http.StatusMethodNotAllowed)
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

	cmd := exec.CommandContext(r.Context(), "/usr/local/bin/goose", "run", "-i", "-")
	cmd.Stdin = strings.NewReader(req.Prompt)
	out, err := cmd.CombinedOutput()

	res := TaskResult{Output: string(out)}
	if err != nil {
		res.Error = err.Error()
		w.WriteHeader(http.StatusInternalServerError)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
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
