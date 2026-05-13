package anvilmcp

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v2"
)

const (
	DefaultDaemonURL      = "http://127.0.0.1:3000"
	DefaultTimeoutSeconds = 300
	DefaultConfigPath     = "configs/anvil-mcp.yaml"

	envDaemonURL      = "ANVIL_DAEMON_URL"
	envAPIToken       = "ANVIL_API_TOKEN"
	envDefaultTimeout = "ANVIL_MCP_DEFAULT_TIMEOUT"
	envConfigPath     = "ANVIL_MCP_CONFIG"
	envSessionStore   = "ANVIL_MCP_SESSION_STORE"
)

type Config struct {
	DaemonURL             string `yaml:"daemon_url"`
	APIToken              string `yaml:"api_token"`
	DefaultTimeoutSeconds int    `yaml:"default_timeout_seconds"`
	SessionStorePath      string `yaml:"session_store_path"`
}

type ConfigSource struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
}

func LoadConfig(src ConfigSource) (Config, error) {
	getenv := src.Getenv
	if getenv == nil {
		getenv = os.Getenv
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
	if cfg.DefaultTimeoutSeconds <= 0 {
		return Config{}, fmt.Errorf("default_timeout_seconds must be positive")
	}
	cfg.SessionStorePath = strings.TrimSpace(cfg.SessionStorePath)

	daemonURLLabel := "daemon_url"
	if getenv(envDaemonURL) != "" {
		daemonURLLabel = envDaemonURL
	}
	daemonURL, err := normalizeDaemonURL(cfg.DaemonURL, daemonURLLabel)
	if err != nil {
		return Config{}, err
	}
	cfg.DaemonURL = daemonURL

	return cfg, nil
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
