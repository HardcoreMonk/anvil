package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"

	"ephemera/internal/storage"
	"ephemera/internal/vm"
)

// fakeGuestVsock stands in for a VM's host-side Firecracker vsock UDS. It speaks
// the same two-line protocol vsockSendOnce expects (CONNECT <port> -> "OK <port>",
// then <command> -> "OK") and records every command it was asked to run, so a test
// can assert which VMs the SIGHUP fan-out actually touched.
type fakeGuestVsock struct {
	path string
	mu   sync.Mutex
	cmds []string
}

func (g *fakeGuestVsock) commands() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.cmds...)
}

func startFakeGuestVsock(t *testing.T, dir, name string) *fakeGuestVsock {
	t.Helper()
	// Deliberately NOT t.TempDir(): a unix socket path is capped at ~108 bytes and
	// t.TempDir() embeds the (long) test name.
	path := filepath.Join(dir, name+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })

	g := &fakeGuestVsock{path: path}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(c)
				if _, err := r.ReadString('\n'); err != nil { // CONNECT <port>
					return
				}
				io.WriteString(c, "OK 1234\n")
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				g.mu.Lock()
				g.cmds = append(g.cmds, strings.TrimSpace(line))
				g.mu.Unlock()
				// Reply only after recording, so a caller that has seen its reply
				// (i.e. propagateCPTokenToVMs after wg.Wait) observes the record.
				io.WriteString(c, "OK\n")
			}(conn)
		}
	}()
	return g
}

func vsockTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cptok")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestPropagateCPTokenToVMs_OnlyDaemonInjectedTokens pins the SIGHUP fan-out
// scope: the operator bearer is re-injected ONLY into guests whose CP token this
// daemon put there in the first place (cpTokenManaged). Two guests must be left
// alone:
//
//   - a plain POST /vms VM, which is provisioned with NO /root/.ephemera-cp-token
//     at all (see storage.InjectVMFiles + TestInjectVMFiles_EmptyControlPlaneToken_SkipsFile).
//     Handing it the operator bearer would grant a workload VM full control-plane
//     access it was never meant to have.
//   - a routed-flock member, whose token is a caller-supplied, per-flock SCOPED
//     relay token (internal/anvilmcp/routed_flock.go). Overwriting it both escalates
//     the guest's privileges and breaks the guest's relay hop to its home daemon.
func TestPropagateCPTokenToVMs_OnlyDaemonInjectedTokens(t *testing.T) {
	cp := newTestCP(t)
	dir := vsockTempDir(t)

	plain := startFakeGuestVsock(t, dir, "plain")
	routed := startFakeGuestVsock(t, dir, "routed")
	local := startFakeGuestVsock(t, dir, "local")

	// A plain VM: no CP token was ever injected.
	cp.vms["vm-plain"] = &runningVM{
		VMInfo:    VMInfo{VMID: "vm-plain", Profile: "researcher"},
		vsockPath: plain.path,
	}
	// A routed-flock member: holds a caller-supplied scoped relay token, not ours.
	cp.vms["vm-routed"] = &runningVM{
		VMInfo:    VMInfo{VMID: "vm-routed", Profile: "researcher"},
		vsockPath: routed.path,
	}
	// A local flock member spawned BEFORE capability tokens: the daemon injected
	// its own operator bearer here, so it stays eligible for rotation.
	cp.vms["vm-local"] = &runningVM{
		VMInfo:         VMInfo{VMID: "vm-local", Profile: "researcher"},
		vsockPath:      local.path,
		cpTokenManaged: true,
	}

	cp.propagateCPTokenToVMs([]APIClient{{Name: "ops", Token: "operator-bearer-secret"}})

	if got := local.commands(); len(got) != 1 || got[0] != "SET_CP_TOKEN operator-bearer-secret" {
		t.Fatalf("local flock VM commands = %v, want exactly [SET_CP_TOKEN operator-bearer-secret]", got)
	}
	if got := plain.commands(); len(got) != 0 {
		t.Fatalf("plain VM received %v; a VM with no daemon-injected CP token must never be handed the operator bearer", got)
	}
	if got := routed.commands(); len(got) != 0 {
		t.Fatalf("routed-flock VM received %v; its scoped relay token must not be overwritten with the operator bearer", got)
	}
}

// TestPropagateCPTokenToVMs_SkipsManagedVMWithoutVsock keeps the pre-existing
// vsockPath guard intact alongside the new managed filter.
func TestPropagateCPTokenToVMs_SkipsManagedVMWithoutVsock(t *testing.T) {
	cp := newTestCP(t)
	cp.vms["vm-novsock"] = &runningVM{
		VMInfo:         VMInfo{VMID: "vm-novsock"},
		cpTokenManaged: true,
	}
	// No listener exists; if the guard regressed this would burn the 20x200ms
	// retry budget instead of returning immediately.
	start := time.Now()
	cp.propagateCPTokenToVMs([]APIClient{{Name: "ops", Token: "operator-bearer-secret"}})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("propagate took %v; a VM without a vsock path must be skipped, not dialled", elapsed)
	}
}

// spawnHarness wires every ControlPlane seam spawnVMInternal touches so a spawn
// can run to completion (registration + state persistence) without Firecracker.
func spawnHarness(t *testing.T, cp *ControlPlane) {
	t.Helper()
	workspace := t.TempDir()
	golden := filepath.Join(workspace, "golden.ext4")
	if err := os.WriteFile(golden, []byte("golden"), 0600); err != nil {
		t.Fatalf("write golden image: %v", err)
	}
	cp.provisioner = &storage.Provisioner{GoldenImagePath: golden, WorkspaceDir: workspace}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	guestIP, port := splitHostPort(t, srv.URL)
	prev := agentPort
	agentPort = port
	t.Cleanup(func() { agentPort = prev })

	cp.allocateNetwork = func() (string, string, string, error) {
		return "tap-test", guestIP, "AA:FC:00:00:00:4D", nil
	}
	cp.releaseVMNetwork = func(tapDevice, ip string) error { return nil }
	cp.cloneDisk = func(vmID string) (string, error) {
		return filepath.Join(workspace, vmID+".ext4"), nil
	}
	cp.prepareVM = func(vmID string, opts storage.VMPrepareOptions) error { return nil }
	cp.startMachine = func(ctx context.Context, cfg vm.VMConfig) (*firecracker.Machine, error) {
		return nil, nil
	}
}

// TestSpawnMarksCPTokenManagedOnlyForDaemonInjectedToken pins the provenance
// record at its source: a VM may be marked as carrying the daemon's own operator
// bearer only if that is in fact what it was injected. Since local flock members
// moved to per-flock guest capability tokens, NO spawn path injects the operator
// bearer any more, so no spawn path may set the flag — the SIGHUP rotation set is
// now fed exclusively by recovery, from VMs spawned before that change (see
// TestSIGHUP_StillRotatesPreExistingManagedVM), and drains as they are replaced.
func TestSpawnMarksCPTokenManagedOnlyForDaemonInjectedToken(t *testing.T) {
	cp := newTestCP(t)
	cp.clients = []APIClient{{Name: "ops", Token: "operator-bearer-secret"}}
	spawnHarness(t, cp)

	// 1) Local flock member: injected a per-flock capability token, which is not
	// the daemon's bearer and therefore not the daemon's to rotate. Marking it
	// managed would let the next SIGHUP replace the scoped token with the broad
	// one and undo the privilege reduction.
	cp.ensureLocalFlockGuestToken("flock-1")
	info, _, err := cp.spawnVMForFlock("flock-1", "researcher-1", "researcher", "", "")
	if err != nil {
		t.Fatalf("spawnVMForFlock: %v", err)
	}
	cp.mu.RLock()
	managed := cp.vms[info.VMID].cpTokenManaged
	cp.mu.RUnlock()
	if managed {
		t.Fatalf("local flock VM %s cpTokenManaged = true; a per-flock capability token is not the operator bearer", info.VMID)
	}
	st, err := storage.LoadVMState(cp.workDir, info.VMID)
	if err != nil {
		t.Fatalf("LoadVMState(%s): %v", info.VMID, err)
	}
	if st.CPTokenManaged {
		t.Fatalf("persisted VMState.CPTokenManaged = true for a capability-token flock VM, want false")
	}

	// 2) Routed flock member: caller supplies a scoped relay token.
	routedID := spawnViaHTTP(t, cp, `{"profile":"researcher","flock_id":"routed-1","agent_id":"researcher-1","control_plane_token":"scoped-relay-token"}`)
	cp.mu.RLock()
	managed = cp.vms[routedID].cpTokenManaged
	cp.mu.RUnlock()
	if managed {
		t.Fatalf("routed flock VM %s cpTokenManaged = true; a caller-supplied token is not the daemon's to rotate", routedID)
	}

	// 3) Plain VM: no CP token at all.
	plainID := spawnViaHTTP(t, cp, `{"profile":"researcher"}`)
	cp.mu.RLock()
	managed = cp.vms[plainID].cpTokenManaged
	cp.mu.RUnlock()
	if managed {
		t.Fatalf("plain VM %s cpTokenManaged = true; no CP token was ever injected there", plainID)
	}
	st, err = storage.LoadVMState(cp.workDir, plainID)
	if err != nil {
		t.Fatalf("LoadVMState(%s): %v", plainID, err)
	}
	if st.CPTokenManaged {
		t.Fatalf("persisted VMState.CPTokenManaged = true for a plain VM, want false")
	}
}

func spawnViaHTTP(t *testing.T, cp *ControlPlane, body string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(body))
	cp.spawnVM(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /vms status = %d, body = %q", rr.Code, rr.Body.String())
	}
	var out VMSpawnResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode spawn result: %v", err)
	}
	return out.VMID
}

// TestRegisterRecoveredVMRestoresCPTokenManaged closes the restart hole: without
// this the flag would default to false for every recovered VM and SIGHUP rotation
// would silently stop working for local flock members after a daemon restart.
func TestRegisterRecoveredVMRestoresCPTokenManaged(t *testing.T) {
	cp := newTestCP(t)

	cp.registerRecoveredVM(storage.VMState{
		VMID:           "vm-managed",
		GuestIP:        "10.0.1.5",
		AgentURL:       "http://10.0.1.5:8080",
		CPTokenManaged: true,
	}, nil, nil)
	cp.registerRecoveredVM(storage.VMState{
		VMID:     "vm-unmanaged",
		GuestIP:  "10.0.1.6",
		AgentURL: "http://10.0.1.6:8080",
	}, nil, nil)

	cp.mu.RLock()
	defer cp.mu.RUnlock()
	if !cp.vms["vm-managed"].cpTokenManaged {
		t.Fatalf("recovered VM lost cpTokenManaged=true; SIGHUP rotation would stop after a daemon restart")
	}
	if cp.vms["vm-unmanaged"].cpTokenManaged {
		t.Fatalf("recovered VM gained cpTokenManaged=true it never had")
	}
}
