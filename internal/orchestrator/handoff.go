package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// HandoffDir returns the per-flock directory used to exchange JSON handoff
// payloads between agents.
func HandoffDir(workDir, flockID string) string {
	return filepath.Join(workDir, "flocks", flockID, "handoff")
}

// WriteHandoff serializes data as JSON to {handoffDir}/{key}.json. The parent
// directory is created if missing.
func WriteHandoff(handoffDir, key string, data any) error {
	if err := os.MkdirAll(handoffDir, 0755); err != nil {
		return fmt.Errorf("handoff: create dir: %w", err)
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("handoff: marshal %s: %w", key, err)
	}
	if err := os.WriteFile(filepath.Join(handoffDir, key+".json"), b, 0644); err != nil {
		return fmt.Errorf("handoff: write %s: %w", key, err)
	}
	return nil
}

// ReadHandoff loads JSON from {handoffDir}/{key}.json into v.
func ReadHandoff(handoffDir, key string, v any) error {
	b, err := os.ReadFile(filepath.Join(handoffDir, key+".json"))
	if err != nil {
		return fmt.Errorf("handoff: read %s: %w", key, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("handoff: unmarshal %s: %w", key, err)
	}
	return nil
}
