package main

import "testing"

var daemonConfigEnvKeys = []string{
	"EPHEMERA_API_ADDR",
	"ANVIL_API_ADDR",
	"EPHEMERA_API_PORT",
	"ANVIL_API_PORT",
	"EPHEMERA_API_TOKENS",
	"ANVIL_API_TOKENS",
	"EPHEMERA_API_TOKEN",
	"ANVIL_API_TOKEN",
	"EPHEMERA_AGENT_PORT",
	"ANVIL_AGENT_PORT",
	"EPHEMERA_PUBLIC_URL",
	"ANVIL_PUBLIC_URL",
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
