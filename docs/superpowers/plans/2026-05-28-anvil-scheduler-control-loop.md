# Anvil Scheduler Control Loop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a store-backed scheduler control loop that polls daemon host health, reconciles VM placements, and exposes operator-visible degraded/unhealthy/suspect state.

**Architecture:** Extend `PlacementStore` with additive control-loop state, add scheduler host bootstrap config parsing, and run a `SchedulerControlLoop` inside `cmd/anvil-scheduler`. The loop updates host observations and suspect placements without changing Firecracker/KVM runtime behavior or adopting upstream ephemera `v0.4.0` PR-A.

**Tech Stack:** Go standard library, existing `internal/anvilmcp` scheduler/placement types, existing Bash smoke harness.

---

## Scope Check

This plan implements one subsystem: anvil scheduler operational control loop. It does not touch upstream sync, VM runtime storage/recovery, Firecracker, guest images, cross-host snapshot replication, flock cross-host placement, UI, billing, or OpenClaw compatibility.

## File Structure

- Modify `internal/anvilmcp/placement_store.go`: additive store fields, host observations, config-managed host tracking, suspect placement helpers, control-loop status helpers.
- Modify `internal/anvilmcp/placement_store_test.go`: persistence and helper behavior tests.
- Create `internal/anvilmcp/scheduler_control_loop.go`: control loop types and poll/reconcile logic.
- Create `internal/anvilmcp/scheduler_control_loop_test.go`: control-loop unit tests.
- Modify `internal/anvilmcp/scheduler.go`: add host status summary to `ScheduleDecision`.
- Modify `internal/anvilmcp/scheduler_service.go`: expose `/control-loop/status`, richer `/placements`, config-managed host delete policy, persistence-required schedule behavior.
- Modify `internal/anvilmcp/scheduler_service_test.go`: endpoint and persistence behavior tests.
- Modify `cmd/anvil-scheduler/main.go`: parse new env vars, load hosts file, merge config hosts, start control loop.
- Modify `cmd/anvil-scheduler/main_test.go`: env parsing and hosts file tests.
- Modify `scripts/anvil-scheduler-smoke.sh`: verify `/control-loop/status`.
- Modify `scripts/anvil_scheduler_smoke_test.go`: fake scheduler support for `/control-loop/status`.
- Modify `README.md`, `docs/operations/runbook.md`, `docs/operations/release-checklist.md`, `docs/architecture/multi-tenant-roadmap.md`: document new scheduler operations.

## Task 1: Placement Store Control-Loop State

**Files:**
- Modify: `internal/anvilmcp/placement_store.go`
- Modify: `internal/anvilmcp/placement_store_test.go`

- [ ] **Step 1: Add failing placement store persistence test**

Append to `internal/anvilmcp/placement_store_test.go`:

```go
func TestPlacementStorePersistsControlLoopState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "placements.json")
	store := NewPlacementStore(path)
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-a: %v", err)
	}
	if err := store.MarkConfigManagedHost("host-a", true); err != nil {
		t.Fatalf("MarkConfigManagedHost: %v", err)
	}
	now := time.Date(2026, 5, 28, 1, 2, 3, 0, time.UTC)
	if err := store.SetHostObservation("host-a", HostObservation{
		Status:                 HostStatusDegraded,
		AvailableVMs:           2,
		AvailableSnapshotBytes: 4096,
		FailureCount:           1,
		LastFailureAt:          now,
		LastError:              "health returned 503",
	}); err != nil {
		t.Fatalf("SetHostObservation: %v", err)
	}
	if err := store.SetVMPlacement("vm-1", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	if err := store.MarkHostPlacementsSuspect("host-a", "host_degraded"); err != nil {
		t.Fatalf("MarkHostPlacementsSuspect: %v", err)
	}
	if err := store.SetControlLoopStatus(ControlLoopStatus{
		Running:                 true,
		PollIntervalSeconds:     10,
		ReconcileIntervalSeconds: 30,
		FailureThreshold:        3,
		PersistenceDegraded:     true,
		LastError:               "replace placement store: permission denied",
	}); err != nil {
		t.Fatalf("SetControlLoopStatus: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	state := reloaded.State()
	if !state.ConfigManagedHosts["host-a"] {
		t.Fatalf("ConfigManagedHosts = %+v, want host-a=true", state.ConfigManagedHosts)
	}
	obs := state.HostObservations["host-a"]
	if obs.Status != HostStatusDegraded || obs.FailureCount != 1 || obs.LastError != "health returned 503" {
		t.Fatalf("HostObservations[host-a] = %+v", obs)
	}
	suspect := state.SuspectVMPlacements["vm-1"]
	if suspect.Host != "host-a" || suspect.Reason != "host_degraded" {
		t.Fatalf("SuspectVMPlacements[vm-1] = %+v", suspect)
	}
	if !state.ControlLoopStatus.Running || !state.ControlLoopStatus.PersistenceDegraded {
		t.Fatalf("ControlLoopStatus = %+v", state.ControlLoopStatus)
	}
}
```

Add `time` to the import list:

```go
import (
	"path/filepath"
	"testing"
	"time"
)
```

- [ ] **Step 2: Run the focused test to verify RED**

Run:

```bash
go test ./internal/anvilmcp -run TestPlacementStorePersistsControlLoopState -count=1
```

Expected: FAIL with undefined identifiers such as `HostObservation`, `HostStatusDegraded`, `ControlLoopStatus`, `MarkConfigManagedHost`, `SetHostObservation`, or `MarkHostPlacementsSuspect`.

- [ ] **Step 3: Add control-loop state types and store fields**

In `internal/anvilmcp/placement_store.go`, add these types near `PlacementStoreState`:

```go
type HostStatus string

const (
	HostStatusHealthy   HostStatus = "healthy"
	HostStatusDegraded  HostStatus = "degraded"
	HostStatusUnhealthy HostStatus = "unhealthy"
)

type HostObservation struct {
	Status                 HostStatus `json:"status"`
	AvailableVMs           int64      `json:"available_vms"`
	AvailableSnapshotBytes int64      `json:"available_snapshot_bytes"`
	FailureCount           int        `json:"failure_count"`
	LastSuccessAt           time.Time  `json:"last_success_at,omitempty"`
	LastFailureAt           time.Time  `json:"last_failure_at,omitempty"`
	LastError               string     `json:"last_error,omitempty"`
}

type SuspectVMPlacement struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

type ControlLoopStatus struct {
	Running                    bool                       `json:"running"`
	PollIntervalSeconds        int64                      `json:"poll_interval_seconds"`
	ReconcileIntervalSeconds   int64                      `json:"reconcile_interval_seconds"`
	FailureThreshold           int                        `json:"failure_threshold"`
	PersistenceDegraded        bool                       `json:"persistence_degraded"`
	LastPollStartedAt          time.Time                  `json:"last_poll_started_at,omitempty"`
	LastPollCompletedAt        time.Time                  `json:"last_poll_completed_at,omitempty"`
	LastReconcileStartedAt     time.Time                  `json:"last_reconcile_started_at,omitempty"`
	LastReconcileCompletedAt   time.Time                  `json:"last_reconcile_completed_at,omitempty"`
	LastError                  string                     `json:"last_error,omitempty"`
	Hosts                      map[string]HostObservation `json:"hosts,omitempty"`
}
```

Add `time` to the import list in `placement_store.go`.

Extend `PlacementStoreState`:

```go
type PlacementStoreState struct {
	Hosts               map[string]RuntimeHost        `json:"hosts"`
	VMPlacements        map[string]string             `json:"vm_placements"`
	SnapshotLocations   map[string][]string           `json:"snapshot_locations"`
	ConfigManagedHosts  map[string]bool               `json:"config_managed_hosts,omitempty"`
	HostObservations    map[string]HostObservation    `json:"host_observations,omitempty"`
	SuspectVMPlacements map[string]SuspectVMPlacement `json:"suspect_vm_placements,omitempty"`
	ControlLoopStatus   ControlLoopStatus             `json:"control_loop_status,omitempty"`
}
```

- [ ] **Step 4: Normalize and clone new store fields**

Update `NewPlacementStore`, `State`, `normalizePlacementStoreState`, and `clonePlacementStoreState` so every map is initialized and cloned.

Use this pattern inside `normalizePlacementStoreState`:

```go
if state.ConfigManagedHosts == nil {
	state.ConfigManagedHosts = make(map[string]bool)
}
if state.HostObservations == nil {
	state.HostObservations = make(map[string]HostObservation)
}
if state.SuspectVMPlacements == nil {
	state.SuspectVMPlacements = make(map[string]SuspectVMPlacement)
}
if state.ControlLoopStatus.Hosts == nil {
	state.ControlLoopStatus.Hosts = make(map[string]HostObservation)
}
```

Use this pattern inside `clonePlacementStoreState`:

```go
out.ConfigManagedHosts = make(map[string]bool, len(state.ConfigManagedHosts))
for host, managed := range state.ConfigManagedHosts {
	out.ConfigManagedHosts[host] = managed
}
out.HostObservations = make(map[string]HostObservation, len(state.HostObservations))
for host, obs := range state.HostObservations {
	out.HostObservations[host] = obs
}
out.SuspectVMPlacements = make(map[string]SuspectVMPlacement, len(state.SuspectVMPlacements))
for vmID, suspect := range state.SuspectVMPlacements {
	out.SuspectVMPlacements[vmID] = suspect
}
out.ControlLoopStatus = state.ControlLoopStatus
out.ControlLoopStatus.Hosts = make(map[string]HostObservation, len(state.ControlLoopStatus.Hosts))
for host, obs := range state.ControlLoopStatus.Hosts {
	out.ControlLoopStatus.Hosts[host] = obs
}
```

- [ ] **Step 5: Add store helper methods**

Add these methods to `internal/anvilmcp/placement_store.go`:

```go
func (s *PlacementStore) MarkConfigManagedHost(name string, managed bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	if managed {
		s.state.ConfigManagedHosts[name] = true
	} else {
		delete(s.state.ConfigManagedHosts, name)
	}
	return nil
}

func (s *PlacementStore) IsConfigManagedHost(name string) bool {
	name = strings.TrimSpace(name)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.ConfigManagedHosts[name]
}

func (s *PlacementStore) SetHostObservation(name string, obs HostObservation) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	if obs.Status == "" {
		obs.Status = HostStatusUnhealthy
	}
	obs.LastError = strings.TrimSpace(obs.LastError)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	s.state.HostObservations[name] = obs
	s.state.ControlLoopStatus.Hosts[name] = obs
	return nil
}

func (s *PlacementStore) HostObservation(name string) (HostObservation, bool) {
	name = strings.TrimSpace(name)
	s.mu.RLock()
	defer s.mu.RUnlock()
	obs, ok := s.state.HostObservations[name]
	return obs, ok
}

func (s *PlacementStore) MarkHostPlacementsSuspect(hostName, reason string) error {
	hostName = strings.TrimSpace(hostName)
	reason = strings.TrimSpace(reason)
	if hostName == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	if reason == "" {
		return fmt.Errorf("suspect reason must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	for vmID, placedHost := range s.state.VMPlacements {
		if placedHost == hostName {
			s.state.SuspectVMPlacements[vmID] = SuspectVMPlacement{Host: hostName, Reason: reason}
		}
	}
	return nil
}

func (s *PlacementStore) ClearHostSuspectPlacements(hostName string) {
	hostName = strings.TrimSpace(hostName)
	s.mu.Lock()
	defer s.mu.Unlock()
	for vmID, suspect := range s.state.SuspectVMPlacements {
		if suspect.Host == hostName {
			delete(s.state.SuspectVMPlacements, vmID)
		}
	}
}

func (s *PlacementStore) SetControlLoopStatus(status ControlLoopStatus) error {
	if status.Hosts == nil {
		status.Hosts = make(map[string]HostObservation)
	}
	status.LastError = strings.TrimSpace(status.LastError)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	s.state.ControlLoopStatus = status
	return nil
}
```

- [ ] **Step 6: Run focused placement store tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestPlacementStore' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit state model**

Run:

```bash
git add internal/anvilmcp/placement_store.go internal/anvilmcp/placement_store_test.go
git commit -m "feat: persist scheduler control loop state"
```

## Task 2: Scheduler Host Config Loading

**Files:**
- Create: `internal/anvilmcp/scheduler_hosts_config.go`
- Create: `internal/anvilmcp/scheduler_hosts_config_test.go`
- Modify: `cmd/anvil-scheduler/main.go`
- Modify: `cmd/anvil-scheduler/main_test.go`

- [ ] **Step 1: Write failing hosts file parser tests**

Create `internal/anvilmcp/scheduler_hosts_config_test.go`:

```go
package anvilmcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSchedulerHostsFileParsesHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.json")
	writeTestFile(t, path, `{"hosts":[{"name":"host-a","endpoint":"http://127.0.0.1:3000","egress_policies":["profile","deny_all"],"smoke_only":true}]}`)

	hosts, err := LoadSchedulerHostsFile(path)
	if err != nil {
		t.Fatalf("LoadSchedulerHostsFile: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("hosts len = %d, want 1", len(hosts))
	}
	host := hosts[0]
	if host.Name != "host-a" || host.Endpoint != "http://127.0.0.1:3000" || !host.SmokeOnly {
		t.Fatalf("host = %+v", host)
	}
	if len(host.EgressPolicies) != 2 || host.EgressPolicies[0] != EgressPolicyProfile || host.EgressPolicies[1] != EgressPolicyDenyAll {
		t.Fatalf("egress policies = %+v", host.EgressPolicies)
	}
}

func TestLoadSchedulerHostsFileRejectsInvalidHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.json")
	writeTestFile(t, path, `{"hosts":[{"name":"","endpoint":"http://127.0.0.1:3000"}]}`)

	if _, err := LoadSchedulerHostsFile(path); err == nil {
		t.Fatal("LoadSchedulerHostsFile error = nil, want invalid host error")
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
```

- [ ] **Step 2: Run parser tests to verify RED**

Run:

```bash
go test ./internal/anvilmcp -run TestLoadSchedulerHostsFile -count=1
```

Expected: FAIL with `undefined: LoadSchedulerHostsFile`.

- [ ] **Step 3: Implement hosts file parser**

Create `internal/anvilmcp/scheduler_hosts_config.go`:

```go
package anvilmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type SchedulerHostsConfig struct {
	Hosts []RuntimeHost `json:"hosts"`
}

func LoadSchedulerHostsFile(path string) ([]RuntimeHost, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scheduler hosts file: %w", err)
	}
	var cfg SchedulerHostsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse scheduler hosts file: %w", err)
	}
	hosts := make([]RuntimeHost, 0, len(cfg.Hosts))
	seen := make(map[string]bool, len(cfg.Hosts))
	for _, host := range cfg.Hosts {
		host.Name = strings.TrimSpace(host.Name)
		host.Endpoint = strings.TrimRight(strings.TrimSpace(host.Endpoint), "/")
		if host.Name == "" {
			return nil, fmt.Errorf("scheduler host name must be non-empty")
		}
		if host.Endpoint == "" {
			return nil, fmt.Errorf("scheduler host %q endpoint must be non-empty", host.Name)
		}
		if seen[host.Name] {
			return nil, fmt.Errorf("duplicate scheduler host %q", host.Name)
		}
		seen[host.Name] = true
		if len(host.EgressPolicies) == 0 {
			host.EgressPolicies = []EgressPolicy{EgressPolicyProfile}
		}
		for _, policy := range host.EgressPolicies {
			if _, err := NormalizeEgressPolicy(string(policy)); err != nil {
				return nil, fmt.Errorf("scheduler host %q: %w", host.Name, err)
			}
		}
		hosts = append(hosts, RuntimeHost{
			Name:           host.Name,
			Endpoint:       host.Endpoint,
			EgressPolicies: host.EgressPolicies,
			SmokeOnly:      host.SmokeOnly,
		})
	}
	return hosts, nil
}
```

- [ ] **Step 4: Add scheduler config env tests**

Replace `cmd/anvil-scheduler/main_test.go` with tests for error-returning config:

```go
package main

import (
	"path/filepath"
	"testing"
	"time"
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
```

- [ ] **Step 5: Run cmd config tests to verify RED**

Run:

```bash
go test ./cmd/anvil-scheduler -run TestLoadSchedulerConfig -count=1
```

Expected: FAIL because `loadSchedulerConfig` does not return `(schedulerConfig, error)` and new fields are missing.

- [ ] **Step 6: Implement scheduler config parsing**

Update `cmd/anvil-scheduler/main.go`:

```go
type schedulerConfig struct {
	Addr              string
	PlacementPath     string
	QuotaStorePath    string
	HostsFile         string
	PollInterval      time.Duration
	ReconcileInterval time.Duration
	HostTimeout       time.Duration
	FailureThreshold  int
	APIToken          string
	RequirePersistence bool
}

func loadSchedulerConfig() (schedulerConfig, error) {
	addr := strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_ADDR"))
	if addr == "" {
		addr = defaultSchedulerAddr
	}
	pollInterval, err := durationEnv("ANVIL_SCHEDULER_POLL_INTERVAL", 10*time.Second)
	if err != nil {
		return schedulerConfig{}, err
	}
	reconcileInterval, err := durationEnv("ANVIL_SCHEDULER_RECONCILE_INTERVAL", 30*time.Second)
	if err != nil {
		return schedulerConfig{}, err
	}
	hostTimeout, err := durationEnv("ANVIL_SCHEDULER_HOST_TIMEOUT", 3*time.Second)
	if err != nil {
		return schedulerConfig{}, err
	}
	failureThreshold, err := intEnv("ANVIL_SCHEDULER_FAILURE_THRESHOLD", 3)
	if err != nil {
		return schedulerConfig{}, err
	}
	if failureThreshold < 1 {
		return schedulerConfig{}, fmt.Errorf("ANVIL_SCHEDULER_FAILURE_THRESHOLD must be >= 1")
	}
	return schedulerConfig{
		Addr:               addr,
		PlacementPath:      strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_STATE")),
		QuotaStorePath:     strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_QUOTA_STORE")),
		HostsFile:          strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_HOSTS_FILE")),
		PollInterval:       pollInterval,
		ReconcileInterval:  reconcileInterval,
		HostTimeout:        hostTimeout,
		FailureThreshold:   failureThreshold,
		APIToken:           strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_API_TOKEN")),
		RequirePersistence: strings.EqualFold(strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_REQUIRE_PERSISTENCE")), "true"),
	}, nil
}
```

Add helpers:

```go
func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be > 0", name)
	}
	return parsed, nil
}

func intEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}
```

Update imports in `cmd/anvil-scheduler/main.go`:

```go
import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ephemera/internal/anvilmcp"
)
```

- [ ] **Step 7: Run config tests**

Run:

```bash
go test ./internal/anvilmcp -run TestLoadSchedulerHostsFile -count=1
go test ./cmd/anvil-scheduler -run TestLoadSchedulerConfig -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit config loading**

Run:

```bash
git add internal/anvilmcp/scheduler_hosts_config.go internal/anvilmcp/scheduler_hosts_config_test.go cmd/anvil-scheduler/main.go cmd/anvil-scheduler/main_test.go
git commit -m "feat: load scheduler host bootstrap config"
```

## Task 3: Scheduler Control Loop Poll and Reconcile

**Files:**
- Create: `internal/anvilmcp/scheduler_control_loop.go`
- Create: `internal/anvilmcp/scheduler_control_loop_test.go`
- Modify: `internal/anvilmcp/placement_store.go`

- [ ] **Step 1: Write failing control loop poll tests**

Create `internal/anvilmcp/scheduler_control_loop_test.go`:

```go
package anvilmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSchedulerControlLoopPollTransitionsHostStatus(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/health" {
			t.Fatalf("path = %s, want /health", r.URL.Path)
		}
		if calls == 1 || calls == 2 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","available_vms":4,"available_snapshot_bytes":8192,"egress_policies":["profile"]}`))
	}))
	defer server.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: server.URL, Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost: %v", err)
	}
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        server.Client(),
		HostTimeout:       time.Second,
		FailureThreshold:  2,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("first PollOnce: %v", err)
	}
	state := store.State()
	if state.HostObservations["host-a"].Status != HostStatusDegraded {
		t.Fatalf("first status = %+v, want degraded", state.HostObservations["host-a"])
	}
	if state.Hosts["host-a"].Healthy {
		t.Fatalf("host healthy = true after degraded poll")
	}

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	state = store.State()
	if state.HostObservations["host-a"].Status != HostStatusUnhealthy {
		t.Fatalf("second status = %+v, want unhealthy", state.HostObservations["host-a"])
	}

	if err := loop.PollOnce(context.Background()); err != nil {
		t.Fatalf("third PollOnce: %v", err)
	}
	state = store.State()
	if state.HostObservations["host-a"].Status != HostStatusHealthy || state.Hosts["host-a"].AvailableVMs != 4 {
		t.Fatalf("third status/host = %+v host=%+v, want healthy capacity 4", state.HostObservations["host-a"], state.Hosts["host-a"])
	}
}
```

- [ ] **Step 2: Write failing reconciliation suspect placement test**

Append:

```go
func TestSchedulerControlLoopReconcilePreservesSuspectPlacementsForUnhealthyHost(t *testing.T) {
	healthyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vms":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]VMInfo{{VMID: "vm-live"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer healthyServer.Close()

	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: healthyServer.URL, Healthy: true})
	_ = store.SetHost(RuntimeHost{Name: "host-b", Endpoint: "http://127.0.0.1:1", Healthy: false})
	_ = store.SetHostObservation("host-b", HostObservation{Status: HostStatusUnhealthy, FailureCount: 3})
	_ = store.SetVMPlacement("vm-stale", "host-b")
	loop := NewSchedulerControlLoop(store, SchedulerControlLoopOptions{
		HTTPClient:        healthyServer.Client(),
		HostTimeout:       50 * time.Millisecond,
		FailureThreshold:  3,
		PollInterval:      time.Second,
		ReconcileInterval: time.Second,
	})

	if err := loop.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	state := store.State()
	if state.VMPlacements["vm-live"] != "host-a" {
		t.Fatalf("vm-live placement = %q, want host-a", state.VMPlacements["vm-live"])
	}
	if state.VMPlacements["vm-stale"] != "host-b" {
		t.Fatalf("vm-stale placement removed, state=%+v", state.VMPlacements)
	}
	suspect := state.SuspectVMPlacements["vm-stale"]
	if suspect.Host != "host-b" || suspect.Reason != "host_unhealthy" {
		t.Fatalf("vm-stale suspect = %+v, want host-b host_unhealthy", suspect)
	}
}
```

- [ ] **Step 3: Run control loop tests to verify RED**

Run:

```bash
go test ./internal/anvilmcp -run TestSchedulerControlLoop -count=1
```

Expected: FAIL with `undefined: NewSchedulerControlLoop`.

- [ ] **Step 4: Implement control loop constructor and options**

Create `internal/anvilmcp/scheduler_control_loop.go`:

```go
package anvilmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type SchedulerControlLoopOptions struct {
	HTTPClient        *http.Client
	APIToken          string
	HostTimeout       time.Duration
	FailureThreshold  int
	PollInterval      time.Duration
	ReconcileInterval time.Duration
}

type SchedulerControlLoop struct {
	store             *PlacementStore
	http              *http.Client
	apiToken          string
	hostTimeout       time.Duration
	failureThreshold  int
	pollInterval      time.Duration
	reconcileInterval time.Duration
	mu                sync.Mutex
	persistenceError  string
}

func NewSchedulerControlLoop(store *PlacementStore, opts SchedulerControlLoopOptions) *SchedulerControlLoop {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	hostTimeout := opts.HostTimeout
	if hostTimeout <= 0 {
		hostTimeout = defaultHostInventoryTimeout
	}
	failureThreshold := opts.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 10 * time.Second
	}
	reconcileInterval := opts.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = 30 * time.Second
	}
	return &SchedulerControlLoop{
		store:             store,
		http:              httpClient,
		apiToken:          strings.TrimSpace(opts.APIToken),
		hostTimeout:       hostTimeout,
		failureThreshold:  failureThreshold,
		pollInterval:      pollInterval,
		reconcileInterval: reconcileInterval,
	}
}
```

- [ ] **Step 5: Implement `PollOnce`**

Add to `scheduler_control_loop.go`:

```go
func (l *SchedulerControlLoop) PollOnce(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store == nil {
		return fmt.Errorf("scheduler control loop placement store is nil")
	}
	started := time.Now().UTC()
	state := l.store.State()
	for _, host := range state.Hosts {
		previous := state.HostObservations[host.Name]
		nextHost, obs := l.pollHost(ctx, host, previous, started)
		if err := l.store.SetHost(nextHost); err != nil {
			l.recordPersistenceError(started, err)
			continue
		}
		if err := l.store.SetHostObservation(nextHost.Name, obs); err != nil {
			l.recordPersistenceError(started, err)
			continue
		}
		if obs.Status == HostStatusHealthy {
			l.store.ClearHostSuspectPlacements(nextHost.Name)
		} else {
			reason := "host_degraded"
			if obs.Status == HostStatusUnhealthy {
				reason = "host_unhealthy"
			}
			if err := l.store.MarkHostPlacementsSuspect(nextHost.Name, reason); err != nil {
				l.recordPersistenceError(started, err)
			}
		}
	}
	completed := time.Now().UTC()
	l.updateStatus(started, completed, false, true)
	if err := l.store.Save(); err != nil {
		l.recordPersistenceError(completed, err)
	}
	return nil
}
```

Add `pollHost`, `hostHealthResponse` reuse is already in `host_inventory.go`; keep a local response type name different from `hostHealthResponse` if needed:

```go
func (l *SchedulerControlLoop) pollHost(ctx context.Context, host RuntimeHost, previous HostObservation, now time.Time) (RuntimeHost, HostObservation) {
	endpoint := strings.TrimRight(strings.TrimSpace(host.Endpoint), "/")
	if endpoint == "" {
		return l.failedHost(host, previous, now, "host endpoint is empty")
	}
	reqCtx, cancel := context.WithTimeout(ctx, l.hostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint+"/health", nil)
	if err != nil {
		return l.failedHost(host, previous, now, err.Error())
	}
	if l.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.apiToken)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return l.failedHost(host, previous, now, err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return l.failedHost(host, previous, now, fmt.Sprintf("health returned %d", resp.StatusCode))
	}
	var health hostHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return l.failedHost(host, previous, now, err.Error())
	}
	if strings.ToLower(strings.TrimSpace(health.Status)) != "ok" {
		return l.failedHost(host, previous, now, "health status is not ok")
	}
	host.Healthy = true
	host.AvailableVMs = health.AvailableVMs
	host.AvailableSnapshotBytes = health.AvailableSnapshotBytes
	host.EgressPolicies = append([]EgressPolicy(nil), health.EgressPolicies...)
	return host, HostObservation{
		Status:                 HostStatusHealthy,
		AvailableVMs:           health.AvailableVMs,
		AvailableSnapshotBytes: health.AvailableSnapshotBytes,
		LastSuccessAt:          now,
	}
}

func (l *SchedulerControlLoop) failedHost(host RuntimeHost, previous HostObservation, now time.Time, message string) (RuntimeHost, HostObservation) {
	host.Healthy = false
	failures := previous.FailureCount + 1
	status := HostStatusDegraded
	if failures >= l.failureThreshold {
		status = HostStatusUnhealthy
	}
	return host, HostObservation{
		Status:                 status,
		AvailableVMs:           previous.AvailableVMs,
		AvailableSnapshotBytes: previous.AvailableSnapshotBytes,
		FailureCount:           failures,
		LastSuccessAt:          previous.LastSuccessAt,
		LastFailureAt:          now,
		LastError:              sanitizeSchedulerError(message),
	}
}
```

- [ ] **Step 6: Implement `ReconcileOnce` and status helpers**

Add to `scheduler_control_loop.go`:

```go
func (l *SchedulerControlLoop) ReconcileOnce(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store == nil {
		return fmt.Errorf("scheduler control loop placement store is nil")
	}
	started := time.Now().UTC()
	state := l.store.State()
	next := make(map[string]string)
	for vmID, hostName := range state.VMPlacements {
		if obs := state.HostObservations[hostName]; obs.Status == HostStatusDegraded || obs.Status == HostStatusUnhealthy {
			next[vmID] = hostName
			reason := "host_degraded"
			if obs.Status == HostStatusUnhealthy {
				reason = "host_unhealthy"
			}
			_ = l.store.MarkHostPlacementsSuspect(hostName, reason)
		}
	}
	for _, host := range state.Hosts {
		obs := state.HostObservations[host.Name]
		if obs.Status != HostStatusHealthy {
			continue
		}
		vms, err := l.listHostVMs(ctx, host)
		if err != nil {
			nextHost, nextObs := l.failedHost(host, obs, started, err.Error())
			_ = l.store.SetHost(nextHost)
			_ = l.store.SetHostObservation(host.Name, nextObs)
			_ = l.store.MarkHostPlacementsSuspect(host.Name, "host_degraded")
			continue
		}
		l.store.ClearHostSuspectPlacements(host.Name)
		for _, vm := range vms {
			vmID := strings.TrimSpace(vm.VMID)
			if vmID != "" {
				next[vmID] = host.Name
			}
		}
	}
	if err := l.store.ReplaceVMPlacements(next); err != nil {
		l.recordPersistenceError(started, err)
	}
	completed := time.Now().UTC()
	l.updateStatus(started, completed, true, true)
	if err := l.store.Save(); err != nil {
		l.recordPersistenceError(completed, err)
	}
	return nil
}
```

Add helper methods:

```go
func (l *SchedulerControlLoop) listHostVMs(ctx context.Context, host RuntimeHost) ([]VMInfo, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(host.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("host endpoint is empty")
	}
	reqCtx, cancel := context.WithTimeout(ctx, l.hostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint+"/vms", nil)
	if err != nil {
		return nil, err
	}
	if l.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+l.apiToken)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list vms returned %d", resp.StatusCode)
	}
	var vms []VMInfo
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		return nil, err
	}
	return vms, nil
}

func (l *SchedulerControlLoop) updateStatus(started, completed time.Time, reconcile bool, running bool) {
	state := l.store.State()
	status := state.ControlLoopStatus
	status.Running = running
	status.PollIntervalSeconds = int64(l.pollInterval.Seconds())
	status.ReconcileIntervalSeconds = int64(l.reconcileInterval.Seconds())
	status.FailureThreshold = l.failureThreshold
	status.PersistenceDegraded = l.persistenceError != ""
	status.LastError = l.persistenceError
	status.Hosts = state.HostObservations
	if reconcile {
		status.LastReconcileStartedAt = started
		status.LastReconcileCompletedAt = completed
	} else {
		status.LastPollStartedAt = started
		status.LastPollCompletedAt = completed
	}
	_ = l.store.SetControlLoopStatus(status)
}

func (l *SchedulerControlLoop) recordPersistenceError(now time.Time, err error) {
	l.persistenceError = sanitizeSchedulerError(err.Error())
	status := l.store.State().ControlLoopStatus
	status.PersistenceDegraded = true
	status.LastError = l.persistenceError
	status.LastPollCompletedAt = now
	_ = l.store.SetControlLoopStatus(status)
}

func sanitizeSchedulerError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 240 {
		return value[:240]
	}
	return value
}
```

- [ ] **Step 7: Implement `Start`**

Add:

```go
func (l *SchedulerControlLoop) Start(ctx context.Context) {
	go func() {
		_ = l.PollOnce(ctx)
		_ = l.ReconcileOnce(ctx)
		pollTicker := time.NewTicker(l.pollInterval)
		reconcileTicker := time.NewTicker(l.reconcileInterval)
		defer pollTicker.Stop()
		defer reconcileTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pollTicker.C:
				_ = l.PollOnce(ctx)
			case <-reconcileTicker.C:
				_ = l.ReconcileOnce(ctx)
			}
		}
	}()
}
```

- [ ] **Step 8: Run control loop tests**

Run:

```bash
go test ./internal/anvilmcp -run TestSchedulerControlLoop -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit control loop core**

Run:

```bash
git add internal/anvilmcp/scheduler_control_loop.go internal/anvilmcp/scheduler_control_loop_test.go internal/anvilmcp/placement_store.go
git commit -m "feat: add scheduler control loop"
```

## Task 4: Scheduler Service API Integration

**Files:**
- Modify: `internal/anvilmcp/scheduler.go`
- Modify: `internal/anvilmcp/scheduler_service.go`
- Modify: `internal/anvilmcp/scheduler_service_test.go`

- [ ] **Step 1: Write failing service tests**

Append to `internal/anvilmcp/scheduler_service_test.go`:

```go
func TestSchedulerServiceControlLoopStatusEndpoint(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusUnhealthy, FailureCount: 3, LastError: "health returned 503"})
	_ = store.SetControlLoopStatus(ControlLoopStatus{Running: true, PollIntervalSeconds: 10, ReconcileIntervalSeconds: 30, FailureThreshold: 3, Hosts: map[string]HostObservation{"host-a": {Status: HostStatusUnhealthy, FailureCount: 3}}})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodGet, "/control-loop/status", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /control-loop/status status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var status ControlLoopStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Running || status.Hosts["host-a"].Status != HostStatusUnhealthy {
		t.Fatalf("status = %+v", status)
	}
}

func TestSchedulerServiceRejectsDeleteConfigManagedHost(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1})
	_ = store.MarkConfigManagedHost("host-a", true)
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodDelete, "/hosts/host-a", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("DELETE config-managed host status = %d body=%s, want 409", rr.Code, rr.Body.String())
	}
}

func TestSchedulerServiceScheduleIncludesHostStatusSummary(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: false, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusDegraded, FailureCount: 1})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodPost, "/schedule/spawn", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /schedule/spawn status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var decision ScheduleDecision
	if err := json.Unmarshal(rr.Body.Bytes(), &decision); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if decision.Allowed || decision.HostStatusSummary.Degraded != 1 {
		t.Fatalf("decision = %+v, want denied with degraded host summary", decision)
	}
}
```

- [ ] **Step 2: Run service tests to verify RED**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerService(ControlLoopStatusEndpoint|RejectsDeleteConfigManagedHost|ScheduleIncludesHostStatusSummary)' -count=1
```

Expected: FAIL because endpoint and `HostStatusSummary` are missing.

- [ ] **Step 3: Add schedule host status summary**

In `internal/anvilmcp/scheduler.go`, add:

```go
type HostStatusSummary struct {
	Healthy   int `json:"healthy"`
	Degraded  int `json:"degraded"`
	Unhealthy int `json:"unhealthy"`
	Unknown   int `json:"unknown"`
}
```

Extend `ScheduleDecision`:

```go
HostStatusSummary HostStatusSummary `json:"host_status_summary"`
```

Add helper:

```go
func SummarizeHostStatuses(hosts []RuntimeHost, observations map[string]HostObservation) HostStatusSummary {
	var summary HostStatusSummary
	for _, host := range hosts {
		obs, ok := observations[host.Name]
		if !ok {
			if host.Healthy {
				summary.Healthy++
			} else {
				summary.Unknown++
			}
			continue
		}
		switch obs.Status {
		case HostStatusHealthy:
			summary.Healthy++
		case HostStatusDegraded:
			summary.Degraded++
		case HostStatusUnhealthy:
			summary.Unhealthy++
		default:
			summary.Unknown++
		}
	}
	return summary
}
```

Set `HostStatusSummary` in `SchedulerService.handleSchedule`, because service has access to store observations:

```go
state := s.placements.State()
decision.HostStatusSummary = SummarizeHostStatuses(s.placements.ListHosts(), state.HostObservations)
```

- [ ] **Step 4: Add `/control-loop/status` handler and config-managed delete guard**

Extend `SchedulerServiceOptions` and `SchedulerService` so Task 6 can add the
persistence gate without another constructor signature change:

```go
type SchedulerServiceOptions struct {
	PlacementStore     *PlacementStore
	QuotaStore         *QuotaStore
	RequirePersistence bool
}

type SchedulerService struct {
	placements         *PlacementStore
	quotas             *QuotaStore
	requirePersistence bool
}
```

Set it in the constructor:

```go
return &SchedulerService{placements: placements, quotas: quotas, requirePersistence: opts.RequirePersistence}
```

Update `SchedulerService.Handler`:

```go
mux.HandleFunc("/control-loop/status", s.handleControlLoopStatus)
```

Add handler:

```go
func (s *SchedulerService) handleControlLoopStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	writeSchedulerJSON(w, s.placements.State().ControlLoopStatus)
}
```

In `handleHostItem`, before `RemoveHostAndSave`:

```go
if s.placements.IsConfigManagedHost(name) {
	http.Error(w, "config-managed host must be removed from hosts file", http.StatusConflict)
	return
}
```

- [ ] **Step 5: Run service tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestSchedulerService|TestScheduler' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit service integration**

Run:

```bash
git add internal/anvilmcp/scheduler.go internal/anvilmcp/scheduler_service.go internal/anvilmcp/scheduler_service_test.go
git commit -m "feat: expose scheduler control loop status"
```

## Task 5: Wire Control Loop Into `cmd/anvil-scheduler`

**Files:**
- Modify: `cmd/anvil-scheduler/main.go`
- Modify: `cmd/anvil-scheduler/main_test.go`

- [ ] **Step 1: Add failing config host merge test**

Append to `cmd/anvil-scheduler/main_test.go`:

```go
func TestApplyConfiguredHostsMarksManagedAndPreservesObservation(t *testing.T) {
	store := anvilmcp.NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(anvilmcp.RuntimeHost{Name: "host-a", Endpoint: "http://old-host", Healthy: true, AvailableVMs: 9})
	_ = store.SetHostObservation("host-a", anvilmcp.HostObservation{Status: anvilmcp.HostStatusDegraded, FailureCount: 1})

	err := applyConfiguredHosts(store, []anvilmcp.RuntimeHost{{Name: "host-a", Endpoint: "http://new-host", EgressPolicies: []anvilmcp.EgressPolicy{anvilmcp.EgressPolicyProfile}}})
	if err != nil {
		t.Fatalf("applyConfiguredHosts: %v", err)
	}
	state := store.State()
	if state.Hosts["host-a"].Endpoint != "http://new-host" {
		t.Fatalf("host endpoint = %q, want config endpoint", state.Hosts["host-a"].Endpoint)
	}
	if state.HostObservations["host-a"].FailureCount != 1 {
		t.Fatalf("observation = %+v, want preserved failure count", state.HostObservations["host-a"])
	}
	if !state.ConfigManagedHosts["host-a"] {
		t.Fatalf("config managed hosts = %+v, want host-a", state.ConfigManagedHosts)
	}
}
```

Add import alias:

```go
import (
	"path/filepath"
	"testing"
	"time"

	"ephemera/internal/anvilmcp"
)
```

- [ ] **Step 2: Run merge test to verify RED**

Run:

```bash
go test ./cmd/anvil-scheduler -run TestApplyConfiguredHosts -count=1
```

Expected: FAIL with `undefined: applyConfiguredHosts`.

- [ ] **Step 3: Implement configured host merge**

Add to `cmd/anvil-scheduler/main.go`:

```go
func applyConfiguredHosts(store *anvilmcp.PlacementStore, hosts []anvilmcp.RuntimeHost) error {
	for _, host := range hosts {
		existing, ok := store.Host(host.Name)
		if ok {
			host.Healthy = existing.Healthy
			host.AvailableVMs = existing.AvailableVMs
			host.AvailableSnapshotBytes = existing.AvailableSnapshotBytes
		}
		if err := store.SetHost(host); err != nil {
			return err
		}
		if err := store.MarkConfigManagedHost(host.Name, true); err != nil {
			return err
		}
	}
	return store.Save()
}
```

- [ ] **Step 4: Wire startup**

Update `main()` in `cmd/anvil-scheduler/main.go`:

```go
func main() {
	cfg, err := loadSchedulerConfig()
	if err != nil {
		log.Fatalf("load scheduler config: %v", err)
	}
	placements := anvilmcp.NewPlacementStore(cfg.PlacementPath)
	if err := placements.Load(); err != nil {
		log.Fatalf("load scheduler placement store: %v", err)
	}
	configuredHosts, err := anvilmcp.LoadSchedulerHostsFile(cfg.HostsFile)
	if err != nil {
		log.Fatalf("load scheduler hosts file: %v", err)
	}
	if err := applyConfiguredHosts(placements, configuredHosts); err != nil {
		log.Fatalf("apply scheduler hosts file: %v", err)
	}
	quotas := anvilmcp.NewQuotaStore(cfg.QuotaStorePath)
	if err := quotas.Load(); err != nil {
		log.Fatalf("load scheduler quota store: %v", err)
	}
	service := anvilmcp.NewSchedulerService(anvilmcp.SchedulerServiceOptions{
		PlacementStore:       placements,
		QuotaStore:           quotas,
		RequirePersistence:   cfg.RequirePersistence,
	})
	loop := anvilmcp.NewSchedulerControlLoop(placements, anvilmcp.SchedulerControlLoopOptions{
		APIToken:          cfg.APIToken,
		HostTimeout:       cfg.HostTimeout,
		FailureThreshold:  cfg.FailureThreshold,
		PollInterval:      cfg.PollInterval,
		ReconcileInterval: cfg.ReconcileInterval,
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	loop.Start(ctx)
	log.Printf("anvil scheduler service on %s", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, service.Handler()); err != nil {
		log.Fatalf("scheduler service: %v", err)
	}
}
```

- [ ] **Step 5: Run cmd scheduler tests**

Run:

```bash
go test ./cmd/anvil-scheduler -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit binary wiring**

Run:

```bash
git add cmd/anvil-scheduler/main.go cmd/anvil-scheduler/main_test.go
git commit -m "feat: run scheduler control loop"
```

## Task 6: Persistence-Required Scheduling Gate

**Files:**
- Modify: `internal/anvilmcp/scheduler_service.go`
- Modify: `internal/anvilmcp/scheduler_service_test.go`

- [ ] **Step 1: Add failing persistence gate test**

Append to `internal/anvilmcp/scheduler_service_test.go`:

```go
func TestSchedulerServiceRequiresPersistenceWhenConfigured(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetControlLoopStatus(ControlLoopStatus{PersistenceDegraded: true, LastError: "save failed"})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: true})

	req := httptest.NewRequest(http.MethodPost, "/schedule/spawn", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /schedule/spawn status = %d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "scheduler persistence degraded") {
		t.Fatalf("body = %q, want persistence degraded message", rr.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify RED**

Run:

```bash
go test ./internal/anvilmcp -run TestSchedulerServiceRequiresPersistenceWhenConfigured -count=1
```

Expected: FAIL because schedule still allows requests while persistence is degraded.

- [ ] **Step 3: Implement persistence gate**

At the start of `handleSchedule`, after method check:

```go
if s.requirePersistence && s.placements.State().ControlLoopStatus.PersistenceDegraded {
	http.Error(w, "scheduler persistence degraded", http.StatusServiceUnavailable)
	return
}
```

- [ ] **Step 4: Run scheduler service tests**

Run:

```bash
go test ./internal/anvilmcp -run TestSchedulerService -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit persistence gate**

Run:

```bash
git add internal/anvilmcp/scheduler_service.go internal/anvilmcp/scheduler_service_test.go
git commit -m "feat: gate scheduling on persistence health"
```

## Task 7: Smoke Harness and Docs

**Files:**
- Modify: `scripts/anvil-scheduler-smoke.sh`
- Modify: `scripts/anvil_scheduler_smoke_test.go`
- Modify: `README.md`
- Modify: `docs/operations/runbook.md`
- Modify: `docs/operations/release-checklist.md`
- Modify: `docs/architecture/multi-tenant-roadmap.md`

- [ ] **Step 1: Add failing smoke fake endpoint test expectation**

In `scripts/anvil_scheduler_smoke_test.go`, update `TestAnvilSchedulerSmokePassesAgainstFakeScheduler` after reading summary:

```go
if !server.controlLoopStatusChecked() {
	t.Fatalf("smoke script did not call GET /control-loop/status")
}
```

Add method to the fake server type:

```go
func (s *anvilSchedulerSmokeFakeServer) controlLoopStatusChecked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.controlLoopStatusCalls > 0
}
```

Add `controlLoopStatusCalls int` to the fake server struct.

- [ ] **Step 2: Run smoke tests to verify RED**

Run:

```bash
go test ./scripts -run TestAnvilSchedulerSmokePassesAgainstFakeScheduler -count=1
```

Expected: FAIL because the script does not call `/control-loop/status`.

- [ ] **Step 3: Add fake server `/control-loop/status`**

In the fake scheduler handler switch, add:

```go
case "/control-loop/status":
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.controlLoopStatusCalls++
	s.mu.Unlock()
	writeSmokeTestJSON(t, w, map[string]any{
		"running":                    true,
		"poll_interval_seconds":      10,
		"reconcile_interval_seconds": 30,
		"failure_threshold":          3,
		"persistence_degraded":       false,
		"hosts":                      map[string]any{},
	})
```

- [ ] **Step 4: Update smoke script**

In `scripts/anvil-scheduler-smoke.sh`, add a new step after `GET /placements`:

```bash
http_request GET "$base_url/control-loop/status" ""
if [[ "$HTTP_STATUS" != "200" ]]; then
  fail "control_loop_status_failed" "GET /control-loop/status returned HTTP $HTTP_STATUS: $HTTP_BODY"
fi
if ! json_has_key "$HTTP_BODY" "running"; then
  fail "control_loop_status_failed" "GET /control-loop/status response missing running"
fi
```

If `json_has_key` does not exist, add:

```bash
json_has_key() {
  local body="$1"
  local key="$2"
  python3 - "$key" <<'PY' <<<"$body"
import json
import sys
key = sys.argv[1]
try:
    value = json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if isinstance(value, dict) and key in value else 1)
PY
}
```

- [ ] **Step 5: Update docs**

In `docs/operations/runbook.md`, add scheduler control loop checks near scheduler health:

```markdown
runtime scheduler control loop 상태:

```bash
curl http://127.0.0.1:3010/control-loop/status
```

`degraded` host는 신규 placement에서 제외되며, `unhealthy` host의 기존 VM placement는
`suspect_vm_placements`로 남는다. host가 다시 응답하면 reconciliation이 daemon
`GET /vms` 결과로 stale placement를 정리한다.
```
```

In `docs/operations/release-checklist.md`, add `/control-loop/status` to the scheduler production automation verification paragraph.

In `docs/architecture/multi-tenant-roadmap.md`, update the scheduler foundation bullets to mention scheduler control loop, host observations, and suspect placements.

In `README.md`, add `ANVIL_SCHEDULER_HOSTS_FILE`, `ANVIL_SCHEDULER_POLL_INTERVAL`, `ANVIL_SCHEDULER_RECONCILE_INTERVAL`, `ANVIL_SCHEDULER_FAILURE_THRESHOLD`, and `/control-loop/status` to the scheduler service section.

- [ ] **Step 6: Run smoke and doc checks**

Run:

```bash
go test ./scripts -run TestAnvilSchedulerSmoke -count=1
bash -n scripts/anvil-scheduler-smoke.sh
git diff --check
```

Expected: PASS.

- [ ] **Step 7: Commit smoke and docs**

Run:

```bash
git add scripts/anvil-scheduler-smoke.sh scripts/anvil_scheduler_smoke_test.go README.md docs/operations/runbook.md docs/operations/release-checklist.md docs/architecture/multi-tenant-roadmap.md
git commit -m "docs: document scheduler control loop operations"
```

## Task 8: End-to-End Verification

**Files:**
- Verify only unless a previous task exposes a real failure.

- [ ] **Step 1: Run focused package tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-scheduler -count=1
go test ./scripts -count=1
```

Expected: PASS.

- [ ] **Step 2: Run full Go test suite**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Run build gates**

Run:

```bash
go build ./cmd/goose-daemon
go build ./cmd/anvil-mcp
go build ./cmd/anvil-scheduler
```

Expected: all commands exit 0.

- [ ] **Step 4: Run shell syntax gates**

Run:

```bash
bash -n scripts/anvil-scheduler-smoke.sh
bash -n scripts/install-anvil-scheduler-systemd.sh
bash -n scripts/vm-workload-e2e.sh
```

Expected: no output and exit code 0.

- [ ] **Step 5: Run local scheduler binary smoke**

Run:

```bash
tmpdir="$(mktemp -d)"
cat >"$tmpdir/hosts.json" <<'JSON'
{"hosts":[{"name":"local-daemon","endpoint":"http://127.0.0.1:3000","egress_policies":["profile"],"smoke_only":false}]}
JSON
ANVIL_SCHEDULER_ADDR=127.0.0.1:3010 \
ANVIL_SCHEDULER_STATE="$tmpdir/scheduler.json" \
ANVIL_SCHEDULER_QUOTA_STORE="$tmpdir/tenants.json" \
ANVIL_SCHEDULER_HOSTS_FILE="$tmpdir/hosts.json" \
ANVIL_SCHEDULER_POLL_INTERVAL=1s \
ANVIL_SCHEDULER_RECONCILE_INTERVAL=1s \
go run ./cmd/anvil-scheduler >"$tmpdir/scheduler.log" 2>&1 &
pid="$!"
sleep 2
curl -fsS http://127.0.0.1:3010/control-loop/status
bash scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010 --json-out "$tmpdir/summary.json"
kill "$pid"
wait "$pid" 2>/dev/null || true
cat "$tmpdir/summary.json"
rm -rf "$tmpdir"
```

Expected: `/control-loop/status` returns JSON with `running:true`; smoke exits 0.

- [ ] **Step 6: Run installer dry-run verification**

Run:

```bash
bash scripts/install-anvil-scheduler-systemd.sh --dry-run --no-build --no-enable --verify
```

Expected: output includes `scripts/anvil-scheduler-smoke.sh --base-url http://127.0.0.1:3010` and does not modify system files.

- [ ] **Step 7: Check final diff and status**

Run:

```bash
git diff --check
git status --short
```

Expected: `git diff --check` exits 0. `git status --short` is clean after commits.

## Self-Review

- Spec coverage: host config bootstrap, config-vs-state merge, degraded/unhealthy transitions, suspect placements, `/placements` extension, `/control-loop/status`, persistence degraded gate, smoke verification, docs, and exclusions are all mapped to tasks.
- Placeholder scan: no task contains unspecified work, empty validation instructions, or deferred implementation markers.
- Type consistency: plan-defined types are `HostStatus`, `HostObservation`, `SuspectVMPlacement`, `ControlLoopStatus`, `HostStatusSummary`, `SchedulerControlLoopOptions`, and `SchedulerControlLoop`; later tasks use the same names.
