package storage

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// holeGranularityFine is the filesystem hole granularity (bytes) that sparse diff
// snapshots require: exactly the 4KiB page/block the Firecracker diff format records
// as a dirty unit. A coarser value (e.g. ZFS recordsize 128K) makes SEEK_DATA/SEEK_HOLE
// over-report the written region of a diff, so overlaySparseDiff would splat unwritten
// record padding over live base memory and triple-fault the guest (defect D3).
const holeGranularityFine = 4096

// holeProbeFileSize is the length the probe truncates its temp file to. It must exceed a
// common coarse record size (ZFS default 128K) so that a coarse fs still exposes an
// interior hole to measure, while staying small enough that the probe is cheap.
const holeProbeFileSize = 256 * 1024

// ProbeHoleGranularity measures dir's filesystem hole granularity in bytes.
//
// It writes a single 4KiB data block at offset 0 of a fresh temp file in dir, truncates
// the file to 256KiB to create a trailing hole, fsyncs (ZFS may not expose the hole while
// the file is still dirty — hence a second fsync+retry), then asks SEEK_HOLE for the first
// hole at/after offset 0. On a fine filesystem (ext4/tmpfs) that hole starts at 4096; on
// ZFS with recordsize>4K it starts at the record size.
//
// If SEEK_HOLE is unsupported, errors, or reports no interior hole, the probe file size is
// returned as the granularity (coarse — the safe side, since callers treat any value >4096
// as unsafe for sparse diffs). The temp file is always removed.
func ProbeHoleGranularity(dir string) (int64, error) {
	f, err := os.CreateTemp(dir, ".hole-probe-*")
	if err != nil {
		return holeProbeFileSize, fmt.Errorf("create hole-granularity probe in %s: %w", dir, err)
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()

	block := make([]byte, holeGranularityFine)
	for i := range block {
		block[i] = 0xff
	}
	if _, err := f.WriteAt(block, 0); err != nil {
		return holeProbeFileSize, fmt.Errorf("write hole-granularity probe %s: %w", name, err)
	}
	if err := f.Truncate(holeProbeFileSize); err != nil {
		return holeProbeFileSize, fmt.Errorf("truncate hole-granularity probe %s: %w", name, err)
	}

	fd := int(f.Fd())
	// Two attempts: some filesystems (ZFS) do not report the truncate-created hole until
	// the dirty file is flushed to the pool.
	for attempt := 0; attempt < 2; attempt++ {
		if err := f.Sync(); err != nil {
			return holeProbeFileSize, fmt.Errorf("fsync hole-granularity probe %s: %w", name, err)
		}
		hole, err := unix.Seek(fd, 0, unix.SEEK_HOLE)
		if err != nil {
			// SEEK_HOLE unsupported on this fs → coarse (safe side).
			return holeProbeFileSize, nil
		}
		if hole > 0 && hole < holeProbeFileSize {
			return hole, nil // interior hole → observed granularity
		}
		// hole == holeProbeFileSize (no interior hole reported yet): retry after another fsync.
	}
	// No interior hole after retry → treat as coarse (safe side).
	return holeProbeFileSize, nil
}

// HoleGranularityCoarse reports whether dir's filesystem exposes holes at coarser than
// 4KiB granularity — or the probe itself failed — in which case sparse diff snapshots are
// unsafe (D3) and callers must demote diff→full (creation) or refuse the overlay (read).
func HoleGranularityCoarse(dir string) bool {
	g, err := ProbeHoleGranularity(dir)
	return err != nil || g > holeGranularityFine
}

// holeGranularityFn measures a directory's filesystem hole granularity. Package-level so
// tests can inject a coarse (ZFS recordsize>4K) filesystem without provisioning real ZFS.
var holeGranularityFn = ProbeHoleGranularity
