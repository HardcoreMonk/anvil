#!/bin/bash
# Ephemera simple end-to-end test scenario
# Run with: sudo bash simple_test_scenario.sh
set -euo pipefail

API="http://localhost:3000"
TASK='{"prompt":"Check my current system environment. Tell me the OS version, available disk space in the current directory."}'
LOG=$(mktemp /tmp/ephemera-test-XXXXXX.log)
PASS=true

step()       { echo; echo "━━━ $* ━━━"; }
ok()         { echo "  ✓ $*"; }
fail()       { echo "  ✗ $*"; PASS=false; }
check_http() { local code=$1 expected=$2 label=$3
               [ "$code" = "$expected" ] && ok "$label (HTTP $code)" \
                                         || fail "$label (HTTP $code, expected $expected)"; }

# ── 1. Start daemon ──────────────────────────────────────────────
step "1. Start daemon"
echo "  Working directory: $(pwd)"
echo "  Log file: $LOG"
./ephemera-daemon >>"$LOG" 2>&1 &
DAEMON_PID=$!
echo "  Daemon PID: $DAEMON_PID"

# Wait until the control plane API accepts connections
for i in $(seq 1 30); do
    curl -s -o /dev/null "$API/vms" 2>/dev/null && break
    sleep 1
done
curl -s -o /dev/null "$API/vms" && ok "Control plane API is responding" || { fail "API not responding"; exit 1; }

cleanup() {
    echo; echo "━━━ Cleanup (trap) ━━━"
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── 2. Create one VM ─────────────────────────────────────────────
step "2. Create one VM"
VM1_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms")
VM1_CODE=$(echo "$VM1_RESP" | tail -1)
VM1_BODY=$(echo "$VM1_RESP" | head -1)
check_http "$VM1_CODE" "201" "POST /vms"
VM1_ID=$(echo    "$VM1_BODY" | jq -r '.vm_id')
VM1_AGENT=$(echo "$VM1_BODY" | jq -r '.agent_url')
VM1_IP=$(echo    "$VM1_BODY" | jq -r '.guest_ip')
ok "VM1: $VM1_ID  IP: $VM1_IP  Agent: $VM1_AGENT"

# ── 3. Run a task on VM1 ─────────────────────────────────────────
step "3. Run a task on VM1"
T1=$(curl -s --max-time 90 -X POST "$VM1_AGENT/tasks" \
         -H "Content-Type: application/json" -d "$TASK")
T1_OUT=$(echo "$T1" | jq -r '.output' 2>/dev/null | grep -v '^$' | tail -3 | tr '\n' ' ')
[ -n "$T1_OUT" ] && ok "Response: $(echo "$T1_OUT" | sed 's/  */ /g')" || fail "No response"

# ── 4. Stop goose agent on VM1 ───────────────────────────────────
step "4. Stop goose agent on VM1"
S1=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$VM1_AGENT/stop")
check_http "$S1" "200" "POST /stop"
sleep 5

# ── 5. Delete VM1 ────────────────────────────────────────────────
step "5. Delete VM1"
D1=$(curl -s -w "\n%{http_code}" -X DELETE "$API/vms/$VM1_ID")
check_http "$(echo "$D1" | tail -1)" "200" "DELETE /vms/$VM1_ID"
echo "  $(echo "$D1" | head -1 | jq -c .)"

# ── 6. Create two VMs ────────────────────────────────────────────
step "6. Create two VMs"
VM2_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms")
VM2_CODE=$(echo "$VM2_RESP" | tail -1)
VM2_BODY=$(echo "$VM2_RESP" | head -1)
check_http "$VM2_CODE" "201" "POST /vms (VM2)"
VM2_ID=$(echo    "$VM2_BODY" | jq -r '.vm_id')
VM2_AGENT=$(echo "$VM2_BODY" | jq -r '.agent_url')
ok "VM2: $VM2_ID  Agent: $VM2_AGENT"

VM3_RESP=$(curl -s -w "\n%{http_code}" -X POST "$API/vms")
VM3_CODE=$(echo "$VM3_RESP" | tail -1)
VM3_BODY=$(echo "$VM3_RESP" | head -1)
check_http "$VM3_CODE" "201" "POST /vms (VM3)"
VM3_ID=$(echo    "$VM3_BODY" | jq -r '.vm_id')
VM3_AGENT=$(echo "$VM3_BODY" | jq -r '.agent_url')
ok "VM3: $VM3_ID  Agent: $VM3_AGENT"

step "Verify VM list (should be 2)"
COUNT=$(curl -s "$API/vms" | jq 'length')
[ "$COUNT" = "2" ] && ok "VM count: $COUNT" || fail "VM count: $COUNT (expected 2)"
curl -s "$API/vms" | jq -r '.[] | "  \(.vm_id)  \(.guest_ip)"'

# ── 7. Run tasks on VM2 and VM3 in parallel ──────────────────────
step "7. Run tasks on VM2 and VM3 in parallel"
curl -s --max-time 90 -X POST "$VM2_AGENT/tasks" \
     -H "Content-Type: application/json" -d "$TASK" >/tmp/t2.json &
PID2=$!
curl -s --max-time 90 -X POST "$VM3_AGENT/tasks" \
     -H "Content-Type: application/json" -d "$TASK" >/tmp/t3.json &
PID3=$!
wait $PID2; wait $PID3

T2=$(jq -r '.output' /tmp/t2.json 2>/dev/null | grep -v '^$' | tail -2 | tr '\n' ' ')
T3=$(jq -r '.output' /tmp/t3.json 2>/dev/null | grep -v '^$' | tail -2 | tr '\n' ' ')
[ -n "$T2" ] && ok "VM2 response: $(echo "$T2" | sed 's/  */ /g')" || fail "No response from VM2"
[ -n "$T3" ] && ok "VM3 response: $(echo "$T3" | sed 's/  */ /g')" || fail "No response from VM3"

# ── 8. Stop goose agents on VM2 and VM3 ──────────────────────────
step "8. Stop goose agents on VM2 and VM3"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$VM2_AGENT/stop")" "200" "VM2 /stop"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X POST "$VM3_AGENT/stop")" "200" "VM3 /stop"
sleep 5

# ── 9. Delete both VMs ───────────────────────────────────────────
step "9. Delete both VMs"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$VM2_ID")" "200" "DELETE VM2"
check_http "$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$API/vms/$VM3_ID")" "200" "DELETE VM3"

step "Verify VM list is empty (should be 0)"
FCOUNT=$(curl -s "$API/vms" | jq 'length')
[ "$FCOUNT" = "0" ] && ok "VM count: $FCOUNT" || fail "VM count: $FCOUNT (expected 0)"

# ── 10. Shut down daemon ─────────────────────────────────────────
step "10. Shut down daemon"
kill "$DAEMON_PID" 2>/dev/null; wait "$DAEMON_PID" 2>/dev/null || true
trap - EXIT
ok "Daemon stopped"

# ── Result ───────────────────────────────────────────────────────
echo
if $PASS; then
    echo "══════════════════════════════════"
    echo "  All test steps passed ✓"
    echo "══════════════════════════════════"
else
    echo "══════════════════════════════════"
    echo "  Some steps failed ✗  (log: $LOG)"
    echo "══════════════════════════════════"
    exit 1
fi
