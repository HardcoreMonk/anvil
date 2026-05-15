package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// FlockMetadata is the on-disk representation of a flock used to recover
// in-memory state across daemon restarts. It captures the spawn-time snapshot;
// status updates from the watchdog are not re-persisted in v0.3.1.
type FlockMetadata struct {
	FlockID       string                `json:"flock_id"`
	Task          string                `json:"task"`
	TenantID      string                `json:"tenant_id,omitempty"`
	EgressPolicy  string                `json:"egress_policy,omitempty"`
	Agents        map[string]*AgentInfo `json:"agents"`
	CreatedAt     time.Time             `json:"created_at"`
	SchemaVersion int                   `json:"schema_version"`
}

// currentSchemaVersion is bumped when the on-disk format changes in a way
// that requires migration logic.
const currentSchemaVersion = 1

// metadataPath returns the per-flock metadata.json location under workDir.
func metadataPath(workDir, flockID string) string {
	return filepath.Join(workDir, "flocks", flockID, "metadata.json")
}

// SaveFlockMetadata writes meta atomically (tmp + rename) so a partial write
// can never produce a half-formed file. Not safe for concurrent writes to the
// same flock; v0.3.1's call sites (createFlock once, deleteFlock once) never
// overlap, but a future per-status-update writer needs its own serialization.
func SaveFlockMetadata(workDir string, meta FlockMetadata) error {
	if meta.SchemaVersion == 0 {
		meta.SchemaVersion = currentSchemaVersion
	}
	dst := metadataPath(workDir, meta.FlockID)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("persistence: create dir: %w", err)
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("persistence: marshal: %w", err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err != nil {
		return fmt.Errorf("persistence: write tmp: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("persistence: rename: %w", err)
	}
	return nil
}

// LoadFlockMetadata reads a single flock's metadata.json.
func LoadFlockMetadata(workDir, flockID string) (FlockMetadata, error) {
	b, err := os.ReadFile(metadataPath(workDir, flockID))
	if err != nil {
		return FlockMetadata{}, fmt.Errorf("persistence: read: %w", err)
	}
	var meta FlockMetadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return FlockMetadata{}, fmt.Errorf("persistence: unmarshal: %w", err)
	}
	return meta, nil
}

// DeleteFlockMetadata removes a flock's metadata.json. The TOWN_WALL.log file
// is intentionally left in place as an audit artifact — operators that want a
// full purge must remove the flock directory manually.
func DeleteFlockMetadata(workDir, flockID string) error {
	err := os.Remove(metadataPath(workDir, flockID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("persistence: delete: %w", err)
	}
	return nil
}

// ListFlockMetadata enumerates every recoverable flock under workDir/flocks/.
// Directories without a parseable metadata.json are skipped (typical for
// flocks deleted before this PR landed — only the audit log remains).
// Results are sorted by CreatedAt ascending so the recovery log reads in the
// same order flocks were originally spawned.
func ListFlockMetadata(workDir string) ([]FlockMetadata, error) {
	base := filepath.Join(workDir, "flocks")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persistence: list dir: %w", err)
	}
	var out []FlockMetadata
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := LoadFlockMetadata(workDir, e.Name())
		if err != nil {
			continue
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}
