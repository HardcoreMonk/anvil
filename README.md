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