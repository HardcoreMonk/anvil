package storage

import (
	"errors"
	"syscall"
	"testing"
)

// fakeStatfs returns a statfsFn stand-in that reports a fixed available-block
// count and block size.
func fakeStatfs(availBlocks uint64, blockSize int64) func(string, *syscall.Statfs_t) error {
	return func(_ string, st *syscall.Statfs_t) error {
		st.Bavail = availBlocks
		st.Bsize = blockSize
		return nil
	}
}

func TestEnsureFreeSpace(t *testing.T) {
	orig := statfsFn
	defer func() { statfsFn = orig }()

	t.Run("enough space", func(t *testing.T) {
		statfsFn = fakeStatfs(100, 1<<20) // 100 MiB available
		if err := EnsureFreeSpace("/x", 50<<20, 10<<20); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("exactly at boundary", func(t *testing.T) {
		statfsFn = fakeStatfs(60, 1<<20) // 60 MiB available; need 50 + margin 10
		if err := EnsureFreeSpace("/x", 50<<20, 10<<20); err != nil {
			t.Fatalf("expected nil at the boundary, got %v", err)
		}
	})

	t.Run("insufficient", func(t *testing.T) {
		statfsFn = fakeStatfs(55, 1<<20) // 55 MiB; need 50 + margin 10 = 60
		err := EnsureFreeSpace("/data", 50<<20, 10<<20)
		var ise *InsufficientStorageError
		if !errors.As(err, &ise) {
			t.Fatalf("expected *InsufficientStorageError, got %v", err)
		}
		if ise.AvailBytes != 55<<20 || ise.NeedBytes != 50<<20 || ise.MarginBytes != 10<<20 {
			t.Fatalf("unexpected fields: %+v", ise)
		}
		if ise.Dir != "/data" {
			t.Fatalf("expected Dir /data, got %q", ise.Dir)
		}
	})

	t.Run("statfs error propagates", func(t *testing.T) {
		statfsFn = func(_ string, _ *syscall.Statfs_t) error { return errors.New("boom") }
		if err := EnsureFreeSpace("/x", 0, 0); err == nil {
			t.Fatal("expected the statfs error to propagate")
		}
	})
}
