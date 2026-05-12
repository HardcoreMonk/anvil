package main

import (
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---- profileConfigPaths ----

func newTestCP(t *testing.T) *ControlPlane {
	t.Helper()
	tmp := t.TempDir()
	// Create stub default config files.
	defaultCfg := filepath.Join(tmp, "goose.yaml")
	defaultSec := filepath.Join(tmp, "goose-secrets.yaml")
	os.WriteFile(defaultCfg, []byte("GOOSE_PROVIDER: default\n"), 0644)
	os.WriteFile(defaultSec, []byte("DEFAULT_KEY: x\n"), 0644)
	return &ControlPlane{
		vms:              make(map[string]*runningVM),
		workDir:          tmp,
		gooseConfigPath:  defaultCfg,
		gooseSecretsPath: defaultSec,
	}
}

func TestProfileConfigPaths_EmptyProfile_ReturnsDefaults(t *testing.T) {
	cp := newTestCP(t)
	cfg, sec, err := cp.profileConfigPaths("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != cp.gooseConfigPath {
		t.Errorf("expected default configPath %q, got %q", cp.gooseConfigPath, cfg)
	}
	if sec != cp.gooseSecretsPath {
		t.Errorf("expected default secretsPath %q, got %q", cp.gooseSecretsPath, sec)
	}
}

func TestProfileConfigPaths_ValidProfile_ReturnsPaths(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "anthropic")
	os.MkdirAll(profileDir, 0755)
	os.WriteFile(filepath.Join(profileDir, "goose.yaml"), []byte("GOOSE_PROVIDER: anthropic\n"), 0644)
	os.WriteFile(filepath.Join(profileDir, "goose-secrets.yaml"), []byte("ANTHROPIC_API_KEY: sk\n"), 0644)

	cfg, sec, err := cp.profileConfigPaths("anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != filepath.Join(profileDir, "goose.yaml") {
		t.Errorf("unexpected configPath: %q", cfg)
	}
	if sec != filepath.Join(profileDir, "goose-secrets.yaml") {
		t.Errorf("unexpected secretsPath: %q", sec)
	}
}

func TestProfileConfigPaths_MissingConfigYaml_Error(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "partial")
	os.MkdirAll(profileDir, 0755)
	// Only goose-secrets.yaml, no goose.yaml
	os.WriteFile(filepath.Join(profileDir, "goose-secrets.yaml"), []byte("KEY: x\n"), 0644)

	_, _, err := cp.profileConfigPaths("partial")
	if err == nil {
		t.Error("expected error for missing goose.yaml")
	}
}

func TestProfileConfigPaths_MissingSecretsYaml_Error(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "partial2")
	os.MkdirAll(profileDir, 0755)
	// Only goose.yaml, no goose-secrets.yaml
	os.WriteFile(filepath.Join(profileDir, "goose.yaml"), []byte("GOOSE_PROVIDER: test\n"), 0644)

	_, _, err := cp.profileConfigPaths("partial2")
	if err == nil {
		t.Error("expected error for missing goose-secrets.yaml")
	}
}

func TestProfileConfigPaths_PathTraversal_Rejected(t *testing.T) {
	cp := newTestCP(t)
	for _, evil := range []string{"../evil", "../../etc", "a/b", `a\b`} {
		_, _, err := cp.profileConfigPaths(evil)
		if err == nil {
			t.Errorf("expected error for path-traversal profile name %q", evil)
		}
	}
}

func TestProfileConfigPaths_DotDot_Rejected(t *testing.T) {
	cp := newTestCP(t)
	_, _, err := cp.profileConfigPaths("..")
	if err == nil {
		t.Error("expected error for profile name '..'")
	}
}

// ---- generateAgentToken ----

func TestGenerateAgentToken_Length(t *testing.T) {
	tok, err := generateAgentToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tok) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Errorf("token is not valid hex: %v", err)
	}
}

func TestGenerateAgentToken_Uniqueness(t *testing.T) {
	a, _ := generateAgentToken()
	b, _ := generateAgentToken()
	if a == b {
		t.Error("two tokens should not be identical (probabilistic)")
	}
}

func TestHandleVMWorkspaceProxiesQueryAuthAndBody(t *testing.T) {
	var gotBody string
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/workspace" {
			t.Fatalf("path = %s, want /workspace", r.URL.Path)
		}
		if r.URL.Query().Get("path") != "notes/task.txt" {
			t.Fatalf("query path = %q, want notes/task.txt", r.URL.Query().Get("path"))
		}
		if got := r.Header.Get("Authorization"); got != "Bearer agent-token" {
			t.Fatalf("Authorization = %q, want Bearer agent-token", got)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotBody = string(data)
		_, _ = w.Write([]byte(`{"path":"notes/task.txt","bytes":5}`))
	}))
	defer agent.Close()

	_, portText, err := net.SplitHostPort(strings.TrimPrefix(agent.URL, "http://"))
	if err != nil {
		t.Fatalf("split agent URL: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse agent port: %v", err)
	}
	oldAgentPort := agentPort
	agentPort = port
	defer func() { agentPort = oldAgentPort }()

	cp := newTestCP(t)
	cp.agentHTTPClient = agent.Client()
	cp.vms["vm-1"] = &runningVM{
		VMInfo: VMInfo{
			VMID:    "vm-1",
			GuestIP: "127.0.0.1",
		},
		agentToken: "agent-token",
	}

	req := httptest.NewRequest(http.MethodPut, "/vms/vm-1/workspace?path=notes/task.txt", strings.NewReader("hello"))
	rr := httptest.NewRecorder()
	cp.handleVM(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	if gotBody != "hello" {
		t.Fatalf("proxied body = %q, want hello", gotBody)
	}
}
