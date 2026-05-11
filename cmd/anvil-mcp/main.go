package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"ephemera/internal/anvilmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "v0.1.0"

func main() {
	cfg, err := anvilmcp.LoadConfig(anvilmcp.ConfigSource{})
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	daemon := anvilmcp.NewDaemonClient(cfg, http.DefaultClient)
	tools := anvilmcp.NewTools(daemon, anvilmcp.NewSessionStore(), time.Duration(cfg.DefaultTimeoutSeconds)*time.Second)
	server := mcp.NewServer(&mcp.Implementation{Name: "anvil-mcp", Version: version}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_spawn_vm",
		Description: "Create an anvil VM and optionally bind a local session_name alias.",
	}, tools.MCPSpawnVM)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_run_task",
		Description: "Run a prompt synchronously in an existing anvil VM using vm_id or session_name.",
	}, tools.MCPRunTask)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_get_vm_health",
		Description: "Return health for an existing anvil VM agent using vm_id or session_name.",
	}, tools.MCPHealth)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_stop_vm",
		Description: "Ask the anvil VM agent to stop gracefully without deleting VM resources.",
	}, tools.MCPStopVM)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "anvil_delete_vm",
		Description: "Delete an anvil VM and release its session_name alias if present.",
	}, tools.MCPDeleteVM)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}
