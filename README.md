# Ephemera

**Enterprise Control Plane for Ephemeral AI Agents via Firecracker MicroVMs**

Ephemera is a high-performance, Go-based orchestrator and Model Context Protocol (MCP) Gateway designed to manage the lifecycle of Agentic AI frameworks (like [Goose](https://github.com/aaif-goose/goose)). It provisions completely isolated, highly secure, and ephemeral MicroVM environments in milliseconds using AWS Firecracker.

## Key Features

* **Automated Infrastructure as Code (IaC) Bootstrapping:** Self-healing design that automatically builds and verifies Golden Images (Ubuntu 22.04 + Goose) via `debootstrap` if they do not exist.
* **Dynamic Network Provisioning:** Built-in IPAM (IP Address Management) that dynamically allocates private IPs (e.g., `10.0.1.X`) and creates/destroys `tap` network interfaces on the Host OS to prevent collisions.
* **Instant Storage Provisioning:** Deep cloning of `ext4` root filesystems into isolated workspaces for each spawned agent, ensuring zero cross-contamination between sessions.
* **MicroVM Lifecycle Management:** Integrates directly with `firecracker-go-sdk` to start, monitor, and gracefully shut down Virtual Machine Monitors (VMMs) via Unix Domain Sockets without manual JSON configuration.

## Architecture

```text
[Client / HTTP Request] -> [Ephemera Daemon]
                               ├── internal/network (IPAM & Tap Manager)
                               ├── internal/storage (Disk Provisioner)
                               └── internal/vm      (Firecracker SDK Wrapper)
                                      ⬇️
                         [Firecracker MicroVM] (Hardware Isolated)
                                      └── [Goose AI Agent] (Port 8000)
```
## Project Layout
* `cmd/goose-daemon/`: The main entry point of the daemon.
* `internal/`: Core business logic modules.
  + `vm/`: Handles Firecracker processes, socket communication, and VM boots.
  + `network/`: Manages thread-safe IP allocation and Linux netlink (tap devices).
  + `storage/`: Manages Golden Image self-building and per-session disk cloning.
* `scripts/`: Bootstrapping scripts (e.g., build_image.sh for unattended image creation).
* `artifacts/`: Directory for storing downloaded kernels and base Golden Images.

## Prerequisites
* **Host OS:** Ubuntu 22.04 LTS (Bare Metal or Cloud Instance with nested virtualization enabled).
* **Hardware:** `/dev/kvm` access is strictly required.
* **Runtime:** Go 1.20+
* **Dependencies:** `firecracker` binary must be installed in `/usr/local/bin/`.

## Getting Started
1. **Clone the repository:**
```
git clone https://github.com/steve-seungeui/ephemera.git
cd ephemera
```
2. **Build the daemon:**
```
go build -o ephemera-daemon ./cmd/goose-daemon/main.go
```
3. **Run the daemon:**
```
sudo ./ephemera-daemon
```
*Note: Root privileges are required to interact with `/dev/kvm` and network interfaces.*

*On the first run, Ephemera will automatically initiate the automated build process to generate the Golden Image. This may take a few minutes.*

## Security Posture
Ephemera ensures that every AI Agent runs within its own KVM-backed hardware boundary. Once a session terminates, the MicroVM is killed, the `tap` interface is deleted, and the isolated `ext4` disk is permanently wiped, leaving zero state behind.
