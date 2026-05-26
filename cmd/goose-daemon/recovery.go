package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"ephemera/internal/orchestrator"
	"ephemera/internal/storage"
	"ephemera/internal/vm"
)

// RecoverVMs cold-restarts every previously-spawned VM whose state.json is
// still on disk. Memory is not preserved — this is a fresh boot of the same
// rootfs (a full clone for plain VMs, or a dm-snapshot re-layered over the
// golden image for COW VMs), reusing the same IP, TAP, MAC, and agent token so
// external callers and any flock association stay stable across the restart.
//
// COW VMs (DiskMode=cow) reconstruct their dm-snapshot from the preserved
// exception store; this assumes the golden image is unchanged since spawn.
//
// A VM's state is dropped (and any flock agent marked dead) when:
//   - its disk file is missing (logged only, not counted as a failure)
//   - its network identity can no longer be reclaimed
//   - dm-snapshot setup, Firecracker boot, or the agent handshake fails
//
// Returns the number of VMs brought back and the list of VMIDs that had state
// but could not be restored.
func (cp *ControlPlane) RecoverVMs() (recovered int, failed []string, err error) {
	states, err := storage.ListVMState(cp.workDir)
	if err != nil {
		return 0, nil, err
	}

	// Reclaim COW dm-snapshot + loop devices left behind by a crashed previous
	// run (no surviving state.json). VMs that still have state are preserved for
	// the recovery loop below to handle (v0.4.0 E).
	liveVMIDs := make(map[string]struct{}, len(states))
	for _, s := range states {
		liveVMIDs[s.VMID] = struct{}{}
	}
	if n := storage.RemoveOrphanCOWDevices(cp.provisioner.WorkspaceDir, liveVMIDs); n > 0 {
		slog.Warn("recovery: reclaimed orphan cow devices", "count", n)
	}

	for _, s := range states {
		if _, statErr := os.Stat(s.DiskPath); os.IsNotExist(statErr) {
			slog.Warn("recovery: disk missing, dropping state", "vm_id", s.VMID, "disk_path", s.DiskPath)
			storage.DeleteVMState(cp.workDir, s.VMID)
			continue
		}

		// Clear orphans left by the previous daemon process.
		if killErr := storage.KillStaleFirecracker(s.SocketPath); killErr != nil {
			slog.Warn("recovery: stale firecracker probe failed", "vm_id", s.VMID, "err", killErr)
		}
		logFifoPath := fmt.Sprintf("/tmp/fc-%s-log.fifo", s.VMID)
		storage.RemoveStaleVMArtifacts(s.SocketPath, s.VsockPath, logFifoPath)

		if reErr := cp.netManager.ReclaimAllocation(s.TapDevice, s.GuestIP, s.MacAddr); reErr != nil {
			slog.Warn("recovery: network reclaim failed, dropping state", "vm_id", s.VMID, "err", reErr)
			storage.DeleteVMState(cp.workDir, s.VMID)
			failed = append(failed, s.VMID)
			cp.markFlockAgentDead(s.FlockID, s.AgentID)
			continue
		}

		// COW VMs reconstruct their dm-snapshot before boot by re-layering the
		// preserved exception store over the golden image. A crashed previous run
		// may have left a live "cow-<vm_id>" device or a dangling store loop, so
		// clear them first (keeping the store) to avoid a dmsetup name collision.
		var dmInfo *storage.DMSnapshotInfo
		if s.DiskMode == storage.DiskModeCOW {
			storage.ReclaimCOWDeviceKeepStore(cp.provisioner.WorkspaceDir, s.VMID)
			// The exception store path is not persisted; it is always
			// "<workspace>/<vm_id>.cow" (see CloneDiskCOW). The base is the golden
			// image, assumed unchanged since spawn — a rebuild would invalidate the
			// block-level COW layering.
			exceptionStore := filepath.Join(cp.provisioner.WorkspaceDir, s.VMID+".cow")
			if _, statErr := os.Stat(exceptionStore); os.IsNotExist(statErr) {
				// Store gone but state survived: SetupDMSnapshot truncates a fresh
				// empty store, so the VM boots from the pristine golden image (writes
				// lost). A clean boot is still usable, so warn rather than fail.
				slog.Warn("recovery: cow exception store missing, booting from golden image", "vm_id", s.VMID, "store", exceptionStore)
			}
			dmSnap, setupErr := storage.SetupDMSnapshot(cp.provisioner.GoldenImagePath, exceptionStore, s.DiskPath)
			if setupErr != nil {
				slog.Warn("recovery: cow dm-snapshot setup failed, dropping state", "vm_id", s.VMID, "err", setupErr)
				cp.netManager.Release(s.TapDevice, s.GuestIP)
				storage.DeleteVMState(cp.workDir, s.VMID)
				failed = append(failed, s.VMID)
				cp.markFlockAgentDead(s.FlockID, s.AgentID)
				continue
			}
			dmInfo = dmSnap
		}

		machine, startErr := vm.StartMachine(context.Background(), vm.VMConfig{
			VMID:           s.VMID,
			SocketPath:     s.SocketPath,
			FirecrackerBin: cp.firecrackerPath,
			KernelPath:     cp.kernelPath,
			RootfsPath:     s.DiskPath,
			TapDevice:      s.TapDevice,
			MacAddress:     s.MacAddr,
			GuestIP:        s.GuestIP,
			GatewayIP:      "10.0.1.1",
			VsockUDSPath:   s.VsockPath,
			VcpuCount:      s.VcpuCount,
			MemSizeMib:     s.MemSizeMib,
		})
		if startErr != nil {
			slog.Warn("recovery: firecracker start failed", "vm_id", s.VMID, "err", startErr)
			if dmInfo != nil {
				storage.TeardownDMSnapshot(dmInfo)
			}
			cp.netManager.Release(s.TapDevice, s.GuestIP)
			storage.DeleteVMState(cp.workDir, s.VMID)
			failed = append(failed, s.VMID)
			cp.markFlockAgentDead(s.FlockID, s.AgentID)
			continue
		}

		// 60s matches the spawn-path waitForAgent budget; cold boot of the
		// golden image typically completes in 4–10s.
		if waitErr := waitForAgent(s.GuestIP, 60*time.Second); waitErr != nil {
			slog.Warn("recovery: agent did not respond", "vm_id", s.VMID, "err", waitErr)
			machine.StopVMM()
			if dmInfo != nil {
				storage.TeardownDMSnapshot(dmInfo)
			}
			cp.netManager.Release(s.TapDevice, s.GuestIP)
			storage.DeleteVMState(cp.workDir, s.VMID)
			failed = append(failed, s.VMID)
			cp.markFlockAgentDead(s.FlockID, s.AgentID)
			continue
		}

		info := VMInfo{
			VMID:     s.VMID,
			GuestIP:  s.GuestIP,
			AgentURL: s.AgentURL,
			Profile:  s.Profile,
		}
		memSize := s.MemSizeMib
		if memSize == 0 {
			memSize = 2048
		}
		cp.mu.Lock()
		cp.vms[s.VMID] = &runningVM{
			VMInfo:     info,
			agentToken: s.AgentToken,
			diskPath:   s.DiskPath,
			dmSnapshot: dmInfo,
			vsockPath:  s.VsockPath,
			machine:    machine,
			tapDevice:  s.TapDevice,
			socketPath: s.SocketPath,
			memSizeMib: memSize,
			spawnedAt:  s.CreatedAt,
		}
		cp.mu.Unlock()

		if s.FlockID != "" && s.AgentID != "" {
			if f, ok := cp.flockMgr.Get(s.FlockID); ok {
				f.UpdateAgentStatus(s.AgentID, orchestrator.AgentStatusReady)
				// Persist so a future LoadFromDisk reads ready rather than
				// the dead state left by the previous daemon's watchdog.
				if err := f.Persist(cp.workDir); err != nil {
					slog.Warn("recovery: persist ready status failed", "agent_id", s.AgentID, "err", err)
				}
			}
		}

		slog.Warn("recovery: vm back up", "vm_id", s.VMID, "guest_ip", s.GuestIP, "flock_id", s.FlockID, "agent_id", s.AgentID)
		recovered++
	}
	return recovered, failed, nil
}

// markFlockAgentDead flips a flock agent's status to dead when its VM could
// not be recovered. Mirrors the watchdog's behavior so a recovered flock with
// some agents lost still reports an accurate state to operators.
func (cp *ControlPlane) markFlockAgentDead(flockID, agentID string) {
	if flockID == "" || agentID == "" {
		return
	}
	f, ok := cp.flockMgr.Get(flockID)
	if !ok {
		return
	}
	f.UpdateAgentStatus(agentID, orchestrator.AgentStatusDead)
	if err := f.Persist(cp.workDir); err != nil {
		slog.Warn("recovery: persist dead status failed", "agent_id", agentID, "err", err)
	}
	if f.TownWall != nil {
		f.TownWall.Post("<orchestrator>", fmt.Sprintf("agent %s could not be recovered after daemon restart (marked dead)", agentID))
	}
}
