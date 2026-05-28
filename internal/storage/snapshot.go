package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// SnapshotMetadata holds everything needed to restore a VM from a snapshot.
// Stored as metadata.json inside the snapshot directory.
type SnapshotMetadata struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile"`
	SnapshotType   string    `json:"snapshot_type"`              // "full" | "diff"
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"` // set for diff snapshots
	GuestIP        string    `json:"guest_ip"`
	TapDevice      string    `json:"tap_device"` // original TAP device name; Firecracker v1.x embeds this in state.bin
	VsockPath      string    `json:"vsock_path"` // original vsock UDS path; Firecracker recreates it from snapshot state on restore
	MacAddr        string    `json:"mac_addr"`
	AgentToken     string    `json:"agent_token"`
	DiskPath       string    `json:"disk_path"`                  // original workspace path (required by Firecracker on restore)
	MemFilePath    string    `json:"mem_file_path"`              // absolute path to memory.bin (or sparse diff)
	StatFilePath   string    `json:"stat_file_path"`             // absolute path to state.bin
	DiskCopyPath   string    `json:"disk_copy_path"`             // full rootfs copy inside snapshot dir (full snapshots; empty for v0.4.0 diff snapshots)
	RootfsDiffPath string    `json:"rootfs_diff_path,omitempty"` // v0.4.0+ diff: sparse rootfs delta vs base.DiskCopyPath
	CreatedAt      time.Time `json:"created_at"`
}

// SnapshotDir returns the canonical directory for a given snapshot ID.
func SnapshotDir(workDir, snapshotID string) string {
	return filepath.Join(workDir, "snapshots", snapshotID)
}

// SaveMetadata writes metadata.json into the snapshot directory.
func SaveMetadata(snapDir string, meta SnapshotMetadata) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot metadata: %w", err)
	}
	return os.WriteFile(filepath.Join(snapDir, "metadata.json"), b, 0600)
}

// LoadMetadata reads and parses metadata.json from a snapshot directory.
func LoadMetadata(snapDir string) (SnapshotMetadata, error) {
	b, err := os.ReadFile(filepath.Join(snapDir, "metadata.json"))
	if err != nil {
		return SnapshotMetadata{}, fmt.Errorf("read snapshot metadata: %w", err)
	}
	var meta SnapshotMetadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return SnapshotMetadata{}, fmt.Errorf("parse snapshot metadata: %w", err)
	}
	return meta, nil
}

// ListSnapshots scans {workDir}/snapshots/ and returns all valid snapshot metadata entries.
func ListSnapshots(workDir string) ([]SnapshotMetadata, error) {
	base := filepath.Join(workDir, "snapshots")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list snapshots dir: %w", err)
	}

	var results []SnapshotMetadata
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := LoadMetadata(filepath.Join(base, e.Name()))
		if err != nil {
			continue // skip corrupted entries silently
		}
		results = append(results, meta)
	}
	return results, nil
}

// DeleteSnapshot removes the entire snapshot directory including all files.
func DeleteSnapshot(snapDir string) error {
	if err := os.RemoveAll(snapDir); err != nil {
		return fmt.Errorf("delete snapshot dir %s: %w", snapDir, err)
	}
	return nil
}

// CopyDiskToSnapshot copies the VM disk into the snapshot directory as rootfs.ext4
// (the full read-only base for full snapshots). Uses reflink/sparse copy so the ~22%
// sparse golden image stays sparse and the copy is instant on btrfs/XFS. Returns the
// destination path.
func CopyDiskToSnapshot(diskPath, snapDir string) (string, error) {
	dst := filepath.Join(snapDir, "rootfs.ext4")
	if err := reflinkOrSparseCopy(diskPath, dst); err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("copy disk to snapshot: %w", err)
	}
	return dst, nil
}

// RestoreDiskFromSnapshot copies the snapshot's rootfs.ext4 back to the original disk path.
// Kept for reference; prefer SetupBindMount for concurrent-restore support.
func RestoreDiskFromSnapshot(diskCopyPath, originalDiskPath string) error {
	if err := os.MkdirAll(filepath.Dir(originalDiskPath), 0755); err != nil {
		return fmt.Errorf("create workspace dir: %w", err)
	}

	src, err := os.Open(diskCopyPath)
	if err != nil {
		return fmt.Errorf("open snapshot disk copy: %w", err)
	}
	defer src.Close()

	out, err := os.Create(originalDiskPath)
	if err != nil {
		return fmt.Errorf("create restored disk: %w", err)
	}

	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(originalDiskPath)
		return fmt.Errorf("restore disk from snapshot: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(originalDiskPath)
		return fmt.Errorf("flush restored disk: %w", err)
	}
	return nil
}

// SetupBindMount prepares the disk for a snapshot restore using Linux bind mounts.
//
// It copies the snapshot disk to newDiskPath (a unique per-restore file), ensures the
// bind mount target (mountTargetPath = the original VM's disk path recorded in state.bin)
// exists as a regular file, then bind-mounts newDiskPath over mountTargetPath.
//
// Firecracker opens mountTargetPath and transparently reads/writes newDiskPath through
// the bind mount. Because each restore uses a distinct newDiskPath, multiple concurrent
// restores of the same snapshot are safe: each Firecracker acquires its own file
// descriptor to its own inode, and subsequent bind mounts simply stack on the same
// target without disturbing open descriptors already held by earlier restores.
//
// Callers must hold a per-snapshot lock across SetupBindMount + RestoreMachine.Start()
// to ensure each Firecracker opens the correct (topmost) bind mount.
func SetupBindMount(diskCopyPath, newDiskPath, mountTargetPath string) error {
	if err := os.MkdirAll(filepath.Dir(newDiskPath), 0755); err != nil {
		return fmt.Errorf("create workspace dir: %w", err)
	}

	// Copy snapshot disk to the unique per-restore path.
	if err := copyFile(diskCopyPath, newDiskPath); err != nil {
		return fmt.Errorf("copy snapshot disk to restore path: %w", err)
	}

	// Ensure mountTargetPath exists as a regular file (required bind mount target).
	// The file may already exist as an empty placeholder from a previous restore.
	if _, err := os.Stat(mountTargetPath); os.IsNotExist(err) {
		if err := os.WriteFile(mountTargetPath, nil, 0644); err != nil {
			os.Remove(newDiskPath)
			return fmt.Errorf("create bind mount target: %w", err)
		}
	}

	// Bind mount: Firecracker opens mountTargetPath → reads/writes newDiskPath.
	if out, err := exec.Command("mount", "--bind", newDiskPath, mountTargetPath).CombinedOutput(); err != nil {
		os.Remove(newDiskPath)
		return fmt.Errorf("bind mount %s → %s: %w: %s", newDiskPath, mountTargetPath, err, out)
	}

	return nil
}

// TeardownBindMount detaches a bind mount and removes the per-restore disk file.
//
// Uses lazy unmount (-l) so the target path is detached from the directory tree
// immediately, while any Firecracker process that already has an open file descriptor
// to the underlying inode continues to work uninterrupted until it exits.
//
// When called out of bind-mount stack order (i.e. not strictly LIFO), the lazy
// unmount removes the topmost stacked mount — which may belong to a different restore.
// This is safe: that restore's Firecracker already holds a file descriptor to its own
// inode, so losing the path binding does not corrupt its disk I/O. The actual disk
// bytes are freed only when both the fd is closed and os.Remove has unlinked the file.
func TeardownBindMount(mountTargetPath, newDiskPath string) {
	exec.Command("umount", "-l", mountTargetPath).Run()
	if err := os.Remove(newDiskPath); err != nil && !os.IsNotExist(err) {
		// Log but do not fail — the inode will be freed when all fds are closed.
		_ = err
	}
}

// DMSnapshotInfo holds the kernel objects created by SetupDMSnapshot.
// All fields are required for correct teardown via TeardownDMSnapshot.
type DMSnapshotInfo struct {
	LoopDevice     string // read-only loop device for the base disk (e.g. "/dev/loop3")
	COWLoopDevice  string // read-write loop device for the exception store (e.g. "/dev/loop4")
	DMDevice       string // dm-snapshot device name (e.g. "cow-vm-xxx")
	ExceptionStore string // sparse COW store file (grows on write, initially ~0 bytes)
	MountTarget    string // original disk path (from Firecracker state.bin)
}

// DMSnapshotAvailable reports whether the host can build dm-snapshot COW devices:
// the losetup/dmsetup/blockdev tools must be on PATH, the device-mapper control
// node must be usable, and the dm_snapshot target must be loadable. Returns a
// descriptive error when COW is unsupported so callers can fall back to a full copy.
func DMSnapshotAvailable() error {
	for _, tool := range []string{"losetup", "dmsetup", "blockdev"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found on PATH: %w", tool, err)
		}
	}
	// `dmsetup version` actually talks to the kernel device-mapper driver, so it
	// catches a missing /dev/mapper/control or an incompatible driver.
	if out, err := exec.Command("dmsetup", "version").CombinedOutput(); err != nil {
		return fmt.Errorf("dmsetup/device-mapper unusable: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Load the dm_snapshot target (idempotent). If modprobe is absent but the
	// target is built into the kernel, /sys/module/dm_snapshot still confirms it.
	if err := exec.Command("modprobe", "dm_snapshot").Run(); err != nil {
		if _, statErr := os.Stat("/sys/module/dm_snapshot"); statErr != nil {
			return fmt.Errorf("dm_snapshot target unavailable: %w", err)
		}
	}
	return nil
}

// SetupDMSnapshot creates a block-level COW view of the snapshot's rootfs using Linux
// device mapper snapshot. The base disk is read-only; all writes from the restored VM
// accumulate in a sparse exception store. This eliminates the ~700 MB full copy that
// SetupBindMount previously required.
//
// The COW device is bind-mounted over mountTargetPath (the original disk path recorded
// in Firecracker state.bin) so that Firecracker can open the path as before.
//
// Caller must call TeardownDMSnapshot on VM destroy to release kernel resources.
func SetupDMSnapshot(baseDiskPath, exceptionStorePath, mountTargetPath string) (*DMSnapshotInfo, error) {
	// 1. Create read-only loop device for the snapshot base disk.
	out, err := exec.Command("losetup", "--read-only", "--find", "--show", baseDiskPath).Output()
	if err != nil {
		return nil, fmt.Errorf("losetup for base disk: %w", err)
	}
	loopDev := strings.TrimSpace(string(out))

	cleanup := func() { exec.Command("losetup", "-d", loopDev).Run() }

	// 2. Get the sector count of the loop device.
	sectorOut, err := exec.Command("blockdev", "--getsz", loopDev).Output()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("blockdev --getsz %s: %w", loopDev, err)
	}
	sectors := strings.TrimSpace(string(sectorOut))

	// 3. Create a sparse exception store (initial disk usage: ~0 bytes).
	//    8 GB sparse file gives ample headroom; actual allocation grows with VM writes.
	if err := exec.Command("truncate", "-s", "8G", exceptionStorePath).Run(); err != nil {
		cleanup()
		return nil, fmt.Errorf("create exception store %s: %w", exceptionStorePath, err)
	}

	// 4. Attach the exception store as a loop device.
	//    dm-snapshot requires block devices for both origin and COW — a regular file path
	//    is not accepted by the device mapper table parser.
	cowOut, err := exec.Command("losetup", "--find", "--show", exceptionStorePath).Output()
	if err != nil {
		cleanup()
		os.Remove(exceptionStorePath)
		return nil, fmt.Errorf("losetup for exception store: %w", err)
	}
	cowLoopDev := strings.TrimSpace(string(cowOut))
	cowCleanup := func() {
		exec.Command("losetup", "-d", cowLoopDev).Run()
		cleanup()
		os.Remove(exceptionStorePath)
	}

	// 5. Create the dm-snapshot device.
	//    "P 8" = persistent, 8-sector (4 KiB) chunks matching the host page size.
	dmName := "cow-" + filepath.Base(exceptionStorePath)
	if len(dmName) > 50 { // dm names are limited; truncate if needed
		dmName = dmName[:50]
	}
	table := fmt.Sprintf("0 %s snapshot %s %s P 8", sectors, loopDev, cowLoopDev)
	if out, err := exec.Command("dmsetup", "create", dmName, "--table", table).CombinedOutput(); err != nil {
		cowCleanup()
		return nil, fmt.Errorf("dmsetup create %s: %w: %s", dmName, err, strings.TrimSpace(string(out)))
	}

	dmPath := "/dev/mapper/" + dmName

	// 6. Ensure the bind-mount target file exists (must be a regular file).
	if _, err := os.Stat(mountTargetPath); os.IsNotExist(err) {
		if err := os.WriteFile(mountTargetPath, nil, 0644); err != nil {
			exec.Command("dmsetup", "remove", dmName).Run()
			cowCleanup()
			return nil, fmt.Errorf("create mount target %s: %w", mountTargetPath, err)
		}
	}

	// 7. Bind-mount the COW device over the original disk path.
	//    Firecracker opens mountTargetPath and transparently reads/writes the COW device.
	if out, err := exec.Command("mount", "--bind", dmPath, mountTargetPath).CombinedOutput(); err != nil {
		exec.Command("dmsetup", "remove", dmName).Run()
		cowCleanup()
		return nil, fmt.Errorf("bind mount %s → %s: %w: %s", dmPath, mountTargetPath, err, strings.TrimSpace(string(out)))
	}

	return &DMSnapshotInfo{
		LoopDevice:     loopDev,
		COWLoopDevice:  cowLoopDev,
		DMDevice:       dmName,
		ExceptionStore: exceptionStorePath,
		MountTarget:    mountTargetPath,
	}, nil
}

// TeardownDMSnapshot releases all kernel resources created by SetupDMSnapshot and
// removes the sparse exception store. Use this on explicit VM delete, where the COW
// writes are no longer needed.
func TeardownDMSnapshot(info *DMSnapshotInfo) {
	teardownDMSnapshot(info, false)
}

// TeardownDMSnapshotKeepStore releases the kernel objects (mount, dm device, loop
// devices) but preserves the sparse exception store file. Used on graceful daemon
// shutdown so a COW VM can be cold-restarted: RecoverVMs re-layers the same store
// over the golden image to reconstruct the disk.
func TeardownDMSnapshotKeepStore(info *DMSnapshotInfo) {
	teardownDMSnapshot(info, true)
}

// teardownDMSnapshot releases the kernel resources created by SetupDMSnapshot.
// Uses lazy unmount so any Firecracker fd that was already opened continues to work
// until the process exits, at which point the underlying inode is freed. When
// keepStore is true the exception store file is left on disk for a later restore.
func teardownDMSnapshot(info *DMSnapshotInfo, keepStore bool) {
	if info == nil {
		return
	}
	exec.Command("umount", "-l", info.MountTarget).Run()
	// Brief wait: dmsetup remove can fail if Firecracker still holds an open fd.
	// The lazy umount detaches the path immediately; the device can be removed once
	// all fds are closed. We retry a few times to handle the transition.
	for i := 0; i < 5; i++ {
		if exec.Command("dmsetup", "remove", info.DMDevice).Run() == nil {
			break
		}
		// Wait for Firecracker to fully exit before retrying.
		exec.Command("sleep", "0.2").Run()
	}
	exec.Command("losetup", "-d", info.COWLoopDevice).Run()
	exec.Command("losetup", "-d", info.LoopDevice).Run()
	if !keepStore {
		os.Remove(info.ExceptionStore)
	}
}

// MergeMemoryDiff produces a merged memory file by overlaying dirty pages from a diff
// snapshot onto a full (base) snapshot.
//
// outputPath must not exist; caller is responsible for cleanup on success.
func MergeMemoryDiff(baseMemPath, diffMemPath, outputPath string) error {
	if err := copyFile(baseMemPath, outputPath); err != nil {
		return fmt.Errorf("copy base memory: %w", err)
	}
	return overlaySparseDiff(diffMemPath, outputPath)
}

// MergeRootfsDiff reconstructs a full rootfs image by overlaying a sparse rootfs diff
// (changed 4 KiB blocks only) onto a copy of the base snapshot's full rootfs. The result
// is a complete ext4 image suitable as the read-only origin of a dm-snapshot on restore.
//
// outputPath must not exist; caller is responsible for cleanup on success.
func MergeRootfsDiff(baseRootfsPath, diffPath, outputPath string) error {
	if err := reflinkOrSparseCopy(baseRootfsPath, outputPath); err != nil {
		return fmt.Errorf("copy base rootfs: %w", err)
	}
	return overlaySparseDiff(diffPath, outputPath)
}

// overlaySparseDiff overlays the data (non-hole) regions of a sparse diff file onto
// outputPath, which must already contain the base image. It walks the diff with
// SEEK_DATA/SEEK_HOLE so only written regions are touched, avoiding a full read/write of
// the base. On error outputPath is removed.
func overlaySparseDiff(diffPath, outputPath string) error {
	diff, err := os.Open(diffPath)
	if err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("open diff: %w", err)
	}
	defer diff.Close()

	out, err := os.OpenFile(outputPath, os.O_WRONLY, 0)
	if err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("open merged output: %w", err)
	}
	defer out.Close()

	const bufSize = 2 << 20 // 2 MiB transfer buffer
	buf := make([]byte, bufSize)
	var offset int64

	diffFd := int(diff.Fd())
	for {
		dataStart, err := unix.Seek(diffFd, offset, unix.SEEK_DATA)
		if err != nil {
			break // no more data regions (ENXIO at EOF)
		}
		holeStart, err := unix.Seek(diffFd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			fi, _ := diff.Stat()
			holeStart = fi.Size()
		}
		remaining := holeStart - dataStart
		if _, err := diff.Seek(dataStart, io.SeekStart); err != nil {
			os.Remove(outputPath)
			return fmt.Errorf("seek diff at %d: %w", dataStart, err)
		}
		if _, err := out.Seek(dataStart, io.SeekStart); err != nil {
			os.Remove(outputPath)
			return fmt.Errorf("seek output at %d: %w", dataStart, err)
		}
		for remaining > 0 {
			n := int64(bufSize)
			if n > remaining {
				n = remaining
			}
			nr, err := diff.Read(buf[:n])
			if nr > 0 {
				if _, werr := out.Write(buf[:nr]); werr != nil {
					os.Remove(outputPath)
					return fmt.Errorf("write merged output: %w", werr)
				}
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				os.Remove(outputPath)
				return fmt.Errorf("read diff: %w", err)
			}
			remaining -= int64(nr)
		}
		offset = holeStart
	}
	return nil
}

// WriteRootfsDiff compares currentPath against basePath in 4 KiB blocks and writes only
// the differing blocks into diffPath at the same offset, leaving matching ranges as holes
// so diffPath is sparse. currentPath and basePath MUST be the same length (both are clones
// of the golden image). Reading goes through os.Open, so a bind-mounted dm-snapshot block
// device (COW / restored source VM) yields its full ext4 view correctly — the diff is
// computed by comparison, never from source sparseness (a block device reports no holes).
// Returns the number of changed bytes written. On error diffPath is removed if created.
func WriteRootfsDiff(currentPath, basePath, diffPath string) (changedBytes int64, err error) {
	cur, err := os.Open(currentPath)
	if err != nil {
		return 0, fmt.Errorf("open current rootfs: %w", err)
	}
	defer cur.Close()
	base, err := os.Open(basePath)
	if err != nil {
		return 0, fmt.Errorf("open base rootfs: %w", err)
	}
	defer base.Close()

	curInfo, err := cur.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat current rootfs: %w", err)
	}
	baseInfo, err := base.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat base rootfs: %w", err)
	}
	size := curInfo.Size()
	// COW spawn VMs expose the rootfs as a dm-snapshot block device (bind mount),
	// whose Stat().Size() reports 0. Query the real device size so the diff against
	// the base (a regular rootfs.ext4 file) lines up instead of failing size-mismatch.
	if curInfo.Mode()&os.ModeDevice != 0 {
		size, err = blockDeviceSize(currentPath)
		if err != nil {
			return 0, fmt.Errorf("size of current rootfs block device: %w", err)
		}
	}
	if size != baseInfo.Size() {
		return 0, fmt.Errorf("rootfs size mismatch: current %d != base %d", size, baseInfo.Size())
	}

	out, err := os.Create(diffPath)
	if err != nil {
		return 0, fmt.Errorf("create rootfs diff: %w", err)
	}
	defer out.Close()
	// Set apparent size up front so trailing equal blocks leave the diff at full length
	// (holes), keeping apparent size == rootfs size for a clean SEEK_DATA/SEEK_HOLE merge.
	if err := out.Truncate(size); err != nil {
		os.Remove(diffPath)
		return 0, fmt.Errorf("truncate rootfs diff: %w", err)
	}

	const block = 4096     // diff granularity: dm-snapshot "P 8" chunk + ext4 block + page
	const window = 1 << 20 // 1 MiB read window (fewer syscalls than per-block reads)
	cbuf := make([]byte, window)
	bbuf := make([]byte, window)
	var offset int64
	for offset < size {
		n := int64(window)
		if size-offset < n {
			n = size - offset
		}
		cr, cerr := io.ReadFull(cur, cbuf[:n])
		br, berr := io.ReadFull(base, bbuf[:n])
		if (cerr != nil && cerr != io.ErrUnexpectedEOF) || (berr != nil && berr != io.ErrUnexpectedEOF) {
			os.Remove(diffPath)
			return 0, fmt.Errorf("read rootfs at %d: cur=%v base=%v", offset, cerr, berr)
		}
		if cr != br {
			os.Remove(diffPath)
			return 0, fmt.Errorf("short read mismatch at %d: cur=%d base=%d", offset, cr, br)
		}
		// Compare in 4 KiB sub-blocks; write only the differing ones (sparse elsewhere).
		for i := 0; i < cr; i += block {
			end := i + block
			if end > cr {
				end = cr
			}
			if !bytes.Equal(cbuf[i:end], bbuf[i:end]) {
				if _, werr := out.WriteAt(cbuf[i:end], offset+int64(i)); werr != nil {
					os.Remove(diffPath)
					return 0, fmt.Errorf("write rootfs diff at %d: %w", offset+int64(i), werr)
				}
				changedBytes += int64(end - i)
			}
		}
		offset += int64(cr)
	}
	if err := out.Close(); err != nil {
		os.Remove(diffPath)
		return 0, fmt.Errorf("flush rootfs diff: %w", err)
	}
	return changedBytes, nil
}

// blockDeviceSize returns a block device's size in bytes via `blockdev --getsize64`.
// A regular file's size comes from Stat, but a block device reports 0 there; this is
// needed for COW spawn VMs whose rootfs is a dm-snapshot device. blockdev is already
// a hard dependency of the COW path (see DMSnapshotAvailable).
func blockDeviceSize(path string) (int64, error) {
	out, err := exec.Command("blockdev", "--getsize64", path).Output()
	if err != nil {
		return 0, fmt.Errorf("blockdev --getsize64 %s: %w", path, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse block device size %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n, nil
}

// reflinkOrSparseCopy copies src→dst. `cp` (non-recursive) copies the CONTENTS of src even
// when src is a bind-mounted block device (COW / restored VM disk); --reflink=auto is an
// instant CoW clone on btrfs/XFS (transparently a full copy on ext4) and --sparse=always
// keeps holes. Falls back to a Go sparse copy if cp is unavailable.
func reflinkOrSparseCopy(src, dst string) error {
	if err := exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst).Run(); err == nil {
		return nil
	}
	return sparseCopyFile(src, dst)
}

// sparseCopyFile copies src→dst preserving holes via SEEK_DATA/SEEK_HOLE. A source with no
// holes (e.g. a block device) is read in full — still correct, just not sparse.
func sparseCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Truncate(fi.Size()); err != nil {
		os.Remove(dst)
		return err
	}
	const bufSize = 2 << 20
	buf := make([]byte, bufSize)
	inFd := int(in.Fd())
	var offset int64
	for {
		dataStart, err := unix.Seek(inFd, offset, unix.SEEK_DATA)
		if err != nil {
			break
		}
		holeStart, err := unix.Seek(inFd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			holeStart = fi.Size()
		}
		if _, err := in.Seek(dataStart, io.SeekStart); err != nil {
			os.Remove(dst)
			return err
		}
		if _, err := out.Seek(dataStart, io.SeekStart); err != nil {
			os.Remove(dst)
			return err
		}
		remaining := holeStart - dataStart
		for remaining > 0 {
			n := int64(bufSize)
			if n > remaining {
				n = remaining
			}
			nr, rerr := in.Read(buf[:n])
			if nr > 0 {
				if _, werr := out.Write(buf[:nr]); werr != nil {
					os.Remove(dst)
					return werr
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					break
				}
				os.Remove(dst)
				return rerr
			}
			remaining -= int64(nr)
		}
		offset = holeStart
	}
	return nil
}
