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

const (
	RoutedFlockModeCrossHostMembersOnly = "cross_host_members_only"

	RoutedFlockStatusCreating             = "creating"
	RoutedFlockStatusReady                = "ready"
	RoutedFlockStatusDeleting             = "deleting"
	RoutedFlockStatusDeleted              = "deleted"
	RoutedFlockStatusFailedCleanupPending = "failed_cleanup_pending"
)

type RoutedFlockRecord struct {
	FlockID      string             `json:"flock_id"`
	Task         string             `json:"task"`
	TenantID     string             `json:"tenant_id,omitempty"`
	EgressPolicy string             `json:"egress_policy,omitempty"`
	Mode         string             `json:"mode"`
	Status       string             `json:"status"`
	HomeHost     string             `json:"home_host,omitempty"`
	Agents       []RoutedFlockAgent `json:"agents"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
	// relayToken carries the flock's per-flock relay secret from create ->
	// store persistence. It is unexported so it is NEVER JSON-serialized into
	// MCP output, audit, or the scheduler placements endpoint. On save it is
	// copied into the store's dedicated RoutedFlockRelayTokens map (which
	// State() redacts from every external view) and then scrubbed from the
	// record retained in the store.
	relayToken string
	// callToken carries the flock's per-flock call secret from create ->
	// store persistence. It mirrors relayToken exactly: unexported so it is
	// NEVER JSON-serialized into MCP output, audit, or the scheduler
	// placements endpoint. On save it is copied into the store's dedicated
	// RoutedFlockCallTokens map (which State() redacts from every external
	// view) and then scrubbed from the record retained in the store.
	callToken string
}

type RoutedFlockAgent struct {
	AgentID  string `json:"agent_id"`
	Role     string `json:"role"`
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
	Host     string `json:"host"`
	Status   string `json:"status"`
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
	Hosts                 map[string]RuntimeHost        `json:"hosts"`
	VMPlacements          map[string]string             `json:"vm_placements"`
	SnapshotLocations     map[string][]string           `json:"snapshot_locations"`
	ConfigManagedHosts    map[string]bool               `json:"config_managed_hosts,omitempty"`
	HostObservations      map[string]HostObservation    `json:"host_observations,omitempty"`
	SuspectVMPlacements   map[string]SuspectVMPlacement `json:"suspect_vm_placements,omitempty"`
	ControlLoopStatus     ControlLoopStatus             `json:"control_loop_status,omitempty"`
	FlockPlacementMetrics FlockPlacementMetricsState    `json:"flock_placement_metrics,omitempty"`
	RoutedFlocks          map[string]RoutedFlockRecord  `json:"routed_flocks,omitempty"`
	// RoutedFlockRelayTokens holds per-flock relay secrets keyed by flock id.
	// Persisted for reconcile re-registration (Task 7) but REDACTED by State()
	// so it never reaches the scheduler placements endpoint or any MCP output.
	RoutedFlockRelayTokens map[string]string `json:"routed_flock_relay_tokens,omitempty"`
	// RoutedFlockCallTokens holds per-flock call secrets keyed by flock id.
	// Mirrors RoutedFlockRelayTokens exactly: persisted for reconcile
	// re-registration but REDACTED by State() so it never reaches the
	// scheduler placements endpoint or any MCP output.
	RoutedFlockCallTokens map[string]string `json:"routed_flock_call_tokens,omitempty"`
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
			FlockPlacementMetrics:  newFlockPlacementMetricsState(),
			RoutedFlocks:           make(map[string]RoutedFlockRecord),
			RoutedFlockRelayTokens: make(map[string]string),
			RoutedFlockCallTokens:  make(map[string]string),
		},
	}
}

func (s *PlacementStore) Load() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	state, exists, err := readPlacementStoreState(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if exists {
		s.state = state
	} else {
		s.ensureMaps()
	}
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
	return savePlacementStoreStatePreserveFlockMetrics(s.path, state)
}

func readPlacementStoreState(path string) (PlacementStoreState, bool, error) {
	var state PlacementStoreState
	if strings.TrimSpace(path) == "" {
		return state, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, false, nil
		}
		return state, false, fmt.Errorf("read placement store: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, true, fmt.Errorf("parse placement store: %w", err)
	}
	normalizePlacementStoreState(&state)
	return state, true, nil
}

func mergePersistedPlacementStoreFields(path string, state *PlacementStoreState, preserveMetrics bool) error {
	persisted, exists, err := readPlacementStoreState(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if preserveMetrics {
		state.FlockPlacementMetrics = cloneFlockPlacementMetricsState(persisted.FlockPlacementMetrics)
	}
	mergePersistedRoutedFlocks(persisted.RoutedFlocks, state)
	mergePersistedRoutedFlockTokens(persisted.RoutedFlockRelayTokens, persisted.RoutedFlockCallTokens, state)
	return nil
}

func savePlacementStoreStatePreserveFlockMetrics(path string, state PlacementStoreState) error {
	if err := mergePersistedPlacementStoreFields(path, &state, true); err != nil {
		return err
	}
	return writePlacementStoreState(path, state)
}

func savePlacementStoreStatePreserveRoutedFlocks(path string, state PlacementStoreState) error {
	if err := mergePersistedPlacementStoreFields(path, &state, false); err != nil {
		return err
	}
	return writePlacementStoreState(path, state)
}

func mergePersistedRoutedFlocks(persisted map[string]RoutedFlockRecord, state *PlacementStoreState) {
	if state.VMPlacements == nil {
		state.VMPlacements = make(map[string]string)
	}
	for _, record := range state.RoutedFlocks {
		for _, agent := range record.Agents {
			if vmID := strings.TrimSpace(agent.VMID); vmID != "" {
				delete(state.VMPlacements, vmID)
			}
		}
	}

	next := make(map[string]RoutedFlockRecord, len(persisted))
	for _, record := range persisted {
		record = normalizeRoutedFlockRecord(record)
		if record.FlockID == "" {
			continue
		}
		next[record.FlockID] = cloneRoutedFlockRecord(record)
		for _, agent := range record.Agents {
			vmID := strings.TrimSpace(agent.VMID)
			host := strings.TrimSpace(agent.Host)
			if vmID != "" && host != "" && agent.Status != "deleted" {
				state.VMPlacements[vmID] = host
			}
		}
	}
	state.RoutedFlocks = next
}

// mergePersistedRoutedFlockTokens mirrors mergePersistedRoutedFlocks' handling
// of RoutedFlocks for the two redacted per-flock secret maps: routed flock
// tokens are only ever mutated through SaveRoutedFlockAndPlacements and
// removeRoutedFlockRelayToken/removeRoutedFlockCallToken, all three of which
// read-modify-write the full persisted state directly (base = disk, mutate,
// write back). That means a generic (non-flock) save never has a staged
// in-memory token change that isn't already reflected on disk -- so, exactly
// like RoutedFlocks records, it is both safe and necessary to take the token
// maps entirely from disk rather than keeping the calling instance's
// possibly-stale in-memory copy. Safe: a stale instance can never be "ahead"
// of disk for tokens it already knows about. Necessary: without this, a
// generic save from an instance that hasn't reloaded since another instance
// persisted (or removed) a flock's token would silently overwrite that
// token's disk entry with its own stale (missing, or stale-present) copy --
// losing a concurrent writer's token, or resurrecting one that removeRouted-
// Flock*Token already deleted from disk.
func mergePersistedRoutedFlockTokens(relayTokens, callTokens map[string]string, state *PlacementStoreState) {
	state.RoutedFlockRelayTokens = make(map[string]string, len(relayTokens))
	for flockID, token := range relayTokens {
		state.RoutedFlockRelayTokens[flockID] = token
	}
	state.RoutedFlockCallTokens = make(map[string]string, len(callTokens))
	for flockID, token := range callTokens {
		state.RoutedFlockCallTokens[flockID] = token
	}
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
	return s.saveLockedPreserveFlockMetrics()
}

func (s *PlacementStore) saveLockedPreserveFlockMetrics() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	state := clonePlacementStoreState(s.state)
	return savePlacementStoreStatePreserveFlockMetrics(s.path, state)
}

func (s *PlacementStore) saveLockedRaw() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	return savePlacementStoreStatePreserveRoutedFlocks(s.path, clonePlacementStoreState(s.state))
}

func (s *PlacementStore) SetHost(host RuntimeHost) error {
	name := strings.TrimSpace(host.Name)
	if name == "" {
		return fmt.Errorf("host name must be non-empty")
	}
	host.Name = name
	host = cloneRuntimeHost(host)
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
	host = cloneRuntimeHost(host)
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

func (s *PlacementStore) ApplyConfiguredHostsAndSave(hosts []RuntimeHost) error {
	desired := make(map[string]RuntimeHost, len(hosts))
	for _, host := range hosts {
		name := strings.TrimSpace(host.Name)
		if name == "" {
			return fmt.Errorf("host name must be non-empty")
		}
		if _, exists := desired[name]; exists {
			return fmt.Errorf("duplicate host %q", name)
		}
		host.Name = name
		desired[name] = cloneRuntimeHost(host)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	previous := clonePlacementStoreState(s.state)

	for name, host := range desired {
		if existing, ok := s.state.Hosts[name]; ok {
			host.Healthy = existing.Healthy
		}
		s.state.Hosts[name] = host
		s.state.ConfigManagedHosts[name] = true
	}
	for name := range s.state.ConfigManagedHosts {
		if _, ok := desired[name]; !ok {
			s.removeHostLocked(name)
		}
	}
	if err := s.saveLocked(); err != nil {
		s.state = previous
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
	s.removeHostLocked(name)
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
	previous := clonePlacementStoreState(s.state)
	_, existed := s.state.Hosts[name]
	if !existed {
		return false, nil
	}
	s.removeHostLocked(name)
	if err := s.saveLocked(); err != nil {
		s.state = previous
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
	return cloneRuntimeHost(host), ok
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
		out = append(out, cloneRuntimeHost(s.state.Hosts[name]))
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

func (s *PlacementStore) ApplyHostObservation(base RuntimeHost, observed RuntimeHost, obs HostObservation) (bool, error) {
	name := strings.TrimSpace(observed.Name)
	if name == "" {
		name = strings.TrimSpace(base.Name)
	}
	if name == "" {
		return false, fmt.Errorf("host name must be non-empty")
	}
	if obs.Status == "" {
		obs.Status = HostStatusUnhealthy
	}
	obs.LastError = strings.TrimSpace(obs.LastError)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()
	current, ok := s.state.Hosts[name]
	if !ok {
		return false, nil
	}
	current.Healthy = observed.Healthy
	current.AvailableVMs = observed.AvailableVMs
	current.AvailableSnapshotBytes = observed.AvailableSnapshotBytes
	current.EgressPolicies = append([]EgressPolicy(nil), observed.EgressPolicies...)
	s.state.Hosts[name] = current
	s.state.HostObservations[name] = obs
	s.state.ControlLoopStatus.Hosts[name] = obs
	return true, nil
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

func (s *PlacementStore) ReconcileVMPlacements(base map[string]string, reconciled map[string]string) error {
	normalizedBase := normalizeVMPlacements(base)
	normalizedReconciled := normalizeVMPlacements(reconciled)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()

	for vmID, baseHost := range normalizedBase {
		currentHost, exists := s.state.VMPlacements[vmID]
		if !exists {
			continue
		}
		if currentHost != baseHost {
			continue
		}
		reconciledHost, ok := normalizedReconciled[vmID]
		if ok {
			s.state.VMPlacements[vmID] = reconciledHost
		} else {
			delete(s.state.VMPlacements, vmID)
		}
	}

	for vmID, hostName := range normalizedReconciled {
		if _, wasInBase := normalizedBase[vmID]; wasInBase {
			continue
		}
		if _, exists := s.state.VMPlacements[vmID]; exists {
			continue
		}
		s.state.VMPlacements[vmID] = hostName
	}
	for vmID, suspect := range s.state.SuspectVMPlacements {
		currentHost, ok := s.state.VMPlacements[vmID]
		if !ok || currentHost != suspect.Host {
			delete(s.state.SuspectVMPlacements, vmID)
		}
	}
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
	state := clonePlacementStoreState(s.state)
	// Relay tokens are per-flock secrets: never expose them through State(),
	// which feeds the scheduler placements endpoint and other external views.
	// Disk persistence clones s.state directly, so it retains the tokens.
	state.RoutedFlockRelayTokens = nil
	// Call tokens mirror relay tokens: same per-flock secret exposure risk,
	// same redaction.
	state.RoutedFlockCallTokens = nil
	return state
}

// RoutedFlockRelayToken returns the persisted per-flock relay secret. It is an
// internal accessor for reconcile re-registration (Task 7); the token is never
// surfaced through State() or any MCP-facing projection.
func (s *PlacementStore) RoutedFlockRelayToken(flockID string) (string, bool) {
	flockID = strings.TrimSpace(flockID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	token, ok := s.state.RoutedFlockRelayTokens[flockID]
	return token, ok
}

// removeRoutedFlockRelayToken deletes a flock's persisted relay secret from the
// store (and disk) so no stale token lingers after the flock is rolled back or
// deleted. It mirrors SaveRoutedFlockAndPlacements' read-modify-write so metrics
// and other routed-flock records persisted concurrently are preserved. It is
// idempotent: removing an absent token is a no-op. Best-effort: the caller may
// ignore the returned error (a persistence failure only leaves a token that a
// later deregistration or reconcile can still purge).
func (s *PlacementStore) removeRoutedFlockRelayToken(flockID string) error {
	flockID = strings.TrimSpace(flockID)
	if flockID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()

	if strings.TrimSpace(s.path) == "" {
		delete(s.state.RoutedFlockRelayTokens, flockID)
		return nil
	}

	previous := clonePlacementStoreState(s.state)
	base := clonePlacementStoreState(s.state)
	persisted, exists, err := readPlacementStoreState(s.path)
	if err != nil {
		return err
	}
	if exists {
		base = persisted
	}
	normalizePlacementStoreState(&base)
	delete(base.RoutedFlockRelayTokens, flockID)
	if err := writePlacementStoreState(s.path, base); err != nil {
		s.state = previous
		return err
	}
	s.state = clonePlacementStoreState(base)
	return nil
}

// RoutedFlockCallToken returns the persisted per-flock call secret. It
// mirrors RoutedFlockRelayToken exactly: an internal accessor whose token is
// never surfaced through State() or any MCP-facing projection.
func (s *PlacementStore) RoutedFlockCallToken(flockID string) (string, bool) {
	flockID = strings.TrimSpace(flockID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	token, ok := s.state.RoutedFlockCallTokens[flockID]
	return token, ok
}

// removeRoutedFlockCallToken deletes a flock's persisted call secret from the
// store (and disk) so no stale token lingers after the flock is rolled back or
// deleted. It mirrors removeRoutedFlockRelayToken's read-modify-write so
// metrics and other routed-flock records persisted concurrently are
// preserved. It is idempotent: removing an absent token is a no-op.
// Best-effort: the caller may ignore the returned error (a persistence
// failure only leaves a token that a later deregistration or reconcile can
// still purge).
func (s *PlacementStore) removeRoutedFlockCallToken(flockID string) error {
	flockID = strings.TrimSpace(flockID)
	if flockID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()

	if strings.TrimSpace(s.path) == "" {
		delete(s.state.RoutedFlockCallTokens, flockID)
		return nil
	}

	previous := clonePlacementStoreState(s.state)
	base := clonePlacementStoreState(s.state)
	persisted, exists, err := readPlacementStoreState(s.path)
	if err != nil {
		return err
	}
	if exists {
		base = persisted
	}
	normalizePlacementStoreState(&base)
	delete(base.RoutedFlockCallTokens, flockID)
	if err := writePlacementStoreState(s.path, base); err != nil {
		s.state = previous
		return err
	}
	s.state = clonePlacementStoreState(base)
	return nil
}

func (s *PlacementStore) ensureMaps() {
	normalizePlacementStoreState(&s.state)
}

func (s *PlacementStore) removeHostLocked(name string) {
	delete(s.state.Hosts, name)
	delete(s.state.ConfigManagedHosts, name)
	delete(s.state.HostObservations, name)
	delete(s.state.ControlLoopStatus.Hosts, name)
	for vmID, suspect := range s.state.SuspectVMPlacements {
		if suspect.Host == name {
			delete(s.state.SuspectVMPlacements, vmID)
		}
	}
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
	normalizeFlockPlacementMetricsState(&state.FlockPlacementMetrics)
	if state.RoutedFlockRelayTokens == nil {
		state.RoutedFlockRelayTokens = make(map[string]string)
	}
	if state.RoutedFlockCallTokens == nil {
		state.RoutedFlockCallTokens = make(map[string]string)
	}
	if state.RoutedFlocks == nil {
		state.RoutedFlocks = make(map[string]RoutedFlockRecord)
	}
	for flockID, record := range state.RoutedFlocks {
		normalized := normalizeRoutedFlockRecord(record)
		if normalized.FlockID == "" {
			delete(state.RoutedFlocks, flockID)
			continue
		}
		state.RoutedFlocks[normalized.FlockID] = normalized
		if normalized.FlockID != flockID {
			delete(state.RoutedFlocks, flockID)
		}
	}
}

func normalizeVMPlacements(placements map[string]string) map[string]string {
	normalized := make(map[string]string, len(placements))
	for vmID, hostName := range placements {
		vmID = strings.TrimSpace(vmID)
		hostName = strings.TrimSpace(hostName)
		if vmID == "" || hostName == "" {
			continue
		}
		normalized[vmID] = hostName
	}
	return normalized
}

func clonePlacementStoreState(state PlacementStoreState) PlacementStoreState {
	out := PlacementStoreState{
		Hosts:                  make(map[string]RuntimeHost, len(state.Hosts)),
		VMPlacements:           make(map[string]string, len(state.VMPlacements)),
		SnapshotLocations:      make(map[string][]string, len(state.SnapshotLocations)),
		ConfigManagedHosts:     make(map[string]bool, len(state.ConfigManagedHosts)),
		HostObservations:       make(map[string]HostObservation, len(state.HostObservations)),
		SuspectVMPlacements:    make(map[string]SuspectVMPlacement, len(state.SuspectVMPlacements)),
		RoutedFlocks:           make(map[string]RoutedFlockRecord, len(state.RoutedFlocks)),
		RoutedFlockRelayTokens: make(map[string]string, len(state.RoutedFlockRelayTokens)),
		RoutedFlockCallTokens:  make(map[string]string, len(state.RoutedFlockCallTokens)),
	}
	for name, host := range state.Hosts {
		out.Hosts[name] = cloneRuntimeHost(host)
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
	for flockID, record := range state.RoutedFlocks {
		out.RoutedFlocks[flockID] = cloneRoutedFlockRecord(record)
	}
	for flockID, token := range state.RoutedFlockRelayTokens {
		out.RoutedFlockRelayTokens[flockID] = token
	}
	for flockID, token := range state.RoutedFlockCallTokens {
		out.RoutedFlockCallTokens[flockID] = token
	}
	out.ControlLoopStatus = state.ControlLoopStatus
	out.ControlLoopStatus.Hosts = make(map[string]HostObservation, len(state.ControlLoopStatus.Hosts))
	for host, obs := range state.ControlLoopStatus.Hosts {
		out.ControlLoopStatus.Hosts[host] = obs
	}
	out.FlockPlacementMetrics = cloneFlockPlacementMetricsState(state.FlockPlacementMetrics)
	return out
}

func cloneRuntimeHost(host RuntimeHost) RuntimeHost {
	host.EgressPolicies = append([]EgressPolicy(nil), host.EgressPolicies...)
	return host
}

func (s *PlacementStore) SaveRoutedFlockAndPlacements(record RoutedFlockRecord, removeVMIDs []string) error {
	record = normalizeRoutedFlockRecord(record)
	if record.FlockID == "" {
		return fmt.Errorf("flock_id must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureMaps()

	if strings.TrimSpace(s.path) == "" {
		applyRoutedFlockAndPlacements(&s.state, record, removeVMIDs)
		return nil
	}

	previous := clonePlacementStoreState(s.state)
	base := clonePlacementStoreState(s.state)
	persisted, exists, err := readPlacementStoreState(s.path)
	if err != nil {
		return err
	}
	if exists {
		base = persisted
	}
	applyRoutedFlockAndPlacements(&base, record, removeVMIDs)
	if err := writePlacementStoreState(s.path, base); err != nil {
		s.state = previous
		return err
	}
	s.state = clonePlacementStoreState(base)
	return nil
}

func applyRoutedFlockAndPlacements(state *PlacementStoreState, record RoutedFlockRecord, removeVMIDs []string) {
	normalizePlacementStoreState(state)
	// Persist the relay secret in the dedicated (State()-redacted) map, then
	// scrub it from the record retained in the store so no record projection
	// can leak it. Only overwrite when the carrier is set, so record re-saves
	// that do not carry the token preserve the persisted value.
	if token := strings.TrimSpace(record.relayToken); token != "" {
		state.RoutedFlockRelayTokens[record.FlockID] = token
	}
	record.relayToken = ""
	// Call tokens mirror relay tokens: persist into the dedicated map (only
	// when the carrier is set, so token-less re-saves preserve the persisted
	// value), then scrub from the record retained in the store.
	if token := strings.TrimSpace(record.callToken); token != "" {
		state.RoutedFlockCallTokens[record.FlockID] = token
	}
	record.callToken = ""
	state.RoutedFlocks[record.FlockID] = cloneRoutedFlockRecord(record)
	for _, vmID := range removeVMIDs {
		delete(state.VMPlacements, strings.TrimSpace(vmID))
	}
	for _, agent := range record.Agents {
		vmID := strings.TrimSpace(agent.VMID)
		host := strings.TrimSpace(agent.Host)
		if vmID != "" && host != "" && agent.Status != "deleted" {
			state.VMPlacements[vmID] = host
		}
	}
}

func (s *PlacementStore) RoutedFlock(flockID string) (RoutedFlockRecord, bool) {
	flockID = strings.TrimSpace(flockID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.state.RoutedFlocks[flockID]
	return cloneRoutedFlockRecord(record), ok
}

func (s *PlacementStore) ListRoutedFlocks() []RoutedFlockRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.state.RoutedFlocks))
	for flockID := range s.state.RoutedFlocks {
		names = append(names, flockID)
	}
	sort.Strings(names)
	out := make([]RoutedFlockRecord, 0, len(names))
	for _, flockID := range names {
		out = append(out, cloneRoutedFlockRecord(s.state.RoutedFlocks[flockID]))
	}
	return out
}

func normalizeRoutedFlockRecord(record RoutedFlockRecord) RoutedFlockRecord {
	record.FlockID = strings.TrimSpace(record.FlockID)
	record.Task = strings.TrimSpace(record.Task)
	record.TenantID = strings.TrimSpace(record.TenantID)
	record.EgressPolicy = strings.TrimSpace(record.EgressPolicy)
	record.HomeHost = strings.TrimSpace(record.HomeHost)
	record.Mode = strings.TrimSpace(record.Mode)
	if record.Mode == "" {
		record.Mode = RoutedFlockModeCrossHostMembersOnly
	}
	record.Status = normalizeRoutedFlockStatus(record.Status)
	agents := make([]RoutedFlockAgent, 0, len(record.Agents))
	for _, agent := range record.Agents {
		agent.AgentID = strings.TrimSpace(agent.AgentID)
		agent.Role = strings.TrimSpace(agent.Role)
		agent.VMID = strings.TrimSpace(agent.VMID)
		agent.AgentURL = strings.TrimSpace(agent.AgentURL)
		agent.Host = strings.TrimSpace(agent.Host)
		agent.Status = strings.TrimSpace(agent.Status)
		if agent.AgentID == "" || agent.Role == "" {
			continue
		}
		agents = append(agents, agent)
	}
	record.Agents = agents
	return record
}

func normalizeRoutedFlockStatus(status string) string {
	switch strings.TrimSpace(status) {
	case RoutedFlockStatusCreating:
		return RoutedFlockStatusCreating
	case RoutedFlockStatusReady:
		return RoutedFlockStatusReady
	case RoutedFlockStatusDeleting:
		return RoutedFlockStatusDeleting
	case RoutedFlockStatusDeleted:
		return RoutedFlockStatusDeleted
	case RoutedFlockStatusFailedCleanupPending:
		return RoutedFlockStatusFailedCleanupPending
	default:
		return RoutedFlockStatusCreating
	}
}

func cloneRoutedFlockRecord(record RoutedFlockRecord) RoutedFlockRecord {
	record.Agents = append([]RoutedFlockAgent(nil), record.Agents...)
	return record
}
