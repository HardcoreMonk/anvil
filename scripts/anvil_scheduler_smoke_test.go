package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAnvilSchedulerSmokePassesAgainstFakeScheduler(t *testing.T) {
	server := newAnvilSchedulerSmokeFakeServer(t, 0)
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "summary.json")
	cmd := exec.Command("bash", "anvil-scheduler-smoke.sh", "--base-url", server.URL, "--host-id", "smoke-test-host", "--json-out", outPath)
	cmd.Dir = scriptsDir(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke script failed: %v\n%s", err, output)
	}

	summary := readSmokeSummary(t, outPath)
	if !summary.OK {
		t.Fatalf("summary ok = false, output=%s summary=%+v", output, summary)
	}
	if summary.HostID != "smoke-test-host" {
		t.Fatalf("host_id = %q, want smoke-test-host", summary.HostID)
	}
	if summary.SelectedHostID != "smoke-test-host" {
		t.Fatalf("selected_host_id = %q, want smoke-test-host", summary.SelectedHostID)
	}
}

func TestAnvilSchedulerSmokeDeletesRegisteredHostAfterSuccess(t *testing.T) {
	server := newAnvilSchedulerSmokeFakeServer(t, 0)
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "summary.json")
	cmd := exec.Command("bash", "anvil-scheduler-smoke.sh", "--base-url", server.URL, "--host-id", "smoke test host", "--json-out", outPath)
	cmd.Dir = scriptsDir(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke script failed: %v\n%s", err, output)
	}

	summary := readSmokeSummary(t, outPath)
	if !summary.OK {
		t.Fatalf("summary ok = false, output=%s summary=%+v", output, summary)
	}
	if server.hasHost("smoke test host") {
		t.Fatalf("fake scheduler still has smoke host after successful smoke:\n%s", output)
	}
}

func TestAnvilSchedulerSmokeFailsSlowHealthWithSummary(t *testing.T) {
	server := newAnvilSchedulerSmokeFakeServer(t, 1500*time.Millisecond)
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "summary.json")
	cmd := exec.Command("bash", "anvil-scheduler-smoke.sh", "--base-url", server.URL, "--host-id", "smoke-test-host", "--json-out", outPath)
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_CONNECT_TIMEOUT=1",
		"ANVIL_SCHEDULER_REQUEST_TIMEOUT=1",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("smoke script unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "health_failed") {
		t.Fatalf("output = %s, want health_failed", output)
	}

	summary := readSmokeSummary(t, outPath)
	if summary.OK {
		t.Fatalf("summary ok = true, want false")
	}
	if summary.FailedStep != "health_failed" {
		t.Fatalf("failed_step = %q, want health_failed", summary.FailedStep)
	}
}

type anvilSchedulerSmokeFakeServer struct {
	*httptest.Server
	mu   sync.Mutex
	host map[string]any
}

func (s *anvilSchedulerSmokeFakeServer) hasHost(hostID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.host == nil {
		return false
	}
	return s.host["name"] == hostID
}

func newAnvilSchedulerSmokeFakeServer(t *testing.T, healthDelay time.Duration) *anvilSchedulerSmokeFakeServer {
	t.Helper()

	otherHost := map[string]any{
		"name":                     "other-eligible-host",
		"endpoint":                 "http://other-eligible-host",
		"healthy":                  true,
		"available_vms":            float64(1),
		"available_snapshot_bytes": float64(4096),
		"egress_policies":          []any{"profile"},
	}
	fake := &anvilSchedulerSmokeFakeServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			if healthDelay > 0 {
				time.Sleep(healthDelay)
			}
			writeSmokeTestJSON(t, w, map[string]string{"status": "ok"})
		case "/hosts":
			switch r.Method {
			case http.MethodPut:
				var next map[string]any
				if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
					http.Error(w, "invalid host body", http.StatusBadRequest)
					return
				}
				if strings.TrimSpace(fmt.Sprint(next["name"])) == "" {
					http.Error(w, "unexpected host name", http.StatusBadRequest)
					return
				}
				fake.mu.Lock()
				fake.host = next
				fake.mu.Unlock()
				writeSmokeTestJSON(t, w, next)
			case http.MethodGet:
				fake.mu.Lock()
				current := fake.host
				fake.mu.Unlock()
				if current == nil {
					writeSmokeTestJSON(t, w, []map[string]any{otherHost})
					return
				}
				writeSmokeTestJSON(t, w, []map[string]any{otherHost, current})
			default:
				http.Error(w, "GET or PUT required", http.StatusMethodNotAllowed)
			}
		case "/schedule/spawn":
			if r.Method != http.MethodPost {
				http.Error(w, "POST required", http.StatusMethodNotAllowed)
				return
			}
			fake.mu.Lock()
			current := fake.host
			fake.mu.Unlock()
			if current == nil {
				http.Error(w, "host missing", http.StatusBadRequest)
				return
			}
			var scheduleReq map[string]any
			if err := json.NewDecoder(r.Body).Decode(&scheduleReq); err != nil {
				http.Error(w, "invalid schedule body", http.StatusBadRequest)
				return
			}
			selected := otherHost
			if preferredHostsInclude(scheduleReq, fmt.Sprint(current["name"])) {
				selected = current
			}
			writeSmokeTestJSON(t, w, map[string]any{
				"allowed":       true,
				"reason":        "scheduled",
				"tenant_id":     "smoke-tenant",
				"host":          selected,
				"egress_policy": "profile",
				"requested":     map[string]any{"active_vms": 1},
			})
		case "/placements":
			if r.Method != http.MethodGet {
				http.Error(w, "GET required", http.StatusMethodNotAllowed)
				return
			}
			fake.mu.Lock()
			current := fake.host
			fake.mu.Unlock()
			hosts := map[string]any{}
			if current != nil {
				hosts[fmt.Sprint(current["name"])] = current
			}
			writeSmokeTestJSON(t, w, map[string]any{
				"hosts":              hosts,
				"vm_placements":      map[string]string{},
				"snapshot_locations": map[string][]string{},
			})
		default:
			if strings.HasPrefix(r.URL.Path, "/hosts/") {
				if r.Method != http.MethodDelete {
					http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
					return
				}
				hostID, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/hosts/"))
				if err != nil || strings.TrimSpace(hostID) == "" {
					http.Error(w, "invalid host path", http.StatusBadRequest)
					return
				}
				fake.mu.Lock()
				deleted := fake.host != nil && fake.host["name"] == hostID
				if deleted {
					fake.host = nil
				}
				fake.mu.Unlock()
				writeSmokeTestJSON(t, w, map[string]any{"deleted": deleted, "host": hostID})
				return
			}
			http.NotFound(w, r)
		}
	}))
	fake.Server = server
	return fake
}

func TestAnvilSchedulerSmokeFailsHealthWithSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "summary.json")
	cmd := exec.Command("bash", "anvil-scheduler-smoke.sh", "--base-url", server.URL, "--json-out", outPath)
	cmd.Dir = scriptsDir(t)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("smoke script unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "health_failed") {
		t.Fatalf("output = %s, want health_failed", output)
	}
	if !strings.Contains(string(output), "not ready") {
		t.Fatalf("output = %s, want response body snippet not ready", output)
	}

	summary := readSmokeSummary(t, outPath)
	if summary.OK {
		t.Fatalf("summary ok = true, want false")
	}
	if summary.FailedStep != "health_failed" {
		t.Fatalf("failed_step = %q, want health_failed", summary.FailedStep)
	}
}

func TestInstallAnvilSchedulerDryRunVerifyPrintsSmokeCommand(t *testing.T) {
	output, err := commandOutput(t, "bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable", "--verify")
	if err != nil {
		t.Fatalf("installer dry-run failed: %v\n%s", err, output)
	}
	requireOutputContains(t, output, "scripts/anvil-scheduler-smoke.sh")
	requireOutputContains(t, output, "--base-url http://127.0.0.1:3010")
}

func TestInstallAnvilSchedulerDryRunVerifyMapsWildcardBindAddressesToLoopback(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{name: "ipv4 wildcard", addr: "0.0.0.0:3010", want: "--base-url http://127.0.0.1:3010"},
		{name: "empty host", addr: ":3010", want: "--base-url http://127.0.0.1:3010"},
		{name: "ipv6 wildcard", addr: "[::]:3010", want: "--base-url http://127.0.0.1:3010"},
		{name: "https url", addr: "https://scheduler.internal:9443", want: "--base-url https://scheduler.internal:9443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable", "--verify")
			cmd.Dir = scriptsDir(t)
			cmd.Env = append(os.Environ(),
				"ANVIL_SCHEDULER_USER=anvil-smoke-user",
				"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
				"ANVIL_SCHEDULER_ADDR="+tc.addr,
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("installer dry-run failed: %v\n%s", err, output)
			}
			requireOutputContains(t, output, tc.want)
		})
	}
}

func TestInstallAnvilSchedulerDryRunCreatesQuotaDirectory(t *testing.T) {
	cmd := exec.Command("bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable")
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_USER=anvil-smoke-user",
		"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
		"ANVIL_SCHEDULER_QUOTA_STORE=/var/lib/anvil/quotas/tenants.json",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer dry-run failed: %v\n%s", err, output)
	}
	requireOutputContains(t, output, "/var/lib/anvil/quotas")
}

func TestInstallAnvilSchedulerWarnsForStateOutsideVarLib(t *testing.T) {
	cmd := exec.Command("bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable")
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_USER=anvil-smoke-user",
		"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
		"ANVIL_SCHEDULER_STATE=/tmp/anvil-scheduler/state.json",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer dry-run failed: %v\n%s", err, output)
	}
	requireOutputContains(t, output, "warning: ANVIL_SCHEDULER_STATE is outside /var/lib/anvil")
}

func TestInstallAnvilSchedulerWarnsForNormalizedStateOutsideVarLib(t *testing.T) {
	cmd := exec.Command("bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable")
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_USER=anvil-smoke-user",
		"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
		"ANVIL_SCHEDULER_STATE=/var/lib/anvil/../anvil-state/state.json",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer dry-run failed: %v\n%s", err, output)
	}
	requireOutputContains(t, output, "warning: ANVIL_SCHEDULER_STATE is outside /var/lib/anvil")
}

func TestInstallAnvilSchedulerRejectsRelativeQuotaStoreBeforeInstallPlan(t *testing.T) {
	cmd := exec.Command("bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build", "--no-enable")
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_USER=anvil-smoke-user",
		"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
		"ANVIL_SCHEDULER_QUOTA_STORE=tenants.json",
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("installer dry-run unexpectedly succeeded:\n%s", output)
	}
	requireOutputContains(t, output, "ANVIL_SCHEDULER_QUOTA_STORE must be an absolute path")
	badInstallPlan := "+ install -d -m 0750 -o anvil-smoke-user -g anvil-smoke-group ."
	if strings.Contains(string(output), badInstallPlan) {
		t.Fatalf("installer printed unsafe install plan %q:\n%s", badInstallPlan, output)
	}
}

func preferredHostsInclude(req map[string]any, hostID string) bool {
	preferredHosts, ok := req["preferred_hosts"].([]any)
	if !ok {
		return false
	}
	for _, preferredHost := range preferredHosts {
		if preferredHost == hostID {
			return true
		}
	}
	return false
}

type smokeSummary struct {
	OK             bool   `json:"ok"`
	BaseURL        string `json:"base_url"`
	HostID         string `json:"host_id"`
	SelectedHostID string `json:"selected_host_id"`
	FailedStep     string `json:"failed_step"`
}

func scriptsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

func writeSmokeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func readSmokeSummary(t *testing.T, path string) smokeSummary {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	var summary smokeSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		t.Fatalf("decode summary %s: %v", data, err)
	}
	if summary.BaseURL == "" {
		t.Fatalf("base_url is empty in summary %+v", summary)
	}
	if summary.HostID == "" {
		t.Fatalf("host_id is empty in summary %+v", summary)
	}
	return summary
}

func requireOutputContains(t *testing.T, output []byte, want string) {
	t.Helper()
	if !strings.Contains(string(output), want) {
		t.Fatalf("output missing %q:\n%s", want, output)
	}
}

func commandOutput(t *testing.T, name string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(),
		"ANVIL_SCHEDULER_USER=anvil-smoke-user",
		"ANVIL_SCHEDULER_GROUP=anvil-smoke-group",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%w", err)
	}
	return output, nil
}
