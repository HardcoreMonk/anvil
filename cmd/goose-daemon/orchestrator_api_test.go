package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ephemera/internal/orchestrator"
)

// TestNextAgentID covers the per-role "<role>-N" id allocation used by
// addFlockAgent (v0.4.3): next = max(existing N for that role) + 1, and a role
// with no existing agents starts at 1.
func TestNextAgentID(t *testing.T) {
	f := &orchestrator.Flock{Agents: map[string]*orchestrator.AgentInfo{
		"worker-1":   {AgentID: "worker-1", Role: "worker"},
		"worker-3":   {AgentID: "worker-3", Role: "worker"},
		"reviewer-1": {AgentID: "reviewer-1", Role: "reviewer"},
	}}
	cases := map[string]string{
		"worker":   "worker-4", // max(1,3)+1
		"reviewer": "reviewer-2",
		"builder":  "builder-1", // none yet
	}
	for role, want := range cases {
		if got := nextAgentID(f, role); got != want {
			t.Errorf("nextAgentID(%q) = %q, want %q", role, got, want)
		}
	}
}

// TestFlockMax covers the per-flock cap fallback (v0.4.3): 0/unset → default,
// otherwise the flock's own MaxAgents.
func TestFlockMax(t *testing.T) {
	f := &orchestrator.Flock{}
	if got := flockMax(f); got != defaultMaxAgentsPerFlock {
		t.Errorf("flockMax(unset) = %d, want %d", got, defaultMaxAgentsPerFlock)
	}
	f.MaxAgents = 5
	if got := flockMax(f); got != 5 {
		t.Errorf("flockMax(5) = %d, want 5", got)
	}
}

func TestFlockAddAgentResponseOmitsAgentTokenFields(t *testing.T) {
	resp := FlockAddAgentResponse{
		AgentID:  "worker-2",
		Role:     "worker",
		VMID:     "vm-added",
		AgentURL: "http://127.0.0.1:8080",
	}
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusCreated, resp)

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response JSON did not decode: %v", err)
	}
	for _, key := range []string{"agent_id", "role", "vm_id", "agent_url"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("response missing %q: %s", key, rr.Body.String())
		}
	}
	for _, key := range []string{"agent_token", "agent_tokens"} {
		if _, ok := body[key]; ok {
			t.Fatalf("response exposed %q: %s", key, rr.Body.String())
		}
	}
}

// TestFilterTownWall covers the v0.4.3 wall/history query filters.
func TestFilterTownWall(t *testing.T) {
	msgs := []orchestrator.Message{
		{AgentID: "worker-1", Timestamp: "2026-05-28T10:00:00Z", Body: "hello world"},
		{AgentID: "worker-2", Timestamp: "2026-05-28T11:00:00Z", Body: "goodbye"},
		{AgentID: "worker-1", Timestamp: "2026-05-28T12:00:00Z", Body: "world again"},
	}
	if got := filterTownWall(msgs, "", "", "", ""); len(got) != 3 {
		t.Errorf("no filter: got %d, want 3", len(got))
	}
	if got := filterTownWall(msgs, "worker-1", "", "", ""); len(got) != 2 {
		t.Errorf("agent_id: got %d, want 2", len(got))
	}
	if got := filterTownWall(msgs, "", "", "", "world"); len(got) != 2 {
		t.Errorf("contains: got %d, want 2", len(got))
	}
	if got := filterTownWall(msgs, "", "2026-05-28T10:30:00Z", "2026-05-28T11:30:00Z", ""); len(got) != 1 {
		t.Errorf("since/until: got %d, want 1", len(got))
	}
	if got := filterTownWall(msgs, "worker-1", "", "", "again"); len(got) != 1 {
		t.Errorf("agent_id+contains: got %d, want 1", len(got))
	}
}
