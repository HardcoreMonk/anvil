package main

import (
	"errors"
	"os"
	"testing"
)

var daemonConfigEnvKeys = []string{
	"EPHEMERA_API_ADDR",
	"ANVIL_API_ADDR",
	"EPHEMERA_API_PORT",
	"ANVIL_API_PORT",
	"EPHEMERA_API_TOKENS",
	"ANVIL_API_TOKENS",
	"EPHEMERA_API_TOKEN",
	"ANVIL_API_TOKEN",
	"EPHEMERA_API_TOKENS_FILE",
	"EPHEMERA_AGENT_PORT",
	"ANVIL_AGENT_PORT",
	"EPHEMERA_PUBLIC_URL",
	"ANVIL_PUBLIC_URL",
	"EPHEMERA_HOME",
}

func clearDaemonConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range daemonConfigEnvKeys {
		t.Setenv(key, "")
	}
}

func TestLoadAPIClients_Empty(t *testing.T) {
	clearDaemonConfigEnv(t)

	if got := loadAPIClients(); len(got) != 0 {
		t.Errorf("expected 0 clients, got %d", len(got))
	}
}

func TestLoadAPIClients_SingleToken(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_TOKEN", "tok1")

	clients := loadAPIClients()
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].Name != "default" || clients[0].Token != "tok1" {
		t.Errorf("unexpected client: %+v", clients[0])
	}
}

func TestLoadAPIClients_MultiToken(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA,bob:tokenB")

	clients := loadAPIClients()
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
	if clients[0].Name != "alice" || clients[0].Token != "tokenA" {
		t.Errorf("unexpected client[0]: %+v", clients[0])
	}
	if clients[1].Name != "bob" || clients[1].Token != "tokenB" {
		t.Errorf("unexpected client[1]: %+v", clients[1])
	}
}

func TestLoadAPIClients_MultiTokenTakesPrecedence(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA")
	t.Setenv("EPHEMERA_API_TOKEN", "single")

	clients := loadAPIClients()
	if len(clients) != 1 || clients[0].Name != "alice" {
		t.Errorf("EPHEMERA_API_TOKENS should take precedence, got: %+v", clients)
	}
}

func TestLoadAPIClients_MalformedEntrySkipped(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA,malformed,bob:tokenB")

	clients := loadAPIClients()
	if len(clients) != 2 {
		t.Errorf("expected 2 valid clients (malformed entry skipped), got %d", len(clients))
	}
}

func TestResolveAPIAddr_Default(t *testing.T) {
	clearDaemonConfigEnv(t)

	if got := resolveAPIAddr(); got != "127.0.0.1:3000" {
		t.Errorf("expected 127.0.0.1:3000, got %q", got)
	}
}

func TestResolveWorkDirDefaultsToCurrentDirectory(t *testing.T) {
	clearDaemonConfigEnv(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	got, err := resolveWorkDir()
	if err != nil {
		t.Fatalf("resolveWorkDir: %v", err)
	}
	if got != cwd {
		t.Fatalf("resolveWorkDir = %q, want cwd %q", got, cwd)
	}
}

func TestResolveWorkDirUsesEphemeraHome(t *testing.T) {
	clearDaemonConfigEnv(t)
	home := t.TempDir()
	t.Setenv("EPHEMERA_HOME", "  "+home+"  ")

	got, err := resolveWorkDir()
	if err != nil {
		t.Fatalf("resolveWorkDir: %v", err)
	}
	if got != home {
		t.Fatalf("resolveWorkDir = %q, want EPHEMERA_HOME %q", got, home)
	}
}

func TestResolveAPIAddr_FromAddr(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_ADDR", "0.0.0.0:8080")

	if got := resolveAPIAddr(); got != "0.0.0.0:8080" {
		t.Errorf("expected 0.0.0.0:8080, got %q", got)
	}
}

func TestResolveAPIAddr_FromPort(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_PORT", "9090")

	if got := resolveAPIAddr(); got != "127.0.0.1:9090" {
		t.Errorf("expected 127.0.0.1:9090, got %q", got)
	}
}

func TestLoadAPIClients_FromAnvilMultiTokenAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("ANVIL_API_TOKENS", "ironclaw:tokenA,operator:tokenB")

	clients := loadAPIClients()
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
	if clients[0].Name != "ironclaw" || clients[0].Token != "tokenA" {
		t.Errorf("unexpected client[0]: %+v", clients[0])
	}
	if clients[1].Name != "operator" || clients[1].Token != "tokenB" {
		t.Errorf("unexpected client[1]: %+v", clients[1])
	}
}

func TestLoadAPIClients_AnvilTokenAliasSupportsExpiry(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("ANVIL_API_TOKENS", "ironclaw:tok:4102444800")

	clients := loadAPIClients()
	if len(clients) != 1 {
		t.Fatalf("clients = %d, want 1", len(clients))
	}
	if clients[0].Name != "ironclaw" || clients[0].Token != "tok" {
		t.Fatalf("client = %+v, want ironclaw/tok", clients[0])
	}
	if clients[0].Expires.IsZero() {
		t.Fatal("Expires is zero, want parsed expiry")
	}
}

func TestLoadAPIClients_EphemeraMultiTokenPrecedesAnvilMultiToken(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_TOKENS", "canonical:tokenA")
	t.Setenv("ANVIL_API_TOKENS", "alias:tokenB")

	clients := loadAPIClients()
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].Name != "canonical" || clients[0].Token != "tokenA" {
		t.Fatalf("expected canonical token to win, got %+v", clients[0])
	}
}

func TestLoadAPIClients_FromAnvilSingleTokenAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("ANVIL_API_TOKEN", "alias-single")

	clients := loadAPIClients()
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].Name != "default" || clients[0].Token != "alias-single" {
		t.Fatalf("unexpected client: %+v", clients[0])
	}
}

func TestLoadAPIClients_AnvilMultiTokenPrecedesEphemeraSingleToken(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("ANVIL_API_TOKENS", "alias:tokenA")
	t.Setenv("EPHEMERA_API_TOKEN", "canonical-single")

	clients := loadAPIClients()
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].Name != "alias" || clients[0].Token != "tokenA" {
		t.Fatalf("expected alias multi-token to win over canonical single token, got %+v", clients[0])
	}
}

func TestResolveAPIAddr_FromAnvilAddrAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("ANVIL_API_ADDR", "0.0.0.0:4000")

	if got := resolveAPIAddr(); got != "0.0.0.0:4000" {
		t.Fatalf("expected ANVIL_API_ADDR value, got %q", got)
	}
}

func TestResolveAPIAddr_EphemeraAddrPrecedesAnvilAddr(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_ADDR", "127.0.0.1:3001")
	t.Setenv("ANVIL_API_ADDR", "0.0.0.0:4000")

	if got := resolveAPIAddr(); got != "127.0.0.1:3001" {
		t.Fatalf("expected EPHEMERA_API_ADDR value, got %q", got)
	}
}

func TestResolveAPIAddr_FromAnvilPortAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("ANVIL_API_PORT", "4000")

	if got := resolveAPIAddr(); got != "127.0.0.1:4000" {
		t.Fatalf("expected ANVIL_API_PORT value, got %q", got)
	}
}

func TestResolveAPIAddr_EphemeraPortPrecedesAnvilPort(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_PORT", "3001")
	t.Setenv("ANVIL_API_PORT", "4000")

	if got := resolveAPIAddr(); got != "127.0.0.1:3001" {
		t.Fatalf("expected EPHEMERA_API_PORT value, got %q", got)
	}
}

func TestResolveAPIAddr_InvalidCanonicalPortDoesNotFallBackToAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_API_PORT", "not-a-port")
	t.Setenv("ANVIL_API_PORT", "4000")

	if got := resolveAPIAddr(); got != "127.0.0.1:3000" {
		t.Fatalf("expected default port when canonical port is invalid, got %q", got)
	}
}

func TestBridgeCallbackPortAllowsWildcardBinds(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:3000", ":3000", "[::]:3000"} {
		port, ok := bridgeCallbackPort(addr)
		if !ok || port != 3000 {
			t.Fatalf("bridgeCallbackPort(%q) = %d, %v; want 3000, true", addr, port, ok)
		}
	}
}

func TestBridgeCallbackPortRejectsLoopbackBinds(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:3000", "localhost:3000", "[::1]:3000"} {
		port, ok := bridgeCallbackPort(addr)
		if ok || port != 0 {
			t.Fatalf("bridgeCallbackPort(%q) = %d, %v; want 0, false", addr, port, ok)
		}
	}
}

func TestBridgeCallbackPortRejectsUnparseableAddr(t *testing.T) {
	port, ok := bridgeCallbackPort("not-a-listen-addr")
	if ok || port != 0 {
		t.Fatalf("bridgeCallbackPort invalid addr = %d, %v; want 0, false", port, ok)
	}
}

func TestResolvePublicURL_FromAnvilAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("ANVIL_PUBLIC_URL", "http://192.168.3.73:3000/")

	if got := resolvePublicURL(); got != "http://192.168.3.73:3000" {
		t.Fatalf("expected trimmed ANVIL_PUBLIC_URL value, got %q", got)
	}
}

func TestResolvePublicURL_EphemeraPrecedesAnvilAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_PUBLIC_URL", "https://canonical.example/")
	t.Setenv("ANVIL_PUBLIC_URL", "http://192.168.3.73:3000/")

	if got := resolvePublicURL(); got != "https://canonical.example" {
		t.Fatalf("expected trimmed EPHEMERA_PUBLIC_URL value, got %q", got)
	}
}

func TestResolveAgentPort_FromAnvilAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("ANVIL_AGENT_PORT", "9091")

	if got := resolveAgentPort(); got != 9091 {
		t.Fatalf("expected ANVIL_AGENT_PORT value, got %d", got)
	}
}

func TestResolveAgentPort_EphemeraPrecedesAnvilAlias(t *testing.T) {
	clearDaemonConfigEnv(t)
	t.Setenv("EPHEMERA_AGENT_PORT", "8081")
	t.Setenv("ANVIL_AGENT_PORT", "9091")

	if got := resolveAgentPort(); got != 8081 {
		t.Fatalf("expected EPHEMERA_AGENT_PORT value, got %d", got)
	}
}

// TestEnvBool_Autosnapshot documents the EPHEMERA_AUTOSNAPSHOT (v0.4.0) mapping.
// It exercises envBool directly rather than the enableAutoSnapshot package var,
// which is captured once at init and would not reflect env changes made here.
func TestEnvBool_Autosnapshot(t *testing.T) {
	const key = "EPHEMERA_AUTOSNAPSHOT"
	cases := []struct {
		name string
		val  string
		set  bool
		want bool
	}{
		{name: "unset defaults off", set: false, want: false},
		{name: "empty defaults off", val: "", set: true, want: false},
		{name: "true", val: "true", set: true, want: true},
		{name: "1", val: "1", set: true, want: true},
		{name: "yes", val: "yes", set: true, want: true},
		{name: "on", val: "on", set: true, want: true},
		{name: "false", val: "false", set: true, want: false},
		{name: "off", val: "off", set: true, want: false},
		{name: "garbage defaults off", val: "garbage", set: true, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				os.Setenv(key, c.val)
				defer os.Unsetenv(key)
			} else {
				os.Unsetenv(key)
			}
			if got := envBool(key, false); got != c.want {
				t.Errorf("envBool(%q=%q) = %v, want %v", key, c.val, got, c.want)
			}
		})
	}
}

// TestResolveDiskModeCOW covers the COW-by-default decision (v0.4.2): unset/"cow"
// resolves to COW when the host probe passes, falls back to plain when it fails,
// and "plain"/"full" force plain regardless of the probe.
func TestResolveDiskModeCOW(t *testing.T) {
	const key = "EPHEMERA_DISK_MODE"
	okProbe := func() error { return nil }
	errProbe := func() error { return errors.New("no dm-snapshot") }
	cases := []struct {
		name  string
		val   string
		set   bool
		probe func() error
		want  bool
	}{
		{name: "unset + probe ok -> cow", set: false, probe: okProbe, want: true},
		{name: "cow + probe ok -> cow", val: "cow", set: true, probe: okProbe, want: true},
		{name: "unset + probe fails -> fallback plain", set: false, probe: errProbe, want: false},
		{name: "cow + probe fails -> fallback plain", val: "cow", set: true, probe: errProbe, want: false},
		{name: "plain forces plain", val: "plain", set: true, probe: okProbe, want: false},
		{name: "full forces plain", val: "full", set: true, probe: okProbe, want: false},
		{name: "PLAIN case-insensitive", val: "PLAIN", set: true, probe: okProbe, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				os.Setenv(key, c.val)
				defer os.Unsetenv(key)
			} else {
				os.Unsetenv(key)
			}
			if got := resolveDiskModeCOW(c.probe); got != c.want {
				t.Errorf("resolveDiskModeCOW(%q=%q) = %v, want %v", key, c.val, got, c.want)
			}
		})
	}
}

// TestResolveDiskModeCOW_OptOutSkipsProbe verifies the plain/full opt-out
// short-circuits before the (potentially expensive) host probe runs.
func TestResolveDiskModeCOW_OptOutSkipsProbe(t *testing.T) {
	const key = "EPHEMERA_DISK_MODE"
	os.Setenv(key, "plain")
	defer os.Unsetenv(key)

	probed := false
	got := resolveDiskModeCOW(func() error { probed = true; return nil })
	if got {
		t.Errorf("plain should resolve to false, got %v", got)
	}
	if probed {
		t.Error("probe must not run when EPHEMERA_DISK_MODE=plain")
	}
}
