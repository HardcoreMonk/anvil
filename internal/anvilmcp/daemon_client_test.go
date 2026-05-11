package anvilmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
