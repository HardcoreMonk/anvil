package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

	"ephemera/internal/network"
	"ephemera/internal/storage"
	"ephemera/internal/vm"
)

// authMiddleware enforces per-client Bearer token authentication on all requests.
// If clients is empty, every request is allowed (auth disabled).
// Each client has its own named token; the matched client name is logged per request
// for auditing and to allow individual token revocation without affecting others.
//
// Timing-safe design: subtle.ConstantTimeCompare always inspects every byte of both
// operands before returning, so response time does not vary with how many leading
// characters match. All registered tokens are compared on every request (no
// early-exit after the first match) to prevent leaking which client index was hit.
// While network jitter already dwarfs the ns-level timing signal, using constant-time
// comparison is the professionally correct approach for any authentication path.
func authMiddleware(clients []APIClient, next http.Handler) http.Handler {
	if len(clients) == 0 {
		return next // auth disabled
	}

	// Pre-compute []byte bearer strings once so each request avoids allocation.
	type clientBearer struct {
		name   string
		bearer []byte
	}
	bearers := make([]clientBearer, len(clients))
	for i, c := range clients {
		bearers[i] = clientBearer{
			name:   c.Name,
			bearer: []byte("Bearer " + c.Token),
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := []byte(r.Header.Get("Authorization"))

		// Compare against every registered token without short-circuiting.
		// matchedClient is written on a match but iteration always completes.
		matchedClient := ""
		for _, cb := range bearers {
			if subtle.ConstantTimeCompare(auth, cb.bearer) == 1 {
				matchedClient = cb.name
			}
		}

		if matchedClient == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ephemera"`)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		log.Printf("[%s] %s %s", matchedClient, r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// VMInfo is returned to the caller when a VM is created.
// AgentURL is the VM's private IP — the caller uses it directly for task communication.
type VMInfo struct {
	VMID     string `json:"vm_id"`
	GuestIP  string `json:"guest_ip"`
	AgentURL string `json:"agent_url"` // http://{private-ip}:8080
}

type runningVM struct {
	VMInfo
	machine    *firecracker.Machine
	tapDevice  string
	socketPath string
}

// ControlPlane manages the MicroVM lifecycle.
// Task submission goes directly to each VM's goose-agent at its private IP;
// this API only handles VM creation and destruction.
type ControlPlane struct {
	mu sync.RWMutex
	vms map[string]*runningVM

	provisioner      *storage.Provisioner
	netManager       *network.Manager
	kernelPath       string
	firecrackerPath  string
	gooseConfigPath  string
	gooseSecretsPath string
	workDir          string

	stopCh chan struct{}
	srv    *http.Server
}

func NewControlPlane(
	provisioner *storage.Provisioner,
	netManager *network.Manager,
	kernelPath, firecrackerPath, gooseConfigPath, gooseSecretsPath, workDir string,
) *ControlPlane {
	cp := &ControlPlane{
		vms:              make(map[string]*runningVM),
		provisioner:      provisioner,
		netManager:       netManager,
		kernelPath:       kernelPath,
		firecrackerPath:  firecrackerPath,
		gooseConfigPath:  gooseConfigPath,
		gooseSecretsPath: gooseSecretsPath,
		workDir:          workDir,
		stopCh:           make(chan struct{}, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/vms", cp.handleVMs)
	mux.HandleFunc("/vms/", cp.handleVM)
	cp.srv = &http.Server{Addr: apiAddr, Handler: authMiddleware(apiClients, mux)}
	return cp
}

func (cp *ControlPlane) Start() error {
	auth := "disabled"
	if len(apiClients) > 0 {
		names := make([]string, len(apiClients))
		for i, c := range apiClients {
			names[i] = c.Name
		}
		auth = fmt.Sprintf("Bearer token (%d client(s): %s)", len(apiClients), strings.Join(names, ", "))
	}
	log.Printf("Control plane API on %s  (auth: %s)", apiAddr, auth)
	log.Printf("  POST   /vms          — spawn VM  → returns {vm_id, guest_ip, agent_url}")
	log.Printf("  GET    /vms          — list VMs")
	log.Printf("  DELETE /vms/{vm_id}  — stop VM")
	log.Printf("  Clients talk directly to each VM: http://<guest_ip>:%d/tasks", agentPort)
	return cp.srv.ListenAndServe()
}

func (cp *ControlPlane) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cp.srv.Shutdown(ctx)
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

// DELETE /vms/{vm_id} → stop VM
func (cp *ControlPlane) handleVM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	vmID := strings.TrimPrefix(r.URL.Path, "/vms/")
	if vmID == "" {
		http.Error(w, "vm_id required", http.StatusBadRequest)
		return
	}
	cp.stopVM(w, vmID)
}

func (cp *ControlPlane) spawnVM(w http.ResponseWriter, r *http.Request) {
	vmID := fmt.Sprintf("vm-%d", time.Now().UnixMilli())

	tapDevice, guestIP, macAddr, err := cp.netManager.Allocate()
	if err != nil {
		http.Error(w, fmt.Sprintf("network allocation failed: %v", err), http.StatusInternalServerError)
		return
	}

	diskPath, err := cp.provisioner.CloneDisk(vmID)
	if err != nil {
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("disk provisioning failed: %v", err), http.StatusInternalServerError)
		return
	}

	if err := cp.provisioner.PrepareVM(vmID, cp.gooseConfigPath, cp.gooseSecretsPath, ""); err != nil {
		cp.provisioner.CleanupDisk(vmID)
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("VM preparation failed: %v", err), http.StatusInternalServerError)
		return
	}

	socketPath := fmt.Sprintf("/tmp/firecracker-%s.sock", vmID)
	os.Remove(socketPath) // clean up stale socket from a previous run

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
	})
	if err != nil {
		cp.provisioner.CleanupDisk(vmID)
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("VM start failed: %v", err), http.StatusInternalServerError)
		return
	}

	info := VMInfo{
		VMID:     vmID,
		GuestIP:  guestIP,
		AgentURL: fmt.Sprintf("http://%s:%d", guestIP, agentPort),
	}

	cp.mu.Lock()
	cp.vms[vmID] = &runningVM{VMInfo: info, machine: machine, tapDevice: tapDevice, socketPath: socketPath}
	cp.mu.Unlock()

	log.Printf("VM [%s] booting at %s — waiting for goose-agent...", vmID, info.AgentURL)
	if err := waitForAgent(guestIP, 60*time.Second); err != nil {
		cp.destroyVM(vmID)
		http.Error(w, fmt.Sprintf("goose-agent not ready: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("VM [%s] ready — agent: %s", vmID, info.AgentURL)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(info)
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
	// cancel() is not called separately — StopVMM handles the shutdown;
	// calling both would send SIGTERM twice.
	v.machine.StopVMM()
	os.Remove(v.socketPath)
	os.Remove(fmt.Sprintf("/tmp/fc-%s-log.fifo", vmID))
	cp.provisioner.CleanupDisk(vmID)
	cp.netManager.Release(v.tapDevice, v.GuestIP)
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
