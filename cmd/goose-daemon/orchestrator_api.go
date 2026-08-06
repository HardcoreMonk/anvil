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
	"strconv"
	"strings"
	"sync"
	"time"

	"ephemera/internal/orchestrator"
)

// defaultMaxAgentsPerFlock is the per-flock agent cap when a flock does not set
// its own max_agents (v0.4.3). Limits IP pool / TAP exhaustion from a runaway caller.
const defaultMaxAgentsPerFlock = 20

// Response-body caps for everything this daemon reads back from a LESS trusted
// peer — an in-guest goose-agent or another host's daemon. Both sit on the far
// side of a VM/host boundary, so their response length is attacker-influenced:
// a guest that repoints its agent port at a body-flooding server would otherwise
// grow the root daemon's heap without bound (io.ReadAll on a network reader has
// no limit), turning a guest-local capability into a host OOM. Mirrors the caps
// already applied on the collector (stats_collector.go) and gateway
// (internal/mcpgateway) read paths.
//
// The caps are sized to the largest LEGITIMATE payload of each path, so a cap
// hit always means "abnormal", never "busy day":
const (
	// maxPeerErrorBody bounds a body read purely to quote a peer's refusal back
	// to the caller. The status code carries the meaning, so these are truncated
	// rather than escalated; 64 KiB holds any realistic JSON error envelope.
	maxPeerErrorBody = 64 << 10

	// maxWallMessageBody bounds the single Town Wall Message a home daemon
	// echoes back for a relayed post. A wall body may be large (agents publish
	// whole files through gtwall), but the echo can never legitimately exceed
	// one message; 1 MiB matches goose-agent's own maxWorkloadOutputBytes.
	maxWallMessageBody = 1 << 20

	// maxWallHistoryBody bounds a relayed /wall/history read — the largest
	// legitimate cross-daemon payload, since it serializes the whole active
	// wall. The active log is itself capped by EPHEMERA_TOWNWALL_MAX_MIB
	// (default 10 MiB), and JSON framing/escaping inflates that by well under
	// 3x, so 32 MiB clears any honest history while still bounding the read.
	maxWallHistoryBody = 32 << 20

	// maxAgentResponseBody bounds a goose-agent task result (/tasks) and the
	// daemon-to-daemon /call hop that wraps one. Matches the 4 MiB ceiling used
	// for workspace file transfers (maxWorkspaceFileBytes) and the MCP gateway.
	maxAgentResponseBody = 4 << 20
)

// readCapped reads at most max bytes from r and reports whether the source had
// more to give. Callers decide what an over-cap read means: an error-snippet
// reader may keep the truncated bytes, but a PAYLOAD reader must fail — a
// silently cut task result or wall record is an integrity failure the receiving
// caller has no way to detect.
func readCapped(r io.Reader, max int64) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if int64(len(b)) > max {
		return b[:max], true, err
	}
	return b, false, err
}

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

// distributedFlockRequest is the POST /flocks/{id}/distributed body: the home
// daemon uses it to register a hub flock owning the canonical cross-host Town
// Wall (v0.7.x routed flocks).
type distributedFlockRequest struct {
	Roster     []orchestrator.RosterMember `json:"roster"`
	RelayToken string                      `json:"relay_token"`
	// CallToken is the per-flock call-hop secret (admitted for /call only,
	// never wall paths — see callPathFlockID).
	CallToken string `json:"call_token"`
}

// relayFlockRequest is the POST /flocks/{id}/relay body: a member daemon uses
// it to register a relay flock forwarding Town Wall traffic to the home
// daemon (v0.7.x routed flocks).
type relayFlockRequest struct {
	HomeAddr   string `json:"home_addr"`
	RelayToken string `json:"relay_token"`
	// CallToken is the per-flock call-hop secret (admitted for /call only,
	// never wall paths — see callPathFlockID).
	CallToken string `json:"call_token"`
	// Agents is this host's member list (agent_id + vm_id only; Host/Addr are
	// remote-host fields and are ignored here) — 2026-07-08 design correction:
	// a hopped call landing on this relay flock (the real topology for a
	// member daemon receiving home's 2nd hop) resolves locally against this
	// list instead of unconditionally 404ing. See callFlockAgent.
	Agents []orchestrator.RosterMember `json:"agents"`
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
	case sub == "distributed" && r.Method == http.MethodPost:
		cp.registerDistributedFlock(w, r, flockID)
	case sub == "relay" && r.Method == http.MethodPost:
		cp.registerRelayFlock(w, r, flockID)
	case sub == "call" && r.Method == http.MethodPost:
		cp.callFlockAgent(w, r, flockID)
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
	// Revoke the flock's scoped relay-token admission (Task 8): once the flock is
	// gone, a stale relay token must no longer authenticate a cross-host wall hop.
	cp.removeRelayToken(flockID)
	// Revoke the flock's scoped call-token admission too, mirroring the relay
	// token above: a stale call token must no longer authenticate a call entry.
	cp.removeCallToken(flockID)
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
	// Delete via the flock (writeMu-serialized) so a late Persist racing this
	// teardown cannot resurrect metadata.json (R1). f is already marked deleted
	// by BeginDelete above, so any such Persist skips its write.
	if err := f.DeleteMetadata(cp.workDir); err != nil {
		slog.Warn("flock: remove persisted metadata failed", "flock_id", flockID, "err", err)
	}
	slog.Warn("flock destroyed", "flock_id", flockID, "agents", len(agents))
	cp.metrics.flockDestroy.Inc()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "flock_id": flockID})
}

// relayTownWallPost forwards a member's post to the home daemon that owns the
// canonical wall. Only {agent_id, body} crosses the wire; the per-flock relay
// token authenticates the daemon-to-daemon hop. No per-VM credential is sent.
func relayTownWallPost(ctx context.Context, homeAddr, relayToken, flockID, agentID, body string) (int, []byte, error) {
	payload, _ := json.Marshal(map[string]string{"agent_id": agentID, "body": body})
	url := strings.TrimRight(homeAddr, "/") + "/flocks/" + flockID + "/post"
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+relayToken)
		return req, nil
	}
	resp, err := doWithDialRetry(ctx, newAgentHTTPClient(), build)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// The echoed record is a payload mirrored verbatim to the guest, so an
	// over-cap body is an error rather than a silent truncation: a cut JSON
	// object would reach the caller as a corrupt Message it cannot detect.
	respBody, truncated, rerr := readCapped(resp.Body, maxWallMessageBody)
	if rerr != nil {
		return 0, nil, rerr
	}
	if truncated {
		return 0, nil, fmt.Errorf("home response exceeded %d bytes", maxWallMessageBody)
	}
	return resp.StatusCode, respBody, nil
}

// errReservedWallAuthor is the refusal for a caller trying to post under the
// control plane's own author label. Shared by the relay and wall-owner paths so
// both report identically.
func errReservedWallAuthor() error {
	return fmt.Errorf("agent_id %q is reserved for the control plane", orchestrator.SystemAuthor)
}

func (cp *ControlPlane) postToTownWall(w http.ResponseWriter, r *http.Request, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	// Relay flocks (member host) own no local wall: forward {agent_id, body} to
	// the home daemon and mirror its status+body back. The per-flock relay token
	// authenticates the daemon hop; no per-VM credential crosses the wire. The
	// hub/local paths fall through to the local f.TownWall.Post below. (A relayed
	// post arriving at the hub was already scoped to THIS flock's wall by
	// authMiddleware's relayTokenFor admission, so no re-check is needed here.)
	if f.Kind == orchestrator.FlockKindRelay {
		var req TownWallPostRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err)
			return
		}
		if req.AgentID == "" || req.Body == "" {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("agent_id and body required"))
			return
		}
		// Reject a forged control-plane author before the hop, so a member host
		// never carries one across the wire. Roster membership itself is NOT
		// checked here: a relay flock owns no wall and has no authoritative
		// roster (POST /flocks/{id}/relay takes `agents` as optional, and the
		// deployed member-host registration omits it), so the wall OWNER — home,
		// which reaches the hub branch below — is the single enforcement point.
		if req.AgentID == orchestrator.SystemAuthor {
			writeJSONError(w, http.StatusForbidden, errReservedWallAuthor())
			return
		}
		status, respBody, err := relayTownWallPost(r.Context(), f.HomeAddr, f.RelayToken, flockID, req.AgentID, req.Body)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, fmt.Errorf("town wall relay to home failed for flock %q", flockID))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
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
	// AUTHORSHIP CHECK (this is the wall owner: local flock or hub). Reaching
	// here only proves admission — /flocks/{id}/post is opened by the flock's
	// relay token, which is the GUEST capability token, so req.AgentID is
	// attacker-controlled input, not an identity. Two rules, in order:
	//
	//  1. SystemAuthor is never assignable by a caller. It labels messages the
	//     control plane posts on its own behalf (lifecycle events, watchdog
	//     notices); agents read the wall to decide what to do next, so a forged
	//     "control-plane" instruction is a direct behavior-manipulation
	//     primitive. Checked before membership so a roster that happens to carry
	//     the reserved label cannot unlock it.
	//  2. The author must be on this flock's roster. Otherwise any flock member
	//     could publish under a peer's name, or invent one, and the wall would
	//     record it as genuine — corrupting both live coordination and the audit
	//     trail that History replays to restarted and late-joining agents.
	if req.AgentID == orchestrator.SystemAuthor {
		writeJSONError(w, http.StatusForbidden, errReservedWallAuthor())
		return
	}
	if !f.HasMember(req.AgentID) {
		slog.Warn("town wall: rejected post from non-member", "flock_id", flockID, "agent_id", req.AgentID)
		writeJSONError(w, http.StatusForbidden, fmt.Errorf("agent %q is not a member of flock %q", req.AgentID, flockID))
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
	// Relay flocks proxy history reads to the home daemon that owns the canonical
	// wall, forwarding the query string (agent_id/since/until/contains filters)
	// verbatim so filtering happens once, at the home. Hub/local read locally below.
	if f.Kind == orchestrator.FlockKindRelay {
		url := strings.TrimRight(f.HomeAddr, "/") + "/flocks/" + flockID + "/wall/history"
		if raw := r.URL.RawQuery; raw != "" {
			url += "?" + raw
		}
		build := func() (*http.Request, error) {
			hreq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
			if err != nil {
				return nil, err
			}
			hreq.Header.Set("Authorization", "Bearer "+f.RelayToken)
			return hreq, nil
		}
		resp, err := doWithDialRetry(r.Context(), newAgentHTTPClient(), build)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, fmt.Errorf("town wall history relay to home failed for flock %q", flockID))
			return
		}
		defer resp.Body.Close()
		// Payload (the home's whole history array): fail rather than mirror a
		// truncated JSON array, which the caller would parse as a short history
		// and mistake for the complete record.
		b, truncated, rerr := readCapped(resp.Body, maxWallHistoryBody)
		if rerr != nil || truncated {
			writeJSONError(w, http.StatusBadGateway, fmt.Errorf("town wall history relay to home failed for flock %q", flockID))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
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
	// Relay flocks own no local wall/subscriber: proxy the SSE stream from the
	// home daemon that owns the canonical wall. Hub/local stream locally below.
	if f.Kind == orchestrator.FlockKindRelay {
		cp.streamTownWallRelay(w, r, f, flockID)
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

// streamTownWallRelay proxies a member host's Town Wall SSE stream to the home
// daemon that owns the canonical wall. It dials {HomeAddr}/flocks/{id}/wall with
// the per-flock relay token and copies each chunk through with a per-chunk
// Flusher so events reach the caller live (mirrors the local flush loop). The
// upstream request rides the caller's context, so a client disconnect tears down
// the home connection too.
func (cp *ControlPlane) streamTownWallRelay(w http.ResponseWriter, r *http.Request, f *orchestrator.Flock, flockID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	url := strings.TrimRight(f.HomeAddr, "/") + "/flocks/" + flockID + "/wall"
	hreq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("town wall stream relay request build failed for flock %q", flockID))
		return
	}
	hreq.Header.Set("Authorization", "Bearer "+f.RelayToken)
	resp, err := newAgentHTTPClient().Do(hreq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("town wall stream relay to home failed for flock %q", flockID))
		return
	}
	defer resp.Body.Close()
	// Home refused the stream (auth failure, unknown flock, ...): mirror its
	// status and JSON error body like the post/history relay handlers instead
	// of stamping SSE headers on a response that carries no event stream.
	if resp.StatusCode != http.StatusOK {
		// Error snippet, not a payload: truncate and keep mirroring home's status.
		// The status code is what the caller acts on, so quoting a partial message
		// beats masking a 401 behind a 502.
		b, _, _ := readCapped(resp.Body, maxPeerErrorBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if rerr != nil {
			return
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
	// Payload (the agent's task result): report the flood as a per-agent failure
	// instead of returning a truncated output that the caller would read as the
	// agent's real answer. One misbehaving member must not fail the fan-out, so
	// this stays a broadcastResult rather than an HTTP error.
	raw, truncated, rerr := readCapped(resp.Body, maxAgentResponseBody)
	if rerr != nil {
		return broadcastResult{Status: "error", Error: rerr.Error()}
	}
	if truncated {
		return broadcastResult{Status: "error", Error: fmt.Sprintf("agent response exceeded %d bytes", maxAgentResponseBody)}
	}
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

// FlockCallRequest is the POST /flocks/{id}/call body: dispatch prompt to
// the named flock member (local, hub-remote, or via relay-to-home).
type FlockCallRequest struct {
	AgentID string `json:"agent_id"`
	Prompt  string `json:"prompt"`
}

const callHopHeader = "X-Ephemera-Call-Hop"
const taskDepthHeader = "X-Ephemera-Task-Depth"

// taskDepthFromRequest reads the current nested-invocation depth from
// X-Ephemera-Task-Depth (absent or non-positive → 0). Shared by
// proxyAgentEndpoint's /tasks depth guard and dispatchFlockCall's local call
// dispatch so both hops enforce the identical accumulation rule.
func taskDepthFromRequest(r *http.Request) int {
	if h := r.Header.Get(taskDepthHeader); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// callFlockAgent dispatches a prompt to a named flock member. Local/hub
// flocks resolve locally (hub falls back to its VMID/Addr roster for members
// on other hosts); relay flocks forward to the home daemon. The per-flock
// call token authenticates the daemon-to-daemon hops. X-Ephemera-Call-Hop
// marks ONLY the final hop (hub -> roster-target); the relay -> home leg is
// deliberately left unmarked because home is a resolver, not a terminus, and
// must remain free to take its own 2nd hop (2026-07-08 C1 fix — see
// forwardFlockCall's markHop doc for the full loop-termination argument). Any
// request that already carries the marker never forwards again (loop guard).
// Only {agent_id, prompt} plus the depth/hop headers cross the wire — the
// target VM's agent token is injected by the target host daemon alone.
// Errors carry flock/host/agent identifiers only.
func (cp *ControlPlane) callFlockAgent(w http.ResponseWriter, r *http.Request, flockID string) {
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("flock not found"))
		return
	}
	var req FlockCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.AgentID == "" || strings.TrimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("agent_id and prompt required"))
		return
	}
	hopped := r.Header.Get(callHopHeader) != ""

	// Hopped request (already forwarded once, by home or a hub 2nd hop): never
	// forward again — loop guard — and this holds regardless of f.Kind. In the
	// real topology a target member daemon has this flock registered as
	// "relay" (it forwards its own outbound wall/call traffic to home), so a
	// hopped call landing here must still resolve against THIS host's local
	// agents (2026-07-08 design correction: relay+hopped is no longer an
	// unconditional 404 — see relayFlockRequest.Agents /
	// FlockManager.RegisterRelay). Local/hub flocks take the identical path.
	if hopped {
		if vmID, ok := cp.flockAgentLocalVM(f, req.AgentID); ok {
			cp.dispatchFlockCall(w, r, vmID, req.Prompt)
			return
		}
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent %q not found in flock %q", req.AgentID, flockID))
		return
	}

	// Relay flock, non-hopped: forward to home. markHop=false — home is a
	// RESOLVER, not a terminus: it must still be free to take its own 2nd hop
	// (hub -> roster-target) below, which requires an unhopped request. See the
	// loop-termination note on forwardFlockCall.
	if f.Kind == orchestrator.FlockKindRelay {
		ctx, cancel := context.WithTimeout(r.Context(), 290*time.Second)
		defer cancel()
		status, body, err := forwardFlockCall(ctx, f.HomeAddr, f.CallToken, flockID, req, r.Header.Get(taskDepthHeader), false)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, fmt.Errorf("call relay to home failed for flock %q", flockID))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	// Local/hub: local VM registry first (works for local flocks and for hub
	// members living on the home host — the daemon does not know its own
	// control-plane host name, so locality == VM presence).
	if vmID, ok := cp.flockAgentLocalVM(f, req.AgentID); ok {
		cp.dispatchFlockCall(w, r, vmID, req.Prompt)
		return
	}
	// Hub: remote member via VMID/Addr roster — one hop only. markHop=true —
	// this IS the terminal hop: the target daemon must resolve purely locally
	// (see the `hopped` branch above), never forwarding again.
	if f.Kind == orchestrator.FlockKindHub {
		if member, ok := rosterMember(f, req.AgentID); ok && member.Addr != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 280*time.Second)
			defer cancel()
			// Hub tokens are re-seated by every reconcile re-POST (UpdateHubTokens),
			// so read them through the locked getter — the raw field races the
			// refill (D1b review).
			_, callToken := f.DistributedTokens()
			status, body, err := forwardFlockCall(ctx, member.Addr, callToken, flockID, req, r.Header.Get(taskDepthHeader), true)
			if err != nil {
				writeJSONError(w, http.StatusBadGateway, fmt.Errorf("call hop to member host %q failed", member.Host))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
	}
	writeJSONError(w, http.StatusNotFound, fmt.Errorf("agent %q not found in flock %q", req.AgentID, flockID))
}

// forwardFlockCall sends {agent_id, prompt} to addr's /flocks/{id}/call with
// the per-flock call token and the caller's task depth propagated verbatim
// (accumulation happens only at the final local dispatch).
//
// markHop controls whether the hop marker (X-Ephemera-Call-Hop) is set on the
// outgoing request, and this is what makes the 2-hop topology terminate
// instead of looping:
//   - relay -> home (markHop=false): home is a RESOLVER, not a terminus. It
//     must receive an unmarked request so it remains free to take its own
//     2nd hop (hub -> roster-target) if the agent isn't local to home.
//   - hub -> roster-target (markHop=true): this is always the FINAL hop. The
//     target daemon's `hopped` branch in callFlockAgent resolves purely
//     locally and never forwards again, regardless of its own flock kind.
//
// Since a request can only ever be unmarked on the first (relay->home) leg
// and marked on the second (hub->target) leg, and a marked request can never
// be forwarded again, the chain is capped at exactly two forwards and cannot
// cycle.
func forwardFlockCall(ctx context.Context, addr, callToken, flockID string, call FlockCallRequest, depth string, markHop bool) (int, []byte, error) {
	payload, _ := json.Marshal(call)
	url := strings.TrimRight(addr, "/") + "/flocks/" + flockID + "/call"
	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+callToken)
		if markHop {
			req.Header.Set(callHopHeader, "1")
		}
		if depth != "" {
			req.Header.Set(taskDepthHeader, depth)
		}
		return req, nil
	}
	resp, err := doWithDialRetry(ctx, newAgentHTTPClient(), build)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// Payload (a remote agent's task result, wrapped by the peer daemon): same
	// integrity rule as the local dispatch — fail rather than mirror a cut body.
	body, truncated, rerr := readCapped(resp.Body, maxAgentResponseBody)
	if rerr != nil {
		return 0, nil, rerr
	}
	if truncated {
		return 0, nil, fmt.Errorf("peer response exceeded %d bytes", maxAgentResponseBody)
	}
	return resp.StatusCode, body, nil
}

// flockAgentLocalVM resolves agentID to a VMID present in THIS daemon's local
// VM registry (cp.vms) — the same map proxyAgentEndpoint looks up by vmID.
// Local flocks and hub flocks share the same check: a hub flock's own
// Agents map is normally empty (it owns no local VMs), so this also checks
// the hub Roster for a matching agentID whose VMID happens to be a VM this
// daemon runs — the "hub member lives on the home host" case. A daemon has
// no way to know its own control-plane host name, so VM-registry presence is
// the only reliable locality test.
func (cp *ControlPlane) flockAgentLocalVM(f *orchestrator.Flock, agentID string) (string, bool) {
	if a, ok := f.Agents[agentID]; ok {
		cp.mu.RLock()
		_, present := cp.vms[a.VMID]
		cp.mu.RUnlock()
		if present {
			return a.VMID, true
		}
	}
	// Locked snapshot, not the live slice: the roster is replaced by every
	// reconcile re-POST (SetRoster) and a raw range races that refresh.
	for _, m := range f.RosterSnapshot() {
		if m.AgentID != agentID || m.VMID == "" {
			continue
		}
		cp.mu.RLock()
		_, present := cp.vms[m.VMID]
		cp.mu.RUnlock()
		if present {
			return m.VMID, true
		}
	}
	return "", false
}

// rosterMember finds agentID in a hub flock's remote-membership roster. It
// iterates a locked snapshot: the live f.Roster slice is replaced by every
// reconcile re-POST (SetRoster), so ranging it directly races the refresh.
func rosterMember(f *orchestrator.Flock, agentID string) (orchestrator.RosterMember, bool) {
	for _, m := range f.RosterSnapshot() {
		if m.AgentID == agentID {
			return m, true
		}
	}
	return orchestrator.RosterMember{}, false
}

// dispatchFlockCall is the final local hop of a /call dispatch: it enforces
// the identical nested-invocation depth guard proxyAgentEndpoint applies to
// direct /tasks proxying (a call-path dispatch must never bypass that cap)
// before POSTing {"prompt": prompt} to the target VM's goose-agent /tasks
// endpoint with the per-VM agent token injected. The agent's raw response
// (status + body) is returned to the caller unmodified.
func (cp *ControlPlane) dispatchFlockCall(w http.ResponseWriter, r *http.Request, vmID, prompt string) {
	depth := taskDepthFromRequest(r)
	if depth >= maxTaskDepth {
		slog.Warn("call task depth exceeded", "vm_id", vmID, "depth", depth, "max", maxTaskDepth)
		writeJSONError(w, http.StatusLoopDetected, fmt.Errorf("max task depth %d exceeded", maxTaskDepth))
		return
	}
	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		writeJSONError(w, http.StatusNotFound, fmt.Errorf("vm not found"))
		return
	}
	body, _ := json.Marshal(map[string]string{"prompt": prompt})
	url := fmt.Sprintf("http://%s:%d/tasks", v.GuestIP, agentPort)
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if v.agentToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+v.agentToken)
	}
	httpReq.Header.Set(taskDepthHeader, strconv.Itoa(depth+1))
	resp, err := cp.agentHTTPClient.Do(httpReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("agent unreachable"))
		return
	}
	defer resp.Body.Close()
	// Payload (the agent's task result, mirrored verbatim): a guest that repoints
	// its agent port at a flooding server is exactly the M4 amplification, so cap
	// the read and fail closed instead of forwarding a truncated result.
	raw, truncated, rerr := readCapped(resp.Body, maxAgentResponseBody)
	if rerr != nil {
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("agent response read failed"))
		return
	}
	if truncated {
		slog.Warn("call: agent response exceeded cap", "vm_id", vmID, "max_bytes", maxAgentResponseBody)
		writeJSONError(w, http.StatusBadGateway, fmt.Errorf("agent response exceeded %d bytes", maxAgentResponseBody))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)
}

// registerDistributedFlock (home host) creates the canonical wall for a
// cross-host routed flock. Idempotent: re-registration reuses the existing wall.
// POST /flocks/{id}/distributed.
func (cp *ControlPlane) registerDistributedFlock(w http.ResponseWriter, r *http.Request, flockID string) {
	var req distributedFlockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if req.RelayToken == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("relay_token required"))
		return
	}
	// Admit the relay hop through authMiddleware, but ONLY for this flock's Town
	// Wall sub-paths: register relay_token in the scoped relay-token store, NOT
	// in cp.clients (a relay token must never be a full control-plane bearer).
	// authMiddleware is the transport gate; the hub post handler additionally
	// checks bearer == flock.RelayToken so a valid-but-wrong-flock token is
	// rejected (Task 4). If the daemon runs auth-disabled, only the hub check
	// applies. This MUST run on BOTH the fresh-registration path and the
	// already-exists idempotent path: a reconcile re-POST (Task 7) heals a
	// daemon that kept the hub flock but lost the relay-token admission. That
	// admission lives only in cp.relayTokens (in-memory, never persisted on the
	// daemon), so a daemon PROCESS RESTART drops it while the hub flock metadata
	// is reconstructed independently (recovery.go); the reconcile re-POST then
	// restores admission before returning. A SIGHUP token reload does NOT clear
	// it — ReloadClients only swaps cp.clients, never cp.relayTokens.
	if existing, ok := cp.flockMgr.Get(flockID); ok {
		switch existing.Kind {
		case orchestrator.FlockKindHub:
			// Re-registration refreshes the roster too (not only the token): the
			// post-spawn re-POST carries the VMID/Addr-enriched roster the initial
			// pre-spawn registration could not know, and reconcile re-POSTs depend
			// on the same path after a daemon restart.
			if len(req.Roster) > 0 {
				// UpdateHubRoster returns false only if the flock vanished (deleted
				// by a concurrent request) between the Get above and this call — a
				// narrow TOCTOU window, not reproducible as a deterministic unit
				// test. Log and continue: the roster update is best-effort here,
				// same as the token admission calls below.
				if !cp.flockMgr.UpdateHubRoster(flockID, req.Roster) {
					slog.Warn("flock: hub roster update raced with delete", "flock_id", flockID)
				}
			}
			cp.setRelayToken(flockID, req.RelayToken)
			cp.setCallToken(flockID, req.CallToken)
			// Re-seat the hub Flock struct's own relay/call tokens too. setRelayToken/
			// setCallToken above only refill INBOUND admission maps; the struct fields
			// drive the hub's OUTBOUND hops. A restarted home recovers the hub without
			// its tokens (never persisted), so without this its 2nd-hop /call to a
			// member sends an empty bearer and the member 401s (D1b). Best-effort like
			// the roster update above; empty tokens are ignored so a token-less re-POST
			// cannot blank a live secret.
			cp.flockMgr.UpdateHubTokens(flockID, req.RelayToken, req.CallToken)
			w.WriteHeader(http.StatusCreated)
			return
		case orchestrator.FlockKindRelay:
			// Failover promotion: the adapter (the sole control plane; this
			// endpoint sits behind the full CP bearer — relay/call tokens are
			// never admitted here) re-elected this member host as the flock's
			// new home. Fall through to the fresh hub registration below:
			// RegisterHub replaces the relay stub, local agent resolution moves
			// to the VMID-enriched roster + cp.vms presence (identical to a
			// first-generation home), and the hub's Agents map stays EMPTY so a
			// later best-effort DELETE of a stale failed-over hub can never
			// destroy this host's member VMs (deleteFlock destroys f.Agents).
		default:
			// A local flock owns this id: an id collision, never a failover.
			writeJSONError(w, http.StatusConflict, fmt.Errorf("flock %q already registered as a non-hub flock", flockID))
			return
		}
	}
	wallPath := filepath.Join(cp.workDir, "flocks", flockID, "TOWN_WALL.log")
	wall, err := orchestrator.NewTownWall(flockID, wallPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	cp.flockMgr.RegisterHub(flockID, wall, req.Roster, req.RelayToken, req.CallToken)
	cp.setRelayToken(flockID, req.RelayToken)
	cp.setCallToken(flockID, req.CallToken)
	w.WriteHeader(http.StatusCreated)
}

// registerRelayFlock (member host) registers a forward-to-home stub. Idempotent.
// POST /flocks/{id}/relay.
func (cp *ControlPlane) registerRelayFlock(w http.ResponseWriter, r *http.Request, flockID string) {
	var req relayFlockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if req.HomeAddr == "" || req.RelayToken == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("home_addr and relay_token required"))
		return
	}
	// A LOCAL flock owning this id is an id collision — refuse (unchanged). A
	// hub-kind occupant is the failover demotion path: the control plane's
	// record now names another host as home, so this (revived old-home) daemon
	// converts its stale hub into a relay stub. RegisterRelay below overwrites
	// the registry entry and persists kind=relay; the old TOWN_WALL.log stays
	// on disk as an audit artifact (spec: wall history is lost, not merged).
	// Relay->relay re-register remains the reconcile heal overwrite.
	if existing, ok := cp.flockMgr.Get(flockID); ok && existing.Kind == orchestrator.FlockKindLocal {
		writeJSONError(w, http.StatusConflict, fmt.Errorf("flock %q already registered as a non-relay flock", flockID))
		return
	}
	cp.flockMgr.RegisterRelay(flockID, req.HomeAddr, req.RelayToken, req.CallToken, req.Agents)
	// Admit the guest's relay/call tokens through THIS (member) daemon's
	// authMiddleware too — mirroring the hub registration above. Without this, a
	// guest VM's inbound gtwall/gtcall request that lands on the member daemon
	// (auth-on) 401s before it can even reach the relay-forward-to-home logic:
	// registerRelayFlock previously only called RegisterRelay on the in-memory
	// flock struct (which the member's own outbound relay hop to home reads from
	// f.RelayToken), never registering the tokens in cp.relayTokens/cp.callTokens
	// for INBOUND admission (wall slice potential defect fix).
	cp.setRelayToken(flockID, req.RelayToken)
	cp.setCallToken(flockID, req.CallToken)
	w.WriteHeader(http.StatusCreated)
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
	vcpu, mem := agentProfile.VcpuCount, agentProfile.MemSizeMib
	// UI-created profiles persist their own sizing in goose.yaml (EPHEMERA_* keys);
	// prefer those over the LookupProfile defaults when present. Mirrors the
	// POST /vms path (spawnVM in api.go) so both spawn entry points size flock
	// members identically. Note: add-agent to an existing flock routes through
	// this same function, so a member joined under a sized profile is sized here
	// too — intentionally, only the new member takes the profile's sizing.
	if pc, err := cp.readProfileConfig(profile); err == nil {
		if pc.VcpuCount > 0 {
			vcpu = pc.VcpuCount
		}
		if pc.MemSizeMib > 0 {
			mem = pc.MemSizeMib
		}
	}
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
		VcpuCount:         vcpu,
		MemSizeMib:        mem,
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
