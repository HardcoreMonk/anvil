package network

import "testing"

// TestAntiSpoofEnabledFromEnv_DefaultOnAndOptOut is an anvil guard for the
// v0.6.1 hardening rule: "EPHEMERA_NET_ANTISPOOF default on, best-effort disable
// if ebtables missing." This locks the env contract — anti-spoof is ON unless an
// operator explicitly opts out with a falsey value — so a future refactor cannot
// silently flip the default to off (which would re-open the source-IP spoofing
// surface the gateway trusts for caller identity).
func TestAntiSpoofEnabledFromEnv_DefaultOnAndOptOut(t *testing.T) {
	// Unset => enabled (default on).
	t.Setenv("EPHEMERA_NET_ANTISPOOF", "")
	if !antiSpoofEnabledFromEnv() {
		t.Fatal("EPHEMERA_NET_ANTISPOOF unset must default to enabled")
	}

	// Only these values (any case, surrounding space tolerated) opt out.
	for _, off := range []string{"0", "false", "no", "off", "FALSE", "Off", " no "} {
		t.Setenv("EPHEMERA_NET_ANTISPOOF", off)
		if antiSpoofEnabledFromEnv() {
			t.Errorf("EPHEMERA_NET_ANTISPOOF=%q must disable anti-spoof", off)
		}
	}

	// Anything else keeps anti-spoof enabled (fail closed toward the secure default).
	for _, on := range []string{"1", "true", "yes", "on", "TRUE", "enabled", "garbage"} {
		t.Setenv("EPHEMERA_NET_ANTISPOOF", on)
		if !antiSpoofEnabledFromEnv() {
			t.Errorf("EPHEMERA_NET_ANTISPOOF=%q must keep anti-spoof enabled", on)
		}
	}
}

// TestEbtablesAvailable_NoPanic is a light anvil guard for the best-effort
// PATH probe. It cannot assert a fixed result (that depends on the host), but it
// pins that the probe is a pure, side-effect-free bool used by NewManager to
// disable anti-spoof when ebtables is absent rather than crashing.
func TestEbtablesAvailable_NoPanic(t *testing.T) {
	_ = ebtablesAvailable()
}
