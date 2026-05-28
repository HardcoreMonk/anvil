package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// extractCommon pulls the global --json / --token flags out of args from any
// position (so they work before or after leaf flags/positionals) and returns the
// remaining args for leaf-specific parsing.
func extractCommon(args []string) (jsonOut bool, token string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			jsonOut = true
		case a == "--token" || a == "-token":
			if i+1 < len(args) {
				token = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--token="):
			token = strings.TrimPrefix(a, "--token=")
		case strings.HasPrefix(a, "-token="):
			token = strings.TrimPrefix(a, "-token=")
		default:
			rest = append(rest, a)
		}
	}
	return jsonOut, token, rest
}

func mkClient(tokOverride string) *client {
	return newClient(resolveURL(), resolveToken(tokOverride))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func confirm(jsonOut bool, data []byte, msg string) {
	if jsonOut {
		printJSON(data)
		return
	}
	fmt.Println(msg)
}

// ── vm ───────────────────────────────────────────────────────────────────────

func vmCmd(args []string) {
	if len(args) == 0 {
		die(fmt.Errorf("vm: missing verb (spawn|ls|rm|health|stop|task|stats|snapshot)"))
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "spawn":
		vmSpawn(rest)
	case "ls":
		vmList(rest)
	case "rm":
		vmSimple(rest, "DELETE", "/vms/%s", "stopped")
	case "stop":
		vmProxy(rest, "POST", "/vms/%s/stop")
	case "health":
		vmProxy(rest, "GET", "/vms/%s/health")
	case "stats":
		vmProxy(rest, "GET", "/vms/%s/stats")
	case "task":
		vmTask(rest)
	case "snapshot":
		vmSnapshot(rest)
	default:
		die(fmt.Errorf("vm: unknown verb %q", verb))
	}
}

func vmSpawn(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("vm spawn", flag.ExitOnError)
	profile := fs.String("profile", "", "agent profile")
	fs.Parse(rest)
	body := map[string]string{}
	if *profile != "" {
		body["profile"] = *profile
	}
	data, err := mkClient(tok).do("POST", "/vms", body)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var r vmSpawnResult
	if err := json.Unmarshal(data, &r); err != nil {
		die(err)
	}
	fmt.Printf("vm_id:       %s\nguest_ip:    %s\nagent_url:   %s\nagent_token: %s\n", r.VMID, r.GuestIP, r.AgentURL, r.AgentToken)
}

func vmList(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("vm ls", flag.ExitOnError)
	stats := fs.Bool("stats", false, "include per-VM stats (use with --json for full detail)")
	fs.Parse(rest)
	path := "/vms"
	if *stats {
		path += "?stats=true"
	}
	data, err := mkClient(tok).do("GET", path, nil)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var vms []vmInfo
	if err := json.Unmarshal(data, &vms); err != nil {
		die(err)
	}
	tw := newTab()
	fmt.Fprintln(tw, "VM_ID\tGUEST_IP\tPROFILE\tAGENT_URL")
	for _, v := range vms {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", v.VMID, v.GuestIP, dash(v.Profile), v.AgentURL)
	}
	tw.Flush()
}

// vmSimple performs a verbless DELETE-style call needing one id positional.
func vmSimple(args []string, method, pathFmt, okWord string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("vm", flag.ExitOnError)
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" {
		die(fmt.Errorf("missing vm_id"))
	}
	data, err := mkClient(tok).do(method, fmt.Sprintf(pathFmt, id), nil)
	if err != nil {
		die(err)
	}
	confirm(jsonOut, data, fmt.Sprintf("vm %s %s", id, okWord))
}

// vmProxy forwards to an agent proxy endpoint and prints the response body.
func vmProxy(args []string, method, pathFmt string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("vm", flag.ExitOnError)
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" {
		die(fmt.Errorf("missing vm_id"))
	}
	data, err := mkClient(tok).do(method, fmt.Sprintf(pathFmt, id), nil)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	fmt.Println(strings.TrimSpace(string(data)))
}

func vmTask(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	if len(rest) < 2 {
		die(fmt.Errorf("usage: vm task <vm_id> <prompt>"))
	}
	id := rest[0]
	prompt := strings.Join(rest[1:], " ")
	data, err := mkClient(tok).do("POST", "/vms/"+id+"/tasks", map[string]string{"prompt": prompt})
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var resp struct {
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(data, &resp) == nil && (resp.Output != "" || resp.Error != "") {
		if resp.Error != "" {
			die(fmt.Errorf("agent error: %s", resp.Error))
		}
		fmt.Println(resp.Output)
		return
	}
	fmt.Println(strings.TrimSpace(string(data)))
}

func vmSnapshot(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("vm snapshot", flag.ExitOnError)
	stopAfter := fs.Bool("stop-after", false, "destroy the VM after snapshotting")
	typ := fs.String("type", "", "snapshot type: full | diff (default auto)")
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" {
		die(fmt.Errorf("missing vm_id"))
	}
	body := map[string]any{"stop_after": *stopAfter}
	if *typ != "" {
		body["type"] = *typ
	}
	data, err := mkClient(tok).do("POST", "/vms/"+id+"/snapshot", body)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var s snapshotInfo
	if err := json.Unmarshal(data, &s); err != nil {
		die(err)
	}
	fmt.Printf("snapshot_id: %s\ntype:        %s\nsource_vm:   %s\n", s.SnapshotID, s.SnapshotType, s.SourceVMID)
}

// ── flock ──────────────────────────────────────────────────────────────────

func flockCmd(args []string) {
	if len(args) == 0 {
		die(fmt.Errorf("flock: missing verb (create|ls|get|rm|post|wall|restart|add-agent|rm-agent|set-role)"))
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "create":
		flockCreate(rest)
	case "ls":
		flockList(rest)
	case "get":
		flockGet(rest)
	case "rm":
		flockRm(rest)
	case "post":
		flockPost(rest)
	case "wall":
		flockWall(rest)
	case "restart":
		flockRestart(rest)
	case "add-agent":
		flockAddAgent(rest)
	case "rm-agent":
		flockRmAgent(rest)
	case "set-role":
		flockSetRole(rest)
	default:
		die(fmt.Errorf("flock: unknown verb %q", verb))
	}
}

func flockCreate(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("flock create", flag.ExitOnError)
	task := fs.String("task", "", "task description (required)")
	roles := fs.String("roles", "", "comma-separated roles, e.g. orchestrator,worker,reviewer (required)")
	fs.Parse(rest)
	if *task == "" || *roles == "" {
		die(fmt.Errorf("flock create requires --task and --roles"))
	}
	body := map[string]any{"task": *task, "roles": strings.Split(*roles, ",")}
	data, err := mkClient(tok).do("POST", "/flocks", body)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var r flockCreateResponse
	if err := json.Unmarshal(data, &r); err != nil {
		die(err)
	}
	fmt.Printf("flock_id:     %s\ntownwall_url: %s\n", r.FlockID, r.TownWallURL)
	tw := newTab()
	fmt.Fprintln(tw, "AGENT_ID\tROLE\tVM_ID\tAGENT_TOKEN")
	for _, a := range r.Agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.AgentID, a.Role, a.VMID, r.AgentTokens[a.AgentID])
	}
	tw.Flush()
}

func flockList(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	flag.NewFlagSet("flock ls", flag.ExitOnError).Parse(rest)
	data, err := mkClient(tok).do("GET", "/flocks", nil)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var flocks []flock
	if err := json.Unmarshal(data, &flocks); err != nil {
		die(err)
	}
	tw := newTab()
	fmt.Fprintln(tw, "FLOCK_ID\tAGENTS\tTASK")
	for _, f := range flocks {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", f.FlockID, len(f.Agents), f.Task)
	}
	tw.Flush()
}

func flockGet(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("flock get", flag.ExitOnError)
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" {
		die(fmt.Errorf("missing flock_id"))
	}
	data, err := mkClient(tok).do("GET", "/flocks/"+id, nil)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var f flock
	if err := json.Unmarshal(data, &f); err != nil {
		die(err)
	}
	fmt.Printf("flock_id: %s\ntask:     %s\n", f.FlockID, f.Task)
	tw := newTab()
	fmt.Fprintln(tw, "AGENT_ID\tROLE\tSTATUS\tVM_ID")
	for _, a := range f.Agents {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.AgentID, a.Role, a.Status, a.VMID)
	}
	tw.Flush()
}

func flockRm(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("flock rm", flag.ExitOnError)
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" {
		die(fmt.Errorf("missing flock_id"))
	}
	data, err := mkClient(tok).do("DELETE", "/flocks/"+id, nil)
	if err != nil {
		die(err)
	}
	confirm(jsonOut, data, fmt.Sprintf("flock %s deleted", id))
}

func flockPost(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("flock post", flag.ExitOnError)
	agent := fs.String("agent", "", "agent_id (required)")
	body := fs.String("body", "", "message body (required)")
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" || *agent == "" || *body == "" {
		die(fmt.Errorf("usage: flock post <flock_id> --agent A --body B"))
	}
	data, err := mkClient(tok).do("POST", "/flocks/"+id+"/post", map[string]string{"agent_id": *agent, "body": *body})
	if err != nil {
		die(err)
	}
	confirm(jsonOut, data, "posted to Town Wall")
}

func flockRestart(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	if len(rest) < 2 {
		die(fmt.Errorf("usage: flock restart <flock_id> <agent_id>"))
	}
	flockID, agentID := rest[0], rest[1]
	data, err := mkClient(tok).do("POST", "/flocks/"+flockID+"/agents/"+agentID+"/restart", nil)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var r vmRestoreResult
	if err := json.Unmarshal(data, &r); err != nil {
		die(err)
	}
	fmt.Printf("agent %s restarted → new vm_id %s\n", agentID, r.VMID)
}

// flock add-agent <flock_id> <role> — POST /flocks/{id}/agents (v0.4.3).
func flockAddAgent(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	if len(rest) < 2 {
		die(fmt.Errorf("usage: flock add-agent <flock_id> <role>"))
	}
	flockID, role := rest[0], rest[1]
	data, err := mkClient(tok).do("POST", "/flocks/"+flockID+"/agents", map[string]string{"role": role})
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var r struct {
		AgentID    string `json:"agent_id"`
		Role       string `json:"role"`
		VMID       string `json:"vm_id"`
		AgentToken string `json:"agent_token"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		die(err)
	}
	fmt.Printf("agent_id:    %s\nrole:        %s\nvm_id:       %s\nagent_token: %s\n", r.AgentID, r.Role, r.VMID, r.AgentToken)
}

// flock rm-agent <flock_id> <agent_id> — DELETE /flocks/{id}/agents/{agent_id} (v0.4.3).
func flockRmAgent(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	if len(rest) < 2 {
		die(fmt.Errorf("usage: flock rm-agent <flock_id> <agent_id>"))
	}
	flockID, agentID := rest[0], rest[1]
	data, err := mkClient(tok).do("DELETE", "/flocks/"+flockID+"/agents/"+agentID, nil)
	if err != nil {
		die(err)
	}
	confirm(jsonOut, data, fmt.Sprintf("agent %s removed from flock %s", agentID, flockID))
}

// flock set-role <flock_id> <agent_id> <role> — PATCH /flocks/{id}/agents/{agent_id} (v0.4.3).
// Recreates the agent's VM under the new role (agent_id + token preserved).
func flockSetRole(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	if len(rest) < 3 {
		die(fmt.Errorf("usage: flock set-role <flock_id> <agent_id> <role>"))
	}
	flockID, agentID, role := rest[0], rest[1], rest[2]
	data, err := mkClient(tok).do("PATCH", "/flocks/"+flockID+"/agents/"+agentID, map[string]string{"role": role})
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var r vmInfo
	if err := json.Unmarshal(data, &r); err != nil {
		die(err)
	}
	fmt.Printf("agent %s role → %s, new vm_id %s\n", agentID, role, r.VMID)
}

func flockWall(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("flock wall", flag.ExitOnError)
	history := fs.Bool("history", false, "print finite history instead of streaming")
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" {
		die(fmt.Errorf("missing flock_id"))
	}
	c := mkClient(tok)
	if *history {
		data, err := c.do("GET", "/flocks/"+id+"/wall/history", nil)
		if err != nil {
			die(err)
		}
		if jsonOut {
			printJSON(data)
			return
		}
		var msgs []townWallMessage
		if err := json.Unmarshal(data, &msgs); err != nil {
			die(err)
		}
		tw := newTab()
		fmt.Fprintln(tw, "SEQ\tTIMESTAMP\tAGENT\tBODY")
		for _, m := range msgs {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", m.Seq, m.Timestamp, m.AgentID, strings.ReplaceAll(m.Body, "\n", " "))
		}
		tw.Flush()
		return
	}
	// Stream SSE until interrupted (Ctrl-C).
	req, _ := http.NewRequest("GET", c.baseURL+"/flocks/"+id+"/wall", nil)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		die(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		die(fmt.Errorf("GET /flocks/%s/wall → HTTP %d", id, resp.StatusCode))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			fmt.Println(strings.TrimPrefix(line, "data: "))
		}
	}
}

// ── snapshot ─────────────────────────────────────────────────────────────────

func snapshotCmd(args []string) {
	if len(args) == 0 {
		die(fmt.Errorf("snapshot: missing verb (ls|restore|rm)"))
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "ls":
		snapshotList(rest)
	case "restore":
		snapshotRestore(rest)
	case "rm":
		snapshotRm(rest)
	default:
		die(fmt.Errorf("snapshot: unknown verb %q", verb))
	}
}

func snapshotList(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	flag.NewFlagSet("snapshot ls", flag.ExitOnError).Parse(rest)
	data, err := mkClient(tok).do("GET", "/snapshots", nil)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var snaps []snapshotInfo
	if err := json.Unmarshal(data, &snaps); err != nil {
		die(err)
	}
	tw := newTab()
	fmt.Fprintln(tw, "SNAPSHOT_ID\tTYPE\tSOURCE_VM\tBASE\tCREATED")
	for _, s := range snaps {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.SnapshotID, s.SnapshotType, s.SourceVMID, dash(s.BaseSnapshotID), s.CreatedAt.Format("2006-01-02T15:04:05Z"))
	}
	tw.Flush()
}

func snapshotRestore(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("snapshot restore", flag.ExitOnError)
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" {
		die(fmt.Errorf("missing snapshot_id"))
	}
	data, err := mkClient(tok).do("POST", "/snapshots/"+id+"/restore", nil)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var r vmRestoreResult
	if err := json.Unmarshal(data, &r); err != nil {
		die(err)
	}
	fmt.Printf("restored vm_id: %s\nguest_ip:       %s\nagent_url:      %s\n", r.VMID, r.GuestIP, r.AgentURL)
}

func snapshotRm(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("snapshot rm", flag.ExitOnError)
	fs.Parse(rest)
	id := fs.Arg(0)
	if id == "" {
		die(fmt.Errorf("missing snapshot_id"))
	}
	data, err := mkClient(tok).do("DELETE", "/snapshots/"+id, nil)
	if err != nil {
		die(err)
	}
	confirm(jsonOut, data, fmt.Sprintf("snapshot %s deleted", id))
}

// ── audit ────────────────────────────────────────────────────────────────────

func auditCmd(args []string) {
	jsonOut, tok, rest := extractCommon(args)
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	limit := fs.Int("limit", 100, "max records (server caps at 1000)")
	clientF := fs.String("client", "", "filter by client name")
	statusF := fs.String("status", "", "filter by HTTP status")
	methodF := fs.String("method", "", "filter by HTTP method")
	fs.Parse(rest)
	q := fmt.Sprintf("/audit?limit=%d", *limit)
	if *clientF != "" {
		q += "&client=" + *clientF
	}
	if *statusF != "" {
		q += "&status=" + *statusF
	}
	if *methodF != "" {
		q += "&method=" + *methodF
	}
	data, err := mkClient(tok).do("GET", q, nil)
	if err != nil {
		die(err)
	}
	if jsonOut {
		printJSON(data)
		return
	}
	var recs []auditRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		die(err)
	}
	tw := newTab()
	fmt.Fprintln(tw, "TS\tCLIENT\tMETHOD\tPATH\tSTATUS\tMS")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\n", r.TS, r.Client, r.Method, r.Path, r.Status, r.DurationMs)
	}
	tw.Flush()
}

// ── metrics ──────────────────────────────────────────────────────────────────

func metricsCmd(args []string) {
	_, tok, rest := extractCommon(args)
	flag.NewFlagSet("metrics", flag.ExitOnError).Parse(rest)
	data, err := mkClient(tok).do("GET", "/metrics", nil)
	if err != nil {
		die(err)
	}
	os.Stdout.Write(data) // Prometheus text exposition — always raw
}
