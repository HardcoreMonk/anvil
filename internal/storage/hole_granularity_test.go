package storage

import (
	"path/filepath"
	"testing"
)

// TestProbeHoleGranularityFineFS asserts that on the local test filesystem
// (ext4/tmpfs) the probe observes the required 4KiB hole granularity, so sparse
// diff snapshots are safe and neither guard fires. This is the regression anchor
// for the KVM gate (ext4), which must see the pre-D3 behaviour unchanged.
func TestProbeHoleGranularityFineFS(t *testing.T) {
	dir := t.TempDir()
	g, err := ProbeHoleGranularity(dir)
	if err != nil {
		t.Fatalf("ProbeHoleGranularity(%s) error: %v", dir, err)
	}
	if g != HoleGranularityFine {
		t.Fatalf("granularity = %d, want %d (fine fs)", g, HoleGranularityFine)
	}
	// g == HoleGranularityFine is exactly the "not coarse" predicate the guards apply
	// (coarse = err != nil || g > HoleGranularityFine), so a fine fs is safe.
}

// TestProbeHoleGranularityMissingDirIsCoarse: a probe that cannot even create its
// temp file (non-existent dir) must fail safe — report coarse — so the guards
// refuse/demote rather than silently proceeding.
func TestProbeHoleGranularityMissingDirIsCoarse(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	g, err := ProbeHoleGranularity(missing)
	if err == nil {
		t.Fatalf("expected error probing non-existent dir, got nil (g=%d)", g)
	}
	// err != nil (and g > HoleGranularityFine) is exactly the "coarse" predicate the
	// guards apply — a probe that fails fails safe (coarse), so the guards refuse/demote.
	if g <= HoleGranularityFine {
		t.Fatalf("granularity on probe failure = %d, want > %d (coarse, safe side)", g, HoleGranularityFine)
	}
}
