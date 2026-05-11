# anvil Daemon Environment Aliases Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** ephemera daemon 설정에 `ANVIL_*` 환경 변수 alias를 추가해 anvil 운영자가 anvil 명명 체계로 daemon을 설정할 수 있게 한다.

**Architecture:** `EPHEMERA_*`는 canonical runtime 계약으로 유지하고 `ANVIL_*`는 fallback alias로만 동작한다. 환경 변수 lookup은 `cmd/goose-daemon/config.go`의 작은 helper에 모아 precedence를 일관되게 적용한다. 문서는 anvil/ephemera 경계를 유지하면서 alias와 우선순위를 명시한다.

**Tech Stack:** Go standard library, `cmd/goose-daemon` config tests, Markdown documentation.

---

## File Structure

- Modify: `cmd/goose-daemon/config_test.go`
  - Existing daemon config tests를 alias-aware 환경에서 안전하게 만들고, `ANVIL_*` fallback/precedence unit test를 추가한다.
- Modify: `cmd/goose-daemon/config.go`
  - `envWithAlias`, `envIntWithAlias`, `resolveAgentPort`, `resolvePublicURL` helper를 추가하고 daemon config lookup에 적용한다.
- Modify: `README.md`
  - 설정 표에 canonical/alias 관계와 precedence를 추가한다.
- Modify: `docs/architecture/service-logic.md`
  - 제어 평면 인증과 runtime 설정 precedence를 설명한다.
- Modify: `CONTEXT.md`
  - 고정 runtime 계약에 `ANVIL_*` alias 관계를 추가하고 후속 후보에서 완료 항목을 제거한다.

---

### Task 1: Add Failing Alias Config Tests

**Files:**
- Modify: `cmd/goose-daemon/config_test.go`

- [ ] **Step 1: Replace existing env setup with a complete env cleanup helper**

At the top of `cmd/goose-daemon/config_test.go`, after imports, add:

```go
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
```

Replace the existing tests in `cmd/goose-daemon/config_test.go` with this block so every test starts from a clean canonical and alias environment:

```go
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
```

- [ ] **Step 2: Add alias and precedence tests**

Append these tests to `cmd/goose-daemon/config_test.go`:

```go
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
```

- [ ] **Step 3: Run tests to verify they fail**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestLoadAPIClients|TestResolveAPIAddr|TestResolvePublicURL|TestResolveAgentPort' -count=1
```

Expected: FAIL with undefined functions `resolvePublicURL` and `resolveAgentPort`, plus alias tests failing because `ANVIL_*` variables are not read yet.

- [ ] **Step 4: Commit is not created in this task**

Do not commit after the failing tests. Task 2 will implement the code and commit the red/green pair together.

---

### Task 2: Implement Alias-Aware Daemon Config

**Files:**
- Modify: `cmd/goose-daemon/config.go`
- Modify: `cmd/goose-daemon/config_test.go`

- [ ] **Step 1: Replace `cmd/goose-daemon/config.go` with alias-aware config code**

Replace the full contents of `cmd/goose-daemon/config.go` with:

```go
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultAPIPort   = 3000
	defaultAgentPort = 8080

	envEphemeraAgentPort = "EPHEMERA_AGENT_PORT"
	envAnvilAgentPort    = "ANVIL_AGENT_PORT"

	envEphemeraAPIAddr = "EPHEMERA_API_ADDR"
	envAnvilAPIAddr    = "ANVIL_API_ADDR"
	envEphemeraAPIPort = "EPHEMERA_API_PORT"
	envAnvilAPIPort    = "ANVIL_API_PORT"

	envEphemeraPublicURL = "EPHEMERA_PUBLIC_URL"
	envAnvilPublicURL    = "ANVIL_PUBLIC_URL"

	envEphemeraAPITokens = "EPHEMERA_API_TOKENS"
	envAnvilAPITokens    = "ANVIL_API_TOKENS"
	envEphemeraAPIToken  = "EPHEMERA_API_TOKEN"
	envAnvilAPIToken     = "ANVIL_API_TOKEN"
)

// APIClient represents a named caller with its own Bearer token.
// Using separate tokens per client allows individual revocation and audit logging.
type APIClient struct {
	Name  string
	Token string
}

var (
	// agentPort is the port goose-agent listens on inside each VM.
	// Must match GOOSE_AGENT_PORT if overridden on the VM side.
	agentPort = resolveAgentPort()

	// apiAddr is the address the control plane API binds to.
	// Default 127.0.0.1:3000 makes the API reachable only on localhost,
	// requiring a reverse proxy for external access.
	// Set EPHEMERA_API_ADDR=0.0.0.0:3000 or ANVIL_API_ADDR=0.0.0.0:3000
	// to bind on all interfaces.
	apiAddr = resolveAPIAddr()

	// publicURL is the externally-reachable base URL of the control plane
	// (no trailing slash). When set, agent_url in VM responses points to the
	// control plane proxy path ("{publicURL}/vms/{vm_id}") instead of the
	// VM's private IP. Example: "https://api.example.com"
	// Set via EPHEMERA_PUBLIC_URL or ANVIL_PUBLIC_URL env var.
	publicURL = resolvePublicURL()

	// apiClients is the set of authorized callers loaded once at startup.
	// Populated from EPHEMERA_API_TOKENS/ANVIL_API_TOKENS (multi-client)
	// or EPHEMERA_API_TOKEN/ANVIL_API_TOKEN (single-client fallback).
	// Empty = authentication disabled.
	apiClients = loadAPIClients()
)

// loadAPIClients parses caller tokens from environment variables.
//
// Multi-client (preferred):
//
//	EPHEMERA_API_TOKENS=alice:token1,bob:token2
//	ANVIL_API_TOKENS=alice:token1,bob:token2
//
// Single-client fallback:
//
//	EPHEMERA_API_TOKEN=token
//	ANVIL_API_TOKEN=token
//
// Precedence:
//
//	EPHEMERA_API_TOKENS -> ANVIL_API_TOKENS -> EPHEMERA_API_TOKEN -> ANVIL_API_TOKEN
func loadAPIClients() []APIClient {
	if raw := envWithAlias(envEphemeraAPITokens, envAnvilAPITokens); raw != "" {
		var clients []APIClient
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			idx := strings.Index(entry, ":")
			if idx <= 0 {
				continue
			}
			clients = append(clients, APIClient{
				Name:  entry[:idx],
				Token: entry[idx+1:],
			})
		}
		return clients
	}
	if t := envWithAlias(envEphemeraAPIToken, envAnvilAPIToken); t != "" {
		return []APIClient{{Name: "default", Token: t}}
	}
	return nil
}

func resolveAgentPort() int {
	return envIntWithAlias(envEphemeraAgentPort, envAnvilAgentPort, defaultAgentPort)
}

func resolvePublicURL() string {
	return strings.TrimRight(envWithAlias(envEphemeraPublicURL, envAnvilPublicURL), "/")
}

// resolveAPIAddr builds the listen address.
// EPHEMERA_API_ADDR/ANVIL_API_ADDR (full host:port) takes precedence over
// EPHEMERA_API_PORT/ANVIL_API_PORT (port only). EPHEMERA_* canonical values
// take precedence over ANVIL_* aliases.
func resolveAPIAddr() string {
	if v := envWithAlias(envEphemeraAPIAddr, envAnvilAPIAddr); v != "" {
		return v
	}
	return fmt.Sprintf("127.0.0.1:%d", envIntWithAlias(envEphemeraAPIPort, envAnvilAPIPort, defaultAPIPort))
}

func envWithAlias(canonicalKey, aliasKey string) string {
	if v := os.Getenv(canonicalKey); v != "" {
		return v
	}
	if aliasKey == "" {
		return ""
	}
	return os.Getenv(aliasKey)
}

func envInt(key string, defaultVal int) int {
	return envIntWithAlias(key, "", defaultVal)
}

func envIntWithAlias(canonicalKey, aliasKey string, defaultVal int) int {
	if v := envWithAlias(canonicalKey, aliasKey); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}
```

- [ ] **Step 2: Run focused config tests**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestLoadAPIClients|TestResolveAPIAddr|TestResolvePublicURL|TestResolveAgentPort' -count=1
```

Expected: PASS.

- [ ] **Step 3: Run package tests**

Run:

```bash
go test ./cmd/goose-daemon -count=1
```

Expected: PASS.

- [ ] **Step 4: Commit config implementation**

Run:

```bash
git add cmd/goose-daemon/config.go cmd/goose-daemon/config_test.go
git commit -m "feat: support anvil daemon env aliases"
```

Expected: commit succeeds and includes only `cmd/goose-daemon/config.go` and `cmd/goose-daemon/config_test.go`.

---

### Task 3: Document ANVIL Alias Contract

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture/service-logic.md`
- Modify: `CONTEXT.md`

- [ ] **Step 1: Update README settings table**

In `README.md`, replace the daemon settings table under `## 설정` with:

```markdown
| Canonical 변수 | Alias 변수 | 기본값 | 설명 |
|---|---|---|---|
| `EPHEMERA_API_ADDR` | `ANVIL_API_ADDR` | `127.0.0.1:3000` | control plane bind 주소. reverse proxy 뒤에서는 `0.0.0.0:3000`으로 설정할 수 있다. |
| `EPHEMERA_API_PORT` | `ANVIL_API_PORT` | `3000` | API addr가 없을 때 사용하는 port. |
| `EPHEMERA_API_TOKENS` | `ANVIL_API_TOKENS` | unset | named Bearer token 목록. 예: `alice:token1,bob:token2` |
| `EPHEMERA_API_TOKEN` | `ANVIL_API_TOKEN` | unset | 단일 Bearer token fallback. |
| `EPHEMERA_AGENT_PORT` | `ANVIL_AGENT_PORT` | `8080` | VM 내부 `goose-agent` listen port. |
| `EPHEMERA_PUBLIC_URL` | `ANVIL_PUBLIC_URL` | unset | 외부에서 접근 가능한 control plane base URL. 설정 시 `agent_url`이 proxy path가 된다. |
```

Replace the paragraph immediately after that table with:

```markdown
`EPHEMERA_*`는 ephemera runtime의 canonical 변수이고 `ANVIL_*`는 anvil 운영자를
위한 alias다. 둘 다 설정되어 있으면 `EPHEMERA_*`가 우선한다.
`EPHEMERA_API_ADDR` 또는 `ANVIL_API_ADDR`가 port 변수보다 우선한다. token은
`SIGHUP`으로 daemon 재시작 없이 reload할 수 있다.
```

In `README.md` security model table, replace the `client -> control plane` row with:

```markdown
| client -> control plane | `EPHEMERA_API_TOKENS`/`EPHEMERA_API_TOKEN` 또는 `ANVIL_API_TOKENS`/`ANVIL_API_TOKEN` Bearer token |
```

- [ ] **Step 2: Update service logic authentication docs**

In `docs/architecture/service-logic.md`, replace:

```markdown
`SIGHUP`은 `ControlPlane.ReloadClients`를 호출한다. daemon 재시작이나 실행 중
VM 중단 없이 `EPHEMERA_API_TOKENS` 또는 `EPHEMERA_API_TOKEN`을 메모리에 다시
로드한다.
```

with:

```markdown
`SIGHUP`은 `ControlPlane.ReloadClients`를 호출한다. daemon 재시작이나 실행 중
VM 중단 없이 `EPHEMERA_API_TOKENS`/`ANVIL_API_TOKENS` 또는
`EPHEMERA_API_TOKEN`/`ANVIL_API_TOKEN`을 메모리에 다시 로드한다.
```

After that paragraph, add:

````markdown
환경 변수 precedence:

```text
EPHEMERA_API_TOKENS
  -> ANVIL_API_TOKENS
  -> EPHEMERA_API_TOKEN
  -> ANVIL_API_TOKEN
  -> 인증 비활성화
```

`EPHEMERA_*`는 ephemera runtime의 canonical 설정이고 `ANVIL_*`는 anvil 운영자를
위한 alias다. canonical 값이 있으면 alias 값보다 우선한다.
````

After the `## 서비스 경계` section tables and before `## 제어 평면 인증`, add:

````markdown
## Runtime 설정 alias

daemon은 기존 `EPHEMERA_*` 환경 변수를 canonical 계약으로 유지하면서 다음
`ANVIL_*` alias를 fallback으로 인식한다.

| Canonical | Alias |
|---|---|
| `EPHEMERA_API_ADDR` | `ANVIL_API_ADDR` |
| `EPHEMERA_API_PORT` | `ANVIL_API_PORT` |
| `EPHEMERA_API_TOKENS` | `ANVIL_API_TOKENS` |
| `EPHEMERA_API_TOKEN` | `ANVIL_API_TOKEN` |
| `EPHEMERA_AGENT_PORT` | `ANVIL_AGENT_PORT` |
| `EPHEMERA_PUBLIC_URL` | `ANVIL_PUBLIC_URL` |

각 설정은 canonical 값이 비어 있을 때만 alias 값을 사용한다. 이 규칙은 기존
ephemera 배포의 동작을 보존하면서 anvil 운영 문서에서 `ANVIL_*` 이름을 사용할 수
있게 한다.
````

- [ ] **Step 3: Update context contract**

In `CONTEXT.md`, replace the fixed runtime contract environment variable bullets:

```markdown
- control-plane token 환경 변수: `EPHEMERA_API_TOKENS`,
  `EPHEMERA_API_TOKEN`
- public agent URL 환경 변수: `EPHEMERA_PUBLIC_URL`
- MCP adapter daemon URL 환경 변수: `ANVIL_DAEMON_URL`
- MCP adapter token 환경 변수: `ANVIL_API_TOKEN`
```

with:

```markdown
- control-plane token canonical 환경 변수: `EPHEMERA_API_TOKENS`,
  `EPHEMERA_API_TOKEN`
- control-plane token alias 환경 변수: `ANVIL_API_TOKENS`,
  `ANVIL_API_TOKEN`
- public agent URL canonical 환경 변수: `EPHEMERA_PUBLIC_URL`
- public agent URL alias 환경 변수: `ANVIL_PUBLIC_URL`
- daemon bind canonical 환경 변수: `EPHEMERA_API_ADDR`,
  `EPHEMERA_API_PORT`
- daemon bind alias 환경 변수: `ANVIL_API_ADDR`,
  `ANVIL_API_PORT`
- guest agent port canonical 환경 변수: `EPHEMERA_AGENT_PORT`
- guest agent port alias 환경 변수: `ANVIL_AGENT_PORT`
- MCP adapter daemon URL 환경 변수: `ANVIL_DAEMON_URL`
- MCP adapter token 환경 변수: `ANVIL_API_TOKEN`
```

Delete the completed follow-up candidate line:

```markdown
- `EPHEMERA_*` 환경 변수의 `ANVIL_*` alias 추가
```

Keep the existing `MCP v2에서 snapshot/restore tool, workspace copy, persistent session 지원`
follow-up candidate unchanged. If that line was accidentally duplicated, leave only one copy.

- [ ] **Step 4: Run documentation consistency checks**

Run:

```bash
rg -n "ANVIL_API_ADDR|ANVIL_API_TOKENS|ANVIL_PUBLIC_URL|ANVIL_AGENT_PORT" README.md docs/architecture/service-logic.md CONTEXT.md
rg -n "EPHEMERA_\\*.*canonical|ANVIL_\\*.*alias|canonical.*alias" README.md docs/architecture/service-logic.md CONTEXT.md
```

Expected: both commands show matches in the updated docs.

- [ ] **Step 5: Commit documentation**

Run:

```bash
git add README.md docs/architecture/service-logic.md CONTEXT.md
git commit -m "docs: document anvil daemon env aliases"
```

Expected: commit succeeds and includes only the three documentation files.

---

### Task 4: Final Verification

**Files:**
- Verify all changed files from Tasks 1 to 3.

- [ ] **Step 1: Run formatting**

Run:

```bash
gofmt -w cmd/goose-daemon/config.go cmd/goose-daemon/config_test.go
```

Expected: command exits 0.

- [ ] **Step 2: Run focused tests**

Run:

```bash
go test ./cmd/goose-daemon -run 'TestLoadAPIClients|TestResolveAPIAddr|TestResolvePublicURL|TestResolveAgentPort' -count=1
go test ./cmd/goose-daemon -count=1
```

Expected: both commands pass.

- [ ] **Step 3: Run full test suite**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 4: Run build verification**

Run:

```bash
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
```

Expected: both builds succeed.

- [ ] **Step 5: Run diff and status checks**

Run:

```bash
git diff --check
git status --short
```

Expected: `git diff --check` prints no output. `git status --short` is clean if every task commit was made.

- [ ] **Step 6: Commit verification adjustments if formatting changed files**

If `gofmt` or documentation correction changed files after prior commits, run:

```bash
git add cmd/goose-daemon/config.go cmd/goose-daemon/config_test.go README.md docs/architecture/service-logic.md CONTEXT.md
git commit -m "chore: verify anvil daemon env aliases"
```

Expected: commit is created only when `git status --short` showed changes after verification.

---

## Self-Review

Spec coverage:

- Alias mapping: Task 1 tests and Task 2 implementation cover all six mappings.
- Precedence: Task 1 tests cover canonical over alias and multi-client token ordering.
- Existing compatibility: Task 1 updates existing tests to clear alias env vars and preserve canonical behavior.
- Config implementation: Task 2 adds alias-aware lookup helpers and applies them to all daemon settings.
- Documentation: Task 3 updates README, service logic, and context.
- Verification: Task 4 runs focused tests, full tests, builds, diff check, and status check.

Placeholder scan:

- This plan contains no forbidden placeholder tokens, no deferred implementation marker, and no ambiguous code references.

Type consistency:

- `resolveAgentPort`, `resolvePublicURL`, `envWithAlias`, and `envIntWithAlias` are introduced in Task 2 after tests reference them in Task 1.
- Token precedence tests match the documented order in the spec.
- Documentation names match the exact environment variable names used by the implementation.
