package sni

import "testing"

func TestMatcherExactAndWildcard(t *testing.T) {
	m, err := NewMatcher([]string{"api.anthropic.com", "*.example.com"})
	if err != nil {
		t.Fatalf("NewMatcher error = %v", err)
	}
	cases := []struct {
		in   string
		want bool
	}{
		{"api.anthropic.com", true},       // exact
		{"API.Anthropic.COM", true},       // case-insensitive
		{"anthropic.com", false},          // exact does not match parent
		{"evil-api.anthropic.com", false}, // exact is not a suffix rule
		{"a.example.com", true},           // wildcard one label
		{"a.b.example.com", true},         // wildcard multiple labels
		{"example.com", false},            // "*." requires >=1 left label
		{"notexample.com", false},         // suffix must be on a label boundary
		{"", false},
	}
	for _, c := range cases {
		if got := m.Match(c.in); got != c.want {
			t.Fatalf("Match(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMatcherEmpty(t *testing.T) {
	m, err := NewMatcher(nil)
	if err != nil {
		t.Fatalf("NewMatcher(nil) error = %v", err)
	}
	if !m.Empty() {
		t.Fatal("Empty() = false, want true for no patterns")
	}
	if m.Match("api.anthropic.com") {
		t.Fatal("empty matcher matched a host (must default-deny)")
	}
}

func TestNewMatcherRejectsBadWildcard(t *testing.T) {
	for _, bad := range []string{"*", "*example.com", "a.*.com", "**.example.com", "ex*mple.com"} {
		if _, err := NewMatcher([]string{bad}); err == nil {
			t.Fatalf("NewMatcher(%q) error = nil, want rejection", bad)
		}
	}
}
