package anvilmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type VMInfo struct {
	VMID         string `json:"vm_id"`
	GuestIP      string `json:"guest_ip"`
	AgentURL     string `json:"agent_url"`
	Profile      string `json:"profile,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	EgressPolicy string `json:"egress_policy,omitempty"`
}

type PlacementStoreState struct {
	Hosts             map[string]RuntimeHost `json:"hosts"`
	VMPlacements      map[string]string      `json:"vm_placements"`
	SnapshotLocations map[string][]string    `json:"snapshot_locations"`
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
			Hosts:             make(map[string]RuntimeHost),
			VMPlacements:      make(map[string]string),
			SnapshotLocations: make(map[string][]string),
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
	state := s.State()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode placement store: %w", err)
	}
	dir := filepath.Dir(s.path)
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
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("replace placement store: %w", err)
	}
	return nil
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

func (s *PlacementStore) State() PlacementStoreState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := PlacementStoreState{
		Hosts:             make(map[string]RuntimeHost, len(s.state.Hosts)),
		VMPlacements:      make(map[string]string, len(s.state.VMPlacements)),
		SnapshotLocations: make(map[string][]string, len(s.state.SnapshotLocations)),
	}
	for name, host := range s.state.Hosts {
		state.Hosts[name] = host
	}
	for vmID, hostName := range s.state.VMPlacements {
		state.VMPlacements[vmID] = hostName
	}
	for snapshotID, locations := range s.state.SnapshotLocations {
		state.SnapshotLocations[snapshotID] = append([]string(nil), locations...)
	}
	return state
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
}
