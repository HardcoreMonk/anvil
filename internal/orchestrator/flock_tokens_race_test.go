package orchestrator

import (
	"sync"
	"testing"
)

// TestDistributedTokens_ConcurrentRefill proves the hub token pair can be read
// while UpdateHubTokens re-seats it (reconcile re-POST racing a guest 2nd-hop
// call). The raw f.CallToken field became mutable-after-publish in the D1b fix,
// so outbound readers must go through the locked DistributedTokens getter; this
// test fails under -race if either side bypasses the per-flock lock.
func TestDistributedTokens_ConcurrentRefill(t *testing.T) {
	fm := NewFlockManager(t.TempDir())
	f := fm.RegisterHub("hub-1", nil, nil, "rt-0", "ct-0")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		// Land one refill unconditionally before entering the stop-checked loop.
		// wg.Wait() blocks on this Done, so this write happens-before the
		// post-condition assertion regardless of scheduling. Without it, a fast
		// main loop can close(stop) before this goroutine is ever scheduled; the
		// loop below would then return via <-stop having written nothing, leaving
		// the tokens at their rt-0/ct-0 seed and flaking the final assertion.
		fm.UpdateHubTokens("hub-1", "rt-1", "ct-1")
		fm.UpdateHubRoster("hub-1", []RosterMember{{AgentID: "a-1", Host: "h", VMID: "vm-1", Addr: "http://x"}})
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			fm.UpdateHubTokens("hub-1", "rt-1", "ct-1")
			fm.UpdateHubRoster("hub-1", []RosterMember{{AgentID: "a-1", Host: "h", VMID: "vm-1", Addr: "http://x"}})
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			relay, call := f.DistributedTokens()
			if relay == "" || call == "" {
				t.Error("tokens observed empty during refill")
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Roster is replaced by every reconcile re-POST; lookups must read a
			// locked snapshot, never range the live slice.
			for _, m := range f.RosterSnapshot() {
				_ = m.AgentID
			}
		}
	}()
	// A short window is enough for the race detector to observe both sides.
	for i := 0; i < 1000; i++ {
		f.DistributedTokens()
		f.RosterSnapshot()
	}
	close(stop)
	wg.Wait()

	relay, call := f.DistributedTokens()
	if relay != "rt-1" || call != "ct-1" {
		t.Fatalf("tokens after refill = %q/%q, want rt-1/ct-1", relay, call)
	}
}
