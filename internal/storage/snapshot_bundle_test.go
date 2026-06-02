package storage

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const snapshotBundleFixtureToken = "fixture-agent-token-secret"

func TestExportSnapshotBundleContainsManifestAndArtifacts(t *testing.T) {
	workDir := t.TempDir()
	writeSnapshotBundleFixture(t, workDir, "snap-1", "full", "")

	var buf bytes.Buffer
	manifest, err := ExportSnapshotBundle(workDir, "snap-1", &buf)
	if err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}
	if manifest.SnapshotID != "snap-1" || manifest.SnapshotType != "full" {
		t.Fatalf("manifest identity = %s/%s, want snap-1/full", manifest.SnapshotID, manifest.SnapshotType)
	}

	entries := readSnapshotBundleTar(t, buf.Bytes())
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	want := []string{"manifest.json", "memory.bin", "metadata.json", "rootfs.ext4", "state.bin"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tar entries = %v, want %v", names, want)
	}
}

func TestImportSnapshotBundleRebasesPathsAndPublishesAtomically(t *testing.T) {
	sourceWorkDir := t.TempDir()
	sourceMeta := writeSnapshotBundleFixture(t, sourceWorkDir, "snap-1", "full", "")

	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceWorkDir, "snap-1", &buf); err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}

	targetWorkDir := t.TempDir()
	result, err := ImportSnapshotBundle(targetWorkDir, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("import snapshot bundle: %v", err)
	}
	if result.Status != SnapshotImportStatusImported || result.Skipped {
		t.Fatalf("import result = status %q skipped %v, want imported/false", result.Status, result.Skipped)
	}

	targetSnapDir := SnapshotDir(targetWorkDir, "snap-1")
	importedMeta, err := LoadMetadata(targetSnapDir)
	if err != nil {
		t.Fatalf("load imported metadata: %v", err)
	}
	if importedMeta.MemFilePath != filepath.Join(targetSnapDir, "memory.bin") {
		t.Fatalf("MemFilePath = %q, want target snapshot path", importedMeta.MemFilePath)
	}
	if importedMeta.StatFilePath != filepath.Join(targetSnapDir, "state.bin") {
		t.Fatalf("StatFilePath = %q, want target snapshot path", importedMeta.StatFilePath)
	}
	if importedMeta.DiskCopyPath != filepath.Join(targetSnapDir, "rootfs.ext4") {
		t.Fatalf("DiskCopyPath = %q, want target snapshot path", importedMeta.DiskCopyPath)
	}
	if importedMeta.DiskPath != sourceMeta.DiskPath || importedMeta.VsockPath != sourceMeta.VsockPath {
		t.Fatalf("Firecracker path fields changed: got disk %q vsock %q", importedMeta.DiskPath, importedMeta.VsockPath)
	}
	if _, err := os.Stat(filepath.Join(targetSnapDir, "manifest.json")); err != nil {
		t.Fatalf("published manifest.json missing: %v", err)
	}
}

func TestImportSnapshotBundleRejectsDiffWithoutBaseAndCleansTemp(t *testing.T) {
	sourceWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-diff", "diff", "base-missing")

	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceWorkDir, "snap-diff", &buf); err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}

	targetWorkDir := t.TempDir()
	err := importSnapshotBundleDiscard(targetWorkDir, buf.Bytes())
	if !errors.Is(err, ErrDiffBaseMissing) {
		t.Fatalf("import error = %v, want ErrDiffBaseMissing", err)
	}

	entries, readErr := os.ReadDir(filepath.Join(targetWorkDir, "snapshots"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read snapshots dir: %v", readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".import-") {
			t.Fatalf("staging directory was not cleaned: %s", entry.Name())
		}
	}
}

func TestImportSnapshotBundleIdempotentAndConflict(t *testing.T) {
	sourceWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-1", "full", "")

	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceWorkDir, "snap-1", &buf); err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}
	bundleBytes := buf.Bytes()

	targetWorkDir := t.TempDir()
	if _, err := ImportSnapshotBundle(targetWorkDir, bytes.NewReader(bundleBytes)); err != nil {
		t.Fatalf("first import snapshot bundle: %v", err)
	}

	result, err := ImportSnapshotBundle(targetWorkDir, bytes.NewReader(bundleBytes))
	if err != nil {
		t.Fatalf("idempotent import snapshot bundle: %v", err)
	}
	if result.Status != SnapshotImportStatusAlreadyPresent || !result.Skipped {
		t.Fatalf("idempotent result = status %q skipped %v, want already_present/true", result.Status, result.Skipped)
	}

	targetRootFS := filepath.Join(SnapshotDir(targetWorkDir, "snap-1"), "rootfs.ext4")
	if err := os.WriteFile(targetRootFS, []byte("mutated-rootfs"), 0600); err != nil {
		t.Fatalf("mutate target rootfs: %v", err)
	}
	_, err = ImportSnapshotBundle(targetWorkDir, bytes.NewReader(bundleBytes))
	if !errors.Is(err, ErrSnapshotBundleConflict) {
		t.Fatalf("conflict import error = %v, want ErrSnapshotBundleConflict", err)
	}
}

func TestSnapshotExportManifestDoesNotExposeAgentToken(t *testing.T) {
	workDir := t.TempDir()
	writeSnapshotBundleFixture(t, workDir, "snap-1", "full", "")

	var buf bytes.Buffer
	manifest, err := ExportSnapshotBundle(workDir, "snap-1", &buf)
	if err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if strings.Contains(string(manifestJSON), snapshotBundleFixtureToken) {
		t.Fatalf("manifest exposes agent token: %s", string(manifestJSON))
	}
}

func writeSnapshotBundleFixture(t *testing.T, workDir, snapshotID, snapshotType, baseSnapshotID string) SnapshotMetadata {
	t.Helper()

	snapDir := SnapshotDir(workDir, snapshotID)
	if err := os.MkdirAll(snapDir, 0700); err != nil {
		t.Fatalf("create snapshot dir: %v", err)
	}

	files := map[string][]byte{
		"memory.bin":  []byte("memory-" + snapshotID),
		"state.bin":   []byte("state-" + snapshotID),
		"rootfs.ext4": []byte("rootfs-" + snapshotID),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(snapDir, name), content, 0600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	meta := SnapshotMetadata{
		SnapshotID:     snapshotID,
		SourceVMID:     "vm-" + snapshotID,
		TenantID:       "tenant-1",
		Profile:        "researcher",
		EgressPolicy:   "restricted",
		SnapshotType:   snapshotType,
		BaseSnapshotID: baseSnapshotID,
		GuestIP:        "10.0.1.11",
		TapDevice:      "tap-snap",
		VsockPath:      "/tmp/firecracker-vsock-" + snapshotID + ".sock",
		MacAddr:        "AA:FC:00:00:00:11",
		AgentToken:     snapshotBundleFixtureToken,
		DiskPath:       filepath.Join(workDir, "workspaces", snapshotID+".ext4"),
		MemFilePath:    filepath.Join(snapDir, "memory.bin"),
		StatFilePath:   filepath.Join(snapDir, "state.bin"),
		DiskCopyPath:   filepath.Join(snapDir, "rootfs.ext4"),
		CreatedAt:      time.Date(2026, 6, 2, 1, 2, 3, 0, time.UTC),
	}
	if err := SaveMetadata(snapDir, meta); err != nil {
		t.Fatalf("save metadata: %v", err)
	}
	return meta
}

func readSnapshotBundleTar(t *testing.T, bundle []byte) map[string][]byte {
	t.Helper()

	tr := tar.NewReader(bytes.NewReader(bundle))
	entries := make(map[string][]byte)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar header: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar entry %s: %v", header.Name, err)
		}
		entries[header.Name] = data
	}
	return entries
}

func importSnapshotBundleDiscard(workDir string, bundle []byte) error {
	_, err := ImportSnapshotBundle(workDir, bytes.NewReader(bundle))
	return err
}
