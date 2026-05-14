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

		var body SpawnVMRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.Profile != "dev" {
			t.Fatalf("profile = %q, want dev", body.Profile)
		}
		if body.TenantID != "tenant-1" || body.EgressPolicy != "profile" {
			t.Fatalf("tenant/egress = %q/%q, want tenant-1/profile", body.TenantID, body.EgressPolicy)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"vm_id":"vm-1","guest_ip":"10.0.0.2","agent_url":"http://10.0.0.2:8080","profile":"dev","tenant_id":"tenant-1","egress_policy":"profile","agent_token":"agent-token"}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL, APIToken: "test-token"}, server.Client())
	resp, err := client.SpawnVM(context.Background(), SpawnVMRequest{
		Profile:      "dev",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
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
	if resp.TenantID != "tenant-1" || resp.EgressPolicy != "profile" {
		t.Fatalf("tenant/egress = %q/%q, want tenant-1/profile", resp.TenantID, resp.EgressPolicy)
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

func TestDaemonClientRuntimeAuditEndpoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"records":[{"tenant_id":"tenant-1","tool_name":"anvil_spawn_vm","daemon_operation":"POST /vms","result_code":"success","timestamp":"2026-05-14T00:00:00Z"}]}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	if _, err := client.ListRuntimeAudit(context.Background(), "tenant-1", 5); err != nil {
		t.Fatalf("ListRuntimeAudit returned error: %v", err)
	}
	if _, err := client.PruneRuntimeAudit(context.Background(), RuntimeAuditRetention{KeepLast: 5}); err != nil {
		t.Fatalf("PruneRuntimeAudit returned error: %v", err)
	}

	want := []string{
		"GET /audit/runtime?limit=5&tenant_id=tenant-1",
		"POST /audit/runtime/prune",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

func TestDaemonClientControlPlaneEndpoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want Bearer test-token", got)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","vm_count":1,"snapshot_count":2,"auth_enabled":true}`))
		case "GET /metrics":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("anvil_vm_create_total 1\n"))
		case "GET /tenants":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"tenant_id":"tenant-1","quota":{"active_vms":2},"usage":{"active_vms":1}}]`))
		case "GET /tenants/tenant-1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tenant_id":"tenant-1","quota":{"active_vms":2},"usage":{"active_vms":1}}`))
		case "PUT /tenants/tenant-1":
			var body TenantUpsertRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode tenant upsert body: %v", err)
			}
			if body.Quota.ActiveVMs != 3 {
				t.Fatalf("active_vms = %d, want 3", body.Quota.ActiveVMs)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tenant_id":"tenant-1","quota":{"active_vms":3},"usage":{"active_vms":1}}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL, APIToken: "test-token"}, server.Client())
	health, err := client.DaemonHealth(context.Background())
	if err != nil {
		t.Fatalf("DaemonHealth returned error: %v", err)
	}
	if health.Status != "ok" || health.VMCount != 1 || health.SnapshotCount != 2 || !health.AuthEnabled {
		t.Fatalf("health = %+v, want ok vm=1 snapshot=2 auth=true", health)
	}
	metrics, err := client.Metrics(context.Background())
	if err != nil {
		t.Fatalf("Metrics returned error: %v", err)
	}
	if metrics != "anvil_vm_create_total 1\n" {
		t.Fatalf("metrics = %q", metrics)
	}
	tenants, err := client.ListTenants(context.Background())
	if err != nil {
		t.Fatalf("ListTenants returned error: %v", err)
	}
	if len(tenants) != 1 || tenants[0].TenantID != "tenant-1" {
		t.Fatalf("tenants = %+v, want tenant-1", tenants)
	}
	tenant, err := client.GetTenant(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("GetTenant returned error: %v", err)
	}
	if tenant.TenantID != "tenant-1" || tenant.Quota.ActiveVMs != 2 || tenant.Usage.ActiveVMs != 1 {
		t.Fatalf("tenant = %+v, want tenant-1 quota=2 usage=1", tenant)
	}
	upserted, err := client.UpsertTenant(context.Background(), "tenant-1", TenantQuota{ActiveVMs: 3})
	if err != nil {
		t.Fatalf("UpsertTenant returned error: %v", err)
	}
	if upserted.Quota.ActiveVMs != 3 {
		t.Fatalf("upserted quota = %+v, want active_vms=3", upserted.Quota)
	}

	want := []string{
		"GET /health",
		"GET /metrics",
		"GET /tenants",
		"GET /tenants/tenant-1",
		"PUT /tenants/tenant-1",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
}

func TestDaemonClientFlockEndpoints(t *testing.T) {
	createdAt := "2026-05-15T01:02:03Z"
	messageAt := "2026-05-15T01:03:04Z"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method + " " + r.URL.Path {
		case "POST /flocks":
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			var body FlockCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create flock body: %v", err)
			}
			if body.Task != "ship feature" {
				t.Fatalf("task = %q, want ship feature", body.Task)
			}
			if len(body.Roles) != 2 || body.Roles[0] != "planner" || body.Roles[1] != "builder" {
				t.Fatalf("roles = %v, want [planner builder]", body.Roles)
			}
			if body.TenantID != "tenant-1" || body.EgressPolicy != "locked" {
				t.Fatalf("tenant/egress = %q/%q, want tenant-1/locked", body.TenantID, body.EgressPolicy)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"flock_id":"flock-1","task":"ship feature","tenant_id":"tenant-1","egress_policy":"locked","agents":null,"townwall_url":"http://127.0.0.1:3000/flocks/flock-1/wall","post_url":"http://daemon/flocks/flock-1/post"}`))
		case "GET /flocks":
			_, _ = w.Write([]byte(`null`))
		case "GET /flocks/flock-1":
			_, _ = w.Write([]byte(`{"flock_id":"flock-1","task":"ship feature","tenant_id":"tenant-1","egress_policy":"locked","agents":{"planner":{"agent_id":"agent-1","role":"planner","vm_id":"vm-1","agent_url":"http://10.0.0.2:8080","status":"running"}},"created_at":"` + createdAt + `"}`))
		case "POST /flocks/flock-1/post":
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			var body TownWallPostRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode town wall post body: %v", err)
			}
			if body.AgentID != "agent-1" || body.Body != "hello wall" {
				t.Fatalf("town wall post = %+v, want agent-1 hello wall", body)
			}
			_, _ = w.Write([]byte(`{"timestamp":"` + messageAt + `","agent_id":"agent-1","body":"hello wall"}`))
		case "GET /flocks/flock-1/wall/history":
			_, _ = w.Write([]byte(`null`))
		case "DELETE /flocks/flock-1":
			_, _ = w.Write([]byte(`{"deleted":true}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL, APIToken: "test-token"}, server.Client())
	ctx := context.Background()

	created, err := client.CreateFlock(ctx, FlockCreateRequest{
		Task:         "ship feature",
		Roles:        []string{"planner", "builder"},
		TenantID:     "tenant-1",
		EgressPolicy: "locked",
	})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if created.FlockID != "flock-1" || created.Task != "ship feature" || created.TenantID != "tenant-1" || created.EgressPolicy != "locked" {
		t.Fatalf("created flock = %+v, want flock-1 ship feature tenant-1 locked", created)
	}
	if created.Agents == nil || len(created.Agents) != 0 {
		t.Fatalf("created agents = %#v, want empty non-nil slice", created.Agents)
	}
	if created.TownWallURL != "http://127.0.0.1:3000/flocks/flock-1/wall" || created.PostURL != "http://daemon/flocks/flock-1/post" {
		t.Fatalf("wall/post urls = %q/%q", created.TownWallURL, created.PostURL)
	}

	flocks, err := client.ListFlocks(ctx)
	if err != nil {
		t.Fatalf("ListFlocks returned error: %v", err)
	}
	if flocks == nil || len(flocks) != 0 {
		t.Fatalf("flocks = %#v, want empty non-nil slice", flocks)
	}

	flock, err := client.GetFlock(ctx, "flock-1")
	if err != nil {
		t.Fatalf("GetFlock returned error: %v", err)
	}
	wantCreatedAt, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		t.Fatalf("parse createdAt fixture: %v", err)
	}
	if flock.FlockID != "flock-1" || !flock.CreatedAt.Equal(wantCreatedAt) {
		t.Fatalf("flock = %+v, want flock-1 created_at %v", flock, wantCreatedAt)
	}
	if flock.Agents["planner"].AgentID != "agent-1" || flock.Agents["planner"].Status != "running" {
		t.Fatalf("planner agent = %+v, want agent-1 running", flock.Agents["planner"])
	}

	message, err := client.PostTownWall(ctx, "flock-1", TownWallPostRequest{AgentID: "agent-1", Body: "hello wall"})
	if err != nil {
		t.Fatalf("PostTownWall returned error: %v", err)
	}
	wantMessageAt, err := time.Parse(time.RFC3339, messageAt)
	if err != nil {
		t.Fatalf("parse messageAt fixture: %v", err)
	}
	if message.AgentID != "agent-1" || message.Body != "hello wall" || !message.Timestamp.Equal(wantMessageAt) {
		t.Fatalf("message = %+v, want agent-1 hello wall at %v", message, wantMessageAt)
	}

	history, err := client.TownWallHistory(ctx, "flock-1")
	if err != nil {
		t.Fatalf("TownWallHistory returned error: %v", err)
	}
	if history == nil || len(history) != 0 {
		t.Fatalf("history = %#v, want empty non-nil slice", history)
	}

	deleted, err := client.DeleteFlock(ctx, "flock-1")
	if err != nil {
		t.Fatalf("DeleteFlock returned error: %v", err)
	}
	if deleted.StatusCode != http.StatusOK || deleted.Body != `{"deleted":true}` {
		t.Fatalf("deleted = %+v, want status 200 body deleted true", deleted)
	}

	want := []string{
		"POST /flocks",
		"GET /flocks",
		"GET /flocks/flock-1",
		"POST /flocks/flock-1/post",
		"GET /flocks/flock-1/wall/history",
		"DELETE /flocks/flock-1",
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
		if body.TenantID != "tenant-1" {
			t.Fatalf("tenant_id = %q, want tenant-1", body.TenantID)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"snapshot_id":"snap-1","source_vm_id":"vm-1","tenant_id":"tenant-1","profile":"dev","egress_policy":"profile","snapshot_type":"diff","base_snapshot_id":"snap-base","created_at":"2026-05-11T00:00:00Z"}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	resp, err := client.CreateSnapshot(context.Background(), "vm-1", CreateSnapshotRequest{
		StopAfter: true,
		Type:      "diff",
		TenantID:  "tenant-1",
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
	if resp.TenantID != "tenant-1" || resp.EgressPolicy != "profile" {
		t.Fatalf("tenant/egress = %q/%q, want tenant-1/profile", resp.TenantID, resp.EgressPolicy)
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

func TestDaemonClientRestoreSnapshotForwardsContractAndOmitsAgentToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want %s", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/snapshots/snap-1/restore" {
			t.Fatalf("path = %s, want /snapshots/snap-1/restore", r.URL.Path)
		}
		var body RestoreSnapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body.TenantID != "tenant-1" || body.EgressPolicy != "profile" {
			t.Fatalf("tenant/egress = %q/%q, want tenant-1/profile", body.TenantID, body.EgressPolicy)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"vm_id":"vm-restored","guest_ip":"10.0.0.9","agent_url":"http://10.0.0.9:8080","profile":"dev","tenant_id":"tenant-1","egress_policy":"profile","source_snapshot_id":"snap-1"}`))
	}))
	defer server.Close()

	client := NewDaemonClient(Config{DaemonURL: server.URL}, server.Client())
	resp, err := client.RestoreSnapshot(context.Background(), "snap-1", RestoreSnapshotRequest{
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}

	if resp.VMID != "vm-restored" {
		t.Fatalf("VMID = %q, want vm-restored", resp.VMID)
	}
	if resp.TenantID != "tenant-1" || resp.EgressPolicy != "profile" {
		t.Fatalf("tenant/egress = %q/%q, want tenant-1/profile", resp.TenantID, resp.EgressPolicy)
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
