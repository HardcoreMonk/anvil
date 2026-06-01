package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// vmStatsSnapshot collects CPU%, memory, network, uptime, and agent_busy for
// one running VM. Failures in any single source leave that field at zero and
// emit a warn log; the snapshot still returns successfully so a dashboard can
// show partial data instead of breaking on a transient /proc race.
func (cp *ControlPlane) collectVMStats(ctx context.Context, vmID string, v *runningVM) VMStats {
	stats := VMStats{
		VMID:          vmID,
		MemTotalMib:   v.memSizeMib,
		UptimeSeconds: int64(time.Since(v.spawnedAt).Seconds()),
	}

	pid, err := cp.getOrFindFCPid(v)
	if err != nil {
		slog.Warn("stats: firecracker pid not found", "vm_id", vmID, "socket", v.socketPath, "err", err)
	} else {
		if pct, err := samplePIDCPUPercent(pid, 100*time.Millisecond); err == nil {
			stats.CPUPercent = pct
		} else {
			slog.Warn("stats: cpu sample failed", "vm_id", vmID, "pid", pid, "err", err)
		}
		if mib, err := readProcStatusVmRSSMiB(pid); err == nil {
			stats.MemUsedMib = mib
		} else {
			slog.Warn("stats: vmrss read failed", "vm_id", vmID, "pid", pid, "err", err)
		}
	}

	if v.tapDevice != "" {
		rx, tx, err := readTapStats(v.tapDevice)
		if err == nil {
			// Swap host TAP perspective to VM perspective:
			// host RX = bytes received from the VM = VM's TX.
			stats.NetworkRxBytes = tx
			stats.NetworkTxBytes = rx
		} else {
			slog.Warn("stats: tap statistics read failed", "vm_id", vmID, "tap", v.tapDevice, "err", err)
		}
	}

	if busy, err := cp.probeAgentBusy(ctx, v.GuestIP); err == nil {
		stats.AgentBusy = busy
	} else {
		// Best-effort stat: a down/unreachable agent must not spam the log every
		// poll (e.g. a halted VM awaiting teardown). Host-side stats still return.
		slog.Debug("stats: agent busy probe failed", "vm_id", vmID, "err", err)
	}

	return stats
}

// getOrFindFCPid returns the cached Firecracker PID, validating it by reading
// /proc/<pid>/comm. When the cache is stale (process exited or PID reused for
// a non-firecracker binary), it falls through to findFirecrackerPID and caches
// the new value.
func (cp *ControlPlane) getOrFindFCPid(v *runningVM) (int, error) {
	cached := atomic.LoadInt32(&v.fcPID)
	if cached > 0 && isFirecrackerPID(int(cached)) {
		return int(cached), nil
	}
	atomic.StoreInt32(&v.fcPID, 0)
	pid, err := findFirecrackerPID(v.socketPath)
	if err != nil {
		return 0, err
	}
	atomic.StoreInt32(&v.fcPID, int32(pid))
	return pid, nil
}

// findFirecrackerPID traces socketPath through /proc/net/unix (path → inode)
// and /proc/<pid>/fd (inode → process). The firecracker-go-sdk does not expose
// the child's PID, so this is the only daemon-side route. comm prefilter avoids
// scanning every kernel thread's fd directory.
func findFirecrackerPID(socketPath string) (int, error) {
	if socketPath == "" {
		return 0, errors.New("empty socket path")
	}
	inode, err := lookupSocketInode(socketPath)
	if err != nil {
		return 0, err
	}
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}
	for _, e := range procEntries {
		if !e.IsDir() {
			continue
		}
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		if !isFirecrackerPID(pid) {
			continue
		}
		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		fdEntries, ferr := os.ReadDir(fdDir)
		if ferr != nil {
			continue
		}
		for _, fd := range fdEntries {
			fi, serr := os.Stat(filepath.Join(fdDir, fd.Name()))
			if serr != nil {
				continue
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			if st.Ino == inode {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("no process holds inode %d for %q", inode, socketPath)
}

// lookupSocketInode parses /proc/net/unix and returns the inode whose Path
// column matches socketPath. Returns 0 + error if not found.
func lookupSocketInode(socketPath string) (uint64, error) {
	f, err := os.Open("/proc/net/unix")
	if err != nil {
		return 0, fmt.Errorf("open /proc/net/unix: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// /proc/net/unix columns: Num RefCount Protocol Flags Type State Inode Path
		if len(fields) < 8 {
			continue
		}
		if fields[7] == socketPath {
			n, err := strconv.ParseUint(fields[6], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse inode %q: %w", fields[6], err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("socket %q not found in /proc/net/unix", socketPath)
}

// isFirecrackerPID returns true when /proc/<pid>/comm reports "firecracker".
// Used to prefilter the /proc scan and to validate the cached PID before reuse.
func isFirecrackerPID(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "firecracker"
}

// samplePIDCPUPercent measures CPU% over the given interval by sampling
// /proc/<pid>/stat twice. Returns the host-CPU-normalized percentage (e.g. 100
// = one full host core). Returns 0 + nil if the process disappears between samples.
func samplePIDCPUPercent(pid int, interval time.Duration) (float64, error) {
	t1, err := readProcStatTicks(pid)
	if err != nil {
		return 0, err
	}
	time.Sleep(interval)
	t2, err := readProcStatTicks(pid)
	if err != nil {
		return 0, nil // process exited mid-sample — return 0 instead of failing
	}
	deltaTicks := float64(t2 - t1)
	if deltaTicks < 0 {
		return 0, nil
	}
	clkTck := float64(getClockTicks())
	pct := (deltaTicks / clkTck) / interval.Seconds() * 100
	return pct, nil
}

// readProcStatTicks returns utime+stime from /proc/<pid>/stat (fields 14, 15
// in 1-based count, immediately after the comm field which we strip from the
// last ')' to avoid spaces-in-comm issues).
func readProcStatTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(b)
	rp := strings.LastIndexByte(s, ')')
	if rp < 0 || rp+2 >= len(s) {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	// After "<pid> (comm) ", the next fields are state, ppid, pgrp, session,
	// tty_nr, tpgid, flags, minflt, cminflt, majflt, cmajflt, utime, stime, ...
	// Index 11 = utime, 12 = stime (0-based after the comm strip).
	tail := strings.Fields(s[rp+2:])
	if len(tail) < 13 {
		return 0, fmt.Errorf("too few fields in /proc/%d/stat", pid)
	}
	utime, err := strconv.ParseUint(tail[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(tail[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return utime + stime, nil
}

// readProcStatusVmRSSMiB returns the resident set size of pid in MiB, parsed
// from the "VmRSS:\t   12345 kB" line of /proc/<pid>/status.
func readProcStatusVmRSSMiB(pid int) (int64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("malformed VmRSS line: %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb / 1024, nil // kB → MiB
	}
	return 0, errors.New("VmRSS not present in /proc/status")
}

// readTapStats reads the host-side TAP rx_bytes / tx_bytes counters from
// /sys/class/net/<tap>/statistics. Note: these are host-perspective; callers
// (e.g. collectVMStats) swap to VM perspective before exposing them.
func readTapStats(tap string) (rxBytes, txBytes int64, err error) {
	base := filepath.Join("/sys/class/net", tap, "statistics")
	rx, rxErr := readNumberFile(filepath.Join(base, "rx_bytes"))
	tx, txErr := readNumberFile(filepath.Join(base, "tx_bytes"))
	if rxErr != nil && txErr != nil {
		return 0, 0, fmt.Errorf("both rx_bytes and tx_bytes unreadable: rx=%v tx=%v", rxErr, txErr)
	}
	// Partial read is acceptable — the caller logs warnings and any unreadable
	// counter stays at zero.
	return rx, tx, nil
}

func readNumberFile(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// probeAgentBusy issues a 1s GET /health against the in-VM agent and parses
// its JSON {"status":"idle|busy"} response. Direct HTTP rather than
// proxyAgentEndpoint because the body needs JSON parsing, not streaming.
func (cp *ControlPlane) probeAgentBusy(ctx context.Context, guestIP string) (bool, error) {
	if guestIP == "" {
		return false, errors.New("empty guest_ip")
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	url := fmt.Sprintf("http://%s:%d/health", guestIP, agentPort)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := cp.agentHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return false, err
	}
	var parsed struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, err
	}
	return parsed.Status == "busy", nil
}

// getClockTicks returns the system clock-tick rate (USER_HZ), typically 100 Hz
// on x86_64 Linux. Required for /proc/<pid>/stat tick→time conversion.
//
// /proc/sys/kernel does not expose USER_HZ. The reliable path on Linux is
// sysconf(_SC_CLK_TCK) which Go does not bind directly. Nearly every modern
// Linux x86_64 kernel uses CONFIG_HZ_100 or _250 with USER_HZ=100, so we
// hard-code 100. A misreading would skew cpu_percent — operators who need
// exactness can run `getconf CLK_TCK` themselves and verify.
func getClockTicks() int64 { return 100 }

// stats_collector.go uses sync for the agent HTTP client guard exposed by
// other handlers. Referenced here to keep imports honest if future helpers
// move in.
var _ = sync.Mutex{}
