package anvilmcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

type SchedulerServiceOptions struct {
	PlacementStore *PlacementStore
	QuotaStore     *QuotaStore
}

type SchedulerService struct {
	placements *PlacementStore
	quotas     *QuotaStore
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
	return &SchedulerService{placements: placements, quotas: quotas}
}

func (s *SchedulerService) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/hosts", s.handleHosts)
	mux.HandleFunc("/hosts/", s.handleHostItem)
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
		previous, existed := s.placements.Host(host.Name)
		if err := s.placements.SetHost(host); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.placements.Save(); err != nil {
			if existed {
				_ = s.placements.SetHost(previous)
			} else {
				s.placements.RemoveHost(host.Name)
			}
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
	previous, existed := s.placements.Host(name)
	if !existed {
		writeSchedulerJSON(w, map[string]any{"deleted": false, "host": name})
		return
	}
	deleted := s.placements.RemoveHost(name)
	if err := s.placements.Save(); err != nil {
		_ = s.placements.SetHost(previous)
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
	var req schedulerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid schedule body", http.StatusBadRequest)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/restore") && len(req.PreferredHosts) == 0 {
		req.PreferredHosts = s.placements.SnapshotHosts(strings.TrimSpace(r.URL.Query().Get("snapshot_id")))
	}
	quotas, usage := s.quotas.SchedulerInputs()
	decision, err := NewScheduler(s.placements.ListHosts(), quotas, usage).Schedule(req.ScheduleRequest, req.Requested)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeSchedulerJSON(w, decision)
}

func writeSchedulerJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
