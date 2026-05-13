package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// SnapshotGCAuditPolicy captures the retention knobs used for one manual GC run.
type SnapshotGCAuditPolicy struct {
	OlderThanSeconds int64 `json:"older_than_seconds"`
	KeepLastPerVM    int   `json:"keep_last_per_vm"`
	MaxTotalBytes    int64 `json:"max_total_bytes"`
}

// SnapshotGCAuditRecord is the JSONL payload appended after an applied GC run.
type SnapshotGCAuditRecord struct {
	Timestamp       time.Time             `json:"timestamp"`
	Applied         bool                  `json:"applied"`
	Policy          SnapshotGCAuditPolicy `json:"policy"`
	CandidatesCount int                   `json:"candidates_count"`
	DeletedCount    int                   `json:"deleted_count"`
	ErrorsCount     int                   `json:"errors_count"`
}

// AppendSnapshotGCAudit appends one GC audit record to snapshots/gc-audit.jsonl.
func AppendSnapshotGCAudit(workDir string, record SnapshotGCAuditRecord) error {
	auditPath := filepath.Join(workDir, "snapshots", "gc-audit.jsonl")
	if err := os.MkdirAll(filepath.Dir(auditPath), 0755); err != nil {
		return fmt.Errorf("create snapshot audit dir: %w", err)
	}

	if info, err := os.Lstat(auditPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("audit file is a symlink")
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("audit file is not regular")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat snapshot audit file: %w", err)
	}

	fd, err := unix.Open(auditPath, unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("open snapshot audit file: %w", err)
	}
	f := os.NewFile(uintptr(fd), auditPath)
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat opened snapshot audit file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("opened audit file is not regular")
	}
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("chmod snapshot audit file before write: %w", err)
	}
	if err := json.NewEncoder(f).Encode(record); err != nil {
		return fmt.Errorf("write snapshot audit record: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("chmod snapshot audit file after append: %w", err)
	}
	return nil
}
