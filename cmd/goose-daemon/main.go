package main

// [v0.7.0 학습 주석] daemon 진입점 개요.
//   - resolveWorkDir()이 EPHEMERA_HOME(systemd 유닛/installer가 심어준 값)을 workdir로
//     쓰고, 없으면 os.Getwd()로 폴백한다(dev/repo 실행 흐름 하위호환).
//   - kernelDownloadURL/kernelSHA256/firecrackerDownloadURL/firecrackerSHA256은
//     scripts/build_release.sh의 pin()이 sed로 그대로 파싱해가는 single source of
//     truth다 — 여기 값을 바꾸면 다음 릴리스 빌드가 자동으로 새 값을 따라간다.
//   - kernel SHA pin + EPHEMERA_HOME 지원은 upstream v0.7.0과의 backport 화해
//     3종 중 2종이며, anvil 기존 구현이 그대로 승리했다(병합은 doc-comment 수준).
//     나머지 1종(waitForAgent per-probe timeout)은 cmd/goose-daemon/api.go에 있다.
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

// [학습 주석] EPHEMERA_HOME 화해 지점: ephemera.service.in의 `Environment=EPHEMERA_HOME=@DEST@`와
// install.sh의 DEST가 여기로 수렴한다. TrimSpace로 공백뿐인 값도 미설정 취급한다.
func resolveWorkDir() (string, error) {
	if home := strings.TrimSpace(os.Getenv("EPHEMERA_HOME")); home != "" {
		return home, nil
	}
	return os.Getwd()
}

func main() {
	initSlog()
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

	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		fatal("fatal: create snapshot dir", "err", err, "dir", snapshotDir)
	}

	// [학습 주석] scripts/build_release.sh의 pin()이 `sed -n "s/.*<name>[[:space:]]*=.../"`
	// 패턴으로 이 4개 상수를 그대로 파싱한다 — 이름/형식(따옴표로 감싼 문자열 리터럴)을
	// 바꾸면 release 스크립트의 파싱이 깨질 수 있다.
	const (
		kernelDownloadURL      = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155"
		kernelSHA256           = "e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2"
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
