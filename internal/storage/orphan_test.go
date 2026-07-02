package storage

import "testing"

func TestParseLosetupOutput(t *testing.T) {
	out := `/dev/loop0: [64769]:12345 (/tmp/goose-workspaces/vm-1.cow)
/dev/loop1: [64769]:12346 (/home/x/artifacts/golden-image.ext4)
/dev/loop2: [64769]:9 (/var/lib/snap/core.img (deleted))
`
	m := parseLosetupOutput(out)
	if len(m) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(m), m)
	}
	if got := m["/dev/loop0"]; got != "/tmp/goose-workspaces/vm-1.cow" {
		t.Errorf("loop0 backing = %q", got)
	}
	if got := m["/dev/loop1"]; got != "/home/x/artifacts/golden-image.ext4" {
		t.Errorf("loop1 backing = %q", got)
	}
	if got := m["/dev/loop2"]; got != "/var/lib/snap/core.img" {
		t.Errorf("loop2 backing = %q (expected trailing ' (deleted)' stripped)", got)
	}
}

func TestParseDMSetupLs(t *testing.T) {
	out := "cow-vm-1.cow\t(253:0)\ncow-vm-2.cow\t(253:1)\nother-dev\t(253:2)\n"
	names := parseDMSetupLs(out)
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d: %v", len(names), names)
	}
	if names[0] != "cow-vm-1.cow" || names[2] != "other-dev" {
		t.Errorf("unexpected names: %v", names)
	}
	if got := parseDMSetupLs("No devices found\n"); len(got) != 0 {
		t.Errorf("expected empty for 'No devices found', got %v", got)
	}
	if got := parseDMSetupLs(""); len(got) != 0 {
		t.Errorf("expected empty for empty input, got %v", got)
	}
}

func TestParseDMSnapshotTable(t *testing.T) {
	origin, cow := parseDMSnapshotTable("0 16777216 snapshot 7:0 7:1 P 8")
	if origin != "/dev/loop0" || cow != "/dev/loop1" {
		t.Errorf("snapshot table parse = (%q, %q)", origin, cow)
	}
	// Non-snapshot target → empty.
	if o, c := parseDMSnapshotTable("0 100 linear 8:0 0"); o != "" || c != "" {
		t.Errorf("linear target should yield empty, got (%q, %q)", o, c)
	}
	// Truncated line → empty.
	if o, c := parseDMSnapshotTable("0 100 snapshot"); o != "" || c != "" {
		t.Errorf("truncated table should yield empty, got (%q, %q)", o, c)
	}
}

func TestLoopPathFromMajorMinor(t *testing.T) {
	cases := map[string]string{
		"7:3":     "/dev/loop3",
		"7:0":     "/dev/loop0",
		"253:0":   "", // not the loop major
		"7:x":     "", // non-numeric minor
		"garbage": "",
		"7":       "",
	}
	for in, want := range cases {
		if got := loopPathFromMajorMinor(in); got != want {
			t.Errorf("loopPathFromMajorMinor(%q) = %q, want %q", in, got, want)
		}
	}
}
