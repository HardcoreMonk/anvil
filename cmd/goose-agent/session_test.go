package main

import (
	"reflect"
	"testing"
)

func TestGooseArgs(t *testing.T) {
	cases := []struct {
		name    string
		session string
		resume  bool
		want    []string
	}{
		{"no session (stateless)", "", false, []string{"run", "--output-format", "json", "-i", "-"}},
		{"session first turn", "vm-1", false, []string{"run", "--output-format", "json", "-n", "vm-1", "-i", "-"}},
		{"session resume", "vm-1", true, []string{"run", "--output-format", "json", "-n", "vm-1", "--resume", "-i", "-"}},
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
