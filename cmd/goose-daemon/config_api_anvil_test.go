package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These guards lock in two anvil surface-policy guarantees that upstream's
// v0.5.0 tests do not cover:
//
//  1. /config/profiles is a DATA API, so it must sit BEHIND bearer auth when
//     auth is configured — only /ui/ (static bundle + login) may live outside
//     auth. Upstream ui_test.go proves /ui/ is auth-exempt and that a *stub* API
//     handler 401s, but never wires the real /config/profiles routes through
//     authMiddleware.
//  2. The profile-config surface must NEVER read or write goose-secrets.yaml;
//     API keys stay server-side and are never reachable through this operator
//     surface.

// buildProfileAPIChain wires the real /config/profiles routes exactly as
// NewControlPlane does (on the internal mux, behind authMiddleware), so the test
// exercises the production routing rather than a stub.
func buildProfileAPIChain(cp *ControlPlane, getClients func() []APIClient) http.Handler {
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/config/profiles", cp.handleConfigProfiles)
	internalMux.HandleFunc("/config/profiles/", cp.handleConfigProfile)
	return authMiddleware(getClients, nil, internalMux)
}

func TestConfigProfilesRequireAuthWhenConfigured(t *testing.T) {
	cp := newTestCP(t)
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)
	chain := buildProfileAPIChain(cp, authEnabled) // auth IS configured

	// Every profile-config method must 401 without a bearer token.
	cases := []struct {
		method, path string
		body         string
	}{
		{http.MethodGet, "/config/profiles", ""},
		{http.MethodGet, "/config/profiles/worker", ""},
		{http.MethodPut, "/config/profiles/worker", `{"provider":"openai","model":"gpt-4o"}`},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		var body *strings.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		} else {
			body = strings.NewReader("")
		}
		chain.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, body))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401 (data API must be behind auth)", c.method, c.path, rr.Code)
		}
	}

	// A valid bearer token must reach the handler (proving the 401 above is auth,
	// not a routing miss).
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/config/profiles", nil)
	req.Header.Set("Authorization", "Bearer secret") // matches authEnabled()'s token
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config/profiles with valid token = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestConfigProfilesAuthDisabledStillServes(t *testing.T) {
	// When no clients are configured (auth disabled) the surface is reachable
	// without a token, same as every other data API in that mode.
	cp := newTestCP(t)
	chain := buildProfileAPIChain(cp, authDisabled)

	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/config/profiles", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config/profiles (auth disabled) = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// secretSentinels are strings that must never appear in a profile-config
// response body: the per-profile API key, its value, the key name, and the
// default secrets sentinel that newTestCP writes into goose-secrets.yaml.
var secretSentinels = []string{"SENTINEL-DO-NOT-LEAK", "sk-", "OPENAI_API_KEY", "DEFAULT_KEY"}

func assertNoSecret(t *testing.T, where, body string) {
	t.Helper()
	for _, s := range secretSentinels {
		if strings.Contains(body, s) {
			t.Errorf("%s leaked secret material %q: %s", where, s, body)
		}
	}
}

func TestConfigProfileSurfaceNeverReadsOrWritesSecrets(t *testing.T) {
	cp := newTestCP(t) // also writes {workDir}/goose-secrets.yaml with DEFAULT_KEY: x
	writeProfileFixture(t, cp, "worker", sampleGooseYAML)

	// Plant a per-profile goose-secrets.yaml alongside the profile's goose.yaml.
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "worker")
	profileSecrets := filepath.Join(profileDir, "goose-secrets.yaml")
	const secretContent = "OPENAI_API_KEY: sk-SENTINEL-DO-NOT-LEAK\n"
	if err := os.WriteFile(profileSecrets, []byte(secretContent), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeProfileSecrets := mustRead(t, profileSecrets)
	beforeDefaultSecrets := mustRead(t, cp.gooseSecretsPath)

	// GET list, GET single, and PUT must all avoid exposing secret material.
	rr := httptest.NewRecorder()
	cp.handleConfigProfiles(rr, httptest.NewRequest(http.MethodGet, "/config/profiles", nil))
	assertNoSecret(t, "GET /config/profiles", rr.Body.String())

	rr = httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodGet, "/config/profiles/worker", nil))
	assertNoSecret(t, "GET /config/profiles/worker", rr.Body.String())

	rr = httptest.NewRecorder()
	cp.handleConfigProfile(rr, httptest.NewRequest(http.MethodPut, "/config/profiles/worker",
		strings.NewReader(`{"provider":"openai","model":"gpt-4o"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT /config/profiles/worker = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertNoSecret(t, "PUT /config/profiles/worker response", rr.Body.String())

	// The secrets files must be byte-for-byte untouched by the write path.
	if mustRead(t, profileSecrets) != beforeProfileSecrets {
		t.Errorf("profile goose-secrets.yaml was modified by the config surface")
	}
	if mustRead(t, cp.gooseSecretsPath) != beforeDefaultSecrets {
		t.Errorf("default goose-secrets.yaml was modified by the config surface")
	}

	// The path the surface resolves for read/write must always be a goose.yaml,
	// never a secrets file.
	for _, name := range []string{"default", "worker"} {
		p, err := cp.gooseConfigPathForProfile(name)
		if err != nil {
			t.Fatalf("gooseConfigPathForProfile(%q): %v", name, err)
		}
		if strings.Contains(strings.ToLower(filepath.Base(p)), "secret") {
			t.Errorf("profile %q resolves to a secrets path %q", name, p)
		}
	}
}

// TestConfigProvidersNeverExposeKeyValues locks in the anvil surface guarantee
// that GET /config/providers reports key *availability* only and never the key
// value itself (v0.5.1 added this endpoint; providerStatus has no key field, so
// this is a regression guard against a future field being marshaled). We plant a
// distinctive sentinel value that must be DETECTED (Available=true, proving the
// key was read) yet must NEVER appear anywhere in the JSON response body.
func TestConfigProvidersNeverExposeKeyValues(t *testing.T) {
	cp := newTestCP(t)
	const providerKeyValue = "AIzaSENTINEL-PROVIDER-KEY-DO-NOT-LEAK"
	// Sole keychain entry: only Google has a (sentinel) key.
	if err := os.WriteFile(cp.gooseSecretsPath, []byte("GOOGLE_API_KEY: \""+providerKeyValue+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Control: the sentinel really is on disk, so a leak would be possible.
	if !strings.Contains(mustRead(t, cp.gooseSecretsPath), providerKeyValue) {
		t.Fatal("precondition: sentinel key value must be present on disk")
	}

	rr := httptest.NewRecorder()
	cp.handleConfigProviders(rr, httptest.NewRequest(http.MethodGet, "/config/providers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config/providers = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// The key must be reported present (availability is the only thing exposed)...
	if !strings.Contains(body, "\"available\":true") {
		t.Errorf("google key present but no provider reported available; body=%s", body)
	}
	// ...but its value must never appear in the response.
	if strings.Contains(body, providerKeyValue) {
		t.Errorf("GET /config/providers leaked the API key value: %s", body)
	}
}
