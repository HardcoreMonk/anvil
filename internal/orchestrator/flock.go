package orchestrator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Agent lifecycle states recorded in AgentInfo.Status.
const (
	AgentStatusSpawning = "spawning"
	AgentStatusReady    = "ready"
	AgentStatusBusy     = "busy"
	AgentStatusDone     = "done"
	AgentStatusDead     = "dead"   // assigned by the health watchdog after consecutive probe failures
	AgentStatusPaused   = "paused" // runtime-only pause state; scrubbed from metadata persistence (v0.4.3)
)

// Kind distinguishes single-host flocks from cross-host routed-flock roles.
const (
	FlockKindLocal = ""
	FlockKindHub   = "hub"
	FlockKindRelay = "relay"
)

// RosterMember records a remote member of a cross-host (hub) flock.
type RosterMember struct {
	AgentID string `json:"agent_id"`
	Host    string `json:"host"`
	// VMID/Addr let the hub resolve a call target and reach its host daemon.
	// Both are filled by the post-spawn re-registration (the initial pre-spawn
	// registration has neither). Addr is daemon-internal: never log it and
	// never emit it on a serialized surface.
	VMID string `json:"vm_id,omitempty"`
	Addr string `json:"addr,omitempty"`
}

// AgentInfo is the per-agent record exposed via flock APIs.
type AgentInfo struct {
	AgentID string `json:"agent_id"` // e.g. "researcher-1"
	Role    string `json:"role"`     // logical role label, e.g. "researcher"
	// Profile is the config profile (sizing/model/system prompt) the VM was spawned
	// with; it may differ from Role. Empty (legacy records) → Role is the profile.
	Profile  string `json:"profile,omitempty"`
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
	Status   string `json:"status"`
}

// Flock is a named group of agents sharing one Town Wall.
type Flock struct {
	mu sync.RWMutex
	// opMu serializes long-running flock lifecycle mutations that must not
	// overlap: add-agent, role-change, pause/resume, and delete.
	opMu sync.Mutex
	// writeMu serializes metadata.json writes against any concurrent Persist
	// caller (createFlock, watchdog.onFailure, recovery.markFlockAgentDead,
	// per-agent restart). Held only for the duration of ToMetadata + tmp+rename
	// inside Persist; never overlaps with the per-agent f.mu critical sections.
	writeMu      sync.Mutex
	ID           string                `json:"flock_id"`
	Task         string                `json:"task"`
	TenantID     string                `json:"tenant_id,omitempty"`
	EgressPolicy string                `json:"egress_policy,omitempty"`
	MaxAgents    int                   `json:"max_agents"` // per-flock agent cap (v0.4.3); 0 means use the default
	Agents       map[string]*AgentInfo `json:"agents"`
	TownWall     *TownWall             `json:"-"`
	CreatedAt    time.Time             `json:"created_at"`
	// Kind distinguishes single-host flocks ("" / local) from cross-host
	// routed-flock roles: "hub" owns the canonical wall on the home host;
	// "relay" forwards posts to the home host. Local flocks are unchanged.
	Kind string `json:"-"`
	// HomeAddr and RelayToken are set only on relay flocks: the home daemon
	// base URL and the per-flock daemon-to-daemon relay token.
	HomeAddr   string `json:"-"`
	RelayToken string `json:"-"`
	// CallToken is the per-flock call-hop secret (parallel to RelayToken but
	// scoped ONLY to the call entry point, never Town Wall paths). Set on both
	// hub and relay flocks.
	CallToken string `json:"-"`
	// Roster lists remote members for a hub flock (informational; the hub owns
	// no local VMs for those agents).
	Roster []RosterMember `json:"-"`
	// Paused is a runtime-only flag set by flock pause/resume (v0.4.3). It is
	// deliberately NOT persisted (Firecracker pause is a runtime state); a daemon
	// restart brings members back running. Exposed via MarshalJSON, not ToMetadata.
	Paused bool `json:"-"`
	// pausedPrevStatus keeps the pre-pause status so pause rollback and metadata
	// persistence can preserve the non-runtime lifecycle state.
	pausedPrevStatus map[string]string
	deleted          bool
}

// BeginMutation serializes a long-running lifecycle operation and rejects new
// operations once the flock is being deleted. Callers must invoke the returned
// unlock function exactly once when ok is true.
func (f *Flock) BeginMutation() (unlock func(), ok bool) {
	f.opMu.Lock()
	f.mu.RLock()
	deleted := f.deleted
	f.mu.RUnlock()
	if deleted {
		f.opMu.Unlock()
		return nil, false
	}
	return f.opMu.Unlock, true
}

// BeginDelete marks the flock as deleted under the mutation lock. It blocks
// until any in-flight mutation finishes and prevents future mutations from
// starting. If another delete already won the race, ok is false.
func (f *Flock) BeginDelete() (unlock func(), ok bool) {
	f.opMu.Lock()
	f.mu.Lock()
	if f.deleted {
		f.mu.Unlock()
		f.opMu.Unlock()
		return nil, false
	}
	f.deleted = true
	f.mu.Unlock()
	return f.opMu.Unlock, true
}

// AddAgent inserts or replaces an agent record under lock.
func (f *Flock) AddAgent(a *AgentInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Agents[a.AgentID] = a
}

// ReserveAgent atomically allocates the next role-N id and inserts a spawning
// placeholder. It serializes max_agents enforcement and id assignment so
// concurrent add-agent requests cannot overrun the cap or collide on the same id.
func (f *Flock) ReserveAgent(role string, maxAgents int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if maxAgents > 0 && len(f.Agents)+1 > maxAgents {
		return "", fmt.Errorf("flock at max_agents (%d)", maxAgents)
	}
	max := 0
	prefix := role + "-"
	for _, a := range f.Agents {
		if strings.HasPrefix(a.AgentID, prefix) {
			if n, err := strconv.Atoi(strings.TrimPrefix(a.AgentID, prefix)); err == nil && n > max {
				max = n
			}
		}
	}
	agentID := fmt.Sprintf("%s-%d", role, max+1)
	f.Agents[agentID] = &AgentInfo{
		AgentID: agentID,
		Role:    role,
		Status:  AgentStatusSpawning,
	}
	return agentID, nil
}

// UpdateAgentStatus updates the status of an existing agent.
// No-op when the agent ID is unknown.
func (f *Flock) UpdateAgentStatus(agentID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.Agents[agentID]; ok {
		a.Status = status
		if status != AgentStatusPaused && f.pausedPrevStatus != nil {
			delete(f.pausedPrevStatus, agentID)
		}
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

// RemoveAgent deletes an agent record from the flock under lock. No-op when the
// agent ID is unknown. The agent's Town Wall messages are intentionally left in
// place — a membership change is not an audit erasure.
func (f *Flock) RemoveAgent(agentID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Agents, agentID)
	if f.pausedPrevStatus != nil {
		delete(f.pausedPrevStatus, agentID)
	}
}

// ChangeAgentRole updates the role label and profile of an existing agent under
// lock. No-op when the agent ID is unknown. Because both are bound at spawn time
// (VM sizing + system prompt), callers that need them to take effect must also
// recreate the agent's VM.
func (f *Flock) ChangeAgentRole(agentID, newRole, newProfile string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.Agents[agentID]; ok {
		a.Role = newRole
		a.Profile = newProfile
	}
}

// SetPaused sets the runtime-only paused flag under lock (v0.4.3).
func (f *Flock) SetPaused(paused bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Paused = paused
}

// SetRoster replaces a hub flock's remote-membership roster under the per-flock
// lock, keeping the write consistent with ToMetadata's f.mu-guarded read so a
// concurrent Persist can never race the roster field.
func (f *Flock) SetRoster(roster []RosterMember) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Roster = roster
}

// SetDistributedTokens re-seats the relay/call admission secrets under the
// per-flock lock (matching SetRoster's discipline). An empty argument leaves the
// corresponding field unchanged so a token-less (backward-compat) re-registration
// cannot blank a live secret. These fields are runtime-only — ToMetadata never
// serializes them and no Persist follows: admission secrets stay off disk.
func (f *Flock) SetDistributedTokens(relayToken, callToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if relayToken != "" {
		f.RelayToken = relayToken
	}
	if callToken != "" {
		f.CallToken = callToken
	}
}

// DistributedTokens returns the relay/call secret pair under the per-flock
// lock. Hub tokens are mutable after publish (UpdateHubTokens re-seats them on
// every reconcile re-POST), so outbound hop builders must read through this
// getter rather than the raw fields — a raw read races SetDistributedTokens.
// Relay flocks are unaffected: re-registration swaps the whole struct, so their
// fields stay immutable after publish and existing raw reads remain safe.
func (f *Flock) DistributedTokens() (relayToken, callToken string) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.RelayToken, f.CallToken
}

// RosterSnapshot returns a defensive copy of the hub roster under the per-flock
// lock. The roster is replaced by every reconcile re-POST (SetRoster), so
// lookups (rosterMember, local-VM resolution) must iterate a snapshot — ranging
// the live slice races the replacement. Mirrors Snapshot()'s discipline for
// agents.
func (f *Flock) RosterSnapshot() []RosterMember {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if len(f.Roster) == 0 {
		return nil
	}
	out := make([]RosterMember, len(f.Roster))
	copy(out, f.Roster)
	return out
}

// MarkAgentPaused changes an agent to the runtime-only paused state while
// remembering the pre-pause status for rollback and metadata scrubbing.
func (f *Flock) MarkAgentPaused(agentID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.Agents[agentID]
	if !ok {
		return
	}
	if a.Status != AgentStatusPaused {
		if f.pausedPrevStatus == nil {
			f.pausedPrevStatus = make(map[string]string)
		}
		f.pausedPrevStatus[agentID] = a.Status
	}
	a.Status = AgentStatusPaused
}

// RestorePausedAgentStatus restores an agent to its pre-pause status. fallback is
// used for old in-memory states that do not have a recorded previous status.
func (f *Flock) RestorePausedAgentStatus(agentID, fallback string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.Agents[agentID]
	if !ok {
		return
	}
	if f.pausedPrevStatus != nil {
		if prev, ok := f.pausedPrevStatus[agentID]; ok {
			a.Status = prev
			delete(f.pausedPrevStatus, agentID)
			return
		}
	}
	if a.Status == AgentStatusPaused {
		a.Status = fallback
	}
}

// MarkAgentDeadIfNotPaused sets an agent dead only if it is not currently in the
// runtime-only paused state. This closes the race where the watchdog crossed its
// threshold just as flock pause was marking the member paused.
func (f *Flock) MarkAgentDeadIfNotPaused(agentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.Agents[agentID]
	if !ok || a.Status == AgentStatusPaused {
		return false
	}
	a.Status = AgentStatusDead
	if f.pausedPrevStatus != nil {
		delete(f.pausedPrevStatus, agentID)
	}
	return true
}

// AgentStatus returns an agent's current status under a read lock, or "" when
// the agent is unknown. Used by the watchdog to skip dead-marking paused agents.
func (f *Flock) AgentStatus(agentID string) string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if a, ok := f.Agents[agentID]; ok {
		return a.Status
	}
	return ""
}

// Persist atomically writes the flock's current metadata to disk. Holds
// writeMu so concurrent callers (createFlock, watchdog, recovery) cannot
// race the tmp+rename inside SaveFlockMetadata. All flock metadata writers
// MUST go through this helper rather than calling SaveFlockMetadata
// directly, otherwise the serialization invariant is lost.
func (f *Flock) Persist(workDir string) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	// Skip the write once the flock is being deleted: deleteFlock removes
	// metadata.json via DeleteMetadata (also under writeMu), and a Persist that
	// races in after BeginDelete must not resurrect it — a ghost file would make
	// the next LoadFromDisk recover a deleted flock. Both orderings converge to
	// "no file": if this Persist wins writeMu it writes and the following
	// DeleteMetadata removes it; if it loses, it reads deleted=true here and
	// skips. deleted is read under f.mu.RLock (same lock ToMetadata takes), so
	// the writeMu->f.mu order is unchanged.
	f.mu.RLock()
	deleted := f.deleted
	f.mu.RUnlock()
	if deleted {
		return nil
	}
	return SaveFlockMetadata(workDir, f.ToMetadata())
}

// DeleteMetadata removes the flock's metadata.json under writeMu so it
// serializes against any concurrent Persist (a metadata writer must never race
// the tmp+rename). Callers must have marked the flock deleted via BeginDelete
// first, so a Persist that loses the writeMu race observes deleted and skips its
// write rather than resurrecting the file.
func (f *Flock) DeleteMetadata(workDir string) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return DeleteFlockMetadata(workDir, f.ID)
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
		if copy.Status == AgentStatusSpawning && copy.VMID == "" {
			continue
		}
		if copy.Status == AgentStatusPaused {
			if prev, ok := f.pausedPrevStatus[k]; ok {
				copy.Status = prev
			} else {
				copy.Status = AgentStatusReady
			}
		}
		agents[k] = &copy
	}
	// Defensively copy the roster (as ToMetadata does for agents) so the caller
	// can mutate the returned metadata without touching live flock state, and
	// scrub each member's Addr: it is daemon-internal (see RosterMember) and must
	// never reach a serialized surface such as the 0644 metadata.json (R2). A
	// recovered hub reloads with Addr="" and cannot resolve a 2nd-hop /call until
	// the reconcile re-POST (UpdateHubRoster) refills Addr in memory — but that
	// hop is already gated by CallToken, which is likewise absent until the same
	// re-POST, so scrubbing costs no additional capability window.
	var roster []RosterMember
	if len(f.Roster) > 0 {
		roster = make([]RosterMember, len(f.Roster))
		copy(roster, f.Roster)
		for i := range roster {
			roster[i].Addr = ""
		}
	}
	return FlockMetadata{
		FlockID:       f.ID,
		Task:          f.Task,
		TenantID:      f.TenantID,
		EgressPolicy:  f.EgressPolicy,
		MaxAgents:     f.MaxAgents,
		Agents:        agents,
		Kind:          f.Kind,
		HomeAddr:      f.HomeAddr,
		Roster:        roster,
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
		MaxAgents    int                   `json:"max_agents"`
		Paused       bool                  `json:"paused"`
		Agents       map[string]*AgentInfo `json:"agents"`
		CreatedAt    time.Time             `json:"created_at"`
	}
	return json.Marshal(flockJSON{
		ID:           f.ID,
		Task:         f.Task,
		TenantID:     f.TenantID,
		EgressPolicy: f.EgressPolicy,
		MaxAgents:    f.MaxAgents,
		Paused:       f.Paused,
		Agents:       f.Agents,
		CreatedAt:    f.CreatedAt,
	})
}

// FlockManager owns the in-memory registry of all live flocks on this host.
type FlockManager struct {
	mu      sync.RWMutex
	flocks  map[string]*Flock
	workDir string

	townWallMaxBytes int64 // Town Wall rotation threshold (v0.4.3); 0 = off
	townWallKeep     int   // rotated Town Wall backups to retain
}

// NewFlockManager returns an empty manager rooted at workDir.
// workDir is where per-flock subdirectories (TOWN_WALL.log, handoff/) live.
func NewFlockManager(workDir string) *FlockManager {
	return &FlockManager{
		flocks:           make(map[string]*Flock),
		workDir:          workDir,
		townWallMaxBytes: int64(envIntDefault("EPHEMERA_TOWNWALL_MAX_MIB", 10)) << 20,
		townWallKeep:     envIntDefault("EPHEMERA_TOWNWALL_KEEP", 3),
	}
}

// envIntDefault parses a positive int env var, falling back to def. The daemon's
// envInt lives in package main; this mirrors it for Town Wall rotation config
// (v0.4.3) since the orchestrator package can't import it.
func envIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// WorkDir returns the directory used to root flock-local files.
func (fm *FlockManager) WorkDir() string { return fm.workDir }

// NewUnregistered allocates a flock and opens its Town Wall without adding it to
// the manager registry. Use when a caller must finish a multi-step spawn before
// list/get/delete can see the flock.
func (fm *FlockManager) NewUnregistered(flockID, task string, args ...string) (*Flock, error) {
	var tenantID, egressPolicy, townWallPath string
	switch len(args) {
	case 1:
		townWallPath = args[0]
	case 3:
		tenantID, egressPolicy, townWallPath = args[0], args[1], args[2]
	default:
		return nil, fmt.Errorf("NewUnregistered expects townWallPath or tenantID, egressPolicy, townWallPath")
	}
	tw, err := NewTownWall(flockID, townWallPath)
	if err != nil {
		return nil, err
	}
	tw.SetRotation(fm.townWallMaxBytes, fm.townWallKeep)
	return &Flock{
		ID:           flockID,
		Task:         task,
		TenantID:     tenantID,
		EgressPolicy: egressPolicy,
		Agents:       make(map[string]*AgentInfo),
		TownWall:     tw,
		CreatedAt:    time.Now().UTC(),
	}, nil
}

// Register adds a fully initialized flock to the manager registry.
func (fm *FlockManager) Register(f *Flock) {
	fm.mu.Lock()
	fm.flocks[f.ID] = f
	fm.mu.Unlock()
}

// RegisterHub registers a hub flock that owns the canonical Town Wall on the
// home host. It has no local member VMs; roster is the remote membership.
func (fm *FlockManager) RegisterHub(flockID string, wall *TownWall, roster []RosterMember, relayToken, callToken string) *Flock {
	f := &Flock{
		ID:         flockID,
		Kind:       FlockKindHub,
		TownWall:   wall,
		Roster:     roster,
		RelayToken: relayToken,
		CallToken:  callToken,
		Agents:     map[string]*AgentInfo{},
		CreatedAt:  time.Now().UTC(),
	}
	fm.mu.Lock()
	fm.flocks[flockID] = f
	fm.mu.Unlock()
	// Persist the hub role so a daemon restart recovers it as a hub (D1) rather
	// than downgrading to a local flock. Persistence failure is logged and the
	// registration proceeds — the in-memory hub works for this daemon process —
	// matching createFlock's persist-failure handling.
	if err := f.Persist(fm.workDir); err != nil {
		slog.Warn("flock: persist hub metadata failed (still in memory)", "flock_id", flockID, "err", err)
	}
	return f
}

// UpdateHubRoster replaces an existing hub flock's Roster under fm.mu. Used
// by the idempotent /distributed re-registration path so a post-spawn re-POST
// carrying a VMID/Addr-enriched roster can update the hub in place without a
// raw field assignment racing concurrent readers of the pointer returned by
// Get. Reports false when flockID is not a registered hub flock.
func (fm *FlockManager) UpdateHubRoster(flockID string, roster []RosterMember) bool {
	fm.mu.Lock()
	f, ok := fm.flocks[flockID]
	if !ok || f.Kind != FlockKindHub {
		fm.mu.Unlock()
		return false
	}
	fm.mu.Unlock()
	// Write the roster under the per-flock lock (SetRoster), not fm.mu, so it is
	// consistent with ToMetadata's f.mu-guarded read of Roster — otherwise a
	// concurrent Persist could race the roster field. Persist outside fm.mu so
	// the small metadata write never blocks the registry.
	f.SetRoster(roster)
	if err := f.Persist(fm.workDir); err != nil {
		slog.Warn("flock: persist hub roster update failed", "flock_id", flockID, "err", err)
	}
	return true
}

// UpdateHubTokens re-seats an existing hub flock's in-memory relay/call
// admission secrets. A restarted home daemon recovers the hub from disk WITHOUT
// its tokens (secrets are never persisted — see FlockMetadata), so f.RelayToken/
// f.CallToken come back empty; the idempotent reconcile re-POST must refill them
// or the hub's outbound 2nd-hop /call (forwardFlockCall(member.Addr, f.CallToken,
// ...)) sends an empty bearer and the member daemon 401s (D1b). Mirrors
// UpdateHubRoster (fm.mu confirms the hub, then the per-flock lock writes) but
// deliberately does NOT Persist — the whole point is that these secrets stay off
// disk. Empty incoming tokens are ignored (see SetDistributedTokens). Reports
// false when flockID is not a registered hub flock.
func (fm *FlockManager) UpdateHubTokens(flockID, relayToken, callToken string) bool {
	fm.mu.Lock()
	f, ok := fm.flocks[flockID]
	if !ok || f.Kind != FlockKindHub {
		fm.mu.Unlock()
		return false
	}
	fm.mu.Unlock()
	f.SetDistributedTokens(relayToken, callToken)
	return true
}

// RegisterRelay registers a relay flock on a member host. It owns no wall;
// posts and reads are forwarded to homeAddr with relayToken. agents is the
// host-local member list (2026-07-08 design correction): a hopped call
// landing on this relay flock (the real topology for a member daemon
// receiving home's 2nd hop) must resolve against THIS host's agents rather
// than unconditionally 404ing. Only AgentID+VMID are used — Host/Addr (if
// present) are remote-host fields that do not apply to a relay's own local
// roster and are ignored. An empty/nil agents leaves Agents an empty map
// (matches the pre-existing behavior).
func (fm *FlockManager) RegisterRelay(flockID, homeAddr, relayToken, callToken string, agents []RosterMember) *Flock {
	agentMap := make(map[string]*AgentInfo, len(agents))
	for _, a := range agents {
		agentMap[a.AgentID] = &AgentInfo{AgentID: a.AgentID, VMID: a.VMID}
	}
	f := &Flock{
		ID:         flockID,
		Kind:       FlockKindRelay,
		HomeAddr:   homeAddr,
		RelayToken: relayToken,
		CallToken:  callToken,
		Agents:     agentMap,
		CreatedAt:  time.Now().UTC(),
	}
	fm.mu.Lock()
	fm.flocks[flockID] = f
	fm.mu.Unlock()
	// Persist the relay role (kind + home_addr + local agents) so a daemon
	// restart recovers it as a relay (D1) instead of downgrading to a local
	// flock. Persistence failure is logged and registration proceeds, matching
	// RegisterHub / createFlock. Admission secrets are never written to disk.
	if err := f.Persist(fm.workDir); err != nil {
		slog.Warn("flock: persist relay metadata failed (still in memory)", "flock_id", flockID, "err", err)
	}
	return f
}

// Create allocates a flock, opens its Town Wall, and registers it.
// Supported call forms:
//   - Create(flockID, task, townWallPath)
//   - Create(flockID, task, tenantID, egressPolicy, townWallPath)
func (fm *FlockManager) Create(flockID, task string, args ...string) (*Flock, error) {
	f, err := fm.NewUnregistered(flockID, task, args...)
	if err != nil {
		return nil, err
	}
	fm.Register(f)
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
		tw.SetRotation(fm.townWallMaxBytes, fm.townWallKeep)
		f := &Flock{
			ID:           meta.FlockID,
			Task:         meta.Task,
			TenantID:     meta.TenantID,
			EgressPolicy: meta.EgressPolicy,
			MaxAgents:    meta.MaxAgents,
			Agents:       meta.Agents,
			TownWall:     tw,
			CreatedAt:    meta.CreatedAt,
			// Restore the cross-host role so a recovered hub/relay is not
			// downgraded to a local flock (D1). Admission secrets stay in memory
			// and are re-injected by the reconcile re-POST. A relay reopens a
			// local wall it never serves from (the Kind==relay handlers forward
			// to home before touching f.TownWall); keeping it non-nil preserves
			// the pre-existing LoadFromDisk invariant that every recovered flock
			// has a wall (the watchdog posts member status to it without a nil
			// guard).
			Kind:     meta.Kind,
			HomeAddr: meta.HomeAddr,
			Roster:   meta.Roster,
		}
		fm.mu.Lock()
		fm.flocks[meta.FlockID] = f
		fm.mu.Unlock()
		recovered++
	}
	return recovered, failed, nil
}
