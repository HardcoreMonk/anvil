package orchestrator

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// Agent lifecycle states recorded in AgentInfo.Status.
const (
	AgentStatusSpawning = "spawning"
	AgentStatusReady    = "ready"
	AgentStatusBusy     = "busy"
	AgentStatusDone     = "done"
	AgentStatusDead     = "dead" // assigned by the health watchdog after consecutive probe failures
)

// AgentInfo is the per-agent record exposed via flock APIs.
type AgentInfo struct {
	AgentID  string `json:"agent_id"` // e.g. "researcher-1"
	Role     string `json:"role"`     // "researcher" | "worker" | "reviewer" | ...
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
	Status   string `json:"status"`
}

// Flock is a named group of agents sharing one Town Wall.
type Flock struct {
	mu sync.RWMutex
	// writeMu serializes metadata.json writes against any concurrent Persist
	// caller (createFlock, watchdog.onFailure, recovery.markFlockAgentDead,
	// per-agent restart). Held only for the duration of ToMetadata + tmp+rename
	// inside Persist; never overlaps with the per-agent f.mu critical sections.
	writeMu      sync.Mutex
	ID           string                `json:"flock_id"`
	Task         string                `json:"task"`
	TenantID     string                `json:"tenant_id,omitempty"`
	EgressPolicy string                `json:"egress_policy,omitempty"`
	Agents       map[string]*AgentInfo `json:"agents"`
	TownWall     *TownWall             `json:"-"`
	CreatedAt    time.Time             `json:"created_at"`
}

// AddAgent inserts or replaces an agent record under lock.
func (f *Flock) AddAgent(a *AgentInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Agents[a.AgentID] = a
}

// UpdateAgentStatus updates the status of an existing agent.
// No-op when the agent ID is unknown.
func (f *Flock) UpdateAgentStatus(agentID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.Agents[agentID]; ok {
		a.Status = status
	}
}

// UpdateAgentVM swaps the VM identity of an existing agent (vm_id / agent_url)
// and resets status to ready. Used by per-agent restart so a single failed
// agent can be replaced without recreating the whole flock; the AgentID and
// Role stay unchanged so callers can keep addressing the same agent slot.
// No-op when the agent ID is unknown.
func (f *Flock) UpdateAgentVM(agentID, newVMID, newAgentURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.Agents[agentID]; ok {
		a.VMID = newVMID
		a.AgentURL = newAgentURL
		a.Status = AgentStatusReady
	}
}

// Persist atomically writes the flock's current metadata to disk. Holds
// writeMu so concurrent callers (createFlock, watchdog, recovery) cannot
// race the tmp+rename inside SaveFlockMetadata. All flock metadata writers
// MUST go through this helper rather than calling SaveFlockMetadata
// directly, otherwise the serialization invariant is lost.
func (f *Flock) Persist(workDir string) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return SaveFlockMetadata(workDir, f.ToMetadata())
}

// Snapshot returns a defensive copy of the agent map for safe iteration
// outside of the flock's lock.
func (f *Flock) Snapshot() []*AgentInfo {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*AgentInfo, 0, len(f.Agents))
	for _, a := range f.Agents {
		copy := *a
		out = append(out, &copy)
	}
	return out
}

// ToMetadata converts the flock into its on-disk persistence form. Holds the
// read lock and defensively copies each AgentInfo so callers can mutate the
// returned metadata without affecting live state.
func (f *Flock) ToMetadata() FlockMetadata {
	f.mu.RLock()
	defer f.mu.RUnlock()
	agents := make(map[string]*AgentInfo, len(f.Agents))
	for k, v := range f.Agents {
		copy := *v
		agents[k] = &copy
	}
	return FlockMetadata{
		FlockID:       f.ID,
		Task:          f.Task,
		TenantID:      f.TenantID,
		EgressPolicy:  f.EgressPolicy,
		Agents:        agents,
		CreatedAt:     f.CreatedAt,
		SchemaVersion: currentSchemaVersion,
	}
}

// MarshalJSON serializes the flock under a read lock so concurrent AddAgent
// or UpdateAgentStatus calls cannot race the encoder.
func (f *Flock) MarshalJSON() ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	// Use an alias type to avoid recursing back into this MarshalJSON method.
	type flockJSON struct {
		ID           string                `json:"flock_id"`
		Task         string                `json:"task"`
		TenantID     string                `json:"tenant_id,omitempty"`
		EgressPolicy string                `json:"egress_policy,omitempty"`
		Agents       map[string]*AgentInfo `json:"agents"`
		CreatedAt    time.Time             `json:"created_at"`
	}
	return json.Marshal(flockJSON{
		ID:           f.ID,
		Task:         f.Task,
		TenantID:     f.TenantID,
		EgressPolicy: f.EgressPolicy,
		Agents:       f.Agents,
		CreatedAt:    f.CreatedAt,
	})
}

// FlockManager owns the in-memory registry of all live flocks on this host.
type FlockManager struct {
	mu      sync.RWMutex
	flocks  map[string]*Flock
	workDir string
}

// NewFlockManager returns an empty manager rooted at workDir.
// workDir is where per-flock subdirectories (TOWN_WALL.log, handoff/) live.
func NewFlockManager(workDir string) *FlockManager {
	return &FlockManager{
		flocks:  make(map[string]*Flock),
		workDir: workDir,
	}
}

// WorkDir returns the directory used to root flock-local files.
func (fm *FlockManager) WorkDir() string { return fm.workDir }

// Create allocates a flock, opens its Town Wall, and registers it.
// Supported call forms:
//   - Create(flockID, task, townWallPath)
//   - Create(flockID, task, tenantID, egressPolicy, townWallPath)
func (fm *FlockManager) Create(flockID, task string, args ...string) (*Flock, error) {
	var tenantID, egressPolicy, townWallPath string
	switch len(args) {
	case 1:
		townWallPath = args[0]
	case 3:
		tenantID, egressPolicy, townWallPath = args[0], args[1], args[2]
	default:
		return nil, fmt.Errorf("Create expects townWallPath or tenantID, egressPolicy, townWallPath")
	}
	tw, err := NewTownWall(flockID, townWallPath)
	if err != nil {
		return nil, err
	}
	f := &Flock{
		ID:           flockID,
		Task:         task,
		TenantID:     tenantID,
		EgressPolicy: egressPolicy,
		Agents:       make(map[string]*AgentInfo),
		TownWall:     tw,
		CreatedAt:    time.Now().UTC(),
	}
	fm.mu.Lock()
	fm.flocks[flockID] = f
	fm.mu.Unlock()
	return f, nil
}

// Get returns the flock with the given ID, if any.
func (fm *FlockManager) Get(flockID string) (*Flock, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	f, ok := fm.flocks[flockID]
	return f, ok
}

// List returns a snapshot of all registered flocks.
func (fm *FlockManager) List() []*Flock {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	out := make([]*Flock, 0, len(fm.flocks))
	for _, f := range fm.flocks {
		out = append(out, f)
	}
	return out
}

// Delete removes the flock from the registry and returns it for cleanup.
func (fm *FlockManager) Delete(flockID string) (*Flock, bool) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	f, ok := fm.flocks[flockID]
	if ok {
		delete(fm.flocks, flockID)
	}
	return f, ok
}

// LoadFromDisk scans workDir/flocks/ for metadata.json files and re-registers
// each recovered flock in memory. Each flock's TownWall is reopened in
// append mode against the existing log, preserving full history. Returns the
// number of recovered flocks and the IDs that had metadata but could not be
// fully restored (e.g. corrupt log file).
//
// Recovered flocks are read-mostly: their VMID references no longer correspond
// to live Firecracker processes (those died with the previous daemon), so
// DELETE on a recovered flock relies on destroyVM's missing-vm guard. Live VM
// re-registration is deferred to v0.3.2.
func (fm *FlockManager) LoadFromDisk() (recovered int, failed []string, err error) {
	metas, err := ListFlockMetadata(fm.workDir)
	if err != nil {
		return 0, nil, err
	}
	for _, meta := range metas {
		twPath := filepath.Join(fm.workDir, "flocks", meta.FlockID, "TOWN_WALL.log")
		tw, twErr := NewTownWall(meta.FlockID, twPath)
		if twErr != nil {
			failed = append(failed, meta.FlockID)
			continue
		}
		f := &Flock{
			ID:           meta.FlockID,
			Task:         meta.Task,
			TenantID:     meta.TenantID,
			EgressPolicy: meta.EgressPolicy,
			Agents:       meta.Agents,
			TownWall:     tw,
			CreatedAt:    meta.CreatedAt,
		}
		fm.mu.Lock()
		fm.flocks[meta.FlockID] = f
		fm.mu.Unlock()
		recovered++
	}
	return recovered, failed, nil
}
