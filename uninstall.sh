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

# [v0.7.0 학습 주석] 개요.
#   - 기본 동작: service만 제거하고 DEST(configs/secrets/이미지)는 보존한다.
#     --purge일 때만 확인 프롬프트 후 DEST 전체를 삭제한다.
#   - -h|--help는 install.sh와 동일하게 헤더 주석만 출력하고 exit 0 — root 검사
#     이전이라 무변이(no side effect) 경로다.
#   - DEST=EPHEMERA_HOME 기본값 /opt/ephemera는 install.sh/ephemera.service.in과
#     동일한 변수로, 셋 다 같은 경로를 가리켜야 재설치/제거가 어긋나지 않는다.
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

# [학습 주석] /tmp 정리 경계: ephemera가 쓰는 접두어(goose-workspaces, goose-mnt-*,
# firecracker-*.sock 등)로만 prefix-anchored 삭제한다 — /tmp 전체를 비우지 않는다.
# 이미 위에서 root 검사(id -u)를 통과했으므로 root-gated cleanup이다.
# 참고: goose-rootfs는 현재 소스에서 이 경로를 생성하는 producer가 없는 legacy
# 항목으로, 실무에서는 대개 no-op(경로가 애초에 존재하지 않아 rm이 아무 일도 안 함).
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
