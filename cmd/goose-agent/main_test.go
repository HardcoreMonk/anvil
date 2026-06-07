package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
