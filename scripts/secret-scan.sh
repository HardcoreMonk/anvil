#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  cat >&2 <<'USAGE'
Usage: bash scripts/secret-scan.sh

Scans without printing secret values.

Environment:
  ANVIL_SECRET_SCAN_STRICT_HISTORY=1  fail when git history contains key patterns
  ANVIL_SECRET_SCAN_STRICT_LOCAL=1    fail when ignored local files contain key patterns
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

patterns='AIza[0-9A-Za-z_-]{20,}|sk-[A-Za-z0-9_-]{20,}|sk-ant-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}'
failed=0

print_block() {
  local title="$1"
  local body="$2"
  printf '%s\n' "$title"
  if [[ -n "$body" ]]; then
    printf '%s\n' "$body" | sed 's/^/  /'
  else
    printf '  none\n'
  fi
}

tracked_hits="$(git grep -I -l -E "$patterns" -- . || true)"
if [[ -n "$tracked_hits" ]]; then
  print_block "tracked tree: FAIL (secret-like patterns in tracked files)" "$tracked_hits"
  failed=1
else
  print_block "tracked tree: PASS" ""
fi

history_hits="$(git log --all --pretty=format:'%H %ad %s' --date=short -G "$patterns" -- . || true)"
if [[ -n "$history_hits" ]]; then
  print_block "git history: WARN (past secret-like changes; rotate exposed keys)" "$history_hits"
  if [[ "${ANVIL_SECRET_SCAN_STRICT_HISTORY:-0}" == "1" ]]; then
    failed=1
  fi
else
  print_block "git history: PASS" ""
fi

local_hits="$(
  rg --hidden --no-ignore -I -l \
    -g '!.git/**' \
    -g '!artifacts/golden-image.ext4' \
    -g '!vms/**' \
    -g '!flocks/**' \
    -g '!snapshots/**' \
    -g '!anvil-daemon' \
    -g '!anvil-scheduler' \
    -g '!artifacts/goose-agent' \
    -g '!artifacts/micro-init' \
    "$patterns" . || true
)"
if [[ -n "$local_hits" ]]; then
  print_block "ignored/local files: WARN (values intentionally not printed)" "$local_hits"
  if [[ "${ANVIL_SECRET_SCAN_STRICT_LOCAL:-0}" == "1" ]]; then
    failed=1
  fi
else
  print_block "ignored/local files: PASS" ""
fi

exit "$failed"
