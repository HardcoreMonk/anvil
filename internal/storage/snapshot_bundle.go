package storage

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const SnapshotBundleContentType = "application/vnd.anvil.snapshot-bundle"

const (
	SnapshotImportStatusImported       = "imported"
	SnapshotImportStatusAlreadyPresent = "already_present"
)

var (
	ErrSnapshotBundleInvalid  = errors.New("invalid snapshot bundle")
	ErrSnapshotBundleConflict = errors.New("snapshot bundle conflicts with existing snapshot")
	ErrDiffBaseMissing        = errors.New("diff base snapshot missing")
)

type SnapshotBundleFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type SnapshotExportManifest struct {
	SnapshotID     string               `json:"snapshot_id"`
	SourceVMID     string               `json:"source_vm_id"`
	TenantID       string               `json:"tenant_id,omitempty"`
	Profile        string               `json:"profile,omitempty"`
	EgressPolicy   string               `json:"egress_policy,omitempty"`
	SnapshotType   string               `json:"snapshot_type"`
	BaseSnapshotID string               `json:"base_snapshot_id,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	Files          []SnapshotBundleFile `json:"files"`
}

type SnapshotImportResult struct {
	SnapshotID     string                 `json:"snapshot_id"`
	SnapshotType   string                 `json:"snapshot_type"`
	BaseSnapshotID string                 `json:"base_snapshot_id,omitempty"`
	Status         string                 `json:"status"`
	Skipped        bool                   `json:"skipped"`
	Manifest       SnapshotExportManifest `json:"-"`
	Metadata       SnapshotMetadata       `json:"-"`
}

const (
	snapshotBundleManifestPath = "manifest.json"
	snapshotBundleMetadataPath = "metadata.json"
	snapshotBundleMemoryPath   = "memory.bin"
	snapshotBundleStatePath    = "state.bin"
	snapshotBundleRootFSPath   = "rootfs.ext4"
)

var snapshotBundleAllowedFiles = map[string]struct{}{
	snapshotBundleManifestPath: {},
	snapshotBundleMetadataPath: {},
	snapshotBundleMemoryPath:   {},
	snapshotBundleStatePath:    {},
	snapshotBundleRootFSPath:   {},
}

var snapshotBundleChecksummedFiles = []string{
	snapshotBundleMetadataPath,
	snapshotBundleMemoryPath,
	snapshotBundleStatePath,
	snapshotBundleRootFSPath,
}

// ExportSnapshotBundle writes a snapshot bundle tar stream for daemon replication.
func ExportSnapshotBundle(workDir, snapshotID string, dst io.Writer) (SnapshotExportManifest, error) {
	snapDir := SnapshotDir(workDir, snapshotID)
	meta, err := LoadMetadata(snapDir)
	if err != nil {
		return SnapshotExportManifest{}, err
	}

	manifest := SnapshotExportManifest{
		SnapshotID:     meta.SnapshotID,
		SourceVMID:     meta.SourceVMID,
		TenantID:       meta.TenantID,
		Profile:        meta.Profile,
		EgressPolicy:   meta.EgressPolicy,
		SnapshotType:   meta.SnapshotType,
		BaseSnapshotID: meta.BaseSnapshotID,
		CreatedAt:      meta.CreatedAt,
	}
	for _, name := range snapshotBundleChecksummedFiles {
		info, err := snapshotBundleFileInfo(filepath.Join(snapDir, name), name)
		if err != nil {
			return SnapshotExportManifest{}, err
		}
		manifest.Files = append(manifest.Files, info)
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].Path < manifest.Files[j].Path
	})

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return SnapshotExportManifest{}, fmt.Errorf("marshal snapshot bundle manifest: %w", err)
	}

	tw := tar.NewWriter(dst)
	if err := snapshotBundleWriteBytes(tw, snapshotBundleManifestPath, manifestBytes, meta.CreatedAt); err != nil {
		tw.Close()
		return SnapshotExportManifest{}, err
	}
	for _, name := range []string{
		snapshotBundleMetadataPath,
		snapshotBundleMemoryPath,
		snapshotBundleStatePath,
		snapshotBundleRootFSPath,
	} {
		if err := snapshotBundleWriteFile(tw, filepath.Join(snapDir, name), name, meta.CreatedAt); err != nil {
			tw.Close()
			return SnapshotExportManifest{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return SnapshotExportManifest{}, fmt.Errorf("close snapshot bundle tar: %w", err)
	}
	return manifest, nil
}

// ImportSnapshotBundle imports a snapshot bundle tar stream into the target workDir.
func ImportSnapshotBundle(workDir string, src io.Reader) (SnapshotImportResult, error) {
	snapshotsDir := filepath.Join(workDir, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0700); err != nil {
		return SnapshotImportResult{}, fmt.Errorf("create snapshots dir: %w", err)
	}

	stageDir, err := os.MkdirTemp(snapshotsDir, ".import-")
	if err != nil {
		return SnapshotImportResult{}, fmt.Errorf("create snapshot import stage: %w", err)
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(stageDir)
		}
	}()

	if err := snapshotBundleReadTar(stageDir, src); err != nil {
		return SnapshotImportResult{}, err
	}

	manifest, err := snapshotBundleLoadManifest(filepath.Join(stageDir, snapshotBundleManifestPath))
	if err != nil {
		return SnapshotImportResult{}, err
	}
	meta, err := LoadMetadata(stageDir)
	if err != nil {
		return SnapshotImportResult{}, fmt.Errorf("%w: read metadata.json: %v", ErrSnapshotBundleInvalid, err)
	}
	if err := snapshotBundleValidateManifestMetadata(manifest, meta); err != nil {
		return SnapshotImportResult{}, err
	}
	if err := snapshotBundleValidateManifestFiles(stageDir, manifest); err != nil {
		return SnapshotImportResult{}, err
	}

	finalDir := SnapshotDir(workDir, manifest.SnapshotID)
	if existing, err := snapshotBundleCheckExisting(finalDir, manifest); err != nil {
		return SnapshotImportResult{}, err
	} else if existing {
		existingMeta, loadErr := LoadMetadata(finalDir)
		if loadErr != nil {
			return SnapshotImportResult{}, fmt.Errorf("%w: existing metadata unreadable: %v", ErrSnapshotBundleConflict, loadErr)
		}
		return SnapshotImportResult{
			SnapshotID:     manifest.SnapshotID,
			SnapshotType:   manifest.SnapshotType,
			BaseSnapshotID: manifest.BaseSnapshotID,
			Status:         SnapshotImportStatusAlreadyPresent,
			Skipped:        true,
			Manifest:       manifest,
			Metadata:       existingMeta,
		}, nil
	}

	if meta.SnapshotType == "diff" {
		if meta.BaseSnapshotID == "" {
			return SnapshotImportResult{}, fmt.Errorf("%w: empty base snapshot id", ErrDiffBaseMissing)
		}
		if _, err := os.Stat(SnapshotDir(workDir, meta.BaseSnapshotID)); err != nil {
			if os.IsNotExist(err) {
				return SnapshotImportResult{}, fmt.Errorf("%w: %s", ErrDiffBaseMissing, meta.BaseSnapshotID)
			}
			return SnapshotImportResult{}, fmt.Errorf("stat diff base snapshot: %w", err)
		}
	}

	meta.MemFilePath = filepath.Join(finalDir, snapshotBundleMemoryPath)
	meta.StatFilePath = filepath.Join(finalDir, snapshotBundleStatePath)
	meta.DiskCopyPath = filepath.Join(finalDir, snapshotBundleRootFSPath)
	if err := SaveMetadata(stageDir, meta); err != nil {
		return SnapshotImportResult{}, err
	}
	if err := os.Rename(stageDir, finalDir); err != nil {
		return SnapshotImportResult{}, fmt.Errorf("publish snapshot bundle: %w", err)
	}
	published = true

	return SnapshotImportResult{
		SnapshotID:     manifest.SnapshotID,
		SnapshotType:   manifest.SnapshotType,
		BaseSnapshotID: manifest.BaseSnapshotID,
		Status:         SnapshotImportStatusImported,
		Skipped:        false,
		Manifest:       manifest,
		Metadata:       meta,
	}, nil
}

func snapshotBundleWriteBytes(tw *tar.Writer, name string, data []byte, modTime time.Time) error {
	header := &tar.Header{
		Name:    name,
		Mode:    0600,
		Size:    int64(len(data)),
		ModTime: modTime,
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write tar entry %s: %w", name, err)
	}
	return nil
}

func snapshotBundleWriteFile(tw *tar.Writer, path, name string, modTime time.Time) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open snapshot bundle file %s: %w", name, err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat snapshot bundle file %s: %w", name, err)
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrSnapshotBundleInvalid, name)
	}

	header := &tar.Header{
		Name:    name,
		Mode:    0600,
		Size:    stat.Size(),
		ModTime: modTime,
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write tar header %s: %w", name, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("write tar entry %s: %w", name, err)
	}
	return nil
}

func snapshotBundleReadTar(stageDir string, src io.Reader) error {
	tr := tar.NewReader(src)
	seen := make(map[string]struct{})
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read tar: %v", ErrSnapshotBundleInvalid, err)
		}
		if err := snapshotBundleValidateTarHeader(header); err != nil {
			return err
		}
		if _, exists := seen[header.Name]; exists {
			return fmt.Errorf("%w: duplicate tar entry %s", ErrSnapshotBundleInvalid, header.Name)
		}
		seen[header.Name] = struct{}{}

		dstPath := filepath.Join(stageDir, header.Name)
		out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("create staged bundle file %s: %w", header.Name, err)
		}
		written, copyErr := io.Copy(out, tr)
		closeErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("%w: copy tar entry %s: %v", ErrSnapshotBundleInvalid, header.Name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close staged bundle file %s: %w", header.Name, closeErr)
		}
		if written != header.Size {
			return fmt.Errorf("%w: tar entry %s size mismatch", ErrSnapshotBundleInvalid, header.Name)
		}
	}
	for name := range snapshotBundleAllowedFiles {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("%w: missing %s", ErrSnapshotBundleInvalid, name)
		}
	}
	return nil
}

func snapshotBundleValidateTarHeader(header *tar.Header) error {
	if header.Name == "" {
		return fmt.Errorf("%w: empty tar entry name", ErrSnapshotBundleInvalid)
	}
	if filepath.IsAbs(header.Name) || filepath.Clean(header.Name) != header.Name {
		return fmt.Errorf("%w: unsafe tar entry %s", ErrSnapshotBundleInvalid, header.Name)
	}
	if _, ok := snapshotBundleAllowedFiles[header.Name]; !ok {
		return fmt.Errorf("%w: unexpected tar entry %s", ErrSnapshotBundleInvalid, header.Name)
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
		return fmt.Errorf("%w: tar entry %s is not a regular file", ErrSnapshotBundleInvalid, header.Name)
	}
	if header.Size < 0 {
		return fmt.Errorf("%w: tar entry %s has negative size", ErrSnapshotBundleInvalid, header.Name)
	}
	return nil
}

func snapshotBundleLoadManifest(path string) (SnapshotExportManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SnapshotExportManifest{}, fmt.Errorf("%w: read manifest.json: %v", ErrSnapshotBundleInvalid, err)
	}
	var manifest SnapshotExportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return SnapshotExportManifest{}, fmt.Errorf("%w: parse manifest.json: %v", ErrSnapshotBundleInvalid, err)
	}
	return manifest, nil
}

func snapshotBundleValidateManifestMetadata(manifest SnapshotExportManifest, meta SnapshotMetadata) error {
	if manifest.SnapshotID == "" {
		return fmt.Errorf("%w: empty snapshot id", ErrSnapshotBundleInvalid)
	}
	if manifest.SnapshotID != meta.SnapshotID {
		return fmt.Errorf("%w: snapshot id mismatch", ErrSnapshotBundleInvalid)
	}
	if manifest.SnapshotType != meta.SnapshotType {
		return fmt.Errorf("%w: snapshot type mismatch", ErrSnapshotBundleInvalid)
	}
	if manifest.BaseSnapshotID != meta.BaseSnapshotID {
		return fmt.Errorf("%w: base snapshot id mismatch", ErrSnapshotBundleInvalid)
	}
	if !manifest.CreatedAt.Equal(meta.CreatedAt) {
		return fmt.Errorf("%w: created_at mismatch", ErrSnapshotBundleInvalid)
	}
	return nil
}

func snapshotBundleValidateManifestFiles(dir string, manifest SnapshotExportManifest) error {
	manifestFiles := make(map[string]SnapshotBundleFile)
	for _, file := range manifest.Files {
		if _, ok := snapshotBundleAllowedChecksumFile(file.Path); !ok {
			return fmt.Errorf("%w: unexpected manifest file %s", ErrSnapshotBundleInvalid, file.Path)
		}
		if _, exists := manifestFiles[file.Path]; exists {
			return fmt.Errorf("%w: duplicate manifest file %s", ErrSnapshotBundleInvalid, file.Path)
		}
		manifestFiles[file.Path] = file
	}

	for _, name := range snapshotBundleChecksummedFiles {
		want, ok := manifestFiles[name]
		if !ok {
			return fmt.Errorf("%w: manifest missing %s", ErrSnapshotBundleInvalid, name)
		}
		got, err := snapshotBundleFileInfo(filepath.Join(dir, name), name)
		if err != nil {
			return fmt.Errorf("%w: checksum %s: %v", ErrSnapshotBundleInvalid, name, err)
		}
		if got.SizeBytes != want.SizeBytes || got.SHA256 != want.SHA256 {
			return fmt.Errorf("%w: checksum mismatch for %s", ErrSnapshotBundleInvalid, name)
		}
	}
	return nil
}

func snapshotBundleAllowedChecksumFile(path string) (struct{}, bool) {
	for _, name := range snapshotBundleChecksummedFiles {
		if path == name {
			return struct{}{}, true
		}
	}
	return struct{}{}, false
}

func snapshotBundleCheckExisting(finalDir string, incoming SnapshotExportManifest) (bool, error) {
	stat, err := os.Stat(finalDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat existing snapshot: %w", err)
	}
	if !stat.IsDir() {
		return false, fmt.Errorf("%w: existing snapshot path is not a directory", ErrSnapshotBundleConflict)
	}

	existingManifest, err := snapshotBundleLoadManifest(filepath.Join(finalDir, snapshotBundleManifestPath))
	if err != nil {
		return false, fmt.Errorf("%w: existing manifest mismatch", ErrSnapshotBundleConflict)
	}
	if !snapshotBundleSamePublicIdentity(existingManifest, incoming) {
		return false, ErrSnapshotBundleConflict
	}
	if !snapshotBundleSameArtifactManifest(existingManifest, incoming) {
		return false, ErrSnapshotBundleConflict
	}
	if err := snapshotBundleCompareCurrentArtifacts(finalDir, incoming); err != nil {
		return false, err
	}
	return true, nil
}

func snapshotBundleSamePublicIdentity(a, b SnapshotExportManifest) bool {
	return a.SnapshotID == b.SnapshotID &&
		a.SourceVMID == b.SourceVMID &&
		a.TenantID == b.TenantID &&
		a.Profile == b.Profile &&
		a.EgressPolicy == b.EgressPolicy &&
		a.SnapshotType == b.SnapshotType &&
		a.BaseSnapshotID == b.BaseSnapshotID &&
		a.CreatedAt.Equal(b.CreatedAt)
}

func snapshotBundleSameArtifactManifest(a, b SnapshotExportManifest) bool {
	return snapshotBundleArtifactFileMap(a).Equal(snapshotBundleArtifactFileMap(b))
}

type snapshotBundleFileMap map[string]SnapshotBundleFile

func (m snapshotBundleFileMap) Equal(other snapshotBundleFileMap) bool {
	if len(m) != len(other) {
		return false
	}
	for path, file := range m {
		otherFile, ok := other[path]
		if !ok || file.SizeBytes != otherFile.SizeBytes || file.SHA256 != otherFile.SHA256 {
			return false
		}
	}
	return true
}

func snapshotBundleArtifactFileMap(manifest SnapshotExportManifest) snapshotBundleFileMap {
	files := make(snapshotBundleFileMap)
	for _, file := range manifest.Files {
		if file.Path == snapshotBundleMemoryPath || file.Path == snapshotBundleStatePath || file.Path == snapshotBundleRootFSPath {
			files[file.Path] = file
		}
	}
	return files
}

func snapshotBundleCompareCurrentArtifacts(finalDir string, incoming SnapshotExportManifest) error {
	want := snapshotBundleArtifactFileMap(incoming)
	if len(want) != 3 {
		return fmt.Errorf("%w: incoming manifest missing artifact checksums", ErrSnapshotBundleInvalid)
	}
	for _, name := range []string{snapshotBundleMemoryPath, snapshotBundleStatePath, snapshotBundleRootFSPath} {
		wantFile, ok := want[name]
		if !ok {
			return fmt.Errorf("%w: incoming manifest missing %s", ErrSnapshotBundleInvalid, name)
		}
		got, err := snapshotBundleFileInfo(filepath.Join(finalDir, name), name)
		if err != nil {
			return fmt.Errorf("%w: existing artifact unreadable: %v", ErrSnapshotBundleConflict, err)
		}
		if got.SizeBytes != wantFile.SizeBytes || got.SHA256 != wantFile.SHA256 {
			return ErrSnapshotBundleConflict
		}
	}
	return nil
}

func snapshotBundleFileInfo(path, bundlePath string) (SnapshotBundleFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return SnapshotBundleFile{}, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return SnapshotBundleFile{}, err
	}
	if !stat.Mode().IsRegular() {
		return SnapshotBundleFile{}, fmt.Errorf("%s is not a regular file", bundlePath)
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return SnapshotBundleFile{}, err
	}
	return SnapshotBundleFile{
		Path:      bundlePath,
		SizeBytes: stat.Size(),
		SHA256:    hex.EncodeToString(hash.Sum(nil)),
	}, nil
}
