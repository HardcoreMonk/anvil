package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// allocatedBytes returns the actual on-disk allocation (sparse-aware) for path.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no syscall.Stat_t for %s", path)
	}
	return int64(st.Blocks) * 512
}

func writeSized(t *testing.T, path string, size int, fill byte) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte{fill}, size), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestWriteAndMergeRootfsDiff is the round-trip: a few changed 4 KiB blocks produce a
// sparse diff far smaller than the rootfs, and MergeRootfsDiff(base, diff) reproduces the
// current image byte-for-byte (including a non-4 KiB-multiple tail block).
func TestWriteAndMergeRootfsDiff(t *testing.T) {
	dir := t.TempDir()
	const size = 4*1024*1024 + 123 // non-4KiB-multiple to exercise the tail
	base := filepath.Join(dir, "base.ext4")
	cur := filepath.Join(dir, "cur.ext4")
	diff := filepath.Join(dir, "rootfs.diff")
	merged := filepath.Join(dir, "merged.ext4")

	baseData := make([]byte, size)
	for i := range baseData {
		baseData[i] = byte(i % 251)
	}
	if err := os.WriteFile(base, baseData, 0644); err != nil {
		t.Fatal(err)
	}
	curData := make([]byte, size)
	copy(curData, baseData)
	for _, blk := range []int{0, 100} { // mutate two full 4 KiB blocks
		for j := 0; j < 4096; j++ {
			curData[blk*4096+j] = 0xAB
		}
	}
	curData[size-1] = 0xCD // touch the final (partial) block
	if err := os.WriteFile(cur, curData, 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := WriteRootfsDiff(cur, base, diff)
	if err != nil {
		t.Fatalf("WriteRootfsDiff: %v", err)
	}
	if changed == 0 || changed > 3*4096 {
		t.Fatalf("changed bytes %d not in expected (0, ~3 blocks] range", changed)
	}

	// The diff must be sparse: actual allocation far below the full image size.
	if alloc := allocatedBytes(t, diff); alloc >= size/2 {
		t.Fatalf("diff not sparse: allocated %d of %d bytes", alloc, size)
	}

	if err := MergeRootfsDiff(base, diff, merged); err != nil {
		t.Fatalf("MergeRootfsDiff: %v", err)
	}
	got, err := os.ReadFile(merged)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, curData) {
		t.Fatalf("merged image != current (got %d bytes, want %d)", len(got), len(curData))
	}
}

// TestWriteRootfsDiffSizeMismatch guards the invariant that current and base are the same
// length (both clones of the golden image); a mismatch must error, never silently corrupt.
func TestWriteRootfsDiffSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	cur := filepath.Join(dir, "cur.ext4")
	writeSized(t, base, 8192, 0x11)
	writeSized(t, cur, 4096, 0x11)
	if _, err := WriteRootfsDiff(cur, base, filepath.Join(dir, "d.diff")); err == nil {
		t.Fatal("expected size-mismatch error, got nil")
	}
}

func TestMergeMemoryDiffRemovesOutputWhenBaseCopyFails(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "base-memory-dir")
	diff := filepath.Join(dir, "memory.diff")
	output := filepath.Join(dir, "merged-memory.bin")
	if err := os.Mkdir(baseDir, 0700); err != nil {
		t.Fatalf("mkdir base dir: %v", err)
	}
	if err := os.WriteFile(diff, []byte("diff"), 0600); err != nil {
		t.Fatalf("write diff: %v", err)
	}

	err := MergeMemoryDiff(baseDir, diff, output)
	if err == nil {
		t.Fatal("MergeMemoryDiff returned nil error, want base copy failure")
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("merged output still exists after copy failure: err=%v", statErr)
	}
}

// TestWriteRootfsDiffIdentical: no changes ⇒ fully sparse (0-byte) diff; merge reproduces base.
func TestWriteRootfsDiffIdentical(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	cur := filepath.Join(dir, "cur.ext4")
	diff := filepath.Join(dir, "d.diff")
	merged := filepath.Join(dir, "m.ext4")
	writeSized(t, base, 1<<20, 0x55)
	writeSized(t, cur, 1<<20, 0x55)
	changed, err := WriteRootfsDiff(cur, base, diff)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Fatalf("expected 0 changed bytes for identical inputs, got %d", changed)
	}
	if err := MergeRootfsDiff(base, diff, merged); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(merged)
	want, _ := os.ReadFile(base)
	if !bytes.Equal(got, want) {
		t.Fatal("merged != base for identical inputs")
	}
}
