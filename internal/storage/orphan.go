package storage

import (
	"fmt"
	"os"
	"os/exec"
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
