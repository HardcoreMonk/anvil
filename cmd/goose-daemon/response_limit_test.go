package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ephemera/internal/orchestrator"
)

// floodBody writes exactly n bytes, streaming from a small repeated chunk so a
// multi-MiB response costs no multi-MiB allocation on the server side. Write
// errors are ignored: once the daemon stops reading at its cap the connection
// tears down mid-write, which is precisely the behavior under test.
func floodBody(w http.ResponseWriter, n int64) {
	chunk := strings.Repeat("x", 32*1024)
	for n > 0 {
		s := int64(len(chunk))
		if n < s {
			s = n
		}
		if _, err := io.WriteString(w, chunk[:s]); err != nil {
			return
		}
		n -= s
	}
}

// floodServer answers every request with status and exactly n bytes.
func floodServer(t *testing.T, status int, n int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		floodBody(w, n)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestReadCapped_Semantics pins the shared helper: at or below the cap the body
// is returned whole and untruncated; one byte over, the caller is told so and
// gets exactly cap bytes (never more).
func TestReadCapped_Semantics(t *testing.T) {
	const cap = 16
	cases := []struct {
		name          string
		size          int
		wantTruncated bool
		wantLen       int
	}{
		{"under cap", 5, false, 5},
		{"exactly cap", cap, false, cap},
		{"one over cap", cap + 1, true, cap},
		{"far over cap", cap * 100, true, cap},
	}
	for _, tc := range cases {
		b, truncated, err := readCapped(strings.NewReader(strings.Repeat("x", tc.size)), cap)
		if err != nil {
			t.Fatalf("%s: readCapped: %v", tc.name, err)
		}
		if truncated != tc.wantTruncated {
			t.Errorf("%s: truncated = %v, want %v", tc.name, truncated, tc.wantTruncated)
		}
		if len(b) != tc.wantLen {
			t.Errorf("%s: len = %d, want %d", tc.name, len(b), tc.wantLen)
		}
	}
}

// TestRelayTownWallPost_RejectsOversizeHomeResponse: a member daemon relays a
// guest's wall post to home and mirrors home's response. A hostile or broken
// home streaming an unbounded body must not be buffered whole into the root
// daemon's heap. The echoed record is a PAYLOAD, so an over-cap response is an
// error (502), never a silently truncated mirror.
func TestRelayTownWallPost_RejectsOversizeHomeResponse(t *testing.T) {
	home := floodServer(t, http.StatusOK, maxWallMessageBody+1)
	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	rec := postWall(t, cp, "routed-1", `{"agent_id":"researcher-1","body":"hi"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("oversize home response = %d, want 502 (daemon buffered it instead of capping)", rec.Code)
	}
	if int64(rec.Body.Len()) > maxWallMessageBody {
		t.Fatalf("daemon mirrored %d bytes, want a small error body", rec.Body.Len())
	}
}

// TestRelayTownWallPost_AcceptsResponseAtCap brackets the threshold from below:
// a response exactly at the cap is legitimate and must pass through whole.
func TestRelayTownWallPost_AcceptsResponseAtCap(t *testing.T) {
	home := floodServer(t, http.StatusOK, maxWallMessageBody)
	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	rec := postWall(t, cp, "routed-1", `{"agent_id":"researcher-1","body":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("at-cap home response = %d, want 200 (cap rejects a legitimate body)", rec.Code)
	}
	if int64(rec.Body.Len()) != maxWallMessageBody {
		t.Fatalf("mirrored %d bytes, want %d (body was truncated at the cap)", rec.Body.Len(), maxWallMessageBody)
	}
}

// TestTownWallHistory_RejectsOversizeHomeResponse: the relayed history read is
// the largest legitimate cross-daemon payload (a whole Town Wall), so it gets
// the largest cap — but it is still a cap.
func TestTownWallHistory_RejectsOversizeHomeResponse(t *testing.T) {
	home := floodServer(t, http.StatusOK, maxWallHistoryBody+1)
	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodGet, "/flocks/routed-1/wall/history", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("oversize home history = %d, want 502 (daemon buffered it instead of capping)", rec.Code)
	}
	if int64(rec.Body.Len()) > maxWallHistoryBody {
		t.Fatalf("daemon mirrored %d bytes, want a small error body", rec.Body.Len())
	}
}

// TestStreamTownWallRelay_TruncatesOversizeErrorBody: the SSE relay's non-200
// branch reads only an error SNIPPET. Here truncation is the right behavior —
// the status code carries the meaning, so the caller still gets its 401 rather
// than having the refusal masked by a 502.
func TestStreamTownWallRelay_TruncatesOversizeErrorBody(t *testing.T) {
	home := floodServer(t, http.StatusUnauthorized, maxPeerErrorBody*4)
	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "", nil)

	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodGet, "/flocks/routed-1/wall", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("mirrored status = %d, want 401", rec.Code)
	}
	if int64(rec.Body.Len()) > maxPeerErrorBody {
		t.Fatalf("mirrored error body = %d bytes, want <= %d (snippet not capped)", rec.Body.Len(), maxPeerErrorBody)
	}
}

// TestDispatchBroadcastTask_RejectsOversizeAgentResponse: a guest that points
// its agent port at a server streaming an unbounded body must not inflate the
// root daemon's RSS through the broadcast fan-out.
func TestDispatchBroadcastTask_RejectsOversizeAgentResponse(t *testing.T) {
	agent := floodServer(t, http.StatusOK, maxAgentResponseBody+1)
	host, port := splitHostPort(t, agent.URL)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	cp := newMetricsTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: orchestrator.AgentStatusReady})
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{VMID: "vm-1", GuestIP: host}}

	rec := httptest.NewRecorder()
	cp.broadcastFlock(rec, httptest.NewRequest(http.MethodPost, "/flocks/flock-1/broadcast", strings.NewReader(`{"body":"do it"}`)), "flock-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("broadcast = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if int64(rec.Body.Len()) > maxAgentResponseBody {
		t.Fatalf("broadcast response = %d bytes, want a small error result", rec.Body.Len())
	}
	var resp struct {
		Failed  int                        `json:"failed"`
		Results map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode broadcast: %v", err)
	}
	if resp.Failed != 1 {
		t.Fatalf("failed = %d, want 1 (over-cap agent response must not count as success)", resp.Failed)
	}
}

// TestDispatchFlockCall_RejectsOversizeAgentResponse: the /call local dispatch
// mirrors the agent's response verbatim. A payload over the cap is an error
// (502), not a truncated mirror — a silently cut task result is an integrity
// failure the caller cannot detect.
func TestDispatchFlockCall_RejectsOversizeAgentResponse(t *testing.T) {
	agent := floodServer(t, http.StatusOK, maxAgentResponseBody+1)
	host, port := splitHostPort(t, agent.URL)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	cp := newTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "researcher-1", Role: "researcher", VMID: "vm-1", Status: orchestrator.AgentStatusReady})
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{VMID: "vm-1", GuestIP: host}}

	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodPost, "/flocks/flock-1/call",
		strings.NewReader(`{"agent_id":"researcher-1","prompt":"ping"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("oversize agent response = %d, want 502 (%d bytes mirrored)", rec.Code, rec.Body.Len())
	}
	if int64(rec.Body.Len()) > maxAgentResponseBody {
		t.Fatalf("daemon mirrored %d bytes, want a small error body", rec.Body.Len())
	}
}

// TestDispatchFlockCall_AcceptsResponseAtCap brackets the /call threshold from
// below: an agent response exactly at the cap must pass through whole.
func TestDispatchFlockCall_AcceptsResponseAtCap(t *testing.T) {
	agent := floodServer(t, http.StatusOK, maxAgentResponseBody)
	host, port := splitHostPort(t, agent.URL)
	oldPort := agentPort
	agentPort = port
	defer func() { agentPort = oldPort }()

	cp := newTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "researcher-1", Role: "researcher", VMID: "vm-1", Status: orchestrator.AgentStatusReady})
	cp.vms["vm-1"] = &runningVM{VMInfo: VMInfo{VMID: "vm-1", GuestIP: host}}

	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodPost, "/flocks/flock-1/call",
		strings.NewReader(`{"agent_id":"researcher-1","prompt":"ping"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("at-cap agent response = %d, want 200 (cap rejects a legitimate body)", rec.Code)
	}
	if int64(rec.Body.Len()) != maxAgentResponseBody {
		t.Fatalf("mirrored %d bytes, want %d (body was truncated at the cap)", rec.Body.Len(), maxAgentResponseBody)
	}
}

// TestForwardFlockCall_RejectsOversizePeerResponse: the daemon-to-daemon /call
// hop wraps a remote agent's task result, so it carries the same payload cap as
// the local dispatch and fails closed rather than mirroring a truncated body.
func TestForwardFlockCall_RejectsOversizePeerResponse(t *testing.T) {
	home := floodServer(t, http.StatusOK, maxAgentResponseBody+1)
	cp := newTestCP(t)
	cp.flockMgr.RegisterRelay("routed-1", home.URL, "rt-1", "ct-1", nil)

	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodPost, "/flocks/routed-1/call",
		strings.NewReader(`{"agent_id":"researcher-1","prompt":"ping"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("oversize peer /call response = %d, want 502 (%d bytes mirrored)", rec.Code, rec.Body.Len())
	}
	if int64(rec.Body.Len()) > maxAgentResponseBody {
		t.Fatalf("daemon mirrored %d bytes, want a small error body", rec.Body.Len())
	}
}
