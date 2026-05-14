package orchestrator

import (
	"encoding/json"
	"sync"
	"time"
)

// Agent lifecycle states recorded in AgentInfo.Status.
const (
	AgentStatusSpawning = "spawning"
	AgentStatusReady    = "ready"
	AgentStatusBusy     = "busy"
	AgentStatusDone     = "done"
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
	mu           sync.RWMutex
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

// Create allocates a flock, opens its Town Wall at townWallPath, and registers it.
func (fm *FlockManager) Create(flockID, task, tenantID, egressPolicy, townWallPath string) (*Flock, error) {
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
