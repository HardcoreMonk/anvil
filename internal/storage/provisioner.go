package storage

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Provisioner handles the disk lifecycle for MicroVMs.
type Provisioner struct {
	GoldenImagePath string // e.g., "artifacts/ubuntu-22.04-goose.ext4"
	WorkspaceDir    string // e.g., "/tmp/goose-workspaces"
	BuildScriptPath string // e.g., "scripts/build_image.sh"
}

// NewProvisioner initializes a new Storage Provisioner and ensures the golden image exists.
func NewProvisioner(goldenImagePath, workspaceDir, buildScriptPath string) (*Provisioner, error) {
	// 1. Ensure the workspace directory exists
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create workspace directory: %w", err)
	}

	p := &Provisioner{
		GoldenImagePath: goldenImagePath,
		WorkspaceDir:    workspaceDir,
		BuildScriptPath: buildScriptPath,
	}

	// 2. Self-bootstrap: Check and build golden image if missing
	if err := p.EnsureGoldenImage(); err != nil {
		return nil, fmt.Errorf("failed to ensure golden image: %w", err)
	}

	return p, nil
}

// EnsureGoldenImage checks if the golden image exists and is up to date with
// its build inputs (build script + bundled in-VM binaries + bundled scripts).
// If any input is newer than the image, the existing image is removed and
// rebuilt. This prevents the trap where editing goose-agent / micro-init /
// build_image.sh / scripts/gtwall / scripts/gtcall leaves a stale image
// baked with old contents.
//
// Build-input paths are derived from the conventional project layout
// (artifacts/ next to the image, scripts/ next to the build script). Missing
// inputs are ignored so older project trees still work.
func (p *Provisioner) EnsureGoldenImage() error {
	stat, err := os.Stat(p.GoldenImagePath)
	if err == nil {
		artifactsDir := filepath.Dir(p.GoldenImagePath)
		scriptDir := filepath.Dir(p.BuildScriptPath)
		inputs := []string{
			p.BuildScriptPath,
			filepath.Join(artifactsDir, "goose-agent"),
			filepath.Join(artifactsDir, "micro-init"),
			filepath.Join(scriptDir, "gtwall"),
			filepath.Join(scriptDir, "gtcall"),
		}
		if !pathsNewerThan(stat.ModTime(), inputs...) {
			slog.Warn("golden image up to date", "path", p.GoldenImagePath)
			return nil
		}
		slog.Warn("golden image stale (build inputs newer), rebuilding", "path", p.GoldenImagePath)
		if err := os.Remove(p.GoldenImagePath); err != nil {
			return fmt.Errorf("remove stale golden image: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("error checking golden image: %w", err)
	}

	slog.Warn("golden image not found, starting automated build", "path", p.GoldenImagePath)
	slog.Warn("this may take a few minutes")

	// Execute the build script
	cmd := exec.Command("bash", p.BuildScriptPath)

	// Pipe the script's output to the daemon's standard output for visibility
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute build script: %w", err)
	}

	// Verify the image was actually created by the script
	if _, err := os.Stat(p.GoldenImagePath); os.IsNotExist(err) {
		return fmt.Errorf("build script completed, but golden image was not found at expected path")
	}

	slog.Warn("golden image built and verified", "path", p.GoldenImagePath)
	return nil
}

// CloneDiskCOW creates a per-VM COW view over the golden image instead of a
// full byte-for-byte copy. Returns the path Firecracker should open as the
// rootfs (a regular file bind-mounted to a dm-snapshot device), the sparse
// COW exception store path, and the DMSnapshotInfo for teardown.
//
// On error every resource allocated by this call is rolled back, so callers
// only need to handle the error path themselves.
//
// Activated via EPHEMERA_DISK_MODE=cow; the default behavior (full copy) is
// preserved when the variable is unset to keep a safe rollback path.
func (p *Provisioner) CloneDiskCOW(vmID string) (string, string, *DMSnapshotInfo, error) {
	mountTarget := filepath.Join(p.WorkspaceDir, vmID+".ext4")
	cowStore := filepath.Join(p.WorkspaceDir, vmID+".cow")

	// Empty regular file that the dm device will be bind-mounted onto.
	if err := os.WriteFile(mountTarget, nil, 0644); err != nil {
		return "", "", nil, fmt.Errorf("create COW mount target: %w", err)
	}
	info, err := SetupDMSnapshot(p.GoldenImagePath, cowStore, mountTarget)
	if err != nil {
		os.Remove(mountTarget)
		return "", "", nil, err
	}
	slog.Info("provisioned cow rootfs", "vm_id", vmID, "base", p.GoldenImagePath, "exception_store", cowStore)
	return mountTarget, cowStore, info, nil
}

// CloneDisk creates an isolated copy of the golden image for a specific VM.
func (p *Provisioner) CloneDisk(vmID string) (string, error) {
	destPath := filepath.Join(p.WorkspaceDir, fmt.Sprintf("%s.ext4", vmID))
	slog.Info("cloning golden image", "dest", destPath)

	srcFile, err := os.Open(p.GoldenImagePath)
	if err != nil {
		return "", fmt.Errorf("failed to open golden image: %w", err)
	}
	defer srcFile.Close()

	destFile, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		return "", fmt.Errorf("failed to copy disk contents: %w", err)
	}

	if err := destFile.Sync(); err != nil {
		return "", fmt.Errorf("failed to sync data to disk: %w", err)
	}

	slog.Info("provisioned isolated disk", "vm_id", vmID)
	return destPath, nil
}

// mountVMDisk mounts the cloned VM disk, calls fn with the mount point, then unmounts.
// All file injection helpers use this to avoid duplicating mount/unmount logic.
func (p *Provisioner) mountVMDisk(vmID string, fn func(mntDir string) error) error {
	diskPath := filepath.Join(p.WorkspaceDir, fmt.Sprintf("%s.ext4", vmID))
	mntDir := fmt.Sprintf("/tmp/goose-mnt-%s", vmID)

	// Clean up any stale mount left by a previous failed run before attempting
	// a fresh mount. -l (lazy) detaches immediately even if the FS is still busy.
	exec.Command("umount", "-l", mntDir).Run()
	os.Remove(mntDir)

	if err := os.MkdirAll(mntDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount dir: %w", err)
	}

	mounted := false
	defer func() {
		if mounted {
			// -l ensures the unmount succeeds even if a background goroutine
			// still holds a file descriptor on the mounted filesystem.
			if err := exec.Command("umount", "-l", mntDir).Run(); err != nil {
				slog.Warn("unmount failed", "dir", mntDir, "err", err)
			}
		}
		os.Remove(mntDir)
	}()

	if out, err := exec.Command("mount", "-o", "loop", diskPath, mntDir).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to mount VM disk: %w: %s", err, out)
	}
	mounted = true

	return fn(mntDir)
}

// VMPrepareOptions carries all per-VM file injection parameters for PrepareVM.
type VMPrepareOptions struct {
	HostConfigPath  string // path to goose.yaml on the host
	HostSecretsPath string // path to goose-secrets.yaml on the host
	Task            string // optional task prompt written to /root/task.txt
	AgentToken      string // if non-empty, written to /root/.ephemera-agent-token (mode 0600)

	// Goosetown flock context. Empty FlockID disables flock-context injection,
	// preserving standalone-VM behavior.
	FlockID      string
	AgentID      string
	SystemPrompt string // optional role system prompt written to /root/.goose-system-prompt

	// ControlPlaneToken, when non-empty, is written to /root/.ephemera-cp-token
	// (mode 0600) and used by the in-VM /townwall/post forwarder as the bearer
	// when calling back into the control plane. Auto-derived from the host's
	// apiClients[0] by the daemon; empty when control plane auth is disabled.
	ControlPlaneToken string

	// MCPGatewayURL, when non-empty, is written to /root/.ephemera-mcp. The in-VM
	// agent reads it and adds the host MCP gateway as a goose streamable-HTTP
	// extension, so the agent's tools include the gateway's aggregated catalog.
	// Empty (gateway off or profile has no allowed servers) injects nothing.
	MCPGatewayURL string

	// MCPGatewayHostsEntry, when non-empty, is appended to the guest's /etc/hosts
	// (e.g. "10.0.1.1 ephemera-gw"). It lets the letter-starting hostname in
	// MCPGatewayURL resolve to the bridge gateway IP, so goose derives a
	// provider-valid extension/tool-name prefix from the URL. Paired with
	// MCPGatewayURL.
	MCPGatewayHostsEntry string
}

// PrepareVM injects all VM-specific files in a single mount/unmount cycle.
// The file-writing logic lives in injectVMFiles so it can be unit-tested
// against a plain temp directory without mounting a loop device.
func (p *Provisioner) PrepareVM(vmID string, opts VMPrepareOptions) error {
	return p.mountVMDisk(vmID, func(mntDir string) error {
		if err := injectVMFiles(mntDir, opts); err != nil {
			return err
		}
		if err := injectHostTimezone(mntDir); err != nil {
			slog.Warn("inject timezone failed", "err", err)
		}
		slog.Info("config, secrets, timezone injected", "vm_id", vmID)
		return nil
	})
}

// injectVMFiles writes every per-VM file into the mounted rootfs at mntDir.
// Files written (all under /root inside the guest):
//   - /root/.config/goose/config.yaml       (provider, model, extensions)
//   - /root/.config/goose/secrets.yaml      (API keys; requires GOOSE_DISABLE_KEYRING=true)
//   - /root/task.txt                         (task prompt; optional)
//   - /root/.ephemera-agent-token            (Bearer for goose-agent; mode 0600; optional)
//   - /root/.ephemera-flock                  (FLOCK_ID + AGENT_ID; mode 0600; optional)
//   - /root/.goose-system-prompt             (role system prompt; optional)
//   - /root/.ephemera-cp-token               (bearer for in-VM /townwall/post forward; mode 0600; optional)
//   - /root/.ephemera-mcp                     (host MCP gateway URL; optional)
func injectVMFiles(mntDir string, opts VMPrepareOptions) error {
	gooseConfigDir := filepath.Join(mntDir, "root", ".config", "goose")
	if err := os.MkdirAll(gooseConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create goose config dir: %w", err)
	}

	for _, pair := range []struct{ src, dst string }{
		{opts.HostConfigPath, "config.yaml"},
		{opts.HostSecretsPath, "secrets.yaml"},
	} {
		if err := copyFile(pair.src, filepath.Join(gooseConfigDir, pair.dst)); err != nil {
			return fmt.Errorf("failed to inject %s: %w", pair.dst, err)
		}
	}

	// task is optional: empty means persistent mode (goose-agent handles requests).
	if opts.Task != "" {
		taskPath := filepath.Join(mntDir, "root", "task.txt")
		if err := os.WriteFile(taskPath, []byte(opts.Task), 0644); err != nil {
			return fmt.Errorf("failed to write task.txt: %w", err)
		}
	}

	// AgentToken is written with mode 0600 so only root can read it inside the VM.
	if opts.AgentToken != "" {
		tokenPath := filepath.Join(mntDir, "root", ".ephemera-agent-token")
		if err := os.WriteFile(tokenPath, []byte(opts.AgentToken), 0600); err != nil {
			return fmt.Errorf("failed to write agent token: %w", err)
		}
	}

	// Flock context: tells the in-VM agent which flock and agent identity it has.
	// Mode 0600 because AGENT_ID is enough to address the agent on the wall.
	if opts.FlockID != "" {
		flockMeta := fmt.Sprintf("FLOCK_ID=%s\nAGENT_ID=%s\n", opts.FlockID, opts.AgentID)
		flockPath := filepath.Join(mntDir, "root", ".ephemera-flock")
		if err := os.WriteFile(flockPath, []byte(flockMeta), 0600); err != nil {
			return fmt.Errorf("failed to write flock meta: %w", err)
		}
	}

	// System prompt: prepended to every /tasks request by goose-agent.
	if opts.SystemPrompt != "" {
		spPath := filepath.Join(mntDir, "root", ".goose-system-prompt")
		if err := os.WriteFile(spPath, []byte(opts.SystemPrompt), 0644); err != nil {
			return fmt.Errorf("failed to write system prompt: %w", err)
		}
	}

	// CP token: read by the in-VM /townwall/post forwarder when calling back
	// to the control plane. 0600 because anyone with this token could post as
	// a different agent over a forged forward request.
	if opts.ControlPlaneToken != "" {
		cpTokenPath := filepath.Join(mntDir, "root", ".ephemera-cp-token")
		if err := os.WriteFile(cpTokenPath, []byte(opts.ControlPlaneToken), 0600); err != nil {
			return fmt.Errorf("failed to write CP token: %w", err)
		}
	}

	// MCP gateway URL: read by the in-VM agent to add the host gateway as a goose
	// streamable-HTTP extension. Not a secret (caller identity is by source IP), so
	// mode 0644 like the system prompt.
	if opts.MCPGatewayURL != "" {
		mcpPath := filepath.Join(mntDir, "root", ".ephemera-mcp")
		if err := os.WriteFile(mcpPath, []byte(opts.MCPGatewayURL), 0644); err != nil {
			return fmt.Errorf("failed to write mcp gateway url: %w", err)
		}
	}

	// MCP gateway /etc/hosts entry: maps the letter-starting gateway hostname to the
	// bridge IP so the URL above resolves (guest nsswitch is files→dns). Appended so
	// the image's existing localhost line is preserved.
	if opts.MCPGatewayHostsEntry != "" {
		hostsPath := filepath.Join(mntDir, "etc", "hosts")
		f, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("failed to open /etc/hosts: %w", err)
		}
		if _, err := f.WriteString(opts.MCPGatewayHostsEntry + "\n"); err != nil {
			f.Close()
			return fmt.Errorf("failed to append mcp hosts entry: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("failed to close /etc/hosts: %w", err)
		}
	}
	return nil
}

// injectHostTimezone configures the VM disk to use the host's timezone.
// It requires tzdata to be installed in the VM (golden image) so that
// /usr/share/zoneinfo/{tzName} exists for the symlink to resolve correctly.
func injectHostTimezone(mntDir string) error {
	// Derive the IANA timezone name from the host.
	// Prefer /etc/timezone (plain text), fall back to resolving /etc/localtime symlink.
	tzName := "UTC"
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		tzName = strings.TrimSpace(string(b))
	} else if target, err := os.Readlink("/etc/localtime"); err == nil {
		if idx := strings.Index(target, "zoneinfo/"); idx >= 0 {
			tzName = target[idx+len("zoneinfo/"):]
		}
	}

	// Verify the zoneinfo file exists inside the VM before creating the symlink.
	zoneFile := filepath.Join(mntDir, "usr", "share", "zoneinfo", tzName)
	if _, err := os.Stat(zoneFile); err != nil {
		return fmt.Errorf("zoneinfo file not found in VM (%s): tzdata may not be installed", zoneFile)
	}

	// Replace /etc/localtime with a symlink to the correct zoneinfo file.
	// This is the standard Linux approach; glibc reads it automatically when TZ is unset.
	dst := filepath.Join(mntDir, "etc", "localtime")
	os.Remove(dst)
	if err := os.Symlink("/usr/share/zoneinfo/"+tzName, dst); err != nil {
		return fmt.Errorf("failed to create localtime symlink: %w", err)
	}

	// Write /etc/timezone for tools that read the plain-text name.
	tzFile := filepath.Join(mntDir, "etc", "timezone")
	if err := os.WriteFile(tzFile, []byte(tzName+"\n"), 0644); err != nil {
		return fmt.Errorf("failed to write /etc/timezone: %w", err)
	}

	slog.Info("vm timezone set", "tz", tzName)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// pathsNewerThan reports whether any file under any of the given paths has an
// mtime later than refMtime. Each path may be a regular file or a directory
// (walked recursively). Missing paths and walk errors are ignored — the helper
// fails open so a disappeared sibling never blocks startup.
func pathsNewerThan(refMtime time.Time, paths ...string) bool {
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			if info.ModTime().After(refMtime) {
				return true
			}
			continue
		}
		var found bool
		_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || found {
				return nil
			}
			if fi, e := d.Info(); e == nil && fi.ModTime().After(refMtime) {
				found = true
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}

// EnsureMicroInit builds the micro-init binary into binaryPath if it is missing
// OR any source file under cmd/micro-init/ is newer than the binary on disk.
// micro-init runs as PID 1 inside each VM; it mounts virtual filesystems, starts
// goose-agent as a child, and calls poweroff(2) on exit for graceful VM shutdown.
func EnsureMicroInit(binaryPath, projectRoot string) error {
	srcDir := filepath.Join(projectRoot, "cmd", "micro-init")
	if stat, err := os.Stat(binaryPath); err == nil {
		if !pathsNewerThan(stat.ModTime(), srcDir) {
			slog.Warn("micro-init up to date", "path", binaryPath)
			return nil
		}
		slog.Warn("micro-init stale, rebuilding", "path", binaryPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat micro-init: %w", err)
	}

	slog.Warn("building micro-init", "path", binaryPath)

	if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
		return fmt.Errorf("failed to create artifacts dir: %w", err)
	}

	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/micro-init/")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")

	if err := cmd.Run(); err != nil {
		os.Remove(binaryPath)
		return fmt.Errorf("failed to build micro-init: %w", err)
	}

	slog.Warn("micro-init built", "path", binaryPath)
	return nil
}

const gooseAgentStampSuffix = ".sha256"

// GooseAgentSourceHash returns a stable hash of inputs that affect the guest goose-agent binary.
func GooseAgentSourceHash(projectRoot string) (string, error) {
	var files []string
	for _, rel := range []string{"go.mod", "go.sum"} {
		if _, err := os.Stat(filepath.Join(projectRoot, rel)); err == nil {
			files = append(files, rel)
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %w", rel, err)
		}
	}

	agentDir := filepath.Join(projectRoot, "cmd", "goose-agent")
	if err := filepath.WalkDir(agentDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(projectRoot, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk goose-agent sources: %w", err)
	}
	sort.Strings(files)

	h := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(projectRoot, rel))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", rel, err)
		}
		io.WriteString(h, filepath.ToSlash(rel))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func gooseAgentArtifactIsCurrent(binaryPath, wantHash string) (bool, error) {
	if _, err := os.Stat(binaryPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	data, err := os.ReadFile(binaryPath + gooseAgentStampSuffix)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(data)) == wantHash, nil
}

func gooseAgentImageStampPath(mntDir string) string {
	return filepath.Join(mntDir, "usr", "local", "bin", "goose-agent.sha256")
}

func gooseAgentImageIsCurrent(mntDir, wantHash string) (bool, error) {
	data, err := os.ReadFile(gooseAgentImageStampPath(mntDir))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(data)) == wantHash, nil
}

func installGooseAgentIntoMountedImage(mntDir, binaryPath, sourceHash string) error {
	dstDir := filepath.Join(mntDir, "usr", "local", "bin")
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return fmt.Errorf("create image binary dir: %w", err)
	}
	if err := copyFile(binaryPath, filepath.Join(dstDir, "goose-agent")); err != nil {
		return fmt.Errorf("install image goose-agent: %w", err)
	}
	if err := os.Chmod(filepath.Join(dstDir, "goose-agent"), 0755); err != nil {
		return fmt.Errorf("chmod image goose-agent: %w", err)
	}
	if err := os.WriteFile(gooseAgentImageStampPath(mntDir), []byte(sourceHash+"\n"), 0644); err != nil {
		return fmt.Errorf("write image goose-agent stamp: %w", err)
	}
	return nil
}

// goldenAgentMarkerPath returns the path of the out-of-image marker that records which
// goose-agent source hash is baked into the golden image, keyed to the image's on-disk
// identity (size + mtime). Kept beside the golden image so EnsureGoldenImageGooseAgent can
// tell WITHOUT mounting whether the image already carries the current agent — the mount is
// what fails after a crash leaves a COW VM's dm-snapshot pinning the golden image through a
// read-only loop device ("/dev/loopN already mounted or mount point busy").
func goldenAgentMarkerPath(goldenImagePath string) string {
	return goldenImagePath + ".agent-stamp"
}

// goldenAgentMarkerCurrent reports whether the marker beside goldenImagePath shows the image
// already carries sourceHash at its current size+mtime. A missing, unreadable, or mismatched
// marker returns false so the caller mounts and re-verifies. A golden-image rebuild changes
// size/mtime and thus invalidates the marker automatically (no coupling to EnsureGoldenImage).
func goldenAgentMarkerCurrent(goldenImagePath, sourceHash string) bool {
	data, err := os.ReadFile(goldenAgentMarkerPath(goldenImagePath))
	if err != nil {
		return false
	}
	var markHash string
	var markSize, markModNs int64
	// Marker format: "<sourceHash> <imageSize> <imageModNs>". A sha256 hex hash has no
	// spaces, so %s parses it cleanly.
	if n, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%s %d %d", &markHash, &markSize, &markModNs); err != nil || n != 3 {
		return false
	}
	info, err := os.Stat(goldenImagePath)
	if err != nil {
		return false
	}
	return markHash == sourceHash && markSize == info.Size() && markModNs == info.ModTime().UnixNano()
}

// writeGoldenAgentMarker records that goldenImagePath currently carries sourceHash, keyed to
// the image's (post-operation) size+mtime. Best-effort: a write failure only forfeits the
// next start's mount-skip optimization, never correctness.
func writeGoldenAgentMarker(goldenImagePath, sourceHash string) error {
	info, err := os.Stat(goldenImagePath)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("%s %d %d\n", sourceHash, info.Size(), info.ModTime().UnixNano())
	return os.WriteFile(goldenAgentMarkerPath(goldenImagePath), []byte(line), 0644)
}

// EnsureGoldenImageGooseAgent patches an existing golden image when its embedded
// goose-agent source-hash stamp is stale. It assumes EnsureGooseAgent already ran.
func EnsureGoldenImageGooseAgent(goldenImagePath, binaryPath string) error {
	stamp, err := os.ReadFile(binaryPath + gooseAgentStampSuffix)
	if err != nil {
		return fmt.Errorf("read goose-agent source stamp: %w", err)
	}
	sourceHash := strings.TrimSpace(string(stamp))
	if sourceHash == "" {
		return fmt.Errorf("goose-agent source stamp is empty")
	}

	// Fast path: if a prior run already synced this exact agent into this exact image, skip
	// the loop mount entirely. Beyond avoiding wasteful I/O when nothing changed, this is
	// what keeps daemon startup alive after a SIGKILL: an orphaned COW VM's dm-snapshot
	// still pins the golden image through a read-only loop device, so `mount -o loop` would
	// reuse that busy loop and fail with EBUSY ("/dev/loopN already mounted or mount point
	// busy"), aborting startup before RecoverVMs could reclaim the orphan.
	if goldenAgentMarkerCurrent(goldenImagePath, sourceHash) {
		log.Printf("golden image goose-agent already current (source hash %s); skipping mount.", sourceHash)
		return nil
	}

	mntDir, err := os.MkdirTemp("", "goose-golden-image-*")
	if err != nil {
		return fmt.Errorf("create golden image mount dir: %w", err)
	}
	mounted := false
	defer func() {
		if mounted {
			if err := exec.Command("umount", "-l", mntDir).Run(); err != nil {
				log.Printf("Warning: failed to unmount golden image %s: %v", mntDir, err)
			}
		}
		os.RemoveAll(mntDir)
	}()

	if out, err := exec.Command("mount", "-o", "loop", goldenImagePath, mntDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount golden image: %w: %s", err, out)
	}
	mounted = true

	current, err := gooseAgentImageIsCurrent(mntDir, sourceHash)
	if err != nil {
		return fmt.Errorf("check golden image goose-agent stamp: %w", err)
	}
	if current {
		log.Printf("golden image goose-agent is current (source hash %s).", sourceHash)
	} else {
		log.Printf("Patching golden image goose-agent (source hash %s) ...", sourceHash)
		if err := installGooseAgentIntoMountedImage(mntDir, binaryPath, sourceHash); err != nil {
			return err
		}
	}

	// Success: unmount synchronously so writes flush and the golden image's on-disk mtime
	// settles, then record the marker so the next start can skip this mount. If the sync
	// unmount fails, fall back to the deferred lazy unmount and skip the marker (a missing
	// marker only costs one extra mount next start — never correctness).
	if out, err := exec.Command("umount", mntDir).CombinedOutput(); err != nil {
		log.Printf("Warning: sync unmount of golden image %s failed, leaving lazy unmount: %v: %s", mntDir, err, strings.TrimSpace(string(out)))
		return nil
	}
	mounted = false
	if err := writeGoldenAgentMarker(goldenImagePath, sourceHash); err != nil {
		log.Printf("Warning: could not record golden agent marker: %v", err)
	}
	return nil
}

// EnsureGooseAgent builds the goose-agent binary into binaryPath if it doesn't exist
// or if the sidecar source-hash stamp no longer matches the current source tree.
// The binary is compiled from cmd/goose-agent/ in the projectRoot directory using
// CGO_ENABLED=0 so it is statically linked and portable across the VM's glibc version.
func EnsureGooseAgent(binaryPath, projectRoot string) error {
	sourceHash, err := GooseAgentSourceHash(projectRoot)
	if err != nil {
		return err
	}
	current, err := gooseAgentArtifactIsCurrent(binaryPath, sourceHash)
	if err != nil {
		return fmt.Errorf("check goose-agent artifact stamp: %w", err)
	}
	if current {
		log.Printf("goose-agent found at %s (source hash %s).", binaryPath, sourceHash)
		return nil
	}

	log.Printf("Building goose-agent at %s (source hash %s) ...", binaryPath, sourceHash)

	if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
		return fmt.Errorf("failed to create artifacts dir: %w", err)
	}

	tempPath := binaryPath + ".tmp"
	os.Remove(tempPath)
	cmd := exec.Command("go", "build", "-o", tempPath, "./cmd/goose-agent/")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")

	if err := cmd.Run(); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to build goose-agent: %w", err)
	}
	if err := os.Rename(tempPath, binaryPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("replace goose-agent binary: %w", err)
	}
	if err := os.WriteFile(binaryPath+gooseAgentStampSuffix, []byte(sourceHash+"\n"), 0644); err != nil {
		return fmt.Errorf("write goose-agent source stamp: %w", err)
	}

	slog.Warn("goose-agent built", "path", binaryPath)
	return nil
}

// EnsureKernel downloads the Firecracker kernel binary to kernelPath if it does
// not exist and verifies the downloaded bytes against expectedSHA256.
func EnsureKernel(kernelPath, downloadURL, expectedSHA256 string) error {
	if _, err := os.Stat(kernelPath); err == nil {
		slog.Warn("kernel found", "path", kernelPath)
		return nil
	}

	slog.Warn("kernel not found, downloading", "path", kernelPath, "url", downloadURL)

	if err := os.MkdirAll(filepath.Dir(kernelPath), 0755); err != nil {
		return fmt.Errorf("failed to create kernel directory: %w", err)
	}

	resp, err := http.Get(downloadURL) //nolint:noctx
	if err != nil {
		return fmt.Errorf("failed to download kernel: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kernel download returned HTTP %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(kernelPath), ".vmlinux-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temporary kernel file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write kernel: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to flush kernel file: %w", err)
	}

	if actual := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(actual, expectedSHA256) {
		return fmt.Errorf("kernel SHA256 mismatch: expected %s, got %s", expectedSHA256, actual)
	}

	if err := os.Rename(tmpPath, kernelPath); err != nil {
		return fmt.Errorf("failed to install kernel file: %w", err)
	}

	slog.Warn("kernel downloaded", "path", kernelPath)
	return nil
}

// EnsureFirecracker downloads the Firecracker release tarball, verifies its SHA256,
// and extracts the firecracker binary to destPath. A no-op if the binary already exists.
func EnsureFirecracker(destPath, downloadURL, expectedSHA256 string) error {
	if _, err := os.Stat(destPath); err == nil {
		slog.Warn("firecracker found", "path", destPath)
		return nil
	}

	slog.Warn("firecracker not found, downloading", "path", destPath, "url", downloadURL)

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Stream download into a temp file and compute SHA256 simultaneously.
	tmp, err := os.CreateTemp("", "firecracker-*.tgz")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	resp, err := http.Get(downloadURL) //nolint:noctx
	if err != nil {
		tmp.Close()
		return fmt.Errorf("failed to download Firecracker: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("firecracker download returned HTTP %s", resp.Status)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write Firecracker tarball: %w", err)
	}
	tmp.Close()

	if actual := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(actual, expectedSHA256) {
		return fmt.Errorf("firecracker SHA256 mismatch: expected %s, got %s", expectedSHA256, actual)
	}

	if err := extractFirecrackerBin(tmpPath, destPath); err != nil {
		return fmt.Errorf("failed to extract Firecracker binary: %w", err)
	}

	slog.Warn("firecracker installed", "path", destPath)
	return nil
}

// extractFirecrackerBin finds the firecracker binary inside a release .tgz and writes it to dest.
// The release tarball layout is: release-v{VERSION}-x86_64/firecracker-v{VERSION}-x86_64
func extractFirecrackerBin(tgzPath, dest string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to open gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}
		// Match "firecracker-*" but not "jailer-*"
		base := filepath.Base(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || !strings.HasPrefix(base, "firecracker-") {
			continue
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("failed to create binary file: %w", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			os.Remove(dest)
			return fmt.Errorf("failed to write binary: %w", err)
		}
		return out.Close()
	}
	return fmt.Errorf("firecracker binary not found in archive")
}

// CleanupDisk safely removes the disk image after the VM is destroyed.
func (p *Provisioner) CleanupDisk(vmID string) error {
	destPath := filepath.Join(p.WorkspaceDir, fmt.Sprintf("%s.ext4", vmID))
	slog.Info("cleaning up disk image", "path", destPath)

	if err := os.Remove(destPath); err != nil {
		if os.IsNotExist(err) {
			slog.Info("disk image already deleted or missing", "path", destPath)
			return nil
		}
		return fmt.Errorf("failed to delete disk file: %w", err)
	}

	slog.Info("storage resources cleaned up", "vm_id", vmID)
	return nil
}
