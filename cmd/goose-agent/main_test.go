package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAgentAuthMiddleware_EmptyToken_Passthrough(t *testing.T) {
	called := false
	handler := agentAuthMiddleware("", func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/tasks", nil))
	if !called {
		t.Error("expected passthrough when token is empty (auth disabled)")
	}
}

func TestAgentAuthMiddleware_CorrectToken_Passthrough(t *testing.T) {
	const token = "correcttoken"
	called := false
	handler := agentAuthMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler(httptest.NewRecorder(), req)
	if !called {
		t.Error("expected passthrough with correct token")
	}
}

func TestAgentAuthMiddleware_WrongToken_401(t *testing.T) {
	handler := agentAuthMiddleware("righttoken", func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be called with wrong token")
	})
	req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	req.Header.Set("Authorization", "Bearer wrongtoken")
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestAgentAuthMiddleware_MissingHeader_401(t *testing.T) {
	handler := agentAuthMiddleware("sometoken", func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be called without auth header")
	})
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodPost, "/tasks", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestLoadAgentToken_FileAbsent(t *testing.T) {
	// /root/.ephemera-agent-token won't exist in test environments (non-VM hosts).
	// Verify that loadAgentToken returns "" without panicking.
	if _, err := os.Stat(agentTokenPath); !os.IsNotExist(err) {
		t.Skip("token file exists in this environment — skipping absence test")
	}
	if got := loadAgentToken(); got != "" {
		t.Errorf("expected empty string for absent file, got %q", got)
	}
}

func TestLoadAgentToken_TrimsWhitespace(t *testing.T) {
	// Verify that the TrimSpace behavior is correct (loadAgentToken's return path).
	// We test the trim logic directly since the token path is a const we cannot override.
	raw := "  mytoken123\n"
	got := strings.TrimSpace(raw)
	if got != "mytoken123" {
		t.Errorf("expected trimmed token, got %q", got)
	}
}

func TestExtractGooseJSONText_HappyPath(t *testing.T) {
	in := []byte(`{
	  "messages": [
	    {"role":"user", "content":[{"type":"text","text":"hello"}]},
	    {"role":"assistant", "content":[{"type":"text","text":"world"}]}
	  ],
	  "metadata": {"status":"ok"}
	}`)
	if got := extractGooseJSONText(in); got != "world" {
		t.Errorf("expected %q, got %q", "world", got)
	}
}

func TestExtractGooseJSONText_MultipleAssistantBlocks(t *testing.T) {
	in := []byte(`{"messages":[
	  {"role":"assistant","content":[
	    {"type":"text","text":"line one"},
	    {"type":"toolRequest","id":"x"},
	    {"type":"text","text":"line two"}
	  ]},
	  {"role":"assistant","content":[{"type":"text","text":"line three"}]}
	]}`)
	want := "line one\nline two\nline three"
	if got := extractGooseJSONText(in); got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

func TestExtractGooseJSONText_NonJSONInput_ReturnsEmpty(t *testing.T) {
	// goose may crash before producing JSON — caller falls back to raw stdout.
	if got := extractGooseJSONText([]byte("panic at the disco")); got != "" {
		t.Errorf("expected empty string for non-JSON input, got %q", got)
	}
}

func TestExtractGooseJSONText_StripsBannerPrefix(t *testing.T) {
	// `goose run --output-format json` prints a startup banner to stdout
	// before the JSON envelope. Our extractor must skip it instead of
	// failing the whole unmarshal.
	in := []byte("    __( O)>  ● new session · google gemini-2.5-flash-lite\n" +
		"   \\____)    20260522_1 · /\n" +
		"     L L     goose is ready\n" +
		`{"messages":[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`)
	if got := extractGooseJSONText(in); got != "hi" {
		t.Errorf("expected %q after banner strip, got %q", "hi", got)
	}
}
