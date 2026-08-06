package anvilmcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// hostileResourceIDs are vm_id / snapshot_id values that must never reach a
// daemon wire path. Each one either escapes the /vms/ or /snapshots/ prefix via
// traversal, pre-empts the query string, smuggles a fragment, or carries a
// separator/control byte into what has to stay a single path segment.
var hostileResourceIDs = []string{
	"../../tenants",
	"../tenants",
	"..",
	".",
	"v/workspace?path=/etc/shadow&z=",
	"a#b",
	`a\b`,
	"vm%2F..%2Ftenants",
	"vm-1\x00",
	"vm-1 2",
}

type vmToolCase struct {
	name  string
	call  func(ctx context.Context, tools *Tools, vmID string) error
	calls func(d *fakeDaemon) int
}

func vmToolCases() []vmToolCase {
	return []vmToolCase{
		{
			name: "anvil_run_task",
			call: func(ctx context.Context, tools *Tools, vmID string) error {
				_, err := tools.RunTask(ctx, RunTaskInput{VMID: vmID, Prompt: "hello"})
				return err
			},
			calls: func(d *fakeDaemon) int { return d.runCalls },
		},
		{
			name: "anvil_copy_in",
			call: func(ctx context.Context, tools *Tools, vmID string) error {
				_, err := tools.CopyIn(ctx, CopyInInput{VMID: vmID, Path: "notes/task.txt", Content: "hello"})
				return err
			},
			calls: func(d *fakeDaemon) int { return d.copyInCalls },
		},
		{
			name: "anvil_copy_out",
			call: func(ctx context.Context, tools *Tools, vmID string) error {
				_, err := tools.CopyOut(ctx, CopyOutInput{VMID: vmID, Path: "notes/task.txt"})
				return err
			},
			calls: func(d *fakeDaemon) int { return d.copyOutCalls },
		},
		{
			name: "anvil_get_vm_health",
			call: func(ctx context.Context, tools *Tools, vmID string) error {
				_, err := tools.Health(ctx, VMIdentityInput{VMID: vmID})
				return err
			},
			calls: func(d *fakeDaemon) int { return d.healthCalls },
		},
		{
			name: "anvil_stop_vm",
			call: func(ctx context.Context, tools *Tools, vmID string) error {
				_, err := tools.StopVM(ctx, VMIdentityInput{VMID: vmID})
				return err
			},
			calls: func(d *fakeDaemon) int { return d.stopCalls },
		},
		{
			name: "anvil_delete_vm",
			call: func(ctx context.Context, tools *Tools, vmID string) error {
				_, err := tools.DeleteVM(ctx, VMIdentityInput{VMID: vmID})
				return err
			},
			calls: func(d *fakeDaemon) int { return d.deleteCalls },
		},
		{
			name: "anvil_create_snapshot",
			call: func(ctx context.Context, tools *Tools, vmID string) error {
				_, err := tools.CreateSnapshot(ctx, CreateSnapshotInput{VMID: vmID})
				return err
			},
			calls: func(d *fakeDaemon) int { return d.createSnapshotCalls },
		},
	}
}

func TestToolsRejectVMIDPathInjection(t *testing.T) {
	for _, tc := range vmToolCases() {
		for _, hostile := range hostileResourceIDs {
			t.Run(tc.name+"/"+hostile, func(t *testing.T) {
				daemon := &fakeDaemon{}
				tools := NewTools(daemon, NewSessionStore(), time.Second)

				err := tc.call(context.Background(), tools, hostile)
				if err == nil {
					t.Fatalf("%s accepted hostile vm_id %q, want error", tc.name, hostile)
				}
				if got := tc.calls(daemon); got != 0 {
					t.Fatalf("%s reached daemon %d times with hostile vm_id %q, want 0", tc.name, got, hostile)
				}
			})
		}
	}
}

// A vm_id smuggled into the session store (tampered on-disk session file, or a
// legacy alias) must be rejected at resolve time too: validating only the
// explicit tool argument would leave the alias path open.
func TestToolsRejectSessionResolvedVMIDPathInjection(t *testing.T) {
	for _, hostile := range hostileResourceIDs {
		if hostile == "" {
			continue
		}
		t.Run(hostile, func(t *testing.T) {
			daemon := &fakeDaemon{}
			store := newFakeSessionStore()
			store.sessions["work"] = hostile
			tools := newTools(daemon, store, time.Second)

			if _, err := tools.RunTask(context.Background(), RunTaskInput{SessionName: "work", Prompt: "hello"}); err == nil {
				t.Fatalf("RunTask accepted session-resolved hostile vm_id %q, want error", hostile)
			}
			if daemon.runCalls != 0 {
				t.Fatalf("RunTask reached daemon %d times with session-resolved hostile vm_id %q, want 0", daemon.runCalls, hostile)
			}
		})
	}
}

func TestToolsRejectSnapshotIDPathInjection(t *testing.T) {
	for _, hostile := range hostileResourceIDs {
		t.Run("anvil_restore_snapshot/"+hostile, func(t *testing.T) {
			daemon := &fakeDaemon{}
			tools := NewTools(daemon, NewSessionStore(), time.Second)

			if _, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{SnapshotID: hostile}); err == nil {
				t.Fatalf("RestoreSnapshot accepted hostile snapshot_id %q, want error", hostile)
			}
			if daemon.restoreSnapshotCalls != 0 {
				t.Fatalf("RestoreSnapshot reached daemon %d times with hostile snapshot_id %q, want 0", daemon.restoreSnapshotCalls, hostile)
			}
		})

		t.Run("anvil_delete_snapshot/"+hostile, func(t *testing.T) {
			daemon := &fakeDaemon{}
			tools := NewTools(daemon, NewSessionStore(), time.Second)

			if _, err := tools.DeleteSnapshot(context.Background(), SnapshotIdentityInput{SnapshotID: hostile}); err == nil {
				t.Fatalf("DeleteSnapshot accepted hostile snapshot_id %q, want error", hostile)
			}
			if daemon.deleteSnapshotCalls != 0 {
				t.Fatalf("DeleteSnapshot reached daemon %d times with hostile snapshot_id %q, want 0", daemon.deleteSnapshotCalls, hostile)
			}
		})

		t.Run("anvil_replicate_snapshot/"+hostile, func(t *testing.T) {
			daemon := &fakeReplicatingDaemon{fakeDaemon: &fakeDaemon{}}
			tools := NewTools(daemon, NewSessionStore(), time.Second)

			_, err := tools.ReplicateSnapshot(context.Background(), ReplicateSnapshotInput{
				SnapshotID: hostile,
				SourceHost: "host-a",
				TargetHost: "host-b",
			})
			if err == nil {
				t.Fatalf("ReplicateSnapshot accepted hostile snapshot_id %q, want error", hostile)
			}
			if daemon.replicateSnapshotCalls != 0 {
				t.Fatalf("ReplicateSnapshot reached daemon %d times with hostile snapshot_id %q, want 0", daemon.replicateSnapshotCalls, hostile)
			}
		})
	}
}

// Daemon-generated IDs (api.go builds "vm-<UnixNano>" / "snap-<UnixNano>") plus
// the descriptive forms already used across this package must keep working.
func TestToolsAcceptDaemonGeneratedResourceIDs(t *testing.T) {
	validVMIDs := []string{"vm-1234567890123456789", "vm-1", "vm-worker-1", "vm_alias.2"}
	for _, tc := range vmToolCases() {
		for _, vmID := range validVMIDs {
			t.Run(tc.name+"/"+vmID, func(t *testing.T) {
				daemon := &fakeDaemon{}
				tools := NewTools(daemon, NewSessionStore(), time.Second)

				if err := tc.call(context.Background(), tools, vmID); err != nil {
					t.Fatalf("%s rejected valid vm_id %q: %v", tc.name, vmID, err)
				}
				if got := tc.calls(daemon); got != 1 {
					t.Fatalf("%s daemon calls = %d for valid vm_id %q, want 1", tc.name, got, vmID)
				}
			})
		}
	}

	for _, snapshotID := range []string{"snap-1234567890123456789", "snap-1", "snap-base-daemon"} {
		t.Run("anvil_delete_snapshot/"+snapshotID, func(t *testing.T) {
			daemon := &fakeDaemon{}
			tools := NewTools(daemon, NewSessionStore(), time.Second)

			if _, err := tools.DeleteSnapshot(context.Background(), SnapshotIdentityInput{SnapshotID: snapshotID}); err != nil {
				t.Fatalf("DeleteSnapshot rejected valid snapshot_id %q: %v", snapshotID, err)
			}
			if daemon.deleteSnapshotID != snapshotID {
				t.Fatalf("DeleteSnapshot daemon snapshot_id = %q, want %q", daemon.deleteSnapshotID, snapshotID)
			}
		})

		t.Run("anvil_restore_snapshot/"+snapshotID, func(t *testing.T) {
			daemon := &fakeDaemon{}
			tools := NewTools(daemon, NewSessionStore(), time.Second)

			if _, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{SnapshotID: snapshotID}); err != nil {
				t.Fatalf("RestoreSnapshot rejected valid snapshot_id %q: %v", snapshotID, err)
			}
			if daemon.restoreSnapshotID != snapshotID {
				t.Fatalf("RestoreSnapshot daemon snapshot_id = %q, want %q", daemon.restoreSnapshotID, snapshotID)
			}
		})
	}
}

// Second layer of defense: even if a hostile ID reaches DaemonClient (a future
// call path that skips tool-level validation), the bytes put on the wire must
// stay inside a single /vms/<id> or /snapshots/<id> segment.
func TestDaemonClientEscapesVMIDInWirePath(t *testing.T) {
	const hostileVMID = `../../tenants`

	cases := []struct {
		name     string
		wantPath string
		call     func(ctx context.Context, c *DaemonClient) error
	}{
		{
			name:     "RunTask",
			wantPath: "/vms/..%2F..%2Ftenants/tasks",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.RunTask(ctx, hostileVMID, "hello")
				return err
			},
		},
		{
			name:     "CopyIn",
			wantPath: "/vms/..%2F..%2Ftenants/workspace",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.CopyIn(ctx, hostileVMID, "notes/task.txt", "hello", true)
				return err
			},
		},
		{
			name:     "CopyOut",
			wantPath: "/vms/..%2F..%2Ftenants/workspace",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.CopyOut(ctx, hostileVMID, "notes/task.txt")
				return err
			},
		},
		{
			name:     "Health",
			wantPath: "/vms/..%2F..%2Ftenants/health",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.Health(ctx, hostileVMID)
				return err
			},
		},
		{
			name:     "Stop",
			wantPath: "/vms/..%2F..%2Ftenants/stop",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.Stop(ctx, hostileVMID)
				return err
			},
		},
		{
			name:     "Delete",
			wantPath: "/vms/..%2F..%2Ftenants",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.Delete(ctx, hostileVMID)
				return err
			},
		},
		{
			name:     "CreateSnapshot",
			wantPath: "/vms/..%2F..%2Ftenants/snapshot",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.CreateSnapshot(ctx, hostileVMID, CreateSnapshotRequest{})
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()

			client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
			if err := tc.call(context.Background(), client); err != nil {
				t.Fatalf("%s returned error: %v", tc.name, err)
			}
			if gotPath != tc.wantPath {
				t.Fatalf("%s wire path = %q, want %q", tc.name, gotPath, tc.wantPath)
			}
		})
	}
}

func TestDaemonClientEscapesSnapshotIDInWirePath(t *testing.T) {
	const hostileSnapshotID = `../../tenants`

	cases := []struct {
		name     string
		wantPath string
		call     func(ctx context.Context, c *DaemonClient) error
	}{
		{
			name:     "RestoreSnapshot",
			wantPath: "/snapshots/..%2F..%2Ftenants/restore",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.RestoreSnapshot(ctx, hostileSnapshotID, RestoreSnapshotRequest{})
				return err
			},
		},
		{
			name:     "DeleteSnapshot",
			wantPath: "/snapshots/..%2F..%2Ftenants",
			call: func(ctx context.Context, c *DaemonClient) error {
				_, err := c.DeleteSnapshot(ctx, hostileSnapshotID)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()

			client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
			if err := tc.call(context.Background(), client); err != nil {
				t.Fatalf("%s returned error: %v", tc.name, err)
			}
			if gotPath != tc.wantPath {
				t.Fatalf("%s wire path = %q, want %q", tc.name, gotPath, tc.wantPath)
			}
		})
	}
}

// A vm_id carrying "?" must not be able to pre-empt the workspace query string
// and override the path= parameter that normalizeWorkspacePath guards.
func TestDaemonClientVMIDCannotPreemptWorkspaceQuery(t *testing.T) {
	const hostileVMID = `v/workspace?path=/etc/shadow&z=`

	t.Run("CopyIn", func(t *testing.T) {
		var gotQueryPath string
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.EscapedPath()
			gotQueryPath = r.URL.Query().Get("path")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()

		client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
		if _, err := client.CopyIn(context.Background(), hostileVMID, "notes/task.txt", "hello", false); err != nil {
			t.Fatalf("CopyIn returned error: %v", err)
		}
		if gotQueryPath != "notes/task.txt" {
			t.Fatalf("CopyIn query path = %q, want notes/task.txt (vm_id pre-empted the query string)", gotQueryPath)
		}
		if want := "/vms/v%2Fworkspace%3Fpath=%2Fetc%2Fshadow&z=/workspace"; gotPath != want {
			t.Fatalf("CopyIn wire path = %q, want %q", gotPath, want)
		}
	})

	t.Run("CopyOut", func(t *testing.T) {
		var gotQueryPath string
		var gotPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.EscapedPath()
			gotQueryPath = r.URL.Query().Get("path")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()

		client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
		if _, err := client.CopyOut(context.Background(), hostileVMID, "notes/task.txt"); err != nil {
			t.Fatalf("CopyOut returned error: %v", err)
		}
		if gotQueryPath != "notes/task.txt" {
			t.Fatalf("CopyOut query path = %q, want notes/task.txt (vm_id pre-empted the query string)", gotQueryPath)
		}
		if want := "/vms/v%2Fworkspace%3Fpath=%2Fetc%2Fshadow&z=/workspace"; gotPath != want {
			t.Fatalf("CopyOut wire path = %q, want %q", gotPath, want)
		}
	})
}
