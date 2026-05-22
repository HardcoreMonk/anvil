#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  cat >&2 <<'USAGE'
Usage: sudo bash scripts/install-anvil-scheduler-systemd.sh [--dry-run] [--no-build] [--no-enable] [--start]

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
STATE_DIR="$(dirname "$STATE_PATH")"
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

printf 'anvil scheduler systemd installation prepared.\n'
printf 'env: %s\nunit: %s\nbinary: %s\n' "$ENV_PATH" "$UNIT_PATH" "$BIN_PATH"
