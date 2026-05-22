package main

import (
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

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}

func main() {
	initSlog()
	slog.Warn("starting ephemera control plane")
	if len(apiClients) == 0 {
		slog.Warn("api unauthenticated (no tokens configured)")
	}

	cwd, err := os.Getwd()
	if err != nil {
		fatal("fatal: getwd", "err", err)
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

	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		fatal("fatal: create snapshot dir", "err", err, "dir", snapshotDir)
	}

	const (
		kernelDownloadURL      = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155"
		firecrackerDownloadURL = "https://github.com/firecracker-microvm/firecracker/releases/download/v1.15.1/firecracker-v1.15.1-x86_64.tgz"
		firecrackerSHA256      = "d4a32ab2322d887ca1bc4a4e7afa9cc35393e6362dfc2b3becb389d362e4275a"
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
	if err := storage.EnsureKernel(kernelPath, kernelDownloadURL); err != nil {
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
