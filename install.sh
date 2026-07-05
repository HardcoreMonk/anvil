#!/usr/bin/env bash
#
# Ephemera installer — places prebuilt binaries + config under /opt/ephemera,
# writes the LLM provider config interactively, and registers a systemd service.
# Run from inside an unpacked release tarball:  sudo ./install.sh
#
# Flags:
#   --reconfigure   re-run the provider/API-key prompt even if a config exists
#
set -euo pipefail

# [v0.7.0 학습 주석] end-user installer 전체 개요.
#   - 흐름: preflight(호스트 요건 검사) → place_files(바이너리/설정 배치) →
#     configure_provider(LLM provider/키 입력) → setup_env(API 토큰) →
#     install_service(systemd 유닛 등록) → wait_ready → summary.
#   - DEST(EPHEMERA_HOME)는 daemon의 WorkingDirectory이자 리소스 root다.
#     ephemera.service.in의 @DEST@ 치환과 cmd/goose-daemon/main.go의
#     resolveWorkDir()이 이 값을 그대로 공유한다(설치 경로 = 런타임 workdir).
#   - -h|--help는 preflight 이전에 sed로 헤더 주석만 출력하고 exit 0 — 시스템에
#     아무 것도 쓰지 않는 무변이(no side effect) 경로다.
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST="${EPHEMERA_HOME:-/opt/ephemera}"
API_ADDR_DEFAULT="127.0.0.1:3000"
RECONFIGURE=0
for a in "$@"; do
	case "$a" in
		--reconfigure) RECONFIGURE=1 ;;
		-h|--help) sed -n '2,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "unknown flag: $a" >&2; exit 2 ;;
	esac
done

say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- preflight ----
# [학습 주석] root/아키텍처/KVM/필수 커맨드 검사 후 VARIANT(slim|full)를 판정한다.
# golden-image.ext4가 이미 SRC/artifacts에 있으면 full(사전 빌드 이미지 포함),
# 없으면 slim(첫 부팅 시 debootstrap으로 이미지 빌드, 추가 툴체인 필요)이다.
preflight() {
	[ "$(id -u)" -eq 0 ] || die "run as root: sudo ./install.sh"
	[ "$(uname -m)" = "x86_64" ] || die "Ephemera ships amd64 binaries; this host is $(uname -m). Unsupported."
	[ -e /dev/kvm ] || die "/dev/kvm not found — enable KVM (or nested virtualization if this host is itself a VM)."
	[ -w /dev/kvm ] || warn "/dev/kvm is not writable as the current user; the root service should still have access."

	local miss=()
	local c
	for c in ip dmsetup iptables; do command -v "$c" >/dev/null 2>&1 || miss+=("$c"); done
	if [ "${#miss[@]}" -gt 0 ]; then
		die "missing runtime tools: ${miss[*]}
     install with:  apt-get install -y iproute2 dmsetup iptables"
	fi
	command -v ebtables >/dev/null 2>&1 || warn "ebtables not found — per-TAP anti-spoof (EPHEMERA_NET_ANTISPOOF) will be disabled. Optional."
	command -v systemctl >/dev/null 2>&1 || die "systemctl not found — this installer targets systemd hosts."

	if [ -f "$SRC/artifacts/golden-image.ext4" ]; then VARIANT=full; else VARIANT=slim; fi
	say "Package variant: ${VARIANT} (golden image $( [ "$VARIANT" = full ] && echo 'bundled' || echo 'built on first boot' ))"

	if [ "$VARIANT" = slim ]; then
		local bmiss=()
		for c in curl debootstrap fallocate mkfs.ext4 e2fsck resize2fs; do
			command -v "$c" >/dev/null 2>&1 || bmiss+=("$c")
		done
		if [ "${#bmiss[@]}" -gt 0 ]; then
			die "the SLIM package builds the VM image on first boot but these tools are missing: ${bmiss[*]}
     install with:  apt-get install -y curl debootstrap util-linux e2fsprogs
     (or use the FULL package, which ships the prebuilt image)"
		fi
	fi
}

# ---------------------------------------------------------- file placement ----
place_files() {
	UPGRADE=0
	if [ -x "$DEST/ephemera-daemon" ]; then
		UPGRADE=1
		say "Existing install detected at $DEST — upgrading (configs are preserved)."
		if systemctl is-active --quiet ephemera 2>/dev/null; then
			say "Stopping the running service before replacing binaries…"
			systemctl stop ephemera || true
		fi
	fi

	say "Installing files into $DEST"
	install -d -m 755 "$DEST" "$DEST/artifacts" "$DEST/scripts" "$DEST/configs"

	# cp -a preserves mtimes — load-bearing: the daemon treats golden-image.ext4 as
	# up to date only while it is newer than its build inputs (goose-agent, micro-init,
	# build_image.sh, gtwall, gtcall). The release script normalized those mtimes; we
	# must not reset them here (never use `install`, which stamps mtime=now).
	cp -a "$SRC/ephemera-daemon" "$DEST/"
	[ -f "$SRC/ephemera-ctl" ] && cp -a "$SRC/ephemera-ctl" "$DEST/"
	cp -a "$SRC/artifacts/." "$DEST/artifacts/"
	cp -a "$SRC/scripts/." "$DEST/scripts/"
	cp -a "$SRC/configs/goose.yaml.example" "$SRC/configs/goose-secrets.yaml.example" "$DEST/configs/"
	[ -f "$SRC/uninstall.sh" ] && { cp -a "$SRC/uninstall.sh" "$DEST/"; chmod 755 "$DEST/uninstall.sh"; }

	chmod 755 "$DEST/ephemera-daemon" "$DEST/artifacts/firecracker" 2>/dev/null || true
	if [ -f "$DEST/ephemera-ctl" ]; then
		chmod 755 "$DEST/ephemera-ctl"
		ln -sf "$DEST/ephemera-ctl" /usr/local/bin/ephemera-ctl
	fi
}

# --------------------------------------------------- provider + key config ----
configure_provider() {
	if [ -f "$DEST/configs/goose.yaml" ] && [ "$RECONFIGURE" -eq 0 ]; then
		say "Keeping existing configs/goose.yaml (pass --reconfigure to change provider/model)."
		return
	fi

	say "Configure the default LLM provider"
	echo "    1) google     (Gemini)   — GOOGLE_API_KEY"
	echo "    2) anthropic  (Claude)   — ANTHROPIC_API_KEY"
	echo "    3) openai     (GPT)      — OPENAI_API_KEY"
	echo "    4) groq                  — GROQ_API_KEY"

	local choice provider model secret
	while true; do
		read -rp "  Select provider [1-4]: " choice
		case "$choice" in
			1) provider=google;    model="gemini-2.5-flash";    secret=GOOGLE_API_KEY;    break ;;
			2) provider=anthropic; model="claude-sonnet-4-6";    secret=ANTHROPIC_API_KEY; break ;;
			3) provider=openai;    model="gpt-4o";               secret=OPENAI_API_KEY;    break ;;
			4) provider=groq;      model="openai/gpt-oss-120b";  secret=GROQ_API_KEY;      break ;;
			*) warn "enter 1, 2, 3, or 4" ;;
		esac
	done

	local custom
	read -rp "  Model [$model]: " custom
	[ -n "$custom" ] && model="$custom"

	local key
	while true; do
		read -rsp "  $secret (input hidden): " key; echo
		[ -n "$key" ] && break
		warn "key cannot be empty"
	done

	# goose.yaml mirrors configs/goose.yaml.example (telemetry off, keyring off, the
	# bundled developer extension). Provider/model substituted in.
	cat > "$DEST/configs/goose.yaml" <<EOF
# Generated by install.sh — provider/model only. API keys live in goose-secrets.yaml.
GOOSE_PROVIDER: $provider
GOOSE_MODEL: $model
GOOSE_TELEMETRY_ENABLED: false

# MicroVM has no keyring daemon — goose reads secrets.yaml in the same directory.
GOOSE_DISABLE_KEYRING: true

extensions:
  developer:
    bundled: true
    enabled: true
    name: developer
    timeout: 300
    type: builtin
EOF
	chmod 644 "$DEST/configs/goose.yaml"

	# Escape backslash then double-quote so the key is a valid YAML double-quoted scalar.
	local key_esc=${key//\\/\\\\}
	key_esc=${key_esc//\"/\\\"}
	( umask 077; printf '%s: "%s"\n' "$secret" "$key_esc" > "$DEST/configs/goose-secrets.yaml" )
	chmod 600 "$DEST/configs/goose-secrets.yaml"
	say "Wrote configs/goose.yaml and configs/goose-secrets.yaml (0600)."
}

# ----------------------------------------------------------- API token / env ----
setup_env() {
	local envf="$DEST/ephemera.env"
	if [ -f "$envf" ] && grep -q '^EPHEMERA_API_TOKEN=' "$envf"; then
		say "Keeping the existing API token in ephemera.env."
		return
	fi

	local ans token=""
	read -rp "Protect the control-plane API with a Bearer token? [Y/n]: " ans
	case "${ans:-Y}" in
		[Nn]*) say "API left unauthenticated (bound to ${API_ADDR_DEFAULT} — localhost only)." ;;
		*)
			if command -v openssl >/dev/null 2>&1; then
				token=$(openssl rand -hex 24)
			else
				token=$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 40)
			fi
			TOKEN_SHOWN=1
			say "Generated API token (store it — ephemera-ctl reads it via EPHEMERA_API_TOKEN):"
			printf '    %s\n' "$token"
			;;
	esac

	( umask 077
	  {
		echo "# Ephemera daemon environment, loaded by the systemd unit."
		echo "# Edit, then: systemctl restart ephemera   (token changes also reload on SIGHUP)."
		[ -n "$token" ] && echo "EPHEMERA_API_TOKEN=$token"
		echo "EPHEMERA_LOG_LEVEL=info"
		echo "# EPHEMERA_API_ADDR=0.0.0.0:3000      # bind beyond localhost — put behind a TLS reverse proxy"
		echo "# EPHEMERA_PUBLIC_URL=https://ephemera.example.com"
		echo "# EPHEMERA_MCP_ENABLED=1              # enable the MCP tool gateway (needs configs/mcp/servers.yaml)"
	  } > "$envf" )
	chmod 600 "$envf"
}

# ------------------------------------------------------------------ systemd ----
# [학습 주석] ephemera.service.in의 @DEST@ 자리표시자를 이 스크립트의 DEST로
# 치환해 실제 유닛 파일을 만든다. 그 결과 WorkingDirectory/EPHEMERA_HOME이 모두
# DEST를 가리키게 되고, daemon 기동 시 resolveWorkDir()이 EPHEMERA_HOME을 읽어
# 같은 경로를 workdir로 삼는다(설치 경로와 런타임 workdir이 한 곳으로 수렴).
install_service() {
	say "Installing the systemd service"
	sed "s|@DEST@|$DEST|g" "$SRC/ephemera.service.in" > /etc/systemd/system/ephemera.service
	systemctl daemon-reload
	systemctl enable --now ephemera
}

wait_ready() {
	say "Waiting for the daemon to become ready…"
	local i ok=0
	for i in $(seq 1 30); do
		if command -v curl >/dev/null 2>&1; then
			curl -fsS "http://${API_ADDR_DEFAULT}/ui/" >/dev/null 2>&1 && { ok=1; break; }
		fi
		systemctl is-active --quiet ephemera || break
		sleep 2
	done
	if [ "$ok" -eq 1 ]; then
		say "Daemon is up."
	elif systemctl is-active --quiet ephemera; then
		if [ "$VARIANT" = slim ]; then
			warn "Service is running; the SLIM first boot is baking the VM image (several minutes, needs network). Follow: journalctl -u ephemera -f"
		else
			warn "Service is running but the API did not answer yet. Check: journalctl -u ephemera -f"
		fi
	else
		warn "Service is not active. Inspect: systemctl status ephemera; journalctl -u ephemera -n 50"
	fi
}

summary() {
	cat <<EOF

────────────────────────────────────────────────────────────
 Ephemera installed at $DEST
   Web UI     http://localhost:3000/ui/
   CLI        ephemera-ctl vm spawn | vm ls | flock create ...
   Service    systemctl status|restart|stop ephemera
   Logs       journalctl -u ephemera -f
   Config     $DEST/configs/goose.yaml , goose-secrets.yaml
   Env        $DEST/ephemera.env   (edit -> systemctl restart ephemera)
   Uninstall  sudo $DEST/uninstall.sh
────────────────────────────────────────────────────────────
EOF
	if [ -n "${TOKEN_SHOWN:-}" ]; then
		echo " API token is set. For the CLI:  export EPHEMERA_API_TOKEN=<the token above>"
	fi
}

main() {
	preflight
	place_files
	configure_provider
	setup_env
	install_service
	wait_ready
	summary
}

main
