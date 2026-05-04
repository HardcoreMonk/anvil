#!/bin/bash
set -euo pipefail

# Guest OS: Debian Bookworm (12) minbase — native glibc, required for linux-gnu Goose binary.
# Alpine + gcompat was attempted but gcompat lacks internal glibc ABI symbols (_Wind_GetDataRelBase).
# Supports host OS: Ubuntu 22.04 and 24.04
# Host dependencies: curl, debootstrap, e2fsprogs, util-linux

IMAGE_NAME="artifacts/golden-image.ext4"
MNT_DIR="/tmp/goose-rootfs"

GOOSE_URL="https://github.com/aaif-goose/goose/releases/download/stable/goose-x86_64-unknown-linux-gnu.tar.bz2"
GOOSE_TARBALL="/tmp/goose.tar.bz2"
GOOSE_TMP=""

cleanup() {
    umount "$MNT_DIR/dev/pts" 2>/dev/null || true
    umount "$MNT_DIR/dev"     2>/dev/null || true
    umount "$MNT_DIR/sys"     2>/dev/null || true
    umount "$MNT_DIR/proc"    2>/dev/null || true
    umount "$MNT_DIR"         2>/dev/null || true
    rm -f "$GOOSE_TARBALL"
    [ -n "$GOOSE_TMP" ] && rm -rf "$GOOSE_TMP"
}
trap cleanup EXIT

check_host_dependencies() {
    local missing=()
    for cmd in curl debootstrap fallocate mkfs.ext4 e2fsck resize2fs; do
        command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        echo "Error: missing required tools: ${missing[*]}" >&2
        echo "Fix: sudo apt-get install -y curl debootstrap util-linux e2fsprogs" >&2
        exit 1
    fi
}

check_host_dependencies
mkdir -p "$(dirname "$IMAGE_NAME")"

echo "==> 1. Downloading Goose binary on host <=="
curl -fL -o "$GOOSE_TARBALL" "$GOOSE_URL"

echo "==> 2. Creating and formatting image (512M initial; shrunk by resize2fs at the end) <=="
fallocate -l 1G "$IMAGE_NAME"
# -m 0: no reserved blocks (unnecessary in a VM image)
mkfs.ext4 -F -L goose-root -m 0 "$IMAGE_NAME"
mkdir -p "$MNT_DIR"
mount "$IMAGE_NAME" "$MNT_DIR"

echo "==> 3. Installing Debian Bookworm minbase via debootstrap <=="
# --variant=minbase: essential packages only (~180MB, proper glibc included)
# --include: batched into the same pass to avoid a separate apt-get run
#   libgomp1:    OpenMP runtime required by Goose
#   ca-certificates: for HTTPS LLM API calls from within the VM
debootstrap \
    --variant=minbase \
    --include=libgomp1,ca-certificates,tzdata \
    bookworm "$MNT_DIR" http://deb.debian.org/debian/

echo "==> 4. Installing Goose binary and goose-agent <=="
GOOSE_TMP=$(mktemp -d)
tar -xjf "$GOOSE_TARBALL" -C "$GOOSE_TMP"
GOOSE_BIN=$(find "$GOOSE_TMP" -name "goose" -type f | head -1)
if [ -z "$GOOSE_BIN" ]; then
    echo "Error: goose binary not found in tarball" >&2
    exit 1
fi
install -m 755 "$GOOSE_BIN" "$MNT_DIR/usr/local/bin/goose"
rm -rf "$GOOSE_TMP"; GOOSE_TMP=""

# goose-agent is pre-built by the daemon (EnsureGooseAgent) before this script runs.
GOOSE_AGENT_BIN="artifacts/goose-agent"
if [ ! -f "$GOOSE_AGENT_BIN" ]; then
    echo "Error: $GOOSE_AGENT_BIN not found. The daemon builds it automatically." >&2
    exit 1
fi
install -m 755 "$GOOSE_AGENT_BIN" "$MNT_DIR/usr/local/bin/goose-agent"

echo "==> 5. Writing minimal init <=="
# Written to /usr/local/sbin/micro-init and referenced via init= kernel parameter.
# Network is already live via kernel ip= parameter injected by the host control plane.
# goose-agent runs as PID 1 (via exec). When it exits, the guest kernel panics —
# this is intentional: the panic is contained within the KVM hardware boundary and
# does not affect the host OS. Firecracker catches the reboot signal and exits cleanly.
# This panic-on-exit approach is simpler and safe enough for ephemeral VMs.
mkdir -p "$MNT_DIR/usr/local/sbin"
cat > "$MNT_DIR/usr/local/sbin/micro-init" << 'INIT_SCRIPT'
#!/bin/sh
mount -t proc  none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev 2>/dev/null || true
mkdir -p /dev/pts
mount -t devpts none /dev/pts 2>/dev/null || true

# Set essential environment variables that init systems normally provide.
# Without HOME, goose cannot resolve ~/.config/goose/config.yaml.
export HOME=/root
export USER=root
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# /etc/localtime is a symlink injected by the host control plane at VM launch
# (pointing to /usr/share/zoneinfo/{tzname}). glibc reads it automatically
# when TZ is unset, so no TZ export is needed here.

# Non-interactive mode: if a task file was injected by the control plane,
# pipe it to goose run. Otherwise fall back to interactive session (debug).
# Persistent mode: goose-agent listens on :8080 and runs goose per request.
# One-shot fallback: if task.txt was injected, run it once and exit.
# Interactive fallback: neither present → open interactive session (debug only).
if [ -f /usr/local/bin/goose-agent ]; then
    exec /usr/local/bin/goose-agent
elif [ -f /root/task.txt ]; then
    exec /usr/local/bin/goose run -i - < /root/task.txt
else
    exec /usr/local/bin/goose
fi
INIT_SCRIPT
chmod +x "$MNT_DIR/usr/local/sbin/micro-init"

printf 'goose-agent\n'                       > "$MNT_DIR/etc/hostname"
printf '127.0.0.1\tlocalhost goose-agent\n'  > "$MNT_DIR/etc/hosts"

# The kernel ip= parameter sets IP/GW/mask but not DNS.
# The host's resolv.conf typically points to 127.0.0.53 (systemd-resolved)
# which is unreachable inside the VM, so pin public resolvers here.
cat > "$MNT_DIR/etc/resolv.conf" << 'EOF'
nameserver 8.8.8.8
nameserver 1.1.1.1
EOF

echo "==> 6. Stripping docs, manpages, locale data and apt cache <=="
rm -rf \
    "$MNT_DIR/var/lib/apt/lists"/* \
    "$MNT_DIR/var/cache/apt"/*.bin \
    "$MNT_DIR/usr/share/doc"/* \
    "$MNT_DIR/usr/share/man"/* \
    "$MNT_DIR/usr/share/info"/* \
    "$MNT_DIR/usr/share/locale"/*

echo "==> 7. Unmounting and shrinking image to actual used size <=="
umount "$MNT_DIR"
trap - EXIT
rm -f "$GOOSE_TARBALL"

e2fsck -f -y "$IMAGE_NAME"
resize2fs -M "$IMAGE_NAME"

FINAL_SIZE=$(du -sh "$IMAGE_NAME" | cut -f1)
echo "==> Golden image ready: $IMAGE_NAME (${FINAL_SIZE}) <=="
