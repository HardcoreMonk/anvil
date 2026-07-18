package anvilmcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchedulerServiceListsPersistentHostsAndSchedulesSpawn(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler", "state.json"))
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, AvailableSnapshotBytes: 4096, EgressPolicies: []EgressPolicy{EgressPolicyProfile}}); err != nil {
		t.Fatalf("SetHost: %v", err)
	}
	quota := NewQuotaStore(filepath.Join(t.TempDir(), "tenants.json"))
	if err := quota.SetTenantQuota("tenant-1", TenantQuota{ActiveVMs: 2}); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, QuotaStore: quota})

	hostReq := httptest.NewRequest(http.MethodGet, "/hosts", nil)
	hostRR := httptest.NewRecorder()
	service.Handler().ServeHTTP(hostRR, hostReq)
	if hostRR.Code != http.StatusOK {
		t.Fatalf("GET /hosts status = %d body=%s, want 200", hostRR.Code, hostRR.Body.String())
	}
	var hosts []RuntimeHost
	if err := json.Unmarshal(hostRR.Body.Bytes(), &hosts); err != nil {
		t.Fatalf("decode hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Name != "host-a" {
		t.Fatalf("hosts = %+v, want host-a", hosts)
	}

	scheduleReq := httptest.NewRequest(http.MethodPost, "/schedule/spawn", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
	scheduleRR := httptest.NewRecorder()
	service.Handler().ServeHTTP(scheduleRR, scheduleReq)
	if scheduleRR.Code != http.StatusOK {
		t.Fatalf("POST /schedule/spawn status = %d body=%s, want 200", scheduleRR.Code, scheduleRR.Body.String())
	}
	var decision ScheduleDecision
	if err := json.Unmarshal(scheduleRR.Body.Bytes(), &decision); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if !decision.Allowed || decision.Host.Name != "host-a" {
		t.Fatalf("decision = %+v, want scheduled host-a", decision)
	}
}

func TestSchedulerServiceDeletesPersistentHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler", "state.json")
	store := NewPlacementStore(path)
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-a: %v", err)
	}
	if err := store.SetHost(RuntimeHost{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-b: %v", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	deleteReq := httptest.NewRequest(http.MethodDelete, "/hosts/host-a", nil)
	deleteRR := httptest.NewRecorder()
	service.Handler().ServeHTTP(deleteRR, deleteReq)
	if deleteRR.Code != http.StatusOK {
		t.Fatalf("DELETE /hosts/host-a status = %d body=%s, want 200", deleteRR.Code, deleteRR.Body.String())
	}
	var response struct {
		Deleted bool   `json:"deleted"`
		Host    string `json:"host"`
	}
	if err := json.Unmarshal(deleteRR.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if !response.Deleted || response.Host != "host-a" {
		t.Fatalf("delete response = %+v, want deleted host-a", response)
	}

	hostReq := httptest.NewRequest(http.MethodGet, "/hosts", nil)
	hostRR := httptest.NewRecorder()
	service.Handler().ServeHTTP(hostRR, hostReq)
	if hostRR.Code != http.StatusOK {
		t.Fatalf("GET /hosts status = %d body=%s, want 200", hostRR.Code, hostRR.Body.String())
	}
	var hosts []RuntimeHost
	if err := json.Unmarshal(hostRR.Body.Bytes(), &hosts); err != nil {
		t.Fatalf("decode hosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Name != "host-b" {
		t.Fatalf("hosts after delete = %+v, want only host-b", hosts)
	}

	reloaded := NewPlacementStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload placement store: %v", err)
	}
	reloadedHosts := reloaded.ListHosts()
	if len(reloadedHosts) != 1 || reloadedHosts[0].Name != "host-b" {
		t.Fatalf("persisted hosts after delete = %+v, want only host-b", reloadedHosts)
	}
}

func TestSchedulerServicePutHostSaveFailureRollsBackNewHost(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodPut, "/hosts", strings.NewReader(`{"name":"host-new","endpoint":"http://host-new","healthy":true,"available_vms":1}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("PUT /hosts status = %d body=%s, want 500", rr.Code, rr.Body.String())
	}

	hosts := schedulerServiceListHosts(t, service)
	if len(hosts) != 0 {
		t.Fatalf("hosts after failed new host save = %+v, want empty", hosts)
	}
}

func TestSchedulerServicePutHostSaveFailureRestoresExistingHost(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://old-host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost old host-a: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodPut, "/hosts", strings.NewReader(`{"name":"host-a","endpoint":"http://new-host-a","healthy":true,"available_vms":9}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("PUT /hosts status = %d body=%s, want 500", rr.Code, rr.Body.String())
	}

	hosts := schedulerServiceListHosts(t, service)
	if len(hosts) != 1 {
		t.Fatalf("hosts after failed replacement save = %+v, want one host", hosts)
	}
	if hosts[0].Name != "host-a" || hosts[0].Endpoint != "http://old-host-a" || hosts[0].AvailableVMs != 1 {
		t.Fatalf("host after failed replacement save = %+v, want old host-a", hosts[0])
	}
}

func TestSchedulerServiceDeleteMissingHostDoesNotRequireSave(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodDelete, "/hosts/missing", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE /hosts/missing status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var response struct {
		Deleted bool   `json:"deleted"`
		Host    string `json:"host"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if response.Deleted || response.Host != "missing" {
		t.Fatalf("delete response = %+v, want deleted=false host=missing", response)
	}
}

func TestSchedulerServiceDeleteHostSaveFailureRestoresHost(t *testing.T) {
	store := NewPlacementStore(t.TempDir())
	if err := store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1}); err != nil {
		t.Fatalf("SetHost host-a: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodDelete, "/hosts/host-a", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE /hosts/host-a status = %d body=%s, want 500", rr.Code, rr.Body.String())
	}

	hosts := schedulerServiceListHosts(t, service)
	if len(hosts) != 1 || hosts[0].Name != "host-a" || hosts[0].Endpoint != "http://host-a" {
		t.Fatalf("hosts after failed delete save = %+v, want restored host-a", hosts)
	}
}

func TestSchedulerServiceRejectsWrongMethodsOnHostItem(t *testing.T) {
	service := NewSchedulerService(SchedulerServiceOptions{})

	req := httptest.NewRequest(http.MethodPost, "/hosts/host-a", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /hosts/host-a status = %d body=%s, want 405", rr.Code, rr.Body.String())
	}
}

func TestSchedulerServiceReconcileReturnsPlacementSnapshot(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler", "state.json"))
	if err := store.SetVMPlacement("vm-1", "host-a"); err != nil {
		t.Fatalf("SetVMPlacement: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodPost, "/reconcile", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /reconcile status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var state PlacementStoreState
	if err := json.Unmarshal(rr.Body.Bytes(), &state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.VMPlacements["vm-1"] != "host-a" {
		t.Fatalf("state = %+v, want vm-1 on host-a", state)
	}
}

func TestSchedulerServiceControlLoopStatusEndpoint(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusUnhealthy, FailureCount: 3, LastError: "health returned 503"})
	_ = store.SetControlLoopStatus(ControlLoopStatus{Running: true, PollIntervalSeconds: 10, ReconcileIntervalSeconds: 30, FailureThreshold: 3, Hosts: map[string]HostObservation{"host-a": {Status: HostStatusUnhealthy, FailureCount: 3}}})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodGet, "/control-loop/status", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /control-loop/status status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var status ControlLoopStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Running || status.Hosts["host-a"].Status != HostStatusUnhealthy {
		t.Fatalf("status = %+v", status)
	}
}

func TestSchedulerServiceControlLoopStatusRejectsNonGET(t *testing.T) {
	service := NewSchedulerService(SchedulerServiceOptions{})

	req := httptest.NewRequest(http.MethodPost, "/control-loop/status", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /control-loop/status status = %d body=%s, want 405", rr.Code, rr.Body.String())
	}
}

func TestSchedulerServiceRejectsDeleteConfigManagedHost(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1})
	_ = store.MarkConfigManagedHost("host-a", true)
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodDelete, "/hosts/host-a", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("DELETE config-managed host status = %d body=%s, want 409", rr.Code, rr.Body.String())
	}
}

func TestSchedulerServiceRejectsDeleteConfigManagedHostWithEncodedWhitespace(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1})
	_ = store.MarkConfigManagedHost("host-a", true)
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	for _, path := range []string{"/hosts/%20host-a%20", "/hosts/host-a%20"} {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		rr := httptest.NewRecorder()
		service.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusConflict {
			t.Fatalf("DELETE %s status = %d body=%s, want 409", path, rr.Code, rr.Body.String())
		}
	}
}

func TestSchedulerServiceScheduleIncludesHostStatusSummary(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: false, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusDegraded, FailureCount: 1})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodPost, "/schedule/spawn", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /schedule/spawn status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var decision ScheduleDecision
	if err := json.Unmarshal(rr.Body.Bytes(), &decision); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if decision.Allowed || decision.HostStatusSummary.Degraded != 1 {
		t.Fatalf("decision = %+v, want denied with degraded host summary", decision)
	}
}

func TestSchedulerServiceAllowedScheduleIncludesHealthyHostStatusSummary(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusHealthy})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store})

	req := httptest.NewRequest(http.MethodPost, "/schedule/spawn", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /schedule/spawn status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var decision ScheduleDecision
	if err := json.Unmarshal(rr.Body.Bytes(), &decision); err != nil {
		t.Fatalf("decode decision: %v", err)
	}
	if !decision.Allowed || decision.HostStatusSummary.Healthy != 1 {
		t.Fatalf("decision = %+v, want allowed with healthy host summary", decision)
	}
}

func TestSchedulerServicePersistenceGateAllowsSchedulingWhenNotRequiredOrNotDegraded(t *testing.T) {
	tests := []struct {
		name                string
		requirePersistence  bool
		persistenceDegraded bool
	}{
		{
			name:                "not required with degraded persistence",
			requirePersistence:  false,
			persistenceDegraded: true,
		},
		{
			name:                "required with healthy persistence",
			requirePersistence:  true,
			persistenceDegraded: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
			_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
			_ = store.SetControlLoopStatus(ControlLoopStatus{PersistenceDegraded: tt.persistenceDegraded})
			service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: tt.requirePersistence})

			req := httptest.NewRequest(http.MethodPost, "/schedule/spawn", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
			rr := httptest.NewRecorder()
			service.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("POST /schedule/spawn status = %d body=%s, want 200", rr.Code, rr.Body.String())
			}
			var decision ScheduleDecision
			if err := json.Unmarshal(rr.Body.Bytes(), &decision); err != nil {
				t.Fatalf("decode decision: %v", err)
			}
			if !decision.Allowed || decision.Host.Name != "host-a" {
				t.Fatalf("decision = %+v, want scheduled host-a", decision)
			}
		})
	}
}

func TestSchedulerServiceRequiresPersistenceWhenConfigured(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetControlLoopStatus(ControlLoopStatus{PersistenceDegraded: true, LastError: "save failed"})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: true})

	req := httptest.NewRequest(http.MethodPost, "/schedule/spawn", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","requested":{"active_vms":1}}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /schedule/spawn status = %d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "scheduler persistence degraded") {
		t.Fatalf("body = %q, want persistence degraded message", rr.Body.String())
	}
}

func TestSchedulerServiceScheduleMethodValidationPrecedesPersistenceGate(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetControlLoopStatus(ControlLoopStatus{PersistenceDegraded: true, LastError: "save failed"})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: true})

	req := httptest.NewRequest(http.MethodGet, "/schedule/spawn", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /schedule/spawn status = %d body=%s, want 405", rr.Code, rr.Body.String())
	}
}

func TestSchedulerServicePersistenceGateDoesNotBlockControlLoopStatus(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetControlLoopStatus(ControlLoopStatus{Running: true, PersistenceDegraded: true, LastError: "save failed"})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: true})

	req := httptest.NewRequest(http.MethodGet, "/control-loop/status", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /control-loop/status status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var status ControlLoopStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.PersistenceDegraded {
		t.Fatalf("status = %+v, want PersistenceDegraded=true", status)
	}
}

func TestSchedulerServiceSchedulesFlockAcrossHosts(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetHost(RuntimeHost{Name: "host-b", Endpoint: "http://host-b", Healthy: true, AvailableVMs: 1, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	_ = store.SetHostObservation("host-a", HostObservation{Status: HostStatusHealthy})
	_ = store.SetHostObservation("host-b", HostObservation{Status: HostStatusHealthy})
	quota := NewQuotaStore(filepath.Join(t.TempDir(), "tenants.json"))
	if err := quota.SetTenantQuota("tenant-1", TenantQuota{ActiveVMs: 2}); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, QuotaStore: quota})

	req := httptest.NewRequest(http.MethodPost, "/schedule/flock", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","roles":["worker","reviewer"]}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /schedule/flock status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var plan FlockPlacementPlan
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if !plan.Allowed || len(plan.Agents) != 2 {
		t.Fatalf("plan = %+v, want allowed two-agent plan", plan)
	}
	if plan.Agents[0].Host.Name != "host-a" || plan.Agents[1].Host.Name != "host-b" {
		t.Fatalf("agent hosts = %q/%q, want host-a/host-b", plan.Agents[0].Host.Name, plan.Agents[1].Host.Name)
	}
	if plan.HostStatusSummary.Healthy != 2 {
		t.Fatalf("host status summary = %+v, want two healthy hosts", plan.HostStatusSummary)
	}
}

func TestSchedulerServiceFlockScheduleDeniesQuota(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetHost(RuntimeHost{Name: "host-a", Endpoint: "http://host-a", Healthy: true, AvailableVMs: 2, EgressPolicies: []EgressPolicy{EgressPolicyProfile}})
	quota := NewQuotaStore(filepath.Join(t.TempDir(), "tenants.json"))
	if err := quota.SetTenantQuota("tenant-1", TenantQuota{ActiveVMs: 1}); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, QuotaStore: quota})

	req := httptest.NewRequest(http.MethodPost, "/schedule/flock", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","roles":["worker","reviewer"]}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /schedule/flock status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var plan FlockPlacementPlan
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if plan.Allowed || plan.Reason != FlockPlacementReasonQuotaExceeded {
		t.Fatalf("plan = %+v, want quota denial", plan)
	}
}

func TestSchedulerServiceFlockScheduleRejectsWrongMethodBeforePersistenceGate(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetControlLoopStatus(ControlLoopStatus{PersistenceDegraded: true})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: true})

	req := httptest.NewRequest(http.MethodGet, "/schedule/flock", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /schedule/flock status = %d body=%s, want 405", rr.Code, rr.Body.String())
	}
}

func TestSchedulerServiceFlockScheduleRequiresPersistenceWhenConfigured(t *testing.T) {
	store := NewPlacementStore(filepath.Join(t.TempDir(), "scheduler.json"))
	_ = store.SetControlLoopStatus(ControlLoopStatus{PersistenceDegraded: true, LastError: "save failed"})
	service := NewSchedulerService(SchedulerServiceOptions{PlacementStore: store, RequirePersistence: true})

	req := httptest.NewRequest(http.MethodPost, "/schedule/flock", strings.NewReader(`{"tenant_id":"tenant-1","egress_policy":"profile","roles":["worker"]}`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /schedule/flock status = %d body=%s, want 503", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "scheduler persistence degraded") {
		t.Fatalf("body = %q, want persistence degraded message", rr.Body.String())
	}
}

func TestSchedulerServiceFlockScheduleRejectsInvalidBody(t *testing.T) {
	service := NewSchedulerService(SchedulerServiceOptions{})

	req := httptest.NewRequest(http.MethodPost, "/schedule/flock", strings.NewReader(`{`))
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /schedule/flock invalid body status = %d body=%s, want 400", rr.Code, rr.Body.String())
	}
}

func schedulerServiceListHosts(t *testing.T, service *SchedulerService) []RuntimeHost {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/hosts", nil)
	rr := httptest.NewRecorder()
	service.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /hosts status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
	var hosts []RuntimeHost
	if err := json.Unmarshal(rr.Body.Bytes(), &hosts); err != nil {
		t.Fatalf("decode hosts: %v", err)
	}
	return hosts
}

func TestSchedulerHandleMetrics_IncludesQuota(t *testing.T) {
	// Seed a disk-backed quota store, then Save() — handleMetrics reloads from disk
	// (the daemon writes this file out-of-process), so the metrics must reflect it.
	dir := t.TempDir()
	q := NewQuotaStore(filepath.Join(dir, "quota.json"))
	if err := q.SetTenantQuota("tenant.alpha", TenantQuota{SnapshotBytes: 100}); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	if err := q.UpdateTenantUsage("tenant.alpha", TenantUsage{SnapshotBytes: 95}); err != nil {
		t.Fatalf("UpdateTenantUsage: %v", err)
	}
	if err := q.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	svc := NewSchedulerService(SchedulerServiceOptions{QuotaStore: q})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	svc.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	wantLines := []string{
		`anvil_scheduler_quota_usage_total{resource="snapshot_bytes"} 95`,
		`anvil_scheduler_quota_limit_total{resource="snapshot_bytes"} 100`,
		`anvil_scheduler_quota_tenants_near{resource="snapshot_bytes"} 1`,
		"anvil_scheduler_quota_tenants_total 1",
		"anvil_scheduler_control_loop_running", // existing scheduler metric still present
	}
	for _, w := range wantLines {
		if !strings.Contains(body, w) {
			t.Errorf("missing line %q in:\n%s", w, body)
		}
	}
}
