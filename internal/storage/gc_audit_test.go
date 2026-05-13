package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendSnapshotGCAuditWritesJSONL(t *testing.T) {
	workDir := t.TempDir()
	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	record := SnapshotGCAuditRecord{
		Timestamp: ts,
		Applied:   true,
		Policy: SnapshotGCAuditPolicy{
			OlderThanSeconds: 604800,
			KeepLastPerVM:    1,
			MaxTotalBytes:    1024,
		},
		CandidatesCount: 2,
		DeletedCount:    1,
		ErrorsCount:     1,
	}

	if err := AppendSnapshotGCAudit(workDir, record); err != nil {
		t.Fatalf("append audit: %v", err)
	}
	if err := AppendSnapshotGCAudit(workDir, record); err != nil {
		t.Fatalf("append second audit: %v", err)
	}

	auditPath := filepath.Join(workDir, "snapshots", "gc-audit.jsonl")
	info, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("stat audit file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("audit mode = %v, want 0600", mode)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d, want 2: %q", len(lines), string(data))
	}

	var got SnapshotGCAuditRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("parse first audit line: %v", err)
	}
	if !got.Timestamp.Equal(ts) || !got.Applied || got.Policy.MaxTotalBytes != 1024 ||
		got.CandidatesCount != 2 || got.DeletedCount != 1 || got.ErrorsCount != 1 {
		t.Fatalf("audit record = %+v, want %+v", got, record)
	}
}

func TestAppendSnapshotGCAuditTightensExistingFilePermissions(t *testing.T) {
	workDir := t.TempDir()
	auditDir := filepath.Join(workDir, "snapshots")
	if err := os.MkdirAll(auditDir, 0755); err != nil {
		t.Fatalf("create audit dir: %v", err)
	}
	auditPath := filepath.Join(auditDir, "gc-audit.jsonl")
	if err := os.WriteFile(auditPath, []byte{}, 0644); err != nil {
		t.Fatalf("create loose audit file: %v", err)
	}

	if err := AppendSnapshotGCAudit(workDir, SnapshotGCAuditRecord{Applied: true}); err != nil {
		t.Fatalf("append audit: %v", err)
	}

	info, err := os.Stat(auditPath)
	if err != nil {
		t.Fatalf("stat audit file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("audit mode = %v, want 0600", mode)
	}
}

func TestAppendSnapshotGCAuditRejectsSymlink(t *testing.T) {
	workDir := t.TempDir()
	auditDir := filepath.Join(workDir, "snapshots")
	if err := os.MkdirAll(auditDir, 0755); err != nil {
		t.Fatalf("create audit dir: %v", err)
	}
	targetPath := filepath.Join(workDir, "target.jsonl")
	if err := os.WriteFile(targetPath, []byte("keep\n"), 0600); err != nil {
		t.Fatalf("create target file: %v", err)
	}
	auditPath := filepath.Join(auditDir, "gc-audit.jsonl")
	if err := os.Symlink(targetPath, auditPath); err != nil {
		t.Fatalf("create audit symlink: %v", err)
	}

	err := AppendSnapshotGCAudit(workDir, SnapshotGCAuditRecord{Applied: true})
	if err == nil {
		t.Fatal("append through symlink succeeded, want error")
	}

	data, readErr := os.ReadFile(targetPath)
	if readErr != nil {
		t.Fatalf("read target file: %v", readErr)
	}
	if string(data) != "keep\n" {
		t.Fatalf("target file = %q, want unchanged", string(data))
	}
}
