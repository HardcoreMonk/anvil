package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ephemera/internal/anvilmcp"
)

const defaultSchedulerAddr = "127.0.0.1:3010"

type schedulerConfig struct {
	Addr               string
	PlacementPath      string
	QuotaStorePath     string
	HostsFile          string
	PollInterval       time.Duration
	ReconcileInterval  time.Duration
	HostTimeout        time.Duration
	FailureThreshold   int
	APIToken           string
	RequirePersistence bool
}

func loadSchedulerConfig() (schedulerConfig, error) {
	addr := strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_ADDR"))
	if addr == "" {
		addr = defaultSchedulerAddr
	}
	pollInterval, err := durationEnv("ANVIL_SCHEDULER_POLL_INTERVAL", 10*time.Second)
	if err != nil {
		return schedulerConfig{}, err
	}
	reconcileInterval, err := durationEnv("ANVIL_SCHEDULER_RECONCILE_INTERVAL", 30*time.Second)
	if err != nil {
		return schedulerConfig{}, err
	}
	hostTimeout, err := durationEnv("ANVIL_SCHEDULER_HOST_TIMEOUT", 3*time.Second)
	if err != nil {
		return schedulerConfig{}, err
	}
	failureThreshold, err := intEnv("ANVIL_SCHEDULER_FAILURE_THRESHOLD", 3)
	if err != nil {
		return schedulerConfig{}, err
	}
	if failureThreshold < 1 {
		return schedulerConfig{}, fmt.Errorf("ANVIL_SCHEDULER_FAILURE_THRESHOLD must be >= 1")
	}
	return schedulerConfig{
		Addr:               addr,
		PlacementPath:      strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_STATE")),
		QuotaStorePath:     strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_QUOTA_STORE")),
		HostsFile:          strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_HOSTS_FILE")),
		PollInterval:       pollInterval,
		ReconcileInterval:  reconcileInterval,
		HostTimeout:        hostTimeout,
		FailureThreshold:   failureThreshold,
		APIToken:           strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_API_TOKEN")),
		RequirePersistence: strings.EqualFold(strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_REQUIRE_PERSISTENCE")), "true"),
	}, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a Go duration: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be > 0", name)
	}
	return parsed, nil
}

func intEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func applyConfiguredHosts(store *anvilmcp.PlacementStore, hosts []anvilmcp.RuntimeHost) error {
	if hosts == nil {
		return nil
	}
	return store.ApplyConfiguredHostsAndSave(hosts)
}

// newSchedulerServer builds the scheduler's HTTP listener.
//
// ReadHeaderTimeout and IdleTimeout both apply before any handler — and thus
// before any token check — runs. Left at net/http's zero value they mean "no
// limit", so an unauthenticated peer could hold connections, goroutines and fds
// open indefinitely.
//
// ReadTimeout and WriteTimeout stay unset on purpose, matching the control plane:
// a global budget on the whole request or response would sever long-lived bodies
// and streamed responses.
func newSchedulerServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func main() {
	cfg, err := loadSchedulerConfig()
	if err != nil {
		log.Fatalf("load scheduler config: %v", err)
	}
	placements := anvilmcp.NewPlacementStore(cfg.PlacementPath)
	if err := placements.Load(); err != nil {
		log.Fatalf("load scheduler placement store: %v", err)
	}
	configuredHosts, err := anvilmcp.LoadSchedulerHostsFile(cfg.HostsFile)
	if err != nil {
		log.Fatalf("load scheduler hosts file: %v", err)
	}
	if err := applyConfiguredHosts(placements, configuredHosts); err != nil {
		log.Fatalf("apply scheduler hosts file: %v", err)
	}
	quotas := anvilmcp.NewQuotaStore(cfg.QuotaStorePath)
	if err := quotas.Load(); err != nil {
		log.Fatalf("load scheduler quota store: %v", err)
	}
	service := anvilmcp.NewSchedulerService(anvilmcp.SchedulerServiceOptions{
		PlacementStore:     placements,
		QuotaStore:         quotas,
		RequirePersistence: cfg.RequirePersistence,
	})
	loop := anvilmcp.NewSchedulerControlLoop(placements, anvilmcp.SchedulerControlLoopOptions{
		APIToken:          cfg.APIToken,
		HostTimeout:       cfg.HostTimeout,
		FailureThreshold:  cfg.FailureThreshold,
		PollInterval:      cfg.PollInterval,
		ReconcileInterval: cfg.ReconcileInterval,
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	loop.Start(ctx)
	log.Printf("anvil scheduler service on %s", cfg.Addr)
	server := newSchedulerServer(cfg.Addr, service.Handler())
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown scheduler service: %v", err)
		}
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("scheduler service: %v", err)
	}
}
