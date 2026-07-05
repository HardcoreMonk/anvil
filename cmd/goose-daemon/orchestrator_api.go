package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ephemera/internal/orchestrator"
)

// defaultMaxAgentsPerFlock is the per-flock agent cap when a flock does not set
// its own max_agents (v0.4.3). Limits IP pool / TAP exhaustion from a runaway caller.
const defaultMaxAgentsPerFlock = 20

// flockMax returns a flock's effective agent cap: its own MaxAgents, or the
// default when unset (0). Recovered flocks with no stored cap also fall back here.
// agentCount renders an agent count with the grammatically correct noun for Town
// Wall system messages: "1 agent" (singular) vs "N agents" (plural).
func agentCount(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return fmt.Sprintf("%d agents", n)
}

func flockMax(f *orchestrator.Flock) int {
	if f.MaxAgents > 0 {
		return f.MaxAgents
	}
	return defaultMaxAgentsPerFlock
}

// FlockCreateRequest is the POST /flocks body. roles[i] becomes one VM.
type FlockCreateRequest struct {
	Task         string   `json:"task"`
	Roles        []string `json:"roles"`
	TenantID     string   `json:"tenant_id,omitempty"`
	EgressPolicy string   `json:"egress_policy,omitempty"`
	// Profiles[i] is the profile Roles[i] uses for sizing/model/system prompt.
	// Empty or absent → the role name is used as the profile (back-compat).
	Profiles  []string `json:"profiles,omitempty"`
	MaxAgents *int     `json:"max_agents,omitempty"` // per-flock cap (v0.4.3); nil → default
}

// FlockCreateResponse is returned by POST /flocks.
type FlockCreateResponse struct {
	FlockID      string                    `json:"flock_id"`
	Task         string                    `json:"task"`
	TenantID     string                    `json:"tenant_id,omitempty"`
	EgressPolicy string                    `json:"egress_policy,omitempty"`
	Agents       []*orchestrator.AgentInfo `json:"agents"`
	TownWallURL  string                    `json:"townwall_url"`
	PostURL      string                    `json:"post_url"`
}

// TownWallPostRequest is the POST /flocks/{id}/post body.
type TownWallPostRequest struct {
	AgentID string `json:"agent_id"`
	Body    string `json:"body"`
}

// registerOrchestratorRoutes wires flock endpoints onto the control plane mux.
func (cp *ControlPlane) registerOrchestratorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/flocks", cp.handleFlocks)
	mux.HandleFunc("/flocks/", cp.handleFlockItem)
}

// /flocks — POST creates, GET lists.
func (cp *ControlPlane) handleFlocks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		cp.createFlock(w, r)
	case http.MethodGet:
		cp.listFlocks(w)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// /flocks/{id}, /flocks/{id}/wall, /flocks/{id}/wall/history, /flocks/{id}/post
func (cp *ControlPlane) handleFlockItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/flocks/")
	if path == "" {
		http.Error(w, `{"error":"flock_id required"}`, http.StatusBadRequest)
		return
	}
	flockID, sub, _ := strings.Cut(path, "/")
	if flockID == "" {
		http.Error(w, `{"error":"flock_id required"}`, http.StatusBadRequest)
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		cp.getFlock(w, flockID)
	case sub == "" && r.Method == http.MethodDelete:
		cp.deleteFlock(w, flockID)
	case sub == "wall" && r.Method == http.MethodGet:
		cp.streamTownWall(w, r, flockID)
	case sub == "wall/history" && r.Method == http.MethodGet:
		cp.townWallHistory(w, r, flockID)
	case sub == "post" && r.Method == http.MethodPost:
		cp.postToTownWall(w, r, flockID)
	case sub == "pause" && r.Method == http.MethodPost:
		cp.pauseFlock(w, flockID)
	case sub == "resume" && r.Method == http.MethodPost:
		cp.resumeFlock(w, flockID)
	case sub == "broadcast" && r.Method == http.MethodPost:
		cp.broadcastFlock(w, r, flockID)
	case sub == "agents" || strings.HasPrefix(sub, "agents/"):
		rest := strings.TrimPrefix(strings.TrimPrefix(sub, "agents"), "/")
		agentID, action, _ := strings.Cut(rest, "/")
		switch {
		case agentID == "" && r.Method == http.MethodPost:
			cp.addFlockAgent(w, r, flockID)
		case agentID != "" && action == "" && r.Method == http.MethodDelete:
			cp.removeFlockAgent(w, flockID, agentID)
		case agentID != "" && action == "" && r.Method == http.MethodPatch:
			cp.changeFlockAgentRole(w, r, flockID, agentID)
		case agentID != "" && action == "restart" && r.Method == http.MethodPost:
			cp.restartAgent(w, flockID, agentID)
		default:
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("unsupported agent action"))
		}
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// createFlock spawns one VM per requested role and registers them under a new flock ID.
// On any spawn failure, all VMs spawned so far are torn down and the flock is removed.
func (cp *ControlPlane) createFlock(w http.ResponseWriter, r *http.Request) {
	var req FlockCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	var err error
	req.Task, err = normalizeDaemonFlockTask(req.Task)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.Roles, err = normalizeDaemonFlockRoles(req.Roles)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.TenantID, err = normalizeDaemonTenantID(req.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.EgressPolicy, err = normalizeDaemonEgressPolicy(req.EgressPolicy)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	maxAgents := defaultMaxAgentsPerFlock
	if req.MaxAgents != nil {
		maxAgents = *req.MaxAgents
	}
	if maxAgents < 1 {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("max_agents must be >= 1"))
		return
	}
	if len(req.Roles) > maxAgents {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("roles exceed max_agents (%d > %d)", len(req.Roles), maxAgents))
		return
	}

	flockID := fmt.Sprintf("flock-%d", time.Now().UnixNano())
	townWallPath := filepath.Join(cp.workDir, "flocks", flockID, "TOWN_WALL.log")
	if err := os.MkdirAll(filepath.Dir(townWallPath), 0755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	flock, err := cp.flockMgr.NewUnregistered(flockID, req.Task, req.TenantID, req.EgressPolicy, townWallPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	unlock, ok := flock.BeginMutation()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	defer unlock()
	flock.MaxAgents = maxAgents

	// Spawn each VM sequentially. On failure, tear everything down so we don't
	// leak resources or leave an unusable flock registered.
	//
	// Agent IDs use per-role indexing (researcher-1, researcher-2, worker-1, ...)
	// so callers can address agents by role-position rather than the global
	// position in req.Roles. This matches the README documentation.
	spawned := make([]string, 0, len(req.Roles))
	roleSeq := make(map[string]int, len(req.Roles))
	cleanup := func() {
		for _, vmID := range spawned {
			cp.destroyVM(vmID)
		}
		cp.flockMgr.Delete(flockID)
	}
	for i, role := range req.Roles {
		roleSeq[role]++
		agentID := fmt.Sprintf("%s-%d", role, roleSeq[role])
		// The profile (sizing/model/system prompt) defaults to the role name but can
		// be chosen independently per role via req.Profiles (parallel to req.Roles).
		profile := role
		if i < len(req.Profiles) && strings.TrimSpace(req.Profiles[i]) != "" {
			profile = strings.TrimSpace(req.Profiles[i])
		}
		vmInfo, _, err := cp.spawnVMForFlock(flockID, agentID, profile, req.TenantID, req.EgressPolicy)
		if err != nil {
			cleanup()
			writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("spawn %s: %w", agentID, err))
			return
		}
		flock.AddAgent(&orchestrator.AgentInfo{
			AgentID:  agentID,
			Role:     role,
			Profile:  profile,
			VMID:     vmInfo.VMID,
			AgentURL: vmInfo.AgentURL,
			Status:   orchestrator.AgentStatusReady,
		})
		spawned = append(spawned, vmInfo.VMID)
	}

	if _, err := flock.TownWall.Post(orchestrator.SystemAuthor,
		fmt.Sprintf("Flock spawned with %s: %v", agentCount(len(req.Roles)), req.Roles)); err != nil {
		slog.Warn("flock: initial town wall post failed", "flock_id", flockID, "err", err)
	}

	// Persist before responding so a daemon crash between here and the next
	// request still leaves a recoverable record. Persistence failure is
	// logged but does not invalidate the spawn — the in-memory flock works
	// for the duration of this daemon process. Uses Flock.Persist so the
	// writeMu serializes against any concurrent watchdog/recovery write.
	if err := flock.Persist(cp.workDir); err != nil {
		slog.Warn("flock: persist metadata failed (still in memory)", "flock_id", flockID, "err", err)
	}
	cp.flockMgr.Register(flock)

	resp := FlockCreateResponse{
		FlockID:      flockID,
		Task:         req.Task,
		TenantID:     req.TenantID,
		EgressPolicy: req.EgressPolicy,
		Agents:       flock.Snapshot(),
		TownWallURL:  buildPublicURLPath("/flocks/" + flockID + "/wall"),
		PostURL:      buildPublicURLPath("/flocks/" + flockID + "/post"),
	}
	cp.metrics.flockSpawn.Inc()
	writeJSON(w, http.StatusCreated, resp)
}

func (cp *ControlPlane) listFlocks(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, cp.flockMgr.List())
}

func (cp *ControlPlane) getFlock(w http.ResponseWriter, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// deleteFlock removes a flock from the registry and tears down all its VMs.
// VM teardowns happen in parallel so a 5-agent flock destroys in ~1s, not 5s.
func (cp *ControlPlane) deleteFlock(w http.ResponseWriter, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	unlockDelete, ok := f.BeginDelete()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	defer unlockDelete()
	cp.flockMgr.Delete(flockID)
	agents := f.Snapshot()
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(vmID string) {
			defer wg.Done()
			// Recovered flocks reference VMIDs from a previous daemon process
			// that no longer exist in cp.vms; destroyVM's missing-vm guard
			// makes this a no-op, but a one-line trace helps when reading
			// the daemon log to confirm cleanup happened.
			cp.mu.RLock()
			_, alive := cp.vms[vmID]
			cp.mu.RUnlock()
			if !alive {
				slog.Warn("flock: vm already absent", "flock_id", flockID, "vm_id", vmID)
				return
			}
			cp.destroyVM(vmID)
		}(a.VMID)
	}
	wg.Wait()
	if err := orchestrator.DeleteFlockMetadata(cp.workDir, flockID); err != nil {
		slog.Warn("flock: remove persisted metadata failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock destroyed", "flock_id", flockID, "agents", len(agents))
	cp.metrics.flockDestroy.Inc()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "flock_id": flockID})
}

func (cp *ControlPlane) postToTownWall(w http.ResponseWriter, r *http.Request, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	var req TownWallPostRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if req.AgentID == "" || req.Body == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("agent_id and body required"))
		return
	}
	msg, err := f.TownWall.Post(req.AgentID, req.Body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (cp *ControlPlane) townWallHistory(w http.ResponseWriter, r *http.Request, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	history, err := f.TownWall.History()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	q := r.URL.Query()
	history, err = filterTownWall(history, q.Get("agent_id"), q.Get("since"), q.Get("until"), q.Get("contains"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if history == nil {
		history = []orchestrator.Message{}
	}
	writeJSON(w, http.StatusOK, history)
}

// filterTownWall applies optional Town Wall history query filters (v0.4.3):
// agent_id (exact match), since/until (RFC3339, inclusive), and contains
// (substring of body).
// An all-empty filter set returns msgs unchanged.
func filterTownWall(msgs []orchestrator.Message, agentID, since, until, contains string) ([]orchestrator.Message, error) {
	if agentID == "" && since == "" && until == "" && contains == "" {
		return msgs, nil
	}
	var sinceTime, untilTime time.Time
	var sinceSet, untilSet bool
	if since != "" {
		var err error
		sinceTime, err = time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return nil, fmt.Errorf("invalid since: must be RFC3339")
		}
		sinceSet = true
	}
	if until != "" {
		var err error
		untilTime, err = time.Parse(time.RFC3339Nano, until)
		if err != nil {
			return nil, fmt.Errorf("invalid until: must be RFC3339")
		}
		untilSet = true
	}
	out := make([]orchestrator.Message, 0, len(msgs))
	for _, m := range msgs {
		if agentID != "" && m.AgentID != agentID {
			continue
		}
		if sinceSet || untilSet {
			msgTime, err := time.Parse(time.RFC3339Nano, m.Timestamp)
			if err != nil {
				continue
			}
			if sinceSet && msgTime.Before(sinceTime) {
				continue
			}
			if untilSet && msgTime.After(untilTime) {
				continue
			}
		}
		if contains != "" && !strings.Contains(m.Body, contains) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// streamTownWall streams new Town Wall messages as Server-Sent Events.
// Sends the existing history once, then forwards each new Post until the client
// disconnects or the server shuts down.
func (cp *ControlPlane) streamTownWall(w http.ResponseWriter, r *http.Request, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe BEFORE flushing history so a message posted between the History
	// read and Subscribe registration is not lost (the subscriber buffer catches it).
	sub := f.TownWall.Subscribe()
	defer f.TownWall.Unsubscribe(sub)

	if hist, err := f.TownWall.History(); err == nil {
		for _, m := range hist {
			sseEmit(w, m)
		}
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-sub:
			if !ok {
				return
			}
			sseEmit(w, m)
			flusher.Flush()
		}
	}
}

func normalizeDaemonFlockTask(task string) (string, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return "", fmt.Errorf("task must be non-empty")
	}
	return task, nil
}

func normalizeDaemonFlockRoles(roles []string) ([]string, error) {
	if len(roles) == 0 {
		return nil, fmt.Errorf("roles required")
	}
	normalized := make([]string, 0, len(roles))
	for idx, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" {
			return nil, fmt.Errorf("roles[%d] must be non-empty", idx)
		}
		if strings.ContainsAny(role, `/\`) {
			return nil, fmt.Errorf("roles[%d] must not contain path separators", idx)
		}
		if role == "." || role == ".." {
			return nil, fmt.Errorf("roles[%d] must not be %q", idx, role)
		}
		normalized = append(normalized, role)
	}
	return normalized, nil
}

// restartAgent tears down a single flock member's VM and respawns it with
// the same agent_id, role, and agent_token so callers that cached the token
// keep working across the restart. The new VM gets a fresh vm_id / guest_ip
// / agent_url. Status flips to ready on success; on spawn failure the agent
// is marked dead so external callers see the truth.
func (cp *ControlPlane) restartAgent(w http.ResponseWriter, flockID, agentID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	unlock, ok := f.BeginMutation()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	defer unlock()

	var oldVMID, role, profileName string
	for _, a := range f.Snapshot() {
		if a.AgentID == agentID {
			oldVMID = a.VMID
			role = a.Role
			profileName = a.Profile
			break
		}
	}
	if oldVMID == "" {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent not found"))
		return
	}
	// Respawn with the SAME profile the agent was created with. Legacy records have
	// no profile → fall back to the role name (the pre-split behavior).
	if profileName == "" {
		profileName = role
	}

	// Token must be read before destroyVM removes the runningVM entry.
	cp.mu.RLock()
	var oldToken string
	if v, ok := cp.vms[oldVMID]; ok {
		oldToken = v.agentToken
	}
	cp.mu.RUnlock()

	cp.destroyVM(oldVMID)
	if cp.watchdog != nil {
		cp.watchdog.ForgetVM(oldVMID)
	}

	configPath, secretsPath, err := cp.profileConfigPaths(profileName)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	agentProfile := LookupProfile(profileName)
	info, _, err := cp.spawnVMInternal(spawnVMOptions{
		Profile:           profileName,
		ConfigPath:        configPath,
		SecretsPath:       secretsPath,
		TenantID:          f.TenantID,
		EgressPolicy:      f.EgressPolicy,
		SystemPrompt:      cp.loadProfileSystemPrompt(agentProfile.ProfileDir),
		FlockID:           flockID,
		AgentID:           agentID,
		AgentToken:        oldToken,
		ControlPlaneToken: cp.controlPlaneTokenForVM(),
		VcpuCount:         agentProfile.VcpuCount,
		MemSizeMib:        agentProfile.MemSizeMib,
	})
	if err != nil {
		// Agent slot no longer has a backing VM — mark dead so callers see it
		// and let them decide whether to retry or DELETE the flock entirely.
		f.UpdateAgentStatus(agentID, orchestrator.AgentStatusDead)
		if perr := f.Persist(cp.workDir); perr != nil {
			slog.Warn("restart agent: persist dead status failed", "agent_id", agentID, "err", perr)
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}

	f.UpdateAgentVM(agentID, info.VMID, info.AgentURL)
	if err := f.Persist(cp.workDir); err != nil {
		slog.Warn("restart agent: persist failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock: agent restarted", "flock_id", flockID, "agent_id", agentID, "old_vm_id", oldVMID, "new_vm_id", info.VMID)
	writeJSON(w, http.StatusOK, info)
}

// AgentRoleRequest is the body for POST /flocks/{id}/agents (add) and
// PATCH /flocks/{id}/agents/{agent_id} (role change): the target role.
type AgentRoleRequest struct {
	Role string `json:"role"`
	// Profile is the config profile to spawn the (re)created VM with. Empty → the
	// role name is used as the profile (back-compat).
	Profile string `json:"profile,omitempty"`
}

// FlockAddAgentResponse is returned by POST /flocks/{id}/agents. The daemon
// keeps the guest token internally; public add-agent responses must not expose
// agent_token or agent_tokens.
type FlockAddAgentResponse struct {
	AgentID  string `json:"agent_id"`
	Role     string `json:"role"`
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
}

// addFlockAgent spawns one new VM and registers it as a member of an existing
// flock. POST /flocks/{id}/agents  {"role":"worker"}. The per-VM agent_token is
// kept internally for proxy injection and restart identity, but is not returned.
func (cp *ControlPlane) addFlockAgent(w http.ResponseWriter, r *http.Request, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	unlock, ok := f.BeginMutation()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	defer unlock()
	var req AgentRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	roles, err := normalizeDaemonFlockRoles([]string{req.Role})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.Role = roles[0]
	agentID, err := f.ReserveAgent(req.Role, flockMax(f))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	profile := req.Role
	if strings.TrimSpace(req.Profile) != "" {
		profile = strings.TrimSpace(req.Profile)
	}
	vmInfo, _, err := cp.spawnVMForFlock(flockID, agentID, profile, f.TenantID, f.EgressPolicy)
	if err != nil {
		f.RemoveAgent(agentID)
		if perr := f.Persist(cp.workDir); perr != nil {
			slog.Warn("add agent: persist reservation rollback failed", "flock_id", flockID, "agent_id", agentID, "err", perr)
		}
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("spawn %s: %w", agentID, err))
		return
	}
	// Replace the ReserveAgent placeholder (which holds the slot + enforces the cap)
	// with the full record, recording the Profile so it survives restart/role-change.
	f.AddAgent(&orchestrator.AgentInfo{
		AgentID:  agentID,
		Role:     req.Role,
		Profile:  profile,
		VMID:     vmInfo.VMID,
		AgentURL: vmInfo.AgentURL,
		Status:   orchestrator.AgentStatusReady,
	})
	if err := f.Persist(cp.workDir); err != nil {
		slog.Warn("add agent: persist failed", "flock_id", flockID, "agent_id", agentID, "err", err)
	}
	if _, err := f.TownWall.Post(orchestrator.SystemAuthor, fmt.Sprintf("Agent %s (%s) joined the flock", agentID, req.Role)); err != nil {
		slog.Warn("add agent: town wall post failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock: agent added", "flock_id", flockID, "agent_id", agentID, "role", req.Role, "vm_id", vmInfo.VMID)
	writeJSON(w, http.StatusCreated, FlockAddAgentResponse{
		AgentID:  agentID,
		Role:     req.Role,
		VMID:     vmInfo.VMID,
		AgentURL: vmInfo.AgentURL,
	})
}

// removeFlockAgent tears down a single agent's VM and removes it from the flock.
// DELETE /flocks/{id}/agents/{agent_id}. Removing the last agent leaves an empty
// flock (recoverable via add); use DELETE /flocks/{id} for the whole flock.
func (cp *ControlPlane) removeFlockAgent(w http.ResponseWriter, flockID, agentID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	unlock, ok := f.BeginMutation()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	defer unlock()
	var vmID string
	for _, a := range f.Snapshot() {
		if a.AgentID == agentID {
			vmID = a.VMID
			break
		}
	}
	if vmID == "" {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent not found"))
		return
	}

	cp.destroyVM(vmID)
	cp.watchdog.ForgetVM(vmID)
	f.RemoveAgent(agentID)
	if err := f.Persist(cp.workDir); err != nil {
		slog.Warn("remove agent: persist failed", "flock_id", flockID, "agent_id", agentID, "err", err)
	}
	if _, err := f.TownWall.Post(orchestrator.SystemAuthor, fmt.Sprintf("Agent %s left the flock", agentID)); err != nil {
		slog.Warn("remove agent: town wall post failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock: agent removed", "flock_id", flockID, "agent_id", agentID, "vm_id", vmID)
	writeJSON(w, http.StatusOK, map[string]string{"flock_id": flockID, "agent_id": agentID, "status": "removed"})
}

// changeFlockAgentRole recreates an agent's VM under a new role so the new role's
// sizing and system prompt take effect (role is bound at spawn time). The
// agent_id and agent_token are preserved (like restart); only the role and the
// backing VM change. PATCH /flocks/{id}/agents/{agent_id}  {"role":"reviewer"}.
func (cp *ControlPlane) changeFlockAgentRole(w http.ResponseWriter, r *http.Request, flockID, agentID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	unlock, ok := f.BeginMutation()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	defer unlock()
	var req AgentRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	roles, err := normalizeDaemonFlockRoles([]string{req.Role})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.Role = roles[0]

	var oldVMID, curRole string
	for _, a := range f.Snapshot() {
		if a.AgentID == agentID {
			oldVMID = a.VMID
			curRole = a.Role
			break
		}
	}
	if oldVMID == "" {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent not found"))
		return
	}
	if req.Role == curRole {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("agent already has role %q (use restart to recreate)", curRole))
		return
	}

	// Role and profile are separate: req.Profile (when set) selects sizing/model/
	// system prompt independently of the role label. Validate BEFORE destroying the
	// old VM so an invalid profile leaves the agent intact.
	newProfile := req.Role
	if strings.TrimSpace(req.Profile) != "" {
		newProfile = strings.TrimSpace(req.Profile)
	}
	configPath, secretsPath, err := cp.profileConfigPaths(newProfile)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	agentProfile := LookupProfile(newProfile)

	// Token must be read before destroyVM removes the runningVM entry.
	cp.mu.RLock()
	var oldToken string
	if v, ok := cp.vms[oldVMID]; ok {
		oldToken = v.agentToken
	}
	cp.mu.RUnlock()

	cp.destroyVM(oldVMID)
	cp.watchdog.ForgetVM(oldVMID)

	info, _, err := cp.spawnVMInternal(spawnVMOptions{
		Profile:           newProfile,
		ConfigPath:        configPath,
		SecretsPath:       secretsPath,
		TenantID:          f.TenantID,
		EgressPolicy:      f.EgressPolicy,
		SystemPrompt:      cp.loadProfileSystemPrompt(agentProfile.ProfileDir),
		FlockID:           flockID,
		AgentID:           agentID,
		AgentToken:        oldToken,
		ControlPlaneToken: cp.controlPlaneTokenForVM(),
		VcpuCount:         agentProfile.VcpuCount,
		MemSizeMib:        agentProfile.MemSizeMib,
	})
	if err != nil {
		f.UpdateAgentStatus(agentID, orchestrator.AgentStatusDead)
		if perr := f.Persist(cp.workDir); perr != nil {
			slog.Warn("change role: persist dead status failed", "agent_id", agentID, "err", perr)
		}
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	f.ChangeAgentRole(agentID, req.Role, newProfile)
	f.UpdateAgentVM(agentID, info.VMID, info.AgentURL)
	if err := f.Persist(cp.workDir); err != nil {
		slog.Warn("change role: persist failed", "flock_id", flockID, "err", err)
	}
	if _, err := f.TownWall.Post(orchestrator.SystemAuthor, fmt.Sprintf("Agent %s role changed %s → %s", agentID, curRole, req.Role)); err != nil {
		slog.Warn("change role: town wall post failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock: agent role changed", "flock_id", flockID, "agent_id", agentID, "old_role", curRole, "new_role", req.Role, "old_vm_id", oldVMID, "new_vm_id", info.VMID)
	writeJSON(w, http.StatusOK, info)
}

// pauseFlock pauses every member VM via Firecracker PauseVM. Runtime-only: agent
// status flips to "paused" and Flock.Paused is set, but nothing is persisted (a
// daemon restart brings members back running). On a partial failure the already-
// paused members are resumed (rollback). POST /flocks/{id}/pause.
func (cp *ControlPlane) pauseFlock(w http.ResponseWriter, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	unlock, ok := f.BeginMutation()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	defer unlock()
	type pausedAgent struct {
		vmID    string
		agentID string
	}
	var paused []pausedAgent
	rollback := func() {
		for i := len(paused) - 1; i >= 0; i-- {
			cp.mu.RLock()
			v, ok := cp.vms[paused[i].vmID]
			cp.mu.RUnlock()
			if ok {
				_ = v.machine.ResumeVM(context.Background())
			}
			f.RestorePausedAgentStatus(paused[i].agentID, orchestrator.AgentStatusReady)
		}
	}
	for _, a := range f.Snapshot() {
		cp.mu.RLock()
		v, ok := cp.vms[a.VMID]
		cp.mu.RUnlock()
		if !ok {
			slog.Warn("flock pause: vm not found, skipping", "flock_id", flockID, "agent_id", a.AgentID, "vm_id", a.VMID)
			continue
		}
		f.MarkAgentPaused(a.AgentID)
		paused = append(paused, pausedAgent{vmID: a.VMID, agentID: a.AgentID})
		if err := v.machine.PauseVM(context.Background()); err != nil {
			rollback()
			writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("pause %s: %w", a.AgentID, err))
			return
		}
	}
	f.SetPaused(true)
	if _, err := f.TownWall.Post(orchestrator.SystemAuthor, fmt.Sprintf("Flock paused (%s)", agentCount(len(paused)))); err != nil {
		slog.Warn("flock pause: town wall post failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock: paused", "flock_id", flockID, "agents", len(paused))
	writeJSON(w, http.StatusOK, map[string]any{"flock_id": flockID, "status": "paused", "agents": len(paused)})
}

// resumeFlock resumes every member VM of a paused flock (best-effort per member).
// POST /flocks/{id}/resume.
func (cp *ControlPlane) resumeFlock(w http.ResponseWriter, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	unlock, ok := f.BeginMutation()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	defer unlock()
	resumed := 0
	for _, a := range f.Snapshot() {
		cp.mu.RLock()
		v, ok := cp.vms[a.VMID]
		cp.mu.RUnlock()
		if !ok {
			slog.Warn("flock resume: vm not found, skipping", "flock_id", flockID, "agent_id", a.AgentID, "vm_id", a.VMID)
			continue
		}
		if err := v.machine.ResumeVM(context.Background()); err != nil {
			slog.Warn("flock resume: ResumeVM failed", "flock_id", flockID, "agent_id", a.AgentID, "err", err)
			continue
		}
		resumed++
		f.RestorePausedAgentStatus(a.AgentID, orchestrator.AgentStatusReady)
		if cp.watchdog != nil {
			cp.watchdog.ForgetVM(a.VMID)
		}
	}
	f.SetPaused(false)
	if _, err := f.TownWall.Post(orchestrator.SystemAuthor, fmt.Sprintf("Flock resumed (%s)", agentCount(resumed))); err != nil {
		slog.Warn("flock resume: town wall post failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock: resumed", "flock_id", flockID, "agents", resumed)
	writeJSON(w, http.StatusOK, map[string]any{"flock_id": flockID, "status": "resumed", "agents": resumed})
}

// BroadcastRequest is the POST /flocks/{id}/broadcast body: the prompt sent to
// every member agent.
type BroadcastRequest struct {
	Body string `json:"body"`
}

// broadcastResult captures one agent's outcome in a broadcast fan-out:
// "ok" (task ran), "busy" (agent already running a task → 503), or "error".
type broadcastResult struct {
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// broadcastFlock fans a single prompt out to every member agent's /tasks
// endpoint in parallel and gathers each agent's result (scatter-gather), then
// records the broadcast on the Town Wall. Agents already running a task answer
// 503 and are reported "busy" (counted as skipped); unreachable agents are
// "error". The call blocks until every agent finishes, like calling /tasks on
// each; cancellation rides on the request context.
// POST /flocks/{id}/broadcast  {"body":"prompt for all agents"}.
func (cp *ControlPlane) broadcastFlock(w http.ResponseWriter, r *http.Request, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	var req BroadcastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if req.Body == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("body required"))
		return
	}

	agents := f.Snapshot()
	results := make(map[string]broadcastResult, len(agents))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a *orchestrator.AgentInfo) {
			defer wg.Done()
			res := cp.dispatchBroadcastTask(r.Context(), a.VMID, req.Body)
			mu.Lock()
			results[a.AgentID] = res
			mu.Unlock()
		}(a)
	}
	wg.Wait()

	sent, skipped, failed := 0, 0, 0
	for _, res := range results {
		switch res.Status {
		case "ok":
			sent++
		case "busy":
			skipped++
		default:
			failed++
		}
	}

	if _, err := f.TownWall.Post(orchestrator.SystemAuthor,
		fmt.Sprintf("Broadcast to %s (%d sent, %d busy, %d failed): %s",
			agentCount(len(agents)), sent, skipped, failed, req.Body)); err != nil {
		slog.Warn("broadcast: town wall post failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock: broadcast", "flock_id", flockID, "agents", len(agents), "sent", sent, "busy", skipped, "failed", failed)
	writeJSON(w, http.StatusOK, map[string]any{
		"flock_id": flockID,
		"agents":   len(agents),
		"sent":     sent,
		"skipped":  skipped,
		"failed":   failed,
		"results":  results,
	})
}

// dispatchBroadcastTask POSTs one prompt to a single VM's goose-agent /tasks
// endpoint, injecting the per-VM agent token, and maps the outcome onto a
// broadcastResult. Mirrors proxyAgentEndpoint's token-injection path but
// buffers the (bounded) result instead of streaming it.
func (cp *ControlPlane) dispatchBroadcastTask(ctx context.Context, vmID, prompt string) broadcastResult {
	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		return broadcastResult{Status: "error", Error: "vm not found"}
	}
	body, _ := json.Marshal(map[string]string{"prompt": prompt})
	url := fmt.Sprintf("http://%s:%d/tasks", v.GuestIP, agentPort)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return broadcastResult{Status: "error", Error: err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if v.agentToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+v.agentToken)
	}
	resp, err := cp.agentHTTPClient.Do(httpReq)
	if err != nil {
		return broadcastResult{Status: "error", Error: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		return broadcastResult{Status: "busy"}
	}
	raw, _ := io.ReadAll(resp.Body)
	var tr struct {
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(raw, &tr)
	if resp.StatusCode != http.StatusOK {
		errMsg := tr.Error
		if errMsg == "" {
			errMsg = strings.TrimSpace(string(raw))
		}
		return broadcastResult{Status: "error", Output: tr.Output, Error: errMsg}
	}
	return broadcastResult{Status: "ok", Output: tr.Output, Error: tr.Error}
}

// spawnVMForFlock spawns one VM as a flock member. profile is mapped through
// LookupProfile to determine VM sizing, the goose config directory, and the
// system prompt injected at boot. The profile may differ from the agent's logical
// role: createFlock passes the chosen profile, while add-agent passes the role
// (preserving the legacy role==profile behavior). The control plane token is
// auto-injected (apiClients[0]) so the in-VM /townwall/post forwarder
// authenticates against an auth-on control plane without manual setup.
func (cp *ControlPlane) spawnVMForFlock(flockID, agentID, profile, tenantID, egressPolicy string) (*VMInfo, string, error) {
	configPath, secretsPath, err := cp.profileConfigPaths(profile)
	if err != nil {
		return nil, "", err
	}
	agentProfile := LookupProfile(profile)
	return cp.spawnVMInternal(spawnVMOptions{
		Profile:           profile,
		ConfigPath:        configPath,
		SecretsPath:       secretsPath,
		TenantID:          tenantID,
		EgressPolicy:      egressPolicy,
		SystemPrompt:      cp.loadProfileSystemPrompt(agentProfile.ProfileDir),
		FlockID:           flockID,
		AgentID:           agentID,
		ControlPlaneToken: cp.controlPlaneTokenForVM(),
		VcpuCount:         agentProfile.VcpuCount,
		MemSizeMib:        agentProfile.MemSizeMib,
	})
}

func sseEmit(w http.ResponseWriter, m orchestrator.Message) {
	b, _ := json.Marshal(m)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

// buildPublicURLPath returns publicURL+p when EPHEMERA_PUBLIC_URL is set,
// otherwise a localhost URL using the configured apiAddr.
func buildPublicURLPath(p string) string {
	if publicURL != "" {
		return publicURL + p
	}
	return "http://" + apiAddr + p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("writeJSON encode failed", "err", err)
	}
}
