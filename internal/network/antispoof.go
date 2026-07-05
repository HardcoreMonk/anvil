package network

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// antiSpoofChain is the dedicated ebtables filter chain holding the per-TAP
// source MAC/IP pins. Keeping our rules in one chain (jumped to from FORWARD and
// INPUT) isolates them from any other ebtables usage and makes startup cleanup a
// single flush.
const antiSpoofChain = "EPHEMERA_AS"

// antiSpoofEnabledFromEnv reports whether per-TAP anti-spoof is on. It defaults
// to enabled; EPHEMERA_NET_ANTISPOOF set to a falsey value opts out.
func antiSpoofEnabledFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EPHEMERA_NET_ANTISPOOF"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// ebtablesAvailable reports whether the ebtables binary is on PATH.
func ebtablesAvailable() bool {
	_, err := exec.LookPath("ebtables")
	return err == nil
}

// setupAntiSpoofChain creates (if absent) and flushes the EPHEMERA_AS chain, then
// idempotently wires jumps into it from FORWARD and INPUT. Flushing at startup
// drops stale per-TAP rules left by a prior daemon run — the bridge persists
// across restarts, but the daemon's VM view is rebuilt, so recycled-TAP rules
// would otherwise accumulate and mis-pin. Best-effort: failures are logged,
// never fatal (matching setupBridge).
func (m *Manager) setupAntiSpoofChain() {
	// Create the chain only if it does not already exist (-N errors otherwise).
	if exec.Command("ebtables", "-t", "filter", "-L", antiSpoofChain).Run() != nil {
		if err := exec.Command("ebtables", "-t", "filter", "-N", antiSpoofChain).Run(); err != nil {
			slog.Warn("anti-spoof chain create failed", "chain", antiSpoofChain, "err", err)
		}
	}
	// Flush stale per-TAP rules from a prior run.
	exec.Command("ebtables", "-t", "filter", "-F", antiSpoofChain).Run()
	// Idempotently jump into the chain from FORWARD and INPUT (-C checks, -A adds).
	for _, base := range []string{"FORWARD", "INPUT"} {
		if exec.Command("ebtables", "-t", "filter", "-C", base, "-j", antiSpoofChain).Run() != nil {
			if err := exec.Command("ebtables", "-t", "filter", "-A", base, "-j", antiSpoofChain).Run(); err != nil {
				slog.Warn("anti-spoof jump install failed", "from", base, "chain", antiSpoofChain, "err", err)
			}
		}
	}
}

// antiSpoofRuleSets returns the ebtables rule specifications (without the
// -t filter -A/-C/-D prefix) that pin a TAP's bridge port to its assigned MAC
// and IP. ARP is allowed when the source MAC matches — without it the guest
// cannot resolve the gateway and all connectivity breaks; IPv4 is allowed only
// with the matching source MAC AND source IP; anything else from the port is
// dropped as spoofed. The trailing per-TAP DROP is scoped by -i, so other taps
// and host traffic are unaffected (the chain policy stays ACCEPT).
func antiSpoofRuleSets(tap, ip, mac string) [][]string {
	return [][]string{
		{"-p", "ARP", "-i", tap, "-s", mac, "-j", "ACCEPT"},
		{"-p", "IPv4", "-i", tap, "-s", mac, "--ip-src", ip, "-j", "ACCEPT"},
		{"-i", tap, "-j", "DROP"},
	}
}

// addAntiSpoofRules pins tap to (ip, mac) in the EPHEMERA_AS chain. No-op when
// anti-spoof is disabled. Each rule is added idempotently (-C check before -A).
func (m *Manager) addAntiSpoofRules(tap, ip, mac string) {
	if !m.antiSpoof {
		return
	}
	for _, rule := range antiSpoofRuleSets(tap, ip, mac) {
		check := append([]string{"-t", "filter", "-C", antiSpoofChain}, rule...)
		if exec.Command("ebtables", check...).Run() != nil {
			add := append([]string{"-t", "filter", "-A", antiSpoofChain}, rule...)
			if err := exec.Command("ebtables", add...).Run(); err != nil {
				slog.Warn("anti-spoof rule add failed", "tap", tap, "err", err)
			}
		}
	}
	slog.Info("anti-spoof rules added", "tap", tap, "ip", ip, "mac", mac)
}

// removeAntiSpoofRules deletes tap's pinning rules from EPHEMERA_AS by exact
// match. No-op when anti-spoof is disabled. Exact-match removal (not a tap-name
// wildcard) is what keeps a recycled tap/IP from inheriting a stale pin.
func (m *Manager) removeAntiSpoofRules(tap, ip, mac string) {
	if !m.antiSpoof {
		return
	}
	for _, rule := range antiSpoofRuleSets(tap, ip, mac) {
		del := append([]string{"-t", "filter", "-D", antiSpoofChain}, rule...)
		exec.Command("ebtables", del...).Run()
	}
}
