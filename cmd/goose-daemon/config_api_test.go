package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProfileFixture creates configs/profiles/{name}/goose.yaml under the test
// ControlPlane's workDir and returns its path.
func writeProfileFixture(t *testing.T, cp *ControlPlane, name, content string) string {
	t.Helper()
	dir := filepath.Join(cp.workDir, "configs", "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "goose.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const sampleGooseYAML = `# Goose config
GOOSE_PROVIDER: google
GOOSE_MODEL: gemini-flash-latest
GOOSE_DISABLE_KEYRING: true

extensions:
  developer:
    bundled: true
    enabled: true
    timeout: 300
`

func TestReadProfileConfig(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	pc, err := cp.readProfileConfig("worker")
	if err != nil {
		t.Fatalf("readProfileConfig: %v", err)
	}
	if pc.Provider != "google" || pc.Model != "gemini-flash-latest" {
		t.Fatalf("got provider=%q model=%q", pc.Provider, pc.Model)
	}
	if pc.Name != "worker" {
		t.Fatalf("name = %q, want worker", pc.Name)
	}
}

func TestWriteProfileConfig_PreservesStructure(t *testing.T) {
	cp := newTestCP(t)
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	if err := cp.writeProfileConfig("worker", "anthropic", "claude-sonnet-4-6"); err != nil {
		t.Fatalf("writeProfileConfig: %v", err)
	}
	out := mustRead(t, path)

	if !strings.Contains(out, "GOOSE_PROVIDER: anthropic") {
		t.Errorf("provider not updated:\n%s", out)
	}
	if !strings.Contains(out, "GOOSE_MODEL: claude-sonnet-4-6") {
		t.Errorf("model not updated:\n%s", out)
	}
	// Comments, unrelated keys, and the nested extensions block must survive.
	for _, want := range []string{"# Goose config", "GOOSE_DISABLE_KEYRING: true", "extensions:", "developer:", "timeout: 300"} {
		if !strings.Contains(out, want) {
			t.Errorf("writeProfileConfig dropped %q:\n%s", want, out)
		}
	}
	// The replaced value must be gone.
	if strings.Contains(out, "gemini-flash-latest") {
		t.Errorf("old model still present:\n%s", out)
	}
}

func TestHandleConfigProfiles_List(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", "GOOSE_PROVIDER: anthropic\nGOOSE_MODEL: claude-sonnet-4-6\n")

	rr := httptest.NewRecorder()
	cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodGet, "/config/profiles", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var list []ProfileConfig
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawDefault, sawWorker bool
	for _, pc := range list {
		if pc.Name == "default" && pc.Provider == "default" {
			sawDefault = true
		}
		if pc.Name == "worker" && pc.Model == "claude-sonnet-4-6" {
			sawWorker = true
		}
	}
	if !sawDefault || !sawWorker {
		t.Fatalf("list missing entries: %+v", list)
	}
}

func TestHandleConfigProfile_Put(t *testing.T) {
	cp := newTestCP(t)
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"provider":"openai","model":"gpt-4o"}`)
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker", body))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	out := mustRead(t, path)
	if !strings.Contains(out, "GOOSE_MODEL: gpt-4o") || !strings.Contains(out, "GOOSE_PROVIDER: openai") {
		t.Fatalf("file not updated:\n%s", out)
	}
}

func TestHandleConfigProfile_RejectsNewlineInjection(t *testing.T) {
	cp := newTestCP(t)
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	rr := httptest.NewRecorder()
	// A newline in the value would smuggle in extra YAML keys — must be rejected.
	body := strings.NewReader(`{"provider":"google","model":"gemini\nGOOSE_DISABLE_KEYRING: false"}`)
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker", body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(mustRead(t, path), "GOOSE_DISABLE_KEYRING: false") {
		t.Fatalf("injection succeeded")
	}
}

func TestHandleConfigProfile_PathTraversal(t *testing.T) {
	cp := newTestCP(t)
	for _, p := range []string{"/config/profiles/../evil", "/config/profiles/a/b"} {
		rr := httptest.NewRecorder()
		cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, p, strings.NewReader(`{"provider":"x","model":"y"}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", p, rr.Code)
		}
	}
}

func TestReadGooseConfigFile(t *testing.T) {
	cp := newTestCP(t)
	path := writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	provider, model := readGooseConfigFile(path)
	if provider != "google" || model != "gemini-flash-latest" {
		t.Fatalf("got provider=%q model=%q", provider, model)
	}
	// A missing file is best-effort: empty strings, no panic.
	if p, m := readGooseConfigFile(filepath.Join(cp.workDir, "nope.yaml")); p != "" || m != "" {
		t.Fatalf("missing file should yield empty, got %q/%q", p, m)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
