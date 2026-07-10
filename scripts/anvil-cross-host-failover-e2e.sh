#!/usr/bin/env bash
set -Eeuo pipefail

# anvil-cross-host-failover-e2e.sh — KVM e2e for cross-host home (Town Wall hub)
# FAILOVER: the adapter re-elects a routed flock's home after the current home
# stops answering, promotes the elected member to hub, retargets every other
# member's relay, and (finally) promotes the REAL member daemon's relay flock to
# hub — all while the single real guest's `gtwall` keeps landing on whatever host
# currently owns the wall.
#
# Topology (single physical host — one real daemon, same reason the wall/gtcall
# e2es stub the home: two real daemons collide on the guest bridge 10.0.1.1):
#
#   real anvil-daemon (member, auth-on, hosts the ONE real KVM VM = researcher)
#   stub A  (python3 recorder, 127.0.0.1:3100)  — initial home  (coordinator)
#   stub B  (python3 recorder, 127.0.0.1:3101)  — 1st election candidate (helper)
#   adapter (cmd/anvil-mcp over MCP stdio, cross_host_flock_create_mode=members_only,
#            reconcile_interval=2s) — owns the reconcile/failover control loop.
#
# Roles order [coordinator, helper, researcher] + per-host capacity 1 makes the
# scheduler place coordinator@stub-a (home), helper@stub-b, researcher@member in
# that exact order, so record.Agents order IS the deterministic election order:
#   home stub-a dies  -> elect stub-b   (first reachable non-home member)
#   home stub-b dies  -> elect member   (stub-a already dead, member reachable)
#
# The stubs extend the wall/gtcall recorder: GET /vms -> [] (adapter reachability
# probe = ListVMs), POST /vms -> a fixed spawn response (member "spawn"),
# POST /flocks/{id}/distributed|/relay|/post -> record + 2xx, DELETE /flocks/{id}
# -> record + 200. Loopback-bound. The real daemon boots the real researcher VM
# and runs the real in-guest gtwall workload (same path as the wall e2e).
#
# What each phase asserts, and WHY it fails if the behavior regresses:
#   Phase 0  home==stub-a in placements.json; stub-a got the hub /distributed;
#            member holds the relay flock (GET /flocks/{id}=200); guest gtwall
#            lands on stub-a with bearer == relay token.
#   Phase 1  kill stub A -> poll home==stub-b; stub-b got a hub /distributed with
#            roster 3 + the SAME relay token; guest gtwall now lands on stub-b
#            (member relay was retargeted — the wire proof); adapter stderr has a
#            failover line but leaks no relay/call token and no stub address.
#   Phase 2  kill stub B -> poll home==member; the REAL daemon's flock is promoted
#            relay->hub: GET /flocks/{id}/wall/history is 200 and STARTS EMPTY
#            (wall-loss contract — phase 0/1 messages lived on the stubs, never a
#            real wall), then holds ONLY the phase-2 message after gtwall; daemon
#            stderr leaks no token and no stub address.
#   Phase 3  restart stub B -> it receives a relay RE-registration (revived old
#            member heals back into a relay). The real-daemon hub->relay demotion
#            (a revived old HOME becoming a relay) needs a second real daemon to
#            hold kind state, so it stays in the manual 2-daemon check
#            (docs/operations/2026-07-08-cross-host-manual-verification.md §6).
#
# ── NOT covered here (deliberate, out of single-host CI scope) ──────────────
# A real HOME daemon owning the canonical wall on a second host, and the
# hub->relay demotion of a REVIVED old real home, are MANUAL two-machine checks
# (manual-verification §5/§6). One host cannot run two daemons without bridge/IP
# collisions, so those paths are verified by hand and excluded from this gate.
#
# Non-LLM: gtwall carries no model call, so this passes with no provider key.

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  cat >&2 <<'USAGE'
Usage: sudo -n bash scripts/anvil-cross-host-failover-e2e.sh

Environment (all optional):
  ANVIL_FO_API=http://127.0.0.1:3000        member control-plane URL (curl target)
  ANVIL_FO_OP_TOKEN=op-e2e                   operator bearer (member daemon auth-on)
  ANVIL_FO_DAEMON_BIND=0.0.0.0:3000          EPHEMERA_API_ADDR for the member daemon
  ANVIL_FO_STUB_A_PORT=3100                  stub A (initial home) loopback port
  ANVIL_FO_STUB_B_PORT=3101                  stub B (1st candidate) loopback port
  ANVIL_FO_RECONCILE=2s                      adapter reconcile_interval
  ANVIL_FO_FAILOVER_TIMEOUT=60               seconds to wait for each re-election
  ANVIL_FO_TASK_TIMEOUT=120                  in-guest gtwall workload timeout
  ANVIL_FO_ARTIFACT_DIR=/tmp/anvil-cross-host-failover-e2e-<timestamp>

The member daemon binds 0.0.0.0:3000 so the guest reaches it on the bridge
gateway (http://10.0.1.1:3000); the host still talks to it over 127.0.0.1:3000.
USAGE
}

if [[ $# -eq 1 && ( "${1:-}" == "-h" || "${1:-}" == "--help" ) ]]; then
  usage
  exit 0
fi
if [[ $# -gt 0 ]]; then
  usage
  exit 2
fi

API="${ANVIL_FO_API:-http://127.0.0.1:3000}"
OP_TOKEN="${ANVIL_FO_OP_TOKEN:-op-e2e}"
DAEMON_BIND="${ANVIL_FO_DAEMON_BIND:-0.0.0.0:3000}"
STUB_A_PORT="${ANVIL_FO_STUB_A_PORT:-3100}"
STUB_B_PORT="${ANVIL_FO_STUB_B_PORT:-3101}"
STUB_A_ADDR="http://127.0.0.1:${STUB_A_PORT}"
STUB_B_ADDR="http://127.0.0.1:${STUB_B_PORT}"
RECONCILE="${ANVIL_FO_RECONCILE:-2s}"
FAILOVER_TIMEOUT="${ANVIL_FO_FAILOVER_TIMEOUT:-60}"
TASK_TIMEOUT="${ANVIL_FO_TASK_TIMEOUT:-120}"
ARTIFACT_DIR="${ANVIL_FO_ARTIFACT_DIR:-/tmp/anvil-cross-host-failover-e2e-$(date +%Y%m%d-%H%M%S)}"

# Host NAMES (not addresses) — these appear in the adapter's failover log line,
# so they must never be loopback addresses (redaction spot check depends on it).
# The scheduler places roles across hosts in ALPHABETICAL name order (ListHosts
# sorts), and record.Agents keeps that order, so election order == name order.
# These names are chosen so home < candidate < member alphabetically, making
# coordinator@host-a-home (initial home), helper@host-b-cand (1st candidate),
# researcher@host-c-member (the real VM) the deterministic placement + failover
# chain.
HOST_A="host-a-home"
HOST_B="host-b-cand"
HOST_MEMBER="host-c-member"

# Operator bearer authenticates every host-side controller curl (VM/flock admin);
# the guest never sees it (it uses the injected relay token via .ephemera-cp-token).
CURL_AUTH_ARGS=(-H "Authorization: Bearer $OP_TOKEN")

PASS=true
FAIL_REASONS=()
STUB_A_PID=""
STUB_B_PID=""
DAEMON_PID=""
DRIVER_PID=""
STARTED_DAEMON=0
FLOCK_ID=""
RESEARCHER_VMID=""
RELAY_TOKEN=""
CALL_TOKEN=""

mkdir -p "$ARTIFACT_DIR"
CAP_A="$ARTIFACT_DIR/stub-a-capture.jsonl"
CAP_B="$ARTIFACT_DIR/stub-b-capture.jsonl"
CAP_B2="$ARTIFACT_DIR/stub-b2-capture.jsonl"     # fresh capture after stub B restart
PLACEMENTS="$ARTIFACT_DIR/placements.json"
HOSTS_FILE="$ARTIFACT_DIR/hosts.json"
ADAPTER_STDERR="$ARTIFACT_DIR/adapter.stderr"
CREATE_OUT="$ARTIFACT_DIR/create.json"
STOP_FILE="$ARTIFACT_DIR/driver.stop"
MCP_BIN="$ARTIFACT_DIR/anvil-mcp"
# Driver lives inside the module (needs the mcp SDK) but under a gitignored,
# dot-prefixed dir: /artifacts/* is gitignored and `go build ./...` skips dot
# dirs, so it neither dirties git status nor joins any package build.
DRIVER_DIR="$repo_root/artifacts/.failover-e2e-driver"

step() { printf '\n━━━ %s ━━━\n' "$*"; }
ok() { printf '  ✓ %s\n' "$*"; }
fail() {
  printf '  ✗ %s\n' "$*"
  PASS=false
  FAIL_REASONS+=("$1")
}

require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    fail "preflight_failed: missing $cmd"
    return 1
  fi
}

kill_pid() {
  local pid="$1"
  [ -n "$pid" ] || return 0
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

cleanup() {
  local status=$?
  trap - EXIT

  step "Cleanup"
  # Signal the driver to close its MCP session (which stops the adapter and its
  # reconcile loop), then make sure it is gone.
  : >"$STOP_FILE" 2>/dev/null || true
  if [ -n "$DRIVER_PID" ]; then
    for _ in $(seq 1 20); do
      kill -0 "$DRIVER_PID" 2>/dev/null || break
      sleep 0.5
    done
    kill_pid "$DRIVER_PID"
    ok "Stopped driver/adapter"
  fi

  if [ -n "$RESEARCHER_VMID" ]; then
    local dc
    dc="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$RESEARCHER_VMID" 2>/dev/null || true)"
    ok "Deleted researcher VM $RESEARCHER_VMID (HTTP $dc)"
  fi
  if [ -n "$FLOCK_ID" ]; then
    curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null -X DELETE "$API/flocks/$FLOCK_ID" 2>/dev/null || true
    rm -rf "$repo_root/flocks/$FLOCK_ID" 2>/dev/null || true
  fi

  kill_pid "$STUB_A_PID"; [ -n "$STUB_A_PID" ] && ok "Stopped stub A"
  kill_pid "$STUB_B_PID"; [ -n "$STUB_B_PID" ] && ok "Stopped stub B"
  if [ "$STARTED_DAEMON" = "1" ]; then
    kill_pid "$DAEMON_PID"; ok "Stopped member daemon"
  fi

  # Destroy only what this script created inside the worktree.
  rm -rf "$DRIVER_DIR" 2>/dev/null || true

  printf '  Artifact directory: %s\n' "$ARTIFACT_DIR"
  if [ "$PASS" != "true" ] || [ "$status" -ne 0 ]; then
    if [ "${#FAIL_REASONS[@]}" -gt 0 ]; then
      printf '  Failure reasons:\n'
      printf '  - %s\n' "${FAIL_REASONS[@]}"
    fi
    exit 1
  fi
  exit 0
}

# ── stub home HTTP recorder (extends the wall/gtcall recorder) ───────────────
# Records every request (method/path/lowercased headers/body) to a JSONL capture
# file, then answers by path so the adapter's routed-flock create + reconcile +
# the member daemon's relay hop all succeed:
#   GET  /vms                    -> []                              (reachability probe)
#   GET  (other)                 -> {"status":"ok"}                 (readiness)
#   POST /vms                    -> a fixed spawn response          (member spawn)
#   POST /flocks/{id}/post       -> a canned wall message           (relay chain 2xx)
#   POST (other, incl. /distributed,/relay,/call) -> {}             (any 2xx)
#   DELETE /flocks/{id}          -> {"status":"deleted"}
# Bound to loopback only.
write_stub() {
  cat >"$ARTIFACT_DIR/home_stub.py" <<'PYEOF'
import http.server
import json
import sys

capture_path = sys.argv[1]
port = int(sys.argv[2])


class Handler(http.server.BaseHTTPRequestHandler):
    def _record(self):
        length = int(self.headers.get("Content-Length", 0) or 0)
        body = self.rfile.read(length).decode("utf-8", "replace") if length else ""
        record = {
            "method": self.command,
            "path": self.path,
            "headers": {k.lower(): v for k, v in self.headers.items()},
            "body": body,
        }
        with open(capture_path, "a") as fh:
            fh.write(json.dumps(record) + "\n")

    def _send(self, payload: bytes, code: int = 200):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        # ListVMs (reachability probe) must decode into a JSON array.
        if self.path == "/vms" or self.path.startswith("/vms?"):
            self._send(b"[]")
        else:
            self._send(b'{"status":"ok"}')

    def do_POST(self):
        self._record()
        if self.path == "/vms":
            payload = json.dumps({
                "vm_id": "stub-vm-%d" % port,
                "guest_ip": "10.9.%d.2" % (port % 250),
                "agent_url": "http://10.9.%d.2:8080" % (port % 250),
            }).encode("utf-8")
            self._send(payload, 201)
        elif self.path.endswith("/post"):
            self._send(b'{"seq":1,"agent_id":"","body":"","author":"","timestamp":""}')
        else:
            self._send(b"{}", 201)

    def do_DELETE(self):
        self._record()
        self._send(b'{"status":"deleted"}')

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PYEOF
}

start_stub() {
  # $1 port, $2 capture file, $3 label -> sets STUB_<label>_PID via nameref
  local port="$1" cap="$2" name="$3"
  local -n _pid="$4"
  python3 "$ARTIFACT_DIR/home_stub.py" "$cap" "$port" >"$ARTIFACT_DIR/${name}.log" 2>&1 &
  _pid=$!
  for _ in $(seq 1 60); do
    if ! kill -0 "$_pid" 2>/dev/null; then
      fail "stub_unavailable: $name exited early (see ${name}.log)"
      return 1
    fi
    if curl -sS -o /dev/null "http://127.0.0.1:${port}/" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  fail "stub_unavailable: $name not ready on port $port"
  return 1
}

# ── in-guest gtwall workload writer (parameterized message) ─────────────────
write_gtwall_workload() {
  local msg="$1" out="$2"
  cat >"$out" <<WLEOF
#!/usr/bin/env bash
set -euo pipefail
gtwall "$msg"
echo "ANVIL_FO_GTWALL_DONE"
WLEOF
}

# run_gtwall <message> — upload + run the workload in the researcher VM via the
# member daemon (same in-guest exec path as the wall e2e). Returns 0 iff the
# workload exits 0 with the done marker. Does NOT call fail() (callers retry).
run_gtwall() {
  local msg="$1"
  local wl="$ARTIFACT_DIR/gtwall-post.sh"
  write_gtwall_workload "$msg" "$wl"
  local up_code
  up_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null -w "%{http_code}" \
    -X PUT "$API/vms/$RESEARCHER_VMID/workspace?path=workloads/gtwall-post.sh&overwrite=true" \
    --data-binary @"$wl" 2>/dev/null || true)"
  [ "$up_code" = "200" ] || return 1
  local run_payload
  run_payload="$(jq -n --arg s "workloads/gtwall-post.sh" --argjson t "$TASK_TIMEOUT" '{script:$s, timeout_seconds:$t}')"
  local run_json="$ARTIFACT_DIR/gtwall-run.json"
  local run_code
  run_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" --max-time "$((TASK_TIMEOUT + 30))" \
    -o "$run_json" -w "%{http_code}" \
    -X POST "$API/vms/$RESEARCHER_VMID/workloads/run" \
    -H "Content-Type: application/json" -d "$run_payload" 2>/dev/null || true)"
  [ "$run_code" = "200" ] || return 1
  local gt_exit gt_timeout gt_stdout
  gt_exit="$(jq -r '.exit_code // "missing"' "$run_json" 2>/dev/null || true)"
  gt_timeout="$(jq -r '.timed_out // false' "$run_json" 2>/dev/null || true)"
  gt_stdout="$(jq -r '.stdout // ""' "$run_json" 2>/dev/null || true)"
  [ "$gt_timeout" = "true" ] && return 1
  [ "$gt_exit" = "0" ] || return 1
  printf '%s' "$gt_stdout" | grep -q "ANVIL_FO_GTWALL_DONE" || return 1
  return 0
}

# home_host_now — echo the routed flock's current home host from placements.json.
home_host_now() {
  [ -s "$PLACEMENTS" ] || { printf ''; return; }
  jq -r --arg f "$FLOCK_ID" '.routed_flocks[$f].home_host // ""' "$PLACEMENTS" 2>/dev/null || printf ''
}

# poll_home <expected> <timeout_s> — wait until home_host == expected.
poll_home() {
  local want="$1" timeout="$2" waited=0
  while [ "$waited" -lt "$timeout" ]; do
    [ "$(home_host_now)" = "$want" ] && return 0
    sleep 1
    waited=$((waited + 1))
  done
  return 1
}

# capture_distributed_relay_token <capfile> — echo relay_token of the LAST
# POST /flocks/{id}/distributed recorded in capfile (empty if none).
capture_distributed_relay_token() {
  local cap="$1" line
  line="$(grep -F "\"path\": \"/flocks/$FLOCK_ID/distributed\"" "$cap" 2>/dev/null | tail -1 || true)"
  [ -n "$line" ] || { printf ''; return; }
  printf '%s' "$line" | jq -r '.body | fromjson | .relay_token // ""' 2>/dev/null || printf ''
}

capture_distributed_roster_len() {
  local cap="$1" line
  line="$(grep -F "\"path\": \"/flocks/$FLOCK_ID/distributed\"" "$cap" 2>/dev/null | tail -1 || true)"
  [ -n "$line" ] || { printf '0'; return; }
  printf '%s' "$line" | jq -r '.body | fromjson | (.roster | length)' 2>/dev/null || printf '0'
}

# ── Preflight ────────────────────────────────────────────────────────────────
step "Preflight"
require_cmd curl || exit 1
require_cmd jq || exit 1
require_cmd python3 || exit 1
require_cmd go || exit 1
if [ "${EUID:-$(id -u)}" -ne 0 ]; then
  fail "preflight_failed: root required (real VM boot)"
  exit 1
fi
if [ ! -e /dev/kvm ]; then
  fail "preflight_failed: /dev/kvm missing"
  exit 1
fi
if [ ! -f configs/goose.yaml ] || [ ! -f configs/goose-secrets.yaml ]; then
  fail "preflight_failed: configs/goose.yaml + goose-secrets.yaml required (VM create 500s silently without them)"
  exit 1
fi
trap cleanup EXIT

if [ ! -x ./anvil-daemon ]; then
  go build -o anvil-daemon ./cmd/goose-daemon
fi
go build -o "$MCP_BIN" ./cmd/anvil-mcp
ok "Preflight complete (daemon + anvil-mcp built)"

# ── Stubs ────────────────────────────────────────────────────────────────────
step "Start stub A ($STUB_A_ADDR, home) and stub B ($STUB_B_ADDR, candidate)"
write_stub
start_stub "$STUB_A_PORT" "$CAP_A" "stub-a" STUB_A_PID || exit 1
start_stub "$STUB_B_PORT" "$CAP_B" "stub-b" STUB_B_PID || exit 1
ok "Stubs ready (A pid $STUB_A_PID, B pid $STUB_B_PID)"

# ── Member daemon (auth-on) ──────────────────────────────────────────────────
step "Start member daemon (auth-on, bind $DAEMON_BIND)"
EPHEMERA_API_ADDR="$DAEMON_BIND" EPHEMERA_API_TOKENS="operator:$OP_TOKEN" \
  ./anvil-daemon >"$ARTIFACT_DIR/daemon.log" 2>&1 &
DAEMON_PID=$!
STARTED_DAEMON=1
for attempt in $(seq 1 120); do
  if curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null "$API/vms" 2>/dev/null; then
    ok "Member control plane ready (pid $DAEMON_PID)"
    break
  fi
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    fail "daemon_unavailable: member daemon exited early (see daemon.log)"
    exit 1
  fi
  if [ "$attempt" = "120" ]; then
    fail "daemon_unavailable: member API not ready after 120s"
    exit 1
  fi
  sleep 1
done

# ── Adapter config (hosts file + env) ────────────────────────────────────────
step "Write scheduler hosts file (order = election order, capacity 1 each)"
jq -n \
  --arg a "$HOST_A" --arg ae "$STUB_A_ADDR" \
  --arg b "$HOST_B" --arg be "$STUB_B_ADDR" \
  --arg m "$HOST_MEMBER" --arg me "$API" \
  '{hosts:[
     {name:$a, endpoint:$ae, healthy:true, available_vms:1},
     {name:$b, endpoint:$be, healthy:true, available_vms:1},
     {name:$m, endpoint:$me, healthy:true, available_vms:1}
   ]}' >"$HOSTS_FILE"
ok "hosts.json written ([$HOST_A, $HOST_B, $HOST_MEMBER])"

export ANVIL_DAEMON_URL="$API"
export ANVIL_API_TOKEN="$OP_TOKEN"
export ANVIL_MCP_CROSS_HOST_FLOCK_CREATE="members_only"
export ANVIL_MCP_SCHEDULER_STATE="$PLACEMENTS"
export ANVIL_MCP_SCHEDULER_HOSTS_FILE="$HOSTS_FILE"
export ANVIL_MCP_RECONCILE_INTERVAL="$RECONCILE"

# ── MCP driver (keeps the adapter + its reconcile loop alive through all phases)
step "Write + launch MCP driver (creates routed flock, holds the adapter open)"
mkdir -p "$DRIVER_DIR"
cat >"$DRIVER_DIR/main.go" <<'GOEOF'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "driver error:", err)
		os.Exit(1)
	}
}

func run() error {
	command := flag.String("command", "", "anvil-mcp binary path")
	out := flag.String("out", "", "file to write the create output JSON")
	stopFile := flag.String("stop-file", "", "exit once this file exists")
	stderrPath := flag.String("stderr", "", "adapter stderr sink file")
	task := flag.String("task", "home failover e2e", "flock task")
	tenant := flag.String("tenant", "e2e", "tenant_id (routed flock create requires a non-empty tenant)")
	rolesCSV := flag.String("roles", "coordinator,helper,researcher", "comma-separated roles")
	connectTimeout := flag.Duration("connect-timeout", 60*time.Second, "connect timeout")
	createTimeout := flag.Duration("create-timeout", 8*time.Minute, "create-tool timeout (real VM boot)")
	maxLife := flag.Duration("max-life", 30*time.Minute, "safety cap so a dropped parent cannot leak the adapter")
	flag.Parse()

	if *command == "" || *out == "" || *stopFile == "" {
		return fmt.Errorf("-command, -out, -stop-file are required")
	}

	var stderrFile *os.File
	if *stderrPath != "" {
		f, err := os.Create(*stderrPath)
		if err != nil {
			return fmt.Errorf("create stderr sink: %w", err)
		}
		defer f.Close()
		stderrFile = f
	}

	// The adapter must keep running (reconcile/failover loop) until the harness
	// touches the stop-file, so tie its lifetime to this long-lived context.
	cmdCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, *command)
	if stderrFile != nil {
		cmd.Stderr = stderrFile // CommandTransport only pipes stdin/stdout.
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "anvil-failover-driver", Version: "v0.1.0"}, nil)
	connectCtx, connectCancel := context.WithTimeout(context.Background(), *connectTimeout)
	defer connectCancel()
	session, err := client.Connect(connectCtx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connect anvil-mcp: %w", err)
	}
	defer session.Close()

	roles := []string{}
	for _, r := range strings.Split(*rolesCSV, ",") {
		if r = strings.TrimSpace(r); r != "" {
			roles = append(roles, r)
		}
	}

	createCtx, createCancel := context.WithTimeout(context.Background(), *createTimeout)
	defer createCancel()
	result, err := session.CallTool(createCtx, &mcp.CallToolParams{
		Name:      "anvil_create_routed_flock_members",
		Arguments: map[string]any{"task": *task, "tenant_id": *tenant, "roles": roles},
	})
	if err != nil {
		return fmt.Errorf("create routed flock: %w", err)
	}
	if result.IsError {
		data, _ := json.Marshal(result)
		return fmt.Errorf("create routed flock tool error: %s", data)
	}
	data, err := json.MarshalIndent(result.StructuredContent, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal create output: %w", err)
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write create output: %w", err)
	}
	if err := os.Rename(tmp, *out); err != nil {
		return fmt.Errorf("rename create output: %w", err)
	}
	fmt.Println("DRIVER_FLOCK_READY")

	deadline := time.Now().Add(*maxLife)
	for {
		if _, err := os.Stat(*stopFile); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("max-life exceeded without stop-file")
		}
		time.Sleep(500 * time.Millisecond)
	}
}
GOEOF

go run "$DRIVER_DIR/main.go" \
  -command "$MCP_BIN" \
  -out "$CREATE_OUT" \
  -stop-file "$STOP_FILE" \
  -stderr "$ADAPTER_STDERR" \
  -task "home failover e2e" \
  -roles "coordinator,helper,researcher" \
  -connect-timeout 60s -create-timeout 8m -max-life 30m \
  >"$ARTIFACT_DIR/driver.log" 2>&1 &
DRIVER_PID=$!

# Wait for the routed flock to be fully created (the create tool blocks on the
# real researcher VM boot), or bail if the driver dies first.
CREATE_READY=false
for _ in $(seq 1 480); do
  if [ -s "$CREATE_OUT" ]; then
    CREATE_READY=true
    break
  fi
  if ! kill -0 "$DRIVER_PID" 2>/dev/null; then
    break
  fi
  sleep 1
done
if [ "$CREATE_READY" != "true" ]; then
  fail "flock_create_failed: driver produced no create output (see driver.log / adapter.stderr / daemon.log)"
  exit 1
fi

FLOCK_ID="$(jq -r '.flock_id // ""' "$CREATE_OUT" 2>/dev/null || true)"
RESEARCHER_VMID="$(jq -r '.agents[] | select(.role=="researcher") | .vm_id' "$CREATE_OUT" 2>/dev/null || true)"
researcher_host="$(jq -r '.agents[] | select(.role=="researcher") | .host' "$CREATE_OUT" 2>/dev/null || true)"
if [ -z "$FLOCK_ID" ] || [ "$FLOCK_ID" = "null" ]; then
  fail "flock_create_failed: no flock_id in create output"
  exit 1
fi
if [ -z "$RESEARCHER_VMID" ] || [ "$RESEARCHER_VMID" = "null" ]; then
  fail "flock_create_failed: no researcher vm_id in create output"
  exit 1
fi
if [ "$researcher_host" != "$HOST_MEMBER" ]; then
  fail "topology_failed: researcher placed on '$researcher_host', want '$HOST_MEMBER' (scheduler order regression)"
  exit 1
fi
ok "Routed flock $FLOCK_ID ready (researcher VM $RESEARCHER_VMID on $HOST_MEMBER)"

# ── Phase 0 — setup assertions ───────────────────────────────────────────────
step "Phase 0 — home is $HOST_A, hub + relay wired, guest gtwall lands on stub A"
if poll_home "$HOST_A" 30; then
  ok "placements.json home_host == $HOST_A"
else
  fail "phase0_failed: home_host is '$(home_host_now)', want $HOST_A"
  exit 1
fi

RT0="$(capture_distributed_relay_token "$CAP_A")"
rl0="$(capture_distributed_roster_len "$CAP_A")"
if [ -n "$RT0" ] && [ "$rl0" = "3" ]; then
  ok "Stub A received hub POST /distributed (roster 3)"
else
  fail "phase0_failed: stub A hub /distributed missing/short (relay_token empty? roster=$rl0)"
fi

get_flock_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null -w "%{http_code}" "$API/flocks/$FLOCK_ID" 2>/dev/null || true)"
if [ "$get_flock_code" = "200" ]; then
  ok "Member daemon holds the relay flock (GET /flocks/$FLOCK_ID = 200)"
else
  fail "phase0_failed: GET /flocks/$FLOCK_ID on member returned $get_flock_code, want 200"
fi

# Read the persisted per-flock secrets as redaction sentinels (never echoed).
RELAY_TOKEN="$(jq -r --arg f "$FLOCK_ID" '.routed_flock_relay_tokens[$f] // ""' "$PLACEMENTS" 2>/dev/null || true)"
CALL_TOKEN="$(jq -r --arg f "$FLOCK_ID" '.routed_flock_call_tokens[$f] // ""' "$PLACEMENTS" 2>/dev/null || true)"
if [ -z "$RELAY_TOKEN" ] || [ -z "$CALL_TOKEN" ]; then
  fail "phase0_failed: could not read persisted relay/call token from placements.json (redaction sentinels unavailable)"
  exit 1
fi
if [ "$RT0" != "$RELAY_TOKEN" ]; then
  fail "phase0_failed: stub A hub relay_token != persisted relay token"
fi

if run_gtwall "PHASE0_WALL"; then
  ok "Guest gtwall ran (exit 0, marker present)"
else
  fail "phase0_failed: guest gtwall did not complete"
fi
p0_post="$(grep -F "\"path\": \"/flocks/$FLOCK_ID/post\"" "$CAP_A" 2>/dev/null | tail -1 || true)"
if [ -n "$p0_post" ]; then
  p0_body="$(printf '%s' "$p0_post" | jq -r '.body | fromjson | .body' 2>/dev/null || true)"
  p0_agent="$(printf '%s' "$p0_post" | jq -r '.body | fromjson | .agent_id' 2>/dev/null || true)"
  p0_auth="$(printf '%s' "$p0_post" | jq -r '.headers.authorization // ""' 2>/dev/null || true)"
  if [ "$p0_body" = "PHASE0_WALL" ] && [ "$p0_agent" = "researcher-1" ]; then
    ok "gtwall post landed on stub A {agent_id:researcher-1, body:PHASE0_WALL}"
  else
    fail "phase0_failed: stub A post body mismatch (agent=$p0_agent body=$p0_body)"
  fi
  if [ "$p0_auth" = "Bearer $RELAY_TOKEN" ]; then
    ok "Relayed post carried the relay token as bearer"
  else
    fail "phase0_failed: stub A post bearer was not the relay token"
  fi
else
  fail "phase0_failed: stub A recorded no POST /flocks/$FLOCK_ID/post"
fi

# ── Phase 1 — kill stub A, re-elect stub B ───────────────────────────────────
step "Phase 1 — kill $HOST_A -> re-elect $HOST_B, retarget member relay, redaction"
kill_pid "$STUB_A_PID"; STUB_A_PID=""
ok "Killed stub A"
if poll_home "$HOST_B" "$FAILOVER_TIMEOUT"; then
  ok "placements.json home_host re-elected to $HOST_B"
else
  fail "phase1_failed: home_host is '$(home_host_now)' after ${FAILOVER_TIMEOUT}s, want $HOST_B"
  exit 1
fi

# Stub B must receive a hub /distributed (roster 3) carrying the SAME relay token.
RT1=""
rl1="0"
for _ in $(seq 1 20); do
  RT1="$(capture_distributed_relay_token "$CAP_B")"
  rl1="$(capture_distributed_roster_len "$CAP_B")"
  [ -n "$RT1" ] && break
  sleep 0.5
done
if [ -n "$RT1" ] && [ "$rl1" = "3" ]; then
  ok "Stub B received hub POST /distributed (roster 3)"
else
  fail "phase1_failed: stub B hub /distributed missing/short (roster=$rl1)"
fi
if [ "$RT1" = "$RELAY_TOKEN" ]; then
  ok "Stub B hub carries the SAME relay token as the original home"
else
  fail "phase1_failed: stub B hub relay_token differs from the original (token rotated on failover)"
fi

# Guest gtwall must now land on stub B (member relay retargeted). Retry to absorb
# the retarget window (the reconcile re-registers the member relay every ~2s).
p1_ok=false
for attempt in $(seq 1 6); do
  if run_gtwall "PHASE1_WALL"; then
    if grep -F "\"path\": \"/flocks/$FLOCK_ID/post\"" "$CAP_B" 2>/dev/null | jq -e -r '.body | fromjson | select(.body=="PHASE1_WALL")' >/dev/null 2>&1; then
      p1_ok=true
      break
    fi
  fi
  sleep 2
done
if [ "$p1_ok" = "true" ]; then
  ok "Guest gtwall now lands on stub B (member relay retargeted to the new home)"
else
  fail "phase1_failed: PHASE1_WALL never reached stub B (member relay not retargeted)"
fi

# Redaction spot check: adapter stderr must carry a failover line but leak no
# relay/call token value and no stub loopback address.
if grep -q "home failover" "$ADAPTER_STDERR" 2>/dev/null; then
  ok "Adapter stderr recorded a home-failover line"
else
  fail "phase1_failed: no home-failover line in adapter stderr"
fi
red_ok=true
if grep -qF "$RELAY_TOKEN" "$ADAPTER_STDERR" 2>/dev/null; then red_ok=false; fi
if grep -qF "$CALL_TOKEN" "$ADAPTER_STDERR" 2>/dev/null; then red_ok=false; fi
if grep -qE "127\.0\.0\.1:(3100|3101)" "$ADAPTER_STDERR" 2>/dev/null; then red_ok=false; fi
if grep "home failover" "$ADAPTER_STDERR" 2>/dev/null | grep -qE '[0-9a-f]{64}'; then red_ok=false; fi
if [ "$red_ok" = "true" ]; then
  ok "Adapter stderr leaks no relay/call token and no stub address"
else
  fail "phase1_failed: adapter stderr leaked a token value or a stub address"
fi

# ── Phase 2 — kill stub B, promote the REAL member daemon to hub ─────────────
step "Phase 2 — kill $HOST_B -> promote $HOST_MEMBER (real daemon relay->hub)"
kill_pid "$STUB_B_PID"; STUB_B_PID=""
ok "Killed stub B"
if poll_home "$HOST_MEMBER" "$FAILOVER_TIMEOUT"; then
  ok "placements.json home_host re-elected to $HOST_MEMBER"
else
  fail "phase2_failed: home_host is '$(home_host_now)' after ${FAILOVER_TIMEOUT}s, want $HOST_MEMBER"
  exit 1
fi

# Wait for the promotion (relay->hub) to land, then assert the canonical wall
# starts EMPTY on the new real home (wall-loss contract).
hist_json="$ARTIFACT_DIR/wall-history.json"
hub_ready=false
for _ in $(seq 1 30); do
  hcode="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o "$hist_json" -w "%{http_code}" "$API/flocks/$FLOCK_ID/wall/history" 2>/dev/null || true)"
  if [ "$hcode" = "200" ]; then
    hub_ready=true
    break
  fi
  sleep 1
done
if [ "$hub_ready" = "true" ]; then
  ok "Real daemon serves the wall locally (GET /wall/history = 200 = promoted to hub)"
else
  fail "phase2_failed: GET /flocks/$FLOCK_ID/wall/history never returned 200 (no relay->hub promotion)"
  exit 1
fi
empty_len="$(jq -r 'length' "$hist_json" 2>/dev/null || echo "err")"
if [ "$empty_len" = "0" ]; then
  ok "Canonical wall starts EMPTY on the new home (phase 0/1 messages lost, as contracted)"
else
  fail "phase2_failed: promoted hub wall is not empty (length=$empty_len) — wall-loss contract violated"
fi

if run_gtwall "PHASE2_WALL"; then
  ok "Guest gtwall ran against the promoted hub"
else
  fail "phase2_failed: guest gtwall did not complete against the promoted hub"
fi
curl -sS "${CURL_AUTH_ARGS[@]}" -o "$hist_json" "$API/flocks/$FLOCK_ID/wall/history" 2>/dev/null || true
after_len="$(jq -r 'length' "$hist_json" 2>/dev/null || echo "err")"
only_bodies="$(jq -r '[.[].body] | unique | join(",")' "$hist_json" 2>/dev/null || true)"
after_agent="$(jq -r '.[0].agent_id // ""' "$hist_json" 2>/dev/null || true)"
if [ "$after_len" = "1" ] && [ "$only_bodies" = "PHASE2_WALL" ] && [ "$after_agent" = "researcher-1" ]; then
  ok "Wall now holds ONLY the phase-2 message (researcher-1: PHASE2_WALL)"
else
  fail "phase2_failed: wall history unexpected (len=$after_len bodies=[$only_bodies] agent=$after_agent)"
fi

# Redaction spot check on the real daemon's stderr.
dred_ok=true
if grep -qF "$RELAY_TOKEN" "$ARTIFACT_DIR/daemon.log" 2>/dev/null; then dred_ok=false; fi
if grep -qF "$CALL_TOKEN" "$ARTIFACT_DIR/daemon.log" 2>/dev/null; then dred_ok=false; fi
if grep -qE "127\.0\.0\.1:(3100|3101)" "$ARTIFACT_DIR/daemon.log" 2>/dev/null; then dred_ok=false; fi
if [ "$dred_ok" = "true" ]; then
  ok "Daemon stderr leaks no relay/call token and no stub address"
else
  fail "phase2_failed: daemon stderr leaked a token value or a stub address"
fi

# ── Phase 3 — revive stub B, it heals back into a relay ──────────────────────
step "Phase 3 — restart $HOST_B -> receives a relay re-registration"
start_stub "$STUB_B_PORT" "$CAP_B2" "stub-b2" STUB_B_PID || exit 1
ok "Stub B restarted (fresh capture)"
p3_ok=false
for _ in $(seq 1 30); do
  if grep -qF "\"path\": \"/flocks/$FLOCK_ID/relay\"" "$CAP_B2" 2>/dev/null; then
    p3_ok=true
    break
  fi
  sleep 1
done
if [ "$p3_ok" = "true" ]; then
  ok "Revived stub B received a relay RE-registration (heals into a relay, no home fail-back)"
else
  fail "phase3_failed: restarted stub B got no POST /flocks/$FLOCK_ID/relay within 30s"
fi
# NOTE: the mirror direction — a revived old REAL HOME being demoted hub->relay —
# needs a second real daemon to hold kind state, so it lives in the manual
# two-daemon check (2026-07-08-cross-host-manual-verification.md §6), not here.

# ── Result ───────────────────────────────────────────────────────────────────
step "Result"
if [ "$PASS" = "true" ]; then
  echo "All failover e2e steps passed ✓"
  exit 0
fi
printf '  Failure reasons:\n'
printf '  - %s\n' "${FAIL_REASONS[@]}"
exit 1
