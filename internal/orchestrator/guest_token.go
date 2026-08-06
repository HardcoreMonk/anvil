package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// guestTokenFileName is the per-flock guest capability token file, stored inside
// the flock's own directory next to metadata.json and TOWN_WALL.log.
//
// It is deliberately NOT a field on FlockMetadata. That struct carries a blanket
// "no admission secret is ever persisted here" invariant which a test enforces by
// reading metadata.json back and failing on any token substring. Adding a token
// field would demote that invariant to a conditional one ("no routed token, but a
// local capability token is fine"), which would require the guard to tell the two
// kinds of secret apart. Conditional invariants rot; a separate file keeps this
// one absolute. The split also matches how the rest of the codebase stores
// per-entity secrets (storage.VMState.AgentToken, PlacementStore's token maps).
const guestTokenFileName = "guest-token"

// FlockGuestTokenPath returns the per-flock guest capability token file location
// under workDir. Exported so the daemon can assert its mode and existence.
func FlockGuestTokenPath(workDir, flockID string) string {
	return filepath.Join(workDir, "flocks", flockID, guestTokenFileName)
}

// SaveFlockGuestToken writes a flock's guest capability token atomically
// (tmp + rename) with an EXPLICIT 0600, set at the call site rather than left to
// the process umask. os.CreateTemp already creates at 0600, and the Chmod pins it
// so neither a permissive umask nor a stale temp file left by an earlier crash
// can widen the destination's mode. The enclosing flock directory stays 0755 (as
// SaveFlockMetadata creates it); a 0600 file inside it is still unreadable to
// other users, so the file mode is the load-bearing control.
func SaveFlockGuestToken(workDir, flockID, token string) error {
	dst := FlockGuestTokenPath(workDir, flockID)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("guest token: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".guest-token-*")
	if err != nil {
		return fmt.Errorf("guest token: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(token); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("guest token: write tmp: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("guest token: chmod tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("guest token: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("guest token: rename: %w", err)
	}
	return nil
}

// LoadFlockGuestToken reads a flock's guest capability token. A flock that never
// had one (created before this file existed, or a routed flock whose token is
// supplied by its adapter) returns an error, which callers treat as "no admission
// registered" rather than as an empty token.
func LoadFlockGuestToken(workDir, flockID string) (string, error) {
	b, err := os.ReadFile(FlockGuestTokenPath(workDir, flockID))
	if err != nil {
		return "", fmt.Errorf("guest token: read: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// DeleteFlockGuestToken removes a flock's guest capability token file. This must
// be called explicitly on flock deletion: the flock directory itself deliberately
// survives (DeleteFlockMetadata keeps TOWN_WALL.log as an audit artifact), so the
// token would otherwise outlive the flock it authorizes. Missing files are not an
// error, so the call is idempotent.
func DeleteFlockGuestToken(workDir, flockID string) error {
	err := os.Remove(FlockGuestTokenPath(workDir, flockID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("guest token: delete: %w", err)
	}
	return nil
}
