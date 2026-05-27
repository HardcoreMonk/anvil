package network

import "testing"

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
