package anvilmcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxTimeoutSeconds = int((24 * time.Hour) / time.Second)
	maxFlockRoles     = 20
	// MaxWorkspaceFileBytes is the v1 single-file copy size limit.
	MaxWorkspaceFileBytes = 4 << 20
)

type Daemon interface {
	SpawnVM(ctx context.Context, req SpawnVMRequest) (*SpawnVMResponse, error)
	RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error)
	CopyIn(ctx context.Context, vmID, workspacePath, content string, overwrite bool) (*RawDaemonResponse, error)
	CopyOut(ctx context.Context, vmID, workspacePath string) (string, error)
	Health(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	Stop(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	CreateSnapshot(ctx context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error)
	ListSnapshots(ctx context.Context) ([]SnapshotInfo, error)
	RestoreSnapshot(ctx context.Context, snapshotID string, req RestoreSnapshotRequest) (*RestoreSnapshotResponse, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) (*RawDaemonResponse, error)
	CreateFlock(ctx context.Context, req FlockCreateRequest) (*FlockCreateResponse, error)
	ListFlocks(ctx context.Context) ([]FlockInfo, error)
	GetFlock(ctx context.Context, flockID string) (*FlockInfo, error)
	DeleteFlock(ctx context.Context, flockID string) (*RawDaemonResponse, error)
	PostTownWall(ctx context.Context, flockID string, req TownWallPostRequest) (*TownWallMessage, error)
	TownWallHistory(ctx context.Context, flockID string) ([]TownWallMessage, error)
}

type sessionStore interface {
	Exists(sessionName string) bool
	Bind(sessionName, vmID string) error
	ResolveIdentity(vmID, sessionName string) (string, error)
	RemoveVM(vmID string) bool
	removeVMAliases(vmID string) map[string]string
	restoreAliases(aliases map[string]string)
}

type Tools struct {
	daemon               Daemon
	sessions             sessionStore
	defaultTimeout       time.Duration
	sessionStorePath     string
	defaultTenantID      string
	auditLogPath         string
	sessionPersistenceMu sync.Mutex
}

type ToolsOptions struct {
	SessionStorePath string
	DefaultTenantID  string
	AuditLogPath     string
}

type SpawnVMInput struct {
	Profile      string `json:"profile,omitempty"`
	SessionName  string `json:"session_name,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
}

type SpawnVMOutput struct {
	VMID         string `json:"vm_id"`
	GuestIP      string `json:"guest_ip"`
	AgentURL     string `json:"agent_url"`
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
	SessionName  string `json:"session_name,omitempty"`
}

type RunTaskInput struct {
	VMID           string `json:"vm_id,omitempty"`
	SessionName    string `json:"session_name,omitempty"`
	TenantID       string `json:"tenant_id,omitempty"`
	Prompt         string `json:"prompt"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type CopyInInput struct {
	VMID        string `json:"vm_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding,omitempty"`
	Overwrite   bool   `json:"overwrite,omitempty"`
}

type CopyOutInput struct {
	VMID        string `json:"vm_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	Path        string `json:"path"`
	Encoding    string `json:"encoding,omitempty"`
}

type CopyOutOutput struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type VMIdentityInput struct {
	VMID        string `json:"vm_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
}

type CreateSnapshotInput struct {
	VMID        string `json:"vm_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	StopAfter   bool   `json:"stop_after"`
	Type        string `json:"type,omitempty"`
}

type ListSnapshotsOutput struct {
	Snapshots []SnapshotInfo `json:"snapshots"`
}

type RestoreSnapshotInput struct {
	SnapshotID   string `json:"snapshot_id"`
	SessionName  string `json:"session_name,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
}

type RestoreSnapshotOutput struct {
	VMID             string `json:"vm_id"`
	GuestIP          string `json:"guest_ip"`
	AgentURL         string `json:"agent_url"`
	Profile          string `json:"profile,omitempty"`
	TenantID         string `json:"tenant_id,omitempty"`
	EgressPolicy     string `json:"egress_policy,omitempty"`
	SourceSnapshotID string `json:"source_snapshot_id"`
	SessionName      string `json:"session_name,omitempty"`
}

type SnapshotIdentityInput struct {
	SnapshotID string `json:"snapshot_id"`
	TenantID   string `json:"tenant_id,omitempty"`
}

type SpawnFlockInput struct {
	Task         string   `json:"task"`
	Roles        []string `json:"roles"`
	TenantID     string   `json:"tenant_id,omitempty"`
	EgressPolicy string   `json:"egress_policy,omitempty"`
}

type SpawnFlockOutput struct {
	FlockID      string           `json:"flock_id"`
	Task         string           `json:"task"`
	TenantID     string           `json:"tenant_id,omitempty"`
	EgressPolicy string           `json:"egress_policy,omitempty"`
	Agents       []FlockAgentInfo `json:"agents"`
	TownWallURL  string           `json:"townwall_url"`
	PostURL      string           `json:"post_url"`
}

type ListFlocksInput struct {
	TenantID string `json:"tenant_id,omitempty"`
}

type ListFlocksOutput struct {
	Flocks []FlockInfo `json:"flocks"`
}

type FlockIdentityInput struct {
	FlockID  string `json:"flock_id"`
	TenantID string `json:"tenant_id,omitempty"`
}

type TownWallPostInput struct {
	FlockID  string `json:"flock_id"`
	AgentID  string `json:"agent_id"`
	Body     string `json:"body"`
	TenantID string `json:"tenant_id,omitempty"`
}

type TownWallHistoryOutput struct {
	Messages []TownWallMessage `json:"messages"`
}

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

func NewTools(daemon Daemon, sessions *SessionStore, defaultTimeout time.Duration) *Tools {
	if sessions == nil {
		sessions = NewSessionStore()
	}
	return newToolsWithSessionStorePath(daemon, sessions, defaultTimeout, "")
}

func NewToolsWithSessionStorePath(daemon Daemon, sessions *SessionStore, defaultTimeout time.Duration, sessionStorePath string) *Tools {
	if sessions == nil {
		sessions = NewSessionStore()
	}
	return newToolsWithOptions(daemon, sessions, defaultTimeout, ToolsOptions{SessionStorePath: sessionStorePath})
}

func NewToolsWithOptions(daemon Daemon, sessions *SessionStore, defaultTimeout time.Duration, opts ToolsOptions) *Tools {
	if sessions == nil {
		sessions = NewSessionStore()
	}
	return newToolsWithOptions(daemon, sessions, defaultTimeout, opts)
}

func newTools(daemon Daemon, sessions sessionStore, defaultTimeout time.Duration) *Tools {
	return newToolsWithOptions(daemon, sessions, defaultTimeout, ToolsOptions{})
}

func newToolsWithSessionStorePath(daemon Daemon, sessions sessionStore, defaultTimeout time.Duration, sessionStorePath string) *Tools {
	return newToolsWithOptions(daemon, sessions, defaultTimeout, ToolsOptions{SessionStorePath: sessionStorePath})
}

func newToolsWithOptions(daemon Daemon, sessions sessionStore, defaultTimeout time.Duration, opts ToolsOptions) *Tools {
	if sessions == nil {
		sessions = NewSessionStore()
	}
	if defaultTimeout <= 0 {
		defaultTimeout = time.Duration(DefaultTimeoutSeconds) * time.Second
	}
	defaultTenantID := strings.TrimSpace(opts.DefaultTenantID)
	if defaultTenantID != "" {
		normalized, err := NormalizeTenantID(defaultTenantID)
		if err == nil {
			defaultTenantID = normalized
		}
	}
	return &Tools{
		daemon:           daemon,
		sessions:         sessions,
		defaultTimeout:   defaultTimeout,
		sessionStorePath: strings.TrimSpace(opts.SessionStorePath),
		defaultTenantID:  defaultTenantID,
		auditLogPath:     strings.TrimSpace(opts.AuditLogPath),
	}
}

func (t *Tools) SpawnVM(ctx context.Context, input SpawnVMInput) (*SpawnVMOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	egressPolicy, err := NormalizeEgressPolicy(input.EgressPolicy)
	if err != nil {
		return nil, err
	}
	profile := strings.TrimSpace(input.Profile)
	sessionName := strings.TrimSpace(input.SessionName)
	if sessionName != "" && t.sessionExists(sessionName) {
		return nil, fmt.Errorf("session %q already exists", sessionName)
	}

	res, err := t.daemon.SpawnVM(ctx, SpawnVMRequest{
		Profile:      profile,
		TenantID:     tenantID,
		EgressPolicy: string(egressPolicy),
	})
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", sessionName, "anvil_spawn_vm", "POST /vms", err)
	}

	if sessionName != "" {
		t.sessionPersistenceMu.Lock()
		bindErr := t.sessions.Bind(sessionName, res.VMID)
		var saveErr error
		if bindErr == nil {
			saveErr = t.saveSessionStore()
			if saveErr != nil {
				t.sessions.RemoveVM(res.VMID)
			}
		}
		t.sessionPersistenceMu.Unlock()

		if bindErr != nil {
			_, _ = t.deleteSpawnedVM(context.WithoutCancel(ctx), res.VMID)
			return nil, bindErr
		}
		if saveErr != nil {
			deleteRes, deleteErr := t.deleteSpawnedVM(context.WithoutCancel(ctx), res.VMID)
			if deleteErr != nil {
				return nil, fmt.Errorf("failed to persist session %q for spawned VM %q: %w; cleanup delete failed: %v", sessionName, res.VMID, saveErr, deleteErr)
			}
			if deleteRes != nil {
				return nil, fmt.Errorf("failed to persist session %q for spawned VM %q: %w; spawned VM was deleted with status %d body %q", sessionName, res.VMID, saveErr, deleteRes.StatusCode, deleteRes.Body)
			}
			return nil, fmt.Errorf("failed to persist session %q for spawned VM %q: %w; spawned VM cleanup was attempted", sessionName, res.VMID, saveErr)
		}
	}

	out := &SpawnVMOutput{
		VMID:         res.VMID,
		GuestIP:      res.GuestIP,
		AgentURL:     res.AgentURL,
		Profile:      res.Profile,
		TenantID:     res.TenantID,
		EgressPolicy: res.EgressPolicy,
		SessionName:  sessionName,
	}
	if err := t.auditSuccess(tenantID, res.VMID, sessionName, "anvil_spawn_vm", "POST /vms"); err != nil {
		return nil, fmt.Errorf("spawn VM %q succeeded but failed to append runtime audit: %w", res.VMID, err)
	}
	return out, nil
}

func (t *Tools) MCPSpawnVM(ctx context.Context, req *mcp.CallToolRequest, input SpawnVMInput) (*mcp.CallToolResult, SpawnVMOutput, error) {
	out, err := t.SpawnVM(ctx, input)
	if err != nil || out == nil {
		return nil, SpawnVMOutput{}, err
	}
	return nil, *out, nil
}

func (t *Tools) RunTask(ctx context.Context, input RunTaskInput) (*RawDaemonResponse, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return nil, fmt.Errorf("prompt must be non-empty")
	}
	if input.TimeoutSeconds < 0 {
		return nil, fmt.Errorf("timeout_seconds must be non-negative")
	}
	if input.TimeoutSeconds > maxTimeoutSeconds {
		return nil, fmt.Errorf("timeout_seconds must be <= %d", maxTimeoutSeconds)
	}

	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}

	timeout := t.defaultTimeout
	if input.TimeoutSeconds > 0 {
		timeout = time.Duration(input.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := t.daemon.RunTask(ctx, vmID, input.Prompt)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, vmID, input.SessionName, "anvil_run_task", "POST /vms/{vm_id}/tasks", err)
	}
	if err := t.auditSuccess(tenantID, vmID, input.SessionName, "anvil_run_task", "POST /vms/{vm_id}/tasks"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPRunTask(ctx context.Context, req *mcp.CallToolRequest, input RunTaskInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.RunTask(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) CopyIn(ctx context.Context, input CopyInInput) (*RawDaemonResponse, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	workspacePath, err := normalizeWorkspacePath(input.Path)
	if err != nil {
		return nil, err
	}
	encoding, err := normalizeWorkspaceEncoding(input.Encoding)
	if err != nil {
		return nil, err
	}
	content := input.Content
	if encoding == "base64" {
		data, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("content must be valid base64")
		}
		content = string(data)
	}
	if len([]byte(content)) > MaxWorkspaceFileBytes {
		return nil, fmt.Errorf("content exceeds %d byte workspace file limit", MaxWorkspaceFileBytes)
	}
	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}
	out, err := t.daemon.CopyIn(ctx, vmID, workspacePath, content, input.Overwrite)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, vmID, input.SessionName, "anvil_copy_in", "PUT /vms/{vm_id}/workspace", err)
	}
	if err := t.auditSuccess(tenantID, vmID, input.SessionName, "anvil_copy_in", "PUT /vms/{vm_id}/workspace"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPCopyIn(ctx context.Context, req *mcp.CallToolRequest, input CopyInInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.CopyIn(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) CopyOut(ctx context.Context, input CopyOutInput) (*CopyOutOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	workspacePath, err := normalizeWorkspacePath(input.Path)
	if err != nil {
		return nil, err
	}
	encoding, err := normalizeWorkspaceEncoding(input.Encoding)
	if err != nil {
		return nil, err
	}
	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}
	content, err := t.daemon.CopyOut(ctx, vmID, workspacePath)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, vmID, input.SessionName, "anvil_copy_out", "GET /vms/{vm_id}/workspace", err)
	}
	if encoding == "base64" {
		content = base64.StdEncoding.EncodeToString([]byte(content))
	}
	if err := t.auditSuccess(tenantID, vmID, input.SessionName, "anvil_copy_out", "GET /vms/{vm_id}/workspace"); err != nil {
		return nil, err
	}
	return &CopyOutOutput{
		Path:     workspacePath,
		Content:  content,
		Encoding: encoding,
	}, nil
}

func (t *Tools) MCPCopyOut(ctx context.Context, req *mcp.CallToolRequest, input CopyOutInput) (*mcp.CallToolResult, CopyOutOutput, error) {
	out, err := t.CopyOut(ctx, input)
	if err != nil || out == nil {
		return nil, CopyOutOutput{}, err
	}
	return nil, *out, nil
}

func (t *Tools) Health(ctx context.Context, input VMIdentityInput) (*RawDaemonResponse, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}
	out, err := t.daemon.Health(ctx, vmID)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, vmID, input.SessionName, "anvil_get_vm_health", "GET /vms/{vm_id}/health", err)
	}
	if err := t.auditSuccess(tenantID, vmID, input.SessionName, "anvil_get_vm_health", "GET /vms/{vm_id}/health"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPHealth(ctx context.Context, req *mcp.CallToolRequest, input VMIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.Health(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) StopVM(ctx context.Context, input VMIdentityInput) (*RawDaemonResponse, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}
	out, err := t.daemon.Stop(ctx, vmID)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, vmID, input.SessionName, "anvil_stop_vm", "POST /vms/{vm_id}/stop", err)
	}
	if err := t.auditSuccess(tenantID, vmID, input.SessionName, "anvil_stop_vm", "POST /vms/{vm_id}/stop"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPStopVM(ctx context.Context, req *mcp.CallToolRequest, input VMIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.StopVM(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) DeleteVM(ctx context.Context, input VMIdentityInput) (*RawDaemonResponse, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}

	res, err := t.daemon.Delete(ctx, vmID)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, vmID, input.SessionName, "anvil_delete_vm", "DELETE /vms/{vm_id}", err)
	}

	t.sessionPersistenceMu.Lock()
	removedAliases := t.sessions.removeVMAliases(vmID)
	var saveErr error
	if len(removedAliases) > 0 {
		saveErr = t.saveSessionStore()
		if saveErr != nil {
			t.sessions.restoreAliases(removedAliases)
		}
	}
	t.sessionPersistenceMu.Unlock()

	if saveErr != nil {
		statusCode := 0
		body := ""
		if res != nil {
			statusCode = res.StatusCode
			body = res.Body
		}
		return nil, fmt.Errorf("delete VM %q succeeded with status %d body %s, but failed to persist removed session aliases: %w", vmID, statusCode, body, saveErr)
	}
	if err := t.auditSuccess(tenantID, vmID, input.SessionName, "anvil_delete_vm", "DELETE /vms/{vm_id}"); err != nil {
		return nil, err
	}
	return res, nil
}

func (t *Tools) MCPDeleteVM(ctx context.Context, req *mcp.CallToolRequest, input VMIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.DeleteVM(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) CreateSnapshot(ctx context.Context, input CreateSnapshotInput) (*SnapshotInfo, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	snapshotType, err := normalizeSnapshotType(input.Type)
	if err != nil {
		return nil, err
	}

	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}

	out, err := t.daemon.CreateSnapshot(ctx, vmID, CreateSnapshotRequest{
		StopAfter: input.StopAfter,
		Type:      snapshotType,
		TenantID:  tenantID,
	})
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, vmID, input.SessionName, "anvil_create_snapshot", "POST /vms/{vm_id}/snapshot", err)
	}
	if input.StopAfter {
		t.sessionPersistenceMu.Lock()
		removedAliases := t.sessions.removeVMAliases(vmID)
		var saveErr error
		if len(removedAliases) > 0 {
			saveErr = t.saveSessionStore()
			if saveErr != nil {
				t.sessions.restoreAliases(removedAliases)
			}
		}
		t.sessionPersistenceMu.Unlock()

		if saveErr != nil {
			snapshotID := ""
			if out != nil {
				snapshotID = out.SnapshotID
			}
			return nil, fmt.Errorf("snapshot %q for VM %q succeeded, but failed to persist removed session aliases: %w", snapshotID, vmID, saveErr)
		}
	}
	if err := t.auditSuccess(tenantID, vmID, input.SessionName, "anvil_create_snapshot", "POST /vms/{vm_id}/snapshot"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPCreateSnapshot(ctx context.Context, req *mcp.CallToolRequest, input CreateSnapshotInput) (*mcp.CallToolResult, SnapshotInfo, error) {
	out, err := t.CreateSnapshot(ctx, input)
	if err != nil || out == nil {
		return nil, SnapshotInfo{}, err
	}
	return nil, *out, nil
}

func (t *Tools) ListSnapshots(ctx context.Context) (*ListSnapshotsOutput, error) {
	tenantID, err := t.resolveTenantID("")
	if err != nil {
		return nil, err
	}
	snapshots, err := t.daemon.ListSnapshots(ctx)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_list_snapshots", "GET /snapshots", err)
	}
	if snapshots == nil {
		snapshots = []SnapshotInfo{}
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_list_snapshots", "GET /snapshots"); err != nil {
		return nil, err
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

func (t *Tools) RestoreSnapshot(ctx context.Context, input RestoreSnapshotInput) (*RestoreSnapshotOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	snapshotID := strings.TrimSpace(input.SnapshotID)
	if snapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	egressPolicy, err := NormalizeEgressPolicy(input.EgressPolicy)
	if err != nil {
		return nil, err
	}

	sessionName := strings.TrimSpace(input.SessionName)
	if sessionName != "" && t.sessionExists(sessionName) {
		return nil, fmt.Errorf("session %q already exists", sessionName)
	}

	res, err := t.daemon.RestoreSnapshot(ctx, snapshotID, RestoreSnapshotRequest{
		TenantID:     tenantID,
		EgressPolicy: string(egressPolicy),
	})
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", sessionName, "anvil_restore_snapshot", "POST /snapshots/{snapshot_id}/restore", err)
	}

	if sessionName != "" {
		t.sessionPersistenceMu.Lock()
		bindErr := t.sessions.Bind(sessionName, res.VMID)
		var saveErr error
		if bindErr == nil {
			saveErr = t.saveSessionStore()
			if saveErr != nil {
				t.sessions.RemoveVM(res.VMID)
			}
		}
		t.sessionPersistenceMu.Unlock()

		if bindErr != nil {
			return nil, &RestoreSessionBindError{
				SessionName:  sessionName,
				RestoredVMID: res.VMID,
				Err:          bindErr,
			}
		}
		if saveErr != nil {
			return nil, &RestoreSessionBindError{
				SessionName:  sessionName,
				RestoredVMID: res.VMID,
				Err:          saveErr,
			}
		}
	}

	out := &RestoreSnapshotOutput{
		VMID:             res.VMID,
		GuestIP:          res.GuestIP,
		AgentURL:         res.AgentURL,
		Profile:          res.Profile,
		TenantID:         res.TenantID,
		EgressPolicy:     res.EgressPolicy,
		SourceSnapshotID: res.SourceSnapshotID,
		SessionName:      sessionName,
	}
	if err := t.auditSuccess(tenantID, res.VMID, sessionName, "anvil_restore_snapshot", "POST /snapshots/{snapshot_id}/restore"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPRestoreSnapshot(ctx context.Context, req *mcp.CallToolRequest, input RestoreSnapshotInput) (*mcp.CallToolResult, RestoreSnapshotOutput, error) {
	out, err := t.RestoreSnapshot(ctx, input)
	if err != nil || out == nil {
		return nil, RestoreSnapshotOutput{}, err
	}
	return nil, *out, nil
}

func (t *Tools) DeleteSnapshot(ctx context.Context, input SnapshotIdentityInput) (*RawDaemonResponse, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	snapshotID := strings.TrimSpace(input.SnapshotID)
	if snapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	out, err := t.daemon.DeleteSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_delete_snapshot", "DELETE /snapshots/{snapshot_id}", err)
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_delete_snapshot", "DELETE /snapshots/{snapshot_id}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPDeleteSnapshot(ctx context.Context, req *mcp.CallToolRequest, input SnapshotIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.DeleteSnapshot(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) SpawnFlock(ctx context.Context, input SpawnFlockInput) (*SpawnFlockOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	egressPolicy, err := NormalizeEgressPolicy(input.EgressPolicy)
	if err != nil {
		return nil, err
	}
	task := strings.TrimSpace(input.Task)
	if task == "" {
		return nil, fmt.Errorf("task must be non-empty")
	}
	roles, err := normalizeFlockRoles(input.Roles)
	if err != nil {
		return nil, err
	}

	res, err := t.daemon.CreateFlock(ctx, FlockCreateRequest{
		Task:         task,
		Roles:        roles,
		TenantID:     tenantID,
		EgressPolicy: string(egressPolicy),
	})
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_spawn_flock", "POST /flocks", err)
	}
	agents := []FlockAgentInfo{}
	if res.Agents != nil {
		agents = res.Agents
	}
	out := &SpawnFlockOutput{
		FlockID:      res.FlockID,
		Task:         res.Task,
		TenantID:     res.TenantID,
		EgressPolicy: res.EgressPolicy,
		Agents:       agents,
		TownWallURL:  res.TownWallURL,
		PostURL:      res.PostURL,
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_spawn_flock", "POST /flocks"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPSpawnFlock(ctx context.Context, req *mcp.CallToolRequest, input SpawnFlockInput) (*mcp.CallToolResult, SpawnFlockOutput, error) {
	out, err := t.SpawnFlock(ctx, input)
	if err != nil || out == nil {
		return nil, SpawnFlockOutput{}, err
	}
	return nil, *out, nil
}

func (t *Tools) ListFlocks(ctx context.Context, input ListFlocksInput) (*ListFlocksOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	flocks, err := t.daemon.ListFlocks(ctx)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_list_flocks", "GET /flocks", err)
	}
	if flocks == nil {
		flocks = []FlockInfo{}
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_list_flocks", "GET /flocks"); err != nil {
		return nil, err
	}
	return &ListFlocksOutput{Flocks: flocks}, nil
}

func (t *Tools) MCPListFlocks(ctx context.Context, req *mcp.CallToolRequest, input ListFlocksInput) (*mcp.CallToolResult, ListFlocksOutput, error) {
	out, err := t.ListFlocks(ctx, input)
	if err != nil || out == nil {
		return nil, ListFlocksOutput{}, err
	}
	return nil, *out, nil
}

func (t *Tools) GetFlock(ctx context.Context, input FlockIdentityInput) (*FlockInfo, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	flockID, err := requireFlockID(input.FlockID)
	if err != nil {
		return nil, err
	}
	out, err := t.daemon.GetFlock(ctx, flockID)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_get_flock", "GET /flocks/{flock_id}", err)
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_get_flock", "GET /flocks/{flock_id}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPGetFlock(ctx context.Context, req *mcp.CallToolRequest, input FlockIdentityInput) (*mcp.CallToolResult, FlockInfo, error) {
	out, err := t.GetFlock(ctx, input)
	if err != nil || out == nil {
		return nil, FlockInfo{}, err
	}
	return nil, *out, nil
}

func (t *Tools) DeleteFlock(ctx context.Context, input FlockIdentityInput) (*RawDaemonResponse, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	flockID, err := requireFlockID(input.FlockID)
	if err != nil {
		return nil, err
	}
	out, err := t.daemon.DeleteFlock(ctx, flockID)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_delete_flock", "DELETE /flocks/{flock_id}", err)
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_delete_flock", "DELETE /flocks/{flock_id}"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPDeleteFlock(ctx context.Context, req *mcp.CallToolRequest, input FlockIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.DeleteFlock(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) PostTownWall(ctx context.Context, input TownWallPostInput) (*TownWallMessage, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	flockID, err := requireFlockID(input.FlockID)
	if err != nil {
		return nil, err
	}
	agentID := strings.TrimSpace(input.AgentID)
	if agentID == "" {
		return nil, fmt.Errorf("agent_id must be non-empty")
	}
	if strings.TrimSpace(input.Body) == "" {
		return nil, fmt.Errorf("body must be non-empty")
	}
	out, err := t.daemon.PostTownWall(ctx, flockID, TownWallPostRequest{
		AgentID: agentID,
		Body:    input.Body,
	})
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_post_townwall", "POST /flocks/{flock_id}/post", err)
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_post_townwall", "POST /flocks/{flock_id}/post"); err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Tools) MCPPostTownWall(ctx context.Context, req *mcp.CallToolRequest, input TownWallPostInput) (*mcp.CallToolResult, TownWallMessage, error) {
	out, err := t.PostTownWall(ctx, input)
	if err != nil || out == nil {
		return nil, TownWallMessage{}, err
	}
	return nil, *out, nil
}

func (t *Tools) TownWallHistory(ctx context.Context, input FlockIdentityInput) (*TownWallHistoryOutput, error) {
	tenantID, err := t.resolveTenantID(input.TenantID)
	if err != nil {
		return nil, err
	}
	flockID, err := requireFlockID(input.FlockID)
	if err != nil {
		return nil, err
	}
	messages, err := t.daemon.TownWallHistory(ctx, flockID)
	if err != nil {
		return nil, t.auditFailureAndReturn(tenantID, "", "", "anvil_get_townwall_history", "GET /flocks/{flock_id}/wall/history", err)
	}
	if messages == nil {
		messages = []TownWallMessage{}
	}
	if err := t.auditSuccess(tenantID, "", "", "anvil_get_townwall_history", "GET /flocks/{flock_id}/wall/history"); err != nil {
		return nil, err
	}
	return &TownWallHistoryOutput{Messages: messages}, nil
}

func (t *Tools) MCPTownWallHistory(ctx context.Context, req *mcp.CallToolRequest, input FlockIdentityInput) (*mcp.CallToolResult, TownWallHistoryOutput, error) {
	out, err := t.TownWallHistory(ctx, input)
	if err != nil || out == nil {
		return nil, TownWallHistoryOutput{}, err
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

func normalizeFlockRoles(roles []string) ([]string, error) {
	if len(roles) == 0 {
		return nil, fmt.Errorf("roles must contain at least one role")
	}
	if len(roles) > maxFlockRoles {
		return nil, fmt.Errorf("roles must contain at most %d roles", maxFlockRoles)
	}
	normalized := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" {
			return nil, fmt.Errorf("roles must not contain empty role")
		}
		if strings.Contains(role, "/") || strings.Contains(role, `\`) {
			return nil, fmt.Errorf("roles must not contain path separators")
		}
		normalized = append(normalized, role)
	}
	return normalized, nil
}

func requireFlockID(value string) (string, error) {
	flockID := strings.TrimSpace(value)
	if flockID == "" {
		return "", fmt.Errorf("flock_id is required")
	}
	if strings.Contains(flockID, "/") || strings.Contains(flockID, `\`) {
		return "", fmt.Errorf("flock_id must not contain path separators")
	}
	return flockID, nil
}

func normalizeWorkspacePath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || strings.Contains(trimmed, `\`) {
		return "", fmt.Errorf("path must be a non-empty relative workspace path")
	}
	clean := path.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path must stay within workspace")
	}
	return clean, nil
}

func normalizeWorkspaceEncoding(value string) (string, error) {
	encoding := strings.ToLower(strings.TrimSpace(value))
	switch encoding {
	case "", "text":
		return "text", nil
	case "base64":
		return "base64", nil
	default:
		return "", fmt.Errorf("encoding must be empty, text, or base64")
	}
}

func (t *Tools) resolveIdentity(vmID, sessionName string) (string, error) {
	t.sessionPersistenceMu.Lock()
	defer t.sessionPersistenceMu.Unlock()

	return t.sessions.ResolveIdentity(vmID, sessionName)
}

func (t *Tools) resolveTenantID(inputTenantID string) (string, error) {
	tenantID := strings.TrimSpace(inputTenantID)
	if tenantID == "" {
		tenantID = t.defaultTenantID
	}
	if tenantID == "" {
		if t.auditLogPath != "" {
			return "", fmt.Errorf("tenant_id or ANVIL_MCP_TENANT_ID is required when runtime audit is enabled")
		}
		return "", nil
	}
	return NormalizeTenantID(tenantID)
}

func (t *Tools) auditSuccess(tenantID, vmID, sessionAlias, toolName, daemonOperation string) error {
	if t.auditLogPath == "" || tenantID == "" {
		return nil
	}
	return AppendRuntimeAudit(t.auditLogPath, RuntimeAuditRecord{
		Timestamp:       time.Now().UTC(),
		TenantID:        tenantID,
		VMID:            vmID,
		SessionAlias:    strings.TrimSpace(sessionAlias),
		ToolName:        toolName,
		DaemonOperation: daemonOperation,
		ResultCode:      "success",
	})
}

func (t *Tools) auditFailureAndReturn(tenantID, vmID, sessionAlias, toolName, daemonOperation string, err error) error {
	if auditErr := t.auditFailure(tenantID, vmID, sessionAlias, toolName, daemonOperation, err); auditErr != nil {
		return fmt.Errorf("%w; failed to append runtime audit: %v", err, auditErr)
	}
	return err
}

func (t *Tools) auditFailure(tenantID, vmID, sessionAlias, toolName, daemonOperation string, err error) error {
	if t.auditLogPath == "" || tenantID == "" {
		return nil
	}
	return AppendRuntimeAudit(t.auditLogPath, RuntimeAuditRecord{
		Timestamp:       time.Now().UTC(),
		TenantID:        tenantID,
		VMID:            vmID,
		SessionAlias:    strings.TrimSpace(sessionAlias),
		ToolName:        toolName,
		DaemonOperation: daemonOperation,
		ResultCode:      "error",
		Error:           runtimeAuditErrorMessage(err),
	})
}

func runtimeAuditErrorMessage(err error) string {
	if err == nil {
		return "operation failed"
	}
	var daemonErr *DaemonError
	if errors.As(err, &daemonErr) {
		return fmt.Sprintf("daemon returned status %d", daemonErr.StatusCode)
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "operation failed"
	}
	lower := strings.ToLower(msg)
	for _, sensitive := range []string{"agent_token", "authorization", "bearer ", "secret", "token"} {
		if strings.Contains(lower, sensitive) {
			return "operation failed"
		}
	}
	if len(msg) > 240 {
		msg = msg[:240] + "..."
	}
	return msg
}

func (t *Tools) sessionExists(sessionName string) bool {
	t.sessionPersistenceMu.Lock()
	defer t.sessionPersistenceMu.Unlock()

	return t.sessions.Exists(sessionName)
}

func (t *Tools) saveSessionStore() error {
	if t.sessionStorePath == "" {
		return nil
	}
	saver, ok := t.sessions.(interface {
		Save(string) error
	})
	if !ok {
		return fmt.Errorf("save session store %q: configured session store does not support persistence", t.sessionStorePath)
	}
	if err := saver.Save(t.sessionStorePath); err != nil {
		return fmt.Errorf("save session store %q: %w", t.sessionStorePath, err)
	}
	return nil
}

func (t *Tools) deleteSpawnedVM(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return t.daemon.Delete(cleanupCtx, vmID)
}
