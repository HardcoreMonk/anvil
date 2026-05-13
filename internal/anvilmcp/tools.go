package anvilmcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxTimeoutSeconds = int((24 * time.Hour) / time.Second)
	// MaxWorkspaceFileBytes is the v1 single-file copy size limit.
	MaxWorkspaceFileBytes = 4 << 20
)

type Daemon interface {
	SpawnVM(ctx context.Context, profile string) (*SpawnVMResponse, error)
	RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error)
	CopyIn(ctx context.Context, vmID, workspacePath, content string, overwrite bool) (*RawDaemonResponse, error)
	CopyOut(ctx context.Context, vmID, workspacePath string) (string, error)
	Health(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	Stop(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error)
	CreateSnapshot(ctx context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error)
	ListSnapshots(ctx context.Context) ([]SnapshotInfo, error)
	RestoreSnapshot(ctx context.Context, snapshotID string) (*RestoreSnapshotResponse, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) (*RawDaemonResponse, error)
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
	sessionPersistenceMu sync.Mutex
}

type SpawnVMInput struct {
	Profile     string `json:"profile,omitempty"`
	SessionName string `json:"session_name,omitempty"`
}

type SpawnVMOutput struct {
	VMID        string `json:"vm_id"`
	GuestIP     string `json:"guest_ip"`
	AgentURL    string `json:"agent_url"`
	Profile     string `json:"profile,omitempty"`
	SessionName string `json:"session_name,omitempty"`
}

type RunTaskInput struct {
	VMID           string `json:"vm_id,omitempty"`
	SessionName    string `json:"session_name,omitempty"`
	Prompt         string `json:"prompt"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type CopyInInput struct {
	VMID        string `json:"vm_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
	Path        string `json:"path"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding,omitempty"`
	Overwrite   bool   `json:"overwrite,omitempty"`
}

type CopyOutInput struct {
	VMID        string `json:"vm_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
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
}

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
	return newToolsWithSessionStorePath(daemon, sessions, defaultTimeout, sessionStorePath)
}

func newTools(daemon Daemon, sessions sessionStore, defaultTimeout time.Duration) *Tools {
	return newToolsWithSessionStorePath(daemon, sessions, defaultTimeout, "")
}

func newToolsWithSessionStorePath(daemon Daemon, sessions sessionStore, defaultTimeout time.Duration, sessionStorePath string) *Tools {
	if sessions == nil {
		sessions = NewSessionStore()
	}
	if defaultTimeout <= 0 {
		defaultTimeout = time.Duration(DefaultTimeoutSeconds) * time.Second
	}
	return &Tools{
		daemon:           daemon,
		sessions:         sessions,
		defaultTimeout:   defaultTimeout,
		sessionStorePath: strings.TrimSpace(sessionStorePath),
	}
}

func (t *Tools) SpawnVM(ctx context.Context, input SpawnVMInput) (*SpawnVMOutput, error) {
	profile := strings.TrimSpace(input.Profile)
	sessionName := strings.TrimSpace(input.SessionName)
	if sessionName != "" && t.sessionExists(sessionName) {
		return nil, fmt.Errorf("session %q already exists", sessionName)
	}

	res, err := t.daemon.SpawnVM(ctx, profile)
	if err != nil {
		return nil, err
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

	return &SpawnVMOutput{
		VMID:        res.VMID,
		GuestIP:     res.GuestIP,
		AgentURL:    res.AgentURL,
		Profile:     res.Profile,
		SessionName: sessionName,
	}, nil
}

func (t *Tools) MCPSpawnVM(ctx context.Context, req *mcp.CallToolRequest, input SpawnVMInput) (*mcp.CallToolResult, SpawnVMOutput, error) {
	out, err := t.SpawnVM(ctx, input)
	if err != nil || out == nil {
		return nil, SpawnVMOutput{}, err
	}
	return nil, *out, nil
}

func (t *Tools) RunTask(ctx context.Context, input RunTaskInput) (*RawDaemonResponse, error) {
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

	return t.daemon.RunTask(ctx, vmID, input.Prompt)
}

func (t *Tools) MCPRunTask(ctx context.Context, req *mcp.CallToolRequest, input RunTaskInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.RunTask(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) CopyIn(ctx context.Context, input CopyInInput) (*RawDaemonResponse, error) {
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
	return t.daemon.CopyIn(ctx, vmID, workspacePath, content, input.Overwrite)
}

func (t *Tools) MCPCopyIn(ctx context.Context, req *mcp.CallToolRequest, input CopyInInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.CopyIn(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) CopyOut(ctx context.Context, input CopyOutInput) (*CopyOutOutput, error) {
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
		return nil, err
	}
	if encoding == "base64" {
		content = base64.StdEncoding.EncodeToString([]byte(content))
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
	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}
	return t.daemon.Health(ctx, vmID)
}

func (t *Tools) MCPHealth(ctx context.Context, req *mcp.CallToolRequest, input VMIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.Health(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) StopVM(ctx context.Context, input VMIdentityInput) (*RawDaemonResponse, error) {
	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}
	return t.daemon.Stop(ctx, vmID)
}

func (t *Tools) MCPStopVM(ctx context.Context, req *mcp.CallToolRequest, input VMIdentityInput) (*mcp.CallToolResult, RawDaemonResponse, error) {
	out, err := t.StopVM(ctx, input)
	if err != nil || out == nil {
		return nil, RawDaemonResponse{}, err
	}
	return nil, *out, nil
}

func (t *Tools) DeleteVM(ctx context.Context, input VMIdentityInput) (*RawDaemonResponse, error) {
	vmID, err := t.resolveIdentity(input.VMID, input.SessionName)
	if err != nil {
		return nil, err
	}

	res, err := t.daemon.Delete(ctx, vmID)
	if err != nil {
		return nil, err
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
	})
	if err != nil {
		return nil, err
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

func (t *Tools) RestoreSnapshot(ctx context.Context, input RestoreSnapshotInput) (*RestoreSnapshotOutput, error) {
	snapshotID := strings.TrimSpace(input.SnapshotID)
	if snapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}

	sessionName := strings.TrimSpace(input.SessionName)
	if sessionName != "" && t.sessionExists(sessionName) {
		return nil, fmt.Errorf("session %q already exists", sessionName)
	}

	res, err := t.daemon.RestoreSnapshot(ctx, snapshotID)
	if err != nil {
		return nil, err
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
