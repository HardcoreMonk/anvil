package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostToTownWall_RelayForwardsToHome(t *testing.T) {
	var gotAuth, gotBody string
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"seq":1,"agent_id":"researcher-1","body":"hi"}`))
	}))
	defer home.Close()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/post", strings.NewReader(`{"agent_id":"researcher-1","body":"hi"}`))
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("relay post status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer rt-1" {
		t.Fatalf("home saw auth %q, want Bearer rt-1", gotAuth)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("home body not json: %v", err)
	}
	if payload["agent_id"] != "researcher-1" || payload["body"] != "hi" {
		t.Fatalf("relayed body = %s, want only agent_id+body", gotBody)
	}
	if _, leaked := payload["agent_token"]; leaked {
		t.Fatalf("relay leaked agent_token")
	}
}

func TestTownWallHistory_RelayProxiesToHome(t *testing.T) {
	const arr = `[{"seq":1,"agent_id":"researcher-1","body":"hi"}]`
	var gotAuth, gotPath, gotQuery string
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(arr))
	}))
	defer home.Close()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/flocks/routed-1/wall/history?agent_id=researcher-1", nil)
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("relay history status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer rt-1" {
		t.Fatalf("home saw auth %q, want Bearer rt-1", gotAuth)
	}
	if gotPath != "/flocks/routed-1/wall/history" {
		t.Fatalf("home saw path %q, want /flocks/routed-1/wall/history", gotPath)
	}
	if gotQuery != "agent_id=researcher-1" {
		t.Fatalf("home saw query %q, want agent_id=researcher-1 (query not forwarded)", gotQuery)
	}
	if strings.TrimSpace(rr.Body.String()) != arr {
		t.Fatalf("relay returned %q, want %q", rr.Body.String(), arr)
	}
}

// TestPostToTownWall_RelayHonorsCallerContext proves the relay post hop rides the
// caller's request context: once that context is cancelled (client disconnect,
// deadline) the outbound Do fails immediately and the handler returns 502 rather
// than reaching a live home. The stub home would answer 200, so a 200 here would
// mean the relay ignored r.Context() and issued a background request.
func TestPostToTownWall_RelayHonorsCallerContext(t *testing.T) {
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"seq":1,"agent_id":"researcher-1","body":"hi"}`))
	}))
	defer home.Close()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the relay hop

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/post", strings.NewReader(`{"agent_id":"researcher-1","body":"hi"}`)).WithContext(ctx)
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("relay post with cancelled caller context = %d, want 502 (relay did not honor r.Context())", rr.Code)
	}
}

// TestTownWallHistory_RelayHonorsCallerContext is the history-read counterpart:
// a cancelled caller context must tear down the relayed history read to home too.
func TestTownWallHistory_RelayHonorsCallerContext(t *testing.T) {
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer home.Close()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/flocks/routed-1/wall/history", nil).WithContext(ctx)
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("relay history with cancelled caller context = %d, want 502 (relay did not honor r.Context())", rr.Code)
	}
}

// TestStreamTownWall_RelayDialsHomeWithToken is the focused SSE test: a relay
// flock's /wall stream dials the home daemon's /wall with the relay token and
// copies the home's SSE frames back to the caller. The stub home writes one
// frame then returns (closing the stream) so the relay's copy loop terminates.
func TestStreamTownWall_RelayDialsHomeWithToken(t *testing.T) {
	var gotAuth, gotPath string
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: {\"seq\":1,\"agent_id\":\"researcher-1\",\"body\":\"hi\"}\n\n"))
	}))
	defer home.Close()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/flocks/routed-1/wall", nil)
	cp.handleFlockItem(rr, req)

	if gotAuth != "Bearer rt-1" {
		t.Fatalf("home saw auth %q, want Bearer rt-1", gotAuth)
	}
	if gotPath != "/flocks/routed-1/wall" {
		t.Fatalf("home saw path %q, want /flocks/routed-1/wall", gotPath)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("relay Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rr.Body.String(), "data: ") {
		t.Fatalf("relay SSE body missing 'data: ' frame:\n%s", rr.Body.String())
	}
}

// TestPostToTownWall_RelayRetriesDialFailure proves the relay post hop
// retries a dial-class failure within the same caller request instead of
// failing on the first attempt (Task 2 of the bounded relay retry slice).
// The relay flock's HomeAddr points at a port that is closed at request
// start (guaranteed dial failure on attempt 1); relayRetrySleep is swapped
// for a hook that starts the real home stub listening on that exact freed
// address before returning, so attempt 2 lands on a live server.
// Deterministic (no wall-clock backoff, no port race window beyond the
// hook's own re-listen): the hook counts its own calls too, so a single
// invocation proves exactly one retry occurred (not an exhaustion-then-lucky
// pass). Home must see exactly one POST — a retry must never duplicate a
// wall post.
func TestPostToTownWall_RelayRetriesDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserved listener (guarantee attempt 1 dial failure): %v", err)
	}

	var homeCalls int32
	var gotAuth, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&homeCalls, 1)
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"seq":1,"agent_id":"researcher-1","body":"hi"}`))
	})

	var sleepCalls int32
	oldSleep := relayRetrySleep
	relayRetrySleep = func(ctx context.Context, d time.Duration) error {
		if atomic.AddInt32(&sleepCalls, 1) == 1 {
			hln, err := net.Listen("tcp", addr)
			if err != nil {
				t.Fatalf("re-listen on freed port %s for attempt 2: %v", addr, err)
			}
			go func() { _ = http.Serve(hln, mux) }()
			t.Cleanup(func() { _ = hln.Close() })
		}
		return noSleep(ctx, d)
	}
	defer func() { relayRetrySleep = oldSleep }()

	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", "http://"+addr, "rt-1", "", nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/flocks/routed-1/post", strings.NewReader(`{"agent_id":"researcher-1","body":"hi"}`))
	cp.handleFlockItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("relay post retry status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt32(&sleepCalls); got != 1 {
		t.Fatalf("relayRetrySleep called %d times, want 1 (exactly one retry before success)", got)
	}
	if got := atomic.LoadInt32(&homeCalls); got != 1 {
		t.Fatalf("home received %d requests, want exactly 1 (retry must not duplicate the post)", got)
	}
	if gotAuth != "Bearer rt-1" {
		t.Fatalf("home saw auth %q, want Bearer rt-1", gotAuth)
	}
	if !strings.Contains(gotBody, "researcher-1") {
		t.Fatalf("home saw body %q, want it to contain researcher-1", gotBody)
	}
}
