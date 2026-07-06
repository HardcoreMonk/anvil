package mcpgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe io.Writer that captures slog output written by
// the asynchronous reap/stderr goroutines under test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureSlog redirects the default slog logger into a buffer for the duration
// of the test, restoring the previous default afterward.
func captureSlog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// waitForLog polls the captured host log until it contains want (the reap
// goroutine logs asynchronously, after the caller's error has surfaced).
func waitForLog(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("host log never contained %q; got:\n%s", want, buf.String())
}

// TestStdioMCPHelper is not a test: it is the fake stdio MCP server the tests
// below re-exec this test binary as (the os/exec helper-process idiom). Inert
// unless GO_EPHEMERA_MCP_STDIO_HELPER=1. Behavior variants are selected with
// HELPER_MODE: ok (default), exit-now, crash-after-init, crash-once, noisy,
// notify, ignore-eof; HELPER_NO_PING=1 answers ping with method-not-found.
func TestStdioMCPHelper(t *testing.T) {
	if os.Getenv("GO_EPHEMERA_MCP_STDIO_HELPER") != "1" {
		return
	}
	runStdioMCPHelper()
	os.Exit(0)
}

func runStdioMCPHelper() {
	mode := os.Getenv("HELPER_MODE")
	if mode == "" {
		mode = "ok"
	}
	if mode == "exit-now" {
		fmt.Fprintln(os.Stderr, "boom: refusing to start")
		os.Exit(2)
	}
	if mode == "noisy" {
		fmt.Println("WELCOME BANNER (not JSON)")
		fmt.Fprintln(os.Stderr, "noisy stderr line")
	}
	var outMu sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		outMu.Lock()
		_, _ = os.Stdout.Write(append(b, '\n'))
		outMu.Unlock()
	}
	respond := func(id json.RawMessage, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		if req.Method == "" {
			// A reply to the server→client request issued in notify mode: the
			// gateway must have answered it with method-not-found.
			if req.Error != nil && string(req.ID) == `"srv-1"` {
				_ = os.WriteFile("srvreq_answered", []byte(req.Error.Message), 0o644)
			}
			continue
		}
		switch req.Method {
		case "initialize":
			if mode == "init-error" {
				// Backend rejects the handshake with a sentinel-bearing protocol
				// error, exercising the initialize-error scrub (#36).
				write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32001, "message": "SENTINEL_INIT_BOOM proprietary handshake refusal"}})
				continue
			}
			respond(req.ID, map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "stdio-helper", "version": "1"},
			})
			continue
		case "notifications/initialized":
			continue
		}
		// First post-handshake request: crash modes trigger here.
		switch mode {
		case "crash-after-init":
			fmt.Fprintln(os.Stderr, "boom after init")
			os.Exit(2)
		case "crash-once":
			if _, err := os.Stat("crashed_once"); err != nil {
				_ = os.WriteFile("crashed_once", nil, 0o644)
				fmt.Fprintln(os.Stderr, "boom once")
				os.Exit(2)
			}
		}
		switch req.Method {
		case "ping":
			if os.Getenv("HELPER_NO_PING") == "1" {
				write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "no ping here"}})
			} else {
				respond(req.ID, map[string]any{})
			}
		case "tools/list":
			respond(req.ID, map[string]any{"tools": []map[string]any{
				{"name": "echo", "description": "echoes input", "inputSchema": map[string]any{"type": "object"}},
				{"name": "env", "description": "dumps the environment"},
				{"name": "pid", "description": "reports the server pid"},
			}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if mode == "notify" {
				write(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{}})
				write(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "roots/list", "params": map[string]any{}})
			}
			id := req.ID
			reply := func() {
				var text string
				switch p.Name {
				case "pid":
					text = strconv.Itoa(os.Getpid())
				case "env":
					text = strings.Join(os.Environ(), "\n")
				default:
					raw, _ := json.Marshal(p.Arguments)
					text = "called " + p.Name + " with " + string(raw)
				}
				respond(id, map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
					"isError": false,
				})
			}
			if d, _ := p.Arguments["delay_ms"].(float64); d > 0 {
				go func() {
					time.Sleep(time.Duration(d) * time.Millisecond)
					reply()
				}()
			} else {
				reply()
			}
		case "resources/list":
			respond(req.ID, map[string]any{"resources": []map[string]any{
				{"uri": "file:///a.txt", "name": "A", "mimeType": "text/plain"},
			}})
		case "resources/read":
			var p struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(req.Params, &p)
			respond(req.ID, map[string]any{"contents": []map[string]any{
				{"uri": p.URI, "mimeType": "text/plain", "text": "read " + p.URI},
			}})
		case "prompts/list":
			respond(req.ID, map[string]any{"prompts": []map[string]any{
				{"name": "greet", "description": "a greeting"},
			}})
		case "prompts/get":
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &p)
			respond(req.ID, map[string]any{"messages": []map[string]any{
				{"role": "user", "content": map[string]any{"type": "text", "text": "got " + p.Name}},
			}})
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
	if mode == "ignore-eof" {
		time.Sleep(time.Hour) // misbehave: outlive the stdin-EOF exit request
	}
}

// helperBackend builds a StdioBackend that re-execs this test binary as the
// fake stdio MCP server. Its scratch dir is the backend's spec.Dir.
func helperBackend(t *testing.T, mode string, extraEnv map[string]string) *StdioBackend {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := map[string]string{
		"GO_EPHEMERA_MCP_STDIO_HELPER": "1",
		"HELPER_MODE":                  mode,
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	b := NewStdioBackend("helper", "helper", StdioSpawnSpec{
		Command: exe,
		Args:    []string{"-test.run=^TestStdioMCPHelper$", "--"},
		Env:     env,
		Dir:     t.TempDir(),
	})
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// callText invokes a tool and returns the text of its first content item.
func callText(t *testing.T, b *StdioBackend, tool string, args string) string {
	t.Helper()
	res, err := b.CallTool(context.Background(), tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(res, &out); err != nil || len(out.Content) == 0 {
		t.Fatalf("CallTool %s: bad result %s (%v)", tool, res, err)
	}
	return out.Content[0].Text
}

func helperPid(t *testing.T, b *StdioBackend) int {
	t.Helper()
	pid, err := strconv.Atoi(callText(t, b, "pid", `{}`))
	if err != nil {
		t.Fatalf("pid tool: %v", err)
	}
	return pid
}

func TestStdioBackend_SpawnHandshakeAndReuse(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	tools, err := b.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 3 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	// A second use must reuse the same child, not respawn.
	if p1, p2 := helperPid(t, b), helperPid(t, b); p1 != p2 {
		t.Fatalf("child respawned between calls: pid %d → %d", p1, p2)
	}
}

func TestStdioBackend_ToolCallRoundTrip(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	text := callText(t, b, "echo", `{"x":1}`)
	if !strings.Contains(text, `called echo with {"x":1}`) {
		t.Fatalf("unexpected echo result: %q", text)
	}
}

func TestStdioBackend_ResourcesAndPrompts(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	ctx := context.Background()
	res, err := b.ListResources(ctx)
	if err != nil || len(res) != 1 || res[0].URI != "file:///a.txt" {
		t.Fatalf("ListResources = %+v, %v", res, err)
	}
	read, err := b.ReadResource(ctx, "file:///a.txt")
	if err != nil || !strings.Contains(string(read), "read file:///a.txt") {
		t.Fatalf("ReadResource = %s, %v", read, err)
	}
	prompts, err := b.ListPrompts(ctx)
	if err != nil || len(prompts) != 1 || prompts[0].Name != "greet" {
		t.Fatalf("ListPrompts = %+v, %v", prompts, err)
	}
	got, err := b.GetPrompt(ctx, "greet", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(got), "got greet") {
		t.Fatalf("GetPrompt = %s, %v", got, err)
	}
}

func TestStdioBackend_CredentialEnvAndCleanEnv(t *testing.T) {
	t.Setenv("EPHEMERA_TEST_CANARY", "leakme") // must NOT reach the child
	b := helperBackend(t, "ok", map[string]string{"MY_TOKEN": "sekrit"})
	envText := callText(t, b, "env", `{}`)
	if !strings.Contains(envText, "MY_TOKEN=sekrit") {
		t.Fatalf("credential env var missing from child env:\n%s", envText)
	}
	if strings.Contains(envText, "EPHEMERA_TEST_CANARY") {
		t.Fatalf("daemon env leaked into child:\n%s", envText)
	}
	if !strings.Contains(envText, "HOME="+b.spec.Dir) {
		t.Fatalf("child HOME is not the scratch dir:\n%s", envText)
	}
}

func TestStdioBackend_ServerRequestsAnsweredNotificationsSkipped(t *testing.T) {
	b := helperBackend(t, "notify", nil)
	// The call itself must succeed despite the interleaved server messages.
	if text := callText(t, b, "echo", `{"y":2}`); !strings.Contains(text, `"y":2`) {
		t.Fatalf("unexpected result: %q", text)
	}
	// The helper records the -32601 it received for its server→client request.
	marker := filepath.Join(b.spec.Dir, "srvreq_answered")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server request was never answered with method-not-found")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStdioBackend_NoisyStdoutTolerated(t *testing.T) {
	b := helperBackend(t, "noisy", nil)
	if _, err := b.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools with a stdout banner: %v", err)
	}
}

func TestStdioBackend_ConcurrentCallsMultiplex(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Inverted delays: the first submission answers last, so replies
			// arrive out of order and must be matched by id.
			args := fmt.Sprintf(`{"delay_ms":%d,"i":%d}`, (n-i)*30, i)
			res, err := b.CallTool(context.Background(), "echo", json.RawMessage(args))
			if err != nil {
				errs[i] = err
				return
			}
			if !strings.Contains(string(res), fmt.Sprintf(`\"i\":%d`, i)) &&
				!strings.Contains(string(res), fmt.Sprintf(`"i":%d`, i)) {
				errs[i] = fmt.Errorf("call %d got someone else's reply: %s", i, res)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestStdioBackend_CrashRespawnsAfterCooldown(t *testing.T) {
	b := helperBackend(t, "crash-once", nil)
	b.spawnCooldown = 50 * time.Millisecond
	if _, err := b.ListTools(context.Background()); err == nil {
		t.Fatal("expected the first call to fail (child crashes once)")
	}
	time.Sleep(80 * time.Millisecond) // let the cooldown lapse
	if _, err := b.ListTools(context.Background()); err != nil {
		t.Fatalf("expected a clean respawn after the cooldown: %v", err)
	}
}

func TestStdioBackend_RespawnCooldownBlocks(t *testing.T) {
	logs := captureSlog(t)
	b := helperBackend(t, "crash-after-init", nil)
	if _, err := b.ListTools(context.Background()); err == nil {
		t.Fatal("expected the first call to fail")
	}
	// reap finishes moments after the failure surfaces; give it a beat.
	time.Sleep(300 * time.Millisecond)
	_, err := b.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("expected a cooldown error, got: %v", err)
	}
	// The child's stderr tail must NOT leak into the error string (it flows to
	// the VM verbatim through the gateway); it goes to the host log instead (#32).
	if strings.Contains(err.Error(), "boom after init") {
		t.Fatalf("stderr tail leaked into the cooldown error: %v", err)
	}
	waitForLog(t, logs, "boom after init")
}

func TestStdioBackend_SpawnFailureSurfacesStderr(t *testing.T) {
	// The child writes a sentinel to stderr and exits at startup. Operators must
	// still see that stderr — now via the host log, not the VM-facing error (#32).
	logs := captureSlog(t)
	b := helperBackend(t, "exit-now", nil)
	if _, err := b.ListTools(context.Background()); err == nil {
		t.Fatal("expected failure for a child that exits at startup")
	}
	time.Sleep(300 * time.Millisecond)
	_, err := b.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected an error for a child that exits at startup")
	}
	if strings.Contains(err.Error(), "boom: refusing to start") {
		t.Fatalf("stderr tail leaked into the VM-facing error: %v", err)
	}
	// The generic exit error still names the failing backend (server name, no stderr).
	if !strings.Contains(err.Error(), "helper") {
		t.Fatalf("exit error should name the backend, got: %v", err)
	}
	waitForLog(t, logs, "boom: refusing to start")
}

// TestGateway_ToolCallError_ScrubsBackendStderr is the VM-facing half of #32: a
// stdio backend that crashes (writing a sentinel to stderr) must produce a
// generic JSON-RPC error to the VM, with the stderr surfaced only host-side.
func TestGateway_ToolCallError_ScrubsBackendStderr(t *testing.T) {
	logs := captureSlog(t)
	b := helperBackend(t, "exit-now", nil) // stderr "boom: refusing to start", exits at startup
	reg := &Registry{
		servers:  []ServerConfig{{ID: "helper", Namespace: "helper", Transport: "stdio", Command: b.spec.Command}},
		backends: map[string]Backend{"helper": b},
		byNS:     map[string]Backend{"helper": b},
	}
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
	})
	resp := doRPC(t, g, "tools/call", toolsCallParams{Name: "helper__echo", Arguments: json.RawMessage(`{}`)})
	if resp.Error == nil {
		t.Fatal("expected a JSON-RPC error for a crashed backend")
	}
	if strings.Contains(resp.Error.Message, "boom: refusing to start") {
		t.Fatalf("VM-facing gateway error leaked child stderr: %q", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "tool call failed") {
		t.Fatalf("expected generic gateway error, got: %q", resp.Error.Message)
	}
	// Operator observability: the stderr is still in the host log.
	waitForLog(t, logs, "boom: refusing to start")
}

// TestGateway_InitializeError_ScrubsBackendDetail is the VM-facing half of #36:
// a stdio backend that rejects the MCP initialize handshake with a sentinel-
// bearing protocol error must produce a generic JSON-RPC error to the VM, with
// the backend-influenced detail surfaced only host-side (mirrors #32's stderr
// scrub).
func TestGateway_InitializeError_ScrubsBackendDetail(t *testing.T) {
	logs := captureSlog(t)
	b := helperBackend(t, "init-error", nil) // initialize responds with "SENTINEL_INIT_BOOM ..."
	reg := &Registry{
		servers:  []ServerConfig{{ID: "helper", Namespace: "helper", Transport: "stdio", Command: b.spec.Command}},
		backends: map[string]Backend{"helper": b},
		byNS:     map[string]Backend{"helper": b},
	}
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
	})
	resp := doRPC(t, g, "tools/call", toolsCallParams{Name: "helper__echo", Arguments: json.RawMessage(`{}`)})
	if resp.Error == nil {
		t.Fatal("expected a JSON-RPC error for a backend that fails initialize")
	}
	if strings.Contains(resp.Error.Message, "SENTINEL_INIT_BOOM") {
		t.Fatalf("VM-facing gateway error leaked backend initialize detail: %q", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "helper") {
		t.Fatalf("generic error should still name the backend, got: %q", resp.Error.Message)
	}
	// Operator observability: the full initialize failure is still in the host log.
	waitForLog(t, logs, "SENTINEL_INIT_BOOM")
}

func TestStdioBackend_CloseGraceful(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	pid := helperPid(t, b)
	start := time.Now()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > b.closeGrace {
		t.Fatalf("graceful close took %v (child should exit on stdin EOF)", elapsed)
	}
	// dead closed ⇒ reaped ⇒ the pid is gone (not a zombie).
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d still exists after Close: %v", pid, err)
	}
	if _, err := b.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected closed error after Close, got: %v", err)
	}
}

func TestStdioBackend_CloseKillsStubborn(t *testing.T) {
	b := helperBackend(t, "ignore-eof", nil)
	b.closeGrace = 200 * time.Millisecond
	pid := helperPid(t, b)
	start := time.Now()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Close took %v, want grace+kill well under 4s", elapsed)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("stubborn child %d survived Close: %v", pid, err)
	}
}

func TestStdioBackend_HealthPing(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	if err := b.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	// A server without ping support is still healthy.
	b2 := helperBackend(t, "ok", map[string]string{"HELPER_NO_PING": "1"})
	if err := b2.Health(context.Background()); err != nil {
		t.Fatalf("Health without ping support: %v", err)
	}
}

func TestStdioBackend_CtxCancelDoesNotKillProcess(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	pid := helperPid(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := b.CallTool(ctx, "echo", json.RawMessage(`{"delay_ms":600}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got: %v", err)
	}
	// The child must have survived the caller's timeout.
	if pid2 := helperPid(t, b); pid2 != pid {
		t.Fatalf("child was killed by a ctx cancel: pid %d → %d", pid, pid2)
	}
}

func TestApplyRlimits(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	pid := helperPid(t, b)
	limits, err := os.ReadFile(fmt.Sprintf("/proc/%d/limits", pid))
	if err != nil {
		t.Fatalf("read limits: %v", err)
	}
	// NPROC is intentionally absent here: it only applies on the privilege-drop
	// path (dedicated uid), which unit tests cannot take; e2e covers it as root.
	for _, want := range []string{"Max open files", "Max core file size"} {
		found := false
		for _, line := range strings.Split(string(limits), "\n") {
			if !strings.HasPrefix(line, want) {
				continue
			}
			found = true
			switch want {
			case "Max open files":
				if !strings.Contains(line, "256") {
					t.Fatalf("%s not clamped to 256: %s", want, line)
				}
			case "Max core file size":
				fields := strings.Fields(strings.TrimPrefix(line, want))
				if len(fields) < 1 || fields[0] != "0" {
					t.Fatalf("%s not clamped to 0: %s", want, line)
				}
			}
		}
		if !found {
			t.Fatalf("limit %q not found in /proc/%d/limits", want, pid)
		}
	}
}

func TestStdioBackend_ScratchDirTraversableParents(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	base := t.TempDir()
	scratch := filepath.Join(base, "var-lib", "mcp-stdio", "helper")
	b := NewStdioBackend("helper", "helper", StdioSpawnSpec{
		Command: exe,
		Args:    []string{"-test.run=^TestStdioMCPHelper$", "--"},
		Env:     map[string]string{"GO_EPHEMERA_MCP_STDIO_HELPER": "1", "HELPER_MODE": "ok"},
		Dir:     scratch,
	})
	t.Cleanup(func() { _ = b.Close() })
	if _, err := b.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// The de-privileged child must be able to TRAVERSE into its own dir:
	// created parents are 0755, only the leaf is 0700. Regression guard — a
	// plain MkdirAll(dir, 0700) once propagated 0700 to the parents, which
	// (with a scratch base under a 0750 home) broke the child's post-setuid
	// chdir with EACCES in the v0.6.4 e2e.
	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{filepath.Join(base, "var-lib"), 0o755},
		{filepath.Join(base, "var-lib", "mcp-stdio"), 0o755},
		{scratch, 0o700},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		if got := fi.Mode().Perm(); got != tc.want {
			t.Fatalf("%s mode = %o, want %o", tc.path, got, tc.want)
		}
	}
}

func TestRegistry_StdioValidation(t *testing.T) {
	secrets := map[string]string{"k": "tok"}
	cases := []struct {
		name    string
		cfg     ServerConfig
		wantErr string // empty = must succeed
	}{
		{"valid stdio", ServerConfig{ID: "s", Transport: "stdio", Command: "/bin/true"}, ""},
		{"valid stdio with credential", ServerConfig{ID: "s", Transport: "stdio", Command: "/bin/true", Credential: "k", CredentialEnv: "TOK"}, ""},
		{"stdio without command", ServerConfig{ID: "s", Transport: "stdio"}, "requires command"},
		{"stdio with url", ServerConfig{ID: "s", Transport: "stdio", Command: "/bin/true", URL: "http://x"}, "url is not valid"},
		{"http with command", ServerConfig{ID: "s", URL: "http://x", Command: "/bin/true"}, "only valid with transport"},
		{"http with credential_env", ServerConfig{ID: "s", URL: "http://x", CredentialEnv: "TOK"}, "only valid with transport"},
		{"credential without credential_env", ServerConfig{ID: "s", Transport: "stdio", Command: "/bin/true", Credential: "k"}, "must be set together"},
		{"credential_env without credential", ServerConfig{ID: "s", Transport: "stdio", Command: "/bin/true", CredentialEnv: "TOK"}, "must be set together"},
		{"bad env name", ServerConfig{ID: "s", Transport: "stdio", Command: "/bin/true", Credential: "k", CredentialEnv: "1BAD"}, "not a valid environment variable name"},
		{"unknown transport", ServerConfig{ID: "s", Transport: "ws", URL: "http://x"}, `only "http" or "stdio"`},
		{"missing binary", ServerConfig{ID: "s", Transport: "stdio", Command: "/nonexistent/ephemera-test-xyz"}, "/nonexistent/ephemera-test-xyz"},
	}
	for _, tc := range cases {
		_, err := NewRegistry([]ServerConfig{tc.cfg}, secrets, nil)
		if tc.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: error = %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestRegistry_TransportBranching(t *testing.T) {
	reg, err := NewRegistry([]ServerConfig{
		{ID: "h", URL: "http://example.invalid/mcp"},
		{ID: "s", Transport: "stdio", Command: "/bin/true"},
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if b, _ := reg.Backend("h"); func() bool { _, ok := b.(*HTTPBackend); return !ok }() {
		t.Fatalf("backend h is %T, want *HTTPBackend", b)
	}
	if b, _ := reg.Backend("s"); func() bool { _, ok := b.(*StdioBackend); return !ok }() {
		t.Fatalf("backend s is %T, want *StdioBackend", b)
	}
	infos := reg.Servers()
	if infos[0].Transport != "http" || infos[0].Command != "" {
		t.Fatalf("http info wrong (transport must normalize): %+v", infos[0])
	}
	if infos[1].Transport != "stdio" || infos[1].Command == "" {
		t.Fatalf("stdio info wrong: %+v", infos[1])
	}
}

func TestRegistry_CloseReapsStdioChildren(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	reg := &Registry{
		servers:  []ServerConfig{{ID: "helper", Namespace: "helper", Transport: "stdio", Command: b.spec.Command}},
		backends: map[string]Backend{"helper": b},
		byNS:     map[string]Backend{"helper": b},
	}
	pid := helperPid(t, b)
	reg.Close()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("Registry.Close left child %d alive: %v", pid, err)
	}
	// An HTTP-only registry has nothing to close.
	httpReg := newTestRegistry(t, ServerConfig{ID: "h", URL: "http://example.invalid"}, nil)
	httpReg.Close()
}

func TestGateway_EndToEnd_StdioBackend(t *testing.T) {
	b := helperBackend(t, "ok", nil)
	reg := &Registry{
		servers:  []ServerConfig{{ID: "helper", Namespace: "helper", Transport: "stdio", Command: b.spec.Command}},
		backends: map[string]Backend{"helper": b},
		byNS:     map[string]Backend{"helper": b},
	}
	var audited []AuditRecord
	g := New(Options{
		Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
		Registry: reg,
		Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
	})
	init := doRPC(t, g, "initialize", initializeParams{ProtocolVersion: "2025-03-26"})
	if init.Error != nil {
		t.Fatalf("initialize error: %v", init.Error)
	}
	list := doRPC(t, g, "tools/list", map[string]any{})
	var lr toolsListResult
	_ = json.Unmarshal(list.Result, &lr)
	if len(lr.Tools) != 3 || lr.Tools[0].Name != "helper__echo" {
		t.Fatalf("tools/list = %+v (want namespaced helper__*)", lr.Tools)
	}
	call := doRPC(t, g, "tools/call", toolsCallParams{Name: "helper__echo", Arguments: json.RawMessage(`{"x":1}`)})
	if call.Error != nil {
		t.Fatalf("tools/call error: %v", call.Error)
	}
	if !strings.Contains(string(call.Result), "called echo") {
		t.Fatalf("unexpected call result: %s", call.Result)
	}
	if len(audited) != 1 || !audited[0].OK || audited[0].Server != "helper" || audited[0].Tool != "echo" {
		t.Fatalf("audit record wrong: %+v", audited)
	}
}
