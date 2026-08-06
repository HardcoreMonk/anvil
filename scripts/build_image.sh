#!/bin/bash
set -euo pipefail

# Guest OS: Debian Trixie minbase — native glibc new enough for the linux-gnu Goose binary.
# Alpine + gcompat was attempted but gcompat lacks internal glibc ABI symbols (_Wind_GetDataRelBase).
# Supports host OS: Ubuntu 22.04 and 24.04
# Host dependencies: curl, debootstrap, e2fsprogs, util-linux

IMAGE_NAME="${EPHEMERA_GOLDEN_IMAGE_PATH:-artifacts/golden-image.ext4}"
DEBIAN_SUITE="trixie"

die() { echo "Error: $*" >&2; exit 1; }

# Goose CLI pin — immutable version tag + SHA256, mirroring the kernel/firecracker
# discipline (constants in cmd/goose-daemon/main.go, `sha256sum -c ... || die` in
# scripts/build_release.sh). Upstream's `stable` tag is a *rolling* pointer whose
# assets are re-uploaded in place, so the old `.../download/stable/...` URL handed
# out different bytes over time with nothing to check them against. This tarball
# becomes /usr/local/bin/goose in the golden image and runs every VM's LLM
# workload holding the provider API key — and on SLIM packages this script runs on
# the end user's host, as root, on first boot. It is pinned and fail-closed verified.
#
# To bump the version:
#   1. pick the new tag from https://github.com/aaif-goose/goose/releases
#   2. v=vX.Y.Z; curl -fsSL \
#        "https://github.com/aaif-goose/goose/releases/download/$v/goose-x86_64-unknown-linux-gnu.tar.bz2" \
#        | sha256sum
#   3. paste the tag and hash below. Editing this file makes the golden image stale
#      (EnsureGoldenImage's mtime input list), so the next daemon start rebuilds it.
GOOSE_VERSION="v1.45.0"
GOOSE_SHA256="ec5da5f018cf68ea446887d30decf847542035ffcf91536d1d134ed94bb24401"
GOOSE_URL="https://github.com/aaif-goose/goose/releases/download/${GOOSE_VERSION}/goose-x86_64-unknown-linux-gnu.tar.bz2"

# Scratch paths are created by mktemp below, not hardcoded under /tmp. The former
# fixed names (/tmp/goose-rootfs, /tmp/goose.tar.bz2) were predictable targets this
# script writes to as root in a shared, world-writable /tmp (the ephemera unit runs
# without PrivateTmp on purpose). Sticky /tmp only stops another user from deleting
# our files; it does not stop them from pre-creating an *absent* name as a symlink
# in the window before we open it, which would make a root `curl -o` clobber the
# link target. mktemp creates the name atomically (O_EXCL) and fails otherwise.
# Same pattern as scripts/build_release.sh:33.
MNT_DIR=""
GOOSE_TARBALL=""
GOOSE_TMP=""

cleanup() {
    if [ -n "$MNT_DIR" ]; then
        # Deepest mount first; the mount point itself last. Only then rmdir it —
        # and rmdir, never `rm -rf`: if any umount above failed, rmdir fails
        # harmlessly on the non-empty mount point, whereas `rm -rf` would recurse
        # into the still-mounted rootfs and delete the image contents.
        umount "$MNT_DIR/dev/pts" 2>/dev/null || true
        umount "$MNT_DIR/dev"     2>/dev/null || true
        umount "$MNT_DIR/sys"     2>/dev/null || true
        umount "$MNT_DIR/proc"    2>/dev/null || true
        umount "$MNT_DIR"         2>/dev/null || true
        rmdir  "$MNT_DIR"         2>/dev/null || true
    fi
    # `|| true` on the rm itself, not just the trailing `return 0`: under `set -e`
    # a failing *last* command of an AND list aborts the function on the spot, so
    # an rm that fails (its containing directory unwritable, say) would exit
    # cleanup before `return 0` is ever reached and replace the script's real exit
    # status with the rm's. The return then covers the remaining paths.
    [ -n "$GOOSE_TARBALL" ] && { rm -f "$GOOSE_TARBALL" || true; }
    [ -n "$GOOSE_TMP" ] && { rm -rf "$GOOSE_TMP" || true; }
    return 0
}
trap cleanup EXIT

check_host_dependencies() {
    local missing=()
    for cmd in curl debootstrap fallocate mkfs.ext4 e2fsck resize2fs mktemp sha256sum; do
        command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        echo "Error: missing required tools: ${missing[*]}" >&2
        echo "Fix: sudo apt-get install -y curl debootstrap util-linux e2fsprogs coreutils" >&2
        exit 1
    fi
}

check_host_dependencies
mkdir -p "$(dirname "$IMAGE_NAME")"

# Unpredictable, atomically created, mode 0600/0700 root-owned scratch (see above).
GOOSE_TARBALL=$(mktemp "${TMPDIR:-/tmp}/goose-tarball.XXXXXXXXXX") \
    || die "could not create a temporary file for the Goose download"
MNT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/goose-rootfs.XXXXXXXXXX") \
    || die "could not create a temporary rootfs mount point"

echo "==> 1. Downloading Goose binary on host <=="
echo "    ${GOOSE_URL}"
curl -fL -o "$GOOSE_TARBALL" "$GOOSE_URL"
# Fail closed: an unverified goose binary must never reach the golden image.
echo "${GOOSE_SHA256}  ${GOOSE_TARBALL}" | sha256sum -c - >/dev/null \
    || die "Goose tarball SHA256 mismatch for ${GOOSE_VERSION} — refusing to bake an unverified binary into the golden image.
       Expected: ${GOOSE_SHA256}
       Got:      $(sha256sum < "$GOOSE_TARBALL" | cut -d' ' -f1)
       If the upstream release was legitimately re-published, update GOOSE_VERSION/GOOSE_SHA256 in this script."
echo "    SHA256 verified (${GOOSE_VERSION})"

echo "==> 2. Creating and formatting image (512M initial; shrunk by resize2fs at the end) <=="
fallocate -l 1G "$IMAGE_NAME"
# -m 0: no reserved blocks (unnecessary in a VM image)
mkfs.ext4 -F -L goose-root -m 0 "$IMAGE_NAME"
mkdir -p "$MNT_DIR"
mount "$IMAGE_NAME" "$MNT_DIR"

echo "==> 3. Installing Debian Trixie minbase via debootstrap <=="
# --variant=minbase: essential packages only (~180MB, proper glibc included)
# --include: batched into the same pass to avoid a separate apt-get run
#   libgomp1:    OpenMP runtime required by Goose
#   libvulkan1:  runtime library required by the linux-gnu Goose binary
#   ca-certificates: for HTTPS LLM API calls from within the VM
#   curl, jq:    used by in-VM agents to call peer agents' /tasks endpoints
#                (e.g. orchestrator dispatching to worker/reviewer in a flock)
debootstrap \
    --variant=minbase \
    --include=libgomp1,libvulkan1,ca-certificates,tzdata,iproute2,curl,jq \
    "$DEBIAN_SUITE" "$MNT_DIR" http://deb.debian.org/debian/

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
GOOSE_AGENT_STAMP="${GOOSE_AGENT_BIN}.sha256"
if [ ! -f "$GOOSE_AGENT_BIN" ]; then
    echo "Error: $GOOSE_AGENT_BIN not found. The daemon builds it automatically." >&2
    exit 1
fi
install -m 755 "$GOOSE_AGENT_BIN" "$MNT_DIR/usr/local/bin/goose-agent"
if [ -f "$GOOSE_AGENT_STAMP" ]; then
    install -m 644 "$GOOSE_AGENT_STAMP" "$MNT_DIR/usr/local/bin/goose-agent.sha256"
fi

echo "==> 5. Installing micro-init binary <=="
# micro-init is a Go binary (cmd/micro-init/) pre-built by the daemon before this
# script runs. It runs as PID 1, mounts virtual filesystems, starts goose-agent as
# a child process, and calls poweroff(2) on exit — preventing the kernel panic that
# would occur if PID 1 exited without a graceful shutdown sequence.
MICRO_INIT_BIN="artifacts/micro-init"
if [ ! -f "$MICRO_INIT_BIN" ]; then
    echo "Error: $MICRO_INIT_BIN not found. The daemon builds it automatically." >&2
    exit 1
fi
mkdir -p "$MNT_DIR/usr/local/sbin"
install -m 755 "$MICRO_INIT_BIN" "$MNT_DIR/usr/local/sbin/micro-init"

echo "==> 5b. Installing gtwall and gtcall CLIs <=="
# gtwall posts to the in-VM goose-agent /townwall/post, which forwards to the
# host control plane /flocks/{id}/post. Available only inside flock-spawned VMs.
install -m 755 scripts/gtwall "$MNT_DIR/usr/local/bin/gtwall"
# gtcall dispatches a prompt to a peer agent in the same flock via the control
# plane's POST /vms/{vm_id}/tasks proxy. Hides curl + jq + bearer + JSON
# quoting behind a `gtcall <agent_id> <prompt>` single-line interface so
# LLM-authored bash does not have to assemble these correctly itself.
install -m 755 scripts/gtcall "$MNT_DIR/usr/local/bin/gtcall"

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
# umount succeeded (set -e would have aborted otherwise), so the mount point is a
# plain empty directory here — rmdir removes it and never reaches into the image.
rmdir "$MNT_DIR"
rm -f "$GOOSE_TARBALL"

e2fsck -f -y "$IMAGE_NAME"
resize2fs -M "$IMAGE_NAME"

FINAL_SIZE=$(du -sh "$IMAGE_NAME" | cut -f1)
echo "==> Golden image ready: $IMAGE_NAME (${FINAL_SIZE}) <=="
