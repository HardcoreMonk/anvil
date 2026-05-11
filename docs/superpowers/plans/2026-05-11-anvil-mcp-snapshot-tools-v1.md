# anvil MCP snapshot tools v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `cmd/anvil-mcp`에 ephemera snapshot lifecycle을 노출하는 `anvil_create_snapshot`, `anvil_list_snapshots`, `anvil_restore_snapshot`, `anvil_delete_snapshot` MCP tool을 추가한다.

**Architecture:** MCP adapter는 ephemera daemon API를 얇게 bridge한다. Snapshot metadata는 daemon response를 decode만 하고, restore 응답의 `agent_token`은 MCP output에서 제거한다. Restore 후 optional `session_name` bind가 실패하면 restored VM은 자동 삭제하지 않고 error에 `restored_vm_id`를 포함한다.

**Tech Stack:** Go 1.25, `github.com/modelcontextprotocol/go-sdk/mcp`, ephemera daemon HTTP API, standard `net/http`, `encoding/json`, Go unit tests.

---

## File Structure

- Modify: `internal/anvilmcp/daemon_client.go`
  - Snapshot request/response structs and daemon HTTP methods.
- Modify: `internal/anvilmcp/daemon_client_test.go`
  - Daemon client TDD coverage for create/list/restore/delete snapshot endpoints.
- Modify: `internal/anvilmcp/tools.go`
  - MCP input/output structs, snapshot tool methods, restore bind failure error, session dependency interface.
- Modify: `internal/anvilmcp/tools_test.go`
  - Tool layer TDD coverage, including restore alias bind race behavior and token redaction.
- Modify: `cmd/anvil-mcp/main.go`
  - Register the four new MCP tools through a small registration helper.
- Create: `cmd/anvil-mcp/main_test.go`
  - Assert the registered MCP tool set includes the new snapshot tools and preserves existing VM tools.
- Modify: `README.md`
  - Update IronClaw MCP adapter section so README describes anvil snapshot tools instead of saying they do not exist.
- Modify: `docs/architecture/mcp-architecture.md`
  - Update MCP architecture and tool contracts in Korean.

---

### Task 1: Add Daemon Client Snapshot Endpoint Coverage

**Files:**
- Modify: `internal/anvilmcp/daemon_client_test.go`
- Modify: `internal/anvilmcp/daemon_client.go`

- [ ] **Step 1: Write failing daemon client tests**

In `internal/anvilmcp/daemon_client_test.go`, add `time` to the imports:

```go
import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)
```

Append these tests after `TestDaemonClientRawEndpoints`:

```go
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
```

- [ ] **Step 2: Run daemon client tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestDaemonClient(CreateSnapshot|ListSnapshots|RestoreSnapshotDecodesAgentToken|DeleteSnapshotPreservesDaemonError)' -count=1
```

Expected: FAIL with compile errors for `CreateSnapshotRequest`, `CreateSnapshot`, `ListSnapshots`, `RestoreSnapshot`, `DeleteSnapshot`, `SnapshotInfo`, or `RestoreSnapshotResponse` being undefined.

- [ ] **Step 3: Implement daemon client snapshot structs and methods**

In `internal/anvilmcp/daemon_client.go`, add `time` to imports:

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)
```

Add these structs after `SpawnVMResponse`:

```go
type SnapshotInfo struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type CreateSnapshotRequest struct {
	StopAfter bool   `json:"stop_after"`
	Type      string `json:"type,omitempty"`
}

type RestoreSnapshotResponse struct {
	VMID             string `json:"vm_id"`
	GuestIP          string `json:"guest_ip"`
	AgentURL         string `json:"agent_url"`
	Profile          string `json:"profile,omitempty"`
	AgentToken       string `json:"agent_token,omitempty"`
	SourceSnapshotID string `json:"source_snapshot_id"`
}
```

Add these methods after `Delete`:

```go
func (c *DaemonClient) CreateSnapshot(ctx context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/vms/"+vmID+"/snapshot", req)
	if err != nil {
		return nil, err
	}

	var resp SnapshotInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode create snapshot response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	_, body, err := c.do(ctx, http.MethodGet, "/snapshots", nil)
	if err != nil {
		return nil, err
	}

	var resp []SnapshotInfo
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode list snapshots response: %w", err)
	}
	return resp, nil
}

func (c *DaemonClient) RestoreSnapshot(ctx context.Context, snapshotID string) (*RestoreSnapshotResponse, error) {
	_, body, err := c.do(ctx, http.MethodPost, "/snapshots/"+snapshotID+"/restore", nil)
	if err != nil {
		return nil, err
	}

	var resp RestoreSnapshotResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("decode restore snapshot response: %w", err)
	}
	return &resp, nil
}

func (c *DaemonClient) DeleteSnapshot(ctx context.Context, snapshotID string) (*RawDaemonResponse, error) {
	return c.raw(ctx, http.MethodDelete, "/snapshots/"+snapshotID, nil)
}
```

- [ ] **Step 4: Run daemon client tests and verify they pass**

Run:

```bash
go test ./internal/anvilmcp -run 'TestDaemonClient(CreateSnapshot|ListSnapshots|RestoreSnapshotDecodesAgentToken|DeleteSnapshotPreservesDaemonError)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit daemon client change**

```bash
git add internal/anvilmcp/daemon_client.go internal/anvilmcp/daemon_client_test.go
git commit -m "feat: add snapshot daemon client methods"
```

---

### Task 2: Add Snapshot Tool Layer Methods

**Files:**
- Modify: `internal/anvilmcp/tools_test.go`
- Modify: `internal/anvilmcp/tools.go`

- [ ] **Step 1: Extend fake daemon and write failing tool tests**

In `internal/anvilmcp/tools_test.go`, change imports to:

```go
import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)
```

Add these fields to `fakeDaemon`:

```go
	createSnapshotCalls int
	createSnapshotVMID  string
	createSnapshotReq   CreateSnapshotRequest
	createSnapshotResp  *SnapshotInfo
	createSnapshotErr   error

	listSnapshotCalls int
	listSnapshotResp  []SnapshotInfo
	listSnapshotErr   error

	restoreSnapshotCalls int
	restoreSnapshotID    string
	restoreSnapshotResp  *RestoreSnapshotResponse
	restoreSnapshotErr   error

	deleteSnapshotCalls int
	deleteSnapshotID    string
	deleteSnapshotResp  *RawDaemonResponse
	deleteSnapshotErr   error
```

Add these fake daemon methods after `Delete`:

```go
func (f *fakeDaemon) CreateSnapshot(_ context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	f.createSnapshotCalls++
	f.createSnapshotVMID = vmID
	f.createSnapshotReq = req
	if f.createSnapshotErr != nil {
		return nil, f.createSnapshotErr
	}
	if f.createSnapshotResp != nil {
		return f.createSnapshotResp, nil
	}
	return &SnapshotInfo{
		SnapshotID:   "snap-1",
		SourceVMID:   vmID,
		SnapshotType: "full",
		CreatedAt:    time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC),
	}, nil
}

func (f *fakeDaemon) ListSnapshots(_ context.Context) ([]SnapshotInfo, error) {
	f.listSnapshotCalls++
	if f.listSnapshotErr != nil {
		return nil, f.listSnapshotErr
	}
	if f.listSnapshotResp != nil {
		return f.listSnapshotResp, nil
	}
	return []SnapshotInfo{{
		SnapshotID:   "snap-1",
		SourceVMID:   "vm-1",
		SnapshotType: "full",
		CreatedAt:    time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC),
	}}, nil
}

func (f *fakeDaemon) RestoreSnapshot(_ context.Context, snapshotID string) (*RestoreSnapshotResponse, error) {
	f.restoreSnapshotCalls++
	f.restoreSnapshotID = snapshotID
	if f.restoreSnapshotErr != nil {
		return nil, f.restoreSnapshotErr
	}
	if f.restoreSnapshotResp != nil {
		return f.restoreSnapshotResp, nil
	}
	return &RestoreSnapshotResponse{
		VMID:             "vm-restored",
		GuestIP:          "10.0.0.9",
		AgentURL:         "http://10.0.0.9:8080",
		Profile:          "dev",
		AgentToken:       "secret-token",
		SourceSnapshotID: snapshotID,
	}, nil
}

func (f *fakeDaemon) DeleteSnapshot(_ context.Context, snapshotID string) (*RawDaemonResponse, error) {
	f.deleteSnapshotCalls++
	f.deleteSnapshotID = snapshotID
	if f.deleteSnapshotErr != nil {
		return nil, f.deleteSnapshotErr
	}
	if f.deleteSnapshotResp != nil {
		return f.deleteSnapshotResp, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: `{"status":"deleted","snapshot_id":"snap-1"}`}, nil
}
```

Append these tests before `assertDeadlineWithin`:

```go
func TestToolsCreateSnapshotUsesSession(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	tools := NewTools(daemon, store, time.Second)

	out, err := tools.CreateSnapshot(context.Background(), CreateSnapshotInput{
		SessionName: "work",
		StopAfter:   true,
		Type:        "DIFF",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	if daemon.createSnapshotCalls != 1 {
		t.Fatalf("CreateSnapshot calls = %d, want 1", daemon.createSnapshotCalls)
	}
	if daemon.createSnapshotVMID != "vm-1" {
		t.Fatalf("CreateSnapshot vmID = %q, want vm-1", daemon.createSnapshotVMID)
	}
	if !daemon.createSnapshotReq.StopAfter {
		t.Fatal("StopAfter = false, want true")
	}
	if daemon.createSnapshotReq.Type != "diff" {
		t.Fatalf("Type = %q, want diff", daemon.createSnapshotReq.Type)
	}
	if out.SnapshotID != "snap-1" {
		t.Fatalf("SnapshotID = %q, want snap-1", out.SnapshotID)
	}
}

func TestToolsCreateSnapshotRejectsInvalidTypeBeforeDaemonCall(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	_, err := tools.CreateSnapshot(context.Background(), CreateSnapshotInput{
		VMID: "vm-1",
		Type: "base",
	})
	if err == nil {
		t.Fatal("CreateSnapshot returned nil error for invalid type")
	}
	if daemon.createSnapshotCalls != 0 {
		t.Fatalf("CreateSnapshot calls = %d, want 0", daemon.createSnapshotCalls)
	}
}

func TestToolsListSnapshotsWrapsDaemonList(t *testing.T) {
	daemon := &fakeDaemon{listSnapshotResp: []SnapshotInfo{{
		SnapshotID:   "snap-1",
		SourceVMID:   "vm-1",
		SnapshotType: "full",
		CreatedAt:    time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC),
	}}}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	out, err := tools.ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots returned error: %v", err)
	}

	if daemon.listSnapshotCalls != 1 {
		t.Fatalf("ListSnapshots calls = %d, want 1", daemon.listSnapshotCalls)
	}
	if len(out.Snapshots) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(out.Snapshots))
	}
	if out.Snapshots[0].SnapshotID != "snap-1" {
		t.Fatalf("SnapshotID = %q, want snap-1", out.Snapshots[0].SnapshotID)
	}
}

func TestToolsDeleteSnapshotPreservesDaemonError(t *testing.T) {
	daemonErr := &DaemonError{StatusCode: 409, Body: `{"error":"base snapshot is referenced"}`}
	daemon := &fakeDaemon{deleteSnapshotErr: daemonErr}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	_, err := tools.DeleteSnapshot(context.Background(), SnapshotIdentityInput{SnapshotID: "snap-1"})
	if !errors.Is(err, daemonErr) {
		t.Fatalf("DeleteSnapshot error = %v, want %v", err, daemonErr)
	}
	if daemon.deleteSnapshotCalls != 1 {
		t.Fatalf("DeleteSnapshot calls = %d, want 1", daemon.deleteSnapshotCalls)
	}
	if daemon.deleteSnapshotID != "snap-1" {
		t.Fatalf("DeleteSnapshot snapshotID = %q, want snap-1", daemon.deleteSnapshotID)
	}
}

func TestToolsDeleteSnapshotRequiresSnapshotID(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	_, err := tools.DeleteSnapshot(context.Background(), SnapshotIdentityInput{})
	if err == nil {
		t.Fatal("DeleteSnapshot returned nil error for empty snapshot_id")
	}
	if daemon.deleteSnapshotCalls != 0 {
		t.Fatalf("DeleteSnapshot calls = %d, want 0", daemon.deleteSnapshotCalls)
	}
}
```

- [ ] **Step 2: Run tool tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestTools(CreateSnapshot|ListSnapshots|DeleteSnapshot)' -count=1
```

Expected: FAIL with compile errors for `CreateSnapshotInput`, `SnapshotIdentityInput`, `CreateSnapshot`, `ListSnapshots`, `DeleteSnapshot`, or new `Daemon` interface methods.

- [ ] **Step 3: Implement snapshot tool methods**

In `internal/anvilmcp/tools.go`, extend `Daemon`:

```go
type Daemon interface {
	SpawnVM(ctx context.Context, profile string) (*SpawnVMResponse, error)
	RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error)
	Health(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	Stop(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	CreateSnapshot(ctx context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error)
	ListSnapshots(ctx context.Context) ([]SnapshotInfo, error)
	RestoreSnapshot(ctx context.Context, snapshotID string) (*RestoreSnapshotResponse, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) (*RawDaemonResponse, error)
}
```

Add these input/output structs after `VMIdentityInput`:

```go
type CreateSnapshotInput struct {
	VMID        string `json:"vm_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
	StopAfter   bool   `json:"stop_after"`
	Type        string `json:"type,omitempty"`
}

type ListSnapshotsOutput struct {
	Snapshots []SnapshotInfo `json:"snapshots"`
}

type RestoreSnapshotInput struct {
	SnapshotID  string `json:"snapshot_id"`
	SessionName string `json:"session_name,omitempty"`
}

type RestoreSnapshotOutput struct {
	VMID             string `json:"vm_id"`
	GuestIP          string `json:"guest_ip"`
	AgentURL         string `json:"agent_url"`
	Profile          string `json:"profile,omitempty"`
	SourceSnapshotID string `json:"source_snapshot_id"`
	SessionName      string `json:"session_name,omitempty"`
}

type SnapshotIdentityInput struct {
	SnapshotID string `json:"snapshot_id"`
}
```

Add these methods before `resolveIdentity`:

```go
func (t *Tools) CreateSnapshot(ctx context.Context, input CreateSnapshotInput) (*SnapshotInfo, error) {
	snapshotType, err := normalizeSnapshotType(input.Type)
	if err != nil {
		return nil, err
	}

	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}

	return t.daemon.CreateSnapshot(ctx, vmID, CreateSnapshotRequest{
		StopAfter: input.StopAfter,
		Type:      snapshotType,
	})
}

func (t *Tools) MCPCreateSnapshot(ctx context.Context, req *mcp.CallToolRequest, input CreateSnapshotInput) (*mcp.CallToolResult, SnapshotInfo, error) {
	out, err := t.CreateSnapshot(ctx, input)
	if err != nil || out == nil {
		return nil, SnapshotInfo{}, err
	}
	return nil, *out, nil
}

func (t *Tools) ListSnapshots(ctx context.Context) (*ListSnapshotsOutput, error) {
	snapshots, err := t.daemon.ListSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	if snapshots == nil {
		snapshots = []SnapshotInfo{}
	}
	return &ListSnapshotsOutput{Snapshots: snapshots}, nil
}

func (t *Tools) MCPListSnapshots(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, ListSnapshotsOutput, error) {
	out, err := t.ListSnapshots(ctx)
	if err != nil || out == nil {
		return nil, ListSnapshotsOutput{}, err
	}
	return nil, *out, nil
}

func (t *Tools) DeleteSnapshot(ctx context.Context, input SnapshotIdentityInput) (*RawDaemonResponse, error) {
	snapshotID := strings.TrimSpace(input.SnapshotID)
	if snapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	return t.daemon.DeleteSnapshot(ctx, snapshotID)
}

func (t *Tools) MCPDeleteSnapshot(ctx context.Context, req *mcp.CallToolRequest, input SnapshotIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.DeleteSnapshot(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func normalizeSnapshotType(value string) (string, error) {
	snapshotType := strings.ToLower(strings.TrimSpace(value))
	switch snapshotType {
	case "", "full", "diff":
		return snapshotType, nil
	default:
		return "", fmt.Errorf("type must be empty, full, or diff")
	}
}
```

- [ ] **Step 4: Run focused tool tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestTools(CreateSnapshot|ListSnapshots|DeleteSnapshot)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit snapshot tool methods**

```bash
git add internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go
git commit -m "feat: add snapshot MCP tool handlers"
```

---

### Task 3: Add Restore Tool Alias Binding Semantics

**Files:**
- Modify: `internal/anvilmcp/tools_test.go`
- Modify: `internal/anvilmcp/tools.go`

- [ ] **Step 1: Add fake session store and failing restore tests**

In `internal/anvilmcp/tools_test.go`, add this fake session store after the fake daemon methods:

```go
type fakeSessionStore struct {
	sessions map[string]string
	bindErr  error
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: make(map[string]string)}
}

func (s *fakeSessionStore) Bind(sessionName, vmID string) error {
	sessionName = strings.TrimSpace(sessionName)
	vmID = strings.TrimSpace(vmID)
	if s.bindErr != nil {
		return s.bindErr
	}
	if sessionName == "" {
		return errors.New("session name must be non-empty")
	}
	if vmID == "" {
		return errors.New("vm ID must be non-empty")
	}
	if s.sessions == nil {
		s.sessions = make(map[string]string)
	}
	if _, ok := s.sessions[sessionName]; ok {
		return errors.New("session already exists")
	}
	s.sessions[sessionName] = vmID
	return nil
}

func (s *fakeSessionStore) Exists(sessionName string) bool {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return false
	}
	_, ok := s.sessions[sessionName]
	return ok
}

func (s *fakeSessionStore) ResolveIdentity(vmID, sessionName string) (string, error) {
	vmID = strings.TrimSpace(vmID)
	sessionName = strings.TrimSpace(sessionName)
	if vmID != "" {
		return vmID, nil
	}
	if sessionName == "" {
		return "", errors.New("vm ID or session name is required")
	}
	resolvedVMID, ok := s.sessions[sessionName]
	if !ok {
		return "", errors.New("unknown session")
	}
	return resolvedVMID, nil
}

func (s *fakeSessionStore) RemoveVM(vmID string) {
	vmID = strings.TrimSpace(vmID)
	for sessionName, mappedVMID := range s.sessions {
		if mappedVMID == vmID {
			delete(s.sessions, sessionName)
		}
	}
}
```

Append these tests before `assertDeadlineWithin`:

```go
func TestToolsRestoreSnapshotBindsSession(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	tools := NewTools(daemon, store, time.Second)

	out, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{
		SnapshotID:  "snap-1",
		SessionName: "restored",
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}

	if daemon.restoreSnapshotCalls != 1 {
		t.Fatalf("RestoreSnapshot calls = %d, want 1", daemon.restoreSnapshotCalls)
	}
	if daemon.restoreSnapshotID != "snap-1" {
		t.Fatalf("RestoreSnapshot snapshotID = %q, want snap-1", daemon.restoreSnapshotID)
	}
	if out.VMID != "vm-restored" {
		t.Fatalf("VMID = %q, want vm-restored", out.VMID)
	}
	if out.SourceSnapshotID != "snap-1" {
		t.Fatalf("SourceSnapshotID = %q, want snap-1", out.SourceSnapshotID)
	}
	if out.SessionName != "restored" {
		t.Fatalf("SessionName = %q, want restored", out.SessionName)
	}
	if vmID, ok := store.Resolve("restored"); !ok || vmID != "vm-restored" {
		t.Fatalf("session restored resolved to %q, %v; want vm-restored, true", vmID, ok)
	}
}

func TestToolsRestoreSnapshotRejectsDuplicateSessionBeforeDaemonCall(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	if err := store.Bind("restored", "vm-existing"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	tools := NewTools(daemon, store, time.Second)

	_, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{
		SnapshotID:  "snap-1",
		SessionName: "restored",
	})
	if err == nil {
		t.Fatal("RestoreSnapshot returned nil error for duplicate session")
	}
	if daemon.restoreSnapshotCalls != 0 {
		t.Fatalf("RestoreSnapshot calls = %d, want 0", daemon.restoreSnapshotCalls)
	}
}

func TestToolsRestoreSnapshotBindFailureDoesNotDeleteRestoredVM(t *testing.T) {
	bindErr := errors.New("bind race")
	daemon := &fakeDaemon{}
	store := newFakeSessionStore()
	store.bindErr = bindErr
	tools := newTools(daemon, store, time.Second)

	_, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{
		SnapshotID:  "snap-1",
		SessionName: "restored",
	})
	if err == nil {
		t.Fatal("RestoreSnapshot returned nil error for bind failure")
	}

	var restoreErr *RestoreSessionBindError
	if !errors.As(err, &restoreErr) {
		t.Fatalf("error type = %T, want *RestoreSessionBindError", err)
	}
	if restoreErr.RestoredVMID != "vm-restored" {
		t.Fatalf("RestoredVMID = %q, want vm-restored", restoreErr.RestoredVMID)
	}
	if restoreErr.SessionName != "restored" {
		t.Fatalf("SessionName = %q, want restored", restoreErr.SessionName)
	}
	if !strings.Contains(err.Error(), `restored VM "vm-restored"; restored VM was not deleted`) {
		t.Fatalf("error = %q, want restored VM cleanup guidance", err.Error())
	}
	if daemon.deleteCalls != 0 {
		t.Fatalf("Delete calls = %d, want 0", daemon.deleteCalls)
	}
}

func TestToolsRestoreSnapshotOutputOmitsAgentToken(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	out, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{SnapshotID: "snap-1"})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	if strings.Contains(string(data), "agent_token") {
		t.Fatalf("restore output JSON exposes agent_token: %s", string(data))
	}
	if strings.Contains(string(data), "secret-token") {
		t.Fatalf("restore output JSON exposes token value: %s", string(data))
	}
}

func TestToolsRestoreSnapshotRequiresSnapshotID(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	_, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{})
	if err == nil {
		t.Fatal("RestoreSnapshot returned nil error for empty snapshot_id")
	}
	if daemon.restoreSnapshotCalls != 0 {
		t.Fatalf("RestoreSnapshot calls = %d, want 0", daemon.restoreSnapshotCalls)
	}
}
```

- [ ] **Step 2: Run restore tests and verify they fail**

Run:

```bash
go test ./internal/anvilmcp -run 'TestToolsRestoreSnapshot' -count=1
```

Expected: FAIL with compile errors for `RestoreSnapshot`, `RestoreSessionBindError`, or `newTools`.

- [ ] **Step 3: Implement session interface, internal constructor, and restore method**

In `internal/anvilmcp/tools.go`, add this interface after `Daemon`:

```go
type sessionStore interface {
	Exists(sessionName string) bool
	Bind(sessionName, vmID string) error
	ResolveIdentity(vmID, sessionName string) (string, error)
	RemoveVM(vmID string)
}
```

Change `Tools.sessions`:

```go
type Tools struct {
	daemon         Daemon
	sessions       sessionStore
	defaultTimeout time.Duration
}
```

Replace `NewTools` with this public constructor plus internal testable constructor:

```go
func NewTools(daemon Daemon, sessions *SessionStore, defaultTimeout time.Duration) *Tools {
	if sessions == nil {
		sessions = NewSessionStore()
	}
	return newTools(daemon, sessions, defaultTimeout)
}

func newTools(daemon Daemon, sessions sessionStore, defaultTimeout time.Duration) *Tools {
	if sessions == nil {
		sessions = NewSessionStore()
	}
	if defaultTimeout <= 0 {
		defaultTimeout = time.Duration(DefaultTimeoutSeconds) * time.Second
	}
	return &Tools{
		daemon:         daemon,
		sessions:       sessions,
		defaultTimeout: defaultTimeout,
	}
}
```

Add this error type after the input/output structs:

```go
type RestoreSessionBindError struct {
	SessionName  string
	RestoredVMID string
	Err          error
}

func (e *RestoreSessionBindError) Error() string {
	return fmt.Sprintf("failed to bind session %q to restored VM %q; restored VM was not deleted: %v", e.SessionName, e.RestoredVMID, e.Err)
}

func (e *RestoreSessionBindError) Unwrap() error {
	return e.Err
}
```

Add these methods before `DeleteSnapshot`:

```go
func (t *Tools) RestoreSnapshot(ctx context.Context, input RestoreSnapshotInput) (*RestoreSnapshotOutput, error) {
	snapshotID := strings.TrimSpace(input.SnapshotID)
	if snapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}

	sessionName := strings.TrimSpace(input.SessionName)
	if sessionName != "" && t.sessions.Exists(sessionName) {
		return nil, fmt.Errorf("session %q already exists", sessionName)
	}

	res, err := t.daemon.RestoreSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
	}

	if sessionName != "" {
		if err := t.sessions.Bind(sessionName, res.VMID); err != nil {
			return nil, &RestoreSessionBindError{
				SessionName:  sessionName,
				RestoredVMID: res.VMID,
				Err:          err,
			}
		}
	}

	return &RestoreSnapshotOutput{
		VMID:             res.VMID,
		GuestIP:          res.GuestIP,
		AgentURL:         res.AgentURL,
		Profile:          res.Profile,
		SourceSnapshotID: res.SourceSnapshotID,
		SessionName:      sessionName,
	}, nil
}

func (t *Tools) MCPRestoreSnapshot(ctx context.Context, req *mcp.CallToolRequest, input RestoreSnapshotInput) (*mcp.CallToolResult, RestoreSnapshotOutput, error) {
	out, err := t.RestoreSnapshot(ctx, input)
	if err != nil || out == nil {
		return nil, RestoreSnapshotOutput{}, err
	}
	return nil, *out, nil
}
```

- [ ] **Step 4: Run restore tests and full tool package tests**

Run:

```bash
go test ./internal/anvilmcp -run 'TestToolsRestoreSnapshot' -count=1
go test ./internal/anvilmcp -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit restore semantics**

```bash
git add internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go
git commit -m "feat: add snapshot restore MCP semantics"
```

---

### Task 4: Register Snapshot Tools in MCP Entrypoint

**Files:**
- Modify: `cmd/anvil-mcp/main.go`
- Create: `cmd/anvil-mcp/main_test.go`

- [ ] **Step 1: Write failing registration test**

Create `cmd/anvil-mcp/main_test.go`:

```go
package main

import "testing"

func TestToolRegistrationsIncludeSnapshotTools(t *testing.T) {
	registrations := toolRegistrations()
	names := make(map[string]bool, len(registrations))
	for _, registration := range registrations {
		if registration.name == "" {
			t.Fatal("tool registration has empty name")
		}
		if registration.description == "" {
			t.Fatalf("tool %q has empty description", registration.name)
		}
		if registration.register == nil {
			t.Fatalf("tool %q has nil register function", registration.name)
		}
		names[registration.name] = true
	}

	want := []string{
		"anvil_spawn_vm",
		"anvil_run_task",
		"anvil_get_vm_health",
		"anvil_stop_vm",
		"anvil_delete_vm",
		"anvil_create_snapshot",
		"anvil_list_snapshots",
		"anvil_restore_snapshot",
		"anvil_delete_snapshot",
	}
	for _, name := range want {
		if !names[name] {
			t.Fatalf("missing tool registration %q; names = %v", name, names)
		}
	}
	if len(registrations) != len(want) {
		t.Fatalf("registration count = %d, want %d", len(registrations), len(want))
	}
}
```

- [ ] **Step 2: Run cmd test and verify it fails**

Run:

```bash
go test ./cmd/anvil-mcp -run TestToolRegistrationsIncludeSnapshotTools -count=1
```

Expected: FAIL with `undefined: toolRegistrations`.

- [ ] **Step 3: Refactor registrations and add snapshot tools**

In `cmd/anvil-mcp/main.go`, add this type after `const version = "v0.1.0"`:

```go
type toolRegistration struct {
	name        string
	description string
	register    func(server *mcp.Server, tools *anvilmcp.Tools)
}
```

Add this function before `main`:

```go
func toolRegistrations() []toolRegistration {
	return []toolRegistration{
		{
			name:        "anvil_spawn_vm",
			description: "Create an ephemera VM and optionally bind a local session_name alias.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_spawn_vm", Description: "Create an ephemera VM and optionally bind a local session_name alias."}, tools.MCPSpawnVM)
			},
		},
		{
			name:        "anvil_run_task",
			description: "Run a prompt synchronously in an existing ephemera VM using vm_id or session_name.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_run_task", Description: "Run a prompt synchronously in an existing ephemera VM using vm_id or session_name."}, tools.MCPRunTask)
			},
		},
		{
			name:        "anvil_get_vm_health",
			description: "Return health for an existing ephemera VM agent using vm_id or session_name.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_get_vm_health", Description: "Return health for an existing ephemera VM agent using vm_id or session_name."}, tools.MCPHealth)
			},
		},
		{
			name:        "anvil_stop_vm",
			description: "Ask the ephemera VM agent to stop gracefully without deleting VM resources.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_stop_vm", Description: "Ask the ephemera VM agent to stop gracefully without deleting VM resources."}, tools.MCPStopVM)
			},
		},
		{
			name:        "anvil_delete_vm",
			description: "Delete an ephemera VM and release its session_name alias if present.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_delete_vm", Description: "Delete an ephemera VM and release its session_name alias if present."}, tools.MCPDeleteVM)
			},
		},
		{
			name:        "anvil_create_snapshot",
			description: "Create a full or diff snapshot for an ephemera VM using vm_id or session_name.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_create_snapshot", Description: "Create a full or diff snapshot for an ephemera VM using vm_id or session_name."}, tools.MCPCreateSnapshot)
			},
		},
		{
			name:        "anvil_list_snapshots",
			description: "List snapshots known to the ephemera daemon.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_list_snapshots", Description: "List snapshots known to the ephemera daemon."}, tools.MCPListSnapshots)
			},
		},
		{
			name:        "anvil_restore_snapshot",
			description: "Restore a new ephemera VM from a snapshot and optionally bind a session_name alias.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_restore_snapshot", Description: "Restore a new ephemera VM from a snapshot and optionally bind a session_name alias."}, tools.MCPRestoreSnapshot)
			},
		},
		{
			name:        "anvil_delete_snapshot",
			description: "Delete a snapshot by snapshot_id through the ephemera daemon.",
			register: func(server *mcp.Server, tools *anvilmcp.Tools) {
				mcp.AddTool(server, &mcp.Tool{Name: "anvil_delete_snapshot", Description: "Delete a snapshot by snapshot_id through the ephemera daemon."}, tools.MCPDeleteSnapshot)
			},
		},
	}
}
```

In `main`, replace the current direct `mcp.AddTool` calls with:

```go
	for _, registration := range toolRegistrations() {
		registration.register(server, tools)
	}
```

- [ ] **Step 4: Run cmd test and package build**

Run:

```bash
go test ./cmd/anvil-mcp -count=1
go build ./cmd/anvil-mcp
```

Expected: PASS and build succeeds.

- [ ] **Step 5: Commit MCP registration**

```bash
git add cmd/anvil-mcp/main.go cmd/anvil-mcp/main_test.go
git commit -m "feat: register snapshot MCP tools"
```

---

### Task 5: Update README and MCP Architecture Docs

**Files:**
- Modify: `README.md`
- Modify: `docs/architecture/mcp-architecture.md`

- [ ] **Step 1: Update README MCP tool table**

In `README.md`, find the `## IronClaw MCP 어댑터` section and replace the MCP tool table plus the sentence that says snapshot tools do not exist with:

```markdown
MCP tool:

| Tool | 역할 |
|---|---|
| `anvil_spawn_vm` | ephemera VM을 만들고 optional `session_name` alias를 연결한다. |
| `anvil_run_task` | `vm_id` 또는 `session_name`으로 VM에 prompt를 실행한다. |
| `anvil_get_vm_health` | VM agent health를 확인한다. |
| `anvil_stop_vm` | guest agent에 graceful stop을 요청한다. |
| `anvil_delete_vm` | host VM 리소스를 삭제하고 session alias를 해제한다. |
| `anvil_create_snapshot` | `vm_id` 또는 `session_name`으로 VM snapshot을 생성한다. |
| `anvil_list_snapshots` | daemon이 알고 있는 snapshot 목록을 조회한다. |
| `anvil_restore_snapshot` | `snapshot_id`에서 새 VM을 restore하고 optional `session_name` alias를 연결한다. |
| `anvil_delete_snapshot` | `snapshot_id`로 snapshot을 삭제한다. |

MCP v1은 얇은 runtime bridge다. workspace copy, snapshot alias, session alias 영속화,
HTTP MCP transport는 제공하지 않는다. Restore 응답은 daemon의 `agent_token`을
decode할 수 있지만 MCP output에는 노출하지 않는다. Restore 후 `session_name`
bind가 실패하면 adapter는 restored VM을 자동 삭제하지 않고 error에 restored VM ID를
포함한다.
```

In `README.md`, replace the limitation bullet `- MCP v1은 snapshot/restore tool을 제공하지 않는다.` with:

```markdown
- MCP v1은 snapshot/restore tool을 제공하지만 snapshot alias와 session alias 영속화는 제공하지 않는다.
```

- [ ] **Step 2: Update MCP architecture tool table and contracts**

In `docs/architecture/mcp-architecture.md`, replace the tool table under `## 도구 계약` with:

```markdown
| MCP tool | Daemon call | 목적 |
|---|---|---|
| `anvil_spawn_vm` | `POST /vms` | VM 생성 및 optional `session_name` alias binding |
| `anvil_run_task` | `POST /vms/{vm_id}/tasks` | daemon agent proxy를 통해 VM에서 prompt 실행 |
| `anvil_get_vm_health` | `GET /vms/{vm_id}/health` | daemon proxy를 통해 guest agent health 반환 |
| `anvil_stop_vm` | `POST /vms/{vm_id}/stop` | guest agent에 graceful stop 요청 |
| `anvil_delete_vm` | `DELETE /vms/{vm_id}` | VM resource 삭제 및 관련 session alias 해제 |
| `anvil_create_snapshot` | `POST /vms/{vm_id}/snapshot` | VM full 또는 diff snapshot 생성 |
| `anvil_list_snapshots` | `GET /snapshots` | 저장된 snapshot 목록 조회 |
| `anvil_restore_snapshot` | `POST /snapshots/{snapshot_id}/restore` | snapshot에서 새 VM restore 및 optional alias binding |
| `anvil_delete_snapshot` | `DELETE /snapshots/{snapshot_id}` | snapshot 삭제 |
```

After the `### anvil_delete_vm` section, add these sections:

````markdown
### `anvil_create_snapshot`

입력:

```json
{
  "vm_id": "optional-if-session-name-is-set",
  "session_name": "optional-if-vm-id-is-set",
  "stop_after": false,
  "type": "full"
}
```

동작:

- VM identity를 resolve한다.
- `type`은 생략, 빈 문자열, `full`, `diff`만 허용한다.
- `type`은 대소문자 차이를 정규화해 daemon에는 소문자로 전달한다.
- `POST /vms/{vm_id}/snapshot`을 호출한다.
- daemon snapshot response를 그대로 구조화해 반환한다.

### `anvil_list_snapshots`

입력:

```json
{}
```

동작:

- `GET /snapshots`를 호출한다.
- snapshot alias나 filtering은 제공하지 않는다.
- 출력은 `{ "snapshots": [...] }` wrapper object다.

### `anvil_restore_snapshot`

입력:

```json
{
  "snapshot_id": "snap-1",
  "session_name": "optional-local-alias"
}
```

동작:

- 빈 `snapshot_id`를 daemon 호출 전에 거부한다.
- `session_name`이 있으면 restore 전에 duplicate 여부를 검사한다.
- `POST /snapshots/{snapshot_id}/restore`를 호출한다.
- restore 성공 후 `session_name -> restored_vm_id`를 bind한다.
- duplicate 사전 검사와 사후 bind 사이의 race로 bind가 실패할 수 있다.
- 사후 bind 실패 시 restored VM은 자동 삭제하지 않는다. error는 restored VM ID와 직접 cleanup 필요성을 포함한다.
- MCP output에는 daemon restore response의 `agent_token`을 노출하지 않는다.

출력:

```json
{
  "vm_id": "vm-restored",
  "guest_ip": "10.0.1.9",
  "agent_url": "http://192.168.3.73:3000/vms/vm-restored/agent",
  "profile": "dev",
  "source_snapshot_id": "snap-1",
  "session_name": "optional-local-alias"
}
```

### `anvil_delete_snapshot`

입력:

```json
{
  "snapshot_id": "snap-1"
}
```

동작:

- 빈 `snapshot_id`를 daemon 호출 전에 거부한다.
- `DELETE /snapshots/{snapshot_id}`를 호출한다.
- diff snapshot이 참조 중인 full snapshot 삭제처럼 daemon이 non-2xx를 반환하면 status code와 body를 `DaemonError`로 보존한다.
````

In the `## 세션 alias 모델` rule list, add:

```markdown
- `anvil_restore_snapshot`은 restore 성공 후 optional alias를 새 VM ID에 연결한다.
- restore 후 alias bind가 실패하면 restored VM은 자동 삭제되지 않는다.
```

In the `## 보안 모델` table, ensure the guest token row reads:

```markdown
| Guest agent token | MCP output에는 노출하지 않고 daemon proxy가 주입한다. Restore response decode용 내부 struct에만 존재한다. |
```

Replace the future-scope list that says snapshot tools are not implemented with:

```markdown
MCP v1은 의도적으로 다음을 구현하지 않는다.

- workspace copy-in/copy-out
- snapshot alias 또는 snapshot name
- session alias 영속화
- session name으로 최신 snapshot 자동 선택
- HTTP MCP transport
- daemon API 의미 재해석
```

- [ ] **Step 3: Run docs consistency checks**

Run:

```bash
rg -n "MCP v1은 snapshot/restore tool을 제공하지 않는다|snapshot 생성 tool|snapshot restore tool" README.md docs/architecture/mcp-architecture.md
rg -n "anvil_create_snapshot|anvil_restore_snapshot|agent_token|restored VM" README.md docs/architecture/mcp-architecture.md
```

Expected: first command returns no matches. Second command returns the new tool and security text.

- [ ] **Step 4: Commit docs update**

```bash
git add README.md docs/architecture/mcp-architecture.md
git commit -m "docs: document snapshot MCP tools"
```

---

### Task 6: Final Verification

**Files:**
- Verify all files changed by Tasks 1 to 5.

- [ ] **Step 1: Run Go formatting**

Run:

```bash
gofmt -w internal/anvilmcp/daemon_client.go internal/anvilmcp/daemon_client_test.go internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go cmd/anvil-mcp/main.go cmd/anvil-mcp/main_test.go
```

Expected: command exits 0.

- [ ] **Step 2: Run focused tests**

Run:

```bash
go test ./internal/anvilmcp -count=1
go test ./cmd/anvil-mcp -count=1
```

Expected: PASS.

- [ ] **Step 3: Run full test suite**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 4: Run build verification**

Run:

```bash
go build ./cmd/anvil-mcp
go build ./cmd/goose-daemon
```

Expected: both builds succeed.

- [ ] **Step 5: Inspect diff**

Run:

```bash
git diff --check
git status --short
```

Expected: `git diff --check` prints no output. `git status --short` shows only intended changes if the previous task commits were not made, or a clean tree if every task commit was made.

- [ ] **Step 6: Final commit if needed**

If formatting or verification adjusted files after the task commits, run:

```bash
git add internal/anvilmcp/daemon_client.go internal/anvilmcp/daemon_client_test.go internal/anvilmcp/tools.go internal/anvilmcp/tools_test.go cmd/anvil-mcp/main.go cmd/anvil-mcp/main_test.go README.md docs/architecture/mcp-architecture.md
git commit -m "chore: verify snapshot MCP tools"
```

Expected: commit is created only when `git status --short` showed changes.

---

## Self-Review

Spec coverage:

- `anvil_create_snapshot`: Task 1 daemon client, Task 2 tool handler, Task 4 registration, Task 5 docs.
- `anvil_list_snapshots`: Task 1 daemon client, Task 2 wrapper output, Task 4 registration, Task 5 docs.
- `anvil_restore_snapshot`: Task 1 daemon client, Task 3 restore semantics, Task 4 registration, Task 5 docs.
- `anvil_delete_snapshot`: Task 1 daemon client, Task 2 tool handler, Task 4 registration, Task 5 docs.
- Optional `session_name -> restored_vm_id` bind: Task 3 tests and implementation.
- Restore bind race handling without automatic delete: Task 3 `RestoreSessionBindError` test and implementation.
- MCP output token redaction: Task 3 JSON output test and `RestoreSnapshotOutput`.
- Snapshot alias exclusion: Task 5 docs.
- README and MCP architecture updates: Task 5.
- Unit test verification and full suite: Task 6.

Placeholder scan:

- The plan contains no forbidden placeholder token, no deferred implementation marker, and no instruction that asks an engineer to infer missing test logic.

Type consistency:

- Daemon client structs are defined in Task 1 before tool layer usage in Task 2.
- `RestoreSnapshotInput`, `RestoreSnapshotOutput`, and `RestoreSessionBindError` are used consistently in Task 3.
- `toolRegistrations` is introduced before `cmd/anvil-mcp/main_test.go` expects it to pass.
