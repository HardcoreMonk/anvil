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

# [v0.7.0 학습 주석] 개요.
#   - SLIM(default)은 golden-image.ext4를 뺀 tarball(첫 부팅에 debootstrap으로
#     빌드). --slim-only 없이 실행하면 FULL도 추가로 만드는데, FULL은 golden
#     image를 이 스크립트가 직접 베이크하므로 root + debootstrap 툴체인이 필요하다.
#   - kernel/firecracker 다운로드는 각각 `sha256sum -c`로 pinned SHA를 검증한다
#     (공급망 무결성). pin 값은 하드코딩이 아니라 pin()이 cmd/goose-daemon/main.go를
#     sed로 파싱해 가져오므로, 릴리스가 daemon이 기대하는 값과 어긋날 수 없다.
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

# -------------------------------------------------- kernel + firecracker ----
say "Downloading pinned kernel + firecracker"
# [학습 주석] 이 sha256sum -c 검증은 internal/storage/provisioner.go의
# EnsureKernel(런타임에서 파일이 없을 때만 동작)과 별개 방어선이다 — EnsureKernel은
# os.Stat으로 이미 있는 파일은 그냥 skip하므로, FULL tarball에 미리 담겨 배포되는
# vmlinux.bin은 이 빌드 시점 검증이 유일한 공급망 체크다.
curl -fL -o "$STAGE/artifacts/vmlinux.bin" "$KURL"
echo "$KSHA  $STAGE/artifacts/vmlinux.bin" | sha256sum -c - >/dev/null || die "kernel SHA256 mismatch"

fctgz="$STAGEROOT/firecracker.tgz"
curl -fL -o "$fctgz" "$FURL"
echo "$FSHA  $fctgz" | sha256sum -c - >/dev/null || die "firecracker SHA256 mismatch"
fcx="$STAGEROOT/fc"; mkdir -p "$fcx"; tar -xzf "$fctgz" -C "$fcx"
fcbin=$(find "$fcx" -type f -name 'firecracker-*' ! -name 'jailer*' | head -1)
[ -n "$fcbin" ] || die "firecracker binary not found in release tarball"
cp "$fcbin" "$STAGE/artifacts/firecracker"
chmod 755 "$STAGE/artifacts/firecracker"

# -------------------------------------------- scripts, configs, installer ----
say "Staging scripts, config templates, and installer"
cp scripts/build_image.sh scripts/gtwall scripts/gtcall "$STAGE/scripts/"
cp configs/goose.yaml.example configs/goose-secrets.yaml.example "$STAGE/configs/"
cp install.sh uninstall.sh ephemera.service.in "$STAGE/"
[ -f INSTALL.md ] && cp INSTALL.md "$STAGE/"
chmod 755 "$STAGE/install.sh" "$STAGE/uninstall.sh" "$STAGE/scripts/"build_image.sh "$STAGE/scripts/"gtwall "$STAGE/scripts/"gtcall

# ----------------------------------------------- FULL: bake golden image ----
# [학습 주석] FULL 변형만 이 블록을 탄다 — guards 섹션에서 이미 root + 이미지
# 빌드 툴체인을 확인했다. golden-image.ext4를 이 호스트에서 직접 debootstrap/mount로
# 굽기 때문에 root 권한이 필요하다(SLIM은 이 단계를 건너뛰고 첫 부팅 시 빌드로 미룬다).
if [ "$SLIM_ONLY" -eq 0 ]; then
	say "Baking golden image for the FULL variant (cd $STAGE)"
	( cd "$STAGE" && bash scripts/build_image.sh )
	[ -f "$STAGE/artifacts/golden-image.ext4" ] || die "golden image was not produced"
	# mtime normalization (load-bearing): the daemon keeps a prebuilt golden image
	# only while it is newer than its build inputs. Stamp inputs old, image newest,
	# so first boot logs "golden image up to date" instead of rebuilding.
	touch -d '2020-01-01 00:00:00' \
		"$STAGE/scripts/build_image.sh" "$STAGE/scripts/gtwall" "$STAGE/scripts/gtcall" \
		"$STAGE/artifacts/goose-agent" "$STAGE/artifacts/micro-init"
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
