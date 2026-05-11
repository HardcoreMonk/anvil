package anvilmcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxTimeoutSeconds = int((24 * time.Hour) / time.Second)

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

type sessionStore interface {
	Exists(sessionName string) bool
	Bind(sessionName, vmID string) error
	ResolveIdentity(vmID, sessionName string) (string, error)
	RemoveVM(vmID string)
}

type Tools struct {
	daemon         Daemon
	sessions       sessionStore
	defaultTimeout time.Duration
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

func (t *Tools) SpawnVM(ctx context.Context, input SpawnVMInput) (*SpawnVMOutput, error) {
	profile := strings.TrimSpace(input.Profile)
	sessionName := strings.TrimSpace(input.SessionName)
	if sessionName != "" && t.sessions.Exists(sessionName) {
		return nil, fmt.Errorf("session %q already exists", sessionName)
	}

	res, err := t.daemon.SpawnVM(ctx, profile)
	if err != nil {
		return nil, err
	}

	if sessionName != "" {
		if err := t.sessions.Bind(sessionName, res.VMID); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_, _ = t.daemon.Delete(cleanupCtx, res.VMID)
			return nil, err
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
	t.sessions.RemoveVM(vmID)
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
		t.sessions.RemoveVM(vmID)
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

func (t *Tools) resolveIdentity(vmID, sessionName string) (string, error) {
	return t.sessions.ResolveIdentity(vmID, sessionName)
}
