package storage

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if err := os.WriteFile(binaryPath+gooseAgentStampSuffix, []byte("deadbeef\n"), 0644); err != nil {
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
