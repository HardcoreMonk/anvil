#!/usr/bin/env bash
# observability_demo.sh — end-to-end demo of the v0.3.5 Observability Trio.
#
# Spins up the ephemera control plane, downloads + launches Prometheus and
# Grafana (with provisioned dashboard), then runs an automatic workload that
# exercises every metric family. The daemon, Prometheus, and Grafana remain
# running until the user hits Ctrl-C, so the browser-based demo is interactive.
#
# Usage:  sudo bash observability_demo.sh
#
# Browser URLs after start-up:
#   http://localhost:3000   ephemera daemon API + /metrics
#   http://localhost:9090   Prometheus query browser
#   http://localhost:3001   Grafana (admin / admin) → Dashboards → "Ephemera Overview"

set -u
set -o pipefail

# ── Constants ────────────────────────────────────────────────────
PROMETHEUS_VERSION="2.51.2"
PROMETHEUS_SHA256="9bec7432fb92d80fdc193a0154f6c53653c37f8302528b06d63cf4a10a8b897f"
PROMETHEUS_TARBALL="prometheus-${PROMETHEUS_VERSION}.linux-amd64.tar.gz"
PROMETHEUS_URL="https://github.com/prometheus/prometheus/releases/download/v${PROMETHEUS_VERSION}/${PROMETHEUS_TARBALL}"

GRAFANA_VERSION="10.4.2"
GRAFANA_SHA256="b12b55d4ea266fa298395c82d5f8372f544b386efab28e9d96ebc887aef37560"
GRAFANA_TARBALL="grafana-${GRAFANA_VERSION}.linux-amd64.tar.gz"
GRAFANA_URL="https://dl.grafana.com/oss/release/${GRAFANA_TARBALL}"

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ARTIFACTS_DIR="${REPO_ROOT}/artifacts"
# These directory names are dictated by the upstream tarball layout:
#   prometheus-X.Y.Z.linux-amd64.tar.gz → prometheus-X.Y.Z.linux-amd64/prometheus
#   grafana-X.Y.Z.linux-amd64.tar.gz   → grafana-vX.Y.Z/bin/grafana
PROM_DIR="${ARTIFACTS_DIR}/prometheus-${PROMETHEUS_VERSION}.linux-amd64"
GRAFANA_DIR="${ARTIFACTS_DIR}/grafana-v${GRAFANA_VERSION}"
CONFIGS_DIR="${REPO_ROOT}/configs/observability"

API="http://localhost:3000"
PROM_PORT=9090
GRAFANA_PORT=3001

# Per-run runtime directory (TSDB, Grafana data dir, logs).
RUNDIR=""
DAEMON_PID=""
PROM_PID=""
GRAFANA_PID=""
WORKLOAD_PID=""
DEMO_OK=true

# ── Output helpers ───────────────────────────────────────────────
step()  { printf "\n━━━ %s ━━━\n" "$*"; }
ok()    { printf "  ✓ %s\n" "$*"; }
fail()  { printf "  ✗ %s\n" "$*"; DEMO_OK=false; }
note()  { printf "    %s\n" "$*"; }
fatal() { printf "\n✗ %s\n" "$*" >&2; exit 1; }

# ── Cleanup trap (always last to register) ───────────────────────
cleanup() {
    local rc=$?
    trap - INT TERM EXIT
    printf "\n━━━ Shutting down demo ━━━\n"

    # Stop workload first so it doesn't race with daemon shutdown.
    if [ -n "$WORKLOAD_PID" ] && kill -0 "$WORKLOAD_PID" 2>/dev/null; then
        kill -TERM "$WORKLOAD_PID" 2>/dev/null || true
        wait "$WORKLOAD_PID" 2>/dev/null || true
    fi

    for pid_var in GRAFANA_PID PROM_PID DAEMON_PID; do
        local pid="${!pid_var}"
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill -TERM "$pid" 2>/dev/null || true
        fi
    done

    # Give each process up to 8s to exit gracefully (daemon does VM teardown).
    local waited=0
    while [ $waited -lt 8 ]; do
        local any_alive=false
        for pid_var in GRAFANA_PID PROM_PID DAEMON_PID; do
            local pid="${!pid_var}"
            if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                any_alive=true
                break
            fi
        done
        $any_alive || break
        sleep 1
        waited=$((waited + 1))
    done

    # Force-kill survivors.
    for pid_var in GRAFANA_PID PROM_PID DAEMON_PID; do
        local pid="${!pid_var}"
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill -KILL "$pid" 2>/dev/null || true
        fi
    done

    if [ -n "$RUNDIR" ] && [ -d "$RUNDIR" ]; then
        rm -rf "$RUNDIR" 2>/dev/null || true
    fi

    if $DEMO_OK; then
        ok "Demo shut down cleanly."
    else
        echo "  ✗ Demo exited with a failure earlier — see messages above."
        exit 1
    fi
    exit "$rc"
}
trap cleanup INT TERM EXIT

# ── 1. Preflight ─────────────────────────────────────────────────
preflight() {
    step "1. Preflight"

    [ "$(id -u)" -eq 0 ] || fatal "Run as root (sudo bash observability_demo.sh) — KVM + network setup require it."
    [ -r /dev/kvm ] || fatal "/dev/kvm not accessible. Enable virtualization in BIOS / nested virt."
    [ -w /dev/kvm ] || fatal "/dev/kvm is read-only for the daemon — check group membership."

    for tool in curl jq tar sha256sum ss; do
        command -v "$tool" >/dev/null || fatal "Missing required tool: $tool"
    done
    ok "Required tools present"

    # Repo state.
    [ -x "${REPO_ROOT}/ephemera-daemon" ] || {
        note "ephemera-daemon binary missing; building..."
        (cd "$REPO_ROOT" && go build -o ephemera-daemon ./cmd/goose-daemon) || fatal "go build failed"
    }
    ok "ephemera-daemon binary ready"

    # Port availability.
    for port in 3000 "$PROM_PORT" "$GRAFANA_PORT"; do
        if ss -tlnH "sport = :$port" 2>/dev/null | grep -q LISTEN; then
            fatal "Port $port is already in use. Free it and retry."
        fi
    done
    ok "Ports 3000 / $PROM_PORT / $GRAFANA_PORT are free"

    # Required config bundle.
    [ -f "${CONFIGS_DIR}/prometheus.yml" ] || fatal "Missing ${CONFIGS_DIR}/prometheus.yml"
    [ -f "${CONFIGS_DIR}/grafana-datasource.yml" ] || fatal "Missing ${CONFIGS_DIR}/grafana-datasource.yml"
    [ -f "${CONFIGS_DIR}/grafana-dashboards.yml" ] || fatal "Missing ${CONFIGS_DIR}/grafana-dashboards.yml"
    [ -f "${CONFIGS_DIR}/dashboards/ephemera-overview.json" ] || fatal "Missing dashboard JSON"
    ok "Provisioning config bundle present"

    RUNDIR=$(mktemp -d /tmp/observability-demo-XXXXXX)
    ok "Runtime directory: $RUNDIR"
}

# ── 2. Download Prometheus & Grafana (cached) ────────────────────
ensure_artifact() {
    # ensure_artifact <name> <version-dir> <tarball> <url> <sha256> <bin-relpath>
    local name="$1" version_dir="$2" tarball="$3" url="$4" sha256="$5" bin_relpath="$6"
    local bin_path="${version_dir}/${bin_relpath}"

    if [ -x "$bin_path" ]; then
        ok "${name} cached at ${bin_path}"
        return 0
    fi

    note "Downloading ${name} ${tarball}..."
    mkdir -p "$ARTIFACTS_DIR"
    local tmp_tarball="${ARTIFACTS_DIR}/${tarball}.partial"
    curl -fL --retry 3 --retry-delay 2 -o "$tmp_tarball" "$url" || {
        rm -f "$tmp_tarball"
        fatal "${name} download failed from ${url}"
    }

    local actual_sha
    actual_sha=$(sha256sum "$tmp_tarball" | awk '{print $1}')
    if [ "$actual_sha" != "$sha256" ]; then
        rm -f "$tmp_tarball"
        fatal "${name} SHA256 mismatch (want $sha256, got $actual_sha)"
    fi

    mv "$tmp_tarball" "${ARTIFACTS_DIR}/${tarball}"
    tar -xzf "${ARTIFACTS_DIR}/${tarball}" -C "$ARTIFACTS_DIR" || fatal "extract ${name} failed"
    rm -f "${ARTIFACTS_DIR}/${tarball}"

    [ -x "$bin_path" ] || fatal "${name} binary missing after extract: $bin_path"
    ok "${name} installed at ${bin_path}"
}

download_stage() {
    step "2. Ensure Prometheus & Grafana"
    ensure_artifact "Prometheus" "$PROM_DIR" "$PROMETHEUS_TARBALL" \
        "$PROMETHEUS_URL" "$PROMETHEUS_SHA256" "prometheus"
    ensure_artifact "Grafana" "$GRAFANA_DIR" "$GRAFANA_TARBALL" \
        "$GRAFANA_URL" "$GRAFANA_SHA256" "bin/grafana"
}

# ── 3. Materialize provisioning bundle into RUNDIR ───────────────
provision_stage() {
    step "3. Provision Grafana datasource + dashboard"

    local prov_ds_dir="${RUNDIR}/grafana-provisioning/datasources"
    local prov_dash_dir="${RUNDIR}/grafana-provisioning/dashboards"
    local dashboards_dir="${RUNDIR}/dashboards-ephemera"
    mkdir -p "$prov_ds_dir" "$prov_dash_dir" "$dashboards_dir" "${RUNDIR}/prom" "${RUNDIR}/grafana-data" "${RUNDIR}/grafana-logs"

    cp "${CONFIGS_DIR}/grafana-datasource.yml" "${prov_ds_dir}/datasource.yml"

    # Rewrite the dashboards-provider path to the runtime dir.
    sed -e "s|/var/lib/grafana/dashboards-ephemera|${dashboards_dir}|" \
        "${CONFIGS_DIR}/grafana-dashboards.yml" > "${prov_dash_dir}/dashboards.yml"

    cp "${CONFIGS_DIR}/dashboards/ephemera-overview.json" "${dashboards_dir}/ephemera-overview.json"
    ok "Provisioning bundle materialized at ${RUNDIR}/grafana-provisioning/"
}

# ── 4. Start ephemera daemon ─────────────────────────────────────
start_daemon() {
    step "4. Start ephemera daemon"

    # Demo-only token; written as a file so SIGHUP token-rotation step works
    # without permanently mutating the operator's environment.
    local tokens_file="${RUNDIR}/ephemera-tokens.txt"
    echo "demo:demo-token-v035" > "$tokens_file"
    chmod 0600 "$tokens_file"

    local log="${RUNDIR}/daemon.log"
    (
        cd "$REPO_ROOT"
        EPHEMERA_API_ADDR=0.0.0.0:3000 \
        EPHEMERA_API_TOKENS_FILE="$tokens_file" \
        EPHEMERA_LOG_FORMAT=json \
        EPHEMERA_LOG_LEVEL=info \
        EPHEMERA_GRAFANA_URL="http://localhost:${GRAFANA_PORT}" \
        ./ephemera-daemon >"$log" 2>&1 &
        echo $! > "${RUNDIR}/daemon.pid"
    )
    DAEMON_PID=$(cat "${RUNDIR}/daemon.pid")
    ok "ephemera-daemon PID=$DAEMON_PID  log=$log"

    note "Waiting for daemon API (up to 600s; first run may bake the golden image)..."
    local waited=0
    until curl -sf -o /dev/null --max-time 2 \
            -H "Authorization: Bearer demo-token-v035" "$API/vms"; do
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
    ok "Daemon API ready at $API (auth=demo:demo-token-v035)"
}

# ── 5. Start Prometheus ──────────────────────────────────────────
start_prometheus() {
    step "5. Start Prometheus"

    local log="${RUNDIR}/prometheus.log"
    "${PROM_DIR}/prometheus" \
        --config.file="${CONFIGS_DIR}/prometheus.yml" \
        --storage.tsdb.path="${RUNDIR}/prom" \
        --web.listen-address="0.0.0.0:${PROM_PORT}" \
        --web.enable-lifecycle \
        >"$log" 2>&1 &
    PROM_PID=$!
    ok "Prometheus PID=$PROM_PID  log=$log"

    local waited=0
    until curl -sf -o /dev/null --max-time 2 "http://localhost:${PROM_PORT}/-/healthy"; do
        if ! kill -0 "$PROM_PID" 2>/dev/null; then
            fatal "Prometheus exited prematurely. Tail of $log:\n$(tail -30 "$log")"
        fi
        sleep 1
        waited=$((waited + 1))
        [ $waited -ge 30 ] && fatal "Prometheus did not become healthy within 30s"
    done
    ok "Prometheus healthy at http://localhost:${PROM_PORT}"
}

# ── 6. Start Grafana ─────────────────────────────────────────────
start_grafana() {
    step "6. Start Grafana"

    local log="${RUNDIR}/grafana.log"
    (
        cd "${GRAFANA_DIR}"
        GF_DEFAULT_INSTANCE_NAME="ephemera-demo" \
        GF_PATHS_DATA="${RUNDIR}/grafana-data" \
        GF_PATHS_LOGS="${RUNDIR}/grafana-logs" \
        GF_PATHS_PROVISIONING="${RUNDIR}/grafana-provisioning" \
        GF_SERVER_HTTP_PORT="${GRAFANA_PORT}" \
        GF_LOG_MODE="file" \
        GF_ANALYTICS_REPORTING_ENABLED="false" \
        GF_ANALYTICS_CHECK_FOR_UPDATES="false" \
        GF_SECURITY_ALLOW_EMBEDDING="true" \
        GF_AUTH_ANONYMOUS_ENABLED="true" \
        GF_AUTH_ANONYMOUS_ORG_ROLE="Viewer" \
        ./bin/grafana server \
            --homepath="${GRAFANA_DIR}" \
            --config="${GRAFANA_DIR}/conf/defaults.ini" \
            >"$log" 2>&1 &
        echo $! > "${RUNDIR}/grafana.pid"
    )
    GRAFANA_PID=$(cat "${RUNDIR}/grafana.pid")
    ok "Grafana PID=$GRAFANA_PID  log=$log"

    local waited=0
    until curl -sf -o /dev/null --max-time 2 "http://localhost:${GRAFANA_PORT}/api/health"; do
        if ! kill -0 "$GRAFANA_PID" 2>/dev/null; then
            fatal "Grafana exited prematurely. Tail of $log:\n$(tail -40 "$log")"
        fi
        sleep 1
        waited=$((waited + 1))
        [ $waited -ge 45 ] && fatal "Grafana did not become healthy within 45s"
    done
    ok "Grafana healthy at http://localhost:${GRAFANA_PORT}"
}

# ── 7. Workload: exercise every metric family ────────────────────
TOKEN_HEADER='Authorization: Bearer demo-token-v035'

api_curl() {
    # Wrapper that always includes the demo auth header.
    curl -sS --max-time 30 -H "$TOKEN_HEADER" "$@"
}

run_workload() {
    step "7. Run workload (background)"

    (
        # All errors here are intentionally non-fatal — the demo continues even
        # if a single workload step has a transient hiccup.
        sleep 5

        # 7a. VM single-cycle, 3 times.
        for i in 1 2 3; do
            local resp
            resp=$(api_curl -X POST -H 'Content-Type: application/json' -d '{}' "$API/vms" || true)
            local vmid
            vmid=$(echo "$resp" | jq -r '.vm_id // empty')
            if [ -n "$vmid" ]; then
                sleep 4
                api_curl -X DELETE "$API/vms/$vmid" >/dev/null || true
            fi
            sleep 3
        done

        # 7b. Snapshot family (full → diff → cleanup).
        local snap_resp snap_vm snap_full snap_diff
        snap_resp=$(api_curl -X POST -H 'Content-Type: application/json' -d '{}' "$API/vms" || true)
        snap_vm=$(echo "$snap_resp" | jq -r '.vm_id // empty')
        if [ -n "$snap_vm" ]; then
            sleep 4
            snap_full=$(api_curl -X POST -H 'Content-Type: application/json' -d '{"type":"full"}' \
                "$API/vms/$snap_vm/snapshot" | jq -r '.snapshot_id // empty')
            sleep 2
            snap_diff=$(api_curl -X POST -H 'Content-Type: application/json' -d '{"type":"diff"}' \
                "$API/vms/$snap_vm/snapshot" | jq -r '.snapshot_id // empty')
            sleep 2
            api_curl -X DELETE "$API/vms/$snap_vm" >/dev/null || true
            [ -n "$snap_diff" ] && api_curl -X DELETE "$API/snapshots/$snap_diff" >/dev/null || true
            [ -n "$snap_full" ] && api_curl -X DELETE "$API/snapshots/$snap_full" >/dev/null || true
        fi

        # 7c. Flock cycle.
        local flock_resp flock_id
        flock_resp=$(api_curl -X POST -H 'Content-Type: application/json' \
            -d '{"task":"demo","roles":["orchestrator","worker","reviewer","researcher","researcher"]}' \
            "$API/flocks" || true)
        flock_id=$(echo "$flock_resp" | jq -r '.flock_id // empty')
        if [ -n "$flock_id" ]; then
            sleep 20
            api_curl -X DELETE "$API/flocks/$flock_id" >/dev/null || true
        fi

        # 7d. SIGHUP token reload (rewrite token file, signal daemon).
        sleep 2
        echo "demo:demo-token-v035" > "${RUNDIR}/ephemera-tokens.txt"
        chmod 0600 "${RUNDIR}/ephemera-tokens.txt"
        kill -HUP "$DAEMON_PID" 2>/dev/null || true
    ) &
    WORKLOAD_PID=$!
    ok "Workload running (PID $WORKLOAD_PID). It will finish in ~2 min."
}

# ── 8. User banner + Ctrl-C wait ─────────────────────────────────
print_banner() {
    cat <<EOF

═══════════════════════════════════════════════════════════════════
  🎯 Ephemera observability demo is running.

  Daemon       http://localhost:3000   (API + /metrics)
  Prometheus   http://localhost:${PROM_PORT}   (query browser)
  Grafana      http://localhost:${GRAFANA_PORT}   (admin / admin)

  Web UI:      http://localhost:3000/ui/  →  System → Monitoring  (embedded Grafana)
  Dashboard:   Grafana → Dashboards → Ephemera Overview

  Try in Prometheus (or the dashboard's panels):
    rate(ephemera_vm_spawn_total[1m])
    histogram_quantile(0.95, sum(rate(ephemera_vm_spawn_duration_seconds_bucket[5m])) by (le))
    ephemera_vm_count
    ephemera_sighup_reload_total

  Workload is generating metrics in the background; allow ~2 min
  for spawn / snapshot / flock / sighup to all show up.

  Press Ctrl-C to shut down the demo (daemon + Prometheus + Grafana).
═══════════════════════════════════════════════════════════════════

EOF

    # Wait indefinitely; cleanup trap fires on Ctrl-C.
    while true; do
        # Bail out if any critical component dies.
        for pid_var in DAEMON_PID PROM_PID GRAFANA_PID; do
            local pid="${!pid_var}"
            if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
                fail "${pid_var} (PID $pid) exited unexpectedly. See ${RUNDIR}/*.log"
                return
            fi
        done
        sleep 5
    done
}

# ── Main ────────────────────────────────────────────────────────
preflight
download_stage
provision_stage
start_daemon
start_prometheus
start_grafana
run_workload
print_banner
