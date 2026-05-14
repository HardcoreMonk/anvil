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

type copyOutOutput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type flockAgentInfo struct {
	AgentID  string `json:"agent_id"`
	Role     string `json:"role"`
	VMID     string `json:"vm_id"`
	AgentURL string `json:"agent_url"`
	Status   string `json:"status"`
}

type spawnFlockOutput struct {
	FlockID      string           `json:"flock_id"`
	Task         string           `json:"task"`
	TenantID     string           `json:"tenant_id,omitempty"`
	EgressPolicy string           `json:"egress_policy,omitempty"`
	Agents       []flockAgentInfo `json:"agents"`
	TownWallURL  string           `json:"townwall_url"`
	PostURL      string           `json:"post_url"`
}

type flockInfo struct {
	FlockID      string                    `json:"flock_id"`
	Task         string                    `json:"task"`
	TenantID     string                    `json:"tenant_id,omitempty"`
	EgressPolicy string                    `json:"egress_policy,omitempty"`
	Agents       map[string]flockAgentInfo `json:"agents"`
	CreatedAt    time.Time                 `json:"created_at"`
}

type listFlocksOutput struct {
	Flocks []flockInfo `json:"flocks"`
}

type townWallMessage struct {
	Timestamp time.Time `json:"timestamp"`
	AgentID   string    `json:"agent_id"`
	Body      string    `json:"body"`
}

type townWallHistoryOutput struct {
	Messages []townWallMessage `json:"messages"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	command := flag.String("command", "./anvil-mcp", "anvil-mcp command to execute")
	mode := flag.String("mode", "lifecycle", "smoke mode: lifecycle, semantic, or flock")
	session := flag.String("session", "smoke", "session_name alias to bind")
	profile := flag.String("profile", "", "optional VM profile")
	prompt := flag.String("prompt", "Reply with exactly: anvil-smoke-ok", "prompt for anvil_run_task")
	expectOutput := flag.String("expect-output", "anvil-smoke-ok", `substring expected in anvil_run_task response body; use -expect-output "" for lifecycle-only mode without semantic output assertion`)
	copyPath := flag.String("copy-path", "smoke/input.txt", "workspace path for copy-in/copy-out smoke")
	copyContent := flag.String("copy-content", "anvil workspace smoke", "workspace content for copy-in/copy-out smoke")
	timeout := flag.Duration("timeout", 8*time.Minute, "overall smoke test timeout")
	taskTimeout := flag.Int("task-timeout", 180, "anvil_run_task timeout_seconds")
	flag.Parse()

	switch *mode {
	case "lifecycle", "semantic", "flock":
	default:
		return fmt.Errorf("unknown mode %q; want lifecycle, semantic, or flock", *mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	commandCtx, stopCommand := context.WithCancel(context.Background())
	defer stopCommand()

	cmd := exec.CommandContext(commandCtx, *command)
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

	switch *mode {
	case "flock":
		return runFlockSmoke(ctx, clientSession)
	}

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

	var copyIn rawOutput
	if err := callStructured(ctx, clientSession, "anvil_copy_in", map[string]any{
		"session_name": *session,
		"path":         *copyPath,
		"content":      *copyContent,
	}, &copyIn); err != nil {
		return err
	}
	fmt.Printf("copy_in status=%d body=%s\n", copyIn.StatusCode, copyIn.Body)

	var copyOut copyOutOutput
	if err := callStructured(ctx, clientSession, "anvil_copy_out", map[string]any{
		"session_name": *session,
		"path":         *copyPath,
	}, &copyOut); err != nil {
		return err
	}
	fmt.Printf("copy_out path=%s content=%q\n", copyOut.Path, copyOut.Content)
	if copyOut.Content != *copyContent {
		return fmt.Errorf("copy_out content = %q, want %q", copyOut.Content, *copyContent)
	}

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

func runFlockSmoke(ctx context.Context, session *mcp.ClientSession) error {
	const smokeBody = "anvil-flock-smoke-ok"

	var spawned spawnFlockOutput
	if err := callStructured(ctx, session, "anvil_spawn_flock", map[string]any{
		"task":  "anvil flock smoke",
		"roles": []string{"orchestrator", "worker"},
	}, &spawned); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup && spawned.FlockID != "" {
			var out rawOutput
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cleanupCancel()
			if err := callStructured(cleanupCtx, session, "anvil_delete_flock", map[string]any{"flock_id": spawned.FlockID}, &out); err != nil {
				log.Printf("cleanup delete flock failed for %s: %v", spawned.FlockID, err)
				return
			}
			log.Printf("cleanup delete flock status=%d body=%s", out.StatusCode, out.Body)
		}
	}()

	fmt.Printf("spawned flock_id=%s agents=%d\n", spawned.FlockID, len(spawned.Agents))
	if spawned.FlockID == "" {
		return fmt.Errorf("anvil_spawn_flock returned empty flock_id")
	}
	if len(spawned.Agents) != 2 {
		return fmt.Errorf("anvil_spawn_flock returned %d agents, want 2", len(spawned.Agents))
	}

	var flocks listFlocksOutput
	if err := callStructured(ctx, session, "anvil_list_flocks", map[string]any{}, &flocks); err != nil {
		return err
	}
	fmt.Printf("flocks: %d\n", len(flocks.Flocks))
	foundFlock := false
	for _, flock := range flocks.Flocks {
		if flock.FlockID == spawned.FlockID {
			foundFlock = true
			break
		}
	}
	if !foundFlock {
		return fmt.Errorf("anvil_list_flocks did not contain spawned flock_id %q", spawned.FlockID)
	}

	var posted townWallMessage
	if err := callStructured(ctx, session, "anvil_post_townwall", map[string]any{
		"flock_id": spawned.FlockID,
		"agent_id": "orchestrator",
		"body":     smokeBody,
	}, &posted); err != nil {
		return err
	}
	fmt.Printf("posted townwall agent_id=%s body=%q\n", posted.AgentID, posted.Body)
	if posted.AgentID != "orchestrator" {
		return fmt.Errorf("anvil_post_townwall agent_id = %q, want orchestrator", posted.AgentID)
	}
	if posted.Body != smokeBody {
		return fmt.Errorf("anvil_post_townwall body = %q, want %q", posted.Body, smokeBody)
	}

	var history townWallHistoryOutput
	if err := callStructured(ctx, session, "anvil_get_townwall_history", map[string]any{
		"flock_id": spawned.FlockID,
	}, &history); err != nil {
		return err
	}
	found := false
	for _, msg := range history.Messages {
		if msg.Body == smokeBody {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("townwall history did not contain body %q", smokeBody)
	}

	var deleted rawOutput
	if err := callStructured(ctx, session, "anvil_delete_flock", map[string]any{
		"flock_id": spawned.FlockID,
	}, &deleted); err != nil {
		return err
	}
	cleanup = false
	fmt.Printf("delete flock status=%d body=%s\n", deleted.StatusCode, deleted.Body)
	fmt.Println("anvil MCP flock smoke test passed")
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
