package anvilmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type TenantRecord struct {
	TenantID string      `json:"tenant_id"`
	Quota    TenantQuota `json:"quota"`
	Usage    TenantUsage `json:"usage"`
}

type TenantQuotaState struct {
	Quota TenantQuota `json:"quota"`
	Usage TenantUsage `json:"usage"`
}

type QuotaStoreState struct {
	Tenants map[string]TenantQuotaState `json:"tenants"`
}

type QuotaStore struct {
	mu    sync.RWMutex
	path  string
	state QuotaStoreState
}

func NewQuotaStore(path string) *QuotaStore {
	return &QuotaStore{
		path: path,
		state: QuotaStoreState{
			Tenants: make(map[string]TenantQuotaState),
		},
	}
}

func (s *QuotaStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.state = QuotaStoreState{Tenants: make(map[string]TenantQuotaState)}
			return nil
		}
		return fmt.Errorf("read quota store: %w", err)
	}
	var state QuotaStoreState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse quota store: %w", err)
	}
	if state.Tenants == nil {
		state.Tenants = make(map[string]TenantQuotaState)
	}
	s.state = state
	return nil
}

func (s *QuotaStore) Save() error {
	s.mu.RLock()
	state := cloneQuotaStoreState(s.state)
	s.mu.RUnlock()
	return writeQuotaStoreState(s.path, state)
}

func (s *QuotaStore) SetTenantQuota(tenantID string, quota TenantQuota) error {
	tenantID, err := NormalizeTenantID(tenantID)
	if err != nil {
		return err
	}
	if err := validateQuotaValues(quota, TenantUsage{}, TenantUsage{}); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTenants()
	current := s.state.Tenants[tenantID]
	current.Quota = quota
	s.state.Tenants[tenantID] = current
	return nil
}

func (s *QuotaStore) UpdateTenantUsage(tenantID string, delta TenantUsage) error {
	tenantID, err := NormalizeTenantID(tenantID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTenants()
	current := s.state.Tenants[tenantID]
	next := addTenantUsage(current.Usage, delta)
	if hasNegativeUsage(next) {
		return fmt.Errorf("tenant usage must not become negative")
	}
	current.Usage = next
	s.state.Tenants[tenantID] = current
	return nil
}

func (s *QuotaStore) SchedulerInputs() (map[string]TenantQuota, map[string]TenantUsage) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	quotas := make(map[string]TenantQuota, len(s.state.Tenants))
	usage := make(map[string]TenantUsage, len(s.state.Tenants))
	for tenantID, state := range s.state.Tenants {
		quotas[tenantID] = state.Quota
		usage[tenantID] = state.Usage
	}
	return quotas, usage
}

func (s *QuotaStore) GetTenant(tenantID string) (TenantRecord, bool, error) {
	tenantID, err := NormalizeTenantID(tenantID)
	if err != nil {
		return TenantRecord{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.state.Tenants[tenantID]
	if !ok {
		return TenantRecord{}, false, nil
	}
	return TenantRecord{TenantID: tenantID, Quota: state.Quota, Usage: state.Usage}, true, nil
}

func (s *QuotaStore) ListTenants() []TenantRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.state.Tenants))
	for tenantID := range s.state.Tenants {
		ids = append(ids, tenantID)
	}
	sort.Strings(ids)
	records := make([]TenantRecord, 0, len(ids))
	for _, tenantID := range ids {
		state := s.state.Tenants[tenantID]
		records = append(records, TenantRecord{TenantID: tenantID, Quota: state.Quota, Usage: state.Usage})
	}
	return records
}

func (s *QuotaStore) ensureTenants() {
	if s.state.Tenants == nil {
		s.state.Tenants = make(map[string]TenantQuotaState)
	}
}

func cloneQuotaStoreState(state QuotaStoreState) QuotaStoreState {
	out := QuotaStoreState{Tenants: make(map[string]TenantQuotaState, len(state.Tenants))}
	for tenantID, tenantState := range state.Tenants {
		out.Tenants[tenantID] = tenantState
	}
	return out
}

func writeQuotaStoreState(path string, state QuotaStoreState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode quota store: %w", err)
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create quota store dir: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".quota-*.json")
	if err != nil {
		return fmt.Errorf("create quota store temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write quota store temp file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod quota store temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close quota store temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace quota store: %w", err)
	}
	return nil
}

func addTenantUsage(current, delta TenantUsage) TenantUsage {
	return TenantUsage{
		ActiveVMs:            current.ActiveVMs + delta.ActiveVMs,
		SnapshotCount:        current.SnapshotCount + delta.SnapshotCount,
		SnapshotBytes:        current.SnapshotBytes + delta.SnapshotBytes,
		ConcurrentTasks:      current.ConcurrentTasks + delta.ConcurrentTasks,
		RetainedAuditRecords: current.RetainedAuditRecords + delta.RetainedAuditRecords,
	}
}

func hasNegativeUsage(usage TenantUsage) bool {
	return usage.ActiveVMs < 0 ||
		usage.SnapshotCount < 0 ||
		usage.SnapshotBytes < 0 ||
		usage.ConcurrentTasks < 0 ||
		usage.RetainedAuditRecords < 0
}
