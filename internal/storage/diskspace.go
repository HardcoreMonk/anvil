package storage

import (
	"fmt"
	"syscall"
)

// statfsFn is indirected so unit tests can inject filesystem readings without a
// real mount.
var statfsFn = syscall.Statfs

// AvailableBytes returns the number of bytes available to unprivileged callers
// on the filesystem backing dir.
func AvailableBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := statfsFn(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	// Bavail is blocks available to non-root callers; Bsize is the block size.
	return st.Bavail * uint64(st.Bsize), nil
}

// InsufficientStorageError reports that a disk operation was refused because it
// would leave less than the required margin free. The daemon maps it to HTTP
// 507 Insufficient Storage.
type InsufficientStorageError struct {
	Dir         string
	NeedBytes   uint64
	MarginBytes uint64
	AvailBytes  uint64
}

func (e *InsufficientStorageError) Error() string {
	return fmt.Sprintf(
		"insufficient storage on %s: operation needs %d MiB plus %d MiB safety margin, only %d MiB available",
		e.Dir, e.NeedBytes>>20, e.MarginBytes>>20, e.AvailBytes>>20)
}

// EnsureFreeSpace returns an *InsufficientStorageError if dir does not have at
// least needBytes + marginBytes available. needBytes is the operation's
// expected consumption; marginBytes is the operator-configured floor that must
// remain free afterwards (EPHEMERA_DISK_MIN_FREE_MIB on the daemon side).
func EnsureFreeSpace(dir string, needBytes, marginBytes uint64) error {
	avail, err := AvailableBytes(dir)
	if err != nil {
		return err
	}
	if avail < needBytes+marginBytes {
		return &InsufficientStorageError{
			Dir:         dir,
			NeedBytes:   needBytes,
			MarginBytes: marginBytes,
			AvailBytes:  avail,
		}
	}
	return nil
}
