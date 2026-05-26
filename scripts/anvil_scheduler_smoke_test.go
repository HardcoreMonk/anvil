package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func newAnvilSchedulerSmokeFakeServer(t *testing.T, healthDelay time.Duration) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	var host map[string]any
	otherHost := map[string]any{
		"name":                     "other-eligible-host",
		"endpoint":                 "http://other-eligible-host",
		"healthy":                  true,
		"available_vms":            float64(1),
		"available_snapshot_bytes": float64(4096),
		"egress_policies":          []any{"profile"},
	}
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
				if next["name"] != "smoke-test-host" {
					http.Error(w, "unexpected host name", http.StatusBadRequest)
					return
				}
				mu.Lock()
				host = next
				mu.Unlock()
				writeSmokeTestJSON(t, w, next)
			case http.MethodGet:
				mu.Lock()
				current := host
				mu.Unlock()
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
			mu.Lock()
			current := host
			mu.Unlock()
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
			if preferredHostsInclude(scheduleReq, "smoke-test-host") {
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
			mu.Lock()
			current := host
			mu.Unlock()
			hosts := map[string]any{}
			if current != nil {
				hosts["smoke-test-host"] = current
			}
			writeSmokeTestJSON(t, w, map[string]any{
				"hosts":              hosts,
				"vm_placements":      map[string]string{},
				"snapshot_locations": map[string][]string{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	return server
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

	summary := readSmokeSummary(t, outPath)
	if summary.OK {
		t.Fatalf("summary ok = true, want false")
	}
	if summary.FailedStep != "health_failed" {
		t.Fatalf("failed_step = %q, want health_failed", summary.FailedStep)
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
