package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ephemera/internal/anvilmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolRegistrationsIncludeSnapshotTools(t *testing.T) {
	registrations := toolRegistrations()
	names := make(map[string]bool, len(registrations))
	want := map[string]string{
		"anvil_spawn_vm":                    "Create an ephemera VM and optionally bind a local session_name alias.",
		"anvil_run_task":                    "Run a prompt synchronously in an existing ephemera VM using vm_id or session_name.",
		"anvil_copy_in":                     "Write a single file into an ephemera VM workspace using vm_id or session_name.",
		"anvil_copy_out":                    "Read a single file from an ephemera VM workspace using vm_id or session_name.",
		"anvil_get_vm_health":               "Return health for an existing ephemera VM agent using vm_id or session_name.",
		"anvil_stop_vm":                     "Ask the ephemera VM agent to stop gracefully without deleting VM resources.",
		"anvil_delete_vm":                   "Delete an ephemera VM and release its session_name alias if present.",
		"anvil_create_snapshot":             "Create a full or diff snapshot for an ephemera VM using vm_id or session_name.",
		"anvil_list_snapshots":              "List snapshots known to the ephemera daemon.",
		"anvil_restore_snapshot":            "Restore a new ephemera VM from a snapshot and optionally bind a session_name alias.",
		"anvil_delete_snapshot":             "Delete a snapshot by snapshot_id through the ephemera daemon.",
		"anvil_replicate_snapshot":          "Replicate a snapshot from one scheduler runtime host to another and update snapshot locality.",
		"anvil_spawn_flock":                 "Create a Goosetown flock of ephemera VMs and return its Town Wall endpoints.",
		"anvil_create_routed_flock_members": "Create an experimental members-only routed flock by spawning role VMs across scheduler runtime hosts without Town Wall.",
		"anvil_list_flocks":                 "List live Goosetown flocks known to the ephemera daemon.",
		"anvil_get_flock":                   "Return a Goosetown flock and its agent status by flock_id.",
		"anvil_delete_flock":                "Delete a Goosetown flock and let the daemon tear down member VMs.",
		"anvil_post_townwall":               "Append a message to a Goosetown flock Town Wall.",
		"anvil_get_townwall_history":        "Return the active Town Wall history for a Goosetown flock.",
	}

	for _, registration := range registrations {
		if registration.name == "" {
			t.Fatal("tool registration has empty name")
		}
		description, ok := want[registration.name]
		if !ok {
			t.Fatalf("unexpected tool registration %q", registration.name)
		}
		if registration.description != description {
			t.Fatalf("tool %q description = %q, want %q", registration.name, registration.description, description)
		}
		if registration.register == nil {
			t.Fatalf("tool %q has nil register function", registration.name)
		}
		var _ func(*mcp.Server, *mcp.Tool, *anvilmcp.Tools) = registration.register
		names[registration.name] = true
	}

	for name := range want {
		if !names[name] {
			t.Fatalf("missing tool registration %q; names = %v", name, names)
		}
	}
	if len(registrations) != len(want) {
		t.Fatalf("registration count = %d, want %d", len(registrations), len(want))
	}
	spawnIdx, routedIdx := -1, -1
	for idx, registration := range registrations {
		switch registration.name {
		case "anvil_spawn_flock":
			spawnIdx = idx
		case "anvil_create_routed_flock_members":
			routedIdx = idx
		}
	}
	if routedIdx != spawnIdx+1 {
		t.Fatalf("anvil_create_routed_flock_members index = %d, want directly after anvil_spawn_flock index %d", routedIdx, spawnIdx)
	}
}

func TestToolRegistrationsHaveIronClawInputSchemas(t *testing.T) {
	schemas := anvilmcp.CurrentIronClawToolInputSchemas()
	schemaNames := make(map[string]bool, len(schemas))
	for _, schema := range schemas {
		schemaNames[schema.ToolName] = true
	}

	for _, registration := range toolRegistrations() {
		if !schemaNames[registration.name] {
			t.Fatalf("tool registration %q has no IronClaw input schema; schemas = %v", registration.name, schemaNames)
		}
	}
}

func TestNewMCPDaemonUsesRuntimeRouterWhenSchedulerConfigIsSet(t *testing.T) {
	base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/snapshots":
			if r.Method != http.MethodGet {
				t.Fatalf("base method for /snapshots = %s, want GET", r.Method)
			}
			_ = json.NewEncoder(w).Encode([]anvilmcp.SnapshotInfo{{
				SnapshotID:   "snap-base-daemon",
				SnapshotType: "full",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer base.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/snapshots":
			if r.Method != http.MethodGet {
				t.Fatalf("source method for /snapshots = %s, want GET", r.Method)
			}
			_ = json.NewEncoder(w).Encode([]anvilmcp.SnapshotInfo{{
				SnapshotID:   "snap-1",
				SnapshotType: "full",
			}})
		case "/snapshots/snap-1/export":
			if r.Method != http.MethodPost {
				t.Fatalf("source method for export = %s, want POST", r.Method)
			}
			_, _ = w.Write([]byte("bundle:snap-1"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer source.Close()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/snapshots":
			if r.Method != http.MethodGet {
				t.Fatalf("target method for /snapshots = %s, want GET", r.Method)
			}
			_ = json.NewEncoder(w).Encode([]anvilmcp.SnapshotInfo{})
		case "/snapshots/import":
			if r.Method != http.MethodPost {
				t.Fatalf("target method for import = %s, want POST", r.Method)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read import body: %v", err)
			}
			if string(body) != "bundle:snap-1" {
				t.Fatalf("import body = %q, want bundle:snap-1", string(body))
			}
			_ = json.NewEncoder(w).Encode(anvilmcp.SnapshotImportResponse{
				SnapshotID:   "snap-1",
				SnapshotType: "full",
				Status:       "replicated",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer target.Close()

	dir := t.TempDir()
	hostsPath := filepath.Join(dir, "hosts.json")
	writeMainTestFile(t, hostsPath, `{"hosts":[{"name":"host-a","endpoint":"`+source.URL+`","healthy":true,"available_vms":1},{"name":"host-b","endpoint":"`+target.URL+`","healthy":true,"available_vms":1}]}`)

	daemon, err := newMCPDaemon(anvilmcp.Config{
		DaemonURL:               base.URL,
		SchedulerStatePath:      filepath.Join(dir, "scheduler.json"),
		SchedulerHostsFile:      hostsPath,
		SchedulerQuotaStorePath: filepath.Join(dir, "quotas.json"),
	}, source.Client())
	if err != nil {
		t.Fatalf("newMCPDaemon returned error: %v", err)
	}

	replicator, ok := daemon.(interface {
		ReplicateSnapshot(context.Context, anvilmcp.SnapshotReplicationRequest) (*anvilmcp.SnapshotReplicationResponse, error)
	})
	if !ok {
		t.Fatalf("daemon type %T does not support snapshot replication", daemon)
	}

	snapshots, err := daemon.ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots returned error: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].SnapshotID != "snap-base-daemon" {
		t.Fatalf("ListSnapshots = %+v, want base daemon snapshot", snapshots)
	}

	resp, err := replicator.ReplicateSnapshot(context.Background(), anvilmcp.SnapshotReplicationRequest{
		SnapshotID: "snap-1",
		SourceHost: "host-a",
		TargetHost: "host-b",
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "replicated" {
		t.Fatalf("Status = %q, want replicated", resp.Status)
	}
}

func TestNewMCPDaemonRejectsMembersOnlyWithoutSchedulerStatePath(t *testing.T) {
	dir := t.TempDir()
	hostsPath := filepath.Join(dir, "hosts.json")
	writeMainTestFile(t, hostsPath, `{"hosts":[{"name":"host-a","endpoint":"http://127.0.0.1:3001","healthy":true,"available_vms":1}]}`)

	_, err := newMCPDaemon(anvilmcp.Config{
		DaemonURL:                "http://127.0.0.1:3000",
		SchedulerHostsFile:       hostsPath,
		CrossHostFlockCreateMode: "members_only",
	}, http.DefaultClient)
	if err == nil {
		t.Fatal("newMCPDaemon error = nil, want scheduler_state_path error")
	}
	if !strings.Contains(err.Error(), "scheduler_state_path") {
		t.Fatalf("newMCPDaemon error = %q, want scheduler_state_path", err.Error())
	}
}

func TestNewMCPDaemonEnablesMembersOnlyRoutedFlockCreate(t *testing.T) {
	base := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flocks":
			if r.Method != http.MethodGet {
				t.Fatalf("base method for /flocks = %s, want GET", r.Method)
			}
			_ = json.NewEncoder(w).Encode([]anvilmcp.FlockInfo{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer base.Close()

	hostA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vms" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(anvilmcp.SpawnVMResponse{
			VMID:     "vm-planner",
			GuestIP:  "10.0.1.10",
			AgentURL: "http://10.0.1.10:3000",
			Profile:  "planner",
		})
	}))
	defer hostA.Close()

	dir := t.TempDir()
	hostsPath := filepath.Join(dir, "hosts.json")
	writeMainTestFile(t, hostsPath, `{"hosts":[{"name":"host-a","endpoint":"`+hostA.URL+`","healthy":true,"available_vms":2,"egress_policies":["deny_all"]}]}`)

	daemon, err := newMCPDaemon(anvilmcp.Config{
		DaemonURL:                base.URL,
		SchedulerStatePath:       filepath.Join(dir, "scheduler.json"),
		SchedulerHostsFile:       hostsPath,
		CrossHostFlockCreateMode: "members_only",
	}, hostA.Client())
	if err != nil {
		t.Fatalf("newMCPDaemon returned error: %v", err)
	}

	creator, ok := daemon.(interface {
		CreateRoutedFlockMembers(context.Context, anvilmcp.FlockCreateRequest) (*anvilmcp.RoutedFlockCreateOutput, error)
	})
	if !ok {
		t.Fatalf("daemon type %T does not support routed flock members create", daemon)
	}

	out, err := creator.CreateRoutedFlockMembers(context.Background(), anvilmcp.FlockCreateRequest{
		Task:         "build town",
		Roles:        []string{"planner"},
		TenantID:     "tenant-1",
		EgressPolicy: "deny_all",
	})
	if err != nil {
		t.Fatalf("CreateRoutedFlockMembers returned error: %v", err)
	}
	if out.Mode != anvilmcp.RoutedFlockModeCrossHostMembersOnly || out.TownWallEnabled {
		t.Fatalf("routed output = %+v, want members-only without Town Wall", out)
	}
	if len(out.Agents) != 1 || out.Agents[0].VMID != "vm-planner" || out.Agents[0].Host != "host-a" {
		t.Fatalf("agents = %+v, want vm-planner on host-a", out.Agents)
	}
}

func writeMainTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
