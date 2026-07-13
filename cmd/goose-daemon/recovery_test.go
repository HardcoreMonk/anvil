package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ephemera/internal/network"
	"ephemera/internal/orchestrator"
	"ephemera/internal/storage"
)

// containsCommand reports whether any captured command joins to exactly want.
func containsCommand(cmds [][]string, want string) bool {
	for _, c := range cmds {
		if strings.Join(c, " ") == want {
			return true
		}
	}
	return false
}

// commandHasComment reports whether an iptables arg list carries --comment value.
func commandHasComment(args []string, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--comment" && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestReapplyRecoveredEgressDenyAllStaysBlocked proves a recovered deny_all VM
// gets its base REJECT re-installed rather than being left behind the base-subnet
// blanket ACCEPT (the pre-existing host-reboot fail-OPEN hole). mode-2 is modeled
// by an empty `iptables -S FORWARD` dump, so nothing is flushed first.
func TestReapplyRecoveredEgressDenyAllStaysBlocked(t *testing.T) {
	cp := newTestCP(t)
	var cmds [][]string
	cp.egress = &commandEgressEnforcer{
		rules: map[string]egressRule{},
		run: func(name string, args ...string) error {
			cmds = append(cmds, append([]string{name}, args...))
			return nil
		},
		runOutput: func(string, ...string) ([]byte, error) { return nil, nil },
	}
	s := storage.VMState{VMID: "vm-deny", GuestIP: "10.0.1.10", TapDevice: "tap-deny", EgressPolicy: "deny_all"}

	if err := cp.reapplyRecoveredEgress(s); err != nil {
		t.Fatalf("reapply err = %v", err)
	}
	want := "iptables -I FORWARD -s 10.0.1.10 -j REJECT -m comment --comment anvil-egress-vm-deny"
	if !containsCommand(cmds, want) {
		t.Fatalf("recovered deny_all VM missing base REJECT (fail-OPEN): cmds = %v", cmds)
	}
}

// TestReapplyRecoveredEgressReRegistersSNI proves an allow_sni VM's in-process
// verdict-loop registry entry — which a daemon restart drops, causing recovered
// :443 traffic to hit the unregistered_source fail-closed DROP — is rebuilt by
// re-applying through the same applyEgressPolicy path spawn uses (Task 5 does the
// Register inside ApplyWithProfile; no extra Register wiring here).
func TestReapplyRecoveredEgressReRegistersSNI(t *testing.T) {
	cp := newTestCP(t)
	profileDir := t.TempDir()
	writeEgressProfileFixtureWithSNI(t, profileDir, "sni")
	loop := newSNIVerdictLoop(88, "", nil)
	loop.ready = true // simulate a started loop so the allow_sni preflight passes
	cp.egress = &commandEgressEnforcer{
		rules:      map[string]egressRule{},
		profileDir: profileDir,
		sniLoop:    loop,
		run:        func(string, ...string) error { return nil },
		runOutput:  func(string, ...string) ([]byte, error) { return nil, nil },
	}
	s := storage.VMState{VMID: "vm-sni", GuestIP: "10.0.1.11", TapDevice: "tap-sni", EgressPolicy: "profile", Profile: "sni", TenantID: "t1"}

	if d := loop.decide("10.0.1.11", nil); !(d.Action == sniDrop && d.Reason == "unregistered_source") {
		t.Fatalf("precondition: guest IP already registered pre-recovery, decide = %+v", d)
	}
	if err := cp.reapplyRecoveredEgress(s); err != nil {
		t.Fatalf("reapply err = %v", err)
	}
	if d := loop.decide("10.0.1.11", nil); d.Action == sniDrop && d.Reason == "unregistered_source" {
		t.Fatal("recovery did not re-register guest IP in SNI verdict loop")
	}
}

// TestReapplyRecoveredEgressIdempotentFlush proves mode-1 idempotency: this VM's
// stale per-VM FORWARD rules (kept in the kernel across a bare daemon restart) are
// deleted BEFORE the fresh apply, while the base-subnet blanket ACCEPT, the
// control-plane callback, and a DIFFERENT VM whose id shares this one's prefix
// ("vm-idem2") are all left untouched.
func TestReapplyRecoveredEgressIdempotentFlush(t *testing.T) {
	cp := newTestCP(t)
	profileDir := t.TempDir()
	writeEgressProfileFixture(t, profileDir, "restricted") // allow_cidrs 203.0.113.10/32
	dump := strings.Join([]string{
		"-P FORWARD ACCEPT",
		"-A FORWARD -i goose-br0 -s 10.0.1.0/24 -j ACCEPT",
		`-A FORWARD -s 10.0.1.1/32 -j ACCEPT -m comment --comment "anvil-cp-callback"`,
		`-A FORWARD -s 10.0.1.12/32 -j REJECT --reject-with icmp-port-unreachable -m comment --comment "anvil-egress-vm-idem-default"`,
		`-A FORWARD -s 10.0.1.12/32 -d 203.0.113.10/32 -j ACCEPT -m comment --comment "anvil-egress-vm-idem-cidr-0"`,
		`-A FORWARD -s 10.0.1.99/32 -j REJECT --reject-with icmp-port-unreachable -m comment --comment "anvil-egress-vm-idem2-default"`,
		"",
	}, "\n")
	var cmds [][]string
	cp.egress = &commandEgressEnforcer{
		rules:      map[string]egressRule{},
		profileDir: profileDir,
		run: func(name string, args ...string) error {
			cmds = append(cmds, append([]string{name}, args...))
			return nil
		},
		runOutput: func(string, ...string) ([]byte, error) { return []byte(dump), nil },
	}
	s := storage.VMState{VMID: "vm-idem", GuestIP: "10.0.1.12", TapDevice: "tap-idem", EgressPolicy: "profile", Profile: "restricted"}

	if err := cp.reapplyRecoveredEgress(s); err != nil {
		t.Fatalf("reapply err = %v", err)
	}
	if len(cmds) < 4 {
		t.Fatalf("expected 2 flush deletes + fresh applies, got %v", cmds)
	}
	// The two stale per-VM rules are deleted first, in listing order.
	wantDel0 := "iptables -D FORWARD -s 10.0.1.12/32 -j REJECT --reject-with icmp-port-unreachable -m comment --comment anvil-egress-vm-idem-default"
	wantDel1 := "iptables -D FORWARD -s 10.0.1.12/32 -d 203.0.113.10/32 -j ACCEPT -m comment --comment anvil-egress-vm-idem-cidr-0"
	if got := strings.Join(cmds[0], " "); got != wantDel0 {
		t.Fatalf("flush cmd[0] = %q, want %q", got, wantDel0)
	}
	if got := strings.Join(cmds[1], " "); got != wantDel1 {
		t.Fatalf("flush cmd[1] = %q, want %q", got, wantDel1)
	}
	// Fresh apply happens AFTER the flush, and re-installs the base REJECT.
	if !containsCommand(cmds, "iptables -I FORWARD -s 10.0.1.12 -j REJECT -m comment --comment anvil-egress-vm-idem-default") {
		t.Fatalf("fresh apply base REJECT missing after flush: cmds = %v", cmds)
	}
	// Nothing that is not this VM's own rule may be deleted.
	for i, c := range cmds {
		j := strings.Join(c, " ")
		if len(c) < 2 || c[1] != "-D" {
			continue
		}
		if strings.Contains(j, "anvil-egress-vm-idem2") || strings.Contains(j, "10.0.1.0/24") || strings.Contains(j, "anvil-cp-callback") || strings.Contains(j, "10.0.1.99") {
			t.Fatalf("flush deleted a foreign/base rule at cmd[%d]: %q", i, j)
		}
	}
}

// TestFlushByCommentDeletesBareDenyAllComment covers flushByComment's
// exact-match arm: a deny_all VM installs a BARE comment ("anvil-egress-<vmID>"
// with no suffix), which the dash-prefix arm ("anvil-egress-<vmID>-") alone would
// miss. The bare rule must be -D'd, while the base-subnet blanket ACCEPT (no
// comment) and a prefix-adjacent VM ("anvil-egress-vm-x2-...") stay untouched.
func TestFlushByCommentDeletesBareDenyAllComment(t *testing.T) {
	dump := strings.Join([]string{
		"-P FORWARD ACCEPT",
		"-A FORWARD -i goose-br0 -s 10.0.1.0/24 -j ACCEPT",
		`-A FORWARD -s 10.0.1.20/32 -j REJECT --reject-with icmp-port-unreachable -m comment --comment "anvil-egress-vm-x"`,
		`-A FORWARD -s 10.0.1.21/32 -j REJECT --reject-with icmp-port-unreachable -m comment --comment "anvil-egress-vm-x2-default"`,
		"",
	}, "\n")
	var cmds [][]string
	e := &commandEgressEnforcer{
		run: func(name string, args ...string) error {
			cmds = append(cmds, append([]string{name}, args...))
			return nil
		},
		runOutput: func(string, ...string) ([]byte, error) { return []byte(dump), nil },
	}

	e.flushByComment("vm-x")

	wantDel := "iptables -D FORWARD -s 10.0.1.20/32 -j REJECT --reject-with icmp-port-unreachable -m comment --comment anvil-egress-vm-x"
	if !containsCommand(cmds, wantDel) {
		t.Fatalf("bare deny_all comment not deleted (exact-match arm miss): cmds = %v", cmds)
	}
	// Exactly one delete: only the bare rule. Base ACCEPT and adjacent VM survive.
	if len(cmds) != 1 {
		t.Fatalf("expected exactly 1 delete (the bare rule), got %v", cmds)
	}
	for _, c := range cmds {
		j := strings.Join(c, " ")
		if strings.Contains(j, "10.0.1.0/24") || strings.Contains(j, "anvil-egress-vm-x2") {
			t.Fatalf("flush deleted a foreign/base rule: %q", j)
		}
	}
}

// TestReapplyRecoveredEgressFailClosedOnApplyError proves invariant 1: when the
// re-apply fails, the VM is fenced with an emergency REJECT (never left behind the
// blanket ACCEPT), the error propagates, and the flock agent is marked dead.
func TestReapplyRecoveredEgressFailClosedOnApplyError(t *testing.T) {
	cp := newTestCP(t)
	profileDir := t.TempDir()
	writeEgressProfileFixture(t, profileDir, "restricted")

	f, err := cp.flockMgr.NewUnregistered("flock-fc", "test", "tenant", "profile", filepath.Join(cp.workDir, "tw-fc.log"))
	if err != nil {
		t.Fatalf("NewUnregistered: %v", err)
	}
	cp.flockMgr.Register(f)
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "worker-1", Role: "worker", VMID: "vm-fc", AgentURL: "http://10.0.1.13:8080", Status: orchestrator.AgentStatusReady})

	var cmds [][]string
	cp.egress = &commandEgressEnforcer{
		rules:      map[string]egressRule{},
		profileDir: profileDir,
		run: func(name string, args ...string) error {
			cmds = append(cmds, append([]string{name}, args...))
			// Fail the apply's base REJECT insert; the fence -I still succeeds.
			if len(args) >= 1 && args[0] == "-I" && commandHasComment(args, "anvil-egress-vm-fc-default") {
				return errors.New("apply insert failed")
			}
			return nil
		},
		runOutput: func(string, ...string) ([]byte, error) { return nil, nil }, // mode-2: nothing to flush
	}
	s := storage.VMState{VMID: "vm-fc", GuestIP: "10.0.1.13", TapDevice: "tap-fc", EgressPolicy: "profile", Profile: "restricted", FlockID: "flock-fc", AgentID: "worker-1"}

	if err := cp.reapplyRecoveredEgress(s); err == nil {
		t.Fatal("reapply returned nil, want fail-closed apply error")
	}
	wantFence := "iptables -I FORWARD -s 10.0.1.13 -j REJECT -m comment --comment anvil-egress-vm-fc-recovery-fenced"
	if !containsCommand(cmds, wantFence) {
		t.Fatalf("emergency fence REJECT not issued: cmds = %v", cmds)
	}
	// No ACCEPT for this VM may survive the failed apply (no blanket-ACCEPT leak).
	for _, c := range cmds {
		j := strings.Join(c, " ")
		if strings.Contains(j, "-j ACCEPT") && strings.Contains(j, "10.0.1.13") {
			t.Fatalf("recovered VM left with an ACCEPT rule after failed apply: %q", j)
		}
	}
	if st := f.AgentStatus("worker-1"); st != orchestrator.AgentStatusDead {
		t.Fatalf("flock agent status = %q, want dead (degraded marking on fence)", st)
	}
}

// TestRecoverRestoredVM_SnapshotMissing_DropsState covers the v0.4.5 recovery
// branch for snapshot-restored VMs whose source snapshot is gone (deleted while
// the VM ran): the VM is unrecoverable, so its state.json is dropped and the
// vmID is surfaced via failed[] rather than silently left behind. Needs no KVM —
// it exercises the early "snapshot missing" path before any Firecracker work.
func TestRecoverRestoredVM_SnapshotMissing_DropsState(t *testing.T) {
	cp := newTestCP(t)
	cp.netManager = network.NewManager("10.0.1.", "10.0.1.1")
	cp.snapshots = make(map[string]storage.SnapshotMetadata) // empty: source snapshot absent

	st := storage.VMState{
		VMID:             "vm-restored-1",
		GuestIP:          "10.0.1.50",
		TapDevice:        "tap-test-99",
		MacAddr:          "AA:FC:00:00:00:99",
		SocketPath:       "/tmp/none-vm-restored-1.sock",
		DiskPath:         "/tmp/none-vm-restored-1.cow",
		DiskMode:         storage.DiskModeCOW,
		AgentURL:         "http://10.0.1.50:8080",
		SourceSnapshotID: "snap-gone", // not present in cp.snapshots
	}
	if err := storage.SaveVMState(cp.workDir, st); err != nil {
		t.Fatalf("seed SaveVMState: %v", err)
	}

	recovered := 0
	var failed []string
	cp.recoverRestoredVM(st, &recovered, &failed)

	if recovered != 0 {
		t.Errorf("recovered = %d, want 0", recovered)
	}
	if len(failed) != 1 || failed[0] != "vm-restored-1" {
		t.Errorf("failed = %v, want [vm-restored-1]", failed)
	}
	if _, err := storage.LoadVMState(cp.workDir, "vm-restored-1"); err == nil {
		t.Error("expected state.json to be dropped for an unrecoverable restored VM")
	}
}
