package orchestrator

import "testing"

// TestWatchdog_OnFailure_NilTownWall_NoPanic covers R3: a fresh relay flock owns
// no Town Wall (RegisterRelay leaves TownWall=nil). When a relay's local member
// VM fails the health threshold, onFailure must still mark it dead and persist —
// without dereferencing the nil wall (which crashes the whole daemon).
func TestWatchdog_OnFailure_NilTownWall_NoPanic(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	relay := fm.RegisterRelay("relay-wd", "http://home:3000", "rt", "ct", nil)
	if relay.TownWall != nil {
		t.Fatal("precondition: relay flock must have a nil Town Wall")
	}
	relay.AddAgent(&AgentInfo{AgentID: "w", Role: "worker", VMID: "vm-1", Status: AgentStatusReady})

	locator := func(string) (string, string, bool) { return "relay-wd", "w", true }
	wd := NewWatchdog(fm, locator, func() []VMRef { return nil }, 8080)
	wd.dyingThreshold = 1

	// Would nil-deref on flock.TownWall.Post before the fix.
	wd.onFailure(VMRef{VMID: "vm-1", GuestIP: "127.0.0.1"})

	if got := relay.AgentStatus("w"); got != AgentStatusDead {
		t.Fatalf("relay member status = %q, want dead", got)
	}
	wd.mu.Lock()
	dead := wd.deadMarked["vm-1"]
	wd.mu.Unlock()
	if !dead {
		t.Fatal("relay member should be recorded dead")
	}
}

// TestWatchdog_OnSuccess_NilTownWall_NoPanic covers the auto-heal twin of R3:
// onSuccess posts a recovery notice to the flock's Town Wall, which is nil for a
// relay flock. A revived relay member must heal back to ready without a panic.
func TestWatchdog_OnSuccess_NilTownWall_NoPanic(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	relay := fm.RegisterRelay("relay-wd", "http://home:3000", "rt", "ct", nil)
	if relay.TownWall != nil {
		t.Fatal("precondition: relay flock must have a nil Town Wall")
	}
	relay.AddAgent(&AgentInfo{AgentID: "w", Role: "worker", VMID: "vm-1", Status: AgentStatusReady})
	relay.MarkAgentDeadIfNotPaused("w")

	locator := func(string) (string, string, bool) { return "relay-wd", "w", true }
	wd := NewWatchdog(fm, locator, func() []VMRef { return nil }, 8080)
	wd.autoHeal = true
	wd.deadMarked["vm-1"] = true

	// Would nil-deref on flock.TownWall.Post before the fix.
	wd.onSuccess("vm-1")

	if got := relay.AgentStatus("w"); got != AgentStatusReady {
		t.Fatalf("relay member status = %q, want ready (auto-healed)", got)
	}
	wd.mu.Lock()
	_, stillDead := wd.deadMarked["vm-1"]
	wd.mu.Unlock()
	if stillDead {
		t.Fatal("auto-heal should clear the dead mark")
	}
}
