package main

import (
	"errors"
	"os"
	"testing"
)

func TestLoadAPIClients_Empty(t *testing.T) {
	os.Unsetenv("EPHEMERA_API_TOKENS")
	os.Unsetenv("EPHEMERA_API_TOKEN")
	if got := loadAPIClients(); len(got) != 0 {
		t.Errorf("expected 0 clients, got %d", len(got))
	}
}

func TestLoadAPIClients_SingleToken(t *testing.T) {
	os.Unsetenv("EPHEMERA_API_TOKENS")
	os.Setenv("EPHEMERA_API_TOKEN", "tok1")
	defer os.Unsetenv("EPHEMERA_API_TOKEN")

	clients := loadAPIClients()
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].Name != "default" || clients[0].Token != "tok1" {
		t.Errorf("unexpected client: %+v", clients[0])
	}
}

func TestLoadAPIClients_MultiToken(t *testing.T) {
	os.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA,bob:tokenB")
	defer os.Unsetenv("EPHEMERA_API_TOKENS")

	clients := loadAPIClients()
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
	if clients[0].Name != "alice" || clients[0].Token != "tokenA" {
		t.Errorf("unexpected client[0]: %+v", clients[0])
	}
	if clients[1].Name != "bob" || clients[1].Token != "tokenB" {
		t.Errorf("unexpected client[1]: %+v", clients[1])
	}
}

func TestLoadAPIClients_MultiTokenTakesPrecedence(t *testing.T) {
	os.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA")
	os.Setenv("EPHEMERA_API_TOKEN", "single")
	defer os.Unsetenv("EPHEMERA_API_TOKENS")
	defer os.Unsetenv("EPHEMERA_API_TOKEN")

	clients := loadAPIClients()
	if len(clients) != 1 || clients[0].Name != "alice" {
		t.Errorf("EPHEMERA_API_TOKENS should take precedence, got: %+v", clients)
	}
}

func TestLoadAPIClients_MalformedEntrySkipped(t *testing.T) {
	os.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA,malformed,bob:tokenB")
	defer os.Unsetenv("EPHEMERA_API_TOKENS")

	clients := loadAPIClients()
	if len(clients) != 2 {
		t.Errorf("expected 2 valid clients (malformed entry skipped), got %d", len(clients))
	}
}

func TestResolveAPIAddr_Default(t *testing.T) {
	os.Unsetenv("EPHEMERA_API_ADDR")
	os.Unsetenv("EPHEMERA_API_PORT")
	if got := resolveAPIAddr(); got != "127.0.0.1:3000" {
		t.Errorf("expected 127.0.0.1:3000, got %q", got)
	}
}

func TestResolveAPIAddr_FromAddr(t *testing.T) {
	os.Setenv("EPHEMERA_API_ADDR", "0.0.0.0:8080")
	defer os.Unsetenv("EPHEMERA_API_ADDR")
	if got := resolveAPIAddr(); got != "0.0.0.0:8080" {
		t.Errorf("expected 0.0.0.0:8080, got %q", got)
	}
}

func TestResolveAPIAddr_FromPort(t *testing.T) {
	os.Unsetenv("EPHEMERA_API_ADDR")
	os.Setenv("EPHEMERA_API_PORT", "9090")
	defer os.Unsetenv("EPHEMERA_API_PORT")
	if got := resolveAPIAddr(); got != "127.0.0.1:9090" {
		t.Errorf("expected 127.0.0.1:9090, got %q", got)
	}
}

// TestEnvBool_Autosnapshot documents the EPHEMERA_AUTOSNAPSHOT (v0.4.0) mapping.
// It exercises envBool directly rather than the enableAutoSnapshot package var,
// which is captured once at init and would not reflect env changes made here.
func TestEnvBool_Autosnapshot(t *testing.T) {
	const key = "EPHEMERA_AUTOSNAPSHOT"
	cases := []struct {
		name string
		val  string
		set  bool
		want bool
	}{
		{name: "unset defaults off", set: false, want: false},
		{name: "empty defaults off", val: "", set: true, want: false},
		{name: "true", val: "true", set: true, want: true},
		{name: "1", val: "1", set: true, want: true},
		{name: "yes", val: "yes", set: true, want: true},
		{name: "on", val: "on", set: true, want: true},
		{name: "false", val: "false", set: true, want: false},
		{name: "off", val: "off", set: true, want: false},
		{name: "garbage defaults off", val: "garbage", set: true, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				os.Setenv(key, c.val)
				defer os.Unsetenv(key)
			} else {
				os.Unsetenv(key)
			}
			if got := envBool(key, false); got != c.want {
				t.Errorf("envBool(%q=%q) = %v, want %v", key, c.val, got, c.want)
			}
		})
	}
}

// TestResolveDiskModeCOW covers the COW-by-default decision (v0.4.2): unset/"cow"
// resolves to COW when the host probe passes, falls back to plain when it fails,
// and "plain"/"full" force plain regardless of the probe.
func TestResolveDiskModeCOW(t *testing.T) {
	const key = "EPHEMERA_DISK_MODE"
	okProbe := func() error { return nil }
	errProbe := func() error { return errors.New("no dm-snapshot") }
	cases := []struct {
		name  string
		val   string
		set   bool
		probe func() error
		want  bool
	}{
		{name: "unset + probe ok -> cow", set: false, probe: okProbe, want: true},
		{name: "cow + probe ok -> cow", val: "cow", set: true, probe: okProbe, want: true},
		{name: "unset + probe fails -> fallback plain", set: false, probe: errProbe, want: false},
		{name: "cow + probe fails -> fallback plain", val: "cow", set: true, probe: errProbe, want: false},
		{name: "plain forces plain", val: "plain", set: true, probe: okProbe, want: false},
		{name: "full forces plain", val: "full", set: true, probe: okProbe, want: false},
		{name: "PLAIN case-insensitive", val: "PLAIN", set: true, probe: okProbe, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				os.Setenv(key, c.val)
				defer os.Unsetenv(key)
			} else {
				os.Unsetenv(key)
			}
			if got := resolveDiskModeCOW(c.probe); got != c.want {
				t.Errorf("resolveDiskModeCOW(%q=%q) = %v, want %v", key, c.val, got, c.want)
			}
		})
	}
}

// TestResolveDiskModeCOW_OptOutSkipsProbe verifies the plain/full opt-out
// short-circuits before the (potentially expensive) host probe runs.
func TestResolveDiskModeCOW_OptOutSkipsProbe(t *testing.T) {
	const key = "EPHEMERA_DISK_MODE"
	os.Setenv(key, "plain")
	defer os.Unsetenv(key)

	probed := false
	got := resolveDiskModeCOW(func() error { probed = true; return nil })
	if got {
		t.Errorf("plain should resolve to false, got %v", got)
	}
	if probed {
		t.Error("probe must not run when EPHEMERA_DISK_MODE=plain")
	}
}
