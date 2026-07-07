package anvilmcp

import (
	"os"
	"strings"
	"testing"
	"time"
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
	if cfg.SessionStorePath != "" {
		t.Errorf("SessionStorePath = %q, want empty", cfg.SessionStorePath)
	}
	if cfg.DefaultTenantID != "" {
		t.Errorf("DefaultTenantID = %q, want empty", cfg.DefaultTenantID)
	}
	if cfg.AuditLogPath != "" {
		t.Errorf("AuditLogPath = %q, want empty", cfg.AuditLogPath)
	}
}

func TestLoadConfigFile(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"daemon_url: https://anvil.example.com/",
			"api_token: file-token",
			"default_timeout_seconds: 45",
			"session_store_path: /var/lib/anvil-mcp/sessions.json",
			"default_tenant_id: tenant.file",
			"audit_log_path: /var/log/anvil-mcp/audit.jsonl",
			"scheduler_state_path: /var/lib/anvil-mcp/scheduler.json",
			"scheduler_hosts_file: /etc/anvil-mcp/hosts.json",
			"scheduler_quota_store_path: /var/lib/anvil-mcp/quotas.json",
			"cross_host_flock_create_mode: members_only",
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
	if cfg.SessionStorePath != "/var/lib/anvil-mcp/sessions.json" {
		t.Errorf("SessionStorePath = %q, want %q", cfg.SessionStorePath, "/var/lib/anvil-mcp/sessions.json")
	}
	if cfg.DefaultTenantID != "tenant.file" {
		t.Errorf("DefaultTenantID = %q, want %q", cfg.DefaultTenantID, "tenant.file")
	}
	if cfg.AuditLogPath != "/var/log/anvil-mcp/audit.jsonl" {
		t.Errorf("AuditLogPath = %q, want %q", cfg.AuditLogPath, "/var/log/anvil-mcp/audit.jsonl")
	}
	if cfg.SchedulerStatePath != "/var/lib/anvil-mcp/scheduler.json" {
		t.Errorf("SchedulerStatePath = %q, want %q", cfg.SchedulerStatePath, "/var/lib/anvil-mcp/scheduler.json")
	}
	if cfg.SchedulerHostsFile != "/etc/anvil-mcp/hosts.json" {
		t.Errorf("SchedulerHostsFile = %q, want %q", cfg.SchedulerHostsFile, "/etc/anvil-mcp/hosts.json")
	}
	if cfg.SchedulerQuotaStorePath != "/var/lib/anvil-mcp/quotas.json" {
		t.Errorf("SchedulerQuotaStorePath = %q, want %q", cfg.SchedulerQuotaStorePath, "/var/lib/anvil-mcp/quotas.json")
	}
	if cfg.CrossHostFlockCreateMode != "members_only" {
		t.Errorf("CrossHostFlockCreateMode = %q, want members_only", cfg.CrossHostFlockCreateMode)
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"daemon_url: https://file.example.com/",
			"api_token: file-token",
			"default_timeout_seconds: 45",
			"session_store_path: /var/lib/anvil-mcp/file-sessions.json",
			"default_tenant_id: tenant.file",
			"audit_log_path: /var/log/anvil-mcp/file-audit.jsonl",
			"scheduler_state_path: /var/lib/anvil-mcp/file-scheduler.json",
			"scheduler_hosts_file: /etc/anvil-mcp/file-hosts.json",
			"scheduler_quota_store_path: /var/lib/anvil-mcp/file-quotas.json",
			"cross_host_flock_create_mode: members_only",
			"",
		}, "\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG":                  "/tmp/anvil-mcp.yaml",
		"ANVIL_DAEMON_URL":                  "http://env.example.com/",
		"ANVIL_API_TOKEN":                   "env-token",
		"ANVIL_MCP_DEFAULT_TIMEOUT":         "90",
		"ANVIL_MCP_SESSION_STORE":           "/var/lib/anvil-mcp/env-sessions.json",
		"ANVIL_MCP_TENANT_ID":               "tenant.env",
		"ANVIL_MCP_AUDIT_LOG":               "/var/log/anvil-mcp/env-audit.jsonl",
		"ANVIL_MCP_SCHEDULER_STATE":         "/var/lib/anvil-mcp/env-scheduler.json",
		"ANVIL_MCP_SCHEDULER_HOSTS_FILE":    "/etc/anvil-mcp/env-hosts.json",
		"ANVIL_MCP_SCHEDULER_QUOTA_STORE":   "/var/lib/anvil-mcp/env-quotas.json",
		"ANVIL_MCP_CROSS_HOST_FLOCK_CREATE": "members_only",
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
	if cfg.SessionStorePath != "/var/lib/anvil-mcp/env-sessions.json" {
		t.Errorf("SessionStorePath = %q, want %q", cfg.SessionStorePath, "/var/lib/anvil-mcp/env-sessions.json")
	}
	if cfg.DefaultTenantID != "tenant.env" {
		t.Errorf("DefaultTenantID = %q, want %q", cfg.DefaultTenantID, "tenant.env")
	}
	if cfg.AuditLogPath != "/var/log/anvil-mcp/env-audit.jsonl" {
		t.Errorf("AuditLogPath = %q, want %q", cfg.AuditLogPath, "/var/log/anvil-mcp/env-audit.jsonl")
	}
	if cfg.SchedulerStatePath != "/var/lib/anvil-mcp/env-scheduler.json" {
		t.Errorf("SchedulerStatePath = %q, want %q", cfg.SchedulerStatePath, "/var/lib/anvil-mcp/env-scheduler.json")
	}
	if cfg.SchedulerHostsFile != "/etc/anvil-mcp/env-hosts.json" {
		t.Errorf("SchedulerHostsFile = %q, want %q", cfg.SchedulerHostsFile, "/etc/anvil-mcp/env-hosts.json")
	}
	if cfg.SchedulerQuotaStorePath != "/var/lib/anvil-mcp/env-quotas.json" {
		t.Errorf("SchedulerQuotaStorePath = %q, want %q", cfg.SchedulerQuotaStorePath, "/var/lib/anvil-mcp/env-quotas.json")
	}
	if cfg.CrossHostFlockCreateMode != "members_only" {
		t.Errorf("CrossHostFlockCreateMode = %q, want members_only", cfg.CrossHostFlockCreateMode)
	}
}

func TestLoadConfigEnvCrossHostFlockCreateOverridesFile(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"cross_host_flock_create_mode: ''",
			"",
		}, "\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG":                  "/tmp/anvil-mcp.yaml",
		"ANVIL_MCP_CROSS_HOST_FLOCK_CREATE": "members_only",
	}

	cfg, err := LoadConfig(testConfigSource(env, files))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.CrossHostFlockCreateMode != "members_only" {
		t.Fatalf("CrossHostFlockCreateMode = %q, want members_only", cfg.CrossHostFlockCreateMode)
	}
}

func TestLoadConfigEmptyEnvCrossHostFlockCreateOverridesFile(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"cross_host_flock_create_mode: members_only",
			"",
		}, "\n"),
	}
	env := map[string]string{
		"ANVIL_MCP_CONFIG":                  "/tmp/anvil-mcp.yaml",
		"ANVIL_MCP_CROSS_HOST_FLOCK_CREATE": "",
	}

	cfg, err := LoadConfig(testConfigSource(env, files))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.CrossHostFlockCreateMode != "" {
		t.Fatalf("CrossHostFlockCreateMode = %q, want empty env override", cfg.CrossHostFlockCreateMode)
	}
}

func TestLoadConfigRejectsInvalidCrossHostFlockCreateMode(t *testing.T) {
	files := map[string]string{
		"/tmp/anvil-mcp.yaml": strings.Join([]string{
			"cross_host_flock_create_mode: full",
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
	if !strings.Contains(err.Error(), "cross_host_flock_create_mode") {
		t.Fatalf("LoadConfig() error = %q, want cross_host_flock_create_mode", err)
	}
}

func TestLoadConfigRejectsInvalidCrossHostFlockCreateEnv(t *testing.T) {
	env := map[string]string{
		"ANVIL_MCP_CROSS_HOST_FLOCK_CREATE": "full",
	}

	_, err := LoadConfig(testConfigSource(env, nil))
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "ANVIL_MCP_CROSS_HOST_FLOCK_CREATE") {
		t.Fatalf("LoadConfig() error = %q, want ANVIL_MCP_CROSS_HOST_FLOCK_CREATE", err)
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

func TestLoadConfigRejectsInvalidTenantID(t *testing.T) {
	env := map[string]string{
		"ANVIL_MCP_TENANT_ID": "../tenant",
	}

	_, err := LoadConfig(testConfigSource(env, nil))
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "ANVIL_MCP_TENANT_ID") {
		t.Fatalf("LoadConfig() error = %q, want ANVIL_MCP_TENANT_ID", err)
	}
}

func TestLoadConfigReconcileIntervalDefault(t *testing.T) {
	cfg, err := LoadConfig(ConfigSource{
		Getenv:   func(string) string { return "" },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReconcileIntervalParsed != 60*time.Second {
		t.Fatalf("default ReconcileIntervalParsed = %v, want 60s", cfg.ReconcileIntervalParsed)
	}
}

func TestLoadConfigReconcileIntervalEnvOverridesYAML(t *testing.T) {
	cfg, err := LoadConfig(ConfigSource{
		Getenv: func(key string) string {
			if key == "ANVIL_MCP_RECONCILE_INTERVAL" {
				return "5m"
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) {
			return []byte("reconcile_interval: 30s\n"), nil
		},
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReconcileIntervalParsed != 5*time.Minute {
		t.Fatalf("ReconcileIntervalParsed = %v, want 5m (env가 yaml을 override)", cfg.ReconcileIntervalParsed)
	}
}

func TestLoadConfigReconcileIntervalZeroDisables(t *testing.T) {
	cfg, err := LoadConfig(ConfigSource{
		Getenv: func(key string) string {
			if key == "ANVIL_MCP_RECONCILE_INTERVAL" {
				return "0"
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ReconcileIntervalParsed != 0 {
		t.Fatalf("ReconcileIntervalParsed = %v, want 0 (off)", cfg.ReconcileIntervalParsed)
	}
}

func TestLoadConfigReconcileIntervalRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"-30s", "banana"} {
		_, err := LoadConfig(ConfigSource{
			Getenv: func(key string) string {
				if key == "ANVIL_MCP_RECONCILE_INTERVAL" {
					return bad
				}
				return ""
			},
			ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		})
		if err == nil {
			t.Fatalf("LoadConfig(%q): want error, got nil", bad)
		}
	}
}

func testConfigSource(env map[string]string, files map[string]string) ConfigSource {
	return ConfigSource{
		Getenv: func(key string) string {
			return env[key]
		},
		LookupEnv: func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
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
