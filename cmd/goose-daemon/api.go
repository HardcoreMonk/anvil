package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	ops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"

	"ephemera/internal/anvilmcp"
	"ephemera/internal/metrics"
	"ephemera/internal/network"
	"ephemera/internal/orchestrator"
	"ephemera/internal/storage"
	"ephemera/internal/vm"
)

// ===== 학습 노트 (anvil v0.5.x 학습용 주석, 참고 전용 브랜치) =====
// 이 파일은 control plane 전체(VM 라이프사이클, 이중 mux 배선, spawn/destroy)를 담는
// 큰 파일이라, 이 주석 세트는 v0.5.x가 건드린 세 표면에만 집중한다: (1) NewControlPlane의
// internalMux/externalMux 이중 mux 배선(아래에서 설명), (2) spawnVMOptions.VcpuCount/
// MemSizeMib를 따라가는 per-VM sizing 흐름(v0.5.3), (3) graceful VM delete
// (destroyVM → gracefulAgentStop, v0.5.0).
//
// 이중 mux 패턴(auth 경계): internalMux는 /vms, /config/*, /audit 등 "데이터" 라우트를
// 모두 등록하고, 이 mux 전체가 auditMiddleware(authMiddleware(...))로 한 번에 감싸져
// apiChain이 된다. externalMux는 실제로 :3000에 바인딩되는 최상위 mux로, "/ui/"(auth
// 밖, ui.go), "/metrics"(기본 auth 밖, EPHEMERA_METRICS_REQUIRE_AUTH로 토글), "/"
// (rootRedirectOr가 apiChain으로 위임)만 등록한다. 즉 auth를 안 거치는 경로는 코드
// 전체에서 딱 이 externalMux 등록 블록 하나로 한정된다 — 새 데이터 라우트를 추가할 땐
// internalMux에 등록해야 authMiddleware를 자동으로 상속받는다.
//
// per-VM sizing 흐름: spawnVMOptions.VcpuCount/MemSizeMib(0=기본값)가 spawnVMInternal을
// 거쳐 vm.VMConfig로 흘러가고, runningVM.vcpuCount/memSizeMib에 기록되어 snapshot
// metadata에도 남는다(레거시 snapshot은 0 → restore 시 2/2048 fallback). spawnVM
// 핸들러가 LookupProfile 기본값 위에 프로필의 goose.yaml 값을 덮어써 override하는
// 반면, flock 멤버 spawn 경로는 이 override 없이 LookupProfile 기본값만 쓴다 —
// config.go 학습 노트의 sizing 갭 설명과 같은 비대칭.
//
// graceful delete(v0.5.0): destroyVM → gracefulAgentStop이 먼저 in-VM agent에
// POST /stop(2s 데드라인, best-effort)을 보내 guest를 깨끗이 종료시키고, 그 다음
// machine.StopVMM()으로 Firecracker를 강제 종료한다. 구버전 UI의 "stop agent"
// 액션(에이전트=guest init 프로세스라 사실상 guest 전체를 죽이면서도 VM은 등록된
// 채로 남아 stats poller가 계속 실패 로그를 뿜던 문제)이 이 버전에서 제거되고
// Delete 하나로 통합됐다.

type authFailureRecorder interface {
	IncAuthFailure()
}

// authMiddleware enforces per-client Bearer token authentication on all requests.
// getClients is called on every request so token changes (via SIGHUP reload) take
// effect immediately without restarting the server or dropping running VMs.
// authTotal records each auth decision (outcome=ok|denied|expired); it may be nil
// in tests, in which case the metric is skipped.
//
// If getClients returns an empty slice, every request is allowed (auth disabled).
//
// Timing-safe design: for equal-length operands, subtle.ConstantTimeCompare
// inspects every byte before returning, so response time does not vary with how
// many leading characters match. All registered tokens are compared on every
// request (no early-exit after the first match) to avoid leaking which client
// index was hit. The expiry decision (v0.4.1) is made AFTER the full compare loop,
// and an expired match returns the same 401 body as no match.
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
		var matches []APIClient
		for _, c := range clients {
			if subtle.ConstantTimeCompare(auth, []byte("Bearer "+c.Token)) == 1 {
				matches = append(matches, c)
			}
		}

		now := time.Now()
		matchedName := ""
		expiredName := ""
		for _, c := range matches {
			if apiClientExpired(c, now) {
				expiredName = c.Name
				continue
			}
			matchedName = c.Name
		}

		unauthorized := func() {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ephemera"`)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		}

		if matchedName == "" {
			if expiredName != "" {
				// Same 401 body as a non-match (no client-facing distinction); only
				// the server-side log + metric record the expiry.
				countAuth("expired")
				slog.Warn("api request rejected: token expired", "client", expiredName, "method", r.Method, "path", r.URL.Path)
				unauthorized()
				return
			}
			countAuth("denied")
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
	VMID         string `json:"vm_id"`
	GuestIP      string `json:"guest_ip"`
	AgentURL     string `json:"agent_url"` // proxy URL via control plane when EPHEMERA_PUBLIC_URL is set; otherwise http://{private-ip}:8080
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
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
	// sourceSnapshotID is non-empty for snapshot-restored VMs (v0.4.5). It protects
	// the source snapshot from GC while the VM is live, and lets DestroyAll
	// distinguish restored VMs from COW-spawn VMs (re-restore from the source
	// snapshot on recovery rather than re-layering the preserved exception store).
	sourceSnapshotID string
	vsockPath        string // host-side UDS for Firecracker vsock proxy; cleaned up on teardown
	machine          *firecracker.Machine
	tapDevice        string
	socketPath       string
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
//   - Agent proxy:  POST /vms/{vm_id}/tasks, POST /vms/{vm_id}/workloads/run,
//     GET/PUT /vms/{vm_id}/workspace, GET /vms/{vm_id}/health,
//     POST /vms/{vm_id}/stop
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
	// metrics holds the Prometheus registry plus typed collectors used across
	// the control plane. Wired in NewControlPlane after vms/snapshots/flockMgr
	// are constructed because GaugeFunc closures observe those fields.
	metrics       *daemonMetrics
	traceExporter *traceExporter

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

	allocateForRestore func(tapDeviceName, macAddr string) (tapDevice string, guestIP string, err error)
	releaseNetwork     func(tapDevice string, guestIP string) error
	reclaimNetwork     func(tapDevice string, guestIP string, macAddr string) error
	allocateNetwork    func() (tapDevice string, guestIP string, macAddr string, err error)
	releaseVMNetwork   func(tapDevice string, guestIP string) error
	cloneDisk          func(vmID string) (diskPath string, err error)
	prepareVM          func(vmID string, opts storage.VMPrepareOptions) error
	startMachine       func(ctx context.Context, cfg vm.VMConfig) (*firecracker.Machine, error)
	setupDMSnapshot    func(baseDiskPath, exceptionStorePath, mountTargetPath string) (*storage.DMSnapshotInfo, error)
	teardownDMSnapshot func(info *storage.DMSnapshotInfo)
	setupBindMount     func(baseDiskPath, newDiskPath, mountTargetPath string) error
	restoreMachine     func(ctx context.Context, cfg vm.VMConfig, memFilePath, snapshotPath string) (*firecracker.Machine, error)
	setGuestAgentToken func(vsockPath, token string) error
	reconfigureGuestIP func(vsockPath, ipCIDR, gateway string) error
	waitForAgent       func(guestIP string, timeout time.Duration) error

	// audit is the access-log writer (v0.4.1): a rotated jsonl file under
	// {workDir}/audit/. On by default; nil/disabled is a no-op.
	audit *auditLogger

	// useCOW selects the spawn disk strategy (COW dm-snapshot view vs full byte
	// copy), resolved once at startup. anvil defaults to plain; see resolveDiskModeCOW.
	useCOW bool

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
		flockMgr:         orchestrator.NewFlockManager(workDir),
		agentHTTPClient:  &http.Client{},
		stopCh:           make(chan struct{}, 1),
		useCOW:           resolveDiskModeCOW(storage.DMSnapshotAvailable),
	}
	if err := cp.tenantStore.Load(); err != nil {
		log.Printf("Warning: failed to load tenant store: %v", err)
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

	// [학습] internalMux 등록 블록: 여기 등록되는 모든 라우트(/vms, /config/*, /audit,
	// /snapshots, /watchdog/status 등)는 아래에서 auditMiddleware(authMiddleware(...))로
	// 한 번에 감싸진 뒤에만 실제로 서빙된다. 새 데이터 API를 추가할 때 이 mux 대신
	// externalMux에 직접 등록하면 auth를 건너뛰게 되므로 주의.
	// Two-mux pattern: /metrics is exempt from authMiddleware by default
	// (standard Prometheus scrape model). When EPHEMERA_METRICS_REQUIRE_AUTH=true
	// the /metrics handler is wrapped in authMiddleware just like everything else.
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/health", cp.handleHealth)
	internalMux.HandleFunc("/metrics/vms", cp.handleVMMetrics)
	internalMux.HandleFunc("/vms", cp.handleVMs)
	internalMux.HandleFunc("/vms/", cp.handleVM)
	internalMux.HandleFunc("/tenants", cp.handleTenants)
	internalMux.HandleFunc("/tenants/", cp.handleTenantItem)
	internalMux.HandleFunc("/audit/runtime", cp.handleRuntimeAudit)
	internalMux.HandleFunc("/audit/runtime/prune", cp.handleRuntimeAuditPrune)
	internalMux.HandleFunc("/snapshots", cp.handleSnapshots)
	internalMux.HandleFunc("/snapshots/gc", cp.handleSnapshotGC)
	internalMux.HandleFunc("/snapshots/import", cp.handleSnapshotImport)
	internalMux.HandleFunc("/snapshots/", cp.handleSnapshotItem)
	internalMux.HandleFunc("/audit", cp.handleAudit)
	internalMux.HandleFunc("/watchdog/status", cp.handleWatchdogStatus)
	internalMux.HandleFunc("/config/providers", cp.handleConfigProviders)
	internalMux.HandleFunc("/config/presets", cp.handleConfigPresets)
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
	// [학습] externalMux는 실제 net/http 리스너에 바인딩되는 최상위 mux다. 이 지점
	// 이후 세 줄(= /ui/, apiChain 배선)이 이 프로세스에서 "auth를 안 거치는 경로가
	// 정확히 무엇인가"를 전부 결정한다 — /ui/(정적+로그인), 그리고 조건부로 /metrics뿐.
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
		"  GET    /health                          — daemon health\n" +
		"  GET    /metrics                         — Prometheus exposition (auth optional)\n" +
		"  GET    /metrics/vms                     — legacy per-VM metadata metrics\n" +
		"  POST   /vms                              — spawn VM\n" +
		"  GET    /vms                              — list VMs (?stats=true for inline per-VM stats)\n" +
		"  DELETE /vms/{vm_id}                      — stop VM\n" +
		"  GET    /vms/{vm_id}/stats                — per-VM cpu/mem/net/uptime snapshot\n" +
		"  POST   /vms/{vm_id}/snapshot             — create snapshot\n" +
		"  POST   /vms/{vm_id}/tasks                — proxy: run task on agent\n" +
		"  POST   /vms/{vm_id}/workloads/run        — proxy: run workload script on agent\n" +
		"  GET/PUT /vms/{vm_id}/workspace?path=...  — proxy: workspace file read/write\n" +
		"  GET    /vms/{vm_id}/health               — proxy: agent health check\n" +
		"  POST   /vms/{vm_id}/stop                 — proxy: stop agent\n" +
		"  GET    /vms/{vm_id}/sessions             — proxy: list agent chat sessions\n" +
		"  GET    /tenants                          — list tenants\n" +
		"  GET/PUT /tenants/{tenant_id}             — tenant quota state\n" +
		"  GET    /audit/runtime                    — list runtime audit records\n" +
		"  POST   /audit/runtime/prune              — prune runtime audit records\n" +
		"  GET    /snapshots                        — list snapshots\n" +
		"  POST   /snapshots/gc                     — plan/apply snapshot retention GC\n" +
		"  POST   /snapshots/{snapshot_id}/restore  — restore VM from snapshot\n" +
		"  POST   /snapshots/{snapshot_id}/export   — export snapshot bundle\n" +
		"  POST   /snapshots/import                 — import snapshot bundle\n" +
		"  DELETE /snapshots/{snapshot_id}          — delete snapshot\n" +
		"  POST   /flocks                           — create multi-agent flock\n" +
		"  GET    /flocks                           — list flocks\n" +
		"  GET    /flocks/{flock_id}                — describe flock\n" +
		"  DELETE /flocks/{flock_id}                — destroy flock\n" +
		"  GET    /flocks/{flock_id}/wall           — SSE stream of Town Wall\n" +
		"  GET    /flocks/{flock_id}/wall/history   — active Town Wall log\n" +
		"  POST   /flocks/{flock_id}/post           — post message to Town Wall\n" +
		"  POST   /flocks/{flock_id}/agents         — add one flock agent\n" +
		"  DELETE /flocks/{flock_id}/agents/{id}    — remove one flock agent\n" +
		"  PATCH  /flocks/{flock_id}/agents/{id}    — change one agent role\n" +
		"  POST   /flocks/{flock_id}/agents/{id}/restart — restart one agent in place\n" +
		"  POST   /flocks/{flock_id}/pause          — pause all flock agents\n" +
		"  POST   /flocks/{flock_id}/resume         — resume all flock agents\n" +
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
	return cp.srv.ListenAndServe()
}

func (cp *ControlPlane) Shutdown() {
	// Stop the watchdog BEFORE the HTTP server. The watchdog reads cp.vms
	// (via listVMRefs) on every tick; tearing down the server first leaves
	// in-flight ticks racing against any cp.vms cleanup that follows.
	cp.watchdog.Stop()
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

	if strings.HasSuffix(path, "/workloads/run") {
		vmID := strings.TrimSuffix(path, "/workloads/run")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.proxyAgentEndpoint(w, r, vmID, "/workloads/run")
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

// [학습] VcpuCount/MemSizeMib(0=기본값 사용)가 이 학습 노트가 따라가는 sizing 흐름의
// 입구다. 표준 POST /vms 경로(spawnVM 핸들러)는 이 필드를 채우기 전에 LookupProfile
// 기본값 위에 UI가 저장한 프로필별 override를 얹지만, orchestrator의 flock 멤버 spawn은
// 이 override 단계를 거치지 않는다 — 동일 struct를 공유하면서도 "누가 채우는가"에
// 따라 실제 sizing이 달라지는 지점.
// spawnVMOptions captures all caller-supplied inputs for spawnVMInternal.
// Callers must pre-resolve the goose config paths so this helper does not need
// to know about profileConfigPaths' specific error semantics.
type spawnVMOptions struct {
	Profile      string // recorded in VMInfo.Profile; used only for logs
	ConfigPath   string // resolved goose.yaml host path
	SecretsPath  string // resolved goose-secrets.yaml host path
	TenantID     string // optional tenant attribution for anvil runtime controls
	EgressPolicy string // optional anvil egress policy applied to the VM IP
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

	tapDevice, guestIP, macAddr, err := cp.allocateVMNetwork()
	if err != nil {
		return nil, "", fmt.Errorf("network allocation: %w", err)
	}
	rollback = append(rollback, func() { cp.releaseAllocatedVMNetwork(tapDevice, guestIP) })

	if err := cp.applyEgressPolicy(vmID, tapDevice, guestIP, opts.EgressPolicy, opts.Profile); err != nil {
		return nil, "", fmt.Errorf("egress policy: %w", err)
	}
	rollback = append(rollback, func() { cp.cleanupEgressPolicy(vmID) })

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
		diskPath, err = cp.cloneVMDisk(vmID)
		if err != nil {
			return nil, "", fmt.Errorf("disk provisioning: %w", err)
		}
		rollback = append(rollback, func() { cp.provisioner.CleanupDisk(vmID) })
	}

	if err := cp.prepareVMFiles(vmID, storage.VMPrepareOptions{
		HostConfigPath:    opts.ConfigPath,
		HostSecretsPath:   opts.SecretsPath,
		AgentToken:        agentToken,
		FlockID:           opts.FlockID,
		AgentID:           opts.AgentID,
		SystemPrompt:      opts.SystemPrompt,
		ControlPlaneToken: opts.ControlPlaneToken,
	}); err != nil {
		return nil, "", fmt.Errorf("VM preparation: %w", err)
	}

	socketPath := fmt.Sprintf("/tmp/firecracker-%s.sock", vmID)
	vsockPath := fmt.Sprintf("/tmp/firecracker-vsock-%s.sock", vmID)
	os.Remove(socketPath)

	machine, err := cp.startVMMachine(context.Background(), vm.VMConfig{
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
		VMID:         vmID,
		GuestIP:      guestIP,
		AgentURL:     buildAgentURL(vmID, guestIP),
		Profile:      opts.Profile,
		TenantID:     opts.TenantID,
		EgressPolicy: opts.EgressPolicy,
		Provider:     provider,
		Model:        model,
	}

	// [학습] 여기서 vcpu/memSize에 적용하는 1/1024 fallback은 config.go의
	// LookupProfile이 반환하는 기본값과 반드시 같은 상수여야 한다(주석에 "matches
	// vm.defaultVcpuCount"라고 명시된 이유) — runningVM에 기록되는 실제 sizing이
	// opts.VcpuCount==0일 때도 "알 수 없음"이 아니라 실제 적용된 값이 되도록 한다.
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
		startedAt:  time.Now().UTC(),
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
		VMID:         vmID,
		GuestIP:      guestIP,
		TapDevice:    tapDevice,
		MacAddr:      macAddr,
		VsockPath:    vsockPath,
		SocketPath:   socketPath,
		AgentToken:   agentToken,
		DiskPath:     diskPath,
		DiskMode:     diskMode,
		Profile:      opts.Profile,
		TenantID:     opts.TenantID,
		EgressPolicy: opts.EgressPolicy,
		Provider:     info.Provider,
		Model:        info.Model,
		VcpuCount:    opts.VcpuCount,
		MemSizeMib:   opts.MemSizeMib,
		FlockID:      opts.FlockID,
		AgentID:      opts.AgentID,
		AgentURL:     info.AgentURL,
		CreatedAt:    spawnedAt,
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
	cp.metrics.IncVMCreate()
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
	agentProfile := LookupProfile(req.Profile)
	vcpu, mem := agentProfile.VcpuCount, agentProfile.MemSizeMib
	// [학습] sizing 흐름의 핵심 override 지점: LookupProfile은 항상 1/1024를 주지만,
	// 바로 아래에서 readProfileConfig(config_api.go)로 해당 프로필의 goose.yaml에
	// 실제 EPHEMERA_VCPU_COUNT/EPHEMERA_MEM_SIZE_MIB가 있으면 그 값으로 덮어쓴다.
	// standalone POST /vms만 이 override를 거치고, flock 멤버 spawn(orchestrator
	// 패키지)은 이 단계 없이 LookupProfile 기본값 그대로 spawnVMOptions를 채운다 —
	// 이것이 v0.5.3에서 알려진 채 남은 "flock sizing 갭"의 코드 상 위치다.
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
		Profile:      req.Profile,
		ConfigPath:   configPath,
		SecretsPath:  secretsPath,
		TenantID:     req.TenantID,
		EgressPolicy: req.EgressPolicy,
		VcpuCount:    vcpu,
		MemSizeMib:   mem,
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

// [학습] stopVM(DELETE /vms/{id} 핸들러) → destroyVM → destroyVMUnderSnapshotLock 순으로
// 호출된다. destroyVM 자체는 snapshotLifecycleMu만 잡고 실제 정리는
// destroyVMUnderSnapshotLock에 위임하는데, 이렇게 나뉜 이유는 DestroyAll 등 다른
// 내부 호출자가 이미 락을 쥔 채로 "언더락" 버전을 직접 부를 수 있게 하기 위해서다.
func (cp *ControlPlane) destroyVM(vmID string) {
	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()
	cp.destroyVMUnderSnapshotLock(vmID)
}

func (cp *ControlPlane) destroyVMUnderSnapshotLock(vmID string) {
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
	// [학습] graceful delete(v0.5.0)의 핵심 순서: gracefulAgentStop(아래)이 먼저
	// POST /stop을 보내 guest를 정상 종료시키려 시도하고, 그 성공 여부와 무관하게
	// 바로 다음 줄 v.machine.StopVMM()으로 Firecracker를 강제 종료한다 — "정중하게
	// 부탁하고, 어쨌든 강제로 끝낸다"는 best-effort 2단 구조.
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
			slog.Warn("delete disk failed", "vm_id", vmID, "disk_path", v.diskPath, "err", err)
		}
	}
	cp.netManager.Release(v.tapDevice, v.GuestIP)
	cp.metrics.IncVMDelete()
	slog.Warn("vm destroyed", "vm_id", vmID)
	cp.metrics.vmDestroyTotal.WithLabelValues("ok").Inc()
}

// [학습] v0.5.0에서 새로 생긴 "정상 종료 요청" 단계. 2초 데드라인 안에 응답이 없거나
// 에러가 나도 그냥 무시(Debug 로그만)하고 리턴한다 — 호출자(destroyVMUnderSnapshotLock)가
// 곧바로 Firecracker VMM을 강제 종료하므로 이 함수의 실패가 delete 자체를 막지 않는다.
// 참고: 이 프록시 호출은 cp.agentHTTPClient를 쓰는데, keep-alive pooling이 destroy된
// VM의 재활용된 guest IP로 stale connection을 재사용하는 문제(v0.5.x KVM gate에서
// 드러난 latent defect, DisableKeepAlives로 수정)와 같은 클라이언트를 공유한다.
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
// dm-snapshot restored VMs (v0.4.5) persist a state.json carrying their
// SourceSnapshotID: their transient exception store is dropped here but the
// state.json is kept so RecoverVMs re-restores them fresh from the source
// snapshot on the next start. Legacy bind-mount restores persist no recoverable
// state and are torn down fully — leaving their per-restore disk copies behind
// would leak resources without any benefit.
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
	probeTimeout := 2 * time.Second
	if timeout > 0 && timeout < probeTimeout {
		probeTimeout = timeout
	}
	client := &http.Client{Timeout: probeTimeout}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
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
	var applied []egressCommand
	for _, command := range commands {
		if err := e.command(command.Name, command.Args...); err != nil {
			applyErr := fmt.Errorf("apply egress policy: %w", err)
			if cleanupErr := e.cleanupEgressCommands(applied); cleanupErr != nil {
				return errors.Join(applyErr, fmt.Errorf("rollback egress policy: %w", cleanupErr))
			}
			return applyErr
		}
		applied = append(applied, command)
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
	if len(commands) == 0 {
		commands = []egressCommand{{Name: "iptables", Args: []string{"-I", "FORWARD", "-s", rule.GuestIP, "-j", "REJECT", "-m", "comment", "--comment", rule.Comment}}}
	}
	if err := e.cleanupEgressCommands(commands); err != nil {
		return fmt.Errorf("cleanup egress policy: %w", err)
	}
	return nil
}

func (e *commandEgressEnforcer) cleanupEgressCommands(commands []egressCommand) error {
	commands = append([]egressCommand(nil), commands...)
	for left, right := 0, len(commands)-1; left < right; left, right = left+1, right-1 {
		commands[left], commands[right] = commands[right], commands[left]
	}
	for _, command := range commands {
		args := append([]string(nil), command.Args...)
		if len(args) > 0 && args[0] == "-I" {
			args[0] = "-D"
		}
		if err := e.command(command.Name, args...); err != nil {
			return fmt.Errorf("delete egress command: %w", err)
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
	snapshotGCReasonSourceSnapshot   = "source_snapshot_for_restored_vm"
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

	byID := make(map[string]storage.SnapshotMetadata, len(snapshots))
	for _, meta := range snapshots {
		byID[meta.SnapshotID] = meta
	}
	for snapshotID := range cp.liveRestoredSourceSnapshotRefs() {
		if _, exists := protected[snapshotID]; exists {
			continue
		}
		meta, ok := byID[snapshotID]
		if !ok {
			continue
		}
		protected[snapshotID] = snapshotGCEntryFrom(meta, snapshotGCReasonSourceSnapshot, nil, sizes[snapshotID])
	}
	if stateRefs, err := cp.persistedRestoredSourceSnapshotRefs(); err == nil {
		for snapshotID := range stateRefs {
			if _, exists := protected[snapshotID]; exists {
				continue
			}
			meta, ok := byID[snapshotID]
			if !ok {
				continue
			}
			protected[snapshotID] = snapshotGCEntryFrom(meta, snapshotGCReasonSourceSnapshot, nil, sizes[snapshotID])
		}
	} else {
		slog.Warn("snapshot gc: list vm state failed", "err", err)
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

func (cp *ControlPlane) liveRestoredSourceSnapshotRefs() map[string][]string {
	cp.mu.RLock()
	defer cp.mu.RUnlock()
	refs := make(map[string][]string)
	for vmID, vm := range cp.vms {
		if vm == nil || strings.TrimSpace(vm.sourceSnapshotID) == "" {
			continue
		}
		refs[vm.sourceSnapshotID] = append(refs[vm.sourceSnapshotID], vmID)
	}
	for snapshotID := range refs {
		sort.Strings(refs[snapshotID])
	}
	return refs
}

func (cp *ControlPlane) persistedRestoredSourceSnapshotRefs() (map[string][]string, error) {
	states, err := storage.ListVMState(cp.workDir)
	if err != nil {
		return nil, err
	}
	refs := make(map[string][]string)
	for _, state := range states {
		sourceID := strings.TrimSpace(state.SourceSnapshotID)
		if sourceID == "" {
			continue
		}
		refs[sourceID] = append(refs[sourceID], state.VMID)
	}
	for snapshotID := range refs {
		sort.Strings(refs[snapshotID])
	}
	return refs, nil
}

// ---- Snapshot handlers ----

func writeJSONError(w http.ResponseWriter, status int, message any) {
	text := fmt.Sprint(message)
	if err, ok := message.(error); ok {
		text = err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": text})
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

func (cp *ControlPlane) allocateVMNetwork() (string, string, string, error) {
	if cp.allocateNetwork != nil {
		return cp.allocateNetwork()
	}
	return cp.netManager.Allocate()
}

func (cp *ControlPlane) releaseAllocatedVMNetwork(tapDevice, guestIP string) error {
	if cp.releaseVMNetwork != nil {
		return cp.releaseVMNetwork(tapDevice, guestIP)
	}
	return cp.netManager.Release(tapDevice, guestIP)
}

func (cp *ControlPlane) cloneVMDisk(vmID string) (string, error) {
	if cp.cloneDisk != nil {
		return cp.cloneDisk(vmID)
	}
	return cp.provisioner.CloneDisk(vmID)
}

func (cp *ControlPlane) prepareVMFiles(vmID string, opts storage.VMPrepareOptions) error {
	if cp.prepareVM != nil {
		return cp.prepareVM(vmID, opts)
	}
	return cp.provisioner.PrepareVM(vmID, opts)
}

func (cp *ControlPlane) startVMMachine(ctx context.Context, cfg vm.VMConfig) (*firecracker.Machine, error) {
	if cp.startMachine != nil {
		return cp.startMachine(ctx, cfg)
	}
	return vm.StartMachine(ctx, cfg)
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

func (cp *ControlPlane) reconfigureRestoredGuestIP(vsockPath, ipCIDR, gateway string) error {
	if cp.reconfigureGuestIP != nil {
		return cp.reconfigureGuestIP(vsockPath, ipCIDR, gateway)
	}
	return vm.ReconfigureGuestIP(vsockPath, ipCIDR, gateway)
}

func (cp *ControlPlane) waitForRestoredAgent(guestIP string, timeout time.Duration) error {
	if cp.waitForAgent != nil {
		return cp.waitForAgent(guestIP, timeout)
	}
	return waitForAgent(guestIP, timeout)
}

func (cp *ControlPlane) agentTokenForRestoredSnapshot(snapID string, meta storage.SnapshotMetadata) (string, error) {
	if token := strings.TrimSpace(meta.AgentToken); token != "" {
		return token, nil
	}
	token, err := generateAgentToken()
	if err != nil {
		return "", fmt.Errorf("generate restored agent token: %w", err)
	}
	setGuestAgentToken := vm.SetGuestAgentToken
	if cp.setGuestAgentToken != nil {
		setGuestAgentToken = cp.setGuestAgentToken
	}
	if err := setGuestAgentToken(meta.VsockPath, token); err != nil {
		return "", fmt.Errorf("snapshot %s: %w", snapID, err)
	}
	return token, nil
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
	var resp SnapshotGCResponse
	func() {
		cp.snapshotLifecycleMu.Lock()
		defer cp.snapshotLifecycleMu.Unlock()
		resp = cp.planSnapshotGC(policy, time.Now().UTC())
		resp.Applied = req.Apply
		if req.Apply {
			cp.applySnapshotGC(&resp)
			cp.metrics.IncSnapshotGC()
		}
	}()

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

func (cp *ControlPlane) handleSnapshotExport(w http.ResponseWriter, r *http.Request, snapID string) {
	defer cp.observeLifecycle("snapshot_export")()
	snapID = strings.TrimSpace(snapID)
	if snapID == "" {
		writeJSONError(w, http.StatusBadRequest, "snapshot_id is required")
		return
	}

	tmpDir := filepath.Join(cp.workDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		slog.Warn("snapshot export temp dir failed", "snapshot_id", snapID, "dir", tmpDir, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "snapshot_export_failed")
		return
	}

	var tmpPath string
	cp.snapshotLifecycleMu.Lock()
	cp.snapshotsMu.RLock()
	_, ok := cp.snapshots[snapID]
	cp.snapshotsMu.RUnlock()
	if !ok {
		cp.snapshotLifecycleMu.Unlock()
		writeJSONError(w, http.StatusNotFound, "snapshot_not_found")
		return
	}

	tmp, err := os.CreateTemp(tmpDir, "snapshot-export-*.tar")
	if err != nil {
		cp.snapshotLifecycleMu.Unlock()
		slog.Warn("snapshot export temp file failed", "snapshot_id", snapID, "dir", tmpDir, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "snapshot_export_failed")
		return
	}
	tmpPath = tmp.Name()
	_, exportErr := storage.ExportSnapshotBundle(cp.workDir, snapID, tmp)
	closeErr := tmp.Close()
	cp.snapshotLifecycleMu.Unlock()
	defer os.Remove(tmpPath)

	if exportErr != nil {
		slog.Warn("snapshot export failed", "snapshot_id", snapID, "err", exportErr)
		writeJSONError(w, http.StatusInternalServerError, "snapshot_export_failed")
		return
	}
	if closeErr != nil {
		slog.Warn("snapshot export temp close failed", "snapshot_id", snapID, "path", tmpPath, "err", closeErr)
		writeJSONError(w, http.StatusInternalServerError, "snapshot_export_failed")
		return
	}

	bundle, err := os.Open(tmpPath)
	if err != nil {
		slog.Warn("snapshot export failed", "snapshot_id", snapID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "snapshot_export_failed")
		return
	}
	defer bundle.Close()

	w.Header().Set("Content-Type", storage.SnapshotBundleContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, bundle); err != nil {
		slog.Warn("snapshot export response write failed", "snapshot_id", snapID, "err", err)
	}
}

func (cp *ControlPlane) handleSnapshotImport(w http.ResponseWriter, r *http.Request) {
	defer cp.observeLifecycle("snapshot_import")()
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(ct, storage.SnapshotBundleContentType) {
		writeJSONError(w, http.StatusBadRequest, "invalid snapshot bundle content type")
		return
	}

	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()

	workspaceDir := ""
	if cp.provisioner != nil {
		workspaceDir = cp.provisioner.WorkspaceDir
	}
	result, err := storage.ImportSnapshotBundleWithOptions(cp.workDir, r.Body, storage.SnapshotImportOptions{
		WorkspaceDir: workspaceDir,
	})
	if err != nil {
		status := http.StatusInternalServerError
		msg := "snapshot_import_failed"
		switch {
		case errors.Is(err, storage.ErrSnapshotBundleInvalid):
			status = http.StatusBadRequest
			msg = "invalid_snapshot_bundle"
		case errors.Is(err, storage.ErrDiffBaseMissing):
			status = http.StatusConflict
			msg = "diff_base_missing"
		case errors.Is(err, storage.ErrSnapshotBundleConflict):
			status = http.StatusConflict
			msg = "snapshot_conflict"
		}
		writeJSONError(w, status, msg)
		return
	}

	cp.snapshotsMu.Lock()
	cp.snapshots[result.SnapshotID] = result.Metadata
	cp.snapshotsMu.Unlock()

	status := http.StatusCreated
	if result.Status == storage.SnapshotImportStatusAlreadyPresent {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}

// handleSnapshotItem routes POST /snapshots/{id}/restore, POST /snapshots/{id}/export, and DELETE /snapshots/{id}.
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

	if strings.HasSuffix(path, "/export") {
		snapID := strings.TrimSuffix(path, "/export")
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		cp.handleSnapshotExport(w, r, snapID)
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

	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()

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
		cp.destroyVMUnderSnapshotLock(vmID)
	}

	// Firecracker v1.x embeds the TAP device name AND vsock UDS path in state.bin.
	// On restore, Firecracker reopens both by the exact names/paths from the snapshot.
	meta := storage.SnapshotMetadata{
		SnapshotID:     snapID,
		SourceVMID:     vmID,
		TenantID:       snapshotTenantID,
		Profile:        v.VMInfo.Profile,
		EgressPolicy:   v.VMInfo.EgressPolicy,
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

	cp.metrics.IncSnapshotCreate()
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
	slog.Warn("restore: allocating network", "snapshot_id", snapID, "tap", meta.TapDevice, "mac", meta.MacAddr)
	tapDevice, newGuestIP, err := cp.allocateNetworkForRestore(meta.TapDevice, meta.MacAddr)
	if err != nil {
		writeRestoreError(w, http.StatusConflict, "network_unavailable", snapID, fmt.Sprintf("network allocation failed: %v", err))
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
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			writeRestoreError(w, http.StatusConflict, "diff_base_missing", snapID, fmt.Sprintf("base snapshot %s not found (was it deleted?)", meta.BaseSnapshotID))
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
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			os.Remove(mergedRootfs)
			writeRestoreError(w, http.StatusInternalServerError, "rootfs_merge_failed", snapID, fmt.Sprintf("failed to merge rootfs diff: %v", mErr))
			return
		}
		baseDiskForCOW = mergedRootfs
	}

	// Firecracker's LoadSnapshot opens the disk path baked into state.bin at snapshot
	// time (meta.DiskPath). The source VM was deleted before restore, so that path no
	// longer exists and must be reconstructed here: the restored COW device is
	// bind-mounted over meta.DiskPath. Per-restore isolation lives in the unique
	// exception store (newVMID.cow), never the mount target. Mirrors upstream and
	// reRestoreMachine (recovery.go) — KEEP THE THREE IN SYNC.
	slog.Warn("restore: setting up dm-snapshot cow", "snapshot_id", snapID, "base", baseDiskForCOW, "store", exceptionStorePath)
	dmInfo, err := cp.setupRestoreDMSnapshot(baseDiskForCOW, exceptionStorePath, meta.DiskPath)
	if err != nil {
		cp.restoreMu.Unlock()
		slog.Warn("restore: dm-snapshot failed, falling back to bind mount", "snapshot_id", snapID, "err", err)
		// Fallback: bind-mount the base disk if dm-snapshot is unavailable.
		newDiskPath := filepath.Join(cp.provisioner.WorkspaceDir, newVMID+"-bind.ext4")
		cp.restoreMu.Lock()
		if bmErr := cp.setupRestoreBindMount(baseDiskForCOW, newDiskPath, meta.DiskPath); bmErr != nil {
			cp.restoreMu.Unlock()
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			if mergedRootfs != "" {
				os.Remove(mergedRootfs)
			}
			writeRestoreError(w, http.StatusInternalServerError, "firecracker_restore_failed", snapID, fmt.Sprintf("failed to set up disk: dm-snapshot: %v; bind-mount fallback: %v", err, bmErr))
			return
		}
		cp.restoreMu.Unlock()
		// SetupBindMount copied baseDiskForCOW into newDiskPath; the transient merge is done.
		if mergedRootfs != "" {
			os.Remove(mergedRootfs)
		}
		delegated = true
		cp.restoreLegacyBindMount(w, snapID, meta, newVMID, newDiskPath, meta.DiskPath, tapDevice, newGuestIP, socketPath, restoreTenantID, restoreEgressPolicy)
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
	defer func() {
		if mergedMemPath != "" {
			os.Remove(mergedMemPath)
		}
	}()
	if meta.SnapshotType == "diff" {
		mergedMemPath = pickMergedMemPath(cp.workDir, newVMID)
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		slog.Warn("restore: merging base memory and diff", "snapshot_id", snapID, "base", base.MemFilePath, "diff", meta.MemFilePath)
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

	slog.Warn("restore: starting vm", "snapshot_id", snapID, "vm_id", newVMID, "type", meta.SnapshotType)
	machine, err := cp.restoreSnapshotMachine(context.Background(), vm.VMConfig{
		VMID:           newVMID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		RootfsPath:     meta.DiskPath,
		TapDevice:      tapDevice,
		MacAddress:     meta.MacAddr,
		GuestIP:        newGuestIP,
		GatewayIP:      "10.0.1.1",
		VsockUDSPath:   meta.VsockPath,
	}, memFileToUse, meta.StatFilePath)

	cp.restoreMu.Unlock()

	if err != nil {
		cp.cleanupEgressPolicy(newVMID)
		cp.teardownRestoreDMSnapshot(dmInfo)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "firecracker_restore_failed", snapID, fmt.Sprintf("failed to restore VM: %v", err))
		return
	}

	// Firecracker has restored vsock at meta.VsockPath. Reconfigure the guest's IP.
	slog.Warn("restore: reconfiguring guest ip", "snapshot_id", snapID, "old_ip", meta.GuestIP, "new_ip", newGuestIP, "vsock", meta.VsockPath)
	if err := cp.reconfigureRestoredGuestIP(meta.VsockPath, newGuestIP+"/24", "10.0.1.1"); err != nil {
		slog.Warn("restore: vsock ip reconfigure failed", "snapshot_id", snapID, "err", err)
		machine.StopVMM()
		cp.cleanupEgressPolicy(newVMID)
		cp.teardownRestoreDMSnapshot(dmInfo)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "guest_reconfigure_failed", snapID, fmt.Sprintf("vsock IP reconfigure failed: %v", err))
		return
	}
	slog.Warn("restore: guest ip reconfigured", "snapshot_id", snapID, "ip", newGuestIP, "cow_store", exceptionStorePath)

	restoredAgentToken, err := cp.agentTokenForRestoredSnapshot(snapID, meta)
	if err != nil {
		slog.Warn("restore: agent token update failed", "snapshot_id", snapID, "err", err)
		machine.StopVMM()
		cp.cleanupEgressPolicy(newVMID)
		cp.teardownRestoreDMSnapshot(dmInfo)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "guest_reconfigure_failed", snapID, fmt.Sprintf("vsock agent token update failed: %v", err))
		return
	}

	info := VMInfo{
		VMID:         newVMID,
		GuestIP:      newGuestIP,
		AgentURL:     buildAgentURL(newVMID, newGuestIP),
		Profile:      meta.Profile,
		TenantID:     restoreTenantID,
		EgressPolicy: restoreEgressPolicy,
		Provider:     meta.Provider,
		Model:        meta.Model,
	}

	rvcpu, rmem := snapshotSizing(meta)
	cp.mu.Lock()
	cp.vms[newVMID] = &runningVM{
		VMInfo:           info,
		agentToken:       restoredAgentToken,
		startedAt:        time.Now().UTC(),
		diskPath:         exceptionStorePath, // only the COW store needs cleanup (not a full disk copy)
		dmSnapshot:       dmInfo,
		sourceSnapshotID: snapID,
		vsockPath:        meta.VsockPath,
		machine:          machine,
		tapDevice:        tapDevice,
		socketPath:       socketPath,
		vcpuCount:        rvcpu,
		memSizeMib:       rmem, // from snapshot meta (legacy snapshots → 2048); the mem file governs actual size
		spawnedAt:        time.Now().UTC(),
	}
	cp.mu.Unlock()

	// Persist a recovery record (v0.4.5) so a daemon restart auto-re-restores this
	// VM from snapID instead of dropping it. DiskPath is the transient COW store;
	// the recoverable artifact is the source snapshot (see recoverRestoredVM).
	// AgentToken is meta.AgentToken (the snapshot's baked token): re-restore reloads
	// the snapshot's memory, so the recovered guest carries that token, not any
	// post-restore rotation. TenantID/EgressPolicy carry anvil attribution across
	// the restart. Non-fatal: the VM is already live; a failed write only forfeits
	// auto-recovery.
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
		TenantID:         restoreTenantID,
		EgressPolicy:     restoreEgressPolicy,
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
	// (two 2GB VMs at once) under host memory pressure.
	if err := cp.waitForRestoredAgent(newGuestIP, 60*time.Second); err != nil {
		cp.destroyVMUnderSnapshotLock(newVMID)
		writeRestoreError(w, http.StatusInternalServerError, "agent_not_ready", snapID, fmt.Sprintf("goose-agent not ready after restore: %v", err))
		return
	}
	slog.Warn("restore: vm ready", "snapshot_id", snapID, "vm_id", newVMID, "agent_url", info.AgentURL)

	cp.metrics.IncVMRestore()
	outcome = "ok"
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
	newVMID, newDiskPath, mountTargetPath, tapDevice, newGuestIP, socketPath string,
	restoreTenantID, restoreEgressPolicy string,
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
	defer func() {
		if mergedMemPath != "" {
			os.Remove(mergedMemPath)
		}
	}()
	if meta.SnapshotType == "diff" {
		cp.snapshotsMu.RLock()
		base, baseOK := cp.snapshots[meta.BaseSnapshotID]
		cp.snapshotsMu.RUnlock()
		if !baseOK {
			storage.TeardownBindMount(mountTargetPath, newDiskPath)
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			writeRestoreError(w, http.StatusConflict, "diff_base_missing", snapID, fmt.Sprintf("base snapshot %s not found", meta.BaseSnapshotID))
			return
		}
		mergedMemPath = pickMergedMemPath(cp.workDir, newVMID)
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		if err := storage.MergeMemoryDiff(base.MemFilePath, meta.MemFilePath, mergedMemPath); err != nil {
			storage.TeardownBindMount(mountTargetPath, newDiskPath)
			cp.releaseRestoreNetwork(tapDevice, newGuestIP)
			writeRestoreError(w, http.StatusInternalServerError, "memory_merge_failed", snapID, fmt.Sprintf("failed to merge diff: %v", err))
			return
		}
		memFileToUse = mergedMemPath
	}

	if err := cp.applyEgressPolicy(newVMID, tapDevice, newGuestIP, restoreEgressPolicy, meta.Profile); err != nil {
		storage.TeardownBindMount(mountTargetPath, newDiskPath)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "egress_policy_failed", snapID, fmt.Sprintf("egress policy failed: %v", err))
		return
	}

	machine, err := cp.restoreSnapshotMachine(context.Background(), vm.VMConfig{
		VMID:           newVMID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		RootfsPath:     mountTargetPath,
		TapDevice:      tapDevice,
		MacAddress:     meta.MacAddr,
		GuestIP:        newGuestIP,
		GatewayIP:      "10.0.1.1",
		VsockUDSPath:   meta.VsockPath,
	}, memFileToUse, meta.StatFilePath)
	if err != nil {
		cp.cleanupEgressPolicy(newVMID)
		storage.TeardownBindMount(mountTargetPath, newDiskPath)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "firecracker_restore_failed", snapID, fmt.Sprintf("failed to restore VM: %v", err))
		return
	}

	if err := cp.reconfigureRestoredGuestIP(meta.VsockPath, newGuestIP+"/24", "10.0.1.1"); err != nil {
		machine.StopVMM()
		cp.cleanupEgressPolicy(newVMID)
		storage.TeardownBindMount(mountTargetPath, newDiskPath)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "guest_reconfigure_failed", snapID, fmt.Sprintf("vsock IP reconfigure failed: %v", err))
		return
	}

	restoredAgentToken, err := cp.agentTokenForRestoredSnapshot(snapID, meta)
	if err != nil {
		machine.StopVMM()
		cp.cleanupEgressPolicy(newVMID)
		storage.TeardownBindMount(mountTargetPath, newDiskPath)
		cp.releaseRestoreNetwork(tapDevice, newGuestIP)
		writeRestoreError(w, http.StatusInternalServerError, "guest_reconfigure_failed", snapID, fmt.Sprintf("vsock agent token update failed: %v", err))
		return
	}

	info := VMInfo{
		VMID:         newVMID,
		GuestIP:      newGuestIP,
		AgentURL:     buildAgentURL(newVMID, newGuestIP),
		Profile:      meta.Profile,
		TenantID:     restoreTenantID,
		EgressPolicy: restoreEgressPolicy,
		Provider:     meta.Provider,
		Model:        meta.Model,
	}
	rvcpu, rmem := snapshotSizing(meta)
	cp.mu.Lock()
	cp.vms[newVMID] = &runningVM{
		VMInfo:           info,
		agentToken:       restoredAgentToken,
		startedAt:        time.Now().UTC(),
		diskPath:         newDiskPath,
		bindMountTarget:  mountTargetPath,
		sourceSnapshotID: snapID,
		vsockPath:        meta.VsockPath,
		machine:          machine,
		tapDevice:        tapDevice,
		socketPath:       socketPath,
		vcpuCount:        rvcpu,
		memSizeMib:       rmem,
		spawnedAt:        time.Now().UTC(),
	}
	cp.mu.Unlock()

	// 60s to match the spawn/recovery boot budget. 30s flaked on concurrent restore
	// (two 2GB VMs at once) under host memory pressure.
	if err := cp.waitForRestoredAgent(newGuestIP, 60*time.Second); err != nil {
		cp.destroyVMUnderSnapshotLock(newVMID)
		writeRestoreError(w, http.StatusInternalServerError, "agent_not_ready", snapID, fmt.Sprintf("goose-agent not ready: %v", err))
		return
	}

	cp.metrics.IncVMRestore()
	outcome = "ok"
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMRestoreResult{
		VMInfo:           info,
		SourceSnapshotID: snapID,
	})
}

func (cp *ControlPlane) deleteSnapshotByID(snapID string) (storage.SnapshotMetadata, int, error) {
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
	if vmIDs := cp.liveRestoredSourceSnapshotRefs()[snapID]; len(vmIDs) > 0 {
		return storage.SnapshotMetadata{}, http.StatusConflict, fmt.Errorf("cannot delete: snapshot %s is referenced by restored VM %s", snapID, vmIDs[0])
	}
	stateRefs, err := cp.persistedRestoredSourceSnapshotRefs()
	if err != nil {
		return storage.SnapshotMetadata{}, http.StatusInternalServerError, fmt.Errorf("cannot verify restored VM snapshot dependencies: %w", err)
	}
	if vmIDs := stateRefs[snapID]; len(vmIDs) > 0 {
		return storage.SnapshotMetadata{}, http.StatusConflict, fmt.Errorf("cannot delete: snapshot %s is referenced by restored VM %s", snapID, vmIDs[0])
	}

	snapDir := storage.SnapshotDir(cp.workDir, snapID)
	if err := storage.DeleteSnapshot(snapDir); err != nil {
		slog.Warn("delete snapshot dir failed", "snapshot_id", snapID, "dir", snapDir, "err", err)
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
	cp.snapshotLifecycleMu.Lock()
	defer cp.snapshotLifecycleMu.Unlock()
	meta, status, err := cp.deleteSnapshotByID(snapID)
	if err != nil {
		if status == http.StatusNotFound {
			http.Error(w, `{"error":"snapshot not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), status)
		return
	}

	slog.Warn("snapshot deleted", "snapshot_id", snapID, "type", meta.SnapshotType, "source_vm_id", meta.SourceVMID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "snapshot_id": snapID})
}
