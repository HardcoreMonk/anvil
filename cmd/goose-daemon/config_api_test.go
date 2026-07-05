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

	if err := cp.writeProfileConfig("worker", "anthropic", "claude-sonnet-4-6", 4, 4096); err != nil {
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
	for _, want := range []string{"# Goose config", "GOOSE_DISABLE_KEYRING: true", "extensions:", "developer:", "timeout: 300", "EPHEMERA_VCPU_COUNT: 4", "EPHEMERA_MEM_SIZE_MIB: 4096"} {
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

func TestHandleConfigProviders_Shape(t *testing.T) {
	cp := newTestCP(t)
	// Only Google has a real key; everything else is unset.
	os.WriteFile(cp.gooseSecretsPath, []byte("GOOGLE_API_KEY: \"AIzaReal\"\n"), 0o644)

	rr := httptest.NewRecorder()
	cp.handleConfigProviders(rr, httptest.NewRequest(http.MethodGet, "/config/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var list []providerStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != len(providerRegistry) {
		t.Fatalf("got %d providers, want %d", len(list), len(providerRegistry))
	}
	byID := map[string]providerStatus{}
	for _, p := range list {
		byID[p.ID] = p
	}
	if !byID["google"].Available {
		t.Error("google should be available")
	}
	if byID["anthropic"].Available || byID["groq"].Available || byID["openai"].Available {
		t.Error("providers without a key should be unavailable")
	}
	if byID["groq"].DefaultModel == "" || len(byID["groq"].SuggestedModels) == 0 {
		t.Error("groq should carry default + suggested models")
	}
}

func TestCreateProfile_WritesGooseYAMLNoSecrets(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	body := strings.NewReader(`{"name":"myprofile","provider":"anthropic","model":"claude-sonnet-4-6"}`)
	cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodPost, "/config/profiles", body))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	dir := filepath.Join(cp.workDir, "configs", "profiles", "myprofile")
	out := mustRead(t, filepath.Join(dir, "goose.yaml"))
	if !strings.Contains(out, "GOOSE_PROVIDER: anthropic") || !strings.Contains(out, "GOOSE_MODEL: claude-sonnet-4-6") {
		t.Fatalf("goose.yaml not written correctly:\n%s", out)
	}
	// No per-profile secrets file is created — keys live in the global keychain.
	if _, err := os.Stat(filepath.Join(dir, "goose-secrets.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unexpected per-profile secrets file")
	}
}

func TestCreateProfile_DefaultsModel(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	// No model field → provider's default model is used.
	cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodPost, "/config/profiles",
		strings.NewReader(`{"name":"groqp","provider":"groq"}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	out := mustRead(t, filepath.Join(cp.workDir, "configs", "profiles", "groqp", "goose.yaml"))
	if !strings.Contains(out, "GOOSE_MODEL: openai/gpt-oss-120b") {
		t.Fatalf("expected groq default model:\n%s", out)
	}
}

func TestCreateProfile_RejectsDefaultAndBadNames(t *testing.T) {
	cp := newTestCP(t)
	for _, n := range []string{"default", "../evil", "Has Space", "UPPER", ".."} {
		rr := httptest.NewRecorder()
		b := strings.NewReader(`{"name":"` + n + `","provider":"google"}`)
		cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodPost, "/config/profiles", b))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("name %q: status = %d, want 400", n, rr.Code)
		}
	}
}

func TestCreateProfile_RejectsUnavailableProvider(t *testing.T) {
	cp := newTestCP(t)
	os.WriteFile(cp.gooseSecretsPath, []byte(""), 0o644) // no keys at all
	rr := httptest.NewRecorder()
	cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodPost, "/config/profiles",
		strings.NewReader(`{"name":"p","provider":"google"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateProfile_Duplicate(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "dup", sampleGooseYAML)
	rr := httptest.NewRecorder()
	cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodPost, "/config/profiles",
		strings.NewReader(`{"name":"dup","provider":"google"}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestDeleteProfile_RemovesDir(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "gone", sampleGooseYAML)
	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodDelete, "/config/profiles/gone", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cp.workDir, "configs", "profiles", "gone")); !os.IsNotExist(err) {
		t.Fatalf("profile dir not removed")
	}
}

func TestDeleteProfile_RejectsDefault(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodDelete, "/config/profiles/default", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestDeleteProfile_NotFound(t *testing.T) {
	cp := newTestCP(t)
	rr := httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodDelete, "/config/profiles/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
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

// systemMdPath is the on-disk path of a profile's system.md under the test workDir.
func systemMdPath(cp *ControlPlane, name string) string {
	return filepath.Join(cp.workDir, "configs", "profiles", name, "system.md")
}

func TestHandleConfigProfileSystem_GetEmpty(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	rr := httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodGet, "/config/profiles/worker/system", nil), "worker")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["system_md"] != "" {
		t.Fatalf("system_md = %q, want empty for a profile with no system.md", got["system_md"])
	}
}

func TestHandleConfigProfileSystem_PutThenGet(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	const prompt = "You are an orchestrator.\nFinish the whole job in one session."

	reqBody, _ := json.Marshal(map[string]string{"system_md": prompt})
	rr := httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/system", strings.NewReader(string(reqBody))), "worker")
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := mustRead(t, systemMdPath(cp, "worker")); got != prompt {
		t.Fatalf("file = %q, want %q", got, prompt)
	}

	rr = httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodGet, "/config/profiles/worker/system", nil), "worker")
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["system_md"] != prompt {
		t.Fatalf("GET system_md = %q, want %q", resp["system_md"], prompt)
	}
}

func TestHandleConfigProfileSystem_Delete(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	if err := os.WriteFile(systemMdPath(cp, "worker"), []byte("a prompt"), 0o644); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodDelete, "/config/profiles/worker/system", nil), "worker")
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(systemMdPath(cp, "worker")); !os.IsNotExist(err) {
		t.Fatalf("system.md not removed")
	}
	// Idempotent: clearing an already-absent prompt still succeeds.
	rr = httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodDelete, "/config/profiles/worker/system", nil), "worker")
	if rr.Code != http.StatusOK {
		t.Fatalf("idempotent DELETE status = %d, want 200", rr.Code)
	}
}

func TestHandleConfigProfileSystem_ProfileNotFound(t *testing.T) {
	cp := newTestCP(t)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rr := httptest.NewRecorder()
		cp.handleConfigProfileSystem(rr, httptest.NewRequest(m, "/config/profiles/nope/system", strings.NewReader(`{"system_md":"x"}`)), "nope")
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", m, rr.Code)
		}
	}
}

func TestHandleConfigProfileSystem_PathTraversal(t *testing.T) {
	cp := newTestCP(t)
	// Drive through the router so the /system suffix dispatch + name guard are exercised.
	for _, p := range []string{"/config/profiles/../evil/system", "/config/profiles/a/b/system"} {
		rr := httptest.NewRecorder()
		cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, p, strings.NewReader(`{"system_md":"x"}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", p, rr.Code)
		}
	}
}

func TestHandleConfigProfileSystem_RejectsDefault(t *testing.T) {
	cp := newTestCP(t)
	for _, m := range []string{http.MethodGet, http.MethodPut} {
		rr := httptest.NewRecorder()
		cp.handleConfigProfile(rr, httptest.NewRequest(m, "/config/profiles/default/system", strings.NewReader(`{"system_md":"x"}`)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", m, rr.Code)
		}
	}
}

func TestHandleConfigProfileSystem_Oversize(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	big := strings.Repeat("a", maxSystemPromptBytes+1)
	reqBody, _ := json.Marshal(map[string]string{"system_md": big})

	rr := httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/system", strings.NewReader(string(reqBody))), "worker")
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(systemMdPath(cp, "worker")); !os.IsNotExist(err) {
		t.Fatalf("oversize PUT should not write the file")
	}
}

func TestHandleConfigProfileSystem_BadJSON(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	rr := httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/system", strings.NewReader(`{bad json`)), "worker")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestLoadProfileSystemPrompt_RoundTrip(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	const prompt = "You are a careful reviewer."

	reqBody, _ := json.Marshal(map[string]string{"system_md": prompt})
	rr := httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker/system", strings.NewReader(string(reqBody))), "worker")
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", rr.Code)
	}
	// The spawn-time read path must see exactly what the handler wrote.
	if got := cp.loadProfileSystemPrompt("worker"); got != prompt {
		t.Fatalf("loadProfileSystemPrompt = %q, want %q", got, prompt)
	}
	// After a clear it reverts to empty.
	rr = httptest.NewRecorder()
	cp.handleConfigProfileSystem(rr, httptest.NewRequest(http.MethodDelete, "/config/profiles/worker/system", nil), "worker")
	if got := cp.loadProfileSystemPrompt("worker"); got != "" {
		t.Fatalf("after DELETE loadProfileSystemPrompt = %q, want empty", got)
	}
}
