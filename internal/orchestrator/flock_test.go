package orchestrator

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlockManager_CreateGetDelete(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)

	f, err := fm.Create("flock-1", "test task", filepath.Join(tmp, "flock-1", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.ID != "flock-1" {
		t.Errorf("unexpected ID: %q", f.ID)
	}

	got, ok := fm.Get("flock-1")
	if !ok || got != f {
		t.Error("Get should return the same flock instance")
	}

	if all := fm.List(); len(all) != 1 {
		t.Errorf("List should return 1 flock, got %d", len(all))
	}

	deleted, ok := fm.Delete("flock-1")
	if !ok || deleted != f {
		t.Error("Delete should return the removed flock")
	}
	if _, ok := fm.Get("flock-1"); ok {
		t.Error("Get should miss after Delete")
	}
}

func TestFlock_AddAgentAndStatus(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, _ := fm.Create("flock-x", "task", filepath.Join(tmp, "wall.log"))

	f.AddAgent(&AgentInfo{
		AgentID: "researcher-1",
		Role:    "researcher",
		VMID:    "vm-1",
		Status:  AgentStatusSpawning,
	})
	f.UpdateAgentStatus("researcher-1", AgentStatusReady)

	snap := f.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(snap))
	}
	if snap[0].Status != AgentStatusReady {
		t.Errorf("expected status %q, got %q", AgentStatusReady, snap[0].Status)
	}
}

func TestFlock_MarshalJSON(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, _ := fm.Create("flock-j", "json task", filepath.Join(tmp, "wall.log"))
	f.AddAgent(&AgentInfo{AgentID: "a-1", Role: "worker", VMID: "vm-1", Status: AgentStatusReady})

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"flock_id":"flock-j"`, `"task":"json task"`, `"agent_id":"a-1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("Marshal output missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, `"TownWall"`) || strings.Contains(s, `"townwall"`) {
		t.Errorf("MarshalJSON should not expose TownWall: %s", s)
	}
}
