#!/usr/bin/env bash
#
# anvil-quic-sni-e2e.sh — KVM end-to-end gate for the QUIC/UDP:443 SNI filter.
#
# WHAT THIS PROVES (real kernel, real VM, real public domains — the first live
# execution of the QUIC verdict path from Tasks 1-6: Initial key derivation,
# header/AES-GCM decrypt, CRYPTO reassembly, ParseInitialSNI, the UDP branch of
# the NFQUEUE verdict loop, and the UDP:443 dispatch/fast-path iptables rules):
#   Phase 0  daemon + verdict loop bind NFQUEUE; allow_sni profile VM spawns
#            (spawn only succeeds when sniLoop.Ready()), and the VM's
#            -sni-udp-nfqueue / -sni-udp-fastpath FORWARD rules exist.
#   Phase 1  guest completes a real HTTP/3 (QUIC) GET to <allow>:443 — the
#            client Initial's SNI is decrypted + parsed + matched, accepted with
#            connmark 0x534e49, and the -sni-udp-fastpath rule (match --mark
#            0x534e49) then carries the rest of the flow (its packet counter
#            climbs past zero — connmark fast-path proven effective, verdict loop
#            stays per-flow not per-packet).
#   Phase 2  guest CANNOT complete an HTTP/3 GET to <deny>:443 — <deny> DOES
#            speak HTTP/3 (so the ONLY reason it fails is our filter, not a
#            server that refuses QUIC), its Initial is silently dropped (no RST
#            for UDP), the client burns its whole deadline and returns non-zero.
#            The probe is HTTP/3-only (quic-go http3.Transport never falls back
#            to TCP), so this measures pure QUIC blocking, not a TCP:443 catch.
#   Phase 3  the deny is recorded in the daemon runtime audit trail
#            (egress_sni_denied + sni=<deny>) and a redaction spot-check confirms
#            no bearer/API-key material leaked into audit or SNI log lines.
#
# HTTP/3 CLIENT (spec open-question #2, resolved here): the Debian-trixie golden
# image ships stock curl, which is built WITHOUT HTTP/3 (no `curl --http3`), and
# a profile VM's egress is default-REJECT so the guest cannot download one. So we
# build a tiny quic-go HTTP/3 client on the host (CGO_ENABLED=0 static
# linux/amd64), split it under the agent's 4 MiB workspace-upload cap, upload the
# chunks, and reassemble+exec it in the guest. This modifies NOTHING in the
# golden image (no staleness rebuild) and sends a REAL QUIC v1 Initial with a
# real TLS ClientHello, so a valid allow domain actually completes the handshake.
#
# WHAT THIS DOES NOT COVER: QUIC v2, IPv6, 0-RTT, wildcard SNI breadth, CRYPTO
# fragmentation edge cases (all unit-tested in internal/network/quic). Verdict =
# exit code + final "passed" line only.
#
# Requires: root, /dev/kvm, iptables + NFQUEUE (nfnetlink_queue), Go toolchain,
# outbound UDP:443. Run:  sudo -n bash scripts/anvil-quic-sni-e2e.sh
#
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

# ---- Configuration -----------------------------------------------------------
# Both domains MUST support HTTP/3, else a "block" cannot be distinguished from a
# server that simply refuses QUIC. cloudflare.com + www.google.com both do.
ALLOW_DOMAIN="${ANVIL_QUIC_E2E_ALLOW_DOMAIN:-cloudflare.com}"
DENY_DOMAIN="${ANVIL_QUIC_E2E_DENY_DOMAIN:-www.google.com}"
# Source checkout that owns the prebuilt VM artifacts + gitignored goose config
# (this worktree may carry neither; the daemon binary is still built from HERE).
SRC_CHECKOUT="${ANVIL_QUIC_E2E_SRC:-/data/projects/claude-zone/anvil}"
API="${ANVIL_QUIC_E2E_API:-http://127.0.0.1:3000}"
API_ADDR="${ANVIL_QUIC_E2E_API_ADDR:-127.0.0.1:3000}"
QUICGO_VERSION="${ANVIL_QUIC_E2E_QUICGO_VERSION:-v0.60.0}"
PROBE_TIMEOUT="${ANVIL_QUIC_E2E_PROBE_TIMEOUT:-15}"
PROFILE_NAME="quic-sni-e2e"
TENANT_ID="quic-sni-e2e-tenant"
ARTIFACT_DIR="${ANVIL_QUIC_E2E_ARTIFACT_DIR:-/tmp/anvil-quic-sni-e2e-$(date +%Y%m%d-%H%M%S)}"

HOME_DIR="$ARTIFACT_DIR/home"
EGRESS_PROFILE_DIR="$ARTIFACT_DIR/egress-profiles"
PROBE_DIR="$ARTIFACT_DIR/quic-probe"
PROBE_BIN="$PROBE_DIR/quic-probe"
CHUNK_DIR="$ARTIFACT_DIR/probe-chunks"
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

# Packet counter of a FORWARD rule identified by its comment (iptables -nvL: pkts
# is column 1). Prints 0 when the rule is absent.
rule_pkts() {
  local comment="$1"
  iptables -nvL FORWARD 2>/dev/null \
    | awk -v c="$comment" '$0 ~ c {print $1; exit}' | tr -dc '0-9' || true
}

# Upload a local file into the guest workspace at $2 (relative path).
ws_put() {
  local src="$1" rel="$2" code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
        "$API/vms/$VM_ID/workspace?path=$rel&overwrite=true" \
        --data-binary @"$src" 2>/dev/null || true)"
  [ "$code" = "200" ]
}

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
    printf '\nAll QUIC SNI e2e steps passed ✓\n'
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
require_cmd split || exit 1
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

# ---- Build the guest HTTP/3 (QUIC) probe client ------------------------------
# A tiny quic-go client: the golden image's stock curl has no --http3, and a
# profile VM is default-REJECT so the guest cannot fetch one. Static so it runs
# on the Debian guest with no shared libs. Built in its own throwaway module so
# the production go.mod stays free of the quic-go dependency.
step "Build QUIC probe (quic-go $QUICGO_VERSION, static linux/amd64)"
mkdir -p "$PROBE_DIR"
# The go directive is 3-component (1.25.0) on purpose: it maps GOTOOLCHAIN=auto
# to the same cached go1.25.0 toolchain the daemon build already uses. A
# 2-component "go 1.25" makes the toolchain selector chase an uncached "go1.25"
# download (fails under root's offline proxy), and quic-go itself requires
# go >= 1.25, so an older directive would trigger a toolchain download too.
cat > "$PROBE_DIR/go.mod" <<MODEOF
module quicprobe

go 1.25.0

require github.com/quic-go/quic-go $QUICGO_VERSION
MODEOF
cat > "$PROBE_DIR/main.go" <<'GOEOF'
// quic-probe: perform one HTTP/3 (QUIC) GET and report OK/FAIL + exit code.
// HTTP/3 ONLY — never falls back to TCP, so a drop of the QUIC Initial surfaces
// as a full-deadline failure rather than a silent TCP:443 success.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: quic-probe <url> [timeout_secs]")
		os.Exit(2)
	}
	url := os.Args[1]
	secs := 15
	if len(os.Args) > 2 {
		fmt.Sscanf(os.Args[2], "%d", &secs)
	}
	// Pin classical X25519 key exchange. Go 1.24+ / quic-go default to the
	// post-quantum X25519MLKEM768 group, whose ~1200-byte key_share inflates the
	// TLS ClientHello past a single QUIC Initial datagram, so it spans two
	// datagrams. anvil inspects only the FIRST Initial datagram (fail-closed by
	// design), so a PQ ClientHello is denied even for an allowed SNI. X25519 keeps
	// the ClientHello ~300 bytes (one datagram), inside anvil's parse envelope, so
	// this probe exercises the real decrypt/reassembly/verdict chain. (The PQ
	// multi-datagram limitation is a separately-reported finding.)
	tr := &http3.Transport{TLSClientConfig: &tls.Config{CurvePreferences: []tls.CurveID{tls.X25519}}}
	defer tr.Close()
	// Do NOT follow redirects: a cross-host 3xx (e.g. cloudflare.com -> www.
	// cloudflare.com) would open a SECOND QUIC connection to a DIFFERENT SNI than
	// the one under test, which the egress filter may legitimately deny. Returning
	// the 3xx as-is keeps the probe scoped to exactly the SNI it was asked to reach;
	// any HTTP response (2xx or 3xx) proves the QUIC handshake to that SNI completed.
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Printf("QUIC_HTTP3_FAIL err=%v\n", err)
		os.Exit(1)
	}
	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start).Seconds()
	if err != nil {
		fmt.Printf("QUIC_HTTP3_FAIL dur=%.1fs err=%v\n", dur, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	fmt.Printf("QUIC_HTTP3_OK status=%d proto=%s dur=%.1fs\n", resp.StatusCode, resp.Proto, dur)
	os.Exit(0)
}
GOEOF
if ! ( cd "$PROBE_DIR" && GOFLAGS=-mod=mod CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -trimpath -ldflags="-s -w" -o quic-probe . ) 2>"$ARTIFACT_DIR/probe-build.log"; then
  cat "$ARTIFACT_DIR/probe-build.log" >&2
  fail "build_failed: quic-go probe (go build)"
  exit 1
fi
probe_sz="$(stat -c%s "$PROBE_BIN")"
ok "Built quic-probe ($probe_sz bytes)"
# Split under the agent's 4 MiB (maxWorkspaceFileBytes) workspace upload cap.
mkdir -p "$CHUNK_DIR"
split -b 3m -d -a 3 "$PROBE_BIN" "$CHUNK_DIR/probe.part."
n_chunks="$(find "$CHUNK_DIR" -type f -name 'probe.part.*' | wc -l | tr -dc '0-9')"
ok "Split into $n_chunks chunk(s) (<= 3 MiB each)"

# ---- Assemble an isolated EPHEMERA_HOME --------------------------------------
step "Assemble scratch EPHEMERA_HOME"
mkdir -p "$HOME_DIR/artifacts" "$HOME_DIR/configs"
for f in "$SRC_CHECKOUT"/artifacts/*; do ln -sf "$f" "$HOME_DIR/artifacts/$(basename "$f")"; done
cp "$SRC_CHECKOUT/configs/goose.yaml" "$HOME_DIR/configs/goose.yaml"
cp "$SRC_CHECKOUT/configs/goose-secrets.yaml" "$HOME_DIR/configs/goose-secrets.yaml"
# Point the build-input scripts dir at the SRC checkout (same provenance as the
# golden image, so EnsureGoldenImage's pathsNewerThan staleness check does NOT
# trigger a rebuild). The daemon binary itself is still built from THIS worktree
# above (that is what carries the QUIC verdict code); only the guest image side
# comes from SRC.
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

# ---- Phase 0: UDP:443 FORWARD rule assertion ---------------------------------
step "Phase 0: UDP:443 dispatch/fast-path FORWARD rules"
iptables -S FORWARD > "$ARTIFACT_DIR/forward-rules.txt" 2>/dev/null || true
udp_fastpath="$(grep -E "anvil-egress-${VM_ID}-sni-udp-fastpath" "$ARTIFACT_DIR/forward-rules.txt" || true)"
udp_nfq="$(grep -E "anvil-egress-${VM_ID}-sni-udp-nfqueue" "$ARTIFACT_DIR/forward-rules.txt" || true)"
if printf '%s' "$udp_fastpath" | grep -q 'udp' && printf '%s' "$udp_fastpath" | grep -q '0x534e49' \
   && printf '%s' "$udp_fastpath" | grep -q 'ACCEPT'; then
  ok "sni-udp-fastpath rule present (udp/443 connmark 0x534e49 -> ACCEPT)"
else
  fail "phase0_failed: sni-udp-fastpath rule (udp connmark 0x534e49 ACCEPT) missing for $VM_ID"
fi
if printf '%s' "$udp_nfq" | grep -q 'udp' && printf '%s' "$udp_nfq" | grep -q 'NFQUEUE' \
   && printf '%s' "$udp_nfq" | grep -q '0x534e49'; then
  ok "sni-udp-nfqueue dispatch rule present (udp/443 connmark ! 0x534e49 -> NFQUEUE)"
else
  fail "phase0_failed: sni-udp-nfqueue dispatch rule missing for $VM_ID"
fi

# Upload the probe chunks into the guest workspace (bin/, reassembled at run).
step "Phase 0: upload QUIC probe into guest"
up_ok=true
for c in "$CHUNK_DIR"/probe.part.*; do
  ws_put "$c" "bin/$(basename "$c")" || up_ok=false
done
if [ "$up_ok" = "true" ]; then
  ok "Uploaded $n_chunks probe chunk(s) to guest workspace bin/"
else
  fail "phase0_failed: probe chunk upload failed"; exit 1
fi

# ---- Phase 1: allow domain reaches :443 over HTTP/3 --------------------------
step "Phase 1: guest completes HTTP/3 GET to $ALLOW_DOMAIN:443"
before_allowed="$(read_metric allowed)"
cat > "$ARTIFACT_DIR/quic-allow.sh" <<EOF
#!/usr/bin/env bash
set -u
cat bin/probe.part.* > /tmp/quic-probe
chmod +x /tmp/quic-probe
start=\$(date +%s)
set +e
out=\$(/tmp/quic-probe "https://${ALLOW_DOMAIN}/" ${PROBE_TIMEOUT} 2>&1)
rc=\$?
set -e
dur=\$(( \$(date +%s) - start ))
echo "\$out"
echo "ALLOW_PROBE_RC=\$rc DUR=\${dur}s"
if [ "\$rc" -eq 0 ]; then echo "QUIC_ALLOW_OK"; exit 0; else echo "QUIC_ALLOW_FAIL"; exit 1; fi
EOF
allow_run="$(run_guest "$ARTIFACT_DIR/quic-allow.sh" "workloads/quic-allow.sh" $((PROBE_TIMEOUT + 25)))"
printf '%s' "$allow_run" > "$ARTIFACT_DIR/allow-run.json"
allow_exit="$(printf '%s' "$allow_run" | jq -r '.exit_code // "err"' 2>/dev/null || echo err)"
allow_out="$(printf '%s' "$allow_run" | jq -r '.stdout // ""' 2>/dev/null || echo '')"
printf '  guest: %s\n' "$(printf '%s' "$allow_out" | tr '\n' ' ')"
if [ "$allow_exit" = "0" ] && printf '%s' "$allow_out" | grep -q QUIC_ALLOW_OK \
   && printf '%s' "$allow_out" | grep -q QUIC_HTTP3_OK; then
  ok "Guest completed a real HTTP/3 handshake+GET to $ALLOW_DOMAIN:443 (allowed SNI reached over QUIC)"
else
  fail "phase1_failed: guest could not reach $ALLOW_DOMAIN over HTTP/3 (exit=$allow_exit)"
fi

# Fast-path proof: the -sni-udp-fastpath rule (match --mark 0x534e49) only counts
# packets AFTER the verdict loop stamped the connmark, i.e. the handshake+data
# tail of the flow. A non-zero counter proves connmark 0x534e49 was applied and
# the fast path carried the flow (verdict loop stayed off the per-packet path).
step "Phase 1: connmark 0x534e49 fast-path effective"
sleep 1
fp_pkts="$(rule_pkts "anvil-egress-${VM_ID}-sni-udp-fastpath")"
nfq_pkts="$(rule_pkts "anvil-egress-${VM_ID}-sni-udp-nfqueue")"
after_allowed="$(read_metric allowed)"
allowed_delta=$(( after_allowed - before_allowed ))
printf '  udp-fastpath pkts=%s | udp-nfqueue pkts=%s | allowed metric %s -> %s (delta=%s)\n' \
  "${fp_pkts:-0}" "${nfq_pkts:-0}" "$before_allowed" "$after_allowed" "$allowed_delta"
if [ "${fp_pkts:-0}" -ge 1 ]; then
  ok "sni-udp-fastpath carried ${fp_pkts} packet(s) (connmark 0x534e49 set + fast path effective)"
else
  fail "phase1_failed: sni-udp-fastpath counter is 0 (connmark not applied? fast path not effective)"
fi
if [ "${nfq_pkts:-0}" -ge 1 ]; then
  ok "sni-udp-nfqueue dispatched ${nfq_pkts} Initial packet(s) to the verdict loop"
else
  fail "phase1_failed: sni-udp-nfqueue counter is 0 (no QUIC Initial reached the verdict loop)"
fi
if [ "$allowed_delta" -ge 1 ] && [ "$allowed_delta" -le 8 ]; then
  ok "Allow flow added $allowed_delta allowed verdict(s) — per-flow, not per-packet"
elif [ "$allowed_delta" -gt 8 ]; then
  fail "phase1_failed: allow flow added $allowed_delta verdicts (per-packet? fast path not effective)"
else
  fail "phase1_failed: allow flow added no allowed verdict (delta=$allowed_delta)"
fi

# ---- Phase 2: deny domain blocked over HTTP/3 (QUIC-only) --------------------
# $DENY_DOMAIN speaks HTTP/3, so the ONLY reason this can fail is our filter
# dropping its QUIC Initial. The probe never falls back to TCP, so this is a pure
# QUIC block, and (unlike TCP RST) a UDP drop is silent -> the client burns its
# whole deadline before giving up (dur ~= PROBE_TIMEOUT).
step "Phase 2: guest blocked from $DENY_DOMAIN:443 over HTTP/3"
before_denied="$(read_metric denied)"
cat > "$ARTIFACT_DIR/quic-deny.sh" <<EOF
#!/usr/bin/env bash
set -u
cat bin/probe.part.* > /tmp/quic-probe
chmod +x /tmp/quic-probe
start=\$(date +%s)
set +e
out=\$(/tmp/quic-probe "https://${DENY_DOMAIN}/" ${PROBE_TIMEOUT} 2>&1)
rc=\$?
set -e
dur=\$(( \$(date +%s) - start ))
echo "\$out"
echo "DENY_PROBE_RC=\$rc DUR=\${dur}s"
if [ "\$rc" -eq 0 ]; then echo "QUIC_DENY_LEAKED"; exit 1; else echo "QUIC_DENY_BLOCKED"; exit 0; fi
EOF
deny_run="$(run_guest "$ARTIFACT_DIR/quic-deny.sh" "workloads/quic-deny.sh" $((PROBE_TIMEOUT + 25)))"
printf '%s' "$deny_run" > "$ARTIFACT_DIR/deny-run.json"
deny_exit="$(printf '%s' "$deny_run" | jq -r '.exit_code // "err"' 2>/dev/null || echo err)"
deny_out="$(printf '%s' "$deny_run" | jq -r '.stdout // ""' 2>/dev/null || echo '')"
deny_rc="$(printf '%s' "$deny_out" | sed -n 's/.*DENY_PROBE_RC=\([0-9]*\).*/\1/p' | head -1)"
deny_dur="$(printf '%s' "$deny_out" | sed -n 's/.*DUR=\([0-9]*\)s.*/\1/p' | head -1)"
printf '  guest: %s\n' "$(printf '%s' "$deny_out" | tr '\n' ' ')"
if [ "$deny_exit" = "0" ] && printf '%s' "$deny_out" | grep -q QUIC_DENY_BLOCKED \
   && ! printf '%s' "$deny_out" | grep -q QUIC_DENY_LEAKED; then
  ok "Guest blocked from $DENY_DOMAIN over HTTP/3 (probe rc=$deny_rc dur=${deny_dur}s => Initial silently dropped)"
else
  fail "phase2_failed: $DENY_DOMAIN was NOT blocked over HTTP/3 (exit=$deny_exit rc=$deny_rc)"
fi
after_denied="$(read_metric denied)"
denied_delta=$(( after_denied - before_denied ))
if [ "$denied_delta" -ge 1 ]; then
  ok "Deny flow added $denied_delta denied verdict(s) (QUIC Initials re-queued + dropped, never marked)"
else
  fail "phase2_failed: no denied verdict recorded for the QUIC deny flow (delta=$denied_delta)"
fi

# ---- Phase 3: audit record + redaction ---------------------------------------
step "Phase 3: audit record for denied QUIC SNI"
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

# ---- Verdict -----------------------------------------------------------------
if [ "$PASS" = "true" ]; then
  step "Result"
  ok "QUIC/UDP:443 SNI filter verdict path verified on real kernel"
  exit 0
fi
exit 1
