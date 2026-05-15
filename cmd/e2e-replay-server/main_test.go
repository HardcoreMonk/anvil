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

func TestSanitizeRecordingStripsTerminalNoise(t *testing.T) {
	input := strings.Join([]string{
		`Script started on 2026-05-12 [COMMAND="/tmp/run"]`,
		"$ ironclaw --version\r",
		"\x1b[1A\x1b[J  O Running anvil_anvil_spawn_vm...",
		"",
		"",
		`Script done on 2026-05-12 [COMMAND_EXIT_CODE="0"]`,
	}, "\n")

	got := sanitizeRecording(input)
	want := []string{
		"$ ironclaw --version",
		"  O Running anvil_anvil_spawn_vm...",
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sanitizeRecording() = %#v, want %#v", got, want)
	}
}

func TestSanitizeRecordingRedactsSecrets(t *testing.T) {
	input := strings.Join([]string{
		`GOOGLE_API_KEY: "AIzaSyADAimcwTU-OEE_qiIOw5QdB5SuO2ZRFvQ"`,
		`Authorization: Bearer abc.def.ghi`,
		`"agent_token":"secret-token-value"`,
	}, "\n")

	got := strings.Join(sanitizeRecording(input), "\n")

	for _, forbidden := range []string{
		"AIzaSyADAimcwTU-OEE_qiIOw5QdB5SuO2ZRFvQ",
		"abc.def.ghi",
		"secret-token-value",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitizeRecording leaked %q in %q", forbidden, got)
		}
	}

	for _, expected := range []string{
		"GOOGLE_API_KEY: \"<redacted>\"",
		"Bearer <redacted>",
		`"agent_token":"<redacted>"`,
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("sanitizeRecording missing %q in %q", expected, got)
		}
	}
}

func TestPlaylistHandlerReportsAvailableAndMissingReplays(t *testing.T) {
	dir := t.TempDir()
	fullPath := filepath.Join(dir, "full-kvm.txt")
	if err := os.WriteFile(fullPath, []byte("PASS  1. Start daemon\nAll test steps passed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newMux([]replayEntry{
		{ID: "full-kvm-e2e", Title: "Full KVM E2E", Path: fullPath},
		{ID: "ironclaw-e2e", Title: "IronClaw E2E", Path: filepath.Join(dir, "missing.typescript")},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/playlist", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp playlistResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(resp.Items))
	}
	if !resp.Items[0].Available || resp.Items[0].LineCount != 2 {
		t.Fatalf("full replay availability = %#v", resp.Items[0])
	}
	if resp.Items[1].Available || resp.Items[1].LineCount != 0 {
		t.Fatalf("missing replay availability = %#v", resp.Items[1])
	}
}

func TestRecordingHandlerServesSelectedReplay(t *testing.T) {
	dir := t.TempDir()
	fullPath := filepath.Join(dir, "full-kvm.txt")
	ironPath := filepath.Join(dir, "ironclaw.typescript")
	if err := os.WriteFile(fullPath, []byte("PASS  full KVM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ironPath, []byte("PASS  ironclaw\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := newMux([]replayEntry{
		{ID: "full-kvm-e2e", Title: "Full KVM E2E", Path: fullPath},
		{ID: "ironclaw-e2e", Title: "IronClaw E2E", Path: ironPath},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/recording?id=ironclaw-e2e", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var resp recordingResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != "ironclaw-e2e" {
		t.Fatalf("ID = %q, want ironclaw-e2e", resp.ID)
	}
	if got := strings.Join(resp.Lines, "\n"); got != "PASS  ironclaw" {
		t.Fatalf("Lines = %q", got)
	}
}

func TestRecordingHandlerRejectsUnknownReplayID(t *testing.T) {
	handler := newMux([]replayEntry{{ID: "full-kvm-e2e", Title: "Full KVM E2E", Path: "missing"}})

	req := httptest.NewRequest(http.MethodGet, "/api/recording?id=bad", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestSummarizeRecordingCoversFullKVMReplay(t *testing.T) {
	lines := []string{
		"PASS  12. Take snapshot of snap-source VM (stop_after=true)",
		"PASS  38. Restore from snapshot (COW path)",
		"PASS  52. Create flock with 5 agents",
		"PASS  Flock spawn response omits agent_token and agent_tokens",
		"PASS  Watchdog start log present across daemon restarts",
		"All test steps passed",
	}

	got := strings.Join(summarizeRecording(lines), "\n")
	for _, want := range []string{
		"snapshot",
		"COW",
		"Goosetown",
		"agent_token redaction",
		"watchdog",
		"all steps passed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q in %q", want, got)
		}
	}
}
