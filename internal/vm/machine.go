package vm

import (
	"context"
	"fmt"
	"os"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

// VMConfig holds the configuration required by the control plane to provision a new VM
type VMConfig struct {
	VMID           string // e.g., "agent-01"
	SocketPath     string // e.g., "/tmp/firecracker-agent-01.sock"
	FirecrackerBin string // Path to artifacts/firecracker
	KernelPath     string // Path to artifacts/vmlinux.bin
	RootfsPath     string // Path to the cloned rootfs
	TapDevice      string // e.g., "tap0"
	MacAddress     string // e.g., "AA:FC:00:00:00:01"
	GuestIP        string
	GatewayIP      string
}

// StartMachine initializes and boots a Firecracker microVM based on the provided VMConfig.
func StartMachine(ctx context.Context, cfg VMConfig) (*firecracker.Machine, error) {
	rootDriveID := "rootfs"
	isRootDevice := true
	isReadOnly := false

	drives := []models.Drive{
		{
			DriveID:      &rootDriveID,
			PathOnHost:   &cfg.RootfsPath,
			IsRootDevice: &isRootDevice,
			IsReadOnly:   &isReadOnly,
		},
	}

	netIfaces := []firecracker.NetworkInterface{
		{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  cfg.MacAddress,
				HostDevName: cfg.TapDevice,
			},
		},
	}

	// Inject dynamic IP via kernel ip= parameter.
	// pci=off, root=/dev/vda, rw are intentionally omitted: the firecracker-go-sdk
	// derives them from the Drive configuration and appends them automatically,
	// so specifying them here would cause duplicates in the kernel command line.
	networkArg := fmt.Sprintf("ip=%s::%s:255.255.255.0::eth0:off", cfg.GuestIP, cfg.GatewayIP)
	// reboot=k panic=1: when goose-agent (PID 1) exits, the guest kernel panics.
	// This is intentional for ephemeral VMs — the panic is contained within the KVM
	// hardware boundary and does not affect the host OS. Firecracker intercepts the
	// kexec reboot triggered by panic=1 and exits cleanly (exit_code=0), after which
	// the control plane releases all host resources (TAP, disk, IP).
	// A more graceful alternative would be a wrapper init that catches SIGTERM and
	// calls poweroff, but the panic→immediate-exit approach is simpler and safe
	// enough for this ephemeral use case.
	kernelArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 %s init=/usr/local/sbin/micro-init", networkArg)

	// Provide a log FIFO path so the SDK sends PUT /logger to Firecracker with
	// LogLevel=Warning, suppressing verbose INFO-level API trace messages.
	// The SDK (CreateLogFilesHandler) creates the FIFO itself — we only remove
	// any stale file left by a previous run so the SDK's Mkfifo call succeeds.
	logFifoPath := fmt.Sprintf("/tmp/fc-%s-log.fifo", cfg.VMID)
	os.Remove(logFifoPath)

	fcCfg := firecracker.Config{
		SocketPath:        cfg.SocketPath,
		KernelImagePath:   cfg.KernelPath,
		KernelArgs:        kernelArgs,
		Drives:            drives,
		NetworkInterfaces: netIfaces,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(2),
			MemSizeMib: firecracker.Int64(2048),
		},
		LogFifo:  logFifoPath,
		LogLevel: "Warning",
	}

	// Capture Firecracker process logs at Warn level.
	// Debug produces hundreds of lines per boot (API PUT/GET traces, handler names)
	// that are not useful in normal operation.
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	logEntry := logrus.NewEntry(logger)

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(cfg.FirecrackerBin).
		WithSocketPath(cfg.SocketPath).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		Build(ctx)

	machineOpts := []firecracker.Opt{
		firecracker.WithLogger(logEntry),
		firecracker.WithProcessRunner(cmd),
	}

	// Create the Machine instance (not yet booted)
	m, err := firecracker.NewMachine(ctx, fcCfg, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create machine: %w", err)
	}

	// Start the process and boot the VM (handles API socket communication automatically)
	if err := m.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start machine: %w", err)
	}

	logger.Infof("MicroVM [%s] successfully started on socket %s", cfg.VMID, cfg.SocketPath)

	// Return the machine instance so the control plane can manage its lifecycle (e.g., stop, wait)
	return m, nil
}