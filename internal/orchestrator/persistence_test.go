package orchestrator

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadFlockMetadata_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	meta := FlockMetadata{
		FlockID:      "flock-rt",
		Task:         "round-trip test",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Agents: map[string]*AgentInfo{
			"worker-1": {AgentID: "worker-1", Role: "worker", VMID: "vm-1", Status: AgentStatusReady},
		},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveFlockMetadata(tmp, meta); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadFlockMetadata(tmp, "flock-rt")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.FlockID != meta.FlockID || loaded.Task != meta.Task {
		t.Errorf("round-trip mismatch: got %+v", loaded)
	}
	if loaded.TenantID != "tenant-1" || loaded.EgressPolicy != "profile" {
		t.Errorf("tenant/egress not preserved: got %q/%q", loaded.TenantID, loaded.EgressPolicy)
	}
	if loaded.Agents["worker-1"].Role != "worker" {
		t.Errorf("agent not preserved: %+v", loaded.Agents)
	}
	if loaded.SchemaVersion != currentSchemaVersion {
		t.Errorf("schema version not set, got %d", loaded.SchemaVersion)
	}
}

func TestListFlockMetadata_SortedByCreatedAt(t *testing.T) {
	tmp := t.TempDir()
	now := time.Now().UTC()
	if err := SaveFlockMetadata(tmp, FlockMetadata{FlockID: "b", CreatedAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := SaveFlockMetadata(tmp, FlockMetadata{FlockID: "a", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := SaveFlockMetadata(tmp, FlockMetadata{FlockID: "c", CreatedAt: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	list, err := ListFlockMetadata(tmp)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(list))
	}
	if list[0].FlockID != "a" || list[1].FlockID != "b" || list[2].FlockID != "c" {
		t.Errorf("not sorted by CreatedAt: %v", list)
	}
}

func TestDeleteFlockMetadata_IdempotentOnMissing(t *testing.T) {
	tmp := t.TempDir()
	if err := DeleteFlockMetadata(tmp, "never-existed"); err != nil {
		t.Errorf("delete on missing should be no-op, got %v", err)
	}
}

func TestFlockManager_LoadFromDisk(t *testing.T) {
	tmp := t.TempDir()

	if err := SaveFlockMetadata(tmp, FlockMetadata{
		FlockID:      "flock-pre",
		Task:         "pre-existing",
		TenantID:     "tenant-pre",
		EgressPolicy: "deny_all",
		Agents:       map[string]*AgentInfo{"r-1": {AgentID: "r-1", Role: "researcher"}},
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	twPath := filepath.Join(tmp, "flocks", "flock-pre", "TOWN_WALL.log")
	if _, err := NewTownWall("flock-pre", twPath); err != nil {
		t.Fatalf("seed townwall: %v", err)
	}

	fm := NewFlockManager(tmp)
	recovered, failed, err := fm.LoadFromDisk()
	if err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	if recovered != 1 {
		t.Errorf("expected 1 recovered flock, got %d", recovered)
	}
	if len(failed) != 0 {
		t.Errorf("expected 0 failures, got %v", failed)
	}
	f, ok := fm.Get("flock-pre")
	if !ok {
		t.Fatal("flock-pre not in registry after LoadFromDisk")
	}
	if f.Task != "pre-existing" {
		t.Errorf("task not restored: %q", f.Task)
	}
	if f.TenantID != "tenant-pre" || f.EgressPolicy != "deny_all" {
		t.Errorf("tenant/egress not restored: got %q/%q", f.TenantID, f.EgressPolicy)
	}
	if f.Agents["r-1"].Role != "researcher" {
		t.Errorf("agent not restored: %+v", f.Agents)
	}
}

func TestFlockManager_LoadFromDisk_EmptyWorkdir(t *testing.T) {
	tmp := t.TempDir()
	fm := NewFlockManager(tmp)
	recovered, failed, err := fm.LoadFromDisk()
	if err != nil {
		t.Fatalf("LoadFromDisk on empty: %v", err)
	}
	if recovered != 0 || len(failed) != 0 {
		t.Errorf("empty workdir should yield (0, [], nil); got (%d, %v)", recovered, failed)
	}
}
