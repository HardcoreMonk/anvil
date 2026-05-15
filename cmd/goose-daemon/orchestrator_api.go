package main

import (
	"encoding/json"
	"fmt"
	"log"
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
	spawned := make([]string, 0, len(req.Roles))
	cleanup := func() {
		for _, vmID := range spawned {
			cp.destroyVM(vmID)
		}
		cp.flockMgr.Delete(flockID)
	}
	for i, role := range req.Roles {
		agentID := fmt.Sprintf("%s-%d", role, i+1)
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
		log.Printf("Flock [%s]: failed to post initial Town Wall message: %v", flockID, err)
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
			cp.destroyVM(vmID)
		}(a.VMID)
	}
	wg.Wait()
	log.Printf("Flock [%s] destroyed (%d agents)", flockID, len(agents))
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

// spawnVMForFlock spawns one VM as a flock member. role is mapped through
// LookupProfile to determine VM sizing, the goose config directory, and the
// system prompt that will be injected at boot.
func (cp *ControlPlane) spawnVMForFlock(flockID, agentID, role, tenantID, egressPolicy string) (*VMInfo, string, error) {
	configPath, secretsPath, err := cp.profileConfigPaths(role)
	if err != nil {
		return nil, "", err
	}
	agentProfile := LookupProfile(role)
	return cp.spawnVMInternal(spawnVMOptions{
		Profile:      role,
		ConfigPath:   configPath,
		SecretsPath:  secretsPath,
		TenantID:     tenantID,
		EgressPolicy: egressPolicy,
		SystemPrompt: cp.loadProfileSystemPrompt(agentProfile.ProfileDir),
		FlockID:      flockID,
		AgentID:      agentID,
		VcpuCount:    agentProfile.VcpuCount,
		MemSizeMib:   agentProfile.MemSizeMib,
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
		log.Printf("writeJSON encode: %v", err)
	}
}
