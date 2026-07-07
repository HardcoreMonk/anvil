#!/usr/bin/env bash
set -Eeuo pipefail

# anvil-cross-host-gtcall-e2e.sh — KVM e2e for cross-host gtcall (routed-flock
# agent-to-agent calls).
#
# Proves that a REAL flock member VM's `gtcall` dispatch traverses the full
# member-side chain:
#
#     guest gtcall → in-VM goose-agent's local curl to the control plane
#                  → MEMBER anvil-daemon /flocks/{id}/call  (relay flock)
#                  → relay hop (call_token) → HOME /flocks/{id}/call
#                  → HOME's reply flows back to guest stdout
#
# The HOME daemon is STUBBED here (a tiny python3 HTTP recorder on
# 127.0.0.1:3100) because running two real anvil-daemons on one host collides
# on the guest bridge subnet / gateway IP (both would want 10.0.1.1). The
# value of this single-host check is the REAL guest → in-VM agent →
# member-daemon → relay hop with a real VM; the home is only asked to record
# what crossed the wire and answer with a canned reply so we can assert both
# the relay payload/headers AND the round-trip back to the guest's stdout.
#
# Unlike the Town Wall e2e (anvil-cross-host-wall-e2e.sh), the member daemon
# here is started AUTH-ON (EPHEMERA_API_TOKENS="operator:$OP_TOKEN"): the
# guest's relay-token admission path (a relay_token bearer opening ONLY that
# flock's wall+call sub-paths, per authMiddleware's relayGuestPathFlockID) is
# only exercised for real when cp.clients is non-empty — an auth-disabled
# daemon short-circuits authMiddleware entirely and would let a defect in that
# admission path go unnoticed (the wall e2e ran auth-off and missed exactly
# this class of bug once). The host controller's own curl calls use the
# operator token; the guest continues to use its injected
# `.ephemera-cp-token` (= the relay token) exactly as gtwall does.
#
# ── NOT covered here (deliberate, out of single-host CI scope) ──────────────
# Full two-daemon cross-host integration (a real HOME anvil-daemon owning the
# canonical roster on a second host) is a MANUAL multi-host check. One host
# cannot run both daemons without bridge/IP collisions, so that path is
# verified by hand across two machines and is intentionally excluded from this
# single-host gate.
#
# Non-LLM: the call carries no model call on the member/relay/home hops (the
# stub home never invokes goose), so this passes with no provider key.

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  cat >&2 <<'USAGE'
Usage: sudo -n bash scripts/anvil-cross-host-gtcall-e2e.sh

Environment:
  ANVIL_GTCALL_API=http://127.0.0.1:3000       host-side control-plane URL (curl target)
  ANVIL_GTCALL_OP_TOKEN=op-e2e                  operator bearer (daemon started auth-on with it)
  ANVIL_GTCALL_REUSE_DAEMON=0                   1 = talk to an already-running daemon
  ANVIL_GTCALL_DAEMON_BIND=0.0.0.0:3000         EPHEMERA_API_ADDR when we start the daemon
  ANVIL_GTCALL_HOME_PORT=3100                   stub-home listen port (loopback)
  ANVIL_GTCALL_FLOCK_ID=gtcall-e2e
  ANVIL_GTCALL_AGENT_ID=member-1                the member VM's OWN flock identity
  ANVIL_GTCALL_TARGET_AGENT_ID=remote-researcher the peer agent gtcall dispatches to (home-resolved)
  ANVIL_GTCALL_PROMPT="ping from member"
  ANVIL_GTCALL_REPLY=CROSSHOST_REPLY_OK         canned reply the stub home answers with
  ANVIL_GTCALL_RELAY_TOKEN=rt-e2e               guest capability token (opens wall+call)
  ANVIL_GTCALL_CALL_TOKEN=ct-e2e                daemon-to-daemon call-hop token (opens call ONLY)
  ANVIL_GTCALL_TASK_TIMEOUT=120
  ANVIL_GTCALL_ARTIFACT_DIR=/tmp/anvil-cross-host-gtcall-e2e-<timestamp>

NOTE: the daemon must bind an address the guest can reach on the bridge subnet
(the in-VM agent's gtcall forwards to the gateway IP http://10.0.1.1:3000), so
we bind 0.0.0.0:3000 even though the host talks to it over 127.0.0.1:3000.
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

API="${ANVIL_GTCALL_API:-http://127.0.0.1:3000}"
OP_TOKEN="${ANVIL_GTCALL_OP_TOKEN:-op-e2e}"
REUSE_DAEMON="${ANVIL_GTCALL_REUSE_DAEMON:-0}"
DAEMON_BIND="${ANVIL_GTCALL_DAEMON_BIND:-0.0.0.0:3000}"
HOME_PORT="${ANVIL_GTCALL_HOME_PORT:-3100}"
HOME_ADDR="http://127.0.0.1:${HOME_PORT}"
FLOCK_ID="${ANVIL_GTCALL_FLOCK_ID:-gtcall-e2e}"
AGENT_ID="${ANVIL_GTCALL_AGENT_ID:-member-1}"
TARGET_AGENT_ID="${ANVIL_GTCALL_TARGET_AGENT_ID:-remote-researcher}"
PROMPT_MSG="${ANVIL_GTCALL_PROMPT:-ping from member}"
REPLY_MSG="${ANVIL_GTCALL_REPLY:-CROSSHOST_REPLY_OK}"
RELAY_TOKEN="${ANVIL_GTCALL_RELAY_TOKEN:-rt-e2e}"
CALL_TOKEN="${ANVIL_GTCALL_CALL_TOKEN:-ct-e2e}"
TASK_TIMEOUT="${ANVIL_GTCALL_TASK_TIMEOUT:-120}"
ARTIFACT_DIR="${ANVIL_GTCALL_ARTIFACT_DIR:-/tmp/anvil-cross-host-gtcall-e2e-$(date +%Y%m%d-%H%M%S)}"

# Auth is always on for this e2e (see header comment) — the operator token
# authenticates every host-side controller curl (VM/flock admin routes); the
# guest never sees it (it uses the relay token via .ephemera-cp-token instead).
CURL_AUTH_ARGS=(-H "Authorization: Bearer $OP_TOKEN")

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
# JSONL capture file, then answers 200 with {"output": "<reply>"} so the relay
# chain (member daemon → home → back to the in-VM agent → gtcall's curl -sf)
# succeeds and the reply text round-trips to the guest's stdout. Header keys
# are lowercased so the Authorization assertion is case-robust. GET on any
# path answers 200 so we can poll for readiness. Bound to loopback only.
write_home_stub() {
  cat >"$ARTIFACT_DIR/home_stub.py" <<'PYEOF'
import http.server
import json
import sys

capture_path = sys.argv[1]
port = int(sys.argv[2])
reply = sys.argv[3]


class Handler(http.server.BaseHTTPRequestHandler):
    def _record_and_reply(self):
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
        payload = json.dumps({"output": reply}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        self._record_and_reply()

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

# ── in-guest gtcall workload ────────────────────────────────────────────────
# Runs the REAL gtcall CLI inside the guest (uploaded to the workspace,
# executed via /vms/{id}/workloads/run, the same in-guest exec path
# vm-workload-e2e.sh uses). gtcall dispatches to the in-VM agent's local
# control-plane call endpoint, which forwards to the member daemon's
# /flocks/{id}/call, which relays to the stub home. No LLM, no provider key.
#
# EPHEMERA_TASK_DEPTH is exported before invoking gtcall: goose-agent's real
# /tasks handler injects this into a spawned task's env (main.go's
# runTaskHandler) so a gtcall issued mid-task re-sends the accumulated depth;
# a raw workloads/run exec has no such task context, so we set it ourselves to
# exercise gtcall's depth-forwarding branch exactly like a real nested call.
write_gtcall_workload() {
  cat >"$ARTIFACT_DIR/gtcall-call.sh" <<WLEOF
#!/usr/bin/env bash
set -euo pipefail
export EPHEMERA_TASK_DEPTH=1
OUT="\$(gtcall "$TARGET_AGENT_ID" "$PROMPT_MSG")" || exit 1
if [ "\$OUT" != "$REPLY_MSG" ]; then
  echo "unexpected reply: \$OUT" >&2
  exit 1
fi
echo "GTCALL_ROUNDTRIP_OK"
WLEOF
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
python3 "$ARTIFACT_DIR/home_stub.py" "$CAPTURE_FILE" "$HOME_PORT" "$REPLY_MSG" >"$ARTIFACT_DIR/home.log" 2>&1 &
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

# ── member daemon (auth-on) ─────────────────────────────────────────────────
step "Start member daemon (auth-on)"
if [ "$REUSE_DAEMON" = "1" ]; then
  ok "Using existing daemon at $API"
else
  # Bind 0.0.0.0 (not 127.0.0.1): the in-VM agent forwards gtcall dispatches to
  # the bridge gateway IP (http://10.0.1.1:3000), so a loopback-only bind is
  # unreachable from the guest. The host still talks to it over
  # 127.0.0.1:3000. EPHEMERA_API_TOKENS registers ONE operator client so
  # authMiddleware's full per-request path runs (relay-token/call-token
  # admission included) instead of the auth-disabled short-circuit.
  EPHEMERA_API_ADDR="$DAEMON_BIND" EPHEMERA_API_TOKENS="operator:$OP_TOKEN" ./anvil-daemon >"$ARTIFACT_DIR/daemon.log" 2>&1 &
  DAEMON_PID=$!
  STARTED_DAEMON=1
  ok "Daemon PID $DAEMON_PID (bind $DAEMON_BIND, auth-on)"
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
relay_body="$(jq -n --arg home "$HOME_ADDR" --arg rt "$RELAY_TOKEN" --arg ct "$CALL_TOKEN" \
  '{home_addr:$home, relay_token:$rt, call_token:$ct}')"
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

# ── run gtcall inside the guest ─────────────────────────────────────────────
step "Run gtcall in guest (workloads/run)"
write_gtcall_workload
up_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" -o "$ARTIFACT_DIR/upload.json" -w "%{http_code}" \
  -X PUT "$API/vms/$VM_ID/workspace?path=workloads/gtcall-call.sh&overwrite=true" \
  --data-binary @"$ARTIFACT_DIR/gtcall-call.sh" || true)"
if [ "$up_code" = "200" ]; then
  ok "Uploaded gtcall workload"
else
  fail "workload_upload_failed: PUT workspace returned HTTP $up_code"
  exit 1
fi

run_payload="$(jq -n --arg s "workloads/gtcall-call.sh" --argjson t "$TASK_TIMEOUT" '{script:$s, timeout_seconds:$t}')"
run_code="$(curl -sS "${CURL_AUTH_ARGS[@]}" --max-time "$((TASK_TIMEOUT + 30))" \
  -o "$ARTIFACT_DIR/gtcall-run.json" -w "%{http_code}" \
  -X POST "$API/vms/$VM_ID/workloads/run" \
  -H "Content-Type: application/json" -d "$run_payload" || true)"
if [ "$run_code" != "200" ]; then
  fail "gtcall_run_failed: workloads/run returned HTTP $run_code"
  exit 1
fi
gt_exit="$(jq -r '.exit_code // "missing"' "$ARTIFACT_DIR/gtcall-run.json" 2>/dev/null || true)"
gt_timeout="$(jq -r '.timed_out // false' "$ARTIFACT_DIR/gtcall-run.json" 2>/dev/null || true)"
gt_stdout="$(jq -r '.stdout // ""' "$ARTIFACT_DIR/gtcall-run.json" 2>/dev/null || true)"
if [ "$gt_timeout" = "true" ]; then
  fail "gtcall_run_failed: workload timed out"
elif [ "$gt_exit" != "0" ]; then
  fail "gtcall_run_failed: exit_code=$gt_exit (see gtcall-run.json)"
elif printf '%s' "$gt_stdout" | grep -q "GTCALL_ROUNDTRIP_OK"; then
  ok "gtcall round-tripped in guest (exit 0, marker present)"
else
  fail "gtcall_run_failed: missing GTCALL_ROUNDTRIP_OK marker"
fi

# ── assert the stub home received the relayed call ──────────────────────────
step "Assert relayed call reached stub home"
# The chain is synchronous (gtcall's curl -sf blocks on the 200), but retry a
# few times to absorb any capture-file flush latency.
CAP=""
for _ in $(seq 1 20); do
  if [ -s "$CAPTURE_FILE" ]; then
    CAP="$(grep -F "\"path\": \"/flocks/$FLOCK_ID/call\"" "$CAPTURE_FILE" | tail -1 || true)"
    [ -n "$CAP" ] && break
  fi
  sleep 0.5
done

if [ -z "$CAP" ]; then
  fail "call_assert_failed: stub home recorded no POST /flocks/$FLOCK_ID/call"
else
  ok "Stub home received POST /flocks/$FLOCK_ID/call"

  # Body: {"agent_id":"remote-researcher","prompt":"ping from member"} exactly
  # — keys sorted must be exactly these two (no extra fields leaked through
  # the relay hop).
  body_keys="$(printf '%s' "$CAP" | jq -c '(.body | fromjson | keys)' 2>/dev/null || true)"
  got_agent="$(printf '%s' "$CAP" | jq -r '.body | fromjson | .agent_id' 2>/dev/null || true)"
  got_prompt="$(printf '%s' "$CAP" | jq -r '.body | fromjson | .prompt' 2>/dev/null || true)"
  if [ "$body_keys" = '["agent_id","prompt"]' ] && [ "$got_agent" = "$TARGET_AGENT_ID" ] && [ "$got_prompt" = "$PROMPT_MSG" ]; then
    ok "Relayed body is exactly {agent_id:$TARGET_AGENT_ID, prompt:$PROMPT_MSG} (no extra fields)"
  else
    fail "call_assert_failed: body mismatch (keys=$body_keys agent_id=$got_agent prompt=$got_prompt)"
  fi

  # Header: Authorization: Bearer <call_token> — the daemon-to-daemon call-hop
  # secret, NEVER the guest's (broader) relay token (Task 3/4 keep the two
  # secrets' blast radii disjoint).
  got_auth="$(printf '%s' "$CAP" | jq -r '.headers.authorization // ""' 2>/dev/null || true)"
  if [ "$got_auth" = "Bearer $CALL_TOKEN" ]; then
    ok "Authorization header is 'Bearer $CALL_TOKEN' (call token)"
  else
    fail "call_assert_failed: Authorization header was '$got_auth', want 'Bearer $CALL_TOKEN'"
  fi
  if [ "$got_auth" = "Bearer $RELAY_TOKEN" ]; then
    fail "call_assert_failed: Authorization header leaked the relay token ('$RELAY_TOKEN') instead of the call token"
  else
    ok "Relay token is NOT the Authorization header's bearer"
  fi

  # Call-hop + depth headers. This capture is the member->home leg: home is a
  # RESOLVER, not a terminus, so (2026-07-08 C1 fix) this leg is deliberately
  # left UNMARKED — home must remain free to take its own 2nd/final hop to a
  # roster target on another host. Setting the marker here made every home
  # 2nd hop 404 unconditionally (the target daemon's hopped branch resolves
  # only locally). Only the hub->roster-target leg carries the marker.
  got_hop="$(printf '%s' "$CAP" | jq -r '.headers["x-ephemera-call-hop"] // ""' 2>/dev/null || true)"
  if [ -z "$got_hop" ]; then
    ok "X-Ephemera-Call-Hop header is absent (member->home leg is unmarked — home is a resolver, not a terminus)"
  else
    fail "call_assert_failed: X-Ephemera-Call-Hop was '$got_hop', want absent/empty on the member->home leg"
  fi
  got_depth="$(printf '%s' "$CAP" | jq -r '.headers["x-ephemera-task-depth"] // ""' 2>/dev/null || true)"
  if [ -n "$got_depth" ]; then
    ok "X-Ephemera-Task-Depth header present ('$got_depth')"
  else
    fail "call_assert_failed: X-Ephemera-Task-Depth header missing"
  fi

  # Secret hygiene sentinels: neither the agent_token field name, the per-VM
  # agent token value, nor the guest's CP token (relay token) value may cross
  # the relay wire to home — only {agent_id, prompt} plus the depth/hop
  # headers and the call token are allowed on this hop.
  if printf '%s' "$CAP" | grep -qi "agent_token"; then
    fail "call_assert_failed: 'agent_token' appears in the relayed request"
  else
    ok "No 'agent_token' field in the relayed request"
  fi
  if [ -n "$AGENT_TOKEN" ] && printf '%s' "$CAP" | grep -qF "$AGENT_TOKEN"; then
    fail "call_assert_failed: per-VM agent token value leaked into the relayed request"
  else
    ok "Per-VM agent token value absent from the relayed request"
  fi
  if printf '%s' "$CAP" | grep -qF "$RELAY_TOKEN"; then
    fail "call_assert_failed: CP token (relay token) value leaked into the relayed request"
  else
    ok "CP token (relay token) value absent from the relayed request"
  fi
fi

if [ "$PASS" = "true" ]; then
  step "Result"
  ok "Cross-host gtcall relay E2E passed"
  echo "  (Full two-daemon cross-host integration remains a MANUAL multi-host check — out of single-host CI scope.)"
  exit 0
fi

step "Result"
printf '  Failure reasons:\n'
printf '  - %s\n' "${FAIL_REASONS[@]}"
exit 1
