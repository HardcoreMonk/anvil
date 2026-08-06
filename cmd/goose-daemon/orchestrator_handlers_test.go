package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ephemera/internal/orchestrator"
)

// seedFlock registers an empty flock with its Town Wall under a temp dir so the
// KVM-free read/lifecycle handler paths can be exercised without spawning VMs.
func seedFlock(t *testing.T, cp *ControlPlane, flockID, task string) *orchestrator.Flock {
	t.Helper()
	twPath := filepath.Join(t.TempDir(), "TOWN_WALL.log")
	f, err := cp.flockMgr.Create(flockID, task, twPath)
	if err != nil {
		t.Fatalf("seed flock %s: %v", flockID, err)
	}
	return f
}

func TestListFlocks_EmptyReturnsJSONArray(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	cp.handleFlocks(rec, httptest.NewRequest(http.MethodGet, "/flocks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("empty list = %q, want []", got)
	}
}

// TestListFlocks_SerializesAgentsMapAndPaused locks the GET response shape the UI
// depends on: agents is an object keyed by agent_id (not the array POST returns),
// and the runtime-only paused flag is exposed by Flock.MarshalJSON.
func TestListFlocks_SerializesAgentsMapAndPaused(t *testing.T) {
	cp := newMetricsTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: orchestrator.AgentStatusReady})
	f.SetPaused(true)

	rec := httptest.NewRecorder()
	cp.handleFlocks(rec, httptest.NewRequest(http.MethodGet, "/flocks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 flock, got %d", len(list))
	}
	var agents map[string]orchestrator.AgentInfo
	if err := json.Unmarshal(list[0]["agents"], &agents); err != nil {
		t.Fatalf("agents is not an object: %v", err)
	}
	if _, ok := agents["worker-1"]; !ok {
		t.Errorf("agents missing worker-1: %v", agents)
	}
	if string(list[0]["paused"]) != "true" {
		t.Errorf("paused = %s, want true", list[0]["paused"])
	}
}

func TestHandleFlocks_RejectsPut(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	cp.handleFlocks(rec, httptest.NewRequest(http.MethodPut, "/flocks", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestGetFlock_NotFound(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodGet, "/flocks/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestGetFlock_OK(t *testing.T) {
	cp := newMetricsTestCP(t)
	seedFlock(t, cp, "flock-1", "demo")
	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodGet, "/flocks/flock-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"flock_id", "agents", "paused", "created_at"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("response missing %q field", k)
		}
	}
}

func TestDeleteFlock_NotFound(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodDelete, "/flocks/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// TestDeleteFlock_OK deletes a flock with no live VMs: destroyVM's missing-vm
// guard makes member teardown a no-op, so the handler returns 200 and the flock
// leaves the registry.
func TestDeleteFlock_OK(t *testing.T) {
	cp := newMetricsTestCP(t)
	seedFlock(t, cp, "flock-1", "demo")
	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodDelete, "/flocks/flock-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"deleted"`) {
		t.Errorf("expected deleted status, got %s", rec.Body.String())
	}
	if _, ok := cp.flockMgr.Get("flock-1"); ok {
		t.Errorf("flock still registered after delete")
	}
}

// TestPauseResumeFlock_NoVMs exercises the absent-VM skip path: a member whose
// VM is not in cp.vms is skipped, but the flock's runtime paused flag still
// toggles and the handler returns the {status, agents} shape the UI reads.
func TestPauseResumeFlock_NoVMs(t *testing.T) {
	cp := newMetricsTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "worker-1", Role: "worker", VMID: "vm-gone", Status: orchestrator.AgentStatusReady})

	rec := httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodPost, "/flocks/flock-1/pause", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: expected 200, got %d", rec.Code)
	}
	if !f.Paused {
		t.Errorf("flock not marked paused")
	}
	var pr map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil {
		t.Fatalf("decode pause: %v", err)
	}
	if pr["status"] != "paused" {
		t.Errorf("pause status = %v, want paused", pr["status"])
	}

	rec = httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodPost, "/flocks/flock-1/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: expected 200, got %d", rec.Code)
	}
	if f.Paused {
		t.Errorf("flock still paused after resume")
	}
}

func TestPostAndHistory(t *testing.T) {
	cp := newMetricsTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	// The wall's write path authorizes the caller-supplied agent_id against the
	// flock roster (see TestPostToTownWall_RejectsNonMemberAgent), so the happy
	// path needs a real member to post as.
	f.AddAgent(&orchestrator.AgentInfo{AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: orchestrator.AgentStatusReady})

	// Missing body → 400.
	rec := httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodPost, "/flocks/flock-1/post", strings.NewReader(`{"agent_id":"a"}`))
	cp.handleFlockItem(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("post missing body: expected 400, got %d", rec.Code)
	}

	// Valid post → 200 Message.
	rec = httptest.NewRecorder()
	ok := httptest.NewRequest(http.MethodPost, "/flocks/flock-1/post", strings.NewReader(`{"agent_id":"worker-1","body":"hello wall"}`))
	cp.handleFlockItem(rec, ok)
	if rec.Code != http.StatusOK {
		t.Fatalf("post: expected 200, got %d", rec.Code)
	}
	var msg orchestrator.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if msg.Body != "hello wall" || msg.AgentID != "worker-1" {
		t.Errorf("unexpected message %+v", msg)
	}

	// History returns the posted message as a JSON array.
	rec = httptest.NewRecorder()
	cp.handleFlockItem(rec, httptest.NewRequest(http.MethodGet, "/flocks/flock-1/wall/history", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("history: expected 200, got %d", rec.Code)
	}
	var hist []orchestrator.Message
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(hist) != 1 || hist[0].Body != "hello wall" {
		t.Errorf("history = %+v, want 1 message 'hello wall'", hist)
	}
}

// TestStreamTownWall_SSEShape guards the framing contract between the daemon's
// sseEmit ("data: {json}\n\n") and the web client's streamSSE parser. The
// handler replays history before blocking on new messages, so cancelling the
// request context ends the stream after the history frame is written.
func TestStreamTownWall_SSEShape(t *testing.T) {
	cp := newMetricsTestCP(t)
	f := seedFlock(t, cp, "flock-1", "demo")
	if _, err := f.TownWall.Post("worker-1", "hello wall"); err != nil {
		t.Fatalf("seed wall: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/flocks/flock-1/wall", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		cp.handleFlockItem(rec, req)
		close(done)
	}()
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Errorf("SSE body missing 'data: ' prefix:\n%s", body)
	}
	if !strings.Contains(body, "hello wall") {
		t.Errorf("SSE body missing posted message:\n%s", body)
	}
	if !strings.Contains(body, "\n\n") {
		t.Errorf("SSE body missing frame delimiter:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}
