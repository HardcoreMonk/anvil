package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ephemera/internal/network"
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

	// decideTCP is the production TCP classifier (decide() has no production
	// caller); the sport is arbitrary here since a nil payload short-circuits
	// before any per-flow reassembler is touched, so this remains a pure
	// registration probe.
	if d := loop.decideTCP("10.0.1.11", 9001, nil); !(d.Action == sniDrop && d.Reason == "unregistered_source") {
		t.Fatalf("precondition: guest IP already registered pre-recovery, decideTCP = %+v", d)
	}
	if err := cp.reapplyRecoveredEgress(s); err != nil {
		t.Fatalf("reapply err = %v", err)
	}
	if d := loop.decideTCP("10.0.1.11", 9001, nil); d.Action == sniDrop && d.Reason == "unregistered_source" {
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

// TestReapplyRecoveredEgressReturnsErrorOnApplyFailure proves the before-boot
// contract: when applyEgressPolicy fails, reapplyRecoveredEgress simply returns
// the error so its caller refuses to boot the VM (fail-closed by not booting).
// The obsolete emergency-fence mechanism is gone — NO fence command is issued,
// and no ACCEPT rule for the VM may survive the failed apply.
func TestReapplyRecoveredEgressReturnsErrorOnApplyFailure(t *testing.T) {
	cp := newTestCP(t)
	profileDir := t.TempDir()
	writeEgressProfileFixture(t, profileDir, "restricted")

	var cmds [][]string
	cp.egress = &commandEgressEnforcer{
		rules:      map[string]egressRule{},
		profileDir: profileDir,
		run: func(name string, args ...string) error {
			cmds = append(cmds, append([]string{name}, args...))
			// Fail the apply's base REJECT insert.
			if len(args) >= 1 && args[0] == "-I" && commandHasComment(args, "anvil-egress-vm-fc-default") {
				return errors.New("apply insert failed")
			}
			return nil
		},
		runOutput: func(string, ...string) ([]byte, error) { return nil, nil }, // mode-2: nothing to flush
	}
	s := storage.VMState{VMID: "vm-fc", GuestIP: "10.0.1.13", TapDevice: "tap-fc", EgressPolicy: "profile", Profile: "restricted"}

	if err := cp.reapplyRecoveredEgress(s); err == nil {
		t.Fatal("reapply returned nil, want fail-closed apply error")
	}
	// The obsolete emergency fence must never be issued (fence mechanism removed);
	// its rules carried a "...-fenced" comment, so no emitted command may mention it.
	for _, c := range cmds {
		j := strings.Join(c, " ")
		if strings.Contains(j, "fenced") {
			t.Fatalf("emergency fence command issued after fence removal: %q", j)
		}
	}
	// No ACCEPT for this VM may survive the failed apply (no blanket-ACCEPT leak).
	for _, c := range cmds {
		j := strings.Join(c, " ")
		if strings.Contains(j, "-j ACCEPT") && strings.Contains(j, "10.0.1.13") {
			t.Fatalf("recovered VM left with an ACCEPT rule after failed apply: %q", j)
		}
	}
}

// TestDropRecoveryStateFlushesEgress locks invariant 2: every boot-failure
// give-up path funnels through dropRecoveryState, which must reclaim this VM's
// per-VM egress rules so a failed recovery never leaks orphan iptables rules.
func TestDropRecoveryStateFlushesEgress(t *testing.T) {
	cp := newTestCP(t)
	// A stale per-VM rule a before-boot apply installed; the give-up flush -D's it.
	dump := strings.Join([]string{
		"-P FORWARD ACCEPT",
		"-A FORWARD -i goose-br0 -s 10.0.1.0/24 -j ACCEPT",
		`-A FORWARD -s 10.0.1.40/32 -j REJECT --reject-with icmp-port-unreachable -m comment --comment "anvil-egress-vm-drop-default"`,
		"",
	}, "\n")
	var cmds [][]string
	cp.egress = &commandEgressEnforcer{
		run: func(name string, args ...string) error {
			cmds = append(cmds, append([]string{name}, args...))
			return nil
		},
		runOutput: func(string, ...string) ([]byte, error) { return []byte(dump), nil },
	}
	s := storage.VMState{VMID: "vm-drop", GuestIP: "10.0.1.40", TapDevice: "tap-drop"}

	cp.dropRecoveryState(s)

	wantDel := "iptables -D FORWARD -s 10.0.1.40/32 -j REJECT --reject-with icmp-port-unreachable -m comment --comment anvil-egress-vm-drop-default"
	if !containsCommand(cmds, wantDel) {
		t.Fatalf("dropRecoveryState did not flush the VM's egress rule (orphan leak): cmds = %v", cmds)
	}
	// It must not delete the base-subnet blanket ACCEPT.
	for _, c := range cmds {
		j := strings.Join(c, " ")
		if len(c) >= 2 && c[1] == "-D" && strings.Contains(j, "10.0.1.0/24") {
			t.Fatalf("dropRecoveryState deleted the base-subnet ACCEPT: %q", j)
		}
	}
}

// TestRecoverVMsEgressApplyFailRefusesBoot proves invariant 1 on the cold path:
// when the before-boot egress apply fails, RecoverVMs refuses to boot the VM
// (StartMachine is never reached, so no KVM is needed to exercise this leg),
// surfaces the VM via failed[], releases its network, drops its state, and never
// registers it in cp.vms. Only the failure leg is unit-testable here; the
// success→boot leg is verified by the full-KVM orchestrator gate.
func TestRecoverVMsEgressApplyFailRefusesBoot(t *testing.T) {
	cp := newTestCP(t)
	// A real rootfs file so the disk-missing branch is skipped and control reaches
	// the before-boot egress apply. Plain disk mode skips all COW/provisioner work.
	disk := filepath.Join(t.TempDir(), "vm-cold.ext4")
	if err := os.WriteFile(disk, []byte("x"), 0644); err != nil {
		t.Fatalf("seed disk: %v", err)
	}
	released := false
	cp.reclaimNetwork = func(tap, ip, mac string) error { return nil }
	cp.releaseVMNetwork = func(tap, ip string) error { released = true; return nil }

	applied := false
	cp.egress = &commandEgressEnforcer{
		rules: map[string]egressRule{},
		run: func(name string, args ...string) error {
			if len(args) >= 1 && args[0] == "-I" { // deny_all base REJECT insert == the apply
				applied = true
				return errors.New("apply insert failed")
			}
			return nil
		},
		runOutput: func(string, ...string) ([]byte, error) { return nil, nil },
	}
	s := storage.VMState{
		VMID: "vm-cold", GuestIP: "10.0.1.60", TapDevice: "tap-cold", MacAddr: "AA:FC:00:00:00:60",
		SocketPath: filepath.Join(t.TempDir(), "vm-cold.sock"),
		DiskPath:   disk, DiskMode: storage.DiskModePlain, EgressPolicy: "deny_all",
	}
	if err := storage.SaveVMState(cp.workDir, s); err != nil {
		t.Fatalf("seed SaveVMState: %v", err)
	}

	recovered, failed, err := cp.RecoverVMs()
	if err != nil {
		t.Fatalf("RecoverVMs err = %v", err)
	}
	if !applied {
		t.Fatal("before-boot egress apply was never attempted")
	}
	if recovered != 0 {
		t.Fatalf("recovered = %d, want 0 (egress failed → no boot)", recovered)
	}
	if len(failed) != 1 || failed[0] != "vm-cold" {
		t.Fatalf("failed = %v, want [vm-cold]", failed)
	}
	if !released {
		t.Fatal("network not released on egress-fail give-up (leak)")
	}
	if _, ok := cp.vms["vm-cold"]; ok {
		t.Fatal("VM registered in cp.vms despite egress-fail (would boot fail-OPEN)")
	}
	if _, err := storage.LoadVMState(cp.workDir, "vm-cold"); err == nil {
		t.Fatal("state.json not dropped after egress-fail give-up")
	}
}

// TestRecoverRestoredVMEgressApplyFailRefusesReRestore proves invariant 1 on the
// snapshot path: a before-boot egress apply failure short-circuits BEFORE
// reRestoreMachine (no KVM reached), surfaces the VM via failed[], releases its
// network, drops its state, and never registers it in cp.vms.
func TestRecoverRestoredVMEgressApplyFailRefusesReRestore(t *testing.T) {
	cp := newTestCP(t)
	cp.provisioner = &storage.Provisioner{WorkspaceDir: t.TempDir()}
	cp.snapshots = map[string]storage.SnapshotMetadata{"snap-1": {SnapshotID: "snap-1"}}

	// Keep the COW-device reclaim hermetic (avoid dmsetup/umount on the test host);
	// this refactor does not change that helper.
	origCOW := removeRestoredCOWDevice
	removeRestoredCOWDevice = func(workspaceDir, vmID string) {}
	defer func() { removeRestoredCOWDevice = origCOW }()

	released := false
	cp.reclaimNetwork = func(tap, ip, mac string) error { return nil }
	cp.releaseVMNetwork = func(tap, ip string) error { released = true; return nil }

	cp.egress = &commandEgressEnforcer{
		rules: map[string]egressRule{},
		run: func(name string, args ...string) error {
			if len(args) >= 1 && args[0] == "-I" {
				return errors.New("apply insert failed")
			}
			return nil
		},
		runOutput: func(string, ...string) ([]byte, error) { return nil, nil },
	}
	s := storage.VMState{
		VMID: "vm-restored-2", GuestIP: "10.0.1.61", TapDevice: "tap-restored-2", MacAddr: "AA:FC:00:00:00:61",
		SocketPath: filepath.Join(t.TempDir(), "vm-restored-2.sock"),
		DiskPath:   filepath.Join(t.TempDir(), "vm-restored-2.cow"),
		DiskMode:   storage.DiskModeCOW, EgressPolicy: "deny_all", SourceSnapshotID: "snap-1",
	}
	if err := storage.SaveVMState(cp.workDir, s); err != nil {
		t.Fatalf("seed SaveVMState: %v", err)
	}

	recovered := 0
	var failed []string
	cp.recoverRestoredVM(s, &recovered, &failed)

	if recovered != 0 {
		t.Fatalf("recovered = %d, want 0", recovered)
	}
	if len(failed) != 1 || failed[0] != "vm-restored-2" {
		t.Fatalf("failed = %v, want [vm-restored-2]", failed)
	}
	if !released {
		t.Fatal("network not released on egress-fail give-up (leak)")
	}
	if _, ok := cp.vms["vm-restored-2"]; ok {
		t.Fatal("VM registered despite egress-fail (would re-restore fail-OPEN)")
	}
	if _, err := storage.LoadVMState(cp.workDir, "vm-restored-2"); err == nil {
		t.Fatal("state.json not dropped after egress-fail give-up")
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
