#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  cat >&2 <<'USAGE'
Usage: sudo bash scripts/install-anvil-scheduler-systemd.sh [--dry-run] [--no-build] [--no-enable] [--start] [--verify]

Installs the anvil scheduler binary, environment file, and systemd unit.

Environment:
  ANVIL_SCHEDULER_USER=anvil
  ANVIL_SCHEDULER_GROUP=anvil
  ANVIL_SCHEDULER_ADDR=127.0.0.1:3010
  ANVIL_SCHEDULER_STATE=/var/lib/anvil/scheduler.json
  ANVIL_SCHEDULER_QUOTA_STORE=/var/lib/anvil/tenants.json
  ANVIL_SCHEDULER_BIN=/usr/local/bin/anvil-scheduler
  ANVIL_SCHEDULER_ENV=/etc/anvil/anvil-scheduler.env

Resident host-inventory polling (optional; written to the env file only when set):
  ANVIL_SCHEDULER_HOSTS_SRC          hosts JSON to install to ANVIL_SCHEDULER_HOSTS_FILE
  ANVIL_SCHEDULER_HOSTS_FILE         inventory path (default /etc/anvil/scheduler-hosts.json when HOSTS_SRC set)
  ANVIL_SCHEDULER_POLL_INTERVAL      health poll cadence (Go duration, default 10s)
  ANVIL_SCHEDULER_RECONCILE_INTERVAL inventory reconcile cadence (Go duration, default 30s)
  ANVIL_SCHEDULER_HOST_TIMEOUT       per-host request timeout (Go duration, default 3s)
  ANVIL_SCHEDULER_FAILURE_THRESHOLD  polls before a host is marked unhealthy (default 3)
  ANVIL_SCHEDULER_API_TOKEN          bearer token the loop sends to daemon /health,/vms
  ANVIL_SCHEDULER_REQUIRE_PERSISTENCE fail scheduling when state persistence degrades
USAGE
}

DRY_RUN=0
BUILD=1
ENABLE=1
START=0
VERIFY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)
      DRY_RUN=1
      ;;
    --no-build)
      BUILD=0
      ;;
    --no-enable)
      ENABLE=0
      ;;
    --start)
      START=1
      ;;
    --verify)
      VERIFY=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
  shift
done

SVC_USER="${ANVIL_SCHEDULER_USER:-anvil}"
SVC_GROUP="${ANVIL_SCHEDULER_GROUP:-anvil}"
SVC_ADDR="${ANVIL_SCHEDULER_ADDR:-127.0.0.1:3010}"
STATE_PATH="${ANVIL_SCHEDULER_STATE:-/var/lib/anvil/scheduler.json}"
QUOTA_PATH="${ANVIL_SCHEDULER_QUOTA_STORE:-/var/lib/anvil/tenants.json}"
BIN_PATH="${ANVIL_SCHEDULER_BIN:-/usr/local/bin/anvil-scheduler}"
ENV_PATH="${ANVIL_SCHEDULER_ENV:-/etc/anvil/anvil-scheduler.env}"
UNIT_PATH="/etc/systemd/system/anvil-scheduler.service"
ENV_DIR="$(dirname "$ENV_PATH")"

# Resident host-inventory polling knobs. These env vars are already read by
# cmd/anvil-scheduler; the installer only propagates the ones an operator set
# into the systemd env file so the control loop polls real hosts in production.
# HOSTS_SRC (optional) is a hosts JSON that gets installed to HOSTS_FILE so the
# inventory is declarative and survives reboots.
HOSTS_FILE_PATH="${ANVIL_SCHEDULER_HOSTS_FILE:-}"
HOSTS_SRC="${ANVIL_SCHEDULER_HOSTS_SRC:-}"
POLL_INTERVAL="${ANVIL_SCHEDULER_POLL_INTERVAL:-}"
RECONCILE_INTERVAL="${ANVIL_SCHEDULER_RECONCILE_INTERVAL:-}"
HOST_TIMEOUT="${ANVIL_SCHEDULER_HOST_TIMEOUT:-}"
FAILURE_THRESHOLD="${ANVIL_SCHEDULER_FAILURE_THRESHOLD:-}"
API_TOKEN="${ANVIL_SCHEDULER_API_TOKEN:-}"
REQUIRE_PERSISTENCE="${ANVIL_SCHEDULER_REQUIRE_PERSISTENCE:-}"

# A hosts source with no explicit destination lands at the canonical config path.
if [[ -n "$HOSTS_SRC" && -z "$HOSTS_FILE_PATH" ]]; then
  HOSTS_FILE_PATH="/etc/anvil/scheduler-hosts.json"
fi

run() {
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

scheduler_base_url() {
  local addr="$1"
  case "$addr" in
    http://*|https://*)
      printf '%s\n' "$addr"
      ;;
    0.0.0.0:*)
      printf 'http://127.0.0.1%s\n' "${addr#0.0.0.0}"
      ;;
    :*)
      printf 'http://127.0.0.1%s\n' "$addr"
      ;;
    *)
      if [[ "$addr" == "[::]:"* ]]; then
        printf 'http://127.0.0.1:%s\n' "${addr##*:}"
      else
        printf 'http://%s\n' "$addr"
      fi
      ;;
  esac
}

require_absolute_path() {
  local name="$1"
  local path="$2"
  case "$path" in
    /*)
      return
      ;;
    *)
      printf 'error: %s must be an absolute path\n' "$name" >&2
      exit 1
      ;;
  esac
}

normalize_absolute_path() {
  local path="$1"
  local part result
  local -a parts normalized

  IFS='/' read -r -a parts <<<"$path"
  for part in "${parts[@]}"; do
    case "$part" in
      ''|.)
        ;;
      ..)
        if ((${#normalized[@]} > 0)); then
          unset "normalized[$((${#normalized[@]} - 1))]"
        fi
        ;;
      *)
        normalized+=("$part")
        ;;
    esac
  done

  result=""
  for part in "${normalized[@]}"; do
    result="${result}/${part}"
  done
  printf '%s\n' "${result:-/}"
}

warn_if_outside_var_lib() {
  local name="$1"
  local path="$2"
  local normalized_path
  normalized_path="$(normalize_absolute_path "$path")"
  case "$normalized_path" in
    /var/lib/anvil|/var/lib/anvil/*)
      return
      ;;
    *)
      printf 'warning: %s is outside /var/lib/anvil; deploy/systemd/anvil-scheduler.service has ReadWritePaths=/var/lib/anvil\n' "$name" >&2
      ;;
  esac
}

require_verify_tools() {
  if [[ "$VERIFY" != "1" || "$DRY_RUN" == "1" ]]; then
    return
  fi
  if ! command -v curl >/dev/null 2>&1; then
    printf 'error: --verify requires curl\n' >&2
    exit 1
  fi
  if ! command -v python3 >/dev/null 2>&1; then
    printf 'error: --verify requires python3\n' >&2
    exit 1
  fi
}

run_scheduler_verify() {
  local base_url
  base_url="$(scheduler_base_url "$SVC_ADDR")"
  run bash scripts/anvil-scheduler-smoke.sh --base-url "$base_url"
}

# render_env_file writes the systemd EnvironmentFile body to stdout. The three
# base vars are always present; the resident-polling knobs are emitted only when
# an operator set them, so an unconfigured install keeps its historical shape.
# When redact=1 the API token value is masked (dry-run preview only) — the real
# token is only ever written to the 0640 root:group env file.
render_env_file() {
  local redact="${1:-0}"
  printf 'ANVIL_SCHEDULER_ADDR=%s\n' "$SVC_ADDR"
  printf 'ANVIL_SCHEDULER_STATE=%s\n' "$STATE_PATH"
  printf 'ANVIL_SCHEDULER_QUOTA_STORE=%s\n' "$QUOTA_PATH"
  [[ -n "$HOSTS_FILE_PATH" ]] && printf 'ANVIL_SCHEDULER_HOSTS_FILE=%s\n' "$HOSTS_FILE_PATH"
  [[ -n "$POLL_INTERVAL" ]] && printf 'ANVIL_SCHEDULER_POLL_INTERVAL=%s\n' "$POLL_INTERVAL"
  [[ -n "$RECONCILE_INTERVAL" ]] && printf 'ANVIL_SCHEDULER_RECONCILE_INTERVAL=%s\n' "$RECONCILE_INTERVAL"
  [[ -n "$HOST_TIMEOUT" ]] && printf 'ANVIL_SCHEDULER_HOST_TIMEOUT=%s\n' "$HOST_TIMEOUT"
  [[ -n "$FAILURE_THRESHOLD" ]] && printf 'ANVIL_SCHEDULER_FAILURE_THRESHOLD=%s\n' "$FAILURE_THRESHOLD"
  if [[ -n "$API_TOKEN" ]]; then
    if [[ "$redact" == "1" ]]; then
      printf 'ANVIL_SCHEDULER_API_TOKEN=%s\n' "<redacted>"
    else
      printf 'ANVIL_SCHEDULER_API_TOKEN=%s\n' "$API_TOKEN"
    fi
  fi
  [[ -n "$REQUIRE_PERSISTENCE" ]] && printf 'ANVIL_SCHEDULER_REQUIRE_PERSISTENCE=%s\n' "$REQUIRE_PERSISTENCE"
  return 0
}

write_env_file() {
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ write %s\n' "$ENV_PATH"
    render_env_file 1 | sed 's/^/  env| /'
    return
  fi
  local tmp
  tmp="$(mktemp)"
  render_env_file 0 >"$tmp"
  install -m 0640 -o root -g "$SVC_GROUP" "$tmp" "$ENV_PATH"
  rm -f "$tmp"
}

install_hosts_file() {
  if [[ -z "$HOSTS_SRC" ]]; then
    return 0
  fi
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ install -d -m 0755 %q\n' "$(dirname "$HOSTS_FILE_PATH")"
    printf '+ install -m 0640 -o root -g %q %q %q\n' "$SVC_GROUP" "$HOSTS_SRC" "$HOSTS_FILE_PATH"
    return
  fi
  if [[ ! -f "$HOSTS_SRC" ]]; then
    printf 'error: ANVIL_SCHEDULER_HOSTS_SRC not found: %s\n' "$HOSTS_SRC" >&2
    exit 1
  fi
  install -d -m 0755 "$(dirname "$HOSTS_FILE_PATH")"
  install -m 0640 -o root -g "$SVC_GROUP" "$HOSTS_SRC" "$HOSTS_FILE_PATH"
}

require_root() {
  if [[ "$DRY_RUN" == "1" ]]; then
    return
  fi
  if [[ "$(id -u)" != "0" ]]; then
    printf 'error: this installer must run as root; use --dry-run for preview\n' >&2
    exit 1
  fi
}

require_absolute_path "ANVIL_SCHEDULER_STATE" "$STATE_PATH"
require_absolute_path "ANVIL_SCHEDULER_QUOTA_STORE" "$QUOTA_PATH"
if [[ -n "$HOSTS_FILE_PATH" ]]; then
  require_absolute_path "ANVIL_SCHEDULER_HOSTS_FILE" "$HOSTS_FILE_PATH"
fi
STATE_DIR="$(dirname "$STATE_PATH")"
QUOTA_DIR="$(dirname "$QUOTA_PATH")"
warn_if_outside_var_lib "ANVIL_SCHEDULER_STATE" "$STATE_PATH"
warn_if_outside_var_lib "ANVIL_SCHEDULER_QUOTA_STORE" "$QUOTA_PATH"
require_verify_tools

require_root

if [[ "$BUILD" == "1" ]]; then
  run go build -o anvil-scheduler ./cmd/anvil-scheduler
fi

if [[ "$DRY_RUN" == "0" && ! -x ./anvil-scheduler ]]; then
  printf 'error: ./anvil-scheduler is missing; run without --no-build or build it first\n' >&2
  exit 1
fi

if ! getent group "$SVC_GROUP" >/dev/null 2>&1; then
  run groupadd --system "$SVC_GROUP"
fi
if ! id -u "$SVC_USER" >/dev/null 2>&1; then
  run useradd --system --home-dir /var/lib/anvil --shell /usr/sbin/nologin --gid "$SVC_GROUP" "$SVC_USER"
fi

run install -d -m 0755 "$(dirname "$BIN_PATH")"
run install -m 0755 ./anvil-scheduler "$BIN_PATH"
run install -d -m 0750 -o "$SVC_USER" -g "$SVC_GROUP" "$STATE_DIR"
if [[ "$QUOTA_DIR" != "$STATE_DIR" ]]; then
  run install -d -m 0750 -o "$SVC_USER" -g "$SVC_GROUP" "$QUOTA_DIR"
fi
run install -d -m 0755 "$ENV_DIR"
write_env_file
install_hosts_file
run install -m 0644 deploy/systemd/anvil-scheduler.service "$UNIT_PATH"
run systemctl daemon-reload

if [[ "$ENABLE" == "1" ]]; then
  run systemctl enable anvil-scheduler.service
fi
if [[ "$START" == "1" ]]; then
  run systemctl restart anvil-scheduler.service
  run systemctl --no-pager --full status anvil-scheduler.service
fi
if [[ "$VERIFY" == "1" ]]; then
  run_scheduler_verify
fi

printf 'anvil scheduler systemd installation prepared.\n'
printf 'env: %s\nunit: %s\nbinary: %s\n' "$ENV_PATH" "$UNIT_PATH" "$BIN_PATH"
