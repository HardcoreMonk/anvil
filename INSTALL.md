# Installing Ephemera (from a release)

This guide is for **running** Ephemera from a prebuilt release tarball — no Go
toolchain or source checkout needed. (Developers building from source: see the
"Quick start" in [`README.md`](README.md).)

## Requirements

| | |
|---|---|
| OS / arch | Linux on **x86-64 (amd64)**, Ubuntu 22.04 or 24.04 |
| Virtualization | `/dev/kvm` present and accessible (bare metal, or a VM with nested virtualization) |
| Privileges | root (the daemon manages KVM, networking, and disk images) |
| Runtime packages | `iproute2`, `dmsetup`, `iptables` — `ebtables` optional (anti-spoof) |
| SLIM variant only | also `curl`, `debootstrap`, `e2fsprogs`, `util-linux` (the first boot builds the VM image) |

```bash
# Runtime (both variants):
sudo apt-get install -y iproute2 dmsetup iptables
# Only if you use the SLIM package (builds the VM image on first boot):
sudo apt-get install -y curl debootstrap util-linux e2fsprogs
```

The kernel and Firecracker binary are bundled in the tarball — they are **not**
OS packages and don't need installing.

## FULL vs SLIM

Two tarballs are published per release; pick one:

| Variant | Download | First VM | Needs at install |
|---|---|---|---|
| **FULL** (`…-full.tar.gz`) | larger (bundles the ~0.5 GB VM image) | **instant** | nothing extra |
| **SLIM** (`…-slim.tar.gz`) | small (~80–90 MB) | first boot bakes the image (several minutes, needs internet) | `debootstrap` + build tools above |

Choose FULL for the simplest experience; choose SLIM if download size matters and
the host can run `debootstrap` with outbound access to `deb.debian.org` and
`github.com/aaif-goose/goose`.

## Quick start

```bash
tar xzf ephemera-<version>-linux-amd64-full.tar.gz
cd ephemera-<version>
sudo ./install.sh
```

The installer is interactive — it will:

1. **Preflight** — verify amd64, `/dev/kvm`, and the runtime tools (and the
   image-build tools for SLIM); abort with a clear message if anything is missing.
2. **Install** binaries + config templates into **`/opt/ephemera`** (the daemon's
   working directory).
3. **Prompt** for your LLM provider (google / anthropic / openai / groq), model,
   and API key — writing `configs/goose.yaml` and `configs/goose-secrets.yaml`
   (mode `0600`).
4. **Offer** to generate a control-plane API token (stored in
   `/opt/ephemera/ephemera.env`).
5. **Register** a `systemd` service (`ephemera`), enable it, and start it.

## After install

| | |
|---|---|
| Web console | http://localhost:3000/ui/ |
| CLI | `ephemera-ctl vm spawn` · `vm ls` · `flock create …` (on `PATH`) |
| Service | `systemctl status\|restart\|stop ephemera` |
| Logs | `journalctl -u ephemera -f` |
| LLM config | `/opt/ephemera/configs/goose.yaml`, `goose-secrets.yaml` |
| Daemon env | `/opt/ephemera/ephemera.env` (edit → `systemctl restart ephemera`) |

If you set an API token, the CLI reads it from `EPHEMERA_API_TOKEN`:

```bash
export EPHEMERA_API_TOKEN=<the token shown by the installer>
ephemera-ctl vm spawn
```

**More providers / models** — edit `/opt/ephemera/configs/goose.yaml` (provider +
model) and add the matching key to `goose-secrets.yaml`, then create per-profile
configs from the **Settings** screen in the Web UI. **MCP tool gateway** — set
`EPHEMERA_MCP_ENABLED=1` in `ephemera.env` and add `configs/mcp/servers.yaml`
(see `docs/guides/runtime-usage.md` → MCP Gateway), then `systemctl restart ephemera`.

## Exposing the API beyond localhost

The API binds to `127.0.0.1:3000` by default. To reach it remotely, set
`EPHEMERA_API_ADDR=0.0.0.0:3000` in `ephemera.env`, keep a token set, and put a
TLS-terminating reverse proxy in front (the daemon speaks plain HTTP). The systemd
service runs as **root with no sandbox** — it needs `/dev/kvm`, raw networking, and
loop mounts — so treat control-plane access as privileged.

## Upgrading

Unpack a newer release and re-run `sudo ./install.sh`. It stops the service,
refreshes the binaries and bundled image, **preserves** your `configs/` and
`ephemera.env`, and restarts. Use `sudo ./install.sh --reconfigure` to re-run the
provider/key prompt.

## Uninstalling

```bash
sudo /opt/ephemera/uninstall.sh            # remove the service; keep configs + image
sudo /opt/ephemera/uninstall.sh --purge    # also delete /opt/ephemera (asks to confirm)
```

OS packages (`iproute2`, etc.) are left untouched.

## Troubleshooting

- **Service won't start** — `journalctl -u ephemera -n 80`. A bad API key surfaces
  here when the first VM runs a task.
- **`/dev/kvm` permission denied** — confirm KVM is enabled; on a cloud VM, enable
  nested virtualization.
- **SLIM first boot hangs for minutes** — that's the one-time image bake; follow
  `journalctl -u ephemera -f`. It needs outbound HTTPS; an offline host can't bake
  (use the FULL package there).
- **`GLIBC_2.xx not found`** — the host's glibc is older than the release build
  host's. Use a release built on Ubuntu 22.04, or build from source.
