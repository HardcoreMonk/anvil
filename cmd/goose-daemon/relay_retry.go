package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Bounded synchronous retry for daemon-to-daemon relay hops (wall post/history
// relay, call forwards). Retries ONLY dial-class transport failures — the one
// case where the request provably never reached the peer — so a retry can
// never duplicate a wall post or double-execute a call prompt. HTTP responses
// (any status) are the peer's answer and are returned as-is; post-connect
// failures (reset/EOF) may have been processed and are surfaced immediately.
// Policy is fixed (no env): 3 total attempts, 1s then 2s ctx-aware backoff —
// well inside the guest 300s > member 290s > hub 280s timeout ladder.
const relayRetryAttempts = 3

var relayRetryBackoff = []time.Duration{time.Second, 2 * time.Second}

// relayRetrySleep is swappable in tests.
var relayRetrySleep = sleepCtx

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// isDialError reports whether err is a dial-phase failure (connection never
// established). It unwraps url.Error/OpError chains.
func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// doWithDialRetry executes build()+Do up to relayRetryAttempts times,
// rebuilding the request each attempt (bodies are single-use readers).
func doWithDialRetry(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < relayRetryAttempts; attempt++ {
		if attempt > 0 {
			// attempt-1 clamped to the last backoff step — future attempt-count bumps beyond len(relayRetryBackoff) reuse the final delay instead of panicking.
			if err := relayRetrySleep(ctx, relayRetryBackoff[min(attempt-1, len(relayRetryBackoff)-1)]); err != nil {
				return nil, lastErr
			}
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isDialError(err) || ctx.Err() != nil {
			return nil, err
		}
		if attempt < relayRetryAttempts-1 {
			// URL/address/token deliberately omitted — this helper has no flock/host
			// context; the final failure's error message (built by the caller) carries
			// identifiers instead. Only the attempt number is safe to log here.
			slog.Warn("relay hop dial failed, retrying", "attempt", attempt+1)
		}
	}
	return nil, lastErr
}
