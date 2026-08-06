package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests require root (loop mount) and mkfs.ext4.
// In CI they are skipped automatically; run them on a host with /dev/kvm.

func makeTestDisk(t *testing.T, dir string) string {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("loop mount requires root")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not in PATH")
	}

	img := filepath.Join(dir, "disk.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("create disk: %v", err)
	}
	if err := f.Truncate(8 << 20); err != nil { // 8 MiB minimum for ext4
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	f.Close()
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v — %s", err, out)
	}
	return img
}

func prepareTestVM(t *testing.T, tmp, workspaceDir string, opts VMPrepareOptions) {
	t.Helper()
	diskImg := makeTestDisk(t, tmp)
	vmID := "test-vm"
	destPath := filepath.Join(workspaceDir, vmID+".ext4")
	data, _ := os.ReadFile(diskImg)
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		t.Fatalf("write disk clone: %v", err)
	}

	p := &Provisioner{WorkspaceDir: workspaceDir}
	if err := p.PrepareVM(vmID, opts); err != nil {
		t.Fatalf("PrepareVM: %v", err)
	}

	// Mount and run the verification callback, then unmount.
	mntDir := filepath.Join(tmp, "mnt")
	os.MkdirAll(mntDir, 0755)
	t.Cleanup(func() {
		exec.Command("umount", "-l", mntDir).Run()
		os.Remove(mntDir)
	})
	if out, err := exec.Command("mount", "-o", "loop", destPath, mntDir).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v — %s", err, out)
	}
	t.Setenv("_TEST_MNT", mntDir) // pass mount point to caller via env (test-only convention)
}

func TestPrepareVM_WritesAgentToken(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	os.MkdirAll(ws, 0755)

	configPath := filepath.Join(tmp, "goose.yaml")
	secretsPath := filepath.Join(tmp, "goose-secrets.yaml")
	os.WriteFile(configPath, []byte("GOOSE_PROVIDER: test\n"), 0644)
	os.WriteFile(secretsPath, []byte("TEST_KEY: x\n"), 0644)

	const token = "deadbeefcafe1234"
	prepareTestVM(t, tmp, ws, VMPrepareOptions{
		HostConfigPath:  configPath,
		HostSecretsPath: secretsPath,
		AgentToken:      token,
	})

	mntDir := os.Getenv("_TEST_MNT")
	tokenFile := filepath.Join(mntDir, "root", ".ephemera-agent-token")

	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("token file not found: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected mode 0600, got %04o", perm)
	}
	content, _ := os.ReadFile(tokenFile)
	if string(content) != token {
		t.Errorf("expected token %q, got %q", token, string(content))
	}
}

// writeGoldenBuildScript writes an executable fake build script at path with the
// given body. The body runs under `bash` exactly like the real build_image.sh, so
// it can exercise EnsureGoldenImage's cwd (cmd.Dir) and EPHEMERA_GOLDEN_IMAGE_PATH
// env wiring without a real debootstrap build.
func writeGoldenBuildScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0755); err != nil {
		t.Fatalf("write build script: %v", err)
	}
}

// goldenTestLayout creates the conventional workdir layout (artifacts/ + scripts/)
// under a fresh temp dir and returns the golden image path and build script path.
func goldenTestLayout(t *testing.T) (golden, script string) {
	t.Helper()
	tmp := t.TempDir()
	artifactsDir := filepath.Join(tmp, "artifacts")
	scriptsDir := filepath.Join(tmp, "scripts")
	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	return filepath.Join(artifactsDir, "golden-image.ext4"), filepath.Join(scriptsDir, "build_image.sh")
}

// TestEnsureGoldenImage_BuildSuccessAtomicRename verifies the happy path: a build
// script that writes the image to EPHEMERA_GOLDEN_IMAGE_PATH (the .building temp)
// results in that temp being atomically renamed onto GoldenImagePath, with the
// exact content the script produced and no leftover temp.
func TestEnsureGoldenImage_BuildSuccessAtomicRename(t *testing.T) {
	golden, script := goldenTestLayout(t)
	writeGoldenBuildScript(t, script, `#!/bin/bash
set -euo pipefail
printf 'GOLDEN_CONTENT' > "$EPHEMERA_GOLDEN_IMAGE_PATH"
`)

	p := &Provisioner{GoldenImagePath: golden, BuildScriptPath: script}
	if err := p.EnsureGoldenImage(); err != nil {
		t.Fatalf("EnsureGoldenImage: %v", err)
	}

	got, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden after build: %v", err)
	}
	if string(got) != "GOLDEN_CONTENT" {
		t.Fatalf("golden content = %q, want GOLDEN_CONTENT", string(got))
	}
	if _, err := os.Stat(golden + ".building"); !os.IsNotExist(err) {
		t.Fatalf("expected no leftover .building temp, stat err = %v", err)
	}
}

// TestEnsureGoldenImage_PassesCmdDirAndEnv proves the two wiring fixes: the build
// runs with cwd set to the workdir (parent of scripts/, i.e. where artifacts/
// lives), and EPHEMERA_GOLDEN_IMAGE_PATH is the .building temp beside the golden.
// The script records both via a cwd-relative side file, so the side file landing
// in the workdir is itself evidence that cmd.Dir resolved to the workdir.
func TestEnsureGoldenImage_PassesCmdDirAndEnv(t *testing.T) {
	golden, script := goldenTestLayout(t)
	// The workdir is the parent of scripts/ — the directory EnsureGoldenImage must
	// use as cmd.Dir. filepath.EvalSymlinks normalizes it to match `pwd -P` output.
	workdir := filepath.Dir(filepath.Dir(script))
	wantCwd, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		t.Fatalf("evalsymlinks workdir: %v", err)
	}

	writeGoldenBuildScript(t, script, `#!/bin/bash
set -euo pipefail
pwd -P > build-cwd.txt
printf '%s' "$EPHEMERA_GOLDEN_IMAGE_PATH" > build-env.txt
printf 'X' > "$EPHEMERA_GOLDEN_IMAGE_PATH"
`)

	p := &Provisioner{GoldenImagePath: golden, BuildScriptPath: script}
	if err := p.EnsureGoldenImage(); err != nil {
		t.Fatalf("EnsureGoldenImage: %v", err)
	}

	// The cwd-relative side files must have landed in the workdir, proving cmd.Dir.
	gotCwd, err := os.ReadFile(filepath.Join(workdir, "build-cwd.txt"))
	if err != nil {
		t.Fatalf("read build-cwd.txt (should be in workdir): %v", err)
	}
	if strings.TrimSpace(string(gotCwd)) != wantCwd {
		t.Fatalf("build cwd = %q, want %q", strings.TrimSpace(string(gotCwd)), wantCwd)
	}

	gotEnv, err := os.ReadFile(filepath.Join(workdir, "build-env.txt"))
	if err != nil {
		t.Fatalf("read build-env.txt: %v", err)
	}
	if wantEnv := golden + ".building"; string(gotEnv) != wantEnv {
		t.Fatalf("EPHEMERA_GOLDEN_IMAGE_PATH = %q, want %q", string(gotEnv), wantEnv)
	}
}

// TestEnsureGoldenImage_BuildFailurePreservesExistingGolden verifies that a failed
// rebuild (script exits non-zero) leaves the existing golden image untouched and
// removes the .building temp — the core robustness fix. The pre-existing golden is
// aged so the build-input freshness check flags it stale and forces a rebuild.
func TestEnsureGoldenImage_BuildFailurePreservesExistingGolden(t *testing.T) {
	golden, script := goldenTestLayout(t)
	if err := os.WriteFile(golden, []byte("EXISTING_GOLDEN_A"), 0644); err != nil {
		t.Fatalf("write existing golden: %v", err)
	}
	// Age the golden so pathsNewerThan sees the (freshly written) build script as
	// newer, marking the image stale and forcing the rebuild path.
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(golden, past, past); err != nil {
		t.Fatalf("chtimes golden: %v", err)
	}

	writeGoldenBuildScript(t, script, `#!/bin/bash
set -euo pipefail
echo "build failed" >&2
exit 1
`)

	p := &Provisioner{GoldenImagePath: golden, BuildScriptPath: script}
	err := p.EnsureGoldenImage()
	if err == nil {
		t.Fatal("EnsureGoldenImage should error when build script fails")
	}

	got, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("existing golden should be preserved after failed rebuild: %v", err)
	}
	if string(got) != "EXISTING_GOLDEN_A" {
		t.Fatalf("golden content = %q, want EXISTING_GOLDEN_A (must not be destroyed)", string(got))
	}
	if _, err := os.Stat(golden + ".building"); !os.IsNotExist(err) {
		t.Fatalf("expected .building temp cleaned up, stat err = %v", err)
	}
}

// TestEnsureGoldenImage_PartialBuildCleanedUp verifies that when a build writes a
// partial image to the temp path and then fails, the partial image never becomes
// the golden (it is not renamed), the temp is cleaned up, and any pre-existing
// golden is preserved. Covers both the pre-existing-golden and no-golden cases.
func TestEnsureGoldenImage_PartialBuildCleanedUp(t *testing.T) {
	partialScript := `#!/bin/bash
set -euo pipefail
printf 'PARTIAL_IMAGE_BYTES' > "$EPHEMERA_GOLDEN_IMAGE_PATH"
exit 1
`

	t.Run("with existing golden preserved", func(t *testing.T) {
		golden, script := goldenTestLayout(t)
		if err := os.WriteFile(golden, []byte("EXISTING_GOLDEN_A"), 0644); err != nil {
			t.Fatalf("write existing golden: %v", err)
		}
		past := time.Now().Add(-1 * time.Hour)
		if err := os.Chtimes(golden, past, past); err != nil {
			t.Fatalf("chtimes golden: %v", err)
		}
		writeGoldenBuildScript(t, script, partialScript)

		p := &Provisioner{GoldenImagePath: golden, BuildScriptPath: script}
		if err := p.EnsureGoldenImage(); err == nil {
			t.Fatal("EnsureGoldenImage should error when build fails after partial write")
		}

		got, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("existing golden should survive partial failed build: %v", err)
		}
		if string(got) != "EXISTING_GOLDEN_A" {
			t.Fatalf("golden = %q, want EXISTING_GOLDEN_A (partial must not replace it)", string(got))
		}
		if _, err := os.Stat(golden + ".building"); !os.IsNotExist(err) {
			t.Fatalf("expected partial .building temp cleaned up, stat err = %v", err)
		}
	})

	t.Run("no golden left behind", func(t *testing.T) {
		golden, script := goldenTestLayout(t)
		writeGoldenBuildScript(t, script, partialScript)

		p := &Provisioner{GoldenImagePath: golden, BuildScriptPath: script}
		if err := p.EnsureGoldenImage(); err == nil {
			t.Fatal("EnsureGoldenImage should error when build fails after partial write")
		}
		if _, err := os.Stat(golden); !os.IsNotExist(err) {
			t.Fatalf("expected no golden image after failed first build, stat err = %v", err)
		}
		if _, err := os.Stat(golden + ".building"); !os.IsNotExist(err) {
			t.Fatalf("expected partial .building temp cleaned up, stat err = %v", err)
		}
	})
}

// TestEnsureGoldenImage_BuildLiesProducesNoImage covers the branch where the build
// script exits 0 but wrote nothing to the temp path (a "successful" build that
// silently produced no image). EnsureGoldenImage must surface a clear error naming
// the temp path and must NOT promote a nonexistent image to the golden path.
func TestEnsureGoldenImage_BuildLiesProducesNoImage(t *testing.T) {
	golden, script := goldenTestLayout(t)
	writeGoldenBuildScript(t, script, `#!/bin/bash
set -euo pipefail
exit 0
`)

	p := &Provisioner{GoldenImagePath: golden, BuildScriptPath: script}
	err := p.EnsureGoldenImage()
	if err == nil {
		t.Fatal("EnsureGoldenImage should error when the build produced no image")
	}
	if !strings.Contains(err.Error(), "temp path") {
		t.Fatalf("error should name the temp path, got: %v", err)
	}
	if _, statErr := os.Stat(golden); !os.IsNotExist(statErr) {
		t.Fatalf("golden must not exist after a no-output build, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(golden + ".building"); !os.IsNotExist(statErr) {
		t.Fatalf("expected no leftover .building temp, stat err = %v", statErr)
	}
}

func TestPathsNewerThan(t *testing.T) {
	tmp := t.TempDir()

	older := filepath.Join(tmp, "older.txt")
	newer := filepath.Join(tmp, "newer.txt")
	srcDir := filepath.Join(tmp, "src")
	os.MkdirAll(srcDir, 0755)
	srcFile := filepath.Join(srcDir, "main.go")

	if err := os.WriteFile(older, []byte("old"), 0644); err != nil {
		t.Fatalf("write older: %v", err)
	}
	if err := os.WriteFile(newer, []byte("new"), 0644); err != nil {
		t.Fatalf("write newer: %v", err)
	}
	if err := os.WriteFile(srcFile, []byte("pkg main"), 0644); err != nil {
		t.Fatalf("write srcFile: %v", err)
	}

	// Reference time strictly between older and newer.
	pastMtime := time.Now().Add(-1 * time.Hour)
	futureMtime := time.Now().Add(1 * time.Hour)
	if err := os.Chtimes(older, pastMtime, pastMtime); err != nil {
		t.Fatalf("chtimes older: %v", err)
	}
	if err := os.Chtimes(newer, futureMtime, futureMtime); err != nil {
		t.Fatalf("chtimes newer: %v", err)
	}
	if err := os.Chtimes(srcFile, futureMtime, futureMtime); err != nil {
		t.Fatalf("chtimes srcFile: %v", err)
	}

	now := time.Now()

	cases := []struct {
		name  string
		ref   time.Time
		paths []string
		want  bool
	}{
		{"file older than ref", now, []string{older}, false},
		{"file newer than ref", now, []string{newer}, true},
		{"directory with one newer file", now, []string{srcDir}, true},
		{"missing file is ignored", now, []string{filepath.Join(tmp, "ghost")}, false},
		{"mix: older + missing", now, []string{older, filepath.Join(tmp, "ghost")}, false},
		{"mix: older + dir-with-newer", now, []string{older, srcDir}, true},
		{"empty paths", now, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pathsNewerThan(tc.ref, tc.paths...)
			if got != tc.want {
				t.Errorf("pathsNewerThan(%v, %v) = %v, want %v", tc.ref, tc.paths, got, tc.want)
			}
		})
	}
}

func TestEnsureKernelRejectsSHA256Mismatch(t *testing.T) {
	body := []byte("not the expected kernel")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()

	kernelPath := filepath.Join(t.TempDir(), "vmlinux.bin")
	err := EnsureKernel(kernelPath, server.URL, strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("EnsureKernel returned nil error for SHA256 mismatch")
	}
	if !strings.Contains(err.Error(), "kernel SHA256 mismatch") {
		t.Fatalf("EnsureKernel error = %q, want SHA256 mismatch", err)
	}
	if _, statErr := os.Stat(kernelPath); !os.IsNotExist(statErr) {
		t.Fatalf("kernel file stat after mismatch = %v, want not exist", statErr)
	}
}

func TestEnsureKernelWritesFileAfterSHA256Match(t *testing.T) {
	body := []byte("expected kernel")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer server.Close()

	sum := sha256.Sum256(body)
	kernelPath := filepath.Join(t.TempDir(), "vmlinux.bin")
	if err := EnsureKernel(kernelPath, server.URL, fmt.Sprintf("%x", sum)); err != nil {
		t.Fatalf("EnsureKernel: %v", err)
	}
	got, err := os.ReadFile(kernelPath)
	if err != nil {
		t.Fatalf("read kernel: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("kernel content = %q, want %q", got, body)
	}
}

// TestInjectVMFiles_ControlPlaneToken verifies the v0.3.3 CP token injection:
// the file lands at /root/.ephemera-cp-token (relative to mntDir), with the
// exact content the caller asked for, at mode 0600. Mount-free so it runs
// in any environment (no root, no loop device required).
func TestInjectVMFiles_ControlPlaneToken(t *testing.T) {
	mnt := t.TempDir()
	srcCfg := filepath.Join(mnt, "cfg.yaml")
	srcSec := filepath.Join(mnt, "sec.yaml")
	if err := os.WriteFile(srcCfg, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcSec, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := injectVMFiles(mnt, VMPrepareOptions{
		HostConfigPath:    srcCfg,
		HostSecretsPath:   srcSec,
		ControlPlaneToken: "e2e-cp-token-1234",
	}); err != nil {
		t.Fatalf("injectVMFiles: %v", err)
	}

	cpTokenPath := filepath.Join(mnt, "root", ".ephemera-cp-token")
	got, err := os.ReadFile(cpTokenPath)
	if err != nil {
		t.Fatalf("read CP token: %v", err)
	}
	if string(got) != "e2e-cp-token-1234" {
		t.Errorf("CP token content mismatch: got %q", string(got))
	}
	info, err := os.Stat(cpTokenPath)
	if err != nil {
		t.Fatalf("stat CP token: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("CP token mode = %o, want 0600", mode)
	}
}

// TestInjectVMFiles_EmptyControlPlaneToken_SkipsFile keeps the backward-
// compatible behavior: when ControlPlaneToken is empty no file is created,
// so auth-disabled daemons don't leave a confusing empty file behind.
func TestInjectVMFiles_EmptyControlPlaneToken_SkipsFile(t *testing.T) {
	mnt := t.TempDir()
	srcCfg := filepath.Join(mnt, "cfg.yaml")
	srcSec := filepath.Join(mnt, "sec.yaml")
	if err := os.WriteFile(srcCfg, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcSec, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := injectVMFiles(mnt, VMPrepareOptions{
		HostConfigPath:  srcCfg,
		HostSecretsPath: srcSec,
		// ControlPlaneToken left empty.
	}); err != nil {
		t.Fatalf("injectVMFiles: %v", err)
	}

	if _, err := os.Stat(filepath.Join(mnt, "root", ".ephemera-cp-token")); !os.IsNotExist(err) {
		t.Errorf("expected no CP token file, stat err = %v", err)
	}
}

func TestPrepareVM_NoTokenFileWhenTokenEmpty(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	os.MkdirAll(ws, 0755)

	configPath := filepath.Join(tmp, "goose.yaml")
	secretsPath := filepath.Join(tmp, "goose-secrets.yaml")
	os.WriteFile(configPath, []byte("GOOSE_PROVIDER: test\n"), 0644)
	os.WriteFile(secretsPath, []byte("TEST_KEY: x\n"), 0644)

	prepareTestVM(t, tmp, ws, VMPrepareOptions{
		HostConfigPath:  configPath,
		HostSecretsPath: secretsPath,
		AgentToken:      "", // empty — no file should be written
	})

	mntDir := os.Getenv("_TEST_MNT")
	tokenFile := filepath.Join(mntDir, "root", ".ephemera-agent-token")
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Errorf("expected no token file, but it exists (stat err: %v)", err)
	}
}

func TestGooseAgentSourceHashChangesWhenSourceChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd", "goose-agent"), 0755); err != nil {
		t.Fatalf("mkdir goose-agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte(""), 0644); err != nil {
		t.Fatalf("write go.sum: %v", err)
	}
	sourcePath := filepath.Join(root, "cmd", "goose-agent", "main.go")
	if err := os.WriteFile(sourcePath, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	first, err := GooseAgentSourceHash(root)
	if err != nil {
		t.Fatalf("GooseAgentSourceHash first: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("package main\nfunc main() { println(\"changed\") }\n"), 0644); err != nil {
		t.Fatalf("rewrite source: %v", err)
	}
	second, err := GooseAgentSourceHash(root)
	if err != nil {
		t.Fatalf("GooseAgentSourceHash second: %v", err)
	}

	if first == second {
		t.Fatalf("source hash did not change after source edit: %s", first)
	}
}

func TestGooseAgentArtifactIsCurrentUsesStamp(t *testing.T) {
	tmp := t.TempDir()
	binaryPath := filepath.Join(tmp, "goose-agent")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.WriteFile(binaryPath+".sha256", []byte("abc123\n"), 0644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	current, err := gooseAgentArtifactIsCurrent(binaryPath, "abc123")
	if err != nil {
		t.Fatalf("gooseAgentArtifactIsCurrent current: %v", err)
	}
	if !current {
		t.Fatal("artifact with matching stamp should be current")
	}

	current, err = gooseAgentArtifactIsCurrent(binaryPath, "def456")
	if err != nil {
		t.Fatalf("gooseAgentArtifactIsCurrent stale: %v", err)
	}
	if current {
		t.Fatal("artifact with mismatched stamp should be stale")
	}
}

func TestGooseAgentArtifactIsCurrentFalseWhenStampMissing(t *testing.T) {
	tmp := t.TempDir()
	binaryPath := filepath.Join(tmp, "goose-agent")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	current, err := gooseAgentArtifactIsCurrent(binaryPath, "abc123")
	if err != nil {
		t.Fatalf("gooseAgentArtifactIsCurrent: %v", err)
	}
	if current {
		t.Fatal("artifact without stamp should be stale")
	}
}

// TestEnsureGooseAgentAcceptsPrebuiltWhenSourceAbsent covers the release-install
// layout: no cmd/goose-agent source checkout, but the shipped prebuilt binary and
// its source-hash stamp are present (as build_release.sh stages them). EnsureGooseAgent
// must accept the prebuilt artifact as current instead of trying to walk the absent
// source tree (the release daemon-startup blocker fixed on fix/release-daemon-goose-agent).
func TestEnsureGooseAgentAcceptsPrebuiltWhenSourceAbsent(t *testing.T) {
	root := t.TempDir()
	// Deliberately NO cmd/goose-agent source tree under root.
	binaryPath := filepath.Join(root, "artifacts", "goose-agent")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("prebuilt"), 0755); err != nil {
		t.Fatalf("write prebuilt binary: %v", err)
	}
	// A valid stamp is a lowercase hex sha256 (64 chars) — the shape GooseAgentSourceHash emits.
	validHash := strings.Repeat("0123456789abcdef", 4)
	if err := os.WriteFile(binaryPath+gooseAgentStampSuffix, []byte(validHash+"\n"), 0644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	if err := EnsureGooseAgent(binaryPath, root); err != nil {
		t.Fatalf("EnsureGooseAgent with source absent + prebuilt present should succeed, got: %v", err)
	}

	// It must NOT rebuild (no go build); the shipped binary content is untouched.
	got, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read binary after ensure: %v", err)
	}
	if string(got) != "prebuilt" {
		t.Fatalf("prebuilt binary was modified: got %q, want prebuilt", string(got))
	}
}

// TestEnsureGooseAgentDiagnosticWhenSourceAndArtifactAbsent covers a corrupt/incomplete
// release install: no source tree AND no usable prebuilt artifact. EnsureGooseAgent must
// fail with a diagnostic pointing at a broken install, not the raw "walk goose-agent
// sources" lstat error that reads like an INSTALL-contract violation.
func TestEnsureGooseAgentDiagnosticWhenSourceAndArtifactAbsent(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "artifacts", "goose-agent")

	err := EnsureGooseAgent(binaryPath, root)
	if err == nil {
		t.Fatal("EnsureGooseAgent with no source and no artifact should error")
	}
	if strings.Contains(err.Error(), "walk goose-agent sources") {
		t.Fatalf("error should be diagnostic, not the raw walk error: %v", err)
	}
	if !strings.Contains(err.Error(), "goose-agent") {
		t.Fatalf("diagnostic error should mention goose-agent: %v", err)
	}
}

// TestEnsureGooseAgentDiagnosticWhenSourceAbsentAndStampMissing covers a source-less
// install where the binary is present but its source-hash stamp is not — an incomplete
// artifact. EnsureGooseAgent must refuse (diagnostic), because the golden-image path
// downstream reads that stamp unconditionally.
func TestEnsureGooseAgentDiagnosticWhenSourceAbsentAndStampMissing(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "artifacts", "goose-agent")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("prebuilt"), 0755); err != nil {
		t.Fatalf("write prebuilt binary: %v", err)
	}

	err := EnsureGooseAgent(binaryPath, root)
	if err == nil {
		t.Fatal("EnsureGooseAgent with binary but no stamp (source absent) should error")
	}
	if strings.Contains(err.Error(), "walk goose-agent sources") {
		t.Fatalf("error should be diagnostic, not the raw walk error: %v", err)
	}
}

// TestEnsureGooseAgentDiagnosticWhenStampContentInvalid covers a source-less install whose
// prebuilt binary + stamp are both present, but the stamp content is not a valid source hash
// (empty, wrong length, or non-hex garbage). EnsureGooseAgent must reject it at accept time
// with the same diagnostic shape as a missing artifact — not accept it silently (which would
// skew currency downstream) and not defer the failure to EnsureGoldenImageGooseAgent.
func TestEnsureGooseAgentDiagnosticWhenStampContentInvalid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stamp string
	}{
		{"empty", "\n"},
		{"whitespace", "   \n"},
		{"non-hex garbage", "not-a-valid-source-hash\n"},
		{"too short", "deadbeef\n"},
		{"uppercase hex", strings.Repeat("ABCDEF0123456789", 4) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			binaryPath := filepath.Join(root, "artifacts", "goose-agent")
			if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
				t.Fatalf("mkdir artifacts: %v", err)
			}
			if err := os.WriteFile(binaryPath, []byte("prebuilt"), 0755); err != nil {
				t.Fatalf("write prebuilt binary: %v", err)
			}
			if err := os.WriteFile(binaryPath+gooseAgentStampSuffix, []byte(tc.stamp), 0644); err != nil {
				t.Fatalf("write stamp: %v", err)
			}

			err := EnsureGooseAgent(binaryPath, root)
			if err == nil {
				t.Fatalf("EnsureGooseAgent with invalid stamp %q should error", tc.stamp)
			}
			if strings.Contains(err.Error(), "walk goose-agent sources") {
				t.Fatalf("error should be diagnostic, not the raw walk error: %v", err)
			}
			if !strings.Contains(err.Error(), "reinstall from an intact release tarball") {
				t.Fatalf("error should carry the corrupt-install diagnostic: %v", err)
			}
		})
	}
}

// TestEnsureGooseAgentSkipsRebuildWhenStampMatchesSource guards the dev path: when the
// source tree IS present and the artifact stamp already matches the current source hash,
// EnsureGooseAgent short-circuits without shelling out to `go build`.
func TestEnsureGooseAgentSkipsRebuildWhenStampMatchesSource(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd", "goose-agent"), 0755); err != nil {
		t.Fatalf("mkdir goose-agent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cmd", "goose-agent", "main.go"), []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	hash, err := GooseAgentSourceHash(root)
	if err != nil {
		t.Fatalf("GooseAgentSourceHash: %v", err)
	}
	binaryPath := filepath.Join(root, "artifacts", "goose-agent")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte("devbinary"), 0755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.WriteFile(binaryPath+gooseAgentStampSuffix, []byte(hash+"\n"), 0644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	if err := EnsureGooseAgent(binaryPath, root); err != nil {
		t.Fatalf("EnsureGooseAgent dev up-to-date path should succeed, got: %v", err)
	}
	got, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	if string(got) != "devbinary" {
		t.Fatalf("dev binary was rebuilt/modified: got %q, want devbinary", string(got))
	}
}

func TestGooseAgentImageIsCurrentUsesMountedStamp(t *testing.T) {
	mntDir := t.TempDir()
	stampPath := filepath.Join(mntDir, "usr", "local", "bin", "goose-agent.sha256")
	if err := os.MkdirAll(filepath.Dir(stampPath), 0755); err != nil {
		t.Fatalf("mkdir stamp dir: %v", err)
	}
	if err := os.WriteFile(stampPath, []byte("abc123\n"), 0644); err != nil {
		t.Fatalf("write stamp: %v", err)
	}

	current, err := gooseAgentImageIsCurrent(mntDir, "abc123")
	if err != nil {
		t.Fatalf("gooseAgentImageIsCurrent current: %v", err)
	}
	if !current {
		t.Fatal("image with matching stamp should be current")
	}

	current, err = gooseAgentImageIsCurrent(mntDir, "def456")
	if err != nil {
		t.Fatalf("gooseAgentImageIsCurrent stale: %v", err)
	}
	if current {
		t.Fatal("image with mismatched stamp should be stale")
	}
}

func TestInstallGooseAgentIntoMountedImageWritesBinaryAndStamp(t *testing.T) {
	tmp := t.TempDir()
	mntDir := filepath.Join(tmp, "mnt")
	if err := os.MkdirAll(filepath.Join(mntDir, "usr", "local", "bin"), 0755); err != nil {
		t.Fatalf("mkdir image bin dir: %v", err)
	}
	binaryPath := filepath.Join(tmp, "goose-agent")
	if err := os.WriteFile(binaryPath, []byte("new-binary"), 0755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	if err := installGooseAgentIntoMountedImage(mntDir, binaryPath, "abc123"); err != nil {
		t.Fatalf("installGooseAgentIntoMountedImage: %v", err)
	}

	imageBinary, err := os.ReadFile(filepath.Join(mntDir, "usr", "local", "bin", "goose-agent"))
	if err != nil {
		t.Fatalf("read image binary: %v", err)
	}
	if string(imageBinary) != "new-binary" {
		t.Fatalf("image binary = %q, want new-binary", string(imageBinary))
	}
	stamp, err := os.ReadFile(filepath.Join(mntDir, "usr", "local", "bin", "goose-agent.sha256"))
	if err != nil {
		t.Fatalf("read image stamp: %v", err)
	}
	if string(stamp) != "abc123\n" {
		t.Fatalf("image stamp = %q, want abc123 newline", string(stamp))
	}
}

// TestGoldenAgentMarkerSkipDecision drives the out-of-image marker that lets
// EnsureGoldenImageGooseAgent skip the loop mount when the golden image already carries the
// current goose-agent. Skipping the mount is what keeps daemon startup alive after a SIGKILL
// left a COW VM's dm-snapshot pinning the golden image through a busy loop device.
func TestGoldenAgentMarkerSkipDecision(t *testing.T) {
	dir := t.TempDir()
	golden := filepath.Join(dir, "golden-image.ext4")
	if err := os.WriteFile(golden, []byte("image-v1"), 0644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
	const hash = "abc123def456"

	// No marker yet → must mount to verify.
	if goldenAgentMarkerCurrent(golden, hash) {
		t.Fatal("expected not-current with no marker present")
	}

	// After recording the marker → current for the same hash + unchanged image → skip mount.
	if err := writeGoldenAgentMarker(golden, hash); err != nil {
		t.Fatalf("writeGoldenAgentMarker: %v", err)
	}
	if !goldenAgentMarkerCurrent(golden, hash) {
		t.Fatal("expected current after writing marker for the same hash and unchanged image")
	}

	// A different goose-agent source hash → stale → must mount.
	if goldenAgentMarkerCurrent(golden, "different-hash") {
		t.Fatal("expected not-current when the agent source hash changed")
	}

	// A golden-image rebuild (content/size/mtime change) invalidates the marker → must mount.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(golden, []byte("image-v2-rebuilt-larger"), 0644); err != nil {
		t.Fatalf("rewrite golden: %v", err)
	}
	if goldenAgentMarkerCurrent(golden, hash) {
		t.Fatal("expected not-current after the golden image was rebuilt (size/mtime changed)")
	}
}

// --- pinned-artifact verification (EnsureKernel / EnsureFirecracker) --------
//
// These tests never touch the real network: every download endpoint is an
// httptest loopback server, and the "download unavailable" cases point at a
// server that has already been closed (connection refused on 127.0.0.1).

// countingServer serves body and records how many requests it received, so a
// test can assert that the fast path did *not* hit the network.
func countingServer(body []byte) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	return srv, &hits
}

// unreachableURL returns a loopback URL that is guaranteed to refuse connections.
func unreachableURL() string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// fakeFirecrackerTarball builds a .tgz with the same layout as a real firecracker
// release: release-v<ver>-x86_64/firecracker-v<ver>-x86_64 (plus a jailer sibling
// that extractFirecrackerBin must ignore).
func fakeFirecrackerTarball(t *testing.T, binary string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := []struct{ name, content string }{
		{"release-v9.9.9-x86_64/jailer-v9.9.9-x86_64", "jailer-not-this-one"},
		{"release-v9.9.9-x86_64/firecracker-v9.9.9-x86_64", binary},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Mode:     0755,
			Size:     int64(len(e.content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatalf("tar write %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func hexSHA256(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func writeFileOrFail(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// A kernel already on disk whose digest differs from the pin must not be trusted:
// the pin is authoritative, so the artifact is re-fetched and replaced.
func TestEnsureKernelReplacesExistingFileOnPinMismatch(t *testing.T) {
	want := []byte("pinned kernel v2")
	srv, hits := countingServer(want)
	defer srv.Close()

	kernelPath := filepath.Join(t.TempDir(), "vmlinux.bin")
	writeFileOrFail(t, kernelPath, "stale kernel v1", 0644)

	if err := EnsureKernel(kernelPath, srv.URL, hexSHA256(want)); err != nil {
		t.Fatalf("EnsureKernel: %v", err)
	}
	if got := readFileOrFail(t, kernelPath); got != string(want) {
		t.Fatalf("kernel content = %q, want %q", got, want)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("download requests = %d, want 1", n)
	}
}

// If the on-disk kernel is known-bad and cannot be re-fetched, startup must fail
// closed with a diagnostic naming both the mismatch and the download failure.
func TestEnsureKernelFailsClosedWhenMismatchedAndUnreachable(t *testing.T) {
	kernelPath := filepath.Join(t.TempDir(), "vmlinux.bin")
	writeFileOrFail(t, kernelPath, "stale kernel v1", 0644)

	err := EnsureKernel(kernelPath, unreachableURL(), strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("EnsureKernel returned nil for a mismatched on-disk kernel that could not be re-downloaded")
	}
	if !strings.Contains(err.Error(), "kernel SHA256 mismatch") {
		t.Fatalf("EnsureKernel error = %q, want it to name the SHA256 mismatch", err)
	}
}

// Regression: the normal path (kernel present and matching the pin) must stay
// offline and must not rewrite the file.
func TestEnsureKernelAcceptsMatchingFileWithoutDownload(t *testing.T) {
	want := []byte("pinned kernel v2")
	srv, hits := countingServer([]byte("should never be served"))
	defer srv.Close()

	kernelPath := filepath.Join(t.TempDir(), "vmlinux.bin")
	writeFileOrFail(t, kernelPath, string(want), 0644)

	if err := EnsureKernel(kernelPath, srv.URL, hexSHA256(want)); err != nil {
		t.Fatalf("EnsureKernel: %v", err)
	}
	if got := readFileOrFail(t, kernelPath); got != string(want) {
		t.Fatalf("kernel content = %q, want it untouched", got)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("download requests = %d, want 0 (matching kernel must not hit the network)", n)
	}
}

// Baseline: a missing firecracker binary is still downloaded, verified and
// extracted — and now also stamped with its provenance.
func TestEnsureFirecrackerDownloadsWhenMissing(t *testing.T) {
	tgz := fakeFirecrackerTarball(t, "firecracker-binary-v9.9.9")
	srv, hits := countingServer(tgz)
	defer srv.Close()

	destPath := filepath.Join(t.TempDir(), "firecracker")
	if err := EnsureFirecracker(destPath, srv.URL, hexSHA256(tgz)); err != nil {
		t.Fatalf("EnsureFirecracker: %v", err)
	}
	if got := readFileOrFail(t, destPath); got != "firecracker-binary-v9.9.9" {
		t.Fatalf("firecracker content = %q", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("download requests = %d, want 1", n)
	}
	stamp := readFileOrFail(t, destPath+firecrackerStampSuffix)
	if !strings.Contains(stamp, hexSHA256(tgz)) {
		t.Fatalf("stamp = %q, want it to record the pinned tarball digest %s", stamp, hexSHA256(tgz))
	}
	if !strings.Contains(stamp, hexSHA256([]byte("firecracker-binary-v9.9.9"))) {
		t.Fatalf("stamp = %q, want it to record the installed binary digest", stamp)
	}
}

// Regression: a firecracker binary whose stamp proves it came from the pinned
// tarball is accepted without any network I/O.
func TestEnsureFirecrackerAcceptsStampedBinaryWithoutDownload(t *testing.T) {
	tgz := fakeFirecrackerTarball(t, "firecracker-binary-v9.9.9")
	srv, hits := countingServer(tgz)
	defer srv.Close()

	dir := t.TempDir()
	destPath := filepath.Join(dir, "firecracker")
	binary := "firecracker-binary-v9.9.9"
	writeFileOrFail(t, destPath, binary, 0755)
	writeFileOrFail(t, destPath+firecrackerStampSuffix, hexSHA256(tgz)+" "+hexSHA256([]byte(binary))+"\n", 0644)

	if err := EnsureFirecracker(destPath, srv.URL, hexSHA256(tgz)); err != nil {
		t.Fatalf("EnsureFirecracker: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("download requests = %d, want 0 (verified firecracker must not hit the network)", n)
	}
	if got := readFileOrFail(t, destPath); got != binary {
		t.Fatalf("firecracker content = %q, want it untouched", got)
	}
}

// The version-drift case from the audit: the host was provisioned from an older
// pinned release and the pin has since been bumped. The stamp records the old
// tarball digest, so the drift is detected and corrected.
func TestEnsureFirecrackerReprovisionsOnPinDrift(t *testing.T) {
	oldBinary := "firecracker-binary-v1.15.1"
	newTgz := fakeFirecrackerTarball(t, "firecracker-binary-v1.16.1")
	srv, hits := countingServer(newTgz)
	defer srv.Close()

	destPath := filepath.Join(t.TempDir(), "firecracker")
	writeFileOrFail(t, destPath, oldBinary, 0755)
	writeFileOrFail(t, destPath+firecrackerStampSuffix, strings.Repeat("b", 64)+" "+hexSHA256([]byte(oldBinary))+"\n", 0644)

	if err := EnsureFirecracker(destPath, srv.URL, hexSHA256(newTgz)); err != nil {
		t.Fatalf("EnsureFirecracker: %v", err)
	}
	if got := readFileOrFail(t, destPath); got != "firecracker-binary-v1.16.1" {
		t.Fatalf("firecracker content = %q, want the pinned build", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("download requests = %d, want 1", n)
	}
	stamp := readFileOrFail(t, destPath+firecrackerStampSuffix)
	if !strings.Contains(stamp, hexSHA256(newTgz)) {
		t.Fatalf("stamp = %q, want it refreshed to the new pin", stamp)
	}
}

// Tamper detection: the stamp names the right pin, but the bytes on disk no
// longer hash to what was installed.
func TestEnsureFirecrackerReprovisionsOnTamperedBinary(t *testing.T) {
	tgz := fakeFirecrackerTarball(t, "firecracker-binary-v9.9.9")
	srv, hits := countingServer(tgz)
	defer srv.Close()

	destPath := filepath.Join(t.TempDir(), "firecracker")
	writeFileOrFail(t, destPath, "tampered-firecracker", 0755)
	writeFileOrFail(t, destPath+firecrackerStampSuffix,
		hexSHA256(tgz)+" "+hexSHA256([]byte("firecracker-binary-v9.9.9"))+"\n", 0644)

	if err := EnsureFirecracker(destPath, srv.URL, hexSHA256(tgz)); err != nil {
		t.Fatalf("EnsureFirecracker: %v", err)
	}
	if got := readFileOrFail(t, destPath); got != "firecracker-binary-v9.9.9" {
		t.Fatalf("firecracker content = %q, want the tampered binary replaced", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("download requests = %d, want 1", n)
	}
}

// A known-bad firecracker that cannot be re-fetched fails closed.
func TestEnsureFirecrackerFailsClosedOnPinDriftWhenUnreachable(t *testing.T) {
	destPath := filepath.Join(t.TempDir(), "firecracker")
	writeFileOrFail(t, destPath, "firecracker-binary-v1.15.1", 0755)
	writeFileOrFail(t, destPath+firecrackerStampSuffix,
		strings.Repeat("b", 64)+" "+hexSHA256([]byte("firecracker-binary-v1.15.1"))+"\n", 0644)

	err := EnsureFirecracker(destPath, unreachableURL(), strings.Repeat("c", 64))
	if err == nil {
		t.Fatal("EnsureFirecracker returned nil for a known-stale binary that could not be re-provisioned")
	}
	if !strings.Contains(err.Error(), strings.Repeat("c", 64)) {
		t.Fatalf("EnsureFirecracker error = %q, want it to name the pin", err)
	}
}

// Legacy install (binary present, no provenance stamp): the daemon cannot prove
// the binary matches the pin, so it re-provisions from the pinned release.
func TestEnsureFirecrackerReprovisionsUnstampedBinary(t *testing.T) {
	tgz := fakeFirecrackerTarball(t, "firecracker-binary-v9.9.9")
	srv, hits := countingServer(tgz)
	defer srv.Close()

	destPath := filepath.Join(t.TempDir(), "firecracker")
	writeFileOrFail(t, destPath, "firecracker-binary-of-unknown-origin", 0755)

	if err := EnsureFirecracker(destPath, srv.URL, hexSHA256(tgz)); err != nil {
		t.Fatalf("EnsureFirecracker: %v", err)
	}
	if got := readFileOrFail(t, destPath); got != "firecracker-binary-v9.9.9" {
		t.Fatalf("firecracker content = %q, want the pinned build", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("download requests = %d, want 1", n)
	}
	if _, err := os.Stat(destPath + firecrackerStampSuffix); err != nil {
		t.Fatalf("stat provenance stamp: %v", err)
	}
}

// Offline availability: an unstamped binary is *unverified*, not *known bad*.
// If it cannot be re-provisioned (air-gapped host), the daemon keeps running on
// the existing binary rather than refusing to boot.
func TestEnsureFirecrackerKeepsUnstampedBinaryWhenUnreachable(t *testing.T) {
	destPath := filepath.Join(t.TempDir(), "firecracker")
	writeFileOrFail(t, destPath, "firecracker-binary-of-unknown-origin", 0755)

	if err := EnsureFirecracker(destPath, unreachableURL(), strings.Repeat("d", 64)); err != nil {
		t.Fatalf("EnsureFirecracker = %v, want nil (offline host keeps its existing binary)", err)
	}
	if got := readFileOrFail(t, destPath); got != "firecracker-binary-of-unknown-origin" {
		t.Fatalf("firecracker content = %q, want it left in place", got)
	}
}

// realLayoutFirecrackerTarball reproduces the entry ORDER and MODES of a genuine
// firecracker release tarball. Both matter and neither is captured by
// fakeFirecrackerTarball: upstream ships the detached debug object
// (firecracker-v<ver>-x86_64.debug, mode 0644) BEFORE the executable
// (firecracker-v<ver>-x86_64, mode 0755), and both share the "firecracker-"
// prefix. Verified against v1.16.1, where .debug is tar entry 7 and the binary
// is entry 15.
func realLayoutFirecrackerTarball(t *testing.T, binary string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := []struct {
		name    string
		mode    int64
		content string
	}{
		{"release-v9.9.9-x86_64/seccompiler-bin-v9.9.9-x86_64", 0755, "seccompiler"},
		{"release-v9.9.9-x86_64/firecracker-v9.9.9-x86_64.debug", 0644, "DEBUG-OBJECT-NOT-EXECUTABLE"},
		{"release-v9.9.9-x86_64/jailer-v9.9.9-x86_64.debug", 0644, "jailer-debug"},
		{"release-v9.9.9-x86_64/firecracker-v9.9.9-x86_64", 0755, binary},
		{"release-v9.9.9-x86_64/jailer-v9.9.9-x86_64", 0755, "jailer-not-this-one"},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatalf("tar write %s: %v", e.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// The detached debug object is not an executable. Installing it as the hypervisor
// makes every VM spawn die with a segfault, so extraction must select the real
// binary even though the debug object shares its prefix and comes first in the tar.
func TestExtractFirecrackerBinSkipsDebugObject(t *testing.T) {
	dir := t.TempDir()
	tgz := filepath.Join(dir, "fc.tgz")
	writeFileOrFail(t, tgz, string(realLayoutFirecrackerTarball(t, "REAL-FIRECRACKER-BINARY")), 0644)

	dest := filepath.Join(dir, "firecracker")
	if err := extractFirecrackerBin(tgz, dest); err != nil {
		t.Fatalf("extractFirecrackerBin: %v", err)
	}

	if got := readFileOrFail(t, dest); got != "REAL-FIRECRACKER-BINARY" {
		t.Fatalf("installed the wrong tar entry as the hypervisor: got %q, want the executable", got)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat %s: %v", dest, err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("installed firecracker is not executable: mode %v", fi.Mode())
	}
}
