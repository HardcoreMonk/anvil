#!/usr/bin/env bash
set -Eeuo pipefail

# anvil-snapshot-replication-e2e.sh — KVM e2e for automatic cross-host snapshot
# replication (the members_only reconcile sweep).
#
# Proves the adapter's reconcile loop converges every observed snapshot toward
# snapshotReplicaFactor (=2) replicas WITHOUT any operator action: it discovers a
# snapshot's real replica locations, dials the replication target, exports the
# snapshot from the source daemon and imports it into the target, records the new
# location, and republishes the queue_depth / attempts / giving_up metrics — with
# a dial-failure cap + revival reset when the target flaps.
#
# Topology (single physical host — one real daemon, same bridge-collision reason
# as the wall/failover e2es stub the second daemon):
#
#   real anvil-daemon (auth-on, SOURCE of truth) — hosts one real KVM VM and
#     creates two real FULL snapshots on it (source replicas). host name real-host.
#   stub B (python3 recorder, 127.0.0.1:3101) — the replication TARGET. Extends the
#     wall/failover recorder with the snapshot transfer surface:
#       GET  /vms                 -> []                              (reachability probe = ListVMs)
#       GET  /snapshots           -> []                              (target holds nothing; also
#                                                                      what ReplicateSnapshot lists)
#       POST /snapshots/import    -> record + {"status":"imported"}  (accepts the export bundle;
#                                                                      body is a chunked binary tar,
#                                                                      de-framed + discarded, only its
#                                                                      arrival + byte count recorded)
#       GET  (other)              -> {"status":"ok"}                 (readiness poll)
#     Loopback-bound. Only the IMPORT side is needed — the real daemon is the source.
#   adapter (cmd/anvil-mcp over MCP stdio, cross_host_flock_create_mode=members_only,
#     reconcile_interval=2s) — owns the reconcile/replication control loop. Held open
#     by a tiny MCP driver (below); no tool calls needed, the loop runs at boot.
#
#   Host inventory (static scheduler hosts file, both healthy, capacity 1, every
#   egress mode): real-host (endpoint = the real daemon) + stub-b (endpoint = the
#   stub). SelectRuntimeHost excludes real-host (already a snapshot location), so
#   stub-b is the only eligible replication target.
#
# /metrics: the adapter (anvil-mcp, stdio) does NOT expose HTTP; the scheduler
# metrics live in the placement state file, which anvil-scheduler's /metrics
# endpoint renders via RenderSchedulerMetrics(store.State()). We render the SAME
# function read-only from that state file (a tiny helper below) instead of running
# anvil-scheduler, so exactly ONE process (the adapter) ever WRITES the state file
# — running anvil-scheduler alongside would start a second control loop that Saves
# the whole state and races the adapter's snapshot writes. The rendered text is
# byte-identical to the /metrics endpoint's body.
#
# What each phase asserts, and WHY it fails if the behavior regresses:
#   Phase 0  snap1 exists on real-host, target DOWN. Reconcile discovers
#            snapshot_locations[snap1]==[real-host] (len 1); /metrics queue_depth==1
#            (below replica factor, no transfer fired); attempts{dial_failed,
#            target_unreachable}>=1 (the sweep DIALED the unreachable target and
#            short-circuited — no doomed export). Regresses if discovery, the
#            replica-factor queue accounting, or the dial short-circuit break.
#   Phase 1  bring target UP -> automatic replication. stub-b records a
#            POST /snapshots/import (the export bundle crossed the wire);
#            snapshot_locations[snap1] converges to [real-host, stub-b] (len 2);
#            /metrics attempts{replicated,scheduled}>=1 and queue_depth==0.
#            Redaction: neither /metrics nor adapter stderr leaks the stub loopback
#            address or the operator token.
#   Phase 2  kill target, create snap2 (again under-replicated). /metrics
#            attempts{dial_failed,target_unreachable} INCREASES and
#            giving_up>=1 (dial cap 3 x 2s reached -> target excluded). The dead
#            stub records no new import.
#   Phase 3  restart target -> revival reset. /metrics giving_up returns to 0;
#            snapshot_locations[snap2] converges to len 2; the restarted stub's
#            fresh capture records a NEW import. adapter stderr's "giving up" and
#            "replicated" log lines carry snapshot id + host NAME only (no stub
#            address, no token).
#
# ── NOT covered here (deliberate, out of single-host CI scope) ──────────────
# A second REAL target daemon receiving + re-serving the imported snapshot on a
# second host is a MANUAL two-machine check (one host cannot run two anvil-daemons
# without bridge/IP collisions). The target's IMPORT contract is exercised against
# a recorder stub here; the real daemon-to-daemon transfer is verified by hand.
#
# Non-LLM: snapshots carry no model call, so this passes with no provider key.

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  cat >&2 <<'USAGE'
Usage: sudo -n bash scripts/anvil-snapshot-replication-e2e.sh

Environment (all optional):
  ANVIL_REPL_API=http://127.0.0.1:3000        source daemon control-plane URL (curl target)
  ANVIL_REPL_OP_TOKEN=op-e2e                   operator bearer (source daemon auth-on)
  ANVIL_REPL_DAEMON_BIND=0.0.0.0:3000          EPHEMERA_API_ADDR for the source daemon
  ANVIL_REPL_STUB_PORT=3101                    stub target loopback port
  ANVIL_REPL_RECONCILE=2s                      adapter reconcile_interval
  ANVIL_REPL_TENANT=e2e                        snapshot tenant_id (SelectRuntimeHost needs one)
  ANVIL_REPL_PHASE_TIMEOUT=120                 seconds to wait for each phase transition
  ANVIL_REPL_ARTIFACT_DIR=/tmp/anvil-snapshot-replication-e2e-<timestamp>

The daemon binds 0.0.0.0:3000 for parity with the other cross-host e2es; the host
still talks to it over 127.0.0.1:3000.
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

API="${ANVIL_REPL_API:-http://127.0.0.1:3000}"
OP_TOKEN="${ANVIL_REPL_OP_TOKEN:-op-e2e}"
DAEMON_BIND="${ANVIL_REPL_DAEMON_BIND:-0.0.0.0:3000}"
STUB_PORT="${ANVIL_REPL_STUB_PORT:-3101}"
STUB_ADDR="http://127.0.0.1:${STUB_PORT}"
RECONCILE="${ANVIL_REPL_RECONCILE:-2s}"
TENANT="${ANVIL_REPL_TENANT:-e2e}"
PHASE_TIMEOUT="${ANVIL_REPL_PHASE_TIMEOUT:-120}"
ARTIFACT_DIR="${ANVIL_REPL_ARTIFACT_DIR:-/tmp/anvil-snapshot-replication-e2e-$(date +%Y%m%d-%H%M%S)}"

# Host NAMES (not addresses) — these appear in the adapter's replication log lines,
# so they must never be loopback addresses (the redaction spot check depends on it).
HOST_SOURCE="real-host"
HOST_TARGET="stub-b"

# Operator bearer authenticates every host-side controller curl AND the adapter's
# daemon clients (auth-on source daemon). The stub ignores it.
CURL_AUTH_ARGS=(-H "Authorization: Bearer $OP_TOKEN")

PASS=true
FAIL_REASONS=()
STUB_PID=""
DAEMON_PID=""
DRIVER_PID=""
STARTED_DAEMON=0
VM_ID=""
SNAP1_ID=""
SNAP2_ID=""

mkdir -p "$ARTIFACT_DIR"
CAP="$ARTIFACT_DIR/stub-capture.jsonl"        # phase 0/1/2 stub target capture
CAP2="$ARTIFACT_DIR/stub-capture-2.jsonl"     # fresh capture after target restart (phase 3)
PLACEMENTS="$ARTIFACT_DIR/placements.json"
HOSTS_FILE="$ARTIFACT_DIR/hosts.json"
ADAPTER_STDERR="$ARTIFACT_DIR/adapter.stderr"
METRICS_OUT="$ARTIFACT_DIR/metrics.txt"
STOP_FILE="$ARTIFACT_DIR/driver.stop"
MCP_BIN="$ARTIFACT_DIR/anvil-mcp"
DRIVER_BIN="$ARTIFACT_DIR/repl-driver"
METRICS_BIN="$ARTIFACT_DIR/metrics-render"
# Helper sources live inside the module (the driver needs the mcp SDK, the metrics
# renderer needs internal/anvilmcp) but under gitignored, dot-prefixed dirs:
# /artifacts/* is gitignored and `go build ./...` skips dot dirs, so they neither
# dirty git status nor join any package build.
DRIVER_DIR="$repo_root/artifacts/.snapshot-repl-e2e-driver"
METRICS_DIR="$repo_root/artifacts/.snapshot-repl-e2e-metrics"

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
  # Signal the driver to close its MCP session (which stops the adapter + its
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

  # Delete the real snapshots (frees the on-disk bundles) and the VM before the
  # daemon goes down, so nothing large is orphaned in the worktree's snapshots/.
  local sid dc
  for sid in "$SNAP1_ID" "$SNAP2_ID"; do
    [ -n "$sid" ] || continue
    dc="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null -w "%{http_code}" -X DELETE "$API/snapshots/$sid" 2>/dev/null || true)"
    ok "Deleted snapshot $sid (HTTP $dc)"
  done
  if [ -n "$VM_ID" ]; then
    dc="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$VM_ID" 2>/dev/null || true)"
    ok "Deleted VM $VM_ID (HTTP $dc)"
  fi

  kill_pid "$STUB_PID"; [ -n "$STUB_PID" ] && ok "Stopped stub target"
  if [ "$STARTED_DAEMON" = "1" ]; then
    kill_pid "$DAEMON_PID"; ok "Stopped source daemon"
  fi

  # Destroy the helper build dirs this script created inside the worktree, plus any
  # root-owned snapshot residue the API delete could not reach (daemon already down).
  rm -rf "$DRIVER_DIR" "$METRICS_DIR" 2>/dev/null || true
  for sid in "$SNAP1_ID" "$SNAP2_ID"; do
    [ -n "$sid" ] || continue
    rm -rf "$repo_root/snapshots/$sid" 2>/dev/null || true
  done

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

# ── stub target HTTP recorder (extends the wall/failover recorder) ───────────
# Records every POST (method/path/lowercased headers/body byte count) to a JSONL
# capture file, then answers by path so the adapter's reachability probe + the
# replication export/import land:
#   GET  /vms         -> []                              (ListVMs reachability probe)
#   GET  /snapshots   -> []                              (target holds nothing; also what
#                                                          ReplicateSnapshot's target.ListSnapshots
#                                                          decodes — MUST be a JSON array)
#   GET  (other)      -> {"status":"ok"}                 (readiness poll)
#   POST /snapshots/import -> record + {"status":"imported"} 200
#   POST (other)      -> record + {}
# The import body is a chunked binary snapshot bundle (Go streams an unknown-length
# io.Reader => Transfer-Encoding: chunked). BaseHTTPRequestHandler does not de-frame
# chunked request bodies, so _read_body_len does it by hand (else the socket desyncs
# and the transfer hangs). The bytes are discarded — only arrival + size are asserted.
# Bound to loopback only.
write_stub() {
  cat >"$ARTIFACT_DIR/target_stub.py" <<'PYEOF'
import http.server
import json
import sys

capture_path = sys.argv[1]
port = int(sys.argv[2])


class Handler(http.server.BaseHTTPRequestHandler):
    def _read_body_len(self):
        te = (self.headers.get("Transfer-Encoding", "") or "").lower()
        total = 0
        if "chunked" in te:
            while True:
                line = self.rfile.readline()
                if not line:
                    break
                size_field = line.strip().split(b";", 1)[0]
                try:
                    size = int(size_field, 16)
                except ValueError:
                    break
                if size == 0:
                    # consume optional trailers up to the terminating blank line
                    while True:
                        trailer = self.rfile.readline()
                        if trailer in (b"\r\n", b"\n", b""):
                            break
                    break
                remaining = size
                while remaining > 0:
                    chunk = self.rfile.read(min(remaining, 65536))
                    if not chunk:
                        break
                    remaining -= len(chunk)
                    total += len(chunk)
                self.rfile.readline()  # trailing CRLF after each chunk's data
            return total
        length = int(self.headers.get("Content-Length", 0) or 0)
        remaining = length
        while remaining > 0:
            chunk = self.rfile.read(min(remaining, 65536))
            if not chunk:
                break
            remaining -= len(chunk)
            total += len(chunk)
        return total

    def _record(self, body_bytes):
        record = {
            "method": self.command,
            "path": self.path,
            "headers": {k.lower(): v for k, v in self.headers.items()},
            "body_bytes": body_bytes,
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
        if (
            self.path == "/vms"
            or self.path.startswith("/vms?")
            or self.path == "/snapshots"
            or self.path.startswith("/snapshots?")
        ):
            self._send(b"[]")
        else:
            self._send(b'{"status":"ok"}')

    def do_POST(self):
        n = self._read_body_len()
        self._record(n)
        if self.path == "/snapshots/import":
            self._send(b'{"snapshot_id":"stub-imported","status":"imported"}', 200)
        else:
            self._send(b"{}", 200)

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PYEOF
}

# start_stub <capture-file> -> sets STUB_PID via the global (single stub target).
start_stub() {
  local cap="$1"
  python3 "$ARTIFACT_DIR/target_stub.py" "$cap" "$STUB_PORT" >"$ARTIFACT_DIR/stub.log" 2>&1 &
  STUB_PID=$!
  for _ in $(seq 1 60); do
    if ! kill -0 "$STUB_PID" 2>/dev/null; then
      fail "stub_unavailable: target exited early (see stub.log)"
      return 1
    fi
    if curl -sS -o /dev/null "$STUB_ADDR/" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  fail "stub_unavailable: target not ready on port $STUB_PORT"
  return 1
}

# ── metrics helpers (read-only render of the scheduler /metrics surface) ─────
render_metrics() {
  "$METRICS_BIN" "$PLACEMENTS" >"$METRICS_OUT" 2>"$ARTIFACT_DIR/metrics-render.err" || return 1
  return 0
}
# gauge_value <metric-name> -> the numeric value (exact field match, comments skipped).
gauge_value() {
  awk -v m="$1" '$0 !~ /^#/ && $1==m {v=$2} END{print v}' "$METRICS_OUT" 2>/dev/null || true
}
# attempt_value <outcome> <reason> -> the counter value (0 if the line is absent).
attempt_value() {
  awk -v m="anvil_scheduler_snapshot_replication_attempts_total{outcome=\"$1\",reason=\"$2\"}" \
    '$1==m {v=$2} END{print (v==""?"0":v)}' "$METRICS_OUT" 2>/dev/null || printf '0'
}

# snapshot_locations helpers (read directly from the atomically-written state file).
snap_locs_len() {
  jq -r --arg s "$1" '(.snapshot_locations[$s] // []) | length' "$PLACEMENTS" 2>/dev/null || printf '0'
}
snap_has_host() {
  jq -e --arg s "$1" --arg h "$2" '(.snapshot_locations[$s] // []) | index($h) != null' "$PLACEMENTS" >/dev/null 2>&1
}
stub_import_count() {
  grep -F '"path": "/snapshots/import"' "$1" 2>/dev/null | wc -l | tr -d ' '
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
write_stub
ok "Preflight complete (daemon + anvil-mcp built, stub recorder written)"

# ── Build the read-only metrics renderer (same RenderSchedulerMetrics the
#    anvil-scheduler /metrics endpoint serves; no second writer of the state file).
step "Build read-only /metrics renderer"
mkdir -p "$METRICS_DIR"
cat >"$METRICS_DIR/main.go" <<'GOEOF'
package main

import (
	"fmt"
	"os"

	"ephemera/internal/anvilmcp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: metrics-render <placement-state.json>")
		os.Exit(2)
	}
	store := anvilmcp.NewPlacementStore(os.Args[1])
	if err := store.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "load placement store:", err)
		os.Exit(1)
	}
	fmt.Print(anvilmcp.RenderSchedulerMetrics(store.State()))
}
GOEOF
go build -o "$METRICS_BIN" "$METRICS_DIR/main.go"
ok "Metrics renderer built"

# ── Source daemon (auth-on) ──────────────────────────────────────────────────
step "Start source daemon (auth-on, bind $DAEMON_BIND)"
EPHEMERA_API_ADDR="$DAEMON_BIND" EPHEMERA_API_TOKENS="operator:$OP_TOKEN" \
  ./anvil-daemon >"$ARTIFACT_DIR/daemon.log" 2>&1 &
DAEMON_PID=$!
STARTED_DAEMON=1
for attempt in $(seq 1 120); do
  if curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null "$API/vms" 2>/dev/null; then
    ok "Source control plane ready (pid $DAEMON_PID)"
    break
  fi
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    fail "daemon_unavailable: source daemon exited early (see daemon.log)"
    exit 1
  fi
  if [ "$attempt" = "120" ]; then
    fail "daemon_unavailable: source API not ready after 120s"
    exit 1
  fi
  sleep 1
done

# ── Real VM + first real snapshot (the source replica) ───────────────────────
step "Spawn real VM + create snap1 (tenant=$TENANT, full)"
vm_resp="$(curl -sS "${CURL_AUTH_ARGS[@]}" -w "\n%{http_code}" -X POST "$API/vms" \
  -H "Content-Type: application/json" -d "$(jq -n --arg t "$TENANT" '{tenant_id:$t}')" 2>/dev/null || true)"
vm_code="$(printf '%s\n' "$vm_resp" | tail -1)"
vm_body="$(printf '%s\n' "$vm_resp" | sed '$d')"
printf '%s' "$vm_body" >"$ARTIFACT_DIR/vm-create.json"
if [ "$vm_code" != "201" ]; then
  fail "vm_create_failed: POST /vms returned HTTP $vm_code (see vm-create.json / daemon.log)"
  exit 1
fi
VM_ID="$(printf '%s' "$vm_body" | jq -r '.vm_id // empty' 2>/dev/null || true)"
if [ -z "$VM_ID" ]; then
  fail "vm_create_failed: response missing vm_id"
  exit 1
fi
ok "Real VM $VM_ID booted"

create_snapshot() {
  # $1 = out json path -> echoes snapshot_id on success, empty on failure.
  local out="$1"
  local code
  code="$(curl -sS "${CURL_AUTH_ARGS[@]}" --max-time 600 -o "$out" -w "%{http_code}" \
    -X POST "$API/vms/$VM_ID/snapshot" -H "Content-Type: application/json" \
    -d "$(jq -n --arg t "$TENANT" '{type:"full", tenant_id:$t, stop_after:false}')" 2>/dev/null || true)"
  if [ "$code" != "200" ] && [ "$code" != "201" ]; then
    printf ''
    return 1
  fi
  jq -r '.snapshot_id // empty' "$out" 2>/dev/null || printf ''
}

SNAP1_ID="$(create_snapshot "$ARTIFACT_DIR/snap1.json")"
if [ -z "$SNAP1_ID" ]; then
  fail "snapshot_create_failed: snap1 create returned no snapshot_id (see snap1.json / daemon.log)"
  exit 1
fi
ok "Created snap1 $SNAP1_ID on $HOST_SOURCE"

# ── Scheduler hosts file (static, both healthy, capacity 1, every egress mode) ─
step "Write scheduler hosts file ([$HOST_SOURCE, $HOST_TARGET])"
jq -n \
  --arg s "$HOST_SOURCE" --arg se "$API" \
  --arg t "$HOST_TARGET" --arg te "$STUB_ADDR" \
  '{hosts:[
     {name:$s, endpoint:$se, healthy:true, available_vms:1,
      available_snapshot_bytes:1073741824, egress_policies:["deny_all","profile","allow_all"]},
     {name:$t, endpoint:$te, healthy:true, available_vms:1,
      available_snapshot_bytes:1073741824, egress_policies:["deny_all","profile","allow_all"]}
   ]}' >"$HOSTS_FILE"
ok "hosts.json written"

export ANVIL_DAEMON_URL="$API"
export ANVIL_API_TOKEN="$OP_TOKEN"
export ANVIL_MCP_CROSS_HOST_FLOCK_CREATE="members_only"
export ANVIL_MCP_SCHEDULER_STATE="$PLACEMENTS"
export ANVIL_MCP_SCHEDULER_HOSTS_FILE="$HOSTS_FILE"
export ANVIL_MCP_RECONCILE_INTERVAL="$RECONCILE"

# ── MCP driver (holds the adapter + its reconcile loop open through all phases) ─
step "Build + launch MCP driver (holds the adapter open; reconcile runs at boot)"
mkdir -p "$DRIVER_DIR"
cat >"$DRIVER_DIR/main.go" <<'GOEOF'
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
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
	stopFile := flag.String("stop-file", "", "exit once this file exists")
	stderrPath := flag.String("stderr", "", "adapter stderr sink file")
	connectTimeout := flag.Duration("connect-timeout", 60*time.Second, "connect timeout")
	maxLife := flag.Duration("max-life", 30*time.Minute, "safety cap so a dropped parent cannot leak the adapter")
	flag.Parse()

	if *command == "" || *stopFile == "" {
		return fmt.Errorf("-command and -stop-file are required")
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

	// Tie the adapter's lifetime to this long-lived context: its reconcile loop
	// must keep running until the harness touches the stop-file.
	cmdCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, *command)
	if stderrFile != nil {
		cmd.Stderr = stderrFile // CommandTransport only pipes stdin/stdout.
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "anvil-snapshot-repl-driver", Version: "v0.1.0"}, nil)
	connectCtx, connectCancel := context.WithTimeout(context.Background(), *connectTimeout)
	defer connectCancel()
	session, err := client.Connect(connectCtx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connect anvil-mcp: %w", err)
	}
	defer session.Close()
	fmt.Println("DRIVER_READY")

	// No tool calls: the members_only reconcile loop starts at adapter boot and
	// converges snapshot replicas on its own. We only need to hold the session
	// (and thus the adapter process + its loop) alive.
	deadline := time.Now().Add(*maxLife)
	for {
		if _, err := os.Stat(*stopFile); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("max-life exceeded without stop-file")
		}
		time.Sleep(300 * time.Millisecond)
	}
}
GOEOF
# Build to a real binary and exec directly (NOT `go run`): a direct binary makes
# DRIVER_PID the driver itself — signal-addressable — so a teardown kill closes its
# stdio to anvil-mcp (EOF -> adapter exits) instead of orphaning the adapter.
go build -o "$DRIVER_BIN" "$DRIVER_DIR/main.go"

# NOTE: the stub target is intentionally NOT running yet — phase 0 asserts the
# under-replicated / dial-failure state against an unreachable target.
"$DRIVER_BIN" \
  -command "$MCP_BIN" \
  -stop-file "$STOP_FILE" \
  -stderr "$ADAPTER_STDERR" \
  -connect-timeout 60s -max-life 30m \
  >"$ARTIFACT_DIR/driver.log" 2>&1 &
DRIVER_PID=$!

DRIVER_READY=false
for _ in $(seq 1 120); do
  if grep -q "DRIVER_READY" "$ARTIFACT_DIR/driver.log" 2>/dev/null; then
    DRIVER_READY=true
    break
  fi
  if ! kill -0 "$DRIVER_PID" 2>/dev/null; then
    break
  fi
  sleep 0.5
done
if [ "$DRIVER_READY" != "true" ]; then
  fail "adapter_start_failed: driver never reported DRIVER_READY (see driver.log / adapter.stderr)"
  exit 1
fi
ok "Adapter reconcile loop running (interval $RECONCILE, target still down)"

# ── Phase 0 — snap1 discovered, target DOWN: under-replicated + dial-failure ──
step "Phase 0 — discover snap1 on $HOST_SOURCE, target down (queue_depth 1, dial_failed)"
p0_ok=false
waited=0
while [ "$waited" -lt "$PHASE_TIMEOUT" ]; do
  render_metrics || true
  if [ "$(snap_locs_len "$SNAP1_ID")" = "1" ] && snap_has_host "$SNAP1_ID" "$HOST_SOURCE" \
    && [ "$(gauge_value anvil_scheduler_snapshot_replication_queue_depth)" = "1" ] \
    && [ "$(attempt_value dial_failed target_unreachable)" -ge 1 ] 2>/dev/null; then
    p0_ok=true
    break
  fi
  sleep 1
  waited=$((waited + 1))
done
if [ "$p0_ok" = "true" ]; then
  ok "snapshot_locations[$SNAP1_ID] == [$HOST_SOURCE] (len 1, under-replicated)"
  ok "/metrics queue_depth == 1"
  ok "/metrics attempts{dial_failed,target_unreachable} == $(attempt_value dial_failed target_unreachable) (target dialed, no transfer)"
else
  fail "phase0_failed: expected len1@[$HOST_SOURCE] + queue_depth 1 + dial_failed>=1; got len=$(snap_locs_len "$SNAP1_ID") queue_depth=$(gauge_value anvil_scheduler_snapshot_replication_queue_depth) dial_failed=$(attempt_value dial_failed target_unreachable)"
  exit 1
fi
# No import can have reached a target that is down.
if [ "$(stub_import_count "$CAP")" != "0" ]; then
  fail "phase0_failed: stub target recorded an import while it was supposed to be down"
fi

# ── Phase 1 — target UP: automatic replication ───────────────────────────────
step "Phase 1 — bring $HOST_TARGET up -> automatic replication of snap1"
start_stub "$CAP" || exit 1
ok "Stub target started (pid $STUB_PID)"
p1_ok=false
waited=0
while [ "$waited" -lt "$PHASE_TIMEOUT" ]; do
  render_metrics || true
  if [ "$(stub_import_count "$CAP")" -ge 1 ] \
    && [ "$(snap_locs_len "$SNAP1_ID")" = "2" ] && snap_has_host "$SNAP1_ID" "$HOST_TARGET" \
    && [ "$(attempt_value replicated scheduled)" -ge 1 ] 2>/dev/null \
    && [ "$(gauge_value anvil_scheduler_snapshot_replication_queue_depth)" = "0" ]; then
    p1_ok=true
    break
  fi
  sleep 1
  waited=$((waited + 1))
done
if [ "$p1_ok" = "true" ]; then
  ok "Stub target recorded POST /snapshots/import (export bundle crossed the wire)"
  ok "snapshot_locations[$SNAP1_ID] converged to [$HOST_SOURCE, $HOST_TARGET] (len 2)"
  ok "/metrics attempts{replicated,scheduled} == $(attempt_value replicated scheduled), queue_depth == 0"
else
  fail "phase1_failed: import=$(stub_import_count "$CAP") len=$(snap_locs_len "$SNAP1_ID") replicated=$(attempt_value replicated scheduled) queue_depth=$(gauge_value anvil_scheduler_snapshot_replication_queue_depth)"
  exit 1
fi

# Redaction spot check: neither the rendered /metrics nor the adapter stderr may
# carry the stub loopback address or the operator token.
red_ok=true
if grep -qE "127\.0\.0\.1:($STUB_PORT|3000)" "$METRICS_OUT" 2>/dev/null; then red_ok=false; fi
if grep -qF "$OP_TOKEN" "$METRICS_OUT" 2>/dev/null; then red_ok=false; fi
if grep -qE "127\.0\.0\.1:($STUB_PORT|3000)" "$ADAPTER_STDERR" 2>/dev/null; then red_ok=false; fi
if grep -qF "$OP_TOKEN" "$ADAPTER_STDERR" 2>/dev/null; then red_ok=false; fi
if [ "$red_ok" = "true" ]; then
  ok "Neither /metrics nor adapter stderr leaks the stub address or the operator token"
else
  fail "phase1_failed: /metrics or adapter stderr leaked a stub address or the operator token"
fi

# ── Phase 2 — target DOWN again + new snapshot: dial cap -> giving up ─────────
step "Phase 2 — kill $HOST_TARGET, create snap2 -> dial cap reached (giving_up)"
kill_pid "$STUB_PID"; STUB_PID=""
ok "Killed stub target"
render_metrics || true
dial_before="$(attempt_value dial_failed target_unreachable)"

SNAP2_ID="$(create_snapshot "$ARTIFACT_DIR/snap2.json")"
if [ -z "$SNAP2_ID" ]; then
  fail "snapshot_create_failed: snap2 create returned no snapshot_id (see snap2.json / daemon.log)"
  exit 1
fi
ok "Created snap2 $SNAP2_ID on $HOST_SOURCE (again under-replicated)"

p2_ok=false
waited=0
while [ "$waited" -lt "$PHASE_TIMEOUT" ]; do
  render_metrics || true
  if [ "$(attempt_value dial_failed target_unreachable)" -gt "$dial_before" ] 2>/dev/null \
    && [ "$(gauge_value anvil_scheduler_snapshot_replication_giving_up)" -ge 1 ] 2>/dev/null; then
    p2_ok=true
    break
  fi
  sleep 1
  waited=$((waited + 1))
done
if [ "$p2_ok" = "true" ]; then
  ok "/metrics attempts{dial_failed,target_unreachable} rose $dial_before -> $(attempt_value dial_failed target_unreachable)"
  ok "/metrics giving_up == $(gauge_value anvil_scheduler_snapshot_replication_giving_up) (dial cap reached, target excluded)"
else
  fail "phase2_failed: dial_before=$dial_before dial_now=$(attempt_value dial_failed target_unreachable) giving_up=$(gauge_value anvil_scheduler_snapshot_replication_giving_up)"
  exit 1
fi

# ── Phase 3 — target RETURNS: revival reset + convergence ─────────────────────
step "Phase 3 — restart $HOST_TARGET -> giving_up resets, snap2 converges"
start_stub "$CAP2" || exit 1
ok "Stub target restarted (fresh capture)"
p3_ok=false
waited=0
while [ "$waited" -lt "$PHASE_TIMEOUT" ]; do
  render_metrics || true
  if [ "$(gauge_value anvil_scheduler_snapshot_replication_giving_up)" = "0" ] \
    && [ "$(snap_locs_len "$SNAP2_ID")" = "2" ] && snap_has_host "$SNAP2_ID" "$HOST_TARGET" \
    && [ "$(stub_import_count "$CAP2")" -ge 1 ]; then
    p3_ok=true
    break
  fi
  sleep 1
  waited=$((waited + 1))
done
if [ "$p3_ok" = "true" ]; then
  ok "/metrics giving_up returned to 0 (revival reset)"
  ok "snapshot_locations[$SNAP2_ID] converged to len 2 (includes $HOST_TARGET)"
  ok "Restarted stub target recorded a NEW POST /snapshots/import"
else
  fail "phase3_failed: giving_up=$(gauge_value anvil_scheduler_snapshot_replication_giving_up) len=$(snap_locs_len "$SNAP2_ID") new_import=$(stub_import_count "$CAP2")"
  exit 1
fi

# Adapter stderr must carry a "giving up" line and a "replicated" line, each with a
# snapshot id + host NAME and no stub address / token.
gu_line="$(grep -i "giving up" "$ADAPTER_STDERR" 2>/dev/null | tail -1 || true)"
rep_line="$(grep -i "replicated" "$ADAPTER_STDERR" 2>/dev/null | tail -1 || true)"
if [ -n "$gu_line" ] && printf '%s' "$gu_line" | grep -qF "$HOST_TARGET"; then
  ok "adapter stderr has a 'giving up' line naming host $HOST_TARGET"
else
  fail "phase3_failed: no 'giving up' line naming $HOST_TARGET in adapter stderr"
fi
if [ -n "$rep_line" ] && printf '%s' "$rep_line" | grep -qF "$HOST_TARGET"; then
  ok "adapter stderr has a 'replicated' line naming host $HOST_TARGET"
else
  fail "phase3_failed: no 'replicated' line naming $HOST_TARGET in adapter stderr"
fi
red3_ok=true
if grep -qE "127\.0\.0\.1:($STUB_PORT|3000)" "$ADAPTER_STDERR" 2>/dev/null; then red3_ok=false; fi
if grep -qF "$OP_TOKEN" "$ADAPTER_STDERR" 2>/dev/null; then red3_ok=false; fi
if [ "$red3_ok" = "true" ]; then
  ok "adapter stderr replication logs carry snapshot id + host name only (no address / token)"
else
  fail "phase3_failed: adapter stderr leaked a stub address or the operator token"
fi

# ── Result ───────────────────────────────────────────────────────────────────
step "Result"
if [ "$PASS" = "true" ]; then
  echo "All snapshot replication e2e steps passed ✓"
  exit 0
fi
printf '  Failure reasons:\n'
printf '  - %s\n' "${FAIL_REASONS[@]}"
exit 1
