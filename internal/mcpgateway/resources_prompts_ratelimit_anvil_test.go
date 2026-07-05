package mcpgateway

import (
	"encoding/json"
	"testing"
	"time"
)

// TestGateway_ResourcesAndPromptsShareRateLimit is an anvil guard for the v0.6.2
// hardening rule: "resources/prompts calls share policy and rate limit with tool
// calls." Upstream's rate-limit test exercises only tools/call, and the
// resources/prompts tests exercise only the policy gate — so nothing pins that
// resources/read and prompts/get actually consult the limiter. Without this a
// refactor could let a compromised VM sidestep its per-(VM, server) budget by
// looping resource reads or prompt fetches. This proves both paths draw from the
// same token bucket as tools/call.
func TestGateway_ResourcesAndPromptsShareRateLimit(t *testing.T) {
	build := func(lim RateLimiter) (*Gateway, *[]AuditRecord) {
		m := newMockMCP(t, false)
		reg := newTestRegistry(t, ServerConfig{ID: "mock", Namespace: "mock", URL: m.srv.URL, Profiles: []string{"leader"}}, nil)
		var audited []AuditRecord
		g := New(Options{
			Resolver: stubResolver{Caller{VMID: "vm1", Profile: "leader"}},
			Registry: reg,
			Limiter:  lim,
			Observe:  func(rec AuditRecord) { audited = append(audited, rec) },
		})
		return g, &audited
	}

	// A 1-token bucket on a frozen clock: the first call per (VM, server) passes,
	// the next gets no refill and is denied.
	frozen := func() RateLimiter {
		lim := NewTokenBucketLimiter(1, 1)
		clock := time.Unix(1000, 0)
		lim.now = func() time.Time { return clock }
		return lim
	}

	t.Run("resources/read consults the limiter", func(t *testing.T) {
		g, audited := build(frozen())
		if r := doRPC(t, g, "resources/read", resourcesReadParams{URI: "mock__file:///a.txt"}); r.Error != nil {
			t.Fatalf("first resources/read should succeed: %v", r.Error)
		}
		r := doRPC(t, g, "resources/read", resourcesReadParams{URI: "mock__file:///a.txt"})
		if r.Error == nil || r.Error.Code != codeInternalError {
			t.Fatalf("second resources/read should be rate-limited, got: %+v", r.Error)
		}
		last := (*audited)[len(*audited)-1]
		if last.OK || last.Err != "rate limited" || last.Kind != "resource" {
			t.Fatalf("rate-limited resource audit wrong: %+v", last)
		}
	})

	t.Run("prompts/get consults the limiter", func(t *testing.T) {
		g, audited := build(frozen())
		if r := doRPC(t, g, "prompts/get", promptsGetParams{Name: "mock__greet"}); r.Error != nil {
			t.Fatalf("first prompts/get should succeed: %v", r.Error)
		}
		r := doRPC(t, g, "prompts/get", promptsGetParams{Name: "mock__greet"})
		if r.Error == nil || r.Error.Code != codeInternalError {
			t.Fatalf("second prompts/get should be rate-limited, got: %+v", r.Error)
		}
		last := (*audited)[len(*audited)-1]
		if last.OK || last.Err != "rate limited" || last.Kind != "prompt" {
			t.Fatalf("rate-limited prompt audit wrong: %+v", last)
		}
	})

	// The bucket is shared across kinds: a tools/call drains the only token, so a
	// following resources/read on the same (VM, server) is already limited — the
	// three call kinds are not each given a separate budget.
	t.Run("bucket is shared across tool/resource/prompt", func(t *testing.T) {
		g, _ := build(frozen())
		if r := doRPC(t, g, "tools/call", toolsCallParams{Name: "mock__echo", Arguments: json.RawMessage(`{}`)}); r.Error != nil {
			t.Fatalf("tools/call should succeed: %v", r.Error)
		}
		if r := doRPC(t, g, "resources/read", resourcesReadParams{URI: "mock__file:///a.txt"}); r.Error == nil {
			t.Fatal("resources/read must be limited: tools/call already drained the shared (VM,server) bucket")
		}
	})
}
