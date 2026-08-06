package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"ephemera/internal/network"
	"ephemera/internal/storage"
)

// printGooseAgentSourceHash implements the build-time helper subcommand
//
//	ephemera-daemon print-goose-agent-source-hash [projectRoot]
//
// used by scripts/build_release.sh to stamp the shipped goose-agent artifact with the
// exact source hash EnsureGooseAgent computes. That lets a source-less release install
// accept the prebuilt binary as current (EnsureGooseAgent) and lets the golden-image
// path read a consistent stamp — without duplicating the hash algorithm in bash.
func printGooseAgentSourceHash(args []string) {
	root := "."
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		root = args[0]
	}
	h, err := storage.GooseAgentSourceHash(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "print-goose-agent-source-hash: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(h)
}

// initSlog configures the global slog default handler from
// EPHEMERA_LOG_FORMAT (text|json) and EPHEMERA_LOG_LEVEL (debug|info|warn|error).
// Default level is Warn to match the historical log.Printf tone — lifecycle
// transitions are logged at Warn or higher so operators see them by default.
func initSlog() {
	level := slog.LevelWarn
	switch strings.ToLower(os.Getenv("EPHEMERA_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.EqualFold(os.Getenv("EPHEMERA_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

// initUmask narrows the inherited file-creation mask to 0077 so that nothing the
// daemon emits is readable by group or other.
//
// The daemon's artifacts — per-VM rootfs images, snapshot memory.bin/state.bin,
// COW exception stores — embed 0600 secrets (LLM provider API keys, the guest
// agent token, the operator control-plane bearer). Under the default 0022 umask
// they land at 0644/0755, so any unprivileged local account recovers those
// secrets with a single `strings vm-*.ext4`.
//
// Per-call file modes cannot close this: a large share of the artifacts are
// created by child processes (Firecracker, `cp`, `truncate`, the image build
// script), whose modes the daemon does not control. The umask does, because it
// is inherited across fork/exec. It must therefore run before any provisioner or
// storage bootstrap. Anything that genuinely needs broader bits (e.g. an
// executable dropped for another user) sets them explicitly with os.Chmod, which
// the umask does not affect.
func initUmask() {
	syscall.Umask(0o077)
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func resolveWorkDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("EPHEMERA_HOME")); home != "" {
		return home, nil
	}
	return os.Getwd()
}

func main() {
	// Build-time helper subcommand (used by scripts/build_release.sh). Handled before
	// any control-plane startup so it stays a pure, side-effect-free print.
	if len(os.Args) > 1 && os.Args[1] == "print-goose-agent-source-hash" {
		printGooseAgentSourceHash(os.Args[2:])
		return
	}

	initSlog()
	// Before any artifact is created (provisioner, storage bootstrap, snapshot
	// dirs, Firecracker children): everything below inherits this mask.
	initUmask()
	slog.Warn("starting ephemera control plane")
	if len(apiClients) == 0 {
		slog.Warn("api unauthenticated (no tokens configured)")
	}

	// Working directory: artifacts/, scripts/, configs/, snapshots/ etc. are all
	// resolved relative to it. resolveWorkDir pins it to EPHEMERA_HOME when set (by
	// the systemd unit and the installer) so the daemon is robust when launched from
	// anywhere; absent, it falls back to the process working directory (dev / repo flow).
	cwd, err := resolveWorkDir()
	if err != nil {
		fatal("fatal: resolve work dir", "err", err)
	}

	goldenImagePath := filepath.Join(cwd, "artifacts/golden-image.ext4")
	buildScriptPath := filepath.Join(cwd, "scripts/build_image.sh")
	kernelPath := filepath.Join(cwd, "artifacts/vmlinux.bin")
	firecrackerPath := filepath.Join(cwd, "artifacts/firecracker")
	microInitPath := filepath.Join(cwd, "artifacts/micro-init")
	gooseAgentPath := filepath.Join(cwd, "artifacts/goose-agent")
	gooseConfigPath := filepath.Join(cwd, "configs/goose.yaml")
	gooseSecretsPath := filepath.Join(cwd, "configs/goose-secrets.yaml")
	snapshotDir := filepath.Join(cwd, "snapshots")

	if err := os.MkdirAll(snapshotDir, 0700); err != nil {
		fatal("fatal: create snapshot dir", "err", err, "dir", snapshotDir)
	}

	const (
		kernelDownloadURL      = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155"
		kernelSHA256           = "e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2"
		firecrackerDownloadURL = "https://github.com/firecracker-microvm/firecracker/releases/download/v1.16.1/firecracker-v1.16.1-x86_64.tgz"
		firecrackerSHA256      = "382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6"
	)

	// 1. Build in-VM binaries (included in the golden image).
	slog.Warn("ensuring micro-init binary")
	if err := storage.EnsureMicroInit(microInitPath, cwd); err != nil {
		fatal("fatal: ensure micro-init", "err", err)
	}

	slog.Warn("ensuring goose-agent binary")
	if err := storage.EnsureGooseAgent(gooseAgentPath, cwd); err != nil {
		fatal("fatal: ensure goose-agent", "err", err)
	}

	// 2. Bootstrap storage artifacts.
	slog.Warn("initializing storage provisioner")
	provisioner, err := storage.NewProvisioner(goldenImagePath, "/tmp/goose-workspaces", buildScriptPath)
	if err != nil {
		fatal("fatal: storage provisioner", "err", err)
	}
	slog.Warn("ensuring golden image goose-agent")
	if err := storage.EnsureGoldenImageGooseAgent(goldenImagePath, gooseAgentPath); err != nil {
		fatal("fatal: ensure golden image goose-agent", "err", err)
	}

	slog.Warn("ensuring kernel binary")
	if err := storage.EnsureKernel(kernelPath, kernelDownloadURL, kernelSHA256); err != nil {
		fatal("fatal: ensure kernel", "err", err)
	}

	slog.Warn("ensuring firecracker binary")
	if err := storage.EnsureFirecracker(firecrackerPath, firecrackerDownloadURL, firecrackerSHA256); err != nil {
		fatal("fatal: ensure firecracker", "err", err)
	}

	// 3. Network.
	slog.Warn("initializing network manager")
	netManager := network.NewManager("10.0.1.", "10.0.1.1")
	if port, ok := bridgeCallbackPort(apiAddr); ok {
		if err := netManager.AllowBridgeHostPort(port); err != nil {
			slog.Warn("bridge host callback rule failed", "port", port, "err", err)
		}
	}

	// 4. Start control plane.
	cp := NewControlPlane(
		provisioner, netManager,
		kernelPath, firecrackerPath, gooseConfigPath, gooseSecretsPath,
		cwd, snapshotDir,
	)
	defer cp.DestroyAll()

	go func() {
		// Fatal exit on listener errors (e.g. bind: address already in use).
		// Logging-and-continuing here would leave a "live" daemon process with
		// a dead API, which silently masks the failure for any liveness probe.
		if err := cp.Start(); err != nil && err != http.ErrServerClosed {
			fatal("fatal: control plane api", "err", err)
		}
	}()
	defer cp.Shutdown()

	// 5. Wait for termination. SIGHUP reloads API tokens without restarting.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		sig := <-sigChan
		if sig == syscall.SIGHUP {
			cp.ReloadClients()
			continue
		}
		break
	}
	slog.Warn("shutting down")
}
