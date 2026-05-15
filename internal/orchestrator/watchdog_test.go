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
	flock, err := fm.Create("flock-wd", "test", filepath.Join(tmp, "flock-wd", "wall.log"))
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
	flock, err := fm.Create("flock-ok", "test", filepath.Join(tmp, "flock-ok", "wall.log"))
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
	flock, err := fm.Create("flock-flap", "test", filepath.Join(tmp, "flock-flap", "wall.log"))
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
