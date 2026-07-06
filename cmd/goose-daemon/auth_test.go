package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ephemera/internal/metrics"
)

func TestAuthMiddleware_Outcomes(t *testing.T) {
	reg := metrics.NewRegistry()
	authTotal := reg.NewCounterVec("ephemera_auth_total", "auth decisions", "outcome")

	var ctxClient string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxClient = clientNameFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	clients := []APIClient{
		{Name: "alice", Token: "tokA"},
		{Name: "bob", Token: "tokB", Expires: time.Now().Add(time.Hour)},    // valid, late index
		{Name: "carol", Token: "tokC", Expires: time.Now().Add(-time.Hour)}, // expired
	}
	h := authMiddleware(func() []APIClient { return clients }, nil, authTotal, next)

	// doReq mirrors the audit wrapper: install a clientHolder so we can read the
	// back-filled client name, like the real outer middleware does.
	doReq := func(authz string) (int, string) {
		ctxClient = ""
		req := httptest.NewRequest(http.MethodGet, "/vms", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		holder := &clientHolder{}
		req = req.WithContext(withClientHolder(req.Context(), holder))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, holder.name
	}

	if code, name := doReq("Bearer tokA"); code != 200 || name != "alice" || ctxClient != "alice" {
		t.Errorf("valid: code=%d holder=%q ctx=%q, want 200/alice/alice", code, name, ctxClient)
	}
	if code, name := doReq("Bearer tokB"); code != 200 || name != "bob" {
		t.Errorf("valid late-index (no early-exit): code=%d holder=%q, want 200/bob", code, name)
	}
	if code, _ := doReq("Bearer tokC"); code != 401 {
		t.Errorf("expired token: code=%d, want 401", code)
	}
	if code, _ := doReq("Bearer nope"); code != 401 {
		t.Errorf("unknown token: code=%d, want 401", code)
	}
	if code, _ := doReq(""); code != 401 {
		t.Errorf("missing token: code=%d, want 401", code)
	}

	if g := authTotal.WithLabelValues("ok").Get(); g != 2 {
		t.Errorf("ok=%d, want 2", g)
	}
	if g := authTotal.WithLabelValues("expired").Get(); g != 1 {
		t.Errorf("expired=%d, want 1", g)
	}
	if g := authTotal.WithLabelValues("denied").Get(); g != 2 {
		t.Errorf("denied=%d, want 2", g)
	}
}

func TestAuthMiddleware_Disabled(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := authMiddleware(func() []APIClient { return nil }, nil, nil, next) // nil relayTokenFor + nil authTotal tolerated
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vms", nil))
	if !called || rec.Code != http.StatusOK {
		t.Errorf("auth disabled should pass through: called=%v code=%d", called, rec.Code)
	}
}

func TestAuthMiddleware_DuplicateTokenPrefersActiveWhenExpiredAppearsAfter(t *testing.T) {
	reg := metrics.NewRegistry()
	authTotal := reg.NewCounterVec("ephemera_auth_total", "auth decisions", "outcome")
	now := time.Now()
	clients := []APIClient{
		{Name: "active", Token: "shared", Expires: now.Add(time.Hour)},
		{Name: "expired", Token: "shared", Expires: now.Add(-time.Hour)},
	}

	var ctxClient string
	h := authMiddleware(func() []APIClient { return clients }, nil, authTotal, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxClient = clientNameFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	req.Header.Set("Authorization", "Bearer shared")
	holder := &clientHolder{}
	req = req.WithContext(withClientHolder(req.Context(), holder))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if holder.name != "active" || ctxClient != "active" {
		t.Fatalf("matched holder=%q ctx=%q, want active", holder.name, ctxClient)
	}
	if g := authTotal.WithLabelValues("ok").Get(); g != 1 {
		t.Fatalf("ok=%d, want 1", g)
	}
	if g := authTotal.WithLabelValues("expired").Get(); g != 0 {
		t.Fatalf("expired=%d, want 0", g)
	}
}

func TestAuthMiddleware_DuplicateTokenPrefersActiveWhenActiveAppearsAfter(t *testing.T) {
	reg := metrics.NewRegistry()
	authTotal := reg.NewCounterVec("ephemera_auth_total", "auth decisions", "outcome")
	now := time.Now()
	clients := []APIClient{
		{Name: "expired", Token: "shared", Expires: now.Add(-time.Hour)},
		{Name: "active", Token: "shared", Expires: now.Add(time.Hour)},
	}

	var ctxClient string
	h := authMiddleware(func() []APIClient { return clients }, nil, authTotal, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxClient = clientNameFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	req.Header.Set("Authorization", "Bearer shared")
	holder := &clientHolder{}
	req = req.WithContext(withClientHolder(req.Context(), holder))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if holder.name != "active" || ctxClient != "active" {
		t.Fatalf("matched holder=%q ctx=%q, want active", holder.name, ctxClient)
	}
	if g := authTotal.WithLabelValues("ok").Get(); g != 1 {
		t.Fatalf("ok=%d, want 1", g)
	}
	if g := authTotal.WithLabelValues("expired").Get(); g != 0 {
		t.Fatalf("expired=%d, want 0", g)
	}
}
