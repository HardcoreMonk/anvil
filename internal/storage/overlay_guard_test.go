package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeBaseAndDiff builds a base image plus a valid sparse diff (one changed 4KiB block).
// WriteRootfsDiff itself never probes hole granularity (the diff is computed by exact
// block comparison, not by reading source sparseness), so it succeeds regardless of the
// injected read-side granularity — the guard under test lives on the merge/overlay side.
func makeBaseAndDiff(t *testing.T, dir string, size int) (base, diff string) {
	t.Helper()
	base = filepath.Join(dir, "base.ext4")
	cur := filepath.Join(dir, "cur.ext4")
	diff = filepath.Join(dir, "rootfs.diff")
	writeSized(t, base, size, 0x11)
	curData := bytes.Repeat([]byte{0x11}, size)
	copy(curData[0:4096], bytes.Repeat([]byte{0x22}, 4096))
	if err := os.WriteFile(cur, curData, 0644); err != nil {
		t.Fatalf("write cur: %v", err)
	}
	if _, err := WriteRootfsDiff(cur, base, diff); err != nil {
		t.Fatalf("WriteRootfsDiff: %v", err)
	}
	return base, diff
}

// TestOverlaySparseDiffRefusesCoarseFS is the read-side guard: when the diff lives on a
// filesystem with coarse hole granularity (ZFS recordsize>4K), the overlay MUST refuse
// with a clear error instead of splatting record padding over base memory (D3). It must
// also remove the partially-written output so no corrupt image survives.
func TestOverlaySparseDiffRefusesCoarseFS(t *testing.T) {
	orig := holeGranularityFn
	t.Cleanup(func() { holeGranularityFn = orig })
	holeGranularityFn = func(string) (int64, error) { return 128 * 1024, nil } // simulate ZFS recordsize 128K

	dir := t.TempDir()
	base, diff := makeBaseAndDiff(t, dir, 1<<20)
	merged := filepath.Join(dir, "merged.ext4")

	err := MergeRootfsDiff(base, diff, merged)
	if err == nil {
		t.Fatal("MergeRootfsDiff on coarse fs returned nil, want refusal")
	}
	if !strings.Contains(err.Error(), "refusing overlay") {
		t.Fatalf("error %q lacks refusal message", err)
	}
	if !strings.Contains(err.Error(), "131072") {
		t.Fatalf("error %q should report the observed granularity", err)
	}
	if _, statErr := os.Stat(merged); !os.IsNotExist(statErr) {
		t.Fatalf("merged output must be removed on refusal, stat err=%v", statErr)
	}
}

// TestOverlaySparseDiffFineInjectionRoundTrip locks that with fine granularity injected
// the overlay proceeds exactly as before — the guard is a gate, not a behaviour change.
func TestOverlaySparseDiffFineInjectionRoundTrip(t *testing.T) {
	orig := holeGranularityFn
	t.Cleanup(func() { holeGranularityFn = orig })
	holeGranularityFn = func(string) (int64, error) { return HoleGranularityFine, nil }

	dir := t.TempDir()
	const size = 1 << 20
	base, diff := makeBaseAndDiff(t, dir, size)
	merged := filepath.Join(dir, "merged.ext4")

	if err := MergeRootfsDiff(base, diff, merged); err != nil {
		t.Fatalf("MergeRootfsDiff on fine fs: %v", err)
	}
	got, err := os.ReadFile(merged)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0x11}, size)
	copy(want[0:4096], bytes.Repeat([]byte{0x22}, 4096))
	if !bytes.Equal(got, want) {
		t.Fatal("merged image != expected current image on fine fs")
	}
}
