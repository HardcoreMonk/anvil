#!/usr/bin/env bash
set -Eeuo pipefail

# anvil-cross-host-wall-e2e.sh — KVM e2e for the cross-host shared Town Wall.
#
# Proves that a REAL flock member VM's `gtwall` post traverses the full member-
# side chain:
#
#     guest gtwall → in-VM goose-agent /townwall/post
#                  → MEMBER anvil-daemon /flocks/{id}/post  (relay flock)
#                  → relay hop → HOME /flocks/{id}/post
#
# The HOME daemon is STUBBED here (a tiny python3 HTTP recorder on 127.0.0.1:3100)
# because running two real anvil-daemons on one host collides on the guest bridge
# subnet / gateway IP (both would want 10.0.1.1). The value of this single-host
# check is the REAL guest → in-VM agent → member-daemon → relay hop with a real
# VM; the home is only asked to record what crossed the wire so we can assert the
# relay payload/headers.
#
# ── NOT covered here (deliberate, out of single-host CI scope) ──────────────
# Full two-daemon cross-host integration (a real HOME anvil-daemon owning the
# canonical wall on a second host) is a MANUAL multi-host check. One host cannot
# run both daemons without bridge/IP collisions, so that path is verified by hand
# across two machines and is intentionally excluded from this single-host gate.
#
# Non-LLM: the post carries no model call, so this passes with no provider key.

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  cat >&2 <<'USAGE'
Usage: sudo -n bash scripts/anvil-cross-host-wall-e2e.sh

Environment:
  ANVIL_WALL_API=http://127.0.0.1:3000        host-side control-plane URL (curl target)
  ANVIL_WALL_API_TOKEN=                        optional bearer for an existing daemon
  ANVIL_WALL_REUSE_DAEMON=0                    1 = talk to an already-running daemon
  ANVIL_WALL_DAEMON_BIND=0.0.0.0:3000          EPHEMERA_API_ADDR when we start the daemon
  ANVIL_WALL_HOME_PORT=3100                    stub-home listen port (loopback)
  ANVIL_WALL_FLOCK_ID=wall-e2e
  ANVIL_WALL_AGENT_ID=researcher-1
  ANVIL_WALL_RELAY_TOKEN=rt-e2e
  ANVIL_WALL_MSG=ROUNDTRIP_OK
  ANVIL_WALL_TASK_TIMEOUT=120
  ANVIL_WALL_ARTIFACT_DIR=/tmp/anvil-cross-host-wall-e2e-<timestamp>

NOTE: the daemon must bind an address the guest can reach on the bridge subnet
(the in-VM agent forwards to the gateway IP http://10.0.1.1:3000), so we bind
0.0.0.0:3000 even though the host talks to it over 127.0.0.1:3000.
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

API="${ANVIL_WALL_API:-http://127.0.0.1:3000}"
API_TOKEN="${ANVIL_WALL_API_TOKEN:-}"
REUSE_DAEMON="${ANVIL_WALL_REUSE_DAEMON:-0}"
DAEMON_BIND="${ANVIL_WALL_DAEMON_BIND:-0.0.0.0:3000}"
HOME_PORT="${ANVIL_WALL_HOME_PORT:-3100}"
HOME_ADDR="http://127.0.0.1:${HOME_PORT}"
FLOCK_ID="${ANVIL_WALL_FLOCK_ID:-wall-e2e}"
AGENT_ID="${ANVIL_WALL_AGENT_ID:-researcher-1}"
RELAY_TOKEN="${ANVIL_WALL_RELAY_TOKEN:-rt-e2e}"
MSG="${ANVIL_WALL_MSG:-ROUNDTRIP_OK}"
TASK_TIMEOUT="${ANVIL_WALL_TASK_TIMEOUT:-120}"
ARTIFACT_DIR="${ANVIL_WALL_ARTIFACT_DIR:-/tmp/anvil-cross-host-wall-e2e-$(date +%Y%m%d-%H%M%S)}"

CURL_AUTH_ARGS=()
if [ -n "$API_TOKEN" ]; then
  CURL_AUTH_ARGS=(-H "Authorization: Bearer $API_TOKEN")
fi

PASS=true
FAIL_REASONS=()
VM_ID=""
VM_IP=""
AGENT_TOKEN=""
DAEMON_PID=""
STARTED_DAEMON=0
HOME_PID=""
CAPTURE_FILE=""

mkdir -p "$ARTIFACT_DIR"
CAPTURE_FILE="$ARTIFACT_DIR/home-capture.jsonl"

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

cleanup() {
  local status=$?
  trap - EXIT

  step "Cleanup"
  if [ -n "$VM_ID" ]; then
    local delete_code
    delete_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o "$ARTIFACT_DIR/delete-vm.json" -w "%{http_code}" -X DELETE "$API/vms/$VM_ID" || true)"
    if [ "$delete_code" = "200" ]; then
      ok "Deleted VM $VM_ID"
    else
      fail "cleanup_failed: DELETE /vms/$VM_ID returned $delete_code"
    fi
  fi

  # Best-effort: drop the relay flock stub so a reused daemon does not accumulate
  # stale relay registrations across runs.
  curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null -X DELETE "$API/flocks/$FLOCK_ID" 2>/dev/null || true

  if [ -n "$HOME_PID" ]; then
    kill "$HOME_PID" 2>/dev/null || true
    wait "$HOME_PID" 2>/dev/null || true
    ok "Stopped stub home PID $HOME_PID"
  fi

  if [ "$STARTED_DAEMON" = "1" ] && [ -n "$DAEMON_PID" ]; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    ok "Stopped daemon PID $DAEMON_PID"
  fi

  printf '  Artifact directory: %s\n' "$ARTIFACT_DIR"
  if [ "$PASS" != "true" ] || [ "$status" -ne 0 ]; then
    exit 1
  fi
  exit 0
}

# ── stub home HTTP recorder ─────────────────────────────────────────────────
# A few lines of python3: records every POST (method/path/headers/body) to a
# JSONL capture file, then answers 200 with a small JSON body so the relay chain
# (member daemon → home → back to the in-VM agent → gtwall's curl -sf) succeeds.
# Header keys are lowercased so the Authorization assertion is case-robust. GET
# on any path answers 200 so we can poll for readiness. Bound to loopback only.
write_home_stub() {
  cat >"$ARTIFACT_DIR/home_stub.py" <<'PYEOF'
import http.server
import json
import sys

capture_path = sys.argv[1]
port = int(sys.argv[2])


class Handler(http.server.BaseHTTPRequestHandler):
    def _record_and_ok(self):
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
        payload = b'{"seq":1,"agent_id":"","body":"","author":"","timestamp":""}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        self._record_and_ok()

    def do_GET(self):
        payload = b'{"status":"ok"}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PYEOF
}

# ── in-guest gtwall workload ────────────────────────────────────────────────
# Runs the REAL gtwall CLI inside the guest (uploaded to the workspace, executed
# via /vms/{id}/workloads/run, the same in-guest exec path vm-workload-e2e.sh
# uses). gtwall posts to the in-VM agent's /townwall/post, which forwards to the
# member daemon's /flocks/{id}/post. No LLM, no provider key.
write_gtwall_workload() {
  cat >"$ARTIFACT_DIR/gtwall-post.sh" <<PYEOF
#!/usr/bin/env bash
set -euo pipefail
gtwall "$MSG"
echo "ANVIL_WALL_GTWALL_DONE"
PYEOF
}

step "Preflight"
require_cmd curl || exit 1
require_cmd jq || exit 1
require_cmd python3 || exit 1
trap cleanup EXIT

if [ "$REUSE_DAEMON" != "1" ]; then
  if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    fail "preflight_failed: root required when starting daemon"
    exit 1
  fi
  if [ ! -e /dev/kvm ]; then
    fail "preflight_failed: /dev/kvm missing"
    exit 1
  fi
  if [ ! -x ./anvil-daemon ]; then
    require_cmd go || exit 1
    go build -o anvil-daemon ./cmd/goose-daemon
  fi
fi
ok "Preflight complete"

# ── stub home ──────────────────────────────────────────────────────────────
step "Start stub home ($HOME_ADDR)"
write_home_stub
python3 "$ARTIFACT_DIR/home_stub.py" "$CAPTURE_FILE" "$HOME_PORT" >"$ARTIFACT_DIR/home.log" 2>&1 &
HOME_PID=$!
HOME_OK=false
for _ in $(seq 1 60); do
  if ! kill -0 "$HOME_PID" 2>/dev/null; then
    fail "home_unavailable: stub home exited early (see home.log)"
    exit 1
  fi
  if curl -sS -o /dev/null "$HOME_ADDR/" 2>/dev/null; then
    HOME_OK=true
    break
  fi
  sleep 0.5
done
if $HOME_OK; then
  ok "Stub home ready PID $HOME_PID"
else
  fail "home_unavailable: not ready"
  exit 1
fi

# ── member daemon ──────────────────────────────────────────────────────────
step "Start member daemon"
if [ "$REUSE_DAEMON" = "1" ]; then
  ok "Using existing daemon at $API"
else
  # Bind 0.0.0.0 (not 127.0.0.1): the in-VM agent forwards Town Wall posts to the
  # bridge gateway IP (http://10.0.1.1:3000), so a loopback-only bind is
  # unreachable from the guest. The host still talks to it over 127.0.0.1:3000.
  EPHEMERA_API_ADDR="$DAEMON_BIND" ./anvil-daemon >"$ARTIFACT_DIR/daemon.log" 2>&1 &
  DAEMON_PID=$!
  STARTED_DAEMON=1
  ok "Daemon PID $DAEMON_PID (bind $DAEMON_BIND)"
fi

for attempt in $(seq 1 120); do
  if curl -sS "${CURL_AUTH_ARGS[@]}" -o /dev/null "$API/vms" 2>/dev/null; then
    ok "Control plane API ready"
    break
  fi
  if [ "$STARTED_DAEMON" = "1" ] && ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    fail "daemon_unavailable: daemon exited early"
    exit 1
  fi
  if [ "$attempt" = "120" ]; then
    fail "daemon_unavailable: API not ready after 120s"
    exit 1
  fi
  sleep 1
done

# ── register the relay flock on the member daemon ──────────────────────────
step "Register relay flock $FLOCK_ID → $HOME_ADDR"
relay_body="$(jq -n --arg home "$HOME_ADDR" --arg tok "$RELAY_TOKEN" '{home_addr:$home, relay_token:$tok}')"
relay_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o "$ARTIFACT_DIR/relay.json" -w "%{http_code}" \
  -X POST "$API/flocks/$FLOCK_ID/relay" \
  -H "Content-Type: application/json" \
  -d "$relay_body" || true)"
if [ "$relay_code" = "201" ]; then
  ok "Relay flock registered (HTTP 201)"
else
  fail "relay_register_failed: POST /flocks/$FLOCK_ID/relay returned HTTP $relay_code"
  exit 1
fi

# ── create the member VM with routed-flock identity ────────────────────────
step "Create member VM (flock=$FLOCK_ID agent=$AGENT_ID)"
vm_req="$(jq -n --arg fid "$FLOCK_ID" --arg aid "$AGENT_ID" --arg cpt "$RELAY_TOKEN" \
  '{flock_id:$fid, agent_id:$aid, control_plane_token:$cpt}')"
vm_resp="$(curl -sS "${CURL_AUTH_ARGS[@]}" -w "\n%{http_code}" \
  -X POST "$API/vms" -H "Content-Type: application/json" -d "$vm_req" || true)"
vm_code="$(printf '%s\n' "$vm_resp" | tail -1)"
vm_body="$(printf '%s\n' "$vm_resp" | sed '$d')"
printf '%s' "$vm_body" >"$ARTIFACT_DIR/vm-create.json"
if [ "$vm_code" != "201" ]; then
  fail "vm_create_failed: POST /vms returned HTTP $vm_code"
  exit 1
fi
VM_ID="$(printf '%s' "$vm_body" | jq -r '.vm_id')"
VM_IP="$(printf '%s' "$vm_body" | jq -r '.guest_ip')"
AGENT_TOKEN="$(printf '%s' "$vm_body" | jq -r '.agent_token // ""')"
if [ -z "$VM_ID" ] || [ "$VM_ID" = "null" ] || [ -z "$VM_IP" ] || [ "$VM_IP" = "null" ]; then
  fail "vm_create_failed: response missing vm_id or guest_ip"
  exit 1
fi
ok "Created member VM $VM_ID at $VM_IP"

# ── run gtwall inside the guest ────────────────────────────────────────────
step "Run gtwall in guest (workloads/run)"
write_gtwall_workload
up_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o "$ARTIFACT_DIR/upload.json" -w "%{http_code}" \
  -X PUT "$API/vms/$VM_ID/workspace?path=workloads/gtwall-post.sh&overwrite=true" \
  --data-binary @"$ARTIFACT_DIR/gtwall-post.sh" || true)"
if [ "$up_code" = "200" ]; then
  ok "Uploaded gtwall workload"
else
  fail "workload_upload_failed: PUT workspace returned HTTP $up_code"
  exit 1
fi

run_payload="$(jq -n --arg s "workloads/gtwall-post.sh" --argjson t "$TASK_TIMEOUT" '{script:$s, timeout_seconds:$t}')"
run_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" --max-time "$((TASK_TIMEOUT + 30))" \
  -o "$ARTIFACT_DIR/gtwall-run.json" -w "%{http_code}" \
  -X POST "$API/vms/$VM_ID/workloads/run" \
  -H "Content-Type: application/json" -d "$run_payload" || true)"
if [ "$run_code" != "200" ]; then
  fail "gtwall_run_failed: workloads/run returned HTTP $run_code"
  exit 1
fi
gt_exit="$(jq -r '.exit_code // "missing"' "$ARTIFACT_DIR/gtwall-run.json" 2>/dev/null || true)"
gt_timeout="$(jq -r '.timed_out // false' "$ARTIFACT_DIR/gtwall-run.json" 2>/dev/null || true)"
gt_stdout="$(jq -r '.stdout // ""' "$ARTIFACT_DIR/gtwall-run.json" 2>/dev/null || true)"
if [ "$gt_timeout" = "true" ]; then
  fail "gtwall_run_failed: workload timed out"
elif [ "$gt_exit" != "0" ]; then
  fail "gtwall_run_failed: exit_code=$gt_exit (see gtwall-run.json)"
elif printf '%s' "$gt_stdout" | grep -q "ANVIL_WALL_GTWALL_DONE"; then
  ok "gtwall ran in guest (exit 0, marker present)"
else
  fail "gtwall_run_failed: missing ANVIL_WALL_GTWALL_DONE marker"
fi

# ── assert the stub home received the relayed post ─────────────────────────
step "Assert relayed post reached stub home"
# The chain is synchronous (gtwall's curl -sf blocks on the 200), but retry a
# few times to absorb any capture-file flush latency.
CAP=""
for _ in $(seq 1 20); do
  if [ -s "$CAPTURE_FILE" ]; then
    CAP="$(grep -F "\"path\": \"/flocks/$FLOCK_ID/post\"" "$CAPTURE_FILE" | tail -1 || true)"
    [ -n "$CAP" ] && break
  fi
  sleep 0.5
done

if [ -z "$CAP" ]; then
  fail "relay_assert_failed: stub home recorded no POST /flocks/$FLOCK_ID/post"
else
  ok "Stub home received POST /flocks/$FLOCK_ID/post"

  # Body: {"agent_id":"researcher-1","body":"ROUNDTRIP_OK"} — parse and compare
  # fields (order/whitespace independent).
  got_agent="$(printf '%s' "$CAP" | jq -r '.body | fromjson | .agent_id' 2>/dev/null || true)"
  got_body="$(printf '%s' "$CAP" | jq -r '.body | fromjson | .body' 2>/dev/null || true)"
  if [ "$got_agent" = "$AGENT_ID" ] && [ "$got_body" = "$MSG" ]; then
    ok "Relayed body is {agent_id:$AGENT_ID, body:$MSG}"
  else
    fail "relay_assert_failed: body mismatch (agent_id=$got_agent body=$got_body)"
  fi

  # Header: Authorization: Bearer <relay_token>.
  got_auth="$(printf '%s' "$CAP" | jq -r '.headers.authorization // ""' 2>/dev/null || true)"
  if [ "$got_auth" = "Bearer $RELAY_TOKEN" ]; then
    ok "Authorization header is 'Bearer $RELAY_TOKEN'"
  else
    fail "relay_assert_failed: Authorization header was '$got_auth', want 'Bearer $RELAY_TOKEN'"
  fi

  # Secret hygiene: the per-VM agent_token must never cross the relay wire —
  # neither the literal field name nor the actual token value.
  if printf '%s' "$CAP" | grep -qi "agent_token"; then
    fail "relay_assert_failed: 'agent_token' appears in the relayed request"
  else
    ok "No 'agent_token' field in the relayed request"
  fi
  if [ -n "$AGENT_TOKEN" ] && printf '%s' "$CAP" | grep -qF "$AGENT_TOKEN"; then
    fail "relay_assert_failed: per-VM agent token value leaked into the relayed request"
  else
    ok "Per-VM agent token value absent from the relayed request"
  fi
fi

if [ "$PASS" = "true" ]; then
  step "Result"
  ok "Cross-host Town Wall relay E2E passed"
  echo "  (Full two-daemon cross-host integration remains a MANUAL multi-host check — out of single-host CI scope.)"
  exit 0
fi

step "Result"
printf '  Failure reasons:\n'
printf '  - %s\n' "${FAIL_REASONS[@]}"
exit 1
