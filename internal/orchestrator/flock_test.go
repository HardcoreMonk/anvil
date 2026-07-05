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

	f, err := fm.Create("flock-1", "test task", "", "", filepath.Join(tmp, "flock-1", "wall.log"))
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

func TestFlockManager_NewUnregisteredIsHiddenUntilRegister(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)

	f, err := fm.NewUnregistered("flock-hidden", "test task", filepath.Join(tmp, "flock-hidden", "wall.log"))
	if err != nil {
		t.Fatalf("NewUnregistered: %v", err)
	}
	if _, ok := fm.Get("flock-hidden"); ok {
		t.Fatal("unregistered flock should not be visible via Get")
	}
	if all := fm.List(); len(all) != 0 {
		t.Fatalf("unregistered flock visible via List: %d", len(all))
	}

	fm.Register(f)
	if got, ok := fm.Get("flock-hidden"); !ok || got != f {
		t.Fatal("registered flock not visible via Get")
	}
}

func TestFlockManager_CreateStoresTenantAndEgress(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)

	f, err := fm.Create("flock-1", "ship review", "tenant-1", "profile", filepath.Join(tmp, "flock-1", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.TenantID != "tenant-1" {
		t.Fatalf("TenantID = %q, want tenant-1", f.TenantID)
	}
	if f.EgressPolicy != "profile" {
		t.Fatalf("EgressPolicy = %q, want profile", f.EgressPolicy)
	}
}

func TestFlock_AddAgentAndStatus(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, _ := fm.Create("flock-x", "task", "", "", filepath.Join(tmp, "wall.log"))

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

// TestFlock_UpdateAgentVM verifies that swapping VM identity preserves the
// agent_id and role while replacing vm_id / agent_url and resetting status
// to ready — the exact contract per-agent restart relies on.
func TestFlock_UpdateAgentVM(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, _ := fm.Create("flock-restart", "task", filepath.Join(tmp, "wall.log"))
	f.AddAgent(&AgentInfo{
		AgentID:  "reviewer-1",
		Role:     "reviewer",
		VMID:     "vm-old",
		AgentURL: "http://10.0.1.5:8080",
		Status:   AgentStatusDead,
	})

	f.UpdateAgentVM("reviewer-1", "vm-new", "http://10.0.1.6:8080")

	snap := f.Snapshot()
	if snap[0].VMID != "vm-new" {
		t.Errorf("VMID not swapped: %q", snap[0].VMID)
	}
	if snap[0].AgentURL != "http://10.0.1.6:8080" {
		t.Errorf("AgentURL not swapped: %q", snap[0].AgentURL)
	}
	if snap[0].Status != AgentStatusReady {
		t.Errorf("status should reset to ready, got %q", snap[0].Status)
	}
	if snap[0].Role != "reviewer" || snap[0].AgentID != "reviewer-1" {
		t.Errorf("role/agent_id changed unexpectedly: %+v", snap[0])
	}

	// Unknown agent ID must be a no-op (parity with UpdateAgentStatus).
	f.UpdateAgentVM("nobody", "vm-x", "http://x")
	if len(f.Snapshot()) != 1 {
		t.Error("UpdateAgentVM should not create new agents")
	}
}

func TestFlock_MarshalJSON(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, _ := fm.Create("flock-j", "json task", "tenant-json", "deny_all", filepath.Join(tmp, "wall.log"))
	f.AddAgent(&AgentInfo{AgentID: "a-1", Role: "worker", VMID: "vm-1", Status: AgentStatusReady})

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"flock_id":"flock-j"`, `"task":"json task"`, `"tenant_id":"tenant-json"`, `"egress_policy":"deny_all"`, `"agent_id":"a-1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("Marshal output missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, `"TownWall"`) || strings.Contains(s, `"townwall"`) {
		t.Errorf("MarshalJSON should not expose TownWall: %s", s)
	}
}

func TestFlock_RemoveAgent(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, err := fm.Create("flock-rm", "task", filepath.Join(tmp, "flock-rm", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.AddAgent(&AgentInfo{AgentID: "worker-1", Role: "worker"})
	f.AddAgent(&AgentInfo{AgentID: "worker-2", Role: "worker"})

	f.RemoveAgent("worker-1")
	if n := len(f.Snapshot()); n != 1 {
		t.Errorf("expected 1 agent after remove, got %d", n)
	}
	for _, a := range f.Snapshot() {
		if a.AgentID == "worker-1" {
			t.Error("worker-1 should be removed")
		}
	}
	f.RemoveAgent("nonexistent") // no-op, must not panic
}

func TestFlock_ChangeAgentRole(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, err := fm.Create("flock-role", "task", filepath.Join(tmp, "flock-role", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.AddAgent(&AgentInfo{AgentID: "worker-1", Role: "worker"})

	f.ChangeAgentRole("worker-1", "reviewer", "reviewer")
	role := ""
	for _, a := range f.Snapshot() {
		if a.AgentID == "worker-1" {
			role = a.Role
		}
	}
	if role != "reviewer" {
		t.Errorf("role not changed: %q", role)
	}
	f.ChangeAgentRole("nonexistent", "x", "x") // no-op, must not panic
}

func TestFlock_PausedAndAgentStatus(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, err := fm.Create("flock-p", "task", filepath.Join(tmp, "flock-p", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.AddAgent(&AgentInfo{AgentID: "worker-1", Role: "worker", Status: AgentStatusReady})

	if f.Paused {
		t.Error("new flock should not be paused")
	}
	f.SetPaused(true)
	if !f.Paused {
		t.Error("SetPaused(true) had no effect")
	}
	f.MarkAgentPaused("worker-1")
	if got := f.AgentStatus("worker-1"); got != AgentStatusPaused {
		t.Errorf("AgentStatus = %q, want %q", got, AgentStatusPaused)
	}
	if got := f.AgentStatus("nobody"); got != "" {
		t.Errorf("AgentStatus(unknown) = %q, want empty", got)
	}
	meta := f.ToMetadata()
	if got := meta.Agents["worker-1"].Status; got != AgentStatusReady {
		t.Errorf("ToMetadata paused status = %q, want pre-pause ready", got)
	}
	f.RestorePausedAgentStatus("worker-1", AgentStatusReady)
	if got := f.AgentStatus("worker-1"); got != AgentStatusReady {
		t.Errorf("RestorePausedAgentStatus = %q, want ready", got)
	}
	if !f.MarkAgentDeadIfNotPaused("worker-1") {
		t.Fatal("MarkAgentDeadIfNotPaused returned false for ready agent")
	}
	if got := f.AgentStatus("worker-1"); got != AgentStatusDead {
		t.Errorf("MarkAgentDeadIfNotPaused status = %q, want dead", got)
	}
	f.MarkAgentPaused("worker-1")
	if f.MarkAgentDeadIfNotPaused("worker-1") {
		t.Fatal("MarkAgentDeadIfNotPaused should reject paused agent")
	}
	f.SetPaused(false)
	if f.Paused {
		t.Error("SetPaused(false) had no effect")
	}
}

func TestFlock_BeginDeleteRejectsFutureMutation(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, err := fm.Create("flock-delete", "task", filepath.Join(tmp, "flock-delete", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	unlockDelete, ok := f.BeginDelete()
	if !ok {
		t.Fatal("BeginDelete unexpectedly failed")
	}
	unlockDelete()

	if unlock, ok := f.BeginMutation(); ok {
		unlock()
		t.Fatal("BeginMutation succeeded after BeginDelete")
	}
	if unlock, ok := f.BeginDelete(); ok {
		unlock()
		t.Fatal("second BeginDelete unexpectedly succeeded")
	}
}

func TestFlock_ToMetadataOmitsReservedPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	f, err := fm.Create("flock-reserve", "task", filepath.Join(tmp, "flock-reserve", "wall.log"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := f.ReserveAgent("worker", 2); err != nil {
		t.Fatalf("ReserveAgent: %v", err)
	}
	meta := f.ToMetadata()
	if len(meta.Agents) != 0 {
		t.Fatalf("reserved placeholder persisted in metadata: %+v", meta.Agents)
	}
}
