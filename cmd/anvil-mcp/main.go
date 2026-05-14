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
			description: "Write a single file into an ephemera VM workspace using vm_id or session_name.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPCopyIn)
			},
		},
		{
			name:        "anvil_copy_out",
			description: "Read a single file from an ephemera VM workspace using vm_id or session_name.",
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
		{
			name:        "anvil_spawn_flock",
			description: "Create a Goosetown flock of ephemera VMs and return its Town Wall endpoints.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPSpawnFlock)
			},
		},
		{
			name:        "anvil_list_flocks",
			description: "List live Goosetown flocks known to the ephemera daemon.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPListFlocks)
			},
		},
		{
			name:        "anvil_get_flock",
			description: "Return a Goosetown flock and its agent status by flock_id.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPGetFlock)
			},
		},
		{
			name:        "anvil_delete_flock",
			description: "Delete a Goosetown flock and let the daemon tear down member VMs.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPDeleteFlock)
			},
		},
		{
			name:        "anvil_post_townwall",
			description: "Append a message to a Goosetown flock Town Wall.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPPostTownWall)
			},
		},
		{
			name:        "anvil_get_townwall_history",
			description: "Return the full Town Wall history for a Goosetown flock.",
			register: func(server *mcp.Server, tool *mcp.Tool, tools *anvilmcp.Tools) {
				mcp.AddTool(server, tool, tools.MCPTownWallHistory)
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
	sessions, err := anvilmcp.LoadSessionStore(cfg.SessionStorePath)
	if err != nil {
		log.Fatalf("load session store: %v", err)
	}
	tools := anvilmcp.NewToolsWithOptions(daemon, sessions, time.Duration(cfg.DefaultTimeoutSeconds)*time.Second, anvilmcp.ToolsOptions{
		SessionStorePath: cfg.SessionStorePath,
		DefaultTenantID:  cfg.DefaultTenantID,
		AuditLogPath:     cfg.AuditLogPath,
	})
	server := mcp.NewServer(&mcp.Implementation{Name: "anvil-mcp", Version: version}, nil)

	for _, registration := range toolRegistrations() {
		registration.register(server, &mcp.Tool{Name: registration.name, Description: registration.description}, tools)
	}

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}
