package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"syscall"
	"testing"
)

// withRestoredUmask snapshots the process umask and restores it when the test
// ends. syscall.Umask both sets and returns the previous value, so reading it
// requires a set — the pair below reads without leaving a change behind.
//
// umask is process-global state shared by every test in this binary, hence the
// mandatory restore; the tests in this package do not run in parallel.
func withRestoredUmask(t *testing.T) {
	t.Helper()
	orig := syscall.Umask(0)
	syscall.Umask(orig)
	t.Cleanup(func() { syscall.Umask(orig) })
}

// currentUmask reads the process umask without changing it.
func currentUmask() int {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return m
}

// TestInitUmaskMasksGroupAndOtherBits proves the daemon narrows the inherited
// umask (0022 by default under systemd) to 0077 before it creates anything.
//
// Everything the daemon and its children emit — VM rootfs images, snapshot
// memory/state files, COW stores, files written by the Firecracker child and by
// `cp`/`truncate` subprocesses — inherits this mask. Those artifacts embed 0600
// secrets, so 0077 is what keeps them off unprivileged local accounts. Per-call
// file modes cannot cover the subprocess-created artifacts; only the umask can.
func TestInitUmaskMasksGroupAndOtherBits(t *testing.T) {
	withRestoredUmask(t)

	initUmask()

	if got := currentUmask(); got != 0o077 {
		t.Fatalf("umask after initUmask() = %#o, want 0o077 (daemon artifacts embed 0600 secrets)", got)
	}
}

// TestDaemonSnapshotDirsCreatedOwnerOnly pins the two snapshot-directory creations
// that no unit test can reach — one inside main(), one deep inside the snapshot
// create handler behind a live Firecracker VM — to 0700 at the source level.
//
// The umask is the primary control; an explicit 0700 is the backstop for any
// future entry point that constructs these paths without going through main().
func TestDaemonSnapshotDirsCreatedOwnerOnly(t *testing.T) {
	for _, tc := range []struct {
		file    string
		dirVar  string
		purpose string
	}{
		{"main.go", "snapshotDir", "daemon snapshot root"},
		{"api.go", "snapDir", "per-snapshot directory (memory.bin/state.bin)"},
	} {
		mode, ok := mkdirAllModeFor(t, tc.file, tc.dirVar)
		if !ok {
			t.Fatalf("%s: no os.MkdirAll(%s, ...) call found — was it renamed or removed? (%s)", tc.file, tc.dirVar, tc.purpose)
		}
		if mode != 0o700 {
			t.Errorf("%s: os.MkdirAll(%s, %#o), want 0700 — %s holds VM memory/state that embeds secrets", tc.file, tc.dirVar, mode, tc.purpose)
		}
	}
}

// mkdirAllModeFor parses file and returns the mode literal of the first
// os.MkdirAll call whose directory argument is the identifier dirVar.
func mkdirAllModeFor(t *testing.T, file, dirVar string) (int64, bool) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var (
		mode  int64
		found bool
	)
	ast.Inspect(f, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "MkdirAll" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
			return true
		}
		if arg, ok := call.Args[0].(*ast.Ident); !ok || arg.Name != dirVar {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			t.Fatalf("%s: os.MkdirAll(%s, ...) mode is not an integer literal: %v", file, dirVar, call.Args[1])
		}
		v, err := strconv.ParseInt(lit.Value, 0, 32)
		if err != nil {
			t.Fatalf("%s: cannot parse mode literal %q: %v", file, lit.Value, err)
		}
		mode, found = v, true
		return false
	})
	return mode, found
}
