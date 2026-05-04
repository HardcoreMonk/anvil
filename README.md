# Ephemera

**Enterprise Control Plane for Ephemeral AI Agents via Firecracker MicroVMs**

Ephemera orchestrates isolated, KVM-backed MicroVM environments for agentic AI workloads. Each VM runs [Goose](https://github.com/aaif-goose/goose) as an autonomous agent inside a minimal Debian guest, fully contained within hardware VM boundaries and completely wiped on termination.

---

## Architecture

```
External Client
      │  HTTPS (TLS-terminated by reverse proxy)
      ▼
Reverse Proxy  :443                   ← Caddy / Nginx (TLS termination)
      │  HTTP (Bearer token, encrypted inside TLS tunnel)
      ▼
Ephemera Control Plane  :3000         ← VM lifecycle management
  POST   /vms                         → spawn VM, return private IP + agent URL
  GET    /vms                         → list running VMs
  DELETE /vms/{vm_id}                 → stop & destroy VM

      │  provision
      ▼
MicroVM (Firecracker + KVM)           ← isolated KVM hardware boundary
  ├── Debian Bookworm minbase (rootfs)
  ├── micro-init  →  goose-agent :8080
  └── goose (AI agent, runs per task)

External Client
      │  HTTP (direct to VM private IP — reachable from host only)
      ▼
goose-agent  http://10.0.1.x:8080    ← private subnet 10.0.1.0/24
  POST  /tasks    {"prompt":"..."}    → run a Goose task, return result
  GET   /health                       → idle | busy
  POST  /stop                         → graceful shutdown
```

### VM Provisioning Flow

```
CloneDisk()      → copy golden image → per-VM ext4 disk
PrepareVM()      → inject config.yaml, secrets.yaml, /etc/localtime (single mount)
StartMachine()   → Firecracker: kernel + disk + TAP NIC
                    network configured via kernel boot parameter ip=<IP>::<GW>:<mask>::eth0:off
                    (no DHCP — IP is live before any user-space process starts)
waitForAgent()   → poll http://10.0.1.x:8080/health until ready
```

### Teardown Flow

```
DELETE /vms/{id}
  → StopVMM()          (SIGTERM to Firecracker process)
                        Firecracker terminates the VM; goose-agent (PID 1) exits,
                        causing the guest kernel to panic. This is intentional:
                        the panic is fully contained within the KVM hardware boundary
                        and does not affect the host OS or other VMs.
                        Firecracker intercepts the kexec reboot triggered by panic=1
                        and exits cleanly (exit_code=0).
                        The panic-on-exit approach is simpler and safe enough for
                        ephemeral VMs — a wrapper init that handles SIGTERM and calls
                        poweroff would be a more graceful alternative if needed.
  → CleanupDisk()      (delete cloned ext4)
  → Release()          (delete TAP device, return IP to pool)
```

---

## Key Features

- **Self-bootstrapping**: golden image, kernel (`vmlinux-6.1.155`), and Firecracker (`v1.15.1`) are downloaded automatically on first run; Firecracker binary is SHA256-verified.
- **Minimal guest OS**: Debian Bookworm `--variant=minbase` + tzdata + libgomp1. No SSH, no init daemon — `micro-init` mounts virtual filesystems and execs `goose-agent` directly as PID 1.
- **Runtime config injection**: `goose.yaml` (provider, model) and `goose-secrets.yaml` (API keys) are injected into each VM's disk at provisioning time — no rebuild required to change config.
- **Host timezone mirroring**: `/etc/localtime` symlink is injected per VM so Goose timestamps match the host.
- **IP and TAP recycling**: IPs (10.0.1.2–254) and TAP IDs are returned to a pool and reused across VM lifecycle.
- **NAT for outbound internet**: host bridge `goose-br0` with iptables MASQUERADE enables VM-to-internet connectivity (needed for LLM API calls).
- **API authentication**: optional Bearer token (`EPHEMERA_API_TOKEN`); API binds to `127.0.0.1` by default.

---

## Project Layout

```
cmd/
  goose-daemon/       Control plane daemon (main binary)
    main.go           Startup, artifact bootstrap, ControlPlane init
    api.go            ControlPlane HTTP API: /vms CRUD, VM lifecycle
    config.go         Env-var configuration (ports, token, address)
  goose-agent/        In-VM HTTP agent (baked into golden image)
    main.go           /tasks, /health, /stop endpoints

internal/
  vm/machine.go       Firecracker SDK wrapper, VMConfig, kernel args
  network/manager.go  IP pool (sorted), TAP device lifecycle, bridge, NAT
  storage/
    provisioner.go    Golden image bootstrap, disk clone, config/task injection,
                      Firecracker/kernel download + SHA256 verification

configs/
  goose.yaml.example       Provider, model, extensions template
  goose-secrets.yaml.example  API key template

scripts/
  build_image.sh      Builds golden image (Debian Bookworm + Goose + goose-agent)

artifacts/            Auto-populated at runtime (gitignored)
  golden-image.ext4   Golden VM disk image
  vmlinux.bin         Firecracker-compatible Linux 6.1 kernel
  firecracker         Firecracker VMM binary (SHA256-verified)
  goose-agent         In-VM HTTP agent binary (compiled from source)
```

---

## Prerequisites

| Requirement | Detail |
|-------------|--------|
| **Host OS** | Ubuntu 22.04 or 24.04 (bare metal, or VM with nested virtualization) |
| **CPU** | `/dev/kvm` accessible |
| **Go** | 1.18+ |
| **Packages** | `curl`, `debootstrap`, `e2fsprogs`, `util-linux` |
| **Privileges** | `sudo` at runtime (KVM + network interface management) |

```bash
sudo apt-get install -y curl debootstrap e2fsprogs util-linux
```

Firecracker, the Linux kernel, and the golden image are **downloaded and built automatically** on first run.

---

## Getting Started

### 1. Clone and build

```bash
git clone https://github.com/steve-seungeui/ephemera.git
cd ephemera
go build -o ephemera-daemon ./cmd/goose-daemon/
```

### 2. Configure Goose

```bash
cp configs/goose.yaml.example    configs/goose.yaml
cp configs/goose-secrets.yaml.example configs/goose-secrets.yaml
```

Edit `configs/goose.yaml` (provider, model):

```yaml
GOOSE_PROVIDER: google
GOOSE_MODEL: gemini-flash-latest
GOOSE_TELEMETRY_ENABLED: false
GOOSE_DISABLE_KEYRING: true   # required — MicroVM has no keyring daemon
```

Edit `configs/goose-secrets.yaml` (API key — **never commit this file**):

```yaml
GOOGLE_API_KEY: "your-key-here"
```

Supported providers: `google` · `anthropic` · `openai` · `ollama` · [others supported by Goose](https://goose-docs.ai/docs/getting-started/providers/).

### 3. Run

```bash
sudo ./ephemera-daemon
```

On first run, Ephemera will:
1. Build the `goose-agent` binary
2. Build the golden image via `debootstrap` (~5–8 minutes)
3. Download the Firecracker kernel and binary

Subsequent starts skip these steps if artifacts already exist.

---

## Configuration

All settings are read from environment variables at startup.

| Variable | Default | Description |
|----------|---------|-------------|
| `EPHEMERA_API_ADDR` | `127.0.0.1:3000` | Control plane bind address. Set to `0.0.0.0:3000` when behind a reverse proxy. |
| `EPHEMERA_API_PORT` | `3000` | Port only (used when `EPHEMERA_API_ADDR` is not set). |
| `EPHEMERA_API_TOKENS` | *(unset)* | Per-client Bearer tokens: `alice:token1,bob:token2`. Preferred over the single-token variable. |
| `EPHEMERA_API_TOKEN` | *(unset)* | Single Bearer token (backward-compatible fallback, treated as client name `default`). |
| `EPHEMERA_AGENT_PORT` | `8080` | Port goose-agent listens on inside each VM. |

`EPHEMERA_API_ADDR` takes precedence over `EPHEMERA_API_PORT`.

---

## API Reference

### Control Plane API (`localhost:3000`)

#### Spawn a VM

```bash
curl -X POST http://localhost:3000/vms \
  -H "Authorization: Bearer $TOKEN"
```

```json
{
  "vm_id":     "vm-1777776598887",
  "guest_ip":  "10.0.1.10",
  "agent_url": "http://10.0.1.10:8080"
}
```

The call blocks until `goose-agent` is ready inside the VM (~60 s).

#### List VMs

```bash
curl http://localhost:3000/vms -H "Authorization: Bearer $TOKEN"
```

#### Delete a VM

```bash
curl -X DELETE http://localhost:3000/vms/vm-1777776598887 \
  -H "Authorization: Bearer $TOKEN"
```

---

### goose-agent API (`http://<guest_ip>:8080`)

Clients call the VM's private IP **directly** — the control plane does not proxy task traffic.

#### Run a task

```bash
curl -X POST http://10.0.1.10:8080/tasks \
  -H "Content-Type: application/json" \
  -d '{"prompt": "Check my current system environment. Tell me the OS version, available disk space in the current directory."}'
```

```json
{ "output": "...", "error": "" }
```

Returns when the task completes. Only one task runs at a time per VM; concurrent requests receive `503 agent busy`.

#### Health check

```bash
curl http://10.0.1.10:8080/health
# {"status":"idle"}  or  {"status":"busy"}
```

#### Stop the agent

```bash
curl -X POST http://10.0.1.10:8080/stop
```

Gracefully shuts down `goose-agent`. The VM's kernel then panics (PID 1 exited) and Firecracker exits. Use `DELETE /vms/{id}` afterwards to release host resources.

---

## Security

### API authentication

#### Per-client tokens (recommended)

Issue a separate token for each caller so individual clients can be revoked without affecting others. Each request is logged with the matching client name for auditing.

```bash
# Generate tokens for each client
ALICE_TOKEN=$(openssl rand -hex 32)
BOB_TOKEN=$(openssl rand -hex 32)

export EPHEMERA_API_TOKENS="alice:$ALICE_TOKEN,bob:$BOB_TOKEN"
sudo -E ./ephemera-daemon
```

Startup log confirms registered clients:

```
Control plane API on 127.0.0.1:3000  (auth: Bearer token (2 client(s): alice, bob))
```

Each request is logged with the authenticated client name:

```
[alice] POST /vms
[bob]   GET  /vms
```

#### Single-token fallback (backward-compatible)

```bash
export EPHEMERA_API_TOKEN=$(openssl rand -hex 32)
sudo -E ./ephemera-daemon
```

Treated internally as a single client named `default`.

If neither variable is set, a startup warning is logged and the API is unauthenticated — **never expose the control plane without a token**.

#### Known limitation: token changes require a daemon restart

Tokens are loaded once at startup from environment variables. Adding, rotating, or revoking a token requires restarting the daemon, which **destroys all running VMs** (`DestroyAll` is called on exit). Planned improvement: SIGHUP-based hot reload that re-reads the token configuration without stopping running VMs.

Operational implications:

| Scenario | Impact |
|----------|--------|
| Adding a new client | Plan restart during a maintenance window |
| Periodic token rotation | Schedule when no long-running VMs are active |
| Emergency revocation (token compromised) | Restart immediately; accept VM interruption as unavoidable |

---

### TLS and network exposure

By default the control plane binds to `127.0.0.1:3000` (localhost only). Bearer tokens are sent in HTTP headers; without TLS they are transmitted in plaintext and can be captured by anyone on the network path.

**Placing a TLS-terminating reverse proxy in front solves the plaintext problem**: TLS encrypts the entire TCP payload (including HTTP headers) before transmission, so the Bearer token is never visible on the wire.

#### Step 1 — allow external binding

```bash
export EPHEMERA_API_ADDR=0.0.0.0:3000
sudo -E ./ephemera-daemon
```

#### Step 2 — configure a reverse proxy

**Caddy** (automatic HTTPS via Let's Encrypt — recommended):

```bash
sudo apt-get install -y caddy
```

`/etc/caddy/Caddyfile`:

```
api.example.com {
    reverse_proxy localhost:3000
}
```

```bash
sudo systemctl restart caddy
```

Caddy obtains and renews TLS certificates automatically.

---

**Nginx** (manual certificate required):

```bash
sudo apt-get install -y nginx
```

`/etc/nginx/sites-available/ephemera`:

```nginx
server {
    listen 443 ssl;
    server_name api.example.com;

    ssl_certificate     /etc/ssl/certs/ephemera.crt;
    ssl_certificate_key /etc/ssl/private/ephemera.key;
    ssl_protocols       TLSv1.2 TLSv1.3;

    location / {
        proxy_pass         http://127.0.0.1:3000;
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_read_timeout 120s;   # POST /vms blocks ~60 s waiting for goose-agent
    }
}

server {
    listen 80;
    server_name api.example.com;
    return 301 https://$host$request_uri;
}
```

```bash
sudo ln -s /etc/nginx/sites-available/ephemera /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl restart nginx
```

#### Step 3 — call via HTTPS

```bash
curl -X POST https://api.example.com/vms \
  -H "Authorization: Bearer $ALICE_TOKEN"
```

The goose-agent (`:8080`) runs on a private VM subnet (`10.0.1.0/24`) reachable only from the host. It is not directly exposed to external networks.

### VM isolation

- Each VM runs in a separate KVM hardware boundary.
- Each VM gets a **cloned** rootfs — no shared filesystem state between VMs.
- On teardown: TAP device deleted, disk wiped, IP returned to pool.
- Goose config and API keys are injected at provisioning time and exist only inside the ephemeral VM disk.

---

## License

MIT — see [LICENSE](LICENSE).
