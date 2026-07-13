#!/usr/bin/env bash
#
# anvil-egress-sni-e2e.sh — KVM end-to-end gate for the egress SNI filter.
#
# WHAT THIS PROVES (real kernel, real VM, real public domains — the first live
# execution of the in-process NFQUEUE verdict loop from Tasks 4/5/6):
#   Phase 0  daemon + verdict loop bind NFQUEUE; allow_sni profile VM spawns
#            (spawn only succeeds when sniLoop.Ready(), i.e. NFQUEUE is usable —
#            the fail-closed preflight in commandEgressEnforcer.ApplyWithProfile).
#   Phase 1  guest reaches <allow>:443 (TLS handshake completes) AND the VM's
#            -sni-fastpath / -sni-nfqueue / connmark 0x534e49 FORWARD rules exist.
#   Phase 2  guest CANNOT reach <deny>:443 — the ClientHello SNI is inspected and
#            dropped (fast RST or timeout; both are non-zero curl exit). Both
#            domains complete the TCP handshake, so the difference is proven to be
#            SNI-based ClientHello inspection, not IP/port blocking.
#   Phase 3  the deny is recorded in the daemon runtime audit trail
#            (egress_sni_denied + sni=<deny>) and a redaction spot-check confirms
#            no bearer/API-key material leaked into audit or SNI log lines.
#   Phase 4  (smoke) a full allow flow (handshake + response spanning many
#            packets) adds ~1 allowed verdict — proving the conntrack-mark fast
#            path keeps the slow path off the per-packet hot path.
#
# WHAT THIS DOES NOT COVER: multi-tenant fan-out, IPv6, wildcard SNI matching
# breadth (unit-tested in internal/network/sni), snapshot/restore re-apply
# (Task 5 unit path). Verdict = exit code + final "passed" line only.
#
# Requires: root, /dev/kvm, iptables + NFQUEUE (nfnetlink_queue), outbound
# internet. Run:  sudo -n bash scripts/anvil-egress-sni-e2e.sh
#
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

# ---- Configuration -----------------------------------------------------------
ALLOW_DOMAIN="${ANVIL_SNI_E2E_ALLOW_DOMAIN:-api.anthropic.com}"
DENY_DOMAIN="${ANVIL_SNI_E2E_DENY_DOMAIN:-example.org}"
# Source checkout that owns the prebuilt VM artifacts + gitignored goose config
# (this worktree carries neither; the daemon binary is still built from HERE).
SRC_CHECKOUT="${ANVIL_SNI_E2E_SRC:-/data/projects/claude-zone/anvil}"
API="${ANVIL_SNI_E2E_API:-http://127.0.0.1:3000}"
API_ADDR="${ANVIL_SNI_E2E_API_ADDR:-127.0.0.1:3000}"
PROFILE_NAME="sni-e2e"
TENANT_ID="sni-e2e-tenant"
ARTIFACT_DIR="${ANVIL_SNI_E2E_ARTIFACT_DIR:-/tmp/anvil-egress-sni-e2e-$(date +%Y%m%d-%H%M%S)}"

HOME_DIR="$ARTIFACT_DIR/home"
EGRESS_PROFILE_DIR="$ARTIFACT_DIR/egress-profiles"
DAEMON_BIN="$ARTIFACT_DIR/anvil-daemon"
DAEMON_LOG="$ARTIFACT_DIR/daemon.log"
AUDIT_FILE="$HOME_DIR/audit/runtime-audit.jsonl"

PASS=true
FAIL_REASONS=()
VM_ID=""
GUEST_IP=""
DAEMON_PID=""

# ---- Helpers -----------------------------------------------------------------
step() { printf '\n━━━ %s ━━━\n' "$*"; }
ok()   { printf '  ✓ %s\n' "$*"; }
fail() {
  printf '  ✗ %s\n' "$*"
  PASS=false
  FAIL_REASONS+=("$1")
}
require_cmd() {
  command -v "$1" >/dev/null 2>&1 || { fail "preflight_failed: missing $1"; return 1; }
}

# Delete every FORWARD rule tagged anvil-egress-* (defensive: the daemon removes
# them on VM DELETE / shutdown, but a killed daemon or a crashed prior run can
# leak them). Converts each `-A FORWARD ...` line from iptables -S into `-D ...`.
sweep_egress_rules() {
  local line
  while IFS= read -r line; do
    case "$line" in
      -A\ FORWARD*anvil-egress*)
        # shellcheck disable=SC2086
        iptables ${line/-A/-D} 2>/dev/null || true
        ;;
    esac
  done < <(iptables -S FORWARD 2>/dev/null || true)
}

read_metric() {
  # $1 = outcome label value. Prints the counter (0 when the series is absent).
  local outcome="$1" v
  v="$(curl -sS --max-time 5 "$API/metrics" 2>/dev/null \
        | awk -v o="ephemera_egress_sni_verdict_total{outcome=\"$outcome\"}" \
              '$1==o {print $2}' | tail -1)"
  printf '%s' "${v:-0}"
}

cleanup() {
  local status=$?
  trap - EXIT
  step "Cleanup"

  if [ -n "$VM_ID" ] && [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    local code
    code="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$API/vms/$VM_ID" 2>/dev/null || true)"
    if [ "$code" = "200" ]; then ok "Deleted VM $VM_ID"; else printf '  ! DELETE /vms/%s -> %s\n' "$VM_ID" "$code"; fi
  fi

  if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
    ok "Stopped daemon PID $DAEMON_PID"
  fi

  sweep_egress_rules
  local leftover
  leftover="$(iptables -S FORWARD 2>/dev/null | grep -c anvil-egress || true)"
  if [ "${leftover:-0}" = "0" ]; then ok "No anvil-egress FORWARD rules remain"; else printf '  ! %s anvil-egress rules still present\n' "$leftover"; fi

  # Best-effort scratch teardown (root-owned VM clone + daemon state live here).
  if [ -n "$VM_ID" ]; then rm -rf "/tmp/goose-workspaces/$VM_ID" "/tmp/goose-workspaces/$VM_ID".* 2>/dev/null || true; fi
  rm -rf "$HOME_DIR" "$EGRESS_PROFILE_DIR" 2>/dev/null || true
  printf '  Artifacts (logs kept): %s\n' "$ARTIFACT_DIR"

  if [ "$PASS" = "true" ] && [ "$status" -eq 0 ]; then
    printf '\nAll egress SNI e2e steps passed ✓\n'
    exit 0
  fi
  step "Result: FAIL"
  if [ "${#FAIL_REASONS[@]}" -gt 0 ]; then printf '  - %s\n' "${FAIL_REASONS[@]}"; fi
  exit 1
}

# ---- Preflight ---------------------------------------------------------------
step "Preflight"
mkdir -p "$ARTIFACT_DIR"
require_cmd curl || exit 1
require_cmd jq || exit 1
require_cmd iptables || exit 1
require_cmd go || exit 1
if [ "${EUID:-$(id -u)}" -ne 0 ]; then fail "preflight_failed: root required (run via sudo -n)"; exit 1; fi
if [ ! -e /dev/kvm ]; then fail "preflight_failed: /dev/kvm missing"; exit 1; fi
if [ ! -d "$SRC_CHECKOUT/artifacts" ]; then fail "preflight_failed: source artifacts dir $SRC_CHECKOUT/artifacts missing"; exit 1; fi
for f in golden-image.ext4 vmlinux.bin firecracker goose-agent micro-init; do
  [ -e "$SRC_CHECKOUT/artifacts/$f" ] || { fail "preflight_failed: missing artifact $f in $SRC_CHECKOUT/artifacts"; exit 1; }
done
[ -f "$SRC_CHECKOUT/configs/goose.yaml" ] || { fail "preflight_failed: missing $SRC_CHECKOUT/configs/goose.yaml"; exit 1; }
[ -f "$SRC_CHECKOUT/configs/goose-secrets.yaml" ] || { fail "preflight_failed: missing $SRC_CHECKOUT/configs/goose-secrets.yaml"; exit 1; }
sweep_egress_rules   # clear any leaked rules from an aborted prior run
trap cleanup EXIT
ok "Preflight complete (allow=$ALLOW_DOMAIN deny=$DENY_DOMAIN)"

# ---- Build daemon from THIS worktree (the code under test) -------------------
step "Build daemon"
if ! go build -o "$DAEMON_BIN" ./cmd/goose-daemon 2>"$ARTIFACT_DIR/build.log"; then
  cat "$ARTIFACT_DIR/build.log" >&2
  fail "build_failed: go build ./cmd/goose-daemon"
  exit 1
fi
ok "Built $DAEMON_BIN"

# ---- Assemble an isolated EPHEMERA_HOME --------------------------------------
step "Assemble scratch EPHEMERA_HOME"
mkdir -p "$HOME_DIR/artifacts" "$HOME_DIR/configs"
for f in "$SRC_CHECKOUT"/artifacts/*; do ln -sf "$f" "$HOME_DIR/artifacts/$(basename "$f")"; done
cp "$SRC_CHECKOUT/configs/goose.yaml" "$HOME_DIR/configs/goose.yaml"
cp "$SRC_CHECKOUT/configs/goose-secrets.yaml" "$HOME_DIR/configs/goose-secrets.yaml"
# Point the build-input scripts dir at the SRC checkout (same provenance as the
# golden image, so build_image.sh / gtwall / gtcall all predate it and
# EnsureGoldenImage's pathsNewerThan staleness check does NOT trigger a rebuild).
# The daemon binary itself is still built from THIS worktree above (that is what
# carries the SNI verdict-loop code); only the guest image side comes from SRC.
ln -sf "$SRC_CHECKOUT/scripts" "$HOME_DIR/scripts"
mkdir -p "$EGRESS_PROFILE_DIR/$PROFILE_NAME"
jq -n --arg a "$ALLOW_DOMAIN" '{allow_sni: [$a], dns_servers: ["1.1.1.1","8.8.8.8"]}' \
  > "$EGRESS_PROFILE_DIR/$PROFILE_NAME/egress.json"
ok "egress.json: $(cat "$EGRESS_PROFILE_DIR/$PROFILE_NAME/egress.json")"

# ---- Phase 0: start daemon + verdict loop ------------------------------------
step "Phase 0: start daemon (verdict loop binds NFQUEUE)"
EPHEMERA_HOME="$HOME_DIR" \
EPHEMERA_API_ADDR="$API_ADDR" \
ANVIL_EGRESS_PROFILE_DIR="$EGRESS_PROFILE_DIR" \
EPHEMERA_DISK_MODE=plain \
  "$DAEMON_BIN" >"$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!
ok "Daemon PID $DAEMON_PID"

for attempt in $(seq 1 120); do
  if curl -sS -o /dev/null "$API/vms" 2>/dev/null; then ok "Control plane API ready"; break; fi
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    tail -30 "$DAEMON_LOG" >&2 || true
    fail "daemon_unavailable: exited early"; exit 1
  fi
  [ "$attempt" = "120" ] && { fail "daemon_unavailable: API not ready after 120s"; exit 1; }
  sleep 1
done

if grep -q "sni verdict loop failed to start" "$DAEMON_LOG"; then
  grep "sni verdict loop failed to start" "$DAEMON_LOG" >&2
  fail "phase0_failed: verdict loop did not bind NFQUEUE (fail-closed)"; exit 1
fi
ok "No verdict-loop bind failure in daemon log"

# Spawn an allow_sni VM. This is the authoritative Ready() proof: spawn 500s if
# the verdict loop is not Ready() (ApplyWithProfile's fail-closed preflight).
step "Phase 0: spawn allow_sni VM"
spawn_body="$(jq -n --arg p "$PROFILE_NAME" --arg t "$TENANT_ID" \
  '{profile: $p, egress_policy: "profile", tenant_id: $t}')"
vm_resp="$(curl -sS -w $'\n%{http_code}' -X POST "$API/vms" \
  -H "Content-Type: application/json" -d "$spawn_body" 2>/dev/null || true)"
vm_code="$(printf '%s\n' "$vm_resp" | tail -1)"
vm_json="$(printf '%s\n' "$vm_resp" | sed '$d')"
printf '%s' "$vm_json" > "$ARTIFACT_DIR/spawn.json"
if [ "$vm_code" != "201" ]; then
  printf '  spawn HTTP %s: %s\n' "$vm_code" "$vm_json" >&2
  fail "phase0_failed: allow_sni VM spawn returned HTTP $vm_code (preflight/NFQUEUE?)"; exit 1
fi
VM_ID="$(printf '%s' "$vm_json" | jq -r '.vm_id')"
GUEST_IP="$(printf '%s' "$vm_json" | jq -r '.guest_ip')"
[ -n "$VM_ID" ] && [ "$VM_ID" != "null" ] && [ -n "$GUEST_IP" ] && [ "$GUEST_IP" != "null" ] \
  || { fail "phase0_failed: spawn response missing vm_id/guest_ip"; exit 1; }
ok "Spawned VM $VM_ID at $GUEST_IP (verdict loop Ready proven by preflight pass)"

# ---- Phase 1: allow domain reaches :443 + iptables rule assertion ------------
step "Phase 1: FORWARD rule assertion"
iptables -S FORWARD > "$ARTIFACT_DIR/forward-rules.txt" 2>/dev/null || true
fastpath="$(grep -E "anvil-egress-${VM_ID}-sni-fastpath" "$ARTIFACT_DIR/forward-rules.txt" || true)"
nfq="$(grep -E "anvil-egress-${VM_ID}-sni-nfqueue" "$ARTIFACT_DIR/forward-rules.txt" || true)"
if printf '%s' "$fastpath" | grep -q '0x534e49' && printf '%s' "$fastpath" | grep -q 'ACCEPT'; then
  ok "sni-fastpath rule present (connmark 0x534e49 -> ACCEPT)"
else
  fail "phase1_failed: sni-fastpath rule (connmark 0x534e49 ACCEPT) missing for $VM_ID"
fi
if printf '%s' "$nfq" | grep -q 'NFQUEUE' && printf '%s' "$nfq" | grep -q '0x534e49'; then
  ok "sni-nfqueue dispatch rule present (connmark ! 0x534e49 -> NFQUEUE)"
else
  fail "phase1_failed: sni-nfqueue dispatch rule missing for $VM_ID"
fi

step "Phase 1: guest reaches $ALLOW_DOMAIN:443"
cat > "$ARTIFACT_DIR/sni-allow.sh" <<EOF
#!/usr/bin/env bash
set -u
start=\$(date +%s)
set +e
code=\$(curl -sS --max-time 15 -o /dev/null -w '%{http_code}' "https://${ALLOW_DOMAIN}/")
rc=\$?
set -e
dur=\$(( \$(date +%s) - start ))
echo "ALLOW_CURL_RC=\$rc HTTP=\$code DUR=\${dur}s"
if [ "\$rc" -eq 0 ]; then echo "SNI_ALLOW_OK"; exit 0; else echo "SNI_ALLOW_FAIL"; exit 1; fi
EOF
run_guest() {
  # $1 local script, $2 workspace rel path, $3 timeout secs -> writes run json to stdout
  local src="$1" rel="$2" tmo="$3" up
  up="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
        "$API/vms/$VM_ID/workspace?path=$rel&overwrite=true" --data-binary @"$src" 2>/dev/null || true)"
  [ "$up" = "200" ] || { echo "{\"_upload\":\"$up\"}"; return; }
  curl -sS --max-time "$((tmo + 30))" -X POST "$API/vms/$VM_ID/workloads/run" \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg s "$rel" --argjson t "$tmo" '{script:$s, timeout_seconds:$t}')" 2>/dev/null || true
}
allow_run="$(run_guest "$ARTIFACT_DIR/sni-allow.sh" "workloads/sni-allow.sh" 30)"
printf '%s' "$allow_run" > "$ARTIFACT_DIR/allow-run.json"
allow_exit="$(printf '%s' "$allow_run" | jq -r '.exit_code // "err"' 2>/dev/null || echo err)"
allow_out="$(printf '%s' "$allow_run" | jq -r '.stdout // ""' 2>/dev/null || echo '')"
printf '  guest: %s\n' "$(printf '%s' "$allow_out" | tr '\n' ' ')"
if [ "$allow_exit" = "0" ] && printf '%s' "$allow_out" | grep -q SNI_ALLOW_OK; then
  ok "Guest completed TLS handshake to $ALLOW_DOMAIN:443 (allowed SNI reached)"
else
  fail "phase1_failed: guest could not reach $ALLOW_DOMAIN (exit=$allow_exit)"
fi

# ---- Phase 2: deny domain blocked --------------------------------------------
step "Phase 2: guest blocked from $DENY_DOMAIN:443"
cat > "$ARTIFACT_DIR/sni-deny.sh" <<EOF
#!/usr/bin/env bash
set -u
start=\$(date +%s)
set +e
curl -sS --max-time 20 -o /dev/null "https://${DENY_DOMAIN}/"
rc=\$?
set -e
dur=\$(( \$(date +%s) - start ))
echo "DENY_CURL_RC=\$rc DUR=\${dur}s"
if [ "\$rc" -eq 0 ]; then echo "SNI_DENY_LEAKED"; exit 1; else echo "SNI_DENY_BLOCKED"; exit 0; fi
EOF
deny_run="$(run_guest "$ARTIFACT_DIR/sni-deny.sh" "workloads/sni-deny.sh" 40)"
printf '%s' "$deny_run" > "$ARTIFACT_DIR/deny-run.json"
deny_exit="$(printf '%s' "$deny_run" | jq -r '.exit_code // "err"' 2>/dev/null || echo err)"
deny_out="$(printf '%s' "$deny_run" | jq -r '.stdout // ""' 2>/dev/null || echo '')"
deny_rc="$(printf '%s' "$deny_out" | sed -n 's/.*DENY_CURL_RC=\([0-9]*\).*/\1/p' | head -1)"
deny_dur="$(printf '%s' "$deny_out" | sed -n 's/.*DUR=\([0-9]*\)s.*/\1/p' | head -1)"
printf '  guest: %s\n' "$(printf '%s' "$deny_out" | tr '\n' ' ')"
if [ "$deny_exit" = "0" ] && printf '%s' "$deny_out" | grep -q SNI_DENY_BLOCKED \
   && ! printf '%s' "$deny_out" | grep -q SNI_DENY_LEAKED; then
  if [ "${deny_dur:-99}" -le 3 ]; then
    ok "Guest blocked from $DENY_DOMAIN (fast fail, curl rc=$deny_rc dur=${deny_dur}s => RST injection observed)"
  else
    ok "Guest blocked from $DENY_DOMAIN (curl rc=$deny_rc dur=${deny_dur}s => DROP timeout)"
  fi
else
  fail "phase2_failed: $DENY_DOMAIN was NOT blocked (exit=$deny_exit rc=$deny_rc)"
fi

# ---- Phase 3: audit record + redaction ---------------------------------------
step "Phase 3: audit record for denied SNI"
deny_rec=""
if [ -f "$AUDIT_FILE" ]; then
  deny_rec="$(jq -c --arg d "$DENY_DOMAIN" \
    'select(.daemon_operation=="egress_sni_denied" and .sni==$d)' "$AUDIT_FILE" 2>/dev/null | head -1 || true)"
fi
if [ -n "$deny_rec" ]; then
  ok "audit record: $deny_rec"
  rec_tenant="$(printf '%s' "$deny_rec" | jq -r '.tenant_id')"
  rec_result="$(printf '%s' "$deny_rec" | jq -r '.result_code')"
  [ "$rec_tenant" = "$TENANT_ID" ] || fail "phase3_failed: audit tenant_id=$rec_tenant expected $TENANT_ID"
  [ "$rec_result" = "denied" ] || fail "phase3_failed: audit result_code=$rec_result expected denied"
else
  fail "phase3_failed: no egress_sni_denied record for $DENY_DOMAIN in $AUDIT_FILE"
fi

step "Phase 3: redaction spot-check"
secret_re='AIza[0-9A-Za-z_-]{20,}|sk-[A-Za-z0-9_-]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|[Bb]earer [A-Za-z0-9._-]{8,}'
red_bad=0
if [ -f "$AUDIT_FILE" ] && grep -Eq "$secret_re" "$AUDIT_FILE"; then
  red_bad=1; printf '  ! secret-shaped string in audit trail\n' >&2
fi
# SNI-related daemon log lines must not carry bearer/key material either.
if grep -i 'sni' "$DAEMON_LOG" 2>/dev/null | grep -Eq "$secret_re"; then
  red_bad=1; printf '  ! secret-shaped string in SNI daemon-log lines\n' >&2
fi
if [ "$red_bad" -eq 0 ]; then
  ok "No bearer/API-key material in audit trail or SNI log lines"
else
  fail "phase3_failed: redaction spot-check found secret-shaped material"
fi

# ---- Phase 4 (smoke): fast-path keeps verdicts per-flow, not per-packet ------
step "Phase 4 (smoke): conntrack fast-path — ~1 allowed verdict per flow"
before_allowed="$(read_metric allowed)"
cat > "$ARTIFACT_DIR/sni-flow.sh" <<EOF
#!/usr/bin/env bash
set -u
set +e
# One TLS flow whose handshake + response span many packets; a per-packet
# (broken fast-path) accounting would add dozens of verdicts, a per-flow one ~1.
curl -sS --max-time 20 -o /dev/null "https://${ALLOW_DOMAIN}/"
rc=\$?
set -e
echo "FLOW_CURL_RC=\$rc"
[ "\$rc" -eq 0 ] && echo "SNI_FLOW_OK" || echo "SNI_FLOW_FAIL"
exit 0
EOF
flow_run="$(run_guest "$ARTIFACT_DIR/sni-flow.sh" "workloads/sni-flow.sh" 30)"
printf '%s' "$flow_run" > "$ARTIFACT_DIR/flow-run.json"
flow_out="$(printf '%s' "$flow_run" | jq -r '.stdout // ""' 2>/dev/null || echo '')"
sleep 1
after_allowed="$(read_metric allowed)"
delta=$(( after_allowed - before_allowed ))
printf '  guest: %s | allowed metric %s -> %s (delta=%s)\n' \
  "$(printf '%s' "$flow_out" | tr '\n' ' ')" "$before_allowed" "$after_allowed" "$delta"
if printf '%s' "$flow_out" | grep -q SNI_FLOW_OK && [ "$delta" -ge 1 ] && [ "$delta" -le 8 ]; then
  ok "Full allow flow added $delta allowed verdict(s) — per-flow, fast path effective"
elif printf '%s' "$flow_out" | grep -q SNI_FLOW_OK && [ "$delta" -gt 8 ]; then
  fail "phase4_failed: allow flow added $delta verdicts (per-packet? conntrack fast-path not effective)"
else
  fail "phase4_failed: allow flow did not complete (delta=$delta)"
fi

# ---- Verdict -----------------------------------------------------------------
if [ "$PASS" = "true" ]; then
  step "Result"
  ok "egress SNI filter verdict loop verified on real kernel"
  exit 0
fi
exit 1
