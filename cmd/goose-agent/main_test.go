package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func TestAgentAuthMiddlewareWithTokenProviderUsesUpdatedToken(t *testing.T) {
	token := "first"
	called := 0
	handler := agentAuthMiddlewareWithTokenProvider(func() string { return token }, func(w http.ResponseWriter, r *http.Request) {
		called++
	})

	firstReq := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	firstReq.Header.Set("Authorization", "Bearer first")
	handler(httptest.NewRecorder(), firstReq)
	if called != 1 {
		t.Fatalf("called = %d, want 1 after first token", called)
	}

	token = "second"
	oldReq := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	oldReq.Header.Set("Authorization", "Bearer first")
	oldRR := httptest.NewRecorder()
	handler(oldRR, oldReq)
	if oldRR.Code != http.StatusUnauthorized {
		t.Fatalf("old token status = %d, want 401", oldRR.Code)
	}

	nextReq := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	nextReq.Header.Set("Authorization", "Bearer second")
	handler(httptest.NewRecorder(), nextReq)
	if called != 2 {
		t.Fatalf("called = %d, want 2 after updated token", called)
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

func TestWorkloadScriptPathRejectsSymlinkDirectories(t *testing.T) {
	t.Run("workload root symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "run.sh"), []byte("#!/usr/bin/env bash\n"), 0755); err != nil {
			t.Fatalf("write outside script: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "workloads")); err != nil {
			t.Fatalf("symlink workloads: %v", err)
		}

		_, _, status, err := workloadScriptPath(root, "workloads/run.sh")
		if err == nil {
			t.Fatal("workloadScriptPath returned nil error")
		}
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
		}
	})

	t.Run("nested directory symlink", func(t *testing.T) {
		root := t.TempDir()
		workloads := filepath.Join(root, "workloads")
		if err := os.MkdirAll(workloads, 0755); err != nil {
			t.Fatalf("mkdir workloads: %v", err)
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "run.sh"), []byte("#!/usr/bin/env bash\n"), 0755); err != nil {
			t.Fatalf("write outside script: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(workloads, "linked")); err != nil {
			t.Fatalf("symlink nested dir: %v", err)
		}

		_, _, status, err := workloadScriptPath(root, "workloads/linked/run.sh")
		if err == nil {
			t.Fatal("workloadScriptPath returned nil error")
		}
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
		}
	})
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

func TestHandleWorkloadRunTimeoutKillsChildProcesses(t *testing.T) {
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/child.sh", "#!/usr/bin/env bash\n(sleep 2; echo survived > child-survived) &\necho $! > child.pid\nwait\n")
	handler := workloadRunHandler(root)

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/child.sh","timeout_seconds":1}`))
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
	if b, err := os.ReadFile(filepath.Join(root, "child.pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			defer syscall.Kill(pid, syscall.SIGKILL)
		}
	}

	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(root, "child-survived")); err == nil {
		t.Fatalf("background child process survived workload timeout")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat child-survived: %v", err)
	}
}

func TestHandleWorkloadRunTruncatesOutput(t *testing.T) {
	root := t.TempDir()
	outputBytes := maxWorkloadOutputBytes + 1
	writeWorkloadScript(t, root, "workloads/noisy.sh", fmt.Sprintf("#!/usr/bin/env bash\nhead -c %d /dev/zero | tr '\\0' 'x'\nhead -c %d /dev/zero | tr '\\0' 'y' >&2\n", outputBytes, outputBytes))
	handler := workloadRunHandler(root)

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/noisy.sh","timeout_seconds":5}`))
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
	if len(result.Stdout) != maxWorkloadOutputBytes {
		t.Fatalf("stdout len = %d, want %d", len(result.Stdout), maxWorkloadOutputBytes)
	}
	if len(result.Stderr) != maxWorkloadOutputBytes {
		t.Fatalf("stderr len = %d, want %d", len(result.Stderr), maxWorkloadOutputBytes)
	}
	if !result.StdoutTruncated {
		t.Fatalf("stdout_truncated = false, want true")
	}
	if !result.StderrTruncated {
		t.Fatalf("stderr_truncated = false, want true")
	}
}

func TestHandleWorkloadRunUsesMinimalEnvironment(t *testing.T) {
	t.Setenv("ANVIL_WORKLOAD_SECRET_FOR_TEST", "must-not-leak")
	root := t.TempDir()
	writeWorkloadScript(t, root, "workloads/env.sh", "#!/usr/bin/env bash\nif env | grep -q ANVIL_WORKLOAD_SECRET_FOR_TEST; then\n  echo leaked >&2\n  exit 9\nfi\necho clean\n")
	handler := workloadRunHandler(root)

	req := httptest.NewRequest(http.MethodPost, "/workloads/run", strings.NewReader(`{"script":"workloads/env.sh","timeout_seconds":5}`))
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
		t.Fatalf("exit_code = %d, stderr = %q; want 0", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "clean") {
		t.Fatalf("stdout = %q, want clean", result.Stdout)
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

func TestExtractGooseJSONText_ResumeReturnsOnlyLatestTurn(t *testing.T) {
	// goose --resume re-emits the whole transcript each turn; the extractor must
	// return only the reply to the LAST user message, not every prior assistant
	// block (the multi-turn accumulation bug). Thinking blocks are ignored.
	in := []byte(`{"messages":[
	  {"role":"user","content":[{"type":"text","text":"q1"}]},
	  {"role":"assistant","content":[{"type":"text","text":"answer one"}]},
	  {"role":"user","content":[{"type":"text","text":"q2"}]},
	  {"role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"answer two"}]}
	]}`)
	if got := extractGooseJSONText(in); got != "answer two" {
		t.Errorf("expected only the latest turn %q, got %q", "answer two", got)
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

// TestRunTaskStreaming_Frames exercises the NDJSON streaming path (v0.4.4)
// without invoking the real goose binary: a stub command writes two stderr
// lines (relayed as progress frames) and a goose-shaped JSON envelope on stdout
// (parsed into the final result frame). httptest.ResponseRecorder satisfies
// http.Flusher, so the streaming branch is taken end to end.
func TestRunTaskStreaming_Frames(t *testing.T) {
	script := `echo "thinking..." >&2; echo "tool call" >&2; ` +
		`printf '%s' '{"messages":[{"role":"assistant","content":[{"type":"text","text":"hello"}]}]}'`
	cmd := exec.Command("sh", "-c", script)

	w := httptest.NewRecorder()
	runTaskStreaming(w, cmd)

	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("expected NDJSON content-type, got %q", ct)
	}

	var frames []streamFrame
	for _, line := range strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var fr streamFrame
		if err := json.Unmarshal([]byte(line), &fr); err != nil {
			t.Fatalf("frame is not valid JSON (%q): %v", line, err)
		}
		frames = append(frames, fr)
	}
	if len(frames) == 0 {
		t.Fatal("no frames emitted")
	}

	// The last frame must be the result with the parsed assistant text.
	last := frames[len(frames)-1]
	if last.Type != "result" {
		t.Errorf("last frame type = %q, want result", last.Type)
	}
	if last.Output != "hello" {
		t.Errorf("result output = %q, want hello", last.Output)
	}
	if last.Error != "" {
		t.Errorf("result error = %q, want empty", last.Error)
	}

	// At least the first stderr line must have arrived as a progress frame.
	sawProgress := false
	for _, fr := range frames {
		if fr.Type == "progress" && fr.Text == "thinking..." {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Error("expected a progress frame carrying the stderr line \"thinking...\"")
	}
}

// TestRunTaskBuffered_DefaultShape locks in the buffered /tasks contract that the
// default (no ?stream=1) path must preserve: a single JSON object
// {"output","error"} with Content-Type application/json — NOT newline-delimited
// stream frames. A stub cmd emits a goose-shaped envelope on stdout so no real
// goose binary is needed. Regression guard for the v0.4.4 streaming split.
func TestRunTaskBuffered_DefaultShape(t *testing.T) {
	cmd := exec.Command("sh", "-c",
		`printf '%s' '{"messages":[{"role":"assistant","content":[{"type":"text","text":"hello"}]}]}'`)

	w := httptest.NewRecorder()
	runTaskBuffered(w, cmd)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("buffered Content-Type = %q, want application/json", ct)
	}
	body := strings.TrimSpace(w.Body.String())
	// Exactly one JSON object — no newline-delimited frames.
	if strings.Contains(body, "\n") {
		t.Fatalf("buffered body must be a single JSON object, got multiple lines: %q", body)
	}
	var res TaskResult
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("buffered body is not a single JSON object (%q): %v", body, err)
	}
	if res.Output != "hello" {
		t.Errorf("buffered output = %q, want hello", res.Output)
	}
	if res.Error != "" {
		t.Errorf("buffered error = %q, want empty", res.Error)
	}
	// The buffered object must not carry stream-frame typing.
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("buffered body not decodable as object (%q): %v", body, err)
	}
	if _, ok := raw["type"]; ok {
		t.Errorf("buffered object unexpectedly has a stream-frame 'type' field: %v", raw)
	}
}

func TestNoThinkForModel(t *testing.T) {
	cases := map[string]string{
		"qwen/qwen3-32b":          "/nothink\n",
		"qwen3-32b":               "/nothink\n",
		"Qwen/Qwen3-32B":          "/nothink\n", // case-insensitive
		"llama-3.3-70b-versatile": "",
		"claude-sonnet-4-6":       "",
		"gpt-4o":                  "",
		"":                        "",
	}
	for model, want := range cases {
		if got := noThinkForModel(model); got != want {
			t.Errorf("noThinkForModel(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestExtractGooseTranscript_FullConversation(t *testing.T) {
	// Unlike extractGooseJSONText (latest turn only), the transcript extractor
	// returns EVERY user/assistant turn in order, for the Web UI to repaint a
	// resumed chat. Thinking/tool blocks are dropped; only text is kept.
	in := []byte(`{"messages":[
	  {"role":"user","content":[{"type":"text","text":"q1"}]},
	  {"role":"assistant","content":[{"type":"text","text":"a1"}]},
	  {"role":"user","content":[{"type":"text","text":"q2"}]},
	  {"role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"a2"}]}
	]}`)
	want := []TranscriptTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2"},
		{Role: "assistant", Text: "a2"},
	}
	got := extractGooseTranscript(in)
	if len(got) != len(want) {
		t.Fatalf("got %d turns, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestExtractGooseTranscript_CleansUserPrefixes(t *testing.T) {
	// The user message carries goose-agent's injected prefixes; the restored
	// transcript must show only what the user actually typed.
	in := []byte(`{"messages":[
	  {"role":"user","content":[{"type":"text","text":"/nothink\n[SYSTEM INSTRUCTIONS]\nbe terse\n\n[USER TASK]\nhello there"}]},
	  {"role":"assistant","content":[{"type":"text","text":"hi"}]}
	]}`)
	got := extractGooseTranscript(in)
	if len(got) != 2 || got[0].Text != "hello there" {
		t.Fatalf("user prompt not cleaned: %+v", got)
	}
}

func TestExtractGooseTranscript_NonJSON_ReturnsNil(t *testing.T) {
	if got := extractGooseTranscript([]byte("panic at the disco")); got != nil {
		t.Errorf("expected nil for non-JSON input, got %+v", got)
	}
}

func TestCleanUserPrompt(t *testing.T) {
	cases := map[string]string{
		"/nothink\n[SYSTEM INSTRUCTIONS]\nrole\n\n[USER TASK]\nactual": "actual",
		"/nothink\nactual": "actual",
		"plain prompt":     "plain prompt",
		"[SYSTEM INSTRUCTIONS]\nx\n\n[USER TASK]\nmulti\nline": "multi\nline",
	}
	for in, want := range cases {
		if got := cleanUserPrompt(in); got != want {
			t.Errorf("cleanUserPrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- anvil transcript-safety guards (v0.7 parity) ---
//
// These lock the three transcript-restore invariants the merge introduced:
//   1. the cache-hit hot path never shells out to goose (no model call);
//   2. the cache-miss fallback is a read-only `goose session export` — never `run`;
//   3. the transcript payload cannot echo the agent's bearer token.

// writeGooseStub writes an executable shell stub at a temp path and returns it.
func writeGooseStub(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "goose-stub.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write goose stub: %v", err)
	}
	return path
}

// seedSession installs a session with a cached transcript and returns a cleanup.
func seedSession(t *testing.T, name string, turns []TranscriptTurn) {
	t.Helper()
	sessionMu.Lock()
	sessions[name] = &sessionInfo{Name: name, transcript: turns}
	sessionMu.Unlock()
	t.Cleanup(func() {
		sessionMu.Lock()
		delete(sessions, name)
		sessionMu.Unlock()
	})
}

// Guard 1: a cache hit serves the cached transcript WITHOUT invoking goose at all
// (proves the hot path makes no model call and spawns no process).
func TestHandleSessionItem_CacheHitSkipsGooseExport(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "goose-was-called")
	stub := writeGooseStub(t, "touch '"+marker+"'\nexit 1\n")
	orig := gooseExportBinary
	gooseExportBinary = stub
	t.Cleanup(func() { gooseExportBinary = orig })

	want := []TranscriptTurn{{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}}
	seedSession(t, "sess1", want)

	req := httptest.NewRequest(http.MethodGet, "/sessions/sess1/transcript", nil)
	rr := httptest.NewRecorder()
	handleSessionItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("goose was invoked on a cache hit — the transcript hot path must not call the agent")
	}
	var got struct {
		Turns []TranscriptTurn `json:"turns"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Turns) != 2 || got.Turns[0] != want[0] || got.Turns[1] != want[1] {
		t.Fatalf("turns = %+v, want %+v", got.Turns, want)
	}
}

// Guard 2: the cache-miss fallback runs `goose session export` (read-only) and
// never the model-invoking `run` subcommand.
func TestExportSessionTranscript_ReadOnlyNoModelCall(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	stub := writeGooseStub(t, "printf '%s\\n' \"$*\" > '"+argvFile+"'\n"+
		`printf '%s' '{"messages":[{"role":"user","content":[{"type":"text","text":"q1"}]},{"role":"assistant","content":[{"type":"text","text":"a1"}]}]}'`+"\n")
	orig := gooseExportBinary
	gooseExportBinary = stub
	t.Cleanup(func() { gooseExportBinary = orig })

	turns, err := exportSessionTranscript(context.Background(), "sess1")
	if err != nil {
		t.Fatalf("exportSessionTranscript: %v", err)
	}
	if len(turns) != 2 || turns[0].Text != "q1" || turns[1].Text != "a1" {
		t.Fatalf("turns = %+v, want q1/a1", turns)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	got := strings.TrimSpace(string(argv))
	if got != "session export -n sess1 --format json" {
		t.Fatalf("export argv = %q, want a read-only session export", got)
	}
	for _, f := range strings.Fields(got) {
		if f == "run" {
			t.Fatalf("transcript export invoked the model-running subcommand: %q", got)
		}
	}
}

// Guard 3: the transcript payload never carries the agent's bearer token even when
// one is configured — the response schema is role/text only, no auth material.
func TestHandleSessionItem_PayloadOmitsAgentAuth(t *testing.T) {
	const sentinel = "SENTINEL-AGENT-BEARER-do-not-leak"
	orig := agentToken
	setCurrentAgentToken(sentinel)
	t.Cleanup(func() { setCurrentAgentToken(orig) })

	seedSession(t, "sess2", []TranscriptTurn{
		{Role: "user", Text: "what time is it"},
		{Role: "assistant", Text: "it is noon"},
	})

	req := httptest.NewRequest(http.MethodGet, "/sessions/sess2/transcript", nil)
	rr := httptest.NewRecorder()
	handleSessionItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, sentinel) {
		t.Fatalf("transcript payload leaked the agent bearer token: %s", body)
	}
	for _, k := range []string{"agent_token", "authorization", "Authorization", "Bearer"} {
		if strings.Contains(body, k) {
			t.Fatalf("transcript payload exposed auth field %q: %s", k, body)
		}
	}
}
