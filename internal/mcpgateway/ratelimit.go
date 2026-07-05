package mcpgateway

import (
	"sync"
	"time"
)

// 학습 주석 개요: v0.6.1 에서 도입된 rate limit 표면. 키는 반드시 (VMID, server)
// 2차원이다 — VM 단위나 server 단위로 뭉치면 한 VM 이 다른 VM 을, 또는 한 backend
// 가 다른 backend 호출을 굶길 수 있다(ratelimit_anvil_test.go 의
// TestTokenBucketLimiter_KeyIsPerVMAndServer 가 이 축소를 막는 가드). v0.6.2 에서
// gateway.go 의 handleResourcesRead/handlePromptsGet 도 같은 limiter.Allow 를
// 호출하도록 확장되어 tool/resource/prompt 가 하나의 budget 을 공유한다.

// RateLimiter decides whether a tool call from a given VM to a given backend
// server may proceed. It is an Options seam (like CallerResolver / PolicyStore)
// so a multi-host build can swap in a distributed limiter without touching the
// protocol core.
// 학습 주석: gateway.go 의 모든 handleToolsCall/handleResourcesRead/
// handlePromptsGet 이 이 interface 하나만 호출한다(구현 교체 가능한 seam).
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
// 학습 주석: buckets map 의 key 는 Allow 안에서 vmID+"\x00"+server 로 조립된다 —
// VM 과 server 이름 어느 쪽에도 나타나지 않는 구분자(\x00)를 써서 두 문자열의
// 우연한 결합 충돌을 막는다.
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
// 학습 주석: cmd/goose-daemon/mcp_gateway.go 의 initMCPGateway 가
// EPHEMERA_MCP_RATE(>0일 때만) 와 EPHEMERA_MCP_BURST 로 이 생성자를 호출한다.
// rate 가 0(기본)이면 Options.Limiter 는 nil 로 남아 New() 가 noopLimiter 를 쓴다.
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
// 학습 주석: 이 메서드가 (VM, server) 키를 실제로 조립하는 지점이다 —
// vmID+"\x00"+server. 별도 VM/server 축을 두지 않고 문자열 결합 키 하나로
// map 을 관리하는 단순한 구현.
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
// 학습 주석: VM 이 삭제돼도 그 VM 의 bucket 은 별도 정리 없이 idle 상태로 남다가
// 여기서 청소된다 — VM lifecycle hook 이 필요 없는 lazy-sweep 설계.
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
