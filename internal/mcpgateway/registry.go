package mcpgateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	yaml "gopkg.in/yaml.v2"
)

// 학습 주석 개요: 이 파일은 configs/mcp/servers.yaml + secrets.yaml 을 읽어
// backend 목록(Registry)을 만든다. v0.6.0 core 는 HTTPBackend(transport "http")
// 만 있었고, v0.6.4 에서 StdioBackend(transport "stdio", 로컬 subprocess)가
// additive 로 추가됐다 — validateServer 의 switch 문에서 두 transport 의
// 상호배타 제약(http 는 command/args 불가, stdio 는 url 불가 등)을 확인한다.
// 핵심 경계: ServerConfig(내부, Credential 문자열 보유 가능)와 ServerInfo(외부
// API/UI 노출용, HasCredential bool 만 보유, Args 필드 자체가 없음)를 분리해
// secret 값이 어떤 API 응답에도 나갈 수 없게 타입 수준에서 막는다.

// ServerConfig describes one backend MCP server, loaded from configs/mcp/servers.yaml.
// Credentials are referenced by key (resolved from configs/mcp/secrets.yaml) so the
// secret value never appears in this struct or any API/UI response.
type ServerConfig struct {
	ID            string   `yaml:"id"`             // stable identifier and default namespace
	Namespace     string   `yaml:"namespace"`      // tool-name prefix; defaults to ID
	Transport     string   `yaml:"transport"`      // "http" (Streamable HTTP, default) or "stdio" (local subprocess, v0.6.4)
	URL           string   `yaml:"url"`            // backend MCP endpoint (http only)
	Command       string   `yaml:"command"`        // executable to spawn (stdio only); a name is resolved on the daemon's PATH at startup
	Args          []string `yaml:"args"`           // command arguments (stdio only); never place secrets here — the cmdline is world-readable
	Credential    string   `yaml:"credential"`     // key into secrets.yaml; optional (public server)
	CredentialEnv string   `yaml:"credential_env"` // env var the credential token is injected as (stdio only; child env, never the cmdline)
	Profiles      []string `yaml:"profiles"`       // Ephemera profiles allowed to use it; empty = all
	ToolsAllow    []string `yaml:"tools_allow"`    // if non-empty, only these tool names are exposed
	ToolsDeny     []string `yaml:"tools_deny"`     // tool names to hide (ignored when tools_allow is set)
}

// serversFile is the on-disk shape of servers.yaml.
type serversFile struct {
	Servers []ServerConfig `yaml:"servers"`
}

// ServerInfo is the secret-free view of a configured backend for the API/UI.
// Args is deliberately absent: argument vectors may embed sensitive values.
type ServerInfo struct {
	ID            string   `json:"id"`
	Namespace     string   `json:"namespace"`
	Transport     string   `json:"transport"`
	URL           string   `json:"url"`
	Command       string   `json:"command,omitempty"` // stdio only
	Profiles      []string `json:"profiles"`
	HasCredential bool     `json:"has_credential"`
}

// Registry holds the configured backends and indexes them by id and namespace.
type Registry struct {
	servers  []ServerConfig
	backends map[string]Backend // by id
	byNS     map[string]Backend // by namespace
}

// LoadServers parses servers.yaml. A missing file yields an empty config (the
// gateway then serves an empty catalog), not an error.
// 학습 주석: cmd/goose-daemon/mcp_gateway.go 의 initMCPGateway 가 이 함수를
// 호출해 실패하면 gateway 전체를 비활성 상태로 남긴다(fatal 아님, VM 은 영향 없음).
func LoadServers(path string) ([]ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f serversFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f.Servers, nil
}

// LoadSecrets parses secrets.yaml into a flat key→token map. A missing file
// yields an empty map (backends are then treated as unauthenticated).
// 학습 주석: secrets.yaml 은 gitignored 운영 파일(configs/mcp/secrets.yaml) —
// 이 값은 NewRegistry 안에서만 소비되고 Registry 구조체 필드로 남지 않는다.
func LoadSecrets(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// RegistryOption customizes NewRegistry (stdio backend spawning, v0.6.4).
type RegistryOption func(*registryConfig)

// registryConfig collects the knobs stdio backends need at build time.
type registryConfig struct {
	stdioUser string // unprivileged user stdio children run as when the daemon is root
	stdioDir  string // parent of the per-server scratch dirs (child cwd + HOME)
}

// WithStdioUser sets the unprivileged user stdio backend subprocesses run as
// when the daemon runs as root. Empty keeps the default ("nobody").
func WithStdioUser(name string) RegistryOption {
	return func(c *registryConfig) {
		if strings.TrimSpace(name) != "" {
			c.stdioUser = name
		}
	}
}

// WithStdioDir sets the parent directory for per-server scratch dirs (each
// stdio child's working directory and HOME). Empty keeps the default.
func WithStdioDir(dir string) RegistryOption {
	return func(c *registryConfig) {
		if strings.TrimSpace(dir) != "" {
			c.stdioDir = dir
		}
	}
}

// NewRegistry validates the server configs, resolves their credentials, and
// builds a backend per server: HTTPBackend for transport "http", StdioBackend
// for transport "stdio". HTTP credentials feed the CredentialProvider wired into
// each HTTP backend; stdio credentials become one child env var each. Secrets
// never leave this constructor.
// 학습 주석: credential 이 host 측에서만 다뤄지는 지점이 바로 여기다 — httpTokens/
// stdioTokens 는 이 함수 로컬 변수이고, 반환되는 *Registry 에는 저장되지 않는다.
// stdio 는 s.CredentialEnv 로 지정된 이름의 child 환경변수 하나로만 주입되며
// argv(s.Args)에는 절대 들어가지 않는다(cmdline 은 world-readable 이기 때문).
func NewRegistry(servers []ServerConfig, secrets map[string]string, client *http.Client, opts ...RegistryOption) (*Registry, error) {
	cfg := registryConfig{
		stdioUser: defaultStdioUser,
		stdioDir:  filepath.Join(os.TempDir(), "ephemera-mcp-stdio"),
	}
	for _, o := range opts {
		o(&cfg)
	}
	httpTokens := map[string]string{}  // http server id → bearer token
	stdioTokens := map[string]string{} // stdio server id → token (child env var)
	seenID := map[string]bool{}        // duplicate-id guard
	seenNS := map[string]bool{}        // duplicate-namespace guard
	hasStdio := false
	for i := range servers {
		s := &servers[i]
		if err := validateServer(*s); err != nil {
			return nil, err
		}
		if s.Namespace == "" {
			s.Namespace = s.ID
		}
		if s.Transport == "" {
			s.Transport = "http"
		}
		hasStdio = hasStdio || s.Transport == "stdio"
		if seenID[s.ID] {
			return nil, fmt.Errorf("duplicate server id %q", s.ID)
		}
		if seenNS[s.Namespace] {
			return nil, fmt.Errorf("duplicate namespace %q", s.Namespace)
		}
		seenID[s.ID], seenNS[s.Namespace] = true, true
		if s.Credential != "" {
			tok, ok := secrets[s.Credential]
			if !ok || strings.TrimSpace(tok) == "" {
				return nil, fmt.Errorf("server %q references credential %q not present in secrets", s.ID, s.Credential)
			}
			if s.Transport == "stdio" {
				stdioTokens[s.ID] = tok
			} else {
				httpTokens[s.ID] = tok
			}
		}
	}
	// Resolve the stdio run-as user once. Only meaningful as root: a non-root
	// daemon (dev, unit tests) cannot setuid, so children inherit its user.
	// A bad user name disables the whole gateway, like a missing credential.
	var stdioCred *syscall.Credential
	if hasStdio && os.Geteuid() == 0 {
		c, err := lookupStdioUser(cfg.stdioUser)
		if err != nil {
			return nil, fmt.Errorf("stdio user %q: %w", cfg.stdioUser, err)
		}
		stdioCred = c
	}
	creds := NewMapCredentialProvider(httpTokens)
	reg := &Registry{
		servers:  servers,
		backends: map[string]Backend{},
		byNS:     map[string]Backend{},
	}
	for _, s := range servers {
		var b Backend
		if s.Transport == "stdio" {
			// Resolve now (fail fast) and exec the absolute path later, so the
			// child's minimal PATH cannot change which binary runs.
			path, err := exec.LookPath(s.Command)
			if err != nil {
				return nil, fmt.Errorf("server %q: command %q: %w", s.ID, s.Command, err)
			}
			env := map[string]string{}
			if tok, ok := stdioTokens[s.ID]; ok {
				env[s.CredentialEnv] = tok
			}
			b = NewStdioBackend(s.ID, s.Namespace, StdioSpawnSpec{
				Command: path,
				Args:    s.Args,
				Env:     env,
				Cred:    stdioCred,
				Dir:     filepath.Join(cfg.stdioDir, s.ID),
			})
		} else {
			b = NewHTTPBackend(s.ID, s.Namespace, s.URL, client, creds)
		}
		reg.backends[s.ID] = b
		reg.byNS[s.Namespace] = b
	}
	return reg, nil
}

// validateServer enforces the minimal invariants the gateway relies on: a
// non-empty id, transport-consistent fields (http needs a URL, stdio needs a
// command), and a namespace free of the "__" separator the gateway uses to
// split namespaced tool names.
// 학습 주석: transport 별 상호배타 검증이 여기 있다 — http 는 command/args/
// credential_env 를 거부하고, stdio 는 url 을 거부하며 credential 과
// credential_env 를 반드시 짝으로 요구한다(설정 실수로 credential 이 새는 조합을
// 원천 차단).
func validateServer(s ServerConfig) error {
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("server has empty id")
	}
	switch s.Transport {
	case "", "http":
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			return fmt.Errorf("server %q: url must be http(s)", s.ID)
		}
		if s.Command != "" || len(s.Args) > 0 {
			return fmt.Errorf("server %q: command/args are only valid with transport \"stdio\"", s.ID)
		}
		if s.CredentialEnv != "" {
			return fmt.Errorf("server %q: credential_env is only valid with transport \"stdio\"", s.ID)
		}
	case "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("server %q: transport \"stdio\" requires command", s.ID)
		}
		if s.URL != "" {
			return fmt.Errorf("server %q: url is not valid with transport \"stdio\"", s.ID)
		}
		if (s.Credential != "") != (s.CredentialEnv != "") {
			return fmt.Errorf("server %q: credential and credential_env must be set together", s.ID)
		}
		if s.CredentialEnv != "" && !validEnvName(s.CredentialEnv) {
			return fmt.Errorf("server %q: credential_env %q is not a valid environment variable name", s.ID, s.CredentialEnv)
		}
	default:
		return fmt.Errorf("server %q: unsupported transport %q (only \"http\" or \"stdio\")", s.ID, s.Transport)
	}
	ns := s.Namespace
	if ns == "" {
		ns = s.ID
	}
	if strings.Contains(ns, namespaceSep) {
		return fmt.Errorf("server %q: namespace %q must not contain %q", s.ID, ns, namespaceSep)
	}
	return nil
}

// validEnvName reports whether s is a portable environment variable name:
// a letter or underscore, then letters, digits, or underscores.
func validEnvName(s string) bool {
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return s != ""
}

// Servers returns the secret-free view of every configured backend, sorted by id.
// 학습 주석: cmd/goose-daemon/mcp_api.go 의 GET /config/mcp/servers 가 이 결과를
// 그대로 직렬화한다 — ServerInfo 에 Args/Credential 필드가 없으므로 stdio 인자나
// 비밀값이 API 응답에 실릴 코드 경로 자체가 없다(mcp_servers_listing_anvil_test.go
// 가드).
func (r *Registry) Servers() []ServerInfo {
	out := make([]ServerInfo, 0, len(r.servers))
	for _, s := range r.servers {
		out = append(out, ServerInfo{
			ID:            s.ID,
			Namespace:     s.Namespace,
			Transport:     s.Transport,
			URL:           s.URL,
			Command:       s.Command,
			Profiles:      s.Profiles,
			HasCredential: s.Credential != "",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close shuts down backends that own external resources (stdio subprocesses);
// backends without a Close (HTTP) are untouched. Backends close in parallel so
// the total wall time is bounded by one backend's grace period, not their sum.
// 학습 주석: cmd/goose-daemon/mcp_gateway.go 의 stopMCPGateway 가 daemon
// 종료/재시작 시 이 함수로 stdio 자식 프로세스들을 정리한다(v0.6.4).
func (r *Registry) Close() {
	var wg sync.WaitGroup
	for _, b := range r.backends {
		c, ok := b.(io.Closer)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(c io.Closer) {
			defer wg.Done()
			_ = c.Close()
		}(c)
	}
	wg.Wait()
}

// ServerConfigs returns the loaded configs (for the PolicyStore).
func (r *Registry) ServerConfigs() []ServerConfig { return r.servers }

// Backend returns the backend with the given id.
func (r *Registry) Backend(id string) (Backend, bool) {
	b, ok := r.backends[id]
	return b, ok
}

// BackendByNamespace returns the backend serving the given namespace.
func (r *Registry) BackendByNamespace(ns string) (Backend, bool) {
	b, ok := r.byNS[ns]
	return b, ok
}

// Health is a backend's reachability, used by GET /config/mcp/servers.
type Health struct {
	ID    string `json:"id"`
	Up    bool   `json:"up"`
	Error string `json:"error,omitempty"`
}

// HealthAll probes every backend (bounded by ctx) and returns per-backend status.
func (r *Registry) HealthAll(ctx context.Context) []Health {
	out := make([]Health, 0, len(r.servers))
	for _, s := range r.servers {
		h := Health{ID: s.ID, Up: true}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := r.backends[s.ID].Health(probeCtx); err != nil {
			h.Up, h.Error = false, err.Error()
		}
		cancel()
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
