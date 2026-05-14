package main

import (
	"log"
	"net/http"
	"os"
	"strings"

	"ephemera/internal/anvilmcp"
)

const defaultSchedulerAddr = "127.0.0.1:3010"

type schedulerConfig struct {
	Addr           string
	PlacementPath  string
	QuotaStorePath string
}

func loadSchedulerConfig() schedulerConfig {
	addr := strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_ADDR"))
	if addr == "" {
		addr = defaultSchedulerAddr
	}
	return schedulerConfig{
		Addr:           addr,
		PlacementPath:  strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_STATE")),
		QuotaStorePath: strings.TrimSpace(os.Getenv("ANVIL_SCHEDULER_QUOTA_STORE")),
	}
}

func main() {
	cfg := loadSchedulerConfig()
	placements := anvilmcp.NewPlacementStore(cfg.PlacementPath)
	if err := placements.Load(); err != nil {
		log.Fatalf("load scheduler placement store: %v", err)
	}
	quotas := anvilmcp.NewQuotaStore(cfg.QuotaStorePath)
	if err := quotas.Load(); err != nil {
		log.Fatalf("load scheduler quota store: %v", err)
	}
	service := anvilmcp.NewSchedulerService(anvilmcp.SchedulerServiceOptions{
		PlacementStore: placements,
		QuotaStore:     quotas,
	})
	log.Printf("anvil scheduler service on %s", cfg.Addr)
	if err := http.ListenAndServe(cfg.Addr, service.Handler()); err != nil {
		log.Fatalf("scheduler service: %v", err)
	}
}
