#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(cd -- "$script_dir/.." && pwd -P)"
cd "$repo_root"

usage() {
  printf 'Usage: %s [lifecycle|semantic]\n' "${0##*/}" >&2
}

mode="${1:-lifecycle}"
if [[ $# -gt 1 ]]; then
  usage
  exit 2
fi

case "$mode" in
  lifecycle|semantic)
    ;;
  *)
    usage
    exit 2
    ;;
esac

go build -o /tmp/anvil-mcp ./cmd/anvil-mcp

case "$mode" in
  lifecycle)
    go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -expect-output ""
    ;;
  semantic)
    go run ./scripts/anvil-mcp-smoke.go -command /tmp/anvil-mcp -expect-output "anvil-smoke-ok"
    ;;
esac
