package anvilmcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDaemonClientSpawnVM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want %s", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/vms" {
			t.Fatalf("path = %s, want /vms", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want Bearer test-token", got)
		}

		var body struct {
			Profile string `json:"profile"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Profile != "dev" {
			t.Fatalf("profile = %q, want dev", body.Profile)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"vm_id":"vm-1","guest_ip":"10.0.0.2","agent_url":"http://10.0.0.2:8080","profile":"dev","agent_token":"agent-token"}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL, APIToken: "test-token"}, server.Client())
	resp, err := client.SpawnVM(context.Background(), "dev")
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}

	if resp.VMID != "vm-1" {
		t.Fatalf("VMID = %q, want vm-1", resp.VMID)
	}
	if resp.GuestIP != "10.0.0.2" {
		t.Fatalf("GuestIP = %q, want 10.0.0.2", resp.GuestIP)
	}
	if resp.AgentURL != "http://10.0.0.2:8080" {
		t.Fatalf("AgentURL = %q, want http://10.0.0.2:8080", resp.AgentURL)
	}
	if resp.Profile != "dev" {
		t.Fatalf("Profile = %q, want dev", resp.Profile)
	}
	if resp.AgentToken != "agent-token" {
		t.Fatalf("AgentToken = %q, want agent-token", resp.AgentToken)
	}
}

func TestDaemonClientRawEndpoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	ctx := context.Background()

	if _, err := client.RunTask(ctx, "vm-1", "do it"); err != nil {
		t.Fatalf("RunTask returned error: %v", err)
	}
	if _, err := client.Health(ctx, "vm-1"); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if _, err := client.Stop(ctx, "vm-1"); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if _, err := client.Delete(ctx, "vm-1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	want := []string{
		"POST /vms/vm-1/tasks",
		"GET /vms/vm-1/health",
		"POST /vms/vm-1/stop",
		"DELETE /vms/vm-1",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q; all paths = %v", i, paths[i], want[i], paths)
		}
	}
}

func TestDaemonClientCopyIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/vms/vm-1/workspace" {
			t.Fatalf("path = %s, want /vms/vm-1/workspace", r.URL.Path)
		}
		if r.URL.Query().Get("path") != "notes/task.txt" {
			t.Fatalf("query path = %q, want notes/task.txt", r.URL.Query().Get("path"))
		}
		if r.URL.Query().Get("overwrite") != "true" {
			t.Fatalf("query overwrite = %q, want true", r.URL.Query().Get("overwrite"))
		}
		if got := r.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Fatalf("Content-Type = %q, want application/octet-stream", got)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(data) != "hello workspace" {
			t.Fatalf("body = %q, want hello workspace", string(data))
		}
		_, _ = w.Write([]byte(`{"path":"notes/task.txt","bytes":15}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	resp, err := client.CopyIn(context.Background(), "vm-1", "notes/task.txt", "hello workspace", true)
	if err != nil {
		t.Fatalf("CopyIn returned error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
}

func TestDaemonClientCopyOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/vms/vm-1/workspace" {
			t.Fatalf("path = %s, want /vms/vm-1/workspace", r.URL.Path)
		}
		if r.URL.Query().Get("path") != "notes/task.txt" {
			t.Fatalf("query path = %q, want notes/task.txt", r.URL.Query().Get("path"))
		}
		_, _ = w.Write([]byte("hello workspace"))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	content, err := client.CopyOut(context.Background(), "vm-1", "notes/task.txt")
	if err != nil {
		t.Fatalf("CopyOut returned error: %v", err)
	}
	if content != "hello workspace" {
		t.Fatalf("content = %q, want hello workspace", content)
	}
}

func TestDaemonClientCreateSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want %s", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/vms/vm-1/snapshot" {
			t.Fatalf("path = %s, want /vms/vm-1/snapshot", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}

		var body CreateSnapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if !body.StopAfter {
			t.Fatal("stop_after = false, want true")
		}
		if body.Type != "diff" {
			t.Fatalf("type = %q, want diff", body.Type)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"snapshot_id":"snap-1","source_vm_id":"vm-1","profile":"dev","snapshot_type":"diff","base_snapshot_id":"snap-base","created_at":"2026-05-11T00:00:00Z"}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	resp, err := client.CreateSnapshot(context.Background(), "vm-1", CreateSnapshotRequest{
		StopAfter: true,
		Type:      "diff",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	if resp.SnapshotID != "snap-1" {
		t.Fatalf("SnapshotID = %q, want snap-1", resp.SnapshotID)
	}
	if resp.SourceVMID != "vm-1" {
		t.Fatalf("SourceVMID = %q, want vm-1", resp.SourceVMID)
	}
	if resp.Profile != "dev" {
		t.Fatalf("Profile = %q, want dev", resp.Profile)
	}
	if resp.SnapshotType != "diff" {
		t.Fatalf("SnapshotType = %q, want diff", resp.SnapshotType)
	}
	if resp.BaseSnapshotID != "snap-base" {
		t.Fatalf("BaseSnapshotID = %q, want snap-base", resp.BaseSnapshotID)
	}
	wantCreatedAt := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	if !resp.CreatedAt.Equal(wantCreatedAt) {
		t.Fatalf("CreatedAt = %v, want %v", resp.CreatedAt, wantCreatedAt)
	}
}

func TestDaemonClientListSnapshots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want %s", r.Method, http.MethodGet)
		}
		if r.URL.Path != "/snapshots" {
			t.Fatalf("path = %s, want /snapshots", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"snapshot_id":"snap-1","source_vm_id":"vm-1","snapshot_type":"full","created_at":"2026-05-11T00:00:00Z"}]`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	resp, err := client.ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots returned error: %v", err)
	}

	if len(resp) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(resp))
	}
	if resp[0].SnapshotID != "snap-1" {
		t.Fatalf("SnapshotID = %q, want snap-1", resp[0].SnapshotID)
	}
	if resp[0].SnapshotType != "full" {
		t.Fatalf("SnapshotType = %q, want full", resp[0].SnapshotType)
	}
}

func TestDaemonClientRestoreSnapshotDecodesAgentToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want %s", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/snapshots/snap-1/restore" {
			t.Fatalf("path = %s, want /snapshots/snap-1/restore", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"vm_id":"vm-restored","guest_ip":"10.0.0.9","agent_url":"http://10.0.0.9:8080","profile":"dev","agent_token":"secret-token","source_snapshot_id":"snap-1"}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	resp, err := client.RestoreSnapshot(context.Background(), "snap-1")
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}

	if resp.VMID != "vm-restored" {
		t.Fatalf("VMID = %q, want vm-restored", resp.VMID)
	}
	if resp.AgentToken != "secret-token" {
		t.Fatalf("AgentToken = %q, want secret-token", resp.AgentToken)
	}
	if resp.SourceSnapshotID != "snap-1" {
		t.Fatalf("SourceSnapshotID = %q, want snap-1", resp.SourceSnapshotID)
	}
}

func TestDaemonClientDeleteSnapshotPreservesDaemonError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want %s", r.Method, http.MethodDelete)
		}
		if r.URL.Path != "/snapshots/snap-1" {
			t.Fatalf("path = %s, want /snapshots/snap-1", r.URL.Path)
		}

		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"base snapshot is referenced"}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	_, err := client.DeleteSnapshot(context.Background(), "snap-1")
	if err == nil {
		t.Fatal("DeleteSnapshot returned nil error")
	}

	var daemonErr *DaemonError
	if !errors.As(err, &daemonErr) {
		t.Fatalf("error type = %T, want *DaemonError", err)
	}
	if daemonErr.StatusCode != http.StatusConflict {
		t.Fatalf("StatusCode = %d, want %d", daemonErr.StatusCode, http.StatusConflict)
	}
	if daemonErr.Body != `{"error":"base snapshot is referenced"}` {
		t.Fatalf("Body = %q, want daemon body", daemonErr.Body)
	}
}

func TestDaemonClientPreservesDaemonError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"agent unreachable"}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	_, err := client.Health(context.Background(), "vm-1")
	if err == nil {
		t.Fatal("Health returned nil error")
	}

	var daemonErr *DaemonError
	if !errors.As(err, &daemonErr) {
		t.Fatalf("error type = %T, want *DaemonError", err)
	}
	if daemonErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d, want %d", daemonErr.StatusCode, http.StatusBadGateway)
	}
	if daemonErr.Body != `{"error":"agent unreachable"}` {
		t.Fatalf("Body = %q, want exact daemon JSON", daemonErr.Body)
	}
}
