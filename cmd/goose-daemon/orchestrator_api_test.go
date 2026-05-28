package main

import (
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
