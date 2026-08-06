package network

import (
	"errors"
	"reflect"
	"testing"
)

func TestSetupBridgeAddsForwardAcceptRules(t *testing.T) {
	m := &Manager{
		gatewayIP:  "10.0.1.1",
		subnet:     "10.0.1.",
		bridgeName: "goose-br0",
	}

	var commands [][]string
	m.runCommand = func(name string, args ...string) error {
		command := append([]string{name}, args...)
		commands = append(commands, command)
		if name == "iptables" && len(args) >= 3 && args[0] == "-C" {
			return errors.New("missing rule")
		}
		if name == "iptables" && len(args) >= 5 && args[0] == "-t" && args[1] == "nat" && args[2] == "-C" {
			return errors.New("missing rule")
		}
		return nil
	}

	if err := m.setupBridge(); err != nil {
		t.Fatalf("setupBridge: %v", err)
	}

	wantOutbound := []string{"iptables", "-I", "FORWARD", "-i", "goose-br0", "-s", "10.0.1.0/24", "-j", "ACCEPT"}
	wantReturn := []string{"iptables", "-I", "FORWARD", "-o", "goose-br0", "-d", "10.0.1.0/24", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"}
	for _, want := range [][]string{wantOutbound, wantReturn} {
		if !hasCommand(commands, want) {
			t.Fatalf("missing command %v in %v", want, commands)
		}
	}
}

func TestAllowBridgeHostPortAddsInputAcceptRule(t *testing.T) {
	m := &Manager{
		gatewayIP:  "10.0.1.1",
		subnet:     "10.0.1.",
		bridgeName: "goose-br0",
	}

	var commands [][]string
	m.runCommand = func(name string, args ...string) error {
		command := append([]string{name}, args...)
		commands = append(commands, command)
		if name == "iptables" && len(args) >= 3 && args[0] == "-C" {
			return errors.New("missing rule")
		}
		return nil
	}

	if err := m.AllowBridgeHostPort(3000); err != nil {
		t.Fatalf("AllowBridgeHostPort: %v", err)
	}

	wantCheck := []string{"iptables", "-C", "INPUT", "-i", "goose-br0", "-s", "10.0.1.0/24", "-d", "10.0.1.1", "-p", "tcp", "--dport", "3000", "-j", "ACCEPT", "-m", "comment", "--comment", "anvil-cp-callback"}
	wantInsert := []string{"iptables", "-I", "INPUT", "-i", "goose-br0", "-s", "10.0.1.0/24", "-d", "10.0.1.1", "-p", "tcp", "--dport", "3000", "-j", "ACCEPT", "-m", "comment", "--comment", "anvil-cp-callback"}
	for _, want := range [][]string{wantCheck, wantInsert} {
		if !hasCommand(commands, want) {
			t.Fatalf("missing command %v in %v", want, commands)
		}
	}
}

func TestAllowBridgeHostPortDoesNotDuplicateExistingRule(t *testing.T) {
	m := &Manager{
		gatewayIP:  "10.0.1.1",
		subnet:     "10.0.1.",
		bridgeName: "goose-br0",
	}

	var commands [][]string
	m.runCommand = func(name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}

	if err := m.AllowBridgeHostPort(3000); err != nil {
		t.Fatalf("AllowBridgeHostPort: %v", err)
	}

	wantInsert := []string{"iptables", "-I", "INPUT", "-i", "goose-br0", "-s", "10.0.1.0/24", "-d", "10.0.1.1", "-p", "tcp", "--dport", "3000", "-j", "ACCEPT", "-m", "comment", "--comment", "anvil-cp-callback"}
	if hasCommand(commands, wantInsert) {
		t.Fatalf("unexpected duplicate insert command in %v", commands)
	}
}

func hasCommand(commands [][]string, want []string) bool {
	for _, got := range commands {
		if reflect.DeepEqual(got, want) {
			return true
		}
	}
	return false
}

// TestRelease_NeverAllocated verifies Release is safe to call for a VM whose IP
// was never reserved in this Manager instance. The v0.4.0 recovery disk-missing
// path calls Release(s.TapDevice, s.GuestIP) to clear a stale host TAP left by a
// prior daemon process, before any allocation is reclaimed — it must not error
// or corrupt the pool. A minimal struct literal avoids NewManager's bridge
// setup (which shells out to `ip`); deleteTapDevice on a nonexistent device is
// a logged no-op.
func TestRelease_NeverAllocated(t *testing.T) {
	m := &Manager{ipInUse: map[string]bool{"10.0.1.5": false}}

	if err := m.Release("tap5", "10.0.1.5"); err != nil {
		t.Fatalf("Release on a never-allocated TAP returned error: %v", err)
	}
	if m.ipInUse["10.0.1.5"] {
		t.Errorf("10.0.1.5 should remain free after Release")
	}
	// The parsed tap ID is returned to the free-list for reuse.
	if len(m.freeTapIDs) != 1 || m.freeTapIDs[0] != 5 {
		t.Errorf("expected freeTapIDs=[5], got %v", m.freeTapIDs)
	}
}

// TestRelease_FreesReservedIP verifies Release returns a reserved IP to the pool.
func TestRelease_FreesReservedIP(t *testing.T) {
	m := &Manager{ipInUse: map[string]bool{"10.0.1.7": true}}

	if err := m.Release("tap7", "10.0.1.7"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if m.ipInUse["10.0.1.7"] {
		t.Errorf("expected 10.0.1.7 freed after Release")
	}
}

// TestRelease_FlushesConntrackForGuestIP is the M15 regression guard: a stale
// conntrack entry (carrying the egress SNI fastpath mark 0x534e49, see
// sni_verdict.go) must not survive VM teardown, or a future VM handed the same
// guest IP could inherit an ACCEPT-without-inspection fastpath match. Release
// must issue `conntrack -D -s <guestIP>` when the conntrack binary is present.
func TestRelease_FlushesConntrackForGuestIP(t *testing.T) {
	m := &Manager{
		ipInUse:               map[string]bool{"10.0.1.7": true},
		conntrackFlushEnabled: true,
	}
	var commands [][]string
	m.runCommand = func(name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}

	if err := m.Release("tap7", "10.0.1.7"); err != nil {
		t.Fatalf("Release: %v", err)
	}

	want := []string{"conntrack", "-D", "-s", "10.0.1.7"}
	if !hasCommand(commands, want) {
		t.Fatalf("missing conntrack flush command %v in %v", want, commands)
	}
}

// TestRelease_ConntrackFlushFailureIsBestEffort verifies a failing conntrack
// invocation (e.g. no matching entries, or a permission error) does not fail
// teardown as a whole: the tap ID must still be recycled and the IP still
// returned to the pool.
func TestRelease_ConntrackFlushFailureIsBestEffort(t *testing.T) {
	m := &Manager{
		ipInUse:               map[string]bool{"10.0.1.7": true},
		conntrackFlushEnabled: true,
	}
	m.runCommand = func(name string, args ...string) error {
		if name == "conntrack" {
			return errors.New("conntrack: no such entry")
		}
		return nil
	}

	if err := m.Release("tap7", "10.0.1.7"); err != nil {
		t.Fatalf("Release must succeed even when conntrack flush fails: %v", err)
	}
	if m.ipInUse["10.0.1.7"] {
		t.Errorf("expected 10.0.1.7 freed after Release despite conntrack failure")
	}
	if len(m.freeTapIDs) != 1 || m.freeTapIDs[0] != 7 {
		t.Errorf("expected freeTapIDs=[7], got %v", m.freeTapIDs)
	}
}

// TestRelease_FlushesConntrackBeforeReturningIPToPool locks in ordering: the
// conntrack flush must run against the guest IP while it is still reserved,
// i.e. strictly before Release frees it back to the pool. Reversing this order
// would let a racing Allocate() hand the IP to a new VM before the stale
// mark is cleared.
func TestRelease_FlushesConntrackBeforeReturningIPToPool(t *testing.T) {
	m := &Manager{
		ipInUse:               map[string]bool{"10.0.1.9": true},
		conntrackFlushEnabled: true,
	}
	var sawConntrackCall bool
	var ipStillReservedAtFlushTime bool
	m.runCommand = func(name string, args ...string) error {
		if name == "conntrack" {
			sawConntrackCall = true
			ipStillReservedAtFlushTime = m.ipInUse["10.0.1.9"]
		}
		return nil
	}

	if err := m.Release("tap9", "10.0.1.9"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !sawConntrackCall {
		t.Fatal("expected conntrack flush to run")
	}
	if !ipStillReservedAtFlushTime {
		t.Fatal("IP was already returned to the pool before the conntrack flush ran")
	}
	if m.ipInUse["10.0.1.9"] {
		t.Errorf("expected 10.0.1.9 freed after Release completes")
	}
}

// TestRelease_SkipsConntrackFlushForInvalidGuestIP guards the argument-injection
// defense: Release must validate guestIP with net.ParseIP before it ever reaches
// exec.Command's argv, so a corrupted caller-supplied value cannot smuggle extra
// conntrack flags/args through as a fake "IP".
func TestRelease_SkipsConntrackFlushForInvalidGuestIP(t *testing.T) {
	m := &Manager{
		ipInUse:               map[string]bool{},
		conntrackFlushEnabled: true,
	}
	var commands [][]string
	m.runCommand = func(name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}

	if err := m.Release("tap1", "--not-an-ip"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, c := range commands {
		if len(c) > 0 && c[0] == "conntrack" {
			t.Fatalf("conntrack must not be invoked for an invalid guest IP, got %v", c)
		}
	}
}

// TestConntrackAvailable_NoPanic mirrors TestEbtablesAvailable_NoPanic: it
// cannot assert a fixed result (host-dependent), but it pins that the PATH
// probe is a pure, side-effect-free bool used by NewManager to disable the
// conntrack flush (with a single startup warning) when the binary is absent,
// rather than shelling out and failing on every VM teardown.
func TestConntrackAvailable_NoPanic(t *testing.T) {
	_ = conntrackAvailable()
}
