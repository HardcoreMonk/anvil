//go:build ignore

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type spawnOutput struct {
	VMID        string `json:"vm_id"`
	SessionName string `json:"session_name"`
}

type rawOutput struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	command := flag.String("command", "./anvil-mcp", "anvil-mcp command to execute")
	session := flag.String("session", "smoke", "session_name alias to bind")
	profile := flag.String("profile", "", "optional VM profile")
	prompt := flag.String("prompt", "Reply with exactly: anvil-smoke-ok", "prompt for anvil_run_task")
	expectOutput := flag.String("expect-output", "anvil-smoke-ok", "substring expected in anvil_run_task response body; empty disables semantic output check")
	timeout := flag.Duration("timeout", 8*time.Minute, "overall smoke test timeout")
	taskTimeout := flag.Int("task-timeout", 180, "anvil_run_task timeout_seconds")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, *command)
	client := mcp.NewClient(&mcp.Implementation{Name: "anvil-smoke-client", Version: "v0.1.0"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connect anvil-mcp: %w", err)
	}
	defer clientSession.Close()

	fmt.Println("connected to anvil-mcp")
	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	fmt.Printf("tools: %d\n", len(tools.Tools))

	var spawned spawnOutput
	if err := callStructured(ctx, clientSession, "anvil_spawn_vm", map[string]any{
		"profile":      *profile,
		"session_name": *session,
	}, &spawned); err != nil {
		return err
	}
	fmt.Printf("spawned vm_id=%s session_name=%s\n", spawned.VMID, spawned.SessionName)

	cleanup := true
	defer func() {
		if cleanup && spawned.VMID != "" {
			var out rawOutput
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cleanupCancel()
			if err := callStructured(cleanupCtx, clientSession, "anvil_delete_vm", map[string]any{"vm_id": spawned.VMID}, &out); err != nil {
				log.Printf("cleanup delete failed for %s: %v", spawned.VMID, err)
				return
			}
			log.Printf("cleanup delete status=%d body=%s", out.StatusCode, out.Body)
		}
	}()

	var task rawOutput
	if err := callStructured(ctx, clientSession, "anvil_run_task", map[string]any{
		"session_name":    *session,
		"prompt":          *prompt,
		"timeout_seconds": *taskTimeout,
	}, &task); err != nil {
		return err
	}
	fmt.Printf("task status=%d body=%s\n", task.StatusCode, task.Body)
	if *expectOutput != "" && !strings.Contains(task.Body, *expectOutput) {
		return fmt.Errorf("task response did not contain expected output %q", *expectOutput)
	}

	var health rawOutput
	if err := callStructured(ctx, clientSession, "anvil_get_vm_health", map[string]any{
		"session_name": *session,
	}, &health); err != nil {
		return err
	}
	fmt.Printf("health status=%d body=%s\n", health.StatusCode, health.Body)

	var stop rawOutput
	if err := callStructured(ctx, clientSession, "anvil_stop_vm", map[string]any{
		"session_name": *session,
	}, &stop); err != nil {
		return err
	}
	fmt.Printf("stop status=%d body=%s\n", stop.StatusCode, stop.Body)

	var deleted rawOutput
	if err := callStructured(ctx, clientSession, "anvil_delete_vm", map[string]any{
		"session_name": *session,
	}, &deleted); err != nil {
		return err
	}
	cleanup = false
	fmt.Printf("delete status=%d body=%s\n", deleted.StatusCode, deleted.Body)
	fmt.Println("anvil MCP smoke test passed")
	return nil
}

func callStructured(ctx context.Context, session *mcp.ClientSession, name string, args map[string]any, out any) error {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return err
	}
	if result.IsError {
		data, _ := json.Marshal(result)
		return fmt.Errorf("%s: tool returned error: %s", name, data)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return fmt.Errorf("%s: marshal structured content: %w", name, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: decode structured content: %w: %s", name, err, data)
	}
	return nil
}
