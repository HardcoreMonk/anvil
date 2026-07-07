package anvilmcp

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

const (
	DefaultDaemonURL      = "http://127.0.0.1:3000"
	DefaultTimeoutSeconds = 300
	DefaultConfigPath     = "configs/anvil-mcp.yaml"

	envDaemonURL         = "ANVIL_DAEMON_URL"
	envAPIToken          = "ANVIL_API_TOKEN"
	envDefaultTimeout    = "ANVIL_MCP_DEFAULT_TIMEOUT"
	envConfigPath        = "ANVIL_MCP_CONFIG"
	envSessionStore      = "ANVIL_MCP_SESSION_STORE"
	envTenantID          = "ANVIL_MCP_TENANT_ID"
	envAuditLog          = "ANVIL_MCP_AUDIT_LOG"
	envSchedulerState    = "ANVIL_MCP_SCHEDULER_STATE"
	envSchedulerHosts    = "ANVIL_MCP_SCHEDULER_HOSTS_FILE"
	envSchedulerQuota    = "ANVIL_MCP_SCHEDULER_QUOTA_STORE"
	envCrossHostFlock    = "ANVIL_MCP_CROSS_HOST_FLOCK_CREATE"
	envReconcileInterval = "ANVIL_MCP_RECONCILE_INTERVAL"
)

type Config struct {
	DaemonURL                string        `yaml:"daemon_url"`
	APIToken                 string        `yaml:"api_token"`
	DefaultTimeoutSeconds    int           `yaml:"default_timeout_seconds"`
	SessionStorePath         string        `yaml:"session_store_path"`
	DefaultTenantID          string        `yaml:"default_tenant_id"`
	AuditLogPath             string        `yaml:"audit_log_path"`
	SchedulerStatePath       string        `yaml:"scheduler_state_path"`
	SchedulerHostsFile       string        `yaml:"scheduler_hosts_file"`
	SchedulerQuotaStorePath  string        `yaml:"scheduler_quota_store_path"`
	CrossHostFlockCreateMode string        `yaml:"cross_host_flock_create_mode"`
	// ReconcileInterval is the raw reconcile-loop interval (time.ParseDuration
	// format). ReconcileIntervalParsed is the validated value LoadConfig fills:
	// 60s default when unset, 0 disables the loop entirely.
	ReconcileInterval       string        `yaml:"reconcile_interval"`
	ReconcileIntervalParsed time.Duration `yaml:"-"`
}

type ConfigSource struct {
	Getenv    func(string) string
	LookupEnv func(string) (string, bool)
	ReadFile  func(string) ([]byte, error)
}

func LoadConfig(src ConfigSource) (Config, error) {
	getenv := src.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	lookupEnv := src.LookupEnv
	if lookupEnv == nil {
		if src.Getenv == nil {
			lookupEnv = os.LookupEnv
		} else {
			lookupEnv = func(key string) (string, bool) {
				value := getenv(key)
				return value, value != ""
			}
		}
	}
	readFile := src.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}

	cfg := Config{
		DaemonURL:             DefaultDaemonURL,
		DefaultTimeoutSeconds: DefaultTimeoutSeconds,
	}

	configPath := strings.TrimSpace(getenv(envConfigPath))
	configPathExplicit := configPath != ""
	if configPath == "" {
		configPath = DefaultConfigPath
	}

	if data, err := readFile(configPath); err != nil {
		if configPathExplicit {
			return Config{}, fmt.Errorf("%s: read %q: %w", envConfigPath, configPath, err)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("read config %q: %w", configPath, err)
		}
	} else if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", configPath, err)
	}

	if v := getenv(envDaemonURL); v != "" {
		cfg.DaemonURL = v
	}
	if v := getenv(envAPIToken); v != "" {
		cfg.APIToken = v
	}
	if v := getenv(envDefaultTimeout); v != "" {
		timeout, err := strconv.Atoi(v)
		if err != nil || timeout <= 0 {
			return Config{}, fmt.Errorf("%s must be a positive integer", envDefaultTimeout)
		}
		cfg.DefaultTimeoutSeconds = timeout
	}
	if v := getenv(envSessionStore); v != "" {
		cfg.SessionStorePath = v
	}
	if v := getenv(envTenantID); v != "" {
		cfg.DefaultTenantID = v
	}
	if v := getenv(envAuditLog); v != "" {
		cfg.AuditLogPath = v
	}
	if v := getenv(envSchedulerState); v != "" {
		cfg.SchedulerStatePath = v
	}
	if v := getenv(envSchedulerHosts); v != "" {
		cfg.SchedulerHostsFile = v
	}
	if v := getenv(envSchedulerQuota); v != "" {
		cfg.SchedulerQuotaStorePath = v
	}
	crossHostFlockEnvValue, crossHostFlockEnvSet := lookupEnv(envCrossHostFlock)
	if crossHostFlockEnvSet {
		v := crossHostFlockEnvValue
		cfg.CrossHostFlockCreateMode = v
	}
	if v := strings.TrimSpace(getenv(envReconcileInterval)); v != "" {
		cfg.ReconcileInterval = v
	}
	if cfg.DefaultTimeoutSeconds <= 0 {
		return Config{}, fmt.Errorf("default_timeout_seconds must be positive")
	}
	cfg.SessionStorePath = strings.TrimSpace(cfg.SessionStorePath)
	cfg.DefaultTenantID = strings.TrimSpace(cfg.DefaultTenantID)
	cfg.AuditLogPath = strings.TrimSpace(cfg.AuditLogPath)
	cfg.SchedulerStatePath = strings.TrimSpace(cfg.SchedulerStatePath)
	cfg.SchedulerHostsFile = strings.TrimSpace(cfg.SchedulerHostsFile)
	cfg.SchedulerQuotaStorePath = strings.TrimSpace(cfg.SchedulerQuotaStorePath)
	cfg.CrossHostFlockCreateMode = strings.TrimSpace(cfg.CrossHostFlockCreateMode)
	crossHostFlockLabel := "cross_host_flock_create_mode"
	if crossHostFlockEnvSet {
		crossHostFlockLabel = envCrossHostFlock
	}
	if err := validateCrossHostFlockCreateMode(cfg.CrossHostFlockCreateMode, crossHostFlockLabel); err != nil {
		return Config{}, err
	}
	if cfg.DefaultTenantID != "" {
		label := "default_tenant_id"
		if getenv(envTenantID) != "" {
			label = envTenantID
		}
		tenantID, err := NormalizeTenantID(cfg.DefaultTenantID)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", label, err)
		}
		cfg.DefaultTenantID = tenantID
	}

	daemonURLLabel := "daemon_url"
	if getenv(envDaemonURL) != "" {
		daemonURLLabel = envDaemonURL
	}
	daemonURL, err := normalizeDaemonURL(cfg.DaemonURL, daemonURLLabel)
	if err != nil {
		return Config{}, err
	}
	cfg.DaemonURL = daemonURL

	cfg.ReconcileInterval = strings.TrimSpace(cfg.ReconcileInterval)
	if cfg.ReconcileInterval == "" {
		cfg.ReconcileIntervalParsed = 60 * time.Second
	} else {
		d, err := time.ParseDuration(cfg.ReconcileInterval)
		if err != nil {
			return Config{}, fmt.Errorf("reconcile_interval must be a duration like 60s (got %q): %w", cfg.ReconcileInterval, err)
		}
		if d < 0 {
			return Config{}, fmt.Errorf("reconcile_interval must not be negative (got %q)", cfg.ReconcileInterval)
		}
		cfg.ReconcileIntervalParsed = d
	}

	return cfg, nil
}

func validateCrossHostFlockCreateMode(mode string, label string) error {
	switch mode {
	case "", "members_only":
		return nil
	default:
		return fmt.Errorf("%s must be empty or members_only", label)
	}
}

func normalizeDaemonURL(raw string, label string) (string, error) {
	normalized := strings.TrimRight(strings.TrimSpace(raw), "/")
	if normalized == "" {
		return "", fmt.Errorf("%s must be non-empty", label)
	}

	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", fmt.Errorf("%s must be a valid URL: %w", label, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s must use http or https", label)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%s must include a host", label)
	}
	return normalized, nil
}
