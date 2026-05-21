package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testAgent stands in for the in-VM goose-agent's /health endpoint. Toggle
// failNow (1=fail, 0=ok) to switch behavior. A plain int32 + atomic.Load/Store
// is used instead of atomic.Bool/atomic.Int32 so the file remains compatible
// with go 1.18 (the module's declared minimum); the atomic.* types were
// added in 1.19.
type testAgent struct {
	server  *httptest.Server
	failNow int32
	port    int
}

// setFail toggles the mock /health response. true → 500, false → 200.
func (ta *testAgent) setFail(v bool) {
	if v {
		atomic.StoreInt32(&ta.failNow, 1)
	} else {
		atomic.StoreInt32(&ta.failNow, 0)
	}
}

func newTestAgent(t *testing.T) *testAgent {
	ta := &testAgent{}
	ta.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&ta.failNow) != 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	parts := strings.Split(strings.TrimPrefix(ta.server.URL, "http://"), ":")
	p, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("parse httptest port: %v", err)
	}
	ta.port = p
	t.Cleanup(func() { ta.server.Close() })
	return ta
}

func TestWatchdog_MarksDeadAfterThreshold(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	flock, err := fm.Create("flock-wd", "test", "", "", filepath.Join(tmp, "flock-wd", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	flock.AddAgent(&AgentInfo{
		AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: AgentStatusReady,
	})

	agent := newTestAgent(t)
	locator := func(vmID string) (string, string, bool) {
		if vmID == "vm-1" {
			return "flock-wd", "worker-1", true
		}
		return "", "", false
	}
	lister := func() []VMRef {
		return []VMRef{{VMID: "vm-1", GuestIP: "127.0.0.1"}}
	}

	wd := NewWatchdog(fm, locator, lister, agent.port)
	wd.interval = 50 * time.Millisecond
	wd.dyingThreshold = 3
	agent.setFail(true)
	wd.Start()
	defer wd.Stop()

	// Wait for at least 3 ticks plus the post-threshold update.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if flock.Snapshot()[0].Status == AgentStatusDead {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	snap := flock.Snapshot()
	if snap[0].Status != AgentStatusDead {
		t.Fatalf("expected status=dead, got %q", snap[0].Status)
	}
	hist, _ := flock.TownWall.History()
	if len(hist) == 0 {
		t.Fatal("expected Town Wall entry on dead detection")
	}
	last := hist[len(hist)-1]
	if !strings.Contains(last.Body, "unresponsive") {
		t.Errorf("Town Wall entry missing unresponsive notice: %q", last.Body)
	}
	if last.AgentID != "orchestrator" {
		t.Errorf("dead notice should be posted as orchestrator, got %q", last.AgentID)
	}
}

func TestWatchdog_HealthyVMNeverMarked(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	flock, err := fm.Create("flock-ok", "test", "", "", filepath.Join(tmp, "flock-ok", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	flock.AddAgent(&AgentInfo{
		AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: AgentStatusReady,
	})

	agent := newTestAgent(t)
	locator := func(string) (string, string, bool) { return "flock-ok", "worker-1", true }
	lister := func() []VMRef {
		return []VMRef{{VMID: "vm-1", GuestIP: "127.0.0.1"}}
	}

	wd := NewWatchdog(fm, locator, lister, agent.port)
	wd.interval = 30 * time.Millisecond
	wd.dyingThreshold = 3
	wd.Start()
	defer wd.Stop()

	time.Sleep(400 * time.Millisecond)

	snap := flock.Snapshot()
	if snap[0].Status != AgentStatusReady {
		t.Errorf("healthy VM should stay ready, got %q", snap[0].Status)
	}
}

func TestWatchdog_TransientFailureDoesNotMark(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	flock, err := fm.Create("flock-flap", "test", "", "", filepath.Join(tmp, "flock-flap", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	flock.AddAgent(&AgentInfo{AgentID: "w", Role: "worker", VMID: "vm-1", Status: AgentStatusReady})

	agent := newTestAgent(t)
	locator := func(string) (string, string, bool) { return "flock-flap", "w", true }
	lister := func() []VMRef { return []VMRef{{VMID: "vm-1", GuestIP: "127.0.0.1"}} }

	wd := NewWatchdog(fm, locator, lister, agent.port)
	wd.interval = 50 * time.Millisecond
	wd.dyingThreshold = 3
	wd.Start()
	defer wd.Stop()

	// One failure, then recovery — fail count should reset on the next success.
	agent.setFail(true)
	time.Sleep(80 * time.Millisecond)
	agent.setFail(false)
	time.Sleep(300 * time.Millisecond)

	snap := flock.Snapshot()
	if snap[0].Status == AgentStatusDead {
		t.Errorf("transient failure should not mark dead, got %q", snap[0].Status)
	}
}

// TestWatchdog_PersistsDeadStatus verifies that crossing the dyingThreshold
// not only flips the in-memory status but also writes metadata.json so the
// dead state survives a daemon restart.
func TestWatchdog_PersistsDeadStatus(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	flock, err := fm.Create("flock-wd-persist", "test", filepath.Join(tmp, "flock-wd-persist", "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	flock.AddAgent(&AgentInfo{
		AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: AgentStatusReady,
	})
	// Seed metadata.json so LoadFromDisk can later pick it up.
	if err := flock.Persist(tmp); err != nil {
		t.Fatalf("seed Persist: %v", err)
	}

	agent := newTestAgent(t)
	locator := func(vmID string) (string, string, bool) {
		if vmID == "vm-1" {
			return "flock-wd-persist", "worker-1", true
		}
		return "", "", false
	}
	lister := func() []VMRef { return []VMRef{{VMID: "vm-1", GuestIP: "127.0.0.1"}} }

	wd := NewWatchdog(fm, locator, lister, agent.port)
	wd.interval = 50 * time.Millisecond
	wd.dyingThreshold = 3
	agent.setFail(true)
	wd.Start()
	defer wd.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if flock.Snapshot()[0].Status == AgentStatusDead {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Cross-check that the on-disk metadata was updated, not just the in-memory copy.
	fm2 := NewFlockManager(tmp)
	if _, _, err := fm2.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	reloaded, ok := fm2.Get("flock-wd-persist")
	if !ok {
		t.Fatal("flock-wd-persist missing after LoadFromDisk")
	}
	if got := reloaded.Snapshot()[0].Status; got != AgentStatusDead {
		t.Errorf("expected on-disk status=dead after watchdog mark, got %q", got)
	}
}

// TestWatchdog_ForgetVM_ClearsState ensures ForgetVM drops both the fail
// counter and the deadMarked bit so a recycled vmID does not inherit the
// previous instance's state. Without this, a per-agent restart that reuses
// the (typically distinct) vmID would still skip the dead notice if the
// same vmID later collided.
func TestWatchdog_ForgetVM_ClearsState(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	flock, _ := fm.Create("flock-forget", "test", filepath.Join(tmp, "flock-forget", "TOWN_WALL.log"))
	flock.AddAgent(&AgentInfo{AgentID: "w", Role: "worker", VMID: "vm-1", Status: AgentStatusReady})

	agent := newTestAgent(t)
	locator := func(string) (string, string, bool) { return "flock-forget", "w", true }
	lister := func() []VMRef { return []VMRef{{VMID: "vm-1", GuestIP: "127.0.0.1"}} }

	wd := NewWatchdog(fm, locator, lister, agent.port)
	wd.interval = 30 * time.Millisecond
	wd.dyingThreshold = 3
	agent.setFail(true)
	wd.Start()
	defer wd.Stop()

	// Wait for dead marking.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if flock.Snapshot()[0].Status == AgentStatusDead {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if flock.Snapshot()[0].Status != AgentStatusDead {
		t.Fatal("setup: expected agent to be marked dead")
	}

	wd.ForgetVM("vm-1")

	wd.mu.Lock()
	_, hasFail := wd.failCount["vm-1"]
	_, hasDead := wd.deadMarked["vm-1"]
	wd.mu.Unlock()
	if hasFail {
		t.Error("ForgetVM should delete failCount entry")
	}
	if hasDead {
		t.Error("ForgetVM should delete deadMarked entry")
	}
}

// TestWatchdog_Configure_AppliesTunables verifies the public Configure
// entry-point lands all four tunables and clamps interval >= httpTimeout.
func TestWatchdog_Configure_AppliesTunables(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	wd := NewWatchdog(fm, func(string) (string, string, bool) { return "", "", false },
		func() []VMRef { return nil }, 8080)

	wd.Configure(123*time.Millisecond, 47*time.Millisecond, 7, true)

	if wd.interval != 123*time.Millisecond {
		t.Errorf("interval: want 123ms, got %s", wd.interval)
	}
	if wd.httpTimeout != 47*time.Millisecond {
		t.Errorf("httpTimeout: want 47ms, got %s", wd.httpTimeout)
	}
	if wd.client.Timeout != 47*time.Millisecond {
		t.Errorf("client.Timeout: want 47ms, got %s", wd.client.Timeout)
	}
	if wd.dyingThreshold != 7 {
		t.Errorf("dyingThreshold: want 7, got %d", wd.dyingThreshold)
	}
	if !wd.autoHeal {
		t.Error("autoHeal: want true, got false")
	}

	// Zero values must NOT overwrite. autoHeal is a bool — it always lands.
	wd.Configure(0, 0, 0, false)
	if wd.interval != 123*time.Millisecond {
		t.Errorf("zero interval should keep prior value, got %s", wd.interval)
	}
	if wd.httpTimeout != 47*time.Millisecond {
		t.Errorf("zero httpTimeout should keep prior value, got %s", wd.httpTimeout)
	}
	if wd.dyingThreshold != 7 {
		t.Errorf("zero threshold should keep prior value, got %d", wd.dyingThreshold)
	}
	if wd.autoHeal {
		t.Error("autoHeal=false should land")
	}

	// Clamp: interval < httpTimeout should bump interval up to httpTimeout.
	wd.Configure(10*time.Millisecond, 100*time.Millisecond, 0, false)
	if wd.interval != 100*time.Millisecond {
		t.Errorf("clamp: interval should be raised to httpTimeout, got %s", wd.interval)
	}
}

// TestWatchdog_AutoHeal_ResetsDeadMark verifies that when autoHeal is on,
// a VM that crosses the dead threshold and then starts responding again is
// auto-cleared back to ready, the change persisted, and a Town Wall notice
// posted. With the default (autoHeal=false) the sticky-dead behavior under
// TestWatchdog_MarksDeadAfterThreshold is the contract.
func TestWatchdog_AutoHeal_ResetsDeadMark(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	flock, err := fm.Create("flock-heal", "test", filepath.Join(tmp, "flock-heal", "TOWN_WALL.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	flock.AddAgent(&AgentInfo{
		AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: AgentStatusReady,
	})
	if err := flock.Persist(tmp); err != nil {
		t.Fatalf("seed Persist: %v", err)
	}

	agent := newTestAgent(t)
	locator := func(vmID string) (string, string, bool) {
		if vmID == "vm-1" {
			return "flock-heal", "worker-1", true
		}
		return "", "", false
	}
	lister := func() []VMRef { return []VMRef{{VMID: "vm-1", GuestIP: "127.0.0.1"}} }

	wd := NewWatchdog(fm, locator, lister, agent.port)
	wd.Configure(50*time.Millisecond, 50*time.Millisecond, 3, true)
	agent.setFail(true)
	wd.Start()
	defer wd.Stop()

	// Phase 1: wait for the dead transition.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if flock.Snapshot()[0].Status == AgentStatusDead {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if flock.Snapshot()[0].Status != AgentStatusDead {
		t.Fatal("setup: expected agent to be marked dead before recovery")
	}

	// Phase 2: flip mock to OK; auto-heal should restore ready on the next tick.
	agent.setFail(false)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if flock.Snapshot()[0].Status == AgentStatusReady {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := flock.Snapshot()[0].Status; got != AgentStatusReady {
		t.Fatalf("expected status=ready after auto-heal, got %q", got)
	}

	// Town Wall must contain BOTH the dead notice and the recovery notice.
	hist, _ := flock.TownWall.History()
	var sawDead, sawHeal bool
	for _, m := range hist {
		if strings.Contains(m.Body, "unresponsive") {
			sawDead = true
		}
		if strings.Contains(m.Body, "recovered") {
			sawHeal = true
		}
	}
	if !sawDead {
		t.Error("expected dead notice in Town Wall")
	}
	if !sawHeal {
		t.Error("expected recovery notice in Town Wall after auto-heal")
	}

	// On-disk metadata must also be ready (auto-heal persists).
	fm2 := NewFlockManager(tmp)
	if _, _, err := fm2.LoadFromDisk(); err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	reloaded, ok := fm2.Get("flock-heal")
	if !ok {
		t.Fatal("flock-heal missing after LoadFromDisk")
	}
	if got := reloaded.Snapshot()[0].Status; got != AgentStatusReady {
		t.Errorf("expected on-disk status=ready after auto-heal, got %q", got)
	}
}

func TestWatchdog_StopReleasesGoroutine(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	wd := NewWatchdog(fm, func(string) (string, string, bool) { return "", "", false },
		func() []VMRef { return nil }, 8080)
	wd.interval = 20 * time.Millisecond
	wd.Start()

	done := make(chan struct{})
	go func() {
		wd.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog Stop did not return within 2s")
	}
}
