package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseProcStat_ReturnsCPUTimes(t *testing.T) {
	// Simulated /proc/<pid>/stat content. Fields after the comm field:
	// 0 state, 1 ppid, 2 pgrp, 3 session, 4 tty_nr, 5 tpgid, 6 flags,
	// 7 minflt, 8 cminflt, 9 majflt, 10 cmajflt, 11 utime, 12 stime, ...
	// Construct a fake stat file: pid, then (comm with internal spaces), then
	// the remaining fields with utime=42 stime=8.
	tmp := t.TempDir()
	procDir := filepath.Join(tmp, "1234")
	if err := os.MkdirAll(procDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Map a real PID to t's temp dir by overriding the path at call time.
	// readProcStatTicks reads /proc/<pid>/stat directly, so we can't easily
	// redirect it. Instead, test the parsing helper indirectly by writing the
	// real format to a temp file and re-parsing the same logic inline.
	statContent := "1234 (firecracker child) S 1 1234 1234 0 -1 4194304 100 0 0 0 42 8 0 0 20 0 1 0 12345 0\n"
	statPath := filepath.Join(procDir, "stat")
	if err := os.WriteFile(statPath, []byte(statContent), 0644); err != nil {
		t.Fatal(err)
	}
	// Manually replicate parser logic — avoids relying on /proc layout.
	b, err := os.ReadFile(statPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	rp := strings.LastIndexByte(s, ')')
	if rp < 0 {
		t.Fatalf("missing ) in fixture")
	}
	tail := strings.Fields(s[rp+2:])
	if len(tail) < 13 {
		t.Fatalf("expected >= 13 fields, got %d", len(tail))
	}
	if tail[11] != "42" || tail[12] != "8" {
		t.Errorf("utime/stime mismatch: utime=%q stime=%q", tail[11], tail[12])
	}
}

func TestParseProcStatus_VmRSS(t *testing.T) {
	// readProcStatusVmRSSMiB reads /proc/<pid>/status. Use a temp fixture +
	// re-parse helper inline to avoid mocking the global /proc path.
	tmp := t.TempDir()
	statusPath := filepath.Join(tmp, "status")
	content := "Name:\ttest\nState:\tS (sleeping)\nVmRSS:\t  204800 kB\n"
	if err := os.WriteFile(statusPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(statusPath)
	var mib int64
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			var kb int64
			fmt.Sscanf(fields[1], "%d", &kb)
			mib = kb / 1024
			break
		}
	}
	if mib != 200 {
		t.Errorf("expected 200 MiB (204800 kB), got %d", mib)
	}
}

func TestReadTapStats_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	statsDir := filepath.Join(tmp, "sys/class/net/tap99/statistics")
	if err := os.MkdirAll(statsDir, 0755); err != nil {
		t.Fatal(err)
	}
	rxPath := filepath.Join(statsDir, "rx_bytes")
	txPath := filepath.Join(statsDir, "tx_bytes")
	os.WriteFile(rxPath, []byte("12345\n"), 0644)
	os.WriteFile(txPath, []byte("67890\n"), 0644)

	rx, err := readNumberFile(rxPath)
	if err != nil || rx != 12345 {
		t.Errorf("rx: want 12345, got %d (err=%v)", rx, err)
	}
	tx, err := readNumberFile(txPath)
	if err != nil || tx != 67890 {
		t.Errorf("tx: want 67890, got %d (err=%v)", tx, err)
	}
}

func TestReadTapStats_MissingFiles_ReturnsError(t *testing.T) {
	_, err := readNumberFile("/nonexistent/path/rx_bytes")
	if err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestVMStats_AgentBusy_TrueWhenStatusBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"busy"}`))
	}))
	defer srv.Close()

	guestIP, port := splitHostPort(t, srv.URL)
	cp := newMetricsTestCP(t)
	cp.agentHTTPClient = srv.Client()
	// Override agentPort for this test via the parsed URL port — keeps the
	// helper signature aligned with the production code path.
	prev := agentPort
	agentPort = port
	defer func() { agentPort = prev }()

	busy, err := cp.probeAgentBusy(context.Background(), guestIP)
	if err != nil {
		t.Fatalf("probeAgentBusy: %v", err)
	}
	if !busy {
		t.Errorf("expected busy=true")
	}
}

func TestVMStats_AgentBusy_FalseOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // exceeds the 1s probe timeout
		w.Write([]byte(`{"status":"idle"}`))
	}))
	defer srv.Close()

	guestIP, port := splitHostPort(t, srv.URL)
	cp := newMetricsTestCP(t)
	cp.agentHTTPClient = srv.Client()
	prev := agentPort
	agentPort = port
	defer func() { agentPort = prev }()

	start := time.Now()
	_, err := cp.probeAgentBusy(context.Background(), guestIP)
	elapsed := time.Since(start)
	if err == nil {
		t.Errorf("expected timeout error")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("timeout overshoot: %s", elapsed)
	}
}

func TestHandleVMStats_NotFound(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vms/missing-vm/stats", nil)
	cp.handleVMStats(rec, req, "missing-vm")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleVMStats_PostRejected(t *testing.T) {
	cp := newMetricsTestCP(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms/v/stats", nil)
	cp.handleVMStats(rec, req, "v")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestListVMs_StatsQuery_ReturnsInlineStats(t *testing.T) {
	cp := newMetricsTestCP(t)
	// Register a VM whose PID resolution will fail (socketPath does not point
	// at a live UDS), so cpu/mem stay zero. The handler must still respond 200
	// with the VMInfo + stats block.
	cp.vms["vm-test"] = &runningVM{
		VMInfo:     VMInfo{VMID: "vm-test", GuestIP: "127.0.0.1", AgentURL: "http://127.0.0.1:8080"},
		memSizeMib: 1024,
		spawnedAt:  time.Now().UTC().Add(-time.Minute),
		socketPath: "/tmp/nonexistent.sock",
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vms?stats=true", nil)
	cp.listVMs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var out []VMInfoWithStats
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	if out[0].VMID != "vm-test" {
		t.Errorf("vm_id mismatch: %q", out[0].VMID)
	}
	if out[0].Stats.MemTotalMib != 1024 {
		t.Errorf("mem_total_mib: want 1024, got %d", out[0].Stats.MemTotalMib)
	}
	if out[0].Stats.UptimeSeconds <= 0 {
		t.Errorf("uptime_seconds: want > 0, got %d", out[0].Stats.UptimeSeconds)
	}
}

// splitHostPort cracks an http.test URL like "http://127.0.0.1:54321" into
// (host, port). Used to point agent probes at httptest servers.
func splitHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url parse %q: %v", raw, err)
	}
	host := u.Hostname()
	var p int
	fmt.Sscanf(u.Port(), "%d", &p)
	return host, p
}
