#!/usr/bin/env bash
#
# Builds Ephemera end-user release tarballs into dist/:
#   ephemera-<ver>-linux-amd64-full.tar.gz   (bundled golden image — instant first VM)
#   ephemera-<ver>-linux-amd64-slim.tar.gz   (no image — built on first boot via debootstrap)
#
# Run on an amd64 Linux host. The FULL variant bakes the golden image
# (debootstrap/mount) and therefore needs root + the image-build toolchain;
# use --slim-only to skip it. Build on the OLDEST supported host (Ubuntu 22.04)
# so the dynamically-linked ephemera-daemon runs on older glibc too.
#
#   sudo bash scripts/build_release.sh v0.7.0
#   bash scripts/build_release.sh v0.7.0 --slim-only      # SLIM only, no root needed
#
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

SLIM_ONLY=0
VERSION=""
for a in "$@"; do
	case "$a" in
		--slim-only) SLIM_ONLY=1 ;;
		-h|--help) sed -n '2,14p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
		-*) echo "unknown flag: $a" >&2; exit 2 ;;
		*) VERSION="$a" ;;
	esac
done
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"

OUT="$REPO/dist"
STAGEROOT="$(mktemp -d)"
STAGE="$STAGEROOT/ephemera-$VERSION"
trap 'rm -rf "$STAGEROOT"' EXIT

say() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

# Single source of truth for the pinned kernel/firecracker — parsed straight from
# main.go so a release can never desync from what the daemon expects.
pin() { sed -n "s/.*$1[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\1/p" cmd/goose-daemon/main.go | head -1; }

# ----------------------------------------------------------------- guards ----
command -v go >/dev/null 2>&1 || die "go toolchain required"
[ -f cmd/goose-daemon/uidist/index.html ] || die "Web UI not built (go:embed input missing). Run: cd web && npm install && npm run build"
if [ "$SLIM_ONLY" -eq 0 ]; then
	[ "$(id -u)" -eq 0 ] || die "FULL variant bakes the golden image (debootstrap/mount) and needs root. Use sudo, or pass --slim-only."
	for c in curl debootstrap fallocate mkfs.ext4 e2fsck resize2fs; do
		command -v "$c" >/dev/null 2>&1 || die "golden-image build needs '$c' (apt-get install -y curl debootstrap util-linux e2fsprogs), or use --slim-only"
	done
fi

KURL=$(pin kernelDownloadURL);      KSHA=$(pin kernelSHA256)
FURL=$(pin firecrackerDownloadURL); FSHA=$(pin firecrackerSHA256)
[ -n "$KURL" ] && [ -n "$KSHA" ] && [ -n "$FURL" ] && [ -n "$FSHA" ] || die "could not parse kernel/firecracker pins from cmd/goose-daemon/main.go"

install -d "$STAGE/artifacts" "$STAGE/scripts" "$STAGE/configs"

# --------------------------------------------------------- host binaries ----
say "Building host binaries (ephemera-daemon, ephemera-ctl)"
go build -trimpath -ldflags "-s -w" -o "$STAGE/ephemera-daemon" ./cmd/goose-daemon/
go build -trimpath -ldflags "-s -w" -o "$STAGE/ephemera-ctl"    ./cmd/ephemera-ctl/

# --------------------------------------------------------- in-VM binaries ----
# Same flags the daemon's EnsureGooseAgent/EnsureMicroInit use, so the shipped
# artifacts are accepted as up to date and `go build` is never invoked at runtime.
say "Building in-VM binaries (CGO_ENABLED=0 static)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$STAGE/artifacts/goose-agent" ./cmd/goose-agent/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$STAGE/artifacts/micro-init"  ./cmd/micro-init/

# goose-agent currency is source-hash based (unlike mtime-based micro-init), so a
# source-less release install cannot recompute it. Ship the source-hash stamp beside the
# binary so EnsureGooseAgent accepts the prebuilt artifact instead of walking an absent
# source tree, and so build_image.sh / EnsureGoldenImageGooseAgent read a consistent hash.
# Computed via the daemon's own helper subcommand — the single source of truth for the
# hash algorithm (no bash reimplementation to drift).
say "Stamping goose-agent artifact with its source hash"
SRCHASH=$("$STAGE/ephemera-daemon" print-goose-agent-source-hash "$REPO")
[ -n "$SRCHASH" ] || die "could not compute goose-agent source hash"
printf '%s\n' "$SRCHASH" > "$STAGE/artifacts/goose-agent.sha256"

# -------------------------------------------------- kernel + firecracker ----
say "Downloading pinned kernel + firecracker"
curl -fL -o "$STAGE/artifacts/vmlinux.bin" "$KURL"
echo "$KSHA  $STAGE/artifacts/vmlinux.bin" | sha256sum -c - >/dev/null || die "kernel SHA256 mismatch"

fctgz="$STAGEROOT/firecracker.tgz"
curl -fL -o "$fctgz" "$FURL"
echo "$FSHA  $fctgz" | sha256sum -c - >/dev/null || die "firecracker SHA256 mismatch"
fcx="$STAGEROOT/fc"; mkdir -p "$fcx"; tar -xzf "$fctgz" -C "$fcx"
# Exclude the detached debug object and require the executable bit. Upstream ships
# firecracker-v<ver>-x86_64.debug (mode 0644) alongside the real binary and both
# match 'firecracker-*', so `head -1` picks whichever the filesystem hands back
# first. It happens to return the executable today, which is exactly why this is
# worth pinning down rather than leaving to directory order: installing the debug
# object as the hypervisor makes every VM spawn die with a segfault, and the
# archive digest still matches the pin, so nothing upstream of here looks wrong.
fcbin=$(find "$fcx" -type f -name 'firecracker-*' ! -name 'jailer*' ! -name '*.debug' -perm -u+x | head -1)
[ -n "$fcbin" ] || die "firecracker binary not found in release tarball"
cp "$fcbin" "$STAGE/artifacts/firecracker"
chmod 755 "$STAGE/artifacts/firecracker"

# Provenance stamp for the extracted binary: "<tarball-sha256> <binary-sha256>".
# The daemon cannot compare the installed binary against the pin directly — the
# pin is the digest of the .tgz, not of what comes out of it — so this sidecar is
# what makes the check possible (storage.firecrackerPinStatus).
#
# Shipping it is not an optimisation. install.sh copies artifacts/ over an
# existing install without deleting anything, so a host that booted once under
# an older pin keeps that stamp. Without a stamp in the tarball there is nothing
# to overwrite it with, and the next boot reads a pin the stamp disagrees with,
# calls it a mismatch and refuses to start — with the correct binary already on
# disk. Carrying the stamp alongside the binary keeps the pair self-consistent
# through an upgrade, the way the kernel's pin already is.
printf '%s  %s\n' "$FSHA" "$(sha256sum "$STAGE/artifacts/firecracker" | cut -d' ' -f1)" \
	> "$STAGE/artifacts/firecracker.pin-state"

# -------------------------------------------- scripts, configs, installer ----
say "Staging scripts, config templates, and installer"
cp scripts/build_image.sh scripts/gtwall scripts/gtcall "$STAGE/scripts/"
cp configs/goose.yaml.example configs/goose-secrets.yaml.example "$STAGE/configs/"
cp install.sh uninstall.sh ephemera.service.in "$STAGE/"
[ -f INSTALL.md ] && cp INSTALL.md "$STAGE/"
chmod 755 "$STAGE/install.sh" "$STAGE/uninstall.sh" "$STAGE/scripts/"build_image.sh "$STAGE/scripts/"gtwall "$STAGE/scripts/"gtcall

# ----------------------------------------------- FULL: bake golden image ----
if [ "$SLIM_ONLY" -eq 0 ]; then
	say "Baking golden image for the FULL variant (cd $STAGE)"
	( cd "$STAGE" && bash scripts/build_image.sh )
	[ -f "$STAGE/artifacts/golden-image.ext4" ] || die "golden image was not produced"
	# mtime normalization (load-bearing): the daemon keeps a prebuilt golden image
	# only while it is newer than its build inputs. Stamp inputs old, image newest,
	# so first boot logs "golden image up to date" instead of rebuilding.
	touch -d '2020-01-01 00:00:00' \
		"$STAGE/scripts/build_image.sh" "$STAGE/scripts/gtwall" "$STAGE/scripts/gtcall" \
		"$STAGE/artifacts/goose-agent" "$STAGE/artifacts/goose-agent.sha256" "$STAGE/artifacts/micro-init"
	touch -d '2020-01-02 00:00:00' \
		"$STAGE/artifacts/golden-image.ext4" "$STAGE/artifacts/vmlinux.bin" "$STAGE/artifacts/firecracker"
fi

# ------------------------------------------------------------- packaging ----
install -d "$OUT"
base="ephemera-${VERSION}-linux-amd64"

say "Packaging SLIM tarball"
# tar preserves the normalized mtimes; install.sh extracts with cp -a / no -m so
# they survive to the installed tree.
tar -czf "$OUT/${base}-slim.tar.gz" --exclude='*/artifacts/golden-image.ext4' \
	-C "$STAGEROOT" "ephemera-$VERSION"
( cd "$OUT" && sha256sum "${base}-slim.tar.gz" > "${base}-slim.tar.gz.sha256" )

if [ -f "$STAGE/artifacts/golden-image.ext4" ]; then
	say "Packaging FULL tarball"
	tar -czf "$OUT/${base}-full.tar.gz" -C "$STAGEROOT" "ephemera-$VERSION"
	( cd "$OUT" && sha256sum "${base}-full.tar.gz" > "${base}-full.tar.gz.sha256" )
fi

say "Done. Release artifacts in $OUT:"
ls -lh "$OUT"/${base}-*.tar.gz
