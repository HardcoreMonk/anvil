# anvil IronClaw MCP v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `cmd/anvil-mcp`, a Go stdio MCP server that lets IronClaw create an anvil VM, run a task, check health, stop the agent, and delete the VM through the anvil 0.2.0 daemon API.

**Architecture:** `anvil-mcp` is a thin runtime bridge. IronClaw talks MCP over stdio to the Go binary; the adapter loads config, keeps an optional in-memory `session_name -> vm_id` map, and calls the anvil daemon over HTTP JSON. v1 intentionally excludes workspace copy-in/out, snapshot tools, persistent sessions, HTTP MCP transport, quota, and automatic cleanup.

**Tech Stack:** Go, `github.com/modelcontextprotocol/go-sdk/mcp`, standard `net/http`, `gopkg.in/yaml.v2`, anvil daemon 0.2.0 HTTP API, Go unit tests with `testing` and `net/http/httptest`.

---

## Source References

- Spec: `docs/superpowers/specs/2026-05-11-anvil-ironclaw-mcp-v1-design.md`
- anvil daemon API: `cmd/goose-daemon/api.go`
- anvil config patterns: `cmd/goose-daemon/config.go`, `cmd/goose-daemon/config_test.go`
- Official MCP Go SDK docs: `https://go.sdk.modelcontextprotocol.io/`
- Official MCP Go SDK repository: `https://github.com/modelcontextprotocol/go-sdk`

## Scope Check

This plan covers one subsystem: the anvil-owned MCP adapter. It does not implement IronClaw-side code, workspace sync, snapshots, restore, persistent session storage, or daemon API changes. Those are separate future specs.

## File Structure

Create:

- `cmd/anvil-mcp/main.go`: binary entrypoint; loads config, creates daemon client/session store/tool server, registers MCP tools, runs stdio transport.
- `internal/anvilmcp/config.go`: config defaults, YAML loading, environment override, URL validation, timeout parsing.
- `internal/anvilmcp/config_test.go`: config precedence and validation tests.
- `internal/anvilmcp/session_store.go`: concurrency-safe optional `session_name -> vm_id` alias map.
- `internal/anvilmcp/session_store_test.go`: bind, duplicate, resolve, remove tests.
- `internal/anvilmcp/daemon_client.go`: small HTTP client for anvil daemon 0.2.0 endpoints.
- `internal/anvilmcp/daemon_client_test.go`: `httptest` coverage for request shape, auth header, daemon error preservation, timeout context propagation.
- `internal/anvilmcp/tools.go`: MCP input/output structs and tool handlers.
- `internal/anvilmcp/tools_test.go`: tool handler validation, session resolution, timeout behavior, delete mapping cleanup.
- `configs/anvil-mcp.yaml.example`: example adapter config.

Modify:

- `go.mod`: raise Go version and add official MCP Go SDK.
- `go.sum`: updated by `go mod tidy`.
- `README.md`: add `anvil-mcp` build/config/use section and update minimum Go version.

Do not modify:

- `cmd/goose-daemon/api.go`
- `cmd/goose-agent/main.go`
- snapshot/restore code

---

### Task 1: Toolchain And MCP SDK Dependency

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `README.md`

- [ ] **Step 1: Confirm local toolchain and SDK requirement**

Run:

```bash
go version
go list -m -json github.com/modelcontextprotocol/go-sdk@v1.6.0
```

Expected:

```text
go version go1.25.x or newer ...
```

The SDK metadata should show `GoVersion` at least `1.25.0`. If local Go is older than the SDK requirement, stop and install a current Go toolchain before editing source.

- [ ] **Step 2: Update module Go version and add SDK**

Run:

```bash
go mod edit -go=1.25.0
go get github.com/modelcontextprotocol/go-sdk@v1.6.0
go mod tidy
```

Expected:

```text
go.mod and go.sum updated
```

- [ ] **Step 3: Verify existing project still builds**

Run:

```bash
go test ./...
```

Expected:

```text
ok  	ephemera/cmd/goose-agent
ok  	ephemera/cmd/goose-daemon
ok  	ephemera/internal/storage
```

Some packages may report `? ... [no test files]`. That is acceptable. Any compile failure from the Go version bump must be fixed before continuing.

- [ ] **Step 4: Update README prerequisite**

Edit the prerequisites table in `README.md` so the Go row reads:

```markdown
| **Go** | 1.25+ |
```

Add a short note near the build instructions:

```markdown
`cmd/anvil-mcp` uses the official MCP Go SDK, which requires Go 1.25 or newer.
```

- [ ] **Step 5: Commit**

Run:

```bash
git add go.mod go.sum README.md
git commit -m "chore: update Go toolchain for MCP adapter"
```

---

### Task 2: Config Loader

**Files:**
- Create: `internal/anvilmcp/config_test.go`
- Create: `internal/anvilmcp/config.go`

- [ ] **Step 1: Write failing config tests**

Create `internal/anvilmcp/config_test.go`:

```go
package anvilmcp

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(ConfigSource{
		Getenv: func(string) string { return "" },
		ReadFile: func(string) ([]byte, error) {
			return nil, errors.New("not found")
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.DaemonURL != "http://127.0.0.1:3000" {
		t.Fatalf("DaemonURL = %q", cfg.DaemonURL)
	}
	if cfg.APIToken != "" {
		t.Fatalf("APIToken = %q", cfg.APIToken)
	}
	if cfg.DefaultTimeoutSeconds != 300 {
		t.Fatalf("DefaultTimeoutSeconds = %d", cfg.DefaultTimeoutSeconds)
	}
}

func TestLoadConfigFile(t *testing.T) {
	files := map[string][]byte{
		"/tmp/anvil-mcp.yaml": []byte("daemon_url: https://anvil.example.com/\napi_token: file-token\ndefault_timeout_seconds: 45\n"),
	}
	cfg, err := LoadConfig(ConfigSource{
		Getenv: func(key string) string {
			if key == "ANVIL_MCP_CONFIG" {
				return "/tmp/anvil-mcp.yaml"
			}
			return ""
		},
		ReadFile: func(path string) ([]byte, error) {
			b, ok := files[path]
			if !ok {
				return nil, errors.New("missing")
			}
			return b, nil
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.DaemonURL != "https://anvil.example.com" {
		t.Fatalf("DaemonURL = %q", cfg.DaemonURL)
	}
	if cfg.APIToken != "file-token" {
		t.Fatalf("APIToken = %q", cfg.APIToken)
	}
	if cfg.DefaultTimeoutSeconds != 45 {
		t.Fatalf("DefaultTimeoutSeconds = %d", cfg.DefaultTimeoutSeconds)
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	files := map[string][]byte{
		"/tmp/anvil-mcp.yaml": []byte("daemon_url: http://file.example\napi_token: file-token\ndefault_timeout_seconds: 45\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG":          "/tmp/anvil-mcp.yaml",
		"ANVIL_DAEMON_URL":          "http://env.example:3000/",
		"ANVIL_API_TOKEN":           "env-token",
		"ANVIL_MCP_DEFAULT_TIMEOUT": "90",
	}
	cfg, err := LoadConfig(ConfigSource{
		Getenv: func(key string) string { return env[key] },
		ReadFile: func(path string) ([]byte, error) { return files[path], nil },
	})
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.DaemonURL != "http://env.example:3000" {
		t.Fatalf("DaemonURL = %q", cfg.DaemonURL)
	}
	if cfg.APIToken != "env-token" {
		t.Fatalf("APIToken = %q", cfg.APIToken)
	}
	if cfg.DefaultTimeoutSeconds != 90 {
		t.Fatalf("DefaultTimeoutSeconds = %d", cfg.DefaultTimeoutSeconds)
	}
}

func TestLoadConfigRejectsInvalidURL(t *testing.T) {
	_, err := LoadConfig(ConfigSource{
		Getenv: func(key string) string {
			if key == "ANVIL_DAEMON_URL" {
				return "ftp://example.com"
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("not found") },
	})
	if err == nil || !strings.Contains(err.Error(), "ANVIL_DAEMON_URL") {
		t.Fatalf("expected ANVIL_DAEMON_URL error, got %v", err)
	}
}

func TestLoadConfigRejectsInvalidTimeout(t *testing.T) {
	_, err := LoadConfig(ConfigSource{
		Getenv: func(key string) string {
			if key == "ANVIL_MCP_DEFAULT_TIMEOUT" {
				return "0"
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("not found") },
	})
	if err == nil || !strings.Contains(err.Error(), "ANVIL_MCP_DEFAULT_TIMEOUT") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run:

```bash
go test ./internal/anvilmcp -run TestLoadConfig -count=1
```

Expected:

```text
FAIL
undefined: LoadConfig
undefined: ConfigSource
```

- [ ] **Step 3: Implement config loader**

Create `internal/anvilmcp/config.go`:

```go
package anvilmcp

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"
)

const (
	DefaultDaemonURL             = "http://127.0.0.1:3000"
	DefaultTimeoutSeconds        = 300
	DefaultConfigPath            = "configs/anvil-mcp.yaml"
	envDaemonURL                 = "ANVIL_DAEMON_URL"
	envAPIToken                  = "ANVIL_API_TOKEN"
	envDefaultTimeout            = "ANVIL_MCP_DEFAULT_TIMEOUT"
	envConfigPath                = "ANVIL_MCP_CONFIG"
)

type Config struct {
	DaemonURL             string `yaml:"daemon_url"`
	APIToken              string `yaml:"api_token"`
	DefaultTimeoutSeconds int    `yaml:"default_timeout_seconds"`
}

type ConfigSource struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
}

func LoadConfig(src ConfigSource) (Config, error) {
	if src.Getenv == nil {
		src.Getenv = os.Getenv
	}
	if src.ReadFile == nil {
		src.ReadFile = os.ReadFile
	}

	cfg := Config{
		DaemonURL:             DefaultDaemonURL,
		DefaultTimeoutSeconds: DefaultTimeoutSeconds,
	}

	configPath := strings.TrimSpace(src.Getenv(envConfigPath))
	if configPath == "" {
		configPath = DefaultConfigPath
	}
	if b, err := src.ReadFile(configPath); err == nil {
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("%s: parse config: %w", configPath, err)
		}
	} else if src.Getenv(envConfigPath) != "" {
		return Config{}, fmt.Errorf("%s: read config: %w", configPath, err)
	}

	if v := strings.TrimSpace(src.Getenv(envDaemonURL)); v != "" {
		cfg.DaemonURL = v
	}
	if v := src.Getenv(envAPIToken); v != "" {
		cfg.APIToken = v
	}
	if v := strings.TrimSpace(src.Getenv(envDefaultTimeout)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive integer", envDefaultTimeout)
		}
		cfg.DefaultTimeoutSeconds = n
	}

	normalized, err := normalizeDaemonURL(cfg.DaemonURL, envDaemonURL)
	if err != nil {
		return Config{}, err
	}
	cfg.DaemonURL = normalized
	if cfg.DefaultTimeoutSeconds <= 0 {
		return Config{}, fmt.Errorf("default_timeout_seconds must be positive")
	}
	return cfg, nil
}

func normalizeDaemonURL(raw, label string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%s must not be empty", label)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s parse: %w", label, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%s must use http or https", label)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s must include host", label)
	}
	return strings.TrimRight(raw, "/"), nil
}
```

- [ ] **Step 4: Run config tests**

Run:

```bash
go test ./internal/anvilmcp -run TestLoadConfig -count=1
```

Expected:

```text
ok  	ephemera/internal/anvilmcp
```

- [ ] **Step 5: Commit**

Run:

```bash
git add internal/anvilmcp/config.go internal/anvilmcp/config_test.go
git commit -m "feat: add anvil MCP config loader"
```

---

### Task 3: Session Store

**Files:**
- Create: `internal/anvilmcp/session_store_test.go`
- Create: `internal/anvilmcp/session_store.go`

- [ ] **Step 1: Write failing session store tests**

Create `internal/anvilmcp/session_store_test.go`:

```go
package anvilmcp

import "testing"

func TestSessionStoreBindResolve(t *testing.T) {
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	vmID, ok := store.Resolve("work")
	if !ok || vmID != "vm-1" {
		t.Fatalf("Resolve = %q, %v", vmID, ok)
	}
}

func TestSessionStoreRejectsDuplicate(t *testing.T) {
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	if err := store.Bind("work", "vm-2"); err == nil {
		t.Fatal("expected duplicate session error")
	}
}

func TestSessionStoreExists(t *testing.T) {
	store := NewSessionStore()
	if store.Exists("work") {
		t.Fatal("empty store should not contain session")
	}
	store.Bind("work", "vm-1")
	if !store.Exists("work") {
		t.Fatal("session should exist")
	}
}

func TestSessionStoreResolveVMIDPriority(t *testing.T) {
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	vmID, err := store.ResolveIdentity("vm-explicit", "work")
	if err != nil {
		t.Fatalf("ResolveIdentity returned error: %v", err)
	}
	if vmID != "vm-explicit" {
		t.Fatalf("vm_id priority not honored: %q", vmID)
	}
}

func TestSessionStoreUnknownSession(t *testing.T) {
	store := NewSessionStore()
	_, err := store.ResolveIdentity("", "missing")
	if err == nil {
		t.Fatal("expected unknown session error")
	}
}

func TestSessionStoreRequiresIdentity(t *testing.T) {
	store := NewSessionStore()
	_, err := store.ResolveIdentity("", "")
	if err == nil {
		t.Fatal("expected missing identity error")
	}
}

func TestSessionStoreRemoveVM(t *testing.T) {
	store := NewSessionStore()
	store.Bind("a", "vm-1")
	store.Bind("b", "vm-2")
	store.Bind("c", "vm-1")
	store.RemoveVM("vm-1")
	if _, ok := store.Resolve("a"); ok {
		t.Fatal("session a should be removed")
	}
	if _, ok := store.Resolve("c"); ok {
		t.Fatal("session c should be removed")
	}
	if vmID, ok := store.Resolve("b"); !ok || vmID != "vm-2" {
		t.Fatalf("session b should remain, got %q %v", vmID, ok)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run:

```bash
go test ./internal/anvilmcp -run TestSessionStore -count=1
```

Expected:

```text
FAIL
undefined: NewSessionStore
```

- [ ] **Step 3: Implement session store**

Create `internal/anvilmcp/session_store.go`:

```go
package anvilmcp

import (
	"fmt"
	"strings"
	"sync"
)

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]string
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]string)}
}

func (s *SessionStore) Bind(sessionName, vmID string) error {
	sessionName = strings.TrimSpace(sessionName)
	vmID = strings.TrimSpace(vmID)
	if sessionName == "" {
		return fmt.Errorf("session_name must not be empty")
	}
	if vmID == "" {
		return fmt.Errorf("vm_id must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.sessions[sessionName]; existing != "" {
		return fmt.Errorf("session_name %q already maps to %s", sessionName, existing)
	}
	s.sessions[sessionName] = vmID
	return nil
}

func (s *SessionStore) Resolve(sessionName string) (string, bool) {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	vmID, ok := s.sessions[sessionName]
	return vmID, ok
}

func (s *SessionStore) Exists(sessionName string) bool {
	_, ok := s.Resolve(sessionName)
	return ok
}

func (s *SessionStore) ResolveIdentity(vmID, sessionName string) (string, error) {
	vmID = strings.TrimSpace(vmID)
	sessionName = strings.TrimSpace(sessionName)
	if vmID != "" {
		return vmID, nil
	}
	if sessionName == "" {
		return "", fmt.Errorf("vm_id or session_name is required")
	}
	resolved, ok := s.Resolve(sessionName)
	if !ok {
		return "", fmt.Errorf("unknown session_name %q", sessionName)
	}
	return resolved, nil
}

func (s *SessionStore) RemoveVM(vmID string) {
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for sessionName, mappedVMID := range s.sessions {
		if mappedVMID == vmID {
			delete(s.sessions, sessionName)
		}
	}
}
```

- [ ] **Step 4: Run session tests**

Run:

```bash
go test ./internal/anvilmcp -run TestSessionStore -count=1
```

Expected:

```text
ok  	ephemera/internal/anvilmcp
```

- [ ] **Step 5: Commit**

Run:

```bash
git add internal/anvilmcp/session_store.go internal/anvilmcp/session_store_test.go
git commit -m "feat: add anvil MCP session aliases"
```

---

### Task 4: Daemon HTTP Client

**Files:**
- Create: `internal/anvilmcp/daemon_client_test.go`
- Create: `internal/anvilmcp/daemon_client.go`

- [ ] **Step 1: Write failing daemon client tests**

Create `internal/anvilmcp/daemon_client_test.go`:

```go
package anvilmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDaemonClientSpawnVM(t *testing.T) {
	var gotAuth, gotProfile string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/vms" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		gotProfile = req.Profile
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"vm_id":"vm-1","guest_ip":"10.0.1.2","agent_url":"http://127.0.0.1:3000/vms/vm-1","profile":"dev","agent_token":"secret"}`))
	}))
	defer ts.Close()

	client := NewDaemonClient(Config{DaemonURL: ts.URL, APIToken: "token", DefaultTimeoutSeconds: 300}, ts.Client())
	res, err := client.SpawnVM(context.Background(), "dev")
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}
	if gotAuth != "Bearer token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotProfile != "dev" {
		t.Fatalf("profile = %q", gotProfile)
	}
	if res.VMID != "vm-1" || res.AgentToken != "secret" {
		t.Fatalf("unexpected response: %+v", res)
	}
}

func TestDaemonClientRawEndpoints(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	client := NewDaemonClient(Config{DaemonURL: ts.URL, APIToken: "token", DefaultTimeoutSeconds: 300}, ts.Client())
	if _, err := client.RunTask(context.Background(), "vm-1", "hello"); err != nil {
		t.Fatalf("RunTask returned error: %v", err)
	}
	if _, err := client.Health(context.Background(), "vm-1"); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if _, err := client.Stop(context.Background(), "vm-1"); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if _, err := client.Delete(context.Background(), "vm-1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	want := []string{
		"POST /vms/vm-1/tasks",
		"GET /vms/vm-1/health",
		"POST /vms/vm-1/stop",
		"DELETE /vms/vm-1",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestDaemonClientPreservesDaemonError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"agent unreachable"}`))
	}))
	defer ts.Close()

	client := NewDaemonClient(Config{DaemonURL: ts.URL, DefaultTimeoutSeconds: 300}, ts.Client())
	_, err := client.Health(context.Background(), "vm-1")
	if err == nil {
		t.Fatal("expected daemon error")
	}
	daemonErr, ok := err.(*DaemonError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if daemonErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d", daemonErr.StatusCode)
	}
	if daemonErr.Body != `{"error":"agent unreachable"}` {
		t.Fatalf("Body = %q", daemonErr.Body)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run:

```bash
go test ./internal/anvilmcp -run TestDaemonClient -count=1
```

Expected:

```text
FAIL
undefined: NewDaemonClient
```

- [ ] **Step 3: Implement daemon client**

Create `internal/anvilmcp/daemon_client.go`:

```go
package anvilmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type DaemonClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type DaemonError struct {
	StatusCode int
	Body       string
}

func (e *DaemonError) Error() string {
	return fmt.Sprintf("daemon returned status %d: %s", e.StatusCode, e.Body)
}

type SpawnVMResponse struct {
	VMID       string `json:"vm_id"`
	GuestIP    string `json:"guest_ip"`
	AgentURL   string `json:"agent_url"`
	Profile    string `json:"profile,omitempty"`
	AgentToken string `json:"agent_token,omitempty"`
}

type RawDaemonResponse struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func NewDaemonClient(cfg Config, httpClient *http.Client) *DaemonClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &DaemonClient{
		baseURL: cfg.DaemonURL,
		token:   cfg.APIToken,
		http:    httpClient,
	}
}

func (c *DaemonClient) SpawnVM(ctx context.Context, profile string) (*SpawnVMResponse, error) {
	payload := map[string]string{}
	if profile != "" {
		payload["profile"] = profile
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	respBody, err := c.do(ctx, http.MethodPost, "/vms", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var res SpawnVMResponse
	if err := json.Unmarshal([]byte(respBody.Body), &res); err != nil {
		return nil, fmt.Errorf("decode spawn response: %w", err)
	}
	return &res, nil
}

func (c *DaemonClient) RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	body, err := json.Marshal(map[string]string{"prompt": prompt})
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, "/vms/"+vmID+"/tasks", "application/json", bytes.NewReader(body))
}

func (c *DaemonClient) Health(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	return c.do(ctx, http.MethodGet, "/vms/"+vmID+"/health", "", nil)
}

func (c *DaemonClient) Stop(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	return c.do(ctx, http.MethodPost, "/vms/"+vmID+"/stop", "application/json", nil)
}

func (c *DaemonClient) Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	return c.do(ctx, http.MethodDelete, "/vms/"+vmID, "", nil)
}

func (c *DaemonClient) do(ctx context.Context, method, path, contentType string, body io.Reader) (*RawDaemonResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	raw := &RawDaemonResponse{StatusCode: resp.StatusCode, Body: string(b)}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &DaemonError{StatusCode: resp.StatusCode, Body: raw.Body}
	}
	return raw, nil
}
```

- [ ] **Step 4: Run daemon client tests**

Run:

```bash
go test ./internal/anvilmcp -run TestDaemonClient -count=1
```

Expected:

```text
ok  	ephemera/internal/anvilmcp
```

- [ ] **Step 5: Commit**

Run:

```bash
git add internal/anvilmcp/daemon_client.go internal/anvilmcp/daemon_client_test.go
git commit -m "feat: add anvil daemon MCP client"
```

---

### Task 5: MCP Tool Handlers

**Files:**
- Create: `internal/anvilmcp/tools_test.go`
- Create: `internal/anvilmcp/tools.go`

- [ ] **Step 1: Write failing tool handler tests**

Create `internal/anvilmcp/tools_test.go`:

```go
package anvilmcp

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeDaemon struct {
	spawnProfile string
	spawnCalls   int
	spawnRes     *SpawnVMResponse
	rawRes       *RawDaemonResponse
	deletedVMID  string
	runPrompt    string
	runVMID      string
	err          error
}

func (f *fakeDaemon) SpawnVM(ctx context.Context, profile string) (*SpawnVMResponse, error) {
	f.spawnCalls++
	f.spawnProfile = profile
	if f.err != nil {
		return nil, f.err
	}
	return f.spawnRes, nil
}

func (f *fakeDaemon) RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	f.runVMID = vmID
	f.runPrompt = prompt
	if f.err != nil {
		return nil, f.err
	}
	return f.rawRes, nil
}

func (f *fakeDaemon) Health(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rawRes, nil
}

func (f *fakeDaemon) Stop(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rawRes, nil
}

func (f *fakeDaemon) Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	f.deletedVMID = vmID
	if f.err != nil {
		return nil, f.err
	}
	return f.rawRes, nil
}

func TestToolsSpawnBindsSession(t *testing.T) {
	fake := &fakeDaemon{spawnRes: &SpawnVMResponse{VMID: "vm-1", GuestIP: "10.0.1.2", AgentURL: "http://agent", Profile: "dev"}}
	tools := NewTools(fake, NewSessionStore(), 300*time.Second)
	out, err := tools.SpawnVM(context.Background(), SpawnVMInput{Profile: "dev", SessionName: "work"})
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}
	if fake.spawnProfile != "dev" {
		t.Fatalf("profile = %q", fake.spawnProfile)
	}
	if out.VMID != "vm-1" || out.SessionName != "work" {
		t.Fatalf("output = %+v", out)
	}
	vmID, ok := tools.sessions.Resolve("work")
	if !ok || vmID != "vm-1" {
		t.Fatalf("session mapping = %q %v", vmID, ok)
	}
}

func TestToolsSpawnRejectsDuplicateSessionBeforeDaemonCall(t *testing.T) {
	fake := &fakeDaemon{spawnRes: &SpawnVMResponse{VMID: "vm-2"}}
	store := NewSessionStore()
	store.Bind("work", "vm-1")
	tools := NewTools(fake, store, 300*time.Second)
	_, err := tools.SpawnVM(context.Background(), SpawnVMInput{SessionName: "work"})
	if err == nil {
		t.Fatal("expected duplicate session error")
	}
	if fake.spawnCalls != 0 {
		t.Fatalf("daemon should not be called for duplicate session, calls=%d", fake.spawnCalls)
	}
}

func TestToolsRunTaskUsesSession(t *testing.T) {
	fake := &fakeDaemon{rawRes: &RawDaemonResponse{StatusCode: 200, Body: `{"output":"ok"}`}}
	store := NewSessionStore()
	store.Bind("work", "vm-1")
	tools := NewTools(fake, store, 300*time.Second)
	out, err := tools.RunTask(context.Background(), RunTaskInput{SessionName: "work", Prompt: "hello"})
	if err != nil {
		t.Fatalf("RunTask returned error: %v", err)
	}
	if fake.runVMID != "vm-1" || fake.runPrompt != "hello" {
		t.Fatalf("run vm=%q prompt=%q", fake.runVMID, fake.runPrompt)
	}
	if out.Body != `{"output":"ok"}` {
		t.Fatalf("body = %q", out.Body)
	}
}

func TestToolsRunTaskRequiresPrompt(t *testing.T) {
	tools := NewTools(&fakeDaemon{}, NewSessionStore(), 300*time.Second)
	_, err := tools.RunTask(context.Background(), RunTaskInput{VMID: "vm-1"})
	if err == nil {
		t.Fatal("expected prompt validation error")
	}
}

func TestToolsDeleteRemovesSession(t *testing.T) {
	fake := &fakeDaemon{rawRes: &RawDaemonResponse{StatusCode: 200, Body: `{"status":"stopped"}`}}
	store := NewSessionStore()
	store.Bind("work", "vm-1")
	tools := NewTools(fake, store, 300*time.Second)
	if _, err := tools.DeleteVM(context.Background(), VMIdentityInput{SessionName: "work"}); err != nil {
		t.Fatalf("DeleteVM returned error: %v", err)
	}
	if fake.deletedVMID != "vm-1" {
		t.Fatalf("deletedVMID = %q", fake.deletedVMID)
	}
	if _, ok := store.Resolve("work"); ok {
		t.Fatal("session mapping should be removed after delete")
	}
}

func TestToolsReturnsDaemonError(t *testing.T) {
	daemonErr := &DaemonError{StatusCode: 502, Body: `{"error":"agent unreachable"}`}
	tools := NewTools(&fakeDaemon{err: daemonErr}, NewSessionStore(), 300*time.Second)
	_, err := tools.Health(context.Background(), VMIdentityInput{VMID: "vm-1"})
	if !errors.Is(err, daemonErr) {
		t.Fatalf("expected daemon error, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run:

```bash
go test ./internal/anvilmcp -run TestTools -count=1
```

Expected:

```text
FAIL
undefined: NewTools
undefined: SpawnVMInput
```

- [ ] **Step 3: Implement tool handlers**

Create `internal/anvilmcp/tools.go`:

```go
package anvilmcp

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Daemon interface {
	SpawnVM(ctx context.Context, profile string) (*SpawnVMResponse, error)
	RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error)
	Health(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	Stop(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error)
}

type Tools struct {
	daemon         Daemon
	sessions       *SessionStore
	defaultTimeout time.Duration
}

type SpawnVMInput struct {
	Profile     string `json:"profile,omitempty" jsonschema:"optional anvil profile to use when creating the VM"`
	SessionName string `json:"session_name,omitempty" jsonschema:"optional local alias to bind to the created VM ID"`
}

type SpawnVMOutput struct {
	VMID        string `json:"vm_id"`
	GuestIP     string `json:"guest_ip"`
	AgentURL    string `json:"agent_url"`
	Profile     string `json:"profile,omitempty"`
	SessionName string `json:"session_name,omitempty"`
}

type RunTaskInput struct {
	VMID           string `json:"vm_id,omitempty" jsonschema:"explicit anvil VM ID; takes priority over session_name"`
	SessionName    string `json:"session_name,omitempty" jsonschema:"optional local alias previously bound by anvil_spawn_vm"`
	Prompt         string `json:"prompt" jsonschema:"prompt to send to the in-VM goose agent"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"optional task timeout in seconds"`
}

type VMIdentityInput struct {
	VMID        string `json:"vm_id,omitempty" jsonschema:"explicit anvil VM ID; takes priority over session_name"`
	SessionName string `json:"session_name,omitempty" jsonschema:"optional local alias previously bound by anvil_spawn_vm"`
}

func NewTools(daemon Daemon, sessions *SessionStore, defaultTimeout time.Duration) *Tools {
	if sessions == nil {
		sessions = NewSessionStore()
	}
	if defaultTimeout <= 0 {
		defaultTimeout = DefaultTimeoutSeconds * time.Second
	}
	return &Tools{daemon: daemon, sessions: sessions, defaultTimeout: defaultTimeout}
}

func (t *Tools) SpawnVM(ctx context.Context, input SpawnVMInput) (SpawnVMOutput, error) {
	sessionName := strings.TrimSpace(input.SessionName)
	profile := strings.TrimSpace(input.Profile)
	if sessionName != "" && t.sessions.Exists(sessionName) {
		return SpawnVMOutput{}, fmt.Errorf("session_name %q already exists", sessionName)
	}
	res, err := t.daemon.SpawnVM(ctx, profile)
	if err != nil {
		return SpawnVMOutput{}, err
	}
	if sessionName != "" {
		if err := t.sessions.Bind(sessionName, res.VMID); err != nil {
			_, _ = t.daemon.Delete(ctx, res.VMID)
			return SpawnVMOutput{}, err
		}
	}
	return SpawnVMOutput{
		VMID:        res.VMID,
		GuestIP:     res.GuestIP,
		AgentURL:    res.AgentURL,
		Profile:     res.Profile,
		SessionName: sessionName,
	}, nil
}

func (t *Tools) RunTask(ctx context.Context, input RunTaskInput) (RawDaemonResponse, error) {
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		return RawDaemonResponse{}, fmt.Errorf("prompt must not be empty")
	}
	vmID, err := t.sessions.ResolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return RawDaemonResponse{}, err
	}
	timeout := t.defaultTimeout
	if input.TimeoutSeconds > 0 {
		timeout = time.Duration(input.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := t.daemon.RunTask(ctx, vmID, prompt)
	if err != nil {
		return RawDaemonResponse{}, err
	}
	return *res, nil
}

func (t *Tools) Health(ctx context.Context, input VMIdentityInput) (RawDaemonResponse, error) {
	vmID, err := t.sessions.ResolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return RawDaemonResponse{}, err
	}
	res, err := t.daemon.Health(ctx, vmID)
	if err != nil {
		return RawDaemonResponse{}, err
	}
	return *res, nil
}

func (t *Tools) StopVM(ctx context.Context, input VMIdentityInput) (RawDaemonResponse, error) {
	vmID, err := t.sessions.ResolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return RawDaemonResponse{}, err
	}
	res, err := t.daemon.Stop(ctx, vmID)
	if err != nil {
		return RawDaemonResponse{}, err
	}
	return *res, nil
}

func (t *Tools) DeleteVM(ctx context.Context, input VMIdentityInput) (RawDaemonResponse, error) {
	vmID, err := t.sessions.ResolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return RawDaemonResponse{}, err
	}
	res, err := t.daemon.Delete(ctx, vmID)
	if err != nil {
		return RawDaemonResponse{}, err
	}
	t.sessions.RemoveVM(vmID)
	return *res, nil
}
```

- [ ] **Step 4: Run tool handler tests**

Run:

```bash
go test ./internal/anvilmcp -run TestTools -count=1
```

Expected:

```text
ok  	ephemera/internal/anvilmcp
```

- [ ] **Step 5: Commit**

Run:

```bash
git add internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go
git commit -m "feat: add anvil MCP tool handlers"
```

---

### Task 6: MCP Stdio Server Entrypoint

**Files:**
- Create: `cmd/anvil-mcp/main.go`
- Modify: `internal/anvilmcp/tools.go`
- Test: `go test ./...`

- [ ] **Step 1: Add MCP adapter methods**

Modify `internal/anvilmcp/tools.go` by appending MCP SDK adapter methods. These methods keep `Tools` testable without MCP and keep SDK-specific code at the boundary.

```go
// Add imports in tools.go:
// "github.com/modelcontextprotocol/go-sdk/mcp"

func (t *Tools) MCPSpawnVM(ctx context.Context, req *mcp.CallToolRequest, input SpawnVMInput) (*mcp.CallToolResult, SpawnVMOutput, error) {
	out, err := t.SpawnVM(ctx, input)
	return nil, out, err
}

func (t *Tools) MCPRunTask(ctx context.Context, req *mcp.CallToolRequest, input RunTaskInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.RunTask(ctx, input)
	return nil, out, err
}

func (t *Tools) MCPHealth(ctx context.Context, req *mcp.CallToolRequest, input VMIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.Health(ctx, input)
	return nil, out, err
}

func (t *Tools) MCPStopVM(ctx context.Context, req *mcp.CallToolRequest, input VMIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.StopVM(ctx, input)
	return nil, out, err
}

func (t *Tools) MCPDeleteVM(ctx context.Context, req *mcp.CallToolRequest, input VMIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.DeleteVM(ctx, input)
	return nil, out, err
}
```

If the SDK handler signature has changed, verify it with:

```bash
go doc github.com/modelcontextprotocol/go-sdk/mcp.AddTool
```

Keep the non-MCP methods unchanged and adapt only these wrapper methods.

- [ ] **Step 2: Create entrypoint**

Create `cmd/anvil-mcp/main.go`:

```go
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"ephemera/internal/anvilmcp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "v0.1.0"

func main() {
	cfg, err := anvilmcp.LoadConfig(anvilmcp.ConfigSource{})
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	daemon := anvilmcp.NewDaemonClient(cfg, http.DefaultClient)
	tools := anvilmcp.NewTools(
		daemon,
		anvilmcp.NewSessionStore(),
		time.Duration(cfg.DefaultTimeoutSeconds)*time.Second,
	)

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "anvil-mcp",
		Version: version,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_spawn_vm",
		Description: "Create an anvil VM and optionally bind a local session_name alias.",
	}, tools.MCPSpawnVM)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_run_task",
		Description: "Run a prompt synchronously in an existing anvil VM using vm_id or session_name.",
	}, tools.MCPRunTask)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_get_vm_health",
		Description: "Return health for an existing anvil VM agent using vm_id or session_name.",
	}, tools.MCPHealth)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_stop_vm",
		Description: "Ask the anvil VM agent to stop gracefully without deleting VM resources.",
	}, tools.MCPStopVM)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_delete_vm",
		Description: "Delete an anvil VM and release its local session_name alias if present.",
	}, tools.MCPDeleteVM)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}
```

- [ ] **Step 3: Build the new binary**

Run:

```bash
go build ./cmd/anvil-mcp
```

Expected:

```text
no output
```

- [ ] **Step 4: Run all tests**

Run:

```bash
go test ./...
```

Expected:

```text
all packages pass
```

- [ ] **Step 5: Commit**

Run:

```bash
git add internal/anvilmcp/tools.go cmd/anvil-mcp/main.go
git commit -m "feat: add anvil MCP stdio server"
```

---

### Task 7: Config Example And README Usage

**Files:**
- Create: `configs/anvil-mcp.yaml.example`
- Modify: `README.md`

- [ ] **Step 1: Add example config**

Create `configs/anvil-mcp.yaml.example`:

```yaml
# anvil-mcp configuration.
# Environment variables override these values:
# - ANVIL_DAEMON_URL
# - ANVIL_API_TOKEN
# - ANVIL_MCP_DEFAULT_TIMEOUT

daemon_url: http://127.0.0.1:3000
api_token: ""
default_timeout_seconds: 300
```

- [ ] **Step 2: Add README section**

Add this section after the API configuration section in `README.md`:

````markdown
## IronClaw MCP Adapter

`cmd/anvil-mcp` exposes the anvil daemon API as a stdio MCP server for IronClaw.

Build:

```bash
go build -o anvil-mcp ./cmd/anvil-mcp
```

Configure with environment variables:

```bash
export ANVIL_DAEMON_URL=http://127.0.0.1:3000
export ANVIL_API_TOKEN="$EPHEMERA_API_TOKEN"
export ANVIL_MCP_DEFAULT_TIMEOUT=300
```

Or use a config file:

```bash
cp configs/anvil-mcp.yaml.example configs/anvil-mcp.yaml
export ANVIL_MCP_CONFIG=configs/anvil-mcp.yaml
```

MCP tools:

- `anvil_spawn_vm`: create a VM; accepts optional `profile` and `session_name`
- `anvil_run_task`: run a prompt in a VM; accepts `vm_id` or `session_name`
- `anvil_get_vm_health`: check VM agent health
- `anvil_stop_vm`: ask the agent to stop gracefully
- `anvil_delete_vm`: delete the VM and release its session alias

v1 is a thin runtime bridge. It does not copy workspace files, create snapshots, restore sessions, or delete VMs automatically.
````

If the README already has a better location for adapter docs, place this content there without changing its meaning.

- [ ] **Step 3: Verify docs and build**

Run:

```bash
go test ./...
go build ./cmd/anvil-mcp
```

Expected:

```text
tests pass and build succeeds
```

- [ ] **Step 4: Commit**

Run:

```bash
git add configs/anvil-mcp.yaml.example README.md
git commit -m "docs: add anvil MCP adapter usage"
```

---

### Task 8: Final Verification And Release Notes

**Files:**
- Modify: `RELEASE_NOTES.md`

- [ ] **Step 1: Add release note entry**

Add an unreleased section at the top of `RELEASE_NOTES.md`:

````markdown
# Unreleased

## Added

- `cmd/anvil-mcp`: Go stdio MCP server for IronClaw integration.
- MCP tools for VM spawn, task execution, health, stop, and delete.
- Optional in-memory `session_name` aliases for MCP callers.
- `configs/anvil-mcp.yaml.example` for adapter configuration.

## Changed

- Minimum Go version is now 1.25+ to support the official MCP Go SDK.
````

- [ ] **Step 2: Run full verification**

Run:

```bash
go test ./...
go vet ./...
go build ./cmd/anvil-mcp
go build ./cmd/goose-daemon
go build ./cmd/goose-agent
go build ./cmd/micro-init
```

Expected:

```text
all commands exit 0
```

- [ ] **Step 3: Inspect git diff**

Run:

```bash
git diff --check
git status --short
```

Expected:

```text
git diff --check prints no output
status shows only intended tracked changes for this task
```

- [ ] **Step 4: Commit**

Run:

```bash
git add RELEASE_NOTES.md
git commit -m "docs: note anvil MCP adapter"
```

---

## Manual Smoke Test

Run this only on a host capable of running anvil daemon and Firecracker.

- [ ] **Step 1: Build binaries**

```bash
go build -o anvil-daemon ./cmd/goose-daemon
go build -o anvil-mcp ./cmd/anvil-mcp
```

- [ ] **Step 2: Start daemon**

```bash
sudo EPHEMERA_API_TOKEN=test-token ./anvil-daemon
```

Expected:

```text
Control plane API on 127.0.0.1:3000
```

- [ ] **Step 3: Configure MCP adapter**

```bash
export ANVIL_DAEMON_URL=http://127.0.0.1:3000
export ANVIL_API_TOKEN=test-token
export ANVIL_MCP_DEFAULT_TIMEOUT=300
```

- [ ] **Step 4: Exercise through an MCP client**

Use IronClaw's MCP client runner or the official Go SDK client example pattern to call:

```text
anvil_spawn_vm {"session_name":"smoke"}
anvil_run_task {"session_name":"smoke","prompt":"print the working directory"}
anvil_get_vm_health {"session_name":"smoke"}
anvil_stop_vm {"session_name":"smoke"}
anvil_delete_vm {"session_name":"smoke"}
```

Expected:

```text
spawn returns vm_id
run_task returns daemon task body
health returns daemon health body
stop returns daemon stop body
delete returns daemon delete body
```

If `anvil_stop_vm` causes the daemon delete endpoint to report VM not found later, record the observed daemon behavior and adjust docs only after confirming actual 0.2.0 semantics.

---

## Self-Review Checklist

- Spec coverage:
  - location `cmd/anvil-mcp`: Task 6
  - Go implementation: all tasks
  - stdio v1: Task 6
  - local/remote daemon URL: Task 2
  - stateless plus optional session alias: Task 3 and Task 5
  - sync task with timeout: Task 5
  - explicit cleanup only: Task 5
  - profile at spawn only: Task 4 and Task 5
  - daemon error preservation: Task 4 and Task 5
  - config env/file/default: Task 2 and Task 7
  - workspace copy-in/out excluded: README and release notes in Task 7 and Task 8
- Placeholder scan:
  - no unresolved placeholder markers
  - no undefined file paths
  - every code-changing task includes code or command content
- Type consistency:
  - `VMID` JSON field is `vm_id`
  - `SessionName` JSON field is `session_name`
  - `DefaultTimeoutSeconds` maps to `default_timeout_seconds`
  - `RawDaemonResponse` is used for non-spawn daemon response bodies
