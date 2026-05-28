package anvilmcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type SchedulerServiceOptions struct {
	PlacementStore     *PlacementStore
	QuotaStore         *QuotaStore
	RequirePersistence bool
}

type SchedulerService struct {
	placements         *PlacementStore
	quotas             *QuotaStore
	requirePersistence bool
}

type schedulerRequest struct {
	ScheduleRequest
	Requested TenantUsage `json:"requested"`
}

func NewSchedulerService(opts SchedulerServiceOptions) *SchedulerService {
	placements := opts.PlacementStore
	if placements == nil {
		placements = NewPlacementStore("")
	}
	quotas := opts.QuotaStore
	if quotas == nil {
		quotas = NewQuotaStore("")
	}
	return &SchedulerService{placements: placements, quotas: quotas, requirePersistence: opts.RequirePersistence}
}

func (s *SchedulerService) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/hosts", s.handleHosts)
	mux.HandleFunc("/hosts/", s.handleHostItem)
	mux.HandleFunc("/control-loop/status", s.handleControlLoopStatus)
	mux.HandleFunc("/placements", s.handlePlacements)
	mux.HandleFunc("/reconcile", s.handleReconcile)
	mux.HandleFunc("/schedule/spawn", s.handleSchedule)
	mux.HandleFunc("/schedule/restore", s.handleSchedule)
	return mux
}

func (s *SchedulerService) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	writeSchedulerJSON(w, map[string]string{"status": "ok"})
}

func (s *SchedulerService) handleHosts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeSchedulerJSON(w, s.placements.ListHosts())
	case http.MethodPut:
		var host RuntimeHost
		if err := json.NewDecoder(r.Body).Decode(&host); err != nil {
			http.Error(w, "invalid host body", http.StatusBadRequest)
			return
		}
		host.Name = strings.TrimSpace(host.Name)
		if host.Name == "" {
			http.Error(w, "host name must be non-empty", http.StatusBadRequest)
			return
		}
		if err := s.placements.SetHostAndSave(host); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeSchedulerJSON(w, host)
	default:
		http.Error(w, "GET or PUT required", http.StatusMethodNotAllowed)
	}
}

func (s *SchedulerService) handleHostItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	name, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/hosts/"))
	if err != nil || strings.TrimSpace(name) == "" {
		http.Error(w, "host name must be non-empty", http.StatusBadRequest)
		return
	}
	name = strings.TrimSpace(name)
	if s.placements.IsConfigManagedHost(name) {
		http.Error(w, "config-managed host must be removed from hosts file", http.StatusConflict)
		return
	}
	deleted, err := s.placements.RemoveHostAndSave(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeSchedulerJSON(w, map[string]any{"deleted": deleted, "host": name})
}

func (s *SchedulerService) handlePlacements(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	writeSchedulerJSON(w, s.placements.State())
}

func (s *SchedulerService) handleControlLoopStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	writeSchedulerJSON(w, s.placements.State().ControlLoopStatus)
}

func (s *SchedulerService) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	writeSchedulerJSON(w, s.placements.State())
}

func (s *SchedulerService) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.requirePersistence && s.placements.State().ControlLoopStatus.PersistenceDegraded {
		http.Error(w, "scheduler persistence degraded", http.StatusServiceUnavailable)
		return
	}
	var req schedulerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid schedule body", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/restore") && len(req.PreferredHosts) == 0 {
		req.PreferredHosts = s.placements.SnapshotHosts(strings.TrimSpace(r.URL.Query().Get("snapshot_id")))
	}
	state := s.placements.State()
	hosts := runtimeHostsFromPlacementState(state)
	quotas, usage := s.quotas.SchedulerInputs()
	decision, err := NewScheduler(hosts, quotas, usage).Schedule(req.ScheduleRequest, req.Requested)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	decision.HostStatusSummary = SummarizeHostStatuses(hosts, state.HostObservations)
	writeSchedulerJSON(w, decision)
}

func runtimeHostsFromPlacementState(state PlacementStoreState) []RuntimeHost {
	names := make([]string, 0, len(state.Hosts))
	for name := range state.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	hosts := make([]RuntimeHost, 0, len(names))
	for _, name := range names {
		hosts = append(hosts, state.Hosts[name])
	}
	return hosts
}

func writeSchedulerJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
