package anvilmcp

import (
	"context"
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
