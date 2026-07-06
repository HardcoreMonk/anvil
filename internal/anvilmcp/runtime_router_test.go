package anvilmcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type routerFakeDaemon struct {
	spawnCalls             int
	spawnReq               SpawnVMRequest
	spawnResp              *SpawnVMResponse
	spawnResponses         []*SpawnVMResponse
	spawnReqs              []SpawnVMRequest
	spawnErr               error
	runTaskCalls           int
	runTaskVMID            string
	healthCalls            int
	healthVMID             string
	createSnapshotCalls    int
	createSnapshotVMID     string
	restoreSnapshotCalls   int
	restoreSnapshotID      string
	restoreResp            *RestoreSnapshotResponse
	restoreErr             error
	deleteCalls            int
	deleteVMID             string
	deleteVMIDs            []string
	deleteErr              error
	deleteErrForVMID       map[string]error
	deleteNilResp          bool
	afterDelete            func()
	listVMResp             []VMInfo
	snapshotList           []SnapshotInfo
	listSnapshotErr        error
	exportCalls            []string
	exportBodies           map[string]string
	exportStreams          map[string]io.ReadCloser
	exportErr              error
	importCalls            []string
	importErrForBody       map[string]error
	importStatusForBody    map[string]string
	distributedCalls       int
	distributedReq         DistributedFlockRequest
	registerDistributedErr error
	relayCalls             int
	relayReq               RelayFlockRequest
	registerRelayErr       error
	postWallCalls          int
	postWallFlockID        string
	postWallReq            TownWallPostRequest
	historyCalls           int
	historyFlockID         string
	deregisterCalls        int
	deregisterFlockIDs     []string
	deregisterErr          error
}

func (f *routerFakeDaemon) RegisterDistributedFlock(_ context.Context, _ string, req DistributedFlockRequest) error {
	f.distributedCalls++
	f.distributedReq = req
	return f.registerDistributedErr
}

func (f *routerFakeDaemon) RegisterRelayFlock(_ context.Context, _ string, req RelayFlockRequest) error {
	f.relayCalls++
	f.relayReq = req
	return f.registerRelayErr
}

func (f *routerFakeDaemon) SpawnVM(_ context.Context, req SpawnVMRequest) (*SpawnVMResponse, error) {
	f.spawnCalls++
	f.spawnReq = req
	f.spawnReqs = append(f.spawnReqs, req)
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	if len(f.spawnResponses) > 0 {
		resp := f.spawnResponses[0]
		f.spawnResponses = f.spawnResponses[1:]
		return resp, nil
	}
	if f.spawnResp != nil {
		return f.spawnResp, nil
	}
	return &SpawnVMResponse{VMID: "vm-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: req.TenantID, EgressPolicy: req.EgressPolicy}, nil
}

func (f *routerFakeDaemon) RunTask(_ context.Context, vmID, prompt string) (*RawDaemonResponse, error) {
	f.runTaskCalls++
	f.runTaskVMID = vmID
	return &RawDaemonResponse{StatusCode: 200, Body: `{"output":"ok"}`}, nil
}

func (f *routerFakeDaemon) CopyIn(context.Context, string, string, string, bool) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *routerFakeDaemon) CopyOut(context.Context, string, string) (string, error) {
	return "content", nil
}

func (f *routerFakeDaemon) Health(_ context.Context, vmID string) (*RawDaemonResponse, error) {
	f.healthCalls++
	f.healthVMID = vmID
	return &RawDaemonResponse{StatusCode: 200, Body: `{"status":"ok"}`}, nil
}

func (f *routerFakeDaemon) Stop(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *routerFakeDaemon) Delete(_ context.Context, vmID string) (*RawDaemonResponse, error) {
	f.deleteCalls++
	f.deleteVMID = vmID
	f.deleteVMIDs = append(f.deleteVMIDs, vmID)
	if f.deleteErrForVMID != nil {
		if err, ok := f.deleteErrForVMID[vmID]; ok {
			return nil, err
		}
	}
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if f.afterDelete != nil {
		f.afterDelete()
	}
	if f.deleteNilResp {
		return nil, nil
	}
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *routerFakeDaemon) CreateSnapshot(_ context.Context, vmID string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	f.createSnapshotCalls++
	f.createSnapshotVMID = vmID
	return &SnapshotInfo{SnapshotID: "snap-1", SourceVMID: vmID, TenantID: req.TenantID}, nil
}

func (f *routerFakeDaemon) ListSnapshots(context.Context) ([]SnapshotInfo, error) {
	if f.listSnapshotErr != nil {
		return nil, f.listSnapshotErr
	}
	return append([]SnapshotInfo(nil), f.snapshotList...), nil
}

func (f *routerFakeDaemon) ExportSnapshot(_ context.Context, snapshotID string) (*SnapshotExportStream, error) {
	f.exportCalls = append(f.exportCalls, snapshotID)
	if f.exportErr != nil {
		return nil, f.exportErr
	}
	body := "bundle:" + snapshotID
	if f.exportBodies != nil {
		if configured, ok := f.exportBodies[snapshotID]; ok {
			body = configured
		}
	}
	if f.exportStreams != nil {
		if configured, ok := f.exportStreams[snapshotID]; ok {
			return &SnapshotExportStream{Body: configured, ContentType: "application/vnd.anvil.snapshot-bundle"}, nil
		}
	}
	return &SnapshotExportStream{Body: io.NopCloser(strings.NewReader(body)), ContentType: "application/vnd.anvil.snapshot-bundle"}, nil
}

func (f *routerFakeDaemon) ImportSnapshot(_ context.Context, body io.Reader) (*SnapshotImportResponse, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	text := string(data)
	f.importCalls = append(f.importCalls, text)
	if f.importErrForBody != nil {
		if err, ok := f.importErrForBody[text]; ok {
			return nil, err
		}
	}
	status := "imported"
	if f.importStatusForBody != nil {
		if configured, ok := f.importStatusForBody[text]; ok {
			status = configured
		}
	}
	snapshotID := strings.TrimPrefix(text, "bundle:")
	return &SnapshotImportResponse{SnapshotID: snapshotID, SnapshotType: "full", Status: status, Skipped: status == "already_present"}, nil
}

func (f *routerFakeDaemon) RestoreSnapshot(_ context.Context, snapshotID string, req RestoreSnapshotRequest) (*RestoreSnapshotResponse, error) {
	f.restoreSnapshotCalls++
	f.restoreSnapshotID = snapshotID
	if f.restoreErr != nil {
		return nil, f.restoreErr
	}
	if f.restoreResp != nil {
		return f.restoreResp, nil
	}
	return &RestoreSnapshotResponse{VMID: "vm-restored", GuestIP: "10.0.1.20", AgentURL: "http://10.0.1.20:8080", TenantID: req.TenantID, EgressPolicy: req.EgressPolicy, SourceSnapshotID: snapshotID}, nil
}

func (f *routerFakeDaemon) DeleteSnapshot(context.Context, string) (*RawDaemonResponse, error) {
	return &RawDaemonResponse{StatusCode: 200, Body: "{}"}, nil
}

func (f *routerFakeDaemon) ListVMs(context.Context) ([]VMInfo, error) {
	return f.listVMResp, nil
}

type routerFlockFakeDaemon struct {
	routerFakeDaemon
	createFlockCalls int
	createFlockReq   FlockCreateRequest
	createFlockResp  *FlockCreateResponse
	createFlockErr   error
	createFlockNil   bool
}

func (f *routerFlockFakeDaemon) CreateFlock(_ context.Context, req FlockCreateRequest) (*FlockCreateResponse, error) {
	f.createFlockCalls++
	f.createFlockReq = req
	if f.createFlockErr != nil {
		return nil, f.createFlockErr
	}
	if f.createFlockNil {
		return nil, nil
	}
	if f.createFlockResp != nil {
		return f.createFlockResp, nil
	}
	return &FlockCreateResponse{FlockID: "flock-1", Agents: []FlockAgentInfo{}}, nil
}

func requireFlockPlacementMetricAttempt(t *testing.T, state FlockPlacementMetricsState, outcome, reason string, want int64) {
	t.Helper()
	key := flockPlacementAttemptKey(outcome, reason)
	if got := state.AttemptsByOutcomeReason[key]; got != want {
		t.Fatalf("flock placement attempts[%q] = %d, want %d", key, got, want)
	}
}

func requireOnlyFlockPlacementMetricAttempt(t *testing.T, state FlockPlacementMetricsState, outcome, reason string) {
	t.Helper()
	requireFlockPlacementMetricAttempt(t, state, outcome, reason, 1)
	var total int64
	for _, count := range state.AttemptsByOutcomeReason {
		total += count
	}
	if total != 1 {
		t.Fatalf("flock placement total attempts = %d, want exactly 1: %+v", total, state.AttemptsByOutcomeReason)
	}
}

func requireFlockPlacementMetricPhaseCount(t *testing.T, state FlockPlacementMetricsState, phase string, want int64) {
	t.Helper()
	hist := state.LatencyByPhase[phase]
	if hist.Count != want {
		t.Fatalf("flock placement latency count phase=%q = %d, want %d", phase, hist.Count, want)
	}
	if got := hist.Buckets["+Inf"]; got != want {
		t.Fatalf("flock placement +Inf bucket phase=%q = %d, want %d", phase, got, want)
	}
}

func TestRuntimeRouterRejectsQuotaBeforeDaemonCall(t *testing.T) {
	daemon := &routerFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler([]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}}, map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}}, map[string]TenantUsage{"tenant-1": {ActiveVMs: 1}}),
		map[string]Daemon{"host-a": daemon},
	)

	_, err := router.SpawnVM(context.Background(), SpawnVMRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, TenantUsage{ActiveVMs: 1})
	if err == nil {
		t.Fatal("SpawnVM error = nil, want quota denial")
	}
	var denied *ScheduleDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error type = %T, want ScheduleDeniedError", err)
	}
	if denied.Decision.Reason != "quota_exceeded" {
		t.Fatalf("denial reason = %q, want quota_exceeded", denied.Decision.Reason)
	}
	if daemon.spawnCalls != 0 {
		t.Fatalf("daemon spawn calls = %d, want 0", daemon.spawnCalls)
	}
}

func TestRuntimeRouterCreateFlockSchedulesByRoleCountAndRecordsPlacements(t *testing.T) {
	hostA := &routerFlockFakeDaemon{}
	hostB := &routerFlockFakeDaemon{createFlockResp: &FlockCreateResponse{
		FlockID: "flock-1",
		Agents: []FlockAgentInfo{
			{AgentID: "agent-worker", Role: "worker", VMID: "vm-worker-1", Status: "running"},
			{AgentID: "agent-reviewer", Role: "reviewer", VMID: "vm-reviewer-1", Status: "running"},
		},
	}}
	router := NewRuntimeRouter(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
	)

	resp, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     " tenant-1 ",
		EgressPolicy: " PROFILE ",
	})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if resp.FlockID != "flock-1" {
		t.Fatalf("flock id = %q, want flock-1", resp.FlockID)
	}
	if hostA.createFlockCalls != 0 || hostB.createFlockCalls != 1 {
		t.Fatalf("CreateFlock calls hostA/hostB = %d/%d, want 0/1", hostA.createFlockCalls, hostB.createFlockCalls)
	}
	if hostB.createFlockReq.TenantID != "tenant-1" {
		t.Fatalf("daemon tenant = %q, want tenant-1", hostB.createFlockReq.TenantID)
	}
	if hostB.createFlockReq.EgressPolicy != "profile" {
		t.Fatalf("daemon egress policy = %q, want profile", hostB.createFlockReq.EgressPolicy)
	}
	for _, vmID := range []string{"vm-worker-1", "vm-reviewer-1"} {
		if host, ok := router.Placement(vmID); !ok || host != "host-b" {
			t.Fatalf("placement for %s = %q,%v want host-b,true", vmID, host, ok)
		}
	}
}

func TestRuntimeRouterCreateFlockRejectsQuotaBeforeDaemonCall(t *testing.T) {
	daemon := &routerFlockFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}},
			map[string]TenantUsage{"tenant-1": {ActiveVMs: 0}},
		),
		map[string]Daemon{"host-a": daemon},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateFlock error = nil, want quota denial")
	}
	var denied *ScheduleDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error type = %T, want ScheduleDeniedError", err)
	}
	if denied.Decision.Reason != "quota_exceeded" {
		t.Fatalf("denial reason = %q, want quota_exceeded", denied.Decision.Reason)
	}
	if daemon.createFlockCalls != 0 {
		t.Fatalf("daemon CreateFlock calls = %d, want 0", daemon.createFlockCalls)
	}
}

func TestRuntimeRouterCreateFlockReportsPlacementSaveFailureWithoutSecrets(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	daemon := &routerFlockFakeDaemon{createFlockResp: &FlockCreateResponse{
		FlockID: "flock-save-failure",
		Agents: []FlockAgentInfo{
			{AgentID: "agent-worker", Role: "worker", VMID: "vm-worker-1", AgentURL: "http://secret-token.example/agent", Status: "running"},
		},
		TownWallURL: "http://secret-token.example/townwall",
		PostURL:     "http://secret-token.example/post",
	}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateFlock error = nil, want placement save failure")
	}
	text := err.Error()
	if !strings.Contains(text, "flock created but placement save failed") {
		t.Fatalf("error = %q, want placement save failure context", text)
	}
	if !strings.Contains(text, "flock-save-failure") {
		t.Fatalf("error = %q, want flock id", text)
	}
	for _, forbidden := range []string{"agent_token", "Authorization", "Bearer", "secret-token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("error contains forbidden secret marker %q: %q", forbidden, text)
		}
	}
	if daemon.createFlockCalls != 1 {
		t.Fatalf("daemon CreateFlock calls = %d, want 1", daemon.createFlockCalls)
	}
}

func TestRuntimeRouterCreateFlockRecordsSuccessMetrics(t *testing.T) {
	store := NewPlacementStore("")
	daemon := &routerFlockFakeDaemon{createFlockResp: &FlockCreateResponse{
		FlockID: "flock-metrics-success",
		Agents: []FlockAgentInfo{
			{AgentID: "agent-worker", Role: "worker", VMID: "vm-worker-1", Status: "running"},
			{AgentID: "agent-reviewer", Role: "reviewer", VMID: "vm-reviewer-1", Status: "running"},
		},
	}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	resp, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatalf("CreateFlock returned error: %v", err)
	}
	if resp.FlockID != "flock-metrics-success" {
		t.Fatalf("flock id = %q, want flock-metrics-success", resp.FlockID)
	}

	state := store.State().FlockPlacementMetrics
	requireFlockPlacementMetricAttempt(t, state, FlockPlacementOutcomeSuccess, FlockPlacementReasonScheduled, 1)
	for _, phase := range []string{
		FlockPlacementPhaseSchedule,
		FlockPlacementPhaseDaemonCreate,
		FlockPlacementPhasePlacementSave,
		FlockPlacementPhaseTotal,
	} {
		requireFlockPlacementMetricPhaseCount(t, state, phase, 1)
	}
	if state.LastSuccessAt.IsZero() {
		t.Fatal("LastSuccessAt is zero, want success timestamp")
	}
}

func TestRuntimeRouterCreateFlockRecordsQuotaDeniedMetrics(t *testing.T) {
	store := NewPlacementStore("")
	daemon := &routerFlockFakeDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}},
			map[string]TenantUsage{"tenant-1": {ActiveVMs: 0}},
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateFlock error = nil, want quota denial")
	}
	var denied *ScheduleDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error type = %T, want ScheduleDeniedError", err)
	}
	if daemon.createFlockCalls != 0 {
		t.Fatalf("daemon CreateFlock calls = %d, want 0", daemon.createFlockCalls)
	}

	state := store.State().FlockPlacementMetrics
	requireFlockPlacementMetricAttempt(t, state, FlockPlacementOutcomeDenied, FlockPlacementReasonQuotaExceeded, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseSchedule, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseTotal, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseDaemonCreate, 0)
	if state.LastFailureAt.IsZero() {
		t.Fatal("LastFailureAt is zero, want failure timestamp")
	}
}

func TestRuntimeRouterCreateFlockRecordsDaemonErrorMetrics(t *testing.T) {
	store := NewPlacementStore("")
	daemonErr := errors.New("daemon CreateFlock failed: agent_token")
	daemon := &routerFlockFakeDaemon{createFlockErr: daemonErr}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if !errors.Is(err, daemonErr) {
		t.Fatalf("CreateFlock error = %v, want daemon error", err)
	}
	if daemon.createFlockCalls != 1 {
		t.Fatalf("daemon CreateFlock calls = %d, want 1", daemon.createFlockCalls)
	}

	state := store.State().FlockPlacementMetrics
	requireFlockPlacementMetricAttempt(t, state, FlockPlacementOutcomeDaemonError, FlockPlacementReasonDaemonCreateFailed, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseSchedule, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseDaemonCreate, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseTotal, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhasePlacementSave, 0)
	output := RenderSchedulerMetrics(store.State())
	if strings.Contains(output, "agent_token") {
		t.Fatalf("metrics output leaked daemon error token marker:\n%s", output)
	}
}

func TestRuntimeRouterCreateFlockRecordsNilResponseMetrics(t *testing.T) {
	store := NewPlacementStore("")
	daemon := &routerFlockFakeDaemon{createFlockNil: true}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateFlock(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateFlock error = nil, want nil response error")
	}
	if !strings.Contains(err.Error(), "runtime daemon CreateFlock returned nil response") {
		t.Fatalf("CreateFlock error = %q, want nil response context", err.Error())
	}
	if daemon.createFlockCalls != 1 {
		t.Fatalf("daemon CreateFlock calls = %d, want 1", daemon.createFlockCalls)
	}

	state := store.State().FlockPlacementMetrics
	requireFlockPlacementMetricAttempt(t, state, FlockPlacementOutcomeDaemonNilResponse, FlockPlacementReasonDaemonNilResponse, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseSchedule, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseDaemonCreate, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseTotal, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhasePlacementSave, 0)
}

func TestRuntimeRouterCreateRoutedFlockMembersSpawnsAcrossHosts(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	hostA := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID:         "vm-worker-1",
		GuestIP:      "10.0.1.10",
		AgentURL:     "http://10.0.1.10:8080",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		AgentToken:   "agent-token-worker",
	}}}
	hostB := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID:         "vm-reviewer-1",
		GuestIP:      "10.0.2.10",
		AgentURL:     "http://10.0.2.10:8080",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		AgentToken:   "agent-token-reviewer",
	}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     " tenant-1 ",
		EgressPolicy: " PROFILE ",
	})
	if err != nil {
		t.Fatalf("CreateRoutedFlockMembers returned error: %v", err)
	}
	if !strings.HasPrefix(out.FlockID, "routed-flock-") {
		t.Fatalf("flock id = %q, want routed-flock- prefix", out.FlockID)
	}
	if out.Task != "review worker output" {
		t.Fatalf("task = %q, want review worker output", out.Task)
	}
	if out.TenantID != "tenant-1" {
		t.Fatalf("tenant id = %q, want tenant-1", out.TenantID)
	}
	if out.EgressPolicy != "profile" {
		t.Fatalf("egress policy = %q, want profile", out.EgressPolicy)
	}
	if out.Mode != RoutedFlockModeCrossHostMembersOnly {
		t.Fatalf("mode = %q, want %q", out.Mode, RoutedFlockModeCrossHostMembersOnly)
	}
	if out.Status != RoutedFlockStatusReady {
		t.Fatalf("status = %q, want %q", out.Status, RoutedFlockStatusReady)
	}
	if !out.TownWallEnabled {
		t.Fatal("town wall enabled = false, want true (shared town wall wired)")
	}
	if len(out.Agents) != 2 {
		t.Fatalf("agents len = %d, want 2: %+v", len(out.Agents), out.Agents)
	}
	if hostA.spawnCalls != 1 || hostB.spawnCalls != 1 {
		t.Fatalf("spawn calls hostA/hostB = %d/%d, want 1/1", hostA.spawnCalls, hostB.spawnCalls)
	}
	if len(hostA.spawnReqs) != 1 || len(hostB.spawnReqs) != 1 {
		t.Fatalf("spawn req counts hostA/hostB = %d/%d, want 1/1", len(hostA.spawnReqs), len(hostB.spawnReqs))
	}
	if req := hostA.spawnReqs[0]; req.Profile != "worker" || req.TenantID != "tenant-1" || req.EgressPolicy != "profile" {
		t.Fatalf("host-a spawn req = %+v, want worker tenant-1 profile", req)
	}
	if req := hostB.spawnReqs[0]; req.Profile != "reviewer" || req.TenantID != "tenant-1" || req.EgressPolicy != "profile" {
		t.Fatalf("host-b spawn req = %+v, want reviewer tenant-1 profile", req)
	}

	agentsByID := make(map[string]RoutedFlockAgent, len(out.Agents))
	for _, agent := range out.Agents {
		agentsByID[agent.AgentID] = agent
	}
	worker := agentsByID["worker-1"]
	if worker.Role != "worker" || worker.VMID != "vm-worker-1" || worker.Host != "host-a" || worker.Status != "running" || worker.AgentURL != "http://10.0.1.10:8080" {
		t.Fatalf("worker agent = %+v, want host-a vm-worker-1 running", worker)
	}
	reviewer := agentsByID["reviewer-1"]
	if reviewer.Role != "reviewer" || reviewer.VMID != "vm-reviewer-1" || reviewer.Host != "host-b" || reviewer.Status != "running" || reviewer.AgentURL != "http://10.0.2.10:8080" {
		t.Fatalf("reviewer agent = %+v, want host-b vm-reviewer-1 running", reviewer)
	}

	outputJSON, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	for _, forbidden := range []string{"agent-token", "agent_token", "secret-endpoint", "host-a.internal", "host-b.internal", "Authorization", "Bearer"} {
		if strings.Contains(string(outputJSON), forbidden) {
			t.Fatalf("routed output leaked forbidden marker %q: %s", forbidden, outputJSON)
		}
	}

	record, ok := store.RoutedFlock(out.FlockID)
	if !ok {
		t.Fatalf("routed flock %q not found in registry", out.FlockID)
	}
	if record.Status != RoutedFlockStatusReady {
		t.Fatalf("registry status = %q, want %q", record.Status, RoutedFlockStatusReady)
	}
	if record.Mode != RoutedFlockModeCrossHostMembersOnly {
		t.Fatalf("registry mode = %q, want %q", record.Mode, RoutedFlockModeCrossHostMembersOnly)
	}
	if len(record.Agents) != 2 {
		t.Fatalf("registry agents len = %d, want 2: %+v", len(record.Agents), record.Agents)
	}
	registryJSON, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal registry record: %v", err)
	}
	for _, forbidden := range []string{"agent-token", "agent_token", "secret-endpoint", "host-a.internal", "host-b.internal", "Authorization", "Bearer"} {
		if strings.Contains(string(registryJSON), forbidden) {
			t.Fatalf("registry record leaked forbidden marker %q: %s", forbidden, registryJSON)
		}
	}

	for vmID, wantHost := range map[string]string{"vm-worker-1": "host-a", "vm-reviewer-1": "host-b"} {
		if host, ok := router.Placement(vmID); !ok || host != wantHost {
			t.Fatalf("router placement for %s = %q,%v want %s,true", vmID, host, ok, wantHost)
		}
		if host, ok := store.VMHost(vmID); !ok || host != wantHost {
			t.Fatalf("store placement for %s = %q,%v want %s,true", vmID, host, ok, wantHost)
		}
	}
}

func TestCreateRoutedFlockMembers_WiresSharedTownWall(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	member := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-researcher-1", GuestIP: "10.0.2.10", AgentURL: "http://10.0.2.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"hostA": home, "hostB": member},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "smoke",
		Roles:        []string{"coordinator", "researcher"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.TownWallEnabled {
		t.Fatalf("TownWallEnabled = false, want true")
	}
	rec, ok := store.RoutedFlock(out.FlockID)
	if !ok {
		t.Fatalf("routed flock %q not found in registry", out.FlockID)
	}
	if rec.HomeHost != "hostA" {
		t.Fatalf("HomeHost = %q, want hostA (roles[0] placement)", rec.HomeHost)
	}
	if home.distributedCalls != 1 {
		t.Fatalf("home distributed (hub) registrations = %d, want 1", home.distributedCalls)
	}
	if member.relayCalls != 1 {
		t.Fatalf("member relay registrations = %d, want 1", member.relayCalls)
	}
	if home.relayCalls != 0 {
		t.Fatalf("home relay registrations = %d, want 0 (home already owns the hub)", home.relayCalls)
	}
	// spawn identity injected on the member
	if member.spawnReq.FlockID != out.FlockID || member.spawnReq.AgentID == "" || member.spawnReq.ControlPlaneToken == "" {
		t.Fatalf("member spawn missing flock identity: %+v", member.spawnReq)
	}
	// tenant/egress propagation preserved on the member spawn
	if member.spawnReq.TenantID != "tenant-1" || member.spawnReq.EgressPolicy != "profile" {
		t.Fatalf("member spawn dropped tenant/egress: %+v", member.spawnReq)
	}
	// relay registration carries the home daemon base URL + the shared relay token
	if member.relayReq.HomeAddr == "" || member.relayReq.RelayToken == "" {
		t.Fatalf("relay registration missing home addr/token: %+v", member.relayReq)
	}
	// the guest control-plane token equals the flock relay token
	if member.spawnReq.ControlPlaneToken != member.relayReq.RelayToken {
		t.Fatalf("guest cp token %q != relay token %q", member.spawnReq.ControlPlaneToken, member.relayReq.RelayToken)
	}
	// the hub registration carries the full roster + the same relay token
	if len(home.distributedReq.Roster) != 2 || home.distributedReq.RelayToken != member.relayReq.RelayToken {
		t.Fatalf("hub registration roster/token mismatch: %+v", home.distributedReq)
	}
	// relay token persisted for reconcile but redacted from every MCP-facing view
	token, ok := store.RoutedFlockRelayToken(out.FlockID)
	if !ok || token == "" {
		t.Fatalf("relay token not persisted for flock %q", out.FlockID)
	}
	if token != member.relayReq.RelayToken {
		t.Fatalf("persisted relay token %q != registered token %q", token, member.relayReq.RelayToken)
	}
	outJSON, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	recJSON, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if strings.Contains(string(outJSON), token) || strings.Contains(string(recJSON), token) {
		t.Fatalf("relay token leaked into MCP-facing output/record: out=%s rec=%s", outJSON, recJSON)
	}
	// State() (scheduler placements endpoint) must not expose the relay token
	stateJSON, err := json.Marshal(store.State())
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if strings.Contains(string(stateJSON), token) {
		t.Fatalf("relay token leaked into placement store State(): %s", stateJSON)
	}
}

func TestReconcile_ReregistersSharedTownWall(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	home := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-coordinator-1", GuestIP: "10.0.1.10", AgentURL: "http://10.0.1.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	member := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID: "vm-researcher-1", GuestIP: "10.0.2.10", AgentURL: "http://10.0.2.10:8080", TenantID: "tenant-1", EgressPolicy: "profile",
	}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "hostA", Endpoint: "http://hostA.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "hostB", Endpoint: "http://hostB.internal:8080", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"hostA": home, "hostB": member},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "smoke",
		Roles:        []string{"coordinator", "researcher"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a daemon restart: the in-memory hub/relay registrations are lost.
	home.distributedCalls, member.relayCalls = 0, 0
	home.relayCalls, member.distributedCalls = 0, 0

	if err := router.ReconcilePlacements(context.Background()); err != nil {
		t.Fatalf("ReconcilePlacements: %v", err)
	}
	if home.distributedCalls != 1 {
		t.Fatalf("home hub re-registrations = %d, want 1", home.distributedCalls)
	}
	if member.relayCalls != 1 {
		t.Fatalf("member relay re-registrations = %d, want 1", member.relayCalls)
	}
	if home.relayCalls != 0 {
		t.Fatalf("home relay re-registrations = %d, want 0 (home owns the hub)", home.relayCalls)
	}
	if member.distributedCalls != 0 {
		t.Fatalf("member hub re-registrations = %d, want 0", member.distributedCalls)
	}

	// Reconcile re-issues the SAME persisted relay token + full roster daemon-to-daemon.
	token, ok := store.RoutedFlockRelayToken(out.FlockID)
	if !ok || token == "" {
		t.Fatalf("relay token not persisted for flock %q", out.FlockID)
	}
	if home.distributedReq.RelayToken != token {
		t.Fatalf("reconcile hub re-registration relay token = %q, want persisted token", home.distributedReq.RelayToken)
	}
	if member.relayReq.RelayToken != token {
		t.Fatalf("reconcile relay re-registration relay token = %q, want persisted token", member.relayReq.RelayToken)
	}
	if member.relayReq.HomeAddr == "" {
		t.Fatalf("reconcile relay re-registration missing home addr")
	}
	if len(home.distributedReq.Roster) != 2 {
		t.Fatalf("reconcile hub roster len = %d, want 2", len(home.distributedReq.Roster))
	}
}

func TestRuntimeRouterCreateRoutedFlockMembersDeniedBeforeDaemonCall(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	daemon := &routerFakeDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a.internal/secret-endpoint", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			map[string]TenantQuota{"tenant-1": {ActiveVMs: 1}},
			map[string]TenantUsage{"tenant-1": {ActiveVMs: 0}},
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want quota denial")
	}
	var denied *ScheduleDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error type = %T, want ScheduleDeniedError", err)
	}
	if denied.Decision.Reason != FlockPlacementReasonQuotaExceeded {
		t.Fatalf("denial reason = %q, want %q", denied.Decision.Reason, FlockPlacementReasonQuotaExceeded)
	}
	if daemon.spawnCalls != 0 {
		t.Fatalf("daemon spawn calls = %d, want 0", daemon.spawnCalls)
	}

	state := store.State().FlockPlacementMetrics
	requireOnlyFlockPlacementMetricAttempt(t, state, FlockPlacementOutcomeCrossHostDenied, FlockPlacementReasonQuotaExceeded)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhasePlan, 1)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseTotal, 1)
}

func TestRuntimeRouterCreateRoutedFlockMembersRollsBackOnSecondSpawnFailure(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	hostA := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID:         "vm-worker",
		GuestIP:      "10.0.1.10",
		AgentURL:     "http://10.0.1.10:8080",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		AgentToken:   "agent-token-worker",
	}}}
	hostB := &routerFakeDaemon{
		spawnErr: errors.New("daemon http://host-b.internal/secret-endpoint failed: agent_token=secret-token Authorization: Bearer secret-token"),
	}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want second spawn failure")
	}
	if out != nil {
		t.Fatalf("CreateRoutedFlockMembers output = %+v, want nil on rollback", out)
	}
	if hostA.spawnCalls != 1 || hostB.spawnCalls != 1 {
		t.Fatalf("spawn calls hostA/hostB = %d/%d, want 1/1", hostA.spawnCalls, hostB.spawnCalls)
	}
	if hostA.deleteCalls != 1 || strings.Join(hostA.deleteVMIDs, ",") != "vm-worker" {
		t.Fatalf("host-a delete calls/vmids = %d/%q, want 1/vm-worker", hostA.deleteCalls, strings.Join(hostA.deleteVMIDs, ","))
	}
	if hostB.deleteCalls != 0 {
		t.Fatalf("host-b delete calls = %d, want 0", hostB.deleteCalls)
	}
	if _, ok := router.Placement("vm-worker"); ok {
		t.Fatal("router placement for vm-worker still exists after rollback")
	}
	if _, ok := store.VMHost("vm-worker"); ok {
		t.Fatal("store placement for vm-worker still exists after rollback")
	}
	records := store.ListRoutedFlocks()
	if len(records) != 1 {
		t.Fatalf("routed records len = %d, want 1", len(records))
	}
	if records[0].Status != RoutedFlockStatusDeleted {
		t.Fatalf("routed record status = %q, want %q", records[0].Status, RoutedFlockStatusDeleted)
	}
	if len(records[0].Agents) != 0 {
		t.Fatalf("routed record agents = %+v, want none after successful rollback", records[0].Agents)
	}

	text := err.Error()
	for _, forbidden := range []string{"agent_token", "secret-token", "Authorization", "Bearer", "host-a.internal", "host-b.internal", "secret-endpoint", "http://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("CreateRoutedFlockMembers error leaked forbidden marker %q: %q", forbidden, text)
		}
	}
	if !strings.Contains(text, FlockPlacementReasonDaemonCreateFailed) {
		t.Fatalf("CreateRoutedFlockMembers error = %q, want bounded daemon_create_failed reason", text)
	}

	state := store.State().FlockPlacementMetrics
	requireOnlyFlockPlacementMetricAttempt(t, state, FlockPlacementOutcomeCrossHostSpawnError, FlockPlacementReasonDaemonCreateFailed)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseRollback, 1)
}

func TestRuntimeRouterCreateRoutedFlockMembersPreservesDeletedRollbackInMemoryWhenFinalSaveFails(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	hostA := &routerFakeDaemon{
		spawnResponses: []*SpawnVMResponse{{
			VMID:         "vm-worker",
			GuestIP:      "10.0.1.10",
			AgentURL:     "http://10.0.1.10:8080",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
		}},
		afterDelete: func() {
			store.path = t.TempDir()
		},
	}
	hostB := &routerFakeDaemon{
		spawnErr: errors.New("daemon http://host-b.internal/secret-endpoint failed: agent_token=secret-token"),
	}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want rollback save failure")
	}
	if out != nil {
		t.Fatalf("CreateRoutedFlockMembers output = %+v, want nil on rollback", out)
	}
	if hostA.deleteCalls != 1 || strings.Join(hostA.deleteVMIDs, ",") != "vm-worker" {
		t.Fatalf("host-a delete calls/vmids = %d/%q, want 1/vm-worker", hostA.deleteCalls, strings.Join(hostA.deleteVMIDs, ","))
	}
	if _, ok := router.Placement("vm-worker"); ok {
		t.Fatal("router placement for vm-worker still exists after rollback cleanup")
	}
	if _, ok := store.VMHost("vm-worker"); ok {
		t.Fatal("store placement for vm-worker still exists after rollback save failure")
	}
	records := store.ListRoutedFlocks()
	if len(records) != 1 {
		t.Fatalf("routed records len = %d, want 1", len(records))
	}
	if records[0].Status != RoutedFlockStatusDeleted {
		t.Fatalf("routed record status = %q, want %q", records[0].Status, RoutedFlockStatusDeleted)
	}
	if len(records[0].Agents) != 0 {
		t.Fatalf("routed record agents = %+v, want none after successful cleanup", records[0].Agents)
	}

	text := err.Error()
	if !strings.Contains(text, "routed flock create cleanup pending") || !strings.Contains(text, FlockPlacementReasonDaemonCreateFailed) {
		t.Fatalf("CreateRoutedFlockMembers error = %q, want cleanup pending daemon_create_failed", text)
	}
	for _, forbidden := range []string{"agent_token", "secret-token", "Authorization", "Bearer", "host-a.internal", "host-b.internal", "secret-endpoint", "http://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("CreateRoutedFlockMembers error leaked forbidden marker %q: %q", forbidden, text)
		}
	}
}

func TestRuntimeRouterCreateRoutedFlockMembersPreservesPartialRollbackInMemoryWhenFinalSaveFails(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	hostA := &routerFakeDaemon{
		spawnResponses: []*SpawnVMResponse{{
			VMID:         "vm-worker",
			GuestIP:      "10.0.1.10",
			AgentURL:     "http://10.0.1.10:8080",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
		}},
		afterDelete: func() {
			store.path = t.TempDir()
		},
	}
	hostB := &routerFakeDaemon{
		spawnResponses: []*SpawnVMResponse{{
			VMID:         "vm-reviewer",
			GuestIP:      "10.0.2.10",
			AgentURL:     "http://10.0.2.10:8080",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
		}},
		deleteErrForVMID: map[string]error{
			"vm-reviewer": errors.New("delete failed: agent_token=secret-token Authorization: Bearer secret-token"),
		},
	}
	hostC := &routerFakeDaemon{
		spawnErr: errors.New("daemon http://host-c.internal/secret-endpoint failed: agent_token=secret-token"),
	}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-c", Endpoint: "http://host-c.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB, "host-c": hostC},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer", "critic"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want rollback cleanup pending")
	}
	if out != nil {
		t.Fatalf("CreateRoutedFlockMembers output = %+v, want nil on rollback", out)
	}
	if hostA.deleteCalls != 1 || strings.Join(hostA.deleteVMIDs, ",") != "vm-worker" {
		t.Fatalf("host-a delete calls/vmids = %d/%q, want 1/vm-worker", hostA.deleteCalls, strings.Join(hostA.deleteVMIDs, ","))
	}
	if hostB.deleteCalls != 1 || strings.Join(hostB.deleteVMIDs, ",") != "vm-reviewer" {
		t.Fatalf("host-b delete calls/vmids = %d/%q, want 1/vm-reviewer", hostB.deleteCalls, strings.Join(hostB.deleteVMIDs, ","))
	}
	if _, ok := router.Placement("vm-worker"); ok {
		t.Fatal("router placement for vm-worker still exists after successful rollback cleanup")
	}
	if _, ok := store.VMHost("vm-worker"); ok {
		t.Fatal("store placement for vm-worker still exists after partial rollback save failure")
	}
	if host, ok := router.Placement("vm-reviewer"); !ok || host != "host-b" {
		t.Fatalf("router placement for vm-reviewer = %q,%v want host-b,true", host, ok)
	}
	if host, ok := store.VMHost("vm-reviewer"); !ok || host != "host-b" {
		t.Fatalf("store placement for vm-reviewer = %q,%v want host-b,true", host, ok)
	}
	records := store.ListRoutedFlocks()
	if len(records) != 1 {
		t.Fatalf("routed records len = %d, want 1", len(records))
	}
	if records[0].Status != RoutedFlockStatusFailedCleanupPending {
		t.Fatalf("routed record status = %q, want %q", records[0].Status, RoutedFlockStatusFailedCleanupPending)
	}
	if len(records[0].Agents) != 1 {
		t.Fatalf("routed record agents = %+v, want one cleanup-pending agent", records[0].Agents)
	}
	if records[0].Agents[0].VMID != "vm-reviewer" || records[0].Agents[0].Status != "cleanup_pending" {
		t.Fatalf("cleanup-pending agent = %+v, want vm-reviewer cleanup_pending", records[0].Agents[0])
	}

	text := err.Error()
	if !strings.Contains(text, "routed flock create cleanup pending") || !strings.Contains(text, FlockPlacementReasonDaemonCreateFailed) {
		t.Fatalf("CreateRoutedFlockMembers error = %q, want cleanup pending daemon_create_failed", text)
	}
	for _, forbidden := range []string{"agent_token", "secret-token", "Authorization", "Bearer", "host-a.internal", "host-b.internal", "host-c.internal", "secret-endpoint", "http://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("CreateRoutedFlockMembers error leaked forbidden marker %q: %q", forbidden, text)
		}
	}
}

func TestRuntimeRouterCreateRoutedFlockMembersLeavesCleanupPendingOnRollbackDeleteFailure(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	hostA := &routerFakeDaemon{
		spawnResponses: []*SpawnVMResponse{{
			VMID:         "vm-worker",
			GuestIP:      "10.0.1.10",
			AgentURL:     "http://10.0.1.10:8080",
			TenantID:     "tenant-1",
			EgressPolicy: "profile",
		}},
		deleteErrForVMID: map[string]error{
			"vm-worker": errors.New("delete failed: agent_token=secret-token Authorization: Bearer secret-token"),
		},
	}
	hostB := &routerFakeDaemon{
		spawnErr: errors.New("daemon http://host-b.internal/secret-endpoint failed: agent_token=secret-token"),
	}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b.internal/secret-endpoint", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	_, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker", "reviewer"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want cleanup pending error")
	}
	if hostA.deleteCalls != 1 || strings.Join(hostA.deleteVMIDs, ",") != "vm-worker" {
		t.Fatalf("host-a delete calls/vmids = %d/%q, want 1/vm-worker", hostA.deleteCalls, strings.Join(hostA.deleteVMIDs, ","))
	}
	records := store.ListRoutedFlocks()
	if len(records) != 1 {
		t.Fatalf("routed records len = %d, want 1", len(records))
	}
	if records[0].Status != RoutedFlockStatusFailedCleanupPending {
		t.Fatalf("routed record status = %q, want %q", records[0].Status, RoutedFlockStatusFailedCleanupPending)
	}
	if len(records[0].Agents) != 1 {
		t.Fatalf("routed record agents = %+v, want one cleanup-pending agent", records[0].Agents)
	}
	if records[0].Agents[0].VMID != "vm-worker" || records[0].Agents[0].Status != "cleanup_pending" {
		t.Fatalf("cleanup-pending agent = %+v, want vm-worker cleanup_pending", records[0].Agents[0])
	}
	if host, ok := router.Placement("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("router placement for vm-worker = %q,%v want host-a,true", host, ok)
	}
	if host, ok := store.VMHost("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("store placement for vm-worker = %q,%v want host-a,true", host, ok)
	}

	text := err.Error()
	for _, forbidden := range []string{"agent_token", "secret-token", "Authorization", "Bearer", "host-a.internal", "host-b.internal", "secret-endpoint", "http://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("CreateRoutedFlockMembers cleanup error leaked forbidden marker %q: %q", forbidden, text)
		}
	}
	if !strings.Contains(text, FlockPlacementReasonDaemonCreateFailed) {
		t.Fatalf("CreateRoutedFlockMembers cleanup error = %q, want bounded daemon_create_failed reason", text)
	}

	state := store.State().FlockPlacementMetrics
	requireOnlyFlockPlacementMetricAttempt(t, state, FlockPlacementOutcomeCrossHostRollbackError, FlockPlacementReasonDaemonCreateFailed)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseRollback, 1)
}

func TestRuntimeRouterCreateRoutedFlockMembersRejectsEmptySpawnVMID(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	daemon := &routerFakeDaemon{spawnResponses: []*SpawnVMResponse{{
		VMID:         "  ",
		GuestIP:      "10.0.1.10",
		AgentURL:     "http://10.0.1.10:8080",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": daemon},
		RuntimeRouterOptions{PlacementStore: store},
	)

	out, err := router.CreateRoutedFlockMembers(context.Background(), FlockCreateRequest{
		Task:         "review worker output",
		Roles:        []string{"worker"},
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
	})
	if err == nil {
		t.Fatal("CreateRoutedFlockMembers error = nil, want empty vm_id error")
	}
	if out != nil {
		t.Fatalf("CreateRoutedFlockMembers output = %+v, want nil on empty vm_id", out)
	}
	if !strings.Contains(err.Error(), FlockPlacementReasonDaemonInvalidResponse) {
		t.Fatalf("CreateRoutedFlockMembers error = %q, want bounded daemon_invalid_response reason", err.Error())
	}
	if daemon.spawnCalls != 1 {
		t.Fatalf("spawn calls = %d, want 1", daemon.spawnCalls)
	}
	if daemon.deleteCalls != 0 {
		t.Fatalf("delete calls = %d, want 0 for invalid empty vm_id response", daemon.deleteCalls)
	}

	records := store.ListRoutedFlocks()
	if len(records) != 1 {
		t.Fatalf("routed records len = %d, want 1", len(records))
	}
	if records[0].Status != RoutedFlockStatusDeleted {
		t.Fatalf("routed record status = %q, want %q", records[0].Status, RoutedFlockStatusDeleted)
	}
	if len(records[0].Agents) != 0 {
		t.Fatalf("routed record agents = %+v, want none for empty vm_id", records[0].Agents)
	}

	state := store.State().FlockPlacementMetrics
	requireOnlyFlockPlacementMetricAttempt(t, state, FlockPlacementOutcomeCrossHostSpawnError, FlockPlacementReasonDaemonInvalidResponse)
	requireFlockPlacementMetricPhaseCount(t, state, FlockPlacementPhaseAgentSpawn, 1)
	if state.LastFailureAt.IsZero() {
		t.Fatal("LastFailureAt is zero, want failure timestamp")
	}
}

func TestRuntimeRouterDeleteRoutedFlockDeletesMemberVMs(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	if err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID:      "routed-flock-delete",
		Task:         "review worker output",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-worker", Host: "host-a", Status: "running"},
			{AgentID: "reviewer-1", Role: "reviewer", VMID: "vm-reviewer", Host: "host-b", Status: "running"},
		},
	}, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	hostA := &routerFakeDaemon{}
	hostB := &routerFakeDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	resp, err := router.DeleteRoutedFlock(context.Background(), " routed-flock-delete ")
	if err != nil {
		t.Fatalf("DeleteRoutedFlock returned error: %v", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("DeleteRoutedFlock resp = %+v, want status 200", resp)
	}
	if !strings.Contains(resp.Body, `"status":"deleted"`) || !strings.Contains(resp.Body, `"flock_id":"routed-flock-delete"`) {
		t.Fatalf("DeleteRoutedFlock body = %q, want deleted status and flock id", resp.Body)
	}
	if hostA.deleteCalls != 1 || strings.Join(hostA.deleteVMIDs, ",") != "vm-worker" {
		t.Fatalf("host-a delete calls/vmids = %d/%q, want 1/vm-worker", hostA.deleteCalls, strings.Join(hostA.deleteVMIDs, ","))
	}
	if hostB.deleteCalls != 1 || strings.Join(hostB.deleteVMIDs, ",") != "vm-reviewer" {
		t.Fatalf("host-b delete calls/vmids = %d/%q, want 1/vm-reviewer", hostB.deleteCalls, strings.Join(hostB.deleteVMIDs, ","))
	}
	for _, vmID := range []string{"vm-worker", "vm-reviewer"} {
		if _, ok := router.Placement(vmID); ok {
			t.Fatalf("router placement for %s still exists after delete", vmID)
		}
		if _, ok := store.VMHost(vmID); ok {
			t.Fatalf("store placement for %s still exists after delete", vmID)
		}
	}
	record, ok := store.RoutedFlock("routed-flock-delete")
	if !ok {
		t.Fatal("routed-flock-delete missing after delete")
	}
	if record.Status != RoutedFlockStatusDeleted {
		t.Fatalf("routed record status = %q, want %q", record.Status, RoutedFlockStatusDeleted)
	}
	if len(record.Agents) != 0 {
		t.Fatalf("routed record agents = %+v, want none after delete", record.Agents)
	}
}

func TestRuntimeRouterDeleteRoutedFlockLeavesCleanupPendingOnPartialFailure(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	if err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID:      "routed-flock-partial-delete",
		Task:         "review worker output",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-worker", Host: "host-a", Status: "running"},
			{AgentID: "reviewer-1", Role: "reviewer", VMID: "vm-reviewer", Host: "host-b", Status: "running"},
		},
	}, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	hostA := &routerFakeDaemon{}
	hostB := &routerFakeDaemon{deleteErrForVMID: map[string]error{
		"vm-reviewer": errors.New("delete failed: agent_token=secret-token Authorization: Bearer secret-token"),
	}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	resp, err := router.DeleteRoutedFlock(context.Background(), "routed-flock-partial-delete")
	if err == nil {
		t.Fatal("DeleteRoutedFlock error = nil, want cleanup pending error")
	}
	if resp != nil {
		t.Fatalf("DeleteRoutedFlock resp = %+v, want nil on partial failure", resp)
	}
	if hostA.deleteCalls != 1 || strings.Join(hostA.deleteVMIDs, ",") != "vm-worker" {
		t.Fatalf("host-a delete calls/vmids = %d/%q, want 1/vm-worker", hostA.deleteCalls, strings.Join(hostA.deleteVMIDs, ","))
	}
	if hostB.deleteCalls != 1 || strings.Join(hostB.deleteVMIDs, ",") != "vm-reviewer" {
		t.Fatalf("host-b delete calls/vmids = %d/%q, want 1/vm-reviewer", hostB.deleteCalls, strings.Join(hostB.deleteVMIDs, ","))
	}
	if _, ok := router.Placement("vm-worker"); ok {
		t.Fatal("router placement for vm-worker still exists after successful delete")
	}
	if _, ok := store.VMHost("vm-worker"); ok {
		t.Fatal("store placement for vm-worker still exists after successful delete")
	}
	if host, ok := router.Placement("vm-reviewer"); !ok || host != "host-b" {
		t.Fatalf("router placement for vm-reviewer = %q,%v want host-b,true", host, ok)
	}
	if host, ok := store.VMHost("vm-reviewer"); !ok || host != "host-b" {
		t.Fatalf("store placement for vm-reviewer = %q,%v want host-b,true", host, ok)
	}
	record, ok := store.RoutedFlock("routed-flock-partial-delete")
	if !ok {
		t.Fatal("routed-flock-partial-delete missing after partial delete")
	}
	if record.Status != RoutedFlockStatusFailedCleanupPending {
		t.Fatalf("routed record status = %q, want %q", record.Status, RoutedFlockStatusFailedCleanupPending)
	}
	if len(record.Agents) != 1 {
		t.Fatalf("routed record agents = %+v, want one cleanup-pending agent", record.Agents)
	}
	if record.Agents[0].VMID != "vm-reviewer" || record.Agents[0].Status != "cleanup_pending" {
		t.Fatalf("cleanup-pending agent = %+v, want vm-reviewer cleanup_pending", record.Agents[0])
	}
	text := err.Error()
	for _, forbidden := range []string{"agent_token", "secret-token", "Authorization", "Bearer"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("DeleteRoutedFlock error leaked forbidden marker %q: %q", forbidden, text)
		}
	}
}

func TestRuntimeRouterDeleteRoutedFlockPreservesCleanupPendingInMemoryWhenFinalSaveFails(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	if err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID:      "routed-flock-final-save-fails",
		Task:         "review worker output",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-worker", Host: "host-a", Status: "running"},
		},
	}, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	hostA := &routerFakeDaemon{afterDelete: func() {
		store.path = t.TempDir()
	}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": hostA},
		RuntimeRouterOptions{PlacementStore: store},
	)

	resp, err := router.DeleteRoutedFlock(context.Background(), "routed-flock-final-save-fails")
	if err == nil {
		t.Fatal("DeleteRoutedFlock error = nil, want cleanup pending error")
	}
	if resp != nil {
		t.Fatalf("DeleteRoutedFlock resp = %+v, want nil on final save failure", resp)
	}
	if hostA.deleteCalls != 1 || strings.Join(hostA.deleteVMIDs, ",") != "vm-worker" {
		t.Fatalf("host-a delete calls/vmids = %d/%q, want 1/vm-worker", hostA.deleteCalls, strings.Join(hostA.deleteVMIDs, ","))
	}
	if _, ok := router.Placement("vm-worker"); ok {
		t.Fatal("router placement for vm-worker still exists after successful delete")
	}
	if _, ok := store.VMHost("vm-worker"); ok {
		t.Fatal("store placement for vm-worker still exists after final save failure")
	}
	record, ok := store.RoutedFlock("routed-flock-final-save-fails")
	if !ok {
		t.Fatal("routed-flock-final-save-fails missing after final save failure")
	}
	if record.Status != RoutedFlockStatusFailedCleanupPending {
		t.Fatalf("routed record status = %q, want %q", record.Status, RoutedFlockStatusFailedCleanupPending)
	}
	if len(record.Agents) != 0 {
		t.Fatalf("routed record agents = %+v, want none after successful cleanup", record.Agents)
	}
	text := err.Error()
	if !strings.Contains(text, "routed flock delete cleanup pending") || !strings.Contains(text, FlockPlacementReasonPlacementSaveFailed) {
		t.Fatalf("DeleteRoutedFlock error = %q, want cleanup pending placement save failure", text)
	}
	for _, forbidden := range []string{"agent_token", "secret-token", "Authorization", "Bearer"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("DeleteRoutedFlock error leaked forbidden marker %q: %q", forbidden, text)
		}
	}
}

func TestRuntimeRouterDeleteRoutedFlockTreatsNilDeleteResponseAsCleanupPending(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	if err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID:      "routed-flock-nil-delete",
		Task:         "review worker output",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusReady,
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-worker", Host: "host-a", Status: "running"},
		},
	}, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	hostA := &routerFakeDaemon{deleteNilResp: true}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": hostA},
		RuntimeRouterOptions{PlacementStore: store},
	)

	resp, err := router.DeleteRoutedFlock(context.Background(), "routed-flock-nil-delete")
	if err == nil {
		t.Fatal("DeleteRoutedFlock error = nil, want cleanup pending error")
	}
	if resp != nil {
		t.Fatalf("DeleteRoutedFlock resp = %+v, want nil on nil delete response", resp)
	}
	if hostA.deleteCalls != 1 || strings.Join(hostA.deleteVMIDs, ",") != "vm-worker" {
		t.Fatalf("host-a delete calls/vmids = %d/%q, want 1/vm-worker", hostA.deleteCalls, strings.Join(hostA.deleteVMIDs, ","))
	}
	if host, ok := router.Placement("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("router placement for vm-worker = %q,%v want host-a,true", host, ok)
	}
	if host, ok := store.VMHost("vm-worker"); !ok || host != "host-a" {
		t.Fatalf("store placement for vm-worker = %q,%v want host-a,true", host, ok)
	}
	record, ok := store.RoutedFlock("routed-flock-nil-delete")
	if !ok {
		t.Fatal("routed-flock-nil-delete missing after nil delete response")
	}
	if record.Status != RoutedFlockStatusFailedCleanupPending {
		t.Fatalf("routed record status = %q, want %q", record.Status, RoutedFlockStatusFailedCleanupPending)
	}
	if len(record.Agents) != 1 {
		t.Fatalf("routed record agents = %+v, want one cleanup-pending agent", record.Agents)
	}
	if record.Agents[0].VMID != "vm-worker" || record.Agents[0].Status != routedFlockAgentStatusCleanupPending {
		t.Fatalf("cleanup-pending agent = %+v, want vm-worker cleanup_pending", record.Agents[0])
	}
	if !strings.Contains(err.Error(), routedFlockReasonCleanupFailed) {
		t.Fatalf("DeleteRoutedFlock error = %q, want cleanup_failed reason", err.Error())
	}
}

// TestRuntimeRouterRoutedFlockTownWallDelegatesToHome locks in the Task 6
// contract: routed members now relay to the shared Town Wall on the home daemon,
// so PostRoutedTownWall / RoutedTownWallHistory no longer hard-error — they proxy
// to the home host's daemon (record.HomeHost) and never touch a member host.
func TestRuntimeRouterRoutedFlockTownWallDelegatesToHome(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	if err := store.SaveRoutedFlockAndPlacements(RoutedFlockRecord{
		FlockID:      "routed-flock-wall",
		Task:         "review worker output",
		TenantID:     "tenant-1",
		EgressPolicy: "profile",
		Mode:         RoutedFlockModeCrossHostMembersOnly,
		Status:       RoutedFlockStatusReady,
		HomeHost:     "host-a",
		Agents: []RoutedFlockAgent{
			{AgentID: "worker-1", Role: "worker", VMID: "vm-worker", Host: "host-a", Status: "running"},
			{AgentID: "worker-2", Role: "worker", VMID: "vm-worker-2", Host: "host-b", Status: "running"},
		},
	}, nil); err != nil {
		t.Fatalf("SaveRoutedFlockAndPlacements: %v", err)
	}
	home := &routerFakeDaemon{}
	member := &routerFakeDaemon{}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": home, "host-b": member},
		RuntimeRouterOptions{PlacementStore: store},
	)

	if _, postErr := router.PostRoutedTownWall(context.Background(), "routed-flock-wall", TownWallPostRequest{AgentID: "worker-1", Body: "hello wall"}); postErr != nil {
		t.Fatalf("PostRoutedTownWall error = %v, want nil (delegates to home)", postErr)
	}
	if home.postWallCalls != 1 || home.postWallFlockID != "routed-flock-wall" || home.postWallReq.Body != "hello wall" {
		t.Fatalf("home post-wall calls/id/body = %d/%q/%q, want 1/routed-flock-wall/hello wall", home.postWallCalls, home.postWallFlockID, home.postWallReq.Body)
	}
	if member.postWallCalls != 0 {
		t.Fatalf("member post-wall calls = %d, want 0 (post goes only to home)", member.postWallCalls)
	}

	if _, historyErr := router.RoutedTownWallHistory(context.Background(), "routed-flock-wall"); historyErr != nil {
		t.Fatalf("RoutedTownWallHistory error = %v, want nil (delegates to home)", historyErr)
	}
	if home.historyCalls != 1 || home.historyFlockID != "routed-flock-wall" {
		t.Fatalf("home history calls/id = %d/%q, want 1/routed-flock-wall", home.historyCalls, home.historyFlockID)
	}
	if member.historyCalls != 0 {
		t.Fatalf("member history calls = %d, want 0 (history read only from home)", member.historyCalls)
	}
}

// TestRuntimeRouterRoutedFlockTownWallRejectsUnknownFlock keeps the not-found
// guard: delegation only applies to a wired routed flock.
func TestRuntimeRouterRoutedFlockTownWallRejectsUnknownFlock(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "placements.json"))
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": &routerFakeDaemon{}},
		RuntimeRouterOptions{PlacementStore: store},
	)
	if _, err := router.PostRoutedTownWall(context.Background(), "missing-flock", TownWallPostRequest{AgentID: "x", Body: "y"}); err == nil {
		t.Fatal("PostRoutedTownWall error = nil, want not-found error")
	}
	if _, err := router.RoutedTownWallHistory(context.Background(), "missing-flock"); err == nil {
		t.Fatal("RoutedTownWallHistory error = nil, want not-found error")
	}
}

func TestRuntimeRouterSpawnRecordsPlacementAndRoutesVMCalls(t *testing.T) {
	hostA := &routerFakeDaemon{}
	hostB := &routerFakeDaemon{}
	router := NewRuntimeRouter(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: false, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
	)

	resp, err := router.SpawnVM(context.Background(), SpawnVMRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}
	if resp.Host.Name != "host-b" {
		t.Fatalf("host = %q, want host-b", resp.Host.Name)
	}
	if hostA.spawnCalls != 0 || hostB.spawnCalls != 1 {
		t.Fatalf("spawn calls hostA/hostB = %d/%d, want 0/1", hostA.spawnCalls, hostB.spawnCalls)
	}
	if host, ok := router.Placement(resp.VMID); !ok || host != "host-b" {
		t.Fatalf("placement = %q,%v want host-b,true", host, ok)
	}

	if _, err := router.Health(context.Background(), resp.VMID); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if hostB.healthCalls != 1 || hostB.healthVMID != resp.VMID {
		t.Fatalf("host-b health = %d/%q, want 1/%q", hostB.healthCalls, hostB.healthVMID, resp.VMID)
	}
	if _, err := router.CreateSnapshot(context.Background(), resp.VMID, CreateSnapshotRequest{TenantID: "tenant-1"}); err != nil {
		t.Fatalf("CreateSnapshot returned error: %v", err)
	}
	if hostB.createSnapshotCalls != 1 || hostB.createSnapshotVMID != resp.VMID {
		t.Fatalf("host-b snapshot = %d/%q, want 1/%q", hostB.createSnapshotCalls, hostB.createSnapshotVMID, resp.VMID)
	}
}

func TestRuntimeRouterRestoreRecordsRestoredPlacement(t *testing.T) {
	daemon := &routerFakeDaemon{restoreResp: &RestoreSnapshotResponse{VMID: "vm-restored", GuestIP: "10.0.1.50", AgentURL: "http://10.0.1.50:8080", TenantID: "tenant-1", EgressPolicy: "profile", SourceSnapshotID: "snap-1"}}
	router := NewRuntimeRouter(
		NewScheduler([]RuntimeHost{{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}}, nil, nil),
		map[string]Daemon{"host-a": daemon},
	)

	resp, err := router.RestoreSnapshot(context.Background(), "snap-1", RestoreSnapshotRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, ScheduleRequest{TenantID: "tenant-1", EgressPolicy: EgressPolicyProfile}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}
	if resp.Host.Name != "host-a" {
		t.Fatalf("host = %q, want host-a", resp.Host.Name)
	}
	if daemon.restoreSnapshotCalls != 1 || daemon.restoreSnapshotID != "snap-1" {
		t.Fatalf("restore calls = %d/%q, want 1/snap-1", daemon.restoreSnapshotCalls, daemon.restoreSnapshotID)
	}
	if host, ok := router.Placement("vm-restored"); !ok || host != "host-a" {
		t.Fatalf("placement = %q,%v want host-a,true", host, ok)
	}
}

func TestRuntimeRouterRestorePrefersSnapshotLocalityHost(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.SetSnapshotLocation("snap-1", "host-b"); err != nil {
		t.Fatalf("SetSnapshotLocation: %v", err)
	}
	hostA := &routerFakeDaemon{}
	hostB := &routerFakeDaemon{restoreResp: &RestoreSnapshotResponse{VMID: "vm-restored", SourceSnapshotID: "snap-1", TenantID: "tenant-1", EgressPolicy: "profile"}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	resp, err := router.RestoreSnapshot(context.Background(), "snap-1", RestoreSnapshotRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, ScheduleRequest{TenantID: "tenant-1", EgressPolicy: EgressPolicyProfile}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("RestoreSnapshot returned error: %v", err)
	}
	if resp.Host.Name != "host-b" {
		t.Fatalf("restore host = %q, want locality host-b", resp.Host.Name)
	}
	if hostA.restoreSnapshotCalls != 0 || hostB.restoreSnapshotCalls != 1 {
		t.Fatalf("restore calls hostA/hostB = %d/%d, want 0/1", hostA.restoreSnapshotCalls, hostB.restoreSnapshotCalls)
	}
}

func TestRuntimeRouterSpawnRetriesOnNextEligibleHost(t *testing.T) {
	hostA := &routerFakeDaemon{spawnErr: errors.New("host-a unavailable")}
	hostB := &routerFakeDaemon{spawnResp: &SpawnVMResponse{VMID: "vm-b", TenantID: "tenant-1", EgressPolicy: "profile"}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 1 << 20, EgressPolicies: []EgressPolicy{EgressPolicyProfile}},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{MaxAttempts: 2},
	)

	resp, err := router.SpawnVM(context.Background(), SpawnVMRequest{TenantID: "tenant-1", EgressPolicy: "profile"}, TenantUsage{ActiveVMs: 1})
	if err != nil {
		t.Fatalf("SpawnVM returned error: %v", err)
	}
	if resp.Host.Name != "host-b" || resp.VMID != "vm-b" {
		t.Fatalf("resp = %+v, want host-b/vm-b", resp)
	}
	if hostA.spawnCalls != 1 || hostB.spawnCalls != 1 {
		t.Fatalf("spawn calls hostA/hostB = %d/%d, want 1/1", hostA.spawnCalls, hostB.spawnCalls)
	}
}

func TestRuntimeRouterReconcilePlacementsFromDaemonVMLists(t *testing.T) {
	store := NewPlacementStore("")
	if err := store.SetVMPlacement("stale-vm", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	hostA := &routerFakeDaemon{listVMResp: []VMInfo{{VMID: "vm-a"}}}
	hostB := &routerFakeDaemon{listVMResp: []VMInfo{{VMID: "vm-b"}}}
	router := NewRuntimeRouterWithOptions(
		NewScheduler(nil, nil, nil),
		map[string]Daemon{"host-a": hostA, "host-b": hostB},
		RuntimeRouterOptions{PlacementStore: store},
	)

	if err := router.ReconcilePlacements(context.Background()); err != nil {
		t.Fatalf("ReconcilePlacements: %v", err)
	}
	if _, ok := router.Placement("stale-vm"); ok {
		t.Fatal("stale-vm still has placement after reconcile")
	}
	if host, ok := router.Placement("vm-a"); !ok || host != "host-a" {
		t.Fatalf("vm-a placement = %q,%v want host-a,true", host, ok)
	}
	if host, ok := store.VMHost("vm-b"); !ok || host != "host-b" {
		t.Fatalf("store vm-b placement = %q,%v want host-b,true", host, ok)
	}
}

func TestRuntimeRouterReplicateSnapshotRecordsTargetLocation(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{}
	store := NewPlacementStore("")
	router := newSnapshotReplicationTestRouter(source, target, store, true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "replicated" {
		t.Fatalf("status = %q, want replicated", resp.Status)
	}
	if got := strings.Join(target.importCalls, ","); got != "bundle:snap-1" {
		t.Fatalf("target import calls = %q, want bundle:snap-1", got)
	}
	hosts := store.SnapshotHosts("snap-1")
	if got := strings.Join(hosts, ","); got != "host-b" {
		t.Fatalf("snapshot hosts = %q, want host-b", got)
	}
}

func TestRuntimeRouterReplicateDiffIncludesBaseFirst(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
		{SnapshotID: "snap-base", SnapshotType: "full"},
		{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
	}}
	target := &routerFakeDaemon{}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-diff",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if got := strings.Join(source.exportCalls, ","); got != "snap-base,snap-diff" {
		t.Fatalf("source export calls = %q, want snap-base,snap-diff", got)
	}
	if got := strings.Join(resp.Replicated, ","); got != "snap-base,snap-diff" {
		t.Fatalf("replicated = %q, want snap-base,snap-diff", got)
	}
	if resp.Status != "replicated" {
		t.Fatalf("status = %q, want replicated", resp.Status)
	}
}

func TestRuntimeRouterReplicateDiffValidatesListedBaseImport(t *testing.T) {
	t.Run("already present base is still imported and recorded", func(t *testing.T) {
		source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
			{SnapshotID: "snap-base", SnapshotType: "full"},
			{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
		}}
		target := &routerFakeDaemon{
			snapshotList:        []SnapshotInfo{{SnapshotID: "snap-base", SnapshotType: "full"}},
			importStatusForBody: map[string]string{"bundle:snap-base": "already_present"},
		}
		store := NewPlacementStore("")
		router := newSnapshotReplicationTestRouter(source, target, store, true, true)

		resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
			SnapshotID:          "snap-diff",
			SourceHost:          "host-a",
			TargetHost:          "host-b",
			IncludeDependencies: true,
		})
		if err != nil {
			t.Fatalf("ReplicateSnapshot returned error: %v", err)
		}
		if got := strings.Join(source.exportCalls, ","); got != "snap-base,snap-diff" {
			t.Fatalf("source export calls = %q, want snap-base,snap-diff", got)
		}
		if got := strings.Join(target.importCalls, ","); got != "bundle:snap-base,bundle:snap-diff" {
			t.Fatalf("target import calls = %q, want bundle:snap-base,bundle:snap-diff", got)
		}
		if got := strings.Join(resp.Skipped, ","); got != "snap-base" {
			t.Fatalf("skipped = %q, want snap-base", got)
		}
		if got := strings.Join(resp.Replicated, ","); got != "snap-diff" {
			t.Fatalf("replicated = %q, want snap-diff", got)
		}
		if got := strings.Join(store.SnapshotHosts("snap-base"), ","); got != "host-b" {
			t.Fatalf("base hosts = %q, want host-b", got)
		}
		if got := strings.Join(store.SnapshotHosts("snap-diff"), ","); got != "host-b" {
			t.Fatalf("diff hosts = %q, want host-b", got)
		}
	})

	t.Run("base conflict stops before diff transfer", func(t *testing.T) {
		source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
			{SnapshotID: "snap-base", SnapshotType: "full"},
			{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
		}}
		target := &routerFakeDaemon{
			snapshotList: []SnapshotInfo{{SnapshotID: "snap-base", SnapshotType: "full"}},
			importErrForBody: map[string]error{
				"bundle:snap-base": &DaemonError{StatusCode: 409, Body: "raw secret"},
			},
		}
		store := NewPlacementStore("")
		router := newSnapshotReplicationTestRouter(source, target, store, true, true)

		resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
			SnapshotID:          "snap-diff",
			SourceHost:          "host-a",
			TargetHost:          "host-b",
			IncludeDependencies: true,
		})
		if err != nil {
			t.Fatalf("ReplicateSnapshot returned error: %v", err)
		}
		if resp.Status != "failed" {
			t.Fatalf("status = %q, want failed", resp.Status)
		}
		if got := strings.Join(source.exportCalls, ","); got != "snap-base" {
			t.Fatalf("source export calls = %q, want snap-base", got)
		}
		if got := strings.Join(target.importCalls, ","); got != "bundle:snap-base" {
			t.Fatalf("target import calls = %q, want bundle:snap-base", got)
		}
		if len(resp.Errors) != 1 {
			t.Fatalf("errors = %+v, want one safe import error", resp.Errors)
		}
		errText := resp.Errors[0]
		for _, forbidden := range []string{"raw secret", "secret"} {
			if strings.Contains(errText, forbidden) {
				t.Fatalf("error %q contains forbidden raw body text %q", errText, forbidden)
			}
		}
		if !strings.Contains(errText, "import_failed") || !strings.Contains(errText, "status_code=409") {
			t.Fatalf("error = %q, want safe import failure with status code", errText)
		}
		if hosts := store.SnapshotHosts("snap-base"); len(hosts) != 0 {
			t.Fatalf("base hosts = %+v, want empty", hosts)
		}
		if hosts := store.SnapshotHosts("snap-diff"); len(hosts) != 0 {
			t.Fatalf("diff hosts = %+v, want empty", hosts)
		}
	})
}

func TestRuntimeRouterReplicateDiffWithoutDependencyFails(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"}}}
	target := &routerFakeDaemon{}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-diff",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: false,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "failed" {
		t.Fatalf("status = %q, want failed", resp.Status)
	}
	if len(resp.Errors) != 1 || resp.Errors[0] != "diff_base_missing" {
		t.Fatalf("errors = %+v, want [diff_base_missing]", resp.Errors)
	}
	if len(source.exportCalls) != 0 || len(target.importCalls) != 0 {
		t.Fatalf("export/import calls = %d/%d, want 0/0", len(source.exportCalls), len(target.importCalls))
	}
}

func TestRuntimeRouterReplicateDoesNotRecordFailedDiffLocation(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{
		{SnapshotID: "snap-base", SnapshotType: "full"},
		{SnapshotID: "snap-diff", SnapshotType: "diff", BaseSnapshotID: "snap-base"},
	}}
	target := &routerFakeDaemon{importErrForBody: map[string]error{"bundle:snap-diff": errors.New("import failed")}}
	store := NewPlacementStore("")
	router := newSnapshotReplicationTestRouter(source, target, store, true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-diff",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "partial" {
		t.Fatalf("status = %q, want partial", resp.Status)
	}
	if got := strings.Join(store.SnapshotHosts("snap-base"), ","); got != "host-b" {
		t.Fatalf("base hosts = %q, want host-b", got)
	}
	if hosts := store.SnapshotHosts("snap-diff"); len(hosts) != 0 {
		t.Fatalf("diff hosts = %+v, want empty", hosts)
	}
}

func TestRuntimeRouterReplicateRejectsUnhealthyHostBeforeDaemonCall(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), false, true)

	_, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err == nil || err.Error() != "source_host_unavailable" {
		t.Fatalf("error = %v, want source_host_unavailable", err)
	}
	if len(source.exportCalls) != 0 || len(target.importCalls) != 0 {
		t.Fatalf("export/import calls = %d/%d, want 0/0", len(source.exportCalls), len(target.importCalls))
	}
}

func TestRuntimeRouterReplicateScrubsImportDaemonErrorBody(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{importErrForBody: map[string]error{
		"bundle:snap-1": &DaemonError{StatusCode: 409, Body: "agent_token secret raw body"},
	}}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "failed" {
		t.Fatalf("status = %q, want failed", resp.Status)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %+v, want one safe import error", resp.Errors)
	}
	errText := resp.Errors[0]
	for _, forbidden := range []string{"agent_token", "secret", "raw body"} {
		if strings.Contains(errText, forbidden) {
			t.Fatalf("error %q contains forbidden raw body text %q", errText, forbidden)
		}
	}
	if !strings.Contains(errText, "import_failed") || !strings.Contains(errText, "status_code=409") {
		t.Fatalf("error = %q, want safe import failure with status code", errText)
	}
}

func TestRuntimeRouterReplicateDoesNotPreSkipTargetListPresence(t *testing.T) {
	t.Run("already present import records skipped locality", func(t *testing.T) {
		source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
		target := &routerFakeDaemon{
			snapshotList:        []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}},
			importStatusForBody: map[string]string{"bundle:snap-1": "already_present"},
		}
		store := NewPlacementStore("")
		router := newSnapshotReplicationTestRouter(source, target, store, true, true)

		resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
			SnapshotID:          "snap-1",
			SourceHost:          "host-a",
			TargetHost:          "host-b",
			IncludeDependencies: true,
		})
		if err != nil {
			t.Fatalf("ReplicateSnapshot returned error: %v", err)
		}
		if got := strings.Join(source.exportCalls, ","); got != "snap-1" {
			t.Fatalf("source export calls = %q, want snap-1", got)
		}
		if got := strings.Join(target.importCalls, ","); got != "bundle:snap-1" {
			t.Fatalf("target import calls = %q, want bundle:snap-1", got)
		}
		if got := strings.Join(resp.Skipped, ","); got != "snap-1" {
			t.Fatalf("skipped = %q, want snap-1", got)
		}
		if got := strings.Join(store.SnapshotHosts("snap-1"), ","); got != "host-b" {
			t.Fatalf("snapshot hosts = %q, want host-b", got)
		}
	})

	t.Run("conflict import does not record locality", func(t *testing.T) {
		source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
		target := &routerFakeDaemon{
			snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}},
			importErrForBody: map[string]error{
				"bundle:snap-1": &DaemonError{StatusCode: 409, Body: "conflict raw daemon body"},
			},
		}
		store := NewPlacementStore("")
		router := newSnapshotReplicationTestRouter(source, target, store, true, true)

		resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
			SnapshotID:          "snap-1",
			SourceHost:          "host-a",
			TargetHost:          "host-b",
			IncludeDependencies: true,
		})
		if err != nil {
			t.Fatalf("ReplicateSnapshot returned error: %v", err)
		}
		if resp.Status != "failed" {
			t.Fatalf("status = %q, want failed", resp.Status)
		}
		if got := strings.Join(source.exportCalls, ","); got != "snap-1" {
			t.Fatalf("source export calls = %q, want snap-1", got)
		}
		if got := strings.Join(target.importCalls, ","); got != "bundle:snap-1" {
			t.Fatalf("target import calls = %q, want bundle:snap-1", got)
		}
		if hosts := store.SnapshotHosts("snap-1"); len(hosts) != 0 {
			t.Fatalf("snapshot hosts = %+v, want empty", hosts)
		}
	})
}

func TestRuntimeRouterReplicateClosesExportStreamOnImportFailure(t *testing.T) {
	body := &routerTrackingReadCloser{Reader: strings.NewReader("bundle:snap-1")}
	source := &routerFakeDaemon{
		snapshotList:  []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}},
		exportStreams: map[string]io.ReadCloser{"snap-1": body},
	}
	target := &routerFakeDaemon{importErrForBody: map[string]error{"bundle:snap-1": errors.New("import failed")}}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "failed" {
		t.Fatalf("status = %q, want failed", resp.Status)
	}
	if !body.closed {
		t.Fatal("export stream was not closed after import failure")
	}
}

func TestRuntimeRouterReplicateScrubsSourceListDaemonErrorBody(t *testing.T) {
	source := &routerFakeDaemon{listSnapshotErr: &DaemonError{StatusCode: 500, Body: "agent_token secret raw body"}}
	target := &routerFakeDaemon{}
	router := newSnapshotReplicationTestRouter(source, target, NewPlacementStore(""), true, true)

	_, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err == nil {
		t.Fatal("ReplicateSnapshot error = nil, want safe source list error")
	}
	errText := err.Error()
	for _, forbidden := range []string{"agent_token", "secret", "raw body"} {
		if strings.Contains(errText, forbidden) {
			t.Fatalf("error %q contains forbidden raw body text %q", errText, forbidden)
		}
	}
	if !strings.Contains(errText, "source_list_failed") || !strings.Contains(errText, "status_code=500") {
		t.Fatalf("error = %q, want safe source list failure with status code", errText)
	}
}

func TestRuntimeRouterReplicateReportsTransferWhenPlacementSaveFails(t *testing.T) {
	source := &routerFakeDaemon{snapshotList: []SnapshotInfo{{SnapshotID: "snap-1", SnapshotType: "full"}}}
	target := &routerFakeDaemon{}
	store := NewPlacementStore(t.TempDir())
	router := newSnapshotReplicationTestRouter(source, target, store, true, true)

	resp, err := router.ReplicateSnapshot(context.Background(), SnapshotReplicationRequest{
		SnapshotID:          "snap-1",
		SourceHost:          "host-a",
		TargetHost:          "host-b",
		IncludeDependencies: true,
	})
	if err != nil {
		t.Fatalf("ReplicateSnapshot returned error: %v", err)
	}
	if resp.Status != "partial" {
		t.Fatalf("status = %q, want partial", resp.Status)
	}
	if got := strings.Join(resp.Replicated, ","); got != "snap-1" {
		t.Fatalf("replicated = %q, want snap-1", got)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "record_location_failed") {
		t.Fatalf("errors = %+v, want record_location_failed", resp.Errors)
	}
}

type routerTrackingReadCloser struct {
	*strings.Reader
	closed bool
}

func (r *routerTrackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func newSnapshotReplicationTestRouter(source, target *routerFakeDaemon, store *PlacementStore, sourceHealthy, targetHealthy bool) *RuntimeRouter {
	return NewRuntimeRouterWithOptions(
		NewScheduler(
			[]RuntimeHost{
				{Name: "host-a", Endpoint: "http://host-a", Healthy: sourceHealthy, AvailableVMs: 1},
				{Name: "host-b", Endpoint: "http://host-b", Healthy: targetHealthy, AvailableVMs: 1},
			},
			nil,
			nil,
		),
		map[string]Daemon{"host-a": source, "host-b": target},
		RuntimeRouterOptions{PlacementStore: store},
	)
}
