package mcpgateway

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v2"
)

// ServerConfig describes one backend MCP server, loaded from configs/mcp/servers.yaml.
// Credentials are referenced by key (resolved from configs/mcp/secrets.yaml) so the
// secret value never appears in this struct or any API/UI response.
type ServerConfig struct {
	ID         string   `yaml:"id"`          // stable identifier and default namespace
	Namespace  string   `yaml:"namespace"`   // tool-name prefix; defaults to ID
	Transport  string   `yaml:"transport"`   // "http" (Streamable HTTP); reserved for future
	URL        string   `yaml:"url"`         // backend MCP endpoint
	Credential string   `yaml:"credential"`  // key into secrets.yaml; optional (public server)
	Profiles   []string `yaml:"profiles"`    // Ephemera profiles allowed to use it; empty = all
	ToolsAllow []string `yaml:"tools_allow"` // if non-empty, only these tool names are exposed
	ToolsDeny  []string `yaml:"tools_deny"`  // tool names to hide (ignored when tools_allow is set)
}

// serversFile is the on-disk shape of servers.yaml.
type serversFile struct {
	Servers []ServerConfig `yaml:"servers"`
}

// ServerInfo is the secret-free view of a configured backend for the API/UI.
type ServerInfo struct {
	ID            string   `json:"id"`
	Namespace     string   `json:"namespace"`
	Transport     string   `json:"transport"`
	URL           string   `json:"url"`
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

// NewRegistry validates the server configs, resolves their credentials, and
// builds an HTTPBackend per server. The returned CredentialProvider is wired into
// each backend; secrets never leave this constructor.
func NewRegistry(servers []ServerConfig, secrets map[string]string, client *http.Client) (*Registry, error) {
	tokens := map[string]string{} // server id → bearer token
	seenID := map[string]bool{}   // duplicate-id guard
	seenNS := map[string]bool{}   // duplicate-namespace guard
	for i := range servers {
		s := &servers[i]
		if err := validateServer(*s); err != nil {
			return nil, err
		}
		if s.Namespace == "" {
			s.Namespace = s.ID
		}
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
			tokens[s.ID] = tok
		}
	}
	creds := NewMapCredentialProvider(tokens)
	reg := &Registry{
		servers:  servers,
		backends: map[string]Backend{},
		byNS:     map[string]Backend{},
	}
	for _, s := range servers {
		b := NewHTTPBackend(s.ID, s.Namespace, s.URL, client, creds)
		reg.backends[s.ID] = b
		reg.byNS[s.Namespace] = b
	}
	return reg, nil
}

// validateServer enforces the minimal invariants the gateway relies on: a
// non-empty id, an http(s) URL, and a namespace free of the "__" separator the
// gateway uses to split namespaced tool names.
func validateServer(s ServerConfig) error {
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("server has empty id")
	}
	if s.Transport != "" && s.Transport != "http" {
		return fmt.Errorf("server %q: unsupported transport %q (only \"http\")", s.ID, s.Transport)
	}
	if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
		return fmt.Errorf("server %q: url must be http(s)", s.ID)
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

// Servers returns the secret-free view of every configured backend, sorted by id.
func (r *Registry) Servers() []ServerInfo {
	out := make([]ServerInfo, 0, len(r.servers))
	for _, s := range r.servers {
		out = append(out, ServerInfo{
			ID:            s.ID,
			Namespace:     s.Namespace,
			Transport:     s.Transport,
			URL:           s.URL,
			Profiles:      s.Profiles,
			HasCredential: s.Credential != "",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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
