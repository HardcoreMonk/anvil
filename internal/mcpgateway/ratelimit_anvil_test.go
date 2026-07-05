package mcpgateway

import (
	"testing"
	"time"
)

// TestTokenBucketLimiter_KeyIsPerVMAndServer is an anvil guard for the v0.6.1
// hardening rule: "Rate limit key is per VM and backend server." Upstream's
// TestGateway_RateLimited only exercises one (VM, server) pair, so it would still
// pass if the limiter collapsed the key to VM-only or server-only — which would
// let one noisy VM (or one hot backend) throttle every other tenant. This pins
// the two-dimensional key: exhausting one pair must leave every other pair with
// its full independent budget.
func TestTokenBucketLimiter_KeyIsPerVMAndServer(t *testing.T) {
	lim := NewTokenBucketLimiter(1, 1) // 1 token, no refill within the frozen clock
	clock := time.Unix(1000, 0)
	lim.now = func() time.Time { return clock }

	// Exhaust vm1's budget against serverA.
	if !lim.Allow("vm1", "serverA") {
		t.Fatal("first vm1/serverA call should be admitted")
	}
	if lim.Allow("vm1", "serverA") {
		t.Fatal("second vm1/serverA call should be rate-limited (bucket empty)")
	}

	// A different VM against the same server has its own bucket.
	if !lim.Allow("vm2", "serverA") {
		t.Error("vm2/serverA must not be throttled by vm1's usage (key must include VM)")
	}

	// The same VM against a different server has its own bucket.
	if !lim.Allow("vm1", "serverB") {
		t.Error("vm1/serverB must not be throttled by vm1/serverA usage (key must include server)")
	}

	// And the exhausted pair is still exhausted (no cross-contamination back).
	if lim.Allow("vm1", "serverA") {
		t.Error("vm1/serverA must remain rate-limited after other pairs were served")
	}
}

// TestNoopLimiter_UnlimitedByDefault is an anvil guard for the v0.6.1 rule:
// "EPHEMERA_MCP_RATE default 0 means unlimited." When no rate is configured the
// daemon leaves Options.Limiter nil, and New() must install the noop limiter that
// admits every call regardless of (VM, server) — so the default deployment has
// zero throttling and zero per-call limiter overhead.
func TestNoopLimiter_UnlimitedByDefault(t *testing.T) {
	var l RateLimiter = noopLimiter{}
	for i := 0; i < 1000; i++ {
		if !l.Allow("vm1", "serverA") {
			t.Fatalf("noop limiter must admit every call; denied at call %d", i)
		}
	}

	// A Gateway built without an explicit Limiter must fall back to the noop.
	g := New(Options{Registry: &Registry{}})
	if g.limiter == nil {
		t.Fatal("New() must install a non-nil default limiter")
	}
	if _, ok := g.limiter.(noopLimiter); !ok {
		t.Fatalf("default limiter must be noopLimiter (unlimited), got %T", g.limiter)
	}
}
