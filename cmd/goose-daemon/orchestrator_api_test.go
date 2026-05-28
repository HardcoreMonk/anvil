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
