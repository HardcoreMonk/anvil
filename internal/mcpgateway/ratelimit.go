package mcpgateway

import (
	"sync"
	"time"
)

// RateLimiter decides whether a tool call from a given VM to a given backend
// server may proceed. It is an Options seam (like CallerResolver / PolicyStore)
// so a multi-host build can swap in a distributed limiter without touching the
// protocol core.
type RateLimiter interface {
	Allow(vmID, server string) bool
}

// noopLimiter allows every call. It is the default when no rate is configured.
type noopLimiter struct{}

func (noopLimiter) Allow(string, string) bool { return true }

// bucket is one token bucket for a (VM, server) key.
type bucket struct {
	tokens float64
	last   time.Time
}

// tokenBucketLimiter rate-limits tool calls per (VMID, server) with a token
// bucket. Tokens refill continuously at ratePerMin and are capped at burst, so a
// steady caller is held to the configured rate while a short spike up to burst
// is absorbed (smoother than a fixed window, which allows 2x across a boundary).
// Idle buckets are swept lazily so the map cannot grow without bound as
// ephemeral VMs come and go.
type tokenBucketLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	ratePerMin float64
	burst      float64
	evictAfter time.Duration
	lastSweep  time.Time
	now        func() time.Time // injectable clock for tests; defaults to time.Now
}

// NewTokenBucketLimiter builds a limiter admitting ratePerMin calls/minute per
// (VM, server) with a burst capacity. burst <= 0 defaults to ratePerMin, so one
// idle minute refills a full minute's allowance.
func NewTokenBucketLimiter(ratePerMin, burst int) *tokenBucketLimiter {
	b := float64(burst)
	if b <= 0 {
		b = float64(ratePerMin)
	}
	return &tokenBucketLimiter{
		buckets:    make(map[string]*bucket),
		ratePerMin: float64(ratePerMin),
		burst:      b,
		evictAfter: 10 * time.Minute,
		now:        time.Now,
	}
}

// Allow refills the caller's bucket by the elapsed time, then admits the call if
// at least one token remains.
func (l *tokenBucketLimiter) Allow(vmID, server string) bool {
	key := vmID + "\x00" + server
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	// Refill: elapsed minutes * ratePerMin, capped at burst.
	b.tokens += now.Sub(b.last).Minutes() * l.ratePerMin
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// sweep drops buckets idle longer than evictAfter. It runs at most once per
// minute and is called under l.mu from Allow. An idle bucket has refilled to
// burst, indistinguishable from a never-seen key, so evicting it is free.
func (l *tokenBucketLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.last) > l.evictAfter {
			delete(l.buckets, k)
		}
	}
}
