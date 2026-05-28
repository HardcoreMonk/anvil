package anvilmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type VMInfo struct {
	VMID         string `json:"vm_id"`
	GuestIP      string `json:"guest_ip"`
	AgentURL     string `json:"agent_url"`
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
}

type HostStatus string

const (
	HostStatusHealthy   HostStatus = "healthy"
	HostStatusDegraded  HostStatus = "degraded"
	HostStatusUnhealthy HostStatus = "unhealthy"
)

type HostObservation struct {
	Status                 HostStatus `json:"status"`
	AvailableVMs           int64      `json:"available_vms"`
	AvailableSnapshotBytes int64      `json:"available_snapshot_bytes"`
	FailureCount           int        `json:"failure_count"`
	LastSuccessAt          time.Time  `json:"last_success_at,omitempty"`
	LastFailureAt          time.Time  `json:"last_failure_at,omitempty"`
	LastError              string     `json:"last_error,omitempty"`
}

type SuspectVMPlacement struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

type ControlLoopStatus struct {
	Running                  bool                       `json:"running"`
	PollIntervalSeconds      int64                      `json:"poll_interval_seconds"`
	ReconcileIntervalSeconds int64                      `json:"reconcile_interval_seconds"`
	FailureThreshold         int                        `json:"failure_threshold"`
	PersistenceDegraded      bool                       `json:"persistence_degraded"`
	LastPollStartedAt        time.Time                  `json:"last_poll_started_at,omitempty"`
	LastPollCompletedAt      time.Time                  `json:"last_poll_completed_at,omitempty"`
	LastReconcileStartedAt   time.Time                  `json:"last_reconcile_started_at,omitempty"`
	LastReconcileCompletedAt time.Time                  `json:"last_reconcile_completed_at,omitempty"`
	LastError                string                     `json:"last_error,omitempty"`
	Hosts                    map[string]HostObservation `json:"hosts,omitempty"`
}

type PlacementStoreState struct {
	Hosts               map[string]RuntimeHost        `json:"hosts"`
	VMPlacements        map[string]string             `json:"vm_placements"`
	SnapshotLocations   map[string][]string           `json:"snapshot_locations"`
	ConfigManagedHosts  map[string]bool               `json:"config_managed_hosts,omitempty"`
	HostObservations    map[string]HostObservation    `json:"host_observations,omitempty"`
	SuspectVMPlacements map[string]SuspectVMPlacement `json:"suspect_vm_placements,omitempty"`
	ControlLoopStatus   ControlLoopStatus             `json:"control_loop_status,omitempty"`
}

type PlacementStore struct {
	mu    sync.RWMutex
	path  string
	state PlacementStoreState
}

func NewPlacementStore(path string) *PlacementStore {
	return &PlacementStore{
		path: path,
		state: PlacementStoreState{
			Hosts:               make(map[string]RuntimeHost),
			VMPlacements:        make(map[string]string),
			SnapshotLocations:   make(map[string][]string),
			ConfigManagedHosts:  make(map[string]bool),
			HostObservations:    make(map[string]HostObservation),
			SuspectVMPlacements: make(map[string]SuspectVMPlacement),
			ControlLoopStatus: ControlLoopStatus{
				Hosts: make(map[string]HostObservation),
			},
		},
	}
}

func (s *PlacementStore) Load() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			s.ensureMaps()
			s.mu.Unlock()
			return nil
		}
		return fmt.Errorf("read placement store: %w", err)
	}
	var state PlacementStoreState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse placement store: %w", err)
	}
	normalizePlacementStoreState(&state)
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
	return nil
}

func (s *PlacementStore) Save() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	s.mu.RLock()
	state := clonePlacementStoreState(s.state)
	s.mu.RUnlock()
	return writePlacementStoreState(s.path, state)
}

func writePlacementStoreState(path string, state PlacementStoreState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode placement store: %w", err)
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create placement store dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".placement-*.json")
	if err != nil {
		return fmt.Errorf("create placement store temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write placement store temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod placement store temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close placement store temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replace placement store: %w", err)
	}
	return nil
}

func (s *PlacementStore) saveLocked() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	return writePlacementStoreState(s.path, clonePlacementStoreState(s.state))
}

func (s *PlacementStore) SetHost(host RuntimeHost) error {
	name := strings.TrimSpace(host.Name)
	if name == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	host.Name = name
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	s.state.Hosts[name] = host
	return nil
}

func (s *PlacementStore) SetHostAndSave(host RuntimeHost) error {
	name := strings.TrimSpace(host.Name)
	if name == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	host.Name = name
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	previous, existed := s.state.Hosts[name]
	s.state.Hosts[name] = host
	if err := s.saveLocked(); err != nil {
		if existed {
			s.state.Hosts[name] = previous
		} else {
			delete(s.state.Hosts, name)
		}
		return err
	}
	return nil
}

func (s *PlacementStore) MarkConfigManagedHost(name string, managed bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	if managed {
		s.state.ConfigManagedHosts[name] = true
	} else {
		delete(s.state.ConfigManagedHosts, name)
	}
	return nil
}

func (s *PlacementStore) IsConfigManagedHost(name string) bool {
	name = strings.TrimSpace(name)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.ConfigManagedHosts[name]
}

func (s *PlacementStore) RemoveHost(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	if _, ok := s.state.Hosts[name]; !ok {
		return false
	}
	delete(s.state.Hosts, name)
	return true
}

func (s *PlacementStore) RemoveHostAndSave(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	previous, existed := s.state.Hosts[name]
	if !existed {
		return false, nil
	}
	delete(s.state.Hosts, name)
	if err := s.saveLocked(); err != nil {
		s.state.Hosts[name] = previous
		return true, err
	}
	return true, nil
}

func (s *PlacementStore) Host(name string) (RuntimeHost, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return RuntimeHost{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	host, ok := s.state.Hosts[name]
	return host, ok
}

func (s *PlacementStore) ListHosts() []RuntimeHost {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.state.Hosts))
	for name := range s.state.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]RuntimeHost, 0, len(names))
	for _, name := range names {
		out = append(out, s.state.Hosts[name])
	}
	return out
}

func (s *PlacementStore) SetVMPlacement(vmID, hostName string) error {
	vmID = strings.TrimSpace(vmID)
	hostName = strings.TrimSpace(hostName)
	if vmID == "" {
		return fmt.Errorf("vm_id must be non-empty")
	}
	if hostName == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	s.state.VMPlacements[vmID] = hostName
	return nil
}

func (s *PlacementStore) VMHost(vmID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	host, ok := s.state.VMPlacements[strings.TrimSpace(vmID)]
	return host, ok
}

func (s *PlacementStore) RemoveVMPlacement(vmID string) {
	s.mu.Lock()
	delete(s.state.VMPlacements, strings.TrimSpace(vmID))
	s.mu.Unlock()
}

func (s *PlacementStore) SetHostObservation(name string, obs HostObservation) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	if obs.Status == "" {
		obs.Status = HostStatusUnhealthy
	}
	obs.LastError = strings.TrimSpace(obs.LastError)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	s.state.HostObservations[name] = obs
	s.state.ControlLoopStatus.Hosts[name] = obs
	return nil
}

func (s *PlacementStore) HostObservation(name string) (HostObservation, bool) {
	name = strings.TrimSpace(name)
	s.mu.RLock()
	defer s.mu.RUnlock()
	obs, ok := s.state.HostObservations[name]
	return obs, ok
}

func (s *PlacementStore) ReplaceVMPlacements(placements map[string]string) error {
	next := make(map[string]string, len(placements))
	for vmID, hostName := range placements {
		vmID = strings.TrimSpace(vmID)
		hostName = strings.TrimSpace(hostName)
		if vmID == "" || hostName == "" {
			continue
		}
		next[vmID] = hostName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	s.state.VMPlacements = next
	return nil
}

func (s *PlacementStore) MarkHostPlacementsSuspect(hostName, reason string) error {
	hostName = strings.TrimSpace(hostName)
	reason = strings.TrimSpace(reason)
	if hostName == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	if reason == "" {
		return fmt.Errorf("suspect reason must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	for vmID, placedHost := range s.state.VMPlacements {
		if placedHost == hostName {
			s.state.SuspectVMPlacements[vmID] = SuspectVMPlacement{Host: hostName, Reason: reason}
		}
	}
	return nil
}

func (s *PlacementStore) ClearHostSuspectPlacements(hostName string) {
	hostName = strings.TrimSpace(hostName)
	s.mu.Lock()
	defer s.mu.Unlock()
	for vmID, suspect := range s.state.SuspectVMPlacements {
		if suspect.Host == hostName {
			delete(s.state.SuspectVMPlacements, vmID)
		}
	}
}

func (s *PlacementStore) SetSnapshotLocation(snapshotID, hostName string) error {
	snapshotID = strings.TrimSpace(snapshotID)
	hostName = strings.TrimSpace(hostName)
	if snapshotID == "" {
		return fmt.Errorf("snapshot_id must be non-empty")
	}
	if hostName == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	seen := make(map[string]bool, len(s.state.SnapshotLocations[snapshotID])+1)
	var locations []string
	for _, existing := range s.state.SnapshotLocations[snapshotID] {
		existing = strings.TrimSpace(existing)
		if existing == "" || seen[existing] {
			continue
		}
		seen[existing] = true
		locations = append(locations, existing)
	}
	if !seen[hostName] {
		locations = append(locations, hostName)
	}
	sort.Strings(locations)
	s.state.SnapshotLocations[snapshotID] = locations
	return nil
}

func (s *PlacementStore) SnapshotHosts(snapshotID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	locations := append([]string(nil), s.state.SnapshotLocations[strings.TrimSpace(snapshotID)]...)
	sort.Strings(locations)
	return locations
}

func (s *PlacementStore) SetControlLoopStatus(status ControlLoopStatus) error {
	status.LastError = strings.TrimSpace(status.LastError)
	hosts := make(map[string]HostObservation, len(status.Hosts))
	for host, obs := range status.Hosts {
		hosts[host] = obs
	}
	status.Hosts = hosts
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	s.state.ControlLoopStatus = status
	return nil
}

func (s *PlacementStore) State() PlacementStoreState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clonePlacementStoreState(s.state)
}

func (s *PlacementStore) ensureMaps() {
	normalizePlacementStoreState(&s.state)
}

func normalizePlacementStoreState(state *PlacementStoreState) {
	if state.Hosts == nil {
		state.Hosts = make(map[string]RuntimeHost)
	}
	if state.VMPlacements == nil {
		state.VMPlacements = make(map[string]string)
	}
	if state.SnapshotLocations == nil {
		state.SnapshotLocations = make(map[string][]string)
	}
	if state.ConfigManagedHosts == nil {
		state.ConfigManagedHosts = make(map[string]bool)
	}
	if state.HostObservations == nil {
		state.HostObservations = make(map[string]HostObservation)
	}
	if state.SuspectVMPlacements == nil {
		state.SuspectVMPlacements = make(map[string]SuspectVMPlacement)
	}
	if state.ControlLoopStatus.Hosts == nil {
		state.ControlLoopStatus.Hosts = make(map[string]HostObservation)
	}
}

func clonePlacementStoreState(state PlacementStoreState) PlacementStoreState {
	out := PlacementStoreState{
		Hosts:               make(map[string]RuntimeHost, len(state.Hosts)),
		VMPlacements:        make(map[string]string, len(state.VMPlacements)),
		SnapshotLocations:   make(map[string][]string, len(state.SnapshotLocations)),
		ConfigManagedHosts:  make(map[string]bool, len(state.ConfigManagedHosts)),
		HostObservations:    make(map[string]HostObservation, len(state.HostObservations)),
		SuspectVMPlacements: make(map[string]SuspectVMPlacement, len(state.SuspectVMPlacements)),
	}
	for name, host := range state.Hosts {
		out.Hosts[name] = host
	}
	for vmID, hostName := range state.VMPlacements {
		out.VMPlacements[vmID] = hostName
	}
	for snapshotID, locations := range state.SnapshotLocations {
		out.SnapshotLocations[snapshotID] = append([]string(nil), locations...)
	}
	for host, managed := range state.ConfigManagedHosts {
		out.ConfigManagedHosts[host] = managed
	}
	for host, obs := range state.HostObservations {
		out.HostObservations[host] = obs
	}
	for vmID, suspect := range state.SuspectVMPlacements {
		out.SuspectVMPlacements[vmID] = suspect
	}
	out.ControlLoopStatus = state.ControlLoopStatus
	out.ControlLoopStatus.Hosts = make(map[string]HostObservation, len(state.ControlLoopStatus.Hosts))
	for host, obs := range state.ControlLoopStatus.Hosts {
		out.ControlLoopStatus.Hosts[host] = obs
	}
	return out
}
