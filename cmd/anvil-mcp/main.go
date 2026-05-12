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

type toolRegistration struct {
	name        string
	description string
	register    func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools)
}

func toolRegistrations() []toolRegistration {
	return []toolRegistration{
		{
			name:        "anvil_spawn_vm",
			description: "Create an ephemera VM and optionally bind a local session_name alias.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPSpawnVM)
			},
		},
		{
			name:        "anvil_run_task",
			description: "Run a prompt synchronously in an existing ephemera VM using vm_id or session_name.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPRunTask)
			},
		},
		{
			name:        "anvil_copy_in",
			description: "Write a single text file into an ephemera VM workspace using vm_id or session_name.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPCopyIn)
			},
		},
		{
			name:        "anvil_copy_out",
			description: "Read a single text file from an ephemera VM workspace using vm_id or session_name.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPCopyOut)
			},
		},
		{
			name:        "anvil_get_vm_health",
			description: "Return health for an existing ephemera VM agent using vm_id or session_name.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPHealth)
			},
		},
		{
			name:        "anvil_stop_vm",
			description: "Ask the ephemera VM agent to stop gracefully without deleting VM resources.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPStopVM)
			},
		},
		{
			name:        "anvil_delete_vm",
			description: "Delete an ephemera VM and release its session_name alias if present.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPDeleteVM)
			},
		},
		{
			name:        "anvil_create_snapshot",
			description: "Create a full or diff snapshot for an ephemera VM using vm_id or session_name.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPCreateSnapshot)
			},
		},
		{
			name:        "anvil_list_snapshots",
			description: "List snapshots known to the ephemera daemon.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPListSnapshots)
			},
		},
		{
			name:        "anvil_restore_snapshot",
			description: "Restore a new ephemera VM from a snapshot and optionally bind a session_name alias.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPRestoreSnapshot)
			},
		},
		{
			name:        "anvil_delete_snapshot",
			description: "Delete a snapshot by snapshot_id through the ephemera daemon.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPDeleteSnapshot)
			},
		},
	}
}

func main() {
	cfg, err := anvilmcp.LoadConfig(anvilmcp.ConfigSource{})
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	daemon := anvilmcp.NewDaemonClient(cfg, http.DefaultClient)
	tools := anvilmcp.NewTools(daemon, anvilmcp.NewSessionStore(), time.Duration(cfg.DefaultTimeoutSeconds)*time.Second)
	server := mcp.NewServer(&mcp.Implementation{Name: "anvil-mcp", Version: version}, nil)

	for _, registration := range toolRegistrations() {
		registration.register(server, &mcp.Tool{Name: registration.name, Description: registration.description}, tools)
	}

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}
