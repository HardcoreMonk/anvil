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
