package anvilmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	copyInCalls     int
	copyInVMID      string
	copyInPath      string
	copyInContent   string
	copyInOverwrite bool
	copyInResp      *RawDaemonResponse
	copyInErr       error

	copyOutCalls   int
	copyOutVMID    string
	copyOutPath    string
	copyOutContent string
	copyOutErr     error

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

func (f *fakeDaemon) CopyIn(_ context.Context, vmID, workspacePath, content string, overwrite bool) (*RawDaemonResponse, error) {
	f.copyInCalls++
	f.copyInVMID = vmID
	f.copyInPath = workspacePath
	f.copyInContent = content
	f.copyInOverwrite = overwrite
	if f.copyInErr != nil {
		return nil, f.copyInErr
	}
	if f.copyInResp != nil {
		return f.copyInResp, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: `{"path":"notes/task.txt","bytes":15}`}, nil
}

func (f *fakeDaemon) CopyOut(_ context.Context, vmID, workspacePath string) (string, error) {
	f.copyOutCalls++
	f.copyOutVMID = vmID
	f.copyOutPath = workspacePath
	if f.copyOutErr != nil {
		return "", f.copyOutErr
	}
	if f.copyOutContent != "" {
		return f.copyOutContent, nil
	}
	return "hello workspace", nil
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

type concurrentSpawnDaemon struct {
	fakeDaemon
	mu   sync.Mutex
	next int
}

func (f *concurrentSpawnDaemon) SpawnVM(_ context.Context, profile string) (*SpawnVMResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.next++
	vmID := fmt.Sprintf("vm-%d", f.next)
	return &SpawnVMResponse{
		VMID:     vmID,
		GuestIP:  "10.0.0.2",
		AgentURL: "http://10.0.0.2:3000",
		Profile:  profile,
	}, nil
}

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

func (s *fakeSessionStore) RemoveVM(vmID string) bool {
	return len(s.removeVMAliases(vmID)) > 0
}

func (s *fakeSessionStore) removeVMAliases(vmID string) map[string]string {
	vmID = strings.TrimSpace(vmID)
	removed := make(map[string]string)
	for sessionName, mappedVMID := range s.sessions {
		if mappedVMID == vmID {
			delete(s.sessions, sessionName)
			removed[sessionName] = mappedVMID
		}
	}
	return removed
}

func (s *fakeSessionStore) restoreAliases(aliases map[string]string) {
	for sessionName, vmID := range aliases {
		s.sessions[sessionName] = vmID
	}
}

type blockingSaveSessionStore struct {
	mu              sync.Mutex
	sessions        map[string]string
	saveStarted     chan struct{}
	releaseSave     chan struct{}
	saveStartedOnce sync.Once
	saveErr         error
}

func newBlockingSaveSessionStore(saveErr error) *blockingSaveSessionStore {
	return &blockingSaveSessionStore{
		sessions:    make(map[string]string),
		saveStarted: make(chan struct{}),
		releaseSave: make(chan struct{}),
		saveErr:     saveErr,
	}
}

func (s *blockingSaveSessionStore) Bind(sessionName, vmID string) error {
	sessionName = strings.TrimSpace(sessionName)
	vmID = strings.TrimSpace(vmID)
	if sessionName == "" {
		return errors.New("session name must be non-empty")
	}
	if vmID == "" {
		return errors.New("vm ID must be non-empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[sessionName]; ok {
		return errors.New("session already exists")
	}
	s.sessions[sessionName] = vmID
	return nil
}

func (s *blockingSaveSessionStore) Exists(sessionName string) bool {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.sessions[sessionName]
	return ok
}

func (s *blockingSaveSessionStore) ResolveIdentity(vmID, sessionName string) (string, error) {
	vmID = strings.TrimSpace(vmID)
	sessionName = strings.TrimSpace(sessionName)
	if vmID != "" {
		return vmID, nil
	}
	if sessionName == "" {
		return "", errors.New("vm ID or session name is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	resolvedVMID, ok := s.sessions[sessionName]
	if !ok {
		return "", errors.New("unknown session")
	}
	return resolvedVMID, nil
}

func (s *blockingSaveSessionStore) RemoveVM(vmID string) bool {
	return len(s.removeVMAliases(vmID)) > 0
}

func (s *blockingSaveSessionStore) removeVMAliases(vmID string) map[string]string {
	vmID = strings.TrimSpace(vmID)
	if vmID == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := make(map[string]string)
	for sessionName, mappedVMID := range s.sessions {
		if mappedVMID == vmID {
			delete(s.sessions, sessionName)
			removed[sessionName] = mappedVMID
		}
	}
	return removed
}

func (s *blockingSaveSessionStore) restoreAliases(aliases map[string]string) {
	if len(aliases) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for sessionName, vmID := range aliases {
		s.sessions[sessionName] = vmID
	}
}

func (s *blockingSaveSessionStore) Save(path string) error {
	s.saveStartedOnce.Do(func() {
		close(s.saveStarted)
	})
	<-s.releaseSave
	return s.saveErr
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

func TestToolsSpawnSavesPersistentSession(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	tools := NewToolsWithSessionStorePath(daemon, store, time.Second, storePath)

	out, err := tools.SpawnVM(context.Background(), SpawnVMInput{
		Profile:     "dev",
		SessionName: "work",
	})
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}

	if out.SessionName != "work" {
		t.Fatalf("SessionName = %q, want work", out.SessionName)
	}
	sessions := readSavedSessions(t, storePath)
	if sessions["work"] != "vm-1" {
		t.Fatalf("saved session work = %q, want vm-1", sessions["work"])
	}
}

func TestToolsSpawnAuditsDefaultTenant(t *testing.T) {
	daemon := &fakeDaemon{}
	auditPath := filepath.Join(t.TempDir(), "runtime-audit.jsonl")
	tools := NewToolsWithOptions(daemon, NewSessionStore(), time.Second, ToolsOptions{
		DefaultTenantID: "tenant-1",
		AuditLogPath:    auditPath,
	})

	if _, err := tools.SpawnVM(context.Background(), SpawnVMInput{SessionName: "work"}); err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit path: %v", err)
	}
	var record RuntimeAuditRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &record); err != nil {
		t.Fatalf("parse audit record: %v", err)
	}
	if record.TenantID != "tenant-1" || record.VMID != "vm-1" || record.SessionAlias != "work" {
		t.Fatalf("audit record = %+v, want tenant-1 vm-1 work", record)
	}
	if record.ToolName != "anvil_spawn_vm" || record.DaemonOperation != "POST /vms" || record.ResultCode != "success" {
		t.Fatalf("audit operation = %+v, want spawn success", record)
	}
}

func TestToolsRejectsInvalidTenantInputBeforeDaemonCall(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewToolsWithOptions(daemon, NewSessionStore(), time.Second, ToolsOptions{})

	_, err := tools.RunTask(context.Background(), RunTaskInput{
		VMID:     "vm-1",
		TenantID: "../tenant",
		Prompt:   "hello",
	})
	if err == nil {
		t.Fatal("RunTask error = nil, want invalid tenant error")
	}
	if daemon.runCalls != 0 {
		t.Fatalf("RunTask daemon calls = %d, want 0", daemon.runCalls)
	}
}

func TestToolsAuditLogRequiresTenantID(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewToolsWithOptions(daemon, NewSessionStore(), time.Second, ToolsOptions{
		AuditLogPath: filepath.Join(t.TempDir(), "runtime-audit.jsonl"),
	})

	_, err := tools.Health(context.Background(), VMIdentityInput{VMID: "vm-1"})
	if err == nil {
		t.Fatal("Health error = nil, want missing tenant error")
	}
	if daemon.healthCalls != 0 {
		t.Fatalf("Health daemon calls = %d, want 0", daemon.healthCalls)
	}
}

func TestToolsConcurrentPersistentSpawnKeepsAllAliasesOnDisk(t *testing.T) {
	daemon := &concurrentSpawnDaemon{}
	store := NewSessionStore()
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	tools := NewToolsWithSessionStorePath(daemon, store, time.Second, storePath)
	start := make(chan struct{})
	errs := make(chan error, 2)

	var wg sync.WaitGroup
	for _, sessionName := range []string{"work", "review"} {
		wg.Add(1)
		go func(sessionName string) {
			defer wg.Done()
			<-start
			_, err := tools.SpawnVM(context.Background(), SpawnVMInput{SessionName: sessionName})
			errs <- err
		}(sessionName)
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("SpawnVM returned error: %v", err)
		}
	}

	loaded, err := LoadSessionStore(storePath)
	if err != nil {
		t.Fatalf("LoadSessionStore() error = %v", err)
	}
	for _, sessionName := range []string{"work", "review"} {
		if vmID, ok := loaded.Resolve(sessionName); !ok || vmID == "" {
			t.Fatalf("Resolve(%q) = %q, %v; want non-empty vm ID, true", sessionName, vmID, ok)
		}
	}
}

func TestToolsSpawnSaveFailureDeletesSpawnedVM(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	storePath := t.TempDir()
	tools := NewToolsWithSessionStorePath(daemon, store, time.Second, storePath)

	_, err := tools.SpawnVM(context.Background(), SpawnVMInput{SessionName: "work"})
	if err == nil {
		t.Fatal("SpawnVM returned nil error for session store save failure")
	}
	if !strings.Contains(err.Error(), "save session store") {
		t.Fatalf("SpawnVM error = %q, want save session store context", err.Error())
	}
	if daemon.deleteCalls != 1 {
		t.Fatalf("Delete calls = %d, want 1", daemon.deleteCalls)
	}
	if daemon.deleteVMID != "vm-1" {
		t.Fatalf("Delete vmID = %q, want vm-1", daemon.deleteVMID)
	}
	if store.Exists("work") {
		t.Fatal("session work still exists after failed persistent spawn bind")
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

func TestToolsCopyInUsesSession(t *testing.T) {
	daemon := &fakeDaemon{copyInResp: &RawDaemonResponse{StatusCode: 200, Body: `{"path":"notes/task.txt","bytes":15}`}}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	tools := NewTools(daemon, store, time.Second)

	out, err := tools.CopyIn(context.Background(), CopyInInput{
		SessionName: "work",
		Path:        "notes/task.txt",
		Content:     "hello workspace",
		Overwrite:   true,
	})
	if err != nil {
		t.Fatalf("CopyIn returned error: %v", err)
	}

	if daemon.copyInCalls != 1 {
		t.Fatalf("CopyIn calls = %d, want 1", daemon.copyInCalls)
	}
	if daemon.copyInVMID != "vm-1" || daemon.copyInPath != "notes/task.txt" || daemon.copyInContent != "hello workspace" {
		t.Fatalf("CopyIn args = %q, %q, %q; want vm-1 notes/task.txt hello workspace", daemon.copyInVMID, daemon.copyInPath, daemon.copyInContent)
	}
	if !daemon.copyInOverwrite {
		t.Fatal("CopyIn overwrite = false, want true")
	}
	if out.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", out.StatusCode)
	}
}

func TestToolsCopyInDefaultsEncodingToTextAndOverwriteFalse(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	if _, err := tools.CopyIn(context.Background(), CopyInInput{
		VMID:    "vm-1",
		Path:    "notes/task.txt",
		Content: "hello workspace",
	}); err != nil {
		t.Fatalf("CopyIn returned error: %v", err)
	}

	if daemon.copyInContent != "hello workspace" {
		t.Fatalf("CopyIn content = %q, want hello workspace", daemon.copyInContent)
	}
	if daemon.copyInOverwrite {
		t.Fatal("CopyIn overwrite = true, want false by default")
	}
}

func TestToolsCopyInDecodesBase64(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	if _, err := tools.CopyIn(context.Background(), CopyInInput{
		VMID:     "vm-1",
		Path:     "bin/payload",
		Content:  "AAEC",
		Encoding: "base64",
	}); err != nil {
		t.Fatalf("CopyIn returned error: %v", err)
	}

	if got := []byte(daemon.copyInContent); string(got) != string([]byte{0, 1, 2}) {
		t.Fatalf("decoded content bytes = %v, want [0 1 2]", got)
	}
}

func TestToolsCopyOutUsesSession(t *testing.T) {
	daemon := &fakeDaemon{copyOutContent: "hello workspace"}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	tools := NewTools(daemon, store, time.Second)

	out, err := tools.CopyOut(context.Background(), CopyOutInput{
		SessionName: "work",
		Path:        "notes/task.txt",
	})
	if err != nil {
		t.Fatalf("CopyOut returned error: %v", err)
	}

	if daemon.copyOutCalls != 1 {
		t.Fatalf("CopyOut calls = %d, want 1", daemon.copyOutCalls)
	}
	if daemon.copyOutVMID != "vm-1" || daemon.copyOutPath != "notes/task.txt" {
		t.Fatalf("CopyOut args = %q, %q; want vm-1 notes/task.txt", daemon.copyOutVMID, daemon.copyOutPath)
	}
	if out.Path != "notes/task.txt" || out.Content != "hello workspace" {
		t.Fatalf("CopyOut output = %+v, want notes/task.txt hello workspace", out)
	}
}

func TestToolsCopyOutEncodesBase64(t *testing.T) {
	daemon := &fakeDaemon{copyOutContent: string([]byte{0, 1, 2})}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	out, err := tools.CopyOut(context.Background(), CopyOutInput{
		VMID:     "vm-1",
		Path:     "bin/payload",
		Encoding: "base64",
	})
	if err != nil {
		t.Fatalf("CopyOut returned error: %v", err)
	}

	if out.Encoding != "base64" {
		t.Fatalf("Encoding = %q, want base64", out.Encoding)
	}
	if out.Content != "AAEC" {
		t.Fatalf("Content = %q, want AAEC", out.Content)
	}
}

func TestToolsCopyRejectsInvalidEncodingBeforeDaemonCall(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	if _, err := tools.CopyIn(context.Background(), CopyInInput{VMID: "vm-1", Path: "file.txt", Content: "x", Encoding: "hex"}); err == nil {
		t.Fatal("CopyIn returned nil error for invalid encoding")
	}
	if _, err := tools.CopyOut(context.Background(), CopyOutInput{VMID: "vm-1", Path: "file.txt", Encoding: "hex"}); err == nil {
		t.Fatal("CopyOut returned nil error for invalid encoding")
	}
	if daemon.copyInCalls != 0 || daemon.copyOutCalls != 0 {
		t.Fatalf("daemon calls = copyIn %d copyOut %d, want 0", daemon.copyInCalls, daemon.copyOutCalls)
	}
}

func TestToolsCopyInRejectsOversizedContentBeforeDaemonCall(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	if _, err := tools.CopyIn(context.Background(), CopyInInput{
		VMID:    "vm-1",
		Path:    "large.txt",
		Content: strings.Repeat("x", MaxWorkspaceFileBytes+1),
	}); err == nil {
		t.Fatal("CopyIn returned nil error for oversized content")
	}
	if daemon.copyInCalls != 0 {
		t.Fatalf("CopyIn calls = %d, want 0", daemon.copyInCalls)
	}
}

func TestToolsCopyRejectsUnsafePathBeforeDaemonCall(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	if _, err := tools.CopyIn(context.Background(), CopyInInput{VMID: "vm-1", Path: "../secret", Content: "x"}); err == nil {
		t.Fatal("CopyIn returned nil error for unsafe path")
	}
	if _, err := tools.CopyOut(context.Background(), CopyOutInput{VMID: "vm-1", Path: "/absolute"}); err == nil {
		t.Fatal("CopyOut returned nil error for unsafe path")
	}
	if daemon.copyInCalls != 0 {
		t.Fatalf("CopyIn calls = %d, want 0", daemon.copyInCalls)
	}
	if daemon.copyOutCalls != 0 {
		t.Fatalf("CopyOut calls = %d, want 0", daemon.copyOutCalls)
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

func TestToolsDeleteSavesPersistentSessionRemoval(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	tools := NewToolsWithSessionStorePath(daemon, store, time.Second, storePath)

	if _, err := tools.DeleteVM(context.Background(), VMIdentityInput{SessionName: "work"}); err != nil {
		t.Fatalf("DeleteVM returned error: %v", err)
	}

	sessions := readSavedSessions(t, storePath)
	if len(sessions) != 0 {
		t.Fatalf("saved sessions = %#v, want empty after delete", sessions)
	}
}

func TestToolsDeleteSaveFailureIncludesDeleteResponseContext(t *testing.T) {
	daemon := &fakeDaemon{deleteResp: &RawDaemonResponse{
		StatusCode: 202,
		Body:       `{"deleted":true,"vm_id":"vm-1"}`,
	}}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	storePath := t.TempDir()
	tools := NewToolsWithSessionStorePath(daemon, store, time.Second, storePath)

	_, err := tools.DeleteVM(context.Background(), VMIdentityInput{SessionName: "work"})
	if err == nil {
		t.Fatal("DeleteVM returned nil error for session store save failure")
	}
	for _, want := range []string{"status 202", `{"deleted":true,"vm_id":"vm-1"}`, "save session store"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("DeleteVM error = %q, want substring %q", err.Error(), want)
		}
	}
	if daemon.deleteCalls != 1 {
		t.Fatalf("Delete calls = %d, want 1", daemon.deleteCalls)
	}
	if vmID, ok := store.Resolve("work"); !ok || vmID != "vm-1" {
		t.Fatalf("session work resolved to %q, %v after failed save; want vm-1, true", vmID, ok)
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
	if _, ok := store.Resolve("work"); ok {
		t.Fatal("session work still exists after successful stop_after snapshot")
	}
}

func TestToolsCreateSnapshotKeepsSessionWithoutStopAfter(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	if err := store.Bind("work", "vm-1"); err != nil {
		t.Fatalf("Bind returned error: %v", err)
	}
	tools := NewTools(daemon, store, time.Second)

	_, err := tools.CreateSnapshot(context.Background(), CreateSnapshotInput{
		SessionName: "work",
		StopAfter:   false,
		Type:        "full",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}

	if _, ok := store.Resolve("work"); !ok {
		t.Fatal("session work was removed after snapshot without stop_after")
	}
}

func TestToolsCreateSnapshotStopAfterSaveFailureRestoresRemovedSessions(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	for sessionName, vmID := range map[string]string{
		"work":   "vm-1",
		"review": "vm-1",
	} {
		if err := store.Bind(sessionName, vmID); err != nil {
			t.Fatalf("Bind(%q, %q) returned error: %v", sessionName, vmID, err)
		}
	}
	storePath := t.TempDir()
	tools := NewToolsWithSessionStorePath(daemon, store, time.Second, storePath)

	_, err := tools.CreateSnapshot(context.Background(), CreateSnapshotInput{
		SessionName: "work",
		StopAfter:   true,
		Type:        "full",
	})
	if err == nil {
		t.Fatal("CreateSnapshot returned nil error for session store save failure")
	}
	for _, want := range []string{"snapshot", "succeeded", "save session store"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("CreateSnapshot error = %q, want substring %q", err.Error(), want)
		}
	}
	for sessionName, wantVMID := range map[string]string{"work": "vm-1", "review": "vm-1"} {
		gotVMID, ok := store.Resolve(sessionName)
		if !ok || gotVMID != wantVMID {
			t.Fatalf("Resolve(%q) = %q, %v; want %q, true", sessionName, gotVMID, ok, wantVMID)
		}
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

func TestToolsRestoreSnapshotSaveFailureReturnsBindErrorWithRestoredVMID(t *testing.T) {
	daemon := &fakeDaemon{}
	store := NewSessionStore()
	storePath := t.TempDir()
	tools := NewToolsWithSessionStorePath(daemon, store, time.Second, storePath)

	_, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{
		SnapshotID:  "snap-1",
		SessionName: "restored",
	})
	if err == nil {
		t.Fatal("RestoreSnapshot returned nil error for session store save failure")
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
	if !strings.Contains(err.Error(), "save session store") {
		t.Fatalf("error = %q, want save session store context", err.Error())
	}
	if daemon.deleteCalls != 0 {
		t.Fatalf("Delete calls = %d, want 0", daemon.deleteCalls)
	}
}

func TestToolsSessionReadWaitsForPendingRestoreSaveFailureRollback(t *testing.T) {
	daemon := &fakeDaemon{}
	store := newBlockingSaveSessionStore(errors.New("disk full"))
	tools := newToolsWithSessionStorePath(daemon, store, time.Second, "/tmp/anvil-mcp-sessions.json")

	restoreErrCh := make(chan error, 1)
	go func() {
		_, err := tools.RestoreSnapshot(context.Background(), RestoreSnapshotInput{
			SnapshotID:  "snap-1",
			SessionName: "restored",
		})
		restoreErrCh <- err
	}()

	<-store.saveStarted

	readErrCh := make(chan error, 1)
	go func() {
		_, err := tools.Health(context.Background(), VMIdentityInput{SessionName: "restored"})
		readErrCh <- err
	}()

	select {
	case err := <-readErrCh:
		t.Fatalf("Health returned during pending session save with error %v; want blocked read", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(store.releaseSave)

	restoreErr := <-restoreErrCh
	if restoreErr == nil {
		t.Fatal("RestoreSnapshot error = nil, want save failure")
	}
	var restoreBindErr *RestoreSessionBindError
	if !errors.As(restoreErr, &restoreBindErr) {
		t.Fatalf("RestoreSnapshot error type = %T, want *RestoreSessionBindError", restoreErr)
	}

	readErr := <-readErrCh
	if readErr == nil {
		t.Fatal("Health error = nil, want unknown session after rollback")
	}
	if !strings.Contains(readErr.Error(), "unknown session") {
		t.Fatalf("Health error = %q, want unknown session", readErr.Error())
	}
	if daemon.healthCalls != 0 {
		t.Fatalf("Health daemon calls = %d, want 0", daemon.healthCalls)
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

func TestToolsMCPRestoreSnapshotOutputOmitsAgentToken(t *testing.T) {
	daemon := &fakeDaemon{}
	tools := NewTools(daemon, NewSessionStore(), time.Second)

	_, out, err := tools.MCPRestoreSnapshot(context.Background(), nil, RestoreSnapshotInput{SnapshotID: "snap-1"})
	if err != nil {
		t.Fatalf("MCPRestoreSnapshot returned error: %v", err)
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	if strings.Contains(string(data), "agent_token") {
		t.Fatalf("MCP restore output JSON exposes agent_token: %s", string(data))
	}
	if strings.Contains(string(data), "secret-token") {
		t.Fatalf("MCP restore output JSON exposes token value: %s", string(data))
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

func readSavedSessions(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	var raw struct {
		Sessions map[string]string `json:"sessions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal(%q) error = %v", path, err)
	}
	if raw.Sessions == nil {
		t.Fatalf("sessions field is nil in %q", path)
	}
	return raw.Sessions
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
