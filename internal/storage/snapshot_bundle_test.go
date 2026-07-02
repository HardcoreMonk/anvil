package storage

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-1", "full", "")

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
	if importedMeta.DiskPath != filepath.Join(os.TempDir(), "goose-workspaces", "snap-1.ext4") {
		t.Fatalf("DiskPath = %q, want preserved safe workspace path", importedMeta.DiskPath)
	}
	if importedMeta.VsockPath != filepath.Join(os.TempDir(), "firecracker-vsock-snap-1.sock") {
		t.Fatalf("VsockPath = %q, want preserved safe tmp path", importedMeta.VsockPath)
	}
	if importedMeta.AgentToken != "" {
		t.Fatalf("AgentToken = %q, want scrubbed", importedMeta.AgentToken)
	}
	if _, err := os.Stat(filepath.Join(targetSnapDir, "manifest.json")); err != nil {
		t.Fatalf("published manifest.json missing: %v", err)
	}

	publishedManifest := loadSnapshotBundleManifestFixture(t, filepath.Join(targetSnapDir, "manifest.json"))
	metadataInfo, err := snapshotBundleFileInfo(filepath.Join(targetSnapDir, "metadata.json"), "metadata.json")
	if err != nil {
		t.Fatalf("checksum final metadata: %v", err)
	}
	publishedMetadataEntry := snapshotBundleManifestFileFixture(t, publishedManifest, "metadata.json")
	if publishedMetadataEntry.SizeBytes != metadataInfo.SizeBytes || publishedMetadataEntry.SHA256 != metadataInfo.SHA256 {
		t.Fatalf("published manifest metadata checksum = %d/%s, want %d/%s",
			publishedMetadataEntry.SizeBytes, publishedMetadataEntry.SHA256, metadataInfo.SizeBytes, metadataInfo.SHA256)
	}
	resultMetadataEntry := snapshotBundleManifestFileFixture(t, result.Manifest, "metadata.json")
	if resultMetadataEntry.SizeBytes != metadataInfo.SizeBytes || resultMetadataEntry.SHA256 != metadataInfo.SHA256 {
		t.Fatalf("result manifest metadata checksum = %d/%s, want %d/%s",
			resultMetadataEntry.SizeBytes, resultMetadataEntry.SHA256, metadataInfo.SizeBytes, metadataInfo.SHA256)
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

func TestExportImportDiffSnapshotBundleUsesRootfsDiff(t *testing.T) {
	sourceWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-base", "full", "")
	diffMeta := writeSnapshotBundleFixture(t, sourceWorkDir, "snap-diff", "diff", "snap-base")
	diffDir := SnapshotDir(sourceWorkDir, "snap-diff")
	if err := os.Remove(filepath.Join(diffDir, "rootfs.ext4")); err != nil {
		t.Fatalf("remove legacy full rootfs from diff fixture: %v", err)
	}
	rootfsDiffPath := filepath.Join(diffDir, "rootfs.diff")
	if err := os.WriteFile(rootfsDiffPath, []byte("rootfs-diff-snap-diff"), 0600); err != nil {
		t.Fatalf("write rootfs diff: %v", err)
	}
	diffMeta.DiskCopyPath = ""
	diffMeta.RootfsDiffPath = rootfsDiffPath
	if err := SaveMetadata(diffDir, diffMeta); err != nil {
		t.Fatalf("save diff metadata: %v", err)
	}

	var buf bytes.Buffer
	manifest, err := ExportSnapshotBundle(sourceWorkDir, "snap-diff", &buf)
	if err != nil {
		t.Fatalf("export diff snapshot bundle: %v", err)
	}
	if snapshotBundleManifestFileFixture(t, manifest, "rootfs.diff").Path != "rootfs.diff" {
		t.Fatalf("manifest missing rootfs.diff: %+v", manifest.Files)
	}
	entries := readSnapshotBundleTar(t, buf.Bytes())
	if _, ok := entries["rootfs.diff"]; !ok {
		t.Fatalf("bundle missing rootfs.diff entries=%v", sortedBundleEntryNames(entries))
	}
	if _, ok := entries["rootfs.ext4"]; ok {
		t.Fatalf("diff bundle unexpectedly contains rootfs.ext4 entries=%v", sortedBundleEntryNames(entries))
	}

	targetWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, targetWorkDir, "snap-base", "full", "")
	result, err := ImportSnapshotBundle(targetWorkDir, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("import diff snapshot bundle: %v", err)
	}
	targetSnapDir := SnapshotDir(targetWorkDir, "snap-diff")
	if result.Metadata.RootfsDiffPath != filepath.Join(targetSnapDir, "rootfs.diff") {
		t.Fatalf("imported RootfsDiffPath = %q, want target rootfs.diff", result.Metadata.RootfsDiffPath)
	}
	if result.Metadata.DiskCopyPath != "" {
		t.Fatalf("imported DiskCopyPath = %q, want empty for rootfs diff snapshot", result.Metadata.DiskCopyPath)
	}
	if _, err := os.Stat(filepath.Join(targetSnapDir, "rootfs.diff")); err != nil {
		t.Fatalf("published rootfs.diff missing: %v", err)
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

	targetSnapDir := SnapshotDir(targetWorkDir, "snap-1")
	existingMeta, err := LoadMetadata(targetSnapDir)
	if err != nil {
		t.Fatalf("load target metadata: %v", err)
	}
	existingMeta.Profile = "corrupted"
	if err := SaveMetadata(targetSnapDir, existingMeta); err != nil {
		t.Fatalf("corrupt target metadata: %v", err)
	}
	_, err = ImportSnapshotBundle(targetWorkDir, bytes.NewReader(bundleBytes))
	if !errors.Is(err, ErrSnapshotBundleConflict) {
		t.Fatalf("metadata conflict import error = %v, want ErrSnapshotBundleConflict", err)
	}
	existingMeta.Profile = "researcher"
	if err := SaveMetadata(targetSnapDir, existingMeta); err != nil {
		t.Fatalf("restore target metadata: %v", err)
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

func sortedBundleEntryNames(entries map[string][]byte) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestImportSnapshotBundleScrubsUnsafeMetadataFields(t *testing.T) {
	sourceWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-1", "full", "")

	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceWorkDir, "snap-1", &buf); err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}
	bundleBytes := mutateSnapshotBundle(t, buf.Bytes(), func(manifest *SnapshotExportManifest, meta *SnapshotMetadata) {
		meta.AgentToken = "imported-secret"
		meta.DiskPath = "/etc/passwd"
		meta.VsockPath = "/var/run/unsafe.sock"
	})

	err := importSnapshotBundleDiscard(t.TempDir(), bundleBytes)
	if !errors.Is(err, ErrSnapshotBundleInvalid) {
		t.Fatalf("import error = %v, want ErrSnapshotBundleInvalid", err)
	}
}

func TestImportSnapshotBundleRejectsPathLikeSnapshotID(t *testing.T) {
	sourceWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-1", "full", "")

	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceWorkDir, "snap-1", &buf); err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}
	bundleBytes := mutateSnapshotBundleID(t, buf.Bytes(), "../outside")

	targetWorkDir := t.TempDir()
	err := importSnapshotBundleDiscard(targetWorkDir, bundleBytes)
	if !errors.Is(err, ErrSnapshotBundleInvalid) {
		t.Fatalf("import error = %v, want ErrSnapshotBundleInvalid", err)
	}
	if _, err := os.Stat(filepath.Join(targetWorkDir, "outside")); !os.IsNotExist(err) {
		t.Fatalf("path-like snapshot id created %s: %v", filepath.Join(targetWorkDir, "outside"), err)
	}
	if _, err := os.Stat(filepath.Join(targetWorkDir, "snapshots", "outside")); !os.IsNotExist(err) {
		t.Fatalf("path-like snapshot id created %s: %v", filepath.Join(targetWorkDir, "snapshots", "outside"), err)
	}
}

func TestImportSnapshotBundleRejectsBogusSnapshotType(t *testing.T) {
	sourceWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-1", "full", "")

	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceWorkDir, "snap-1", &buf); err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}
	bundleBytes := mutateSnapshotBundle(t, buf.Bytes(), func(manifest *SnapshotExportManifest, meta *SnapshotMetadata) {
		manifest.SnapshotType = "bogus"
		meta.SnapshotType = "bogus"
	})

	err := importSnapshotBundleDiscard(t.TempDir(), bundleBytes)
	if !errors.Is(err, ErrSnapshotBundleInvalid) {
		t.Fatalf("import error = %v, want ErrSnapshotBundleInvalid", err)
	}
}

func TestImportSnapshotBundleRejectsFullSnapshotWithBaseID(t *testing.T) {
	sourceWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-1", "full", "")

	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceWorkDir, "snap-1", &buf); err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}
	bundleBytes := mutateSnapshotBundle(t, buf.Bytes(), func(manifest *SnapshotExportManifest, meta *SnapshotMetadata) {
		manifest.BaseSnapshotID = "base-1"
		meta.BaseSnapshotID = "base-1"
	})

	err := importSnapshotBundleDiscard(t.TempDir(), bundleBytes)
	if !errors.Is(err, ErrSnapshotBundleInvalid) {
		t.Fatalf("import error = %v, want ErrSnapshotBundleInvalid", err)
	}
}

func TestImportSnapshotBundleRejectsDiffWithInvalidBaseMetadata(t *testing.T) {
	sourceWorkDir := t.TempDir()
	writeSnapshotBundleFixture(t, sourceWorkDir, "snap-diff", "diff", "base-1")

	var buf bytes.Buffer
	if _, err := ExportSnapshotBundle(sourceWorkDir, "snap-diff", &buf); err != nil {
		t.Fatalf("export snapshot bundle: %v", err)
	}
	bundleBytes := buf.Bytes()

	t.Run("empty base dir", func(t *testing.T) {
		targetWorkDir := t.TempDir()
		if err := os.MkdirAll(SnapshotDir(targetWorkDir, "base-1"), 0700); err != nil {
			t.Fatalf("create empty base dir: %v", err)
		}
		err := importSnapshotBundleDiscard(targetWorkDir, bundleBytes)
		if !errors.Is(err, ErrDiffBaseMissing) {
			t.Fatalf("import error = %v, want ErrDiffBaseMissing", err)
		}
	})

	t.Run("non full base metadata", func(t *testing.T) {
		targetWorkDir := t.TempDir()
		writeSnapshotBundleFixture(t, targetWorkDir, "base-1", "diff", "base-0")
		err := importSnapshotBundleDiscard(targetWorkDir, bundleBytes)
		if !errors.Is(err, ErrDiffBaseMissing) {
			t.Fatalf("import error = %v, want ErrDiffBaseMissing", err)
		}
	})
}

func TestExportSnapshotBundleRejectsPathLikeSnapshotID(t *testing.T) {
	workDir := t.TempDir()
	outsideDir := filepath.Join(workDir, "outside")
	if err := os.MkdirAll(outsideDir, 0700); err != nil {
		t.Fatalf("create outside dir: %v", err)
	}
	for name, content := range map[string][]byte{
		"memory.bin":  []byte("outside-memory"),
		"state.bin":   []byte("outside-state"),
		"rootfs.ext4": []byte("outside-rootfs"),
	} {
		if err := os.WriteFile(filepath.Join(outsideDir, name), content, 0600); err != nil {
			t.Fatalf("write outside %s: %v", name, err)
		}
	}
	if err := SaveMetadata(outsideDir, SnapshotMetadata{
		SnapshotID:   "../outside",
		SourceVMID:   "vm-outside",
		SnapshotType: "full",
		MemFilePath:  filepath.Join(outsideDir, "memory.bin"),
		StatFilePath: filepath.Join(outsideDir, "state.bin"),
		DiskCopyPath: filepath.Join(outsideDir, "rootfs.ext4"),
		CreatedAt:    time.Date(2026, 6, 2, 1, 2, 3, 0, time.UTC),
	}); err != nil {
		t.Fatalf("save outside metadata: %v", err)
	}

	var buf bytes.Buffer
	_, err := ExportSnapshotBundle(workDir, "../outside", &buf)
	if !errors.Is(err, ErrSnapshotBundleInvalid) {
		t.Fatalf("export error = %v, want ErrSnapshotBundleInvalid", err)
	}
}

func TestSnapshotExportBundleDoesNotExposeAgentTokenInMetadata(t *testing.T) {
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
	entries := readSnapshotBundleTar(t, buf.Bytes())
	if strings.Contains(string(entries["metadata.json"]), snapshotBundleFixtureToken) {
		t.Fatalf("bundle metadata exposes agent token: %s", string(entries["metadata.json"]))
	}
	var bundleMeta SnapshotMetadata
	if err := json.Unmarshal(entries["metadata.json"], &bundleMeta); err != nil {
		t.Fatalf("parse bundle metadata: %v", err)
	}
	if bundleMeta.AgentToken != "" {
		t.Fatalf("bundle metadata AgentToken = %q, want empty", bundleMeta.AgentToken)
	}
	if bundleMeta.DiskPath != filepath.Join(os.TempDir(), "goose-workspaces", "snap-1.ext4") {
		t.Fatalf("bundle metadata DiskPath = %q, want safe workspace path", bundleMeta.DiskPath)
	}
	if bundleMeta.VsockPath != filepath.Join(os.TempDir(), "firecracker-vsock-snap-1.sock") {
		t.Fatalf("bundle metadata VsockPath = %q, want safe tmp path", bundleMeta.VsockPath)
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
		VsockPath:      filepath.Join(os.TempDir(), "firecracker-vsock-"+snapshotID+".sock"),
		MacAddr:        "AA:FC:00:00:00:11",
		AgentToken:     snapshotBundleFixtureToken,
		DiskPath:       filepath.Join(os.TempDir(), "goose-workspaces", snapshotID+".ext4"),
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

func mutateSnapshotBundleID(t *testing.T, bundle []byte, snapshotID string) []byte {
	t.Helper()

	return mutateSnapshotBundle(t, bundle, func(manifest *SnapshotExportManifest, meta *SnapshotMetadata) {
		manifest.SnapshotID = snapshotID
		meta.SnapshotID = snapshotID
	})
}

func mutateSnapshotBundle(t *testing.T, bundle []byte, mutate func(*SnapshotExportManifest, *SnapshotMetadata)) []byte {
	t.Helper()

	entries := readSnapshotBundleTar(t, bundle)

	var manifest SnapshotExportManifest
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("parse manifest fixture: %v", err)
	}

	var meta SnapshotMetadata
	if err := json.Unmarshal(entries["metadata.json"], &meta); err != nil {
		t.Fatalf("parse metadata fixture: %v", err)
	}

	mutate(&manifest, &meta)

	metadataBytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal mutated metadata: %v", err)
	}
	entries["metadata.json"] = metadataBytes

	for i := range manifest.Files {
		if manifest.Files[i].Path == "metadata.json" {
			sum := sha256.Sum256(metadataBytes)
			manifest.Files[i].SizeBytes = int64(len(metadataBytes))
			manifest.Files[i].SHA256 = hex.EncodeToString(sum[:])
		}
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal mutated manifest with metadata checksum: %v", err)
	}
	entries["manifest.json"] = manifestBytes

	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, name := range []string{"manifest.json", "metadata.json", "memory.bin", "state.bin", "rootfs.ext4"} {
		data := entries[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			t.Fatalf("write mutated tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write mutated tar entry %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close mutated tar: %v", err)
	}
	return out.Bytes()
}

func loadSnapshotBundleManifestFixture(t *testing.T, path string) SnapshotExportManifest {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest fixture: %v", err)
	}
	var manifest SnapshotExportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest fixture: %v", err)
	}
	return manifest
}

func snapshotBundleManifestFileFixture(t *testing.T, manifest SnapshotExportManifest, path string) SnapshotBundleFile {
	t.Helper()

	for _, file := range manifest.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("manifest missing file %s", path)
	return SnapshotBundleFile{}
}
