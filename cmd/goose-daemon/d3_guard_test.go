package main

import (
	"errors"
	"sync/atomic"
	"testing"
)

// These tests exercise the D3 creation-side guard (diff→full demotion) without KVM by
// injecting the hole-granularity probe. The full VM path (an actual full snapshot whose
// metadata/response records type "full") is covered by the ext4 KVM gate, where the real
// probe reports fine granularity and nothing is demoted.

// TestApplyD3DiffGuard_CoarseDemotesDiffToFull: on a coarse snapshots filesystem a diff
// request is demoted to a full snapshot (honest type + no base), so the sparse-diff
// corruption path is never taken.
func TestApplyD3DiffGuard_CoarseDemotesDiffToFull(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.workDir = t.TempDir()
	var probes int32
	cp.holeProbeFn = func(string) (int64, error) {
		atomic.AddInt32(&probes, 1)
		return 128 * 1024, nil // ZFS recordsize 128K
	}

	gotType, gotBase := cp.applyD3DiffGuard("diff", "snap-base")
	if gotType != "full" || gotBase != "" {
		t.Fatalf("applyD3DiffGuard(diff) = (%q,%q), want (full, \"\")", gotType, gotBase)
	}
	// Probe once per daemon lifetime (cached): a second decision must not re-probe.
	cp.applyD3DiffGuard("diff", "snap-base")
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Fatalf("probe invoked %d times, want exactly 1 (cached)", n)
	}
}

// TestApplyD3DiffGuard_FineKeepsDiff: on a fine filesystem the diff request is untouched.
func TestApplyD3DiffGuard_FineKeepsDiff(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.workDir = t.TempDir()
	cp.holeProbeFn = func(string) (int64, error) { return 4096, nil }

	gotType, gotBase := cp.applyD3DiffGuard("diff", "snap-base")
	if gotType != "diff" || gotBase != "snap-base" {
		t.Fatalf("applyD3DiffGuard(diff) on fine fs = (%q,%q), want (diff, snap-base)", gotType, gotBase)
	}
}

// TestApplyD3DiffGuard_FullRequestNeverProbes: a full request is left alone and must not
// probe the filesystem at all (the guard only concerns diff snapshots).
func TestApplyD3DiffGuard_FullRequestNeverProbes(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.workDir = t.TempDir()
	var probes int32
	cp.holeProbeFn = func(string) (int64, error) {
		atomic.AddInt32(&probes, 1)
		return 128 * 1024, nil
	}

	gotType, gotBase := cp.applyD3DiffGuard("full", "")
	if gotType != "full" || gotBase != "" {
		t.Fatalf("applyD3DiffGuard(full) = (%q,%q), want (full, \"\")", gotType, gotBase)
	}
	if n := atomic.LoadInt32(&probes); n != 0 {
		t.Fatalf("probe invoked %d times for a full request, want 0", n)
	}
}

// TestSnapshotsDirCoarse_ProbeErrorIsCoarse: a probe error fails safe as coarse.
func TestSnapshotsDirCoarse_ProbeErrorIsCoarse(t *testing.T) {
	cp := newMetricsTestCP(t)
	cp.workDir = t.TempDir()
	cp.holeProbeFn = func(string) (int64, error) {
		return 4096, errors.New("probe boom")
	}
	if !cp.snapshotsDirCoarse() {
		t.Fatal("snapshotsDirCoarse on probe error = false, want true (safe side)")
	}
}
