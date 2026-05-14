package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	ops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"

	"ephemera/internal/anvilmcp"
	"ephemera/internal/network"
	"ephemera/internal/storage"
	"ephemera/internal/vm"
)

// authMiddleware enforces per-client Bearer token authentication on all requests.
// getClients is called on every request so token changes (via SIGHUP reload) take
// effect immediately without restarting the server or dropping running VMs.
//
// If getClients returns an empty slice, every request is allowed (auth disabled).
//
// Timing-safe design: subtle.ConstantTimeCompare always inspects every byte of both
// operands before returning, so response time does not vary with how many leading
// characters match. All registered tokens are compared on every request (no
// early-exit after the first match) to prevent leaking which client index was hit.
func authMiddleware(getClients func() []APIClient, metrics *controlPlaneMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clients := getClients()
		if len(clients) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		auth := []byte(r.Header.Get("Authorization"))

		// Compare against every registered token without short-circuiting.
		matchedClient := ""
		for _, c := range clients {
			if subtle.ConstantTimeCompare(auth, []byte("Bearer "+c.Token)) == 1 {
				matchedClient = c.Name
			}
		}

		if matchedClient == "" {
			if metrics != nil {
				metrics.IncAuthFailure()
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="ephemera"`)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		log.Printf("[%s] %s %s", matchedClient, r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// VMInfo is stored per-VM and returned by GET /vms (no token).
type VMInfo struct {
	VMID         string `json:"vm_id"`
	GuestIP      string `json:"guest_ip"`
	AgentURL     string `json:"agent_url"` // proxy URL via control plane when EPHEMERA_PUBLIC_URL is set; otherwise http://{private-ip}:8080
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
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
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
}

type runningVM struct {
	VMInfo
	agentToken      string                  // per-VM bearer token; only returned at spawn time, never re-serialized
	startedAt       time.Time               // host-local start time for structured VM metrics
	diskPath        string                  // actual disk file to delete on teardown (spawned) or exception store (COW-restored)
	bindMountTarget string                  // non-empty for bind-mount restored VMs (legacy path)
	dmSnapshot      *storage.DMSnapshotInfo // non-nil for COW-restored VMs; replaces bindMountTarget
	vsockPath       string                  // host-side UDS for Firecracker vsock proxy; cleaned up on teardown
	machine         *firecracker.Machine
	tapDevice       string
	socketPath      string
}

type controlPlaneMetrics struct {
	mu             sync.RWMutex
	vmCreate       int64
	vmRestore      int64
	vmDelete       int64
	snapshotCreate int64
	snapshotDelete int64
	snapshotGC     int64
	cleanupFailure int64
	authFailure    int64
	queueDepth     int64
	durations      map[string]durationMetric
}

type durationMetric struct {
	Count int64
	Sum   float64
}

func (m *controlPlaneMetrics) IncVMCreate()       { m.add(&m.vmCreate, 1) }
func (m *controlPlaneMetrics) IncVMRestore()      { m.add(&m.vmRestore, 1) }
func (m *controlPlaneMetrics) IncVMDelete()       { m.add(&m.vmDelete, 1) }
func (m *controlPlaneMetrics) IncSnapshotCreate() { m.add(&m.snapshotCreate, 1) }
func (m *controlPlaneMetrics) IncSnapshotDelete() { m.add(&m.snapshotDelete, 1) }
func (m *controlPlaneMetrics) IncSnapshotGC()     { m.add(&m.snapshotGC, 1) }
func (m *controlPlaneMetrics) IncCleanupFailure() { m.add(&m.cleanupFailure, 1) }
func (m *controlPlaneMetrics) IncAuthFailure()    { m.add(&m.authFailure, 1) }
func (m *controlPlaneMetrics) IncQueueDepth()     { m.add(&m.queueDepth, 1) }
func (m *controlPlaneMetrics) DecQueueDepth()     { m.add(&m.queueDepth, -1) }

func (m *controlPlaneMetrics) ObserveDuration(name string, duration time.Duration) {
	name = strings.TrimSpace(name)
	if name == "" || duration < 0 {
		return
	}
	m.mu.Lock()
	if m.durations == nil {
		m.durations = make(map[string]durationMetric)
	}
	metric := m.durations[name]
	metric.Count++
	metric.Sum += duration.Seconds()
	m.durations[name] = metric
	m.mu.Unlock()
}

func (m *controlPlaneMetrics) add(target *int64, delta int64) {
	m.mu.Lock()
	*target += delta
	m.mu.Unlock()
}

func (m *controlPlaneMetrics) snapshot() controlPlaneMetrics {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return controlPlaneMetrics{
		vmCreate:       m.vmCreate,
		vmRestore:      m.vmRestore,
		vmDelete:       m.vmDelete,
		snapshotCreate: m.snapshotCreate,
		snapshotDelete: m.snapshotDelete,
		snapshotGC:     m.snapshotGC,
		cleanupFailure: m.cleanupFailure,
		authFailure:    m.authFailure,
		queueDepth:     m.queueDepth,
		durations:      cloneDurationMetrics(m.durations),
	}
}

func cloneDurationMetrics(in map[string]durationMetric) map[string]durationMetric {
	out := make(map[string]durationMetric, len(in))
	for name, metric := range in {
		out[name] = metric
	}
	return out
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
//   - Agent proxy:  POST /vms/{vm_id}/tasks, GET/PUT /vms/{vm_id}/workspace,
//     GET /vms/{vm_id}/health, POST /vms/{vm_id}/stop
//     (forwarded to the VM's private goose-agent)
type ControlPlane struct {
	mu  sync.RWMutex
	vms map[string]*runningVM

	clientsMu sync.RWMutex
	clients   []APIClient

	snapshotsMu      sync.RWMutex
	snapshots        map[string]storage.SnapshotMetadata
	tenantStore      *anvilmcp.QuotaStore
	egress           egressEnforcer
	runtimeAuditPath string
	metrics          controlPlaneMetrics
	traceExporter    *traceExporter

	snapshotLifecycleMu sync.Mutex

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

	// agentHTTPClient is used for proxying requests to VM goose-agents.
	// No global timeout — timeouts are controlled by the incoming request's context.
	agentHTTPClient *http.Client

	allocateForRestore func(tapDeviceName, macAddr string) (tapDevice string, guestIP string, err error)
	releaseNetwork     func(tapDevice string, guestIP string) error
	setupDMSnapshot    func(baseDiskPath, exceptionStorePath, mountTargetPath string) (*storage.DMSnapshotInfo, error)
	teardownDMSnapshot func(info *storage.DMSnapshotInfo)
	setupBindMount     func(baseDiskPath, newDiskPath, mountTargetPath string) error
	restoreMachine     func(ctx context.Context, cfg vm.VMConfig, memFilePath, snapshotPath string) (*firecracker.Machine, error)

	stopCh chan struct{}
	srv    *http.Server
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
		tenantStore:      anvilmcp.NewQuotaStore(filepath.Join(workDir, "tenants", "tenants.json")),
		egress:           newCommandEgressEnforcer(),
		runtimeAuditPath: filepath.Join(workDir, "audit", "runtime-audit.jsonl"),
		traceExporter:    newTraceExporterFromEnv(http.DefaultClient),
		provisioner:      provisioner,
		netManager:       netManager,
		kernelPath:       kernelPath,
		firecrackerPath:  firecrackerPath,
		gooseConfigPath:  gooseConfigPath,
		gooseSecretsPath: gooseSecretsPath,
		workDir:          workDir,
		snapshotDir:      snapshotDir,
		agentHTTPClient:  &http.Client{},
		stopCh:           make(chan struct{}, 1),
	}
	if err := cp.tenantStore.Load(); err != nil {
		log.Printf("Warning: failed to load tenant store: %v", err)
	}

	// Load any snapshots persisted from previous daemon runs.
	if existing, err := storage.ListSnapshots(workDir); err == nil {
		for _, meta := range existing {
			cp.snapshots[meta.SnapshotID] = meta
		}
		if len(existing) > 0 {
			log.Printf("Loaded %d existing snapshot(s) from %s", len(existing), snapshotDir)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", cp.handleHealth)
	mux.HandleFunc("/metrics", cp.handleMetrics)
	mux.HandleFunc("/metrics/vms", cp.handleVMMetrics)
	mux.HandleFunc("/vms", cp.handleVMs)
	mux.HandleFunc("/vms/", cp.handleVM)
	mux.HandleFunc("/tenants", cp.handleTenants)
	mux.HandleFunc("/tenants/", cp.handleTenantItem)
	mux.HandleFunc("/audit/runtime", cp.handleRuntimeAudit)
	mux.HandleFunc("/audit/runtime/prune", cp.handleRuntimeAuditPrune)
	mux.HandleFunc("/snapshots", cp.handleSnapshots)
	mux.HandleFunc("/snapshots/gc", cp.handleSnapshotGC)
	mux.HandleFunc("/snapshots/", cp.handleSnapshotItem)
	cp.srv = &http.Server{Addr: apiAddr, Handler: authMiddleware(cp.getClients, &cp.metrics, mux)}
	return cp
}

// getClients returns the current authorized client list under a read lock.
func (cp *ControlPlane) getClients() []APIClient {
	cp.clientsMu.RLock()
	defer cp.clientsMu.RUnlock()
	return cp.clients
}

// ReloadClients re-reads API tokens from the environment and hot-swaps the client list.
// Called on SIGHUP. Running VMs are not affected.
func (cp *ControlPlane) ReloadClients() {
	newClients := loadAPIClients()
	cp.clientsMu.Lock()
	cp.clients = newClients
	cp.clientsMu.Unlock()

	if len(newClients) == 0 {
		log.Println("SIGHUP: token reload complete — auth disabled (no tokens configured)")
		return
	}
	names := make([]string, len(newClients))
	for i, c := range newClients {
		names[i] = c.Name
	}
	log.Printf("SIGHUP: token reload complete — %d client(s): %s", len(newClients), strings.Join(names, ", "))
}

func (cp *ControlPlane) Start() error {
	clients := cp.getClients()
	auth := "disabled"
	if len(clients) > 0 {
		names := make([]string, len(clients))
		for i, c := range clients {
			names[i] = c.Name
		}
		auth = fmt.Sprintf("Bearer token (%d client(s): %s)", len(clients), strings.Join(names, ", "))
	}
	log.Printf("Control plane API on %s  (auth: %s)", apiAddr, auth)
	log.Printf("  GET    /health                          — daemon health")
	log.Printf("  GET    /metrics                         — daemon metrics")
	log.Printf("  POST   /vms                              — spawn VM")
	log.Printf("  GET    /vms                              — list VMs")
	log.Printf("  GET    /tenants                          — list tenants")
	log.Printf("  GET/PUT /tenants/{tenant_id}             — tenant quota state")
	log.Printf("  GET    /audit/runtime                    — list runtime audit records")
	log.Printf("  POST   /audit/runtime/prune              — prune runtime audit records")
	log.Printf("  DELETE /vms/{vm_id}                      — stop VM")
	log.Printf("  POST   /vms/{vm_id}/snapshot             — create snapshot")
	log.Printf("  POST   /vms/{vm_id}/tasks                — proxy: run task on agent")
	log.Printf("  GET/PUT /vms/{vm_id}/workspace?path=...  — proxy: workspace file read/write")
	log.Printf("  GET    /vms/{vm_id}/health               — proxy: agent health check")
	log.Printf("  POST   /vms/{vm_id}/stop                 — proxy: stop agent")
	log.Printf("  GET    /snapshots                        — list snapshots")
	log.Printf("  POST   /snapshots/gc                     — plan/apply snapshot retention GC")
	log.Printf("  POST   /snapshots/{snapshot_id}/restore  — restore VM from snapshot")
	log.Printf("  DELETE /snapshots/{snapshot_id}          — delete snapshot")
	if publicURL != "" {
		log.Printf("  agent_url base: %s (EPHEMERA_PUBLIC_URL)", publicURL)
	}
	return cp.srv.ListenAndServe()
}

func (cp *ControlPlane) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cp.srv.Shutdown(ctx)
}

func (cp *ControlPlane) StopCh() <-chan struct{} { return cp.stopCh }

func (cp *ControlPlane) observeLifecycle(name string) func() {
	start := time.Now()
	cp.metrics.IncQueueDepth()
	return func() {
		duration := time.Since(start)
		cp.metrics.DecQueueDepth()
		cp.metrics.ObserveDuration(name, duration)
		if cp.traceExporter != nil {
			if err := cp.traceExporter.Export(context.Background(), name, start, duration, nil); err != nil {
				log.Printf("Warning: failed to export trace span %q: %v", name, err)
			}
		}
	}
}

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

	if strings.HasSuffix(path, "/workspace") {
		vmID := strings.TrimSuffix(path, "/workspace")
		if r.Method != http.MethodGet && r.Method != http.MethodPut {
			http.Error(w, `{"error":"GET or PUT required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.proxyAgentEndpoint(w, r, vmID, "/workspace")
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
	if agentPath == "/health" {
		defer cp.observeLifecycle("agent_health_readiness")()
	}
	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"vm not found"}`, http.StatusNotFound)
		return
	}

	targetURL := fmt.Sprintf("http://%s:%d%s", v.GuestIP, agentPort, agentPath)
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
	io.Copy(w, resp.Body)
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

func normalizeDaemonTenantID(value string) (string, error) {
	tenantID := strings.TrimSpace(value)
	if tenantID == "" {
		return "", nil
	}
	if len(tenantID) > 64 {
		return "", fmt.Errorf("tenant_id must be <= 64 bytes")
	}
	for _, r := range tenantID {
		if r > 127 {
			return "", fmt.Errorf("tenant_id must use ASCII letters, digits, dot, underscore, or hyphen")
		}
		b := byte(r)
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '.' || b == '_' || b == '-' {
			continue
		}
		return "", fmt.Errorf("tenant_id must use ASCII letters, digits, dot, underscore, or hyphen")
	}
	first := tenantID[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return "", fmt.Errorf("tenant_id must start with an ASCII letter or digit")
	}
	if strings.Contains(tenantID, "..") {
		return "", fmt.Errorf("tenant_id must not contain path traversal")
	}
	return tenantID, nil
}

func normalizeDaemonEgressPolicy(value string) (string, error) {
	policy := strings.ToLower(strings.TrimSpace(value))
	switch policy {
	case "":
		return "", nil
	case "deny_all", "profile", "allow_all":
		return policy, nil
	default:
		return "", fmt.Errorf("egress_policy must be empty, deny_all, profile, or allow_all")
	}
}

// profileConfigPaths resolves the goose.yaml and goose-secrets.yaml paths for a given profile.
// An empty profile returns the ControlPlane's default paths (existing behavior).
// Returns HTTP 400-appropriate errors if the profile name is unsafe or the files are missing.
func (cp *ControlPlane) profileConfigPaths(profile string) (configPath, secretsPath string, err error) {
	if profile == "" {
		return cp.gooseConfigPath, cp.gooseSecretsPath, nil
	}
	// Reject path traversal attempts.
	if strings.ContainsAny(profile, "/\\") || profile == ".." {
		return "", "", fmt.Errorf("invalid profile name: %q", profile)
	}
	dir := filepath.Join(cp.workDir, "configs", "profiles", profile)
	configPath = filepath.Join(dir, "goose.yaml")
	secretsPath = filepath.Join(dir, "goose-secrets.yaml")
	if _, e := os.Stat(configPath); os.IsNotExist(e) {
		return "", "", fmt.Errorf("profile %q not found (missing goose.yaml)", profile)
	}
	if _, e := os.Stat(secretsPath); os.IsNotExist(e) {
		return "", "", fmt.Errorf("profile %q not found (missing goose-secrets.yaml)", profile)
	}
	return configPath, secretsPath, nil
}

func (cp *ControlPlane) spawnVM(w http.ResponseWriter, r *http.Request) {
	defer cp.observeLifecycle("vm_create")()
	// Parse optional request body. An empty body is valid (uses default profile).
	var req VMSpawnRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"invalid request body: %v"}`, err), http.StatusBadRequest)
			return
		}
	}
	req.Profile = strings.TrimSpace(req.Profile)
	var err error
	req.TenantID, err = normalizeDaemonTenantID(req.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.EgressPolicy, err = normalizeDaemonEgressPolicy(req.EgressPolicy)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	configPath, secretsPath, err := cp.profileConfigPaths(req.Profile)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadRequest)
		return
	}

	agentToken, err := generateAgentToken()
	if err != nil {
		http.Error(w, fmt.Sprintf("token generation failed: %v", err), http.StatusInternalServerError)
		return
	}

	vmID := fmt.Sprintf("vm-%d", time.Now().UnixNano())

	tapDevice, guestIP, macAddr, err := cp.netManager.Allocate()
	if err != nil {
		http.Error(w, fmt.Sprintf("network allocation failed: %v", err), http.StatusInternalServerError)
		return
	}

	if err := cp.applyEgressPolicy(vmID, tapDevice, guestIP, req.EgressPolicy, req.Profile); err != nil {
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("egress policy failed: %v", err), http.StatusInternalServerError)
		return
	}

	diskPath, err := cp.provisioner.CloneDisk(vmID)
	if err != nil {
		cp.cleanupEgressPolicy(vmID)
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("disk provisioning failed: %v", err), http.StatusInternalServerError)
		return
	}

	if err := cp.provisioner.PrepareVM(vmID, storage.VMPrepareOptions{
		HostConfigPath:  configPath,
		HostSecretsPath: secretsPath,
		AgentToken:      agentToken,
	}); err != nil {
		cp.provisioner.CleanupDisk(vmID)
		cp.cleanupEgressPolicy(vmID)
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("VM preparation failed: %v", err), http.StatusInternalServerError)
		return
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
	})
	if err != nil {
		cp.provisioner.CleanupDisk(vmID)
		cp.cleanupEgressPolicy(vmID)
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("VM start failed: %v", err), http.StatusInternalServerError)
		return
	}

	info := VMInfo{
		VMID:         vmID,
		GuestIP:      guestIP,
		AgentURL:     buildAgentURL(vmID, guestIP),
		Profile:      req.Profile,
		TenantID:     req.TenantID,
		EgressPolicy: req.EgressPolicy,
	}

	cp.mu.Lock()
	cp.vms[vmID] = &runningVM{
		VMInfo:     info,
		agentToken: agentToken,
		startedAt:  time.Now().UTC(),
		diskPath:   diskPath,
		vsockPath:  vsockPath,
		machine:    machine,
		tapDevice:  tapDevice,
		socketPath: socketPath,
	}
	cp.mu.Unlock()

	log.Printf("VM [%s] booting at %s — waiting for goose-agent...", vmID, info.AgentURL)
	if err := waitForAgent(guestIP, 60*time.Second); err != nil {
		cp.destroyVM(vmID)
		http.Error(w, fmt.Sprintf("goose-agent not ready: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("VM [%s] ready — agent: %s  profile: %q", vmID, info.AgentURL, req.Profile)

	cp.metrics.IncVMCreate()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMSpawnResult{VMInfo: info, AgentToken: agentToken})
}

func (cp *ControlPlane) listVMs(w http.ResponseWriter, _ *http.Request) {
	cp.mu.RLock()
	list := make([]VMInfo, 0, len(cp.vms))
	for _, v := range cp.vms {
		list = append(list, v.VMInfo)
	}
	cp.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (cp *ControlPlane) stopVM(w http.ResponseWriter, vmID string) {
	defer cp.observeLifecycle("vm_delete")()
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
	// StopVMM sends SIGTERM and waits for Firecracker to exit.
	v.machine.StopVMM()
	os.Remove(v.socketPath)
	os.Remove(fmt.Sprintf("/tmp/fc-%s-log.fifo", vmID))
	if v.vsockPath != "" {
		os.Remove(v.vsockPath)
	}
	cp.cleanupEgressPolicy(vmID)

	if v.dmSnapshot != nil {
		// COW-restored VM: release dm-snapshot device, loop device, and exception store.
		if err := storage.TeardownDMSnapshot(v.dmSnapshot); err != nil {
			cp.metrics.IncCleanupFailure()
			log.Printf("Warning: failed to teardown COW resources for VM [%s]: %v", vmID, err)
		}
	} else if v.bindMountTarget != "" {
		// Bind-mount restored VM (legacy): lazy-umount + remove per-restore disk copy.
		storage.TeardownBindMount(v.bindMountTarget, v.diskPath)
	} else if v.diskPath != "" {
		if err := os.Remove(v.diskPath); err != nil && !os.IsNotExist(err) {
			log.Printf("Warning: failed to delete disk %s for VM [%s]: %v", v.diskPath, vmID, err)
		}
	}
	cp.netManager.Release(v.tapDevice, v.GuestIP)
	cp.metrics.IncVMDelete()
	log.Printf("VM [%s] destroyed.", vmID)
}

// DestroyAll stops all running VMs. Called on daemon shutdown.
func (cp *ControlPlane) DestroyAll() {
	cp.mu.RLock()
	ids := make([]string, 0, len(cp.vms))
	for id := range cp.vms {
		ids = append(ids, id)
	}
	cp.mu.RUnlock()
	for _, id := range ids {
		cp.destroyVM(id)
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
	TenantID       string    `json:"tenant_id,omitempty"`
	Profile        string    `json:"profile,omitempty"`
	EgressPolicy   string    `json:"egress_policy,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`              // "full" | "diff"
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"` // set for diff snapshots
	CreatedAt      time.Time `json:"created_at"`
}

// VMRestoreResult is returned by POST /snapshots/{id}/restore.
type VMRestoreResult struct {
	VMInfo
	SourceSnapshotID string `json:"source_snapshot_id"`
}

type RestoreErrorResponse struct {
	Error            string `json:"error"`
	Code             string `json:"code"`
	SourceSnapshotID string `json:"source_snapshot_id,omitempty"`
}

// SnapshotRequest is the optional body for POST /vms/{id}/snapshot.
type SnapshotRequest struct {
	StopAfter bool   `json:"stop_after"`
	Type      string `json:"type,omitempty"` // "full" | "diff" | "" (auto-detect)
	TenantID  string `json:"tenant_id,omitempty"`
}

// RestoreSnapshotRequest is the optional body for POST /snapshots/{id}/restore.
type RestoreSnapshotRequest struct {
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
}

type TenantRecord = anvilmcp.TenantRecord

type TenantUpsertRequest struct {
	Quota anvilmcp.TenantQuota `json:"quota"`
}

type RuntimeAuditListResponse = anvilmcp.RuntimeAuditListResponse

type HealthResponse struct {
	Status        string `json:"status"`
	VMCount       int    `json:"vm_count"`
	SnapshotCount int    `json:"snapshot_count"`
	AuthEnabled   bool   `json:"auth_enabled"`
}

type VMMetricsResponse struct {
	VMID         string    `json:"vm_id"`
	GuestIP      string    `json:"guest_ip"`
	Profile      string    `json:"profile,omitempty"`
	TenantID     string    `json:"tenant_id,omitempty"`
	EgressPolicy string    `json:"egress_policy,omitempty"`
	StartedAt    time.Time `json:"started_at"`
}

type egressEnforcer interface {
	Apply(vmID, tapDevice, guestIP, policy string) error
	Cleanup(vmID string) error
}

type egressRule struct {
	GuestIP  string
	Comment  string
	Commands []egressCommand
}

type commandEgressEnforcer struct {
	mu         sync.Mutex
	rules      map[string]egressRule
	profileDir string
	run        func(name string, args ...string) error
}

func newCommandEgressEnforcer() *commandEgressEnforcer {
	return &commandEgressEnforcer{
		rules:      make(map[string]egressRule),
		profileDir: egressProfileDir(),
		run: func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		},
	}
}

func (e *commandEgressEnforcer) Apply(vmID, tapDevice, guestIP, policy string) error {
	return e.ApplyWithProfile(vmID, tapDevice, guestIP, policy, "")
}

func (e *commandEgressEnforcer) ApplyWithProfile(vmID, tapDevice, guestIP, policy, profileName string) error {
	_ = tapDevice
	policy, err := normalizeDaemonEgressPolicy(policy)
	if err != nil {
		return err
	}
	if policy == "" || policy == "allow_all" {
		return nil
	}
	var commands []egressCommand
	comment := "anvil-egress-" + vmID
	if policy == "profile" {
		profile, ok, err := loadEgressProfile(e.profileDir, profileName)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		commands, err = planProfileEgressCommands(vmID, guestIP, profile)
		if err != nil {
			return err
		}
	} else {
		commands = []egressCommand{{
			Name: "iptables",
			Args: []string{"-I", "FORWARD", "-s", guestIP, "-j", "REJECT", "-m", "comment", "--comment", comment},
		}}
	}
	for _, command := range commands {
		if err := e.command(command.Name, command.Args...); err != nil {
			return fmt.Errorf("apply egress policy: %w", err)
		}
	}
	e.mu.Lock()
	if e.rules == nil {
		e.rules = make(map[string]egressRule)
	}
	e.rules[vmID] = egressRule{GuestIP: guestIP, Comment: comment, Commands: commands}
	e.mu.Unlock()
	return nil
}

func (e *commandEgressEnforcer) Cleanup(vmID string) error {
	e.mu.Lock()
	rule, ok := e.rules[vmID]
	if ok {
		delete(e.rules, vmID)
	}
	e.mu.Unlock()
	if !ok {
		return nil
	}
	commands := append([]egressCommand(nil), rule.Commands...)
	for left, right := 0, len(commands)-1; left < right; left, right = left+1, right-1 {
		commands[left], commands[right] = commands[right], commands[left]
	}
	if len(commands) == 0 {
		commands = []egressCommand{{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", rule.GuestIP, "-j", "REJECT", "-m", "comment", "--comment", rule.Comment}}}
	}
	for _, command := range commands {
		args := append([]string(nil), command.Args...)
		if len(args) > 0 && args[0] == "-I" {
			args[0] = "-D"
		}
		if err := e.command(command.Name, args...); err != nil {
			return fmt.Errorf("cleanup egress policy: %w", err)
		}
	}
	return nil
}

func (e *commandEgressEnforcer) command(name string, args ...string) error {
	if e.run != nil {
		return e.run(name, args...)
	}
	return exec.Command(name, args...).Run()
}

// SnapshotGCRequest is the optional body for POST /snapshots/gc.
type SnapshotGCRequest struct {
	OlderThanSeconds int64 `json:"older_than_seconds"`
	KeepLastPerVM    int   `json:"keep_last_per_vm"`
	MaxTotalBytes    int64 `json:"max_total_bytes"`
	Apply            bool  `json:"apply"`
}

// SnapshotGCPolicy is echoed in GC responses without the apply flag.
type SnapshotGCPolicy struct {
	OlderThanSeconds int64 `json:"older_than_seconds"`
	KeepLastPerVM    int   `json:"keep_last_per_vm"`
	MaxTotalBytes    int64 `json:"max_total_bytes"`
}

// SnapshotGCEntry is the public representation of one GC decision.
type SnapshotGCEntry struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Reason         string    `json:"reason"`
	ReferencedBy   []string  `json:"referenced_by,omitempty"`
	SizeBytes      int64     `json:"size_bytes,omitempty"`
}

// SnapshotGCError records a per-snapshot apply failure.
type SnapshotGCError struct {
	SnapshotID string `json:"snapshot_id"`
	Error      string `json:"error"`
}

// SnapshotGCResponse is returned by POST /snapshots/gc for dry-run and apply.
type SnapshotGCResponse struct {
	Applied     bool              `json:"applied"`
	RequestedAt time.Time         `json:"requested_at"`
	Policy      SnapshotGCPolicy  `json:"policy"`
	Candidates  []SnapshotGCEntry `json:"candidates"`
	Protected   []SnapshotGCEntry `json:"protected"`
	Deleted     []SnapshotGCEntry `json:"deleted"`
	Errors      []SnapshotGCError `json:"errors"`
}

const (
	snapshotGCReasonOlderThan        = "older_than"
	snapshotGCReasonReferencedByDiff = "referenced_by_diff"
	snapshotGCReasonKeepLastPerVM    = "keep_last_per_vm"
	snapshotGCReasonMaxTotalBytes    = "max_total_bytes"
)

func snapshotInfoFrom(meta storage.SnapshotMetadata) SnapshotInfo {
	return SnapshotInfo{
		SnapshotID:     meta.SnapshotID,
		SourceVMID:     meta.SourceVMID,
		TenantID:       meta.TenantID,
		Profile:        meta.Profile,
		EgressPolicy:   meta.EgressPolicy,
		SnapshotType:   meta.SnapshotType,
		BaseSnapshotID: meta.BaseSnapshotID,
		CreatedAt:      meta.CreatedAt,
	}
}

func snapshotGCEntryFrom(meta storage.SnapshotMetadata, reason string, referencedBy []string, sizeBytes int64) SnapshotGCEntry {
	refs := append([]string(nil), referencedBy...)
	sort.Strings(refs)
	return SnapshotGCEntry{
		SnapshotID:     meta.SnapshotID,
		SourceVMID:     meta.SourceVMID,
		Profile:        meta.Profile,
		SnapshotType:   meta.SnapshotType,
		BaseSnapshotID: meta.BaseSnapshotID,
		CreatedAt:      meta.CreatedAt,
		Reason:         reason,
		ReferencedBy:   refs,
		SizeBytes:      sizeBytes,
	}
}

func sortSnapshotsOldestFirst(snapshots []storage.SnapshotMetadata) {
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].CreatedAt.Equal(snapshots[j].CreatedAt) {
			return snapshots[i].SnapshotID < snapshots[j].SnapshotID
		}
		return snapshots[i].CreatedAt.Before(snapshots[j].CreatedAt)
	})
}

func (cp *ControlPlane) snapshotMetadataList() []storage.SnapshotMetadata {
	cp.snapshotsMu.RLock()
	defer cp.snapshotsMu.RUnlock()

	list := make([]storage.SnapshotMetadata, 0, len(cp.snapshots))
	for _, meta := range cp.snapshots {
		list = append(list, meta)
	}
	sortSnapshotsOldestFirst(list)
	return list
}

func (cp *ControlPlane) snapshotSizeBytes(snapshotID string) int64 {
	var total int64
	snapDir := storage.SnapshotDir(cp.workDir, snapshotID)
	_ = filepath.WalkDir(snapDir, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

func (cp *ControlPlane) planSnapshotGC(policy SnapshotGCPolicy, now time.Time) SnapshotGCResponse {
	snapshots := cp.snapshotMetadataList()
	sizes := make(map[string]int64, len(snapshots))
	var totalBytes int64
	for _, meta := range snapshots {
		sizeBytes := cp.snapshotSizeBytes(meta.SnapshotID)
		sizes[meta.SnapshotID] = sizeBytes
		totalBytes += sizeBytes
	}

	referencedBy := make(map[string][]string)
	for _, meta := range snapshots {
		if meta.BaseSnapshotID != "" {
			referencedBy[meta.BaseSnapshotID] = append(referencedBy[meta.BaseSnapshotID], meta.SnapshotID)
		}
	}
	for id := range referencedBy {
		sort.Strings(referencedBy[id])
	}

	protected := make(map[string]SnapshotGCEntry)
	for _, meta := range snapshots {
		if refs, ok := referencedBy[meta.SnapshotID]; ok {
			protected[meta.SnapshotID] = snapshotGCEntryFrom(meta, snapshotGCReasonReferencedByDiff, refs, sizes[meta.SnapshotID])
		}
	}

	if policy.KeepLastPerVM > 0 {
		byVM := make(map[string][]storage.SnapshotMetadata)
		for _, meta := range snapshots {
			byVM[meta.SourceVMID] = append(byVM[meta.SourceVMID], meta)
		}
		for _, group := range byVM {
			sort.Slice(group, func(i, j int) bool {
				if group[i].CreatedAt.Equal(group[j].CreatedAt) {
					return group[i].SnapshotID > group[j].SnapshotID
				}
				return group[i].CreatedAt.After(group[j].CreatedAt)
			})
			for i := 0; i < len(group) && i < policy.KeepLastPerVM; i++ {
				meta := group[i]
				if _, exists := protected[meta.SnapshotID]; !exists {
					protected[meta.SnapshotID] = snapshotGCEntryFrom(meta, snapshotGCReasonKeepLastPerVM, nil, sizes[meta.SnapshotID])
				}
			}
		}
	}

	resp := SnapshotGCResponse{
		RequestedAt: now,
		Policy:      policy,
		Candidates:  []SnapshotGCEntry{},
		Protected:   []SnapshotGCEntry{},
		Deleted:     []SnapshotGCEntry{},
		Errors:      []SnapshotGCError{},
	}

	cutoff := now.Add(-time.Duration(policy.OlderThanSeconds) * time.Second)
	candidateIDs := make(map[string]struct{})
	projectedRemainingBytes := totalBytes
	selectAgeCandidates := policy.OlderThanSeconds > 0 || policy.MaxTotalBytes == 0
	for _, meta := range snapshots {
		if entry, ok := protected[meta.SnapshotID]; ok {
			resp.Protected = append(resp.Protected, entry)
			continue
		}
		if selectAgeCandidates && (policy.OlderThanSeconds == 0 || !meta.CreatedAt.After(cutoff)) {
			resp.Candidates = append(resp.Candidates, snapshotGCEntryFrom(meta, snapshotGCReasonOlderThan, nil, sizes[meta.SnapshotID]))
			candidateIDs[meta.SnapshotID] = struct{}{}
			projectedRemainingBytes -= sizes[meta.SnapshotID]
		}
	}

	if policy.MaxTotalBytes > 0 && projectedRemainingBytes > policy.MaxTotalBytes {
		for _, meta := range snapshots {
			if projectedRemainingBytes <= policy.MaxTotalBytes {
				break
			}
			if _, ok := protected[meta.SnapshotID]; ok {
				continue
			}
			if _, ok := candidateIDs[meta.SnapshotID]; ok {
				continue
			}
			resp.Candidates = append(resp.Candidates, snapshotGCEntryFrom(meta, snapshotGCReasonMaxTotalBytes, nil, sizes[meta.SnapshotID]))
			candidateIDs[meta.SnapshotID] = struct{}{}
			projectedRemainingBytes -= sizes[meta.SnapshotID]
		}
	}
	return resp
}

// ---- Snapshot handlers ----

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func writeRestoreError(w http.ResponseWriter, status int, code string, snapshotID string, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(RestoreErrorResponse{
		Error:            message,
		Code:             code,
		SourceSnapshotID: snapshotID,
	})
}

func (cp *ControlPlane) ensureTenantStore() *anvilmcp.QuotaStore {
	if cp.tenantStore == nil {
		cp.tenantStore = anvilmcp.NewQuotaStore(filepath.Join(cp.workDir, "tenants", "tenants.json"))
		_ = cp.tenantStore.Load()
	}
	return cp.tenantStore
}

func (cp *ControlPlane) applyEgressPolicy(vmID, tapDevice, guestIP, policy, profile string) error {
	if cp.egress == nil {
		return nil
	}
	if profileEnforcer, ok := cp.egress.(interface {
		ApplyWithProfile(vmID, tapDevice, guestIP, policy, profile string) error
	}); ok {
		return profileEnforcer.ApplyWithProfile(vmID, tapDevice, guestIP, policy, profile)
	}
	return cp.egress.Apply(vmID, tapDevice, guestIP, policy)
}

func (cp *ControlPlane) cleanupEgressPolicy(vmID string) {
	if cp.egress == nil {
		return
	}
	if err := cp.egress.Cleanup(vmID); err != nil {
		cp.metrics.IncCleanupFailure()
		log.Printf("Warning: failed to cleanup egress policy for VM [%s]: %v", vmID, err)
	}
}

func (cp *ControlPlane) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	cp.mu.RLock()
	vmCount := len(cp.vms)
	cp.mu.RUnlock()
	cp.snapshotsMu.RLock()
	snapshotCount := len(cp.snapshots)
	cp.snapshotsMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(HealthResponse{
		Status:        "ok",
		VMCount:       vmCount,
		SnapshotCount: snapshotCount,
		AuthEnabled:   len(cp.getClients()) > 0,
	})
}

func (cp *ControlPlane) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	m := cp.metrics.snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "anvil_vm_create_total %d\n", m.vmCreate)
	fmt.Fprintf(w, "anvil_vm_restore_total %d\n", m.vmRestore)
	fmt.Fprintf(w, "anvil_vm_delete_total %d\n", m.vmDelete)
	fmt.Fprintf(w, "anvil_snapshot_create_total %d\n", m.snapshotCreate)
	fmt.Fprintf(w, "anvil_snapshot_delete_total %d\n", m.snapshotDelete)
	fmt.Fprintf(w, "anvil_snapshot_gc_total %d\n", m.snapshotGC)
	fmt.Fprintf(w, "anvil_cleanup_failure_total %d\n", m.cleanupFailure)
	fmt.Fprintf(w, "anvil_auth_failure_total %d\n", m.authFailure)
	fmt.Fprintf(w, "anvil_lifecycle_queue_depth %d\n", m.queueDepth)
	durationNames := make([]string, 0, len(m.durations))
	for name := range m.durations {
		durationNames = append(durationNames, name)
	}
	sort.Strings(durationNames)
	for _, name := range durationNames {
		metric := m.durations[name]
		fmt.Fprintf(w, "anvil_%s_duration_seconds_count %d\n", name, metric.Count)
		fmt.Fprintf(w, "anvil_%s_duration_seconds_sum %.6f\n", name, metric.Sum)
	}
}

func (cp *ControlPlane) handleVMMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	cp.mu.RLock()
	metrics := make([]VMMetricsResponse, 0, len(cp.vms))
	for _, vm := range cp.vms {
		metrics = append(metrics, VMMetricsResponse{
			VMID:         vm.VMID,
			GuestIP:      vm.GuestIP,
			Profile:      vm.Profile,
			TenantID:     vm.TenantID,
			EgressPolicy: vm.EgressPolicy,
			StartedAt:    vm.startedAt,
		})
	}
	cp.mu.RUnlock()
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].VMID < metrics[j].VMID })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(metrics)
}

func (cp *ControlPlane) handleTenants(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/tenants" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	records := cp.ensureTenantStore().ListTenants()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(records)
}

func (cp *ControlPlane) handleTenantItem(w http.ResponseWriter, r *http.Request) {
	tenantID := strings.TrimPrefix(r.URL.Path, "/tenants/")
	tenantID, err := anvilmcp.NormalizeTenantID(tenantID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	store := cp.ensureTenantStore()

	switch r.Method {
	case http.MethodGet:
		record, ok, err := store.GetTenant(tenantID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !ok {
			writeJSONError(w, http.StatusNotFound, "tenant not found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(record)
	case http.MethodPut:
		var req TenantUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
			return
		}
		if err := store.SetTenantQuota(tenantID, req.Quota); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := store.Save(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("save tenant store: %v", err))
			return
		}
		record, _, err := store.GetTenant(tenantID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(record)
	default:
		http.Error(w, "GET or PUT required", http.StatusMethodNotAllowed)
	}
}

func (cp *ControlPlane) handleRuntimeAudit(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/audit/runtime" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	records, err := anvilmcp.ReadRuntimeAudit(cp.runtimeAuditPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tenantID := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if tenantID != "" {
		normalized, err := anvilmcp.NormalizeTenantID(tenantID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		tenantID = normalized
	}
	limit := 0
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 0 {
			writeJSONError(w, http.StatusBadRequest, "limit must be a non-negative integer")
			return
		}
		limit = parsed
	}
	records = filterRuntimeAuditRecords(records, tenantID, limit)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(RuntimeAuditListResponse{Records: records})
}

func (cp *ControlPlane) handleRuntimeAuditPrune(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/audit/runtime/prune" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var policy anvilmcp.RuntimeAuditRetention
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil && err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if err := anvilmcp.PruneRuntimeAudit(cp.runtimeAuditPath, policy); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	records, err := anvilmcp.ReadRuntimeAudit(cp.runtimeAuditPath)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	records = filterRuntimeAuditRecords(records, "", 0)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(RuntimeAuditListResponse{Records: records})
}

func filterRuntimeAuditRecords(records []anvilmcp.RuntimeAuditRecord, tenantID string, limit int) []anvilmcp.RuntimeAuditRecord {
	filtered := make([]anvilmcp.RuntimeAuditRecord, 0, len(records))
	for _, record := range records {
		if tenantID != "" && record.TenantID != tenantID {
			continue
		}
		filtered = append(filtered, sanitizeRuntimeAuditRecord(record))
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered
}

func sanitizeRuntimeAuditRecord(record anvilmcp.RuntimeAuditRecord) anvilmcp.RuntimeAuditRecord {
	lowerError := strings.ToLower(record.Error)
	if strings.Contains(lowerError, "agent_token") || strings.Contains(lowerError, "secret") {
		record.Error = "[redacted]"
	}
	return record
}

func (cp *ControlPlane) allocateNetworkForRestore(tapDeviceName, macAddr string) (string, string, error) {
	if cp.allocateForRestore != nil {
		return cp.allocateForRestore(tapDeviceName, macAddr)
	}
	return cp.netManager.AllocateForRestore(tapDeviceName, macAddr)
}

func (cp *ControlPlane) releaseRestoreNetwork(tapDevice, guestIP string) error {
	if cp.releaseNetwork != nil {
		return cp.releaseNetwork(tapDevice, guestIP)
	}
	return cp.netManager.Release(tapDevice, guestIP)
}

func (cp *ControlPlane) setupRestoreDMSnapshot(baseDiskPath, exceptionStorePath, mountTargetPath string) (*storage.DMSnapshotInfo, error) {
	if cp.setupDMSnapshot != nil {
		return cp.setupDMSnapshot(baseDiskPath, exceptionStorePath, mountTargetPath)
	}
	return storage.SetupDMSnapshot(baseDiskPath, exceptionStorePath, mountTargetPath)
}

func (cp *ControlPlane) teardownRestoreDMSnapshot(info *storage.DMSnapshotInfo) {
	if cp.teardownDMSnapshot != nil {
		cp.teardownDMSnapshot(info)
		return
	}
	if err := storage.TeardownDMSnapshot(info); err != nil {
		log.Printf("Warning: failed to teardown restore COW resources: %v", err)
	}
}

func (cp *ControlPlane) setupRestoreBindMount(baseDiskPath, newDiskPath, mountTargetPath string) error {
	if cp.setupBindMount != nil {
		return cp.setupBindMount(baseDiskPath, newDiskPath, mountTargetPath)
	}
	return storage.SetupBindMount(baseDiskPath, newDiskPath, mountTargetPath)
}

func (cp *ControlPlane) restoreSnapshotMachine(ctx context.Context, cfg vm.VMConfig, memFilePath, snapshotPath string) (*firecracker.Machine, error) {
	if cp.restoreMachine != nil {
		return cp.restoreMachine(ctx, cfg, memFilePath, snapshotPath)
	}
	return vm.RestoreMachine(ctx, cfg, memFilePath, snapshotPath)
}

// POST /snapshots/gc
func (cp *ControlPlane) handleSnapshotGC(w http.ResponseWriter, r *http.Request) {
	defer cp.observeLifecycle("snapshot_gc")()
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	var req SnapshotGCRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
			return
		}
	}
	if req.OlderThanSeconds < 0 {
		writeJSONError(w, http.StatusBadRequest, "older_than_seconds must be non-negative")
		return
	}
	if req.KeepLastPerVM < 0 {
		writeJSONError(w, http.StatusBadRequest, "keep_last_per_vm must be non-negative")
		return
	}
	if req.MaxTotalBytes < 0 {
		writeJSONError(w, http.StatusBadRequest, "max_total_bytes must be non-negative")
		return
	}
	policy := SnapshotGCPolicy{
		OlderThanSeconds: req.OlderThanSeconds,
		KeepLastPerVM:    req.KeepLastPerVM,
		MaxTotalBytes:    req.MaxTotalBytes,
	}
	resp := cp.planSnapshotGC(policy, time.Now().UTC())
	resp.Applied = req.Apply
	if req.Apply {
		cp.applySnapshotGC(&resp)
		cp.metrics.IncSnapshotGC()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (cp *ControlPlane) applySnapshotGC(resp *SnapshotGCResponse) {
	for _, candidate := range resp.Candidates {
		_, _, err := cp.deleteSnapshotByID(candidate.SnapshotID)
		if err != nil {
			resp.Errors = append(resp.Errors, SnapshotGCError{
				SnapshotID: candidate.SnapshotID,
				Error:      err.Error(),
			})
			continue
		}
		resp.Deleted = append(resp.Deleted, candidate)
	}

	record := storage.SnapshotGCAuditRecord{
		Timestamp: time.Now().UTC(),
		Applied:   resp.Applied,
		Policy: storage.SnapshotGCAuditPolicy{
			OlderThanSeconds: resp.Policy.OlderThanSeconds,
			KeepLastPerVM:    resp.Policy.KeepLastPerVM,
			MaxTotalBytes:    resp.Policy.MaxTotalBytes,
		},
		CandidatesCount: len(resp.Candidates),
		DeletedCount:    len(resp.Deleted),
		ErrorsCount:     len(resp.Errors),
	}
	if err := storage.AppendSnapshotGCAudit(cp.workDir, record); err != nil {
		log.Printf("Warning: failed to write snapshot GC audit: %v", err)
		resp.Errors = append(resp.Errors, SnapshotGCError{
			Error: "write GC audit: failed to append audit record",
		})
	}
}

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
		cp.restoreSnapshotFromRequest(w, r, snapID)
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

// POST /vms/{vm_id}/snapshot
func (cp *ControlPlane) createSnapshot(w http.ResponseWriter, r *http.Request, vmID string) {
	defer cp.observeLifecycle("snapshot_create")()
	var req SnapshotRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
			return
		}
	}
	var err error
	req.TenantID, err = normalizeDaemonTenantID(req.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"VM not found"}`, http.StatusNotFound)
		return
	}
	if req.TenantID != "" && v.VMInfo.TenantID != "" && req.TenantID != v.VMInfo.TenantID {
		writeJSONError(w, http.StatusForbidden, "tenant_id does not match VM tenant")
		return
	}
	snapshotTenantID := v.VMInfo.TenantID
	if snapshotTenantID == "" {
		snapshotTenantID = req.TenantID
	}

	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()

	// Determine snapshot type (full or diff) and base snapshot ID.
	snapType, baseSnapID, err := cp.resolveSnapshotType(req.Type, vmID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadRequest)
		return
	}

	snapID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	snapDir := storage.SnapshotDir(cp.workDir, snapID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to create snapshot dir: %v"}`, err), http.StatusInternalServerError)
		return
	}

	memPath := filepath.Join(snapDir, "memory.bin")
	statPath := filepath.Join(snapDir, "state.bin")

	log.Printf("Snapshot [%s] (%s): pausing VM [%s]...", snapID, snapType, vmID)
	if err := v.machine.PauseVM(context.Background()); err != nil {
		os.RemoveAll(snapDir)
		http.Error(w, fmt.Sprintf(`{"error":"failed to pause VM: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Build SDK opts: Diff snapshots pass SnapshotType="Diff" to Firecracker.
	var snapOpts []firecracker.CreateSnapshotOpt
	if snapType == "diff" {
		snapOpts = append(snapOpts, func(p *ops.CreateSnapshotParams) {
			p.Body.SnapshotType = models.SnapshotCreateParamsSnapshotTypeDiff
		})
		log.Printf("Snapshot [%s]: creating Diff snapshot (base: %s)...", snapID, baseSnapID)
	} else {
		log.Printf("Snapshot [%s]: creating Full snapshot...", snapID)
	}

	if err := v.machine.CreateSnapshot(context.Background(), memPath, statPath, snapOpts...); err != nil {
		v.machine.ResumeVM(context.Background())
		os.RemoveAll(snapDir)
		http.Error(w, fmt.Sprintf(`{"error":"failed to create snapshot: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Copy disk while VM is still paused (ensures consistent state).
	// Diff snapshots still copy the full rootfs — rootfs diff is a future optimization.
	diskPath := filepath.Join("/tmp/goose-workspaces", vmID+".ext4")
	log.Printf("Snapshot [%s]: copying disk...", snapID)
	diskCopyPath, err := storage.CopyDiskToSnapshot(diskPath, snapDir)
	if err != nil {
		v.machine.ResumeVM(context.Background())
		os.RemoveAll(snapDir)
		http.Error(w, fmt.Sprintf(`{"error":"failed to copy disk: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if !req.StopAfter {
		log.Printf("Snapshot [%s]: resuming VM [%s]...", snapID, vmID)
		if err := v.machine.ResumeVM(context.Background()); err != nil {
			log.Printf("Warning: failed to resume VM [%s] after snapshot: %v", vmID, err)
		}
	} else {
		log.Printf("Snapshot [%s]: stop_after=true, destroying VM [%s]", snapID, vmID)
		cp.destroyVM(vmID)
	}

	// Firecracker v1.x embeds the TAP device name AND vsock UDS path in state.bin.
	// On restore, Firecracker reopens both by the exact names/paths from the snapshot.
	meta := storage.SnapshotMetadata{
		SnapshotID:     snapID,
		SourceVMID:     vmID,
		TenantID:       snapshotTenantID,
		Profile:        v.VMInfo.Profile,
		EgressPolicy:   v.VMInfo.EgressPolicy,
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
		CreatedAt:      time.Now().UTC(),
	}

	if err := storage.SaveMetadata(snapDir, meta); err != nil {
		log.Printf("Warning: failed to save snapshot metadata: %v", err)
	}

	cp.snapshotsMu.Lock()
	cp.snapshots[snapID] = meta
	cp.snapshotsMu.Unlock()

	log.Printf("Snapshot [%s] (%s) created from VM [%s]", snapID, snapType, vmID)
	cp.metrics.IncSnapshotCreate()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(snapshotInfoFrom(meta))
}

// deriveMACFromTap reproduces the MAC address from a tap device name (e.g. "tap3").
// Must match the formula in network.Manager.Allocate().
func deriveMACFromTap(tapDevice string) string {
	var tapID int
	fmt.Sscanf(tapDevice, "tap%d", &tapID)
	return fmt.Sprintf("AA:FC:00:00:%02X:%02X", tapID/256, tapID%256)
}

// POST /snapshots/{snapshot_id}/restore
func (cp *ControlPlane) restoreSnapshot(w http.ResponseWriter, snapID string) {
	cp.restoreSnapshotWithRequest(w, snapID, RestoreSnapshotRequest{})
}

func (cp *ControlPlane) restoreSnapshotFromRequest(w http.ResponseWriter, r *http.Request, snapID string) {
	var req RestoreSnapshotRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeRestoreError(w, http.StatusBadRequest, "invalid_restore_request", snapID, fmt.Sprintf("invalid JSON body: %v", err))
			return
		}
	}
	cp.restoreSnapshotWithRequest(w, snapID, req)
}

func restoreTenantAndEgress(meta storage.SnapshotMetadata, req RestoreSnapshotRequest) (tenantID, egressPolicy string, status int, code, message string, ok bool) {
	reqTenantID, err := normalizeDaemonTenantID(req.TenantID)
	if err != nil {
		return "", "", http.StatusBadRequest, "invalid_tenant_id", err.Error(), false
	}
	reqEgressPolicy, err := normalizeDaemonEgressPolicy(req.EgressPolicy)
	if err != nil {
		return "", "", http.StatusBadRequest, "invalid_egress_policy", err.Error(), false
	}
	if meta.TenantID != "" && reqTenantID != "" && reqTenantID != meta.TenantID {
		return "", "", http.StatusForbidden, "tenant_mismatch", "tenant_id does not match snapshot tenant", false
	}
	if meta.EgressPolicy != "" && reqEgressPolicy != "" && reqEgressPolicy != meta.EgressPolicy {
		return "", "", http.StatusForbidden, "egress_policy_mismatch", "egress_policy does not match snapshot egress policy", false
	}
	tenantID = meta.TenantID
	if tenantID == "" {
		tenantID = reqTenantID
	}
	egressPolicy = meta.EgressPolicy
	if egressPolicy == "" {
		egressPolicy = reqEgressPolicy
	}
	return tenantID, egressPolicy, 0, "", "", true
}

func (cp *ControlPlane) restoreSnapshotWithRequest(w http.ResponseWriter, snapID string, req RestoreSnapshotRequest) {
	defer cp.observeLifecycle("vm_restore")()
	// Prevent delete/GC from removing snapshot files while restore reads them.
	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()

	cp.snapshotsMu.RLock()
	meta, ok := cp.snapshots[snapID]
	cp.snapshotsMu.RUnlock()
	if !ok {
		writeRestoreError(w, http.StatusNotFound, "snapshot_not_found", snapID, "snapshot not found")
		return
	}
	restoreTenantID, restoreEgressPolicy, status, code, message, ok := restoreTenantAndEgress(meta, req)
	if !ok {
		writeRestoreError(w, status, code, snapID, message)
		return
	}

	// Prevent restoring if the source VM is still running (its disk is in active use).
	cp.mu.RLock()
	for id := range cp.vms {
		if id == meta.SourceVMID {
			cp.mu.RUnlock()
			writeRestoreError(w, http.StatusConflict, "source_vm_running", snapID, fmt.Sprintf("source VM %s is still running (delete it first)", meta.SourceVMID))
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
	log.Printf("Restore [%s]: allocating network (TAP: %s, MAC: %s)...", snapID, meta.TapDevice, meta.MacAddr)
	tapDevice, newGuestIP, err := cp.allocateNetworkForRestore(meta.TapDevice, meta.MacAddr)
	if err != nil {
		writeRestoreError(w, http.StatusConflict, "network_unavailable", snapID, fmt.Sprintf("network allocation failed: %v", err))
		return
	}

	// Serialize dm-snapshot setup + Firecracker open so each restore sees its own COW device.
	cp.restoreMu.Lock()

	log.Printf("Restore [%s]: setting up dm-snapshot COW (base: %s, store: %s)...", snapID, meta.DiskCopyPath, exceptionStorePath)
	dmInfo, err := cp.setupRestoreDMSnapshot(meta.DiskCopyPath, exceptionStorePath, meta.DiskPath)
	if err != nil {
		cp.restoreMu.Unlock()
		log.Printf("Restore [%s]: dm-snapshot failed (%v), falling back to bind mount", snapID, err)
		// Fallback: use the existing bind-mount approach if dm-snapshot is unavailable.
		newDiskPath := filepath.Join(cp.provisioner.WorkspaceDir, newVMID+".ext4")
		cp.restoreMu.Lock()
		if bmErr := cp.setupRestoreBindMount(meta.DiskCopyPath, newDiskPath, meta.DiskPath); bmErr != nil {
			cp.restoreMu.Unlock()
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			writeRestoreError(w, http.StatusInternalServerError, "firecracker_restore_failed", snapID, fmt.Sprintf("failed to set up disk: dm-snapshot: %v; bind-mount fallback: %v", err, bmErr))
			return
		}
		// Continue with bind-mount path (legacy runningVM fields).
		cp.restoreMu.Unlock()
		cp.restoreLegacyBindMount(w, snapID, meta, newVMID, newDiskPath, tapDevice, newGuestIP, socketPath, restoreTenantID, restoreEgressPolicy)
		return
	}

	// For diff snapshots: merge base memory + diff memory into a temp file.
	// The merged file is used for restoration and deleted when the VM is destroyed.
	memFileToUse := meta.MemFilePath
	var mergedMemPath string
	if meta.SnapshotType == "diff" {
		cp.snapshotsMu.RLock()
		base, baseOK := cp.snapshots[meta.BaseSnapshotID]
		cp.snapshotsMu.RUnlock()
		if !baseOK {
			cp.restoreMu.Unlock()
			cp.teardownRestoreDMSnapshot(dmInfo)
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			writeRestoreError(w, http.StatusConflict, "diff_base_missing", snapID, fmt.Sprintf("base snapshot %s not found (was it deleted?)", meta.BaseSnapshotID))
			return
		}
		mergedMemPath = filepath.Join(cp.workDir, "tmp", newVMID+"-merged.bin")
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		log.Printf("Restore [%s]: merging base memory (%s) + diff (%s)...", snapID, base.MemFilePath, meta.MemFilePath)
		if err := storage.MergeMemoryDiff(base.MemFilePath, meta.MemFilePath, mergedMemPath); err != nil {
			cp.restoreMu.Unlock()
			cp.teardownRestoreDMSnapshot(dmInfo)
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			writeRestoreError(w, http.StatusInternalServerError, "memory_merge_failed", snapID, fmt.Sprintf("failed to merge diff snapshot: %v", err))
			return
		}
		memFileToUse = mergedMemPath
	}

	if err := cp.applyEgressPolicy(newVMID, tapDevice, newGuestIP, restoreEgressPolicy, meta.Profile); err != nil {
		cp.restoreMu.Unlock()
		cp.teardownRestoreDMSnapshot(dmInfo)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "egress_policy_failed", snapID, fmt.Sprintf("egress policy failed: %v", err))
		return
	}

	log.Printf("Restore [%s]: starting VM [%s] from snapshot (%s)...", snapID, newVMID, meta.SnapshotType)
	machine, err := cp.restoreSnapshotMachine(context.Background(), vm.VMConfig{
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
		cp.cleanupEgressPolicy(newVMID)
		cp.teardownRestoreDMSnapshot(dmInfo)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "firecracker_restore_failed", snapID, fmt.Sprintf("failed to restore VM: %v", err))
		return
	}

	// Firecracker has restored vsock at meta.VsockPath. Reconfigure the guest's IP.
	log.Printf("Restore [%s]: reconfiguring guest IP %s → %s via vsock %s...", snapID, meta.GuestIP, newGuestIP, meta.VsockPath)
	if err := vm.ReconfigureGuestIP(meta.VsockPath, newGuestIP+"/24", "10.0.1.1"); err != nil {
		log.Printf("Restore [%s]: vsock IP reconfigure failed: %v", snapID, err)
		machine.StopVMM()
		cp.cleanupEgressPolicy(newVMID)
		cp.teardownRestoreDMSnapshot(dmInfo)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "guest_reconfigure_failed", snapID, fmt.Sprintf("vsock IP reconfigure failed: %v", err))
		return
	}
	log.Printf("Restore [%s]: guest IP reconfigured to %s (COW exception store: %s)", snapID, newGuestIP, exceptionStorePath)

	info := VMInfo{
		VMID:         newVMID,
		GuestIP:      newGuestIP,
		AgentURL:     buildAgentURL(newVMID, newGuestIP),
		Profile:      meta.Profile,
		TenantID:     restoreTenantID,
		EgressPolicy: restoreEgressPolicy,
	}

	cp.mu.Lock()
	cp.vms[newVMID] = &runningVM{
		VMInfo:     info,
		agentToken: meta.AgentToken,
		startedAt:  time.Now().UTC(),
		diskPath:   exceptionStorePath, // only the COW store needs cleanup (not a full disk copy)
		dmSnapshot: dmInfo,
		vsockPath:  meta.VsockPath,
		machine:    machine,
		tapDevice:  tapDevice,
		socketPath: socketPath,
	}
	cp.mu.Unlock()

	log.Printf("Restore [%s]: waiting for goose-agent at %s...", snapID, info.AgentURL)
	if err := waitForAgent(newGuestIP, 30*time.Second); err != nil {
		cp.destroyVM(newVMID)
		writeRestoreError(w, http.StatusInternalServerError, "agent_not_ready", snapID, fmt.Sprintf("goose-agent not ready after restore: %v", err))
		return
	}
	log.Printf("Restore [%s]: VM [%s] ready — agent: %s", snapID, newVMID, info.AgentURL)

	cp.metrics.IncVMRestore()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMRestoreResult{
		VMInfo:           info,
		SourceSnapshotID: snapID,
	})
}

// restoreLegacyBindMount handles the fallback path when dm-snapshot is unavailable.
// It uses the original bind-mount approach (full 700 MB copy per restore).
func (cp *ControlPlane) restoreLegacyBindMount(
	w http.ResponseWriter,
	snapID string, meta storage.SnapshotMetadata,
	newVMID, newDiskPath, tapDevice, newGuestIP, socketPath string,
	restoreTenantID, restoreEgressPolicy string,
) {
	// Diff memory merge if needed.
	memFileToUse := meta.MemFilePath
	var mergedMemPath string
	if meta.SnapshotType == "diff" {
		cp.snapshotsMu.RLock()
		base, baseOK := cp.snapshots[meta.BaseSnapshotID]
		cp.snapshotsMu.RUnlock()
		if !baseOK {
			storage.TeardownBindMount(meta.DiskPath, newDiskPath)
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			writeRestoreError(w, http.StatusConflict, "diff_base_missing", snapID, fmt.Sprintf("base snapshot %s not found", meta.BaseSnapshotID))
			return
		}
		mergedMemPath = filepath.Join(cp.workDir, "tmp", newVMID+"-merged.bin")
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		if err := storage.MergeMemoryDiff(base.MemFilePath, meta.MemFilePath, mergedMemPath); err != nil {
			storage.TeardownBindMount(meta.DiskPath, newDiskPath)
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			writeRestoreError(w, http.StatusInternalServerError, "memory_merge_failed", snapID, fmt.Sprintf("failed to merge diff: %v", err))
			return
		}
		memFileToUse = mergedMemPath
	}

	if err := cp.applyEgressPolicy(newVMID, tapDevice, newGuestIP, restoreEgressPolicy, meta.Profile); err != nil {
		storage.TeardownBindMount(meta.DiskPath, newDiskPath)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "egress_policy_failed", snapID, fmt.Sprintf("egress policy failed: %v", err))
		return
	}

	machine, err := cp.restoreSnapshotMachine(context.Background(), vm.VMConfig{
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
		cp.cleanupEgressPolicy(newVMID)
		storage.TeardownBindMount(meta.DiskPath, newDiskPath)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "firecracker_restore_failed", snapID, fmt.Sprintf("failed to restore VM: %v", err))
		return
	}

	if err := vm.ReconfigureGuestIP(meta.VsockPath, newGuestIP+"/24", "10.0.1.1"); err != nil {
		machine.StopVMM()
		cp.cleanupEgressPolicy(newVMID)
		storage.TeardownBindMount(meta.DiskPath, newDiskPath)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "guest_reconfigure_failed", snapID, fmt.Sprintf("vsock IP reconfigure failed: %v", err))
		return
	}

	info := VMInfo{
		VMID:         newVMID,
		GuestIP:      newGuestIP,
		AgentURL:     buildAgentURL(newVMID, newGuestIP),
		Profile:      meta.Profile,
		TenantID:     restoreTenantID,
		EgressPolicy: restoreEgressPolicy,
	}
	cp.mu.Lock()
	cp.vms[newVMID] = &runningVM{
		VMInfo:          info,
		agentToken:      meta.AgentToken,
		startedAt:       time.Now().UTC(),
		diskPath:        newDiskPath,
		bindMountTarget: meta.DiskPath,
		vsockPath:       meta.VsockPath,
		machine:         machine,
		tapDevice:       tapDevice,
		socketPath:      socketPath,
	}
	cp.mu.Unlock()

	if err := waitForAgent(newGuestIP, 30*time.Second); err != nil {
		cp.destroyVM(newVMID)
		writeRestoreError(w, http.StatusInternalServerError, "agent_not_ready", snapID, fmt.Sprintf("goose-agent not ready: %v", err))
		return
	}

	cp.metrics.IncVMRestore()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMRestoreResult{
		VMInfo:           info,
		SourceSnapshotID: snapID,
	})
}

func (cp *ControlPlane) deleteSnapshotByID(snapID string) (storage.SnapshotMetadata, int, error) {
	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()

	cp.snapshotsMu.RLock()
	for id, snap := range cp.snapshots {
		if snap.BaseSnapshotID == snapID {
			cp.snapshotsMu.RUnlock()
			return storage.SnapshotMetadata{}, http.StatusConflict, fmt.Errorf("cannot delete: snapshot %s is the base for diff snapshot %s — delete the diff first", snapID, id)
		}
	}
	meta, ok := cp.snapshots[snapID]
	cp.snapshotsMu.RUnlock()
	if !ok {
		return storage.SnapshotMetadata{}, http.StatusNotFound, fmt.Errorf("snapshot not found")
	}

	snapDir := storage.SnapshotDir(cp.workDir, snapID)
	if err := storage.DeleteSnapshot(snapDir); err != nil {
		log.Printf("Warning: failed to delete snapshot dir %s: %v", snapDir, err)
		return meta, http.StatusInternalServerError, fmt.Errorf("failed to delete snapshot %s", snapID)
	}

	cp.snapshotsMu.Lock()
	delete(cp.snapshots, snapID)
	cp.snapshotsMu.Unlock()
	cp.metrics.IncSnapshotDelete()
	return meta, http.StatusOK, nil
}

// DELETE /snapshots/{snapshot_id}
func (cp *ControlPlane) deleteSnapshot(w http.ResponseWriter, snapID string) {
	defer cp.observeLifecycle("snapshot_delete")()
	meta, status, err := cp.deleteSnapshotByID(snapID)
	if err != nil {
		if status == http.StatusNotFound {
			http.Error(w, `{"error":"snapshot not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), status)
		return
	}

	log.Printf("Snapshot [%s] (%s, from VM %s) deleted.", snapID, meta.SnapshotType, meta.SourceVMID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "snapshot_id": snapID})
}
