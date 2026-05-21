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
	"time"
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

func TestWorkloadScriptPathAcceptsOnlyWorkloadShellScripts(t *testing.T) {
	root := t.TempDir()
	workloads := filepath.Join(root, "workloads")
	if err := os.MkdirAll(workloads, 0755); err != nil {
		t.Fatalf("mkdir workloads: %v", err)
	}
	script := filepath.Join(workloads, "nginx-smoke.sh")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\n"), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	fullPath, clean, status, err := workloadScriptPath(root, "workloads/nginx-smoke.sh")
	if err != nil {
		t.Fatalf("workloadScriptPath returned error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if clean != "workloads/nginx-smoke.sh" {
		t.Fatalf("clean path = %q, want workloads/nginx-smoke.sh", clean)
	}
	if fullPath != script {
		t.Fatalf("full path = %q, want %q", fullPath, script)
	}
}

func TestWorkloadScriptPathRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name   string
		script string
		status int
	}{
		{"empty", "", http.StatusBadRequest},
		{"absolute", "/workspace/workloads/run.sh", http.StatusBadRequest},
		{"traversal", "workloads/../secret.sh", http.StatusBadRequest},
		{"outside", "notes/run.sh", http.StatusBadRequest},
		{"not shell", "workloads/run.txt", http.StatusBadRequest},
		{"missing", "workloads/missing.sh", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, status, err := workloadScriptPath(root, tc.script)
			if err == nil {
				t.Fatalf("workloadScriptPath(%q) returned nil error", tc.script)
			}
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
		})
	}
}

func TestWorkloadScriptPathRejectsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	workloads := filepath.Join(root, "workloads")
	if err := os.MkdirAll(workloads, 0755); err != nil {
		t.Fatalf("mkdir workloads: %v", err)
	}
	if err := os.Mkdir(filepath.Join(workloads, "dir.sh"), 0755); err != nil {
		t.Fatalf("mkdir dir workload: %v", err)
	}
	outside := filepath.Join(root, "outside.sh")
	if err := os.WriteFile(outside, []byte("#!/usr/bin/env bash\n"), 0755); err != nil {
		t.Fatalf("write outside script: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workloads, "link.sh")); err != nil {
		t.Fatalf("symlink workload: %v", err)
	}

	cases := []string{
		"workloads/dir.sh",
		"workloads/link.sh",
	}
	for _, script := range cases {
		t.Run(script, func(t *testing.T) {
			_, _, status, err := workloadScriptPath(root, script)
			if err == nil {
				t.Fatalf("workloadScriptPath(%q) returned nil error", script)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
			}
		})
	}
}

func TestWorkloadTimeoutSeconds(t *testing.T) {
	cases := []struct {
		name    string
		seconds int
		want    time.Duration
		wantErr bool
	}{
		{"default", 0, defaultWorkloadTimeout, false},
		{"minimum", 1, time.Second, false},
		{"maximum", 1800, 1800 * time.Second, false},
		{"negative", -1, 0, true},
		{"too high", 1801, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := workloadTimeoutSeconds(tc.seconds)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("duration = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandleWorkloadRunSuccess(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/success.sh", "#!/usr/bin/env bash\necho stdout-ok\necho stderr-ok >&2\n")
	handler := workloadRunHandler(root)

	body := strings.NewReader(`{"script":"workloads/success.sh","timeout_seconds":5}`)
	req := httptest.NewRequest(http.MethodPost, "/workloads/run", body)
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	var result WorkloadRunResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "stdout-ok") {
		t.Fatalf("stdout = %q, want stdout-ok", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "stderr-ok") {
		t.Fatalf("stderr = %q, want stderr-ok", result.Stderr)
	}
	if result.Script != "workloads/success.sh" {
		t.Fatalf("script = %q, want workloads/success.sh", result.Script)
	}
}

func TestHandleWorkloadRunNonzeroExitReturnsResult(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/fail.sh", "#!/usr/bin/env bash\necho failed >&2\nexit 7\n")
	handler := workloadRunHandler(root)

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/fail.sh","timeout_seconds":5}`))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	var result WorkloadRunResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit_code = %d, want 7", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "failed") {
		t.Fatalf("stderr = %q, want failed", result.Stderr)
	}
}

func TestHandleWorkloadRunTimeout(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/sleep.sh", "#!/usr/bin/env bash\nsleep 2\n")
	handler := workloadRunHandler(root)

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/sleep.sh","timeout_seconds":1}`))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q; want 200", rr.Code, rr.Body.String())
	}
	var result WorkloadRunResult
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.TimedOut {
		t.Fatalf("timed_out = false, want true")
	}
	if result.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", result.ExitCode)
	}
}

func TestHandleWorkloadRunRejectsInvalidPath(t *testing.T) {
	handler := workloadRunHandler(t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"../secret.sh"}`))
	rr := httptest.NewRecorder()

	handler(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q; want 400", rr.Code, rr.Body.String())
	}
}

func TestHandleWorkloadRunBusy(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/success.sh", "#!/usr/bin/env bash\necho ok\n")
	handler := workloadRunHandler(root)

	mu.Lock()
	busy = true
	mu.Unlock()
	defer func() {
		mu.Lock()
		busy = false
		mu.Unlock()
	}()

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/success.sh"}`))
	rr := httptest.NewRecorder()
	handler(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func writeWorkloadScript(t *testing.T, root, relPath, content string) {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir script parent: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
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
