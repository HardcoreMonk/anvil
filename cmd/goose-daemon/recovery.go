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

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
)

// RecoverVMs brings every previously-spawned VM whose state.json is still on
// disk back up after a daemon restart, reusing the same IP, TAP, MAC, and agent
// token so external callers and any flock association stay stable.
//
// By default this is a cold restart: a fresh boot of the same rootfs (a full
// clone for plain VMs, or a dm-snapshot re-layered over the golden image for COW
// VMs) — in-VM memory is not preserved. When EPHEMERA_AUTOSNAPSHOT is enabled and
// a memory snapshot from the last graceful shutdown is present under
// vms/<id>/auto/, recovery instead attempts a WARM restore (vm.RestoreMachine,
// memory preserved, same identity/IP). The snapshot is one-shot: deleted after
// the attempt, and on any warm-restore failure the VM falls back to a cold boot.
//
// COW VMs (DiskMode=cow) reconstruct their dm-snapshot from the preserved
// exception store; this assumes the golden image is unchanged since spawn.
//
// A VM's state is dropped (and any flock agent marked dead) when:
//   - its disk file is missing (stale TAP released, counted as a failure)
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
		// Snapshot-restored VMs (v0.4.5) re-restore from their source snapshot
		// rather than cold-booting a rootfs clone. Their DiskPath is the transient
		// exception store that graceful shutdown discards, so they must branch off
		// BEFORE the disk-missing check below (which would otherwise drop them all).
		if s.SourceSnapshotID != "" {
			cp.recoverRestoredVM(s, &recovered, &failed)
			continue
		}
		if _, statErr := os.Stat(s.DiskPath); os.IsNotExist(statErr) {
			// state.json exists but its rootfs vanished (manual delete, partial
			// crash, etc.): the VM is unrecoverable. Clear the stale host TAP the
			// previous run left, drop the state + any orphaned auto-snapshot, mark
			// the flock agent dead, and surface it via failed[] rather than
			// dropping it silently. Release is idempotent (no allocation reclaimed
			// for this VM yet, so it just deletes the leftover host link).
			slog.Warn("recovery: disk missing, dropping state", "vm_id", s.VMID, "disk_path", s.DiskPath)
			cp.netManager.Release(s.TapDevice, s.GuestIP)
			storage.DeleteVMState(cp.workDir, s.VMID)
			storage.RemoveAutoSnapshot(cp.workDir, s.VMID)
			cp.markFlockAgentDead(s.FlockID, s.AgentID)
			failed = append(failed, s.VMID)
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

		// Warm restore (v0.4.0, opt-in): if a memory auto-snapshot is present,
		// restore the VM from it (memory preserved) instead of cold-booting. The
		// SAME IP/TAP/MAC are reused (reclaimed above) — the guest's live network
		// state is baked into the snapshot and there is no ReconfigureGuestIP on
		// this path, so the IP must match. The rootfs is already at s.DiskPath
		// (plain clone, or the COW dm device set up above), valid for both warm
		// and cold. On ANY failure, the one-shot snapshot is deleted and we fall
		// through to the cold-boot path below, reusing the reclaimed network and
		// (for COW) dmInfo — so dmInfo is NOT torn down here.
		if enableAutoSnapshot && storage.AutoSnapshotExists(cp.workDir, s.VMID) {
			memPath, statPath := storage.AutoSnapshotPaths(cp.workDir, s.VMID)
			machine, restoreErr := vm.RestoreMachine(context.Background(), vm.VMConfig{
				VMID:           s.VMID,
				SocketPath:     s.SocketPath,
				FirecrackerBin: cp.firecrackerPath,
				RootfsPath:     s.DiskPath,
				TapDevice:      s.TapDevice,
				MacAddress:     s.MacAddr,
				GuestIP:        s.GuestIP,
				GatewayIP:      "10.0.1.1",
				VsockUDSPath:   s.VsockPath,
				VcpuCount:      s.VcpuCount,
				MemSizeMib:     s.MemSizeMib,
			}, memPath, statPath)
			if restoreErr != nil {
				slog.Warn("recovery: warm-restore failed, falling back to cold boot", "vm_id", s.VMID, "err", restoreErr)
				cp.metrics.autoRestore.WithLabelValues("fail").Inc()
				storage.RemoveAutoSnapshot(cp.workDir, s.VMID)
			} else if waitErr := waitForAgent(s.GuestIP, 60*time.Second); waitErr != nil {
				slog.Warn("recovery: warm-restored agent did not respond, falling back to cold boot", "vm_id", s.VMID, "err", waitErr)
				cp.metrics.autoRestore.WithLabelValues("fail").Inc()
				machine.StopVMM()
				storage.RemoveAutoSnapshot(cp.workDir, s.VMID)
			} else {
				cp.registerRecoveredVM(s, machine, dmInfo)
				// One-shot: drop the snapshot after a successful restore so a
				// second bounce never rolls the VM back to this now-stale image.
				storage.RemoveAutoSnapshot(cp.workDir, s.VMID)
				cp.metrics.autoRestore.WithLabelValues("ok").Inc()
				slog.Warn("recovery: vm warm-restored", "vm_id", s.VMID, "guest_ip", s.GuestIP, "flock_id", s.FlockID, "agent_id", s.AgentID)
				recovered++
				continue
			}
		}

		// Clear the API socket a failed warm attempt may have left behind so the
		// cold boot starts clean (StartMachine does not remove it; mirrors the
		// spawn path). A no-op when no warm attempt ran.
		os.Remove(s.SocketPath)
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

		cp.registerRecoveredVM(s, machine, dmInfo)
		slog.Warn("recovery: vm back up", "vm_id", s.VMID, "guest_ip", s.GuestIP, "flock_id", s.FlockID, "agent_id", s.AgentID)
		recovered++
	}
	return recovered, failed, nil
}

// registerRecoveredVM adds a recovered machine to cp.vms and flips its flock
// agent status back to ready. Shared by the cold-boot and warm-restore paths so
// the two cannot drift; the caller logs the outcome and bumps the recovered count.
func (cp *ControlPlane) registerRecoveredVM(s storage.VMState, machine *firecracker.Machine, dmInfo *storage.DMSnapshotInfo) {
	vcpu := s.VcpuCount
	if vcpu == 0 {
		vcpu = 1 // matches vm.defaultVcpuCount (unsized VMs cold-boot at the default)
	}
	memSize := s.MemSizeMib
	if memSize == 0 {
		memSize = 1024 // matches vm.defaultMemSizeMib
	}
	cp.mu.Lock()
	cp.vms[s.VMID] = &runningVM{
		VMInfo: VMInfo{
			VMID:     s.VMID,
			GuestIP:  s.GuestIP,
			AgentURL: s.AgentURL,
			Profile:  s.Profile,
			Provider: s.Provider,
			Model:    s.Model,
		},
		agentToken:       s.AgentToken,
		diskPath:         s.DiskPath,
		dmSnapshot:       dmInfo,
		vsockPath:        s.VsockPath,
		machine:          machine,
		tapDevice:        s.TapDevice,
		socketPath:       s.SocketPath,
		vcpuCount:        vcpu,
		memSizeMib:       memSize,
		spawnedAt:        s.CreatedAt,
		sourceSnapshotID: s.SourceSnapshotID,
	}
	cp.mu.Unlock()

	if s.FlockID != "" && s.AgentID != "" {
		if f, ok := cp.flockMgr.Get(s.FlockID); ok {
			f.UpdateAgentStatus(s.AgentID, orchestrator.AgentStatusReady)
			// Persist so a future LoadFromDisk reads ready rather than the dead
			// state left by the previous daemon's watchdog.
			if err := f.Persist(cp.workDir); err != nil {
				slog.Warn("recovery: persist ready status failed", "agent_id", s.AgentID, "err", err)
			}
		}
	}
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

// recoverRestoredVM re-restores a snapshot-derived VM (v0.4.5) after a daemon
// restart, reusing the persisted identity (vm_id, IP, TAP, MAC, agent token).
// Unlike a spawn VM it cannot cold-boot — it is re-loaded from its source
// snapshot's memory+state (the same path POST /snapshots/{id}/restore takes), so
// the VM returns to its snapshot-time memory and disk; writes made after the
// original restore are not preserved (identical to a manual re-restore). The
// exception store is recreated fresh. On any failure the state is dropped, the
// network released, and the flock agent (if any) marked dead; vmID → failed.
func (cp *ControlPlane) recoverRestoredVM(s storage.VMState, recovered *int, failed *[]string) {
	meta, ok := cp.snapshots[s.SourceSnapshotID]
	if !ok {
		// Source snapshot was deleted while the VM ran: nothing to re-restore from.
		slog.Warn("recovery: source snapshot gone, dropping restored vm", "vm_id", s.VMID, "snapshot_id", s.SourceSnapshotID)
		cp.netManager.Release(s.TapDevice, s.GuestIP)
		storage.DeleteVMState(cp.workDir, s.VMID)
		storage.RemoveAutoSnapshot(cp.workDir, s.VMID)
		cp.markFlockAgentDead(s.FlockID, s.AgentID)
		*failed = append(*failed, s.VMID)
		return
	}

	// Clear orphans from the previous daemon process and force a FRESH exception
	// store — re-loading the snapshot's memory onto an evolved store would be
	// inconsistent, so the store is recreated from scratch by reRestoreMachine.
	if killErr := storage.KillStaleFirecracker(s.SocketPath); killErr != nil {
		slog.Warn("recovery: stale firecracker probe failed", "vm_id", s.VMID, "err", killErr)
	}
	logFifoPath := fmt.Sprintf("/tmp/fc-%s-log.fifo", s.VMID)
	storage.RemoveStaleVMArtifacts(s.SocketPath, s.VsockPath, logFifoPath)
	storage.ReclaimCOWDeviceKeepStore(cp.provisioner.WorkspaceDir, s.VMID)
	exceptionStore := filepath.Join(cp.provisioner.WorkspaceDir, s.VMID+".cow")
	os.Remove(exceptionStore)

	if reErr := cp.netManager.ReclaimAllocation(s.TapDevice, s.GuestIP, s.MacAddr); reErr != nil {
		slog.Warn("recovery: network reclaim failed, dropping restored vm", "vm_id", s.VMID, "err", reErr)
		storage.DeleteVMState(cp.workDir, s.VMID)
		cp.markFlockAgentDead(s.FlockID, s.AgentID)
		*failed = append(*failed, s.VMID)
		return
	}

	machine, dmInfo, err := cp.reRestoreMachine(meta, s.VMID, exceptionStore, s.SocketPath, s.TapDevice, s.GuestIP)
	if err != nil {
		slog.Warn("recovery: re-restore failed, dropping state", "vm_id", s.VMID, "snapshot_id", s.SourceSnapshotID, "err", err)
		cp.netManager.Release(s.TapDevice, s.GuestIP)
		storage.DeleteVMState(cp.workDir, s.VMID)
		cp.markFlockAgentDead(s.FlockID, s.AgentID)
		*failed = append(*failed, s.VMID)
		return
	}

	if waitErr := waitForAgent(s.GuestIP, 60*time.Second); waitErr != nil {
		slog.Warn("recovery: re-restored agent did not respond, dropping state", "vm_id", s.VMID, "err", waitErr)
		machine.StopVMM()
		storage.TeardownDMSnapshot(dmInfo)
		cp.netManager.Release(s.TapDevice, s.GuestIP)
		storage.DeleteVMState(cp.workDir, s.VMID)
		cp.markFlockAgentDead(s.FlockID, s.AgentID)
		*failed = append(*failed, s.VMID)
		return
	}

	cp.registerRecoveredVM(s, machine, dmInfo)
	slog.Warn("recovery: vm re-restored from snapshot", "vm_id", s.VMID, "snapshot_id", s.SourceSnapshotID, "guest_ip", s.GuestIP)
	*recovered++
}

// reRestoreMachine rebuilds a Firecracker machine from a snapshot for recovery
// (v0.4.5): set up a COW dm-snapshot over the snapshot's base (merging a rootfs
// diff if present), load the (diff-merged) memory snapshot, then reconfigure the
// guest IP to the recovered VM's address. It mirrors the dm-snapshot success path
// of restoreSnapshot (api.go) — KEEP THE TWO IN SYNC — but omits that handler's
// bind-mount fallback and fresh-network allocation. cp.restoreMu serializes the
// dm-setup + Firecracker-open window. On failure any dm-snapshot created is torn
// down before returning.
func (cp *ControlPlane) reRestoreMachine(meta storage.SnapshotMetadata, vmID, exceptionStore, socketPath, tapDevice, guestIP string) (*firecracker.Machine, *storage.DMSnapshotInfo, error) {
	var base storage.SnapshotMetadata
	if meta.SnapshotType == "diff" {
		b, ok := cp.snapshots[meta.BaseSnapshotID]
		if !ok {
			return nil, nil, fmt.Errorf("base snapshot %s not found", meta.BaseSnapshotID)
		}
		base = b
	}

	baseDiskForCOW := meta.DiskCopyPath
	var mergedRootfs string
	if meta.RootfsDiffPath != "" {
		mergedRootfs = pickMergedRootfsPath(cp.workDir, vmID)
		os.MkdirAll(filepath.Dir(mergedRootfs), 0755)
		os.Remove(mergedRootfs)
		if mErr := storage.MergeRootfsDiff(base.DiskCopyPath, meta.RootfsDiffPath, mergedRootfs); mErr != nil {
			os.Remove(mergedRootfs)
			return nil, nil, fmt.Errorf("merge rootfs diff: %w", mErr)
		}
		baseDiskForCOW = mergedRootfs
	}

	cp.restoreMu.Lock()
	dmInfo, err := storage.SetupDMSnapshot(baseDiskForCOW, exceptionStore, meta.DiskPath)
	if err != nil {
		cp.restoreMu.Unlock()
		if mergedRootfs != "" {
			os.Remove(mergedRootfs)
		}
		return nil, nil, fmt.Errorf("dm-snapshot setup: %w", err)
	}
	if mergedRootfs != "" {
		os.Remove(mergedRootfs) // dm pins it via its read-only loop; safe to unlink
	}

	memFileToUse := meta.MemFilePath
	var mergedMemPath string
	if meta.SnapshotType == "diff" {
		mergedMemPath = pickMergedMemPath(cp.workDir, vmID)
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		if mErr := storage.MergeMemoryDiff(base.MemFilePath, meta.MemFilePath, mergedMemPath); mErr != nil {
			cp.restoreMu.Unlock()
			storage.TeardownDMSnapshot(dmInfo)
			return nil, nil, fmt.Errorf("merge memory diff: %w", mErr)
		}
		memFileToUse = mergedMemPath
	}

	machine, err := vm.RestoreMachine(context.Background(), vm.VMConfig{
		VMID:           vmID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		RootfsPath:     meta.DiskPath,
		TapDevice:      tapDevice,
		MacAddress:     meta.MacAddr,
		GuestIP:        guestIP,
		GatewayIP:      "10.0.1.1",
		// VsockUDSPath empty: snapshot state recreates vsock at meta.VsockPath.
	}, memFileToUse, meta.StatFilePath)
	cp.restoreMu.Unlock()
	if mergedMemPath != "" {
		os.Remove(mergedMemPath)
	}
	if err != nil {
		storage.TeardownDMSnapshot(dmInfo)
		return nil, nil, fmt.Errorf("restore machine: %w", err)
	}

	// Firecracker recreated vsock at meta.VsockPath; reconfigure the guest's IP to
	// the recovered address (the snapshot's baked IP is the original).
	if rErr := vm.ReconfigureGuestIP(meta.VsockPath, guestIP+"/24", "10.0.1.1"); rErr != nil {
		machine.StopVMM()
		storage.TeardownDMSnapshot(dmInfo)
		return nil, nil, fmt.Errorf("vsock ip reconfigure: %w", rErr)
	}
	return machine, dmInfo, nil
}
