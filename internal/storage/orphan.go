package storage

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// KillStaleFirecracker terminates any Firecracker process that was launched
// against the given API socket path and is still running. Used at daemon
// startup to clear orphans left by a previous run before we re-spawn or
// re-bind against the same VMID.
//
// Returns nil if no matching process is found. Sends SIGTERM first, waits up to
// killWait, then escalates to SIGKILL.
func KillStaleFirecracker(socketPath string) error {
	pids, err := findFirecrackerPIDs(socketPath)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		return nil
	}
	const killWait = 1500 * time.Millisecond
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(killWait)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			// signal 0 probes existence without affecting the process.
			if err := syscall.Kill(pid, 0); err == nil {
				alive = true
				break
			}
		}
		if !alive {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

// findFirecrackerPIDs returns PIDs of firecracker processes whose cmdline
// references socketPath via the --api-sock argument.
//
// pgrep -af firecracker prints "<pid> <cmdline>"; we scan for an exact
// "--api-sock <socketPath>" match so we don't accidentally match a different
// VM whose path happens to share a prefix.
func findFirecrackerPIDs(socketPath string) ([]int, error) {
	out, err := exec.Command("pgrep", "-af", "firecracker").Output()
	if err != nil {
		// pgrep exit 1 = no match; treat as empty rather than error.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep firecracker: %w", err)
	}
	needle := "--api-sock " + socketPath
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) < 1 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// RemoveStaleVMArtifacts deletes per-VM transient files left over from a prior
// run: API socket, log FIFO, and vsock UDS. Errors are ignored — these paths
// may not exist if the previous daemon shut down cleanly.
func RemoveStaleVMArtifacts(socketPath, vsockPath, logFifoPath string) {
	if socketPath != "" {
		os.Remove(socketPath)
	}
	if vsockPath != "" {
		os.Remove(vsockPath)
	}
	if logFifoPath != "" {
		os.Remove(logFifoPath)
	}
}

// cowDMPrefix is the device-mapper name prefix SetupDMSnapshot assigns to every
// COW VM: "cow-" + filepath.Base(exceptionStore) where the store is
// "<vm_id>.cow" and vm_id always starts with "vm-". Matching on this prefix
// keeps orphan cleanup from touching unrelated dm-snapshot devices on the host.
const cowDMPrefix = "cow-vm-"

// RemoveOrphanCOWDevices tears down the dm-snapshot device, its loop devices,
// and the on-disk exception store for any Ephemera COW VM whose vm_id is NOT in
// liveVMIDs — kernel/file leftovers from a daemon that crashed before
// TeardownDMSnapshot could run. VMs that still have a state.json (passed in via
// liveVMIDs) are left untouched for the recovery path to manage.
//
// Best-effort: every step is attempted and individual failures are logged, never
// fatal. Returns the number of orphan dm-snapshot devices removed.
//
// Limitation: a base-disk (golden image) loop left by a crash that happened
// after `losetup` but before `dmsetup create` cannot be attributed to a vm_id
// (all base loops share the same backing file), so it is not reclaimed here.
func RemoveOrphanCOWDevices(workspaceDir string, liveVMIDs map[string]struct{}) int {
	loops := listLoopDevices() // loopDev -> backing file
	detached := make(map[string]bool)
	removed := 0

	for _, dmName := range listSnapshotDMDevices() {
		if !strings.HasPrefix(dmName, cowDMPrefix) {
			continue
		}
		vmID := strings.TrimSuffix(strings.TrimPrefix(dmName, "cow-"), ".cow")
		if _, live := liveVMIDs[vmID]; live {
			continue
		}
		// Orphan: reconstruct the loop devices from the dm table, then unwind the
		// stack in the same order TeardownDMSnapshot uses (dm device → cow loop →
		// base loop → exception store file).
		base, cow := dmSnapshotLoops(dmName)
		// Detach any bind mount on the rootfs target before removing the dm device.
		// A crashed COW spawn VM leaves its dm device bind-mounted over <vm_id>.ext4;
		// removing the device without unmounting first leaves a dangling block-device
		// node that even `rm` cannot clear, leaking across runs and pinning a dm minor.
		exec.Command("umount", "-l", filepath.Join(workspaceDir, vmID+".ext4")).Run()
		removeDMDevice(dmName)
		for _, lp := range []string{cow, base} {
			if lp != "" {
				exec.Command("losetup", "-d", lp).Run()
				detached[lp] = true
			}
		}
		os.Remove(filepath.Join(workspaceDir, vmID+".cow"))
		os.Remove(filepath.Join(workspaceDir, vmID+".ext4"))
		slog.Warn("recovery: removed orphan cow device", "vm_id", vmID, "dm", dmName)
		removed++
	}

	// Detach dangling COW-store loops that have no dm device on top — the result
	// of a crash between `losetup` on the exception store and `dmsetup create`.
	for loopDev, backing := range loops {
		if detached[loopDev] {
			continue
		}
		dir, file := filepath.Split(backing)
		if filepath.Clean(dir) != filepath.Clean(workspaceDir) || !strings.HasSuffix(file, ".cow") {
			continue
		}
		vmID := strings.TrimSuffix(file, ".cow")
		if _, live := liveVMIDs[vmID]; live {
			continue
		}
		exec.Command("losetup", "-d", loopDev).Run()
		os.Remove(backing)
		slog.Warn("recovery: detached orphan cow loop", "vm_id", vmID, "loop", loopDev)
	}
	return removed
}

// ReclaimCOWDeviceKeepStore tears down a single COW VM's stale dm-snapshot device,
// its loop devices, and any lingering bind mount on its rootfs target — while
// PRESERVING the on-disk exception store. RecoverVMs calls this before re-creating
// the dm-snapshot for a COW VM that still has a state.json: if the previous run
// crashed (no graceful teardown), a live "cow-<vm_id>.cow" device would otherwise
// make SetupDMSnapshot's `dmsetup create` fail with a name collision, and a dangling
// store loop would let losetup attach the .cow twice.
//
// Best-effort and idempotent: a no-op after a clean shutdown (the device is already
// gone). The .cow store is never removed — it holds the COW writes the restore
// re-layers over the golden image.
func ReclaimCOWDeviceKeepStore(workspaceDir, vmID string) {
	// Clear any lingering bind mount on the rootfs target so a fresh
	// SetupDMSnapshot bind can take its place (lazy: detaches immediately).
	exec.Command("umount", "-l", filepath.Join(workspaceDir, vmID+".ext4")).Run()

	detached := make(map[string]bool)

	// Tear down the dm-snapshot device for this exact vm_id, unwinding the stack in
	// TeardownDMSnapshot order (dm device → cow loop → base loop). Keep the store.
	for _, dmName := range listSnapshotDMDevices() {
		if !strings.HasPrefix(dmName, cowDMPrefix) {
			continue
		}
		if strings.TrimSuffix(strings.TrimPrefix(dmName, "cow-"), ".cow") != vmID {
			continue
		}
		base, cow := dmSnapshotLoops(dmName)
		removeDMDevice(dmName)
		for _, lp := range []string{cow, base} {
			if lp != "" {
				exec.Command("losetup", "-d", lp).Run()
				detached[lp] = true
			}
		}
		slog.Warn("recovery: cleared stale cow device for restore", "vm_id", vmID, "dm", dmName)
	}

	// Detach a dangling exception-store loop with no dm device on top (crash between
	// `losetup` on the store and `dmsetup create`). Leaving it attached would let
	// SetupDMSnapshot's `losetup --find` bind the same .cow to a second loop.
	store := filepath.Join(workspaceDir, vmID+".cow")
	for loopDev, backing := range listLoopDevices() {
		if detached[loopDev] {
			continue
		}
		if filepath.Clean(backing) == filepath.Clean(store) {
			exec.Command("losetup", "-d", loopDev).Run()
			slog.Warn("recovery: detached stale cow loop for restore", "vm_id", vmID, "loop", loopDev)
		}
	}
}

// listLoopDevices maps each loop device to its backing file via `losetup -a`.
// Returns an empty map when losetup is unavailable or reports nothing.
func listLoopDevices() map[string]string {
	out, err := exec.Command("losetup", "-a").Output()
	if err != nil {
		return map[string]string{}
	}
	return parseLosetupOutput(string(out))
}

// listSnapshotDMDevices returns the names of snapshot-target device-mapper
// devices via `dmsetup ls --target snapshot`.
func listSnapshotDMDevices() []string {
	out, err := exec.Command("dmsetup", "ls", "--target", "snapshot").Output()
	if err != nil {
		return nil
	}
	return parseDMSetupLs(string(out))
}

// dmSnapshotLoops returns the origin (base) and COW loop device paths backing a
// snapshot dm device, parsed from `dmsetup table <name>`.
func dmSnapshotLoops(dmName string) (origin, cow string) {
	out, err := exec.Command("dmsetup", "table", dmName).Output()
	if err != nil {
		return "", ""
	}
	return parseDMSnapshotTable(string(out))
}

// removeDMDevice runs `dmsetup remove` with the same retry budget as
// TeardownDMSnapshot, tolerating a brief window where a dying process still
// holds the device open.
func removeDMDevice(dmName string) {
	for i := 0; i < 5; i++ {
		if exec.Command("dmsetup", "remove", dmName).Run() == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// parseLosetupOutput maps loop device → backing file from `losetup -a` output.
// Lines look like "/dev/loop0: [64769]:12345 (/path/file)"; a trailing
// " (deleted)" annotation on the backing path is stripped.
func parseLosetupOutput(out string) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.Index(line, ":")
		if colon <= 0 {
			continue
		}
		open := strings.Index(line, "(")
		closeIdx := strings.LastIndex(line, ")")
		if open < 0 || closeIdx <= open {
			continue
		}
		backing := strings.TrimSuffix(line[open+1:closeIdx], " (deleted)")
		m[line[:colon]] = backing
	}
	return m
}

// parseDMSetupLs returns device names from `dmsetup ls --target snapshot`.
// Lines are "<name>\t(major:minor)"; "No devices found" yields an empty slice.
func parseDMSetupLs(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "No devices") {
			continue
		}
		fields := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
		if len(fields) == 0 {
			continue
		}
		names = append(names, fields[0])
	}
	return names
}

// parseDMSnapshotTable extracts the origin and COW loop device paths from a
// snapshot `dmsetup table` line, e.g. "0 16777216 snapshot 7:0 7:1 P 8".
// Device-mapper prints major:minor; loop devices use major 7, mapped back to
// /dev/loopN. Non-loop or malformed fields yield "".
func parseDMSnapshotTable(table string) (origin, cow string) {
	fields := strings.Fields(strings.TrimSpace(table))
	if len(fields) < 5 || fields[2] != "snapshot" {
		return "", ""
	}
	return loopPathFromMajorMinor(fields[3]), loopPathFromMajorMinor(fields[4])
}

// loopPathFromMajorMinor converts a "major:minor" id to /dev/loopN when major is
// the loop major (7); otherwise returns "".
func loopPathFromMajorMinor(mm string) string {
	parts := strings.SplitN(mm, ":", 2)
	if len(parts) != 2 || parts[0] != "7" {
		return ""
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return ""
	}
	return "/dev/loop" + parts[1]
}
