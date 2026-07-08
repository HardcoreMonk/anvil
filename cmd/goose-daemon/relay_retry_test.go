package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRT returns scripted results per attempt and counts calls.
type fakeRT struct {
	calls   atomic.Int32
	results []func() (*http.Response, error)
}

func (f *fakeRT) RoundTrip(*http.Request) (*http.Response, error) {
	n := int(f.calls.Add(1)) - 1
	if n >= len(f.results) {
		n = len(f.results) - 1
	}
	return f.results[n]()
}

func okResp() (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func dialErr() (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}
}

func resetErr() (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: "http://x", Err: &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}}
}

func buildReq(t *testing.T, ctx context.Context) func() (*http.Request, error) {
	t.Helper()
	return func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, "http://home.invalid/flocks/f/post", strings.NewReader(`{}`))
	}
}

func noSleep(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func TestIsDialError(t *testing.T) {
	if _, err := dialErr(); !isDialError(err) {
		t.Fatal("wrapped dial OpError must classify as dial error")
	}
	if _, err := resetErr(); isDialError(err) {
		t.Fatal("read/reset OpError must NOT classify as dial error")
	}
	if isDialError(nil) || isDialError(errors.New("boom")) {
		t.Fatal("nil/plain errors must not classify as dial error")
	}
}

func TestDoWithDialRetry_RetriesDialThenSucceeds(t *testing.T) {
	old := relayRetrySleep
	relayRetrySleep = noSleep
	defer func() { relayRetrySleep = old }()

	rt := &fakeRT{results: []func() (*http.Response, error){dialErr, okResp}}
	client := &http.Client{Transport: rt}
	ctx := context.Background()
	resp, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v, want 200 after one retry", resp, err)
	}
	if got := rt.calls.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestDoWithDialRetry_StopsAtAttemptCap(t *testing.T) {
	old := relayRetrySleep
	relayRetrySleep = noSleep
	defer func() { relayRetrySleep = old }()

	rt := &fakeRT{results: []func() (*http.Response, error){dialErr}}
	client := &http.Client{Transport: rt}
	ctx := context.Background()
	_, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err == nil {
		t.Fatal("want error after exhausting attempts")
	}
	if got := rt.calls.Load(); got != int32(relayRetryAttempts) {
		t.Fatalf("attempts = %d, want %d", got, relayRetryAttempts)
	}
}

func TestDoWithDialRetry_NoRetryOnHTTPResponse(t *testing.T) {
	rt := &fakeRT{results: []func() (*http.Response, error){func() (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("boom"))}, nil
	}}}
	client := &http.Client{Transport: rt}
	ctx := context.Background()
	resp, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err != nil || resp.StatusCode != 500 {
		t.Fatalf("resp=%v err=%v, want the 500 passed through", resp, err)
	}
	if got := rt.calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (HTTP responses never retry)", got)
	}
}

func TestDoWithDialRetry_NoRetryOnResetError(t *testing.T) {
	rt := &fakeRT{results: []func() (*http.Response, error){resetErr}}
	client := &http.Client{Transport: rt}
	ctx := context.Background()
	_, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err == nil {
		t.Fatal("want the reset error surfaced")
	}
	if got := rt.calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (post-connect failures never retry)", got)
	}
}

func TestDoWithDialRetry_CtxCancelAbortsBackoff(t *testing.T) {
	rt := &fakeRT{results: []func() (*http.Response, error){dialErr}}
	client := &http.Client{Transport: rt}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 이미 취소된 ctx — 첫 dial 실패 후 backoff에서 즉시 중단해야 함
	_, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err == nil {
		t.Fatal("want error")
	}
	if got := rt.calls.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (cancelled ctx must abort backoff)", got)
	}
}

// TestDoWithDialRetry_SleepAbortReturnsLastDialErr directly exercises the
// sleep-abort branch in doWithDialRetry (`if err := relayRetrySleep(...); err
// != nil { return nil, lastErr }`), which TestDoWithDialRetry_CtxCancelAbortsBackoff
// does NOT reach: that test cancels ctx before the first call, so attempt 0's
// post-dial `ctx.Err() != nil` check returns first and relayRetrySleep is
// never invoked. Here ctx starts live; the fake sleep hook cancels ctx itself
// and returns ctx.Err() when it is called ahead of attempt 1's retry, forcing
// the sleep-abort path to run for the first time. The retry must surface the
// ORIGINAL dial error from attempt 0 (lastErr), not the sleep's ctx.Err().
func TestDoWithDialRetry_SleepAbortReturnsLastDialErr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	old := relayRetrySleep
	relayRetrySleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	defer func() { relayRetrySleep = old }()

	rt := &fakeRT{results: []func() (*http.Response, error){dialErr}}
	client := &http.Client{Transport: rt}

	_, err := doWithDialRetry(ctx, client, buildReq(t, ctx))
	if err == nil {
		t.Fatal("want the original dial error surfaced")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("returned err = %v, want the attempt-0 dial error, not ctx.Err() from the aborted sleep", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("returned err = %v, want the attempt-0 dial error (connection refused)", err)
	}
	if got := rt.calls.Load(); got != 1 {
		t.Fatalf("RoundTrip calls = %d, want 1 (attempt 1 must abort before a second dial)", got)
	}
}
