package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildTestMux replicates the externalMux wiring from NewControlPlane (api.go)
// without standing up a full ControlPlane (no KVM, network, or storage). It
// exercises the two invariants the UI integration must preserve:
//   - /ui/ is served WITHOUT auth (login page must load token-free)
//   - "/" still routes the API through authMiddleware (data stays protected)
//
// getClients controls whether auth is enabled (empty slice = disabled).
func buildTestMux(getClients func() []APIClient) http.Handler {
	cp := &ControlPlane{} // uiHandler does not touch ControlPlane state

	// Stub API handler standing in for internalMux.
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`)) // pretend this is GET /vms
	})

	mux := http.NewServeMux()
	mux.Handle("/ui/", cp.uiHandler())
	apiChain := authMiddleware(getClients, nil, api)
	mux.Handle("/", rootRedirectOr(apiChain))
	return mux
}

func authDisabled() []APIClient { return nil }
func authEnabled() []APIClient  { return []APIClient{{Name: "t", Token: "secret"}} }

func TestUIServesIndex(t *testing.T) {
	mux := buildTestMux(authDisabled)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /ui/ status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /ui/ content-type = %q, want text/html*", ct)
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "<html") {
		t.Fatalf("GET /ui/ body is not an HTML document")
	}
}

func TestUISPAFallback(t *testing.T) {
	mux := buildTestMux(authDisabled)
	rr := httptest.NewRecorder()
	// A client-side route with no matching file must fall back to index.html.
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/vms/abc-123", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("SPA route status = %d, want 200 (index fallback)", rr.Code)
	}
	if !strings.Contains(strings.ToLower(rr.Body.String()), "<html") {
		t.Fatalf("SPA route body is not the index document")
	}
}

func TestUIMissingAsset404(t *testing.T) {
	mux := buildTestMux(authDisabled)
	rr := httptest.NewRecorder()
	// A missing asset (has an extension) must 404, NOT fall back to index.
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/assets/does-not-exist.js", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", rr.Code)
	}
}

func TestUIAuthExempt(t *testing.T) {
	mux := buildTestMux(authEnabled) // auth IS configured
	rr := httptest.NewRecorder()
	// No Authorization header — the UI must still load.
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/ui/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /ui/ with auth enabled, no token = %d, want 200 (auth-exempt)", rr.Code)
	}
}

func TestAPIStillProtected(t *testing.T) {
	mux := buildTestMux(authEnabled)
	rr := httptest.NewRecorder()
	// The API path must still 401 without a token — UI mounting must not leak.
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vms", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("GET /vms with auth enabled, no token = %d, want 401", rr.Code)
	}
}

func TestRootRedirectsToUI(t *testing.T) {
	mux := buildTestMux(authDisabled)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusFound {
		t.Fatalf("GET / status = %d, want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/" {
		t.Fatalf("GET / Location = %q, want /ui/", loc)
	}
}
