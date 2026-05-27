package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// vmStateSchemaVersion is the current on-disk format version for per-VM state.
// Bump when the wire layout changes in a way that needs migration logic.
const vmStateSchemaVersion = 1

// Disk mode tags recorded in VMState.DiskMode. Cold-restart recovery handles both:
// "plain" boots the full rootfs clone, "cow" re-layers the preserved dm-snapshot
// exception store over the golden image (v0.4.0).
const (
	DiskModePlain = "plain"
	DiskModeCOW   = "cow"
)

// VMState is the on-disk snapshot of everything cold-restart needs to bring a
// previously-running VM back after a daemon restart (graceful or crash).
// Persisted per-VM at workDir/vms/<vm_id>/state.json.
//
// Memory state is NOT preserved in state.json; cold-restart boots the VM from
// its rootfs clone and re-attaches the same network identity and agent token.
// (Opt-in EPHEMERA_AUTOSNAPSHOT additionally writes a separate memory snapshot
// under auto/ for warm restore — see AutoSnapshotDir.)
type VMState struct {
	SchemaVersion int       `json:"schema_version"`
	VMID          string    `json:"vm_id"`
	GuestIP       string    `json:"guest_ip"`
	TapDevice     string    `json:"tap_device"`
	MacAddr       string    `json:"mac_addr"`
	VsockPath     string    `json:"vsock_path"`
	SocketPath    string    `json:"socket_path"`
	AgentToken    string    `json:"agent_token"`
	DiskPath      string    `json:"disk_path"`
	DiskMode      string    `json:"disk_mode"` // "plain" | "cow"
	Profile       string    `json:"profile,omitempty"`
	VcpuCount     int64     `json:"vcpu_count"`
	MemSizeMib    int64     `json:"mem_size_mib"`
	FlockID       string    `json:"flock_id,omitempty"`
	AgentID       string    `json:"agent_id,omitempty"`
	AgentURL      string    `json:"agent_url"`
	CreatedAt     time.Time `json:"created_at"`
}

// vmStatePath returns the per-VM state.json location under workDir.
func vmStatePath(workDir, vmID string) string {
	return filepath.Join(workDir, "vms", vmID, "state.json")
}

// AutoSnapshotDir returns the per-VM memory auto-snapshot directory under
// workDir — a subdirectory of the VM's state directory (v0.4.0). When
// EPHEMERA_AUTOSNAPSHOT is enabled, the graceful-shutdown path writes a
// memory+state snapshot here and RecoverVMs warm-restores from it.
func AutoSnapshotDir(workDir, vmID string) string {
	return filepath.Join(workDir, "vms", vmID, "auto")
}

// AutoSnapshotPaths returns the memory and state file paths inside a VM's
// auto-snapshot directory.
func AutoSnapshotPaths(workDir, vmID string) (memPath, statPath string) {
	dir := AutoSnapshotDir(workDir, vmID)
	return filepath.Join(dir, "memory.bin"), filepath.Join(dir, "state.bin")
}

// AutoSnapshotExists reports whether a usable memory auto-snapshot is present
// for vmID (the memory file is the load-bearing artifact for a warm restore).
func AutoSnapshotExists(workDir, vmID string) bool {
	memPath, _ := AutoSnapshotPaths(workDir, vmID)
	_, err := os.Stat(memPath)
	return err == nil
}

// RemoveAutoSnapshot deletes a VM's auto-snapshot directory (best-effort).
// Auto-snapshots are one-shot: consumed on a restore attempt and rewritten by
// the next graceful shutdown, so a stale image is never reused. Call this
// whenever a VM's state.json is dropped so an orphaned auto/ does not linger.
func RemoveAutoSnapshot(workDir, vmID string) {
	os.RemoveAll(AutoSnapshotDir(workDir, vmID))
}

// SaveVMState writes state atomically (tmp + rename). Not safe for concurrent
// writes to the same VM; the daemon's call sites (spawnVMInternal once,
// destroyVM once) never overlap.
func SaveVMState(workDir string, state VMState) error {
	if state.SchemaVersion == 0 {
		state.SchemaVersion = vmStateSchemaVersion
	}
	dst := vmStatePath(workDir, state.VMID)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("vm_state: create dir: %w", err)
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("vm_state: marshal: %w", err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return fmt.Errorf("vm_state: write tmp: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("vm_state: rename: %w", err)
	}
	return nil
}

// LoadVMState reads a single VM's state.json.
func LoadVMState(workDir, vmID string) (VMState, error) {
	b, err := os.ReadFile(vmStatePath(workDir, vmID))
	if err != nil {
		return VMState{}, fmt.Errorf("vm_state: read: %w", err)
	}
	var s VMState
	if err := json.Unmarshal(b, &s); err != nil {
		return VMState{}, fmt.Errorf("vm_state: unmarshal: %w", err)
	}
	return s, nil
}

// VMStateExists reports whether a persisted state.json is present for vmID. Only
// spawn VMs persist state (SaveVMState), so a present file means RecoverVMs will
// bring the VM back — the graceful-shutdown path uses this to decide whether to
// preserve a COW exception store or tear it down.
func VMStateExists(workDir, vmID string) bool {
	_, err := os.Stat(vmStatePath(workDir, vmID))
	return err == nil
}

// DeleteVMState removes a VM's state.json and its parent directory if empty.
// Missing entries are not an error.
func DeleteVMState(workDir, vmID string) error {
	dir := filepath.Dir(vmStatePath(workDir, vmID))
	err := os.Remove(vmStatePath(workDir, vmID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("vm_state: delete: %w", err)
	}
	// Best-effort directory removal; os.Remove only succeeds on empty dirs.
	os.Remove(dir)
	return nil
}

// ListVMState enumerates every recoverable VM under workDir/vms/.
// Directories without a parseable state.json are skipped. Results are sorted by
// VMID so recovery order is deterministic.
func ListVMState(workDir string) ([]VMState, error) {
	base := filepath.Join(workDir, "vms")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vm_state: list dir: %w", err)
	}
	var out []VMState
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := LoadVMState(workDir, e.Name())
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].VMID < out[j].VMID
	})
	return out, nil
}
