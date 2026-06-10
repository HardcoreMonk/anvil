package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	ops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"

	"ephemera/internal/mcpgateway"
	"ephemera/internal/metrics"
	"ephemera/internal/network"
	"ephemera/internal/orchestrator"
	"ephemera/internal/storage"
	"ephemera/internal/vm"
)

// authMiddleware enforces per-client Bearer token authentication on all requests.
// getClients is called on every request so token changes (via SIGHUP reload) take
// effect immediately without restarting the server or dropping running VMs.
// authTotal records each auth decision (outcome=ok|denied|expired); it may be nil
// in tests, in which case the metric is skipped.
//
// If getClients returns an empty slice, every request is allowed (auth disabled).
//
// Timing-safe design: subtle.ConstantTimeCompare always inspects every byte of both
// operands before returning, so response time does not vary with how many leading
// characters match. All registered tokens are compared on every request (no
// early-exit after the first match) to prevent leaking which client index was hit.
// The expiry check (v0.4.1) runs AFTER the full compare loop so it adds no timing
// signal, and an expired match returns the same 401 body as no match.
func authMiddleware(getClients func() []APIClient, authTotal *metrics.CounterVec, next http.Handler) http.Handler {
	countAuth := func(outcome string) {
		if authTotal != nil {
			authTotal.WithLabelValues(outcome).Inc()
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clients := getClients()
		if len(clients) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		auth := []byte(r.Header.Get("Authorization"))

		// Compare against every registered token without short-circuiting.
		matchedName := ""
		var matched APIClient
		for _, c := range clients {
			if subtle.ConstantTimeCompare(auth, []byte("Bearer "+c.Token)) == 1 {
				matchedName = c.Name
				matched = c
			}
		}

		unauthorized := func() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ephemera"`)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		}

		if matchedName == "" {
			countAuth("denied")
			unauthorized()
			return
		}
		if !matched.Expires.IsZero() && time.Now().After(matched.Expires) {
			// Same 401 body as a non-match (no client-facing distinction); only
			// the server-side log + metric record the expiry.
			countAuth("expired")
			slog.Warn("api request rejected: token expired", "client", matchedName, "method", r.Method, "path", r.URL.Path)
			unauthorized()
			return
		}
		countAuth("ok")
		// Surface the caller identity: the request-scoped holder lets the outer
		// audit middleware read it; the context value serves any future handler.
		if h := clientHolderFromContext(r.Context()); h != nil {
			h.name = matchedName
		}
		r = r.WithContext(withClientName(r.Context(), matchedName))
		slog.Info("api request", "client", matchedName, "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// VMInfo is stored per-VM and returned by GET /vms (no token).
type VMInfo struct {
	VMID     string `json:"vm_id"`
	GuestIP  string `json:"guest_ip"`
	AgentURL string `json:"agent_url"` // proxy URL via control plane when EPHEMERA_PUBLIC_URL is set; otherwise http://{private-ip}:8080
	Profile  string `json:"profile,omitempty"`
	// Provider/Model record the goose.yaml LLM config baked into this VM at spawn
	// time. They are a point-in-time snapshot: editing a profile later does NOT
	// change a running VM, so the UI shows what each VM is actually using.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// VMSpawnResult is returned only by POST /vms.
// AgentToken is the per-VM Bearer token for goose-agent; it is not stored on the server
// after this response — callers must persist it themselves.
type VMSpawnResult struct {
	VMInfo
	AgentToken string `json:"agent_token"`
}

// VMSpawnRequest is the optional JSON body for POST /vms.
type VMSpawnRequest struct {
	Profile string `json:"profile,omitempty"`
}

type runningVM struct {
	VMInfo
	agentToken      string                  // per-VM bearer token; only returned at spawn time, never re-serialized
	diskPath        string                  // actual disk file to delete on teardown (spawned) or exception store (COW-restored)
	bindMountTarget string                  // non-empty for bind-mount restored VMs (legacy path)
	dmSnapshot      *storage.DMSnapshotInfo // non-nil for COW-restored VMs; replaces bindMountTarget
	vsockPath       string                  // host-side UDS for Firecracker vsock proxy; cleaned up on teardown
	machine         *firecracker.Machine
	tapDevice       string
	socketPath      string
	// sourceSnapshotID is non-empty for snapshot-restored VMs (v0.4.5). It lets
	// DestroyAll distinguish them from COW-spawn VMs (re-restore from the source
	// snapshot on recovery rather than re-layering the preserved exception store).
	sourceSnapshotID string
	// v0.3.5 additions for /vms/{vm_id}/stats. memSizeMib mirrors VMState.MemSizeMib,
	// spawnedAt mirrors VMState.CreatedAt, and fcPID caches the Firecracker child
	// PID resolved via /proc/net/unix on first stats request. atomic stores so
	// concurrent stats requests across goroutines remain race-free. vcpuCount mirrors
	// the VM's vCPU count; it is not used by stats but is recorded into snapshot
	// metadata (alongside memSizeMib) so a restore can report the VM's true sizing.
	vcpuCount  int64
	memSizeMib int64
	spawnedAt  time.Time
	fcPID      int32
}

// rootfsPath returns the host path of the VM's live rootfs — the device/file the
// guest actually reads and writes, which is what a snapshot must copy. It must NOT
// be reconstructed from the VM ID: COW VMs (both spawned and snapshot-restored)
// expose their rootfs at the dm-snapshot device's bind-mount target, and a
// restored VM keeps the *source* VM's original disk path (baked into the snapshot's
// Firecracker state), so {workspace}/{vmID}.ext4 does not exist for it.
func (v *runningVM) rootfsPath() string {
	if v.dmSnapshot != nil {
		return v.dmSnapshot.MountTarget // COW spawn & COW restore
	}
	if v.bindMountTarget != "" {
		return v.bindMountTarget // legacy bind-mount restore
	}
	return v.diskPath // plain spawn: the full .ext4 clone
}

// generateAgentToken creates a 32-byte cryptographically random token, hex-encoded (64 chars).
func generateAgentToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate agent token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ControlPlane manages the MicroVM lifecycle and proxies agent requests.
// External clients interact entirely through the control plane URL:
//   - VM lifecycle: POST/GET/DELETE /vms, POST/GET/DELETE /snapshots
//   - Agent proxy:  POST /vms/{vm_id}/tasks, GET /vms/{vm_id}/health,
//     POST /vms/{vm_id}/stop  (forwarded to the VM's private goose-agent)
type ControlPlane struct {
	mu  sync.RWMutex
	vms map[string]*runningVM

	clientsMu sync.RWMutex
	clients   []APIClient

	snapshotsMu sync.RWMutex
	snapshots   map[string]storage.SnapshotMetadata

	// restoreMu serializes the bind-mount-setup + Firecracker-open window so that each
	// Firecracker instance opens the topmost (correct) bind mount before the next restore
	// can stack another one on top. Released as soon as RestoreMachine returns.
	restoreMu sync.Mutex

	provisioner      *storage.Provisioner
	netManager       *network.Manager
	kernelPath       string
	firecrackerPath  string
	gooseConfigPath  string
	gooseSecretsPath string
	workDir          string
	snapshotDir      string

	// flockMgr tracks multi-agent groupings ("flocks") and their Town Wall logs.
	// Populated lazily by the orchestrator API; standalone VM lifecycle is unaffected.
	flockMgr *orchestrator.FlockManager

	// watchdog polls each flock-member VM's /health endpoint and marks
	// non-responsive agents dead. Started in cp.Start, stopped in cp.Shutdown
	// BEFORE the HTTP server so it cannot observe a half-torn-down vms map.
	watchdog *orchestrator.Watchdog

	// agentHTTPClient is used for proxying requests to VM goose-agents.
	// No global timeout — timeouts are controlled by the incoming request's context.
	agentHTTPClient *http.Client

	// metrics holds the Prometheus registry plus typed collectors used across
	// the control plane (v0.3.5). Wired in NewControlPlane after vms/snapshots/
	// flockMgr are constructed because GaugeFunc closures observe those fields.
	metrics *daemonMetrics

	// audit is the access-log writer (v0.4.1): a rotated jsonl file under
	// {workDir}/audit/. On by default; nil/disabled is a no-op.
	audit *auditLogger

	// useCOW selects the spawn disk strategy (COW dm-snapshot view vs full byte
	// copy), resolved once at startup (default COW, plain fallback). See resolveDiskModeCOW.
	useCOW bool

	stopCh chan struct{}
	srv    *http.Server

	// MCP gateway (v0.6.0): a host-resident MCP server bound to the bridge IP that
	// aggregates backend MCP servers behind one policy-filtered, namespaced catalog
	// for the in-VM goose clients. All nil/empty when EPHEMERA_MCP_ENABLED is unset,
	// in which case VMs get no MCP extension and behavior is unchanged.
	mcpGateway  *mcpgateway.Gateway
	mcpRegistry *mcpgateway.Registry
	mcpSrv      *http.Server
	mcpEndpoint string // gateway URL injected into VMs, e.g. http://10.0.1.1:3001/mcp
}

func NewControlPlane(
	provisioner *storage.Provisioner,
	netManager *network.Manager,
	kernelPath, firecrackerPath, gooseConfigPath, gooseSecretsPath, workDir, snapshotDir string,
) *ControlPlane {
	cp := &ControlPlane{
		vms:              make(map[string]*runningVM),
		clients:          apiClients,
		snapshots:        make(map[string]storage.SnapshotMetadata),
		provisioner:      provisioner,
		netManager:       netManager,
		kernelPath:       kernelPath,
		firecrackerPath:  firecrackerPath,
		gooseConfigPath:  gooseConfigPath,
		gooseSecretsPath: gooseSecretsPath,
		workDir:          workDir,
		snapshotDir:      snapshotDir,
		flockMgr:         orchestrator.NewFlockManager(workDir),
		agentHTTPClient:  &http.Client{},
		stopCh:           make(chan struct{}, 1),
		useCOW:           resolveDiskModeCOW(storage.DMSnapshotAvailable),
	}
	slog.Info("spawn disk mode resolved", "cow", cp.useCOW)

	// Audit access log (v0.4.1): on by default, rotated jsonl under {workDir}/audit/.
	audit, auditErr := newAuditLogger(workDir)
	if auditErr != nil {
		slog.Warn("audit log init failed; continuing without it", "err", auditErr)
	}
	cp.audit = audit

	// Load any snapshots persisted from previous daemon runs.
	if existing, err := storage.ListSnapshots(workDir); err == nil {
		for _, meta := range existing {
			cp.snapshots[meta.SnapshotID] = meta
		}
		if len(existing) > 0 {
			slog.Warn("loaded existing snapshots", "count", len(existing), "dir", snapshotDir)
		}
	}

	// Recover flocks persisted from previous daemon runs. Town Wall + agent
	// metadata are restored here; the actual VM cold-restart happens in
	// RecoverVMs below, which also flips per-agent status to ready on success.
	if recovered, failed, err := cp.flockMgr.LoadFromDisk(); err != nil {
		slog.Warn("scan flock metadata failed", "err", err)
	} else {
		if recovered > 0 {
			slog.Warn("recovered flocks", "count", recovered, "dir", filepath.Join(workDir, "flocks"))
		}
		if len(failed) > 0 {
			slog.Warn("flocks not fully restored", "count", len(failed), "failed", failed)
		}
	}

	// Register all Prometheus collectors BEFORE RecoverVMs: warm-restore (v0.4.0)
	// records ephemera_auto_restore_total during recovery, so cp.metrics must be
	// live by then or that path nil-derefs. GaugeFunc closures read
	// cp.vms/snapshots/flockMgr (already allocated above) only at scrape time, so
	// registering here is safe. Do not move this back below RecoverVMs.
	cp.metrics = newDaemonMetrics(cp)

	// Cold-restart any VMs that were running when the previous daemon stopped.
	// Booted from the same rootfs clone with the same network identity (TAP/IP/
	// MAC) and agent token; memory is preserved only when EPHEMERA_AUTOSNAPSHOT
	// warm-restore succeeds (else a fresh boot). External callers and flock
	// associations stay stable across the restart either way.
	if recovered, failed, err := cp.RecoverVMs(); err != nil {
		slog.Warn("scan vm state for recovery failed", "err", err)
	} else {
		if recovered > 0 {
			slog.Warn("recovered vms via cold-restart", "count", recovered)
		}
		if len(failed) > 0 {
			slog.Warn("vms not cold-restarted", "count", len(failed), "failed", failed)
		}
	}

	cp.watchdog = orchestrator.NewWatchdog(cp.flockMgr, cp.locateFlockAgent, cp.listVMRefs, agentPort)
	cp.watchdog.Configure(
		time.Duration(watchdogIntervalSec)*time.Second,
		time.Duration(watchdogTimeoutSec)*time.Second,
		watchdogThreshold,
		watchdogAutoHeal,
	)
	// Wire watchdog metrics callbacks so orchestrator/ does not import metrics/.
	cp.watchdog.OnDead = func(string, string, string) { cp.metrics.watchdogDead.Inc() }
	cp.watchdog.OnHeal = func(string, string, string) { cp.metrics.watchdogHeal.Inc() }
	cp.watchdog.OnProbeDuration = func(d time.Duration) {
		cp.metrics.watchdogProbeDuration.Observe(d.Seconds())
	}

	// Two-mux pattern: /metrics is exempt from authMiddleware by default
	// (standard Prometheus scrape model). When EPHEMERA_METRICS_REQUIRE_AUTH=true
	// the /metrics handler is wrapped in authMiddleware just like everything else.
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/vms", cp.handleVMs)
	internalMux.HandleFunc("/vms/", cp.handleVM)
	internalMux.HandleFunc("/snapshots", cp.handleSnapshots)
	internalMux.HandleFunc("/snapshots/", cp.handleSnapshotItem)
	internalMux.HandleFunc("/audit", cp.handleAudit)
	internalMux.HandleFunc("/watchdog/status", cp.handleWatchdogStatus)
	internalMux.HandleFunc("/config/providers", cp.handleConfigProviders)
	internalMux.HandleFunc("/config/presets", cp.handleConfigPresets)
	internalMux.HandleFunc("/config/builtins", cp.handleConfigBuiltins)
	internalMux.HandleFunc("/config/mcp", cp.handleConfigMCP)
	internalMux.HandleFunc("/config/mcp/servers", cp.handleConfigMCPServers)
	internalMux.HandleFunc("/config/clients", cp.handleConfigClients)
	internalMux.HandleFunc("/config/monitoring", cp.handleConfigMonitoring)
	internalMux.HandleFunc("/config/profiles", cp.handleConfigProfiles)
	internalMux.HandleFunc("/config/profiles/", cp.handleConfigProfile)
	cp.registerOrchestratorRoutes(internalMux)

	externalMux := http.NewServeMux()
	if metricsRequireAuth {
		externalMux.Handle("/metrics", authMiddleware(cp.getClients, cp.metrics.authTotal, http.HandlerFunc(cp.handleMetrics)))
	} else {
		externalMux.HandleFunc("/metrics", cp.handleMetrics)
	}
	// The embedded Web UI is served under /ui/ OUTSIDE the auth/audit chain: the
	// login page + JS bundle must load before the user has a token, and the
	// bundle carries no secrets. Longest-prefix matching means /ui/ wins over the
	// "/" catch-all, so the API routing below is unaffected. The SPA's own API
	// calls still flow through authMiddleware via "/".
	externalMux.Handle("/ui/", cp.uiHandler())
	// audit (outer) wraps auth (inner): the access log captures 401s + final
	// status, and reads the client name auth back-fills via the request holder.
	// rootRedirectOr sends the bare "/" to /ui/ while delegating every other path
	// to the unchanged API chain.
	apiChain := cp.auditMiddleware(authMiddleware(cp.getClients, cp.metrics.authTotal, internalMux))
	externalMux.Handle("/", rootRedirectOr(apiChain))

	cp.srv = &http.Server{Addr: apiAddr, Handler: externalMux}
	return cp
}

// getClients returns the current authorized client list under a read lock.
func (cp *ControlPlane) getClients() []APIClient {
	cp.clientsMu.RLock()
	defer cp.clientsMu.RUnlock()
	return cp.clients
}

// controlPlaneTokenForVM returns the bearer the in-VM /townwall/post forwarder
// uses when calling back into the control plane. Returns the first NON-EXPIRED
// API client's token when auth is enabled, or "" when auth is disabled or every
// token has expired — in the latter case the in-VM forwarder calls CP
// unauthenticated (v0.4.1: was blindly clients[0]).
//
// Read under clientsMu so SIGHUP-driven ReloadClients is safe.
func (cp *ControlPlane) controlPlaneTokenForVM() string {
	cp.clientsMu.RLock()
	defer cp.clientsMu.RUnlock()
	if c, ok := firstActiveClient(cp.clients); ok {
		return c.Token
	}
	return ""
}

// ReloadClients re-reads API tokens from the environment (or EPHEMERA_API_TOKENS_FILE)
// and hot-swaps the client list. Called on SIGHUP. Also propagates the first
// non-expired client's token to every running flock VM via vsock so the in-VM
// /townwall/post forwarder keeps authenticating after rotation.
func (cp *ControlPlane) ReloadClients() {
	newClients := loadAPIClients()
	cp.clientsMu.Lock()
	cp.clients = newClients
	cp.clientsMu.Unlock()

	if len(newClients) == 0 {
		slog.Warn("sighup: token reload complete (auth disabled)")
	} else {
		names := make([]string, len(newClients))
		for i, c := range newClients {
			names[i] = c.Name
		}
		expired, expiring := countTokenExpiry(newClients)
		slog.Warn("sighup: token reload complete", "client_count", len(newClients), "clients", strings.Join(names, ", "), "expired", expired, "expiring_24h", expiring)
	}

	cp.propagateCPTokenToVMs(newClients)
	cp.metrics.sighupReload.Inc()
}

// propagateCPTokenToVMs fans out the first non-expired client's token to every
// running VM that has a vsock UDS path. Best-effort: per-VM failure is logged, not
// propagated. Older (pre-v0.3.4) guests lack the SET_CP_TOKEN handler and will fail
// here; operators can fall back to POST /flocks/{id}/agents/{agent_id}/restart.
// When auth is enabled but every token has expired, an empty token is propagated
// (the in-VM forwarder then calls CP unauthenticated) and a warning is logged.
func (cp *ControlPlane) propagateCPTokenToVMs(clients []APIClient) {
	newToken := ""
	if c, ok := firstActiveClient(clients); ok {
		newToken = c.Token
	} else if len(clients) > 0 {
		slog.Warn("cp token propagation: all api clients expired; propagating empty token (in-VM forwarder will call unauthenticated)")
	}

	cp.mu.RLock()
	type target struct{ vmID, vsock string }
	targets := make([]target, 0, len(cp.vms))
	for id, v := range cp.vms {
		if v.vsockPath != "" {
			targets = append(targets, target{id, v.vsockPath})
		}
	}
	cp.mu.RUnlock()

	if len(targets) == 0 {
		return
	}

	var wg sync.WaitGroup
	var okCount int32
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			if err := vm.SetGuestCPToken(t.vsock, newToken); err != nil {
				slog.Warn("sighup: cp token propagation failed", "vm_id", t.vmID, "err", err)
				cp.metrics.cpTokenPropagated.WithLabelValues("fail").Inc()
				return
			}
			atomic.AddInt32(&okCount, 1)
			cp.metrics.cpTokenPropagated.WithLabelValues("ok").Inc()
		}(t)
	}
	wg.Wait()
	slog.Warn("sighup: cp token propagated", "ok", atomic.LoadInt32(&okCount), "total", len(targets))
}

func (cp *ControlPlane) Start() error {
	clients := cp.getClients()
	auth := "disabled"
	if len(clients) > 0 {
		names := make([]string, len(clients))
		for i, c := range clients {
			names[i] = c.Name
		}
		expired, expiring := countTokenExpiry(clients)
		auth = fmt.Sprintf("Bearer token (%d client(s): %s; expired=%d expiring_24h=%d)", len(clients), strings.Join(names, ", "), expired, expiring)
	}
	slog.Warn("control plane api ready", "addr", apiAddr, "auth", auth)
	// Endpoints banner — emitted as a single block so JSON-mode log consumers
	// don't get 19 records of UI noise during startup.
	endpoints := "endpoints:\n" +
		"  POST   /vms                              — spawn VM\n" +
		"  GET    /vms                              — list VMs (?stats=true for inline per-VM stats)\n" +
		"  DELETE /vms/{vm_id}                      — stop VM\n" +
		"  GET    /vms/{vm_id}/stats                — per-VM cpu/mem/net/uptime snapshot\n" +
		"  POST   /vms/{vm_id}/snapshot             — create snapshot\n" +
		"  POST   /vms/{vm_id}/tasks                — proxy: run task on agent\n" +
		"  GET    /vms/{vm_id}/health               — proxy: agent health check\n" +
		"  POST   /vms/{vm_id}/stop                 — proxy: stop agent\n" +
		"  GET    /vms/{vm_id}/sessions             — proxy: list agent chat sessions\n" +
		"  GET    /snapshots                        — list snapshots\n" +
		"  POST   /snapshots/{snapshot_id}/restore  — restore VM from snapshot\n" +
		"  DELETE /snapshots/{snapshot_id}          — delete snapshot\n" +
		"  POST   /flocks                           — create multi-agent flock\n" +
		"  GET    /flocks                           — list flocks\n" +
		"  GET    /flocks/{flock_id}                — describe flock\n" +
		"  DELETE /flocks/{flock_id}                — destroy flock\n" +
		"  GET    /flocks/{flock_id}/wall           — SSE stream of Town Wall\n" +
		"  GET    /flocks/{flock_id}/wall/history   — full Town Wall log\n" +
		"  POST   /flocks/{flock_id}/post           — post message to Town Wall\n" +
		"  POST   /flocks/{flock_id}/agents/{id}/restart — restart one agent in place\n" +
		"  GET    /audit                            — recent API access log (jsonl, rotated)\n" +
		"  GET    /metrics                          — Prometheus exposition (auth optional)"
	slog.Warn(endpoints)
	if publicURL != "" {
		slog.Warn("public agent_url base configured", "public_url", publicURL)
	}
	cp.watchdog.Start()
	slog.Warn("watchdog started",
		"interval_sec", watchdogIntervalSec,
		"timeout_sec", watchdogTimeoutSec,
		"threshold", watchdogThreshold,
		"auto_heal", watchdogAutoHeal)
	// MCP gateway (v0.6.0): build and start its bridge-bound listener (no-op when
	// EPHEMERA_MCP_ENABLED is unset). Independent of the control-plane API server.
	cp.initMCPGateway()
	cp.startMCPGateway()
	return cp.srv.ListenAndServe()
}

func (cp *ControlPlane) Shutdown() {
	// Stop the watchdog BEFORE the HTTP server. The watchdog reads cp.vms
	// (via listVMRefs) on every tick; tearing down the server first leaves
	// in-flight ticks racing against any cp.vms cleanup that follows.
	cp.watchdog.Stop()
	cp.stopMCPGateway()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cp.srv.Shutdown(ctx)
	if cp.audit != nil {
		cp.audit.Close()
	}
}

// locateFlockAgent maps a vmID back to its (flockID, agentID) by scanning
// every flock. Used by the watchdog so it can update agent status and Town
// Wall by identity rather than by VM. Returns ok=false for standalone VMs.
func (cp *ControlPlane) locateFlockAgent(vmID string) (string, string, bool) {
	for _, f := range cp.flockMgr.List() {
		for _, a := range f.Snapshot() {
			if a.VMID == vmID {
				return f.ID, a.AgentID, true
			}
		}
	}
	return "", "", false
}

// listVMRefs snapshots the currently-registered VMs in a form the watchdog
// can probe (vm_id + guest_ip only). Read-locks cp.mu to keep the snapshot
// consistent with concurrent destroyVM calls.
func (cp *ControlPlane) listVMRefs() []orchestrator.VMRef {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	out := make([]orchestrator.VMRef, 0, len(cp.vms))
	for _, v := range cp.vms {
		out = append(out, orchestrator.VMRef{VMID: v.VMID, GuestIP: v.GuestIP})
	}
	return out
}

func (cp *ControlPlane) StopCh() <-chan struct{} { return cp.stopCh }

// POST /vms → spawn VM, return VMInfo with private IP
// GET  /vms → list running VMs
func (cp *ControlPlane) handleVMs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		cp.spawnVM(w, r)
	case http.MethodGet:
		cp.listVMs(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVM routes /vms/{vm_id} and its sub-paths.
func (cp *ControlPlane) handleVM(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/vms/")

	if strings.HasSuffix(path, "/stats") {
		vmID := strings.TrimSuffix(path, "/stats")
		if vmID == "" {
			http.Error(w, `{"error":"vm_id required"}`, http.StatusBadRequest)
			return
		}
		cp.handleVMStats(w, r, vmID)
		return
	}

	if strings.HasSuffix(path, "/snapshot") {
		vmID := strings.TrimSuffix(path, "/snapshot")
		if vmID == "" {
			http.Error(w, `{"error":"vm_id required"}`, http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.createSnapshot(w, r, vmID)
		return
	}

	// Agent proxy sub-paths: forward to the VM's private goose-agent.
	// The control plane injects the per-VM agent token; callers use their
	// control plane Bearer token only.
	if strings.HasSuffix(path, "/tasks") {
		vmID := strings.TrimSuffix(path, "/tasks")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.proxyAgentEndpoint(w, r, vmID, "/tasks")
		return
	}

	if strings.HasSuffix(path, "/health") {
		vmID := strings.TrimSuffix(path, "/health")
		cp.proxyAgentEndpoint(w, r, vmID, "/health")
		return
	}

	if strings.HasSuffix(path, "/stop") {
		vmID := strings.TrimSuffix(path, "/stop")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.proxyAgentEndpoint(w, r, vmID, "/stop")
		return
	}

	if strings.HasSuffix(path, "/sessions") {
		vmID := strings.TrimSuffix(path, "/sessions")
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"GET required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.proxyAgentEndpoint(w, r, vmID, "/sessions")
		return
	}

	// DELETE /vms/{vm_id}
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	if path == "" {
		http.Error(w, "vm_id required", http.StatusBadRequest)
		return
	}
	cp.stopVM(w, path)
}

// proxyAgentEndpoint forwards an HTTP request to the VM's private goose-agent
// and streams the response back to the caller. The control plane injects the
// per-VM agent token so external callers only need the control plane Bearer token.
// /health is forwarded without an Authorization header (it is unauthenticated on the agent).
func (cp *ControlPlane) proxyAgentEndpoint(w http.ResponseWriter, r *http.Request, vmID, agentPath string) {
	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"vm not found"}`, http.StatusNotFound)
		return
	}

	targetURL := fmt.Sprintf("http://%s:%d%s", v.GuestIP, agentPort, agentPath)
	// Forward the query string so agent-side switches reach goose-agent
	// (e.g. /tasks?stream=1 selects the NDJSON streaming path, v0.4.4).
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"proxy request: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if ct := r.Header.Get("Content-Type"); ct != "" {
		proxyReq.Header.Set("Content-Type", ct)
	}
	// /health is always unauthenticated on the agent side.
	if agentPath != "/health" && v.agentToken != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+v.agentToken)
	}

	// Nested-invocation depth guard (v0.4.4). Only /tasks is a task hop; the
	// incoming header is the current depth (absent → 0). At/over the cap we
	// refuse with 508 Loop Detected (distinct from the agent's own 503-busy);
	// otherwise forward depth+1 so the next hop accumulates.
	if agentPath == "/tasks" {
		depth := 0
		if h := r.Header.Get("X-Ephemera-Task-Depth"); h != "" {
			if n, err := strconv.Atoi(h); err == nil && n > 0 {
				depth = n
			}
		}
		if depth >= maxTaskDepth {
			slog.Warn("task depth exceeded", "vm_id", vmID, "depth", depth, "max", maxTaskDepth)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, fmt.Sprintf(`{"error":"max task depth %d exceeded"}`, maxTaskDepth), http.StatusLoopDetected)
			return
		}
		proxyReq.Header.Set("X-Ephemera-Task-Depth", strconv.Itoa(depth+1))
	}

	resp, err := cp.agentHTTPClient.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"agent unreachable: %v"}`, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		for _, val := range vals {
			w.Header().Add(k, val)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// Flush per chunk so a streaming agent response (NDJSON /tasks?stream=1,
	// v0.4.4) reaches the caller incrementally rather than sitting in net/http's
	// write buffer. statusRecorder forwards Flush to the real ResponseWriter
	// (the same plumbing the Town Wall SSE stream relies on). Harmless for
	// buffered/small responses.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// handleWatchdogStatus serves GET /watchdog/status (v0.4.4): a JSON snapshot of
// the health watchdog's tunables and current per-VM failure/dead state.
// Registered on internalMux so it is always behind authMiddleware.
func (cp *ControlPlane) handleWatchdogStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"GET required"}`, http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, cp.watchdog.Status())
}

// buildAgentURL returns the agent_url field for VMInfo.
// When EPHEMERA_PUBLIC_URL is configured, returns the control plane proxy path
// so external clients can reach the agent through the control plane.
// Otherwise falls back to the VM's private IP (backward-compatible).
func buildAgentURL(vmID, guestIP string) string {
	if publicURL != "" {
		return publicURL + "/vms/" + vmID
	}
	return fmt.Sprintf("http://%s:%d", guestIP, agentPort)
}

// profileConfigPaths resolves the goose.yaml and goose-secrets.yaml host paths
// for a given profile. Secrets ALWAYS come from the single global keychain
// (configs/goose-secrets.yaml) — profiles carry provider/model only, never their
// own keys. The config path is the profile's own goose.yaml when that profile
// directory exists, and falls back to the daemon's default goose.yaml otherwise,
// so flock roles and ad-hoc profile names that have no on-disk directory still
// spawn (using the default provider/model). Only an unsafe (path-traversal)
// profile name is an error.
func (cp *ControlPlane) profileConfigPaths(profile string) (configPath, secretsPath string, err error) {
	// API keys live in one place, regardless of profile.
	secretsPath = cp.gooseSecretsPath
	if profile == "" {
		return cp.gooseConfigPath, secretsPath, nil
	}
	// Reject path traversal attempts before any filesystem access.
	if strings.ContainsAny(profile, "/\\") || profile == ".." {
		return "", "", fmt.Errorf("invalid profile name: %q", profile)
	}
	// LookupProfile maps a profile name to its on-disk directory (unknown names
	// map to the name itself).
	p := LookupProfile(profile)
	if p.ProfileDir == "" {
		return cp.gooseConfigPath, secretsPath, nil
	}
	candidate := filepath.Join(cp.workDir, "configs", "profiles", p.ProfileDir, "goose.yaml")
	if _, e := os.Stat(candidate); e == nil {
		return candidate, secretsPath, nil
	}
	// Profile directory absent → use the default config (global secrets already set).
	return cp.gooseConfigPath, secretsPath, nil
}

// spawnVMOptions captures all caller-supplied inputs for spawnVMInternal.
// Callers must pre-resolve the goose config paths so this helper does not need
// to know about profileConfigPaths' specific error semantics.
type spawnVMOptions struct {
	Profile      string // recorded in VMInfo.Profile; used only for logs
	ConfigPath   string // resolved goose.yaml host path
	SecretsPath  string // resolved goose-secrets.yaml host path
	SystemPrompt string // optional role system prompt injected into the VM
	FlockID      string // optional: when set, agent is part of a flock
	AgentID      string // optional: per-flock agent ID (e.g. "researcher-1")
	// AgentToken, when set, is reused as the in-VM bearer instead of being
	// freshly generated. Used by per-agent restart so callers that already
	// cached a token keep working across the restart.
	AgentToken string
	// ControlPlaneToken, when set, is injected into the VM at /root/.ephemera-cp-token
	// so the in-VM /townwall/post forwarder can authenticate when calling
	// back into the control plane. Auto-populated by spawnVMForFlock from
	// the daemon's apiClients[0]; standalone spawnVM leaves this empty.
	ControlPlaneToken string
	VcpuCount         int64 // 0 → default 1
	MemSizeMib        int64 // 0 → default 1024
}

// spawnVMInternal performs the actual VM lifecycle: allocate networking, clone
// disk, inject config, start Firecracker, register, and wait for goose-agent.
// On any error it cleans up every resource it allocated and returns.
// Used by both the public POST /vms handler and the orchestrator's flock spawner.
func (cp *ControlPlane) spawnVMInternal(opts spawnVMOptions) (*VMInfo, string, error) {
	start := time.Now()
	outcome := "fail"
	defer func() {
		cp.metrics.vmSpawnTotal.WithLabelValues(outcome).Inc()
		cp.metrics.vmSpawnDuration.Observe(time.Since(start).Seconds())
	}()
	agentToken := opts.AgentToken
	if agentToken == "" {
		t, err := generateAgentToken()
		if err != nil {
			return nil, "", fmt.Errorf("token generation: %w", err)
		}
		agentToken = t
	}
	vmID := fmt.Sprintf("vm-%d", time.Now().UnixNano())

	// Disk-space pre-flight (v0.4.0): refuse before allocating any resources if
	// the clone would push free space below the operator margin. A full clone
	// copies the entire golden image; COW's exception store is a sparse 8 GiB
	// file (~0 bytes up front), so only the margin must be free.
	{
		var needBytes uint64
		if !cp.useCOW {
			if fi, statErr := os.Stat(cp.provisioner.GoldenImagePath); statErr == nil {
				needBytes = uint64(fi.Size())
			}
		}
		if err := storage.EnsureFreeSpace(cp.provisioner.WorkspaceDir, needBytes, uint64(diskMinFreeMiB)<<20); err != nil {
			return nil, "", err
		}
	}

	// Atomic spawn rollback: each successfully-allocated resource pushes its
	// cleanup onto rollback; the deferred closure unwinds them LIFO unless the
	// VM is committed (registered in cp.vms, after which destroyVM owns cleanup).
	// This makes a mid-spawn failure leak-free and stops a future early-return
	// added between allocation and commit from silently leaking a resource.
	committed := false
	var rollback []func()
	defer func() {
		if committed {
			return
		}
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}()

	tapDevice, guestIP, macAddr, err := cp.netManager.Allocate()
	if err != nil {
		return nil, "", fmt.Errorf("network allocation: %w", err)
	}
	rollback = append(rollback, func() { cp.netManager.Release(tapDevice, guestIP) })

	// Disk provisioning: full clone (default) vs dm-snapshot COW (env-gated).
	// COW mode trades per-VM ~700 MiB of writes for a sparse exception store
	// that grows only as the VM writes. Mode is process-wide so all flock
	// members share the same provisioning strategy in a given daemon run.
	var diskPath string
	var dmInfo *storage.DMSnapshotInfo
	if cp.useCOW {
		var cowStore string
		diskPath, cowStore, dmInfo, err = cp.provisioner.CloneDiskCOW(vmID)
		_ = cowStore // tracked via dmInfo.ExceptionStore for cleanup
		if err != nil {
			return nil, "", fmt.Errorf("disk provisioning (cow): %w", err)
		}
		rollback = append(rollback, func() { storage.TeardownDMSnapshot(dmInfo) })
	} else {
		diskPath, err = cp.provisioner.CloneDisk(vmID)
		if err != nil {
			return nil, "", fmt.Errorf("disk provisioning: %w", err)
		}
		rollback = append(rollback, func() { cp.provisioner.CleanupDisk(vmID) })
	}

	// Resolve the MCP gateway URL for this profile; pair it with the /etc/hosts
	// entry only when the URL is set (gateway on and profile permitted a backend).
	mcpURL := cp.mcpURLForProfile(opts.Profile)
	mcpHostsLine := ""
	if mcpURL != "" {
		mcpHostsLine = mcpHostsEntry()
	}
	if err := cp.provisioner.PrepareVM(vmID, storage.VMPrepareOptions{
		HostConfigPath:    opts.ConfigPath,
		HostSecretsPath:   opts.SecretsPath,
		AgentToken:        agentToken,
		FlockID:           opts.FlockID,
		AgentID:           opts.AgentID,
		SystemPrompt:      opts.SystemPrompt,
		ControlPlaneToken: opts.ControlPlaneToken,
		// MCP gateway (v0.6.0): inject the gateway URL only when it is enabled AND
		// this VM's profile is permitted at least one backend; otherwise the agent
		// runs without the extension. The /etc/hosts entry (paired with the URL) maps
		// the letter-starting gateway hostname to the bridge IP. Empty injects nothing.
		MCPGatewayURL:        mcpURL,
		MCPGatewayHostsEntry: mcpHostsLine,
	}); err != nil {
		return nil, "", fmt.Errorf("VM preparation: %w", err)
	}

	socketPath := fmt.Sprintf("/tmp/firecracker-%s.sock", vmID)
	vsockPath := fmt.Sprintf("/tmp/firecracker-vsock-%s.sock", vmID)
	os.Remove(socketPath)

	machine, err := vm.StartMachine(context.Background(), vm.VMConfig{
		VMID:           vmID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		KernelPath:     cp.kernelPath,
		RootfsPath:     diskPath,
		TapDevice:      tapDevice,
		MacAddress:     macAddr,
		GuestIP:        guestIP,
		GatewayIP:      "10.0.1.1",
		VsockUDSPath:   vsockPath,
		VcpuCount:      opts.VcpuCount,
		MemSizeMib:     opts.MemSizeMib,
	})
	if err != nil {
		return nil, "", fmt.Errorf("VM start: %w", err)
	}

	provider, model := readGooseConfigFile(opts.ConfigPath)
	info := VMInfo{
		VMID:     vmID,
		GuestIP:  guestIP,
		AgentURL: buildAgentURL(vmID, guestIP),
		Profile:  opts.Profile,
		Provider: provider,
		Model:    model,
	}

	// runningVM.dmSnapshot drives the COW teardown branch in destroyVM; when
	// nil, destroyVM falls back to deleting diskPath as a plain file.
	vcpu := opts.VcpuCount
	if vcpu == 0 {
		vcpu = 1 // matches vm.defaultVcpuCount
	}
	memSize := opts.MemSizeMib
	if memSize == 0 {
		memSize = 1024 // matches vm.defaultMemSizeMib
	}
	spawnedAt := time.Now().UTC()
	cp.mu.Lock()
	cp.vms[vmID] = &runningVM{
		VMInfo:     info,
		agentToken: agentToken,
		diskPath:   diskPath,
		dmSnapshot: dmInfo,
		vsockPath:  vsockPath,
		machine:    machine,
		tapDevice:  tapDevice,
		socketPath: socketPath,
		vcpuCount:  vcpu,
		memSizeMib: memSize,
		spawnedAt:  spawnedAt,
	}
	cp.mu.Unlock()
	// The VM is now registered in cp.vms and owned by the daemon; from here
	// cleanup is destroyVM's job (e.g. the waitForAgent failure path below), so
	// disarm the spawn-rollback stack to avoid a double free.
	committed = true

	// Persist VM state for cold-restart recovery after daemon restart. Both plain
	// and COW VMs are recovered: DiskMode tells RecoverVMs whether to boot the full
	// rootfs clone or re-layer the dm-snapshot exception store over the golden image.
	diskMode := storage.DiskModePlain
	if dmInfo != nil {
		diskMode = storage.DiskModeCOW
	}
	if err := storage.SaveVMState(cp.workDir, storage.VMState{
		VMID:       vmID,
		GuestIP:    guestIP,
		TapDevice:  tapDevice,
		MacAddr:    macAddr,
		VsockPath:  vsockPath,
		SocketPath: socketPath,
		AgentToken: agentToken,
		DiskPath:   diskPath,
		DiskMode:   diskMode,
		Profile:    opts.Profile,
		Provider:   info.Provider,
		Model:      info.Model,
		VcpuCount:  opts.VcpuCount,
		MemSizeMib: opts.MemSizeMib,
		FlockID:    opts.FlockID,
		AgentID:    opts.AgentID,
		AgentURL:   info.AgentURL,
		CreatedAt:  spawnedAt,
	}); err != nil {
		// State persistence failure must not abort the spawn — the VM is
		// already live. Log and continue; recovery just won't include it.
		slog.Warn("persist vm state failed", "vm_id", vmID, "err", err)
	}

	slog.Warn("vm booting, waiting for agent", "vm_id", vmID, "agent_url", info.AgentURL)
	if err := waitForAgent(guestIP, 60*time.Second); err != nil {
		cp.destroyVM(vmID)
		return nil, "", fmt.Errorf("goose-agent not ready: %w", err)
	}
	if opts.FlockID != "" {
		slog.Warn("vm ready", "vm_id", vmID, "agent_url", info.AgentURL, "flock_id", opts.FlockID, "agent_id", opts.AgentID)
	} else {
		slog.Warn("vm ready", "vm_id", vmID, "agent_url", info.AgentURL, "profile", opts.Profile)
	}
	outcome = "ok"
	return &info, agentToken, nil
}

// loadProfileSystemPrompt reads {workDir}/configs/profiles/{dir}/system.md.
// Returns an empty string when the file is missing or the profile has no dir.
func (cp *ControlPlane) loadProfileSystemPrompt(profileDir string) string {
	if profileDir == "" {
		return ""
	}
	path := filepath.Join(cp.workDir, "configs", "profiles", profileDir, "system.md")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func (cp *ControlPlane) spawnVM(w http.ResponseWriter, r *http.Request) {
	// Parse optional request body. An empty body is valid (uses default profile).
	var req VMSpawnRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"invalid request body: %v"}`, err), http.StatusBadRequest)
			return
		}
	}
	req.Profile = strings.TrimSpace(req.Profile)

	configPath, secretsPath, err := cp.profileConfigPaths(req.Profile)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadRequest)
		return
	}
	agentProfile := LookupProfile(req.Profile)
	vcpu, mem := agentProfile.VcpuCount, agentProfile.MemSizeMib
	// UI-created profiles persist their own sizing in goose.yaml (EPHEMERA_* keys);
	// prefer those over the LookupProfile defaults when present.
	if pc, err := cp.readProfileConfig(req.Profile); err == nil {
		if pc.VcpuCount > 0 {
			vcpu = pc.VcpuCount
		}
		if pc.MemSizeMib > 0 {
			mem = pc.MemSizeMib
		}
	}

	info, agentToken, err := cp.spawnVMInternal(spawnVMOptions{
		Profile:     req.Profile,
		ConfigPath:  configPath,
		SecretsPath: secretsPath,
		VcpuCount:   vcpu,
		MemSizeMib:  mem,
	})
	if err != nil {
		status := http.StatusInternalServerError
		var ise *storage.InsufficientStorageError
		if errors.As(err, &ise) {
			status = http.StatusInsufficientStorage
		}
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMSpawnResult{VMInfo: *info, AgentToken: agentToken})
}

func (cp *ControlPlane) listVMs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("stats") == "true" {
		cp.listVMsWithStats(w, r)
		return
	}
	cp.mu.RLock()
	list := make([]VMInfo, 0, len(cp.vms))
	for _, v := range cp.vms {
		list = append(list, v.VMInfo)
	}
	cp.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// listVMsWithStats serves GET /vms?stats=true. Each list element is
// VMInfoWithStats. Per-VM stats failures degrade to zero values (logged by
// collectVMStats); the response never partial-errors.
func (cp *ControlPlane) listVMsWithStats(w http.ResponseWriter, r *http.Request) {
	cp.mu.RLock()
	type ref struct {
		id string
		v  *runningVM
	}
	refs := make([]ref, 0, len(cp.vms))
	for id, v := range cp.vms {
		refs = append(refs, ref{id, v})
	}
	cp.mu.RUnlock()

	out := make([]VMInfoWithStats, 0, len(refs))
	for _, rf := range refs {
		out = append(out, VMInfoWithStats{
			VMInfo: rf.v.VMInfo,
			Stats:  cp.collectVMStats(r.Context(), rf.id, rf.v),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (cp *ControlPlane) stopVM(w http.ResponseWriter, vmID string) {
	cp.mu.RLock()
	_, ok := cp.vms[vmID]
	cp.mu.RUnlock()

	if !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	cp.destroyVM(vmID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped", "vm_id": vmID})
}

func (cp *ControlPlane) destroyVM(vmID string) {
	cp.mu.Lock()
	v, ok := cp.vms[vmID]
	if ok {
		delete(cp.vms, vmID)
	}
	cp.mu.Unlock()

	if !ok {
		return
	}
	// Drop the persisted state first so a crash between StopVMM and resource
	// release doesn't resurrect the VM on next boot with stale identity.
	if err := storage.DeleteVMState(cp.workDir, vmID); err != nil {
		slog.Warn("delete vm state failed", "vm_id", vmID, "err", err)
	}
	// Clear any auto-snapshot so an explicit destroy leaves no orphaned memory
	// image behind (DeleteVMState only removes an empty state dir).
	storage.RemoveAutoSnapshot(cp.workDir, vmID)
	// Ask the in-VM agent to shut down cleanly first. goose-agent is the guest's
	// main process, so /stop halts the guest gracefully; StopVMM below then reaps
	// the Firecracker VMM whether or not the guest finished halting. Best-effort.
	cp.gracefulAgentStop(vmID, v.GuestIP, v.agentToken)
	// StopVMM sends SIGTERM and waits for Firecracker to exit.
	v.machine.StopVMM()
	os.Remove(v.socketPath)
	os.Remove(fmt.Sprintf("/tmp/fc-%s-log.fifo", vmID))
	if v.vsockPath != "" {
		os.Remove(v.vsockPath)
	}

	if v.dmSnapshot != nil {
		// COW-restored VM: release dm-snapshot device, loop device, and exception store.
		storage.TeardownDMSnapshot(v.dmSnapshot)
	} else if v.bindMountTarget != "" {
		// Bind-mount restored VM (legacy): lazy-umount + remove per-restore disk copy.
		storage.TeardownBindMount(v.bindMountTarget, v.diskPath)
	} else if v.diskPath != "" {
		if err := os.Remove(v.diskPath); err != nil && !os.IsNotExist(err) {
			slog.Warn("delete disk failed", "vm_id", vmID, "disk_path", v.diskPath, "err", err)
		}
	}
	cp.netManager.Release(v.tapDevice, v.GuestIP)
	slog.Warn("vm destroyed", "vm_id", vmID)
	cp.metrics.vmDestroyTotal.WithLabelValues("ok").Inc()
}

// gracefulAgentStop best-effort asks a VM's goose-agent to shut down (POST /stop,
// bearer-authenticated) with a short deadline. The agent is the guest's init
// process, so this halts the guest cleanly; any error is ignored because the
// caller force-stops the Firecracker VMM regardless.
func (cp *ControlPlane) gracefulAgentStop(vmID, guestIP, agentToken string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://%s:%d/stop", guestIP, agentPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return
	}
	if agentToken != "" {
		req.Header.Set("Authorization", "Bearer "+agentToken)
	}
	resp, err := cp.agentHTTPClient.Do(req)
	if err != nil {
		slog.Debug("graceful agent stop failed; forcing teardown", "vm_id", vmID, "err", err)
		return
	}
	resp.Body.Close()
}

// DestroyAll is called on graceful daemon shutdown. It stops every running
// Firecracker process but preserves each recoverable VM's state.json + rootfs so
// the next daemon start can cold-restart it with the same identity. For plain VMs
// that means keeping the rootfs clone; for COW spawn VMs it means releasing the
// dm-snapshot kernel objects while keeping the sparse exception store.
// Explicit DELETE /vms/{id} (which routes through destroyVM) still does a full
// cleanup — only the daemon-lifecycle path takes the preserving branch.
//
// Snapshot-restored VMs (dm-snapshot or legacy bind-mount) persist no state.json,
// so they are torn down fully here: leaving their devices or per-restore disk
// copies behind would leak resources without any benefit.
func (cp *ControlPlane) DestroyAll() {
	cp.mu.RLock()
	snapshot := make([]*runningVM, 0, len(cp.vms))
	for _, v := range cp.vms {
		snapshot = append(snapshot, v)
	}
	cp.mu.RUnlock()
	for _, v := range snapshot {
		// Memory auto-snapshot (v0.4.0, opt-in). This MUST run before StopVMM — a
		// stopped VM cannot be snapshotted. Only recoverable VMs (those with a
		// state.json: plain or COW spawn, never restored VMs) are eligible — the
		// same predicate the COW keep-store branch below uses. A failure is
		// non-fatal: it is logged, the partial snapshot removed, and the VM falls
		// through to the normal stop + cold-restart preservation below, so the
		// next start cold-boots it. Auto-snapshot is strictly additive.
		if enableAutoSnapshot && storage.VMStateExists(cp.workDir, v.VMID) && v.sourceSnapshotID == "" {
			autoDir := storage.AutoSnapshotDir(cp.workDir, v.VMID)
			memPath, statPath := storage.AutoSnapshotPaths(cp.workDir, v.VMID)
			if mkErr := os.MkdirAll(autoDir, 0755); mkErr != nil {
				slog.Warn("auto-snapshot dir failed, will cold-boot on next start", "vm_id", v.VMID, "err", mkErr)
				cp.metrics.autoSnapshot.WithLabelValues("fail").Inc()
			} else if snapErr := cp.snapshotVMMemory(v, memPath, statPath); snapErr != nil {
				slog.Warn("auto-snapshot failed, will cold-boot on next start", "vm_id", v.VMID, "err", snapErr)
				cp.metrics.autoSnapshot.WithLabelValues("fail").Inc()
				storage.RemoveAutoSnapshot(cp.workDir, v.VMID)
			} else {
				slog.Warn("auto-snapshot written", "vm_id", v.VMID)
				cp.metrics.autoSnapshot.WithLabelValues("ok").Inc()
			}
		}
		v.machine.StopVMM()
		os.Remove(v.socketPath)
		os.Remove(fmt.Sprintf("/tmp/fc-%s-log.fifo", v.VMID))
		if v.vsockPath != "" {
			os.Remove(v.vsockPath)
		}
		if v.dmSnapshot != nil && v.sourceSnapshotID == "" && storage.VMStateExists(cp.workDir, v.VMID) {
			// COW spawn VM: release the dm-snapshot kernel objects (mount, dm device,
			// loops) but PRESERVE the exception store + state.json so RecoverVMs can
			// re-layer the store over the golden image and bring the VM back with the
			// same identity on the next start.
			storage.TeardownDMSnapshotKeepStore(v.dmSnapshot)
		} else if v.dmSnapshot != nil && v.sourceSnapshotID != "" {
			// Snapshot-restored VM (recoverable, v0.4.5): drop the dm device AND the
			// transient exception store, but KEEP state.json so RecoverVMs re-restores
			// it fresh from the source snapshot on the next start. (Re-loading the
			// snapshot's memory onto an evolved store would be inconsistent, so the
			// store is intentionally discarded and recreated.)
			storage.TeardownDMSnapshot(v.dmSnapshot)
		} else if v.dmSnapshot != nil {
			// dm-snapshot restored VM with no persisted state (legacy, or persist
			// failed → not recoverable): full teardown including the exception store.
			storage.TeardownDMSnapshot(v.dmSnapshot)
			storage.DeleteVMState(cp.workDir, v.VMID)
		} else if v.bindMountTarget != "" {
			// Bind-mount restored VM (legacy, not recoverable): drop the per-restore
			// disk copy and state.json.
			storage.TeardownBindMount(v.bindMountTarget, v.diskPath)
			storage.DeleteVMState(cp.workDir, v.VMID)
		}
		// Plain rootfs ext4 + state.json are intentionally preserved here;
		// RecoverVMs will pick them up on the next daemon start.
		cp.netManager.Release(v.tapDevice, v.GuestIP)
		slog.Warn("vm paused for cold-restart", "vm_id", v.VMID)
	}
}

func waitForAgent(guestIP string, timeout time.Duration) error {
	url := fmt.Sprintf("http://%s:%d/health", guestIP, agentPort)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("agent not ready after %v", timeout)
}

// ---- Snapshot types ----

// SnapshotInfo is the public representation of a snapshot (no sensitive fields).
type SnapshotInfo struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`              // "full" | "diff"
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"` // set for diff snapshots
	CreatedAt      time.Time `json:"created_at"`
}

// VMRestoreResult is returned by POST /snapshots/{id}/restore.
type VMRestoreResult struct {
	VMSpawnResult
	SourceSnapshotID string `json:"source_snapshot_id"`
}

// SnapshotRequest is the optional body for POST /vms/{id}/snapshot.
type SnapshotRequest struct {
	StopAfter bool   `json:"stop_after"`
	Type      string `json:"type,omitempty"` // "full" | "diff" | "" (auto-detect)
}

func snapshotInfoFrom(meta storage.SnapshotMetadata) SnapshotInfo {
	return SnapshotInfo{
		SnapshotID:     meta.SnapshotID,
		SourceVMID:     meta.SourceVMID,
		Profile:        meta.Profile,
		SnapshotType:   meta.SnapshotType,
		BaseSnapshotID: meta.BaseSnapshotID,
		CreatedAt:      meta.CreatedAt,
	}
}

// ---- Snapshot handlers ----

// GET /snapshots
func (cp *ControlPlane) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	cp.snapshotsMu.RLock()
	list := make([]SnapshotInfo, 0, len(cp.snapshots))
	for _, meta := range cp.snapshots {
		list = append(list, snapshotInfoFrom(meta))
	}
	cp.snapshotsMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// handleSnapshotItem routes POST /snapshots/{id}/restore and DELETE /snapshots/{id}.
func (cp *ControlPlane) handleSnapshotItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/snapshots/")

	if strings.HasSuffix(path, "/restore") {
		snapID := strings.TrimSuffix(path, "/restore")
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		cp.restoreSnapshot(w, snapID)
		return
	}

	// DELETE /snapshots/{id}
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	cp.deleteSnapshot(w, path)
}

// resolveSnapshotType determines whether to create a Full or Diff snapshot.
// "" (auto): Full if no prior Full snapshot of this VM exists; Diff otherwise.
// "full" / "diff": explicit override. "diff" without a base returns an error.
// Returns (snapshotType, baseSnapshotID, error).
func (cp *ControlPlane) resolveSnapshotType(reqType, vmID string) (string, string, error) {
	switch strings.ToLower(reqType) {
	case "full":
		return "full", "", nil
	case "diff":
		base := cp.latestFullSnapshot(vmID)
		if base == nil {
			return "", "", fmt.Errorf("no full snapshot found for VM %s; create a full snapshot first", vmID)
		}
		return "diff", base.SnapshotID, nil
	default: // auto
		base := cp.latestFullSnapshot(vmID)
		if base == nil {
			return "full", "", nil
		}
		return "diff", base.SnapshotID, nil
	}
}

// latestFullSnapshot returns the most recent full snapshot for vmID, or nil if none.
func (cp *ControlPlane) latestFullSnapshot(vmID string) *storage.SnapshotMetadata {
	cp.snapshotsMu.RLock()
	defer cp.snapshotsMu.RUnlock()
	var latest *storage.SnapshotMetadata
	for i := range cp.snapshots {
		s := cp.snapshots[i]
		if s.SourceVMID == vmID && s.SnapshotType == "full" {
			if latest == nil || s.CreatedAt.After(latest.CreatedAt) {
				latest = &s
			}
		}
	}
	return latest
}

// snapshotVMMemory pauses the VM and writes a memory+state snapshot to
// memPath/statPath. It does NOT resume or destroy the VM — the caller decides
// (createSnapshot resumes or destroys per its request; DestroyAll proceeds to
// StopVMM). With no opts the snapshot is FULL (the SDK default); createSnapshot
// passes a Diff opt for incremental snapshots. On success the VM is left paused.
func (cp *ControlPlane) snapshotVMMemory(v *runningVM, memPath, statPath string, opts ...firecracker.CreateSnapshotOpt) error {
	if err := v.machine.PauseVM(context.Background()); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}
	if err := v.machine.CreateSnapshot(context.Background(), memPath, statPath, opts...); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	return nil
}

// POST /vms/{vm_id}/snapshot
func (cp *ControlPlane) createSnapshot(w http.ResponseWriter, r *http.Request, vmID string) {
	var req SnapshotRequest
	if r.Body != nil && r.ContentLength != 0 {
		json.NewDecoder(r.Body).Decode(&req)
	}

	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"VM not found"}`, http.StatusNotFound)
		return
	}

	// Determine snapshot type (full or diff) and base ID first, so the disk pre-flight
	// can reserve accurately (a diff stores only the changed rootfs blocks).
	snapType, baseSnapID, err := cp.resolveSnapshotType(req.Type, vmID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadRequest)
		return
	}

	// Disk-space pre-flight (v0.4.0): refuse before pausing the VM if the snapshot would
	// push free space below the operator margin. Full snapshots reserve memory.bin (≈ guest
	// RAM) + a full rootfs copy; diff snapshots reserve memory only (the rootfs delta is
	// bounded by changed blocks, typically a few MB).
	{
		needBytes := uint64(v.memSizeMib) << 20
		if snapType != "diff" {
			if fi, statErr := os.Stat(v.rootfsPath()); statErr == nil {
				needBytes += uint64(fi.Size())
			}
		}
		if err := storage.EnsureFreeSpace(cp.workDir, needBytes, uint64(diskMinFreeMiB)<<20); err != nil {
			w.Header().Set("Content-Type", "application/json")
			status := http.StatusInternalServerError
			var ise *storage.InsufficientStorageError
			if errors.As(err, &ise) {
				status = http.StatusInsufficientStorage
			}
			http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), status)
			return
		}
	}

	snapID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	snapDir := storage.SnapshotDir(cp.workDir, snapID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to create snapshot dir: %v"}`, err), http.StatusInternalServerError)
		return
	}

	memPath := filepath.Join(snapDir, "memory.bin")
	statPath := filepath.Join(snapDir, "state.bin")

	slog.Warn("snapshot: pausing vm", "snapshot_id", snapID, "type", snapType, "vm_id", vmID)
	// Build SDK opts: Diff snapshots pass SnapshotType="Diff" to Firecracker.
	var snapOpts []firecracker.CreateSnapshotOpt
	if snapType == "diff" {
		snapOpts = append(snapOpts, func(p *ops.CreateSnapshotParams) {
			p.Body.SnapshotType = models.SnapshotCreateParamsSnapshotTypeDiff
		})
		slog.Warn("snapshot: creating diff", "snapshot_id", snapID, "base_id", baseSnapID)
	} else {
		slog.Warn("snapshot: creating full", "snapshot_id", snapID)
	}

	if err := cp.snapshotVMMemory(v, memPath, statPath, snapOpts...); err != nil {
		// Best-effort resume: a no-op if the pause itself failed, or brings the
		// VM back if the snapshot failed after pausing.
		v.machine.ResumeVM(context.Background())
		os.RemoveAll(snapDir)
		http.Error(w, fmt.Sprintf(`{"error":"snapshot failed: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Capture the rootfs while the VM is still paused (consistent state). Full snapshots
	// store a complete rootfs.ext4; diff snapshots store only the 4 KiB blocks changed since
	// the base snapshot (sparse rootfs.diff), merged back to a full image on restore.
	diskPath := v.rootfsPath()
	var diskCopyPath, rootfsDiffPath string
	if snapType == "diff" {
		cp.snapshotsMu.RLock()
		base, baseOK := cp.snapshots[baseSnapID]
		cp.snapshotsMu.RUnlock()
		if !baseOK {
			v.machine.ResumeVM(context.Background())
			os.RemoveAll(snapDir)
			http.Error(w, fmt.Sprintf(`{"error":"base snapshot %s not found"}`, baseSnapID), http.StatusInternalServerError)
			return
		}
		rootfsDiffPath = filepath.Join(snapDir, "rootfs.diff")
		slog.Warn("snapshot: writing rootfs diff", "snapshot_id", snapID, "base", base.DiskCopyPath)
		changed, derr := storage.WriteRootfsDiff(diskPath, base.DiskCopyPath, rootfsDiffPath)
		if derr != nil {
			v.machine.ResumeVM(context.Background())
			os.RemoveAll(snapDir)
			http.Error(w, fmt.Sprintf(`{"error":"failed to write rootfs diff: %v"}`, derr), http.StatusInternalServerError)
			return
		}
		slog.Warn("snapshot: rootfs diff written", "snapshot_id", snapID, "changed_bytes", changed)
	} else {
		slog.Warn("snapshot: copying disk", "snapshot_id", snapID)
		diskCopyPath, err = storage.CopyDiskToSnapshot(diskPath, snapDir)
		if err != nil {
			v.machine.ResumeVM(context.Background())
			os.RemoveAll(snapDir)
			http.Error(w, fmt.Sprintf(`{"error":"failed to copy disk: %v"}`, err), http.StatusInternalServerError)
			return
		}
	}

	if !req.StopAfter {
		slog.Warn("snapshot: resuming vm", "snapshot_id", snapID, "vm_id", vmID)
		if err := v.machine.ResumeVM(context.Background()); err != nil {
			slog.Warn("resume vm after snapshot failed", "vm_id", vmID, "err", err)
		}
	} else {
		slog.Warn("snapshot: stop_after, destroying vm", "snapshot_id", snapID, "vm_id", vmID)
		cp.destroyVM(vmID)
	}

	// Firecracker v1.x embeds the TAP device name AND vsock UDS path in state.bin.
	// On restore, Firecracker reopens both by the exact names/paths from the snapshot.
	meta := storage.SnapshotMetadata{
		SnapshotID:     snapID,
		SourceVMID:     vmID,
		Profile:        v.VMInfo.Profile,
		Provider:       v.VMInfo.Provider,
		Model:          v.VMInfo.Model,
		SnapshotType:   snapType,
		BaseSnapshotID: baseSnapID,
		GuestIP:        v.VMInfo.GuestIP,
		TapDevice:      v.tapDevice,
		MacAddr:        deriveMACFromTap(v.tapDevice),
		VsockPath:      v.vsockPath,
		AgentToken:     v.agentToken,
		DiskPath:       diskPath,
		MemFilePath:    memPath,
		StatFilePath:   statPath,
		DiskCopyPath:   diskCopyPath,
		RootfsDiffPath: rootfsDiffPath,
		VcpuCount:      v.vcpuCount,
		MemSizeMib:     v.memSizeMib,
		CreatedAt:      time.Now().UTC(),
	}

	if err := storage.SaveMetadata(snapDir, meta); err != nil {
		slog.Warn("save snapshot metadata failed", "snapshot_id", snapID, "err", err)
	}

	cp.snapshotsMu.Lock()
	cp.snapshots[snapID] = meta
	cp.snapshotsMu.Unlock()

	slog.Warn("snapshot created", "snapshot_id", snapID, "type", snapType, "vm_id", vmID)
	cp.metrics.snapshotCreate.WithLabelValues(snapType).Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(snapshotInfoFrom(meta))
}

// pickMergedMemPath returns the path for a merged memory file used during diff
// snapshot restore. Prefers /dev/shm (tmpfs, RAM-backed) so the 2 GiB copy does
// not hit disk; falls back to {workDir}/tmp when /dev/shm is unavailable
// (e.g. minimal containers) or unwritable.
func pickMergedMemPath(workDir, newVMID string) string {
	const tmpfsDir = "/dev/shm"
	if info, err := os.Stat(tmpfsDir); err == nil && info.IsDir() {
		return filepath.Join(tmpfsDir, "ephemera-"+newVMID+"-merged.bin")
	}
	return filepath.Join(workDir, "tmp", newVMID+"-merged.bin")
}

// pickMergedRootfsPath returns the path for the transient full rootfs produced by merging a
// base snapshot's rootfs with a diff's sparse delta on restore. Unlike pickMergedMemPath it
// is NOT placed on /dev/shm: the merged rootfs is the dm-snapshot's read-only origin, pinned
// by a loop device for the VM's entire life, so tmpfs would hold ~570 MB RAM per restored
// VM. Disk-backed; unlinked right after losetup (its blocks are freed at teardown).
func pickMergedRootfsPath(workDir, newVMID string) string {
	return filepath.Join(workDir, "tmp", newVMID+"-rootfs-merged.ext4")
}

// deriveMACFromTap reproduces the MAC address from a tap device name (e.g. "tap3").
// Must match the formula in network.Manager.Allocate().
func deriveMACFromTap(tapDevice string) string {
	var tapID int
	fmt.Sscanf(tapDevice, "tap%d", &tapID)
	return fmt.Sprintf("AA:FC:00:00:%02X:%02X", tapID/256, tapID%256)
}

// POST /snapshots/{snapshot_id}/restore
// snapshotSizing returns the VM sizing to apply when restoring from snapshot meta.
// Snapshots created from v0.5.2+ carry their sizing; legacy snapshots leave the
// fields zero and were always captured at the historical 2 vCPU / 2048 MiB default,
// so fall back to that. The mem file governs the actual restored memory size — this
// only sets the values reported via the stats API.
func snapshotSizing(meta storage.SnapshotMetadata) (vcpu, mem int64) {
	vcpu, mem = meta.VcpuCount, meta.MemSizeMib
	if vcpu == 0 {
		vcpu = 2
	}
	if mem == 0 {
		mem = 2048
	}
	return vcpu, mem
}

func (cp *ControlPlane) restoreSnapshot(w http.ResponseWriter, snapID string) {
	start := time.Now()
	outcome := "fail"
	delegated := false
	defer func() {
		if delegated {
			// Fallback handler (restoreLegacyBindMount) records its own outcome.
			return
		}
		cp.metrics.snapshotRestore.WithLabelValues(outcome).Inc()
		cp.metrics.snapshotRestoreDuration.Observe(time.Since(start).Seconds())
	}()
	cp.snapshotsMu.RLock()
	meta, ok := cp.snapshots[snapID]
	cp.snapshotsMu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"snapshot not found"}`, http.StatusNotFound)
		return
	}

	// Prevent restoring if the source VM is still running (its disk is in active use).
	cp.mu.RLock()
	for id := range cp.vms {
		if id == meta.SourceVMID {
			cp.mu.RUnlock()
			http.Error(w, fmt.Sprintf(`{"error":"source VM %s is still running (delete it first)"}`, meta.SourceVMID), http.StatusConflict)
			return
		}
	}
	cp.mu.RUnlock()

	newVMID := fmt.Sprintf("vm-%d", time.Now().UnixNano())
	exceptionStorePath := filepath.Join(cp.provisioner.WorkspaceDir, newVMID+".cow")
	socketPath := fmt.Sprintf("/tmp/firecracker-%s.sock", newVMID)
	os.Remove(socketPath)

	// Vsock UDS path: use the original path from the snapshot.
	os.Remove(meta.VsockPath)

	// Allocate any available IP — the guest will be reconfigured to this IP via vsock.
	slog.Warn("restore: allocating network", "snapshot_id", snapID, "tap", meta.TapDevice, "mac", meta.MacAddr)
	tapDevice, newGuestIP, err := cp.netManager.AllocateForRestore(meta.TapDevice, meta.MacAddr)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"network allocation failed: %v"}`, err), http.StatusConflict)
		return
	}

	// Serialize dm-snapshot setup + Firecracker open so each restore sees its own COW device.
	cp.restoreMu.Lock()

	// Look up the base snapshot once for diff restores (shared by the rootfs + memory
	// merges below). A diff's base is always a full snapshot (resolveSnapshotType).
	var base storage.SnapshotMetadata
	if meta.SnapshotType == "diff" {
		cp.snapshotsMu.RLock()
		b, baseOK := cp.snapshots[meta.BaseSnapshotID]
		cp.snapshotsMu.RUnlock()
		if !baseOK {
			cp.restoreMu.Unlock()
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"base snapshot %s not found (was it deleted?)"}`, meta.BaseSnapshotID), http.StatusConflict)
			return
		}
		base = b
	}

	// Resolve the read-only base disk for the COW device. v0.4.0 diff snapshots store only
	// a sparse rootfs delta (RootfsDiffPath); merge it onto the base's full rootfs into a
	// transient full image. Legacy diff + full snapshots use their full rootfs.ext4 directly.
	baseDiskForCOW := meta.DiskCopyPath
	var mergedRootfs string
	if meta.RootfsDiffPath != "" {
		mergedRootfs = pickMergedRootfsPath(cp.workDir, newVMID)
		os.MkdirAll(filepath.Dir(mergedRootfs), 0755)
		os.Remove(mergedRootfs)
		slog.Warn("restore: merging base rootfs and diff", "snapshot_id", snapID, "base", base.DiskCopyPath, "diff", meta.RootfsDiffPath)
		if mErr := storage.MergeRootfsDiff(base.DiskCopyPath, meta.RootfsDiffPath, mergedRootfs); mErr != nil {
			cp.restoreMu.Unlock()
			cp.netManager.Release(tapDevice, newGuestIP)
			os.Remove(mergedRootfs)
			http.Error(w, fmt.Sprintf(`{"error":"failed to merge rootfs diff: %v"}`, mErr), http.StatusInternalServerError)
			return
		}
		baseDiskForCOW = mergedRootfs
	}

	slog.Warn("restore: setting up dm-snapshot cow", "snapshot_id", snapID, "base", baseDiskForCOW, "store", exceptionStorePath)
	dmInfo, err := storage.SetupDMSnapshot(baseDiskForCOW, exceptionStorePath, meta.DiskPath)
	if err != nil {
		cp.restoreMu.Unlock()
		cp.netManager.Release(tapDevice, newGuestIP)
		slog.Warn("restore: dm-snapshot failed, falling back to bind mount", "snapshot_id", snapID, "err", err)
		// Fallback: bind-mount the base disk if dm-snapshot is unavailable.
		newDiskPath := filepath.Join(cp.provisioner.WorkspaceDir, newVMID+".ext4")
		cp.restoreMu.Lock()
		if bmErr := storage.SetupBindMount(baseDiskForCOW, newDiskPath, meta.DiskPath); bmErr != nil {
			cp.restoreMu.Unlock()
			cp.netManager.Release(tapDevice, newGuestIP)
			if mergedRootfs != "" {
				os.Remove(mergedRootfs)
			}
			http.Error(w, fmt.Sprintf(`{"error":"failed to set up disk: dm-snapshot: %v; bind-mount fallback: %v"}`, err, bmErr), http.StatusInternalServerError)
			return
		}
		cp.restoreMu.Unlock()
		// SetupBindMount copied baseDiskForCOW into newDiskPath; the transient merge is done.
		if mergedRootfs != "" {
			os.Remove(mergedRootfs)
		}
		delegated = true
		cp.restoreLegacyBindMount(w, snapID, meta, newVMID, newDiskPath, tapDevice, newGuestIP, socketPath)
		return
	}
	// dm-snapshot pins the merged rootfs via its read-only loop device; unlink the transient
	// file now (its blocks are freed by losetup -d in TeardownDMSnapshot at VM destroy).
	if mergedRootfs != "" {
		os.Remove(mergedRootfs)
	}

	// For diff snapshots: merge base memory + diff memory into a temp file (used for
	// restoration, deleted right after RestoreMachine loads it).
	memFileToUse := meta.MemFilePath
	var mergedMemPath string
	if meta.SnapshotType == "diff" {
		mergedMemPath = pickMergedMemPath(cp.workDir, newVMID)
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		slog.Warn("restore: merging base memory and diff", "snapshot_id", snapID, "base", base.MemFilePath, "diff", meta.MemFilePath)
		if err := storage.MergeMemoryDiff(base.MemFilePath, meta.MemFilePath, mergedMemPath); err != nil {
			cp.restoreMu.Unlock()
			storage.TeardownDMSnapshot(dmInfo)
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"failed to merge diff snapshot: %v"}`, err), http.StatusInternalServerError)
			return
		}
		memFileToUse = mergedMemPath
	}

	slog.Warn("restore: starting vm", "snapshot_id", snapID, "vm_id", newVMID, "type", meta.SnapshotType)
	machine, err := vm.RestoreMachine(context.Background(), vm.VMConfig{
		VMID:           newVMID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		RootfsPath:     meta.DiskPath,
		TapDevice:      tapDevice,
		MacAddress:     meta.MacAddr,
		GuestIP:        newGuestIP,
		GatewayIP:      "10.0.1.1",
		// VsockUDSPath intentionally empty: snapshot state recreates vsock at meta.VsockPath
	}, memFileToUse, meta.StatFilePath)

	cp.restoreMu.Unlock()
	if mergedMemPath != "" {
		os.Remove(mergedMemPath) // temp merged file no longer needed after RestoreMachine
	}

	if err != nil {
		storage.TeardownDMSnapshot(dmInfo)
		cp.netManager.Release(tapDevice, newGuestIP)
		http.Error(w, fmt.Sprintf(`{"error":"failed to restore VM: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Firecracker has restored vsock at meta.VsockPath. Reconfigure the guest's IP.
	slog.Warn("restore: reconfiguring guest ip", "snapshot_id", snapID, "old_ip", meta.GuestIP, "new_ip", newGuestIP, "vsock", meta.VsockPath)
	if err := vm.ReconfigureGuestIP(meta.VsockPath, newGuestIP+"/24", "10.0.1.1"); err != nil {
		slog.Warn("restore: vsock ip reconfigure failed", "snapshot_id", snapID, "err", err)
		machine.StopVMM()
		storage.TeardownDMSnapshot(dmInfo)
		cp.netManager.Release(tapDevice, newGuestIP)
		http.Error(w, fmt.Sprintf(`{"error":"vsock IP reconfigure failed: %v"}`, err), http.StatusInternalServerError)
		return
	}
	slog.Warn("restore: guest ip reconfigured", "snapshot_id", snapID, "ip", newGuestIP, "cow_store", exceptionStorePath)

	info := VMInfo{
		VMID:     newVMID,
		GuestIP:  newGuestIP,
		AgentURL: buildAgentURL(newVMID, newGuestIP),
		Profile:  meta.Profile,
		Provider: meta.Provider,
		Model:    meta.Model,
	}

	rvcpu, rmem := snapshotSizing(meta)
	cp.mu.Lock()
	cp.vms[newVMID] = &runningVM{
		VMInfo:           info,
		agentToken:       meta.AgentToken,
		diskPath:         exceptionStorePath, // only the COW store needs cleanup (not a full disk copy)
		dmSnapshot:       dmInfo,
		vsockPath:        meta.VsockPath,
		machine:          machine,
		tapDevice:        tapDevice,
		socketPath:       socketPath,
		vcpuCount:        rvcpu,
		memSizeMib:       rmem, // from snapshot meta (legacy snapshots → 2048); the mem file governs actual size
		spawnedAt:        time.Now().UTC(),
		sourceSnapshotID: snapID,
	}
	cp.mu.Unlock()

	// Persist a recovery record (v0.4.5) so a daemon restart auto-re-restores this
	// VM from snapID instead of dropping it. DiskPath is the transient COW store;
	// the recoverable artifact is the source snapshot (see recoverRestoredVM).
	// Non-fatal: the VM is already live; a failed write only forfeits auto-recovery.
	if err := storage.SaveVMState(cp.workDir, storage.VMState{
		VMID:             newVMID,
		GuestIP:          newGuestIP,
		TapDevice:        tapDevice,
		MacAddr:          meta.MacAddr,
		VsockPath:        meta.VsockPath,
		SocketPath:       socketPath,
		AgentToken:       meta.AgentToken,
		DiskPath:         exceptionStorePath,
		DiskMode:         storage.DiskModeCOW,
		Profile:          meta.Profile,
		Provider:         meta.Provider,
		Model:            meta.Model,
		VcpuCount:        rvcpu,
		MemSizeMib:       rmem,
		AgentURL:         info.AgentURL,
		SourceSnapshotID: snapID,
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		slog.Warn("restore: persist vm state failed (VM live but won't auto-recover)", "vm_id", newVMID, "err", err)
	}

	slog.Warn("restore: waiting for agent", "snapshot_id", snapID, "agent_url", info.AgentURL)
	// 60s to match the spawn/recovery boot budget. 30s flaked on concurrent restore
	// (two 2GB VMs at once) under host memory pressure — see project-e2e-diff-restore-flaky.
	if err := waitForAgent(newGuestIP, 60*time.Second); err != nil {
		cp.destroyVM(newVMID)
		http.Error(w, fmt.Sprintf(`{"error":"goose-agent not ready after restore: %v"}`, err), http.StatusInternalServerError)
		return
	}
	slog.Warn("restore: vm ready", "snapshot_id", snapID, "vm_id", newVMID, "agent_url", info.AgentURL)

	outcome = "ok"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMRestoreResult{
		VMSpawnResult:    VMSpawnResult{VMInfo: info, AgentToken: meta.AgentToken},
		SourceSnapshotID: snapID,
	})
}

// restoreLegacyBindMount handles the fallback path when dm-snapshot is unavailable.
// It uses the original bind-mount approach (full 700 MB copy per restore).
func (cp *ControlPlane) restoreLegacyBindMount(
	w http.ResponseWriter,
	snapID string, meta storage.SnapshotMetadata,
	newVMID, newDiskPath, tapDevice, newGuestIP, socketPath string,
) {
	start := time.Now()
	outcome := "fail"
	defer func() {
		cp.metrics.snapshotRestore.WithLabelValues(outcome).Inc()
		cp.metrics.snapshotRestoreDuration.Observe(time.Since(start).Seconds())
	}()
	// Diff memory merge if needed.
	memFileToUse := meta.MemFilePath
	var mergedMemPath string
	if meta.SnapshotType == "diff" {
		cp.snapshotsMu.RLock()
		base, baseOK := cp.snapshots[meta.BaseSnapshotID]
		cp.snapshotsMu.RUnlock()
		if !baseOK {
			storage.TeardownBindMount(meta.DiskPath, newDiskPath)
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"base snapshot %s not found"}`, meta.BaseSnapshotID), http.StatusConflict)
			return
		}
		mergedMemPath = pickMergedMemPath(cp.workDir, newVMID)
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		if err := storage.MergeMemoryDiff(base.MemFilePath, meta.MemFilePath, mergedMemPath); err != nil {
			storage.TeardownBindMount(meta.DiskPath, newDiskPath)
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"failed to merge diff: %v"}`, err), http.StatusInternalServerError)
			return
		}
		memFileToUse = mergedMemPath
	}

	machine, err := vm.RestoreMachine(context.Background(), vm.VMConfig{
		VMID:           newVMID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		RootfsPath:     meta.DiskPath,
		TapDevice:      tapDevice,
		MacAddress:     meta.MacAddr,
		GuestIP:        newGuestIP,
		GatewayIP:      "10.0.1.1",
	}, memFileToUse, meta.StatFilePath)
	if mergedMemPath != "" {
		os.Remove(mergedMemPath)
	}
	if err != nil {
		storage.TeardownBindMount(meta.DiskPath, newDiskPath)
		cp.netManager.Release(tapDevice, newGuestIP)
		http.Error(w, fmt.Sprintf(`{"error":"failed to restore VM: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if err := vm.ReconfigureGuestIP(meta.VsockPath, newGuestIP+"/24", "10.0.1.1"); err != nil {
		machine.StopVMM()
		storage.TeardownBindMount(meta.DiskPath, newDiskPath)
		cp.netManager.Release(tapDevice, newGuestIP)
		http.Error(w, fmt.Sprintf(`{"error":"vsock IP reconfigure failed: %v"}`, err), http.StatusInternalServerError)
		return
	}

	info := VMInfo{
		VMID:     newVMID,
		GuestIP:  newGuestIP,
		AgentURL: buildAgentURL(newVMID, newGuestIP),
		Profile:  meta.Profile,
		Provider: meta.Provider,
		Model:    meta.Model,
	}
	rvcpu, rmem := snapshotSizing(meta)
	cp.mu.Lock()
	cp.vms[newVMID] = &runningVM{
		VMInfo:          info,
		agentToken:      meta.AgentToken,
		diskPath:        newDiskPath,
		bindMountTarget: meta.DiskPath,
		vsockPath:       meta.VsockPath,
		machine:         machine,
		tapDevice:       tapDevice,
		socketPath:      socketPath,
		vcpuCount:       rvcpu,
		memSizeMib:      rmem,
		spawnedAt:       time.Now().UTC(),
	}
	cp.mu.Unlock()

	// 60s to match the spawn/recovery boot budget. 30s flaked on concurrent restore
	// (two 2GB VMs at once) under host memory pressure — see project-e2e-diff-restore-flaky.
	if err := waitForAgent(newGuestIP, 60*time.Second); err != nil {
		cp.destroyVM(newVMID)
		http.Error(w, fmt.Sprintf(`{"error":"goose-agent not ready: %v"}`, err), http.StatusInternalServerError)
		return
	}

	outcome = "ok"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMRestoreResult{
		VMSpawnResult:    VMSpawnResult{VMInfo: info, AgentToken: meta.AgentToken},
		SourceSnapshotID: snapID,
	})
}

// DELETE /snapshots/{snapshot_id}
func (cp *ControlPlane) deleteSnapshot(w http.ResponseWriter, snapID string) {
	// Check for diff snapshots that reference this snapshot as their base.
	// Deleting a base would make those diffs un-restorable.
	cp.snapshotsMu.RLock()
	for id, snap := range cp.snapshots {
		if snap.BaseSnapshotID == snapID {
			cp.snapshotsMu.RUnlock()
			http.Error(w, fmt.Sprintf(`{"error":"cannot delete: snapshot %s is the base for diff snapshot %s — delete the diff first"}`, snapID, id), http.StatusConflict)
			return
		}
	}
	cp.snapshotsMu.RUnlock()

	cp.snapshotsMu.Lock()
	meta, ok := cp.snapshots[snapID]
	if ok {
		delete(cp.snapshots, snapID)
	}
	cp.snapshotsMu.Unlock()

	if !ok {
		http.Error(w, `{"error":"snapshot not found"}`, http.StatusNotFound)
		return
	}

	snapDir := storage.SnapshotDir(cp.workDir, snapID)
	if err := storage.DeleteSnapshot(snapDir); err != nil {
		slog.Warn("delete snapshot dir failed", "snapshot_id", snapID, "dir", snapDir, "err", err)
	}

	slog.Warn("snapshot deleted", "snapshot_id", snapID, "type", meta.SnapshotType, "source_vm_id", meta.SourceVMID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "snapshot_id": snapID})
}
