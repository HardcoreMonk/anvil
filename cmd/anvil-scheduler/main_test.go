package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ephemera/internal/anvilmcp"
)

func TestLoadSchedulerConfigDefaultsAndEnv(t *testing.T) {
	t.Setenv("ANVIL_SCHEDULER_ADDR", "")
	t.Setenv("ANVIL_SCHEDULER_STATE", "")
	t.Setenv("ANVIL_SCHEDULER_QUOTA_STORE", "")
	cfg, err := loadSchedulerConfig()
	if err != nil {
		t.Fatalf("loadSchedulerConfig defaults: %v", err)
	}
	if cfg.Addr != defaultSchedulerAddr {
		t.Fatalf("default addr = %q, want %q", cfg.Addr, defaultSchedulerAddr)
	}
	if cfg.PollInterval != 10*time.Second || cfg.ReconcileInterval != 30*time.Second || cfg.HostTimeout != 3*time.Second || cfg.FailureThreshold != 3 {
		t.Fatalf("default control loop config = %+v", cfg)
	}

	hostsFile := filepath.Join(t.TempDir(), "hosts.json")
	t.Setenv("ANVIL_SCHEDULER_ADDR", "0.0.0.0:3999")
	t.Setenv("ANVIL_SCHEDULER_STATE", "/var/lib/anvil/scheduler.json")
	t.Setenv("ANVIL_SCHEDULER_QUOTA_STORE", "/var/lib/anvil/tenants.json")
	t.Setenv("ANVIL_SCHEDULER_HOSTS_FILE", hostsFile)
	t.Setenv("ANVIL_SCHEDULER_POLL_INTERVAL", "2s")
	t.Setenv("ANVIL_SCHEDULER_RECONCILE_INTERVAL", "7s")
	t.Setenv("ANVIL_SCHEDULER_HOST_TIMEOUT", "500ms")
	t.Setenv("ANVIL_SCHEDULER_FAILURE_THRESHOLD", "5")
	t.Setenv("ANVIL_SCHEDULER_API_TOKEN", "scheduler-token")
	t.Setenv("ANVIL_SCHEDULER_REQUIRE_PERSISTENCE", "true")
	cfg, err = loadSchedulerConfig()
	if err != nil {
		t.Fatalf("loadSchedulerConfig env: %v", err)
	}
	if cfg.Addr != "0.0.0.0:3999" || cfg.PlacementPath != "/var/lib/anvil/scheduler.json" || cfg.QuotaStorePath != "/var/lib/anvil/tenants.json" {
		t.Fatalf("cfg paths = %+v", cfg)
	}
	if cfg.HostsFile != hostsFile || cfg.PollInterval != 2*time.Second || cfg.ReconcileInterval != 7*time.Second || cfg.HostTimeout != 500*time.Millisecond || cfg.FailureThreshold != 5 {
		t.Fatalf("cfg control loop = %+v", cfg)
	}
	if cfg.APIToken != "scheduler-token" || !cfg.RequirePersistence {
		t.Fatalf("cfg security/persistence = %+v", cfg)
	}
}

func TestLoadSchedulerConfigRejectsInvalidValues(t *testing.T) {
	t.Setenv("ANVIL_SCHEDULER_POLL_INTERVAL", "nope")
	if _, err := loadSchedulerConfig(); err == nil {
		t.Fatal("invalid poll interval error = nil")
	}

	t.Setenv("ANVIL_SCHEDULER_POLL_INTERVAL", "")
	t.Setenv("ANVIL_SCHEDULER_FAILURE_THRESHOLD", "0")
	if _, err := loadSchedulerConfig(); err == nil {
		t.Fatal("invalid failure threshold error = nil")
	}
}

func TestApplyConfiguredHostsMarksManagedAndPreservesObservation(t *testing.T) {
	store := anvilmcp.NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(anvilmcp.RuntimeHost{Name: "host-a", Endpoint: "http://old-host", Healthy: true, AvailableVMs: 9})
	_ = store.SetHostObservation("host-a", anvilmcp.HostObservation{Status: anvilmcp.HostStatusDegraded, FailureCount: 1})

	err := applyConfiguredHosts(store, []anvilmcp.RuntimeHost{{
		Name:                   "host-a",
		Endpoint:               "http://new-host",
		AvailableVMs:           3,
		AvailableSnapshotBytes: 4096,
		EgressPolicies:         []anvilmcp.EgressPolicy{anvilmcp.EgressPolicyProfile},
	}})
	if err != nil {
		t.Fatalf("applyConfiguredHosts: %v", err)
	}
	state := store.State()
	if state.Hosts["host-a"].Endpoint != "http://new-host" {
		t.Fatalf("host endpoint = %q, want config endpoint", state.Hosts["host-a"].Endpoint)
	}
	if state.Hosts["host-a"].AvailableVMs != 3 || state.Hosts["host-a"].AvailableSnapshotBytes != 4096 {
		t.Fatalf("host capacity = %d/%d, want config capacity 3/4096", state.Hosts["host-a"].AvailableVMs, state.Hosts["host-a"].AvailableSnapshotBytes)
	}
	if state.HostObservations["host-a"].FailureCount != 1 {
		t.Fatalf("observation = %+v, want preserved failure count", state.HostObservations["host-a"])
	}
	if !state.ConfigManagedHosts["host-a"] {
		t.Fatalf("config managed hosts = %+v, want host-a", state.ConfigManagedHosts)
	}
}

func TestApplyConfiguredHostsRemovesAbsentManagedHosts(t *testing.T) {
	store := anvilmcp.NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(anvilmcp.RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1})
	_ = store.SetHost(anvilmcp.RuntimeHost{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 2})
	_ = store.SetHost(anvilmcp.RuntimeHost{Name: "runtime-host", Endpoint: "http://runtime-host", Healthy: true, AvailableVMs: 3})
	_ = store.MarkConfigManagedHost("host-a", true)
	_ = store.MarkConfigManagedHost("host-b", true)
	_ = store.SetHostObservation("host-a", anvilmcp.HostObservation{Status: anvilmcp.HostStatusUnhealthy, FailureCount: 3})
	_ = store.SetVMPlacement("vm-1", "host-a")
	_ = store.MarkHostPlacementsSuspect("host-a", "host_unhealthy")

	err := applyConfiguredHosts(store, []anvilmcp.RuntimeHost{{Name: "host-b", Endpoint: "http://host-b-new", EgressPolicies: []anvilmcp.EgressPolicy{anvilmcp.EgressPolicyProfile}}})
	if err != nil {
		t.Fatalf("applyConfiguredHosts: %v", err)
	}
	state := store.State()
	if _, ok := state.Hosts["host-a"]; ok {
		t.Fatalf("removed config host retained: %+v", state.Hosts["host-a"])
	}
	if state.ConfigManagedHosts["host-a"] {
		t.Fatalf("config managed hosts = %+v, want host-a removed", state.ConfigManagedHosts)
	}
	if _, ok := state.HostObservations["host-a"]; ok {
		t.Fatalf("host-a observation retained after config removal: %+v", state.HostObservations["host-a"])
	}
	if _, ok := state.SuspectVMPlacements["vm-1"]; ok {
		t.Fatalf("host-a suspect placement retained after config removal: %+v", state.SuspectVMPlacements["vm-1"])
	}
	if state.Hosts["host-b"].Endpoint != "http://host-b-new" || !state.ConfigManagedHosts["host-b"] {
		t.Fatalf("host-b = %+v managed=%v, want updated managed host", state.Hosts["host-b"], state.ConfigManagedHosts["host-b"])
	}
	if _, ok := state.Hosts["runtime-host"]; !ok {
		t.Fatalf("runtime host removed by config reconciliation: %+v", state.Hosts)
	}
}

func TestApplyConfiguredHostsNoopsWithoutHosts(t *testing.T) {
	store := anvilmcp.NewPlacementStore(t.TempDir())

	if err := applyConfiguredHosts(store, nil); err != nil {
		t.Fatalf("applyConfiguredHosts nil hosts: %v", err)
	}
}

func TestSchedulerProcessLoadsHostsPollsMetricsAndSchedules(t *testing.T) {
	if os.Getenv("ANVIL_SCHEDULER_TEST_CHILD") == "1" {
		return
	}

	fakeDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","vm_count":0}`))
		case "/vms":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeDaemon.Close()

	tempDir := t.TempDir()
	statePath := filepath.Join(tempDir, "scheduler-state.json")
	quotaPath := filepath.Join(tempDir, "scheduler-quotas.json")
	hostsPath := filepath.Join(tempDir, "hosts.json")
	stale := anvilmcp.NewPlacementStore(statePath)
	if err := stale.SetHost(anvilmcp.RuntimeHost{Name: "host-a", Endpoint: "http://stale-host", Healthy: false, AvailableVMs: 0, EgressPolicies: []anvilmcp.EgressPolicy{anvilmcp.EgressPolicyProfile}}); err != nil {
		t.Fatalf("SetHost stale host: %v", err)
	}
	if err := stale.Save(); err != nil {
		t.Fatalf("Save stale scheduler state: %v", err)
	}
	quotas := anvilmcp.NewQuotaStore(quotaPath)
	if err := quotas.SetTenantQuota("tenant-1", anvilmcp.TenantQuota{ActiveVMs: 2}); err != nil {
		t.Fatalf("SetTenantQuota tenant-1: %v", err)
	}
	if err := quotas.Save(); err != nil {
		t.Fatalf("Save scheduler quota store: %v", err)
	}
	hostsJSON := fmt.Sprintf(`{"hosts":[{"name":"host-a","endpoint":%q,"available_vms":2,"available_snapshot_bytes":4096,"egress_policies":["profile"]}]}`, fakeDaemon.URL)
	if err := os.WriteFile(hostsPath, []byte(hostsJSON), 0600); err != nil {
		t.Fatalf("write hosts file: %v", err)
	}

	addr := reserveLoopbackAddr(t)
	baseURL := "http://" + addr
	cmd := exec.Command(os.Args[0], "-test.run=TestSchedulerProcessChildMain", "-test.v")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_TEST_CHILD=1",
		"ANVIL_SCHEDULER_ADDR="+addr,
		"ANVIL_SCHEDULER_STATE="+statePath,
		"ANVIL_SCHEDULER_QUOTA_STORE="+quotaPath,
		"ANVIL_SCHEDULER_HOSTS_FILE="+hostsPath,
		"ANVIL_SCHEDULER_POLL_INTERVAL=50ms",
		"ANVIL_SCHEDULER_RECONCILE_INTERVAL=100ms",
		"ANVIL_SCHEDULER_HOST_TIMEOUT=500ms",
		"ANVIL_SCHEDULER_FAILURE_THRESHOLD=2",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start scheduler child: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	})

	waitForSchedulerBody(t, baseURL+"/health", func(body string) bool {
		return strings.Contains(body, `"status":"ok"`)
	}, &output)
	waitForSchedulerBody(t, baseURL+"/control-loop/status", func(body string) bool {
		return strings.Contains(body, `"running":true`)
	}, &output)
	waitForSchedulerBody(t, baseURL+"/metrics", func(body string) bool {
		return strings.Contains(body, "anvil_scheduler_control_loop_running 1") &&
			strings.Contains(body, "anvil_scheduler_host_status_count{status=\"healthy\"} 1")
	}, &output)

	resp, err := http.Post(baseURL+"/schedule/spawn", "application/json", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
	if err != nil {
		t.Fatalf("POST /schedule/spawn: %v\nscheduler output:\n%s", err, output.String())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /schedule/spawn status = %d, want 200\nscheduler output:\n%s", resp.StatusCode, output.String())
	}
	var decision anvilmcp.ScheduleDecision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		t.Fatalf("decode schedule decision: %v", err)
	}
	if !decision.Allowed || decision.Host.Name != "host-a" || decision.Host.AvailableVMs != 2 {
		t.Fatalf("decision = %+v, want allowed host-a with configured capacity", decision)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal scheduler child: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("scheduler child exited with error: %v\noutput:\n%s", err, output.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("scheduler child did not exit after SIGTERM\noutput:\n%s", output.String())
	}
}

func TestSchedulerProcessChildMain(t *testing.T) {
	if os.Getenv("ANVIL_SCHEDULER_TEST_CHILD") != "1" {
		return
	}
	main()
}

func reserveLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback addr: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}
	return addr
}

func waitForSchedulerBody(t *testing.T, url string, accepts func(string) bool, output *bytes.Buffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastBody string
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		bodyBytes := new(bytes.Buffer)
		_, _ = bodyBytes.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		lastBody = bodyBytes.String()
		if resp.StatusCode == http.StatusOK && accepts(lastBody) {
			return lastBody
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("scheduler endpoint %s did not become ready; lastErr=%v lastBody=%q\nscheduler output:\n%s", url, lastErr, lastBody, output.String())
	return ""
}
