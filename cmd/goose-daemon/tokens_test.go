package main

import (
	"testing"
	"time"
)

func TestParseAPIClients_TTL(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name      string
		raw       string
		wantName  string
		wantTok   string
		wantNever bool // Expires.IsZero()
	}{
		{"two-field never expires", "alice:tokenA", "alice", "tokenA", true},
		{"three-field rfc3339", "bob:tokenB:" + future, "bob", "tokenB", false},
		{"colon-in-token no expiry", "carol:tok:en", "carol", "tok:en", true},
		{"colon-in-token with expiry", "dave:tok:en:" + future, "dave", "tok:en", false},
		{"trailing non-timestamp kept as token", "erin:secret:xyz", "erin", "secret:xyz", true},
		{"short numeric tail kept as token", "frank:secret:42", "frank", "secret:42", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := parseAPIClients(c.raw)
			if len(cl) != 1 {
				t.Fatalf("expected 1 client, got %d (%+v)", len(cl), cl)
			}
			if cl[0].Name != c.wantName || cl[0].Token != c.wantTok {
				t.Errorf("got name=%q token=%q, want name=%q token=%q", cl[0].Name, cl[0].Token, c.wantName, c.wantTok)
			}
			if cl[0].Expires.IsZero() != c.wantNever {
				t.Errorf("Expires.IsZero()=%v, want %v (expires=%v)", cl[0].Expires.IsZero(), c.wantNever, cl[0].Expires)
			}
		})
	}
}

func TestParseExpiry(t *testing.T) {
	if _, ok := parseExpiry("2026-06-01T00:00:00Z"); !ok {
		t.Error("RFC3339 should parse")
	}
	if _, ok := parseExpiry("1750000000"); !ok {
		t.Error("plausible unix seconds should parse")
	}
	if _, ok := parseExpiry("42"); ok {
		t.Error("small integer must NOT be treated as expiry")
	}
	if _, ok := parseExpiry("nottime"); ok {
		t.Error("non-timestamp must not parse")
	}
}

func TestFirstActiveClient(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	if c, ok := firstActiveClient([]APIClient{{Name: "a", Token: "ta"}, {Name: "b"}}); !ok || c.Name != "a" {
		t.Errorf("all active: want a, got ok=%v name=%q", ok, c.Name)
	}
	if c, ok := firstActiveClient([]APIClient{{Name: "a", Expires: past}, {Name: "b", Expires: future}}); !ok || c.Name != "b" {
		t.Errorf("first expired: want b, got ok=%v name=%q", ok, c.Name)
	}
	if _, ok := firstActiveClient([]APIClient{{Name: "a", Expires: past}}); ok {
		t.Error("all expired: want ok=false")
	}
	if _, ok := firstActiveClient(nil); ok {
		t.Error("empty: want ok=false")
	}
}

func TestAPIClientExpiredAtBoundary(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	if apiClientExpired(APIClient{Name: "boundary", Expires: now}, now) != true {
		t.Fatal("Expires == now must be expired")
	}
	if apiClientExpired(APIClient{Name: "future", Expires: now.Add(time.Nanosecond)}, now) != false {
		t.Fatal("Expires after now must still be active")
	}
	if apiClientExpired(APIClient{Name: "never"}, now) != false {
		t.Fatal("zero Expires must never expire")
	}
}

func TestCountTokenExpiry(t *testing.T) {
	now := time.Now()
	clients := []APIClient{
		{Name: "never"},
		{Name: "expired", Expires: now.Add(-time.Hour)},
		{Name: "soon", Expires: now.Add(time.Hour)},
		{Name: "later", Expires: now.Add(48 * time.Hour)},
	}
	expired, soon := countTokenExpiry(clients)
	if expired != 1 || soon != 1 {
		t.Errorf("expired=%d soon=%d, want 1,1", expired, soon)
	}
}
