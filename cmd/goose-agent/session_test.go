package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGooseArgs(t *testing.T) {
	cases := []struct {
		name    string
		session string
		resume  bool
		want    []string
	}{
		{"no session (stateless)", "", false, []string{"run", "--output-format", "json", "--no-profile", "--with-builtin", "developer", "-i", "-"}},
		{"session first turn", "vm-1", false, []string{"run", "--output-format", "json", "--no-profile", "--with-builtin", "developer", "-n", "vm-1", "-i", "-"}},
		{"session resume", "vm-1", true, []string{"run", "--output-format", "json", "--no-profile", "--with-builtin", "developer", "-n", "vm-1", "--resume", "-i", "-"}},
	}
	for _, c := range cases {
		if got := gooseArgs(c.session, c.resume); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: gooseArgs(%q,%v) = %v, want %v", c.name, c.session, c.resume, got, c.want)
		}
	}
}

func TestValidSessionName(t *testing.T) {
	for _, s := range []string{"vm-1", "vm-1780285947214003861-1717200000000", "abc_DEF-123"} {
		if !validSessionName(s) {
			t.Errorf("validSessionName(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "a b", "a/b", "../x", "a;b", "a\nb", string(make([]byte, 65))} {
		if validSessionName(s) {
			t.Errorf("validSessionName(%q) = true, want false", s)
		}
	}
}

func TestSessionTitle(t *testing.T) {
	if got := sessionTitle("  build a web server\n "); got != "build a web server" {
		t.Errorf("trim/flatten: got %q", got)
	}
	if got := sessionTitle("line1\nline2"); got != "line1 line2" {
		t.Errorf("newline flatten: got %q", got)
	}
	// Rune-safe truncation: 70 Korean runes → 60 runes + ellipsis (not a byte cut).
	long := strings.Repeat("가", 70)
	got := sessionTitle(long)
	if r := []rune(got); len(r) != 61 || r[60] != '…' {
		t.Errorf("truncation: got %d runes (want 61 incl. ellipsis)", len(r))
	}
}

func TestHandleSessions(t *testing.T) {
	sessionMu.Lock()
	sessions = map[string]*sessionInfo{
		"s-old": {Name: "s-old", CreatedAt: time.Unix(100, 0), Title: "old", Turns: 2},
		"s-new": {Name: "s-new", CreatedAt: time.Unix(200, 0), Title: "new", Turns: 1},
	}
	sessionMu.Unlock()
	defer func() { sessionMu.Lock(); sessions = map[string]*sessionInfo{}; sessionMu.Unlock() }()

	rec := httptest.NewRecorder()
	handleSessions(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var list []sessionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(list))
	}
	// Newest first.
	if list[0].Name != "s-new" || list[1].Name != "s-old" {
		t.Errorf("expected newest-first order, got %s then %s", list[0].Name, list[1].Name)
	}
}

func TestHandleSessions_RejectsNonGET(t *testing.T) {
	rec := httptest.NewRecorder()
	handleSessions(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}
