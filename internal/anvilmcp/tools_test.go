package anvilmcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeDaemon struct {
	spawnCalls   int
	spawnProfile string
	spawnResp    *SpawnVMResponse
	spawnErr     error
	onSpawn      func()

	runCalls       int
	runVMID        string
	runPrompt      string
	runDeadline    time.Time
	runHasDeadline bool
	runResp        *RawDaemonResponse
	runErr         error

	healthCalls int
	healthVMID  string
	healthResp  *RawDaemonResponse
	healthErr   error

	stopCalls int
	stopVMID  string
	stopResp  *RawDaemonResponse
	stopErr   error

	deleteCalls       int
	deleteVMID        string
	deleteContextErr  error
	deleteDeadline    time.Time
	deleteHasDeadline bool
	deleteResp        *RawDaemonResponse
	deleteErr         error

	createSnapshotCalls int
	createSnapshotVMID  string
	createSnapshotReq   CreateSnapshotRequest
	createSnapshotResp  *SnapshotInfo
	createSnapshotErr   error

	listSnapshotCalls     int
	listSnapshotResp      []SnapshotInfo
	listSnapshotReturnNil bool
	listSnapshotErr       error

	restoreSnapshotCalls int
	restoreSnapshotID    string
	restoreSnapshotResp  *RestoreSnapshotResponse
	restoreSnapshotErr   error

	deleteSnapshotCalls int
	deleteSnapshotID    string
	deleteSnapshotResp  *RawDaemonResponse
	deleteSnapshotErr   error
}

func (f *fakeDaemon) SpawnVM(_ context.Context, profile string) (*SpawnVMResponse, error) {
	f.spawnCalls++
	f.spawnProfile = profile
	if f.onSpawn != nil {
		f.onSpawn()
	}
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	if f.spawnResp != nil {
		return f.spawnResp, nil
	}
	return &SpawnVMResponse{VMID: "vm-1", GuestIP: "10.0.0.2", AgentURL: "http://10.0.0.2:3000", Profile: profile}, nil
}

func (f *fakeDaemon) RunTask(ctx context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	f.runCalls++
	f.runVMID = vmID
	f.runPrompt = prompt
	f.runDeadline, f.runHasDeadline = ctx.Deadline()
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.runResp != nil {
		return f.runResp, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: `{"ok":true}`}, nil
}

func (f *fakeDaemon) Health(_ context.Context, vmID string) (*RawDaemonResponse, error) {
	f.healthCalls++
	f.healthVMID = vmID
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	if f.healthResp != nil {
		return f.healthResp, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: `{"healthy":true}`}, nil
}

func (f *fakeDaemon) Stop(_ context.Context, vmID string) (*RawDaemonResponse, error) {
	f.stopCalls++
	f.stopVMID = vmID
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	if f.stopResp != nil {
		return f.stopResp, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: `{"stopped":true}`}, nil
}

func (f *fakeDaemon) Delete(ctx context.Context, vmID string) (*RawDaemonResponse, error) {
	f.deleteCalls++
	f.deleteVMID = vmID
	f.deleteContextErr = ctx.Err()
	f.deleteDeadline, f.deleteHasDeadline = ctx.Deadline()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if f.deleteResp != nil {
		return f.deleteResp, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: `{"deleted":true}`}, nil
}

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
	if f.listSnapshotReturnNil {
		return nil, nil
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

func TestToolsSpawnBindsSession(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	tools := NewTools(daemon, store, time.Second)

	out, err := tools.SpawnVM(context.Background(), SpawnVMInput{
		Profile:     "dev",
		SessionName: "work",
	})
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}

	if daemon.spawnCalls != 1 {
		t.Fatalf("SpawnVM calls = %d, want 1", daemon.spawnCalls)
	}
	if daemon.spawnProfile != "dev" {
		t.Errorf("SpawnVM profile = %q, want dev", daemon.spawnProfile)
	}
	if out.VMID != "vm-1" || out.SessionName != "work" {
		t.Errorf("SpawnVM output = %+v, want vm-1 session work", out)
	}
	if vmID, ok := store.Resolve("work"); !ok || vmID != "vm-1" {
		t.Fatalf("session work resolved to %q, %v; want vm-1, true", vmID, ok)
	}
}

func TestToolsSpawnRejectsDuplicateSessionBeforeDaemonCall(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-existing"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	tools := NewTools(daemon, store, time.Second)

	if _, err := tools.SpawnVM(context.Background(), SpawnVMInput{SessionName: "work"}); err == nil {
		t.Fatal("SpawnVM returned nil error for duplicate session")
	}
	if daemon.spawnCalls != 0 {
		t.Fatalf("SpawnVM calls = %d, want 0", daemon.spawnCalls)
	}
}

func TestToolsSpawnCleanupIgnoresCanceledCallerContext(t *testing.T) {
	store := NewSessionStore()
	daemon := &fakeDaemon{onSpawn: func() {
		if err := store.Bind("work", "vm-race"); err != nil {
			t.Fatalf("Bind returned error: %v", err)
		}
	}}
	tools := NewTools(daemon, store, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := time.Now()

	if _, err := tools.SpawnVM(ctx, SpawnVMInput{SessionName: "work"}); err == nil {
		t.Fatal("SpawnVM returned nil error for failed session bind")
	}

	if daemon.deleteCalls != 1 {
		t.Fatalf("Delete calls = %d, want 1", daemon.deleteCalls)
	}
	if daemon.deleteVMID != "vm-1" {
		t.Errorf("Delete vmID = %q, want vm-1", daemon.deleteVMID)
	}
	if daemon.deleteContextErr != nil {
		t.Fatalf("Delete context err = %v, want nil", daemon.deleteContextErr)
	}
	assertDeadlineWithin(t, daemon.deleteDeadline, daemon.deleteHasDeadline, before.Add(10*time.Second), time.Second)
}

func TestToolsRunTaskUsesSession(t *testing.T) {
	daemon := &fakeDaemon{runResp: &RawDaemonResponse{StatusCode: 202, Body: "task started"}}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	tools := NewTools(daemon, store, time.Second)

	out, err := tools.RunTask(context.Background(), RunTaskInput{SessionName: "work", Prompt: "hello"})
	if err != nil {
		t.Fatalf("RunTask returned error: %v", err)
	}

	if daemon.runCalls != 1 {
		t.Fatalf("RunTask calls = %d, want 1", daemon.runCalls)
	}
	if daemon.runVMID != "vm-1" || daemon.runPrompt != "hello" {
		t.Errorf("RunTask args = %q, %q; want vm-1, hello", daemon.runVMID, daemon.runPrompt)
	}
	if out.StatusCode != 202 || out.Body != "task started" {
		t.Errorf("RunTask output = %+v, want status 202 body task started", out)
	}
}

func TestToolsRunTaskPreservesPromptWhitespace(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)
	prompt := "  hello\n"

	if _, err := tools.RunTask(context.Background(), RunTaskInput{VMID: "vm-1", Prompt: prompt}); err != nil {
		t.Fatalf("RunTask returned error: %v", err)
	}

	if daemon.runPrompt != prompt {
		t.Fatalf("RunTask prompt = %q, want %q", daemon.runPrompt, prompt)
	}
}

func TestToolsRunTaskRequiresPrompt(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	if _, err := tools.RunTask(context.Background(), RunTaskInput{VMID: "vm-1"}); err == nil {
		t.Fatal("RunTask returned nil error for empty prompt")
	}
	if daemon.runCalls != 0 {
		t.Fatalf("RunTask calls = %d, want 0", daemon.runCalls)
	}
}

func TestToolsRunTaskUsesDefaultTimeout(t *testing.T) {
	daemon := &fakeDaemon{}
	defaultTimeout := 2 * time.Second
	tools := NewTools(daemon, NewSessionStore(), defaultTimeout)
	before := time.Now()

	if _, err := tools.RunTask(context.Background(), RunTaskInput{VMID: "vm-1", Prompt: "hello"}); err != nil {
		t.Fatalf("RunTask returned error: %v", err)
	}

	assertDeadlineWithin(t, daemon.runDeadline, daemon.runHasDeadline, before.Add(defaultTimeout), time.Second)
}

func TestToolsRunTaskUsesTimeoutSecondsOverride(t *testing.T) {
	daemon := &fakeDaemon{}
	defaultTimeout := 10 * time.Second
	tools := NewTools(daemon, NewSessionStore(), defaultTimeout)
	before := time.Now()

	if _, err := tools.RunTask(context.Background(), RunTaskInput{VMID: "vm-1", Prompt: "hello", TimeoutSeconds: 3}); err != nil {
		t.Fatalf("RunTask returned error: %v", err)
	}

	assertDeadlineWithin(t, daemon.runDeadline, daemon.runHasDeadline, before.Add(3*time.Second), time.Second)
}

func TestToolsRunTaskRejectsNegativeTimeout(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	if _, err := tools.RunTask(context.Background(), RunTaskInput{VMID: "vm-1", Prompt: "hello", TimeoutSeconds: -1}); err == nil {
		t.Fatal("RunTask returned nil error for negative timeout")
	}
	if daemon.runCalls != 0 {
		t.Fatalf("RunTask calls = %d, want 0", daemon.runCalls)
	}
}

func TestToolsRunTaskRejectsExcessiveTimeout(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	tooLargeTimeout := int((24*time.Hour)/time.Second) + 1
	if _, err := tools.RunTask(context.Background(), RunTaskInput{VMID: "vm-1", Prompt: "hello", TimeoutSeconds: tooLargeTimeout}); err == nil {
		t.Fatal("RunTask returned nil error for excessive timeout")
	}
	if daemon.runCalls != 0 {
		t.Fatalf("RunTask calls = %d, want 0", daemon.runCalls)
	}
}

func TestToolsDeleteRemovesSession(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	tools := NewTools(daemon, store, time.Second)

	if _, err := tools.DeleteVM(context.Background(), VMIdentityInput{SessionName: "work"}); err != nil {
		t.Fatalf("DeleteVM returned error: %v", err)
	}

	if daemon.deleteCalls != 1 {
		t.Fatalf("Delete calls = %d, want 1", daemon.deleteCalls)
	}
	if daemon.deleteVMID != "vm-1" {
		t.Errorf("Delete vmID = %q, want vm-1", daemon.deleteVMID)
	}
	if _, ok := store.Resolve("work"); ok {
		t.Fatal("session work still exists after DeleteVM")
	}
}

func TestToolsReturnsDaemonError(t *testing.T) {
	daemonErr := errors.New("daemon down")
	daemon := &fakeDaemon{healthErr: daemonErr}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	_, err := tools.Health(context.Background(), VMIdentityInput{VMID: "vm-1"})
	if !errors.Is(err, daemonErr) {
		t.Fatalf("Health error = %v, want %v", err, daemonErr)
	}
}

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

func TestToolsListSnapshotsConvertsNilDaemonSliceToEmptyJSONList(t *testing.T) {
	daemon := &fakeDaemon{listSnapshotReturnNil: true}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	out, err := tools.ListSnapshots(context.Background())
	if err != nil {
		t.Fatalf("ListSnapshots returned error: %v", err)
	}

	if out.Snapshots == nil {
		t.Fatal("Snapshots = nil, want empty non-nil slice")
	}
	if len(out.Snapshots) != 0 {
		t.Fatalf("snapshot count = %d, want 0", len(out.Snapshots))
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if string(data) != `{"snapshots":[]}` {
		t.Fatalf("json = %s, want %s", data, `{"snapshots":[]}`)
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

func assertDeadlineWithin(t *testing.T, got time.Time, ok bool, want time.Time, tolerance time.Duration) {
	t.Helper()
	if !ok {
		t.Fatal("context had no deadline")
	}
	delta := got.Sub(want)
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		t.Fatalf("deadline = %v, want within %v of %v", got, tolerance, want)
	}
}
