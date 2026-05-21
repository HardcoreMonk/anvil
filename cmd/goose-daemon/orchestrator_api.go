package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ephemera/internal/orchestrator"
)

// Maximum number of agents a single POST /flocks request may spawn.
// Limits IP pool / TAP exhaustion from a runaway caller.
const maxAgentsPerFlock = 20

// FlockCreateRequest is the POST /flocks body. roles[i] becomes one VM.
type FlockCreateRequest struct {
	Task         string   `json:"task"`
	Roles        []string `json:"roles"`
	TenantID     string   `json:"tenant_id,omitempty"`
	EgressPolicy string   `json:"egress_policy,omitempty"`
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
		cp.townWallHistory(w, flockID)
	case sub == "post" && r.Method == http.MethodPost:
		cp.postToTownWall(w, r, flockID)
	case strings.HasPrefix(sub, "agents/") && r.Method == http.MethodPost:
		rest := strings.TrimPrefix(sub, "agents/")
		agentID, action, _ := strings.Cut(rest, "/")
		if action == "restart" && agentID != "" {
			cp.restartAgent(w, flockID, agentID)
			return
		}
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("unsupported agent action"))
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

	flockID := fmt.Sprintf("flock-%d", time.Now().UnixNano())
	townWallPath := filepath.Join(cp.workDir, "flocks", flockID, "TOWN_WALL.log")
	if err := os.MkdirAll(filepath.Dir(townWallPath), 0755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	flock, err := cp.flockMgr.Create(flockID, req.Task, req.TenantID, req.EgressPolicy, townWallPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}

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
	for _, role := range req.Roles {
		roleSeq[role]++
		agentID := fmt.Sprintf("%s-%d", role, roleSeq[role])
		vmInfo, _, err := cp.spawnVMForFlock(flockID, agentID, role, req.TenantID, req.EgressPolicy)
		if err != nil {
			cleanup()
			writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("spawn %s: %w", agentID, err))
			return
		}
		flock.AddAgent(&orchestrator.AgentInfo{
			AgentID:  agentID,
			Role:     role,
			VMID:     vmInfo.VMID,
			AgentURL: vmInfo.AgentURL,
			Status:   orchestrator.AgentStatusReady,
		})
		spawned = append(spawned, vmInfo.VMID)
	}

	if _, err := flock.TownWall.Post("orchestrator",
		fmt.Sprintf("Flock spawned with %d agents: %v", len(req.Roles), req.Roles)); err != nil {
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
	f, ok := cp.flockMgr.Delete(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
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

func (cp *ControlPlane) townWallHistory(w http.ResponseWriter, flockID string) {
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
	if history == nil {
		history = []orchestrator.Message{}
	}
	writeJSON(w, http.StatusOK, history)
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
	if len(roles) > maxAgentsPerFlock {
		return nil, fmt.Errorf("max %d agents per flock", maxAgentsPerFlock)
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

	var oldVMID, role string
	for _, a := range f.Snapshot() {
		if a.AgentID == agentID {
			oldVMID = a.VMID
			role = a.Role
			break
		}
	}
	if oldVMID == "" {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent not found"))
		return
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

	configPath, secretsPath, err := cp.profileConfigPaths(role)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	profile := LookupProfile(role)
	info, _, err := cp.spawnVMInternal(spawnVMOptions{
		Profile:           role,
		ConfigPath:        configPath,
		SecretsPath:       secretsPath,
		TenantID:          f.TenantID,
		EgressPolicy:      f.EgressPolicy,
		SystemPrompt:      cp.loadProfileSystemPrompt(profile.ProfileDir),
		FlockID:           flockID,
		AgentID:           agentID,
		AgentToken:        oldToken,
		ControlPlaneToken: cp.controlPlaneTokenForVM(),
		VcpuCount:         profile.VcpuCount,
		MemSizeMib:        profile.MemSizeMib,
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

// spawnVMForFlock spawns one VM as a flock member. role is mapped through
// LookupProfile to determine VM sizing, the goose config directory, and the
// system prompt that will be injected at boot. The control plane token is
// auto-injected (apiClients[0]) so the in-VM /townwall/post forwarder
// authenticates against an auth-on control plane without manual setup.
func (cp *ControlPlane) spawnVMForFlock(flockID, agentID, role, tenantID, egressPolicy string) (*VMInfo, string, error) {
	configPath, secretsPath, err := cp.profileConfigPaths(role)
	if err != nil {
		return nil, "", err
	}
	agentProfile := LookupProfile(role)
	return cp.spawnVMInternal(spawnVMOptions{
		Profile:           role,
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
