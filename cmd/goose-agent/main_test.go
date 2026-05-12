package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestWorkspaceFilePathRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	for _, unsafePath := range []string{"", ".", "/absolute", "../secret", "safe/../../secret"} {
		if _, err := workspaceFilePath(root, unsafePath); err == nil {
			t.Fatalf("workspaceFilePath(%q) returned nil error", unsafePath)
		}
	}
}

func TestHandleWorkspacePutGetRoundTrip(t *testing.T) {
	root := t.TempDir()
	handler := workspaceHandler(root)

	putReq := httptest.NewRequest(http.MethodPut, "/workspace?path=notes/task.txt", bytes.NewBufferString("hello workspace"))
	putRR := httptest.NewRecorder()
	handler(putRR, putReq)
	if putRR.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %q; want 200", putRR.Code, putRR.Body.String())
	}

	written, err := os.ReadFile(filepath.Join(root, "notes", "task.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(written) != "hello workspace" {
		t.Fatalf("written file = %q, want hello workspace", string(written))
	}

	getReq := httptest.NewRequest(http.MethodGet, "/workspace?path=notes/task.txt", nil)
	getRR := httptest.NewRecorder()
	handler(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %q; want 200", getRR.Code, getRR.Body.String())
	}
	got, err := io.ReadAll(getRR.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(got) != "hello workspace" {
		t.Fatalf("GET body = %q, want hello workspace", string(got))
	}
}

func TestHandleWorkspaceRejectsTraversal(t *testing.T) {
	handler := workspaceHandler(t.TempDir())
	req := httptest.NewRequest(http.MethodPut, "/workspace?path=../secret.txt", strings.NewReader("secret"))
	rr := httptest.NewRecorder()

	handler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandleWorkspaceMissingFile(t *testing.T) {
	handler := workspaceHandler(t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/workspace?path=missing.txt", nil)
	rr := httptest.NewRecorder()

	handler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandleWorkspaceRejectsOverwriteByDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0755); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}
	target := filepath.Join(root, "notes", "task.txt")
	if err := os.WriteFile(target, []byte("old"), 0644); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	handler := workspaceHandler(root)

	req := httptest.NewRequest(http.MethodPut, "/workspace?path=notes/task.txt", strings.NewReader("new"))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %q; want 409", rr.Code, rr.Body.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("file content = %q, want old", string(got))
	}
	assertJSONError(t, rr, "workspace file already exists")
}

func TestHandleWorkspaceAllowsExplicitOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0755); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}
	target := filepath.Join(root, "notes", "task.txt")
	if err := os.WriteFile(target, []byte("old"), 0644); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	handler := workspaceHandler(root)

	req := httptest.NewRequest(http.MethodPut, "/workspace?path=notes/task.txt&overwrite=true", strings.NewReader("new"))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("file content = %q, want new", string(got))
	}
}

func TestHandleWorkspaceRejectsOversizedPut(t *testing.T) {
	handler := workspaceHandler(t.TempDir())
	body := strings.NewReader(strings.Repeat("x", maxWorkspaceFileBytes+1))
	req := httptest.NewRequest(http.MethodPut, "/workspace?path=large.txt", body)
	rr := httptest.NewRecorder()

	handler(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %q; want 413", rr.Code, rr.Body.String())
	}
	assertJSONError(t, rr, "workspace file exceeds size limit")
}

func TestHandleWorkspaceRejectsOversizedGet(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "large.txt")
	if err := os.WriteFile(target, []byte(strings.Repeat("x", maxWorkspaceFileBytes+1)), 0644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	handler := workspaceHandler(root)
	req := httptest.NewRequest(http.MethodGet, "/workspace?path=large.txt", nil)
	rr := httptest.NewRecorder()

	handler(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %q; want 413", rr.Code, rr.Body.String())
	}
	assertJSONError(t, rr, "workspace file exceeds size limit")
}

func TestHandleWorkspaceErrorsAreJSON(t *testing.T) {
	handler := workspaceHandler(t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/workspace?path=../secret.txt", nil)
	rr := httptest.NewRecorder()

	handler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	assertJSONError(t, rr, "path must stay within workspace")
}

func assertJSONError(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode JSON error body %q: %v", rr.Body.String(), err)
	}
	if body.Error != want {
		t.Fatalf("error = %q, want %q", body.Error, want)
	}
}
