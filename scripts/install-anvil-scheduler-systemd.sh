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

write_env_file() {
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ write %s\n' "$ENV_PATH"
    return
  fi
  local tmp
  tmp="$(mktemp)"
  {
    printf 'ANVIL_SCHEDULER_ADDR=%s\n' "$SVC_ADDR"
    printf 'ANVIL_SCHEDULER_STATE=%s\n' "$STATE_PATH"
    printf 'ANVIL_SCHEDULER_QUOTA_STORE=%s\n' "$QUOTA_PATH"
  } >"$tmp"
  install -m 0640 -o root -g "$SVC_GROUP" "$tmp" "$ENV_PATH"
  rm -f "$tmp"
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
