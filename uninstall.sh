#!/usr/bin/env bash
#
# Ephemera uninstaller. By default it removes the service + symlink and leaves
# /opt/ephemera in place (configs, secrets, and any baked VM image are preserved).
# Pass --purge to delete the whole install directory after a confirmation.
#
#   sudo ./uninstall.sh            # remove service, keep data
#   sudo ./uninstall.sh --purge    # remove everything under /opt/ephemera too
#
set -euo pipefail

DEST="${EPHEMERA_HOME:-/opt/ephemera}"
PURGE=0
for a in "$@"; do
	case "$a" in
		--purge) PURGE=1 ;;
		-h|--help) sed -n '2,9p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "unknown flag: $a" >&2; exit 2 ;;
	esac
done

say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root: sudo ./uninstall.sh"

say "Stopping and disabling the service"
systemctl disable --now ephemera 2>/dev/null || true
rm -f /etc/systemd/system/ephemera.service
systemctl daemon-reload 2>/dev/null || true

# Remove the CLI symlink only if it still points into this install.
if [ -L /usr/local/bin/ephemera-ctl ]; then
	target=$(readlink -f /usr/local/bin/ephemera-ctl 2>/dev/null || true)
	case "$target" in
		"$DEST"/*) rm -f /usr/local/bin/ephemera-ctl ;;
	esac
fi

# Best-effort cleanup of the daemon's runtime scratch (safe now the service is stopped).
say "Cleaning runtime scratch under /tmp"
rm -rf /tmp/goose-workspaces /tmp/goose-rootfs 2>/dev/null || true
rm -rf /tmp/goose-mnt-* 2>/dev/null || true
rm -f /tmp/firecracker-*.sock /tmp/firecracker-vsock-*.sock /tmp/fc-*-log.fifo 2>/dev/null || true

if [ "$PURGE" -eq 1 ]; then
	warn "About to delete $DEST entirely — this includes configs/goose-secrets.yaml (API keys) and any baked VM image."
	read -rp "Type 'yes' to confirm: " ans
	if [ "$ans" = "yes" ]; then
		rm -rf "$DEST"
		say "Removed $DEST."
	else
		say "Aborted purge; $DEST left in place."
	fi
else
	say "Service removed. Kept $DEST (configs, secrets, image). Re-run install.sh to reactivate, or pass --purge to delete it."
fi

say "Note: OS packages (iproute2, dmsetup, iptables, …) were not touched."
