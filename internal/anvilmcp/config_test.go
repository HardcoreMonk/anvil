package anvilmcp

import (
	"os"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(testConfigSource(nil, nil))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.DaemonURL != "http://127.0.0.1:3000" {
		t.Errorf("DaemonURL = %q, want %q", cfg.DaemonURL, "http://127.0.0.1:3000")
	}
	if cfg.APIToken != "" {
		t.Errorf("APIToken = %q, want empty", cfg.APIToken)
	}
	if cfg.DefaultTimeoutSeconds != 300 {
		t.Errorf("DefaultTimeoutSeconds = %d, want 300", cfg.DefaultTimeoutSeconds)
	}
}

func TestLoadConfigFile(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"daemon_url: https://anvil.example.com/",
			"api_token: file-token",
			"default_timeout_seconds: 45",
			"",
		}, "\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG": "/tmp/anvil-mcp.yaml",
	}

	cfg, err := LoadConfig(testConfigSource(env, files))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.DaemonURL != "https://anvil.example.com" {
		t.Errorf("DaemonURL = %q, want %q", cfg.DaemonURL, "https://anvil.example.com")
	}
	if cfg.APIToken != "file-token" {
		t.Errorf("APIToken = %q, want %q", cfg.APIToken, "file-token")
	}
	if cfg.DefaultTimeoutSeconds != 45 {
		t.Errorf("DefaultTimeoutSeconds = %d, want 45", cfg.DefaultTimeoutSeconds)
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"daemon_url: https://file.example.com/",
			"api_token: file-token",
			"default_timeout_seconds: 45",
			"",
		}, "\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG":          "/tmp/anvil-mcp.yaml",
		"ANVIL_DAEMON_URL":          "http://env.example.com/",
		"ANVIL_API_TOKEN":           "env-token",
		"ANVIL_MCP_DEFAULT_TIMEOUT": "90",
	}

	cfg, err := LoadConfig(testConfigSource(env, files))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.DaemonURL != "http://env.example.com" {
		t.Errorf("DaemonURL = %q, want %q", cfg.DaemonURL, "http://env.example.com")
	}
	if cfg.APIToken != "env-token" {
		t.Errorf("APIToken = %q, want %q", cfg.APIToken, "env-token")
	}
	if cfg.DefaultTimeoutSeconds != 90 {
		t.Errorf("DefaultTimeoutSeconds = %d, want 90", cfg.DefaultTimeoutSeconds)
	}
}

func TestLoadConfigEnvTimeoutOverridesInvalidFileTimeout(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"daemon_url: https://file.example.com/",
			"default_timeout_seconds: 0",
			"",
		}, "\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG":          "/tmp/anvil-mcp.yaml",
		"ANVIL_MCP_DEFAULT_TIMEOUT": "90",
	}

	cfg, err := LoadConfig(testConfigSource(env, files))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.DefaultTimeoutSeconds != 90 {
		t.Errorf("DefaultTimeoutSeconds = %d, want 90", cfg.DefaultTimeoutSeconds)
	}
}

func TestLoadConfigTrimsConfigPath(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"daemon_url: https://anvil.example.com/",
			"api_token: file-token",
			"default_timeout_seconds: 45",
			"",
		}, "\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG": "  /tmp/anvil-mcp.yaml \n",
	}

	cfg, err := LoadConfig(testConfigSource(env, files))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.APIToken != "file-token" {
		t.Errorf("APIToken = %q, want %q", cfg.APIToken, "file-token")
	}
}

func TestLoadConfigRejectsInvalidURL(t *testing.T) {
	env := map[string]string{
		"ANVIL_DAEMON_URL": "ftp://example.com",
	}

	_, err := LoadConfig(testConfigSource(env, nil))
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "ANVIL_DAEMON_URL") {
		t.Fatalf("LoadConfig() error = %q, want ANVIL_DAEMON_URL", err)
	}
}

func TestLoadConfigRejectsInvalidTimeout(t *testing.T) {
	env := map[string]string{
		"ANVIL_MCP_DEFAULT_TIMEOUT": "0",
	}

	_, err := LoadConfig(testConfigSource(env, nil))
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "ANVIL_MCP_DEFAULT_TIMEOUT") {
		t.Fatalf("LoadConfig() error = %q, want ANVIL_MCP_DEFAULT_TIMEOUT", err)
	}
}

func TestLoadConfigRejectsInvalidFileTimeout(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"default_timeout_seconds: 0",
			"",
		}, "\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG": "/tmp/anvil-mcp.yaml",
	}

	_, err := LoadConfig(testConfigSource(env, files))
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "default_timeout_seconds") {
		t.Fatalf("LoadConfig() error = %q, want default_timeout_seconds", err)
	}
}

func testConfigSource(env map[string]string, files map[string]string) ConfigSource {
	return ConfigSource{
		Getenv: func(key string) string {
			return env[key]
		},
		ReadFile: func(path string) ([]byte, error) {
			content, ok := files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(content), nil
		},
	}
}
