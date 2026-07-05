#!/usr/bin/env bash
# webdev_demo.sh — ephemera-powered autonomous 3-agent React+Vite website team.
#
# Spins up ephemera-daemon, posts a flock of {orchestrator, worker, reviewer},
# then dispatches a single high-level brief to the orchestrator's /tasks
# endpoint. The orchestrator (in-VM Goose agent) drives worker and reviewer
# peers via the `gtcall` helper and publishes every file body to the Town
# Wall as `<<<FILE: path>>>...<<<END>>>` sentinels. The host subscribes to
# the Town Wall SSE stream, extracts each sentinel block into site/, then
# builds and serves the result with vite preview.
#
# Designed for an 8 GiB RAM laptop: 3 VMs total 2 GiB (orchestrator 1024 +
# worker 512 + reviewer 512 MiB) plus host build overhead. Set WEBDEV_MIN_MEM_MIB
# lower (or drop_caches first) if the preflight rejects.
#
# Usage:
#   sudo bash webdev_demo.sh
# Optional env:
#   WEBDEV_TASK="..."             override the website requirement task
#   WEBDEV_TIMEOUT_SECS=900       max wallclock for orchestrator to finish (default 15 min)
#   WEBDEV_ORCH_ATTEMPTS=4        retries on Groq's intermittent tool-call failures
#   WEBDEV_ORCH_MODEL=...         override a role's Groq model (also WEBDEV_WORKER_MODEL, WEBDEV_REVIEWER_MODEL)
#   WEBDEV_PREVIEW_PORT=5173      vite preview port
#   WEBDEV_MIN_MEM_MIB=3584       memory floor before preflight refuses

set -u
set -o pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROFILES_DIR="${REPO_ROOT}/configs/profiles"
ASSETS_DIR="${REPO_ROOT}/configs/webdev-demo/profiles"
TEMPLATE_DIR="${REPO_ROOT}/configs/webdev-demo/vite-template"
GLOBAL_SECRETS="${REPO_ROOT}/configs/goose-secrets.yaml"

API="http://localhost:3000"
PREVIEW_PORT="${WEBDEV_PREVIEW_PORT:-5173}"
ORCH_TIMEOUT="${WEBDEV_TIMEOUT_SECS:-900}"
# Three VMs total 2 GiB (1024+512+512) + a host budget for npm/vite/node. The
# preflight uses MemAvailable, which already excludes reclaimable cache, so 3584
# is a fair guardrail for an 8 GiB laptop with light desktop load.
MIN_MEM_MIB="${WEBDEV_MIN_MEM_MIB:-3584}"

DEFAULT_TASK="Build a single-page React + Vite portfolio site for a software engineer named Jane Doe. Include exactly four sections: a Hero (name + one-line tagline), About (two short paragraphs), Projects (three project cards with title + one-sentence description), and Contact (mailto link). Modern minimal design using inline styles only — no external CSS frameworks, no extra npm packages, just React + react-dom. Produce these files via your operating loop: src/App.jsx, src/main.jsx, src/index.css, index.html."

TASK="${WEBDEV_TASK:-$DEFAULT_TASK}"

# All three roles participate. orchestrator drives the loop; worker emits raw
# file bodies; reviewer is a best-effort verdict gate.
ROLES=(orchestrator worker reviewer)

# State (populated as the script runs).
RUNDIR=""
DAEMON_PID=""
PREVIEW_PID=""
HARVEST_PID=""
FLOCK_ID=""
DEMO_OK=true
ORCH_VM_ID=""
PROFILES_INSTALLED=false
ORCH_URL=""

# ── Output helpers ───────────────────────────────────────────────
step()  { printf "\n━━━ %s ━━━\n" "$*"; }
ok()    { printf "  ✓ %s\n" "$*"; }
fail()  { printf "  ✗ %s\n" "$*"; DEMO_OK=false; }
note()  { printf "    %s\n" "$*"; }
fatal() { printf "\n✗ %s\n" "$*" >&2; exit 1; }

# ── Cleanup trap ─────────────────────────────────────────────────
cleanup() {
    local rc=$?
    trap - INT TERM EXIT
    printf "\n━━━ Shutting down demo ━━━\n"

    # 1) Stop the SSE harvester if still running.
    if [ -n "$HARVEST_PID" ] && kill -0 "$HARVEST_PID" 2>/dev/null; then
        kill -TERM "$HARVEST_PID" 2>/dev/null || true
        wait "$HARVEST_PID" 2>/dev/null || true
    fi

    # 2) Delete flock so VMs/TAPs/IPs go back cleanly while daemon still up.
    if [ -n "$FLOCK_ID" ] && [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
        curl -sf -X DELETE "$API/flocks/${FLOCK_ID}" >/dev/null 2>&1 || true
        ok "Flock ${FLOCK_ID} deleted"
    fi

    # 3) Stop vite preview.
    if [ -n "$PREVIEW_PID" ] && kill -0 "$PREVIEW_PID" 2>/dev/null; then
        kill -TERM "$PREVIEW_PID" 2>/dev/null || true
        wait "$PREVIEW_PID" 2>/dev/null || true
    fi

    # 4) Stop daemon (graceful, then force).
    if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
        kill -TERM "$DAEMON_PID" 2>/dev/null || true
        local waited=0
        while [ $waited -lt 8 ] && kill -0 "$DAEMON_PID" 2>/dev/null; do
            sleep 1; waited=$((waited + 1))
        done
        kill -0 "$DAEMON_PID" 2>/dev/null && kill -KILL "$DAEMON_PID" 2>/dev/null || true
    fi

    # 5) Remove demo profiles the run installed (and restore any backups).
    if $PROFILES_INSTALLED; then
        uninstall_profiles
    fi

    if $DEMO_OK; then
        ok "Demo shut down cleanly. Artifacts kept at: ${RUNDIR}"
    else
        printf "  ✗ Demo exited with a failure earlier. Logs: %s\n" "$RUNDIR"
        exit 1
    fi
    exit "$rc"
}
trap cleanup INT TERM EXIT

# ── 1. Preflight ─────────────────────────────────────────────────
preflight() {
    step "1. Preflight"

    [ "$(id -u)" -eq 0 ] || fatal "Run as root (sudo bash webdev_demo.sh) — KVM + network setup require it."
    [ -r /dev/kvm ] && [ -w /dev/kvm ] || fatal "/dev/kvm not accessible. Enable virtualization in BIOS / nested virt."

    for tool in curl jq tar awk python3 sha256sum ss node npm; do
        command -v "$tool" >/dev/null || fatal "Missing required tool: $tool"
    done
    ok "Required tools present (curl jq awk python3 ss node npm)"

    local node_major
    node_major=$(node --version | sed -E 's/^v([0-9]+).*/\1/')
    [ "$node_major" -ge 20 ] || fatal "Node.js >= 20 required, found v${node_major}"
    ok "Node.js v$(node --version | tr -d v), npm $(npm --version)"

    # Daemon binary.
    [ -x "${REPO_ROOT}/ephemera-daemon" ] || {
        note "ephemera-daemon binary missing; building..."
        (cd "$REPO_ROOT" && go build -o ephemera-daemon ./cmd/goose-daemon) || fatal "go build failed"
    }
    ok "ephemera-daemon binary ready"

    # Ports.
    if ss -tlnH "sport = :3000" 2>/dev/null | grep -q LISTEN; then
        fatal "Port 3000 already in use. Stop the existing process and retry."
    fi
    if ss -tlnH "sport = :${PREVIEW_PORT}" 2>/dev/null | grep -q LISTEN; then
        fatal "Vite preview port ${PREVIEW_PORT} already in use."
    fi
    ok "Ports 3000 and ${PREVIEW_PORT} are free"

    # Demo profile assets (system.md + goose.yaml) exist for every role.
    for role in "${ROLES[@]}"; do
        for f in goose.yaml system.md; do
            [ -f "${ASSETS_DIR}/${role}/${f}" ] || fatal "Missing ${ASSETS_DIR}/${role}/${f}"
        done
    done
    ok "Demo profile assets present for ${ROLES[*]}"

    # vite-template present.
    for f in package.json vite.config.js index.html src/main.jsx src/App.jsx src/index.css; do
        [ -f "${TEMPLATE_DIR}/${f}" ] || fatal "Missing template file: ${TEMPLATE_DIR}/${f}"
    done
    ok "vite-template scaffolded at ${TEMPLATE_DIR}"

    # Surface stale binaries so the user knows the next start may rebuild.
    if [ -e "${REPO_ROOT}/artifacts/micro-init" ]; then
        if [ "${REPO_ROOT}/cmd/micro-init/main.go" -nt "${REPO_ROOT}/artifacts/micro-init" ]; then
            note "micro-init source newer than binary — daemon will rebuild on start (~5s)"
        fi
    fi
    if [ -e "${REPO_ROOT}/artifacts/golden-image.ext4" ]; then
        for input in "${REPO_ROOT}/scripts/build_image.sh" "${REPO_ROOT}/scripts/gtwall" "${REPO_ROOT}/scripts/gtcall" "${REPO_ROOT}/cmd/micro-init/main.go" "${REPO_ROOT}/cmd/goose-agent/main.go"; do
            if [ -e "$input" ] && [ "$input" -nt "${REPO_ROOT}/artifacts/golden-image.ext4" ]; then
                note "Golden image stale relative to $(basename "$input") — daemon will rebuild (~5-10 min on first start)"
                break
            fi
        done
    fi

    # Hybrid demo: the orchestrator runs on Google Gemini (reliable multi-turn tool
    # use), worker/reviewer on Groq gpt-oss. Both keys must be in the global keychain —
    # flock agents read them from there; the demo writes no per-role secrets.
    local keyname kval
    for keyname in GROQ_API_KEY GOOGLE_API_KEY; do
        if ! grep -qE "^[^#]*${keyname}:" "$GLOBAL_SECRETS" 2>/dev/null; then
            fatal "${keyname} not set in ${GLOBAL_SECRETS}. The hybrid demo needs both GROQ_API_KEY (worker/reviewer) and GOOGLE_API_KEY (orchestrator)."
        fi
        kval=$(grep -E "^[^#]*${keyname}:" "$GLOBAL_SECRETS" | head -1 | sed -E 's/^[^:]+:[[:space:]]*"?([^"]*)"?$/\1/')
        [ -n "$kval" ] || fatal "${keyname} value empty in ${GLOBAL_SECRETS}"
        case "$kval" in your-key-here|gsk_your-key-here|AIza-your-key-here) fatal "${keyname} in ${GLOBAL_SECRETS} is still the placeholder." ;; esac
    done
    ok "API keys present: GROQ_API_KEY (worker/reviewer) + GOOGLE_API_KEY (orchestrator)"

    # Memory headroom — autonomous 3-VM flock needs ~6 GiB.
    local avail_mib
    avail_mib=$(awk '/MemAvailable:/ {print int($2/1024)}' /proc/meminfo)
    if [ "$avail_mib" -lt "$MIN_MEM_MIB" ]; then
        fail "Available memory ${avail_mib} MiB < ${MIN_MEM_MIB} MiB needed."
        note "Try one of:"
        note "  sudo sync && sudo sysctl -w vm.drop_caches=3       (reclaim page cache)"
        note "  sudo WEBDEV_MIN_MEM_MIB=${avail_mib} bash ${BASH_SOURCE[0]}   (override; env must be inside sudo, sudoers strips outer env)"
        note "  Close browser tabs, IDE, Docker, etc. and retry"
        fatal "Insufficient memory headroom."
    fi
    ok "Memory headroom: ${avail_mib} MiB available (need ≥ ${MIN_MEM_MIB})"

    RUNDIR=$(mktemp -d /tmp/ephemera-webdev-XXXXXX)
    chmod 0755 "$RUNDIR"
    mkdir -p "${RUNDIR}/site" "${RUNDIR}/townwall"
    ok "Runtime directory: $RUNDIR"
}

# ── 2. Install demo role profiles into configs/profiles/ ─────────
# Flock spawn resolves each role from configs/profiles/{role}/ — goose.yaml for
# provider/model/sizing, system.md for behavior. We install the demo assets there
# for the run and remove them on cleanup. A pre-existing user profile of the same
# name is backed up to {role}.webdev_bak and restored afterwards. Secrets are NOT
# written per role — flock agents read the global configs/goose-secrets.yaml.
install_profiles() {
    step "2. Install demo role profiles (system.md + goose.yaml)"

    for role in "${ROLES[@]}"; do
        local src="${ASSETS_DIR}/${role}"
        local dst="${PROFILES_DIR}/${role}"

        # Replace any same-named profile wholesale so no stale files linger;
        # keep a backup to restore on cleanup.
        if [ -e "$dst" ]; then
            rm -rf "${dst}.webdev_bak"
            mv -f "$dst" "${dst}.webdev_bak"
            note "${role}: existing profile backed up to ${role}.webdev_bak"
        fi

        mkdir -p "$dst"
        cp -f "${src}/goose.yaml" "${dst}/goose.yaml"
        cp -f "${src}/system.md" "${dst}/system.md"

        # Optional per-role Groq model override for experimentation:
        # WEBDEV_ORCH_MODEL / WEBDEV_WORKER_MODEL / WEBDEV_REVIEWER_MODEL. sudo strips
        # outer env, so pass it inside sudo:
        #   sudo WEBDEV_ORCH_MODEL=openai/gpt-oss-20b bash webdev_demo.sh
        local mvar="WEBDEV_$(printf '%s' "$role" | tr '[:lower:]' '[:upper:]' | sed 's/^ORCHESTRATOR$/ORCH/')_MODEL"
        local override="${!mvar:-}"
        if [ -n "$override" ]; then
            sed -i -E "s|^GOOSE_MODEL:.*|GOOSE_MODEL: ${override}|" "${dst}/goose.yaml"
            ok "${role}: installed (model override → ${override})"
        else
            ok "${role}: installed (Groq + sizing from configs/webdev-demo/profiles/${role})"
        fi
    done

    PROFILES_INSTALLED=true
}

uninstall_profiles() {
    note "Removing demo profiles and restoring any backups..."
    for role in "${ROLES[@]}"; do
        local dst="${PROFILES_DIR}/${role}"
        rm -rf "$dst"
        if [ -e "${dst}.webdev_bak" ]; then
            mv -f "${dst}.webdev_bak" "$dst"
            ok "${role}: original profile restored"
        else
            ok "${role}: demo profile removed"
        fi
    done
    PROFILES_INSTALLED=false
}

# ── 3. Start ephemera daemon ─────────────────────────────────────
start_daemon() {
    step "3. Start ephemera-daemon"

    local log="${RUNDIR}/daemon.log"
    (
        cd "$REPO_ROOT"
        EPHEMERA_API_ADDR=0.0.0.0:3000 \
        EPHEMERA_LOG_FORMAT=json \
        EPHEMERA_LOG_LEVEL=info \
        ./ephemera-daemon >"$log" 2>&1 &
        echo $! > "${RUNDIR}/daemon.pid"
    )
    DAEMON_PID=$(cat "${RUNDIR}/daemon.pid")
    ok "ephemera-daemon PID=$DAEMON_PID  log=$log"

    note "Waiting for daemon API (up to 600s; first run bakes the golden image)..."
    local waited=0
    until curl -sf -o /dev/null --max-time 2 "$API/vms"; do
        if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
            fatal "Daemon exited prematurely. Tail of $log:\n$(tail -40 "$log")"
        fi
        sleep 2
        waited=$((waited + 2))
        if [ $waited -ge 600 ]; then
            fatal "Daemon did not become ready within 600s. Tail:\n$(tail -40 "$log")"
        fi
        if [ $((waited % 30)) -eq 0 ]; then
            note "  ... still waiting (${waited}s)"
        fi
    done
    ok "Daemon API ready at $API"
}

# ── 4. Create the 3-agent flock ──────────────────────────────────
create_flock() {
    step "4. Create flock (orchestrator + worker + reviewer)"

    local body
    body=$(jq -n --arg task "$TASK" \
        '{task: $task, roles: ["orchestrator","worker","reviewer"]}')

    local resp_file="${RUNDIR}/flock-create.json"
    local http_code
    http_code=$(curl -s -o "$resp_file" -w '%{http_code}' \
        -X POST "$API/flocks" \
        -H "Content-Type: application/json" \
        -d "$body" || true)

    case "$http_code" in
        200|201) ;;
        *)
            printf "\n✗ POST /flocks failed (HTTP %s).\n  Body:\n%s\n  Daemon log tail:\n%s\n" \
                "$http_code" \
                "$(cat "$resp_file" 2>/dev/null | sed 's/^/    /')" \
                "$(tail -30 "${RUNDIR}/daemon.log" | sed 's/^/    /')" >&2
            exit 1
            ;;
    esac

    FLOCK_ID=$(jq -r '.flock_id' "$resp_file")
    [ -n "$FLOCK_ID" ] && [ "$FLOCK_ID" != "null" ] || fatal "flock_id missing in response"
    ok "Flock spawned: ${FLOCK_ID}"

    local orch_id
    orch_id=$(jq -r '.agents[] | select(.role=="orchestrator") | .agent_id' "$resp_file")
    ORCH_VM_ID=$(jq -r '.agents[] | select(.role=="orchestrator") | .vm_id' "$resp_file")
    ORCH_URL="${API}/vms/${ORCH_VM_ID}"

    for var in ORCH_VM_ID ORCH_URL; do
        [ -n "${!var}" ] && [ "${!var}" != "null" ] || fatal "Missing $var in flock response."
    done
    ok "Orchestrator: ${ORCH_URL}  id=${orch_id}  vm=${ORCH_VM_ID}"

    # Surface worker + reviewer agent_id so the user can correlate Town Wall
    # entries with peers. The orchestrator references them as worker-1 / reviewer-1
    # in its system prompt; the actual agent_id format is role-<index>.
    note "Worker agent_id:   $(jq -r '.agents[] | select(.role=="worker") | .agent_id' "$resp_file")"
    note "Reviewer agent_id: $(jq -r '.agents[] | select(.role=="reviewer") | .agent_id' "$resp_file")"
}

# ── 5. Materialize vite-template into the site dir ───────────────
prepare_site_dir() {
    step "5. Materialize vite-template into runtime site/"
    local site_dir="${RUNDIR}/site"
    cp -R "${TEMPLATE_DIR}/"* "${site_dir}/"
    cp "${TEMPLATE_DIR}/.gitignore" "${site_dir}/.gitignore" 2>/dev/null || true
    ok "vite-template materialized at ${site_dir} (orchestrator output will overwrite)"
}

# ── 6. Subscribe to the Town Wall and harvest <<<FILE:>>> sentinels ─
# Runs in the background. Exits when <<<DONE>>> is observed, on SIGTERM,
# or when curl hits its timeout cap. The step heading is printed by main()
# instead so the user sees the progress in the foreground.
harvest_townwall() {
    local site_dir="${RUNDIR}/site"
    local sse_log="${RUNDIR}/townwall/harvest.log"
    local site_dir_env="$site_dir" sse_log_env="$sse_log"
    export site_dir_env sse_log_env

    # SSE stream is unbounded; cap with ORCH_TIMEOUT + 60s grace so the
    # harvester does not outlive the orchestrator's worst case.
    local cap=$((ORCH_TIMEOUT + 60))

    # python3 receives the event stream on stdin via process substitution for
    # its script body. Heredoc-on-stdin would shadow the stream, which is the
    # exact antipattern that bit the v0.3.5 webdev_demo cycle.
    curl -sN -m "$cap" "${API}/flocks/${FLOCK_ID}/wall" 2>>"$sse_log" \
        | python3 <(cat <<'PY'
import json, os, re, sys

site_dir = os.environ["site_dir_env"]
log = open(os.environ["sse_log_env"], "a")

# Each SSE event is exactly "data: <json>\n\n" — we only care about lines
# beginning with "data: ".
FILE_RE = re.compile(r"<<<FILE:\s*([^>]+)>>>\n(.*?)\n<<<END>>>", re.DOTALL)

saved = set()
done = False
for raw in sys.stdin:
    line = raw.rstrip("\n")
    if not line.startswith("data: "):
        continue
    try:
        evt = json.loads(line[6:])
    except json.JSONDecodeError:
        continue
    body = evt.get("body", "")
    agent = evt.get("agent_id", "?")
    log.write(f"[{agent}] {body[:120].replace(chr(10), ' ')}\n")
    log.flush()
    if "<<<DONE>>>" in body:
        sys.stdout.write("DONE\n")
        sys.stdout.flush()
        done = True
        break
    m = FILE_RE.search(body)
    if not m:
        continue
    path = m.group(1).strip()
    if path in saved:
        continue
    content = m.group(2)
    target = os.path.join(site_dir, path)
    os.makedirs(os.path.dirname(target), exist_ok=True)
    with open(target, "w") as f:
        f.write(content)
        if not content.endswith("\n"):
            f.write("\n")
    saved.add(path)
    sys.stdout.write(f"SAVED {path} ({len(content)} bytes)\n")
    sys.stdout.flush()

log.write(f"--- harvest end (done={done}, files={len(saved)}) ---\n")
log.close()
sys.exit(0 if done else 2)
PY
)
}

# ── 7. Dispatch the brief to the orchestrator ────────────────────
dispatch_orchestrator() {
    step "7. Dispatch the website brief to the orchestrator"

    local req_file="${RUNDIR}/orchestrator-task.req.json"
    local resp_file="${RUNDIR}/orchestrator-task.resp.json"
    jq -n --arg p "$TASK" '{prompt: $p}' > "$req_file"

    # Groq + Llama tool calling is intermittently flaky: the model sometimes emits a
    # non-standard tool-call format Groq rejects ("Failed to call a function"), which
    # ends the orchestrator's operating loop on its very first command. The
    # orchestrator runs as a stateless one-shot (no session) and the Town Wall
    # harvester ignores duplicate <<<FILE:>>> paths, so re-dispatching the whole brief
    # is safe. Retry a few times; a clean run ends by posting <<<DONE>>>.
    local max_attempts="${WEBDEV_ORCH_ATTEMPTS:-4}"
    local attempt=1 t0 http_code elapsed err_summary
    while [ "$attempt" -le "$max_attempts" ]; do
        note "Brief → ${ORCH_URL}/tasks (attempt ${attempt}/${max_attempts}, timeout ${ORCH_TIMEOUT}s)"
        t0=$(date +%s)
        http_code=$(curl -s -o "$resp_file" -w '%{http_code}' \
            --max-time "$ORCH_TIMEOUT" \
            -X POST "${ORCH_URL}/tasks" \
            -H "Content-Type: application/json" \
            -d @"$req_file" || echo "000")
        elapsed=$(( $(date +%s) - t0 ))

        # Success = HTTP 200 and the agent did NOT return a recoverable-error report.
        # goose prefixes those with "Ran into this error:" — this covers Groq's
        # "Failed to call a function" (Llama tool-call format) AND "reasoning_content
        # is unsupported" (gpt-oss multi-turn). The real completion signal is
        # <<<DONE>>> on the Town Wall (the harvester watches it); this is the fast-fail
        # gate so a broken run doesn't burn the whole timeout before retrying.
        if [ "$http_code" = "200" ] && ! grep -q "Ran into this error" "$resp_file" 2>/dev/null; then
            ok "Orchestrator returned cleanly in ${elapsed}s (attempt ${attempt}; response: ${resp_file})"
            return
        fi

        err_summary=$(grep -o "Ran into this error[^\"]*" "$resp_file" 2>/dev/null | head -1 | cut -c1-180)
        if [ -n "$err_summary" ]; then
            note "Attempt ${attempt} (${elapsed}s): ${err_summary} — retrying"
        else
            note "Attempt ${attempt}: orchestrator HTTP ${http_code} after ${elapsed}s — retrying"
        fi
        attempt=$((attempt + 1))
        [ "$attempt" -le "$max_attempts" ] && sleep 2
    done

    fail "Orchestrator did not complete after ${max_attempts} attempts. Last error:"
    note "  ${err_summary:-(HTTP ${http_code}; see ${resp_file})}"
    note "Adjust the orchestrator model/provider in configs/webdev-demo/profiles/orchestrator/goose.yaml,"
    note "or override with WEBDEV_ORCH_MODEL. The harvester may have caught partial files."
}

# ── 8. Build the React site on the host ──────────────────────────
build_site() {
    step "8. Build the site (npm install + vite build)"

    local site_dir="${RUNDIR}/site"
    note "Files in site/ (post-harvest):"
    find "$site_dir" -type f -not -path '*/node_modules/*' -not -path '*/dist/*' \
        | sort | sed "s|${site_dir}/|      |"

    # Surface a partial run: unfilled template files still carry the PLACEHOLDER
    # marker. The site still builds (so the result is viewable), but flag that the
    # agent did not publish everything.
    local placeholder_files
    placeholder_files=$(grep -rl "PLACEHOLDER" "$site_dir/src" "$site_dir/index.html" 2>/dev/null || true)
    if [ -n "$placeholder_files" ]; then
        fail "Partial run — these files were never published by the agent (still PLACEHOLDER):"
        printf '%s\n' "$placeholder_files" | sed "s|${site_dir}/|      |"
        note "See ${RUNDIR}/townwall/harvest.log for what the orchestrator actually published."
    fi

    (
        cd "$site_dir" || exit 1
        npm install --silent --no-audit --no-fund 2>&1 | tail -5
    ) || fatal "npm install failed in ${site_dir}. See npm output above."
    ok "npm install complete"

    (cd "$site_dir" && npm run build 2>&1 | tail -8) || \
        fatal "vite build failed. Check ${site_dir}/src/*.jsx for syntax errors and re-run."
    ok "vite build produced ${site_dir}/dist/"
}

# ── 9. Serve with vite preview ───────────────────────────────────
serve_preview() {
    step "9. Start vite preview on port ${PREVIEW_PORT}"

    local site_dir="${RUNDIR}/site"
    local preview_log="${RUNDIR}/vite-preview.log"
    (
        cd "$site_dir"
        npx vite preview --port "$PREVIEW_PORT" --host 127.0.0.1 >"$preview_log" 2>&1 &
        echo $! > "${RUNDIR}/preview.pid"
    )
    PREVIEW_PID=$(cat "${RUNDIR}/preview.pid")
    sleep 2
    if ! kill -0 "$PREVIEW_PID" 2>/dev/null; then
        fatal "vite preview died on start. Log:\n$(tail -30 "$preview_log")"
    fi
    ok "vite preview PID=$PREVIEW_PID  log=$preview_log"
}

# ── 10. Banner ───────────────────────────────────────────────────
banner() {
    cat <<EOF

━━━ Demo is live ━━━

  Website:       http://localhost:${PREVIEW_PORT}
  Town Wall:     ${API}/flocks/${FLOCK_ID}/wall  (SSE)
  Wall history:  ${API}/flocks/${FLOCK_ID}/wall/history
  Daemon log:    ${RUNDIR}/daemon.log
  Orchestrator:  ${RUNDIR}/orchestrator-task.resp.json
  SSE/parser:    ${RUNDIR}/townwall/harvest.log
  Site source:   ${RUNDIR}/site/

  Press Ctrl-C to tear down the flock, daemon, and preview server.

EOF
}

# ── Entry point ──────────────────────────────────────────────────
main() {
    preflight
    install_profiles
    start_daemon
    create_flock
    prepare_site_dir

    # Start the Town Wall harvester BEFORE dispatching the orchestrator so
    # the very first <<<FILE:>>> post is captured. The harvester exits when
    # it sees <<<DONE>>>, on SIGTERM from the grace block below, or when
    # curl hits its cap.
    step "6. Subscribe to Town Wall SSE and harvest file sentinels"
    note "(harvester runs in background; logs at ${RUNDIR}/townwall/)"
    harvest_townwall > "${RUNDIR}/townwall/harvest.stdout" 2>&1 &
    HARVEST_PID=$!
    sleep 1   # let curl establish the SSE subscription before the orchestrator starts posting

    dispatch_orchestrator

    # Grace window for trailing <<<DONE>>> post-orchestrator-return, then
    # tear the harvester down so we do not block on the 960s curl cap when
    # the orchestrator already finished (e.g. quota-hit, early exit).
    note "Granting harvester 30s to catch a trailing <<<DONE>>>..."
    local grace=0
    while [ $grace -lt 30 ] && kill -0 "$HARVEST_PID" 2>/dev/null; do
        sleep 1; grace=$((grace + 1))
    done
    if kill -0 "$HARVEST_PID" 2>/dev/null; then
        note "Grace expired; signalling harvester to stop (no <<<DONE>>> seen)."
        kill -TERM "$HARVEST_PID" 2>/dev/null || true
    fi
    wait "$HARVEST_PID" || note "(harvester returned non-zero; check ${RUNDIR}/townwall/harvest.log)"
    HARVEST_PID=""

    build_site
    serve_preview
    banner

    # Idle wait — Ctrl-C triggers cleanup.
    while true; do sleep 60; done
}

main "$@"
