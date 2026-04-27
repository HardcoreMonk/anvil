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
	VMID       string // e.g., "agent-01"
	SocketPath string // e.g., "/tmp/firecracker-agent-01.sock"
	KernelPath string // Path to artifacts/vmlinux-5.10.bin
	RootfsPath string // Path to the cloned rootfs, e.g., artifacts/ubuntu-22.04-goose.ext4
	TapDevice  string // e.g., "tap0"
	MacAddress string // e.g., "AA:FC:00:00:00:01"
	GuestIP    string
	GatewayIP  string
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

	// Force inject dynamic IP into Guest OS via kernel parameters
	networkArg := fmt.Sprintf("ip=%s::%s:255.255.255.0::eth0:off", cfg.GuestIP, cfg.GatewayIP)
	kernelArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw %s", networkArg)

	fcCfg := firecracker.Config{
		SocketPath:        cfg.SocketPath,
		KernelImagePath:   cfg.KernelPath,
		KernelArgs:        kernelArgs, // Updated
		Drives:            drives,
		NetworkInterfaces: netIfaces,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(2),
			MemSizeMib: firecracker.Int64(2048),
		},
	}

	// Configure the logger to capture Firecracker's internal logs
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logEntry := logrus.NewEntry(logger)

	// Build the command to run the Firecracker binary as a child process
	// Ensure the binary exists at the specified path on the Host OS
	cmd := firecracker.VMCommandBuilder{}.
		WithBin("/usr/local/bin/firecracker").
		WithSocketPath(cfg.SocketPath).
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